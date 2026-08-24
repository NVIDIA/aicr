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

import "testing"

// auditedOwnsCRDs pins the exact chart versions whose CRD ownership was
// audited. ownsCRDs asserts two properties of a specific chart: that the
// component solely owns every CRD it ships, and that it ships none using
// spec.conversion.strategy: Webhook. Both are properties of the chart at a
// version, not of the component name.
//
// A recipe that overrides coordinates is already handled at generation time
// (usesRegistryChart disables the policy). This guard covers the other
// direction, which nothing else does: bumping defaultVersion in the registry
// keeps ownsCRDs true and would apply CreateReplace to a chart nobody
// re-checked, which may by then share a CRD or use webhook conversion.
//
// Re-audit procedure when this test fails:
//
//  1. helm show crds <chart> --version <new> and list the CRD names.
//  2. Confirm no other registry component ships any of those names, including
//     via templates/ — `helm show crds` does not report those, and
//     prometheus-operator-crds ships through templates/ the same CRDs
//     kube-prometheus-stack ships under crds/.
//  3. Confirm none declares spec.conversion.strategy: Webhook.
//  4. Update the pin below, or drop ownsCRDs if either property no longer holds.
//
// See https://github.com/NVIDIA/aicr/issues/2264.
var auditedOwnsCRDs = map[string]string{
	"gatekeeper": "3.22.2",
	"k8s-aibom":  "1.3.0",
	"nvsentinel": "v1.20.0",
}

func TestOwnsCRDsPinsMatchAuditedVersions(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}

	enrolled := map[string]string{}
	for _, name := range registry.Names() {
		cfg := registry.Get(name)
		if cfg == nil || !cfg.OwnsCRDs {
			continue
		}
		enrolled[name] = cfg.Helm.DefaultVersion
	}

	for name, version := range enrolled {
		audited, ok := auditedOwnsCRDs[name]
		if !ok {
			t.Errorf("component %q sets ownsCRDs but has no audited version recorded; "+
				"run the re-audit procedure in this file and add it", name)
			continue
		}
		if audited != version {
			t.Errorf("component %q is pinned to chart %q but was audited at %q; "+
				"a version bump does not carry the CRD-ownership audit forward — "+
				"re-audit and update auditedOwnsCRDs, or drop ownsCRDs",
				name, version, audited)
		}
	}

	for name := range auditedOwnsCRDs {
		if _, ok := enrolled[name]; !ok {
			t.Errorf("component %q has an audited version recorded but no longer sets "+
				"ownsCRDs; remove the stale entry", name)
		}
	}
}
