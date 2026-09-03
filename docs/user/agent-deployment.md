# Agent Deployment

Deploy AICR as a Kubernetes Job to automatically capture cluster configuration snapshots.

## Overview

The agent is a Kubernetes Job that captures system configuration and writes output to a ConfigMap.

**Deployment:** Use `aicr snapshot` to deploy and manage the Job programmatically.

**What it does:**

- Runs `aicr snapshot --namespace gpu-operator --output cm://gpu-operator/aicr-snapshot` on a GPU node
- Writes snapshot to ConfigMap via Kubernetes API (no PersistentVolume required)
- Exits after snapshot capture

**What it does not do:**

- Recipe generation (use `aicr recipe` CLI or API server)
- Bundle generation (use `aicr bundle` CLI)
- Continuous monitoring (use CronJob for periodic snapshots)

**Use cases:**

- Cluster auditing and compliance
- Multi-cluster configuration management
- Drift detection (compare snapshots over time)
- CI/CD integration (automated configuration validation)

### ConfigMap storage

Agent uses ConfigMap URI scheme (`cm://namespace/name`) to write snapshots:

```bash
aicr snapshot --namespace gpu-operator --output cm://gpu-operator/aicr-snapshot
```

The agent's namespaced Role grants ConfigMap write access only in its
deployment namespace (`--namespace`, default `default`). The `cm://` target
namespace **must match `--namespace`** — otherwise the Job's ServiceAccount has
no permission to create the ConfigMap and the snapshot write fails.

This creates:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: aicr-snapshot
  namespace: gpu-operator
  labels:
    app.kubernetes.io/name: aicr
    app.kubernetes.io/component: Snapshot
    app.kubernetes.io/version: <aicr-version>
data:
  snapshot.yaml: |  # Complete snapshot YAML
    apiVersion: aicr.run/v1alpha2
    kind: Snapshot
    measurements: [...]
  format: yaml
  timestamp: "2026-01-03T10:30:00Z"
```

## Prerequisites

- Kubernetes cluster with GPU nodes
- aicr CLI installed
- GPU Operator installed (or appropriate namespace configured via `--namespace`)
- Permission to create and delete the run's Job and RBAC in the target
  namespace, plus the cluster-scoped `ClusterRole`/`ClusterRoleBinding` the
  agent needs. Every run starts by verifying this and stops before touching
  the cluster if anything is missing — see
  [Pre-flight permission gate](#pre-flight-permission-gate). Pointing
  `--service-account-name` at a ServiceAccount you provisioned yourself
  requires **no** RBAC permissions at all

## Quick Start

### 1. Deploy Agent with Single Command

```shell
aicr snapshot
```

This single command:
1. Creates RBAC resources (ServiceAccount, Role, RoleBinding, ClusterRole, ClusterRoleBinding)
2. Deploys Job to capture snapshot
3. Waits for Job completion (5m timeout by default)
4. Retrieves snapshot from ConfigMap
5. Writes snapshot to stdout (or specified output)
6. Cleans up Job and RBAC resources (use `--no-cleanup` to keep for debugging)

### 2. View Snapshot Output

Snapshot is written to specified output:

```shell
# Output to stdout (default)
aicr snapshot

# Save to file
aicr snapshot --output snapshot.yaml

# Keep in ConfigMap for later use (deployment namespace must match the cm:// namespace)
aicr snapshot --namespace gpu-operator --output cm://gpu-operator/aicr-snapshot

# Retrieve from ConfigMap later
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}'
```

### 3. Customize Deployment

Target specific nodes and configure scheduling:

```shell
# Target GPU nodes with specific label
aicr snapshot \
  --node-selector accelerator=nvidia-h100

# Handle tainted nodes (by default all taints are tolerated)
# Only needed if you want to restrict which taints are tolerated
aicr snapshot \
  --toleration nvidia.com/gpu=present:NoSchedule

# Full customization
aicr snapshot \
  --namespace gpu-operator \
  --image ghcr.io/nvidia/aicr:v0.19.0 \
  --node-selector accelerator=nvidia-h100 \
  --toleration nvidia.com/gpu:NoSchedule \
  --timeout 10m \
  --output cm://gpu-operator/aicr-snapshot
```

**Available flags:**
- `--kubeconfig`: Custom kubeconfig path (default: `~/.kube/config` or `$KUBECONFIG`)
- `--namespace`: Deployment namespace (default: `default`)
- `--image`: Container image (default: matches the CLI version, e.g. `ghcr.io/nvidia/aicr:v0.19.0`; dev and snapshot builds use `:latest`)
- `--image-pull-secret`: Secret name for pulling the agent image from a private registry (repeatable)
- `--job-name`: Job name prefix (default: `aicr`); the run ID is always appended (`<prefix>-<run-id>`)
- `--service-account-name`: ServiceAccount the agent pod runs as. **Exact-if-exists** — an existing ServiceAccount of exactly this name in `--namespace` is used verbatim and the run creates no RBAC; otherwise it is a name prefix (default: `aicr`) and the run ID is appended (`<prefix>-<run-id>`). See [Using an existing ServiceAccount](#using-an-existing-serviceaccount-irsa-and-workload-identity)
- `--add-roles-to-service-account`: **Writes manifests and applies nothing.** Renders the RBAC that grants the agent's permissions to the named ServiceAccount into `./snapshot-rbac-<run-id>/` and exits **without taking a snapshot**. No cluster is contacted. You review the files, then apply and later delete them yourself. See [Using an existing ServiceAccount](#using-an-existing-serviceaccount-irsa-and-workload-identity)
- `--node-selector`: Node selector (format: `key=value`, repeatable)
- `--toleration`: Toleration (format: `key=value:effect`, repeatable). **Default: all taints are tolerated** (uses `operator: Exists` without key). Only specify this flag if you want to restrict which taints the Job can tolerate.
- `--timeout`: Wait timeout (default: `5m`)
- `--no-cleanup`: Skip removal of Job and RBAC resources on completion. **Warning:** leaves the run-scoped `aicr-node-reader-<run-id>` ClusterRole and ClusterRoleBinding active. By default these grant only read access to nodes, pods, ClusterPolicy CRDs, Slinky Controller/NodeSet/LoginSet/RestApi/Accounting CRs, and official MariaDB CRs (not cluster-admin); however, when combined with `--discover-network` the retained ClusterRole also carries the cluster-scoped **mutating** discovery rules (CRD/namespace/DaemonSet create-delete, `pods/exec`, `nodes/patch`, `NicClusterPolicy` patch — see [Security Considerations](#security-considerations)), so it is **not** read-only in that case.
- `--privileged`: Run agent in privileged mode (default: enabled; required for GPU/SystemD collectors). Set to `false` for PSS-restricted namespaces.
- `--require-gpu`: Fail the snapshot if no GPU is found. In agent mode also requests an `nvidia.com/gpu` resource for the pod (required in CDI environments).
- `--runtime-class`: Set `runtimeClassName` on the agent pod for `nvidia-smi` access without consuming a GPU. Use with `--node-selector` to target GPU nodes.
- `--os`: Node OS family (`ubuntu`, `rhel`, `cos`, `amazonlinux`, `ol`, `talos`). Selects the per-OS pod configuration and service collector backend.
- `--requests` / `--limits`: Override agent container resource requests/limits (comma-separated `name=quantity` pairs).
- `--cluster-config`: Path to a pre-existing k8s-launch-kit cluster-config.yaml to ingest network topology (local agent mode only).
- `--oke-addons`: Path to an `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json` dump, projected into the `K8s.oke-addons.nvidia-gpu-plugin` reading. Controller-side: works in agent Job mode too — the file never enters the pod; the CLI merges the projection into the returned snapshot.
- `--aks-gpu-pools`: Path to an `az aks nodepool list -o json` dump, projected into the `K8s.aks-gpu-pools.gpu-driver` reading. Controller-side: works in agent Job mode too — the file never enters the pod; the CLI merges the projection into the returned snapshot.
- `--discover-network`: Enable live l8k discovery to populate the NetworkTopology measurement. **Not read-only** — writes `nvidia.kubernetes-launch-kit.*` node labels and may patch `NicClusterPolicy`.

### 4. Check Agent Logs (Debugging)

If something goes wrong, check Job logs:

```shell
# Get Job status
kubectl get jobs -n gpu-operator -l app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent

# View logs
kubectl logs -n gpu-operator -l app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent

# Describe Job for events
kubectl describe job -n gpu-operator -l app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent
```

## Customization

### Node Selection

Target specific GPU nodes using `--node-selector`:

```shell
aicr snapshot --node-selector nvidia.com/gpu.present=true
```

**Common node selectors:**

| Selector | Purpose |
|----------|---------|
| `nvidia.com/gpu.present=true` | Any node with GPU |
| `nodeGroup=gpu-nodes` | Specific node pool (EKS/GKE) |
| `node.kubernetes.io/instance-type=p4d.24xlarge` | AWS instance type |
| `cloud.google.com/gke-accelerator=nvidia-tesla-h100` | GKE GPU type |

### Tolerations

By default, the agent Job tolerates **all taints** using the universal toleration (`operator: Exists` without a key). Only specify `--toleration` flags to **restrict** which taints are tolerated.

**Common tolerations:**

| Taint Key | Effect | Purpose |
|-----------|--------|---------|
| `nvidia.com/gpu` | NoSchedule | GPU Operator default |
| `dedicated` | NoSchedule | Dedicated GPU nodes |
| `workload` | NoSchedule | Workload-specific nodes |

### Image Version

Pin to a specific version:

```shell
aicr snapshot --image ghcr.io/nvidia/aicr:v0.19.0
```

**Finding versions:**
- [GitHub Releases](https://github.com/NVIDIA/aicr/releases)
- Container registry: [ghcr.io/nvidia/aicr](https://github.com/NVIDIA/aicr/pkgs/container/aicr)

## Using an existing ServiceAccount (IRSA and Workload Identity)

By default the agent creates its own ServiceAccount for each run and deletes it
at cleanup, so no two runs share an identity. That does not work when the
ServiceAccount must carry cloud IAM credentials: **EKS IRSA**
(`eks.amazonaws.com/role-arn`) and **GKE Workload Identity**
(`iam.gke.io/gcp-service-account`) both pin trust to the ServiceAccount *name*.
IRSA's role trust policy conditions on
`system:serviceaccount:<namespace>:<name>`, and a GKE IAM binding names the KSA
as `PROJECT.svc.id.goog[<namespace>/<name>]` and accepts no wildcard. A
per-run name can never be trusted by either, and copying the annotations onto a
run-scoped ServiceAccount does not help.

`--service-account-name` therefore behaves as **exact-if-exists**:

| Does a ServiceAccount of exactly that name exist in `--namespace`? | Behavior |
|---|---|
| Yes | Used verbatim. aicr creates **no** ServiceAccount, Role, RoleBinding, ClusterRole, or ClusterRoleBinding for the run, binds nothing to it, and deletes nothing at cleanup. |
| No | The value is a name prefix. The run creates `<prefix>-<run-id>` plus the full run-scoped RBAC set, and deletes them at cleanup — the pre-existing behavior. |

An unset `--service-account-name` is never probed for existence, so a stray
ServiceAccount named `aicr` cannot silently capture a run.

### Migrating a pre-created ServiceAccount

**What changed.** Before run isolation, passing `--service-account-name` at a
ServiceAccount you had created out of band got that ServiceAccount adopted, and
aicr attached its Role and RoleBinding to it. Run isolation made every
run-owned object run-scoped, which turned the flag into a prefix — so a
pre-created ServiceAccount stopped being used at all, and an agent pod that
had been running with IRSA or Workload Identity credentials silently started
running without them. Exact-if-exists restores the pre-created ServiceAccount
as the one the pod runs as; what does **not** come back is aicr managing its
permissions.

**Supported flow.** Generate the RBAC manifests, read them, apply them, then
take snapshots normally:

```shell
# 1. Your ServiceAccount, created and annotated by you (or by eksctl / Terraform).
kubectl create serviceaccount irsa-snapshotter -n gpu-operator
kubectl annotate serviceaccount irsa-snapshotter -n gpu-operator \
  eks.amazonaws.com/role-arn=arn:aws:iam::123456789012:role/aicr-snapshot

# 2. Write the RBAC manifests. Applies NOTHING, contacts no cluster, takes no
#    snapshot. Prints the directory it wrote and the commands below.
aicr snapshot --namespace gpu-operator --add-roles-to-service-account irsa-snapshotter

# 3. Read what you are about to grant. Each file explains its rules.
less snapshot-rbac-<run-id>/*.yaml

# 4. Grant it, once you are satisfied.
kubectl apply -f snapshot-rbac-<run-id>/

# 5. Capture snapshots as that ServiceAccount, as often as you like.
aicr snapshot \
  --namespace gpu-operator \
  --service-account-name irsa-snapshotter \
  --output cm://gpu-operator/aicr-snapshot

# 6. Revoke the grant when the ServiceAccount no longer needs it.
kubectl delete -f snapshot-rbac-<run-id>/
```

Step 2 does **not** check that the ServiceAccount exists — it contacts no
cluster at all, so it cannot. A mistyped name yields manifests naming a subject
that does not resolve; Kubernetes accepts such a binding and it simply grants
nothing. The generated `02-rolebinding.yaml` tells you how to verify the name,
and `aicr` never creates a ServiceAccount for you.

### What the manifests contain

`aicr snapshot --add-roles-to-service-account <sa>` writes a new directory in
your current working directory, one object per file:

```text
snapshot-rbac-<run-id>/
├── 01-role.yaml                 Role/aicr-agent-<sa>-rbac                       (namespaced)
├── 02-rolebinding.yaml          RoleBinding/aicr-agent-<sa>-rbac                (namespaced)
├── 03-clusterrole.yaml          ClusterRole/aicr-agent-<ns>.<sa>-rbac           (cluster)
└── 04-clusterrolebinding.yaml   ClusterRoleBinding/aicr-agent-<ns>.<sa>-rbac    (cluster)
```

Every file opens with a YAML comment header naming what the object grants and
why the agent needs each rule, so you can decide rule by rule whether to apply
it. The numeric prefixes exist because `kubectl apply -f <dir>/` visits a
directory in lexical order — they keep each Role ahead of the binding that
references it. You can also apply or read the files individually.

The rules are the same ones a run-scoped grant carries, so a shared
ServiceAccount is never less capable than a run-owned one. The names end in
`-rbac`, which no run-scoped name can (a run-scoped name always ends in a run
ID whose last segment is hexadecimal), so the two name spaces cannot collide.

**Nothing is applied for you, and nothing is removed for you.** The objects you
apply carry no `aicr.run/run-id` label, no run enters them into its cleanup
list, and no `aicr snapshot` or `aicr validate` invocation deletes them.
Teardown is one command, which is why keeping the directory is worth it:

```shell
kubectl delete -f snapshot-rbac-<run-id>/
```

If you no longer have the directory, delete the objects by name instead:

```shell
kubectl delete role,rolebinding aicr-agent-irsa-snapshotter-rbac -n gpu-operator
kubectl delete clusterrole,clusterrolebinding aicr-agent-gpu-operator.irsa-snapshotter-rbac
```

The directory name carries a fresh run ID on every invocation, so generating
twice never overwrites a set you are still reviewing; a directory that already
exists fails the command with `CONFLICT`. After an aicr upgrade, re-generate
and `kubectl apply -f` the new directory to refresh the rules in place.

The cluster-scoped pair is named `aicr-agent-<namespace>.<service-account>-rbac`.
The two segments join on `.`, which a namespace can never contain, so no other
namespace and ServiceAccount combination can produce the same name — applying
one grant cannot retarget another's.

### Trade-off: per-run permission isolation is waived

Using an existing ServiceAccount is an opt-in exchange, and it is worth
understanding before you choose it:

- **Concurrent runs share one identity.** Two `aicr snapshot` invocations using
  the same ServiceAccount hold exactly the same grants. Run isolation's
  guarantee that one run's permissions cannot reach another run's does not
  apply to them.
- **`--discover-network` grants become permanent.** With a run-owned
  ServiceAccount, the cluster-scoped **mutating** discovery rules
  (`nodes: patch`, `pods/exec: create`, CRD / namespace / DaemonSet
  create-delete — see [Security Considerations](#security-considerations))
  exist for one run and are revoked at cleanup. Rendered with
  `aicr snapshot --add-roles-to-service-account <sa> --discover-network` and
  applied, they sit on that ServiceAccount until you remove them.

Generate without `--discover-network` unless you need live network discovery;
that grant is read-only. When you do use it, `03-clusterrole.yaml` carries a
warning header naming every mutating rule and the discovery step it exists for
— read it before applying, which is precisely what writing manifests instead of
applying them is for. If you need discovery only occasionally, prefer a
run-owned ServiceAccount for those runs, or keep a separate ServiceAccount used
only for discovery.

## Post-Deployment

### Retrieve Snapshot

```shell
# View snapshot from ConfigMap
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}'

# Save to file
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot-$(date +%Y%m%d).yaml
```

### Generate Recipe from Snapshot

```shell
# Use ConfigMap directly (no file needed)
aicr recipe --snapshot cm://gpu-operator/aicr-snapshot --intent training --platform kubeflow --output recipe.yaml

# Generate bundle
aicr bundle --recipe recipe.yaml --output ./bundles
```

## Complete Workflow

```shell
# Step 1: Capture snapshot to ConfigMap (deployment namespace must match the cm:// namespace)
aicr snapshot --namespace gpu-operator --output cm://gpu-operator/aicr-snapshot

# Step 2: Generate recipe from ConfigMap
aicr recipe \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --intent training \
  --platform kubeflow \
  --output recipe.yaml

# Step 3: Create deployment bundle
aicr bundle \
  --recipe recipe.yaml \
  --output ./bundles

# Step 4: Deploy to cluster
cd bundles && chmod +x deploy.sh && ./deploy.sh

# Step 5: Verify deployment
kubectl get pods -n gpu-operator
kubectl logs -n gpu-operator -l app=nvidia-operator-validator
```

## Integration Patterns

### CI/CD Pipeline

```yaml
# GitHub Actions example
- name: Capture snapshot using agent
  run: |
    aicr snapshot \
      --namespace gpu-operator \
      --output cm://gpu-operator/aicr-snapshot \
      --timeout 10m

- name: Generate recipe from ConfigMap
  run: |
    aicr recipe \
      --snapshot cm://gpu-operator/aicr-snapshot \
      --intent training \
      --output recipe.yaml

- name: Generate bundle
  run: |
    aicr bundle -r recipe.yaml -o ./bundles

- name: Upload artifacts
  uses: actions/upload-artifact@v4
  with:
    name: cluster-config
    path: |
      recipe.yaml
      bundles/
```

### Multi-Cluster Auditing

```shell
#!/bin/bash
# Capture snapshots from multiple clusters

clusters=("prod-us-east" "prod-eu-west" "staging")

for cluster in "${clusters[@]}"; do
  echo "Capturing snapshot from $cluster..."

  # Switch context
  kubectl config use-context $cluster

  # Deploy agent and capture snapshot
  aicr snapshot \
    --namespace gpu-operator \
    --output snapshot-${cluster}.yaml \
    --timeout 10m
done
```

### Drift Detection

```shell
#!/bin/bash
# Compare current snapshot with baseline

# Baseline (first snapshot)
aicr snapshot --output baseline.yaml

# Current (later snapshot)
aicr snapshot --output current.yaml

# Compare (semantic snapshot diff; --fail-on-drift exits non-zero on drift)
aicr diff --baseline baseline.yaml --target current.yaml --fail-on-drift \
  || { echo "Configuration drift detected!"; exit 1; }
```

## Troubleshooting

### Job Fails to Start

Check RBAC permissions. The ServiceAccount name is run-scoped (`aicr-<run-id>`), so look it up first.

Select it by run ID, not by position: with concurrent snapshot runs the namespace
holds one ServiceAccount per run, and `.items[0]` would pick an arbitrary one — so
the checks below could report on a healthy run while you are debugging a failed one.
The CLI logs the run ID when it starts (`snapshot agent run: runID=...`), and the
Job, its pods, and its RBAC resources all carry it as the `aicr.run/run-id`
label. (The staging ConfigMap is the exception — it is written from inside the
pod and carries only `app.kubernetes.io/name`, `app.kubernetes.io/component` and
`app.kubernetes.io/version`; find it by its run-scoped name instead, see
[Job Completes but No Output](#job-completes-but-no-output).)

```shell
# From the failing Job (or use the runID the CLI logged at start).
RUN_ID=$(kubectl get job -n gpu-operator <job-name> -o jsonpath='{.metadata.labels.aicr\.run/run-id}')

SA=$(kubectl get sa -n gpu-operator -l app.kubernetes.io/name=aicr,aicr.run/run-id=$RUN_ID -o jsonpath='{.items[0].metadata.name}')
kubectl auth can-i get nodes --as=system:serviceaccount:gpu-operator:$SA
kubectl auth can-i get pods --as=system:serviceaccount:gpu-operator:$SA
kubectl auth can-i list controllers.slinky.slurm.net --all-namespaces \
  --as=system:serviceaccount:gpu-operator:$SA
kubectl auth can-i list nodesets.slinky.slurm.net --all-namespaces \
  --as=system:serviceaccount:gpu-operator:$SA
kubectl auth can-i list loginsets.slinky.slurm.net --all-namespaces \
  --as=system:serviceaccount:gpu-operator:$SA
kubectl auth can-i list restapis.slinky.slurm.net --all-namespaces \
  --as=system:serviceaccount:gpu-operator:$SA
kubectl auth can-i list accountings.slinky.slurm.net --all-namespaces \
  --as=system:serviceaccount:gpu-operator:$SA
kubectl auth can-i list mariadbs.k8s.mariadb.com --all-namespaces \
  --as=system:serviceaccount:gpu-operator:$SA
```

### Job Pending

Check node selectors and tolerations:

```shell
# View pod events
kubectl describe pod -n gpu-operator -l app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent

# Check node labels
kubectl get nodes --show-labels

# Check node taints
kubectl get nodes -o custom-columns=NAME:.metadata.name,TAINTS:.spec.taints
```

### Job Completes but No Output

Check ConfigMap and container logs:

```shell
# Every name below is run-scoped, so start from the run ID. The CLI logs it
# when it starts ("snapshot agent run: runID=..."); this reads it back off the
# Job instead. Substitute your own Job name (or use the logged run ID).
RUN_ID=$(kubectl get job -n gpu-operator <job-name> -o jsonpath='{.metadata.labels.aicr\.run/run-id}')

# Check if the staging ConfigMap was created. Without an explicit
# "-o cm://<namespace>/<name>", the agent stages its result in a run-scoped
# ConfigMap named "aicr-agent-snapshot-<run-id>" that cleanup deletes when
# the run ends — pass --no-cleanup to keep it around for inspection.
#
# This object is written by the in-pod agent, not by the CLI, so it does NOT
# carry the aicr.run/run-id label the Job and RBAC resources do. Address it by
# name: with concurrent runs, the label selector below matches every run's
# staging ConfigMap plus any delivered cm:// artifact in the namespace.
kubectl get configmap -n gpu-operator aicr-agent-snapshot-$RUN_ID

# View ConfigMap contents
kubectl get configmap -n gpu-operator aicr-agent-snapshot-$RUN_ID -o yaml

# Or list every aicr-written ConfigMap in the namespace (all runs)
kubectl get configmap -n gpu-operator -l app.kubernetes.io/name=aicr

# View pod logs for errors
kubectl logs -n gpu-operator -l app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent
```

### Permission Denied

A run that stops with `missing required permissions` never reached the cluster
— read the list it printed, which names every missing verb, its scope, and
whether you or the agent ServiceAccount lacked it. See
[Pre-flight permission gate](#pre-flight-permission-gate) for the full matrix
and for why exact-ServiceAccount mode needs fewer of them.

If the run got past the gate, verify the RBAC it deployed:

```shell
# Verify ClusterRole (run-scoped: "aicr-node-reader-<run-id>")
kubectl get clusterrole -l app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent

# Verify ClusterRoleBinding
kubectl get clusterrolebinding -l app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent

# Verify Role and RoleBinding (run-scoped: "aicr-<run-id>")
kubectl get role -n gpu-operator -l app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent
kubectl get rolebinding -n gpu-operator -l app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent

# Verify ServiceAccount (run-scoped: "aicr-<run-id>")
kubectl get serviceaccount -n gpu-operator -l app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent
```

### Cleanup Left a Resource Behind

Cleanup removes only the objects the run itself created, and pins every delete
to the UID the apiserver returned when it created them. If a create's response
is lost in flight, the run knows the name it used but never learned the
object's identity — so cleanup re-reads the object and deletes it only when it
still carries that invocation's own labels. When something else has taken the
name over, cleanup keeps the object and says so:

```text
WARN cleanup left behind an object it cannot prove this run created; if it is a
stale orphan of this run, remove it by hand kind=Role name=aicr-<run-id>
runID=<run-id> objectRunID=<other-run-id>
objectInvocationID=<other-invocation-id>
```

The deciding label there is `aicr.run/invocation-id`, not `aicr.run/run-id`.
Every object a run creates carries both, but the run ID is yours to set — `aicr
validate` gives one ID to its snapshot agent and its validation Jobs, and
pinned CI runs reuse one on purpose — so two commands can legitimately stamp
the same one. The invocation ID is generated inside the process for each
`Deployer` and is not configurable, so it is what separates "the object I
created" from "an object another invocation created at the same name". Do not
select on it: its value changes every invocation.

Inspect the named object and delete it yourself if it is a stale leftover:

```shell
kubectl get role -n gpu-operator aicr-<run-id> -o yaml
```

The staging ConfigMap is warned about the same way, but proved differently. It
is written by the in-pod agent rather than by the CLI, so it carries neither of
those labels (see [Job Completes but No Output](#job-completes-but-no-output))
and the agent image may be a different aicr version than the CLI. Cleanup
therefore takes its evidence from the Job whose pod wrote it: it sweeps the
ConfigMap only when this run created that Job successfully AND that same Job —
matched by UID, not by name — is still the one standing in the cluster, and
only when the object looks like an aicr artifact
(`app.kubernetes.io/name=aicr`). If the Job was deleted or replaced first, the
ConfigMap is kept:

```text
WARN cleanup left behind the ConfigMap at this run's staging name: this run no
longer holds the Job whose agent would have written it
name=aicr-agent-snapshot-<run-id> jobName=aicr-<run-id> jobUID=<uid>
liveJobUID=<other-uid>
```

Delete it by hand once you have confirmed it is not a live run's:

```shell
kubectl get cm -n gpu-operator aicr-agent-snapshot-<run-id> -o yaml
```

## Security Considerations

### RBAC Permissions

The agent requires these permissions (created automatically by the CLI):
- **ClusterRole** (`aicr-node-reader-<run-id>`, run-scoped): Read access to nodes and pods; `get`/`list` access to ClusterPolicy CRDs (`nvidia.com`); cluster-wide `list` access to Slinky Controller, NodeSet, LoginSet, RestApi, and Accounting CRs (`slinky.slurm.net`); and cluster-wide `list` access to official MariaDB CRs (`k8s.mariadb.com`)
- **Role** (`aicr-<run-id>`, run-scoped): Create/update ConfigMaps and list pods in the deployment namespace

The baseline ClusterRole above is read-only (`get`/`list` only). Slinky
detection projects only allowlisted identity, association, and boolean fields;
it omits free-form configuration, status, pod templates, and Secret/ConfigMap
references or contents. MariaDB detection records only official API-group and
CR presence; it does not inspect database configuration, Services, operator
Deployments, pods, or external databases.

**Additional privileges with `--discover-network`.** When `--discover-network`
is set, the CLI appends a set of **cluster-scoped mutating** rules to the
ClusterRole so k8s-launch-kit's live discovery can run. These grant far more
than read access:
- `apiextensions.k8s.io` CustomResourceDefinitions: `get`, `list`, `create`, `update`, `patch`
- `namespaces`: `get`, `create`, `delete` (l8k creates and tears down a bootstrap namespace)
- `apps/daemonsets`: `get`, `list`, `watch`, `create`, `delete`
- `serviceaccounts`, `configmaps`: `get`, `create`, `delete`
- `rbac.authorization.k8s.io` roles, rolebindings: `get`, `create`, `delete`
- `pods/exec`: `create` (l8k exec's into the discovery DaemonSet pods to read VPD / link state)
- `nodes`: `patch` (writes `nvidia.kubernetes-launch-kit.*` node labels)
- `configuration.net.nvidia.com` nicdevices: `get`, `list`
- `mellanox.com` nicclusterpolicies: `get`, `patch`

Use `--discover-network` only against clusters where this mutation and the
broader RBAC grant are acceptable.

### Pre-flight permission gate

Every run begins by verifying the permissions it will actually use and stops
before writing anything if any are missing. The gate is read-only: an access
review is an authorization query that persists nothing, and the one object it
reads — the ServiceAccount named by `--service-account-name` — is read with
`get`. A failed gate therefore leaves the cluster exactly as it found it.

All failures are reported together, so one run tells you everything to fix.
Each line names the verb, the resource, the scope, and which of the two
identities lacked it:

```text
missing required permissions:
  - the caller (your kubeconfig identity) cannot "delete" clusterroles.rbac.authorization.k8s.io (cluster-scoped)
  - agent ServiceAccount "system:serviceaccount:gpu-operator/irsa-snapshotter" cannot "list" nodes (cluster-scoped)
```

#### What the caller must be able to do

Checked with `SelfSubjectAccessReview` against your kubeconfig identity. The
first group is required in both ServiceAccount modes:

| Resource | Verbs | Scope | Used by |
|---|---|---|---|
| `serviceaccounts` | `get` | namespace | Deciding whether `--service-account-name` names an existing ServiceAccount or is a prefix |
| `jobs.batch` | `create`, `get`, `list`, `watch`, `delete` | namespace | Creating the agent Job, waiting on it, cleaning it up |
| `pods` | `get`, `list`, `watch` | namespace | Finding the agent pod and waiting for it to be ready |
| `pods/log` | `get` | namespace | Streaming the agent's output back to your terminal |
| `configmaps` | `get`, `list` | namespace | Reading the snapshot the agent staged |
| `configmaps` | `delete` | namespace | Only when aicr owns the output ConfigMap (i.e. you did not pass your own `cm://` URI) |

`serviceaccounts: get` is not optional. Without it the run cannot tell whether
`--service-account-name` names an existing ServiceAccount or is a prefix, and
guessing "prefix" would run the agent under a generated ServiceAccount carrying
none of the named account's IRSA or Workload Identity annotations. The run
stops instead of guessing.

The second group is required **only in prefix mode** — when the run creates
its own run-scoped ServiceAccount:

| Resource | Verbs | Scope |
|---|---|---|
| `serviceaccounts` | `create`, `delete` | namespace |
| `roles.rbac.authorization.k8s.io` | `create`, `delete` | namespace |
| `rolebindings.rbac.authorization.k8s.io` | `create`, `delete` | namespace |
| `clusterroles.rbac.authorization.k8s.io` | `create`, `delete` | cluster |
| `clusterrolebindings.rbac.authorization.k8s.io` | `create`, `delete` | cluster |

`delete` is required alongside `create` because cleanup always runs, including
on the failure path. An identity that can create but not delete would leave a
ServiceAccount, Role, RoleBinding, ClusterRole and ClusterRoleBinding behind on
every single run.

**Exact-ServiceAccount mode requires fewer caller permissions.** When
`--service-account-name` names a ServiceAccount that already exists, the run
creates and deletes no RBAC at all, so none of the five kinds above is
demanded of you. What it requires instead is that the ServiceAccount was
actually provisioned — see below.

#### What the agent ServiceAccount must be able to do

The agent pod runs as a ServiceAccount, not as you, so its permissions are
checked separately with `SubjectAccessReview` naming
`system:serviceaccount:<namespace>:<name>` as the subject. The questions are
derived from the same rule set the run-scoped `Role` and `ClusterRole` grant
(and that `--add-roles-to-service-account` renders), so the gate cannot fall
behind what the agent needs: namespaced `configmaps` and `pods` access, plus
cluster-scoped `nodes`, `pods`, `nvidia.com` ClusterPolicies, the Slinky CRs
and the MariaDB CRs — widened to the full mutating set when
`--discover-network` is passed.

This check runs **in exact-ServiceAccount mode only**. In prefix mode the
ServiceAccount does not exist yet and the run is about to grant it exactly
those rules, so there is nothing to verify. In exact mode aicr grants nothing,
and the most common failure is that the manifests from
`--add-roles-to-service-account` were rendered but never applied. The gate
catches that up front and tells you how to fix it, rather than letting a Job
start and fail inside the pod minutes later.

**When the ServiceAccount's permissions cannot be verified.** Creating a
`SubjectAccessReview` is itself a privilege. If you do not hold it, the run
does **not** silently skip the check — it says so and continues, because the
agent will still fail visibly in-pod if a rule is missing:

```text
WARN could not verify the agent ServiceAccount's own permissions; continuing,
but a missing rule will surface as an in-pod failure minutes from now instead
of here serviceAccount=irsa-snapshotter namespace=gpu-operator
uncheckedRules=18 remedy="grant the caller 'create
subjectaccessreviews.authorization.k8s.io', or verify by hand with: kubectl
auth can-i --list --as system:serviceaccount:gpu-operator/irsa-snapshotter"
```

### Pod Security Context

The agent requires elevated privileges to collect system configuration from the host:
- `hostPID`, `hostNetwork`, `hostIPC`: Required to read host system configuration
- `privileged` + `SYS_ADMIN`: Required to access GPU configuration and kernel parameters
- `/run/systemd` mount: Required to query systemd service states

## See Also

- [CLI Reference](cli-reference.md) - aicr CLI commands
- [Installation Guide](installation.md) - Install CLI locally
- [API Reference](api-reference.md) - REST API usage
- [Kubernetes Deployment](../integrator/kubernetes-deployment.md) - API server deployment
