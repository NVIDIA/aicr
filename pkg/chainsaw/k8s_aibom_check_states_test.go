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
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// deploymentStatus models the k8s-aibom controller Deployment's status for one
// table case. Every field is a pointer so a case can distinguish "the API
// reports zero" from "the API omits the field entirely" — every status field
// the health check reads is `omitempty`, so absence is the state a freshly
// created Deployment is actually in, and it is the state the check's
// `|| `0` defaults exist to handle.
type deploymentStatus struct {
	replicas           *int64
	available          *int64
	updated            *int64
	observedGeneration *int64
}

// TestK8sAIBOMHealthCheckClusterStates drives the shipped health check through
// the in-process executor against synthetic cluster states.
//
// Each case pins wantOutput, not just the pass/fail verdict. Without it every
// negative case would still pass if the *Deployment* step were the thing
// breaking — a test that cannot tell which assertion caught the fault does not
// protect the assertion it claims to. wantOutput names the resource kind whose
// step must be the one that fails.
func TestK8sAIBOMHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/k8s-aibom/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}

	// healthyDeployment is the steady state every case starts from: a
	// single-replica rollout that has fully converged on generation 3.
	healthyDeployment := deploymentStatus{
		replicas: new(int64(1)), available: new(int64(1)),
		updated: new(int64(1)), observedGeneration: new(int64(3)),
	}

	tests := []struct {
		name string
		// Deployment inputs.
		desired            *int64
		deploymentGen      int64
		status             deploymentStatus
		omitDeploymentSpec bool
		// AIBOMControllerConfig inputs.
		configPresent      bool
		omitConfigStatus   bool
		generation         int64
		observedGeneration int64
		readyGeneration    int64
		readyStatus        string
		// Expectations.
		wantPass   bool
		wantOutput string
	}{
		{
			name:    "healthy rollout and current Ready condition",
			desired: new(int64(1)), deploymentGen: 3, status: healthyDeployment,
			configPresent: true,
			generation:    2, observedGeneration: 2, readyGeneration: 2, readyStatus: "True",
			wantPass: true,
		},
		{
			name:    "zero desired replicas fails closed",
			desired: new(int64(0)), deploymentGen: 3,
			status:        deploymentStatus{replicas: new(int64(0)), available: new(int64(0)), updated: new(int64(0)), observedGeneration: new(int64(3))},
			configPresent: true,
			generation:    2, observedGeneration: 2, readyGeneration: 2, readyStatus: "True",
			wantOutput: "Deployment",
		},
		{
			name:    "partial rollout fails closed",
			desired: new(int64(2)), deploymentGen: 3,
			status:        deploymentStatus{replicas: new(int64(2)), available: new(int64(1)), updated: new(int64(2)), observedGeneration: new(int64(3))},
			configPresent: true,
			generation:    2, observedGeneration: 2, readyGeneration: 2, readyStatus: "True",
			wantOutput: "Deployment",
		},
		{
			// The pre-upgrade-ReplicaSet trap: availableReplicas already
			// equals spec.replicas because the OLD pods are the available
			// ones, while the new revision has zero updated replicas.
			// Without the updatedReplicas term this state reports healthy.
			name:    "converged old ReplicaSet mid-upgrade fails closed",
			desired: new(int64(2)), deploymentGen: 4,
			status:        deploymentStatus{replicas: new(int64(2)), available: new(int64(2)), updated: new(int64(0)), observedGeneration: new(int64(4))},
			configPresent: true,
			generation:    2, observedGeneration: 2, readyGeneration: 2, readyStatus: "True",
			wantOutput: "Deployment",
		},
		{
			// The apiserver has accepted a new spec but the Deployment
			// controller has not observed it, so every replica count still
			// describes the previous generation.
			name:    "unobserved Deployment generation fails closed",
			desired: new(int64(1)), deploymentGen: 5,
			status:        deploymentStatus{replicas: new(int64(1)), available: new(int64(1)), updated: new(int64(1)), observedGeneration: new(int64(4))},
			configPresent: true,
			generation:    2, observedGeneration: 2, readyGeneration: 2, readyStatus: "True",
			wantOutput: "Deployment",
		},
		{
			// A freshly created Deployment: the whole status subresource is
			// still absent. This is what the `|| `0` defaults are for; without
			// them the comparisons evaluate against null.
			name:    "absent Deployment status fields fail closed",
			desired: new(int64(1)), deploymentGen: 1,
			status:        deploymentStatus{},
			configPresent: true,
			generation:    2, observedGeneration: 2, readyGeneration: 2, readyStatus: "True",
			wantOutput: "Deployment",
		},
		{
			// spec.replicas omitted entirely. The apiserver defaults it, but
			// the check must not report healthy off a manifest that never
			// reached defaulting.
			name:               "absent spec.replicas fails closed",
			deploymentGen:      1,
			omitDeploymentSpec: true,
			status:             healthyDeployment,
			configPresent:      true,
			generation:         2, observedGeneration: 2, readyGeneration: 2, readyStatus: "True",
			wantOutput: "Deployment",
		},
		{
			// The maxUnavailable=0 stall @yuanchen8911 described. spec.replicas
			// is 1, so the default 25% maxUnavailable rounds to 0 and the old
			// ReplicaSet's pod is held alive until the new one is ready. The
			// new pod exists (updatedReplicas=1) but never becomes ready, while
			// availableReplicas=1 is satisfied by the OLD pod. Only
			// status.replicas — which counts pods across every ReplicaSet —
			// reveals the second pod and the stalled rollout.
			name:    "stalled rollout with the old ReplicaSet still counted fails closed",
			desired: new(int64(1)), deploymentGen: 4,
			status: deploymentStatus{
				replicas: new(int64(2)), available: new(int64(1)),
				updated: new(int64(1)), observedGeneration: new(int64(4)),
			},
			configPresent: true,
			generation:    2, observedGeneration: 2, readyGeneration: 2, readyStatus: "True",
			wantOutput: "Deployment",
		},
		{
			name:    "missing controller config fails closed",
			desired: new(int64(1)), deploymentGen: 3, status: healthyDeployment,
			wantOutput: "AIBOMControllerConfig",
		},
		{
			// Config exists but has never been reconciled, so status is
			// absent: no observedGeneration and no conditions array. Exercises
			// both the `|| `0` and the `|| `[]` defaults on the config step.
			name:    "absent controller config status fails closed",
			desired: new(int64(1)), deploymentGen: 3, status: healthyDeployment,
			configPresent: true, omitConfigStatus: true,
			generation: 2,
			wantOutput: "AIBOMControllerConfig",
		},
		{
			name:    "stale top-level status fails closed",
			desired: new(int64(1)), deploymentGen: 3, status: healthyDeployment,
			configPresent: true,
			generation:    2, observedGeneration: 1, readyGeneration: 2, readyStatus: "True",
			wantOutput: "AIBOMControllerConfig",
		},
		{
			name:    "stale Ready condition fails closed",
			desired: new(int64(1)), deploymentGen: 3, status: healthyDeployment,
			configPresent: true,
			generation:    2, observedGeneration: 2, readyGeneration: 1, readyStatus: "True",
			wantOutput: "AIBOMControllerConfig",
		},
		{
			name:    "Ready false fails closed",
			desired: new(int64(1)), deploymentGen: 3, status: healthyDeployment,
			configPresent: true,
			generation:    2, observedGeneration: 2, readyGeneration: 2, readyStatus: "False",
			wantOutput: "AIBOMControllerConfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetcher := newFakeFetcher()

			deploymentSpec := map[string]any{}
			if !tt.omitDeploymentSpec {
				deploymentSpec["replicas"] = *tt.desired
			}
			deploymentStatusObj := map[string]any{}
			if tt.status.replicas != nil {
				deploymentStatusObj["replicas"] = *tt.status.replicas
			}
			if tt.status.available != nil {
				deploymentStatusObj["availableReplicas"] = *tt.status.available
			}
			if tt.status.updated != nil {
				deploymentStatusObj["updatedReplicas"] = *tt.status.updated
			}
			if tt.status.observedGeneration != nil {
				deploymentStatusObj["observedGeneration"] = *tt.status.observedGeneration
			}
			fetcher.addGet("apps/v1", "Deployment", "k8s-aibom-system", "k8s-aibom", map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]any{
					"name":       "k8s-aibom",
					"namespace":  "k8s-aibom-system",
					"generation": tt.deploymentGen,
				},
				"spec":   deploymentSpec,
				"status": deploymentStatusObj,
			})

			if tt.configPresent {
				config := map[string]any{
					"apiVersion": "aibom.k8saibom.dev/v1alpha1",
					"kind":       "AIBOMControllerConfig",
					"metadata": map[string]any{
						"name":       "default",
						"generation": tt.generation,
					},
				}
				if !tt.omitConfigStatus {
					config["status"] = map[string]any{
						"observedGeneration": tt.observedGeneration,
						"conditions": []any{map[string]any{
							"type":               "Ready",
							"status":             tt.readyStatus,
							"observedGeneration": tt.readyGeneration,
						}},
					}
				}
				fetcher.addGet("aibom.k8saibom.dev/v1alpha1", "AIBOMControllerConfig", "", "default", config)
			}

			result := runChainsawTestInProcess(
				context.Background(), "k8s-aibom", string(data), 2*time.Second, fetcher,
			)
			if result.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v (output: %s)", result.Passed, tt.wantPass, result.Output)
			}
			if tt.wantOutput != "" && !strings.Contains(result.Output, tt.wantOutput) {
				t.Fatalf("output = %q, want it to name %q — the wrong assertion caught this state",
					result.Output, tt.wantOutput)
			}
		})
	}
}
