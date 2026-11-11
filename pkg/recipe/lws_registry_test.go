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
	"testing"

	"aicr.run/recipes"
	"gopkg.in/yaml.v3"
)

const (
	lwsValuesFile = "components/lws/values.yaml"

	// lwsNamespace is fixed by the chart, not chosen by AICR: its
	// LeaderWorkerSet CRD names this namespace for the conversion webhook
	// Service whatever the release namespace is.
	lwsNamespace = "lws-system"

	// lwsControllerDeployment is the name Run:ai's LWS detection looks for.
	lwsControllerDeployment = "lws-controller-manager"
)

// TestLWSRegisteredWithoutOverlay is ADR-024 Gate 5: lws stays installable
// from the registry while no shipped overlay or mixin references it. Adoption
// by a stock recipe amends the ADR and removes the overlay half of this test.
func TestLWSRegisteredWithoutOverlay(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get("lws")
	if comp == nil {
		t.Fatal("lws is missing from recipes/registry.yaml")
	}
	if comp.Helm.DefaultNamespace != lwsNamespace {
		t.Errorf("lws defaultNamespace = %q, want %q (the chart's CRD hard-codes it)",
			comp.Helm.DefaultNamespace, lwsNamespace)
	}
	if comp.OwnsCRDs {
		t.Error("ownsCRDs must stay false: the LeaderWorkerSet CRD uses webhook conversion, which the ownsCRDs audit excludes")
	}
	if comp.HealthCheck.AssertFile != "checks/lws/health-check.yaml" {
		t.Errorf("lws assertFile = %q, want checks/lws/health-check.yaml", comp.HealthCheck.AssertFile)
	}

	for _, path := range stockFilesReferencing(t, "lws") {
		t.Errorf("%s declares componentRef lws; ADR-024 keeps lws opt-in until an adopting recipe amends it", path)
	}
}

// TestLWSHealthCheckMatchesValues guards the names the health check asserts as
// literals against the values that would change them: a fullnameOverride or
// nameOverride renames the controller Deployment, and the check would then
// report a healthy install as unhealthy.
func TestLWSHealthCheckMatchesValues(t *testing.T) {
	t.Parallel()

	raw, err := recipes.FS.ReadFile(lwsValuesFile)
	if err != nil {
		t.Fatalf("read %s: %v", lwsValuesFile, err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse %s: %v", lwsValuesFile, err)
	}
	for _, key := range []string{"fullnameOverride", "nameOverride"} {
		if v, ok := values[key]; ok && v != "" {
			t.Errorf("%s sets %s = %v; the controller Deployment must stay %q",
				lwsValuesFile, key, v, lwsControllerDeployment)
		}
	}

	gotName, gotNamespace := healthCheckDeployment(t, "checks/lws/health-check.yaml")
	if gotName != lwsControllerDeployment {
		t.Errorf("health check asserts Deployment %q, want %q", gotName, lwsControllerDeployment)
	}
	if gotNamespace != lwsNamespace {
		t.Errorf("health check asserts namespace %q, want %q", gotNamespace, lwsNamespace)
	}
}
