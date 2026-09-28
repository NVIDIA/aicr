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
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// preflightOn is the values shape the nvsentinel-preflight mixin produces for
// the two fields these gates read.
func preflightOn(gangGroup string) map[string]any {
	return preflightOnWithAddr(gangGroup, "nvidia-dcgm.gpu-operator.svc:5555")
}

// preflightOnWithoutDCGMCheck turns preflight on with an explicit
// initContainers list that omits preflight-dcgm-diag. Unlike an absent list --
// which Helm fills with the chart's own default, DCGM check included -- this is
// the only way to genuinely opt out of the DCGM check.
func preflightOnWithoutDCGMCheck() map[string]any {
	return map[string]any{
		"global": map[string]any{"preflight": map[string]any{"enabled": true}},
		"preflight": map[string]any{
			"initContainers": []any{
				map[string]any{"name": "preflight-nccl-loopback"},
			},
		},
	}
}

// preflightOnWithAddr is preflightOn with an explicit DCGM_HOSTENGINE_ADDR, so
// the DCGM gate can be exercised against a retargeted or malformed address the
// way a leaf override or --set-json would produce one. addr == "" omits the
// initContainers list entirely, which leaves the chart's own default list in
// force -- so the DCGM check still runs, against the chart-default endpoint.
// preflightOnWithoutDCGMCheck is the genuine opt-out.
func preflightOnWithAddr(gangGroup, addr string) map[string]any {
	preflight := map[string]any{}
	if gangGroup != "" {
		preflight["gangCoordination"] = map[string]any{"enabled": true}
		preflight["gangDiscovery"] = map[string]any{
			"podGroupGVR": map[string]any{
				"group":    gangGroup,
				"version":  "v2alpha2",
				"resource": "podgroups",
			},
		}
	}
	if addr != "" {
		preflight["initContainers"] = []any{
			map[string]any{
				"name": "preflight-dcgm-diag",
				"env":  []any{map[string]any{"name": "DCGM_HOSTENGINE_ADDR", "value": addr}},
			},
		}
	}
	overrides := map[string]any{
		"global": map[string]any{"preflight": map[string]any{"enabled": true}},
	}
	if len(preflight) > 0 {
		overrides["preflight"] = preflight
	}
	return overrides
}

// preflightGangGVR builds a preflight block whose podGroupGVR parts are set
// individually, so a group-only match can be distinguished from a full one.
func preflightGangGVR(group, version, resource string) map[string]any {
	return map[string]any{
		"global": map[string]any{"preflight": map[string]any{"enabled": true}},
		"preflight": map[string]any{
			"gangCoordination": map[string]any{"enabled": true},
			"gangDiscovery": map[string]any{
				"podGroupGVR": map[string]any{"group": group, "version": version, "resource": resource},
			},
		},
	}
}

// TestCheckNVSentinelPreflightGangSchedulerRequired pins the fail-closed gate
// that the dependencyRefs edge alone does not provide: the bundler prunes an
// edge to a declared-but-disabled component, so ordering says nothing about
// whether kai-scheduler is actually installed.
func TestCheckNVSentinelPreflightGangSchedulerRequired(t *testing.T) {
	t.Parallel()

	sentinel := func(overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: nvsentinelComponent, Overrides: overrides}
	}
	kai := func(overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: "kai-scheduler", Overrides: overrides}
	}
	result := func(refs ...recipe.ComponentRef) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: refs}
	}

	tests := []struct {
		name          string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		wantBlocked   bool
	}{
		{
			name:         "nil recipe result -> skipped",
			recipeResult: nil,
		},
		{
			name:         "no nvsentinel ref -> skipped",
			recipeResult: result(kai(nil)),
		},
		{
			name:         "preflight off (chart default) -> skipped even without kai",
			recipeResult: result(sentinel(nil)),
		},
		{
			name:         "nvsentinel disabled -> skipped",
			recipeResult: result(sentinel(map[string]any{"enabled": false})),
		},
		{
			name:         "preflight on, gang coordination off -> skipped",
			recipeResult: result(sentinel(preflightOn(""))),
		},
		{
			// The chart ships gangCoordination.enabled: true, so omitting the key
			// leaves coordination ON. Reading absence as off would skip the
			// kai-scheduler requirement for values that still build a KAI
			// discoverer -- the controller then crash-loops on the missing
			// PodGroup GVR while failurePolicy: Ignore admits pods unchecked.
			name: "KAI GVR with gangCoordination unset -> chart default on, kai absent -> blocked",
			recipeResult: result(sentinel(map[string]any{
				"global": map[string]any{"preflight": map[string]any{"enabled": true}},
				"preflight": map[string]any{
					"gangDiscovery": map[string]any{
						"podGroupGVR": map[string]any{
							"group": "scheduling.run.ai", "version": "v2alpha2", "resource": "podgroups",
						},
					},
				},
			})),
			wantBlocked: true,
		},
		{
			// Only an explicit false disables it.
			name: "KAI GVR with gangCoordination explicitly false -> skipped",
			recipeResult: result(sentinel(map[string]any{
				"global": map[string]any{"preflight": map[string]any{"enabled": true}},
				"preflight": map[string]any{
					"gangCoordination": map[string]any{"enabled": false},
					"gangDiscovery": map[string]any{
						"podGroupGVR": map[string]any{
							"group": "scheduling.run.ai", "version": "v2alpha2", "resource": "podgroups",
						},
					},
				},
			})),
		},
		{
			name:         "preflight on with a non-KAI scheduler GVR -> skipped",
			recipeResult: result(sentinel(preflightOn("scheduling.x-k8s.io"))),
		},
		{
			// Rejected, not skipped: the controller resolves the whole GVR at
			// startup and exits when it is absent, so a wrong version
			// crash-loops it exactly like a missing CRD -- and failurePolicy
			// Ignore then admits every GPU pod unchecked.
			name:         "KAI group but wrong version -> blocked",
			recipeResult: result(sentinel(preflightGangGVR("scheduling.run.ai", "v1alpha1", "podgroups")), kai(nil)),
			wantBlocked:  true,
		},
		{
			name:         "KAI group but wrong resource -> blocked",
			recipeResult: result(sentinel(preflightGangGVR("scheduling.run.ai", "v2alpha2", "workloads")), kai(nil)),
			wantBlocked:  true,
		},
		{
			// The fail-open path: statically not KAI, so the gate would return
			// early -- but --dynamic lets an operator switch it to KAI at
			// install time with nothing validated.
			name:         "not targeting KAI statically, but --dynamic on the GVR -> blocked",
			recipeResult: result(sentinel(preflightOn("scheduling.x-k8s.io"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"preflight.gangDiscovery.podGroupGVR"},
			})),
			wantBlocked: true,
		},
		{
			name:         "preflight statically off, but --dynamic on global.preflight.enabled -> blocked",
			recipeResult: result(sentinel(nil)),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.preflight.enabled"},
			})),
			wantBlocked: true,
		},
		{
			// ...but an unrelated dynamic path must not block a recipe that
			// never uses preflight.
			name:         "preflight off, --dynamic on an unrelated path -> skipped",
			recipeResult: result(sentinel(nil)),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.tracing.endpoint"},
			})),
		},
		{
			name:         "complete KAI GVR with kai absent -> blocked",
			recipeResult: result(sentinel(preflightGangGVR("scheduling.run.ai", "v2alpha2", "podgroups"))),
			wantBlocked:  true,
		},
		{
			// --dynamic moves the value into cluster-values.yaml, loaded after
			// this gate runs, so kai could be disabled post-validation.
			name:         "kai enabled but --dynamic on its enabled flag -> blocked",
			recipeResult: result(sentinel(preflightOn("scheduling.run.ai")), kai(nil)),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"kai-scheduler": {"enabled"},
			})),
			wantBlocked: true,
		},
		{
			name:         "--dynamic on preflight gang discovery -> blocked",
			recipeResult: result(sentinel(preflightOn("scheduling.run.ai")), kai(nil)),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"preflight.gangDiscovery.podGroupGVR"},
			})),
			wantBlocked: true,
		},
		{
			name:         "preflight on, KAI GVR, kai enabled -> passes",
			recipeResult: result(sentinel(preflightOn("scheduling.run.ai")), kai(nil)),
		},
		{
			name:         "preflight on, KAI GVR, kai absent from the recipe -> blocked",
			recipeResult: result(sentinel(preflightOn("scheduling.run.ai"))),
			wantBlocked:  true,
		},
		{
			name:         "preflight on, KAI GVR, kai disabled by the recipe -> blocked",
			recipeResult: result(sentinel(preflightOn("scheduling.run.ai")), kai(map[string]any{"enabled": false})),
			wantBlocked:  true,
		},
		{
			name:         "preflight on, KAI GVR, kai disabled by --set -> blocked",
			recipeResult: result(sentinel(preflightOn("scheduling.run.ai")), kai(nil)),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"kai-scheduler": {"enabled": "false"},
			})),
			wantBlocked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.bundlerConfig
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelPreflightGangSchedulerRequired(
				t.Context(), nvsentinelComponent, tt.recipeResult, cfg, nil)
			gotBlocked := len(warnings) > 0 || len(errs) > 0
			if gotBlocked != tt.wantBlocked {
				t.Errorf("blocked = %v (warnings=%v errs=%v), want %v", gotBlocked, warnings, errs, tt.wantBlocked)
			}
		})
	}
}

// TestCheckNVSentinelPreflightDCGMReachable pins every way the fixed DCGM
// address can fail to resolve. The namespace case alone is not enough: the
// shipped Kind overlay disables the standalone hostengine entirely, which a
// namespace-only check would wave through.
func TestCheckNVSentinelPreflightDCGMReachable(t *testing.T) {
	t.Parallel()

	sentinel := func(overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: nvsentinelComponent, Overrides: overrides}
	}
	gpuOperator := func(namespace string) recipe.ComponentRef {
		return recipe.ComponentRef{Name: "gpu-operator", Namespace: namespace}
	}
	gpuOperatorWith := func(namespace string, overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: "gpu-operator", Namespace: namespace, Overrides: overrides}
	}
	// The OCP shape: recipes/overlays/ocp.yaml declares the OCP variant enabled
	// and the canonical name disabled, both in the gpu-operator namespace.
	gpuOperatorOCP := func(namespace string, overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: "gpu-operator-ocp", Namespace: namespace, Overrides: overrides}
	}
	dcgmEnabled := func(enabled any) map[string]any {
		return map[string]any{"dcgm": map[string]any{"enabled": enabled}}
	}
	result := func(refs ...recipe.ComponentRef) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: refs}
	}

	// A `bundlers=nvsentinel` subset: gpu-operator is declared by the recipe but
	// filtered out of the components actually being bundled. The check must read
	// it through the declared union for BOTH its ref and its values -- resolving
	// values against the filtered result fails and rejects a valid partial
	// bundle.
	subsetResult := func(refs []recipe.ComponentRef, declared []recipe.ComponentRef) *recipe.RecipeResult {
		return (&recipe.RecipeResult{ComponentRefs: refs}).WithDeclaredComponents(declared)
	}

	tests := []struct {
		name          string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		wantBlocked   bool
	}{
		{
			name:         "nil recipe result -> skipped",
			recipeResult: nil,
		},
		{
			name: "bundlers= subset excluding gpu-operator -> passes",
			recipeResult: subsetResult(
				[]recipe.ComponentRef{sentinel(preflightOn(""))},
				[]recipe.ComponentRef{sentinel(preflightOn("")), gpuOperatorWith("gpu-operator", dcgmEnabled(true))},
			),
		},
		{
			name: "bundlers= subset, declared gpu-operator has dcgm disabled -> blocked",
			recipeResult: subsetResult(
				[]recipe.ComponentRef{sentinel(preflightOn(""))},
				[]recipe.ComponentRef{sentinel(preflightOn("")), gpuOperatorWith("gpu-operator", dcgmEnabled(false))},
			),
			wantBlocked: true,
		},
		{
			// The reachable fail-open this gate previously had. gpu-operator's
			// chart defaults dcgm.enabled to false, so an unset key means no
			// standalone hostengine -- not "inherits an enabled default".
			name:         "dcgm.enabled unset -> chart default false -> blocked",
			recipeResult: result(sentinel(preflightOn("")), gpuOperator("gpu-operator")),
			wantBlocked:  true,
		},
		{
			// How that state is actually produced: --set-json deletes the key
			// together with AICR's own dcgm.enabled: true.
			name: "dcgm section explicitly null -> blocked",
			recipeResult: result(
				sentinel(preflightOn("")),
				gpuOperatorWith("gpu-operator", map[string]any{"dcgm": nil}),
			),
			wantBlocked: true,
		},
		{
			name: "dcgm.enabled explicitly null -> blocked",
			recipeResult: result(
				sentinel(preflightOn("")),
				gpuOperatorWith("gpu-operator", map[string]any{"dcgm": map[string]any{"enabled": nil}}),
			),
			wantBlocked: true,
		},
		{
			// The exact post-override state from the review:
			// --set-json gpuoperator:dcgm='{"enabled":null}' deletes enabled and
			// leaves dcgm as an empty map -- still a map, so a type assertion on
			// the section alone accepts it while enabled is gone.
			name: "dcgm present but empty map -> blocked",
			recipeResult: result(
				sentinel(preflightOn("")),
				gpuOperatorWith("gpu-operator", map[string]any{"dcgm": map[string]any{}}),
			),
			wantBlocked: true,
		},
		{
			name: "dcgm present as a non-map -> blocked",
			recipeResult: result(
				sentinel(preflightOn("")),
				gpuOperatorWith("gpu-operator", map[string]any{"dcgm": "enabled"}),
			),
			wantBlocked: true,
		},
		{
			name: "dcgm.enabled non-boolean -> blocked",
			recipeResult: result(
				sentinel(preflightOn("")),
				gpuOperatorWith("gpu-operator", map[string]any{"dcgm": map[string]any{"enabled": "yes"}}),
			),
			wantBlocked: true,
		},
		{
			// The exact production shape: pkg/bundler/bundler.go pins resolved
			// values for the FILTERED set and attaches the declared union, so
			// the pinned snapshot has no gpu-operator entry and the check must
			// fall through to resolving from the declared ref.
			name: "bundlers= subset with pinned values omitting gpu-operator -> passes",
			recipeResult: subsetResult(
				[]recipe.ComponentRef{sentinel(preflightOn(""))},
				[]recipe.ComponentRef{sentinel(preflightOn("")), gpuOperatorWith("gpu-operator", dcgmEnabled(true))},
			).WithResolvedValues(map[string]map[string]any{
				nvsentinelComponent: preflightOn(""),
			}),
		},
		{
			name:         "preflight off, gpu-operator relocated -> skipped",
			recipeResult: result(sentinel(nil), gpuOperator("privileged-gpu-operator")),
		},
		{
			name:         "preflight on, gpu-operator in its default namespace -> passes",
			recipeResult: result(sentinel(preflightOn("")), gpuOperatorWith("gpu-operator", dcgmEnabled(true))),
		},
		{
			name:         "preflight on, gpu-operator namespace unset -> passes",
			recipeResult: result(sentinel(preflightOn("")), gpuOperatorWith("", dcgmEnabled(true))),
		},
		{
			// Fails closed: with no gpu-operator nothing serves the Service.
			name:         "preflight on, no gpu-operator in the recipe -> blocked",
			recipeResult: result(sentinel(preflightOn(""))),
			wantBlocked:  true,
		},
		{
			name:         "preflight on, gpu-operator disabled -> blocked",
			recipeResult: result(sentinel(preflightOn("")), gpuOperatorWith("gpu-operator", map[string]any{"enabled": false})),
			wantBlocked:  true,
		},
		{
			// The shipped Kind overlay's configuration. A namespace-only check
			// passes this while the injected check cannot reach anything.
			name:         "preflight on, gpu-operator with dcgm.enabled false -> blocked",
			recipeResult: result(sentinel(preflightOn("")), gpuOperatorWith("gpu-operator", dcgmEnabled(false))),
			wantBlocked:  true,
		},
		{
			name:         "preflight on, gpu-operator with dcgm.enabled true -> passes",
			recipeResult: result(sentinel(preflightOn("")), gpuOperatorWith("gpu-operator", dcgmEnabled(true))),
		},
		{
			// The address is read, not assumed: a retargeted one that names
			// where gpu-operator actually is must pass.
			name: "retargeted address matching a relocated gpu-operator -> passes",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "nvidia-dcgm.privileged-gpu-operator.svc:5555")),
				gpuOperatorWith("privileged-gpu-operator", dcgmEnabled(true)),
			),
			wantBlocked: false,
		},
		{
			// ...and a bogus one must fail even though gpu-operator sits in
			// its normal namespace, which a hardcoded check would have passed.
			name: "bogus address while gpu-operator is in its default namespace -> blocked",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "nvidia-dcgm.does-not-exist.svc:5555")),
				gpuOperator("gpu-operator"),
			),
			wantBlocked: true,
		},
		{
			// A Service gpu-operator does not own, even in its namespace: an
			// independent hostengine, so gpu-operator's state is irrelevant and
			// rejecting it would be a false positive.
			name: "custom Service name in gpu-operator's namespace -> skipped",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "custom-hostengine.gpu-operator.svc:5555")),
				gpuOperatorWith("gpu-operator", dcgmEnabled(false)),
			),
		},
		{
			name: "custom Service name, gpu-operator disabled -> skipped",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "custom-hostengine.gpu-operator.svc:5555")),
				gpuOperatorWith("gpu-operator", map[string]any{"enabled": false}),
			),
		},
		{
			name: "custom Service name, --dynamic on gpu-operator dcgm.enabled -> skipped",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "custom-hostengine.gpu-operator.svc:5555")),
				gpuOperatorWith("gpu-operator", dcgmEnabled(true)),
			),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"dcgm.enabled"},
			})),
		},
		{
			// The relocation bug must still be caught: this IS gpu-operator's
			// Service name, pointed at a namespace gpu-operator is not in.
			name: "nvidia-dcgm Service in the wrong namespace -> blocked",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "nvidia-dcgm.gpu-operator.svc:5555")),
				gpuOperator("privileged-gpu-operator"),
			),
			wantBlocked: true,
		},
		{
			// An external hostengine is a deliberate choice this gate cannot
			// verify, so it must not guess.
			name: "external (non-cluster-local) address -> skipped",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "dcgm.example.com:5555")),
				gpuOperator("gpu-operator"),
			),
		},
		{
			// Helm replaces lists wholesale, so an unset preflight.initContainers
			// ships the chart's own list -- DCGM check included, pointed at
			// nvidia-dcgm.gpu-operator.svc:5555. Skipping here would pass a
			// bundle whose opted-in GPU pods strand on a hostengine that the
			// disabled dcgm never starts.
			name: "initContainers unset -> chart default DCGM check validated, dcgm disabled -> blocked",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "")),
				gpuOperatorWith("gpu-operator", dcgmEnabled(false)),
			),
			wantBlocked: true,
		},
		{
			name: "initContainers unset, dcgm enabled -> chart default reaches it",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "")),
				gpuOperatorWith("gpu-operator", dcgmEnabled(true)),
			),
		},
		{
			// gpuOperatorComponentNames lists the canonical name first, so taking
			// the first declared ref reports the disabled one and rejects a
			// bundle whose OCP variant serves that address perfectly well.
			name: "OCP: canonical gpu-operator disabled, gpu-operator-ocp enabled -> allowed",
			recipeResult: result(
				sentinel(preflightOn("")),
				gpuOperatorWith("gpu-operator", map[string]any{"enabled": false}),
				gpuOperatorOCP("gpu-operator", dcgmEnabled(true)),
			),
		},
		{
			// The enabled OCP variant is the one whose dcgm.enabled decides it.
			name: "OCP: gpu-operator-ocp enabled but dcgm disabled -> blocked",
			recipeResult: result(
				sentinel(preflightOn("")),
				gpuOperatorWith("gpu-operator", map[string]any{"enabled": false}),
				gpuOperatorOCP("gpu-operator", dcgmEnabled(false)),
			),
			wantBlocked: true,
		},
		{
			// With none enabled the disabled case is still reported, not the
			// absent one.
			name: "OCP: both GPU Operator variants disabled -> blocked",
			recipeResult: result(
				sentinel(preflightOn("")),
				gpuOperatorWith("gpu-operator", map[string]any{"enabled": false}),
				gpuOperatorOCP("gpu-operator", map[string]any{"enabled": false}),
			),
			wantBlocked: true,
		},
		{
			// A malformed preflight block is not a missing one: it must error
			// rather than silently validate the chart-default endpoint.
			name: "preflight values replaced by a scalar -> blocked",
			recipeResult: result(
				sentinel(map[string]any{
					"global":    map[string]any{"preflight": map[string]any{"enabled": true}},
					"preflight": "not-a-map",
				}),
				gpuOperatorWith("gpu-operator", dcgmEnabled(true)),
			),
			wantBlocked: true,
		},
		{
			// The only genuine opt-out: an explicit list without the check.
			name: "explicit initContainers without the DCGM check -> skipped",
			recipeResult: result(
				sentinel(preflightOnWithoutDCGMCheck()),
				gpuOperatorWith("gpu-operator", dcgmEnabled(false)),
			),
		},
		{
			// The right Service on a port it does not serve: every later check
			// passes, so without this the bundle ships and every opted-in GPU
			// pod fails to reach DCGM.
			name: "GPU Operator DCGM Service on the wrong port -> blocked",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "nvidia-dcgm.gpu-operator.svc:5556")),
				gpuOperatorWith("gpu-operator", dcgmEnabled(true)),
			),
			wantBlocked: true,
		},
		{
			name: "GPU Operator DCGM Service on the right port -> allowed",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "nvidia-dcgm.gpu-operator.svc:5555")),
				gpuOperatorWith("gpu-operator", dcgmEnabled(true)),
			),
		},
		{
			// An omitted port leaves the default to the DCGM client, which this
			// gate cannot determine -- so it is not rejected.
			name: "GPU Operator DCGM Service with no port -> allowed",
			recipeResult: result(
				sentinel(preflightOnWithAddr("", "nvidia-dcgm.gpu-operator.svc")),
				gpuOperatorWith("gpu-operator", dcgmEnabled(true)),
			),
		},
		{
			// The fail-open path: statically off, so the gate returns early --
			// but --dynamic lets an operator enable preflight at install time
			// with the DCGM endpoint never validated.
			name:         "preflight statically off, but --dynamic on global.preflight.enabled -> blocked",
			recipeResult: result(sentinel(nil), gpuOperatorWith("gpu-operator", dcgmEnabled(false))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.preflight.enabled"},
			})),
			wantBlocked: true,
		},
		{
			name:         "preflight off, --dynamic on an unrelated path -> skipped",
			recipeResult: result(sentinel(nil), gpuOperatorWith("gpu-operator", dcgmEnabled(false))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.tracing.endpoint"},
			})),
		},
		{
			name:         "--dynamic on gpu-operator dcgm.enabled -> blocked",
			recipeResult: result(sentinel(preflightOn("")), gpuOperatorWith("gpu-operator", dcgmEnabled(true))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"dcgm.enabled"},
			})),
			wantBlocked: true,
		},
		{
			// No DCGM check is configured, so there is no gpu-operator
			// dependency to protect -- guarding its paths here would reject a
			// legitimate recipe for a value this gate never reads. An explicit
			// list is required: an absent one means the chart default runs, and
			// that does depend on gpu-operator.
			name:         "no DCGM check configured, --dynamic on gpu-operator dcgm.enabled -> skipped",
			recipeResult: result(sentinel(preflightOnWithoutDCGMCheck()), gpuOperatorWith("gpu-operator", dcgmEnabled(true))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"dcgm.enabled"},
			})),
		},
		{
			// Same reasoning for an external hostengine: gpu-operator does not
			// serve it, so its dynamics are irrelevant to this check.
			name:         "external DCGM address, --dynamic on gpu-operator dcgm.enabled -> skipped",
			recipeResult: result(sentinel(preflightOnWithAddr("", "dcgm.example.com:5555")), gpuOperatorWith("gpu-operator", dcgmEnabled(true))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"dcgm.enabled"},
			})),
		},
		{
			// ...but a cluster-local address in gpu-operator's own namespace
			// does establish the dependency, so the guard must still fire.
			name:         "retargeted in-cluster address, --dynamic on gpu-operator enabled -> blocked",
			recipeResult: result(sentinel(preflightOnWithAddr("", "nvidia-dcgm.privileged-gpu-operator.svc:5555")), gpuOperator("privileged-gpu-operator")),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"enabled"},
			})),
			wantBlocked: true,
		},
		{
			name:         "--dynamic on preflight.initContainers -> blocked",
			recipeResult: result(sentinel(preflightOn("")), gpuOperatorWith("gpu-operator", dcgmEnabled(true))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"preflight.initContainers"},
			})),
			wantBlocked: true,
		},
		{
			name:         "preflight on, gpu-operator relocated (os-talos) -> blocked",
			recipeResult: result(sentinel(preflightOn("")), gpuOperator("privileged-gpu-operator")),
			wantBlocked:  true,
		},
		{
			name:         "preflight on, gpu-operator relocated anywhere else -> blocked",
			recipeResult: result(sentinel(preflightOn("")), gpuOperator("nvidia-gpu-operator")),
			wantBlocked:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.bundlerConfig
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelPreflightDCGMReachable(
				t.Context(), nvsentinelComponent, tt.recipeResult, cfg, nil)
			gotBlocked := len(warnings) > 0 || len(errs) > 0
			if gotBlocked != tt.wantBlocked {
				t.Errorf("blocked = %v (warnings=%v errs=%v), want %v", gotBlocked, warnings, errs, tt.wantBlocked)
			}
		})
	}
}
