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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

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
			wantReason: "needs both a GPU Operator and a DRA driver",
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

			enabled, order, reasons, err := b.filterEnabledComponents(rr)
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
		b, err := New(WithConfig(config.NewConfig(
			config.WithDRAEvictionNodeLabel(label),
			config.WithAcceleratedNodeTolerations([]corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}),
		)))
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
			"key: nvidia.com/gpu",
		} {
			if !strings.Contains(manifest, want) {
				t.Errorf("rendered labeler lacks %q", want)
			}
		}
		if strings.Contains(manifest, "hostNetwork") {
			t.Errorf("labeler must not request hostNetwork")
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
