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

package chainsaw

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// remediationStepWorkloads maps each step's global toggle to the Deployment
// it renders.
var remediationStepWorkloads = []struct{ toggle, deployment string }{
	{"faultQuarantine", "fault-quarantine"},
	{"nodeDrainer", "node-drainer"},
	{"faultRemediation", "fault-remediation"},
	{"janitor", "janitor"},
	{"janitorProvider", "janitor-provider"},
}

type remediationStepCheck struct {
	mixin       string
	check       string
	deployments []string
}

// loadRemediationStepChecks reads each step mixin's inline nvsentinel-mongodb
// check and the Deployments its nvsentinel toggles enable.
func loadRemediationStepChecks(t *testing.T) []remediationStepCheck {
	t.Helper()
	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	mixins := []string{"nvsentinel-observe", "nvsentinel-quarantine", "nvsentinel-remediation"}
	out := make([]remediationStepCheck, 0, len(mixins))
	for _, mixin := range mixins {
		data, err := provider.ReadFile(context.Background(), "mixins/"+mixin+".yaml")
		if err != nil {
			t.Fatalf("read %s: %v", mixin, err)
		}
		var doc struct {
			Spec struct {
				ComponentRefs []struct {
					Name               string         `json:"name"`
					Overrides          map[string]any `json:"overrides"`
					HealthCheckAsserts string         `json:"healthCheckAsserts"`
				} `json:"componentRefs"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("parse %s: %v", mixin, err)
		}
		step := remediationStepCheck{mixin: mixin}
		for _, ref := range doc.Spec.ComponentRefs {
			switch ref.Name {
			case "nvsentinel-mongodb":
				step.check = ref.HealthCheckAsserts
			case "nvsentinel":
				global, _ := ref.Overrides["global"].(map[string]any)
				for _, w := range remediationStepWorkloads {
					toggle, _ := global[w.toggle].(map[string]any)
					if enabled, _ := toggle["enabled"].(bool); enabled {
						step.deployments = append(step.deployments, w.deployment)
					}
				}
			}
		}
		if step.check == "" {
			t.Fatalf("%s sets no healthCheckAsserts on nvsentinel-mongodb", mixin)
		}
		out = append(out, step)
	}
	return out
}

func checkSteps(t *testing.T, content string) []any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("parse check: %v", err)
	}
	spec, _ := doc["spec"].(map[string]any)
	steps, _ := spec["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("check has %d steps, want 1", len(steps))
	}
	step, _ := steps[0].(map[string]any)
	try, _ := step["try"].([]any)
	return try
}

func assertedDeployments(try []any) []string {
	var names []string
	for _, op := range try {
		res, _ := op.(map[string]any)["assert"].(map[string]any)["resource"].(map[string]any)
		if res["kind"] != "Deployment" {
			continue
		}
		md, _ := res["metadata"].(map[string]any)
		name, _ := md["name"].(string)
		names = append(names, name)
	}
	return names
}

// TestNVSentinelRemediationStepChecksMatchMixins pins each step's inline
// check to the registry check it extends and to the Deployments the step
// turns on, so a mixin that enables a workload cannot ship without a
// positive assert for it, and the three copies cannot drift from the
// registry file.
func TestNVSentinelRemediationStepChecksMatchMixins(t *testing.T) {
	t.Parallel()
	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	registryCheck, err := provider.ReadFile(context.Background(), "checks/nvsentinel-mongodb/health-check.yaml")
	if err != nil {
		t.Fatalf("read registry check: %v", err)
	}
	base := checkSteps(t, string(registryCheck))

	for _, step := range loadRemediationStepChecks(t) {
		t.Run(step.mixin, func(t *testing.T) {
			t.Parallel()
			if err := ValidateTestReadOnly("nvsentinel-mongodb", step.check); err != nil {
				t.Fatalf("inline check is not read-only: %v", err)
			}
			try := checkSteps(t, step.check)
			if len(try) < len(base) || !reflect.DeepEqual(try[:len(base)], base) {
				t.Error("inline check does not start with the registry check's asserts")
			}
			got := assertedDeployments(try)
			slices.Sort(got)
			want := slices.Clone(step.deployments)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("asserted Deployments = %v, want the step's %v", got, want)
			}
		})
	}
}

// TestNVSentinelRemediationStepChecksClusterStates runs each step's inline
// check against a healthy cluster and against one missing each Deployment
// the step enables.
func TestNVSentinelRemediationStepChecksClusterStates(t *testing.T) {
	t.Parallel()

	datastore := func(f *fakeFetcher) {
		f.addGet("psmdb.percona.com/v1", "PerconaServerMongoDB", "nvsentinel", "nvsentinel-mongodb", map[string]any{
			"apiVersion": "psmdb.percona.com/v1", "kind": "PerconaServerMongoDB",
			"metadata": map[string]any{"name": "nvsentinel-mongodb", "namespace": "nvsentinel"},
			"status":   map[string]any{"state": "ready"},
		})
		f.addGet("apps/v1", "StatefulSet", "nvsentinel", "nvsentinel-mongodb-rs0", map[string]any{
			"apiVersion": "apps/v1", "kind": "StatefulSet",
			"metadata": map[string]any{"name": "nvsentinel-mongodb-rs0", "namespace": "nvsentinel"},
			"spec":     map[string]any{"replicas": int64(3)},
			"status":   map[string]any{"readyReplicas": int64(3)},
		})
		f.addGet("cert-manager.io/v1", "Certificate", "nvsentinel", "nvsentinel-mongodb-app-client", map[string]any{
			"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
			"metadata": map[string]any{"name": "nvsentinel-mongodb-app-client", "namespace": "nvsentinel"},
			"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
		})
		f.addGet("v1", "Secret", "nvsentinel", "nvsentinel-mongodb-uri", map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{"name": "nvsentinel-mongodb-uri", "namespace": "nvsentinel"},
			"data":     map[string]any{"MONGODB_URI": "bW9uZ29kYjovLw=="},
		})
	}
	deployment := func(name string) map[string]any {
		return map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": name, "namespace": "nvsentinel"},
			"status":   map[string]any{"availableReplicas": int64(1)},
		}
	}

	for _, step := range loadRemediationStepChecks(t) {
		cases := []struct {
			name, missing string
		}{{name: "all enabled workloads available"}}
		for _, d := range step.deployments {
			cases = append(cases, struct{ name, missing string }{"missing " + d, d})
		}
		for _, tc := range cases {
			t.Run(step.mixin+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				f := newFakeFetcher()
				datastore(f)
				for _, d := range step.deployments {
					if d != tc.missing {
						f.addGet("apps/v1", "Deployment", "nvsentinel", d, deployment(d))
					}
				}
				res := runChainsawTestInProcess(context.Background(), "nvsentinel-mongodb", step.check,
					2*time.Second, f)
				if wantPass := tc.missing == ""; res.Passed != wantPass {
					t.Fatalf("passed = %v, want %v (output: %s)", res.Passed, wantPass, res.Output)
				}
				if tc.missing != "" && !strings.Contains(res.Output, tc.missing) {
					t.Errorf("output does not name %s: %s", tc.missing, res.Output)
				}
			})
		}
	}
}
