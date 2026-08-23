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

# Run Scoping

Every deployment belongs to a single run identified by Config.RunID (generate
one with runid.Generate). The run ID is suffixed onto every object this package
creates — Job, ServiceAccount, Role, RoleBinding, ClusterRole,
ClusterRoleBinding, and the staging ConfigMap — so concurrent runs never share
an object. Config.JobName and Config.ServiceAccountName are prefixes, not exact
names; when empty they fall back to Config.NameBase (default "aicr"). See
ADR-020 (docs/design/020-snapshot-agent-run-isolation.md).

Because a run-scoped name cannot already belong to another run, creates are
plain creates: there is no delete-and-recreate of the Job, and no
create-or-update of the RBAC objects. An AlreadyExists implies a duplicate
RunID and is returned as an error rather than adopted or overwritten. The one
courtesy check is in ensureServiceAccount, which warns when a ServiceAccount
already exists under the bare (unscoped) prefix so a caller relying on the old
adoption behavior is not left guessing; it never blocks the deploy.

Two objects are deliberately NOT run-scoped:

  - The Namespace is ensured, never deleted: it is created if absent and
    labeled "app.kubernetes.io/managed-by=aicr", patching the label onto a
    pre-existing namespace rather than silently dropping intent.
  - A caller-supplied "cm://namespace/name" Output is the caller's delivered
    artifact. It is written on purpose and never deleted
    (Config.OwnsOutputConfigMap is false for it).

Every created object carries app.kubernetes.io/name=aicr,
app.kubernetes.io/managed-by=aicr,
app.kubernetes.io/component=snapshot-agent, and aicr.run/run-id=<RunID>, on the
Job's pod template as well as the Job itself. Select agent pods across runs
with the component label; the Job name changes every run.

Job and Pod lifecycle waits use the Kubernetes watch API (not polling) for
efficiency. Pod selection narrows by label and then authorizes the candidate
against the controlling ownerReference carrying the recorded Job UID, since pod
labels are writable by anything that can update pods in the namespace.

# Cleanup

The Deployer records (kind, name, UID) for each object it successfully creates.
Cleanup deletes exactly that set, passing the recorded UID as a
metav1.Preconditions so a same-named object belonging to another run is never
collected; a UID mismatch (Conflict) and a NotFound are both treated as
success. Cleanup also runs on the Deploy failure path, which is why it is
scoped to what was created rather than to configured names.

The staging ConfigMap is written by the in-pod agent, so its UID is observed
when GetSnapshot reads it. When the run owns that ConfigMap and failed before
it could be observed, Cleanup Gets it by its run-scoped name and deletes it
pinned to the UID that Get returned.

# Usage Example

	package main

	import (
		"context"
		"time"

		"github.com/NVIDIA/aicr/pkg/k8s/agent"
		"github.com/NVIDIA/aicr/pkg/k8s/client"
		"github.com/NVIDIA/aicr/pkg/runid"
	)

	func main() {
		ctx := context.Background()

		// Get Kubernetes client
		clientset, _, err := client.GetKubeClient()
		if err != nil {
			panic(err)
		}

		// One run ID scopes every object this deployment creates.
		runID := runid.Generate()

		// Configure deployer
		config := agent.Config{
			Namespace: "gpu-operator",
			RunID:     runID,
			Image:     "ghcr.io/nvidia/aicr-validator:latest",
			Output:    "cm://gpu-operator/" + agent.StagingConfigMapName(runID),
			// Output is owned by this run, so Cleanup may delete it. Point
			// Output at a ConfigMap of your own and leave this false: an
			// artifact you named is never deleted here.
			OwnsOutputConfigMap: true,
			NodeSelector: map[string]string{
				"nodeGroup": "customer-gpu",
			},
		}

		// Create deployer
		deployer := agent.NewDeployer(clientset, config)

		// Always clean up this run's objects, including on the failure path.
		defer func() {
			_ = deployer.Cleanup(context.Background(), agent.CleanupOptions{Enabled: true})
		}()

		// Deploy RBAC and Job
		if err := deployer.Deploy(ctx); err != nil {
			panic(err)
		}

		// Wait for completion (deployer.JobName() is the run-scoped name)
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
			RunID:     "20260821-142233-9f3a1c0b7e2d4a55",
			Image:     "test:latest",
		})
		// Test deployment logic...
	}
*/
package agent
