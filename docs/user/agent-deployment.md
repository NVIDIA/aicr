# Agent Deployment

Deploy Eidos as a Kubernetes Job to automatically capture cluster configuration snapshots.

## Overview

The agent is a Kubernetes Job that captures system configuration and writes output to a ConfigMap.

**Deployment options:**

1. **CLI-based deployment** (recommended): Use `eidos snapshot --deploy-agent` to deploy and manage Job programmatically
2. **kubectl deployment**: Manually apply YAML manifests with `kubectl apply`

**What it does:**

- Runs `eidos snapshot --output cm://gpu-operator/eidos-snapshot` on a GPU node
- Writes snapshot to ConfigMap via Kubernetes API (no PersistentVolume required)
- Exits after snapshot capture

**What it does not do:**

- Recipe generation (use `eidos recipe` CLI or API server)
- Bundle generation (use `eidos bundle` CLI)
- Continuous monitoring (use CronJob for periodic snapshots)

**Use cases:**

- Cluster auditing and compliance
- Multi-cluster configuration management
- Drift detection (compare snapshots over time)
- CI/CD integration (automated configuration validation)

**ConfigMap storage:**

Agent uses ConfigMap URI scheme (`cm://namespace/name`) to write snapshots:
```bash
eidos snapshot --output cm://gpu-operator/eidos-snapshot
```

This creates:
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: eidos-snapshot
  namespace: gpu-operator
  labels:
    app.kubernetes.io/name: eidos
    app.kubernetes.io/component: snapshot
    app.kubernetes.io/version: v0.17.0
data:
  snapshot.yaml: |  # Complete snapshot YAML
    apiVersion: eidos.nvidia.com/v1alpha1
    kind: Snapshot
    measurements: [...]
  format: yaml
  timestamp: "2026-01-03T10:30:00Z"
```

## Prerequisites

- Kubernetes cluster with GPU nodes
- `kubectl` configured with cluster access (for manual deployment) OR eidos CLI installed (for CLI-based deployment)
- GPU Operator installed (agent runs in `gpu-operator` namespace)
- Cluster admin permissions (for RBAC setup)

## Quick Start (CLI-Based Deployment)

**Recommended approach**: Deploy agent programmatically using the CLI.

### 1. Deploy Agent with Single Command

```shell
eidos snapshot --deploy-agent
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
eidos snapshot --deploy-agent

# Save to file
eidos snapshot --deploy-agent --output snapshot.yaml

# Keep in ConfigMap for later use
eidos snapshot --deploy-agent --output cm://gpu-operator/eidos-snapshot

# Retrieve from ConfigMap later
kubectl get configmap eidos-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}'
```

### 3. Customize Deployment

Target specific nodes and configure scheduling:

```shell
# Target GPU nodes with specific label
eidos snapshot --deploy-agent \
  --node-selector accelerator=nvidia-h100

# Handle tainted nodes (by default all taints are tolerated)
# Only needed if you want to restrict which taints are tolerated
eidos snapshot --deploy-agent \
  --toleration nvidia.com/gpu=present:NoSchedule

# Full customization
eidos snapshot --deploy-agent \
  --namespace gpu-operator \
  --image ghcr.io/nvidia/eidos:v0.8.0 \
  --node-selector accelerator=nvidia-h100 \
  --toleration nvidia.com/gpu:NoSchedule \
  --timeout 10m \
  --output cm://gpu-operator/eidos-snapshot
```

**Available flags:**
- `--deploy-agent`: Enable agent deployment mode
- `--kubeconfig`: Custom kubeconfig path (default: `~/.kube/config` or `$KUBECONFIG`)
- `--namespace`: Deployment namespace (default: `gpu-operator`)
- `--image`: Container image (default: `ghcr.io/nvidia/eidos-validator:latest`)
- `--job-name`: Job name (default: `eidos`)
- `--service-account-name`: ServiceAccount name (default: `eidos`)
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
kubectl logs -n gpu-operator job/eidos

# Describe Job for events
kubectl describe job eidos -n gpu-operator
```

## Manual Deployment (kubectl)

Alternative approach using kubectl with YAML manifests.

### 1. Deploy RBAC and ServiceAccount

The agent requires permissions to read Kubernetes resources and write to ConfigMaps:

```shell
kubectl apply -f https://raw.githubusercontent.com/nvidia/eidos/main/deployments/eidos-agent/1-deps.yaml
```

**What this creates:**
- **Namespace**: `gpu-operator` (if not exists)
- **ServiceAccount**: `eidos` in `gpu-operator` namespace
- **Role**: `eidos` - Permissions to create/update ConfigMaps and list pods in `gpu-operator` namespace
- **RoleBinding**: `eidos` - Binds Role to ServiceAccount in `gpu-operator` namespace
- **ClusterRole**: `eidos-node-reader` - Permissions to read nodes, pods, services, and ClusterPolicy (nvidia.com)
- **ClusterRoleBinding**: `eidos-node-reader` - Binds ClusterRole to ServiceAccount

### 2. Deploy the Agent Job

```shell
kubectl apply -f https://raw.githubusercontent.com/nvidia/eidos/main/deployments/eidos-agent/2-job.yaml
```

**What this creates:**
- **Job**: `eidos` in the `gpu-operator` namespace
- Job runs `eidos snapshot --output cm://gpu-operator/eidos-snapshot`
- Snapshot is written directly to ConfigMap via Kubernetes API

### 3. View Snapshot Output

Check job status:
```shell
kubectl get jobs -n gpu-operator
```

Check job logs (for errors/debugging):
```shell
kubectl logs -n gpu-operator job/eidos
```

Retrieve snapshot from ConfigMap:
```shell
kubectl get configmap eidos-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}'
```

Save snapshot to file:
```shell
kubectl get configmap eidos-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot.yaml
```

## Customization

Before deploying, you may need to customize the Job manifest for your environment.

### Download and Edit Manifest

```shell
# Download job manifest
curl -O https://raw.githubusercontent.com/nvidia/eidos/main/deployments/eidos-agent/2-job.yaml

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
        - name: eidos
          image: ghcr.io/nvidia/eidos:v0.8.0  # Pin to version
```

**Finding versions:**
- [GitHub Releases](https://github.com/nvidia/eidos/releases)
- Container registry: [ghcr.io/nvidia/eidos](https://github.com/nvidia/eidos/pkgs/container/eidos)

### Resource Limits

The agent uses the following default resource allocations:

```yaml
spec:
  template:
    spec:
      containers:
        - name: eidos
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
        - name: eidos
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
  name: eidos
  namespace: gpu-operator
  labels:
    app.kubernetes.io/name: eidos
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    spec:
      serviceAccountName: eidos
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
        - name: eidos
          image: ghcr.io/nvidia/eidos-validator:latest
          command: ["/bin/sh", "-c"]
          args: ["eidos snapshot -o cm://gpu-operator/eidos-snapshot"]
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
  name: eidos
  namespace: gpu-operator
  labels:
    app.kubernetes.io/name: eidos
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    spec:
      serviceAccountName: eidos
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
        - name: eidos
          image: ghcr.io/nvidia/eidos-validator:latest
          command: ["/bin/sh", "-c"]
          args: ["eidos snapshot -o cm://gpu-operator/eidos-snapshot"]
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
  name: eidos-snapshot
  namespace: gpu-operator
spec:
  schedule: "0 */6 * * *"  # Every 6 hours
  jobTemplate:
    metadata:
      labels:
        app.kubernetes.io/name: eidos
    spec:
      backoffLimit: 0
      ttlSecondsAfterFinished: 3600
      template:
        spec:
          serviceAccountName: eidos
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
            - name: eidos
              image: ghcr.io/nvidia/eidos-validator:latest
              command: ["/bin/sh", "-c"]
              args: ["eidos snapshot -o cm://gpu-operator/eidos-snapshot"]
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
kubectl get jobs -n gpu-operator -l job-name=eidos-snapshot

# Get latest snapshot from ConfigMap (updated by most recent job)
kubectl get configmap eidos-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > latest-snapshot.yaml

# Check ConfigMap update timestamp
kubectl get configmap eidos-snapshot -n gpu-operator -o jsonpath='{.metadata.creationTimestamp}'

# View job logs for debugging (if needed)
kubectl logs -n gpu-operator job/eidos-snapshot-28405680
```

**Note**: The ConfigMap `eidos-snapshot` is updated by each CronJob run. For historical tracking, save snapshots to external storage (S3, Git, etc.) using a post-job step.

## Post-Deployment

### Monitor Job Status

```shell
# Check job status
kubectl get jobs -n gpu-operator

# Describe job for events
kubectl describe job eidos -n gpu-operator

# Check pod status
kubectl get pods -n gpu-operator -l job-name=eidos
```

### Retrieve Snapshot

```shell
# View snapshot from ConfigMap
kubectl get configmap eidos-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}'

# Save to file
kubectl get configmap eidos-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot-$(date +%Y%m%d).yaml

# View job logs (for debugging)
kubectl logs -n gpu-operator job/eidos

# Check ConfigMap metadata
kubectl get configmap eidos-snapshot -n gpu-operator -o yaml
```

### Generate Recipe from Snapshot

```shell
# Option 1: Use ConfigMap directly (no file needed)
eidos recipe --snapshot cm://gpu-operator/eidos-snapshot --intent training --platform kubeflow --output recipe.yaml

# Option 2: Save snapshot to file first
kubectl get configmap eidos-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot.yaml
eidos recipe --snapshot snapshot.yaml --intent training --platform kubeflow --output recipe.yaml

# Generate bundle
eidos bundle --recipe recipe.yaml --output ./bundles
```

### Clean Up

```shell
# Delete job
kubectl delete job eidos -n gpu-operator

# Delete RBAC (if no longer needed)
kubectl delete -f https://raw.githubusercontent.com/NVIDIA/eidos/main/deployments/eidos-agent/1-deps.yaml
```

## Complete Workflow Examples

### CLI-Based Workflow (Recommended)

```shell
# Step 1: Deploy agent and capture snapshot to ConfigMap
eidos snapshot --deploy-agent --output cm://gpu-operator/eidos-snapshot

# Step 2: Generate recipe from ConfigMap (with kubeconfig if needed)
eidos recipe \
  --snapshot cm://gpu-operator/eidos-snapshot \
  --kubeconfig ~/.kube/config \
  --intent training \
  --platform kubeflow \
  --output recipe.yaml

# Step 3: Create deployment bundle
eidos bundle \
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
kubectl apply -f deployments/eidos-agent/1-deps.yaml
kubectl apply -f deployments/eidos-agent/2-job.yaml

# Step 2: Wait for completion
kubectl wait --for=condition=complete job/eidos -n gpu-operator --timeout=5m

# Step 3: Generate recipe from ConfigMap
eidos recipe \
  --snapshot cm://gpu-operator/eidos-snapshot \
  --intent training \
  --output recipe.yaml

# Step 4: Create bundle
eidos bundle \
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
    eidos snapshot --deploy-agent \
      --kubeconfig ${{ secrets.KUBECONFIG }} \
      --namespace gpu-operator \
      --output cm://gpu-operator/eidos-snapshot \
      --timeout 10m
    
- name: Generate recipe from ConfigMap
  run: |
    eidos recipe \
      --snapshot cm://gpu-operator/eidos-snapshot \
      --kubeconfig ${{ secrets.KUBECONFIG }} \
      --intent training \
      --output recipe.yaml
    
- name: Generate bundle
  run: |
    eidos bundle -r recipe.yaml -o ./bundles
    
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
    kubectl apply -f deployments/eidos-agent/1-deps.yaml
    kubectl apply -f deployments/eidos-agent/2-job.yaml
    kubectl wait --for=condition=complete --timeout=300s job/eidos -n gpu-operator
    
- name: Generate recipe from ConfigMap
  run: |
    # Option 1: Use ConfigMap directly (no file needed)
    eidos recipe -s cm://gpu-operator/eidos-snapshot -i training -o recipe.yaml
    
    # Option 2: Write recipe to ConfigMap as well
    eidos recipe -s cm://gpu-operator/eidos-snapshot -i training -o cm://gpu-operator/eidos-recipe
    
    # Option 3: Export snapshot to file for archival
    kubectl get configmap eidos-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot.yaml
    
- name: Generate bundle
  run: |
    eidos bundle -r recipe.yaml -o ./bundles
    
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
  eidos snapshot --deploy-agent \
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
  kubectl apply -f deployments/eidos-agent/2-job.yaml
  
  # Wait for completion
  kubectl wait --for=condition=complete --timeout=300s job/eidos -n gpu-operator
  
  # Save snapshot from ConfigMap
  kubectl get configmap eidos-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot-${cluster}.yaml
  
  # Clean up
  kubectl delete job eidos -n gpu-operator
done
```

### 5. Drift Detection

```shell
#!/bin/bash
# Compare current snapshot with baseline

# Baseline (first snapshot) - using CLI
eidos snapshot --deploy-agent --output baseline.yaml

# Current (later snapshot)
eidos snapshot --deploy-agent --output current.yaml

# Compare
diff baseline.yaml current.yaml || echo "Configuration drift detected!"
```

## Troubleshooting

### Job Fails to Start

Check RBAC permissions:
```shell
kubectl auth can-i get nodes --as=system:serviceaccount:gpu-operator:eidos
kubectl auth can-i get pods --as=system:serviceaccount:gpu-operator:eidos
```

### Job Pending

Check node selectors and tolerations:
```shell
# View pod events
kubectl describe pod -n gpu-operator -l job-name=eidos

# Check node labels
kubectl get nodes --show-labels

# Check node taints
kubectl get nodes -o custom-columns=NAME:.metadata.name,TAINTS:.spec.taints
```

### Job Completes but No Output

Check ConfigMap and container logs:
```shell
# Check if ConfigMap was created
kubectl get configmap eidos-snapshot -n gpu-operator

# View ConfigMap contents
kubectl get configmap eidos-snapshot -n gpu-operator -o yaml

# View pod logs for errors
kubectl logs -n gpu-operator -l job-name=eidos

# Check for previous pod errors
kubectl logs -n gpu-operator -l job-name=eidos --previous
```

### Permission Denied

Ensure RBAC is correctly deployed:
```shell
# Verify ClusterRole
kubectl get clusterrole eidos-node-reader

# Verify ClusterRoleBinding
kubectl get clusterrolebinding eidos-node-reader

# Verify Role and RoleBinding
kubectl get role eidos -n gpu-operator
kubectl get rolebinding eidos -n gpu-operator

# Verify ServiceAccount
kubectl get serviceaccount eidos -n gpu-operator
```

### Image Pull Errors

Check image access:
```shell
# Describe pod
kubectl describe pod -n gpu-operator -l job-name=eidos

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
- **ClusterRole** (`eidos-node-reader`): Read access to nodes, pods, services, and ClusterPolicy CRDs (nvidia.com)
- **Role** (`eidos`): Create/update ConfigMaps and list pods in the deployment namespace

### Network Policies

Restrict agent network access:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: eidos-agent
  namespace: gpu-operator
spec:
  podSelector:
    matchLabels:
      job-name: eidos
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
        - name: eidos
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

- [CLI Reference](cli-reference.md) - eidos CLI commands
- [Installation Guide](installation.md) - Install CLI locally
- [API Reference](api-reference.md) - REST API usage
- [Kubernetes Deployment](../integrator/kubernetes-deployment.md) - API server deployment
- [RBAC Manifest](../../deployments/eidos-agent/1-deps.yaml) - Full RBAC configuration
- [Job Manifest](../../deployments/eidos-agent/2-job.yaml) - Full Job configuration
