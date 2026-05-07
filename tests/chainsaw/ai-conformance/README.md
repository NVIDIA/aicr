# AI Conformance Chainsaw Tests

Chainsaw suites validating AI conformance flows across environments:

- `offline/` — no-cluster recipe and bundle generation
- `cluster/` — deployed inference-stack health checks (external cluster flow)
- `kind-inference-dynamo/` — H100 Kind inference leaf-suite (GPU CI)
- `kind-training-kubeflow/` — H100 Kind training leaf-suite (GPU CI)
- `common/` — assertions shared by `cluster/` and both Kind GPU suites
- `kind-common/` — assertions shared only by Kind GPU suites

The `cluster/` suite validates the NVIDIA AI-conformance inference stack: KAI Scheduler (GPU scheduling), kgateway with Gateway API Inference Extension (inference routing), and the NVIDIA Dynamo serving platform.

## Cluster Inference Recipe

Generated with:

```bash
aicr recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent inference \
  --platform dynamo \
  --output recipe.yaml
```

Overlay chain: `base` → `monitoring-hpa` → `eks` → `eks-inference` → `h100-eks-inference` → `h100-eks-ubuntu-inference` → `h100-eks-ubuntu-inference-dynamo`

Bundle generated with:

```bash
aicr bundle \
  --recipe recipe.yaml \
  --output ./bundle \
  --system-node-selector nodeGroup=system-pool \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule
```

The Kind GPU workflows use these leaf recipes instead:

- `h100-kind-inference-dynamo`
- `h100-kind-training-kubeflow`

## Cluster Inference Components (16)

| Component | Namespace | Type | What is Validated |
|-----------|-----------|------|-------------------|
| cert-manager | cert-manager | Helm | 3 Deployments (controller, webhook, cainjector) |
| gpu-operator | gpu-operator | Helm | Operator Deployment, ClusterPolicy ready, 6 DaemonSets (driver, device-plugin, dcgm-exporter, toolkit, gfd, validator) |
| nvsentinel | nvsentinel | Helm | Controller Deployment, platform-connector DaemonSet |
| nodewright-operator | skyhook | Helm | Controller-manager Deployment |
| kube-prometheus-stack | monitoring | Helm | 3 Deployments (operator, grafana, kube-state-metrics), 2 StatefulSets (prometheus, alertmanager), node-exporter DaemonSet |
| k8s-ephemeral-storage-metrics | monitoring | Helm | Deployment |
| prometheus-adapter | monitoring | Helm | Deployment |
| aws-ebs-csi-driver | kube-system | Helm | **Disabled by default** (EKS managed addon) |
| aws-efa | kube-system | Helm | Device plugin DaemonSet |
| kgateway-crds | kgateway-system | Helm | CRDs only (Gateway API + Inference Extension) |
| kgateway | kgateway-system | Helm | Controller Deployment |
| nodewright-customizations | skyhook | Manifest | No workloads (NodeConfiguration CRs) |
| nvidia-dra-driver-gpu | nvidia-dra-driver | Helm | Controller Deployment, kubelet-plugin DaemonSet |
| kai-scheduler | kai-scheduler | Helm | Scheduler Deployment |
| grove | dynamo-system | Helm | Pod lifecycle management |
| dynamo-platform | dynamo-system | Helm | Operator Deployment (CRDs bundled) |

## Test Structure

```
tests/chainsaw/ai-conformance/
├── README.md
├── common/                              # Shared across cluster + Kind GPU suites
│   ├── assert-cert-manager.yaml         # cert-manager healthy
│   ├── assert-dra-driver.yaml           # DRA driver healthy
│   ├── assert-kai-scheduler.yaml        # KAI scheduler healthy
│   ├── assert-monitoring.yaml           # Prometheus stack healthy with Grafana
│   └── assert-skyhook.yaml              # Skyhook operator healthy
├── kind-common/                         # Shared Kind-only assertions
│   ├── assert-gpu-operator.yaml         # GPU operator healthy on kind
│   ├── assert-monitoring.yaml           # Prometheus stack healthy without Grafana
│   ├── assert-network-operator.yaml     # Network operator healthy on kind
│   └── assert-nvsentinel.yaml           # NVSentinel healthy on kind
├── kind-inference-dynamo/               # Kind + H100 + inference + dynamo leaf suite
│   ├── chainsaw-test.yaml               # Inference leaf health check orchestration
│   ├── assert-crds.yaml                 # Inference-specific CRDs installed
│   ├── assert-dynamo.yaml               # Dynamo platform healthy on kind
│   ├── assert-kgateway.yaml             # kgateway healthy on kind
│   └── assert-namespaces.yaml           # Inference-specific namespaces exist
├── kind-training-kubeflow/              # Kind + H100 + training + kubeflow leaf suite
│   ├── chainsaw-test.yaml               # Training leaf health check orchestration
│   ├── assert-crds.yaml                 # Training-specific CRDs installed
│   ├── assert-kubeflow-trainer.yaml     # Kubeflow trainer healthy on kind
│   └── assert-namespaces.yaml           # Training-specific namespaces exist
├── offline/                             # No cluster needed
│   ├── chainsaw-test.yaml               # Recipe + bundle generation
│   └── assert-recipe.yaml               # Recipe structure assertion
└── cluster/                             # Requires deployed inference stack
    ├── chainsaw-test.yaml               # Cluster health check orchestration
    ├── assert-namespaces.yaml           # 9 namespaces exist
    ├── assert-crds.yaml                 # Critical CRDs installed
    ├── assert-gpu-operator.yaml         # GPU operator + DaemonSets healthy
    ├── assert-kube-system.yaml          # AWS EFA healthy
    ├── assert-kgateway.yaml             # kgateway healthy
    ├── assert-nvsentinel.yaml           # NVSentinel healthy
    └── assert-dynamo.yaml               # Dynamo platform healthy
```

## Prerequisites

- Chainsaw installed (`make tools-setup`)
- For offline: `aicr` binary at `dist/e2e/aicr` (`go build -o dist/e2e/aicr ./cmd/aicr`)
- For cluster: `kubectl` configured, AI-conformance inference stack deployed (via bundle `deploy.sh`), at least one H100 GPU node
- For Kind GPU suites: Kind cluster with corresponding leaf stack already deployed (`h100-kind-inference-dynamo` or `h100-kind-training-kubeflow`), GPU passthrough enabled

## Running

### Offline — recipe + bundle generation

```bash
go build -o dist/e2e/aicr ./cmd/aicr
AICR_BIN=$(pwd)/dist/e2e/aicr chainsaw test \
  --no-cluster \
  --test-dir tests/chainsaw/ai-conformance/offline
```

### Cluster inference — post-deployment health check

```bash
chainsaw test \
  --test-dir tests/chainsaw/ai-conformance/cluster
```

To override the default kubeconfig:

```bash
chainsaw test \
  --test-dir tests/chainsaw/ai-conformance/cluster \
  --kube-config-overrides /path/to/kubeconfig
```

### Kind inference — H100 + Dynamo leaf suite

```bash
chainsaw test \
  --test-dir tests/chainsaw/ai-conformance/kind-inference-dynamo \
  --config tests/chainsaw/chainsaw-config.yaml
```

### Kind training — H100 + Kubeflow leaf suite

```bash
chainsaw test \
  --test-dir tests/chainsaw/ai-conformance/kind-training-kubeflow \
  --config tests/chainsaw/chainsaw-config.yaml
```

## Cluster Suite Timeouts

| Component Group | Timeout | Reason |
|-----------------|---------|--------|
| Namespaces, CRDs | 2m | Should exist immediately after deployment |
| cert-manager, kgateway, skyhook, monitoring, kai-scheduler | 5m | Standard Deployment rollout |
| gpu-operator, nvidia-dra-driver-gpu | 10m | GPU driver compilation on nodes is slow |
| dynamo-platform | 5m | Operator + etcd + NATS startup |

## Assertion Patterns

| Resource | Condition |
|----------|-----------|
| Deployment | `status.conditions[type=Available].status = "True"` |
| DaemonSet | `numberReady > 0` and `desiredNumberScheduled > 0` |
| StatefulSet | `readyReplicas > 0` |
| ClusterPolicy | `status.state = ready` (GPU operator umbrella) |
| CRDs | Existence by fully-qualified name |
| Namespaces | `status.phase = Active` |

Chainsaw retries assertions until the timeout expires.

## Cluster Suite Customization

- `aws-ebs-csi-driver` is disabled by default (EKS managed addon). If enabled via `--set aws-ebs-csi-driver.enabled=true`, add an assertion step.
- GPU operator DaemonSet names come from the ClusterPolicy, not the chart. If non-default, update `assert-gpu-operator.yaml`. The `ClusterPolicy` `status.state: ready` assertion is a safety net.
