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
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const nvsentinelComponentName = "nvsentinel"

func TestWarnNRIManagementAllowlist(t *testing.T) {
	result, err := recipe.NewBuilder().BuildFromCriteriaWithProfile(t.Context(),
		&recipe.Criteria{Service: recipe.CriteriaServiceRKE2, Intent: recipe.CriteriaIntentTraining}, "")
	if err != nil {
		t.Fatalf("BuildFromCriteriaWithProfile: %v", err)
	}
	sentinelRef := result.GetComponentRef(nvsentinelComponentName)
	if sentinelRef == nil || sentinelRef.Namespace == "" {
		t.Fatal("rke2 training recipe has no namespaced nvsentinel ref — the cases below would be vacuous")
	}
	gpuValues, err := result.GetValuesForComponentWithContext(t.Context(), gpuOperatorComponentName)
	if err != nil {
		t.Fatalf("GetValuesForComponentWithContext(gpu-operator): %v", err)
	}
	if got := nriManagementNamespaces(gpuValues); !slices.Contains(got, sentinelRef.Namespace) {
		t.Fatalf("rke2 gpu-operator allowlist = %v, want it to contain %q — the cases below would be vacuous", got, sentinelRef.Namespace)
	}

	// bundled returns the final per-component values map Make passes the
	// check: every enabled recipe component except the dropped ones, with
	// gpu-operator's real values (or an override of them).
	bundled := func(gpu map[string]any, dropped ...string) map[string]map[string]any {
		values := make(map[string]map[string]any)
		for _, ref := range result.ComponentRefs {
			if !ref.IsEnabled() || slices.Contains(dropped, ref.Name) {
				continue
			}
			values[ref.Name] = map[string]any{}
		}
		if !slices.Contains(dropped, gpuOperatorComponentName) {
			values[gpuOperatorComponentName] = gpu
		}
		return values
	}
	cleared := map[string]any{"toolkit": map[string]any{"env": []any{}}}

	tests := []struct {
		name        string
		bundlers    []string
		values      map[string]map[string]any
		wantWarning bool
	}{
		{name: "full rke2 bundle deploys nvsentinel", values: bundled(gpuValues)},
		{name: "nvsentinel dropped leaves the allowlist orphaned", values: bundled(gpuValues, nvsentinelComponentName), wantWarning: true},
		{name: "subset bundle may deploy nvsentinel elsewhere", bundlers: []string{gpuOperatorComponentName}, values: bundled(gpuValues, nvsentinelComponentName)},
		{name: "nvsentinel dropped with the allowlist cleared", values: bundled(cleared, nvsentinelComponentName)},
		{name: "no gpu-operator in the bundle", values: bundled(gpuValues, gpuOperatorComponentName, nvsentinelComponentName)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []config.Option
			if tt.bundlers != nil {
				opts = append(opts, config.WithBundlers(tt.bundlers))
			}
			b := &DefaultBundler{Config: config.NewConfig(opts...)}
			b.warnNRIManagementAllowlist(result, tt.values)

			if !tt.wantWarning {
				if len(b.warnings) != 0 {
					t.Fatalf("warnings = %v, want none", b.warnings)
				}
				return
			}
			if len(b.warnings) != 1 {
				t.Fatalf("warnings = %v, want exactly one", b.warnings)
			}
			for _, want := range []string{sentinelRef.Namespace, nriManagementNamespacesEnvName, "every GPU", "gpuoperator:toolkit.env=[]"} {
				if !strings.Contains(b.warnings[0], want) {
					t.Errorf("warning %q does not mention %q", b.warnings[0], want)
				}
			}
		})
	}
}

func TestNRIManagementNamespaces(t *testing.T) {
	tests := []struct {
		name string
		env  any
		want []string
	}{
		{name: "no toolkit env", env: nil, want: nil},
		{name: "other variables only", env: []any{map[string]any{"name": "CONTAINERD_SOCKET", "value": "/run/k3s/containerd/containerd.sock"}}, want: nil},
		{name: "single namespace", env: []any{map[string]any{"name": nriManagementNamespacesEnvName, "value": "nvsentinel"}}, want: []string{"nvsentinel"}},
		{name: "comma list with spaces and duplicates", env: []any{map[string]any{"name": nriManagementNamespacesEnvName, "value": " b, a,,b "}}, want: []string{"a", "b"}},
		{name: "malformed entries are skipped", env: []any{"nope", map[string]any{"name": nriManagementNamespacesEnvName, "value": 7}}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string]any{}
			if tt.env != nil {
				values["toolkit"] = map[string]any{"env": tt.env}
			}
			if got := nriManagementNamespaces(values); !slices.Equal(got, tt.want) {
				t.Errorf("nriManagementNamespaces() = %v, want %v", got, tt.want)
			}
		})
	}
}
