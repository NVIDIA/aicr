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

/*
Package agent provides Kubernetes Job deployment for automated snapshot capture.

The agent package deploys a Kubernetes Job that runs aicr snapshot on GPU nodes
and writes output to ConfigMap storage. It handles RBAC setup, Job lifecycle
management, and snapshot retrieval.

# Deployment Strategy

RBAC resources (ServiceAccount, Role, RoleBinding, ClusterRole, ClusterRoleBinding)
are created idempotently - if they exist, they are reused. Mutable resources
(Role, RoleBinding, ClusterRole, ClusterRoleBinding) use create-or-update
semantics so stale rules from a previous run cannot persist.

The agent Namespace is created with an "app.kubernetes.io/managed-by=aicr"
label; if the namespace pre-existed without that label, ensureNamespace
patches the label rather than silently dropping intent.

The Job is deleted and recreated for each snapshot to ensure clean state.
Job and Pod lifecycle waits use the Kubernetes watch API (not polling)
for efficiency.

# Usage Example

	package main

	import (
		"context"
		"time"

		"github.com/NVIDIA/aicr/pkg/k8s/agent"
		"github.com/NVIDIA/aicr/pkg/k8s/client"
	)

	func main() {
		ctx := context.Background()

		// Get Kubernetes client
		clientset, _, err := client.GetKubeClient()
		if err != nil {
			panic(err)
		}

		// Configure deployer
		config := agent.Config{
			Namespace: "gpu-operator",
			Image:     "ghcr.io/nvidia/aicr-validator:latest",
			Output:    "cm://gpu-operator/aicr-snapshot",
			NodeSelector: map[string]string{
				"nodeGroup": "customer-gpu",
			},
		}

		// Create deployer
		deployer := agent.NewDeployer(clientset, config)

		// Deploy RBAC and Job
		if err := deployer.Deploy(ctx); err != nil {
			panic(err)
		}

		// Wait for completion
		if err := deployer.WaitForCompletion(ctx, 5*time.Minute); err != nil {
			panic(err)
		}

		// Get snapshot
		snapshot, err := deployer.GetSnapshot(ctx)
		if err != nil {
			panic(err)
		}

		// Use snapshot...
	}

# Reconciliation

The deployer ensures idempotent operation:
  - Namespace: Created with managed-by label, or patched if pre-existing
  - Immutable RBAC (ServiceAccount): Created if missing, reused if exists
  - Mutable RBAC (Role/RoleBinding/ClusterRole/ClusterRoleBinding):
    create-or-update semantics so stale rules cannot persist
  - Job: Deleted and recreated for clean state each run; deletion is
    observed via watch (watch.Deleted event), not polling
  - ConfigMap: Created or updated with latest snapshot

# Testing

The package is designed for testability with Kubernetes fake clients:

	import (
		"testing"
		"k8s.io/client-go/kubernetes/fake"
	)

	func TestDeployer(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		deployer := agent.NewDeployer(clientset, agent.Config{
			Namespace: "test",
			Image:     "test:latest",
		})
		// Test deployment logic...
	}
*/
package agent
