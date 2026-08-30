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
	stderrors "errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestInjectDRAEvictionLabel(t *testing.T) {
	tests := []struct {
		name       string
		draName    string
		gpuName    string
		configured config.NodeLabel
		wantKey    string
		wantValue  string
	}{
		{
			name:      "standard components use default",
			draName:   draComponentName,
			gpuName:   gpuOperatorComponentName,
			wantKey:   defaults.DRAEvictionNodeLabelKey,
			wantValue: defaults.DRAEvictionNodeLabelValue,
		},
		{
			name:       "standard components use configured label",
			draName:    draComponentName,
			gpuName:    gpuOperatorComponentName,
			configured: config.NodeLabel{Key: "example.com/dra-ready", Value: "enabled"},
			wantKey:    "example.com/dra-ready",
			wantValue:  "enabled",
		},
		{
			name:      "OpenShift components use default",
			draName:   "nvidia-dra-driver-gpu-ocp",
			gpuName:   "gpu-operator-ocp",
			wantKey:   defaults.DRAEvictionNodeLabelKey,
			wantValue: defaults.DRAEvictionNodeLabelValue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The contract is opt-in (#2469), so these mechanics tests
			// configure a label explicitly; cases with no override exercise
			// the documented default label.
			configured := tt.configured
			if configured == (config.NodeLabel{}) {
				configured = config.DefaultDRAEvictionNodeLabel()
			}
			opts := []config.Option{config.WithDRAEvictionNodeLabel(configured)}
			b, err := New(WithConfig(config.NewConfig(opts...)))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			nodeSelector := map[string]any{
				"node.dgxc.nvidia.com/has-gpu": "true",
				tt.wantKey:                     "stale",
			}
			if tt.configured != (config.NodeLabel{}) {
				nodeSelector[defaults.DRAEvictionNodeLabelKey] = "stale-default"
			}
			values := map[string]map[string]any{
				tt.draName: {
					"kubeletPlugin": map[string]any{
						"nodeSelector": nodeSelector,
					},
				},
				tt.gpuName: {
					"driver": map[string]any{
						"manager": map[string]any{
							"env": []any{
								"preserved-non-map",
								map[string]any{"name": "UNRELATED", "value": "preserved"},
								map[string]any{
									"name":      draEvictionEnvName,
									"valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}},
								},
								map[string]any{"name": draEvictionEnvName, "value": "duplicate"},
							},
						},
					},
				},
			}
			rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: tt.gpuName},
				{Name: tt.draName},
			}}

			if err := b.injectDRAEvictionLabel(values, rr); err != nil {
				t.Fatalf("injectDRAEvictionLabel() error = %v", err)
			}

			if got := dig(values[tt.draName], "kubeletPlugin", "nodeSelector", tt.wantKey); got != tt.wantValue {
				t.Errorf("DRA node selector = %v, want %q", got, tt.wantValue)
			}
			if got := dig(values[tt.draName], "kubeletPlugin", "nodeSelector", "node.dgxc.nvidia.com/has-gpu"); got != "true" {
				t.Errorf("existing accelerated selector = %v, want preserved", got)
			}
			if tt.configured != (config.NodeLabel{}) {
				if got := dig(values[tt.draName], "kubeletPlugin", "nodeSelector", defaults.DRAEvictionNodeLabelKey); got != nil {
					t.Errorf("default DRA selector survived custom-label replacement: %v", got)
				}
			}
			if got := driverManagerEnvValues(values[tt.gpuName], draEvictionEnvName); len(got) != 1 || got[0] != tt.wantKey {
				t.Errorf("Driver Manager eviction env values = %v, want [%s]", got, tt.wantKey)
			}
			if got := driverManagerEnvValues(values[tt.gpuName], "UNRELATED"); len(got) != 1 || got[0] != "preserved" {
				t.Errorf("unrelated Driver Manager env values = %v, want [preserved]", got)
			}
			env, _ := dig(values[tt.gpuName], "driver", "manager", "env").([]any)
			if len(env) == 0 || env[0] != "preserved-non-map" {
				t.Errorf("non-map Driver Manager env entry = %v, want preserved", env)
			}
		})
	}
}

func TestInjectDRAEvictionLabel_CreatesMissingManagedPaths(t *testing.T) {
	b, err := New(WithConfig(config.NewConfig(
		config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel()))))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	values := map[string]map[string]any{
		draComponentName:         {},
		gpuOperatorComponentName: {},
	}
	rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
		{Name: gpuOperatorComponentName},
		{Name: draComponentName},
	}}

	if err := b.injectDRAEvictionLabel(values, rr); err != nil {
		t.Fatalf("injectDRAEvictionLabel() error = %v", err)
	}
	if got := dig(values[draComponentName], "kubeletPlugin", "nodeSelector", defaults.DRAEvictionNodeLabelKey); got != defaults.DRAEvictionNodeLabelValue {
		t.Errorf("DRA eviction node selector = %v, want %s", got, defaults.DRAEvictionNodeLabelValue)
	}
	if got := driverManagerEnvValues(values[gpuOperatorComponentName], draEvictionEnvName); len(got) != 1 || got[0] != defaults.DRAEvictionNodeLabelKey {
		t.Errorf("Driver Manager eviction env values = %v, want [%s]", got, defaults.DRAEvictionNodeLabelKey)
	}
}

func TestInjectDRAEvictionLabel_PreservesStringMapNodeSelector(t *testing.T) {
	b, err := New(WithConfig(config.NewConfig(
		config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel()))))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	values := map[string]map[string]any{
		draComponentName: {
			"kubeletPlugin": map[string]any{
				"nodeSelector": map[string]string{"example.com/existing": "yes"},
			},
		},
		gpuOperatorComponentName: {},
	}
	rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
		{Name: gpuOperatorComponentName},
		{Name: draComponentName},
	}}

	if err := b.injectDRAEvictionLabel(values, rr); err != nil {
		t.Fatalf("injectDRAEvictionLabel() error = %v", err)
	}
	if got := dig(values[draComponentName], "kubeletPlugin", "nodeSelector", "example.com/existing"); got != "yes" {
		t.Errorf("existing DRA node selector = %v, want yes", got)
	}
}

func TestInjectDRAEvictionLabel_RejectsMalformedManagedPaths(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		mutate func(map[string]map[string]any)
	}{
		{
			name: "DRA kubelet plugin must be an object",
			path: "kubeletPlugin",
			mutate: func(values map[string]map[string]any) {
				values[draComponentName]["kubeletPlugin"] = "invalid"
			},
		},
		{
			name: "DRA node selector must be an object",
			path: "kubeletPlugin.nodeSelector",
			mutate: func(values map[string]map[string]any) {
				values[draComponentName]["kubeletPlugin"] = map[string]any{"nodeSelector": "invalid"}
			},
		},
		{
			name: "GPU Operator driver must be an object",
			path: "driver",
			mutate: func(values map[string]map[string]any) {
				values[gpuOperatorComponentName]["driver"] = "invalid"
			},
		},
		{
			name: "GPU Operator manager must be an object",
			path: "driver.manager",
			mutate: func(values map[string]map[string]any) {
				values[gpuOperatorComponentName]["driver"] = map[string]any{"manager": "invalid"}
			},
		},
		{
			name: "GPU Operator env must be an array",
			path: "driver.manager.env",
			mutate: func(values map[string]map[string]any) {
				values[gpuOperatorComponentName]["driver"] = map[string]any{
					"manager": map[string]any{"env": "invalid"},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(
				config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel()))))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			values := map[string]map[string]any{
				draComponentName:         {},
				gpuOperatorComponentName: {},
			}
			tt.mutate(values)
			rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: gpuOperatorComponentName},
				{Name: draComponentName},
			}}

			err = b.injectDRAEvictionLabel(values, rr)
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("injectDRAEvictionLabel() error = %v, want ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.path) {
				t.Errorf("injectDRAEvictionLabel() error = %q, want path %q", err, tt.path)
			}
		})
	}
}

func TestInjectDRAEvictionLabel_RequiresBothComponents(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]map[string]any
		refs   []recipe.ComponentRef
	}{
		{
			name:   "DRA absent",
			values: map[string]map[string]any{gpuOperatorComponentName: {}},
			refs:   []recipe.ComponentRef{{Name: gpuOperatorComponentName}},
		},
		{
			name:   "GPU Operator absent",
			values: map[string]map[string]any{draComponentName: {}},
			refs:   []recipe.ComponentRef{{Name: draComponentName}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(
				config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel()))))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if err := b.injectDRAEvictionLabel(tt.values, &recipe.RecipeResult{ComponentRefs: tt.refs}); err != nil {
				t.Fatalf("injectDRAEvictionLabel() error = %v", err)
			}
			for name, values := range tt.values {
				if len(values) != 0 {
					t.Errorf("component %s values changed with only one contract half enabled: %v", name, values)
				}
			}
		})
	}
}

func TestRejectDRAEvictionDynamicPaths(t *testing.T) {
	standardRefs := []recipe.ComponentRef{
		{Name: gpuOperatorComponentName},
		{Name: draComponentName},
	}
	ocpRefs := []recipe.ComponentRef{
		{Name: "gpu-operator-ocp"},
		{Name: "nvidia-dra-driver-gpu-ocp"},
	}
	tests := []struct {
		name          string
		refs          []recipe.ComponentRef
		dynamicValues map[string][]string
		wantPath      string
		wantErr       bool
	}{
		{
			name: "standard DRA exact path",
			refs: standardRefs,
			dynamicValues: map[string][]string{
				draComponentName: {draEvictionNodeSelectorPath},
			},
			wantPath: draEvictionNodeSelectorPath,
			wantErr:  true,
		},
		{
			name: "standard GPU Operator parent path",
			refs: standardRefs,
			dynamicValues: map[string][]string{
				gpuOperatorComponentName: {"driver.manager"},
			},
			wantPath: "driver.manager",
			wantErr:  true,
		},
		{
			name: "OpenShift DRA parent path",
			refs: ocpRefs,
			dynamicValues: map[string][]string{
				"nvidia-dra-driver-gpu-ocp": {"kubeletPlugin"},
			},
			wantPath: "kubeletPlugin",
			wantErr:  true,
		},
		{
			name: "OpenShift GPU Operator descendant path",
			refs: ocpRefs,
			dynamicValues: map[string][]string{
				"gpu-operator-ocp": {gpuOperatorDRAEvictionEnvPath + ".value"},
			},
			wantPath: gpuOperatorDRAEvictionEnvPath + ".value",
			wantErr:  true,
		},
		{
			name: "unrelated dynamic path remains allowed",
			refs: standardRefs,
			dynamicValues: map[string][]string{
				gpuOperatorComponentName: {"driver.version"},
			},
		},
		{
			name: "single contract half remains allowed",
			refs: []recipe.ComponentRef{{Name: draComponentName}},
			dynamicValues: map[string][]string{
				draComponentName: {draEvictionNodeSelectorPath},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rejectDRAEvictionDynamicPaths(
				&recipe.RecipeResult{ComponentRefs: tt.refs}, tt.dynamicValues,
				config.DefaultDRAEvictionNodeLabel())
			if tt.wantErr {
				if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
					t.Fatalf("rejectDRAEvictionDynamicPaths() error = %v, want ErrCodeInvalidRequest", err)
				}
				if !strings.Contains(err.Error(), tt.wantPath) {
					t.Errorf("rejectDRAEvictionDynamicPaths() error = %q, want path %q", err, tt.wantPath)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejectDRAEvictionDynamicPaths() error = %v, want nil", err)
			}
		})
	}
}

func TestMake_DRAEvictionLabelMergesSchedulingSelector(t *testing.T) {
	b, err := New(WithConfig(config.NewConfig(
		config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel()),
		config.WithAcceleratedNodeSelector(map[string]string{
			"node.dgxc.nvidia.com/has-gpu": "true",
		}),
	)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	rr := testDRAEvictionRecipeResult()

	outputDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), draBundleMakeTimeout)
	defer cancel()
	if _, err := b.Make(ctx, rr, outputDir); err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	var draValues map[string]any
	if err := yaml.Unmarshal(readBundleValues(t, outputDir, "002-nvidia-dra-driver-gpu/values.yaml"), &draValues); err != nil {
		t.Fatalf("decode DRA values: %v", err)
	}
	if got := dig(draValues, "kubeletPlugin", "nodeSelector", "node.dgxc.nvidia.com/has-gpu"); got != "true" {
		t.Errorf("accelerated node selector = %v, want true", got)
	}
	if got := dig(draValues, "kubeletPlugin", "nodeSelector", defaults.DRAEvictionNodeLabelKey); got != defaults.DRAEvictionNodeLabelValue {
		t.Errorf("DRA eviction node selector = %v, want %s", got, defaults.DRAEvictionNodeLabelValue)
	}

	var gpuValues map[string]any
	if err := yaml.Unmarshal(readBundleValues(t, outputDir, "001-gpu-operator/values.yaml"), &gpuValues); err != nil {
		t.Fatalf("decode GPU Operator values: %v", err)
	}
	if got := driverManagerEnvValues(gpuValues, draEvictionEnvName); len(got) != 1 || got[0] != defaults.DRAEvictionNodeLabelKey {
		t.Errorf("Driver Manager eviction env values = %v, want [%s]", got, defaults.DRAEvictionNodeLabelKey)
	}
}

func TestMake_DRAEvictionLabelRejectsMalformedManagedOverrides(t *testing.T) {
	tests := []struct {
		name      string
		component string
		path      string
	}{
		{
			name:      "DRA node selector scalar",
			component: draComponentName,
			path:      "kubeletPlugin.nodeSelector",
		},
		{
			name:      "GPU Operator environment scalar",
			component: gpuOperatorComponentName,
			path:      "driver.manager.env",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(
				config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel()),
				config.WithValueOverrides(
					map[string]map[string]string{
						tt.component: {tt.path: "invalid"},
					},
				))))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), draBundleMakeTimeout)
			defer cancel()
			_, err = b.Make(ctx, testDRAEvictionRecipeResult(), t.TempDir())
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("Make() error = %v, want ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.path) {
				t.Errorf("Make() error = %q, want path %q", err, tt.path)
			}
		})
	}
}

func TestMake_DRAEvictionLabelRejectsDynamicManagedPaths(t *testing.T) {
	tests := []struct {
		name         string
		componentKey string
		path         string
	}{
		{
			name:         "DRA node selector",
			componentKey: "dradriver",
			path:         draEvictionNodeSelectorPath,
		},
		{
			name:         "GPU Operator environment",
			componentKey: "gpuoperator",
			path:         gpuOperatorDRAEvictionEnvPath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(
				config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel()),
				config.WithDynamicValues(
					map[string][]string{tt.componentKey: {tt.path}},
				))))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), draBundleMakeTimeout)
			defer cancel()
			_, err = b.Make(ctx, testDRAEvictionRecipeResult(), t.TempDir())
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("Make() error = %v, want ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.path) {
				t.Errorf("Make() error = %q, want path %q", err, tt.path)
			}
		})
	}
}

func testDRAEvictionRecipeResult() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		APIVersion: "aicr.run/v1alpha2",
		Kind:       "Recipe",
		Criteria: &recipe.Criteria{
			Service:     "eks",
			Accelerator: "h100",
			Intent:      "training",
		},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    gpuOperatorComponentName,
				Version: "v26.4.0",
				Type:    recipe.ComponentTypeHelm,
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
			{
				Name:    draComponentName,
				Version: "25.12.0",
				Type:    recipe.ComponentTypeHelm,
				Source:  "https://helm.ngc.nvidia.com/nvidia",
				Overrides: map[string]any{
					"nvidiaDriverRoot": "/run/nvidia/driver",
				},
			},
		},
		DeploymentOrder: []string{gpuOperatorComponentName, draComponentName},
	}
}

func driverManagerEnvValues(values map[string]any, name string) []string {
	env, _ := dig(values, "driver", "manager", "env").([]any)
	result := make([]string, 0, 1)
	for _, entry := range env {
		envMap, ok := entry.(map[string]any)
		if !ok || envMap["name"] != name {
			continue
		}
		if value, ok := envMap["value"].(string); ok {
			result = append(result, value)
		}
	}
	return result
}

// TestWarnDRAEvictionNodeLabelRequired asserts the non-blocking bundle-time
// warning fires exactly when both halves of the eviction contract are enabled,
// mirroring the StorageClass warning precedent (issue #2456).
func TestWarnDRAEvictionNodeLabelRequired(t *testing.T) {
	tests := []struct {
		name        string
		refs        []recipe.ComponentRef
		configured  config.NodeLabel
		wantWarning bool
		wantSubstr  string
		alsoWant    []string
		// wantCount is the exact number of node-label warnings expected.
		// Zero means one, the single-DRA-component default.
		wantCount int
	}{
		{
			name: "both components enabled warns",
			refs: []recipe.ComponentRef{
				{Name: draComponentName},
				{Name: gpuOperatorComponentName},
			},
			wantWarning: true,
			wantSubstr: draComponentName + " schedules its kubelet plugin only on nodes labeled " +
				defaults.DRAEvictionNodeLabelKey + "=" + defaults.DRAEvictionNodeLabelValue,
		},
		{
			name: "configured label appears in the warning",
			refs: []recipe.ComponentRef{
				{Name: draComponentName},
				{Name: gpuOperatorComponentName},
			},
			configured:  config.NodeLabel{Key: "example.com/dra-ready", Value: "enabled"},
			wantWarning: true,
			wantSubstr:  "nodes labeled example.com/dra-ready=enabled",
		},
		{
			name: "OpenShift components warn under their own names",
			refs: []recipe.ComponentRef{
				{Name: "nvidia-dra-driver-gpu-ocp"},
				{Name: "gpu-operator-ocp"},
			},
			wantWarning: true,
			wantSubstr:  "nvidia-dra-driver-gpu-ocp schedules its kubelet plugin only on nodes labeled ",
		},
		{
			// The warning must distinguish per-node absence from total absence.
			// Partial label coverage — the shape node replacement and autoscaling
			// produce — leaves labeled nodes working while the rest silently lack
			// DRA; DESIRED=0 applies only when NO GPU node matches. An earlier
			// wording described every case as DESIRED=0, which overstates the
			// common case and understates how hard a split cluster is to notice.
			name: "warning distinguishes per-node absence from DESIRED=0",
			refs: []recipe.ComponentRef{
				{Name: draComponentName},
				{Name: gpuOperatorComponentName},
			},
			wantWarning: true,
			// Both halves of the distinction are asserted. Pinning only the
			// zero-match clause would let a regression delete the per-node
			// clause — the more common case — and still pass.
			wantSubstr: "Unlabeled GPU nodes silently run without DRA",
			alsoWant: []string{
				"if no GPU node carries the label the kubelet-plugin DaemonSet sits at DESIRED=0",
			},
		},
		{
			// Arbitrary node labels live at NodePool.spec.template.metadata.labels.
			// An EC2NodeClass has no such field, so naming nodeClass here sent
			// operators to a resource that cannot carry the label.
			name: "warning names the Karpenter resource that carries labels",
			refs: []recipe.ComponentRef{
				{Name: draComponentName},
				{Name: gpuOperatorComponentName},
			},
			wantWarning: true,
			wantSubstr:  "Karpenter NodePool spec.template.metadata.labels",
		},
		{
			name:        "DRA absent does not warn",
			refs:        []recipe.ComponentRef{{Name: gpuOperatorComponentName}},
			wantWarning: false,
		},
		{
			name:        "GPU Operator absent does not warn",
			refs:        []recipe.ComponentRef{{Name: draComponentName}},
			wantWarning: false,
		},
		{
			name:        "neither component present does not warn",
			refs:        []recipe.ComponentRef{{Name: "some-other-component"}},
			wantWarning: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configured := tt.configured
			if configured.Key == "" {
				configured = config.DefaultDRAEvictionNodeLabel()
			}
			b, err := New(WithConfig(config.NewConfig(
				config.WithDRAEvictionNodeLabel(configured))))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			values := make(map[string]map[string]any, len(tt.refs))
			for _, ref := range tt.refs {
				values[ref.Name] = map[string]any{}
			}

			if err := b.injectDRAEvictionLabel(values, &recipe.RecipeResult{ComponentRefs: tt.refs}); err != nil {
				t.Fatalf("injectDRAEvictionLabel() error = %v", err)
			}

			// Collect every matching warning rather than the first: taking
			// one and breaking cannot tell a correct single emission from a
			// regression that emits the same warning twice.
			var matched []string
			for _, w := range b.warnings {
				if strings.Contains(w, "kubelet plugin only on nodes labeled") {
					matched = append(matched, w)
				}
			}
			var got string
			if len(matched) > 0 {
				got = matched[0]
			}

			if !tt.wantWarning {
				if got != "" {
					t.Fatalf("unexpected DRA node-label warning: %q", got)
				}
				return
			}

			if got == "" {
				t.Fatalf("missing DRA node-label warning; warnings = %v", b.warnings)
			}
			wantCount := tt.wantCount
			if wantCount == 0 {
				wantCount = 1
			}
			if len(matched) != wantCount {
				t.Errorf("emitted %d node-label warnings, want %d; warnings = %v",
					len(matched), wantCount, matched)
			}
			if !strings.HasPrefix(got, "Warning: ") {
				t.Errorf("warning %q lacks the %q prefix used by other bundle warnings", got, "Warning: ")
			}
			if !strings.Contains(got, tt.wantSubstr) {
				t.Errorf("warning %q does not contain %q", got, tt.wantSubstr)
			}
			for _, want := range tt.alsoWant {
				if !strings.Contains(got, want) {
					t.Errorf("warning %q does not mention %q", got, want)
				}
			}

			for _, want := range []string{
				"node-pool provisioning time",
				"upgrading an existing cluster",
				"DESIRED=0",
				"no ResourceSlices",
				"Neither Helm nor deploy.sh reports an error either way",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("warning %q does not mention %q", got, want)
				}
			}
		})
	}
}

// TestInjectDRAEvictionLabel_OptOutByDefault covers the default introduced by
// issue #2469: without a configured eviction label AICR injects neither half
// of the contract, so the DRA kubelet plugin carries no AICR-introduced
// placement requirement.
func TestInjectDRAEvictionLabel_OptOutByDefault(t *testing.T) {
	tests := []struct {
		name    string
		draName string
		gpuName string
	}{
		{"standard components", draComponentName, gpuOperatorComponentName},
		{"OpenShift components", "nvidia-dra-driver-gpu-ocp", "gpu-operator-ocp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New()
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			values := map[string]map[string]any{
				tt.draName: {"kubeletPlugin": map[string]any{
					"nodeSelector": map[string]any{"node.dgxc.nvidia.com/has-gpu": "true"},
				}},
				tt.gpuName: {"driver": map[string]any{
					"manager": map[string]any{"env": []any{
						map[string]any{"name": "UNRELATED", "value": "preserved"},
					}},
				}},
			}
			rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: tt.gpuName}, {Name: tt.draName},
			}}

			if err := b.injectDRAEvictionLabel(values, rr); err != nil {
				t.Fatalf("injectDRAEvictionLabel() error = %v", err)
			}

			if got := dig(values[tt.draName], "kubeletPlugin", "nodeSelector", defaults.DRAEvictionNodeLabelKey); got != nil {
				t.Errorf("DRA eviction selector = %v, want absent when not opted in", got)
			}
			if got := dig(values[tt.draName], "kubeletPlugin", "nodeSelector", "node.dgxc.nvidia.com/has-gpu"); got != "true" {
				t.Errorf("accelerated selector = %v, want preserved", got)
			}
			if got := driverManagerEnvValues(values[tt.gpuName], draEvictionEnvName); len(got) != 0 {
				t.Errorf("Driver Manager eviction env = %v, want none when not opted in", got)
			}
			if got := driverManagerEnvValues(values[tt.gpuName], "UNRELATED"); len(got) != 1 || got[0] != "preserved" {
				t.Errorf("unrelated Driver Manager env = %v, want [preserved]", got)
			}
		})
	}
}

// TestWarnDRAEvictionNotConfigured pins the opt-out warning to the only
// configuration where the defect it describes can occur: GPU Operator managing
// the driver. A provider-installed driver deploys no Driver Manager.
func TestWarnDRAEvictionNotConfigured(t *testing.T) {
	tests := []struct {
		name         string
		driverValues map[string]any
		wantWarning  bool
	}{
		{"operator-managed driver warns", map[string]any{"enabled": true}, true},
		{"absent driver.enabled means enabled", map[string]any{}, true},
		{"missing driver block means enabled", nil, true},
		{"provider-installed driver stays quiet", map[string]any{"enabled": false}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New()
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			gpuValues := map[string]any{}
			if tt.driverValues != nil {
				gpuValues["driver"] = tt.driverValues
			}
			values := map[string]map[string]any{
				draComponentName:         {},
				gpuOperatorComponentName: gpuValues,
			}
			rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: gpuOperatorComponentName}, {Name: draComponentName},
			}}

			if err := b.injectDRAEvictionLabel(values, rr); err != nil {
				t.Fatalf("injectDRAEvictionLabel() error = %v", err)
			}

			var found bool
			for _, w := range b.warnings {
				if strings.Contains(w, "did not configure automatic eviction") {
					found = true
				}
			}
			if found != tt.wantWarning {
				t.Errorf("opt-out warning present = %v, want %v; warnings = %v", found, tt.wantWarning, b.warnings)
			}
		})
	}
}

// TestRejectDRAEvictionDynamicPaths_AllowedWhenNotOptedIn: AICR owns neither
// path unless the contract is opted into, so a --dynamic declaration on them
// is the user's own business.
func TestRejectDRAEvictionDynamicPaths_AllowedWhenNotOptedIn(t *testing.T) {
	refs := []recipe.ComponentRef{
		{Name: gpuOperatorComponentName}, {Name: draComponentName},
	}
	dynamic := map[string][]string{
		draComponentName:         {draEvictionNodeSelectorPath},
		gpuOperatorComponentName: {gpuOperatorDRAEvictionEnvPath},
	}

	if err := rejectDRAEvictionDynamicPaths(
		&recipe.RecipeResult{ComponentRefs: refs}, dynamic, config.NodeLabel{},
	); err != nil {
		t.Errorf("rejectDRAEvictionDynamicPaths() error = %v, want nil when not opted in", err)
	}

	if err := rejectDRAEvictionDynamicPaths(
		&recipe.RecipeResult{ComponentRefs: refs}, dynamic, config.DefaultDRAEvictionNodeLabel(),
	); err == nil {
		t.Error("rejectDRAEvictionDynamicPaths() = nil, want error when opted in")
	}
}

// TestWarnDRAEvictionNotConfigured_OCPAndMixedOperators covers the two shapes
// the single-operator table cannot: the -ocp component pair, and a recipe
// carrying two GPU Operator components where only one manages the driver. The
// warning is an OR across operators, because one operator-managed driver is
// enough to make a restart possible.
func TestWarnDRAEvictionNotConfigured_OCPAndMixedOperators(t *testing.T) {
	tests := []struct {
		name        string
		components  map[string]map[string]any
		refs        []recipe.ComponentRef
		wantWarning bool
	}{
		{
			name: "ocp pair with operator-managed driver warns",
			components: map[string]map[string]any{
				"nvidia-dra-driver-gpu-ocp": {},
				"gpu-operator-ocp":          {"driver": map[string]any{"enabled": true}},
			},
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator-ocp"}, {Name: "nvidia-dra-driver-gpu-ocp"},
			},
			wantWarning: true,
		},
		{
			name: "ocp pair with provider-installed driver stays quiet",
			components: map[string]map[string]any{
				"nvidia-dra-driver-gpu-ocp": {},
				"gpu-operator-ocp":          {"driver": map[string]any{"enabled": false}},
			},
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator-ocp"}, {Name: "nvidia-dra-driver-gpu-ocp"},
			},
			wantWarning: false,
		},
		{
			name: "one of two operators manages the driver warns",
			components: map[string]map[string]any{
				draComponentName:         {},
				gpuOperatorComponentName: {"driver": map[string]any{"enabled": false}},
				"gpu-operator-ocp":       {"driver": map[string]any{"enabled": true}},
			},
			refs: []recipe.ComponentRef{
				{Name: gpuOperatorComponentName}, {Name: "gpu-operator-ocp"}, {Name: draComponentName},
			},
			wantWarning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New()
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if err := b.injectDRAEvictionLabel(tt.components, &recipe.RecipeResult{ComponentRefs: tt.refs}); err != nil {
				t.Fatalf("injectDRAEvictionLabel() error = %v", err)
			}

			var optOut, placement bool
			for _, w := range b.warnings {
				if strings.Contains(w, "did not configure automatic eviction") {
					optOut = true
				}
				if strings.Contains(w, "schedules its kubelet plugin only on nodes labeled") {
					placement = true
				}
			}
			if optOut != tt.wantWarning {
				t.Errorf("opt-out warning = %v, want %v; warnings = %v", optOut, tt.wantWarning, b.warnings)
			}
			// The placement warning belongs to the opt-in path only; it must
			// never accompany the opt-out warning.
			if placement {
				t.Errorf("placement warning emitted without opt-in; warnings = %v", b.warnings)
			}
		})
	}
}
