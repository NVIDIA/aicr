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
    app.kubernetes.io/component: snapshot
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
- Cluster admin permissions (for RBAC setup)

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
  --image ghcr.io/nvidia/aicr:v0.8.0 \
  --node-selector accelerator=nvidia-h100 \
  --toleration nvidia.com/gpu:NoSchedule \
  --timeout 10m \
  --output cm://gpu-operator/aicr-snapshot
```

**Available flags:**
- `--kubeconfig`: Custom kubeconfig path (default: `~/.kube/config` or `$KUBECONFIG`)
- `--namespace`: Deployment namespace (default: `default`)
- `--image`: Container image (default: matches the CLI version, e.g. `ghcr.io/nvidia/aicr:v0.8.0`; dev and snapshot builds use `:latest`)
- `--image-pull-secret`: Secret name for pulling the agent image from a private registry (repeatable)
- `--job-name`: Job name (default: `aicr`)
- `--service-account-name`: ServiceAccount name (default: `aicr`)
- `--node-selector`: Node selector (format: `key=value`, repeatable)
- `--toleration`: Toleration (format: `key=value:effect`, repeatable). **Default: all taints are tolerated** (uses `operator: Exists` without key). Only specify this flag if you want to restrict which taints the Job can tolerate.
- `--timeout`: Wait timeout (default: `5m`)
- `--no-cleanup`: Skip removal of Job and RBAC resources on completion. **Warning:** leaves the `aicr-node-reader` ClusterRole and ClusterRoleBinding active. By default these grant only read access to nodes, pods, and ClusterPolicy CRDs (not cluster-admin); however, when combined with `--discover-network` the retained ClusterRole also carries the cluster-scoped **mutating** discovery rules (CRD/namespace/DaemonSet create-delete, `pods/exec`, `nodes/patch`, `NicClusterPolicy` patch — see [Security Considerations](#security-considerations)), so it is **not** read-only in that case.
- `--privileged`: Run agent in privileged mode (default: enabled; required for GPU/SystemD collectors). Set to `false` for PSS-restricted namespaces.
- `--require-gpu`: Fail the snapshot if no GPU is found. In agent mode also requests an `nvidia.com/gpu` resource for the pod (required in CDI environments).
- `--runtime-class`: Set `runtimeClassName` on the agent pod for `nvidia-smi` access without consuming a GPU. Use with `--node-selector` to target GPU nodes.
- `--os`: Node OS family (`ubuntu`, `rhel`, `cos`, `amazonlinux`, `talos`). Selects the per-OS pod configuration and service collector backend.
- `--requests` / `--limits`: Override agent container resource requests/limits (comma-separated `name=quantity` pairs).
- `--cluster-config`: Path to a pre-existing k8s-launch-kit cluster-config.yaml to ingest network topology (local agent mode only).
- `--discover-network`: Enable live l8k discovery to populate the NetworkTopology measurement. **Not read-only** — writes `nvidia.kubernetes-launch-kit.*` node labels and may patch `NicClusterPolicy`.

### 4. Check Agent Logs (Debugging)

If something goes wrong, check Job logs:

```shell
# Get Job status
kubectl get jobs -n gpu-operator

# View logs
kubectl logs -n gpu-operator job/aicr

# Describe Job for events
kubectl describe job aicr -n gpu-operator
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
aicr snapshot --image ghcr.io/nvidia/aicr:v0.8.0
```

**Finding versions:**
- [GitHub Releases](https://github.com/NVIDIA/aicr/releases)
- Container registry: [ghcr.io/nvidia/aicr](https://github.com/NVIDIA/aicr/pkgs/container/aicr)

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

Check RBAC permissions:
```shell
kubectl auth can-i get nodes --as=system:serviceaccount:gpu-operator:aicr
kubectl auth can-i get pods --as=system:serviceaccount:gpu-operator:aicr
```

### Job Pending

Check node selectors and tolerations:
```shell
# View pod events
kubectl describe pod -n gpu-operator -l job-name=aicr

# Check node labels
kubectl get nodes --show-labels

# Check node taints
kubectl get nodes -o custom-columns=NAME:.metadata.name,TAINTS:.spec.taints
```

### Job Completes but No Output

Check ConfigMap and container logs:
```shell
# Check if ConfigMap was created
kubectl get configmap aicr-snapshot -n gpu-operator

# View ConfigMap contents
kubectl get configmap aicr-snapshot -n gpu-operator -o yaml

# View pod logs for errors
kubectl logs -n gpu-operator -l job-name=aicr
```

### Permission Denied

Ensure RBAC is correctly deployed:
```shell
# Verify ClusterRole
kubectl get clusterrole aicr-node-reader

# Verify ClusterRoleBinding
kubectl get clusterrolebinding aicr-node-reader

# Verify Role and RoleBinding
kubectl get role aicr -n gpu-operator
kubectl get rolebinding aicr -n gpu-operator

# Verify ServiceAccount
kubectl get serviceaccount aicr -n gpu-operator
```

## Security Considerations

### RBAC Permissions

The agent requires these permissions (created automatically by the CLI):
- **ClusterRole** (`aicr-node-reader`): Read access to nodes, pods, and ClusterPolicy CRDs (nvidia.com)
- **Role** (`aicr`): Create/update ConfigMaps and list pods in the deployment namespace

The baseline ClusterRole above is read-only (`get`/`list` only).

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
