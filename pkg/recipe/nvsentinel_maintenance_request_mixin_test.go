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
	"reflect"
	"strings"
	"testing"
)

const maintenanceRequestMixin = "nvsentinel-maintenance-request"

// TestMixinNVSentinelMaintenanceRequest_Composes pins that the mixin sets only
// the MaintenanceRequest controller toggle, and composes without a collision
// beside nvsentinel-node-validation, which owns lifecycle-manager, and each
// step that runs fault-quarantine and node-drainer.
func TestMixinNVSentinelMaintenanceRequest_Composes(t *testing.T) {
	ctx, store := remediationStore(t)
	mixin, ok := store.Mixins[maintenanceRequestMixin]
	if !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", maintenanceRequestMixin, maintenanceRequestMixin)
	}
	if n := len(mixin.Spec.ComponentRefs); n != 1 {
		t.Fatalf("mixin has %d componentRefs, want only nvsentinel", n)
	}
	ref, ok := findComponentRefByName(mixin.Spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("mixin has no nvsentinel componentRef")
	}
	want := map[string]any{"lifecycle-manager": map[string]any{"controllers": map[string]any{
		"maintenanceRequest": map[string]any{"enabled": true},
	}}}
	if !reflect.DeepEqual(ref.Overrides, want) {
		t.Errorf("mixin overrides = %v, want exactly %v", ref.Overrides, want)
	}

	for _, mixins := range [][]string{
		{maintenanceRequestMixin},
		{quarantineMixin, nodeValidationMixin, maintenanceRequestMixin},
		{remediationMixin, healthEventsAnalyzerMixin, nodeValidationMixin, maintenanceRequestMixin},
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
			lm, _ := nvsentinel.Overrides["lifecycle-manager"].(map[string]any)
			controllers, _ := lm["controllers"].(map[string]any)
			if mr, _ := controllers["maintenanceRequest"].(map[string]any); mr["enabled"] != true {
				t.Errorf("lifecycle-manager.controllers.maintenanceRequest = %v, want {enabled: true}", mr)
			}
		})
	}
}
