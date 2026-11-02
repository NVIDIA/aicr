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
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

const (
	nodeValidationMixin = "nvsentinel-node-validation"
	lifecycleManagerImg = "ghcr.io/nvidia/nvsentinel/lifecycle-manager"
	// cordonedCriterion is the chart's cordoned criterion, which keeps a
	// diagnostic off a node that registered schedulable.
	cordonedCriterion = "has(node.spec.unschedulable) && node.spec.unschedulable"
	// quarantineCriterion keeps lifecycle-manager from selecting a node by
	// fault-quarantine's cordon.
	quarantineCriterion = `!(has(node.metadata.annotations) && "quarantineHealthEvent" in node.metadata.annotations)`
)

// TestMixinNVSentinelNodeValidation_Composes pins what the mixin sets, alone
// and beside the full remediation step without a collision: lifecycle-manager
// on with only its ValidationRequest controller, the cordon release under the
// chart's label prefix, and new-node validation with the cordoned and
// not-under-quarantine criteria and dcgm-diag-test.
func TestMixinNVSentinelNodeValidation_Composes(t *testing.T) {
	ctx, store := remediationStore(t)
	if _, ok := store.Mixins[nodeValidationMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", nodeValidationMixin, nodeValidationMixin)
	}

	for _, mixins := range [][]string{
		{nodeValidationMixin},
		{remediationMixin, healthEventsAnalyzerMixin, nodeValidationMixin},
		{remediationMixin, healthEventsAnalyzerMixin, logCollectorMixin, nodeValidationMixin},
	} {
		t.Run(strings.Join(mixins, "+"), func(t *testing.T) {
			spec := remediationLeaf(mixins...)
			if _, err := store.mergeMixins(ctx, &spec); err != nil {
				t.Fatalf("mergeMixins: %v", err)
			}
			nvsentinel, ok := findComponentRefByName(spec.ComponentRefs, "nvsentinel")
			if !ok {
				t.Fatal("nvsentinel missing from merged spec")
			}
			global, _ := nvsentinel.Overrides["global"].(map[string]any)
			if section, _ := global["lifecycleManager"].(map[string]any); len(section) != 1 || section["enabled"] != true {
				t.Errorf("global.lifecycleManager = %v, want exactly {enabled: true}", global["lifecycleManager"])
			}
			assertLifecycleManagerOverrides(t, nvsentinel.Overrides)
		})
	}

	ref, ok := findComponentRefByName(store.Mixins[nodeValidationMixin].Spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("mixin has no nvsentinel componentRef")
	}
	if got := slices.Sorted(maps.Keys(ref.Overrides)); !slices.Equal(got, []string{"global", "lifecycle-manager"}) {
		t.Errorf("mixin top-level overrides = %v, want [global lifecycle-manager]", got)
	}
	global, _ := ref.Overrides["global"].(map[string]any)
	if got := slices.Sorted(maps.Keys(global)); !slices.Equal(got, []string{"lifecycleManager"}) {
		t.Errorf("mixin global overrides = %v, want [lifecycleManager]: node validation needs no datastore", got)
	}
	if n := len(store.Mixins[nodeValidationMixin].Spec.ComponentRefs); n != 1 {
		t.Errorf("mixin has %d componentRefs, want only nvsentinel", n)
	}
}

// assertLifecycleManagerOverrides pins the mixin's lifecycle-manager block
// exactly, so a path added or dropped fails here.
func assertLifecycleManagerOverrides(t *testing.T, overrides map[string]any) {
	t.Helper()
	lm, _ := overrides["lifecycle-manager"].(map[string]any)
	if got := slices.Sorted(maps.Keys(lm)); !slices.Equal(got, []string{"config", "controllers"}) {
		t.Errorf("lifecycle-manager overrides = %v, want exactly [config controllers]", got)
	}

	controllers, _ := lm["controllers"].(map[string]any)
	wantControllers := map[string]bool{"validationRequest": true, "maintenanceRequest": false}
	if got := slices.Sorted(maps.Keys(controllers)); !slices.Equal(got, slices.Sorted(maps.Keys(wantControllers))) {
		t.Errorf("lifecycle-manager.controllers = %v, want exactly %v", got, slices.Sorted(maps.Keys(wantControllers)))
	}
	for name, want := range wantControllers {
		controller, _ := controllers[name].(map[string]any)
		if len(controller) != 1 || controller["enabled"] != want {
			t.Errorf("lifecycle-manager.controllers.%s = %v, want exactly {enabled: %v}", name, controller, want)
		}
	}

	config, _ := lm["config"].(map[string]any)
	if got := slices.Sorted(maps.Keys(config)); !slices.Equal(got, []string{"newNodeValidation", "schedulingGate"}) {
		t.Errorf("lifecycle-manager.config = %v, want exactly [newNodeValidation schedulingGate]", got)
	}
	gate, _ := config["schedulingGate"].(map[string]any)
	cordon, _ := gate["cordon"].(map[string]any)
	if len(gate) != 1 || !maps.Equal(cordon, map[string]any{"remove": true, "labelPrefix": "k8saas.nvidia.com/"}) {
		t.Errorf("lifecycle-manager.config.schedulingGate = %v, want exactly cordon {remove: true, "+
			"labelPrefix: k8saas.nvidia.com/}", gate)
	}

	newNode, _ := config["newNodeValidation"].(map[string]any)
	if got := slices.Sorted(maps.Keys(newNode)); !slices.Equal(got, []string{"criteria", "newNodeTests"}) {
		t.Errorf("lifecycle-manager.config.newNodeValidation = %v, want exactly [criteria newNodeTests]", got)
	}
	if tests, _ := newNode["newNodeTests"].([]any); !slices.Equal(tests, []any{"dcgm-diag-test"}) {
		t.Errorf("newNodeValidation.newNodeTests = %v, want [dcgm-diag-test]", newNode["newNodeTests"])
	}
	criteria, _ := newNode["criteria"].([]any)
	names := make([]string, 0, len(criteria))
	var cordoned, notQuarantined string
	for _, item := range criteria {
		criterion, _ := item.(map[string]any)
		name, _ := criterion["name"].(string)
		names = append(names, name)
		switch name {
		case "cordoned":
			cordoned, _ = criterion["expression"].(string)
		case "not-under-quarantine":
			notQuarantined, _ = criterion["expression"].(string)
		}
	}
	if want := []string{"recently-joined", "gpu-present", "cordoned", "not-under-quarantine"}; !slices.Equal(names, want) {
		t.Errorf("newNodeValidation.criteria names = %v, want %v", names, want)
	}
	if strings.Join(strings.Fields(cordoned), " ") != cordonedCriterion {
		t.Errorf("cordoned criterion = %q, want %q: without it a new node that registered schedulable is "+
			"diagnosed while it takes work", cordoned, cordonedCriterion)
	}
	if strings.Join(strings.Fields(notQuarantined), " ") != quarantineCriterion {
		t.Errorf("not-under-quarantine criterion = %q, want %q: without it a node fault-quarantine cordons is "+
			"selected and diagnosed once fault-quarantine releases it to work", notQuarantined, quarantineCriterion)
	}
}

// TestNoShippedRecipeAdoptsNodeValidation keeps node validation an operator's
// choice: no overlay composes the mixin, and no resolved recipe turns
// lifecycle-manager on.
func TestNoShippedRecipeAdoptsNodeValidation(t *testing.T) {
	ctx, store := remediationStore(t)

	if slices.Contains(store.Base.Spec.Mixins, nodeValidationMixin) {
		t.Errorf("recipes/overlays/base.yaml adopts %s; node validation stays opt-in", nodeValidationMixin)
	}
	for name, overlay := range store.Overlays {
		if slices.Contains(overlay.Spec.Mixins, nodeValidationMixin) {
			t.Errorf("overlay %q adopts %s; node validation stays opt-in", name, nodeValidationMixin)
		}
	}

	resolved := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
		if err != nil {
			t.Errorf("resolving %s: %v", name, err)
			continue
		}
		resolved++
		if _, ok := findComponentRefByName(result.ComponentRefs, "nvsentinel"); !ok {
			continue
		}
		values, err := result.GetValuesForComponentWithContext(ctx, "nvsentinel")
		if err != nil {
			t.Errorf("%s: GetValuesForComponentWithContext: %v", name, err)
			continue
		}
		global, _ := values["global"].(map[string]any)
		if raw, present := global["lifecycleManager"]; present {
			section, isMap := raw.(map[string]any)
			if !isMap || section["enabled"] != false {
				t.Errorf("%s resolves with global.lifecycleManager = %v; it must be absent or off", name, raw)
			}
		}
	}
	if resolved == 0 {
		t.Fatal("no shipped recipes were resolved -- the walker is broken")
	}
}

// TestLifecycleManagerImagePinnedEverywhere keeps the weekly scan and the
// render test in step with nvsentinel's registry pin. The subchart image
// follows the parent chart's version, so a bump moves the deployed image
// while these copies keep naming the old one.
func TestLifecycleManagerImagePinnedEverywhere(t *testing.T) {
	_, store := remediationStore(t)
	registry, err := GetComponentRegistryFor(store.provider)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}
	nvsentinel := registry.Get("nvsentinel")
	if nvsentinel == nil {
		t.Fatal("nvsentinel not found in registry")
	}
	version := nvsentinel.Helm.DefaultVersion
	if version == "" {
		t.Fatal("nvsentinel registry entry has no defaultVersion")
	}

	assertScanned(t, lifecycleManagerImg, version)
	const renderTest = "../../pkg/bundler/validations/nvsentinel_node_validation_render_test.go"
	data, err := os.ReadFile(renderTest)
	if err != nil {
		t.Fatalf("reading %s: %v", renderTest, err)
	}
	if ref := fmt.Sprintf("%s:%s", lifecycleManagerImg, version); !strings.Contains(string(data), ref) {
		t.Errorf("%s does not pin %s -- the render test asserts this image reaches the chart; bump it alongside "+
			"nvsentinel's defaultVersion", renderTest, ref)
	}
}
