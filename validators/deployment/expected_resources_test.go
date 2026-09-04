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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// testDefaultDRADSName is the kubelet-plugin DaemonSet name the upstream
// nvidia-dra-driver-gpu chart renders when no fullname override is in play.
// Tests use it as a sane default fixture name, and a separate test exercises
// an intentionally-different name to prove the role-suffix discovery works.
const testDefaultDRADSName = "nvidia-dra-driver-gpu-kubelet-plugin"

// testNodewrightManifest is the path of the Nodewright manifest the AICR embedded
// data provider ships for the eks/h100/inference recipe used in most tests.
// Declaring it once here keeps test setup aligned with the recipe defaults.
const testNodewrightManifest = "components/nodewright-customizations/manifests/tuning.yaml"

// testAICRCreatedBy{Key,Value} mirror the label convention AICR manifests
// apply to synthesized fixtures that should look like real production objects.
const (
	testAICRCreatedByLabelKey   = "app.kubernetes.io/created-by"
	testAICRCreatedByLabelValue = "aicr"
)

func TestCheckExpectedResources_IncludesDeploymentCompletenessAndGPUReadiness(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("skyhook"),
			activeNamespace("nvidia-dra-driver"),
			activeNamespace("app-ns"),
			readyDeployment("app-ns", "app-deployment", 1),
			readyDaemonSet("nvidia-dra-driver", testDefaultDRADSName, 2),
		},
		[]runtime.Object{
			nodewrightWithStatus("tuning", nodewrightCompleteState),
		},
		[]recipe.ComponentRef{
			{Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest}},
			{Name: draDriverComponent, Namespace: "nvidia-dra-driver"},
			{
				Name:      "app-component",
				Namespace: "app-ns",
				ExpectedResources: []recipe.ExpectedResource{
					{Kind: "Deployment", Namespace: "app-ns", Name: "app-deployment"},
				},
			},
		},
	)

	if err := checkExpectedResources(ctx); err != nil {
		t.Fatalf("checkExpectedResources() error = %v, want nil", err)
		return
	}
}

func TestCheckExpectedResources_FailsWhenNodewrightIncomplete(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("skyhook"),
			activeNamespace("nvidia-dra-driver"),
			readyDaemonSet("nvidia-dra-driver", testDefaultDRADSName, 1),
		},
		[]runtime.Object{
			nodewrightWithStatus("tuning", "waiting"),
		},
		[]recipe.ComponentRef{
			{Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest}},
			{Name: draDriverComponent, Namespace: "nvidia-dra-driver"},
		},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when Nodewright is not complete")
		return
	}
	if !strings.Contains(err.Error(), "Nodewright tuning: status=waiting") {
		t.Fatalf("expected Nodewright readiness failure, got: %v", err)
		return
	}
}

func TestCheckExpectedResources_FailsWhenNamespaceNotActive(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			inactiveNamespace("app-ns"),
		},
		nil,
		[]recipe.ComponentRef{
			{Name: "app-component", Namespace: "app-ns"},
		},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when namespace is not Active")
		return
	}
	if !strings.Contains(err.Error(), "namespace app-ns: phase=Terminating") {
		t.Fatalf("expected namespace readiness failure, got: %v", err)
		return
	}
}

func TestCheckExpectedResources_SkipsDisabledComponents(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("app-ns"),
			readyDeployment("app-ns", "app-deployment", 1),
		},
		nil,
		[]recipe.ComponentRef{
			{
				Name:      nodewrightCustomizationsComponent,
				Namespace: "skyhook",
				Overrides: map[string]any{"enabled": false},
			},
			{
				Name:      draDriverComponent,
				Namespace: "nvidia-dra-driver",
				Overrides: map[string]any{"enabled": false},
			},
			{
				Name:      "app-component",
				Namespace: "app-ns",
				ExpectedResources: []recipe.ExpectedResource{
					{Kind: "Deployment", Namespace: "app-ns", Name: "app-deployment"},
				},
			},
		},
	)

	if err := checkExpectedResources(ctx); err != nil {
		t.Fatalf("checkExpectedResources() error = %v, want nil for disabled optional components", err)
		return
	}
}

// Regression test: Nodewright is a cluster-scoped CR. The validator must list it
// without a namespace; otherwise the API server returns 404 even when the
// resource exists on a real cluster.
func TestVerifyNodewrightReady_ListsClusterScoped(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{activeNamespace("skyhook")},
		[]runtime.Object{nodewrightWithStatus("tuning", nodewrightCompleteState)},
		[]recipe.ComponentRef{{Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest}}},
	)

	if err := checkExpectedResources(ctx); err != nil {
		t.Fatalf("checkExpectedResources() error = %v, want nil for cluster-scoped Nodewright", err)
		return
	}
}

// Issue #607 acceptance: Nodewright check must skip gracefully when the CRD is
// not registered on the cluster, even when nodewright-customizations is declared
// in the recipe's componentRefs.
func TestCheckExpectedResources_SkipsNodewrightWhenCRDNotRegistered(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContextWithUnregistered(t,
		[]runtime.Object{activeNamespace("skyhook")},
		nil,
		[]schema.GroupVersionResource{nodewrightGVR},
		[]recipe.ComponentRef{{Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest}}},
	)

	if err := checkExpectedResources(ctx); err != nil {
		t.Fatalf("checkExpectedResources() error = %v, want nil when Nodewright CRD is not registered", err)
		return
	}
}

// When the Nodewright CRD is registered but the specific CR declared by the
// recipe is absent, verifyNodewrightReady should take the explicit IsNotFound
// branch and surface the recipe-scoped "declared but missing" diagnostic.
func TestCheckExpectedResources_FailsWhenNodewrightCRMissing(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContextWithDiscovery(t,
		[]runtime.Object{activeNamespace("skyhook")},
		nil,
		[]schema.GroupVersion{nodewrightGVR.GroupVersion()},
		nil,
		[]recipe.ComponentRef{{Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest}}},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when Nodewright CR is missing but CRD is registered")
		return
	}
	if !strings.Contains(err.Error(), "Nodewright tuning: not found (recipe declared it but the cluster has no such CR)") {
		t.Fatalf("expected recipe-scoped Nodewright not-found failure, got: %v", err)
		return
	}
}

// Fail-closed test: when the discovery API itself returns a non-NotFound
// error (e.g., 403 from RBAC, 5xx from an overloaded API server, network
// timeout), a Go-resident readiness check must NOT treat that as "CRD not
// registered" and skip. Anything other than IsNotFound means we cannot
// prove readiness, so the check must surface a failure. Exercised here via
// the Nodewright discovery gate, which shares the fail-closed pattern with
// the other GPU readiness signals.
func TestCheckExpectedResources_FailsWhenDiscoveryReturnsNonNotFoundError(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContextWithDiscovery(t,
		[]runtime.Object{activeNamespace("skyhook")},
		nil,
		nil,
		nil,
		[]recipe.ComponentRef{{Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest}}},
	)

	clientset, ok := ctx.Clientset.(*k8sfake.Clientset)
	if !ok {
		t.Fatalf("expected *k8sfake.Clientset, got %T", ctx.Clientset)
		return
	}
	// Resource name is the literal string "resource" (not "apiresources") —
	// that is the string FakeDiscovery hard-codes when synthesizing the
	// testing.Action for ServerResourcesForGroupVersion. See
	// vendor/k8s.io/client-go/discovery/fake/discovery.go: the action is built
	// with schema.GroupVersionResource{Resource: "resource"}. A tighter-looking
	// "apiresources" would not match anything, the reactor would not fire, and
	// the test would fall through to the default 404 path (IsNotFound) instead
	// of exercising the fail-closed branch.
	clientset.PrependReactor("get", "resource", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "skyhook.nvidia.com", Resource: "apiresources"},
			"",
			stderrors.New("forbidden: user cannot list apiresources"))
	})

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when discovery returns a non-NotFound error (fail-closed)")
		return
	}
	if !strings.Contains(err.Error(), "failed to discover") {
		t.Fatalf("expected discovery failure to surface, got: %v", err)
		return
	}
	if strings.Contains(err.Error(), "not registered, skipping") {
		t.Fatalf("discovery failure must not be treated as CRD-not-registered skip, got: %v", err)
		return
	}
}

// TestCheckExpectedResources_IgnoresStaleUnrelatedNodewright pins the fix for
// Codex review comment #2: an unrelated Nodewright CR left on the cluster from
// a prior deploy (or from a different tenant) must NOT influence this
// recipe's readiness result. The check is scoped to the Nodewright name(s) the
// recipe itself declares via ComponentRef.ManifestFiles.
func TestCheckExpectedResources_IgnoresStaleUnrelatedNodewright(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("skyhook"),
		},
		[]runtime.Object{
			// The recipe's manifestFiles point at tuning.yaml → expected name "tuning".
			nodewrightWithStatus("tuning", nodewrightCompleteState),
			// A stale "no-op" Nodewright lingering on the cluster in waiting state
			// (simulating a partially-cleaned previous deploy). It happens to
			// carry the AICR label — under the pre-fix implementation this would
			// have failed the check.
			nodewrightWithStatus("no-op", "waiting"),
		},
		[]recipe.ComponentRef{
			{Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest}},
		},
	)

	if err := checkExpectedResources(ctx); err != nil {
		t.Fatalf("checkExpectedResources() error = %v, want nil — stale unrelated Nodewright must not affect the result", err)
		return
	}
}

// TestCheckExpectedResources_FailsWhenRuntimeRequiredTaintPresent proves the
// readiness gate stays closed while a GPU node still carries the nodewright
// runtime-required NoSchedule taint, even though every expected Skyhook CR
// already reports status.status == "complete". This is the issue #1775 race:
// status.status momentarily reads "complete" in the lull between two package
// reboots, but the durable taint is still present because tuning is not truly
// done on the node.
func TestCheckExpectedResources_FailsWhenRuntimeRequiredTaintPresent(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("skyhook"),
			nodeWithRuntimeRequiredTaint("gpu-node-0"),
		},
		[]runtime.Object{
			nodewrightWithStatus("tuning", nodewrightCompleteState),
		},
		[]recipe.ComponentRef{
			{Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest}},
		},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error while a node still carries the runtime-required taint")
		return
	}
	if !strings.Contains(err.Error(), "node gpu-node-0: still carries the runtime-required taint") {
		t.Fatalf("expected runtime-required taint failure, got: %v", err)
		return
	}
}

// TestCheckExpectedResources_PassesWhenRuntimeRequiredTaintCleared proves the
// gate opens once the taint is removed from every node — including nodes that
// carry only unrelated taints, which must not gate.
func TestCheckExpectedResources_PassesWhenRuntimeRequiredTaintCleared(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("skyhook"),
			// A GPU node whose tuning finished (taint removed) but still carries
			// the workload dedication taint it was provisioned with.
			nodeWithTaints("gpu-node-0", corev1.Taint{
				Key: "dedicated", Value: "user-workload", Effect: corev1.TaintEffectNoSchedule,
			}),
			// A control-plane node with the standard master taint.
			nodeWithTaints("control-plane-0", corev1.Taint{
				Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule,
			}),
		},
		[]runtime.Object{
			nodewrightWithStatus("tuning", nodewrightCompleteState),
		},
		[]recipe.ComponentRef{
			{Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest}},
		},
	)

	if err := checkExpectedResources(ctx); err != nil {
		t.Fatalf("checkExpectedResources() error = %v, want nil once the runtime-required taint is cleared", err)
		return
	}
}

// TestCheckExpectedResources_FailsWhenNoExpectedNodewrightNames pins the
// fail-closed behavior when an enabled nodewright-customizations ref declares
// no manifest files. Rather than silently pass, the check must surface this as a
// recipe misconfiguration. (Contrast with #1844: manifests that ARE declared but
// render zero Skyhook CRs due to a value gate pass — see
// TestVerifyNodewrightReady_TolerantWhenAllCRsSuppressed — because the render
// reflects the effective values, so absence there is deliberate, not missing.)
func TestCheckExpectedResources_FailsWhenNoExpectedNodewrightNames(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("skyhook"),
		},
		nil,
		[]recipe.ComponentRef{
			// Intentionally no ManifestFiles — simulates a misconfigured recipe.
			{Name: nodewrightCustomizationsComponent, Namespace: "skyhook"},
		},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when enabled nodewright-customizations ref has no expected Nodewright names")
		return
	}
	if !strings.Contains(err.Error(), "no Nodewright CR names could be extracted") {
		t.Fatalf("expected 'no Nodewright CR names could be extracted' failure, got: %v", err)
		return
	}
}

func TestCheckExpectedResources_FailsWhenDRAKubeletPluginMissing(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("nvidia-dra-driver"),
		},
		nil,
		[]recipe.ComponentRef{
			{Name: draDriverComponent, Namespace: "nvidia-dra-driver"},
		},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when DRA kubelet plugin DaemonSet is missing")
		return
	}
	if !strings.Contains(err.Error(), "no kubelet-plugin DaemonSet") {
		t.Fatalf("expected DRA missing DaemonSet failure, got: %v", err)
		return
	}
}

func TestCheckExpectedResources_FailsWhenDRAKubeletPluginIsUnhealthy(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("nvidia-dra-driver"),
			unreadyDaemonSet("nvidia-dra-driver", testDefaultDRADSName, 2, 1),
		},
		nil,
		[]recipe.ComponentRef{
			{Name: draDriverComponent, Namespace: "nvidia-dra-driver"},
		},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when DRA kubelet plugin DaemonSet is unhealthy")
		return
	}
	if !strings.Contains(err.Error(), "DaemonSet nvidia-dra-driver/"+testDefaultDRADSName) {
		t.Fatalf("expected DRA DaemonSet context in failure, got: %v", err)
		return
	}
	if !strings.Contains(err.Error(), "not healthy: 1/2 pods ready") {
		t.Fatalf("expected unhealthy DaemonSet detail, got: %v", err)
		return
	}
}

func TestCheckExpectedResources_FailsWhenDRAKubeletPluginHasNoScheduledPods(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("nvidia-dra-driver"),
			unreadyDaemonSet("nvidia-dra-driver", testDefaultDRADSName, 0, 0),
		},
		nil,
		[]recipe.ComponentRef{
			{Name: draDriverComponent, Namespace: "nvidia-dra-driver"},
		},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when DRA kubelet plugin DaemonSet has no scheduled pods")
		return
	}
	if !strings.Contains(err.Error(), "no ready kubelet-plugin pods scheduled (0/0 pods ready)") {
		t.Fatalf("expected zero-pod DaemonSet detail, got: %v", err)
		return
	}
}

// TestCheckExpectedResources_DRAKubeletPluginCustomName pins the fix for
// Codex review comment #1: the check must locate the kubelet-plugin
// DaemonSet by its chart-template role suffix ("-kubelet-plugin"), not by
// its hard-coded default name. This lets it find the DaemonSet even when a
// user overrides fullnameOverride (or the chart renders a different
// fullname for any reason).
func TestCheckExpectedResources_DRAKubeletPluginCustomName(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("nvidia-dra-driver"),
			// DaemonSet named after a custom fullnameOverride; still ends in
			// the upstream chart's hard-coded role suffix.
			readyDaemonSet("nvidia-dra-driver", "my-custom-gpu-kubelet-plugin", 2),
		},
		nil,
		[]recipe.ComponentRef{
			{Name: draDriverComponent, Namespace: "nvidia-dra-driver"},
		},
	)

	if err := checkExpectedResources(ctx); err != nil {
		t.Fatalf("checkExpectedResources() error = %v, want nil for custom-named kubelet-plugin DaemonSet", err)
		return
	}
}

// TestCheckExpectedResources_FailsWhenMultipleKubeletPluginDaemonSets pins
// the ambiguity guard: two DaemonSets in the same namespace both ending in
// the role suffix must produce an explicit failure listing their names,
// rather than silently picking one.
func TestCheckExpectedResources_FailsWhenMultipleKubeletPluginDaemonSets(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("nvidia-dra-driver"),
			readyDaemonSet("nvidia-dra-driver", "alpha-kubelet-plugin", 2),
			readyDaemonSet("nvidia-dra-driver", "beta-kubelet-plugin", 2),
		},
		nil,
		[]recipe.ComponentRef{
			{Name: draDriverComponent, Namespace: "nvidia-dra-driver"},
		},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when multiple kubelet-plugin DaemonSets match")
		return
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity failure, got: %v", err)
		return
	}
	for _, name := range []string{"alpha-kubelet-plugin", "beta-kubelet-plugin"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("expected matched DaemonSet name %q in failure, got: %v", name, err)
			return
		}
	}
}

// TestCheckExpectedResources_IgnoresUnrelatedDaemonSetInNamespace pins the
// scoping guarantee: DaemonSets in the same namespace that don't match the
// kubelet-plugin role suffix must be ignored entirely.
func TestCheckExpectedResources_IgnoresUnrelatedDaemonSetInNamespace(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("nvidia-dra-driver"),
			// An unrelated DaemonSet (e.g. monitoring agent) sharing the
			// namespace — must not interfere.
			unreadyDaemonSet("nvidia-dra-driver", "node-exporter", 3, 0),
			// The real kubelet-plugin, healthy.
			readyDaemonSet("nvidia-dra-driver", testDefaultDRADSName, 2),
		},
		nil,
		[]recipe.ComponentRef{
			{Name: draDriverComponent, Namespace: "nvidia-dra-driver"},
		},
	)

	if err := checkExpectedResources(ctx); err != nil {
		t.Fatalf("checkExpectedResources() error = %v, want nil — unrelated DaemonSet must be ignored", err)
		return
	}
}

// TestCheckExpectedResources_NodewrightLiveness pins that readiness keys on the
// declared CR being live, not merely on some Skyhook reporting complete.
//
// The component health check's assert is deliberately name-agnostic, so a stale
// or unrelated live complete Skyhook satisfies it on its own. Only this
// per-name check binds liveness to the CR the recipe actually declared, so a
// Terminating CR must fail even while it still reports complete — Nodewright
// uses a deletion finalizer, so that state persists. Passing there would be a
// false PASS on state about to disappear, the same direction the Chainsaw
// executor guards by skipping ghosts on positive assertions (#2041).
//
// The live row is the control: without it a gate that rejected everything would
// still satisfy the terminating row.
func TestCheckExpectedResources_NodewrightLiveness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		nodewrights []runtime.Object
		wantErr     bool
		wantNeedles []string
	}{
		{
			name: "terminating declared CR fails despite a stale live complete CR",
			nodewrights: []runtime.Object{
				nodewrightTerminatingWithStatus("no-op", "complete"),
				nodewrightWithStatus("some-other-skyhook", "complete"),
			},
			wantErr:     true,
			wantNeedles: []string{"no-op", "terminating"},
		},
		{
			name:        "live complete declared CR passes",
			nodewrights: []runtime.Object{nodewrightWithStatus("no-op", "complete")},
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := newDeploymentTestContext(t,
				[]runtime.Object{activeNamespace("skyhook")},
				tt.nodewrights,
				[]recipe.ComponentRef{
					{
						Name:      nodewrightCustomizationsComponent,
						Namespace: "skyhook",
						ManifestFiles: []string{
							"components/nodewright-customizations/manifests/no-op.yaml",
						},
					},
				},
			)

			err := checkExpectedResources(ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkExpectedResources() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			for _, needle := range tt.wantNeedles {
				if !strings.Contains(err.Error(), needle) {
					t.Fatalf("expected %q in failure, got: %v", needle, err)
					return
				}
			}
		})
	}
}

// TestCheckExpectedResources_SurfacesMultipleNodewrightFailures pins Codex's
// non-blocking observation #1: when a recipe declares multiple Nodewright CRs
// and several are non-complete, all failures must surface in the error so
// the user can diagnose the whole state, not just the first issue.
func TestCheckExpectedResources_SurfacesMultipleNodewrightFailures(t *testing.T) {
	t.Parallel()

	// Use a synthetic recipe ref whose ManifestFiles point at the two real
	// manifests that declare different names. tuning.yaml yields "tuning";
	// no-op.yaml yields "no-op". The check must report both failures.
	ctx := newDeploymentTestContext(t,
		[]runtime.Object{activeNamespace("skyhook")},
		[]runtime.Object{
			nodewrightWithStatus("tuning", "waiting"),
			nodewrightWithStatus("no-op", "erroring"),
		},
		[]recipe.ComponentRef{
			{
				Name:      nodewrightCustomizationsComponent,
				Namespace: "skyhook",
				ManifestFiles: []string{
					"components/nodewright-customizations/manifests/tuning.yaml",
					"components/nodewright-customizations/manifests/no-op.yaml",
				},
			},
		},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when multiple expected Nodewrights are non-complete")
		return
	}
	for _, needle := range []string{
		"Nodewright tuning: status=waiting",
		"Nodewright no-op: status=erroring",
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("expected %q in failure, got: %v", needle, err)
			return
		}
	}
}

// TestExtractNodewrightNamesFromManifest exercises the narrow manifest parser
// directly. The most important case is Codex's: tuning-gke.yaml's filename
// suggests "tuning-gke" but the actual metadata.name is "tuning".
func TestExtractNodewrightNamesFromManifest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content []byte
		want    []string
	}{
		{
			name: "simple single-document manifest",
			content: []byte(`---
apiVersion: skyhook.nvidia.com/v1alpha1
kind: Skyhook
metadata:
  name: tuning
spec:
  runtimeRequired: true
`),
			want: []string{"tuning"},
		},
		{
			name: "multi-document manifest — both Skyhook names captured",
			content: []byte(`---
apiVersion: skyhook.nvidia.com/v1alpha1
kind: Skyhook
metadata:
  name: first
---
apiVersion: skyhook.nvidia.com/v1alpha1
kind: Skyhook
metadata:
  name: second
`),
			want: []string{"first", "second"},
		},
		{
			name: "mixed kinds — non-Skyhook documents ignored",
			content: []byte(`---
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-cm
---
apiVersion: skyhook.nvidia.com/v1alpha1
kind: Skyhook
metadata:
  name: tuning
`),
			want: []string{"tuning"},
		},
		{
			name: "Helm template preamble — not-valid-YAML lines do not break extraction",
			content: []byte(`{{- $cust := index .Values "nodewright-customizations" }}
{{- if ne (toString (index $cust "enabled")) "false" }}
---
apiVersion: skyhook.nvidia.com/v1alpha1
kind: Skyhook
metadata:
  annotations:
    "helm.sh/hook": post-install,post-upgrade
  labels:
    app.kubernetes.io/part-of: nodewright-operator
  name: tuning
  namespace: {{ .Release.Namespace }}
spec:
  runtimeRequired: true
  additionalTolerations:
    {{- if $cust.acceleratedTolerations }}
    {{- toYaml $cust.acceleratedTolerations | nindent 4 }}
    {{- end }}
{{- end }}
`),
			want: []string{"tuning"},
		},
		{
			name:    "empty content",
			content: []byte(""),
			want:    nil,
		},
		{
			name: "templated name — skipped (validator cannot evaluate Helm at validate time)",
			content: []byte(`---
apiVersion: skyhook.nvidia.com/v1alpha1
kind: Skyhook
metadata:
  name: {{ .Chart.Name }}
`),
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractNodewrightNamesFromManifest(tc.content)
			if !stringSlicesEqual(got, tc.want) {
				t.Fatalf("extractNodewrightNamesFromManifest(...) = %v, want %v", got, tc.want)
				return
			}
		})
	}
}

// TestExtractNodewrightNamesFromManifest_TuningGke is the regression test for
// Codex's explicit ask: tuning-gke.yaml's metadata.name is "tuning", not
// "tuning-gke". A basename-derived heuristic would get this wrong.
func TestExtractNodewrightNamesFromManifest_TuningGke(t *testing.T) {
	t.Parallel()

	content, err := recipe.GetManifestContent("components/nodewright-customizations/manifests/tuning-gke.yaml")
	if err != nil {
		t.Fatalf("failed to load tuning-gke manifest: %v", err)
		return
	}

	got := extractNodewrightNamesFromManifest(content)
	want := []string{"tuning"}
	if !stringSlicesEqual(got, want) {
		t.Fatalf("extractNodewrightNamesFromManifest(tuning-gke.yaml) = %v, want %v (metadata.name is 'tuning', not the filename basename)", got, want)
		return
	}
}

// TestExpectedNodewrightNames_RenderAware pins the value-aware behavior added
// for #1844: expectedNodewrightNames renders each manifest with the component's
// effective values, so a CR gated off by those values drops out of the
// extracted set. The `enabled: false` override exercises the same value-gate
// mechanism the tuningEnabled=false gate will use on the single-package tuning
// manifests — the whole Skyhook CR is suppressed, and the check tolerates its
// absence rather than asserting a CR that was deliberately not rendered.
func TestExpectedNodewrightNames_RenderAware(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		manifests []string
		overrides map[string]any
		want      []string
	}{
		{
			name:      "tuning.yaml, default values → CR expected",
			manifests: []string{"components/nodewright-customizations/manifests/tuning.yaml"},
			overrides: nil,
			want:      []string{"tuning"},
		},
		{
			name:      "single-package tuning-generic.yaml, default values → CR expected",
			manifests: []string{"components/nodewright-customizations/manifests/tuning-generic.yaml"},
			overrides: nil,
			want:      []string{"tuning"},
		},
		{
			name:      "value gate suppresses the whole CR → nothing expected",
			manifests: []string{"components/nodewright-customizations/manifests/tuning-generic.yaml"},
			overrides: map[string]any{"enabled": false},
			want:      nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ref := recipe.ComponentRef{
				Name:          nodewrightCustomizationsComponent,
				Namespace:     "skyhook",
				ManifestFiles: tc.manifests,
				Overrides:     tc.overrides,
			}
			got, err := expectedNodewrightNames(ref)
			if err != nil {
				t.Fatalf("expectedNodewrightNames() error = %v, want nil", err)
				return
			}
			if !stringSlicesEqual(got, tc.want) {
				t.Fatalf("expectedNodewrightNames() = %v, want %v", got, tc.want)
				return
			}
		})
	}
}

// TestNodewrightHealthCheckSuppressed pins the gate that skips the static
// chainsaw health-check assert when the component's effective values suppress
// the tuning Skyhook CR (#1844). Only the nodewright-customizations component is
// subject to it; a component with a renderable CR or no manifests keeps its
// assert; a render-empty component with manifests declared is suppressed.
func TestNodewrightHealthCheckSuppressed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  recipe.ComponentRef
		want bool
	}{
		{
			name: "other component is never suppressed",
			ref: recipe.ComponentRef{
				Name:          "gpu-operator",
				ManifestFiles: []string{"components/nodewright-customizations/manifests/tuning-generic.yaml"},
				Overrides:     map[string]any{"enabled": false},
			},
			want: false,
		},
		{
			name: "nodewright with a renderable CR keeps its assert",
			ref: recipe.ComponentRef{
				Name:          nodewrightCustomizationsComponent,
				ManifestFiles: []string{"components/nodewright-customizations/manifests/tuning-generic.yaml"},
			},
			want: false,
		},
		{
			name: "nodewright with all CRs gated off is suppressed",
			ref: recipe.ComponentRef{
				Name:          nodewrightCustomizationsComponent,
				ManifestFiles: []string{"components/nodewright-customizations/manifests/tuning-generic.yaml"},
				Overrides:     map[string]any{"enabled": false},
			},
			want: true,
		},
		{
			name: "nodewright with no manifests keeps its assert (misconfig surfaces elsewhere)",
			ref: recipe.ComponentRef{
				Name: nodewrightCustomizationsComponent,
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := nodewrightHealthCheckSuppressed(tc.ref)
			if err != nil {
				t.Fatalf("nodewrightHealthCheckSuppressed() error = %v, want nil", err)
				return
			}
			if got != tc.want {
				t.Fatalf("nodewrightHealthCheckSuppressed() = %v, want %v", got, tc.want)
				return
			}
		})
	}
}

// TestVerifyNodewrightReady_TolerantWhenAllCRsSuppressed pins the fail-closed
// distinction added for #1844: when manifests are declared but the effective
// values render zero Skyhook CRs, readiness passes (nothing to verify) — the CR
// was deliberately suppressed. This is checked before the CRD-discovery gate, so
// no cluster state is required. Contrast with the no-manifests case, which still
// fails closed as a misconfiguration (TestCheckExpectedResources_FailsWhenNoExpectedNodewrightNames).
func TestVerifyNodewrightReady_TolerantWhenAllCRsSuppressed(t *testing.T) {
	t.Parallel()

	// A ref that keeps the component enabled for the readiness check but whose
	// manifest gate renders the whole Skyhook CR away. `enabled: false` drives
	// the same value gate the tuningEnabled=false path uses; calling
	// verifyNodewrightReady directly bypasses the upstream IsEnabled filter so we
	// exercise the render-empty branch itself.
	ref := recipe.ComponentRef{
		Name:          nodewrightCustomizationsComponent,
		Namespace:     "skyhook",
		ManifestFiles: []string{"components/nodewright-customizations/manifests/tuning-generic.yaml"},
		Overrides:     map[string]any{"enabled": false},
	}
	ctx := newDeploymentTestContext(t, []runtime.Object{activeNamespace("skyhook")}, nil,
		[]recipe.ComponentRef{ref})

	if err := verifyNodewrightReady(ctx, ref); err != nil {
		t.Fatalf("verifyNodewrightReady() error = %v, want nil (all CRs suppressed by effective values)", err)
	}
}

// TestIsRuntimeRequiredTaint pins the matcher: only the exact key+value with the
// NoSchedule effect gates. A taint that shares the key but differs in value or
// effect must not be mistaken for an in-flight tuning (or the gate would block
// forever on an unrelated taint).
func TestIsRuntimeRequiredTaint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		taint corev1.Taint
		want  bool
	}{
		{
			name:  "exact runtime-required NoSchedule",
			taint: corev1.Taint{Key: runtimeRequiredTaintKey, Value: runtimeRequiredTaintValue, Effect: corev1.TaintEffectNoSchedule},
			want:  true,
		},
		{
			name:  "right key+value but NoExecute effect",
			taint: corev1.Taint{Key: runtimeRequiredTaintKey, Value: runtimeRequiredTaintValue, Effect: corev1.TaintEffectNoExecute},
			want:  false,
		},
		{
			name:  "right key but different value",
			taint: corev1.Taint{Key: runtimeRequiredTaintKey, Value: "something-else", Effect: corev1.TaintEffectNoSchedule},
			want:  false,
		},
		{
			name:  "unrelated dedication taint",
			taint: corev1.Taint{Key: "dedicated", Value: "user-workload", Effect: corev1.TaintEffectNoSchedule},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isRuntimeRequiredTaint(&tt.taint); got != tt.want {
				t.Errorf("isRuntimeRequiredTaint(%+v) = %v, want %v", tt.taint, got, tt.want)
			}
		})
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newDeploymentTestContext(t *testing.T, kubeObjects, dynamicObjects []runtime.Object, refs []recipe.ComponentRef) *validators.Context {
	t.Helper()
	return newDeploymentTestContextWithUnregistered(t, kubeObjects, dynamicObjects, nil, refs)
}

// newDeploymentTestContextWithUnregistered builds a test context where the
// given GVRs are treated as unregistered on the cluster. List/Get against
// them return a meta.NoKindMatchError on the dynamic client, and the fake
// clientset's discovery service does not advertise their GroupVersion —
// mirroring what a real client sees when the CRD has not been installed.
// Used by CRD-missing skip tests.
//
// Discovery registration for the *present* GVRs is inferred automatically
// from dynamicObjects: every GVR that has at least one object in that slice
// has its GroupVersion advertised (unless it also appears in unregistered).
// Tests that need to advertise a GV without any objects should use
// newDeploymentTestContextWithDiscovery below.
func newDeploymentTestContextWithUnregistered(
	t *testing.T,
	kubeObjects, dynamicObjects []runtime.Object,
	unregistered []schema.GroupVersionResource,
	refs []recipe.ComponentRef,
) *validators.Context {

	t.Helper()
	return newDeploymentTestContextWithDiscovery(t, kubeObjects, dynamicObjects, nil, unregistered, refs)
}

// newDeploymentTestContextWithDiscovery is the fully-explicit variant: callers
// pass the exact list of GroupVersions the fake discovery service should
// advertise. Needed by the "CRD present but CR missing" test, where we need
// the Nodewright GroupVersion to appear in discovery without any Skyhook object.
func newDeploymentTestContextWithDiscovery(
	t *testing.T,
	kubeObjects, dynamicObjects []runtime.Object,
	extraRegistered []schema.GroupVersion,
	unregistered []schema.GroupVersionResource,
	refs []recipe.ComponentRef,
) *validators.Context {

	t.Helper()

	clientset := k8sfake.NewClientset(kubeObjects...)
	configureFakeDiscovery(t, clientset, dynamicObjects, extraRegistered, unregistered)
	dynClient := newFakeDynamicClient(dynamicObjects, unregistered...)

	rec := &recipe.RecipeResult{
		ComponentRefs: refs,
	}

	return &validators.Context{
		Ctx:             context.Background(),
		Clientset:       clientset,
		DynamicClient:   dynClient,
		ValidationInput: v1.ToValidationInput(rec),
	}
}

// configureFakeDiscovery wires the fake clientset's Discovery service so that
// ServerResourcesForGroupVersion returns a non-error result for the
// GroupVersions represented by dynamicObjects (minus any unregistered GVRs)
// plus any extraRegistered GVs that the test declares explicitly.
func configureFakeDiscovery(
	t *testing.T,
	clientset *k8sfake.Clientset,
	dynamicObjects []runtime.Object,
	extraRegistered []schema.GroupVersion,
	unregistered []schema.GroupVersionResource,
) {

	t.Helper()

	unregSet := make(map[schema.GroupVersion]bool, len(unregistered))
	for _, gvr := range unregistered {
		unregSet[gvr.GroupVersion()] = true
	}

	gvSet := make(map[schema.GroupVersion]bool)
	for _, object := range dynamicObjects {
		u, ok := object.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		gv := u.GroupVersionKind().GroupVersion()
		if unregSet[gv] {
			continue
		}
		gvSet[gv] = true
	}
	for _, gv := range extraRegistered {
		if unregSet[gv] {
			continue
		}
		gvSet[gv] = true
	}

	fakeDisc, ok := clientset.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatalf("expected *fakediscovery.FakeDiscovery, got %T", clientset.Discovery())
		return
	}
	for gv := range gvSet {
		fakeDisc.Resources = append(fakeDisc.Resources, &metav1.APIResourceList{
			GroupVersion: gv.String(),
		})
	}
}

type fakeDynamicClient struct {
	objects      map[schema.GroupVersionResource][]*unstructured.Unstructured
	unregistered map[schema.GroupVersionResource]bool
}

func newFakeDynamicClient(objects []runtime.Object, unregistered ...schema.GroupVersionResource) dynamic.Interface {
	store := make(map[schema.GroupVersionResource][]*unstructured.Unstructured)
	for _, object := range objects {
		item := object.(*unstructured.Unstructured)
		gvk := item.GroupVersionKind()
		gvr := gvrForTestObject(gvk)
		store[gvr] = append(store[gvr], item.DeepCopy())
	}
	unregSet := make(map[schema.GroupVersionResource]bool, len(unregistered))
	for _, gvr := range unregistered {
		unregSet[gvr] = true
	}
	return &fakeDynamicClient{objects: store, unregistered: unregSet}
}

func gvrForTestObject(gvk schema.GroupVersionKind) schema.GroupVersionResource {
	switch {
	case gvk.Group == nodewrightGVR.Group && gvk.Version == nodewrightGVR.Version && gvk.Kind == "Skyhook":
		return nodewrightGVR
	default:
		return schema.GroupVersionResource{
			Group:    gvk.Group,
			Version:  gvk.Version,
			Resource: strings.ToLower(gvk.Kind) + "s",
		}
	}
}

func (f *fakeDynamicClient) Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	if f.unregistered[resource] {
		return &fakeResourceClient{
			resource:     resource,
			unregistered: true,
		}
	}
	return &fakeResourceClient{
		resource: resource,
		objects:  f.objects[resource],
	}
}

// clusterScopedGVRs mirrors the API server's scope model so the fake fails
// loudly if production code calls .Namespace(x) on a cluster-scoped resource
// (which real k8s answers with a 404 "server could not find the requested
// resource", not a silently empty list).
var clusterScopedGVRs = map[schema.GroupVersionResource]bool{
	nodewrightGVR: true,
}

type fakeResourceClient struct {
	resource         schema.GroupVersionResource
	namespace        string
	objects          []*unstructured.Unstructured
	unregistered     bool
	invalidScopeCall bool
}

func (f *fakeResourceClient) Namespace(namespace string) dynamic.ResourceInterface {
	if f.unregistered {
		return f
	}
	if clusterScopedGVRs[f.resource] && namespace != "" {
		// Any op on this client returns a "not found" error, matching the real
		// API server's behavior for a namespaced request against a
		// cluster-scoped resource.
		return &fakeResourceClient{
			resource:         f.resource,
			invalidScopeCall: true,
		}
	}
	return &fakeResourceClient{
		resource:  f.resource,
		namespace: namespace,
		objects:   f.objects,
	}
}

func (f *fakeResourceClient) Create(context.Context, *unstructured.Unstructured, metav1.CreateOptions, ...string) (*unstructured.Unstructured, error) {
	panic("not implemented")
}

func (f *fakeResourceClient) Update(context.Context, *unstructured.Unstructured, metav1.UpdateOptions, ...string) (*unstructured.Unstructured, error) {
	panic("not implemented")
}

func (f *fakeResourceClient) UpdateStatus(context.Context, *unstructured.Unstructured, metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	panic("not implemented")
}

func (f *fakeResourceClient) Delete(context.Context, string, metav1.DeleteOptions, ...string) error {
	panic("not implemented")
}

func (f *fakeResourceClient) DeleteCollection(context.Context, metav1.DeleteOptions, metav1.ListOptions) error {
	panic("not implemented")
}

func (f *fakeResourceClient) noKindMatchError() error {
	return &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: f.resource.Group, Kind: f.resource.Resource},
		SearchedVersions: []string{f.resource.Version},
	}
}

func (f *fakeResourceClient) Get(_ context.Context, name string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	if f.unregistered {
		return nil, f.noKindMatchError()
	}
	if f.invalidScopeCall {
		return nil, stderrors.New("the server could not find the requested resource")
	}
	for _, object := range f.objects {
		if object.GetName() != name {
			continue
		}
		if f.namespace != "" && object.GetNamespace() != f.namespace {
			continue
		}
		return object.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(
		schema.GroupResource{Group: f.resource.Group, Resource: f.resource.Resource},
		name,
	)
}

func (f *fakeResourceClient) List(_ context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	if f.unregistered {
		return nil, f.noKindMatchError()
	}
	if f.invalidScopeCall {
		return nil, stderrors.New("the server could not find the requested resource")
	}
	list := &unstructured.UnstructuredList{
		Items: make([]unstructured.Unstructured, 0, len(f.objects)),
	}

	for _, object := range f.objects {
		if f.namespace != "" && object.GetNamespace() != f.namespace {
			continue
		}
		if opts.LabelSelector != "" && !matchesLabelSelector(object, opts.LabelSelector) {
			continue
		}
		list.Items = append(list.Items, *object.DeepCopy())
	}

	return list, nil
}

func (f *fakeResourceClient) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	panic("not implemented")
}

func (f *fakeResourceClient) Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions, ...string) (*unstructured.Unstructured, error) {
	panic("not implemented")
}

func (f *fakeResourceClient) Apply(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions, ...string) (*unstructured.Unstructured, error) {
	panic("not implemented")
}

func (f *fakeResourceClient) ApplyStatus(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	panic("not implemented")
}

func matchesLabelSelector(object *unstructured.Unstructured, selector string) bool {
	parts := strings.SplitN(selector, "=", 2)
	if len(parts) != 2 {
		return false
	}
	return object.GetLabels()[parts[0]] == parts[1]
}

func activeNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NamespaceStatus{
			Phase: corev1.NamespaceActive,
		},
	}
}

func inactiveNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NamespaceStatus{
			Phase: corev1.NamespaceTerminating,
		},
	}
}

func readyDeployment(namespace, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: replicas,
		},
	}
}

//nolint:unparam // namespace is a meaningful test input even if current call sites all happen to use the same namespace
func readyDaemonSet(namespace, name string, ready int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: ready,
			NumberReady:            ready,
		},
	}
}

func unreadyDaemonSet(namespace, name string, desired, ready int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: desired,
			NumberReady:            ready,
		},
	}
}

// nodeWithTaints builds a Node fixture carrying the given taints.
func nodeWithTaints(name string, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Taints: taints},
	}
}

// nodeWithRuntimeRequiredTaint builds a Node fixture carrying the nodewright
// runtime-required NoSchedule taint the readiness gate blocks on.
func nodeWithRuntimeRequiredTaint(name string) *corev1.Node {
	return nodeWithTaints(name, corev1.Taint{
		Key:    runtimeRequiredTaintKey,
		Value:  runtimeRequiredTaintValue,
		Effect: corev1.TaintEffectNoSchedule,
	})
}

// nodewrightWithStatus builds a Nodewright fixture. Nodewright is a cluster-scoped CR,
// so metadata.namespace is intentionally not set.
// nodewrightTerminatingWithStatus builds a Skyhook that is mid-deletion
// (deletionTimestamp set, as Nodewright's finalizer leaves it) while still
// reporting the given status. The combination is the trap: status.status alone
// says "ready" about a CR that is on its way out.
func nodewrightTerminatingWithStatus(name, status string) *unstructured.Unstructured {
	sk := nodewrightWithStatus(name, status)
	meta, _ := sk.Object["metadata"].(map[string]interface{})
	meta["deletionTimestamp"] = "2026-01-01T00:00:00Z"
	meta["finalizers"] = []interface{}{"skyhook.nvidia.com/finalizer"}
	return sk
}

func nodewrightWithStatus(name, status string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "skyhook.nvidia.com/v1alpha1",
			"kind":       "Skyhook",
			"metadata": map[string]any{
				"name": name,
				"labels": map[string]any{
					testAICRCreatedByLabelKey: testAICRCreatedByLabelValue,
				},
			},
			"status": map[string]any{
				"status": status,
			},
		},
	}
}

// TestRDMAFabricCoverage_Disclosure exercises the headline behavior of #1952 as a
// pure function of the partition: a cordoned Mellanox RDMA node must be listed
// "skipped (cordoned)", counted in nodesTotal, and never omitted — the same
// spuriously-narrowed-pass guard check-nvidia-smi got in #1668/#1936, applied to
// the RDMA fabric gate. It also pins the two zero-cordoned/zero-total phrasings.
func TestRDMAFabricCoverage_Disclosure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		cov              rdmaFabricCoverage
		validated        int
		wantTotal        int
		wantEnumeration  []string
		wantCoverageLine string
	}{
		{
			name:      "cordoned RDMA node is disclosed and counted",
			cov:       rdmaFabricCoverage{schedulable: 1, cordoned: []string{"rdma-drain-0"}},
			validated: 1,
			wantTotal: 2,
			wantEnumeration: []string{
				"Found 2 Mellanox RDMA-capable GPU node(s), 1 schedulable, 1 cordoned:",
				"  rdma-drain-0: skipped (cordoned)",
			},
			wantCoverageLine: "RESULT: nodesValidated: 1/2 (1 cordoned, skipped)",
		},
		{
			name:      "fail-closed exit reports zero validated but still counts cordoned",
			cov:       rdmaFabricCoverage{schedulable: 2, cordoned: []string{"rdma-drain-0", "rdma-drain-1"}},
			validated: 0,
			wantTotal: 4,
			wantEnumeration: []string{
				"Found 4 Mellanox RDMA-capable GPU node(s), 2 schedulable, 2 cordoned:",
				"  rdma-drain-0: skipped (cordoned)",
				"  rdma-drain-1: skipped (cordoned)",
			},
			wantCoverageLine: "RESULT: nodesValidated: 0/4 (2 cordoned, skipped)",
		},
		{
			name:             "no cordoned nodes omits the parenthetical",
			cov:              rdmaFabricCoverage{schedulable: 3},
			validated:        3,
			wantTotal:        3,
			wantEnumeration:  []string{"Found 3 Mellanox RDMA-capable GPU node(s), 3 schedulable, 0 cordoned:"},
			wantCoverageLine: "RESULT: nodesValidated: 3/3",
		},
		{
			name:             "zero total nodes gets a plain sentence",
			cov:              rdmaFabricCoverage{},
			validated:        0,
			wantTotal:        0,
			wantEnumeration:  []string{"Found 0 Mellanox RDMA-capable GPU node(s)."},
			wantCoverageLine: "RESULT: nodesValidated: 0/0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cov.total(); got != tt.wantTotal {
				t.Errorf("total() = %d, want %d", got, tt.wantTotal)
			}
			gotEnum := tt.cov.enumerationLines()
			if len(gotEnum) != len(tt.wantEnumeration) {
				t.Fatalf("enumerationLines() = %v, want %v", gotEnum, tt.wantEnumeration)
			}
			for i, want := range tt.wantEnumeration {
				if gotEnum[i] != want {
					t.Errorf("enumerationLines()[%d] = %q, want %q", i, gotEnum[i], want)
				}
			}
			if got := tt.cov.coverageLine(tt.validated); got != tt.wantCoverageLine {
				t.Errorf("coverageLine(%d) = %q, want %q", tt.validated, got, tt.wantCoverageLine)
			}
		})
	}
}

// TestRDMAFabricCoverageExtra proves the structured coverage disclosure carries
// exactly the two allowlisted count keys (nodesValidated/nodesTotal) as decimal
// strings and nothing else — no node names or IPs leak into the Extra channel.
func TestRDMAFabricCoverageExtra(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		validated     int
		total         int
		wantValidated string
		wantTotal     string
	}{
		{"full cohort, one cordoned excluded", 1, 2, "1", "2"},
		{"uniform cohort no cordoned", 2, 2, "2", "2"},
		{"fail-closed zero validated", 0, 3, "0", "3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			extra := rdmaFabricCoverageExtra(tt.validated, tt.total)
			if extra["nodesValidated"] != tt.wantValidated {
				t.Errorf("nodesValidated = %q, want %q", extra["nodesValidated"], tt.wantValidated)
			}
			if extra["nodesTotal"] != tt.wantTotal {
				t.Errorf("nodesTotal = %q, want %q", extra["nodesTotal"], tt.wantTotal)
			}
			if len(extra) != 2 {
				t.Errorf("coverage extra must carry exactly the two count keys, got %v", extra)
			}
		})
	}
}

// TestRDMAFabricProbeCoverage_DisclosesCordoned is the end-to-end proof of #1952:
// a cordoned Mellanox RDMA GPU node is enumerated (via helper.FindGpuNodes) and
// surfaced in the coverage partition — visible and counted — while still being
// excluded from the validated cohort. Under the pre-fix code path
// (FindSchedulableGpuNodes) the cordoned node vanished entirely, so this test
// fails without the production change.
func TestRDMAFabricProbeCoverage_DisclosesCordoned(t *testing.T) {
	t.Parallel()

	clientset := k8sfake.NewClientset(
		rdmaGPUNode("rdma-gpu-0", 8, 1000),         // schedulable, fabric ready → validated cohort
		cordon(rdmaGPUNode("rdma-drain-0", 8, -1)), // cordoned RDMA node → disclosed, not dropped
	)
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

	cov, err := rdmaFabricProbeCoverage(ctx, helper.AKSRdmaSharedResource)
	if err != nil {
		t.Fatalf("rdmaFabricProbeCoverage() error = %v, want nil (the one schedulable RDMA node carries the fabric)", err)
	}
	if cov.schedulable != 1 {
		t.Errorf("schedulable cohort = %d, want 1 (cordoned node excluded from validation)", cov.schedulable)
	}
	if len(cov.cordoned) != 1 || cov.cordoned[0] != "rdma-drain-0" {
		t.Errorf("cordoned = %v, want [rdma-drain-0] (must be disclosed, not silently dropped)", cov.cordoned)
	}
	if got := cov.total(); got != 2 {
		t.Errorf("total() = %d, want 2 (schedulable + cordoned, never narrowed)", got)
	}
}

// TestRDMAFabricProbeCoverage_CountsCordonedOnFailClosed proves the cordoned
// disclosure survives the fail-closed paths too: when the sole schedulable RDMA
// node has not finished rolling out the fabric, the probe returns an error AND
// still reports the cordoned node in the coverage so the terminal disclosure can
// name it.
func TestRDMAFabricProbeCoverage_CountsCordonedOnFailClosed(t *testing.T) {
	t.Parallel()

	clientset := k8sfake.NewClientset(
		rdmaGPUNode("rdma-gpu-0", 8, -1),           // schedulable but fabric absent → not ready
		cordon(rdmaGPUNode("rdma-drain-0", 8, -1)), // cordoned RDMA node → still disclosed
	)
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

	cov, err := rdmaFabricProbeCoverage(ctx, helper.AKSRdmaSharedResource)
	if err == nil {
		t.Fatal("expected a fail-closed error while the fabric is absent, got nil")
	}
	if !strings.Contains(err.Error(), "not yet allocatable") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cov.cordoned) != 1 || cov.cordoned[0] != "rdma-drain-0" {
		t.Errorf("cordoned = %v, want [rdma-drain-0] even on the fail-closed path", cov.cordoned)
	}
	if got := cov.total(); got != 2 {
		t.Errorf("total() = %d, want 2 (cordoned counted even on failure)", got)
	}
}

// TestGatedHealthCheckSuppressed pins the render-aware static-assert
// suppression dispatch for values-gated components: the gcp-driver-installer
// health check is skipped exactly when the effective values gate the render
// off, other components' asserts queue unconditionally, and render/read
// failures propagate rather than being read as "nothing to assert".
func TestGatedHealthCheckSuppressed(t *testing.T) {
	t.Parallel()

	installerManifest := "components/gcp-driver-installer/manifests/nvidia-driver-installer.yaml"
	tests := []struct {
		name           string
		ref            recipe.ComponentRef
		wantSuppressed bool
		wantErr        bool
	}{
		{
			name: "installer gated off (default values) suppresses the assert",
			ref: recipe.ComponentRef{
				Name:          "gcp-driver-installer",
				Type:          recipe.ComponentTypeHelm,
				ValuesFile:    "components/gcp-driver-installer/values.yaml",
				ManifestFiles: []string{installerManifest},
			},
			wantSuppressed: true,
		},
		{
			// A wholesale override can drop the gate key entirely; the
			// template must fail closed to not-rendering, never panic.
			name: "missing gate key renders nothing and suppresses",
			ref: recipe.ComponentRef{
				Name:          "gcp-driver-installer",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{installerManifest},
			},
			wantSuppressed: true,
		},
		{
			name: "installer gated on renders objects and keeps the assert",
			ref: recipe.ComponentRef{
				Name:          "gcp-driver-installer",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{installerManifest},
				Overrides: map[string]any{
					"installer": map[string]any{"enabled": true},
				},
			},
			wantSuppressed: false,
		},
		{
			name: "no manifests leaves the assert in place",
			ref: recipe.ComponentRef{
				Name: "gcp-driver-installer",
				Type: recipe.ComponentTypeHelm,
			},
			wantSuppressed: false,
		},
		{
			name: "unreadable manifest fails closed",
			ref: recipe.ComponentRef{
				Name:          "gcp-driver-installer",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{"components/gcp-driver-installer/manifests/no-such-file.yaml"},
			},
			wantErr: true,
		},
		{
			name: "non-gated component queues unconditionally",
			ref: recipe.ComponentRef{
				Name:          "gpu-operator",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{installerManifest},
			},
			wantSuppressed: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			suppressed, reason, err := gatedHealthCheckSuppressed(t.Context(), tt.ref)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("gatedHealthCheckSuppressed() error = nil, want failure")
				}
				return
			}
			if err != nil {
				t.Fatalf("gatedHealthCheckSuppressed() error = %v", err)
			}
			if suppressed != tt.wantSuppressed {
				t.Errorf("suppressed = %v, want %v", suppressed, tt.wantSuppressed)
			}
			if suppressed && reason == "" {
				t.Error("suppressed with an empty reason — operators need the why")
			}
		})
	}

	t.Run("canceled context stops manifest evaluation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := gatedHealthCheckSuppressed(ctx, recipe.ComponentRef{
			Name:          "gcp-driver-installer",
			Type:          recipe.ComponentTypeHelm,
			ManifestFiles: []string{installerManifest},
		})
		if err == nil {
			t.Fatal("gatedHealthCheckSuppressed() error = nil, want cancellation")
		}
	})
}
