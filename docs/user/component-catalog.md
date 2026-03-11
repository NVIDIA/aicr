# Component Catalog

AICR recipes are composed of components — the individual software packages that make up a GPU-accelerated Kubernetes runtime. This page lists every component that can appear in a recipe. 

> ***Note:*** Components are included as appropriate in recipes. Not every component listed here will appear in a recipe.

The source of truth is [`recipes/registry.yaml`](../../recipes/registry.yaml). Each entry in the registry defines the component's Helm chart (or Kustomize source), default version, namespace, and node scheduling configuration. If a component is not listed there, it cannot appear in a recipe.

## Components

| Component | Description | Source |
|-----------|-------------|--------|
| **gpu-operator** | Manages the GPU driver and runtime lifecycle on Kubernetes nodes. Handles driver installation, container runtime configuration, device plugin, and GPU feature discovery. | [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) |
| **network-operator** | Manages high-performance networking for GPU workloads. Configures RDMA, SR-IOV, and host networking for multi-node communication. | [NVIDIA Network Operator](https://github.com/Mellanox/network-operator) |
| **aws-efa** | Device plugin for AWS Elastic Fabric Adapter. Enables low-latency networking on EKS clusters with EFA-capable instances. EKS-specific. | [AWS EFA K8s Device Plugin](https://github.com/aws/eks-charts) |
| **cert-manager** | Automates TLS certificate management. Required by several operators for webhook and API server certificates. | [cert-manager](https://github.com/cert-manager/cert-manager) |
| **skyhook-operator** | OS-level node tuning and configuration management. Applies kernel parameters, sysctl settings, and system-level optimizations to nodes. | [Skyhook](https://github.com/nvidia/skyhook) |
| **skyhook-customizations** | Environment-specific node tuning profiles applied via Skyhook. Extends the operator with kernel params, hugepages, and other host-level configurations. | — |
| **nvsentinel** | GPU health monitoring and automated remediation. Detects GPU errors and can cordon or drain affected nodes. | [NVSentinel](https://github.com/NVIDIA/nvsentinel) |
| **nvidia-dra-driver-gpu** | Dynamic Resource Allocation driver for GPUs. Enables structured GPU device advertisement and claim-based allocation in Kubernetes 1.33+. | [NVIDIA DRA Driver](https://github.com/NVIDIA/k8s-dra-driver-gpu) |
| **kube-prometheus-stack** | Cluster monitoring: Prometheus, Grafana, Alertmanager, and node exporters. Provides GPU and cluster metrics collection and dashboards. | [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts) |
| **prometheus-adapter** | Exposes custom metrics from Prometheus to the Kubernetes metrics API. Enables HPA scaling based on GPU utilization and other custom metrics. | [prometheus-adapter](https://github.com/kubernetes-sigs/prometheus-adapter) |
| **aws-ebs-csi-driver** | CSI driver for Amazon EBS volumes. Provides persistent storage for workloads on EKS. EKS-specific. | [AWS EBS CSI Driver](https://github.com/kubernetes-sigs/aws-ebs-csi-driver) |
| **k8s-ephemeral-storage-metrics** | Exports ephemeral storage usage metrics per pod. Useful for monitoring scratch space consumption on GPU nodes. | [k8s-ephemeral-storage-metrics](https://github.com/jmcgrath207/k8s-ephemeral-storage-metrics) |
| **kai-scheduler** | DRA-aware gang scheduler with hierarchical queues and topology-aware placement. Ensures distributed training jobs land on nodes with optimal interconnect topology. | [KAI Scheduler](https://github.com/NVIDIA/KAI-Scheduler) |
| **dynamo-crds** | Custom Resource Definitions for NVIDIA Dynamo inference serving. Installed separately to support CRD lifecycle management. | [Dynamo](https://github.com/ai-dynamo/dynamo) |
| **dynamo-platform** | NVIDIA Dynamo inference serving platform. Distributed inference with prefix-cache-aware routing and disaggregated prefill/decode. | [Dynamo](https://github.com/ai-dynamo/dynamo) |
| **kgateway-crds** | Custom Resource Definitions for kgateway (Kubernetes Gateway API implementation). | [kgateway](https://github.com/kgateway-dev/kgateway) |
| **kgateway** | Kubernetes Gateway API implementation. Provides model-aware ingress routing for inference workloads. | [kgateway](https://github.com/kgateway-dev/kgateway) |
| **kubeflow-trainer** | Kubeflow Training Operator for distributed training jobs (PyTorch, etc.). Manages multi-node training job lifecycle with JobSet integration. | [Kubeflow Trainer](https://github.com/kubeflow/trainer) |

## How Components Are Selected

Not every component appears in every recipe. The recipe engine selects components based on the overlay chain for your environment:

- **Base components** (cert-manager, kube-prometheus-stack) appear in most recipes.
- **Cloud-specific components** (aws-efa, aws-ebs-csi-driver) are added when the service matches.
- **Intent-specific components** (kubeflow-trainer, dynamo-platform, kai-scheduler) are added based on workload intent.
- **Accelerator/OS-specific tuning** (skyhook-customizations, nvidia-dra-driver-gpu) varies by hardware and OS combination.

To see exactly which components appear in a given recipe, generate one:

```bash
aicr recipe --service eks --accelerator h100 --os ubuntu --intent training -o recipe.yaml
```

The output lists every component with its pinned version and configuration values.

## Adding Components

New components are added declaratively in `recipes/registry.yaml` — no Go code required. See the [Contributing Guide](../../CONTRIBUTING.md) and [Bundler Development](../contributor/component.md) docs for details.
