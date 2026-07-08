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

package v1

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"gopkg.in/yaml.v3"
)

// draDriverRef builds a nvidia-dra-driver-gpu componentRef with the given
// switch/waiver values as inline overrides (no valuesFile — hydration merges
// overrides on top, so inline-only refs exercise the same lookup path).
func draDriverRef(gpusEnabled, override bool) recipe.ComponentRef {
	return recipe.ComponentRef{
		Name: "nvidia-dra-driver-gpu",
		Overrides: map[string]any{
			"gpuResourcesEnabledOverride": override,
			"resources": map[string]any{
				"gpus": map[string]any{"enabled": gpusEnabled},
			},
		},
	}
}

// gpuOperatorRef builds a gpu-operator componentRef pinning
// devicePlugin.enabled.
func gpuOperatorRef(devicePluginEnabled bool) recipe.ComponentRef {
	return recipe.ComponentRef{
		Name: "gpu-operator",
		Overrides: map[string]any{
			"devicePlugin": map[string]any{"enabled": devicePluginEnabled},
		},
	}
}

// TestResolveGPUAllocationPolicy pins the #1327 contract table: the seven
// configuration rows plus component-absent, component-disabled, and
// no-recipe-context resolution.
func TestResolveGPUAllocationPolicy(t *testing.T) {
	tests := []struct {
		name    string
		recipe  *recipe.RecipeResult
		want    string
		wantErr bool
		// wantMsg, when non-empty, must appear in the error text — pins the
		// actionable guidance on the promoted rejection rows.
		wantMsg string
	}{
		{
			// Row 1: production default.
			name: "gpus disabled, waiver off, device plugin on: device-plugin policy",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(false, false), gpuOperatorRef(true),
			}},
			want: GPUAllocationPolicyDevicePluginExtendedResource,
		},
		{
			// Row 2: the three-part experimental DRA opt-in (gpus.enabled=true
			// + waiver=true + devicePlugin.enabled=false) still resolves
			// dra-resource-claim cleanly after the row-3/5 promotion.
			name: "gpus enabled, waiver on, device plugin off: dra-resource-claim",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(true, true), gpuOperatorRef(false),
			}},
			want: GPUAllocationPolicyDRAResourceClaim,
		},
		{
			// Row 3: dual advertisement — rejected since the
			// production-default flip (was a transitional warning).
			name: "gpus enabled, waiver on, device plugin on: invalid (dual advertisement)",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(true, true), gpuOperatorRef(true),
			}},
			wantErr: true,
			wantMsg: "dual advertisement",
		},
		{
			// Row 4: the upstream chart install guard rejects this.
			name: "gpus enabled without waiver: invalid",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(true, false), gpuOperatorRef(true),
			}},
			wantErr: true,
		},
		{
			// Row 5: inert waiver — rejected since the production-default
			// flip (was a transitional warning).
			name: "gpus disabled with inert waiver, device plugin on: invalid (inert waiver)",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(false, true), gpuOperatorRef(true),
			}},
			wantErr: true,
			wantMsg: "inert waiver",
		},
		{
			// Row 6: no whole-GPU advertiser at all.
			name: "gpus disabled and device plugin disabled: invalid",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(false, false), gpuOperatorRef(false),
			}},
			wantErr: true,
		},
		{
			// Row 7: standalone runs.
			name:   "no recipe context: unspecified",
			recipe: nil,
			want:   GPUAllocationPolicyUnspecified,
		},
		{
			name: "DRA driver component absent: device-plugin policy",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				gpuOperatorRef(true),
			}},
			want: GPUAllocationPolicyDevicePluginExtendedResource,
		},
		{
			name: "DRA driver component disabled: device-plugin policy despite gpus.enabled=true",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				func() recipe.ComponentRef {
					ref := draDriverRef(true, true)
					ref.Overrides["enabled"] = false
					return ref
				}(),
				gpuOperatorRef(true),
			}},
			want: GPUAllocationPolicyDevicePluginExtendedResource,
		},
		{
			name: "DRA driver absent and device plugin explicitly disabled: invalid",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				gpuOperatorRef(false),
			}},
			wantErr: true,
		},
		{
			// The chart's DECLARED default is gpus.enabled=true, so an
			// enabled DRA component with the switch absent must fail closed
			// rather than silently resolve device-plugin while Helm would
			// deploy full-GPU DRA (or trip the chart guard). Stock recipes
			// always pin the value via the component values.
			name: "enabled DRA component without an explicit gpus.enabled: invalid",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: "nvidia-dra-driver-gpu"}, {Name: "gpu-operator"},
			}},
			wantErr: true,
		},
		{
			// Row 6 via component absence: no enabled GPU operator component
			// means no device-plugin advertiser — externally managed
			// advertisers are an explicit #1327 non-goal.
			name: "no GPU operator component at all and DRA disabled: invalid (no advertiser)",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(false, false),
			}},
			wantErr: true,
		},
		{
			name: "gpu-operator disabled and DRA disabled: invalid (no advertiser)",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(false, false),
				func() recipe.ComponentRef {
					ref := gpuOperatorRef(true)
					ref.Overrides["enabled"] = false
					return ref
				}(),
			}},
			wantErr: true,
		},
		{
			// OCP recipes disable gpu-operator and carry gpu-operator-ocp,
			// whose values pin devicePlugin.enabled: true.
			name: "gpu-operator-ocp provides the device-plugin advertiser",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(false, false),
				{
					Name: "gpu-operator-ocp",
					Overrides: map[string]any{
						"devicePlugin": map[string]any{"enabled": true},
					},
				},
			}},
			want: GPUAllocationPolicyDevicePluginExtendedResource,
		},
		{
			// The REAL OCP recipe shape (recipes/overlays/ocp.yaml): canonical
			// gpu-operator present but DISABLED, nvidia-dra-driver-gpu present
			// but DISABLED, gpu-operator-ocp enabled with the devicePlugin pin
			// — must resolve device-plugin, never the row-6 error.
			name: "real OCP shape: disabled gpu-operator + disabled DRA + enabled gpu-operator-ocp",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				func() recipe.ComponentRef {
					ref := draDriverRef(true, true) // values irrelevant once disabled
					ref.Overrides["enabled"] = false
					return ref
				}(),
				func() recipe.ComponentRef {
					ref := gpuOperatorRef(true)
					ref.Overrides["enabled"] = false
					return ref
				}(),
				{
					Name: "gpu-operator-ocp",
					Overrides: map[string]any{
						"devicePlugin": map[string]any{"enabled": true},
					},
				},
			}},
			want: GPUAllocationPolicyDevicePluginExtendedResource,
		},
		{
			name: "gpu-operator-ocp enabled without the devicePlugin key defaults to enabled",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: "gpu-operator-ocp"},
			}},
			want: GPUAllocationPolicyDevicePluginExtendedResource,
		},
		{
			// Both operator components enabled: gpu-operator wins (warn).
			name: "both gpu-operator and gpu-operator-ocp enabled: gpu-operator resolves the advertiser",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				gpuOperatorRef(true),
				{
					Name: "gpu-operator-ocp",
					Overrides: map[string]any{
						"devicePlugin": map[string]any{"enabled": false},
					},
				},
			}},
			want: GPUAllocationPolicyDevicePluginExtendedResource,
		},
		{
			// DRA opt-in stands on its own: no operator component needed.
			name: "DRA enabled with no GPU operator component: dra-resource-claim",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(true, true),
			}},
			want: GPUAllocationPolicyDRAResourceClaim,
		},
		{
			name: "non-boolean switch value: invalid (fail closed)",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{
					Name: "nvidia-dra-driver-gpu",
					Overrides: map[string]any{
						"resources": map[string]any{
							"gpus": map[string]any{"enabled": "true"},
						},
					},
				},
			}},
			wantErr: true,
		},
		{
			name: "non-map path segment: invalid (fail closed)",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{
					Name:      "nvidia-dra-driver-gpu",
					Overrides: map[string]any{"resources": "oops"},
				},
			}},
			wantErr: true,
		},
		{
			// Symmetric fail-closed coverage: the waiver shares the same
			// malformed-value contract as the switch.
			name: "non-boolean override waiver: invalid (fail closed)",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{
					Name: "nvidia-dra-driver-gpu",
					Overrides: map[string]any{
						"resources":                   map[string]any{"gpus": map[string]any{"enabled": true}},
						"gpuResourcesEnabledOverride": "yes",
					},
				},
			}},
			wantErr: true,
		},
		{
			// Explicit YAML null is the "empty this map" idiom — it must
			// behave exactly like an absent key, not like a malformed value
			// (devicePlugin: null == devicePlugin: {} == key absent).
			name: "devicePlugin: null treated as absent (chart default true)",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(false, false),
				{
					Name:      "gpu-operator",
					Overrides: map[string]any{"devicePlugin": nil},
				},
			}},
			want: GPUAllocationPolicyDevicePluginExtendedResource,
		},
		{
			// Null on the DRA resources map likewise reads as absent — and
			// an ENABLED DRA component with the switch absent is the
			// explicit-pin rejection, not a malformed-value error.
			name: "resources: null on enabled DRA component: explicit-pin rejection",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{
					Name:      "nvidia-dra-driver-gpu",
					Overrides: map[string]any{"resources": nil},
				},
				gpuOperatorRef(true),
			}},
			wantErr: true,
		},
		{
			// Symmetric fail-closed coverage: devicePlugin.enabled on the
			// operator component shares the malformed-value contract.
			name: "non-boolean devicePlugin.enabled: invalid (fail closed)",
			recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				draDriverRef(false, false),
				{
					Name: "gpu-operator",
					Overrides: map[string]any{
						"devicePlugin": map[string]any{"enabled": 1},
					},
				},
			}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveGPUAllocationPolicy(context.Background(), tt.recipe)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantMsg)
				}
				return
			}
			if got != tt.want {
				t.Errorf("policy = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToValidationInputWithContext(t *testing.T) {
	t.Run("nil recipe returns nil input and no error", func(t *testing.T) {
		got, err := ToValidationInputWithContext(context.Background(), nil)
		if err != nil || got != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("populates resolved policy", func(t *testing.T) {
		r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
			draDriverRef(true, true), gpuOperatorRef(false),
		}}
		got, err := ToValidationInputWithContext(context.Background(), r)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.GPUAllocationPolicy != GPUAllocationPolicyDRAResourceClaim {
			t.Errorf("GPUAllocationPolicy = %q, want %q",
				got.GPUAllocationPolicy, GPUAllocationPolicyDRAResourceClaim)
		}
	})

	t.Run("invalid allocation configuration fails closed", func(t *testing.T) {
		r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
			draDriverRef(true, false),
		}}
		if _, err := ToValidationInputWithContext(context.Background(), r); err == nil {
			t.Fatal("expected error for gpus.enabled=true without the chart-guard waiver")
		}
	})
}

// TestValidationInputGPUAllocationPolicyRoundTrip verifies the field survives
// the same serialization path the validator Job ConfigMap uses (yaml.Marshal
// → validation.yaml → deserialization in validators.LoadContext), and that
// it is omitted when empty (older consumers see no new field).
func TestValidationInputGPUAllocationPolicyRoundTrip(t *testing.T) {
	in := &ValidationInput{GPUAllocationPolicy: GPUAllocationPolicyDRAResourceClaim}
	data, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if !strings.Contains(string(data), "gpuAllocationPolicy: dra-resource-claim") {
		t.Fatalf("serialized form missing gpuAllocationPolicy key:\n%s", data)
	}
	var out ValidationInput
	if unmarshalErr := yaml.Unmarshal(data, &out); unmarshalErr != nil {
		t.Fatalf("unmarshal error: %v", unmarshalErr)
	}
	if out.GPUAllocationPolicy != GPUAllocationPolicyDRAResourceClaim {
		t.Errorf("round-trip policy = %q, want %q",
			out.GPUAllocationPolicy, GPUAllocationPolicyDRAResourceClaim)
	}

	empty, err := yaml.Marshal(&ValidationInput{})
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if strings.Contains(string(empty), "gpuAllocationPolicy") {
		t.Errorf("empty policy must be omitted (omitempty):\n%s", empty)
	}
}

func TestGetGPUAllocationPolicy(t *testing.T) {
	tests := []struct {
		name  string
		input *ValidationInput
		want  string
	}{
		{"nil input", nil, GPUAllocationPolicyUnspecified},
		{"empty field", &ValidationInput{}, GPUAllocationPolicyUnspecified},
		{"populated field",
			&ValidationInput{GPUAllocationPolicy: GPUAllocationPolicyDevicePluginExtendedResource},
			GPUAllocationPolicyDevicePluginExtendedResource},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.input.GetGPUAllocationPolicy(); got != tt.want {
				t.Errorf("GetGPUAllocationPolicy() = %q, want %q", got, tt.want)
			}
		})
	}
}
