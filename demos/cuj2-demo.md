## AICR Deployment Flow

```
  ┌────────────┐      ┌────────────┐      ┌────────────┐      ┌────────────┐
  │  1. Recipe │─────▶│  2. Bundle │─────▶│  3. Deploy │─────▶│ 4. Validate│
  └────────────┘      └────────────┘      └────────────┘      └────────────┘

  ┌────────────────────────────────────────────────────────────────────────┐
  │ 1. RECIPE — A generated configuration recommendation containing        │
  │   component references, constraints, and deployment order.             │
  │                                                                        │
  │  $ aicr recipe --service eks --accelerator h100 \                      │
  │      --intent inference --os ubuntu --platform dynamo                  │
  │                                                                        │
  │  Criteria ──▶ Overlay Chain ──▶ recipe.yaml                            │
  │                                                                        │
  │  base ─▶ eks ─▶ eks-inference ─▶ h100-eks-inference ─▶                 │
  │          h100-eks-ubuntu-inference ─▶ h100-eks-ubuntu-inference-dynamo │
  │                                                                        │
  │  Output: 16 components, constraints, deployment order                  │
  └────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
  ┌────────────────────────────────────────────────────────────────────────┐
  │ 2. BUNDLE — Deployment artifacts generated from a recipe: Helm values   │
  │   files, Kubernetes manifests, installation scripts, and checksums.    │
  │                                                                        │
  │  $ aicr bundle --recipe recipe.yaml \                                  │
  │      --accelerated-node-selector nodeGroup=gpu-worker \                │
  │      --accelerated-node-toleration dedicated=worker-workload:NoSchedule│
  │      --accelerated-node-toleration dedicated=worker-workload:NoExecute │
  │      --system-node-selector nodeGroup=system-worker \                  │
  │      --system-node-toleration dedicated=system-workload:NoSchedule     │
  │      --system-node-toleration dedicated=system-workload:NoExecute      │
  │                                                                        │
  │  recipe.yaml ──▶ bundle/                                               │
  │    ├── deploy.sh                                                       │
  │    ├── cert-manager/             (TLS certificates)                    │
  │    ├── kube-prometheus-stack/    (Prometheus, Grafana, alerting)       │
  │    ├── prometheus-adapter/       (custom metrics API for HPA)          │
  │    ├── k8s-ephemeral-storage-metrics/  (storage monitoring)            │
  │    ├── gpu-operator/             (GPU driver, device-plugin, DCGM)     │
  │    ├── nvidia-dra-driver-gpu/    (Dynamic Resource Allocation)         │
  │    ├── kai-scheduler/            (gang scheduling)                     │
  │    ├── kgateway-crds/            (Gateway API + inference CRDs)        │
  │    ├── kgateway/                 (inference gateway controller)        │
  │    ├── nvsentinel/               (security/compliance)                 │
  │    ├── skyhook-operator/         (node configuration)                  │
  │    ├── skyhook-customizations/   (H100 tuning)                         │
  │    ├── aws-ebs-csi-driver/       (EBS storage)                         │
  │    ├── aws-efa/                  (Elastic Fabric Adapter)              │
  │    ├── dynamo-crds/              (Dynamo CRDs)                         │
  │    └── dynamo-platform/          (inference serving platform)          │
  └────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
  ┌────────────────────────────────────────────────────────────────────────┐
  │ 3. DEPLOY — Install to cluster                                         │
  │                                                                        │
  │  $ cd bundle && ./deploy.sh                                            │
  │                                                                        │
  │  cert-manager ──▶ kube-prometheus-stack ──▶ gpu-operator ──▶           │
  │  kai-scheduler ──▶ kgateway ──▶ nvidia-dra-driver ──▶                  │
  │  dynamo-platform ──▶ skyhook ──▶ nvsentinel ──▶ ...                    │
  │                                                                        │
  │  Result: Fully configured GPU cluster                                  │
  │    • 8x H100 GPUs advertised via DRA                                   │
  │    • Gang scheduling (KAI Scheduler)                                   │
  │    • Inference gateway (kgateway)                                      │
  │    • GPU metrics (DCGM → Prometheus → HPA)                             │
  │    • Dynamo inference platform                                         │
  └────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
  ┌────────────────────────────────────────────────────────────────────────┐
  │ 4. VALIDATE — Verify conformance                                       │
  │                                                                        │
  │  $ aicr validate --recipe recipe.yaml \                                │
  │      --phase deployment --phase conformance                            │
  │                                                                        │
  │  ┌──────────────────────────────────────────────────────────────┐      │
  │  │ CNCF AI Conformance — All 9 Requirements PASS                │      │
  │  │                                                              │      │
  │  │  ✅ DRA Support          ✅ Gang Scheduling                  │      │
  │  │  ✅ Secure GPU Access    ✅ Accelerator Metrics              │      │
  │  │  ✅ AI Service Metrics   ✅ Inference Gateway                │      │
  │  │  ✅ Robust Controller    ✅ Pod Autoscaling (HPA)            │      │
  │  │  ✅ Cluster Autoscaling                                      │      │
  │  └──────────────────────────────────────────────────────────────┘      │
  └────────────────────────────────────────────────────────────────────────┘
```


## Recipe Overlay Chains — Training vs Inference

```
┌─────────────────────────────────────┬─────────────────────────────────────┐
│      TRAINING (kubeflow)            │      INFERENCE (dynamo)             │
│      13 components, 7 overlays      │      16 components, 7 overlays      │
├─────────────────────────────────────┼─────────────────────────────────────┤
│                                     │                                     │
│  base.yaml                          │  base.yaml                          │
│  ├── cert-manager                   │  ├── cert-manager                   │
│  ├── kube-prometheus-stack          │  ├── kube-prometheus-stack          │
│  ├── k8s-ephemeral-storage-metrics  │  ├── k8s-ephemeral-storage-metrics  │
│  ├── gpu-operator                   │  ├── gpu-operator                   │
│  ├── nvidia-dra-driver-gpu          │  ├── nvidia-dra-driver-gpu          │
│  ├── kai-scheduler                  │  ├── kai-scheduler                  │
│  ├── nvsentinel                     │  ├── nvsentinel                     │
│  └── skyhook-operator               │  └── skyhook-operator               │
│      │                              │      │                              │
│  eks.yaml                           │  eks.yaml                           │
│  ├── aws-ebs-csi-driver             │  ├── aws-ebs-csi-driver             │
│  └── aws-efa                        │  └── aws-efa                        │
│      │                              │      │                              │
│  eks-training.yaml                  │  eks-inference.yaml                 │
│  (no new components)                │  ├── kgateway-crds          ◀── NEW │
│      │                              │  └── kgateway               ◀── NEW │
│      │                              │      │                              │
│  h100-eks-training.yaml             │  h100-eks-inference.yaml            │
│  ├── gpu-operator (CDI, gdrcopy)    │  └── skyhook-customizations         │
│  └── skyhook-customizations         │      │                              │
│      │                              │  h100-eks-ubuntu-inference.yaml     │
│  h100-eks-ubuntu-training.yaml      │  (Ubuntu constraints)               │
│  (Ubuntu constraints)               │      │                              │
│      │                              │  h100-eks-ubuntu-inference-dynamo   │
│  h100-eks-ubuntu-training-kubeflow  │  ├── gpu-operator (v25.3.4, CDI)    │
│  └── kubeflow-trainer       ◀── NEW │  ├── nvidia-dra-driver (gpuRes)◀─NEW│
│                                     │  ├── dynamo-crds             ◀─ NEW │
│                                     │  └── dynamo-platform         ◀─ NEW │
│                                     │                                     │
├─────────────────────────────────────┼─────────────────────────────────────┤
│  Unique: kubeflow-trainer           │  Unique: kgateway-crds, kgateway,   │
│                                     │    dynamo-crds, dynamo-platform     │
├─────────────────────────────────────┴─────────────────────────────────────┤
│  Shared (base + eks): cert-manager, kube-prometheus-stack, gpu-operator,  │
│    kai-scheduler, nvidia-dra-driver-gpu, nvsentinel, skyhook-operator,    │
│    k8s-ephemeral-storage-metrics, aws-ebs-csi-driver, aws-efa             │
└───────────────────────────────────────────────────────────────────────────┘
```

### Node Labels and Taints

| Role | Instance | Label | Taint |
|------|----------|-------|-------|
| GPU worker | p5.48xlarge | `nodeGroup=gpu-worker` | `dedicated=worker-workload:NoSchedule` + `:NoExecute` |
| System | m4.16xlarge | `nodeGroup=system-worker` | `dedicated=system-workload:NoSchedule` + `:NoExecute` |
| CPU worker | m4.16xlarge | `nodeGroup=cpu-worker` | `dedicated=worker-workload:NoSchedule` + `:NoExecute` |

- **GPU nodes**: Run GPU operator DaemonSets, DRA driver, skyhook tuning, and GPU workloads
- **System nodes**: Run control-plane components (cert-manager, monitoring, schedulers, operators)
- **CPU nodes**: Run CPU-only workloads (e.g., Dynamo frontend, inference gateway)
- EKS-managed add-ons (CoreDNS, metrics-server) tolerate `dedicated=system-workload` by default

### Recipe and Bundle Generation 
```
 aicr recipe --service eks --accelerator h100 \
      --intent inference --os ubuntu --platform dynamo \
      --output recipe.yaml
```
```
   aicr bundle --recipe recipe.yaml \
    --accelerated-node-selector nodeGroup=gpu-worker \
    --accelerated-node-toleration dedicated=worker-workload:NoSchedule \
    --accelerated-node-toleration dedicated=worker-workload:NoExecute \
    --system-node-selector nodeGroup=system-worker \
    --system-node-toleration dedicated=system-workload:NoSchedule \
    --system-node-toleration dedicated=system-workload:NoExecute \
    --output bundle
```

## Dynamo Platform — Components & Deployment

```
┌─────────────────────────────────────────────────────────────────┐
│                      dynamo-system                              │
│                                                                 │
│  ┌──────────────────────┐       ┌──────────────────────┐        │
│  │   dynamo-operator    │       │    grove-operator    │        │
│  │   (controller +      │       │    (autoscaling)     │        │
│  │    webhooks)         │       │                      │        │
│  │                      │       │                      │        │
│  │  Reconciles:         │       │  Scales:             │        │
│  │  DynamoGraphDeploy   │       │  Worker replicas     │        │
│  │  → PodCliques        │       │  based on demand     │        │
│  │  → Services          │       │                      │        │
│  └──────────────────────┘       └──────────────────────┘        │
│                                                                 │
│  Discovery: Kubernetes-native (no etcd)                         │
│  KV Store:  In-memory (DYN_STORE_KV=mem)                        │
│  Events:    ZeroMQ (DYN_EVENT_PLANE=zmq, no NATS)               │
│                                                                 │
│  CRDs (6):                                                      │
│  ├── DynamoGraphDeployment         (inference serving graph)    │
│  ├── DynamoComponentDeployment     (per-component pod mgmt)     │
│  ├── DynamoGraphDeploymentRequest  (deployment lifecycle)       │
│  ├── DynamoModel                   (model metadata)             │
│  ├── DynamoWorkerMetadata          (worker state tracking)      │
│  └── DynamoGraphDeploymentScalingAdapter  (autoscaling config)  │
│                                                                 │
│  Webhooks: 4 validating (schema + business rule enforcement)    │
└─────────────────────────────────────────────────────────────────┘
                              │
                              │ reconciles
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    dynamo-workload                              │
│                                                                 │
│  DynamoGraphDeployment: vllm-agg                                │
│  Status: successful — All resources are ready                   │
│                                                                 │
│  ┌─────────┐  HTTP  ┌───────────────┐  ZMQ   ┌──────────────┐   │
│  │  Client │───────▶│   Frontend    │───────▶│ VllmDecode   │   │
│  │ (OpenAI │ :8000  │               │        │   Worker     │   │
│  │  API)   │◀───────│ vllm-runtime  │◀───────│              │   │
│  └─────────┘        │ Qwen3-0.6B    │        │ dynamo.vllm  │   │
│                     │               │        │ Qwen3-0.6B   │   │
│                     │  CPU node     │        │ 1x H100 GPU  │   │
│                     └───────────────┘        └──────────────┘   │
│                       svc: :8000               svc: :9090       │
│                                                                 │
│  Services:                                                      │
│    Frontend          1/1 Ready   componentType: frontend        │
│    VllmDecodeWorker  1/1 Ready   componentType: worker  gpu: 1  │
│                                                                 │
│  Flow:                                                          │
│    1. Client → /v1/chat/completions → Frontend :8000            │
│    2. Frontend → ZMQ → VllmDecodeWorker                         │
│    3. VllmDecodeWorker runs Qwen3-0.6B on H100                  │
│    4. Response: Worker → ZMQ → Frontend → Client                │
└─────────────────────────────────────────────────────────────────┘
```
### ChatBot
```
kubectl apply -f vllm-agg.yaml
chat-server.sh
http://127.0.0.1:9090/chat.html
```

## CNCF AI Conformance 

[Requirements](https://github.com/cncf/k8s-ai-conformance/blob/main/docs/AIConformance-1.34.yaml)

### Components Mapping

```
┌───┬────────────────────────────┬──────────────────────────────────────────┬─────────┐
│ # │ Requirement                │ Component(s)                             │ Layer   │
├───┼────────────────────────────┼──────────────────────────────────────────┼─────────┤
│ 1 │ dra_support                │ nvidia-dra-driver-gpu                    │ base    │
│ 2 │ gang_scheduling            │ kai-scheduler                            │ base    │
│ 3 │ secure_accelerator_access  │ gpu-operator (driver, device-plugin,     │ base    │
│   │                            │   toolkit, DCGM, validator)              │         │
│ 4 │ accelerator_metrics        │ gpu-operator (DCGM exporter)             │ base    │
│ 5 │ ai_service_metrics         │ kube-prometheus-stack, prometheus-adapter│ base    │
│ 6 │ ai_inference               │ kgateway-crds, kgateway                  │ eks-inf │
│ 7 │ robust_controller          │ dynamo-crds, dynamo-platform             │ dynamo  │
│ 8 │ pod_autoscaling            │ prometheus-adapter + HPA                 │ base    │
│ 9 │ cluster_autoscaling        │ EKS Auto Scaling Group (ASG)             │ infra   │
├───┴────────────────────────────┴──────────────────────────────────────────┴─────────┤
│                                                                                     │
│  base layer (6 of 9 requirements):                                                  │
│    DRA, gang scheduling, secure access, accelerator metrics,                        │
│    AI service metrics, pod autoscaling                                              │
│                                                                                     │
│  eks-inference layer (+1):  inference gateway (kgateway)                            │
│  dynamo layer (+1):         robust controller (Dynamo operator)                     │
│  infra layer (+1):          cluster autoscaling (EKS ASG)                           │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

### CNCF AI Conformance Evidence Collection
```
 aicr validate --phase conformance --cncf-submission --evidence-dir <dir> [--feature <name>] [--timeout <duration>]

  Available evidence features:

    Feature                  Description
    ──────────────────────── ─────────────────────────────────────────────
    dra-support              DRA GPU allocation test
    gang-scheduling          Gang scheduling co-scheduling test
    secure-access            Secure accelerator access verification
    accelerator-metrics      Accelerator & AI service metrics
    inference-gateway        Inference API gateway conditions
    robust-operator          Robust AI operator + webhook test
    pod-autoscaling          HPA pod autoscaling (scale-up + scale-down)
    cluster-autoscaling      Cluster autoscaling (ASG configuration)

    Short aliases: dra, gang, secure, metrics, gateway, operator, hpa

```

```
  aicr validate --phase conformance --cncf-submission --evidence-dir /tmp --feature gang-scheduling
```

### CNCF AI Conformance Program Submission

- [Evidence Docs](https://github.com/NVIDIA/aicr/tree/main/docs/conformance/cncf)
- [Submission Docs](https://github.com/NVIDIA/aicr/tree/main/docs/conformance/cncf/submission)

## Upstream PRs

| # | Date | Repo | PR | Title | Status |
|---|------|------|----|-------|--------|
| 1 | 2026-02-18 | [NVIDIA/KAI-Scheduler](https://github.com/NVIDIA/KAI-Scheduler) | [#1035](https://github.com/NVIDIA/KAI-Scheduler/pull/1035) | fix: skip runtimeClassName injection when gpuPodRuntimeClassName is empty | Merged |
| 2 | 2026-02-11 | [Mellanox/network-operator](https://github.com/Mellanox/network-operator) | [#2167](https://github.com/Mellanox/network-operator/pull/2167) | fix: relax kubeVersion constraint to support pre-release suffixes | Merged |
| 3 | 2026-02-06 | [jmcgrath207/k8s-ephemeral-storage-metrics](https://github.com/jmcgrath207/k8s-ephemeral-storage-metrics) | [#181](https://github.com/jmcgrath207/k8s-ephemeral-storage-metrics/pull/181) | chore: add nameOverride and fullnameOverride values | Open |
| 4 | 2026-02-04 | [NVIDIA/NVSentinel](https://github.com/NVIDIA/NVSentinel) | [#789](https://github.com/NVIDIA/NVSentinel/pull/789) | Make metrics-access network policy configurable | Merged |
| 5 | 2026-02-02 | [prometheus-community/helm-charts](https://github.com/prometheus-community/helm-charts) | [#6584](https://github.com/prometheus-community/helm-charts/pull/6584) | chore(prometheus-adapter): add nameOverride and fullnameOverride values | Merged |
