# Component Catalog

AICR recipes are composed of components — the individual software packages that make up a GPU-accelerated Kubernetes runtime. This page lists every component that can appear in a recipe.

> **Note:** Components are included as appropriate in recipes. Not every component listed here will appear in a recipe.

The source of truth is [`recipes/registry.yaml`](https://github.com/NVIDIA/aicr/blob/main/recipes/registry.yaml). Each entry in the registry defines the component's Helm chart (or Kustomize source), default version, namespace, and node scheduling configuration. If a component is not listed there, it cannot appear in a recipe.

## Components

| Component | Description | Source |
|-----------|-------------|--------|
| **gpu-operator** | Manages the GPU driver and runtime lifecycle on Kubernetes nodes. Handles driver installation, container runtime configuration, device plugin, and GPU feature discovery. | [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) |
| **network-operator** | Manages high-performance networking for GPU workloads. Configures RDMA, SR-IOV, and host networking for multi-node communication. | [NVIDIA Network Operator](https://github.com/Mellanox/network-operator) |
| **nfd** | Node Feature Discovery — labels nodes with hardware features (PCI device IDs, kernel modules, CPU capabilities). Both gpu-operator and network-operator consume these labels. On production GPU recipes, the Topology Updater publishes per-node `NodeResourceTopology` CRDs describing NUMA zones and GPU/NIC affinity for downstream NUMA-aware schedulers. | [Node Feature Discovery](https://github.com/kubernetes-sigs/node-feature-discovery) |
| **gke-nccl-tcpxo** | NCCL TCPxO network plugin for GKE. Provides optimized collective communication for multi-node GPU workloads on Google Kubernetes Engine. GKE-specific. | — |
| **aws-efa** | Device plugin for AWS Elastic Fabric Adapter. Enables low-latency networking on EKS clusters with EFA-capable instances. EKS-specific. | [AWS EFA K8s Device Plugin](https://github.com/aws/eks-charts) |
| **cert-manager** | Automates TLS certificate management. Required by several operators for webhook and API server certificates. | [cert-manager](https://github.com/cert-manager/cert-manager) |
| **nodewright-operator** | OS-level node tuning and configuration management. Applies kernel parameters, sysctl settings, and system-level optimizations to nodes. | [Nodewright](https://github.com/nvidia/nodewright) |
| **nodewright-customizations** | Environment-specific node tuning profiles applied via Nodewright. Extends the operator with kernel params, hugepages, and other host-level configurations. | — |
| **nvsentinel** | GPU health monitoring and automated remediation. Detects GPU errors and can cordon or drain affected nodes. | [NVSentinel](https://github.com/NVIDIA/nvsentinel) |
| **nvidia-dra-driver-gpu** | Dynamic Resource Allocation (DRA) driver for GPUs. Advertises GPUs via the Kubernetes `resource.k8s.io/v1` API instead of the legacy device plugin. Requires Kubernetes 1.34+ (DRA is GA in 1.34). See [AKS GPU Setup](../integrator/aks-gpu-setup.md#dynamic-resource-allocation-dra) for details. CLI alias: `dradriver`. | [NVIDIA DRA Driver](https://github.com/NVIDIA/k8s-dra-driver-gpu) |
| **prometheus-operator-crds** | Custom Resource Definitions for the prometheus-operator (`Alertmanager`, `AlertmanagerConfig`, `PodMonitor`, `Probe`, `Prometheus`, `PrometheusRule`, `ServiceMonitor`, `ThanosRuler`). Shipped as a separate release so the CRDs land before any chart that creates monitoring CRs; this breaks the helm-diff self-reference that otherwise blocks `helmfile apply` on a fresh cluster. | [prometheus-operator-crds](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus-operator-crds) |
| **kube-prometheus-stack** | Cluster monitoring: Prometheus, Grafana, Alertmanager, and node exporters. Provides GPU and cluster metrics collection and dashboards. CRDs are installed by the sibling `prometheus-operator-crds` release (this chart runs with `crds.enabled: false`). | [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts) |
| **prometheus-adapter** | Exposes custom metrics from Prometheus to the Kubernetes metrics API. Enables HPA scaling based on GPU utilization and other custom metrics. | [prometheus-adapter](https://github.com/kubernetes-sigs/prometheus-adapter) |
| **aws-ebs-csi-driver** | CSI driver for Amazon EBS volumes. Provides persistent storage for workloads on EKS. EKS-specific. **Cluster-wide default StorageClass:** AICR enables `defaultStorageClass.enabled`, so this component provisions a **cluster-default** gp3 StorageClass (`ebs-csi-default-sc`) on **every** EKS cluster that includes it — not just inference recipes; training overlays inherit it too. EKS ships no default SC of its own, so this makes dynamic provisioning (e.g. the inference-perf model cache) work zero-config. Two consequences to note: (1) if the cluster already has a default SC, Kubernetes treats multiple defaults as ambiguous — unset the other; (2) a PVC that previously failed-fast on "no default SC" will now silently bind gp3, which can mask a misconfiguration. | [AWS EBS CSI Driver](https://github.com/kubernetes-sigs/aws-ebs-csi-driver) |
| **k8s-ephemeral-storage-metrics** | Exports ephemeral storage usage metrics per pod. Useful for monitoring scratch space consumption on GPU nodes. | [k8s-ephemeral-storage-metrics](https://github.com/jmcgrath207/k8s-ephemeral-storage-metrics) |
| **kai-scheduler** | DRA-aware gang scheduler with hierarchical queues and topology-aware placement. Ensures distributed training jobs land on nodes with optimal interconnect topology. | [KAI Scheduler](https://github.com/kai-scheduler/KAI-Scheduler) |
| **grove** | Pod lifecycle management for Dynamo inference platform. Installed as a standalone component. | [Grove](https://github.com/ai-dynamo/grove) |
| **dynamo-platform** | NVIDIA Dynamo inference serving platform with bundled CRDs. Distributed inference with prefix-cache-aware routing and disaggregated prefill/decode. | [Dynamo](https://github.com/ai-dynamo/dynamo) |
| **agentgateway-crds** | Custom Resource Definitions for agentgateway (Kubernetes Gateway API implementation for AI/ML inference). | [agentgateway](https://github.com/agentgateway/agentgateway) |
| **agentgateway** | Kubernetes Gateway API implementation for AI/ML inference. Implements the Gateway API Inference Extension for model-aware ingress routing to InferencePool backends. | [agentgateway](https://github.com/agentgateway/agentgateway) |
| **k8s-nim-operator** | NVIDIA NIM Operator for managing NIM (NVIDIA Inference Microservices) deployments on Kubernetes. | [K8s NIM Operator](https://github.com/NVIDIA/k8s-nim-operator) |
| **kueue** | Kubernetes-native job queuing system. Manages quotas and admits jobs for batch and AI workloads. | [Kueue](https://github.com/kubernetes-sigs/kueue) |
| **kubeflow-trainer** | Kubeflow Training Operator for distributed training jobs (PyTorch, etc.). Manages multi-node training job lifecycle with JobSet integration. | [Kubeflow Trainer](https://github.com/kubeflow/trainer) |
| **slinky-slurm-operator-crds** | Custom Resource Definitions for the SchedMD Slinky Slurm operator. Installs the `slinky.slurm.net` CRDs (Controller, NodeSet, LoginSet, Accounting, RestApi, Token). Installed separately to support CRD lifecycle management. | [Slinky Slurm Operator](https://github.com/SlinkyProject/slurm-operator) |
| **slinky-slurm-operator** | SchedMD Slinky Slurm operator and admission webhook. Manages the lifecycle of Slurm clusters declared via Slinky CRs (Controller, NodeSet, LoginSet, Accounting, RestApi, Token). **Known limitation:** chart v1.1.0 silently ignores `operator.nodeSelector` and `webhook.nodeSelector` (current chart behavior, not a planned feature); tracking [SlinkyProject/slurm-operator#187](https://github.com/SlinkyProject/slurm-operator/pull/187) for the upstream fix. | [Slinky Slurm Operator](https://github.com/SlinkyProject/slurm-operator) |
| **slinky-slurm** | Slinky-managed Slurm cluster instance: Controller (slurmctld) + LoginSet (sackd/sshd) + NodeSet (slurmd) + RestApi (slurmrestd). Reconciled by `slinky-slurm-operator`. Declared inline per slurm leaf overlay alongside `slinky-slurm-operator-crds` and `slinky-slurm-operator` (matching the dynamo-platform pattern) so each leaf can carry its own GPU/GRES tuning. Accounting (slurmdbd) requires an external MariaDB and is disabled in defaults — see `recipes/components/slinky-slurm/values.yaml`. | [Slinky Slurm Cluster Chart](https://github.com/SlinkyProject/slurm-operator/tree/main/helm/slurm) |

## How Components Are Selected

Not every component appears in every recipe. The recipe engine selects components based on the overlay chain for your environment:

- **Base components** (cert-manager, kube-prometheus-stack) appear in most recipes.
- **Cloud-specific components** (aws-efa, aws-ebs-csi-driver) are added when the service matches.
- **Intent-specific components** (agentgateway, agentgateway-crds) are added based on workload intent (e.g., inference recipes include the inference gateway).
- **Platform-specific components** (slinky-slurm-operator, slinky-slurm, kubeflow-trainer, dynamo-platform) are added when the recipe selects a matching `--platform`. For `--platform slurm`, all three Slinky pieces (`slinky-slurm-operator-crds`, `slinky-slurm-operator`, `slinky-slurm`) are declared inline per slurm leaf overlay — the same shape `dynamo-platform` uses across `*-inference-dynamo` leaves. Leaves that want the operator only inline the CRDs + operator and omit the `slinky-slurm` componentRef.
- **Accelerator/OS-specific tuning** (nodewright-customizations, nvidia-dra-driver-gpu) varies by hardware and OS combination.

### NFD Topology Updater

Production GPU leaf recipes (H100, GB200, RTX Pro 6000 on EKS / AKS / GKE / OKE / LKE) enable the NFD Topology Updater. It publishes per-node `NodeResourceTopology` CRDs that describe NUMA zones, GPU-to-NUMA affinity, and NIC-to-NUMA affinity. Runtime consumers (NUMA-aware schedulers, debugging via `kubectl get noderesourcetopologies`) can read these CRDs without further configuration.

The Topology Updater requires the kubelet `podResources` gRPC socket. The `KubeletPodResources` feature gate has been on by default since Kubernetes 1.15 (Beta) and reached GA in Kubernetes 1.28; AICR's recipe constraints on the affected leaves require K8s ≥ 1.30 or higher, so this is satisfied in practice. Recipes targeting Kubernetes < 1.15 must enable the feature gate explicitly. Kind / KWOK simulated clusters do not run a real kubelet and therefore leave the Topology Updater disabled — kind-based recipes will not see `NodeResourceTopology` CRDs.

See the upstream [Topology Updater docs](https://kubernetes-sigs.github.io/node-feature-discovery/stable/usage/nfd-topology-updater.html) for runtime consumer examples.

To see exactly which components appear in a given recipe, generate one:

```bash
aicr recipe --service eks --accelerator h100 --os ubuntu --intent training -o recipe.yaml
```

The output lists every component with its pinned version and configuration values.

## Inference Gateway Network Exposure

Inference recipes include the **agentgateway** component, which deploys an `inference-gateway` Gateway. The agentgateway controller materializes that Gateway into a `Service` of type `LoadBalancer`, so on every cloud the platform provisions an internet-facing load balancer for the (plaintext HTTP, unauthenticated) inference endpoint. By default that load balancer accepts traffic from any source (`0.0.0.0/0`).

To restrict it to trusted networks, set `agentgateway.allowedSourceRanges` to a list of CIDR (Classless Inter-Domain Routing) blocks. The values are rendered into the generated Service's `spec.loadBalancerSourceRanges`, which the AWS, GCP, Azure, and OCI cloud load balancers all honor — so one setting locks the gateway down on every platform.

Do **not** use `--set` for this key. `--set agentgateway:allowedSourceRanges=<cidr>` exits 0 but renders `loadBalancerSourceRanges` as a bare string instead of a list, producing a type-invalid Service (the gateway may stay open to `0.0.0.0/0`, or the CR apply is rejected). Scope the gateway through a recipe overlay or `componentRef` override instead:

```yaml
componentRefs:
  - name: agentgateway
    type: Helm
    overrides:
      allowedSourceRanges:
        - 216.228.127.128/30   # e.g. corporate egress
```

The default is intentionally empty rather than a fixed CIDR: a baked-in range would firewall every downstream deployment to one network and lock other operators out of their own gateway. Each operator should scope this to their own trusted networks. An empty list leaves the load balancer open to `0.0.0.0/0`.

This setting filters by source IP only; it does not add TLS or authentication to the gateway listener.

## Adding Components

New components are added declaratively in `recipes/registry.yaml` — no Go code required. See the [Contributing Guide](https://github.com/NVIDIA/aicr/blob/main/CONTRIBUTING.md) and [Components](../contributor/component.md) docs for details.

## Upgrade Notes

Migration steps when upgrading from a prior AICR-generated bundle to a newer one that changes how a component delivers its Kubernetes resources.

A generated recipe is a point-in-time artifact of the AICR binary that produced it: the embedded registry, overlays, manifest paths, and chart pins are part of that binary's surface. When upgrading AICR, regenerate the recipe from scratch with the new binary (`aicr recipe ...`) before re-bundling. `aicr bundle --recipe <old-file>` against a newer binary may fail if the saved recipe references manifest paths the new release has moved or removed (see [Bundle Generation Fails](cli-reference.md#bundle-generation-fails) for the specific error).

### `gpu-operator`: `dcgm-exporter` ConfigMap moved into the main release

Earlier bundles shipped the `dcgm-exporter` ConfigMap as a post-manifest in a separate Helm release named `gpu-operator-post`. The in-cluster ConfigMap therefore carries ownership annotations pointing at that release:

```yaml
meta.helm.sh/release-name: gpu-operator-post
meta.helm.sh/release-namespace: gpu-operator
```

Newer bundles render the ConfigMap directly from the main `gpu-operator` chart's `dcgmExporter.config.data` values. On upgrade, Helm 3 refuses to claim the existing ConfigMap because its annotations point at a different release:

```text
Error: ConfigMap "dcgm-exporter" in namespace "gpu-operator" exists and cannot be
imported into the current release: invalid ownership metadata; annotation
validation error: key "meta.helm.sh/release-name" must equal "gpu-operator":
current value is "gpu-operator-post"
```

Fresh installs are not affected. To migrate an existing cluster, remove the stale `gpu-operator-post` release before applying the new bundle.

**Raw Helm (per-component bundle / `deploy.sh`):**

```bash
helm uninstall gpu-operator-post --namespace gpu-operator
```

`helm uninstall` removes the ConfigMap it owns; the next `gpu-operator` upgrade re-creates it from values.

**Helmfile** — the new bundle no longer references `gpu-operator-post`, so `helmfile apply` will not prune it on its own. Run the `helm uninstall` above first, then `helmfile apply`.

**Argo CD** — delete the stale Application (it will not self-prune unless an `ApplicationSet` was managing it), then sync the updated `gpu-operator` application:

```bash
argocd app delete gpu-operator-post --cascade
```

**Flux** — delete the stale `HelmRelease` so Flux uninstalls the release and removes the ConfigMap, then reconcile the updated `gpu-operator` HelmRelease. The example below assumes the Flux control plane runs in `flux-system`; substitute the namespace where your Flux installation lives:

```bash
kubectl delete helmrelease gpu-operator-post --namespace flux-system
```

After migration, confirm the ConfigMap is owned by the `gpu-operator` release:

```bash
kubectl get configmap dcgm-exporter -n gpu-operator \
  -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}'
# Expected: gpu-operator
```
