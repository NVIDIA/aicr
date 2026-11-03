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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

const (
	routeTestNamespace = "aicr-inference-perf-test"
	routeTestName      = "aicr-inference-route"
)

func httpRouteObject(accepted string, reason string) *unstructured.Unstructured {
	route := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]any{"name": routeTestName, "namespace": routeTestNamespace},
	}}
	if accepted != "" {
		route.Object["status"] = map[string]any{"parents": []any{map[string]any{
			"parentRef":  map[string]any{"name": "inference-gateway", "namespace": "edge-system"},
			"conditions": []any{map[string]any{"type": "Accepted", "status": accepted, "reason": reason, "message": "listener policy"}},
		}}}
	}
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
		name     string
		route    *unstructured.Unstructured
		wantErr  string
		wantCode errors.ErrorCode
	}{
		{name: "already accepted", route: httpRouteObject("True", "Accepted")},
		{
			name: "rejected by the listener policy", route: httpRouteObject("False", "NotAllowedByListeners"),
			wantErr: "allowedRoutes.namespaces.from: All", wantCode: errors.ErrCodeInvalidRequest,
		},
		{name: "no condition before the wait expires proceeds", route: httpRouteObject("", "")},
		{name: "unknown status is not a decision", route: httpRouteObject("Unknown", "Pending")},
		{
			name: "route missing after apply is a read failure", route: nil,
			wantErr: "failed to read the HTTPRoute", wantCode: errors.ErrCodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vctx := &validators.Context{Ctx: context.Background(), DynamicClient: newRouteClient(t, tt.route)}
			err := waitForHTTPRouteAccepted(vctx, routeTestNamespace, routeTestName, 200*time.Millisecond)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
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
			err := waitForHTTPRouteAccepted(vctx, routeTestNamespace, routeTestName, 5*time.Second)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestWaitForHTTPRouteAccepted_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	vctx := &validators.Context{Ctx: ctx, DynamicClient: newRouteClient(t, httpRouteObject("", ""))}
	err := waitForHTTPRouteAccepted(vctx, routeTestNamespace, routeTestName, time.Second)
	if err == nil {
		t.Fatal("expected an error for a canceled context")
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
		{name: "recipe with no components is an error", input: &validatorv1.ValidationInput{}, wantErr: true},
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
