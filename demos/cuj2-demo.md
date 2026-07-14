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
  │  Output: 18 components, constraints, deployment order                  │
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
  │      --storage-class <storage-class>                                   │
  │                                                                        │
  │  recipe.yaml ──▶ bundle/                                               │
  │    ├── deploy.sh        (root automation script)                       │
  │    ├── README.md        (root deployment guide)                        │
  │    ├── checksums.txt    (SHA256 of all generated payload files)        │
  │    ├── recipe.yaml      (resolved recipe; covered by checksums)        │
  │    ├── 001-agentgateway-crds/              (agentgateway.dev CRDs)     │
  │    ├── 002-agentgateway-crds-post/         (Gateway API + Inf-Ext CRDs)│
  │    ├── 003-aws-ebs-csi-driver/             (EBS storage)               │
  │    ├── 004-aws-efa/                        (Elastic Fabric Adapter)    │
  │    ├── 005-cert-manager/                   (TLS certificates)          │
  │    ├── 006-agentgateway/                   (inference gateway)         │
  │    ├── 007-agentgateway-post/              (post-chart manifests)      │
  │    ├── 008-grove/                          (multinode inference)       │
  │    ├── 009-nfd/                            (node feature discovery)    │
  │    ├── 010-nodewright-operator/            (node configuration)        │
  │    ├── 011-nodewright-customizations/      (H100 tuning)               │
  │    ├── 012-prometheus-operator-crds/       (monitoring CRDs)           │
  │    ├── 013-kube-prometheus-stack/          (Prometheus, Grafana)       │
  │    ├── 014-gpu-operator/                   (driver, plugin, DCGM)      │
  │    ├── 015-k8s-ephemeral-storage-metrics/  (storage metrics)           │
  │    ├── 016-kai-scheduler/                  (gang scheduling)           │
  │    ├── 017-dynamo-platform/                (inference serving)         │
  │    ├── 018-nvidia-dra-driver-gpu/          (DRA driver)                │
  │    ├── 019-nvsentinel/                     (GPU health/remediation)    │
  │    └── 020-prometheus-adapter/             (custom metrics API (HPA))  │
  └────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
  ┌────────────────────────────────────────────────────────────────────────┐
  │ 3. DEPLOY — Install to cluster                                         │
  │                                                                        │
  │  $ cd bundle && ./deploy.sh                                            │
  │                                                                        │
  │  selected components in deployment order (post-folders &               │
  │  some steps omitted): agentgateway-crds ──▶ ... ──▶ cert-manager       │
  │  ──▶ agentgateway ──▶ ... ──▶ gpu-operator ──▶ dynamo-platform ──▶ ... │
  │                                                                        │
  │  Result: Fully configured GPU cluster                                  │
  │    • 8x H100 GPUs via device plugin (nvidia.com/gpu)                   │
  │    • Gang scheduling (KAI Scheduler)                                   │
  │    • Inference gateway (agentgateway)                                  │
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
│  15 components, 8 overlays +mixins  │  18 components, 8 overlays +mixins  │
├─────────────────────────────────────┼─────────────────────────────────────┤
│                                     │                                     │
│  base.yaml                          │  base.yaml                          │
│  ├── nfd                            │  ├── nfd                            │
│  ├── cert-manager                   │  ├── cert-manager                   │
│  ├── gpu-operator                   │  ├── gpu-operator                   │
│  ├── nvsentinel                     │  ├── nvsentinel                     │
│  ├── nodewright-operator            │  ├── nodewright-operator            │
│  ├── prometheus-operator-crds       │  ├── prometheus-operator-crds       │
│  ├── kube-prometheus-stack          │  ├── kube-prometheus-stack          │
│  ├── k8s-ephemeral-storage-metrics  │  ├── k8s-ephemeral-storage-metrics  │
│  ├── nvidia-dra-driver-gpu          │  ├── nvidia-dra-driver-gpu          │
│  └── kai-scheduler                  │  └── kai-scheduler                  │
│  monitoring-hpa (metadata)          │  monitoring-hpa (metadata)          │
│  └── prometheus-adapter             │  └── prometheus-adapter             │
│  h100-any (validation floor)        │  h100-any (validation floor)        │
│  eks.yaml                           │  eks.yaml                           │
│  ├── aws-ebs-csi-driver             │  ├── aws-ebs-csi-driver             │
│  └── aws-efa                        │  └── aws-efa                        │
│  eks-training.yaml                  │  eks-inference.yaml                 │
│  (gpu-operator overrides)           │  (inference constraints)            │
│  h100-eks-training.yaml             │  h100-eks-inference.yaml            │
│  └── nodewright-customizations      │  └── nodewright-customizations      │
│  h100-eks-ubuntu-training.yaml      │  h100-eks-ubuntu-inference.yaml     │
│  (Ubuntu constraints)               │  (Ubuntu constraints)               │
│  h100-eks-ubuntu-training-kubeflow  │  h100-eks-ubuntu-inference-dynamo   │
│  mixins (merged separately):        │  ├── grove                          │
│  + os-ubuntu, platform-kubeflow     │  └── dynamo-platform                │
│  └── kubeflow-trainer (via mixin)   │  mixins (merged separately):        │
│                                     │  + os-ubuntu, platform-inference    │
│                                     │  ├── agentgateway-crds (via mixin)  │
│                                     │  └── agentgateway (via mixin)       │
├─────────────────────────────────────┴─────────────────────────────────────┤
│  Unique training: kubeflow-trainer                                        │
│  Unique inference: agentgateway-crds, agentgateway, grove, dynamo-platform│
├───────────────────────────────────────────────────────────────────────────┤
│  Shared (base/eks/h100/hpa layers):                                       │
│    cert-manager, kube-prometheus-stack, gpu-operator, kai-scheduler,      │
│    nvidia-dra-driver-gpu, nvsentinel, nfd, nodewright-operator,           │
│    nodewright-customizations, prometheus-adapter,                         │
│    prometheus-operator-crds, k8s-ephemeral-storage-metrics,               │
│    aws-ebs-csi-driver, aws-efa                                            │
└───────────────────────────────────────────────────────────────────────────┘
```

### Node Labels and Taints

| Role | Instance | Label | Taint |
|------|----------|-------|-------|
| GPU worker | p5.48xlarge | `nodeGroup=gpu-worker` | `dedicated=worker-workload:NoSchedule` + `:NoExecute` |
| System | m4.16xlarge | `nodeGroup=system-worker` | `dedicated=system-workload:NoSchedule` + `:NoExecute` |
| CPU worker | m4.16xlarge | `nodeGroup=cpu-worker` | `dedicated=worker-workload:NoSchedule` + `:NoExecute` |

- **GPU nodes**: Run GPU operator DaemonSets, DRA driver, nodewright tuning, and GPU workloads
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
    --storage-class <storage-class> \
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
│  Requests:  Dynamo request plane (default TCP)                  │
│  Events:    NATS event plane for worker KV-cache events         │
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
│  ┌─────────┐  HTTP  ┌───────────────┐  TCP   ┌──────────────┐   │
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
│    Frontend          1/1 Ready   type: frontend                 │
│    VllmDecodeWorker  1/1 Ready   type: worker  gpu: 1           │
│                                                                 │
│  Flow:                                                          │
│    1. Client → /v1/chat/completions → Frontend :8000            │
│    2. Frontend → Dynamo request plane (TCP) → VllmDecodeWorker  │
│    3. VllmDecodeWorker runs Qwen3-0.6B on H100                  │
│    4. Worker relays local vLLM ZMQ KV events to NATS            │
│    5. KV router consumes NATS events; response returns over TCP │
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
│ 6 │ ai_inference               │ agentgateway-crds, agentgateway          │ eks-inf │
│ 7 │ robust_controller          │ dynamo-platform                          │ dynamo  │
│ 8 │ pod_autoscaling            │ prometheus-adapter + HPA                 │ base    │
│ 9 │ cluster_autoscaling        │ EKS Auto Scaling Group (ASG)             │ infra   │
├───┴────────────────────────────┴──────────────────────────────────────────┴─────────┤
│                                                                                     │
│  base layer (6 of 9 requirements):                                                  │
│    DRA, gang scheduling, secure access, accelerator metrics,                        │
│    AI service metrics, pod autoscaling                                              │
│                                                                                     │
│  eks-inference layer (+1):  inference gateway (agentgateway)                        │
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
    dra-support              DRA support test (mode-aware: full-GPU claim under DRA
                             policy, slice-evidence N/A path under device-plugin)
    gang-scheduling          Gang scheduling co-scheduling test
    secure-access            Secure accelerator access (device-plugin or DRA
                             ResourceClaim isolation, policy/mode-selected)
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

## Upstream PRs

| # | Date | Repo | PR | Title | Status |
|---|------|------|----|-------|--------|
| 1 | 2026-02-18 | [NVIDIA/KAI-Scheduler](https://github.com/NVIDIA/KAI-Scheduler) | [#1035](https://github.com/NVIDIA/KAI-Scheduler/pull/1035) | fix: skip runtimeClassName injection when gpuPodRuntimeClassName is empty | Merged |
| 2 | 2026-02-11 | [Mellanox/network-operator](https://github.com/Mellanox/network-operator) | [#2167](https://github.com/Mellanox/network-operator/pull/2167) | fix: relax kubeVersion constraint to support pre-release suffixes | Merged |
| 3 | 2026-02-06 | [jmcgrath207/k8s-ephemeral-storage-metrics](https://github.com/jmcgrath207/k8s-ephemeral-storage-metrics) | [#181](https://github.com/jmcgrath207/k8s-ephemeral-storage-metrics/pull/181) | chore: add nameOverride and fullnameOverride values | Open |
| 4 | 2026-02-04 | [NVIDIA/NVSentinel](https://github.com/NVIDIA/NVSentinel) | [#789](https://github.com/NVIDIA/NVSentinel/pull/789) | Make metrics-access network policy configurable | Merged |
| 5 | 2026-02-02 | [prometheus-community/helm-charts](https://github.com/prometheus-community/helm-charts) | [#6584](https://github.com/prometheus-community/helm-charts/pull/6584) | chore(prometheus-adapter): add nameOverride and fullnameOverride values | Merged |
