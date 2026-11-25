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
	"context"
	"regexp"
	"strings"
	"testing"

	"aicr.run/pkg/bundler/config"
	"aicr.run/pkg/helm/helmtest"
	"aicr.run/pkg/recipe"
)

// runaiGPUFaultTaint is Run:ai's documented taint for a faulty GPU node, as
// the gate prints it; runaiGPUFaultTaintBlock is the same taint as the chart
// writes it into one rule set of fault-quarantine's config.
const runaiGPUFaultTaint = "runai/gpu-fault=true:NoSchedule"

var runaiGPUFaultTaintBlock = regexp.MustCompile(`(?m)^[ \t]*\[rule-sets\.taint\]\n` +
	`[ \t]*key = "runai/gpu-fault"\n[ \t]*value = "true"\n[ \t]*effect = "NoSchedule"\n`)

// runaiLeaves are the shipped leaves that install Run:ai, by the criteria
// that resolve them.
func runaiLeaves() map[string]*recipe.Criteria {
	leaf := func(service recipe.CriteriaServiceType, os recipe.CriteriaOSType) *recipe.Criteria {
		c := recipe.NewCriteria()
		c.Service = service
		c.Accelerator = recipe.CriteriaAcceleratorH100
		c.OS = os
		c.Intent = recipe.CriteriaIntentTraining
		c.Platform = recipe.CriteriaPlatformRunai
		return c
	}
	return map[string]*recipe.Criteria{
		"h100-eks-ubuntu-training-runai": leaf(recipe.CriteriaServiceEKS, recipe.CriteriaOSUbuntu),
		"h100-gke-cos-training-runai":    leaf(recipe.CriteriaServiceGKE, recipe.CriteriaOSCOS),
	}
}

// runaiLeafNVSentinelValues resolves the nvsentinel values a bundle of the
// Run:ai leaf selected by criteria renders with, composing the named mixins
// on top of the leaf's own overrides in order.
func runaiLeafNVSentinelValues(t *testing.T, store *recipe.MetadataStore, criteria *recipe.Criteria, mixinNames ...string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nvsentinelObservabilityChartTimeout)
	defer cancel()

	result, err := recipe.NewBuilder().BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria: %v", err)
	}
	leafRef := result.GetComponentRef(nvsentinelComponent)
	if leafRef == nil {
		t.Fatalf("%s is absent from the recipe %v resolves to", nvsentinelComponent, result.Metadata.AppliedOverlays)
	}

	// Fresh map so nothing aliases the resolved recipe or the metadata store.
	composed := map[string]any{}
	mergeValuesForRender(composed, leafRef.Overrides)
	for _, mixinName := range mixinNames {
		mixin, ok := store.Mixins[mixinName]
		if !ok {
			t.Fatalf("%s mixin not present; check recipes/mixins/", mixinName)
		}
		for _, c := range mixin.Spec.ComponentRefs {
			if c.Name == nvsentinelComponent {
				mergeValuesForRender(composed, c.Overrides)
			}
		}
	}

	ref := *leafRef
	ref.Overrides = composed
	values, err := recipe.GetComponentValuesWithContext(ctx, nil, &ref)
	if err != nil {
		t.Fatalf("GetComponentValuesWithContext: %v", err)
	}
	return values
}

// TestCheckNVSentinelPostRemediationValidationRunaiLeaves runs the gate on
// the Run:ai leaves, whose fault-quarantine rule sets apply Run:ai's
// runai/gpu-fault=true:NoSchedule beside each cordon. The quarantine step
// lifts that taint itself and passes. The remediation step hands it to
// lifecycle-manager, which must list a taint to lift it, and the chart's test
// Job template (NVSentinel v1.32.0) writes a listed value unquoted, so no
// entry can release a taint whose value is "true": the gate refuses the
// bundle. When upstream quotes that value, the leaves gain the
// schedulingGate.taints entry and the remediation case here passes.
func TestCheckNVSentinelPostRemediationValidationRunaiLeaves(t *testing.T) {
	t.Parallel()
	store, err := recipe.LoadMetadataStoreFor(t.Context(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	gpuOperator := recipe.ComponentRef{Name: "gpu-operator", Namespace: nodeValidationDCGMNamespace,
		Overrides: map[string]any{"dcgm": map[string]any{"enabled": true}}}

	steps := []struct {
		name    string
		mixins  []string
		refused bool
	}{
		{name: "quarantine", mixins: []string{"nvsentinel-quarantine"}},
		{name: "node validation alone", mixins: []string{nodeValidationMixin}},
		{name: "remediation with node validation", refused: true,
			mixins: []string{"nvsentinel-remediation", healthEventsAnalyzerMixinName, nodeValidationMixin}},
	}
	for leaf, criteria := range runaiLeaves() {
		for _, step := range steps {
			t.Run(leaf+"/"+step.name, func(t *testing.T) {
				t.Parallel()
				values := runaiLeafNVSentinelValues(t, store, criteria, step.mixins...)
				rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
					{Name: nvsentinelComponent, Overrides: values}, gpuOperator,
				}}
				_, errs := CheckNVSentinelPostRemediationValidationCoherent(t.Context(), nvsentinelComponent, rr,
					config.NewConfig(), nil)
				if !step.refused {
					if len(errs) != 0 {
						t.Errorf("got errors %v, want none", errs)
					}
					return
				}
				named := false
				for _, err := range errs {
					msg := err.Error()
					if strings.Contains(msg, runaiGPUFaultTaint) && strings.Contains(msg, "would break every test Job") {
						named = true
					}
				}
				if !named {
					t.Errorf("got errors %v, want one that names %s and the test Job template", errs, runaiGPUFaultTaint)
				}
			})
		}
	}
}

// TestNVSentinelRunaiQuarantineTaintChartRender is the guard that makes
// restating fault-quarantine.ruleSets on the Run:ai leaves acceptable.
//
// Helm replaces lists, so the leaves own the whole list and a chart bump that
// adds, drops or rewrites a rule set would be silently overridden by AICR's
// stale copy. This renders the pinned chart twice per leaf with
// nvsentinel-quarantine composed -- once with the leaf's values, once with the
// leaf's ruleSets removed so the chart's own list applies -- and requires
// fault-quarantine's config to be identical apart from one intended
// deviation: a runai/gpu-fault=true:NoSchedule taint on every rule set that
// cordons, and on no other.
//
// Pulls the chart over the network by mutable tag, so it is excluded from
// `make test`/`make qualify` (always -short) and runs in the scheduled
// render-check workflow. Run on demand:
// `go test ./pkg/bundler/validations/... -run TestNVSentinelRunaiQuarantineTaintChartRender`.
func TestNVSentinelRunaiQuarantineTaintChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	helmtest.RequireBinary(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	quarantineConfig := func(t *testing.T, values map[string]any) string {
		t.Helper()
		cm, ok := renderedDocument(renderNVSentinel(t, values), "ConfigMap", faultQuarantineName)
		if !ok {
			t.Fatal("fault-quarantine ConfigMap not rendered; nvsentinel-quarantine no longer enables it")
		}
		return cm
	}

	for leaf, criteria := range runaiLeaves() {
		t.Run(leaf, func(t *testing.T) {
			withLeaf := runaiLeafNVSentinelValues(t, store, criteria, "nvsentinel-quarantine")

			// Same values, minus the list, so the chart supplies its own.
			chartDefaults := map[string]any{}
			mergeValuesForRender(chartDefaults, withLeaf)
			quarantineValues, ok := chartDefaults[faultQuarantineName].(map[string]any)
			if !ok {
				t.Fatalf("resolved values carry no %s map; got %T", faultQuarantineName, chartDefaults[faultQuarantineName])
			}
			if _, present := quarantineValues["ruleSets"]; !present {
				t.Fatal("the leaf no longer sets fault-quarantine.ruleSets; this guard and the leaf have diverged")
			}
			delete(quarantineValues, "ruleSets")

			leafConfig := quarantineConfig(t, withLeaf)
			chartConfig := quarantineConfig(t, chartDefaults)

			if runaiGPUFaultTaintBlock.MatchString(chartConfig) {
				t.Fatalf("the chart's own rule sets now apply %s; drop the leaf's copy of the list", runaiGPUFaultTaint)
			}
			cordoning := strings.Count(chartConfig, "shouldCordon = true")
			if cordoning == 0 {
				t.Fatalf("the chart's rule sets cordon nowhere:\n%s", chartConfig)
			}
			if got := len(runaiGPUFaultTaintBlock.FindAllString(leafConfig, -1)); got != cordoning {
				t.Errorf("%d rule sets apply %s, want %d, one per rule set that cordons", got, runaiGPUFaultTaint, cordoning)
			}
			for i, ruleSet := range strings.Split(leafConfig, "[[rule-sets]]")[1:] {
				cordons := strings.Contains(ruleSet, "shouldCordon = true")
				if applies := runaiGPUFaultTaintBlock.MatchString(ruleSet); applies != cordons {
					t.Errorf("rule set %d: cordons=%v but applies %s=%v:\n%s", i, cordons, runaiGPUFaultTaint, applies, ruleSet)
				}
			}
			if stripped := runaiGPUFaultTaintBlock.ReplaceAllString(leafConfig, ""); stripped != chartConfig {
				t.Errorf("fault-quarantine.ruleSets on %s drifted from the chart.\nleaf, taint removed:\n%s\nchart:\n%s\n"+
					"Update recipes/overlays/%s.yaml to the chart's list, keeping the taint on every rule set that cordons.",
					leaf, stripped, chartConfig, leaf)
			}
		})
	}
}
