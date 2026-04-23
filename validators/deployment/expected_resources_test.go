// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
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

// TestMain forces the global recipe data provider to initialize before any
// parallel t.Parallel() tests start. GetDataProvider() in pkg/recipe
// performs lazy, unsynchronized initialization of the package-level
// globalDataProvider — safe at normal runtime (the CLI initializes it on
// startup), but racy under parallel unit tests that each call
// recipe.GetManifestContent simultaneously. Calling it once here from the
// test goroutine serializes the init.
func TestMain(m *testing.M) {
	_ = recipe.GetDataProvider()
	os.Exit(m.Run())
}

func TestCheckExpectedResources_IncludesDeploymentCompletenessAndGPUReadiness(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("gpu-operator"),
			activeNamespace("skyhook"),
			activeNamespace("nvidia-dra-driver"),
			activeNamespace("app-ns"),
			readyDeployment("app-ns", "app-deployment", 1),
			readyDaemonSet("nvidia-dra-driver", testDefaultDRADSName, 2),
		},
		[]runtime.Object{
			clusterPolicyWithState(clusterPolicyReadyState),
			nodewrightWithStatus("tuning", nodewrightCompleteState),
		},
		[]recipe.ComponentRef{
			{Name: gpuOperatorComponent, Namespace: "gpu-operator"},
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
			activeNamespace("gpu-operator"),
			activeNamespace("skyhook"),
			activeNamespace("nvidia-dra-driver"),
			readyDaemonSet("nvidia-dra-driver", testDefaultDRADSName, 1),
		},
		[]runtime.Object{
			clusterPolicyWithState(clusterPolicyReadyState),
			nodewrightWithStatus("tuning", "waiting"),
		},
		[]recipe.ComponentRef{
			{Name: gpuOperatorComponent, Namespace: "gpu-operator"},
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

// Issue #607 acceptance (by symmetry): the gpu-operator readiness check must
// skip gracefully when the ClusterPolicy CRD is not registered, so a recipe
// can declare gpu-operator before the operator's CRDs are installed.
func TestCheckExpectedResources_SkipsClusterPolicyWhenCRDNotRegistered(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContextWithUnregistered(t,
		[]runtime.Object{activeNamespace("gpu-operator")},
		nil,
		[]schema.GroupVersionResource{clusterPolicyGVR},
		[]recipe.ComponentRef{{Name: gpuOperatorComponent, Namespace: "gpu-operator"}},
	)

	if err := checkExpectedResources(ctx); err != nil {
		t.Fatalf("checkExpectedResources() error = %v, want nil when ClusterPolicy CRD is not registered", err)
		return
	}
}

// Fail-closed test: when the discovery API itself returns a non-NotFound
// error (e.g., 403 from RBAC, 5xx from an overloaded API server, network
// timeout), the ClusterPolicy check must NOT treat that as "CRD not
// registered" and skip. Anything other than IsNotFound means we cannot
// prove readiness, so the check must surface a failure.
func TestCheckExpectedResources_FailsWhenDiscoveryReturnsNonNotFoundError(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContextWithDiscovery(t,
		[]runtime.Object{activeNamespace("gpu-operator")},
		nil,
		nil,
		nil,
		[]recipe.ComponentRef{{Name: gpuOperatorComponent, Namespace: "gpu-operator"}},
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
			schema.GroupResource{Group: "nvidia.com", Resource: "apiresources"},
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

// Disambiguation test: when the ClusterPolicy CRD *is* registered on the
// cluster but the cluster-policy CR itself is absent, the check must fail
// rather than skip. That state means gpu-operator is installed but has no
// singleton to reconcile — a real misconfiguration the user should see, not
// silently treat as "not applicable".
func TestCheckExpectedResources_FailsWhenClusterPolicyCRMissing(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContextWithDiscovery(t,
		[]runtime.Object{activeNamespace("gpu-operator")},
		nil,
		[]schema.GroupVersion{clusterPolicyGVR.GroupVersion()},
		nil,
		[]recipe.ComponentRef{{Name: gpuOperatorComponent, Namespace: "gpu-operator"}},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when ClusterPolicy CR is missing but CRD is registered")
		return
	}
	if !strings.Contains(err.Error(), "failed to get ClusterPolicy cluster-policy") {
		t.Fatalf("expected ClusterPolicy-missing failure, got: %v", err)
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

// TestCheckExpectedResources_FailsWhenNoExpectedNodewrightNames pins the
// fail-closed behavior when an enabled nodewright-customizations ref declares
// no manifest files (or the manifests contain no Nodewright CRs). Rather than
// silently pass, the check must surface this as a recipe misconfiguration.
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

func TestCheckExpectedResources_FailsWhenClusterPolicyMissingState(t *testing.T) {
	t.Parallel()

	ctx := newDeploymentTestContext(t,
		[]runtime.Object{
			activeNamespace("gpu-operator"),
		},
		[]runtime.Object{
			clusterPolicyWithoutState(),
		},
		[]recipe.ComponentRef{
			{Name: gpuOperatorComponent, Namespace: "gpu-operator"},
		},
	)

	err := checkExpectedResources(ctx)
	if err == nil {
		t.Fatal("expected error when ClusterPolicy status.state is missing")
		return
	}
	if !strings.Contains(err.Error(), "ClusterPolicy status.state not found") {
		t.Fatalf("expected ClusterPolicy readiness failure, got: %v", err)
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
// nvidia.com/v1 to appear in discovery without any ClusterPolicy object.
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

	return &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: dynClient,
		Recipe: &recipe.RecipeResult{
			ComponentRefs: refs,
		},
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
	case gvk.Group == clusterPolicyGVR.Group && gvk.Version == clusterPolicyGVR.Version && gvk.Kind == "ClusterPolicy":
		return clusterPolicyGVR
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
	clusterPolicyGVR: true,
	nodewrightGVR:    true,
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

func clusterPolicyWithState(state string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "nvidia.com/v1",
			"kind":       "ClusterPolicy",
			"metadata": map[string]interface{}{
				"name": clusterPolicyName,
			},
			"status": map[string]interface{}{
				"state": state,
			},
		},
	}
}

// nodewrightWithStatus builds a Nodewright fixture. Nodewright is a cluster-scoped CR,
// so metadata.namespace is intentionally not set.
func nodewrightWithStatus(name, status string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "skyhook.nvidia.com/v1alpha1",
			"kind":       "Skyhook",
			"metadata": map[string]interface{}{
				"name": name,
				"labels": map[string]interface{}{
					testAICRCreatedByLabelKey: testAICRCreatedByLabelValue,
				},
			},
			"status": map[string]interface{}{
				"status": status,
			},
		},
	}
}

func clusterPolicyWithoutState() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "nvidia.com/v1",
			"kind":       "ClusterPolicy",
			"metadata": map[string]interface{}{
				"name": clusterPolicyName,
			},
			"status": map[string]interface{}{},
		},
	}
}
