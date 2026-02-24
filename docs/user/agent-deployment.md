# Agent Deployment

Deploy AICR as a Kubernetes Job to automatically capture cluster configuration snapshots.

## Overview

The agent is a Kubernetes Job that captures system configuration and writes output to a ConfigMap.

**Deployment options:**

1. **CLI-based deployment** (recommended): Use `aicr snapshot --deploy-agent` to deploy and manage Job programmatically
2. **kubectl deployment**: Manually apply YAML manifests with `kubectl apply`

**What it does:**

- Runs `aicr snapshot --output cm://gpu-operator/aicr-snapshot` on a GPU node
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

**ConfigMap storage:**

Agent uses ConfigMap URI scheme (`cm://namespace/name`) to write snapshots:
```bash
aicr snapshot --output cm://gpu-operator/aicr-snapshot
```

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
    app.kubernetes.io/version: v0.17.0
data:
  snapshot.yaml: |  # Complete snapshot YAML
    apiVersion: aicr.nvidia.com/v1alpha1
    kind: Snapshot
    measurements: [...]
  format: yaml
  timestamp: "2026-01-03T10:30:00Z"
```

## Prerequisites

- Kubernetes cluster with GPU nodes
- `kubectl` configured with cluster access (for manual deployment) OR aicr CLI installed (for CLI-based deployment)
- GPU Operator installed (agent runs in `gpu-operator` namespace)
- Cluster admin permissions (for RBAC setup)

## Quick Start (CLI-Based Deployment)

**Recommended approach**: Deploy agent programmatically using the CLI.

### 1. Deploy Agent with Single Command

```shell
aicr snapshot --deploy-agent
```

This single command:
1. Creates RBAC resources (ServiceAccount, Role, RoleBinding, ClusterRole, ClusterRoleBinding)
2. Deploys Job to capture snapshot
3. Waits for Job completion (5m timeout by default)
4. Retrieves snapshot from ConfigMap
5. Writes snapshot to stdout (or specified output)
6. Cleans up Job and RBAC resources (use `--cleanup=false` to keep for debugging)

### 2. View Snapshot Output

Snapshot is written to specified output:

```shell
# Output to stdout (default)
aicr snapshot --deploy-agent

# Save to file
aicr snapshot --deploy-agent --output snapshot.yaml

# Keep in ConfigMap for later use
aicr snapshot --deploy-agent --output cm://gpu-operator/aicr-snapshot

# Retrieve from ConfigMap later
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}'
```

### 3. Customize Deployment

Target specific nodes and configure scheduling:

```shell
# Target GPU nodes with specific label
aicr snapshot --deploy-agent \
  --node-selector accelerator=nvidia-h100

# Handle tainted nodes (by default all taints are tolerated)
# Only needed if you want to restrict which taints are tolerated
aicr snapshot --deploy-agent \
  --toleration nvidia.com/gpu=present:NoSchedule

# Full customization
aicr snapshot --deploy-agent \
  --namespace gpu-operator \
  --image ghcr.io/nvidia/aicr:v0.8.0 \
  --node-selector accelerator=nvidia-h100 \
  --toleration nvidia.com/gpu:NoSchedule \
  --timeout 10m \
  --output cm://gpu-operator/aicr-snapshot
```

**Available flags:**
- `--deploy-agent`: Enable agent deployment mode
- `--kubeconfig`: Custom kubeconfig path (default: `~/.kube/config` or `$KUBECONFIG`)
- `--namespace`: Deployment namespace (default: `gpu-operator`)
- `--image`: Container image (default: `ghcr.io/nvidia/aicr-validator:latest`)
- `--job-name`: Job name (default: `aicr`)
- `--service-account-name`: ServiceAccount name (default: `aicr`)
- `--node-selector`: Node selector (format: `key=value`, repeatable)
- `--toleration`: Toleration (format: `key=value:effect`, repeatable). **Default: all taints are tolerated** (uses `operator: Exists` without key). Only specify this flag if you want to restrict which taints the Job can tolerate.
- `--timeout`: Wait timeout (default: `5m`)
- `--cleanup`: Delete Job and RBAC resources on completion. **Default: `true`**. Use `--cleanup=false` to keep resources for debugging.

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

## Manual Deployment (kubectl)

Alternative approach using kubectl with YAML manifests.

### 1. Deploy RBAC and ServiceAccount

The agent requires permissions to read Kubernetes resources and write to ConfigMaps:

```shell
kubectl apply -f https://raw.githubusercontent.com/nvidia/aicr/main/deployments/aicr-agent/1-deps.yaml
```

**What this creates:**
- **Namespace**: `gpu-operator` (if not exists)
- **ServiceAccount**: `aicr` in `gpu-operator` namespace
- **Role**: `aicr` - Permissions to create/update ConfigMaps and list pods in `gpu-operator` namespace
- **RoleBinding**: `aicr` - Binds Role to ServiceAccount in `gpu-operator` namespace
- **ClusterRole**: `aicr-node-reader` - Permissions to read nodes, pods, secrets (Helm releases), services, ClusterPolicy (nvidia.com), and Application (argoproj.io)
- **ClusterRoleBinding**: `aicr-node-reader` - Binds ClusterRole to ServiceAccount

### 2. Deploy the Agent Job

```shell
kubectl apply -f https://raw.githubusercontent.com/nvidia/aicr/main/deployments/aicr-agent/2-job.yaml
```

**What this creates:**
- **Job**: `aicr` in the `gpu-operator` namespace
- Job runs `aicr snapshot --output cm://gpu-operator/aicr-snapshot`
- Snapshot is written directly to ConfigMap via Kubernetes API

### 3. View Snapshot Output

Check job status:
```shell
kubectl get jobs -n gpu-operator
```

Check job logs (for errors/debugging):
```shell
kubectl logs -n gpu-operator job/aicr
```

Retrieve snapshot from ConfigMap:
```shell
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}'
```

Save snapshot to file:
```shell
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot.yaml
```

## Customization

Before deploying, you may need to customize the Job manifest for your environment.

### Download and Edit Manifest

```shell
# Download job manifest
curl -O https://raw.githubusercontent.com/nvidia/aicr/main/deployments/aicr-agent/2-job.yaml

# Edit with your preferred editor
vim 2-job.yaml
```

### Node Selection

Target specific GPU nodes using `nodeSelector`:

```yaml
spec:
  template:
    spec:
      nodeSelector:
        nvidia.com/gpu.present: "true"        # Any GPU node
        # nodeGroup: your-gpu-node-group      # Specific node group
        # instance-type: p4d.24xlarge         # Specific instance type
```

**Common node selectors:**

| Selector | Purpose |
|----------|---------|
| `nvidia.com/gpu.present: "true"` | Any node with GPU |
| `nodeGroup: gpu-nodes` | Specific node pool (EKS/GKE) |
| `node.kubernetes.io/instance-type: p4d.24xlarge` | AWS instance type |
| `cloud.google.com/gke-accelerator: nvidia-tesla-h100` | GKE GPU type |

### Tolerations

**CLI-deployed agents**: By default, the agent Job tolerates **all taints** using the universal toleration (`operator: Exists` without a key). This means the agent can schedule on any node regardless of taints. Only specify `--toleration` flags if you want to **restrict** which taints are tolerated.

**kubectl-deployed agents**: If deploying manually with YAML manifests, you need to explicitly add tolerations for tainted nodes:

```yaml
spec:
  template:
    spec:
      tolerations:
        # Universal toleration (same as CLI default)
        - operator: Exists
        # Or specify individual taints:
        - key: nvidia.com/gpu
          operator: Exists
          effect: NoSchedule
        - key: dedicated
          operator: Equal
          value: gpu
          effect: NoSchedule
```

**Common tolerations:**

| Taint Key | Effect | Purpose |
|-----------|--------|---------|
| `nvidia.com/gpu` | NoSchedule | GPU Operator default |
| `dedicated` | NoSchedule | Dedicated GPU nodes |
| `workload` | NoSchedule | Workload-specific nodes |

### Image Version

Use a specific version instead of `latest`:

```yaml
spec:
  template:
    spec:
      containers:
        - name: aicr
          image: ghcr.io/nvidia/aicr:v0.8.0  # Pin to version
```

**Finding versions:**
- [GitHub Releases](https://github.com/nvidia/aicr/releases)
- Container registry: [ghcr.io/nvidia/aicr](https://github.com/nvidia/aicr/pkgs/container/aicr)

### Resource Limits

The agent uses the following default resource allocations:

```yaml
spec:
  template:
    spec:
      containers:
        - name: aicr
          resources:
            requests:
              cpu: "1"
              memory: "4Gi"
              ephemeral-storage: "2Gi"
            limits:
              cpu: "2"
              memory: "8Gi"
              ephemeral-storage: "4Gi"
```

You can adjust these values in a custom Job manifest if needed.

### Custom Output Format

Change output format via command arguments:

```yaml
spec:
  template:
    spec:
      containers:
        - name: aicr
          args:
            - snapshot
            - --format
            - json  # Change to: yaml, json, table
```

## Deployment Examples

### Example 1: AWS EKS with GPU Node Group

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: aicr
  namespace: gpu-operator
  labels:
    app.kubernetes.io/name: aicr
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    spec:
      serviceAccountName: aicr
      restartPolicy: Never
      hostPID: true
      hostNetwork: true
      hostIPC: true
      nodeSelector:
        nodeGroup: gpu-nodes  # Your EKS node group
      tolerations:
        - key: nvidia.com/gpu
          operator: Exists
          effect: NoSchedule
      securityContext:
        runAsUser: 0
        runAsGroup: 0
        fsGroup: 0
      containers:
        - name: aicr
          image: ghcr.io/nvidia/aicr-validator:latest
          command: ["/bin/sh", "-c"]
          args: ["aicr snapshot -o cm://gpu-operator/aicr-snapshot"]
          securityContext:
            privileged: true
          volumeMounts:
            - name: run-systemd
              mountPath: /run/systemd
              readOnly: true
      volumes:
        - name: run-systemd
          hostPath:
            path: /run/systemd
            type: Directory
```

### Example 2: GKE with H100 GPUs

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: aicr
  namespace: gpu-operator
  labels:
    app.kubernetes.io/name: aicr
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    spec:
      serviceAccountName: aicr
      restartPolicy: Never
      hostPID: true
      hostNetwork: true
      hostIPC: true
      nodeSelector:
        cloud.google.com/gke-accelerator: nvidia-tesla-h100
      securityContext:
        runAsUser: 0
        runAsGroup: 0
        fsGroup: 0
      containers:
        - name: aicr
          image: ghcr.io/nvidia/aicr-validator:latest
          command: ["/bin/sh", "-c"]
          args: ["aicr snapshot -o cm://gpu-operator/aicr-snapshot"]
          securityContext:
            privileged: true
          volumeMounts:
            - name: run-systemd
              mountPath: /run/systemd
              readOnly: true
      volumes:
        - name: run-systemd
          hostPath:
            path: /run/systemd
            type: Directory
```

### Example 3: Periodic Snapshots (CronJob)

Automatic snapshots for drift detection:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: aicr-snapshot
  namespace: gpu-operator
spec:
  schedule: "0 */6 * * *"  # Every 6 hours
  jobTemplate:
    metadata:
      labels:
        app.kubernetes.io/name: aicr
    spec:
      backoffLimit: 0
      ttlSecondsAfterFinished: 3600
      template:
        spec:
          serviceAccountName: aicr
          restartPolicy: Never
          hostPID: true
          hostNetwork: true
          hostIPC: true
          nodeSelector:
            nvidia.com/gpu.present: "true"
          securityContext:
            runAsUser: 0
            runAsGroup: 0
            fsGroup: 0
          containers:
            - name: aicr
              image: ghcr.io/nvidia/aicr-validator:latest
              command: ["/bin/sh", "-c"]
              args: ["aicr snapshot -o cm://gpu-operator/aicr-snapshot"]
              securityContext:
                privileged: true
              volumeMounts:
                - name: run-systemd
                  mountPath: /run/systemd
                  readOnly: true
          volumes:
            - name: run-systemd
              hostPath:
                path: /run/systemd
                type: Directory
```

Retrieve historical snapshots:
```shell
# List completed jobs
kubectl get jobs -n gpu-operator -l job-name=aicr-snapshot

# Get latest snapshot from ConfigMap (updated by most recent job)
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > latest-snapshot.yaml

# Check ConfigMap update timestamp
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.metadata.creationTimestamp}'

# View job logs for debugging (if needed)
kubectl logs -n gpu-operator job/aicr-snapshot-28405680
```

**Note**: The ConfigMap `aicr-snapshot` is updated by each CronJob run. For historical tracking, save snapshots to external storage (S3, Git, etc.) using a post-job step.

## Post-Deployment

### Monitor Job Status

```shell
# Check job status
kubectl get jobs -n gpu-operator

# Describe job for events
kubectl describe job aicr -n gpu-operator

# Check pod status
kubectl get pods -n gpu-operator -l job-name=aicr
```

### Retrieve Snapshot

```shell
# View snapshot from ConfigMap
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}'

# Save to file
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot-$(date +%Y%m%d).yaml

# View job logs (for debugging)
kubectl logs -n gpu-operator job/aicr

# Check ConfigMap metadata
kubectl get configmap aicr-snapshot -n gpu-operator -o yaml
```

### Generate Recipe from Snapshot

```shell
# Option 1: Use ConfigMap directly (no file needed)
aicr recipe --snapshot cm://gpu-operator/aicr-snapshot --intent training --platform kubeflow --output recipe.yaml

# Option 2: Save snapshot to file first
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot.yaml
aicr recipe --snapshot snapshot.yaml --intent training --platform kubeflow --output recipe.yaml

# Generate bundle
aicr bundle --recipe recipe.yaml --output ./bundles
```

### Clean Up

```shell
# Delete job
kubectl delete job aicr -n gpu-operator

# Delete RBAC (if no longer needed)
kubectl delete -f https://raw.githubusercontent.com/NVIDIA/aicr/main/deployments/aicr-agent/1-deps.yaml
```

## Complete Workflow Examples

### CLI-Based Workflow (Recommended)

```shell
# Step 1: Deploy agent and capture snapshot to ConfigMap
aicr snapshot --deploy-agent --output cm://gpu-operator/aicr-snapshot

# Step 2: Generate recipe from ConfigMap (with kubeconfig if needed)
aicr recipe \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --kubeconfig ~/.kube/config \
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

### Manual kubectl Workflow

### Manual kubectl Workflow

```shell
# Step 1: Deploy RBAC and Job using kubectl
kubectl apply -f deployments/aicr-agent/1-deps.yaml
kubectl apply -f deployments/aicr-agent/2-job.yaml

# Step 2: Wait for completion
kubectl wait --for=condition=complete job/aicr -n gpu-operator --timeout=5m

# Step 3: Generate recipe from ConfigMap
aicr recipe \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --intent training \
  --output recipe.yaml

# Step 4: Create bundle
aicr bundle \
  --recipe recipe.yaml \
  --output ./bundles

# Step 5: Deploy and verify
cd bundles && chmod +x deploy.sh && ./deploy.sh
kubectl get pods -n gpu-operator
```

## Integration Patterns

### 1. CI/CD Pipeline (CLI-Based)

```yaml
# GitHub Actions example with CLI
- name: Capture snapshot using agent
  run: |
    aicr snapshot --deploy-agent \
      --kubeconfig ${{ secrets.KUBECONFIG }} \
      --namespace gpu-operator \
      --output cm://gpu-operator/aicr-snapshot \
      --timeout 10m
    
- name: Generate recipe from ConfigMap
  run: |
    aicr recipe \
      --snapshot cm://gpu-operator/aicr-snapshot \
      --kubeconfig ${{ secrets.KUBECONFIG }} \
      --intent training \
      --output recipe.yaml
    
- name: Generate bundle
  run: |
    aicr bundle -r recipe.yaml -o ./bundles
    
- name: Upload artifacts
  uses: actions/upload-artifact@v3
  with:
    name: cluster-config
    path: |
      recipe.yaml
      bundles/
```

### 2. CI/CD Pipeline (kubectl-Based)

```yaml
# GitHub Actions example with kubectl
- name: Deploy agent to capture snapshot
  run: |
    kubectl apply -f deployments/aicr-agent/1-deps.yaml
    kubectl apply -f deployments/aicr-agent/2-job.yaml
    kubectl wait --for=condition=complete --timeout=300s job/aicr -n gpu-operator
    
- name: Generate recipe from ConfigMap
  run: |
    # Option 1: Use ConfigMap directly (no file needed)
    aicr recipe -s cm://gpu-operator/aicr-snapshot -i training -o recipe.yaml
    
    # Option 2: Write recipe to ConfigMap as well
    aicr recipe -s cm://gpu-operator/aicr-snapshot -i training -o cm://gpu-operator/aicr-recipe
    
    # Option 3: Export snapshot to file for archival
    kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot.yaml
    
- name: Generate bundle
  run: |
    aicr bundle -r recipe.yaml -o ./bundles
    
- name: Upload artifacts
  uses: actions/upload-artifact@v3
  with:
    name: cluster-config
    path: |
      snapshot.yaml
      recipe.yaml
      bundles/
```

### 3. Multi-Cluster Auditing (CLI-Based)

```shell
#!/bin/bash
# Capture snapshots from multiple clusters using CLI

clusters=("prod-us-east" "prod-eu-west" "staging")

for cluster in "${clusters[@]}"; do
  echo "Capturing snapshot from $cluster..."
  
  # Switch context
  kubectl config use-context $cluster
  
  # Deploy agent and capture snapshot
  aicr snapshot --deploy-agent \
    --namespace gpu-operator \
    --output snapshot-${cluster}.yaml \
    --timeout 10m
done
```

### 4. Multi-Cluster Auditing (kubectl-Based)

```shell
#!/bin/bash
# Capture snapshots from multiple clusters using kubectl

clusters=("prod-us-east" "prod-eu-west" "staging")

for cluster in "${clusters[@]}"; do
  echo "Capturing snapshot from $cluster..."
  
  # Switch context
  kubectl config use-context $cluster
  
  # Deploy agent
  kubectl apply -f deployments/aicr-agent/2-job.yaml
  
  # Wait for completion
  kubectl wait --for=condition=complete --timeout=300s job/aicr -n gpu-operator
  
  # Save snapshot from ConfigMap
  kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot-${cluster}.yaml
  
  # Clean up
  kubectl delete job aicr -n gpu-operator
done
```

### 5. Drift Detection

```shell
#!/bin/bash
# Compare current snapshot with baseline

# Baseline (first snapshot) - using CLI
aicr snapshot --deploy-agent --output baseline.yaml

# Current (later snapshot)
aicr snapshot --deploy-agent --output current.yaml

# Compare
diff baseline.yaml current.yaml || echo "Configuration drift detected!"
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

# Check for previous pod errors
kubectl logs -n gpu-operator -l job-name=aicr --previous
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

### Image Pull Errors

Check image access:
```shell
# Describe pod
kubectl describe pod -n gpu-operator -l job-name=aicr

# For private registries, create image pull secret:
kubectl create secret docker-registry regcred \
  --docker-server=ghcr.io \
  --docker-username=<your-username> \
  --docker-password=<your-pat> \
  -n gpu-operator

# Add to job spec:
# imagePullSecrets:
#   - name: regcred
```

## Security Considerations

### RBAC Permissions

The agent requires these permissions:
- **ClusterRole** (`aicr-node-reader`): Read access to nodes, pods, secrets (Helm releases), services, ClusterPolicy CRDs (nvidia.com), and Application CRDs (argoproj.io)
- **Role** (`aicr`): Create/update ConfigMaps and list pods in the deployment namespace

### Network Policies

Restrict agent network access:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: aicr-agent
  namespace: gpu-operator
spec:
  podSelector:
    matchLabels:
      job-name: aicr
  policyTypes:
    - Egress
  egress:
    - to:
        - namespaceSelector: {}
      ports:
        - protocol: TCP
          port: 443  # Kubernetes API only
```

### Pod Security Context

The agent requires elevated privileges to collect system configuration from the host:

```yaml
spec:
  template:
    spec:
      hostPID: true       # Access host process namespace
      hostNetwork: true   # Access host network namespace
      hostIPC: true       # Access host IPC namespace
      securityContext:
        runAsUser: 0
        runAsGroup: 0
        fsGroup: 0
      containers:
        - name: aicr
          securityContext:
            privileged: true
            runAsUser: 0
            runAsGroup: 0
            allowPrivilegeEscalation: true
            capabilities:
              add: ["SYS_ADMIN", "SYS_CHROOT"]
          volumeMounts:
            - name: run-systemd
              mountPath: /run/systemd
              readOnly: true
      volumes:
        - name: run-systemd
          hostPath:
            path: /run/systemd
            type: Directory
```

**Why elevated privileges are needed:**
- `hostPID`, `hostNetwork`, `hostIPC`: Required to read host system configuration
- `privileged` + `SYS_ADMIN`: Required to access GPU configuration and kernel parameters
- `/run/systemd` mount: Required to query systemd service states

## See Also

- [CLI Reference](cli-reference.md) - aicr CLI commands
- [Installation Guide](installation.md) - Install CLI locally
- [API Reference](api-reference.md) - REST API usage
- [Kubernetes Deployment](../integrator/kubernetes-deployment.md) - API server deployment
- [RBAC Manifest](../../deployments/aicr-agent/1-deps.yaml) - Full RBAC configuration
- [Job Manifest](../../deployments/aicr-agent/2-job.yaml) - Full Job configuration
