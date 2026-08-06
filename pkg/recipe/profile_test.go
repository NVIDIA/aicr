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

package recipe

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"gopkg.in/yaml.v3"
)

func testProfileDeclaration() *ProfileDeclaration {
	return &ProfileDeclaration{
		Name:        "gpuStack",
		Description: "Who installs the GPU driver and toolkit.",
		Default:     "driver-installed",
		Values: map[string]ProfileValue{
			"driver-installed": {
				ComponentRefs: []ProfileComponentRef{{
					Name: "gpu-operator",
					Overrides: map[string]any{
						"driver":  map[string]any{"enabled": false},
						"toolkit": map[string]any{"enabled": false},
					},
				}},
			},
			"operator-managed": {
				ComponentRefs: []ProfileComponentRef{{
					Name: "gpu-operator",
					Overrides: map[string]any{
						"driver":  map[string]any{"enabled": true},
						"toolkit": map[string]any{"enabled": true},
					},
				}},
			},
		},
	}
}

func testProfileStore(declaration *ProfileDeclaration) *MetadataStore {
	base := &RecipeMetadata{}
	base.Metadata.Name = baseRecipeName
	base.Spec.ComponentRefs = []ComponentRef{{
		Name: "gpu-operator",
		Type: ComponentTypeHelm,
	}}

	overlay := &RecipeMetadata{
		RecipeMetadataHeader: RecipeMetadataHeader{
			APIVersion: RecipeProfileAPIVersion,
			Kind:       RecipeMetadataKind,
		},
	}
	overlay.Metadata.Name = "aks"
	overlay.Spec.Criteria = &Criteria{Service: CriteriaServiceAKS}
	overlay.Spec.Profile = declaration

	return &MetadataStore{
		Base: base,
		Overlays: map[string]*RecipeMetadata{
			"aks": overlay,
		},
	}
}

func TestParseProfileSelection(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    *ProfileSelection
		wantErr bool
	}{
		{name: "empty uses default"},
		{
			name: "valid",
			raw:  "gpuStack=operator-managed",
			want: &ProfileSelection{Name: "gpuStack", Value: "operator-managed"},
		},
		{name: "missing equals", raw: "gpuStack", wantErr: true},
		{name: "multiple equals", raw: "gpuStack=x=y", wantErr: true},
		{name: "whitespace", raw: " gpuStack=x", wantErr: true},
		{name: "empty name", raw: "=x", wantErr: true},
		{name: "empty value", raw: "gpuStack=", wantErr: true},
		{name: "invalid character", raw: "gpuStack=x/y", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseProfileSelection(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseProfileSelection() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseProfileSelection() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestValidateProfileDeclaration(t *testing.T) {
	valid := testProfileDeclaration()
	withOverride := func(component string, overrides map[string]any) *ProfileDeclaration {
		return &ProfileDeclaration{
			Name:    "mode",
			Default: "one",
			Values: map[string]ProfileValue{
				"one": {ComponentRefs: []ProfileComponentRef{{Name: component, Overrides: overrides}}},
				"two": {ComponentRefs: []ProfileComponentRef{{Name: component, Overrides: overrides}}},
			},
		}
	}
	cyclicMap := map[string]any{}
	cyclicMap["self"] = cyclicMap
	cyclicSlice := make([]any, 1)
	cyclicSlice[0] = cyclicSlice
	pointerMap := &map[string]any{"enabled": true}
	sharedMap := map[string]any{"enabled": true}

	tests := []struct {
		name      string
		decl      *ProfileDeclaration
		wantOwned map[string][]string
		wantErr   string
	}{
		{
			name: "valid",
			decl: valid,
			wantOwned: map[string][]string{
				"gpu-operator": {"driver.enabled", "enabled", "toolkit.enabled"},
			},
		},
		{
			name: "list leaf is valid",
			decl: withOverride("gpu-operator", map[string]any{
				"tolerations": []any{map[string]any{"key": "gpu"}},
			}),
			wantOwned: map[string][]string{
				"gpu-operator": {"enabled", "tolerations"},
			},
		},
		{
			name: "maximum JSON round-trip-safe integer is valid",
			decl: withOverride("gpu-operator", map[string]any{
				"driver": int64(profileJSONSafeIntegerMax),
			}),
			wantOwned: map[string][]string{
				"gpu-operator": {"driver", "enabled"},
			},
		},
		{
			name: "integer outside JSON round-trip-safe range is unsupported",
			decl: withOverride("gpu-operator", map[string]any{
				"driver": uint64(profileJSONSafeIntegerMax + 1),
			}),
			wantErr: "outside the JSON round-trip-safe range",
		},
		{
			name: "typed list is unsupported",
			decl: withOverride("gpu-operator", map[string]any{
				"tolerations": []map[string]any{{"key": "gpu"}},
			}),
			wantErr: "unsupported list type",
		},
		{
			name: "pointer wrapped mapping is unsupported",
			decl: withOverride("gpu-operator", map[string]any{
				"driver": pointerMap,
			}),
			wantErr: "unsupported pointer type",
		},
		{
			name: "shared acyclic map is valid",
			decl: withOverride("gpu-operator", map[string]any{
				"driver":  sharedMap,
				"toolkit": sharedMap,
			}),
			wantOwned: map[string][]string{
				"gpu-operator": {"driver.enabled", "enabled", "toolkit.enabled"},
			},
		},
		{
			name: "function scalar is unsupported",
			decl: withOverride("gpu-operator", map[string]any{
				"driver": func() {},
			}),
			wantErr: "unsupported scalar type",
		},
		{
			name: "channel scalar is unsupported",
			decl: withOverride("gpu-operator", map[string]any{
				"driver": make(chan bool),
			}),
			wantErr: "unsupported scalar type",
		},
		{
			name: "struct scalar is unsupported",
			decl: withOverride("gpu-operator", map[string]any{
				"driver": struct{ Enabled bool }{Enabled: true},
			}),
			wantErr: "unsupported scalar type",
		},
		{
			name: "non-string nested mapping key",
			decl: withOverride("gpu-operator", map[string]any{
				"driver": map[any]any{42: false},
			}),
			wantErr: "non-string mapping key",
		},
		{
			name: "non-string mapping key inside list",
			decl: withOverride("gpu-operator", map[string]any{
				"tolerations": []any{map[any]any{42: "gpu"}},
			}),
			wantErr: "non-string mapping key",
		},
		{
			name: "unsupported typed map",
			decl: withOverride("gpu-operator", map[string]any{
				"driver": map[int]any{1: false},
			}),
			wantErr: "unsupported map type",
		},
		{
			name: "unsupported typed list containing typed map",
			decl: withOverride("gpu-operator", map[string]any{
				"tolerations": []map[int]any{{1: "gpu"}},
			}),
			wantErr: "unsupported list type",
		},
		{
			name: "cyclic map",
			decl: withOverride("gpu-operator", map[string]any{
				"driver": cyclicMap,
			}),
			wantErr: "cyclic reference",
		},
		{
			name: "cyclic slice",
			decl: withOverride("gpu-operator", map[string]any{
				"tolerations": cyclicSlice,
			}),
			wantErr: "cyclic reference",
		},
		{name: "nil declaration", wantErr: "required"},
		{
			name:    "invalid name",
			decl:    &ProfileDeclaration{Name: "gpu stack", Default: "one", Values: map[string]ProfileValue{"one": {}}},
			wantErr: "profile name",
		},
		{
			name:    "missing default value",
			decl:    &ProfileDeclaration{Name: "mode", Default: "missing", Values: map[string]ProfileValue{"one": {}}},
			wantErr: "is not a declared value",
		},
		{
			name:    "invalid value name",
			decl:    &ProfileDeclaration{Name: "mode", Default: "one", Values: map[string]ProfileValue{"one": {}, "bad/value": {}}},
			wantErr: "profile value",
		},
		{
			name:    "no values",
			decl:    &ProfileDeclaration{Name: "mode", Default: "one"},
			wantErr: "at least one value",
		},
		{
			name:    "literal dotted key",
			decl:    withOverride("gpu-operator", map[string]any{"driver.enabled": false}),
			wantErr: "literal dot",
		},
		{
			name:    "empty nested map",
			decl:    withOverride("gpu-operator", map[string]any{"driver": map[string]any{}}),
			wantErr: "empty map",
		},
		{
			name:    "root enabled assignment",
			decl:    withOverride("gpu-operator", map[string]any{"enabled": false}),
			wantErr: "may not assign overrides.enabled",
		},
		{
			name: "root enabled map assignment",
			decl: withOverride("gpu-operator", map[string]any{
				"enabled": map[string]any{"nested": true},
			}),
			wantErr: "may not assign overrides.enabled",
		},
		{
			name: "duplicate component",
			decl: &ProfileDeclaration{
				Name: "mode", Default: "one",
				Values: map[string]ProfileValue{
					"one": {ComponentRefs: []ProfileComponentRef{
						{Name: "gpu-operator"},
						{Name: "gpu-operator"},
					}},
				},
			},
			wantErr: "repeats componentRef",
		},
		{
			name: "union totality",
			decl: &ProfileDeclaration{
				Name: "mode", Default: "one",
				Values: map[string]ProfileValue{
					"one": {ComponentRefs: []ProfileComponentRef{{
						Name: "gpu-operator", Overrides: map[string]any{
							"driver": map[string]any{"enabled": true},
						},
					}}},
					"two": {ComponentRefs: []ProfileComponentRef{{
						Name: "gpu-operator", Overrides: map[string]any{
							"toolkit": map[string]any{"enabled": true},
						},
					}}},
				},
			},
			wantErr: "union totality",
		},
		{
			// Totality is evaluated over leaf-flattened override paths only,
			// before synthetic presence is added, so values may legitimately
			// differ in which components they reference presence-only. The
			// synthetic marker still lands in the declaration-wide OwnedPaths
			// for every referenced component, which is what locks nfd's
			// presence regardless of the selected value.
			name: "presence-only componentRef is exempt from union totality",
			decl: &ProfileDeclaration{
				Name: "mode", Default: "one",
				Values: map[string]ProfileValue{
					"one": {ComponentRefs: []ProfileComponentRef{
						{Name: "gpu-operator", Overrides: map[string]any{
							"driver": map[string]any{"enabled": true},
						}},
						{Name: "nfd"},
					}},
					"two": {ComponentRefs: []ProfileComponentRef{
						{Name: "gpu-operator", Overrides: map[string]any{
							"driver": map[string]any{"enabled": false},
						}},
					}},
				},
			},
			wantOwned: map[string][]string{
				"gpu-operator": {"driver.enabled", "enabled"},
				"nfd":          {"enabled"},
			},
		},
		{
			name: "constraint without a name",
			decl: &ProfileDeclaration{
				Name: "mode", Default: "one",
				Values: map[string]ProfileValue{
					"one": {Constraints: []Constraint{{Value: ">= 1.32"}}},
				},
			},
			wantErr: "declares a constraint with no name",
		},
		{
			name: "constraint without a value",
			decl: &ProfileDeclaration{
				Name: "mode", Default: "one",
				Values: map[string]ProfileValue{
					"one": {Constraints: []Constraint{{Name: "K8s.server.version"}}},
				},
			},
			wantErr: `constraint "K8s.server.version" has no value`,
		},
		{
			name: "repeated constraint name within a value",
			decl: &ProfileDeclaration{
				Name: "mode", Default: "one",
				Values: map[string]ProfileValue{
					"one": {Constraints: []Constraint{
						{Name: "K8s.server.version", Value: ">= 1.32"},
						{Name: "K8s.server.version", Value: ">= 1.33"},
					}},
				},
			},
			wantErr: `repeats constraint "K8s.server.version"`,
		},
		{
			name: "unknown advertiser rejected",
			decl: &ProfileDeclaration{
				Name: "mode", Default: "one",
				Values: map[string]ProfileValue{"one": {Advertiser: "csp"}},
			},
			wantErr: "unknown advertiser",
		},
		{
			// The GKE profile extension activated the reserved vocabulary:
			// "external" is the one valid non-empty advertiser value.
			name: "external advertiser accepted",
			decl: &ProfileDeclaration{
				Name: "mode", Default: "one",
				Values: map[string]ProfileValue{"one": {Advertiser: "external"}},
			},
			wantOwned: map[string][]string{},
		},
		{
			// Owning a #1327 policy-selector path is legal since the GKE
			// extension — it triggers the recomputed closure instead of
			// rejecting at catalog load.
			name: "allocation-policy selector path ownership accepted",
			decl: withOverride("gpu-operator", map[string]any{"devicePlugin": map[string]any{"enabled": false}}),
			wantOwned: map[string][]string{
				"gpu-operator": {"devicePlugin.enabled", "enabled"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owned, err := ValidateProfileDeclaration(tt.decl)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ValidateProfileDeclaration() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateProfileDeclaration() error = %v", err)
			}
			if !reflect.DeepEqual(owned, tt.wantOwned) {
				t.Fatalf("owned paths = %#v, want %#v", owned, tt.wantOwned)
			}
		})
	}
}

func TestValidateRecipeMetadataProfileYAMLScalars(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantErr  string
		wantTime bool
	}{
		{
			name:  "finite float accepted",
			value: "1.5",
		},
		{
			name:    "NaN rejected",
			value:   ".nan",
			wantErr: "non-finite float",
		},
		{
			name:    "infinity rejected",
			value:   ".inf",
			wantErr: "non-finite float",
		},
		{
			name:     "timestamp accepted",
			value:    "2026-07-28T00:00:00Z",
			wantTime: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var metadata RecipeMetadata
			data := fmt.Sprintf(`apiVersion: aicr.run/v1alpha3
kind: RecipeMetadata
metadata:
  name: scalar-test
spec:
  profile:
    name: mode
    default: one
    values:
      one:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                value: %s
`, tt.value)
			if err := yaml.Unmarshal([]byte(data), &metadata); err != nil {
				t.Fatalf("yaml.Unmarshal() error = %v", err)
			}
			scalar := metadata.Spec.Profile.Values["one"].ComponentRefs[0].
				Overrides["driver"].(map[string]any)["value"]
			if tt.wantTime {
				if _, ok := scalar.(time.Time); !ok {
					t.Fatalf("decoded scalar type = %T, want time.Time", scalar)
				}
			}

			err := ValidateRecipeMetadataProfile(&metadata)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateRecipeMetadataProfile() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateRecipeMetadataProfile() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestBuildRecipeResultWithProfile(t *testing.T) {
	ctx := context.Background()
	criteria := &Criteria{Service: CriteriaServiceAKS}
	store := testProfileStore(testProfileDeclaration())

	tests := []struct {
		name           string
		selection      string
		wantValue      string
		wantDriver     bool
		wantToolkit    bool
		wantErr        string
		wantAPIVersion string
	}{
		{
			name:           "declared default",
			wantValue:      "driver-installed",
			wantDriver:     false,
			wantToolkit:    false,
			wantAPIVersion: RecipeProfileAPIVersion,
		},
		{
			name:           "explicit value",
			selection:      "gpuStack=operator-managed",
			wantValue:      "operator-managed",
			wantDriver:     true,
			wantToolkit:    true,
			wantAPIVersion: RecipeProfileAPIVersion,
		},
		{name: "wrong profile name", selection: "other=operator-managed", wantErr: "declares profile"},
		{name: "unknown value", selection: "gpuStack=missing", wantErr: "valid values"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := store.BuildRecipeResultWithProfile(ctx, criteria, tt.selection)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("BuildRecipeResultWithProfile() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildRecipeResultWithProfile() error = %v", err)
			}
			if result.APIVersion != tt.wantAPIVersion {
				t.Fatalf("apiVersion = %q, want %q", result.APIVersion, tt.wantAPIVersion)
			}
			if result.Metadata.SelectedProfile == nil ||
				result.Metadata.SelectedProfile.Value != tt.wantValue {

				t.Fatalf("selectedProfile = %#v, want value %q", result.Metadata.SelectedProfile, tt.wantValue)
			}
			values, valuesErr := result.GetValuesForComponentWithContext(ctx, "gpu-operator")
			if valuesErr != nil {
				t.Fatalf("GetValuesForComponentWithContext() error = %v", valuesErr)
			}
			driver := values["driver"].(map[string]any)
			toolkit := values["toolkit"].(map[string]any)
			if driver["enabled"] != tt.wantDriver || toolkit["enabled"] != tt.wantToolkit {
				t.Fatalf("selected values driver=%v toolkit=%v, want driver=%v toolkit=%v",
					driver["enabled"], toolkit["enabled"], tt.wantDriver, tt.wantToolkit)
			}
		})
	}

	t.Run("selection without declaration fails", func(t *testing.T) {
		legacy := testProfileStore(nil)
		legacy.Overlays["aks"].APIVersion = RecipeAPIVersion
		result, err := legacy.BuildRecipeResultWithProfile(ctx, criteria, "gpuStack=operator-managed")
		if err == nil || result != nil {
			t.Fatalf("BuildRecipeResultWithProfile() = (%#v, %v), want error", result, err)
		}
	})

	t.Run("composition without declaration stays legacy", func(t *testing.T) {
		legacy := testProfileStore(nil)
		legacy.Overlays["aks"].APIVersion = RecipeAPIVersion
		result, err := legacy.BuildRecipeResult(ctx, criteria)
		if err != nil {
			t.Fatalf("BuildRecipeResult() error = %v", err)
		}
		if result.APIVersion != RecipeAPIVersion || result.Metadata.SelectedProfile != nil {
			t.Fatalf("legacy result apiVersion=%q selectedProfile=%#v",
				result.APIVersion, result.Metadata.SelectedProfile)
		}
	})
}

func TestProfileResolutionGuards(t *testing.T) {
	base := &RecipeMetadata{}
	base.Metadata.Name = baseRecipeName
	base.Spec.ComponentRefs = []ComponentRef{{Name: "gpu-operator", Type: ComponentTypeHelm}}

	newOverlay := func(name string, criteria *Criteria, profile *ProfileDeclaration) *RecipeMetadata {
		overlay := &RecipeMetadata{}
		overlay.Metadata.Name = name
		overlay.Spec.Criteria = criteria
		overlay.Spec.Profile = profile
		if profile != nil {
			overlay.APIVersion = RecipeProfileAPIVersion
		}
		return overlay
	}

	t.Run("typed declaration requires profile api version", func(t *testing.T) {
		overlay := newOverlay("service", &Criteria{Service: CriteriaServiceAKS}, testProfileDeclaration())
		overlay.APIVersion = RecipeAPIVersion
		store := &MetadataStore{
			Base:     base,
			Overlays: map[string]*RecipeMetadata{"service": overlay},
		}
		_, err := store.BuildRecipeResult(
			context.Background(),
			&Criteria{Service: CriteriaServiceAKS},
		)
		if err == nil || !strings.Contains(err.Error(), "declares spec.profile") {
			t.Fatalf("BuildRecipeResult() error = %v, want profile apiVersion failure", err)
		}
	})

	t.Run("independent declarations fail before composition", func(t *testing.T) {
		store := &MetadataStore{
			Base: base,
			Overlays: map[string]*RecipeMetadata{
				"service": newOverlay("service", &Criteria{Service: CriteriaServiceAKS}, testProfileDeclaration()),
				"gpu": newOverlay("gpu", &Criteria{Accelerator: CriteriaAcceleratorH100}, &ProfileDeclaration{
					Name: "other", Default: "one", Values: map[string]ProfileValue{"one": {}},
				}),
			},
		}
		_, err := store.BuildRecipeResult(context.Background(), &Criteria{
			Service: CriteriaServiceAKS, Accelerator: CriteriaAcceleratorH100,
		})
		if err == nil || !strings.Contains(err.Error(), "multiple profile declarations") {
			t.Fatalf("BuildRecipeResult() error = %v, want uniqueness failure", err)
		}
	})

	t.Run("same declaring ancestor reached twice is deduplicated", func(t *testing.T) {
		owner := newOverlay("owner", &Criteria{Service: CriteriaServiceAKS}, testProfileDeclaration())
		leafGPU := newOverlay("leaf-gpu", &Criteria{
			Service: CriteriaServiceAKS, Accelerator: CriteriaAcceleratorH100,
		}, nil)
		leafGPU.Spec.Base = "owner"
		leafIntent := newOverlay("leaf-intent", &Criteria{
			Service: CriteriaServiceAKS, Intent: CriteriaIntentTraining,
		}, nil)
		leafIntent.Spec.Base = "owner"
		store := &MetadataStore{
			Base: base,
			Overlays: map[string]*RecipeMetadata{
				"owner":       owner,
				"leaf-gpu":    leafGPU,
				"leaf-intent": leafIntent,
			},
		}
		result, err := store.BuildRecipeResult(context.Background(), &Criteria{
			Service: CriteriaServiceAKS, Accelerator: CriteriaAcceleratorH100,
			Intent: CriteriaIntentTraining,
		})
		if err != nil {
			t.Fatalf("BuildRecipeResult() error = %v", err)
		}
		if result.Metadata.SelectedProfile == nil {
			t.Fatal("selectedProfile is nil")
		}
	})

	t.Run("missing components are reported deterministically", func(t *testing.T) {
		declaration := &ProfileDeclaration{
			Name:    "mode",
			Default: "one",
			Values: map[string]ProfileValue{
				"one": {
					ComponentRefs: []ProfileComponentRef{
						{Name: "z-component"},
						{Name: "a-component"},
					},
				},
			},
		}
		store := &MetadataStore{
			Base: base,
			Overlays: map[string]*RecipeMetadata{
				"service": newOverlay(
					"service",
					&Criteria{Service: CriteriaServiceAKS},
					declaration,
				),
			},
		}
		for range 100 {
			_, err := store.BuildRecipeResult(
				context.Background(),
				&Criteria{Service: CriteriaServiceAKS},
			)
			if err == nil || !strings.Contains(err.Error(), `"a-component"`) {
				t.Fatalf("BuildRecipeResult() error = %v, want first missing component a-component", err)
			}
		}
	})

	t.Run("snapshot exclusion cannot remove declaration", func(t *testing.T) {
		owner := newOverlay("owner", &Criteria{Service: CriteriaServiceAKS}, testProfileDeclaration())
		owner.Spec.Constraints = []Constraint{{Name: "signal", Value: "installed"}}
		store := &MetadataStore{
			Base: base,
			Overlays: map[string]*RecipeMetadata{
				"owner": owner,
			},
		}
		_, err := store.BuildRecipeResultWithEvaluator(
			context.Background(),
			&Criteria{Service: CriteriaServiceAKS},
			func(_ Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{Passed: false, Actual: "operator"}
			},
		)
		if err == nil || !strings.Contains(err.Error(), "removed by snapshot constraint filtering") {
			t.Fatalf("BuildRecipeResultWithEvaluator() error = %v, want survival failure", err)
		}
	})

	t.Run("selected constraints fail closed", func(t *testing.T) {
		declaration := testProfileDeclaration()
		value := declaration.Values["driver-installed"]
		value.Constraints = []Constraint{{Name: "signal", Value: "driver-installed"}}
		declaration.Values["driver-installed"] = value
		store := testProfileStore(declaration)
		_, err := store.BuildRecipeResultWithEvaluator(
			context.Background(),
			&Criteria{Service: CriteriaServiceAKS},
			func(_ Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{Passed: false, Actual: "operator-managed"}
			},
		)
		if err == nil || !strings.Contains(err.Error(), "profile gpuStack=driver-installed constraint") {
			t.Fatalf("BuildRecipeResultWithEvaluator() error = %v, want profile constraint failure", err)
		}
	})

	t.Run("missing profile reading is distinguishable", func(t *testing.T) {
		declaration := testProfileDeclaration()
		value := declaration.Values["driver-installed"]
		value.Constraints = []Constraint{{Name: "signal", Value: "driver-installed"}}
		declaration.Values["driver-installed"] = value
		store := testProfileStore(declaration)
		_, err := store.BuildRecipeResultWithEvaluator(
			context.Background(),
			&Criteria{Service: CriteriaServiceAKS},
			func(_ Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{
					Error: aicrerrors.New(aicrerrors.ErrCodeNotFound, "missing"),
				}
			},
		)
		if err == nil || !strings.Contains(err.Error(), "reading \"signal\" is unavailable") {
			t.Fatalf("BuildRecipeResultWithEvaluator() error = %v, want unavailable-reading diagnostic", err)
		}
	})
}

func TestProfileArtifactContract(t *testing.T) {
	selected := &SelectedProfile{
		Name:  "gpuStack",
		Value: "driver-installed",
		OwnedPaths: map[string][]string{
			"gpu-operator": {"driver.enabled", "enabled"},
		},
	}
	profileResult := func(metadata RecipeResultMetadata) *RecipeResult {
		metadata.SelectedProfile = selected
		return &RecipeResult{
			APIVersion: RecipeProfileAPIVersion,
			Metadata:   metadata,
		}
	}
	configuredAccountingRecipeResult := func(selectedProfile *SelectedProfile) *RecipeResult {
		return &RecipeResult{
			APIVersion: RecipeProfileAPIVersion,
			Metadata:   RecipeResultMetadata{SelectedProfile: selectedProfile},
			Configuration: &RecipeConfiguration{
				Slurm: &SlurmConfiguration{
					Accounting: &SlurmAccountingConfiguration{Mode: AccountingModeDisabled},
				},
			},
		}
	}
	validWarning := ConstraintWarning{
		Overlay:    "overlay-a",
		Constraint: "signal",
		Expected:   "ready",
		Reason:     "constraint did not match",
	}
	warningWithoutConstraint := validWarning
	warningWithoutConstraint.Constraint = ""
	warningWithoutExpected := validWarning
	warningWithoutExpected.Expected = ""
	warningWithoutReason := validWarning
	warningWithoutReason.Reason = ""

	tests := []struct {
		name    string
		result  *RecipeResult
		wantErr string
	}{
		{name: "legacy", result: &RecipeResult{APIVersion: RecipeAPIVersion}},
		{
			name: "profile",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata:   RecipeResultMetadata{SelectedProfile: selected},
			},
		},
		{
			name: "profile metadata items",
			result: profileResult(RecipeResultMetadata{
				ExcludedOverlays: []ExcludedOverlay{{
					Name:   "overlay-a",
					Reason: ExcludedOverlayReasonMixinConstraintFailed,
				}},
				ConstraintWarnings: []ConstraintWarning{validWarning},
			}),
		},
		{
			name: "profile excluded overlay requires name",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{
					SelectedProfile: selected,
					ExcludedOverlays: []ExcludedOverlay{{
						Reason: ExcludedOverlayReasonConstraintFailed,
					}},
				},
			},
			wantErr: "excludedOverlays[0].name is required",
		},
		{
			name: "profile excluded overlay rejects unknown reason",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{
					SelectedProfile: selected,
					ExcludedOverlays: []ExcludedOverlay{{
						Name: "overlay-a", Reason: ExcludedOverlayReason("unknown"),
					}},
				},
			},
			wantErr: `excludedOverlays[0].reason "unknown" is unsupported`,
		},
		{
			name: "profile excluded overlay permits missing legacy reason",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{
					SelectedProfile:  selected,
					ExcludedOverlays: []ExcludedOverlay{{Name: "overlay-a"}},
				},
			},
		},
		{
			name:    "profile constraint warning requires overlay",
			result:  profileResult(RecipeResultMetadata{ConstraintWarnings: []ConstraintWarning{{}}}),
			wantErr: "constraintWarnings[0].overlay is required",
		},
		{
			name: "profile constraint warning requires constraint",
			result: profileResult(RecipeResultMetadata{
				ConstraintWarnings: []ConstraintWarning{warningWithoutConstraint},
			}),
			wantErr: "constraintWarnings[0].constraint is required",
		},
		{
			name: "profile constraint warning requires expected",
			result: profileResult(RecipeResultMetadata{
				ConstraintWarnings: []ConstraintWarning{warningWithoutExpected},
			}),
			wantErr: "constraintWarnings[0].expected is required",
		},
		{
			name: "profile constraint warning requires reason",
			result: profileResult(RecipeResultMetadata{
				ConstraintWarnings: []ConstraintWarning{warningWithoutReason},
			}),
			wantErr: "constraintWarnings[0].reason is required",
		},
		{
			name: "legacy with selection",
			result: &RecipeResult{
				APIVersion: RecipeAPIVersion,
				Metadata:   RecipeResultMetadata{SelectedProfile: selected},
			},
			wantErr: "cannot carry",
		},
		{
			name:    "profile version without selection",
			result:  &RecipeResult{APIVersion: RecipeProfileAPIVersion},
			wantErr: "requires",
		},
		{
			name:   "configured accounting version without profile selection",
			result: configuredAccountingRecipeResult(nil),
		},
		{
			name: "configured accounting validates excluded overlays",
			result: func() *RecipeResult {
				result := configuredAccountingRecipeResult(nil)
				result.Metadata.ExcludedOverlays = []ExcludedOverlay{{
					Reason: ExcludedOverlayReasonConstraintFailed,
				}}
				return result
			}(),
			wantErr: "excludedOverlays[0].name is required",
		},
		{
			name: "configured accounting validates constraint warnings",
			result: func() *RecipeResult {
				result := configuredAccountingRecipeResult(nil)
				result.Metadata.ConstraintWarnings = []ConstraintWarning{warningWithoutReason}
				return result
			}(),
			wantErr: "constraintWarnings[0].reason is required",
		},
		{
			name:   "profile selection and configured accounting coexist",
			result: configuredAccountingRecipeResult(selected),
		},
		{
			name:    "unknown version",
			result:  &RecipeResult{APIVersion: "aicr.run/v99"},
			wantErr: "unsupported",
		},
		{
			name: "unsorted ownership paths",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{SelectedProfile: &SelectedProfile{
					Name: "gpuStack", Value: "driver-installed",
					OwnedPaths: map[string][]string{"gpu-operator": {"enabled", "driver.enabled"}},
				}},
			},
			wantErr: "lexicographically sorted",
		},
		{
			name: "ownership component requires synthetic enabled path",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{SelectedProfile: &SelectedProfile{
					Name: "gpuStack", Value: "driver-installed",
					OwnedPaths: map[string][]string{"gpu-operator": {"driver.enabled"}},
				}},
			},
			wantErr: "must include synthetic path",
		},
		{
			name: "invalid selection identity",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{SelectedProfile: &SelectedProfile{
					Name: "gpu stack", Value: "driver-installed", OwnedPaths: map[string][]string{},
				}},
			},
			wantErr: "name and value",
		},
		{
			name: "unknown advertiser rejected",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{SelectedProfile: &SelectedProfile{
					Name: "gpuStack", Value: "driver-installed", Advertiser: "managed",
					OwnedPaths: map[string][]string{},
				}},
			},
			wantErr: "unknown advertiser",
		},
		{
			// "external" is valid since the GKE extension; the empty
			// ownedPaths map stays acceptable at this shape gate (path-level
			// coherence is the hydrating gate's job).
			name: "external advertiser accepted",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{SelectedProfile: &SelectedProfile{
					Name: "gpuStack", Value: "gke-default", Advertiser: "external",
					OwnedPaths: map[string][]string{},
				}},
			},
		},
		{
			name: "ownership map required",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{SelectedProfile: &SelectedProfile{
					Name: "gpuStack", Value: "driver-installed",
				}},
			},
			wantErr: "ownedPaths is required",
		},
		{
			name: "empty ownership component",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{SelectedProfile: &SelectedProfile{
					Name: "gpuStack", Value: "driver-installed",
					OwnedPaths: map[string][]string{"": {"enabled"}},
				}},
			},
			wantErr: "empty component name",
		},
		{
			name: "invalid ownership path",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{SelectedProfile: &SelectedProfile{
					Name: "gpuStack", Value: "driver-installed",
					OwnedPaths: map[string][]string{"gpu-operator": {".driver", "enabled"}},
				}},
			},
			wantErr: "invalid path",
		},
		{
			name: "duplicate ownership path",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{SelectedProfile: &SelectedProfile{
					Name: "gpuStack", Value: "driver-installed",
					OwnedPaths: map[string][]string{"gpu-operator": {"enabled", "enabled"}},
				}},
			},
			wantErr: "repeats path",
		},
		{
			// Owning a #1327 policy-selector path is legal since the GKE
			// extension: the recorded ownership triggers the recomputed
			// closure downstream instead of failing the shape gate.
			name: "allocation policy ownership accepted",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Metadata: RecipeResultMetadata{SelectedProfile: &SelectedProfile{
					Name: "gpuStack", Value: "driver-installed",
					OwnedPaths: map[string][]string{
						"gpu-operator": {"devicePlugin.enabled", "enabled"},
					},
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.ValidateProfileContract()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateProfileContract() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateProfileContract() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeRecipeResult_ProfileStrictness(t *testing.T) {
	valid := []byte(`apiVersion: aicr.run/v1alpha3
kind: RecipeResult
metadata:
  selectedProfile:
    name: gpuStack
    value: driver-installed
    ownedPaths: {}
componentRefs: []
`)
	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{name: "valid", data: valid},
		{
			name:    "unknown field",
			data:    []byte(strings.Replace(string(valid), "componentRefs:", "profie: typo\ncomponentRefs:", 1)),
			wantErr: "field profie not found",
		},
		{
			name: "unknown excluded overlay field",
			data: []byte(strings.Replace(
				string(valid),
				"  selectedProfile:",
				"  excludedOverlays:\n    - name: overlay-a\n      reasn: constraint-failed\n  selectedProfile:",
				1,
			)),
			wantErr: `unknown field "reasn"`,
		},
		{
			name: "excluded overlay must be object",
			data: []byte(strings.Replace(
				string(valid),
				"  selectedProfile:",
				"  excludedOverlays:\n    - 42\n  selectedProfile:",
				1,
			)),
			wantErr: "must be an object",
		},
		{
			name: "excluded overlay fields must be strings",
			data: []byte(strings.Replace(
				string(valid),
				"  selectedProfile:",
				"  excludedOverlays:\n    - name: 42\n  selectedProfile:",
				1,
			)),
			wantErr: `field "name"`,
		},
		{
			name: "excluded overlay requires name",
			data: []byte(strings.Replace(
				string(valid),
				"  selectedProfile:",
				"  excludedOverlays:\n    - reason: constraint-failed\n  selectedProfile:",
				1,
			)),
			wantErr: "excluded overlay object requires a non-empty name",
		},
		{
			name: "excluded overlay rejects unknown reason",
			data: []byte(strings.Replace(
				string(valid),
				"  selectedProfile:",
				"  excludedOverlays:\n    - name: overlay-a\n      reason: unknown\n  selectedProfile:",
				1,
			)),
			wantErr: `excludedOverlays[0].reason "unknown" is unsupported`,
		},
		{
			name: "constraint warning requires fields",
			data: []byte(strings.Replace(
				string(valid),
				"  selectedProfile:",
				"  constraintWarnings:\n    - {}\n  selectedProfile:",
				1,
			)),
			wantErr: "constraintWarnings[0].overlay is required",
		},
		{
			name:    "profile version requires kind",
			data:    []byte(strings.Replace(string(valid), "kind: RecipeResult\n", "", 1)),
			wantErr: `requires kind "RecipeResult"`,
		},
		{
			name:    "trailing document",
			data:    append(append([]byte(nil), valid...), []byte("---\nkind: RecipeResult\n")...),
			wantErr: "trailing document",
		},
		{
			name:    "legacy version with selected profile",
			data:    []byte(strings.Replace(string(valid), RecipeProfileAPIVersion, RecipeAPIVersion, 1)),
			wantErr: "cannot carry",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := DecodeRecipeResult(tt.data, serializer.FormatYAML)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("DecodeRecipeResult() error = %v", err)
				}
				if result.Metadata.SelectedProfile == nil {
					t.Fatal("selectedProfile is nil")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("DecodeRecipeResult() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeRecipeResult_ProfileStrictJSONExcludedOverlays(t *testing.T) {
	const prefix = `{
  "apiVersion": "aicr.run/v1alpha3",
  "kind": "RecipeResult",
  "metadata": {
    "excludedOverlays": [`
	const suffix = `],
    "selectedProfile": {
      "name": "gpuStack",
      "value": "driver-installed",
      "ownedPaths": {}
    }
  },
  "componentRefs": []
}`
	tests := []struct {
		name    string
		item    string
		wantErr string
	}{
		{
			name:    "unknown field",
			item:    `{"name":"overlay-a","reasn":"constraint-failed"}`,
			wantErr: `unknown field "reasn"`,
		},
		{
			name:    "numeric name",
			item:    `{"name":42}`,
			wantErr: "cannot unmarshal number",
		},
		{
			name:    "missing name",
			item:    `{"reason":"constraint-failed"}`,
			wantErr: "excluded overlay object requires a non-empty name",
		},
		{
			name:    "unknown reason",
			item:    `{"name":"overlay-a","reason":"unknown"}`,
			wantErr: `excludedOverlays[0].reason "unknown" is unsupported`,
		},
		{
			name:    "null item",
			item:    "null",
			wantErr: "must be an object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeRecipeResult(
				[]byte(prefix+tt.item+suffix),
				serializer.FormatJSON,
			)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("DecodeRecipeResult() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeRecipeResult_LegacyExcludedOverlaysCompatibility(t *testing.T) {
	tests := []struct {
		name   string
		format serializer.Format
		data   []byte
	}{
		{
			name:   "YAML scalar and additive object",
			format: serializer.FormatYAML,
			data: []byte(`apiVersion: aicr.run/v1alpha2
kind: RecipeResult
metadata:
  excludedOverlays:
    - overlay-a
    - name: overlay-b
      futureField: accepted
componentRefs: []
`),
		},
		{
			name:   "JSON scalar and additive object",
			format: serializer.FormatJSON,
			data: []byte(`{
  "apiVersion": "aicr.run/v1alpha2",
  "kind": "RecipeResult",
  "metadata": {
    "excludedOverlays": [
      "overlay-a",
      {"name": "overlay-b", "futureField": "accepted"}
    ]
  },
  "componentRefs": []
}`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := DecodeRecipeResult(tt.data, tt.format)
			if err != nil {
				t.Fatalf("DecodeRecipeResult() error = %v", err)
			}
			want := []ExcludedOverlay{{Name: "overlay-a"}, {Name: "overlay-b"}}
			if !reflect.DeepEqual(result.Metadata.ExcludedOverlays, want) {
				t.Errorf("excludedOverlays = %+v, want %+v", result.Metadata.ExcludedOverlays, want)
			}
		})
	}
}

func TestBuildMetadataStore_ProfileVersionMatrix(t *testing.T) {
	base := []byte(`apiVersion: aicr.run/v1alpha2
kind: RecipeMetadata
metadata:
  name: base
spec:
  componentRefs: []
`)
	validProfile := `profile:
    name: mode
    default: one
    values:
      one:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                enabled: false
      two:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                enabled: true
`
	overlay := func(version, extra string) []byte {
		return fmt.Appendf(nil, `apiVersion: %s
kind: RecipeMetadata
metadata:
  name: aks
spec:
  criteria:
    service: aks
  componentRefs: []
  %s
`, version, extra)
	}

	tests := []struct {
		name    string
		content []byte
		wantErr string
	}{
		{name: "legacy without profile", content: overlay(RecipeAPIVersion, "")},
		{name: "empty version without profile", content: overlay("", "")},
		{name: "unknown version without profile", content: overlay("aicr.run/v99", "")},
		{name: "profile version with declaration", content: overlay(RecipeProfileAPIVersion, validProfile)},
		{
			name:    "profile version without declaration",
			content: overlay(RecipeProfileAPIVersion, ""),
			wantErr: "has no spec.profile",
		},
		{
			name: "profile version rejects wrong kind",
			content: []byte(strings.Replace(
				string(overlay(RecipeProfileAPIVersion, validProfile)),
				"kind: RecipeMetadata", "kind: RecipeMetdata", 1,
			)),
			wantErr: `requires kind "RecipeMetadata", got "RecipeMetdata"`,
		},
		{
			name: "profile version requires metadata name",
			content: []byte(strings.Replace(
				string(overlay(RecipeProfileAPIVersion, validProfile)),
				"  name: aks\n", "", 1,
			)),
			wantErr: "requires metadata.name",
		},
		{
			name:    "legacy version with declaration",
			content: overlay(RecipeAPIVersion, validProfile),
			wantErr: "expected \"aicr.run/v1alpha3\"",
		},
		{
			name: "profile version rejects unknown root field",
			content: []byte(strings.Replace(
				string(overlay(RecipeProfileAPIVersion, validProfile)),
				"  profile:", "  profie:", 1,
			)),
			wantErr: "field profie not found",
		},
		{
			name: "profile version rejects expanded component ref",
			content: []byte(strings.Replace(
				string(overlay(RecipeProfileAPIVersion, validProfile)),
				"            overrides:", "            valuesFile: other.yaml\n            overrides:", 1,
			)),
			wantErr: "field valuesFile not found",
		},
		{
			name: "profile version rejects non-string override key",
			content: overlay(RecipeProfileAPIVersion, strings.Replace(
				validProfile,
				"              driver:\n                enabled: false",
				"              driver:\n                42: false",
				1,
			)),
			wantErr: "non-string mapping key",
		},
		{
			name: "profile version rejects multiple documents",
			content: append(
				overlay(RecipeProfileAPIVersion, validProfile),
				[]byte("---\nkind: RecipeMetadata\n")...,
			),
			wantErr: "multiple YAML documents",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newInMemoryProvider(tt.name, map[string][]byte{
				"overlays/base.yaml": base,
				"overlays/aks.yaml":  tt.content,
			})
			store, err := buildMetadataStore(context.Background(), provider)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("buildMetadataStore() error = %v", err)
				}
				if store.Overlays["aks"] == nil {
					t.Fatal("aks overlay was not loaded")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("buildMetadataStore() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestListCatalogWithProfiles_ProjectsEffectiveDeclaration(t *testing.T) {
	store := testProfileStore(testProfileDeclaration())
	leaf := &RecipeMetadata{}
	leaf.Metadata.Name = "h100-aks"
	leaf.Spec.Base = "aks"
	leaf.Spec.Criteria = &Criteria{
		Service:     CriteriaServiceAKS,
		Accelerator: CriteriaAcceleratorH100,
	}
	store.Overlays[leaf.Metadata.Name] = leaf

	entries, err := store.ListCatalogWithProfiles(t.Context(), &Criteria{
		Service: CriteriaServiceAKS,
	})
	if err != nil {
		t.Fatalf("ListCatalogWithProfiles() error = %v", err)
	}
	for _, entry := range entries {
		if entry.Name != leaf.Metadata.Name {
			continue
		}
		if entry.Profile == nil || entry.Profile.Name != "gpuStack" ||
			entry.Profile.Default != "driver-installed" ||
			!reflect.DeepEqual(entry.Profile.Values, []string{"driver-installed", "operator-managed"}) {

			t.Fatalf("leaf profile summary = %#v", entry.Profile)
		}
		return
	}
	t.Fatalf("catalog entries do not contain %q: %#v", leaf.Metadata.Name, entries)
}

func TestListCatalogWithProfilesCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	entries, err := testProfileStore(testProfileDeclaration()).
		ListCatalogWithProfiles(ctx, nil)
	if entries != nil {
		t.Fatalf("ListCatalogWithProfiles() entries = %#v, want nil", entries)
	}
	if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeTimeout, "")) {
		t.Fatalf("ListCatalogWithProfiles() error = %v, want ErrCodeTimeout", err)
	}
}

func TestObserveValuePath(t *testing.T) {
	values := map[string]any{
		"driver": map[string]any{
			"enabled": false,
			"null":    nil,
		},
		"blocked": "scalar",
	}
	tests := []struct {
		name  string
		path  string
		state PathState
	}{
		{name: "present", path: "driver.enabled", state: PathPresent},
		{name: "explicit null is present", path: "driver.null", state: PathPresent},
		{name: "absent", path: "driver.version", state: PathAbsent},
		{name: "blocked", path: "blocked.child", state: PathBlocked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ObserveValuePath(values, tt.path)
			if err != nil {
				t.Fatalf("ObserveValuePath() error = %v", err)
			}
			if got.State != tt.state {
				t.Fatalf("ObserveValuePath() state = %q, want %q", got.State, tt.state)
			}
		})
	}
}

func TestValidateProfileLock(t *testing.T) {
	ctx := context.Background()
	store := testProfileStore(testProfileDeclaration())
	result, err := store.BuildRecipeResult(ctx, &Criteria{Service: CriteriaServiceAKS})
	if err != nil {
		t.Fatalf("BuildRecipeResult() error = %v", err)
	}
	baseline, err := result.GetValuesForComponentWithContext(ctx, "gpu-operator")
	if err != nil {
		t.Fatalf("GetValuesForComponentWithContext() error = %v", err)
	}
	refs := append([]ComponentRef(nil), result.ComponentRefs...)
	if !result.OwnsProfilePath("gpu-operator", "driver.enabled") {
		t.Fatal("OwnsProfilePath() = false for exact owned path")
	}
	if result.OwnsProfilePath("gpu-operator", "driver.version") {
		t.Fatal("OwnsProfilePath() = true for unrelated path")
	}
	var nilResult *RecipeResult
	if nilResult.OwnsProfilePath("gpu-operator", "driver.enabled") {
		t.Fatal("nil RecipeResult owns a profile path")
	}

	tests := []struct {
		name    string
		refs    []ComponentRef
		values  map[string]map[string]any
		dynamic map[string][]string
		wantErr string
	}{
		{
			name: "identical",
			refs: refs,
			values: map[string]map[string]any{
				"gpu-operator": serializer.DeepCopyAnyMap(baseline),
			},
		},
		{
			name: "divergent leaf",
			refs: refs,
			values: map[string]map[string]any{
				"gpu-operator": func() map[string]any {
					values := serializer.DeepCopyAnyMap(baseline)
					values["driver"].(map[string]any)["enabled"] = true
					return values
				}(),
			},
			wantErr: "driver.enabled diverged",
		},
		{
			name: "blocked candidate",
			refs: refs,
			values: map[string]map[string]any{
				"gpu-operator": func() map[string]any {
					values := serializer.DeepCopyAnyMap(baseline)
					values["driver"] = "blocked"
					return values
				}(),
			},
			wantErr: "driver.enabled diverged",
		},
		{
			name:    "component omitted",
			refs:    nil,
			values:  map[string]map[string]any{},
			wantErr: "absent or disabled",
		},
		{
			name: "dynamic exact",
			refs: refs,
			values: map[string]map[string]any{
				"gpu-operator": serializer.DeepCopyAnyMap(baseline),
			},
			dynamic: map[string][]string{"gpu-operator": {"driver.enabled"}},
			wantErr: "intersects profile-owned",
		},
		{
			name: "dynamic ancestor",
			refs: refs,
			values: map[string]map[string]any{
				"gpu-operator": serializer.DeepCopyAnyMap(baseline),
			},
			dynamic: map[string][]string{"gpu-operator": {"driver"}},
			wantErr: "intersects profile-owned",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := result.ValidateProfileLock(ctx, tt.refs, tt.values, tt.dynamic)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateProfileLock() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateProfileLock() error = %v, want containing %q", err, tt.wantErr)
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("ValidateProfileLock() error code = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

type readCountingProvider struct {
	DataProvider
	reads int
}

func (p *readCountingProvider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	p.reads++
	return p.DataProvider.ReadFile(ctx, path)
}

func TestValidateProfileLockHydratesOwnedValuesOnce(t *testing.T) {
	ctx := t.Context()
	result, err := testProfileStore(testProfileDeclaration()).
		BuildRecipeResult(ctx, &Criteria{Service: CriteriaServiceAKS})
	if err != nil {
		t.Fatalf("BuildRecipeResult() error = %v", err)
	}

	const valuesFile = "components/gpu-operator/values.yaml"
	ref := result.GetComponentRef("gpu-operator")
	if ref == nil {
		t.Fatal("resolved recipe has no gpu-operator component")
	}
	ref.ValuesFile = valuesFile
	provider := &readCountingProvider{DataProvider: defaultEmbeddedProvider}
	result.BindDataProvider(provider)
	baseline, err := result.GetValuesForComponentWithContext(ctx, "gpu-operator")
	if err != nil {
		t.Fatalf("GetValuesForComponentWithContext() error = %v", err)
	}
	provider.reads = 0

	err = result.ValidateProfileLock(
		ctx,
		append([]ComponentRef(nil), result.ComponentRefs...),
		map[string]map[string]any{"gpu-operator": baseline},
		nil,
	)
	if err != nil {
		t.Fatalf("ValidateProfileLock() error = %v", err)
	}
	if provider.reads != 1 {
		t.Errorf("profile-owned values read count = %d, want 1", provider.reads)
	}
}

func TestValidateProfileValuesWithContextCanceled(t *testing.T) {
	result, err := testProfileStore(testProfileDeclaration()).
		BuildRecipeResult(t.Context(), &Criteria{Service: CriteriaServiceAKS})
	if err != nil {
		t.Fatalf("BuildRecipeResult() error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = result.ValidateProfileValuesWithContext(ctx)
	if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeTimeout, "")) {
		t.Fatalf("ValidateProfileValuesWithContext() error = %v, want ErrCodeTimeout", err)
	}
}

func TestValidateProfileValuesRejectsInvalidBaseline(t *testing.T) {
	tests := []struct {
		name    string
		result  *RecipeResult
		wantErr string
	}{
		{
			name: "missing components are checked deterministically",
			result: func() *RecipeResult {
				result := &RecipeResult{APIVersion: RecipeProfileAPIVersion}
				result.Metadata.SelectedProfile = &SelectedProfile{
					Name:  "gpuStack",
					Value: "driver-installed",
					OwnedPaths: map[string][]string{
						"z-component": {"enabled"},
						"a-component": {"enabled"},
					},
				}
				return result
			}(),
			wantErr: `"a-component" is missing or disabled`,
		},
		{
			name: "blocked recipe path",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				ComponentRefs: []ComponentRef{{
					Name:      "gpu-operator",
					Overrides: map[string]any{"driver": "blocked"},
				}},
				Metadata: RecipeResultMetadata{
					SelectedProfile: &SelectedProfile{
						Name:  "gpuStack",
						Value: "driver-installed",
						OwnedPaths: map[string][]string{
							"gpu-operator": {"driver.enabled"},
						},
					},
				},
			},
			wantErr: "profile-owned recipe path gpu-operator.driver.enabled is blocked by a non-map ancestor",
		},
		{
			// Generation cannot produce this: the selected fragment's overrides
			// are merged into the componentRef, and totality makes that fragment
			// assign every non-synthetic owned path. A hand-authored or
			// externally-supplied artifact can, and inheriting the value from
			// valuesFile would get it locked and attested as profile-qualified.
			name: "owned path absent from component overrides",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				ComponentRefs: []ComponentRef{{
					Name:      "gpu-operator",
					Overrides: map[string]any{"driver": map[string]any{"version": "570.86.16"}},
				}},
				Metadata: RecipeResultMetadata{
					SelectedProfile: &SelectedProfile{
						Name:  "gpuStack",
						Value: "driver-installed",
						OwnedPaths: map[string][]string{
							"gpu-operator": {"driver.enabled", "enabled"},
						},
					},
				},
			},
			wantErr: "profile-owned path gpu-operator.driver.enabled is not assigned by the component overrides",
		},
		{
			// A null ancestor is the case the inline and hydrated checks cannot
			// split between them: inline it observes as PathBlocked, and
			// mergeValues deletes a nil-valued key, so the hydrated observation
			// is PathAbsent. Deferring either state to the other check lets the
			// artifact through with driver.version inherited from the baseline.
			name: "owned path shadowed by a null ancestor in overrides",
			result: &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				Kind:       RecipeResultKind,
				ComponentRefs: []ComponentRef{{
					Name:      "gpu-operator",
					Type:      ComponentTypeHelm,
					Overrides: map[string]any{"driver": nil},
				}},
				Metadata: RecipeResultMetadata{
					SelectedProfile: &SelectedProfile{
						Name:  "gpuStack",
						Value: "preinstalled",
						OwnedPaths: map[string][]string{
							"gpu-operator": {"driver.version", "enabled"},
						},
					},
				},
			},
			wantErr: "profile-owned recipe path gpu-operator.driver.version is blocked by a non-map ancestor",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result.ValidateProfileValuesWithContext(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateProfileValuesWithContext() error = %v, want containing %q", err, tt.wantErr)
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("ValidateProfileValuesWithContext() error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

// TestApplyEffectiveProfileConstraints covers the two constraint outcomes of a
// selection: a name already present in the composed recipe is rejected rather
// than silently shadowing it, and a fresh name is appended in sorted order.
func TestApplyEffectiveProfileConstraints(t *testing.T) {
	newDecl := func(constraintName string) *effectiveProfileDeclaration {
		return &effectiveProfileDeclaration{
			Source: "test-overlay",
			Declaration: &ProfileDeclaration{
				Name: "gpuStack", Default: "preinstalled",
				Values: map[string]ProfileValue{
					"preinstalled": {
						ComponentRefs: []ProfileComponentRef{{
							Name:      "gpu-operator",
							Overrides: map[string]any{"driver": map[string]any{"enabled": false}},
						}},
						Constraints: []Constraint{{Name: constraintName, Value: ">= 1.32"}},
					},
				},
			},
		}
	}
	newSpec := func() *RecipeMetadataSpec {
		return &RecipeMetadataSpec{
			ComponentRefs: []ComponentRef{{Name: "gpu-operator", Type: ComponentTypeHelm}},
			Constraints:   []Constraint{{Name: "K8s.server.version", Value: ">= 1.30"}},
		}
	}

	t.Run("collision with the composed recipe is rejected", func(t *testing.T) {
		spec := newSpec()
		_, err := applyEffectiveProfile(spec, newDecl("K8s.server.version"), "", nil)
		if err == nil || !strings.Contains(err.Error(), "collides with the composed recipe") {
			t.Fatalf("applyEffectiveProfile() error = %v, want collision", err)
		}
		if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("applyEffectiveProfile() error = %v, want ErrCodeInvalidRequest", err)
		}
		if len(spec.Constraints) != 1 {
			t.Fatalf("composed constraints = %v, want the recipe's own left intact", spec.Constraints)
		}
	})

	t.Run("distinct constraint is merged", func(t *testing.T) {
		spec := newSpec()
		selected, err := applyEffectiveProfile(spec, newDecl("Driver.gpu.mode"), "", nil)
		if err != nil {
			t.Fatalf("applyEffectiveProfile() error = %v", err)
		}
		if selected == nil || selected.Value != "preinstalled" {
			t.Fatalf("selected profile = %#v, want value preinstalled", selected)
		}
		got := make([]string, 0, len(spec.Constraints))
		for _, constraint := range spec.Constraints {
			got = append(got, constraint.Name)
		}
		want := []string{"Driver.gpu.mode", "K8s.server.version"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("composed constraint names = %v, want %v", got, want)
		}
	})
}

// TestValidateProfileValuesAcceptsNullOwnedAssignment pins the boundary of the
// inline-ownership check: an explicit null is an assignment by the fragment, so
// it must not be read as the baseline-inheritance case the check rejects.
func TestValidateProfileValuesAcceptsNullOwnedAssignment(t *testing.T) {
	result := &RecipeResult{
		APIVersion: RecipeProfileAPIVersion,
		ComponentRefs: []ComponentRef{{
			Name:      "gpu-operator",
			Overrides: map[string]any{"driver": map[string]any{"enabled": nil}},
		}},
		Metadata: RecipeResultMetadata{
			SelectedProfile: &SelectedProfile{
				Name:  "gpuStack",
				Value: "driver-installed",
				OwnedPaths: map[string][]string{
					"gpu-operator": {"driver.enabled", "enabled"},
				},
			},
		},
	}
	if err := result.ValidateProfileValuesWithContext(t.Context()); err != nil {
		t.Fatalf("ValidateProfileValuesWithContext() error = %v, want nil", err)
	}
}

func TestValidateProfileValuesRejectsUnsupportedArtifactScalars(t *testing.T) {
	resultWithValue := func(value any) *RecipeResult {
		return &RecipeResult{
			APIVersion: RecipeProfileAPIVersion,
			Kind:       RecipeResultKind,
			ComponentRefs: []ComponentRef{{
				Name: "gpu-operator",
				Overrides: map[string]any{
					"driver": map[string]any{"enabled": value},
				},
			}},
			Metadata: RecipeResultMetadata{
				SelectedProfile: &SelectedProfile{
					Name:  "gpuStack",
					Value: "driver-installed",
					OwnedPaths: map[string][]string{
						"gpu-operator": {"driver.enabled", "enabled"},
					},
				},
			},
		}
	}
	decode := func(t *testing.T, input *RecipeResult, format serializer.Format) *RecipeResult {
		t.Helper()
		var (
			data []byte
			err  error
		)
		switch format {
		case serializer.FormatJSON:
			data, err = json.Marshal(input)
		case serializer.FormatYAML:
			data, err = yaml.Marshal(input)
		case serializer.FormatTable:
			t.Fatalf("table format is unsupported for profile artifacts")
		default:
			t.Fatalf("unsupported test format %q", format)
		}
		if err != nil {
			t.Fatalf("marshal profile artifact: %v", err)
		}
		result, err := DecodeRecipeResult(data, format)
		if err != nil {
			t.Fatalf("DecodeRecipeResult() error: %v", err)
		}
		return result
	}

	tests := []struct {
		name    string
		result  func(*testing.T) *RecipeResult
		wantErr string
	}{
		{
			name: "typed channel",
			result: func(*testing.T) *RecipeResult {
				return resultWithValue(make(chan bool))
			},
			wantErr: "unsupported scalar type",
		},
		{
			name: "typed cyclic map",
			result: func(*testing.T) *RecipeResult {
				result := resultWithValue(false)
				result.ComponentRefs[0].Overrides["driver"] =
					result.ComponentRefs[0].Overrides
				return result
			},
			wantErr: "cyclic reference",
		},
		{
			name: "YAML non-finite float",
			result: func(t *testing.T) *RecipeResult {
				return decode(t, resultWithValue(math.NaN()), serializer.FormatYAML)
			},
			wantErr: "non-finite float",
		},
		{
			name: "JSON integer outside round-trip-safe range",
			result: func(t *testing.T) *RecipeResult {
				return decode(t, resultWithValue(^uint64(0)), serializer.FormatJSON)
			},
			wantErr: "outside the JSON round-trip-safe range",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.result(t).ValidateProfileValuesWithContext(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateProfileValuesWithContext() error = %v, want containing %q", err, tt.wantErr)
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("ValidateProfileValuesWithContext() error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestValidateProfileValuesScopesArtifactValidationToOwnedPaths(t *testing.T) {
	newResult := func() *RecipeResult {
		return &RecipeResult{
			APIVersion: RecipeProfileAPIVersion,
			Kind:       RecipeResultKind,
			ComponentRefs: []ComponentRef{{
				Name: "gpu-operator",
			}},
			Metadata: RecipeResultMetadata{
				SelectedProfile: &SelectedProfile{
					Name:  "gpuStack",
					Value: "driver-installed",
					OwnedPaths: map[string][]string{
						"gpu-operator": {"driver.enabled", "enabled"},
					},
				},
			},
		}
	}

	tests := []struct {
		name    string
		prepare func(*RecipeResult)
		wantErr string
	}{
		{
			name: "typed list at unowned inline path",
			prepare: func(result *RecipeResult) {
				result.ComponentRefs[0].Overrides = map[string]any{
					"driver":  map[string]any{"enabled": true},
					"unowned": []string{"one", "two"},
				}
			},
		},
		{
			name: "large integer at unowned values-file path",
			prepare: func(result *RecipeResult) {
				const valuesFile = "components/gpu-operator/values.yaml"
				result.ComponentRefs[0].Overrides = map[string]any{
					"driver": map[string]any{"enabled": true},
				}
				result.ComponentRefs[0].ValuesFile = valuesFile
				result.BindDataProvider(newInMemoryProvider("unowned-large-integer", map[string][]byte{
					valuesFile: []byte("driver:\n  enabled: true\nunowned: 9007199254740992\n"),
				}))
			},
		},
		{
			// The owned value may not come from the values file at all. A
			// non-finite float there is therefore unreachable rather than
			// caught: the fragment's override wins the merge, so an owned-path
			// scalar is only ever validated from the overrides, which
			// TestValidateProfileValuesRejectsUnsupportedArtifactScalars covers.
			name: "owned path supplied only by the values file",
			prepare: func(result *RecipeResult) {
				const valuesFile = "components/gpu-operator/values.yaml"
				result.ComponentRefs[0].ValuesFile = valuesFile
				result.BindDataProvider(newInMemoryProvider("owned-non-finite", map[string][]byte{
					valuesFile: []byte("driver:\n  enabled: .nan\n"),
				}))
			},
			wantErr: "is not assigned by the component overrides",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := newResult()
			tt.prepare(result)
			err := result.ValidateProfileValuesWithContext(t.Context())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateProfileValuesWithContext() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateProfileValuesWithContext() error = %v, want containing %q", err, tt.wantErr)
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("ValidateProfileValuesWithContext() error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestValidateProfileValuesKustomizeOwnership(t *testing.T) {
	tests := []struct {
		name    string
		paths   []string
		wantErr string
	}{
		{
			name:  "presence-only ownership remains valid",
			paths: []string{"enabled"},
		},
		{
			name:    "values ownership is rejected",
			paths:   []string{"enabled", "feature.mode"},
			wantErr: `profile-owned component "kustomize-app" has type "Kustomize", which does not consume values overrides`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &RecipeResult{
				APIVersion: RecipeProfileAPIVersion,
				ComponentRefs: []ComponentRef{{
					Name:   "kustomize-app",
					Type:   ComponentTypeKustomize,
					Source: "https://github.com/example/app",
					Tag:    "v1.0.0",
					Path:   "deploy",
					Overrides: map[string]any{
						"feature": map[string]any{"mode": "profiled"},
					},
				}},
				Metadata: RecipeResultMetadata{
					SelectedProfile: &SelectedProfile{
						Name:       "mode",
						Value:      "profiled",
						OwnedPaths: map[string][]string{"kustomize-app": tt.paths},
					},
				},
			}

			err := result.ValidateProfileValuesWithContext(t.Context())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateProfileValuesWithContext() error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateProfileValuesWithContext() error = %v, want containing %q", err, tt.wantErr)
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("ValidateProfileValuesWithContext() error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

// TestPathsIntersect covers the exported helper directly. It is otherwise
// reached only through ownsAllocationPolicySelectorPath, the dynamic-path guard,
// and OwnsProfilePath, so a change to its prefix semantics would surface as a
// failure in one of those rather than here — and the ancestor/descendant cases
// below are precisely what the profile lock depends on.
func TestPathsIntersect(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "driver.enabled", "driver.enabled", true},
		{"b is ancestor of a", "driver.enabled", "driver", true},
		{"a is ancestor of b", "driver", "driver.enabled", true},
		{"common ancestor, divergent leaf", "driver.enabled", "driver.version", false},
		{"disjoint roots", "driver.enabled", "toolkit.enabled", false},
		{"single segment equal", "driver", "driver", true},
		{"single segment differing", "driver", "toolkit", false},
		{"empty both", "", "", true},
		{"empty vs populated", "", "driver", false},
		// Prefix comparison is per segment, not per character: "driver" must not
		// be treated as intersecting "driverRoot".
		{"segment prefix is not an intersection", "driver", "driverRoot", false},
		{"deep shared prefix", "a.b.c.d", "a.b.c", true},
		{"deep divergence", "a.b.c.d", "a.b.x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PathsIntersect(tt.a, tt.b); got != tt.want {
				t.Errorf("PathsIntersect(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			if got := PathsIntersect(tt.b, tt.a); got != tt.want {
				t.Errorf("PathsIntersect(%q, %q) = %v, want %v (must be symmetric)",
					tt.b, tt.a, got, tt.want)
			}
		})
	}
}

// TestValidateProfileDeclaration_CaseInsensitiveValueUniqueness pins the
// evidence-path invariant: lowercase segments are derived from value names,
// so names differing only by case must be rejected at catalog load.
func TestValidateProfileDeclaration_CaseInsensitiveValueUniqueness(t *testing.T) {
	decl := &ProfileDeclaration{
		Name:    "gpuStack",
		Default: "operator",
		Values: map[string]ProfileValue{
			"operator": {},
			"Operator": {},
		},
	}
	_, err := ValidateProfileDeclaration(decl)
	if err == nil || !strings.Contains(err.Error(), "differ only by case") {
		t.Fatalf("ValidateProfileDeclaration() error = %v, want case-uniqueness rejection", err)
	}
}

func TestValidateOwnershipDisjoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		first      OwnershipDomain
		second     OwnershipDomain
		wantErr    bool
		wantDetail string
	}{
		{
			name: "disjoint components",
			first: OwnershipDomain{
				Name:  "profile gpuStack=operator",
				Paths: map[string][]string{"gpu-operator": {"driver.enabled"}},
			},
			second: OwnershipDomain{
				Name:  "configuration.slurm.accounting.mode=disabled",
				Paths: map[string][]string{"slinky-slurm": {"accounting.enabled"}},
			},
		},
		{
			name: "disjoint paths on one component",
			first: OwnershipDomain{
				Name:  "first",
				Paths: map[string][]string{"component": {"driver.enabled"}},
			},
			second: OwnershipDomain{
				Name:  "second",
				Paths: map[string][]string{"component": {"toolkit.enabled"}},
			},
		},
		{
			name: "exact overlap",
			first: OwnershipDomain{
				Name:  "first",
				Paths: map[string][]string{"component": {"driver.enabled"}},
			},
			second: OwnershipDomain{
				Name:  "second",
				Paths: map[string][]string{"component": {"driver.enabled"}},
			},
			wantErr:    true,
			wantDetail: "component.driver.enabled",
		},
		{
			name: "first path is ancestor",
			first: OwnershipDomain{
				Name:  "first",
				Paths: map[string][]string{"component": {"driver"}},
			},
			second: OwnershipDomain{
				Name:  "second",
				Paths: map[string][]string{"component": {"driver.enabled"}},
			},
			wantErr:    true,
			wantDetail: "component.driver",
		},
		{
			name: "second path is ancestor",
			first: OwnershipDomain{
				Name:  "first",
				Paths: map[string][]string{"component": {"driver.enabled"}},
			},
			second: OwnershipDomain{
				Name:  "second",
				Paths: map[string][]string{"component": {"driver"}},
			},
			wantErr:    true,
			wantDetail: "component.driver.enabled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateOwnershipDisjoint(tt.first, tt.second)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateOwnershipDisjoint() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
					t.Fatalf("ValidateOwnershipDisjoint() error = %v, want ErrCodeInvalidRequest", err)
				}
				if !strings.Contains(err.Error(), tt.wantDetail) {
					t.Fatalf("ValidateOwnershipDisjoint() error = %v, want containing %q",
						err, tt.wantDetail)
				}
			}
		})
	}
}
