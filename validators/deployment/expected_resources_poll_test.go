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
	stderrors "errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// TestMain shrinks the GPU readiness poll tunables so the poll-based tests run
// quickly instead of in minutes. Set once before any test runs and never
// mutated afterward, so it stays race-free under t.Parallel. The values are kept
// well above single-digit-ms so timer coalescing / scheduler jitter under -race
// and high parallelism cannot starve a ride-through past its budget (which would
// flake): the timeout is ~14x a ride-through's real work.
func TestMain(m *testing.M) {
	gpuReadinessPollInterval = 10 * time.Millisecond
	gpuReadinessStabilityWindow = 50 * time.Millisecond
	gpuReadinessTimeout = 1 * time.Second
	os.Exit(m.Run())
}

// TestVerifyNodewrightReady_Poll drives the deployment-phase Skyhook readiness
// poll through a scripted status.status sequence (the last entry repeats for
// every subsequent Get). It proves the poll rides through transient
// status=in_progress (a reboot flap) and still fails closed — surfacing the last
// observed status — when the CR never reaches complete within the budget.
func TestVerifyNodewrightReady_Poll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statuses   []string
		wantErrSub string // "" => expect success
		minGets    int    // minimum Gets expected on the success path
	}{
		{
			name:     "rides through transient in_progress",
			statuses: []string{"in_progress", "in_progress", nodewrightCompleteState},
			minGets:  3,
		},
		{
			name:       "times out on persistent in_progress",
			statuses:   []string{"in_progress"},
			wantErrSub: "Nodewright tuning: status=in_progress (want complete)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dyn := newFlippingDynamicClient("tuning", tt.statuses)
			ref := recipe.ComponentRef{Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest}}
			ctx := newDeploymentTestContextWithDynamic(t,
				[]runtime.Object{activeNamespace("skyhook")}, dyn, []recipe.ComponentRef{ref})

			err := verifyNodewrightReady(ctx, ref)
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("verifyNodewrightReady() error = nil, want error containing %q", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("verifyNodewrightReady() error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("verifyNodewrightReady() error = %v, want nil (should ride through transient in_progress)", err)
			}
			if got := dyn.getCount(); got < tt.minGets {
				t.Fatalf("expected the poll to sample the CR at least %d times (through the reboot), got %d", tt.minGets, got)
			}
		})
	}
}

// TestVerifyNodewrightReady_RidesThroughRuntimeRequiredTaint proves the taint
// gate is polled, not sampled once: with every expected Skyhook CR already
// "complete", the check still stays closed while a node carries the
// runtime-required taint and only passes once the operator removes it (the
// monotone terminal step). The last-observed taint is surfaced when it never
// clears within the budget.
func TestVerifyNodewrightReady_RidesThroughRuntimeRequiredTaint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		tainted    []bool // per-List: whether gpu-node-0 still carries the taint (last repeats)
		wantErrSub string // "" => expect success
		minLists   int
	}{
		{
			name:     "rides through a lingering taint that clears",
			tainted:  []bool{true, true, false},
			minLists: 3,
		},
		{
			name:       "times out while the taint persists",
			tainted:    []bool{true},
			wantErrSub: "node gpu-node-0: still carries the runtime-required taint",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ref := recipe.ComponentRef{Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest}}
			// Skyhook CR is always complete; the taint is the only gating signal.
			dyn := newFlippingDynamicClient("tuning", []string{nodewrightCompleteState})
			ctx := newDeploymentTestContextWithDynamic(t,
				[]runtime.Object{activeNamespace("skyhook")}, dyn, []recipe.ComponentRef{ref})

			taintSeq := tt.tainted
			var lists int32
			ctx.Clientset.(*k8sfake.Clientset).PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
				idx := int(atomic.AddInt32(&lists, 1)) - 1
				if idx >= len(taintSeq) {
					idx = len(taintSeq) - 1
				}
				node := nodeWithTaints("gpu-node-0")
				if taintSeq[idx] {
					node = nodeWithRuntimeRequiredTaint("gpu-node-0")
				}
				return true, &corev1.NodeList{Items: []corev1.Node{*node}}, nil
			})

			err := verifyNodewrightReady(ctx, ref)
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("verifyNodewrightReady() error = nil, want error containing %q", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("verifyNodewrightReady() error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("verifyNodewrightReady() error = %v, want nil (should ride through the lingering taint)", err)
			}
			if got := int(atomic.LoadInt32(&lists)); got < tt.minLists {
				t.Fatalf("expected the poll to list nodes at least %d times (through the taint), got %d", tt.minLists, got)
			}
		})
	}
}

// TestRuntimeRequiredTaintFailures_FailsClosedOnListError proves a node-list
// failure is surfaced (not read as "taint absent"), so the poll fails closed and
// retries rather than certifying readiness on missing data.
func TestRuntimeRequiredTaintFailures_FailsClosedOnListError(t *testing.T) {
	t.Parallel()

	clientset := k8sfake.NewClientset()
	clientset.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New("apiserver unavailable")
	})
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

	failures, err := runtimeRequiredTaintFailures(ctx)
	if err == nil {
		t.Fatal("expected an error when listing nodes fails, got nil (must fail closed)")
	}
	if failures != nil {
		t.Fatalf("expected nil failures on list error, got %v", failures)
	}
	if !strings.Contains(err.Error(), "failed to list nodes for the nodewright runtime-required taint gate") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestVerifyDRAKubeletPluginReady_Poll drives the DRA kubelet-plugin readiness
// poll through a scripted DaemonSet readiness sequence (each entry is the
// NumberReady==DesiredNumberScheduled count for one List; the last repeats). It
// proves the poll rides through the transient 0/0 window a GPU-node reboot opens
// and still fails closed — surfacing the last observed pod counts — when the
// DaemonSet never becomes ready within the budget.
func TestVerifyDRAKubeletPluginReady_Poll(t *testing.T) {
	t.Parallel()

	const namespace = "nvidia-dra-driver"
	tests := []struct {
		name       string
		readySeq   []int32
		wantErrSub string // "" => expect success
		minLists   int    // minimum Lists expected on the success path
	}{
		{
			name:     "rides through transient 0/0",
			readySeq: []int32{0, 0, 2},
			minLists: 3,
		},
		{
			name:       "times out on persistent unready",
			readySeq:   []int32{0},
			wantErrSub: "no ready kubelet-plugin pods scheduled (0/0 pods ready)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			readySeq := tt.readySeq
			clientset := k8sfake.NewClientset()
			var calls int32
			clientset.PrependReactor("list", "daemonsets", func(clienttesting.Action) (bool, runtime.Object, error) {
				idx := int(atomic.AddInt32(&calls, 1)) - 1
				if idx >= len(readySeq) {
					idx = len(readySeq) - 1
				}
				return true, &appsv1.DaemonSetList{
					Items: []appsv1.DaemonSet{*readyDaemonSet(namespace, testDefaultDRADSName, readySeq[idx])},
				}, nil
			})

			ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}
			err := verifyDRAKubeletPluginReady(ctx, namespace)
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("verifyDRAKubeletPluginReady() error = nil, want error containing %q", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("verifyDRAKubeletPluginReady() error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("verifyDRAKubeletPluginReady() error = %v, want nil (should ride through transient 0/0)", err)
			}
			if got := int(atomic.LoadInt32(&calls)); got < tt.minLists {
				t.Fatalf("expected the poll to list DaemonSets at least %d times (through the reboot), got %d", tt.minLists, got)
			}
		})
	}
}

// TestVerifyDRAKubeletPluginReady_FailsFastOnAmbiguousMatch proves the upfront
// structural gate returns the ambiguous-match error immediately (a deterministic
// misconfiguration) rather than retrying it through the poll for the full
// budget: exactly one List is issued, and no poll iterations run.
func TestVerifyDRAKubeletPluginReady_FailsFastOnAmbiguousMatch(t *testing.T) {
	t.Parallel()

	const namespace = "nvidia-dra-driver"
	var calls int32
	clientset := k8sfake.NewClientset()
	clientset.PrependReactor("list", "daemonsets", func(clienttesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt32(&calls, 1)
		return true, &appsv1.DaemonSetList{Items: []appsv1.DaemonSet{
			*readyDaemonSet(namespace, "dra-a"+draKubeletPluginSuffix, 2),
			*readyDaemonSet(namespace, "dra-b"+draKubeletPluginSuffix, 2),
		}}, nil
	})

	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}
	err := verifyDRAKubeletPluginReady(ctx, namespace)
	if err == nil {
		t.Fatal("expected an error when two DaemonSets match the kubelet-plugin suffix")
	}
	if !strings.Contains(err.Error(), "ambiguous:") {
		t.Fatalf("expected the ambiguous-match error, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 List (fail fast, no polling), got %d", got)
	}
}

// TestPollUntilStable_TimesOutWhenNeverHoldsWindow covers pollUntilStable's
// distinct cause==nil timeout branch: the signal is healthy but never stays
// healthy long enough to satisfy the stability window before the budget elapses.
//
// Driving this through the flipping/reactor helpers would be timing-flaky (which
// sample lands last before the deadline is scheduler-dependent, so cause is not
// reliably nil). Instead it calls pollUntilStable directly with an always-healthy
// probe and a stability window larger than the timeout, which makes the "did not
// hold" path deterministic. It is intentionally NOT parallel: it briefly narrows
// the shared poll tunables and restores them in Cleanup, running in the
// sequential phase before the t.Parallel tests resume, so it stays race-free.
func TestPollUntilStable_TimesOutWhenNeverHoldsWindow(t *testing.T) {
	origInterval, origWindow, origTimeout := gpuReadinessPollInterval, gpuReadinessStabilityWindow, gpuReadinessTimeout
	t.Cleanup(func() {
		gpuReadinessPollInterval, gpuReadinessStabilityWindow, gpuReadinessTimeout = origInterval, origWindow, origTimeout
	})
	// Window >> timeout: an always-healthy probe can never satisfy the dwell.
	gpuReadinessPollInterval = 1 * time.Millisecond
	gpuReadinessStabilityWindow = 10 * time.Second
	gpuReadinessTimeout = 20 * time.Millisecond

	ctx := &validators.Context{Ctx: context.Background()}
	err := pollUntilStable(ctx, "tuning signal",
		func() error { return nil },
		func() { t.Fatal("onStable must not fire when the stability window is never satisfied") })
	if err == nil {
		t.Fatal("expected a timeout error when the signal never holds the stability window")
	}
	if !strings.Contains(err.Error(), "tuning signal became healthy but did not hold it for the") {
		t.Fatalf("expected the distinct 'did not hold' message, got: %v", err)
	}
	// StructuredError.Error() prefixes the code, so this asserts ErrCodeTimeout
	// (not ErrCodeInternal) without importing pkg/errors.
	if !strings.Contains(err.Error(), "[TIMEOUT]") {
		t.Fatalf("expected ErrCodeTimeout classification, got: %v", err)
	}
}

// newDeploymentTestContextWithDynamic builds a validators.Context with the
// skyhook GroupVersion registered in discovery and a caller-supplied dynamic
// client, so tests can drive time-varying Skyhook status.
func newDeploymentTestContextWithDynamic(
	t *testing.T,
	kubeObjects []runtime.Object,
	dynClient dynamic.Interface,
	refs []recipe.ComponentRef,
) *validators.Context {

	t.Helper()

	clientset := k8sfake.NewClientset(kubeObjects...)
	configureFakeDiscovery(t, clientset, nil, []schema.GroupVersion{nodewrightGVR.GroupVersion()}, nil)

	rec := &recipe.RecipeResult{ComponentRefs: refs}

	return &validators.Context{
		Ctx:             context.Background(),
		Clientset:       clientset,
		DynamicClient:   dynClient,
		ValidationInput: v1.ToValidationInput(rec),
	}
}

// flippingDynamicClient returns a Skyhook CR whose status.status advances
// through a scripted sequence on successive Get calls (the last entry repeats),
// simulating the complete→in_progress→complete flaps a reboot introduces. It is
// safe for concurrent use.
type flippingDynamicClient struct {
	mu       sync.Mutex
	calls    int
	name     string
	statuses []string
}

func newFlippingDynamicClient(name string, statuses []string) *flippingDynamicClient {
	return &flippingDynamicClient{name: name, statuses: statuses}
}

func (f *flippingDynamicClient) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *flippingDynamicClient) next() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.calls
	f.calls++
	if idx >= len(f.statuses) {
		idx = len(f.statuses) - 1
	}
	return f.statuses[idx]
}

func (f *flippingDynamicClient) Resource(schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	// Embed a fakeResourceClient (from the shared test helpers) for its panic
	// implementations of the rest of the interface; only Get is overridden.
	return &flippingResourceClient{
		fakeResourceClient: &fakeResourceClient{resource: nodewrightGVR},
		parent:             f,
	}
}

type flippingResourceClient struct {
	*fakeResourceClient
	parent *flippingDynamicClient
}

func (f *flippingResourceClient) Get(_ context.Context, name string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	return nodewrightWithStatus(name, f.parent.next()), nil
}
