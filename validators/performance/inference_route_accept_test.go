// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	routeTestNamespace = "aicr-inference-perf-test"
	routeTestName      = "aicr-inference-route"
	routeTestGateway   = "inference-gateway"
	routeTestGatewayNS = "edge-system"
)

// routeParentStatus is one status.parents entry as a controller writes it.
func routeParentStatus(name, namespace, accepted, reason string, observedGeneration int64) map[string]any {
	return map[string]any{
		"parentRef": map[string]any{"name": name, "namespace": namespace},
		"conditions": []any{map[string]any{
			"type": "Accepted", "status": accepted, "reason": reason, "message": "listener policy",
			"observedGeneration": observedGeneration,
		}},
	}
}

// httpRouteObject is a route at generation 1 whose configured gateway reports
// accepted (or nothing when accepted is empty) at that generation.
func httpRouteObject(accepted string, reason string) *unstructured.Unstructured {
	route := httpRouteAt(1)
	if accepted != "" {
		route.Object["status"] = map[string]any{"parents": []any{
			routeParentStatus(routeTestGateway, routeTestGatewayNS, accepted, reason, 1),
		}}
	}
	return route
}

func httpRouteAt(generation int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]any{"name": routeTestName, "namespace": routeTestNamespace, "generation": generation},
	}}
}

func withParents(route *unstructured.Unstructured, parents ...map[string]any) *unstructured.Unstructured {
	entries := make([]any, 0, len(parents))
	for _, p := range parents {
		entries = append(entries, p)
	}
	route.Object["status"] = map[string]any{"parents": entries}
	return route
}

func newRouteClient(t *testing.T, route *unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{httpRouteGVR: "HTTPRouteList"})
	if route != nil {
		if err := dyn.Tracker().Create(httpRouteGVR, route, route.GetNamespace()); err != nil {
			t.Fatalf("seed HTTPRoute: %v", err)
		}
	}
	return dyn
}

func TestWaitForHTTPRouteAccepted(t *testing.T) {
	tests := []struct {
		name       string
		route      *unstructured.Unstructured
		wantErr    string
		wantNoHint bool
		wantCode   errors.ErrorCode
	}{
		{name: "already accepted", route: httpRouteObject("True", "Accepted")},
		{
			name: "rejected by the listener policy", route: httpRouteObject("False", "NotAllowedByListeners"),
			wantErr: "allowedRoutes.namespaces.from: All, or a Selector matching the per-run namespace's label app.kubernetes.io/component=inference-perf", wantCode: errors.ErrCodeInvalidRequest,
		},
		{name: "no condition before the wait expires proceeds", route: httpRouteObject("", "")},
		{
			// Only a listener-policy rejection points at allowedRoutes; another
			// reason is reported as the controller states it.
			name: "rejected for another reason carries no allowedRoutes hint", route: httpRouteObject("False", "NoMatchingParent"),
			wantErr: "Accepted=False reason=NoMatchingParent", wantNoHint: true, wantCode: errors.ErrCodeInvalidRequest,
		},
		{name: "unknown status is not a decision", route: httpRouteObject("Unknown", "Pending")},
		{
			// A retained namespace updates the route in place; the previous
			// run's verdict is observed at an older generation and must not
			// pass or fail the current one.
			name: "stale rejection from an earlier generation is ignored",
			route: withParents(httpRouteAt(2),
				routeParentStatus(routeTestGateway, routeTestGatewayNS, "False", "NotAllowedByListeners", 1)),
		},
		{
			name: "stale acceptance from an earlier generation is ignored",
			route: withParents(httpRouteAt(2),
				routeParentStatus(routeTestGateway, routeTestGatewayNS, "True", "Accepted", 1)),
		},
		{
			name: "another parent's verdict is ignored",
			route: withParents(httpRouteAt(1),
				routeParentStatus("other-gateway", routeTestGatewayNS, "False", "NotAllowedByListeners", 1)),
		},
		{
			name: "parentRef without a namespace defaults to the route's namespace",
			route: withParents(httpRouteAt(1),
				routeParentStatus(routeTestGateway, "", "False", "NotAllowedByListeners", 1)),
		},
		{
			// observedGeneration is optional on a condition; a controller
			// that omits it must still be able to reject the route.
			name: "rejection without observedGeneration is decisive",
			route: withParents(httpRouteAt(3), func() map[string]any {
				p := routeParentStatus(routeTestGateway, routeTestGatewayNS, "False", "NotAllowedByListeners", 0)
				delete(p["conditions"].([]any)[0].(map[string]any), "observedGeneration")
				return p
			}()),
			wantErr: "NotAllowedByListeners", wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name: "current-generation rejection beside a stale acceptance is decisive",
			route: withParents(httpRouteAt(2),
				routeParentStatus(routeTestGateway, routeTestGatewayNS, "True", "Accepted", 1),
				routeParentStatus(routeTestGateway, routeTestGatewayNS, "False", "NotAllowedByListeners", 2)),
			wantErr: "NotAllowedByListeners", wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name: "route missing after apply is a read failure", route: nil,
			wantErr: "failed to read the HTTPRoute", wantCode: errors.ErrCodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vctx := &validators.Context{Ctx: context.Background(), DynamicClient: newRouteClient(t, tt.route)}
			err := waitForHTTPRouteAccepted(vctx, routeTestNamespace, routeTestName, routeTestGatewayNS, routeTestGateway, "", 200*time.Millisecond)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			if tt.wantNoHint && strings.Contains(err.Error(), "allowedRoutes") {
				t.Errorf("error %q points at allowedRoutes for a reason the listener policy did not cause", err)
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
		})
	}
}

// TestWaitForHTTPRouteAccepted_DecidedDuringWatch covers the path a real
// controller takes: status arrives after the route exists.
func TestWaitForHTTPRouteAccepted_DecidedDuringWatch(t *testing.T) {
	for _, tt := range []struct {
		name     string
		accepted string
		wantErr  bool
	}{
		{"accepted during the watch", "True", false},
		{"rejected during the watch", "False", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dyn := newRouteClient(t, httpRouteObject("", ""))
			go func() {
				time.Sleep(50 * time.Millisecond)
				if err := dyn.Tracker().Update(httpRouteGVR, httpRouteObject(tt.accepted, "NotAllowedByListeners"), routeTestNamespace); err != nil {
					t.Errorf("update HTTPRoute: %v", err)
				}
			}()
			vctx := &validators.Context{Ctx: context.Background(), DynamicClient: dyn}
			err := waitForHTTPRouteAccepted(vctx, routeTestNamespace, routeTestName, routeTestGatewayNS, routeTestGateway, "", 5*time.Second)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestWaitForHTTPRouteAccepted_WatchClosed covers an apiserver closing the
// watch early: the route is re-read, a verdict written meanwhile is honored,
// and an undecided route still proceeds.
func TestWaitForHTTPRouteAccepted_WatchClosed(t *testing.T) {
	for _, tt := range []struct {
		name string
		// afterWatch, when set, replaces the route once the watch has been
		// requested, so the initial read cannot have seen it and only the
		// re-read can.
		afterWatch *unstructured.Unstructured
		wantErr    bool
	}{
		{"undecided after re-read proceeds", nil, false},
		{"rejection seen on re-read fails", httpRouteObject("False", "NotAllowedByListeners"), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dyn := newRouteClient(t, httpRouteObject("", ""))
			watched := false
			dyn.PrependWatchReactor("httproutes", func(k8stesting.Action) (bool, watch.Interface, error) {
				watched = true
				if tt.afterWatch != nil {
					if err := dyn.Tracker().Update(httpRouteGVR, tt.afterWatch, routeTestNamespace); err != nil {
						return false, nil, err
					}
				}
				w := watch.NewFake()
				w.Stop()
				return true, w, nil
			})
			vctx := &validators.Context{Ctx: context.Background(), DynamicClient: dyn}
			err := waitForHTTPRouteAccepted(vctx, routeTestNamespace, routeTestName, routeTestGatewayNS, routeTestGateway, "", 5*time.Second)
			if !watched {
				t.Fatal("the route was decided before the watch; the re-read branch did not run")
			}
			gets := 0
			for _, action := range dyn.Actions() {
				if action.GetVerb() == "get" {
					gets++
				}
			}
			if gets != 2 {
				t.Fatalf("route read %d time(s), want 2 (the initial read and the re-read after the watch closed)", gets)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

// TestWaitForHTTPRouteAccepted_DeletedDuringWatch pins that a Deleted event,
// which carries the route's last state, is not read as a decision: a route
// that was accepted and then removed leaves the benchmark nothing to send
// through.
func TestWaitForHTTPRouteAccepted_DeletedDuringWatch(t *testing.T) {
	dyn := newRouteClient(t, httpRouteObject("", ""))
	// The watch is fed the Deleted event directly: through the tracker, the
	// accepted update that precedes a delete could decide the wait first, and
	// only the ordering where the Deleted event is observed proves the guard.
	dyn.PrependWatchReactor("httproutes", func(k8stesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFake()
		go func() {
			time.Sleep(20 * time.Millisecond)
			w.Delete(httpRouteObject("True", "Accepted"))
		}()
		return true, w, nil
	})
	vctx := &validators.Context{Ctx: context.Background(), DynamicClient: dyn}
	err := waitForHTTPRouteAccepted(vctx, routeTestNamespace, routeTestName, routeTestGatewayNS, routeTestGateway, "", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "was deleted while waiting") {
		t.Fatalf("error = %v, want the deleted-route failure", err)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
}

func TestWaitForHTTPRouteAccepted_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	vctx := &validators.Context{Ctx: ctx, DynamicClient: newRouteClient(t, httpRouteObject("", ""))}
	err := waitForHTTPRouteAccepted(vctx, routeTestNamespace, routeTestName, routeTestGatewayNS, routeTestGateway, "", time.Second)
	if err == nil {
		t.Fatal("expected an error for a canceled context")
	}
}

// TestDeployInferenceWorkload_ResolvesGatewayBeforeProvisioning pins that a
// gateway-epp run with a recipe error fails before anything is created: the
// namespace is the first resource provisioned, so its absence proves the
// profile was resolved first (the model-cache populate Job behind it can hold
// a run for minutes).
func TestDeployInferenceWorkload_ResolvesGatewayBeforeProvisioning(t *testing.T) {
	client := fake.NewClientset()
	vctx := &validators.Context{
		Ctx: context.Background(), Clientset: client, DynamicClient: newRouteClient(t, nil),
		ValidationInput: &validatorv1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}},
	}
	config := &inferenceWorkloadConfig{namespace: routeTestNamespace, routingMode: inferenceRoutingModeGatewayEPP}

	err := deployInferenceWorkload(vctx, config)
	if err == nil {
		t.Fatal("expected the missing gateway declaration to fail the run")
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if _, getErr := client.CoreV1().Namespaces().Get(context.Background(), routeTestNamespace, metav1.GetOptions{}); getErr == nil {
		t.Errorf("namespace %s was created before the gateway declaration was checked", routeTestNamespace)
	}
	if config.deployedByUs {
		t.Error("deployedByUs must stay false when nothing was provisioned")
	}
}

// TestInferenceGatewayProfile_Gate pins when the built-in target is allowed:
// only a run with no recipe at all. A recipe that declares no gateway, even
// one with no components, is an error rather than a benchmark of whatever
// carries the conventional name.
func TestInferenceGatewayProfile_Gate(t *testing.T) {
	tests := []struct {
		name    string
		input   *validatorv1.ValidationInput
		want    string
		wantErr bool
	}{
		{name: "no recipe keeps the built-in target", input: nil, want: validators.DefaultInferenceGatewayName},
		{
			// A Job always loads a validation input, so a run without a
			// recipe presents as one with no components.
			name:  "validation input with no components keeps the built-in target",
			input: &validatorv1.ValidationInput{},
			want:  validators.DefaultInferenceGatewayName,
		},
		{
			name:    "recipe without a gateway is an error",
			input:   &validatorv1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}},
			wantErr: true,
		},
		{
			name:  "recipe with the built-in gateway",
			input: &validatorv1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: "agentgateway"}}},
			want:  validators.DefaultInferenceGatewayName,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile, err := inferenceGatewayProfile(&validators.Context{ValidationInput: tt.input})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if profile.GatewayName != tt.want {
				t.Errorf("GatewayName = %q, want %q", profile.GatewayName, tt.want)
			}
		})
	}
}

// TestWaitForHTTPRouteAccepted_PinnedSection pins that a route attached to
// one listener is judged by that listener's parent entry only. A retained
// route updated from unpinned to pinned can still carry the unpinned entry,
// and observedGeneration is optional, so that entry could otherwise pass.
func TestWaitForHTTPRouteAccepted_PinnedSection(t *testing.T) {
	withSection := func(p map[string]any, section string) map[string]any {
		p["parentRef"].(map[string]any)["sectionName"] = section
		delete(p["conditions"].([]any)[0].(map[string]any), "observedGeneration")
		return p
	}
	unpinned := func(accepted, reason string) map[string]any {
		p := routeParentStatus(routeTestGateway, routeTestGatewayNS, accepted, reason, 0)
		delete(p["conditions"].([]any)[0].(map[string]any), "observedGeneration")
		return p
	}
	tests := []struct {
		name    string
		route   *unstructured.Unstructured
		wantErr bool
	}{
		{
			name:  "an unpinned acceptance does not decide a pinned route",
			route: withParents(httpRouteAt(2), unpinned("True", "Accepted")),
		},
		{
			name: "the pinned listener's rejection decides beside an unpinned acceptance",
			route: withParents(httpRouteAt(2), unpinned("True", "Accepted"),
				withSection(routeParentStatus(routeTestGateway, routeTestGatewayNS, "False", "NotAllowedByListeners", 0), "open")),
			wantErr: true,
		},
		{
			name: "another listener's acceptance does not decide",
			route: withParents(httpRouteAt(2),
				withSection(routeParentStatus(routeTestGateway, routeTestGatewayNS, "True", "Accepted", 0), "other")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decided, err := httpRouteAccepted(tt.route, routeTestGatewayNS, routeTestGateway, "open")
			if decided != tt.wantErr || (err != nil) != tt.wantErr {
				t.Fatalf("decided=%v err=%v, want decided=%v with error=%v", decided, err, tt.wantErr, tt.wantErr)
			}
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}
