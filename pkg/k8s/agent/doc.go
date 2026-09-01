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
an object. Config.JobName is a prefix, not an exact name;
Config.ServiceAccountName is a prefix only when no ServiceAccount of that
exact name exists (see Existing ServiceAccounts below). When empty they fall
back to Config.NameBase (default "aicr"). See ADR-020
(docs/design/020-snapshot-agent-run-isolation.md).

Because a run-scoped name cannot already belong to another run, creates are
plain creates: there is no delete-and-recreate of the Job, and no
create-or-update of the RBAC objects. An AlreadyExists implies a duplicate
RunID and is returned as an error rather than adopted or overwritten.

# Existing ServiceAccounts

Config.ServiceAccountName is exact-if-exists. When a ServiceAccount of exactly
that name already exists in the namespace, the agent pod runs as it verbatim
and the run creates NO ServiceAccount, Role, RoleBinding, ClusterRole or
ClusterRoleBinding — aicr adds and removes no permissions on an identity it
did not create. Nothing of those kinds enters the created-set, so Cleanup has
nothing of those kinds to delete and the operator's grants outlive the run.

This exists because IRSA (eks.amazonaws.com/role-arn) and GKE Workload
Identity (iam.gke.io/gcp-service-account) both pin trust to the ServiceAccount
NAME — IRSA's trust policy conditions on system:serviceaccount:<ns>/<name>,
GKE's IAM binding names PROJECT.svc.id.goog[<ns>/<name>] and accepts no
wildcard — so a per-run name can never be trusted by either, and copying the
annotations onto a run-scoped ServiceAccount would not help.

Render the RBAC that grants such a ServiceAccount the agent's permissions
with BuildServiceAccountRoleManifests (CLI: aicr snapshot
--add-roles-to-service-account). That path APPLIES NOTHING and contacts no
cluster: it writes manifests the operator reviews and applies themselves, so
the decision to grant cluster-scoped -- and, under DiscoverNetwork, mutating
-- permissions is an informed one. What they then apply sits outside every
run: no run-ID label, never in a created-set, never deleted by run cleanup,
and removed with kubectl delete -f.

The trade-off is deliberate and opt-in: an adopted ServiceAccount waives
per-run permission isolation. Concurrent runs sharing it share its grants, and
a DiscoverNetwork grant leaves cluster-scoped mutating permissions in
place permanently rather than for one run's lifetime.

Two objects are deliberately NOT run-scoped:

  - The Namespace is ensured, never deleted: it is created if absent and
    labeled "app.kubernetes.io/managed-by=aicr", patching the label onto a
    pre-existing namespace rather than silently dropping intent.
  - A caller-supplied "cm://namespace/name" Output is the caller's delivered
    artifact. It is written on purpose and never deleted
    (Config.OwnsOutputConfigMap is false for it).

Every object this package itself creates — the Job, ServiceAccount, Role,
RoleBinding, ClusterRole, and ClusterRoleBinding — carries
app.kubernetes.io/name=aicr, app.kubernetes.io/managed-by=aicr,
app.kubernetes.io/component=snapshot-agent, aicr.run/run-id=<RunID>, and
aicr.run/invocation-id, on the Job's pod template as well as the Job itself.
Select agent pods across runs with the component label; the Job name changes
every run.

aicr.run/invocation-id identifies the one Deployer that created the object,
which the first four labels cannot: Config.RunID is caller-settable and
sharing it is a supported scenario, so two invocations stamp identical values
for all four. The invocation ID is generated inside NewDeployer and settable
through no Config field, and it is what Cleanup requires before adopting an
object whose creation it never had confirmed. Do not select on it — its value
changes every invocation.

The staging ConfigMap is the exception: it is written from inside the pod by
pkg/serializer's ConfigMap writer, which stamps app.kubernetes.io/name=aicr,
app.kubernetes.io/component=<snapshot kind> and app.kubernetes.io/version — not
managed-by, not the run-ID label, and not the invocation-ID label. That writer also produces the user's
delivered cm:// artifact, so it deliberately does not stamp the run-ID sweep
key onto an object this package must never delete. Run scoping for the staging
ConfigMap comes from its name (see StagingConfigMapName), which is what both
Cleanup paths key on.

Job and Pod lifecycle waits use the Kubernetes watch API (not polling) for
efficiency. Pod selection narrows by label and then authorizes the candidate
against the controlling ownerReference carrying the recorded Job UID, since pod
labels are writable by anything that can update pods in the namespace.

# Cleanup

The Deployer records (kind, name) immediately before each Create and writes the
returned UID onto that entry on success. Cleanup deletes exactly that set,
passing the recorded UID as a metav1.Preconditions so a same-named object
belonging to another run is never collected; a UID mismatch (Conflict) and a
NotFound are both treated as success. Cleanup also runs on the Deploy failure
path, which is why it is scoped to what was created rather than to configured
names.

Recording before the Create is what keeps a lost Create response from
orphaning an object forever: if the apiserver commits the create but the
response never arrives, the entry is already in the set. That entry carries no
UID, and its (run-unique) name is not evidence of ownership — it says what
this run WOULD have created, not what is standing there now — so Cleanup never
deletes it by bare name. It Gets the live object and re-verifies it: the
delete is issued only when that object carries the full label set this
INVOCATION stamps at creation time — aicr.run/invocation-id included — AND a
non-empty UID, and it is pinned to the UID that Get observed. A label mismatch
or a missing UID fails closed — no delete at all, and a warning names the
object left behind for an operator to judge — while a NotFound means there is
nothing to reclaim. The one response that proves the object is not ours —
AlreadyExists — discards the entry again.

The invocation ID is what makes that re-verification mean anything. Pinning
the delete to the UID this Get returned protects only against a replacement
made after the Get; a replacement standing there before it is simply what the
Get returns. Without a per-invocation label, an object another invocation
created under the same RunID and the same name would pass every check and be
deleted.

The staging ConfigMap is written by the in-pod agent, so its UID is observed
when GetSnapshot reads it. When the run owns that ConfigMap and failed before
it could be observed, Cleanup Gets it by its run-scoped name and deletes it
pinned to the UID that Get returned. That object cannot carry the invocation
ID — the agent image writing it may be a different aicr version — so the sweep
takes its ownership evidence from the Job instead: it runs only while the Job
this invocation created is still the live Job at its name, which is the one
thing that rules out a second invocation's agent having written the ConfigMap
at the shared staging name.

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
		clientset := fake.NewClientset()
		deployer := agent.NewDeployer(clientset, agent.Config{
			Namespace: "test",
			RunID:     "20260821-142233-9f3a1c0b7e2d4a55",
			Image:     "test:latest",
		})
		// Test deployment logic...
	}
*/
package agent
