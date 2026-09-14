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

package validations

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const nvsentinelComponent = "nvsentinel"

// TestCheckNVSentinelDriverLabelDetectable covers the gate's decision
// table on synthetic recipes: every precondition in both directions.
func TestCheckNVSentinelDriverLabelDetectable(t *testing.T) {
	t.Parallel()

	sentinelRef := func(overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: nvsentinelComponent, Overrides: overrides}
	}
	gpuOpRef := func(name string, overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: name, Overrides: overrides}
	}
	driverOff := func() map[string]any {
		return map[string]any{"driver": map[string]any{"enabled": false}}
	}
	driverOn := func() map[string]any {
		return map[string]any{"driver": map[string]any{"enabled": true}}
	}
	assume := func(v any) map[string]any {
		return map[string]any{"labeler": map[string]any{"assumeDriverInstalled": v}}
	}
	result := func(service recipe.CriteriaServiceType, refs ...recipe.ComponentRef) *recipe.RecipeResult {
		return &recipe.RecipeResult{
			ComponentRefs: refs,
			Criteria:      &recipe.Criteria{Service: service},
		}
	}
	profiled := func(r *recipe.RecipeResult, name, value string) *recipe.RecipeResult {
		r.Metadata.SelectedProfile = &recipe.SelectedProfile{Name: name, Value: value}
		return r
	}
	aks := recipe.CriteriaServiceAKS
	gke := recipe.CriteriaServiceGKE

	tests := []struct {
		name          string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		conditions    map[string][]string
		wantBlocked   bool
		wantErrs      int
		// wantContains overrides the default blocked-message assertions
		// (used by the dynamic-guard rows, whose message differs).
		wantContains []string
		// wantNotContains asserts substrings absent from the blocked
		// message (the consumer-accurate message rows).
		wantNotContains []string
	}{
		{
			name:         "nil recipe result → skipped",
			recipeResult: nil,
		},
		{
			name:         "no nvsentinel ref → skipped",
			recipeResult: result(aks, gpuOpRef("gpu-operator", driverOff())),
		},
		{
			name: "nvsentinel disabled by recipe → skipped",
			recipeResult: result(aks,
				sentinelRef(map[string]any{"enabled": false}),
				gpuOpRef("gpu-operator", driverOff())),
		},
		{
			name: "nvsentinel disabled via --set alias → skipped",
			recipeResult: result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"enabled": "false"},
			})),
		},
		{
			name:         "no gpu-operator ref → skipped (no driver-ownership signal)",
			recipeResult: result(aks, sentinelRef(nil)),
		},
		{
			name: "gpu-operator disabled → skipped",
			recipeResult: result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", map[string]any{
					"enabled": false,
					"driver":  map[string]any{"enabled": false},
				})),
		},
		{
			name:         "conditions mismatch → skipped",
			recipeResult: result(aks, sentinelRef(nil), gpuOpRef("gpu-operator", driverOff())),
			conditions:   map[string][]string{"service": {"eks"}},
		},
		{
			name: "driver.enabled absent → chart default true → skipped",
			recipeResult: result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", map[string]any{})),
		},
		{
			name: "driver.enabled=true → operator driver pod exists → skipped",
			recipeResult: result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOn())),
		},
		{
			name: "non-boolean driver toggle → deferred to CheckDriverOwnershipCoherence",
			recipeResult: result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", map[string]any{
					"driver": map[string]any{"enabled": "false"},
				})),
		},
		{
			// The deferral above has no backstop on a subset bundle:
			// CheckDriverOwnershipCoherence is registered on the GPU
			// Operator and RunComponentValidations iterates the filtered
			// refs, so with only nvsentinel rendered nothing reports the
			// malformed toggle. Fail closed instead of deferring.
			name: "subset bundle + unreadable driver toggle → blocked (no sibling backstop)",
			recipeResult: func() *recipe.RecipeResult {
				r := result(recipe.CriteriaServiceOKE, sentinelRef(nil))
				return r.WithDeclaredComponents([]recipe.ComponentRef{
					{Name: nvsentinelComponent},
					{Name: "gpu-operator", Overrides: map[string]any{
						"driver": map[string]any{"enabled": float64(0)},
					}},
				})
			}(),
			wantBlocked: true,
			wantContains: []string{
				"driver ownership is unreadable", "bundlers= subset",
				nvsentinelAssumeDriverInstalledOverrideSet,
			},
		},
		{
			// The unreadable toggle must not short-circuit past the
			// exemptions: the remedy makes the label unconditional
			// whatever the operator's toggle says, so blocking here
			// would reject a bundle that is fine.
			name: "subset bundle + unreadable toggle + remedy set → skipped",
			recipeResult: func() *recipe.RecipeResult {
				r := result(recipe.CriteriaServiceOKE, sentinelRef(assume(true)))
				return r.WithDeclaredComponents([]recipe.ComponentRef{
					{Name: nvsentinelComponent},
					{Name: "gpu-operator", Overrides: map[string]any{
						"driver": map[string]any{"enabled": float64(0)},
					}},
				})
			}(),
		},
		{
			// Same: both label consumers disabled means nothing reads
			// the label, so an unreadable toggle is immaterial.
			name: "subset bundle + unreadable toggle + both consumers disabled → skipped",
			recipeResult: func() *recipe.RecipeResult {
				r := result(recipe.CriteriaServiceOKE, sentinelRef(map[string]any{
					"global": map[string]any{
						"metadataCollector":   map[string]any{"enabled": false},
						"syslogHealthMonitor": map[string]any{"enabled": false},
					},
				}))
				return r.WithDeclaredComponents([]recipe.ComponentRef{
					{Name: nvsentinelComponent},
					{Name: "gpu-operator", Overrides: map[string]any{
						"driver": map[string]any{"enabled": float64(0)},
					}},
				})
			}(),
		},
		{
			// The counterpart: with the operator rendered the sibling
			// runs, so this gate must stay silent rather than emit a
			// second message about the same value.
			name: "full bundle + unreadable driver toggle → still deferred",
			recipeResult: func() *recipe.RecipeResult {
				r := result(recipe.CriteriaServiceOKE, sentinelRef(nil),
					gpuOpRef("gpu-operator", map[string]any{
						"driver": map[string]any{"enabled": float64(0)},
					}))
				return r.WithDeclaredComponents([]recipe.ComponentRef{
					{Name: nvsentinelComponent},
					{Name: "gpu-operator", Overrides: map[string]any{
						"driver": map[string]any{"enabled": float64(0)},
					}},
				})
			}(),
		},
		{
			// Symmetric to the nvsentinel-values hard-error row below,
			// which trips a later branch: this one pins the gate's
			// fail-closed contract on the GPU OPERATOR's values, the
			// first resolution the gate performs. The scalar --set
			// traverses a path whose parent the recipe pins to a scalar,
			// which ApplyMapOverrides rejects.
			name: "gpu-operator overrides that cannot be applied → hard error (fails closed)",
			recipeResult: result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", map[string]any{"driver": "scalar"})),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"driver.enabled": "false"},
			})),
			wantErrs: 1,
		},
		{
			name: "AKS azure-managed: driver.enabled=false, flag unset → blocked",
			recipeResult: profiled(result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())), "gpuStack", "azure-managed"),
			wantBlocked: true,
		},
		{
			name: "AKS operator-managed: driver.enabled=true → skipped",
			recipeResult: profiled(result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOn())), "gpuStack", "operator-managed"),
		},
		{
			name: "GKE gke-default: driver.enabled=false, flag unset → blocked",
			recipeResult: profiled(result(gke, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())), "gpuStack", "gke-default"),
			wantBlocked: true,
		},
		{
			name: "GKE COS bundle-installer: bundle's installer DaemonSet supplies a driver pod → skipped",
			recipeResult: func() *recipe.RecipeResult {
				r := profiled(result(gke, sentinelRef(nil),
					gpuOpRef("gpu-operator", driverOff())), "gpuStack", "bundle-installer")
				r.Criteria.OS = recipe.CriteriaOSCOS
				return r
			}(),
		},
		{
			// Profile names are not reserved: an external overlay on any
			// service can declare a gpuStack profile with a value named
			// bundle-installer — with no installer DaemonSet ever
			// deploying. The exemption is scoped to the GKE COS shape the
			// embedded catalog documents; everything else fails closed.
			name: "non-GKE recipe borrowing the bundle-installer profile name → still blocked",
			recipeResult: profiled(result(recipe.CriteriaServiceOKE, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())), "gpuStack", "bundle-installer"),
			wantBlocked: true,
		},
		{
			name: "GKE non-COS recipe with the bundle-installer profile → still blocked (COS-only boundary)",
			recipeResult: func() *recipe.RecipeResult {
				r := profiled(result(gke, sentinelRef(nil),
					gpuOpRef("gpu-operator", driverOff())), "gpuStack", "bundle-installer")
				r.Criteria.OS = recipe.CriteriaOSUbuntu
				return r
			}(),
			wantBlocked: true,
		},
		{
			name: "bundle-installer under a different profile name → not exempt",
			recipeResult: profiled(result(gke, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())), "somethingElse", "bundle-installer"),
			wantBlocked: true,
		},
		{
			name: "unprofiled recipe (OKE): driver.enabled=false, flag unset → blocked",
			recipeResult: result(recipe.CriteriaServiceOKE, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())),
			wantBlocked: true,
		},
		{
			name: "OKE with the documented GPU-Operator-managed --set override → skipped",
			recipeResult: result(recipe.CriteriaServiceOKE, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"driver.enabled": "true"},
			})),
		},
		{
			name: "OCP variant resolved by name → blocked",
			recipeResult: result(recipe.CriteriaServiceOCP, sentinelRef(nil),
				gpuOpRef("gpu-operator-ocp", driverOff())),
			wantBlocked: true,
		},
		{
			// Pins the fail-closed contract the gate's doc comment states:
			// when the user's overrides cannot be reapplied to the
			// effective values, the gate returns a hard error rather than
			// skip. The scalar --set below traverses a path whose parent
			// the recipe pins to a scalar, which ApplyMapOverrides rejects.
			name: "override that cannot be applied → hard error (fails closed)",
			recipeResult: result(aks, sentinelRef(map[string]any{"labeler": "scalar"}),
				gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"labeler.assumeDriverInstalled": "true"},
			})),
			wantErrs: 1,
		},
		{
			name: "flag set in the recipe values → skipped",
			recipeResult: result(aks, sentinelRef(assume(true)),
				gpuOpRef("gpu-operator", driverOff())),
		},
		{
			name: "flag set to false in the recipe values → blocked",
			recipeResult: result(aks, sentinelRef(assume(false)),
				gpuOpRef("gpu-operator", driverOff())),
			wantBlocked: true,
		},
		{
			name: "flag set via the documented --set remedy → skipped",
			recipeResult: result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"labeler.assumeDriverInstalled": "true"},
			})),
		},
		{
			name: "flag set via --set under the canonical component name → skipped",
			recipeResult: result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				nvsentinelComponent: {"labeler.assumeDriverInstalled": "true"},
			})),
		},
		{
			name: "flag set to an empty map via --set-json → blocked (renders without the flag)",
			recipeResult: result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "nv-sentinel", Path: "labeler.assumeDriverInstalled", Value: map[string]any{}},
			})),
			wantBlocked: true,
		},
		{
			name: "flag set to a non-empty map via --set-json → truthy under Helm if → skipped",
			recipeResult: result(aks, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "nv-sentinel", Path: "labeler.assumeDriverInstalled", Value: map[string]any{"x": 1}},
			})),
		},
		{
			name: "--dynamic on the remedy path → blocked even with the static remedy set",
			recipeResult: result(aks, sentinelRef(assume(true)),
				gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"labeler.assumeDriverInstalled"},
			})),
			wantBlocked: true,
			wantContains: []string{
				"--dynamic nv-sentinel:labeler.assumeDriverInstalled",
				"cluster-values.yaml",
			},
		},
		{
			// Row (a) of the guard truth table — regression pinned on a
			// supported OKE shape: with both consumers statically disabled
			// and their enable paths static, nothing reads the label, so a
			// dynamic on the remedy path cannot recreate #2175 and is
			// allowed.
			name: "both consumers disabled + --dynamic on the remedy path → not gated (row a)",
			recipeResult: result(recipe.CriteriaServiceOKE, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"nv-sentinel": {
						"global.metadataCollector.enabled":   "false",
						"global.syslogHealthMonitor.enabled": "false",
					},
				}),
				config.WithDynamicValues(map[string][]string{
					"nv-sentinel": {"labeler.assumeDriverInstalled"},
				})),
		},
		{
			// Row (b1) — regression: OKE with both consumers disabled and
			// a static remedy on a static remedy path must not gate a
			// consumer-enable dynamic — an install-time re-enable finds
			// the label already applied.
			name: "consumers disabled + STATIC remedy + consumer-enable dynamic → not gated (row b1)",
			recipeResult: result(recipe.CriteriaServiceOKE, sentinelRef(map[string]any{
				"labeler": map[string]any{"assumeDriverInstalled": true},
				"global": map[string]any{
					"metadataCollector":   map[string]any{"enabled": false},
					"syslogHealthMonitor": map[string]any{"enabled": false},
				},
			}), gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.syslogHealthMonitor.enabled"},
			})),
		},
		{
			// Row (b2): the static remedy does not help when its own path
			// is ALSO dynamic — an install-time edit can strip the remedy
			// and re-enable a consumer.
			name: "consumers disabled + static remedy + remedy AND consumer-enable dynamics → blocked (row b2)",
			recipeResult: result(recipe.CriteriaServiceOKE, sentinelRef(map[string]any{
				"labeler": map[string]any{"assumeDriverInstalled": true},
				"global": map[string]any{
					"metadataCollector":   map[string]any{"enabled": false},
					"syslogHealthMonitor": map[string]any{"enabled": false},
				},
			}), gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"labeler.assumeDriverInstalled", "global.syslogHealthMonitor.enabled"},
			})),
			wantBlocked: true,
			wantContains: []string{
				"--dynamic nv-sentinel:global.syslogHealthMonitor.enabled",
				"absent or itself declared dynamic",
			},
		},
		{
			// Row (b) with the remedy also dynamic: the untrusted skip
			// wins — the consumer-enable guard blocks regardless of the
			// remedy declaration.
			name: "both consumers disabled + consumer-enable AND remedy dynamics → blocked (row b)",
			recipeResult: result(recipe.CriteriaServiceOKE, sentinelRef(map[string]any{
				"global": map[string]any{
					"metadataCollector":   map[string]any{"enabled": false},
					"syslogHealthMonitor": map[string]any{"enabled": false},
				},
			}), gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.metadataCollector.enabled", "labeler.assumeDriverInstalled"},
			})),
			wantBlocked: true,
			wantContains: []string{
				"--dynamic nv-sentinel:global.metadataCollector.enabled",
			},
		},
		{
			name: "--dynamic on a consumer-enable condition → blocked (could re-enable a skipped consumer)",
			recipeResult: result(aks, sentinelRef(map[string]any{
				"global": map[string]any{
					"metadataCollector":   map[string]any{"enabled": false},
					"syslogHealthMonitor": map[string]any{"enabled": false},
				},
			}), gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.syslogHealthMonitor.enabled"},
			})),
			wantBlocked: true,
			wantContains: []string{
				"--dynamic nv-sentinel:global.syslogHealthMonitor.enabled",
			},
		},
		{
			// Regression: on EKS the operator installs the driver, so
			// #2175 is unreachable and no install-time edit to the
			// consumer or remedy paths can create it — the guards are
			// scoped to platforms where the gate has a decision to protect.
			name: "EKS + --dynamic on a consumer-enable path → not gated (gate exits at driver-enabled)",
			recipeResult: result(recipe.CriteriaServiceEKS, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOn())),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.syslogHealthMonitor.enabled"},
			})),
		},
		{
			name: "EKS + --dynamic on the remedy path → not gated (same scoping)",
			recipeResult: result(recipe.CriteriaServiceEKS, sentinelRef(nil),
				gpuOpRef("gpu-operator", driverOn())),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"labeler.assumeDriverInstalled"},
			})),
		},
		{
			name: "GKE COS bundle-installer + --dynamic on the remedy path → not gated (observable driver pod)",
			recipeResult: func() *recipe.RecipeResult {
				r := profiled(result(recipe.CriteriaServiceGKE, sentinelRef(nil),
					gpuOpRef("gpu-operator", driverOff())), "gpuStack", "bundle-installer")
				r.Criteria.OS = recipe.CriteriaOSCOS
				return r
			}(),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"labeler.assumeDriverInstalled"},
			})),
		},
		{
			name: "--dynamic on an unrelated nvsentinel path → not gated",
			recipeResult: result(aks, sentinelRef(assume(true)),
				gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"platformConnector.resources.limits.cpu"},
			})),
		},
		{
			// The bundlers=nvsentinel subset-bundle shape: the filtered
			// refs carry only nvsentinel, and the pre-filter union
			// (attached by DefaultBundler.Make via WithDeclaredComponents)
			// carries the gpu-operator declaration whose
			// driver.enabled=false is the gate's platform evidence.
			name: "subset bundle: gpu-operator only in the declared union → still blocked",
			recipeResult: func() *recipe.RecipeResult {
				r := result(recipe.CriteriaServiceOKE, sentinelRef(nil))
				return r.WithDeclaredComponents([]recipe.ComponentRef{
					{Name: nvsentinelComponent},
					{Name: "gpu-operator", Overrides: driverOff()},
				})
			}(),
			wantBlocked: true,
		},
		{
			name: "subset bundle with the remedy set → passes",
			recipeResult: func() *recipe.RecipeResult {
				r := result(recipe.CriteriaServiceOKE, sentinelRef(assume(true)))
				return r.WithDeclaredComponents([]recipe.ComponentRef{
					{Name: nvsentinelComponent},
					{Name: "gpu-operator", Overrides: driverOff()},
				})
			}(),
		},
		{
			// Consumer-aware skip: metadata-collector and the syslog
			// monitors are the driver.installed label's consumers; with
			// BOTH explicitly disabled nothing reads the label.
			name: "both label consumers disabled → skipped (nothing reads the label)",
			recipeResult: result(aks, sentinelRef(map[string]any{
				"global": map[string]any{
					"metadataCollector":   map[string]any{"enabled": false},
					"syslogHealthMonitor": map[string]any{"enabled": false},
				},
			}), gpuOpRef("gpu-operator", driverOff())),
		},
		{
			name: "only metadata-collector disabled → still blocked, naming only the rendering consumers",
			recipeResult: result(aks, sentinelRef(map[string]any{
				"global": map[string]any{
					"metadataCollector": map[string]any{"enabled": false},
				},
			}), gpuOpRef("gpu-operator", driverOff())),
			wantBlocked: true,
			wantContains: []string{
				"syslog-health-monitor-regular, syslog-health-monitor-kata come up with 0 desired pods",
				nvsentinelAssumeDriverInstalledOverrideSet,
			},
			wantNotContains: []string{"metadata-collector,"},
		},
		{
			name: "only syslog disabled → still blocked, naming only the rendering consumer",
			recipeResult: result(aks, sentinelRef(map[string]any{
				"global": map[string]any{
					"syslogHealthMonitor": map[string]any{"enabled": false},
				},
			}), gpuOpRef("gpu-operator", driverOff())),
			wantBlocked: true,
			wantContains: []string{
				"metadata-collector come up with 0 desired pods",
				nvsentinelAssumeDriverInstalledOverrideSet,
			},
			wantNotContains: []string{"syslog-health-monitor"},
		},
		{
			name: "flag turned off via --set on a recipe that had it → blocked",
			recipeResult: result(aks, sentinelRef(assume(true)),
				gpuOpRef("gpu-operator", driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"labeler.assumeDriverInstalled": "false"},
			})),
			wantBlocked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.bundlerConfig
			if cfg == nil {
				// Every artifact-producing path carries a non-nil config;
				// the nil-config skip is pinned separately below.
				cfg = config.NewConfig()
			}
			msgs, errs := CheckNVSentinelDriverLabelDetectable(
				t.Context(), nvsentinelComponent, tt.recipeResult, cfg, tt.conditions)
			if len(errs) != tt.wantErrs {
				t.Fatalf("errs = %d (%v), want %d", len(errs), errs, tt.wantErrs)
			}
			for _, err := range errs {
				if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
					t.Errorf("hard error code = %v, want ErrCodeInvalidRequest", err)
				}
			}
			blocked := len(msgs) > 0
			if blocked != tt.wantBlocked {
				t.Fatalf("blocked = %v (msgs %v), want %v", blocked, msgs, tt.wantBlocked)
			}
			if !blocked {
				return
			}
			wantContains := tt.wantContains
			if wantContains == nil {
				wantContains = []string{
					nvsentinelAssumeDriverInstalledOverrideSet,
					"0 desired pods",
					"metadata-collector",
					"syslog-health-monitor-regular",
					"nvsentinel.dgxc.nvidia.com/driver.installed",
				}
			}
			for _, want := range wantContains {
				if !strings.Contains(msgs[0], want) {
					t.Errorf("message missing %q:\n%s", want, msgs[0])
				}
			}
			for _, notWant := range tt.wantNotContains {
				if strings.Contains(msgs[0], notWant) {
					t.Errorf("message unexpectedly contains %q:\n%s", notWant, msgs[0])
				}
			}
		})
	}
}

// TestNVSentinelDriverLabelPlatformMatrix runs the gate against recipes
// resolved from the real embedded catalog, one row per shipping platform
// and gpuStack profile value. Reasoning about the matrix on paper is what
// produced the false-positive risk this test exists to rule out: GKE COS
// gpuStack=bundle-installer has driver.enabled=false yet must NOT be
// rejected, because Google's standalone nvidia-driver-installer DaemonSet
// supplies a driver pod the labeler detects.
func TestNVSentinelDriverLabelPlatformMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		criteria    *recipe.Criteria
		profile     string
		overrides   map[string]map[string]string
		wantBlocked bool
	}{
		{
			// The azure-managed profile value now assigns
			// labeler.assumeDriverInstalled=true itself (#2181), so the
			// default AKS recipe bundles clean with no override.
			name: "AKS azure-managed (default, profile supplies the value)",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceAKS, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
			},
			wantBlocked: false,
		},
		{
			name: "AKS operator-managed",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceAKS, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
			},
			profile:     "gpuStack=operator-managed",
			wantBlocked: false,
		},
		{
			// Control against a vacuous pass on the row above: overriding
			// the profile-supplied value back to false must still block.
			// (Bundle generation rejects this override earlier still, at
			// the profile lock — this row pins the gate itself.)
			name: "AKS azure-managed with the profile value overridden to false",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceAKS, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
			},
			overrides: map[string]map[string]string{
				"nv-sentinel": {"labeler.assumeDriverInstalled": "false"},
			},
			wantBlocked: true,
		},
		{
			name: "GKE COS gke-default (default, profile supplies the value)",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining,
			},
			wantBlocked: false,
		},
		{
			name: "GKE COS bundle-installer",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining,
			},
			profile:     "gpuStack=bundle-installer",
			wantBlocked: false,
		},
		{
			// OKE has no gpuStack profile, so its value is set at overlay
			// level instead (#2181). The gate is satisfied identically;
			// what differs is that the value is not profile-locked.
			name: "OKE default (overlay supplies the value)",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceOKE, Accelerator: recipe.CriteriaAcceleratorA100,
				OS: recipe.CriteriaOSOracleLinux, Intent: recipe.CriteriaIntentTraining,
			},
			wantBlocked: false,
		},
		{
			name: "OKE with the documented GPU-Operator-managed override",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceOKE, Accelerator: recipe.CriteriaAcceleratorA100,
				OS: recipe.CriteriaOSOracleLinux, Intent: recipe.CriteriaIntentTraining,
			},
			overrides: map[string]map[string]string{
				"gpuoperator": {
					"driver.enabled":  "true",
					"toolkit.enabled": "true",
				},
			},
			wantBlocked: false,
		},
		{
			// The kind overlay supplies the value at overlay level (#2181),
			// which is what lets Client.BundleComponents — a nil-config path
			// with no --set channel — resolve a Kind recipe at all.
			name: "Kind (host-installed driver, overlay supplies the value)",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceKind, Accelerator: recipe.CriteriaAcceleratorH100,
				Intent: recipe.CriteriaIntentTraining,
			},
			wantBlocked: false,
		},
		{
			// Control against a vacuous pass on the row above.
			name: "Kind with the overlay value overridden to false → blocked",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceKind, Accelerator: recipe.CriteriaAcceleratorH100,
				Intent: recipe.CriteriaIntentTraining,
			},
			overrides: map[string]map[string]string{
				"nv-sentinel": {"labeler.assumeDriverInstalled": "false"},
			},
			wantBlocked: true,
		},
		{
			name: "Kind inference (host-installed driver, overlay supplies the value)",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceKind, Accelerator: recipe.CriteriaAcceleratorH100,
				Intent: recipe.CriteriaIntentInference,
			},
			wantBlocked: false,
		},
		{
			name: "EKS (GPU Operator installs the driver)",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
			},
			wantBlocked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := recipe.NewBuilder().BuildFromCriteriaWithProfile(t.Context(), tt.criteria, tt.profile)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile(%v, %q) failed: %v", tt.criteria, tt.profile, err)
			}
			// Control: a matrix row that silently lost its nvsentinel
			// component would pass the not-blocked expectation vacuously.
			if result.GetComponentRef(nvsentinelComponent) == nil {
				t.Fatalf("resolved recipe has no %s component ref", nvsentinelComponent)
			}
			// Control against a vacuous pass: the gate also skips when
			// the GPU Operator ref is absent or disabled, so a catalog
			// change removing/disabling it would let wantBlocked:false
			// rows keep passing while silently losing their coverage.
			gpuOpName, gpuOpRef, _ := resolveGPUOperatorRef(result)
			if gpuOpRef == nil {
				t.Fatal("resolved recipe has no GPU Operator ref; the gate would skip and the row would pass vacuously")
			}
			if !gpuOpRef.IsEnabled() {
				t.Fatalf("GPU Operator %q is disabled; the gate would skip and the row would pass vacuously", gpuOpName)
			}
			cfg := config.NewConfig()
			if tt.overrides != nil {
				cfg = config.NewConfig(config.WithValueOverrides(tt.overrides))
			}
			msgs, errs := CheckNVSentinelDriverLabelDetectable(
				t.Context(), nvsentinelComponent, result, cfg, nil)
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if blocked := len(msgs) > 0; blocked != tt.wantBlocked {
				t.Fatalf("blocked = %v, want %v (msgs: %v)", blocked, tt.wantBlocked, msgs)
			}
		})
	}
}

func TestHelmTruthy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value any
		want  bool
	}{
		{"nil", nil, false},
		{"bool true", true, true},
		{"bool false", false, false},
		{"empty string", "", false},
		// Helm's `if` treats any non-empty string as true, so a bundle
		// spelled --set-json ...='"false"' really would deploy with the
		// flag set; rejecting it would be a false positive.
		{"string false", "false", true},
		{"string true", "true", true},
		{"zero int", 0, false},
		{"nonzero int", 1, true},
		{"zero float", float64(0), false},
		{"nonzero float", float64(1), true},
		// Go's template `if` (text/template isTrue, which Helm uses)
		// treats empty collections as FALSE — an empty map here
		// renders the labeler WITHOUT --assume-driver-installed, so
		// the gate must not count it as the remedy (the F2 fail-open:
		// --set-json nv-sentinel:labeler.assumeDriverInstalled={}
		// used to pass the gate and ship the silent 0-desired state).
		{"empty map", map[string]any{}, false},
		{"non-empty map", map[string]any{"a": 1}, true},
		{"empty slice", []any{}, false},
		{"non-empty slice", []any{"x"}, true},
		{"zero int32", int32(0), false},
		{"zero uint", uint(0), false},
		{"zero float32", float32(0), false},
		{"nonzero int32", int32(3), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := helmTruthy(tt.value); got != tt.want {
				t.Errorf("helmTruthy(%#v) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// TestCheckNVSentinelDriverLabelDetectable_NilConfigRuns pins the
// values-only SDK path (Client.BundleComponents passes a nil bundler
// config). The gate used to no-op there because its only remedy was a
// --set flag that path cannot express. Since #2181 the recipes carry
// labeler.assumeDriverInstalled for every supported configuration that
// needs it, so the gate is satisfiable from resolved values alone and
// must verify this path like any other — a values-only caller may not
// receive values the bundle path would refuse to render.
func TestCheckNVSentinelDriverLabelDetectable_NilConfigRuns(t *testing.T) {
	t.Parallel()

	rr := &recipe.RecipeResult{
		Criteria: &recipe.Criteria{Service: recipe.CriteriaServiceAKS},
		ComponentRefs: []recipe.ComponentRef{
			{Name: nvsentinelComponent},
			{Name: "gpu-operator", Overrides: map[string]any{"driver": map[string]any{"enabled": false}}},
		},
	}

	// A recipe missing the value is blocked whether or not a config is
	// supplied — the nil-config exemption is gone.
	msgs, errs := CheckNVSentinelDriverLabelDetectable(
		t.Context(), nvsentinelComponent, rr, config.NewConfig(), nil)
	if len(errs) != 0 || len(msgs) != 1 {
		t.Fatalf("non-nil config: msgs = %v, errs = %v; want exactly one blocking message", msgs, errs)
	}

	msgs, errs = CheckNVSentinelDriverLabelDetectable(t.Context(), nvsentinelComponent, rr, nil, nil)
	if len(errs) != 0 || len(msgs) != 1 {
		t.Fatalf("nil config: msgs = %v, errs = %v; want exactly one blocking message", msgs, errs)
	}

	// Control against a gate that blocks unconditionally: a recipe that
	// supplies the value itself passes on the same nil-config path. This
	// is the shape #2181 gives every supported AKS/GKE-COS/OKE recipe.
	supplied := &recipe.RecipeResult{
		Criteria: &recipe.Criteria{Service: recipe.CriteriaServiceAKS},
		ComponentRefs: []recipe.ComponentRef{
			{Name: nvsentinelComponent, Overrides: map[string]any{
				"labeler": map[string]any{"assumeDriverInstalled": true},
			}},
			{Name: "gpu-operator", Overrides: map[string]any{"driver": map[string]any{"enabled": false}}},
		},
	}
	msgs, errs = CheckNVSentinelDriverLabelDetectable(t.Context(), nvsentinelComponent, supplied, nil, nil)
	if len(errs) != 0 || len(msgs) != 0 {
		t.Fatalf("nil config with the value supplied: msgs = %v, errs = %v; want it to pass", msgs, errs)
	}
}

// TestCheckNVSentinelDriverLabelDetectable_ReadsPinnedValues pins the
// read-once coherence contract (issue #1873 item A): when the caller
// pins resolved values via WithResolvedValues — as DefaultBundler.Make
// does with exactly the values the bundle emits — the gate reads the
// pinned snapshot, not a fresh provider/overrides resolution. The
// pinned values below carry the remedy while the component ref does
// not; the gate passing proves it validated what is emitted.
func TestCheckNVSentinelDriverLabelDetectable_ReadsPinnedValues(t *testing.T) {
	t.Parallel()

	rr := &recipe.RecipeResult{
		Criteria: &recipe.Criteria{Service: recipe.CriteriaServiceAKS},
		ComponentRefs: []recipe.ComponentRef{
			{Name: nvsentinelComponent},
			{Name: "gpu-operator", Overrides: map[string]any{"driver": map[string]any{"enabled": false}}},
		},
	}
	// Control: unpinned, the same recipe is blocked.
	msgs, errs := CheckNVSentinelDriverLabelDetectable(
		t.Context(), nvsentinelComponent, rr, config.NewConfig(), nil)
	if len(errs) != 0 || len(msgs) != 1 {
		t.Fatalf("control: msgs = %v, errs = %v; want exactly one blocking message", msgs, errs)
	}

	pinned := rr.WithResolvedValues(map[string]map[string]any{
		nvsentinelComponent: {"labeler": map[string]any{"assumeDriverInstalled": true}},
		"gpu-operator":      {"driver": map[string]any{"enabled": false}},
	})
	msgs, errs = CheckNVSentinelDriverLabelDetectable(
		t.Context(), nvsentinelComponent, pinned, config.NewConfig(), nil)
	if len(errs) != 0 || len(msgs) != 0 {
		t.Fatalf("pinned: msgs = %v, errs = %v; want the pinned remedy honored", msgs, errs)
	}
}
