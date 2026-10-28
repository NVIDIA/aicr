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

package bundler

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// draLabelerImageRE matches the labeler's image line in rendered output: a
// literal, digest-pinned alpine/kubectl reference. The registry host stays
// explicit because short-name-enforcing runtimes reject unqualified references,
// and the tag is constrained to a version shape so a floating tag such as
// :latest cannot satisfy it.
var draLabelerImageRE = regexp.MustCompile(`(?m)^\s*image: docker\.io/alpine/kubectl:[0-9]+(?:\.[0-9]+)*@sha256:[0-9a-f]{64}\s*$`)

// testDRANodeLabelerRecipeResult is testDRAEvictionRecipeResult plus the
// dra-node-labeler component wired the way recipes/overlays/base.yaml wires
// it: the labeler depends on gpu-operator and the DRA driver depends on the
// labeler.
func testDRANodeLabelerRecipeResult() *recipe.RecipeResult {
	rr := testDRAEvictionRecipeResult()
	for i := range rr.ComponentRefs {
		if rr.ComponentRefs[i].Name == draComponentName {
			rr.ComponentRefs[i].DependencyRefs = []string{gpuOperatorComponentName, draNodeLabelerComponentName}
		}
	}
	rr.ComponentRefs = append(rr.ComponentRefs, recipe.ComponentRef{
		Name:           draNodeLabelerComponentName,
		Namespace:      "gpu-operator",
		Type:           recipe.ComponentTypeHelm,
		Source:         "",
		ValuesFile:     "components/dra-node-labeler/values.yaml",
		ManifestFiles:  []string{"components/dra-node-labeler/manifests/dra-node-labeler.yaml"},
		DependencyRefs: []string{gpuOperatorComponentName},
	})
	rr.DeploymentOrder = []string{gpuOperatorComponentName, draNodeLabelerComponentName, draComponentName}
	return rr
}

func componentNames(refs []recipe.ComponentRef) []string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	return names
}

// TestFilterEnabledComponents_DRANodeLabelerGate pins the bundle-time gate:
// the labeler is rendered only when the eviction contract is opted into and
// both halves of that contract are in the bundle. When dropped, the DRA
// driver's dependency edge on it must be pruned like any other
// declared-but-disabled dependency.
func TestFilterEnabledComponents_DRANodeLabelerGate(t *testing.T) {
	tests := []struct {
		name       string
		opts       []config.Option
		mutate     func(*recipe.RecipeResult)
		wantKept   bool
		wantReason string
		wantErr    string
	}{
		{
			name:       "not opted in drops the labeler",
			wantKept:   false,
			wantReason: "not opted in",
		},
		{
			name:     "opted in keeps the labeler",
			opts:     []config.Option{config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel())},
			wantKept: true,
		},
		{
			// The positive bundlers filter runs before the gate: a selection
			// that names the labeler but leaves out a component it serves is
			// contradictory and is rejected rather than rendered without it.
			name: "bundlers filter naming the labeler without the DRA driver is rejected",
			opts: []config.Option{
				config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel()),
				config.WithBundlers([]string{gpuOperatorComponentName, draNodeLabelerComponentName}),
			},
			wantErr: "needs both gpu-operator and nvidia-dra-driver-gpu",
		},
		{
			name: "bundlers filter naming the labeler without the flag is rejected",
			opts: []config.Option{
				config.WithBundlers([]string{gpuOperatorComponentName, draComponentName, draNodeLabelerComponentName}),
			},
			wantErr: "not opted in",
		},
		{
			// Leaving the labeler out of the selection is not a request for
			// it, so the gate has nothing to reject.
			name: "bundlers filter that omits the labeler drops it quietly",
			opts: []config.Option{
				config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel()),
				config.WithBundlers([]string{gpuOperatorComponentName, draComponentName}),
			},
			wantKept:   false,
			wantReason: "the bundlers filter excludes it",
		},
		{
			// OpenShift recipes disable gpu-operator and nvidia-dra-driver-gpu in
			// favor of the -ocp aliases; the labeler's edges are not wired there
			// (#2828), so it must not render.
			name: "OpenShift aliases do not satisfy the gate",
			opts: []config.Option{config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel())},
			mutate: func(rr *recipe.RecipeResult) {
				for i := range rr.ComponentRefs {
					if rr.ComponentRefs[i].Name == draComponentName || rr.ComponentRefs[i].Name == gpuOperatorComponentName {
						rr.ComponentRefs[i].Overrides = map[string]any{"enabled": false}
					}
				}
				rr.ComponentRefs = append(rr.ComponentRefs,
					recipe.ComponentRef{Name: "gpu-operator-ocp", Type: recipe.ComponentTypeHelm, Source: "",
						ManifestFiles: []string{"components/gpu-operator-ocp/manifests/clusterpolicy.yaml"}},
					recipe.ComponentRef{Name: "nvidia-dra-driver-gpu-ocp", Type: recipe.ComponentTypeHelm, Source: "https://helm.ngc.nvidia.com/nvidia", Version: "25.12.0"},
				)
				rr.DeploymentOrder = append(rr.DeploymentOrder, "gpu-operator-ocp", "nvidia-dra-driver-gpu-ocp")
			},
			wantKept:   false,
			wantReason: "OpenShift variants are not wired",
		},
		{
			name: "opted in without a DRA driver drops the labeler",
			opts: []config.Option{config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel())},
			mutate: func(rr *recipe.RecipeResult) {
				for i := range rr.ComponentRefs {
					if rr.ComponentRefs[i].Name == draComponentName {
						rr.ComponentRefs[i].Overrides = map[string]any{"enabled": false}
					}
				}
			},
			wantKept:   false,
			wantReason: "needs both gpu-operator and nvidia-dra-driver-gpu",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(tt.opts...)))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			rr := testDRANodeLabelerRecipeResult()
			if tt.mutate != nil {
				tt.mutate(rr)
			}
			if perr := rr.PrepareAndValidateWithContext(context.Background()); perr != nil {
				t.Fatalf("PrepareAndValidate: %v", perr)
			}

			selection, err := b.filterEnabledComponents(rr)
			enabled, order, reasons := selection.emitted, selection.order, selection.excluded
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("filterEnabledComponents() error = nil, want substring %q", tt.wantErr)
				}
				if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("filterEnabledComponents() error = %v", err)
			}

			names := componentNames(enabled)
			gotKept := false
			for _, n := range names {
				if n == draNodeLabelerComponentName {
					gotKept = true
				}
			}
			if gotKept != tt.wantKept {
				t.Fatalf("labeler kept = %v, want %v (enabled: %v)", gotKept, tt.wantKept, names)
			}
			if !tt.wantKept {
				if reason := reasons[draNodeLabelerComponentName]; !strings.Contains(reason, tt.wantReason) {
					t.Errorf("excluded reason = %q, want substring %q", reason, tt.wantReason)
				}
				for _, n := range order {
					if n == draNodeLabelerComponentName {
						t.Errorf("deployment order still lists the dropped labeler: %v", order)
					}
				}
				for _, ref := range enabled {
					for _, dep := range ref.DependencyRefs {
						if dep == draNodeLabelerComponentName {
							t.Errorf("%s still depends on the dropped labeler", ref.Name)
						}
					}
				}
			}
		})
	}
}

// TestInjectDRAEvictionLabel_SetsLabelerPair checks the third half of the
// contract: the labeler receives the configured pair, so the value it writes
// is the one the kubelet plugin selects on and the Driver Manager flips.
func TestInjectDRAEvictionLabel_SetsLabelerPair(t *testing.T) {
	label := config.NodeLabel{Key: "example.com/dra-ready", Value: "enabled"}
	b, err := New(WithConfig(config.NewConfig(config.WithDRAEvictionNodeLabel(label))))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	values := map[string]map[string]any{
		draComponentName:            {},
		gpuOperatorComponentName:    {},
		draNodeLabelerComponentName: {draNodeLabelerKeyPath: "stale", draNodeLabelerValuePath: "stale"},
	}
	rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
		{Name: gpuOperatorComponentName}, {Name: draNodeLabelerComponentName}, {Name: draComponentName},
	}}
	if err := b.injectDRAEvictionLabel(values, rr); err != nil {
		t.Fatalf("injectDRAEvictionLabel() error = %v", err)
	}

	if got := values[draNodeLabelerComponentName][draNodeLabelerKeyPath]; got != label.Key {
		t.Errorf("labeler %s = %v, want %s", draNodeLabelerKeyPath, got, label.Key)
	}
	if got := values[draNodeLabelerComponentName][draNodeLabelerValuePath]; got != label.Value {
		t.Errorf("labeler %s = %v, want %s", draNodeLabelerValuePath, got, label.Value)
	}
	if got := dig(values[draComponentName], "kubeletPlugin", "nodeSelector", label.Key); got != label.Value {
		t.Errorf("kubelet plugin selector = %v, want %s", got, label.Value)
	}
	if got := values[draNodeLabelerComponentName][draNodeLabelerEnabledPath]; got != true {
		t.Errorf("labeler %s = %v, want true", draNodeLabelerEnabledPath, got)
	}
	// The gate must also land on the ref, which is what recipe.yaml serializes.
	var labelerRef *recipe.ComponentRef
	for i := range rr.ComponentRefs {
		if rr.ComponentRefs[i].Name == draNodeLabelerComponentName {
			labelerRef = &rr.ComponentRefs[i]
		}
	}
	if labelerRef == nil || labelerRef.Overrides[draNodeLabelerEnabledPath] != true {
		t.Errorf("labeler ref overrides = %v, want %s: true", labelerRef, draNodeLabelerEnabledPath)
	}

	var derived, provisioned bool
	for _, w := range b.warnings {
		if strings.Contains(w, "dra-node-labeler applies that label") {
			derived = true
		}
		if strings.Contains(w, "apply that label to every GPU node at node-pool provisioning time") {
			provisioned = true
		}
	}
	if !derived {
		t.Errorf("expected the derived-label warning, got %v", b.warnings)
	}
	if provisioned {
		t.Errorf("provisioning warning must not fire when the labeler is in the bundle: %v", b.warnings)
	}
}

// TestInjectDRAEvictionLabel_WithoutLabelerKeepsProvisioningWarning pins the
// opt-out path (--set dra-node-labeler:enabled=false): with the labeler gone,
// the operator is back to provisioning the label and must be told so.
func TestInjectDRAEvictionLabel_WithoutLabelerKeepsProvisioningWarning(t *testing.T) {
	b, err := New(WithConfig(config.NewConfig(config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel()))))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	values := map[string]map[string]any{draComponentName: {}, gpuOperatorComponentName: {}}
	rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
		{Name: gpuOperatorComponentName}, {Name: draComponentName},
	}}
	if err := b.injectDRAEvictionLabel(values, rr); err != nil {
		t.Fatalf("injectDRAEvictionLabel() error = %v", err)
	}
	if _, present := values[draNodeLabelerComponentName]; present {
		t.Errorf("labeler values must not be created when the component is absent")
	}
	found := false
	for _, w := range b.warnings {
		if strings.Contains(w, "node-pool provisioning time") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the provisioning warning, got %v", b.warnings)
	}
}

// TestWarnDRAEvictionNotConfigured_NoProvisioningClause pins the opt-out
// message an operator reads before deciding to opt in: it must describe the
// labeler, not tell them to label node pools (that cost is what the labeler
// removes).
func TestWarnDRAEvictionNotConfigured_NoProvisioningClause(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	values := map[string]map[string]any{draComponentName: {}, gpuOperatorComponentName: {}}
	rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: gpuOperatorComponentName}, {Name: draComponentName}}}
	if err := b.injectDRAEvictionLabel(values, rr); err != nil {
		t.Fatalf("injectDRAEvictionLabel() error = %v", err)
	}
	for _, w := range b.warnings {
		if !strings.Contains(w, "did not configure automatic eviction") {
			continue
		}
		if strings.Contains(w, "node-pool provisioning time") {
			t.Errorf("opt-out warning still asks for node-pool labeling: %s", w)
		}
		if !strings.Contains(w, "dra-node-labeler") {
			t.Errorf("opt-out warning does not mention the labeler: %s", w)
		}
		return
	}
	t.Fatalf("opt-out warning not emitted: %v", b.warnings)
}

// TestRejectDRAEvictionDynamicPaths_LabelerPaths: the labeler's pair is
// bundler-managed once opted in, so a --dynamic declaration on it is rejected
// exactly like the kubelet-plugin selector and Driver Manager env.
func TestRejectDRAEvictionDynamicPaths_LabelerPaths(t *testing.T) {
	rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
		{Name: gpuOperatorComponentName}, {Name: draNodeLabelerComponentName}, {Name: draComponentName},
	}}
	for _, path := range []string{draNodeLabelerKeyPath, draNodeLabelerValuePath} {
		err := rejectDRAEvictionDynamicPaths(rr,
			map[string][]string{draNodeLabelerComponentName: {path}},
			config.DefaultDRAEvictionNodeLabel())
		if err == nil {
			t.Errorf("dynamic %s on the labeler was accepted; want rejection", path)
		}
	}
	if err := rejectDRAEvictionDynamicPaths(rr,
		map[string][]string{draNodeLabelerComponentName: {"image"}},
		config.DefaultDRAEvictionNodeLabel()); err != nil {
		t.Errorf("dynamic image on the labeler was rejected: %v", err)
	}
}

// TestMake_DRANodeLabelerRendered runs the real bundler on the labeler recipe
// with and without the flag and checks what lands in the bundle.
func TestMake_DRANodeLabelerRendered(t *testing.T) {
	label := config.NodeLabel{Key: "example.com/dra-ready", Value: "enabled"}

	t.Run("opted in renders the labeler with the configured pair", func(t *testing.T) {
		// No accelerated tolerations on purpose: this is the SDK path, where
		// WithAcceleratedNodeTolerations(nil) is a no-op, so the template's
		// own fallback must render.
		b, err := New(WithConfig(config.NewConfig(config.WithDRAEvictionNodeLabel(label))))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		outputDir := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), draBundleMakeTimeout)
		defer cancel()
		if _, err := b.Make(ctx, testDRANodeLabelerRecipeResult(), outputDir); err != nil {
			t.Fatalf("Make() error = %v", err)
		}
		manifest := string(readBundleValues(t, outputDir, filepath.Join("002-"+draNodeLabelerComponentName, "templates", "dra-node-labeler.yaml")))
		for _, want := range []string{
			`LABEL_KEY="example.com/dra-ready"`,
			`LABEL_VALUE="enabled"`,
			"key: nvidia.com/gpu.present",
			`maxUnavailable: "100%"`,
			// SDK-path toleration fallback.
			"- operator: Exists",
			// Ready only once the node carries the key (#2813 review).
			`command: ["test", "-f", "/var/run/dra-node-labeler/labeled"]`,
			`touch "${READY}"`,
			"cpu: 20m",
			"cpu: 200m",
		} {
			if !strings.Contains(manifest, want) {
				t.Errorf("rendered labeler lacks %q", want)
			}
		}
		// Shape rather than an exact digest: Renovate rotates the digest as
		// upstream rebuilds the tag. The reference must stay literal and
		// digest-pinned; see TestSurveyComponent_DRANodeLabelerImageInventoried.
		if !draLabelerImageRE.MatchString(manifest) {
			t.Errorf("rendered labeler lacks a literal digest-pinned image matching %s", draLabelerImageRE)
		}
		if strings.Contains(manifest, "hostNetwork") {
			t.Errorf("labeler must not request hostNetwork")
		}
		if strings.Contains(manifest, "set -e") {
			t.Errorf("labeler must retry in place, not exit (restarts fail the health check)")
		}
		if strings.Contains(manifest, "{{") {
			t.Errorf("unrendered template expression in labeler manifest")
		}
	})

	t.Run("not opted in leaves the labeler out of the bundle", func(t *testing.T) {
		b, err := New()
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		outputDir := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), draBundleMakeTimeout)
		defer cancel()
		if _, merr := b.Make(ctx, testDRANodeLabelerRecipeResult(), outputDir); merr != nil {
			t.Fatalf("Make() error = %v", merr)
		}
		entries, err := os.ReadDir(outputDir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), draNodeLabelerComponentName) {
				t.Errorf("bundle contains %s without the eviction flag", e.Name())
			}
		}
		// The DRA driver keeps its slot right after gpu-operator once the
		// labeler is dropped, and its selector carries no eviction label.
		if _, err := os.Stat(filepath.Join(outputDir, "002-"+draComponentName)); err != nil {
			t.Errorf("expected 002-%s in the bundle: %v (entries: %v)", draComponentName, err, entries)
		}
	})
}

// TestMake_DRANodeLabelerRecipePersistsGate pins what the bundle's recipe.yaml
// says about the labeler, because that file -- not the bundler's in-memory
// values -- is what post-deployment validation reads (#2848). Opted in, the
// labeler ref carries enabled: true so the validator renders and checks it;
// not opted in, the ref is absent. The caller's RecipeResult is never touched.
func TestMake_DRANodeLabelerRecipePersistsGate(t *testing.T) {
	label := config.NodeLabel{Key: "example.com/dra-ready", Value: "enabled"}

	labelerRef := func(t *testing.T, outputDir string) *recipe.ComponentRef {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), draBundleMakeTimeout)
		defer cancel()
		loaded, err := recipe.LoadFromFileWithProvider(ctx, filepath.Join(outputDir, RecipeFileName), "", "test", nil)
		if err != nil {
			t.Fatalf("load %s: %v", RecipeFileName, err)
		}
		for i := range loaded.ComponentRefs {
			if loaded.ComponentRefs[i].Name == draNodeLabelerComponentName {
				return &loaded.ComponentRefs[i]
			}
		}
		return nil
	}

	t.Run("opted in persists enabled=true on the labeler ref", func(t *testing.T) {
		b, err := New(WithConfig(config.NewConfig(config.WithDRAEvictionNodeLabel(label))))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		input := testDRANodeLabelerRecipeResult()
		input.Kind = recipe.RecipeResultKind
		input.APIVersion = recipe.RecipeResultAPIVersion
		outputDir := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), draBundleMakeTimeout)
		defer cancel()
		if _, err := b.Make(ctx, input, outputDir); err != nil {
			t.Fatalf("Make() error = %v", err)
		}

		ref := labelerRef(t, outputDir)
		if ref == nil {
			t.Fatalf("%s lacks the %s ref after opting in", RecipeFileName, draNodeLabelerComponentName)
		}
		if got := ref.Overrides[draNodeLabelerEnabledPath]; got != true {
			t.Errorf("%s ref overrides.%s = %v, want true", draNodeLabelerComponentName, draNodeLabelerEnabledPath, got)
		}
		if !ref.IsEnabled() {
			t.Errorf("persisted gate must keep the ref enabled")
		}
		for _, in := range input.ComponentRefs {
			if in.Name == draNodeLabelerComponentName && in.Overrides != nil {
				t.Errorf("caller's labeler ref overrides mutated: %v", in.Overrides)
			}
		}
	})

	t.Run("not opted in leaves the labeler out of the recipe", func(t *testing.T) {
		b, err := New()
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		input := testDRANodeLabelerRecipeResult()
		input.Kind = recipe.RecipeResultKind
		input.APIVersion = recipe.RecipeResultAPIVersion
		outputDir := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), draBundleMakeTimeout)
		defer cancel()
		if _, err := b.Make(ctx, input, outputDir); err != nil {
			t.Fatalf("Make() error = %v", err)
		}
		if ref := labelerRef(t, outputDir); ref != nil {
			t.Errorf("%s carries %s without the eviction flag: %+v", RecipeFileName, draNodeLabelerComponentName, ref)
		}
	})
}

// TestDRANodeLabelerManifest_SecuritySurface compares the labeler's RBAC,
// placement and readiness marker as whole structures, so a widened verb, an
// extra binding subject, a moved affinity key or a drifted marker path fails.
func TestDRANodeLabelerManifest_SecuritySurface(t *testing.T) {
	objs := renderDRALabelerObjects(t, config.DefaultDRAEvictionNodeLabel())

	wantKinds := map[string]int{"ServiceAccount": 1, "ClusterRole": 1, "ClusterRoleBinding": 1, "DaemonSet": 1}
	if !reflect.DeepEqual(objs.kinds, wantKinds) {
		t.Errorf("rendered kinds = %v, want %v", objs.kinds, wantKinds)
	}
	sa, role, binding, ds := objs.serviceAccount, objs.clusterRole, objs.binding, objs.daemonSet
	if ds.Namespace == "" {
		t.Fatal("labeler DaemonSet has no namespace")
	}
	if sa.Name != draNodeLabelerComponentName || sa.Namespace != ds.Namespace {
		t.Errorf("ServiceAccount = %s/%s, want %s/%s", sa.Namespace, sa.Name, ds.Namespace, draNodeLabelerComponentName)
	}

	// Read and patch Node objects, nothing else.
	wantRules := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "patch"}}}
	if role.Name != draNodeLabelerComponentName || !reflect.DeepEqual(role.Rules, wantRules) {
		t.Errorf("ClusterRole %q rules = %+v, want %q rules %+v", role.Name, role.Rules, draNodeLabelerComponentName, wantRules)
	}
	wantRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}
	wantSubjects := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa.Name, Namespace: sa.Namespace}}
	if binding.RoleRef != wantRef || !reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Errorf("ClusterRoleBinding roleRef %+v subjects %+v, want %+v %+v", binding.RoleRef, binding.Subjects, wantRef, wantSubjects)
	}
	spec := ds.Spec.Template.Spec
	if spec.ServiceAccountName != sa.Name {
		t.Errorf("DaemonSet serviceAccountName = %q, want the bound %q", spec.ServiceAccountName, sa.Name)
	}

	// Placement is hardware truth, so one required term on GFD's gpu.present
	// and no nodeSelector.
	wantTerms := []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
		Key: "nvidia.com/gpu.present", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"},
	}}}}
	var gotTerms []corev1.NodeSelectorTerm
	if a := spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		gotTerms = a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	}
	if !reflect.DeepEqual(gotTerms, wantTerms) {
		t.Errorf("required node affinity = %+v, want %+v", gotTerms, wantTerms)
	}
	if len(spec.NodeSelector) != 0 {
		t.Errorf("labeler carries a nodeSelector %v", spec.NodeSelector)
	}

	// The probe's marker is the script's READY env, on a writable emptyDir
	// because the root filesystem is read-only.
	if len(spec.Containers) != 1 {
		t.Fatalf("labeler DaemonSet has %d containers, want 1", len(spec.Containers))
	}
	c := spec.Containers[0]
	if sc := c.SecurityContext; sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("labeler root filesystem is not read-only: %+v", c.SecurityContext)
	}
	probe := c.ReadinessProbe
	if probe == nil || probe.Exec == nil || len(probe.Exec.Command) != 3 ||
		probe.Exec.Command[0] != "test" || probe.Exec.Command[1] != "-f" {

		t.Fatalf("labeler readiness probe = %+v, want test -f <marker>", probe)
	}
	marker := probe.Exec.Command[2]
	var ready []string
	for _, e := range c.Env {
		if e.Name == "READY" {
			ready = append(ready, e.Value)
		}
	}
	if !reflect.DeepEqual(ready, []string{marker}) {
		t.Errorf("READY env = %q, want the probe's marker %q", ready, marker)
	}
	var mounts []corev1.VolumeMount
	for _, m := range c.VolumeMounts {
		if m.MountPath == path.Dir(marker) {
			mounts = append(mounts, m)
		}
	}
	if len(mounts) != 1 || mounts[0].ReadOnly {
		t.Fatalf("mounts at %s = %+v, want one writable mount", path.Dir(marker), mounts)
	}
	emptyDir := false
	for _, v := range spec.Volumes {
		if v.Name == mounts[0].Name {
			emptyDir = v.EmptyDir != nil
		}
	}
	if !emptyDir {
		t.Errorf("volume %q at %s is not an emptyDir", mounts[0].Name, path.Dir(marker))
	}
}

// draLabelerObjects is the rendered labeler manifest, decoded. kinds counts
// every document by kind, including kinds the typed fields do not hold.
type draLabelerObjects struct {
	kinds          map[string]int
	serviceAccount *corev1.ServiceAccount
	clusterRole    *rbacv1.ClusterRole
	binding        *rbacv1.ClusterRoleBinding
	daemonSet      *appsv1.DaemonSet
}

// renderDRALabelerObjects bundles the labeler recipe opted in to label and
// decodes the rendered manifest.
func renderDRALabelerObjects(t *testing.T, label config.NodeLabel) draLabelerObjects {
	t.Helper()
	b, err := New(WithConfig(config.NewConfig(config.WithDRAEvictionNodeLabel(label))))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	outputDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), draBundleMakeTimeout)
	defer cancel()
	if _, err = b.Make(ctx, testDRANodeLabelerRecipeResult(), outputDir); err != nil {
		t.Fatalf("Make() error = %v", err)
	}
	data := readBundleValues(t, outputDir, filepath.Join("002-"+draNodeLabelerComponentName, "templates", "dra-node-labeler.yaml"))

	objs := draLabelerObjects{kinds: map[string]int{}}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var doc map[string]any
		if err = dec.Decode(&doc); err != nil {
			if stderrors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode labeler manifest: %v", err)
		}
		if doc == nil {
			continue
		}
		kind, _ := doc["kind"].(string)
		objs.kinds[kind]++
		var into any
		switch kind {
		case "ServiceAccount":
			objs.serviceAccount = &corev1.ServiceAccount{}
			into = objs.serviceAccount
		case "ClusterRole":
			objs.clusterRole = &rbacv1.ClusterRole{}
			into = objs.clusterRole
		case "ClusterRoleBinding":
			objs.binding = &rbacv1.ClusterRoleBinding{}
			into = objs.binding
		case "DaemonSet":
			objs.daemonSet = &appsv1.DaemonSet{}
			into = objs.daemonSet
		default:
			continue
		}
		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(doc, into); err != nil {
			t.Fatalf("convert %s: %v", kind, err)
		}
	}
	if objs.serviceAccount == nil || objs.clusterRole == nil || objs.binding == nil || objs.daemonSet == nil {
		t.Fatalf("labeler manifest lacks a ServiceAccount, ClusterRole, ClusterRoleBinding or DaemonSet:\n%s", data)
	}
	return objs
}
