# Component Catalog

AICR recipes are composed of components — the individual software packages that make up a GPU-accelerated Kubernetes runtime. This page lists every component that can appear in a recipe.

> **Note:** Components are included as appropriate in recipes. Not every component listed here will appear in a recipe.

The source of truth is [`recipes/registry.yaml`](https://github.com/NVIDIA/aicr/blob/main/recipes/registry.yaml). Each entry in the registry defines the component's Helm chart (or Kustomize source), default version, namespace, and node scheduling configuration. If a component is not listed there, it cannot appear in a recipe.

> **See also:** [Recipe Health](recipe-health.md) reports the structural health of every recipe these components compose into — resolvability and chart-pin hygiene across the whole criteria matrix.

## Components

| Component | Description | Source |
|-----------|-------------|--------|
| **gpu-operator** | Manages the GPU driver and runtime lifecycle on Kubernetes nodes. Handles driver installation, container runtime configuration, device plugin, and GPU feature discovery. | [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) |
| **network-operator** | Manages high-performance networking for GPU workloads. Configures RDMA, SR-IOV, and host networking for multi-node communication. | [NVIDIA Network Operator](https://github.com/Mellanox/network-operator) |
| **nfd** | Node Feature Discovery — labels nodes with hardware features (PCI device IDs, kernel modules, CPU capabilities). Both gpu-operator and network-operator consume these labels. On production GPU recipes, the Topology Updater publishes per-node `NodeResourceTopology` CRDs describing NUMA zones and GPU/NIC affinity for downstream NUMA-aware schedulers. | [Node Feature Discovery](https://github.com/kubernetes-sigs/node-feature-discovery) |
| **node-problem-detector** | Detects node-level faults the GPU stack does not watch — XFS shutdown, fatal UEFI CPER hardware errors, a read-only root filesystem — and publishes them as Node Conditions for NVSentinel's Object Monitor to consume. Opt-in via the `npd` mixin, and only where the platform does not already run its own. | [node-problem-detector](https://github.com/kubernetes/node-problem-detector) |
| **dranet** | DRA network driver ([kubernetes-sigs/dranet](https://github.com/kubernetes-sigs/dranet)) for the ConnectX-9 RDMA fabric on VR200 (Vera Rubin NVL72) RKE2 clusters. Publishes the mlx5 NICs as `ResourceSlice`s (driver `dra.net`) and injects them into pods via NRI under `DeviceClass` `mlnx-cx9`. Manifest-only (vendored `dranet.yaml`), evaluated as a lean alternative to `network-operator`. Ships only in the VR200 RKE2 overlays. | [DraNet](https://github.com/kubernetes-sigs/dranet) |
| **rdma-netns-exclusive** | Host RDMA "exclusive" netns-mode Nodewright (Skyhook) CR. Pairs with `dranet` so a pod sees only its DRA-allocated HCA, which keeps NCCL/GPUDirect device enumeration clean. Sets the `ib_core` `netns_mode=0` module parameter through `modprobe.d` and **reboots the node** — the parameter cannot be changed live. Requires `nodewright-operator` (declared as a `componentRef` dependency). Ships only in the VR200 RKE2 overlays. | — |
| **gke-nccl-tcpxo** | NCCL TCPXO network plugin for GKE. Provides optimized collective communication for multi-node GPU workloads on Google Kubernetes Engine. GKE-specific. | — |
| **gke-gb200-rdma** | NCCL gIB (GPUDirect-RDMA over RoCE) plugin installer for GB200 (A4X, ARM64) GKE nodes. Installs RDMA binaries and the NCCL library so workloads select `NCCL_NET=gIB` over the cluster's `gvnic-1`/`rdma-0..rdma-3` `Network` objects. GKE-specific — see [GKE GB200 Networking](../integrator/gke-gb200-networking.md). | — |
| **gcp-driver-installer** | Google's cos-gpu-installer DaemonSet as an AICR-managed, values-gated component. Present in every GKE COS recipe; renders only under the `gpuStack=bundle-installer` profile value, where it installs the recipe-pinned NVIDIA driver on pools created with `gpu-driver-version=disabled`. GKE-specific. | — |
| **aws-efa** | Device plugin for AWS Elastic Fabric Adapter. Enables low-latency networking on EKS clusters with EFA-capable instances. EKS-specific. | [AWS EFA K8s Device Plugin](https://github.com/aws/eks-charts) |
| **cert-manager** | Automates TLS certificate management. Required by several operators for webhook and API server certificates. | [cert-manager](https://github.com/cert-manager/cert-manager) |
| **gatekeeper** | Admission controller for Kubernetes. Enforces policies and governance across the cluster using OPA (Open Policy Agent) ConstraintTemplates and Constraints. | [Open Policy Agent Gatekeeper](https://github.com/open-policy-agent/gatekeeper) |
| **nodewright-operator** | OS-level node tuning and configuration management. Applies kernel parameters, sysctl settings, and system-level optimizations to nodes. `v0.18.0` renamed the `Skyhook` API to `NodeWright`; crossing that boundary needs operator steps — see [Upgrade Notes](#nodewright-operator-v0180-renames-skyhook-to-nodewright) below. | [Nodewright](https://github.com/NVIDIA/nodewright) |
| **nodewright-customizations** | Environment-specific node tuning profiles applied via Nodewright. Extends the operator with kernel params, hugepages, and other host-level configurations. | — |
| **nvsentinel** | GPU health monitoring. Detects GPU errors and publishes health events; the components that cordon, drain, reboot or terminate a node are off by default — see [NVSentinel Deployment Posture](#nvsentinel-deployment-posture). On platforms where the provider installs the driver but no driver pod is observable by NVSentinel, the recipes set `labeler.assumeDriverInstalled` for you — see [NVSentinel on provider-installed-driver platforms](#nvsentinel-on-provider-installed-driver-platforms). | [NVSentinel](https://github.com/NVIDIA/nvsentinel) |
| **nvidia-dra-driver-gpu** | Dynamic Resource Allocation (DRA) driver. Advertises devices via the Kubernetes `resource.k8s.io` API (`v1` on 1.34+, `v1beta1`/`v1beta2` on 1.32/1.33) — ComputeDomain/IMEX channels for MNNVL platforms, and optionally whole GPUs. Stock recipes disable whole-GPU DRA advertisement (`resources.gpus.enabled: false`) — the device plugin is the production default whole-GPU advertiser, and DRA whole-GPU allocation is an experimental recipe-level opt-in ([#1327](https://github.com/NVIDIA/aicr/issues/1327)). Whole-GPU DRA and the GPU Operator device plugin (`nvidia.com/gpu`) are mutually exclusive per node: recipe-backed validation rejects a configuration that enables both (at policy-resolution time — skipping validation bypasses the check), because the two allocators keep independent ledgers and concurrent advertisement can double-allocate the same physical GPUs (see the guidance in `recipes/components/nvidia-dra-driver-gpu/values.yaml`). See [AKS GPU Setup](../integrator/aks-gpu-setup.md#dynamic-resource-allocation-dra) for details. CLI alias: `dradriver`. | [NVIDIA DRA Driver](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu) |
| **dra-node-labeler** | Applies the DRA eviction node label (`--dra-eviction-node-label`) to every node GFD reports as `nvidia.com/gpu.present=true`, so the DRA kubelet plugin's node selector and GPU Operator's Driver Manager eviction hook need no node-pool labeling. Write-once: an existing value, including the Driver Manager's `paused-for-driver-upgrade`, is never rewritten. Present in the bundle only when the eviction label is configured. | — |
| **prometheus-operator-crds** | Custom Resource Definitions for the prometheus-operator (`Alertmanager`, `AlertmanagerConfig`, `PodMonitor`, `Probe`, `Prometheus`, `PrometheusRule`, `ServiceMonitor`, `ThanosRuler`). Shipped as a separate release so the CRDs land before any chart that creates monitoring CRs; this breaks the helm-diff self-reference that otherwise blocks `helmfile apply` on a fresh cluster. | [prometheus-operator-crds](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus-operator-crds) |
| **kube-prometheus-stack** | Cluster monitoring: Prometheus, Grafana, Alertmanager, and node exporters. Provides GPU and cluster metrics collection and dashboards. CRDs are installed by the sibling `prometheus-operator-crds` release (this chart runs with `crds.enabled: false`). | [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts) |
| **prometheus-adapter** | Exposes custom metrics from Prometheus to the Kubernetes metrics API. Enables HPA scaling based on GPU utilization and other custom metrics. | [prometheus-adapter](https://github.com/kubernetes-sigs/prometheus-adapter) |
| **aws-ebs-csi-driver** | CSI driver for Amazon EBS volumes. Provides persistent storage for workloads on EKS. EKS-specific. **Cluster-wide default StorageClass:** AICR enables `defaultStorageClass.enabled`, so this component provisions a **cluster-default** gp3 StorageClass (`ebs-csi-default-sc`) on **every** EKS cluster that includes it — not just inference recipes; training overlays inherit it too. EKS ships no default SC of its own, so this makes dynamic provisioning (e.g. the inference-perf model cache) work zero-config. Two consequences to note: (1) if the cluster already has a default SC, Kubernetes treats multiple defaults as ambiguous — unset the other; (2) a PVC that previously failed-fast on "no default SC" will now silently bind gp3, which can mask a misconfiguration. | [AWS EBS CSI Driver](https://github.com/kubernetes-sigs/aws-ebs-csi-driver) |
| **k8s-ephemeral-storage-metrics** | Exports ephemeral storage usage metrics per pod. Useful for monitoring scratch space consumption on GPU nodes. | [k8s-ephemeral-storage-metrics](https://github.com/jmcgrath207/k8s-ephemeral-storage-metrics) |
| **k8s-aibom** | Optional runtime AI workload inventory. Produces namespace-scoped CycloneDX 1.6 ML-BOM resources for explicitly opted-in namespaces. Installed by one stock recipe, `h100-gke-cos-inference`; every other stock recipe leaves it out. Decline it with `aicr recipe --runtime-inventory disabled`. CLI aliases: `k8saibom`, `aibom`. See [k8s-aibom Runtime Inventory](#k8s-aibom-runtime-inventory). | [k8s-aibom](https://github.com/GoogleCloudPlatform/k8s-aibom) |
| **kai-scheduler** | Gang scheduler with hierarchical queues and topology-aware placement; works with device-plugin (`nvidia.com/gpu`) and DRA GPU allocation alike. Ensures distributed training jobs land on nodes with optimal interconnect topology. AICR pins `defaultQueue.createDefaultQueue: true`, so the chart creates the `default-parent-queue`/`default-queue` hierarchy on install. The `gang-scheduling` conformance check submits its synthetic test PodGroup to `default-queue` by name, so that queue is a hard dependency of validation, not an optional extra. Note the chart creates the queues only on first install and annotates them `helm.sh/resource-policy: keep` — a `helm upgrade` will not recreate them if they are deleted, so restore them manually (or reinstall the release) if that happens. Workloads are not restricted to this queue: Dynamo submits to its own `dynamo`/`dynamo-default` hierarchy, which its chart creates via post-install and post-upgrade hooks. | [KAI Scheduler](https://github.com/kai-scheduler/KAI-Scheduler) |
| **grove** | Pod lifecycle management for Dynamo inference platform. Installed as a standalone component. Upgrading from `v0.1.0-alpha.8` (or earlier) requires a CRD migration step — see [Upgrade Notes](#grove-v010-alpha8-or-earlier-to-v010-alpha12) below. | [Grove](https://github.com/ai-dynamo/grove) |
| **dynamo-platform** | NVIDIA Dynamo inference serving platform with bundled CRDs. Distributed inference with KV-cache-aware routing, Dynamo request-plane traffic, a ZMQ-based KV-cache event plane, and disaggregated prefill/decode. | [Dynamo](https://github.com/ai-dynamo/dynamo) |
| **agentgateway-crds** | Custom Resource Definitions for agentgateway (Kubernetes Gateway API implementation for AI/ML inference). | [agentgateway](https://github.com/agentgateway/agentgateway) |
| **agentgateway** | Kubernetes Gateway API implementation for AI/ML inference. Implements the Gateway API Inference Extension for model-aware ingress routing to InferencePool backends. | [agentgateway](https://github.com/agentgateway/agentgateway) |
| **k8s-nim-operator** | NVIDIA NIM Operator for managing NIM (NVIDIA Inference Microservices) deployments on Kubernetes. AICR installs the operator only — it creates no `NIMService` and no credentials; see [NIM workload credentials](#nim-workload-credentials). | [K8s NIM Operator](https://github.com/NVIDIA/k8s-nim-operator) |
| **kueue** | Kubernetes-native job queuing system. Manages quotas and admits jobs for batch and AI workloads. Ships default quota CRs (ResourceFlavor `default-flavor`, ClusterQueue `cluster-queue`, LocalQueue `default` in the `default` namespace) so admission works out of the box — tune the ClusterQueue's nominal quotas to cluster capacity to enact real limits. Managed frameworks are pinned to batch/job, JobSet, and TrainJob. Upgrade note: the quota CRs are helm post-install/post-upgrade hooks with a delete-and-recreate policy — quiesce queues before upgrading the bundle (Kueue's resource-in-use finalizer on an active ClusterQueue/ResourceFlavor blocks the delete and can wedge the upgrade), and re-apply tuned quotas afterwards since upgrades reset them to the shipped defaults. Uninstalling leaves the hook-created CRs behind; delete them manually when removing Kueue. Overlays that override the component's `manifestFiles` (replacing the default quota CRs) must also override its health check — the shipped check asserts the default CR names above. Upgrading from a 0.18.x bundle needs two checks first: see [Upgrade Notes](#kueue-018x-to-019x) below. | [Kueue](https://github.com/kubernetes-sigs/kueue) |
| **kubeflow-trainer** | Kubeflow Training Operator for distributed training jobs (PyTorch, etc.). Manages multi-node training job lifecycle with JobSet integration. | [Kubeflow Trainer](https://github.com/kubeflow/trainer) |
| **nvcre** | NVIDIA Cluster Readiness Engine — GPU cluster burn-in certification controller. Runs training and NCCL workloads, measures goodput and bandwidth. **Not installed by default** — enabling it takes both a `valuesFile` and a Trainer source; see [Enabling NVCRE](#enabling-nvcre) for a fragment that resolves. With that values file referenced, `metrics.serviceMonitor.enabled` is **false** so install does not require prometheus-operator CRDs; turn it on with `--set cre:metrics.serviceMonitor.enabled=true` only after those CRDs exist, and add `prometheus-operator-crds` to `dependencyRefs`. The chart has no manager `nodeSelector`; for hard placement, set `manager.affinity` in `recipes/components/nvcre/values.yaml` or a complete JSON object, for example `--set-json cre:manager.affinity='{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"nvidia.com/gpu.present","operator":"Exists"}]}]}}}'` (scalar `--set cre:manager.affinity=...` renders an invalid string). CLI aliases: `cre`, `cluster-readiness-engine`. Shipped EKS H100 training still uses the TrainJob NCCL check. Opt-in AICR validators drive CRE with `Certification` (create, wait, delete), not `WorkloadRun`. | [Cluster Readiness Engine](https://github.com/NVIDIA/cluster-readiness-engine) |
| **mariadb-operator-crds** | Official MariaDB Operator CRDs. Declared in every Slurm recipe but installed only for `accounting.mode: aicr-provided`. | [MariaDB Operator](https://github.com/mariadb-operator/mariadb-operator) |
| **mariadb-operator** | Official MariaDB Operator controller, webhook, and certificate controller. AICR installs it only for `accounting.mode: aicr-provided`. | [MariaDB Operator](https://github.com/mariadb-operator/mariadb-operator) |
| **slurm-accounting-mariadb** | Installation-managed MariaDB instance whose initial database, all-privileges accounting user, and generated Secret reference are configured atomically on the MariaDB resource. Declared in every Slurm recipe and rendered only for `accounting.mode: aicr-provided`. | [MariaDB Cluster chart](https://artifacthub.io/packages/helm/mariadb-operator/mariadb-cluster) |
| **slinky-slurm-operator-crds** | Custom Resource Definitions for the SchedMD Slinky Slurm operator. Installs the `slinky.slurm.net` CRDs (Controller, NodeSet, LoginSet, Accounting, RestApi, Token). Installed separately to support CRD lifecycle management. | [Slinky Slurm Operator](https://github.com/SlinkyProject/slurm-operator) |
| **slinky-slurm-operator** | SchedMD Slinky Slurm operator and admission webhook. Manages the lifecycle of Slurm clusters declared via Slinky CRs (Controller, NodeSet, LoginSet, Accounting, RestApi, Token). AICR's system node-selector and toleration bundle flags apply to both deployments; affinity remains available through component values or typed overrides. | [Slinky Slurm Operator](https://github.com/SlinkyProject/slurm-operator) |
| **slinky-slurm** | Slinky-managed Slurm cluster instance: Controller (slurmctld) + LoginSet (sackd/sshd) + NodeSet (slurmd) + RestApi (slurmrestd), with SlurmDBD derived from the recipe's typed accounting mode. Reconciled by `slinky-slurm-operator`. See [Slurm Accounting](slinky-slurm-accounting.md), [Slurm Enroot Configuration](slinky-slurm-enroot.md), and [Slurm Shared Storage](slinky-slurm-storage.md). | [Slinky Slurm Cluster Chart](https://github.com/SlinkyProject/slurm-operator/tree/main/helm/slurm) |
| **slinky-topograph** | Slinky/Slurm-scoped instance of Topograph — queries cloud provider topology APIs (GCP, AWS, OCI …) to generate Slurm `topology.conf`, enabling topology-aware placement decisions in the Slinky-managed scheduler. **Not installed by default**; leaf overlays opt in by adding an explicit `componentRef` entry for `slinky-topograph` — the `componentRef` is what schedules the release; `dependencyRefs` alone does not install anything. That `componentRef` declares `slinky-slurm` as a `dependencyRef` to deploy **after** it: `slinky-slurm` renders and owns the `slinky-slurm-config-extra` ConfigMap (from its `configFiles`, mounted into slurmctld via the Controller CR's `configFileRefs`), and Topograph patches only that ConfigMap's `topology.conf` key on each sync, preserving the chart-owned `cgroup.conf`/`gres.conf` keys — Helm has to own the ConfigMap first. `TopologyPlugin: topology/tree` is set per-leaf via `slinky-slurm`'s `controller.extraConfMap`. Includes the `node-observer` component, which watches the topograph API pod and regenerates topology on restarts or selected node/pod changes. Requires cloud provider IAM access (e.g. GCP `roles/compute.viewer` for Workload Identity). | [Topograph](https://github.com/dsx-ai-factory/topograph) |
| **nfd-ocp-olm** | OLM installer for Node Feature Discovery on OpenShift. Creates the OperatorGroup and Subscription resources that install NFD via the Operator Lifecycle Manager. Paired with `nfd-ocp`. OCP-specific. | [Node Feature Discovery (Certified)](https://catalog.redhat.com/software/container-stacks/detail/5ec53e8c110f56bd24f5f8db) |
| **nfd-ocp** | Node Feature Discovery CR for OpenShift. Configures NFD's operand (worker, topology updater) via a NodeFeatureDiscovery custom resource. Deployed after `nfd-ocp-olm`. OCP-specific. | [Node Feature Discovery](https://github.com/kubernetes-sigs/node-feature-discovery) |
| **gpu-operator-ocp-olm** | OLM installer for the GPU Operator on OpenShift. Creates the OperatorGroup and Subscription resources that install the certified GPU Operator via the Operator Lifecycle Manager. Paired with `gpu-operator-ocp`. OCP-specific. | [NVIDIA GPU Operator (Certified)](https://catalog.redhat.com/software/container-stacks/detail/5e7b210b8a3c1e00013d636d) |
| **gpu-operator-ocp** | GPU Operator ClusterPolicy CR for OpenShift. Configures the GPU Operator's runtime behavior (driver, toolkit, DCGM, device plugin, MIG manager) via a ClusterPolicy custom resource. Deployed after `gpu-operator-ocp-olm`. OCP-specific. | [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) |
| **network-operator-ocp-olm** | OLM installer for the Network Operator on OpenShift. Creates the OperatorGroup and Subscription resources that install the certified Network Operator via the Operator Lifecycle Manager. Paired with `network-operator-ocp`. OCP-specific. | [NVIDIA Network Operator (Certified)](https://catalog.redhat.com/software/container-stacks/detail/60bfbc14e1207e67e9e29585) |
| **network-operator-ocp** | Network Operator NicClusterPolicy CR for OpenShift. Configures RDMA, MOFED driver, shared device plugin, and NV-IPAM via a NicClusterPolicy custom resource. Deployed after `network-operator-ocp-olm`. OCP-specific. | [NVIDIA Network Operator](https://github.com/Mellanox/network-operator) |
| **cert-manager-ocp-olm** | OLM installer for cert-manager on OpenShift. Creates the OperatorGroup and Subscription resources that install the certified cert-manager Operator via the Operator Lifecycle Manager. Paired with `cert-manager-ocp`. OCP-specific. | [cert-manager (Certified)](https://catalog.redhat.com/software/container-stacks/detail/5ec3f5a5eebc3d6acb0ee71c) |
| **cert-manager-ocp** | cert-manager CertManager CR for OpenShift. The operand Deployments (controller, cainjector, webhook) land in a hardcoded `cert-manager` namespace regardless of the operator's own namespace. Deployed after `cert-manager-ocp-olm`. OCP-specific. | [cert-manager](https://github.com/cert-manager/cert-manager) |
| **prometheus-adapter-ocp** | Prometheus Adapter for OpenShift. Reuses the same upstream chart as `prometheus-adapter`, pointed at OCP's built-in Thanos Querier instead of kube-prometheus-stack (which stays disabled on OCP). No certified OCP operator exists for this component. OCP-specific. | [prometheus-adapter](https://github.com/kubernetes-sigs/prometheus-adapter) |
| **nvidia-dra-driver-gpu-ocp** | NVIDIA DRA GPU driver for OpenShift. Reuses the same upstream chart as `nvidia-dra-driver-gpu`, with an added SCC RoleBinding granting the kubelet-plugin DaemonSet the host device access OCP's default restricted-v2 SCC forbids. No certified OCP operator exists for this component. OCP-specific. Known limitation: some GPU-driver rollout protections and remedy hints do not yet cover the OCP aliases (`gpu-operator-ocp`, `nvidia-dra-driver-gpu-ocp`) — the deployer's stale-NVML migration wait/restart, driver-version annotation injection, and the driver-absent remedy's `gpuoperator:`/`dradriver:` override keys; tracked in [#2136](https://github.com/NVIDIA/aicr/issues/2136). | [NVIDIA DRA Driver](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu) |
| **k8s-nim-operator-ocp** | NVIDIA NIM Operator for OpenShift. Reuses the same upstream chart as `k8s-nim-operator`, with OCP-specific RBAC. Requires `cert-manager-ocp` for admission-webhook TLS. OCP-specific. | [K8s NIM Operator](https://github.com/NVIDIA/k8s-nim-operator) |

## VR200 Preview coverage

> **`service=rke2` and `accelerator=vr200` are Preview.** They publish an early-adopter recipe path without the full production support and lifecycle qualification required for Supported status. Published validation evidence exists for all four coordinates at [validation.aicr.run](https://validation.aicr.run/); freshness against the current recipe is captured in the **Evidence status** note below.

Four coordinates ship in v1:

| Coordinate | Evidence |
|---|---|
| `rke2 / vr200 / ubuntu / training` | [validation.aicr.run/#/rke2/vr200-ubuntu/training](https://validation.aicr.run/#/rke2/vr200-ubuntu/training) |
| `rke2 / vr200 / ubuntu / training / kubeflow` | [validation.aicr.run/#/rke2/vr200-ubuntu/training-kubeflow](https://validation.aicr.run/#/rke2/vr200-ubuntu/training-kubeflow) |
| `rke2 / vr200 / ubuntu / inference` | [validation.aicr.run/#/rke2/vr200-ubuntu/inference](https://validation.aicr.run/#/rke2/vr200-ubuntu/inference) |
| `rke2 / vr200 / ubuntu / inference / dynamo` | [validation.aicr.run/#/rke2/vr200-ubuntu/inference-dynamo](https://validation.aicr.run/#/rke2/vr200-ubuntu/inference-dynamo) |

The platform-neutral `inference` coordinate is the base the Dynamo leaf inherits from; it exists so that resolving `rke2/vr200/ubuntu/inference` **without** `--platform` resolves to the VR200-safe overlay rather than falling through to the generic `rke2-inference` base. It carries the same VR200 hardware overrides as its Dynamo child.

> **Evidence status.** The recipes for the three original coordinates have changed since evidence publication (`aicr evidence digest` reports a mismatch against each pointer's `predicate.recipe.digest`); treat that linked evidence as historical precedent for the recipe content at publication time, not as validating the current recipe. The `training / kubeflow` evidence is current — it was published from a three-phase run against the recipe as it ships today.

> **Every VR200 coordinate carries the same node-level prerequisites** — the 64k-page kernel, the Skyhook kernel-cmdline reboots, and the mandatory host `nvidia-imex` masking. Other requirements differ by intent (the inference leaves additionally cap Kubernetes at `< 1.36.0`). See [RKE2 VR200 Setup](../integrator/rke2-vr200-setup.md) before deploying any of them.

For the definitional Preview-vs-Supported distinction, see [Preview recipes](../integrator/recipe-development.md#preview-recipes). For bare-metal cluster prerequisites, Skyhook reboot behavior, and known gaps on this coordinate, see [RKE2 VR200 Setup](../integrator/rke2-vr200-setup.md).

## k0s Preview coverage

> **`service=k0s` is Preview.** It publishes an early-adopter recipe path without the full production support and lifecycle qualification required for Supported status. Published validation evidence exists at [validation.aicr.run](https://validation.aicr.run/).

One coordinate ships today:

| Coordinate | Evidence |
|---|---|
| `k0s / h200 / ubuntu / training` | [validation.aicr.run/#/k0s/h200-ubuntu/training](https://validation.aicr.run/#/k0s/h200-ubuntu/training) |

> **Node-level prerequisites differ from the VR200 coordinates.** This leaf ships no rebooting Skyhook CRs and declares no NCCL performance floor. It does expect the node image to carry the NVIDIA driver: the GPU Operator's driver install is off, and the container toolkit targets k0s's own bundled containerd through its drop-in directory.

For the definitional Preview-vs-Supported distinction, see [Preview recipes](../integrator/recipe-development.md#preview-recipes). For cluster prerequisites, the host-provided driver posture, and known gaps on this coordinate, see [k0s H200 Setup](../integrator/k0s-h200-setup.md).

## How Components Are Selected

Not every component appears in every recipe. The recipe engine selects components based on the overlay chain for your environment:

- **Base components** (cert-manager, kube-prometheus-stack) appear in most recipes.
- **Cloud-specific components** (aws-efa, aws-ebs-csi-driver) are added when the service matches. OCP recipes replace base components (gpu-operator, nfd, network-operator, cert-manager) with OLM+CR pairs where a certified operator exists (e.g., `gpu-operator-ocp-olm` + `gpu-operator-ocp`, `cert-manager-ocp-olm` + `cert-manager-ocp`). Components with no certified OCP operator (for example, prometheus-adapter, nvidia-dra-driver-gpu, k8s-nim-operator) instead reuse the same upstream Helm chart as their base component, with OCP-specific manifests (SCC RoleBindings, RBAC, CA bundle injection) layered on top. `k8s-nim-operator-ocp` is available on OCP via the `ocp-inference-nim` overlay and depends on `cert-manager-ocp` for webhook TLS.
- **Intent-specific components** (agentgateway, agentgateway-crds) are added based on workload intent (e.g., inference recipes include the inference gateway).
- **Platform-specific components** (slinky-slurm-operator, slinky-slurm, kubeflow-trainer, dynamo-platform) are added when the recipe selects a matching `--platform`. For `--platform slurm`, all three core Slinky pieces (`slinky-slurm-operator-crds`, `slinky-slurm-operator`, `slinky-slurm`) are declared inline per slurm leaf overlay — the same shape `dynamo-platform` uses across `*-inference-dynamo` leaves. IMEX-capable Slurm leaves attach a fixed ComputeDomain through `slinky-slurm.preManifestFiles` so slurmd pods can consume DRA-provisioned IMEX channels. Leaves that want the operator only inline the CRDs + operator and omit the `slinky-slurm` componentRef. For an end-to-end walkthrough (recipe → bundle → install → validate → `srun` smoke job on AKS, EKS, GKE, or Kind), see [`demos/cuj1-slinky-slurm.md`](https://github.com/NVIDIA/aicr/blob/main/demos/cuj1-slinky-slurm.md).
- **Topology-aware optional components** (`slinky-topograph`) are not installed by default. Opting in requires an explicit `componentRef` entry for `slinky-topograph` in the leaf overlay — the `componentRef` is what installs it; `dependencyRefs` alone does not. That `componentRef` declares `slinky-slurm` as a `dependencyRef`, so Topograph deploys after the Slurm cluster chart, which owns the ConfigMap Topograph patches. See the wiring example in the [Recipe Development Guide](../integrator/recipe-development.md#slinky-slurm-inline-components).
- **Accelerator/OS-specific tuning** (nodewright-customizations, nvidia-dra-driver-gpu) varies by hardware and OS combination.

### NFD Topology Updater

Production GPU leaf recipes (H100, GB200, RTX Pro 6000 on EKS / AKS / GKE / OKE / LKE) enable the NFD Topology Updater. It publishes per-node `NodeResourceTopology` CRDs that describe NUMA zones, GPU-to-NUMA affinity, and NIC-to-NUMA affinity. Runtime consumers (NUMA-aware schedulers, debugging via `kubectl get noderesourcetopologies`) can read these CRDs without further configuration.

The Topology Updater requires the kubelet `podResources` gRPC socket. The `KubeletPodResources` feature gate has been on by default since Kubernetes 1.15 (Beta) and reached GA in Kubernetes 1.28; AICR's recipe constraints require K8s ≥ 1.32, so this is satisfied in practice. Recipes targeting Kubernetes `< 1.15` must enable the feature gate explicitly. Kind / KWOK simulated clusters do not run a real kubelet and therefore leave the Topology Updater disabled — kind-based recipes will not see `NodeResourceTopology` CRDs.

See the upstream [Topology Updater docs](https://kubernetes-sigs.github.io/node-feature-discovery/stable/usage/nfd-topology-updater.html) for runtime consumer examples.

### GPU Operator Driver Auto-Detect

When a recipe is resolved from a snapshot (`aicr recipe --snapshot snap.yaml`, or the `ResolveRecipeFromSnapshot` SDK entry point), AICR reads the sampled GPU node's `driver-loaded` measurement and, when the NVIDIA kernel module is already loaded, injects `components.gpu-operator.overrides.driver.enabled=false` into the resolved recipe. **On recipes whose ADR-015 profile owns `driver.enabled` (the AKS family), the injector is subordinated**: the profile fragment owns the path, the injector skips it without mutating (logging the skip), and the fragment's value is authoritative. Subordination follows path ownership, not mere profile presence — the GKE family is also profiled (`gpuStack` owns `devicePlugin.enabled`, not `driver.enabled`), so the injection/teardown discussion in this section applies to GKE-COS exactly as to unprofiled compositions (OKE, legacy AKS artifacts). The override lands at the top of the merge chain (`base values.yaml → ValuesFile → Overrides`), so the rendered Helm values a deployer installs carry `driver.enabled: false` regardless of what the resolved overlay's values file would default to. This prevents the GPU Operator from installing a second driver on top of one the platform has already provisioned. Explicit `--set` flags at bundle generation (`aicr bundle --set gpuoperator:driver.enabled=true`) retain higher precedence and can supersede the injection **unless the path is profile-owned** — on recipes carrying `metadata.selectedProfile` (the AKS family's `gpuStack`), a `--set` diverging from the selected value on an owned path fails closed at bundle time. `--set` is a bundle-time flag, not an `aicr recipe` flag.

Injection is gated on the resolved overlay already declaring `driver.enabled=false` in its merged base+valuesFile. That marker check inspects `driver.enabled` alone; the shipped preinstalled-driver overlays additionally carry coordinated ownership settings (AKS and OKE set `toolkit.enabled=false`; GKE-COS keeps the toolkit enabled with the COS-specific `toolkit.installDir` under the host-managed driver root) plus `hostPaths.driverInstallDir`, and the bundle-time `CheckDriverOwnershipCoherence` validation enforces full ownership coherence for any recipe. That scopes auto-detect to overlays like AKS, GKE-COS, and OKE where every dependent setting is already aligned. Bare EKS overlays lack the marker; the auto-detect **skips them and logs a warning** (`gpu-operator driver auto-detect: pre-installed driver observed …`) telling the operator to use a preinstalled-profile overlay rather than land a half-configured Operator (driver off, toolkit and gdrcopy still enabled with no operator-managed driver root). The case is still tracked as separate work:

- **EKS** — GPU-optimized AMIs that ship an NVIDIA driver preinstalled on the AMI itself. Today this warns; a full preinstalled EKS overlay is tracked separately.

On preinstalled-driver overlays whose profile does not own `driver.enabled` (GKE-COS) or that carry no profile (OKE, legacy AKS artifacts), the injection is **semantically idempotent** — the rendered `driver.enabled` value is unchanged; the resolved recipe records the override explicitly (visible in `aicr recipe -o recipe.yaml`) so the reason for the value is auditable end-to-end. On profiled AKS there is no injection at all: the `gpuStack` fragment writes the value and the injector skips the owned path (see above). The AKS default is the [AKS azure-managed profile](../integrator/aks-gpu-setup.md#default-use-the-aks-azure-managed-profile) (`driver.enabled=false`, `toolkit.enabled=false`, `operator.runtimeClass=nvidia-container-runtime` — the Azure default, where the node image preinstalls driver and toolkit); a `--gpu-driver none` pool selects the operator-managed profile value at recipe time (`--profile gpuStack=operator-managed`, see the [GPU Operator-managed profile](../integrator/aks-gpu-setup.md#alternative-let-gpu-operator-manage-the-driver)).

The inverse mismatch (no NVIDIA driver loaded on the sampled GPU node while the resolved overlay declares the preinstalled-driver profile) is handled differently depending on whether the family carries an ADR-015 configuration profile:

- **On AKS (profiled), a pool reading that mismatches the SELECTED value fails closed at resolution.** Selection comes from `--profile` (or the `azure-managed` default); the `K8s.aks-gpu-pools.gpu-driver` reading then verifies it before any driver-state post-processing. Pools reading `None` fail the azure-managed default but **qualify `--profile gpuStack=operator-managed` — rerun with that selection against the same snapshot; no pool change or recapture is needed**. `Mixed` and `Managed` reject either selection naming the observed state — fix the pools and recapture; a *missing* reading (snapshot captured without `--aks-gpu-pools`) rejects with "reading unavailable" — recapture with the pool dump (see the [AKS GPU Operator-managed profile](../integrator/aks-gpu-setup.md#alternative-let-gpu-operator-manage-the-driver)).
- **On AKS (profiled), Install-mode pools whose sampled node has no driver loaded still enter the record-and-gate flow.** Pool mode is the ownership contract, not live state: `gpu-driver: Install` satisfies the azure-managed constraint even while a failed AKS driver install or a mid-reimage node samples no loaded driver. Resolution succeeds, records `metadata.gpuDriverState: absent`, and the bundle-time `CheckDriverOwnershipCoherence` gate blocks `aicr bundle` with the AKS remedy (repair the pools and recapture, or switch pools to `--gpu-driver none`, recapture, and regenerate with `--profile gpuStack=operator-managed`). The driver-ownership paths are profile-owned, so the pre-profile per-path `--set` override tuple is rejected at bundle time.
- **On families without a profile (and legacy pre-profile AKS artifacts), every inverse mismatch takes that same warn-record-gate path**: resolution logs a warning, records `metadata.gpuDriverState: absent` in the recipe, and the bundle-time `CheckDriverOwnershipCoherence` validation blocks `aicr bundle` ([#1757](https://github.com/NVIDIA/aicr/issues/1757)) unless the values are flipped to operator-managed mode or the GPU pools are reprovisioned with the platform's default driver install and re-snapshotted. Ownership overrides are bundle-time flags there, so resolution itself cannot fail hard without cutting off the supported override path — bundle generation is the first point where the final effective values are known.

One per-OS limitation is intentionally out of the check's scope: the check verifies value *coherence* (driver ownership and driver-root lockstep), not per-OS install capability. On GKE COS node images a deliberate `--set gpuoperator:driver.enabled=true` clears the gate — the values are internally coherent — but the GPU Operator cannot install a driver on COS, so the deployment fails at deploy time, where the `gpu-operator-health` deployment-phase check is the backstop. The check's GKE remedy therefore points COS clusters at the GKE-managed driver install (`gpu-driver-version`) rather than the override tuple.

The policy is **only-false**: the auto-detect never forces `driver.enabled=true`, so recipes resolved without a snapshot (or targeting a node without a loaded driver) fall back to today's static defaults. Two operational consequences:

- Criteria-only resolves (`aicr recipe --service ... --accelerator ...`) and no-cluster mode see zero behavior change — no snapshot, no override.
- A stale snapshot from an older CLI that omits the `driver-loaded` reading is treated as *unknown*, not *absent*, so it cannot flip a hardened overlay.

**Capture the snapshot BEFORE deploying the GPU Operator** (unprofiled compositions; on profiled AKS the pool reading, not `driver-loaded`, gates resolution, and a post-deploy snapshot cannot flip an owned path). The `driver-loaded` reading is installer-agnostic — it reports whether the `nvidia` kernel module is currently loaded, not who loaded it. A snapshot taken after a prior AICR deploy has run the operator's driver container will still report `driver-loaded=true`, and a re-resolve from that post-deploy snapshot would flip a working overlay toward `driver.enabled=false`, tearing the operator-managed driver DaemonSet down and leaving new or rebooted GPU nodes driverless. AICR emits a `gpu-operator driver auto-detect: driver-loaded=true AND a ClusterPolicy is already present…` warning when both signals appear together in the same snapshot, but the guard is observability, not prevention: a pre-deploy snapshot is the intended workflow.

The signal is a single-node sample: the snapshotter Job runs on one `nvidia.com/gpu.present=true` node, so its `driver-loaded` reading is representative only when every GPU pool is in the same driver state. Mixed-pool clusters (some nodes with a preinstalled driver, some without) are out of scope for the auto-detect and tracked in [#464](https://github.com/NVIDIA/aicr/issues/464); AICR emits a `topology reports non-uniform GPU labels…` warning when the snapshot's node-topology labels indicate divergent GPU nodes so the fail-direction (some non-preinstalled pools may come up driverless) is at least observable.

To see exactly which components appear in a given recipe, generate one:

```bash
aicr recipe --service eks --accelerator h100 --os ubuntu --intent training -o recipe.yaml
```

The output lists every component with its pinned version and configuration values.

## GKE Device-Plugin Ownership

**Device-plugin ownership is a configuration profile.** The GKE recipes declare an ADR-015 `gpuStack` profile with two qualified values, selected at recipe generation and recorded in `metadata.selectedProfile`:

- **`gke-default` (the default)** — GKE's managed device plugin is the `nvidia.com/gpu` advertiser (recorded as `advertiser: external`), and the recipe disables the GPU Operator's plugin (`devicePlugin.enabled: false`, profile-owned). Its constraint requires that **no** GPU node — identified by its `cloud.google.com/gke-accelerator` label — carries the opt-out label `gke-no-default-nvidia-gpu-device-plugin`. This is the default GKE cluster shape: create GPU node pools normally (with `gpu-driver-version=default` or `latest` for GKE's managed driver install) and **no further cluster setup is required**.
- **`bundle-installer`** (`aicr recipe ... --profile gpuStack=bundle-installer`) — the GPU Operator's device plugin is the sole advertiser (`devicePlugin.enabled: true`, profile-owned), and the constraint inverts: every GPU node must carry `gke-no-default-nvidia-gpu-device-plugin=true` on pools created `gpu-driver-version=disabled`. The bundle's `gcp-driver-installer` component supplies the driver — the version is pinned in the recipe and upgrades roll with the bundle; nothing is applied by hand.

For the end-to-end setup flow (snapshot → recipe → validate), the qualification and selection-vs-verification matrices, and troubleshooting, see [GKE GPU Setup](../integrator/gke-gpu-setup.md).

Why exactly one advertiser: two plugins registering `nvidia.com/gpu` on one node is not a benign overlap. Kubelet's device manager keys its endpoint and device inventory by resource name, so competing registrations and `ListAndWatch` updates replace each other. Ownership becomes nondeterministic, and one plugin's device IDs (GKE uses `nvidia0`-style names, NVIDIA uses GPU UUIDs) can reach the other plugin's `Allocate`. Expect intermittent allocation and runtime failures.

**Bundle-installer cluster setup.** The opt-out label forfeits GKE's managed driver install: the managed install (`gpu-driver-version=default`/`latest`) is finalized by an init container of the **same** kube-system DaemonSet the label disables, so a labeled pool paired with `gpu-driver-version=default` comes up **driverless** — never combine the label with the managed driver install. Pools for the `bundle-installer` value must be created with `gpu-driver-version=disabled`; the bundle's `gcp-driver-installer` component carries the installer DaemonSet, so there is nothing to apply out-of-band. (AICR's GKE-COS overlays keep `driver.enabled: false` in either mode — the GPU Operator cannot install a driver on COS node images.) A previously hand-applied standalone `nvidia-driver-installer` DaemonSet must be deleted before deploying the bundle: the bundle's DaemonSet shares its name in `kube-system` and Helm will not adopt the pre-existing object. The operational procedures live in [GKE GPU Setup](../integrator/gke-gpu-setup.md#alternative-let-the-bundle-own-the-gpu-stack).

**`aicr validate` enforces the selected value deterministically, before any phase runs.** The selected value's `NodeTopology.gpu-nodes.label` constraint ([#1755](https://github.com/NVIDIA/aicr/issues/1755)) is verified against the snapshot at recipe generation and re-evaluated by the validate readiness pre-flight. The check fails closed: labels contradicting the selected value, mixed labels, an empty GPU-node set, and readings that `--max-nodes-per-entry` actually truncated (a cap larger than the node count truncates nothing and validates normally) all fail with exit 2 and remediation text pointing back at this section, before any check Jobs deploy. See [Validation](validation.md) for the readiness-gate mechanics.

The profile also locks the ownership tuple: `devicePlugin.enabled` is profile-owned, and because both values govern advertisement, the #1327 allocation-policy paths (`devicePlugin.enabled`, DRA `resources.gpus.enabled` / `gpuResourcesEnabledOverride`) are closure-locked — a bundle- or install-time override diverging at any of them is rejected rather than warned. Switching modes is a recipe-generation decision (`--profile`), never a `--set`.

The selected constraint is the only deterministic detection point. `aicr bundle` is offline by design and cannot read node labels. The operator-health deployment check passes under the conflict because it verifies only that GPU Operator controller pods are Running — it never inspects the device plugin. Allocation probes such as `check-nvidia-smi` schedule a pod requesting `nvidia.com/gpu` on each schedulable GPU node, but skip cordoned nodes and skip entirely when any schedulable GPU node is busy; when they do run, they may fail nondeterministically without identifying the missing label as the cause.

See GKE's [GPU node-pool guide](https://cloud.google.com/kubernetes-engine/docs/how-to/gpus) for the authoritative pool-creation procedures. The [NVIDIA GPU Operator GKE guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/google-gke.html) documents the same `gpu-driver-version=disabled` + installer-DaemonSet combination the `bundle-installer` value builds on ([#1716](https://github.com/NVIDIA/aicr/issues/1716)) — with AICR, that DaemonSet ships inside the bundle rather than being applied by hand, and the two values are distinguished at generation time by the opt-out pool label alone (positive vs negated), which resolved ADR-015 Deferred Decision 5 by construction.

## OKE Device-Plugin Ownership

**Same profile mechanism as GKE, one control-plane signal.** The OKE recipes declare an ADR-015 `gpuStack` profile with two qualified values; because both ownership axes (driver and device plugin) move together on OKE, a single signal — the `NvidiaGpuPlugin` cluster add-on's control-plane state — qualifies a selection:

- **`oci-managed` (the default)** — Oracle's GPU node image supplies the driver and toolkit, and the `NvidiaGpuPlugin` add-on is the `nvidia.com/gpu` advertiser (recorded as `advertiser: external`); the GPU Operator's driver, toolkit, and plugin are all off (profile-owned). Its constraint requires the add-on **installed and ACTIVE**.
- **`operator-managed`** (`aicr recipe ... --profile gpuStack=operator-managed`) — bring-your-own driverless image with the add-on **removed**: the GPU Operator owns driver, toolkit, and plugin, and the DRA driver root moves to the operator install path in lockstep. Its constraint requires the add-on **absent**.

The signal is supplied as an `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json` dump via `--oke-addons` on `aicr snapshot` and `aicr validate` (projected into `K8s.oke-addons.nvidia-gpu-plugin`); it is evaluated at snapshot-based generation and re-evaluated by the validate readiness pre-flight, failing closed on any other add-on lifecycle state or a missing reading. Per-node label disablement (`oci.oraclecloud.com/disable-gpu-device-plugin`) is out of contract — it leaves the add-on installed. The exactly-one-advertiser rationale is the same as [GKE's](#gke-device-plugin-ownership), and the profile closure-locks the ownership tuple the same way — switching modes is a recipe-generation decision, never a `--set`.

**Legacy device-plugin tripwire.** Older OKE clusters ship the plugin through a second, pre-add-on mechanism — a `kube-system/nvidia-gpu-device-plugin` DaemonSet reconciled by the legacy Kubernetes addon-manager — which `list-addons` cannot see. The snapshot observes that DaemonSet directly (`K8s.oke-legacy-plugin.nvidia-gpu-device-plugin`), and `operator-managed` additionally requires it `none` (absent or fully disabled), so a legacy cluster whose add-on reads `absent` still fails closed instead of double-advertising; remediation is the per-pool `oci.oraclecloud.com/disable-gpu-device-plugin=true` label or migration to the managed add-on. `oci-managed` is deliberately not gated on it — the installed add-on reconciles the same DaemonSet. For the end-to-end flow and the qualification matrices, see [OKE GPU Setup](../integrator/oke-gpu-setup.md).

## NVSentinel Deployment Posture

AICR ships NVSentinel in the upstream chart's **monitoring-only** configuration: it detects GPU and node faults and publishes health events, but takes no automatic action on a node. AICR does not disable remediation — the upstream chart ships it off, and AICR inherits that default rather than overriding it.

`recipes/components/nvsentinel/values.yaml` enables and disables no NVSentinel *component*. It carries deployment-shaping values: `fullnameOverride`, tolerate-all scheduling so GPU-node DaemonSets land on tainted nodes, `networkPolicy.enabled: false` (the metrics policy otherwise blocks cert-manager webhook traffic in the same namespace — this is the one upstream default AICR overrides here), `platformConnector` resources, and `janitor-provider.csp.provider: generic`, which selects the reboot mechanism used *if* remediation is later enabled but does not enable it. It also tunes which *checks* an already-on component runs: `syslog-health-monitor.enabledChecks` adds `SysLogsNICDriverError` to the three GPU checks the chart enables by default — see [NIC and fabric fault detection](#nic-and-fabric-fault-detection). Every component on/off default below is the chart's.

**On by default** — the detection path:

| Component | Role |
|---|---|
| `gpuHealthMonitor` | DCGM-based GPU fault detection |
| `syslogHealthMonitor` | node syslog fault detection (two DaemonSets) |
| `metadataCollector` | node and GPU inventory |
| `labeler` | applies `nvsentinel.dgxc.nvidia.com/driver.installed` |
| `platformConnector` | health-event ingest socket |

**Off by default** — the datastore and remediation path:

| Component | What enabling it does |
|---|---|
| `mongodbStore` | deploys the in-cluster datastore |
| `faultQuarantine` | cordons a node on a qualifying fault |
| `nodeDrainer` | evicts workloads from a quarantined node |
| `faultRemediation` | decides the remediation action |
| `janitor` / `janitorProvider` | executes it — reboot or terminate |

Also off: `healthEventsAnalyzer`, `lifecycleManager`, `cspHealthMonitor`, `kubernetesObjectMonitor`, `nicHealthMonitor`, `slurmDrainMonitor`, `preflight`, `eventExporter`, `inclusterFileServer`, `k8sdatastoreCrds`. Verified against chart `v1.20.0`, the version pinned in `recipes/registry.yaml`.

`nicHealthMonitor` is the one entry above that AICR's shipped recipes turn back on, and only on AKS and OKE — see [NIC and fabric fault detection](#nic-and-fabric-fault-detection).

**The practical effect.** A stock AICR bundle surfaces GPU faults; it does not act on them. A node that needs a reboot is reported, not rebooted, and an operator intervenes. That is deliberate: `janitor` can reboot or terminate nodes, and enabling it without the operator having chosen to is not a safe default.

### Audit Logging and Tracing

Both are off by default and independent of the detection/remediation path above — pure observability, no datastore, no remediation dependency.

**Opt in via the `nvsentinel-observability` mixin** (`recipes/mixins/nvsentinel-observability.yaml`) on your own leaf overlay:

```yaml
# your-leaf-overlay.yaml
spec:
  mixins:
    - nvsentinel-observability
```

**This overlay must be part of the resolved catalog** -- either an embedded overlay in `recipes/overlays/` (a real PR to this repo) or a file under an external `--data <dir>/overlays/` directory (`aicr recipe --data <dir> ...`, `aicr bundle --data <dir> ...`). An external `--data` directory must also carry a `registry.yaml` at its root even when it adds nothing but an overlay; see [the minimal stub](../integrator/data-extension.md#registryyaml-is-required). Passing your leaf overlay file directly to `aicr bundle -r <file>` or `aicr validate -r <file>` does **not** work for this (`aicr recipe` has no equivalent flag -- it only builds a recipe from criteria or an AICRConfig `--config` file, never loads an existing overlay directly): AICR auto-hydrates a directly-passed overlay by re-resolving its `spec.criteria` against the catalog (so a bare `aicr recipe` step isn't required first) -- it does not read `spec.mixins` or any other field from that file. A leaf overlay containing `mixins: [nvsentinel-observability]` that is never registered via `--data` therefore cannot contribute it, and AICR rejects the direct load with `INVALID_REQUEST` naming the dropped mixin rather than shipping a bundle without audit logging and tracing. A mixin that another applied overlay in the chain already supplies is not reported, since its content did reach the recipe.

**Has no effect if `nvsentinel` is disabled by the chain.** The OCP overlay, for example, sets `nvsentinel`'s `overrides.enabled: false`; composing this mixin on top still succeeds (a `slog.Warn` names the mixin and the disabled component, but the recipe/bundle call itself returns success either way) and produces values nothing ever reads. Confirm `nvsentinel` isn't disabled elsewhere in your chain before relying on this mixin.

`nvsentinel` is always in a recipe's inheritance chain (`recipes/overlays/base.yaml`), so this mixin composes onto an already-chained component -- something AICR's mixin-merge guard (`pkg/recipe/metadata_store.go`, ADR-005's "Silent constraint override" mitigation) otherwise hard-errors on. The mixin is allowed through a narrower gate, not a blanket relaxation: `nvsentinel`'s own registry entry (`recipes/registry.yaml`) explicitly allowlists the exact leaf paths (`mixinSafeOverridePaths`) a mixin may set on it -- `global.auditLogging.*` and `global.tracing.enabled`/`.insecure` -- and `mixinOverridesSafeForMerge` rejects, at compose time, both a path outside that allowlist and a path that collides with one your own leaf (or another mixin) already set, rather than silently letting one value overwrite the other. `global.tracing.endpoint` is deliberately excluded from the allowlist: it must always come from you, not the mixin.

The mixin sets:

```yaml
global:
  auditLogging:
    enabled: true
    logRequestBody: false   # request bodies may carry sensitive data
    maxSizeMB: 100           # explicit, not inherited -- matches chart default today
    maxBackups: 7
    maxAgeDays: 30
    compress: true
  tracing:
    enabled: true
    insecure: false          # collector endpoint is expected to use TLS
```

You must still supply the endpoint yourself -- either on your own leaf's `componentRefs` (the mixin's allowlist deliberately excludes it, but your leaf owns its own values) or at bundle time. `aicr bundle` fails closed without it:

```shell
aicr bundle -r <your-recipe>.yaml \
  --set nv-sentinel:global.tracing.endpoint=otel-collector.example:4317 \
  -o ./bundles
```

If your leaf needs a different value for something the mixin already sets (a different retention policy, for instance), you cannot set it on your leaf *and* adopt the mixin: the retention paths are allowlisted, but the mixin already owns them, so a leaf setting them too is rejected as a collision rather than silently overwritten. Set them directly on your own leaf's `componentRefs` *instead of* adopting the mixin -- the same pattern `recipes/overlays/vr200-rke2-ubuntu-training.yaml` uses for other `nvsentinel` values -- or override at bundle time with `--set`/`--set-json`.

**Audit logging** (`global.auditLogging.enabled`) writes a durable, rotated JSON record of every write NVSentinel makes to the Kubernetes API or a cloud API, to `/var/log/nvsentinel/{POD_NAME}-audit.log` on `platform-connectors` (the root chart's DaemonSet — it renders under that name regardless of `fullnameOverride`) and `labeler`. Retention is set explicitly (`maxSizeMB: 100`, `maxBackups: 7`, `maxAgeDays: 30`, `compress: true`) rather than inherited from the chart default, so a future upstream default change can't silently alter it. `logRequestBody` stays `false`: request bodies may carry sensitive data. The mount is a hostPath (`DirectoryOrCreate`) — any process with host filesystem access can read the file once it exists, independent of Kubernetes RBAC. AICR ships no log forwarder; collecting the file off the node is the operator's responsibility.

**Disk cost, precisely.** The filename embeds the pod's own name (`{POD_NAME}-audit.log`), and lumberjack's rotation only knows about the *current* process's own filename — it has no way to find or clean up files a previous pod instance left behind. So each live pod costs up to 100 MB (current file) plus 7 compressed backups before rotation catches up, and that's a **per-pod-identity** cost, not a bounded per-node cost: `platform-connectors` is a DaemonSet (one pod per node, so this recurs on every node, but a given node's pod identity is comparatively stable), while `labeler` is a Deployment whose pod gets a new name on every restart or reschedule — each restart starts a fresh rotation set, and the previous pod's files are never rotated away or deleted by NVSentinel itself. Left unmanaged, `/var/log/nvsentinel/` accumulates stale files from every past pod identity. Operators enabling this need their own retention or cleanup policy for the mount path, not just the mixin's built-in rotation numbers.

**Tracing** (`global.tracing.enabled`) emits OpenTelemetry traces to an OTLP collector at `global.tracing.endpoint`. At the pinned chart version, under AICR's current base configuration, the exporter env (`OTEL_EXPORTER_OTLP_ENDPOINT`/`_INSECURE`) renders on the `platform-connectors` DaemonSet only — so enabling tracing does not instrument the other workloads AICR deploys today (including `labeler`, which does receive audit logging). The chart instruments additional workloads (event-exporter, fault-remediation, node-drainer) when those optional subcharts are enabled, which AICR's base values do not do. `TestNVSentinelObservabilityChartRender` asserts that env so a chart bump that drops it is caught. The chart has no `required` guard on that value — `global.tracing.enabled: true` with no endpoint renders and deploys without error, and the exporter fails silently at runtime. AICR closes that gap at bundle time: `CheckNVSentinelTracingEndpointRequired` fails the bundle unless an endpoint is supplied, e.g. `--set nv-sentinel:global.tracing.endpoint=<host:port>`. `insecure` defaults to `false` (the endpoint is expected to use TLS); override with `--set nv-sentinel:global.tracing.insecure=true` for a non-TLS collector.

### Kubernetes Object Monitor

`kubernetesObjectMonitor` is off by default (see the table above). It evaluates CEL predicates against live Kubernetes objects on a resync loop and, when a predicate turns true, emits a health event that the platform connector turns into a node condition today — cordon/drain only follow once remediation exists (see "Enabling Remediation" below). It is the runtime complement to `aicr validate`'s install-time DaemonSet checks: `aicr validate` catches a broken rollout once, at install; the Object Monitor keeps watching afterward, so a driver pod that starts crashlooping weeks later still produces a signal.

**Opt in via the `nvsentinel-object-monitor` mixin** (`recipes/mixins/nvsentinel-object-monitor.yaml`) on your own leaf overlay:

```yaml
# your-leaf-overlay.yaml
spec:
  mixins:
    - nvsentinel-object-monitor
```

**This overlay must be part of the resolved catalog** -- either an embedded overlay in `recipes/overlays/` (a real PR to this repo) or a file under an external `--data <dir>/overlays/` directory (`aicr recipe --data <dir> ...`, `aicr bundle --data <dir> ...`). An external `--data` directory must also carry a `registry.yaml` at its root even when it adds nothing but an overlay; see [the minimal stub](../integrator/data-extension.md#registryyaml-is-required). Passing your leaf overlay file directly to `aicr bundle -r <file>` or `aicr validate -r <file>` does **not** work for this (`aicr recipe` has no equivalent flag): AICR auto-hydrates a directly-passed overlay by re-resolving its `spec.criteria` against the catalog, not by reading `spec.mixins` or any other field from that file. Confirm the mixin actually applied by checking the generated recipe's `nvsentinel` componentRef for `global.kubernetesObjectMonitor.enabled` before bundling.

`nvsentinel` is always in a recipe's inheritance chain (`recipes/overlays/base.yaml`), so this mixin composes onto an already-chained component -- something AICR's mixin-merge guard (`pkg/recipe/metadata_store.go`, ADR-005's "Silent constraint override" mitigation) otherwise hard-errors on. It is allowed through a narrower gate, not a blanket relaxation: `nvsentinel`'s own registry entry (`recipes/registry.yaml`) explicitly allowlists the exact leaf paths (`mixinSafeOverridePaths`) a mixin may set on it -- `global.kubernetesObjectMonitor.enabled` and `kubernetes-object-monitor.policies` -- and `mixinOverridesSafeForMerge` rejects, at compose time, both a path outside that allowlist and a path that collides with one your own leaf (or another mixin) already set.

**Has no effect if `nvsentinel` is disabled by the chain.** `recipes/overlays/ocp.yaml`, for example, sets `nvsentinel`'s `overrides.enabled: false` (no OLM variant exists yet). Composing this mixin on top still succeeds -- a `slog.Warn` names the mixin and the disabled component, but the recipe/bundle call returns success either way -- and produces values nothing ever reads. Set `overrides.enabled: true` on `nvsentinel` in your own leaf before adding this mixin, and confirm nothing later in your chain disables it again.

The mixin carries two policies adopted from NVSentinel's own `docs/monitoring-critical-operators.md`, with two deliberate deviations. First, the namespaces: upstream hardcodes `network-operator`, but this registry deploys that component into `nvidia-network-operator`, and the `os-talos` mixin relocates both operators again into `privileged-`-prefixed namespaces. Each policy therefore matches **both** namespaces its component can land in -- a policy naming only one watches a namespace nothing runs in, and never fires. Second, and following from that, `resource.namespace` is left unset (it accepts a single namespace), so the informer watches Pods cluster-wide exactly as upstream's own policies do.

Both policies fire when a **DaemonSet-owned Pod** in a watched namespace has been scheduled to a node, has been running past a **30-minute grace period**, and is unhealthy -- phase other than `Running`/`Succeeded`, or a container in `CrashLoopBackOff`:

| Policy | Namespaces watched | `errorCode` |
|---|---|---|
| `gpu-operator-pods-health` | `gpu-operator`, `privileged-gpu-operator` | `GPU_OPERATOR_POD_UNHEALTHY` |
| `network-operator-pod-health` | `nvidia-network-operator`, `privileged-network-operator` | `NETWORK_OPERATOR_POD_UNHEALTHY` |

**What these policies do not catch.** The predicate requires a Pod that is scheduled (`spec.nodeName` set) and has a `status.startTime`, because a health event has to be attached to a node. A DaemonSet Pod the scheduler never placed -- Pending because no node can satisfy its requests -- has neither, so it never fires, at any elapsed time. Nor does a Pod whose *phase* is `Running` while a container is wedged in a state other than `CrashLoopBackOff` (`ImagePullBackOff` on a restart, `CreateContainerConfigError`, a permanently failing readiness probe), or a k8s ≥1.29 native sidecar crash-looping at phase `Running`: `initContainerStatuses` is not inspected. An init container that crash-loops *before* the Pod reaches `Running` is caught, via the phase clause.

**Each policy matches only its own operator's operands.** Namespace plus "owned by a DaemonSet" would not be enough: an unrelated DaemonSet an administrator happens to run in `gpu-operator` or `nvidia-network-operator` would, once unhealthy past the grace period, raise a *fatal* event and node condition blaming the operator. Each predicate therefore also requires the label that operator stamps on the DaemonSet pods it owns. The two are not the same label, and neither is a documented API -- both were read off live deployments at the versions this repo pins:

| Operator | Required label | Coverage |
|---|---|---|
| `gpu-operator` (v26.7.0) | `app.kubernetes.io/managed-by: gpu-operator` | Confirmed on a live H100 cluster: all nine operand DaemonSets (driver, toolkit, device-plugin, DCGM, DCGM exporter, validator, GFD, MIG manager, MPS control), and the running Pods inherit it. The bundled node-feature-discovery subchart does not carry it and is out of scope. |
| `network-operator` (26.4.1) | `ds-owner: NicClusterPolicy` | Verified on Kind only. The label is applied per-operand, not uniformly, so coverage depends on which `NicClusterPolicy` a recipe ships -- see below. |

**Network Operator coverage is partial, and it varies by recipe.** The `ds-owner` label is stamped per operand rather than by a shared helper, so which components a policy watches depends on what that recipe's `NicClusterPolicy` enables:

| `recipes/components/network-operator/manifests/` | Operands enabled | Watched |
|---|---|---|
| `nic-cluster-policy-generic-gb300.yaml` | ofedDriver, rdmaSharedDevicePlugin | both |
| `nic-cluster-policy-oke-gb200.yaml` | rdmaSharedDevicePlugin | yes |
| `nic-cluster-policy-aks.yaml` | ofedDriver, rdmaSharedDevicePlugin, docaTelemetryService | first two; `docaTelemetryService` unverified |
| `nic-cluster-policy-oke-l40s.yaml` | nvIpam, secondaryNetwork, sriovDevicePlugin | **none confirmed** -- `nv-ipam-node` demonstrably omits the label, the other two are unverified |

The RDMA driver and shared device plugin — the components whose failure actually means a node can no longer run RDMA workloads — are covered everywhere they are deployed. The uncovered cases fail by staying silent rather than by raising a wrong event, which is the safe direction, but on OKE L40S the network policy should not be relied on until those operands are checked against a cluster with RDMA NICs.

Because neither label is contractual, an operator release that renames one would turn that policy into a silent no-op — and nothing inside this repo can detect that, since the labels come from the operators' own controllers rather than from any chart AICR renders. Each assumption is therefore bound to the version it was verified against: `TestObjectMonitorOperandIdentityPinnedToVerifiedVersion` fails the moment `gpu-operator` or `network-operator` is bumped in `recipes/registry.yaml`, forcing whoever bumps it to re-read the labels off the new release first. That converts a silent no-op into a required revalidation step; it is not a live check.

**The grace period debounces Pod age, not unhealthiness.** `status.startTime` is when the Pod started, so for a Pod that has been up for weeks -- the case this mixin exists for -- the 30-minute floor is already satisfied and a brief container restart fires immediately. The floor suppresses events during a rollout; it does not require a fault to persist for 30 minutes.

**Event handling is the chart's default, `processingStrategy: EXECUTE_REMEDIATION`.** The mixin does not set it, so events flow through NVSentinel's normal path (a node condition today; cordon/drain once remediation exists). The subchart also offers `STORE_ONLY`, which records events without acting on them; it is not on `nvsentinel`'s `mixinSafeOverridePaths` allowlist, so set it on your own leaf's `componentRefs` if you want it.

**RBAC worth naming:** the subchart's ClusterRole grants `nodes: get/list/watch/patch/update` unconditionally, independent of which policies you configure. That is not read-only -- it is how the node condition gets written.

**Watch load is the chart's defaults, inherited deliberately.** Because each policy must match two namespaces, `resource.namespace` cannot be set, so the monitor keeps a cluster-wide Pod informer -- the same shape as upstream's own policies. The mixin leaves `resyncPeriod` (5m) and `maxConcurrentReconciles` (1) at the subchart defaults; on the clusters AICR targets that is a single informer over Pods, not a per-policy one. Neither is on `nvsentinel`'s `mixinSafeOverridePaths`, so tuning them means setting them on your own leaf's `componentRefs` rather than through a mixin. [#2430](https://github.com/NVIDIA/aicr/issues/2430) owns requalifying those defaults against real cluster sizes.

Both policies set `isFatal: true` and leave `quarantineOverrides`/`drainOverrides` unset -- deliberately, not by omission. Today, with no quarantine component enabled anywhere in this repo's recipes, `isFatal: true` produces only a node condition; there is nothing to cordon or drain yet. Once remediation is enabled through #1014, the same policies drive an actual cordon/drain when an operator DaemonSet pod stays unhealthy past the grace period -- which is the correct behavior for a fault that means the node can no longer safely run GPU or RDMA workloads, not an accident of inheriting upstream's default.

**`node-not-ready` is deliberately dropped, not inherited.** The `kubernetes-object-monitor` subchart ships a third policy by default, `node-not-ready` (watches `Node` for `Ready=False`); setting `kubernetes-object-monitor.policies` replaces that default list wholesale (Helm values do not merge lists), so this mixin does not carry it forward. It is a general node-readiness signal unrelated to this mixin's scope (operator DaemonSet pod health) and would enable a new class of node-cordon behavior nobody asked for here. Adopt it explicitly, with its own deliberate `isFatal`/quarantine decision, via your own leaf overlay's `componentRefs` if you want it.

### Preflight Checks

Off by default. NVSentinel's preflight is a mutating admission webhook that appends init containers to GPU pods, so the node runs hardware checks *before* your workload's own containers start.

**These checks gate.** A node that fails one leaves the pod in `Init:Error` — the workload's own containers never start — and the failure is recorded on the node: a **fatal** result becomes a NodeCondition named after the check, while an **unhealthy but non-fatal** result becomes a Kubernetes Event instead. (They are alternatives, not both.) That is the point: the job fails in seconds rather than hanging minutes into training. Nothing is cordoned, drained or rebooted; AICR deploys no remediation component.

**Opt in via the `nvsentinel-preflight` mixin** (`recipes/mixins/nvsentinel-preflight.yaml`) on your own leaf overlay:

```yaml
# your-leaf-overlay.yaml
spec:
  mixins:
    - nvsentinel-preflight
```

The catalog-registration rule described under [Audit Logging and Tracing](#audit-logging-and-tracing) applies here too: the overlay must be in the resolved catalog — committed to `recipes/overlays/`, or placed at `<dir>/overlays/` and loaded with `--data <dir>`. Passing the file directly to `aicr bundle -r` or `aicr validate -r` hydrates it from `spec.criteria` alone and never reads `spec.mixins`, so the mixin cannot compose that way; AICR rejects the load with `INVALID_REQUEST`, naming the mixins that were dropped, rather than shipping a bundle without them. The "has no effect if `nvsentinel` is disabled by the chain" caveat applies equally, as does the allowlist mechanism — `nvsentinel`'s `mixinSafeOverridePaths` entry names each `preflight.*` leaf path this mixin may set, and anything outside it fails at compose time.

**Two gates, not one.** Adopting the mixin only deploys the webhook. Nothing is injected until you also label the namespaces whose pods should be checked:

```shell
kubectl label namespace <ns> nvsentinel.nvidia.com/preflight=enabled
```

Within a labeled namespace, only pods that request a GPU resource (`nvidia.com/gpu`) are mutated; everything else passes through untouched.

**Adopting this on a cluster that already runs NVSentinel needs one manual step.** The preflight subchart ships its `PreflightConfig` CRD under `crds/`, and Helm installs a `crds/` directory only on `helm install`, never on `helm upgrade`. A cluster that installed NVSentinel *before* adopting this mixin had the subchart — and therefore its CRD — pruned by the `global.preflight.enabled` condition, so enabling the mixin later cannot backfill it. The controller still admits and injects correctly, but its `preflightconfig` controller never starts and it logs `no matches for kind "PreflightConfig"` every 10 seconds. Apply the CRD once, from the chart:

```shell
kubectl apply -f <chart>/charts/preflight/crds/preflight.nvsentinel.nvidia.com_preflightconfigs.yaml
```

A fresh bundle install is unaffected, which is also why CI does not catch this — every bundle in CI is a first install.

**What the mixin sets:**

```yaml
global:
  preflight:
    enabled: true
preflight:
  processingStrategy: EXECUTE_REMEDIATION   # chart default; the checks gate
  webhook:
    failurePolicy: Ignore             # chart default is Fail
  gangCoordination:
    enabled: true
  gangDiscovery:
    name: kai
    annotationKeys: [pod-group-name]
    podGroupGVR:
      group: scheduling.run.ai
      version: v2alpha2
      resource: podgroups
    minCountExpr: "podGroup.spec.minMember"
  initContainers:                       # restated from the chart, see below
    - name: preflight-dcgm-diag         #   (chart defaults)
    - name: preflight-nccl-loopback     #   (chart defaults)
    - name: preflight-nccl-allreduce
      defaultEnabled: false             # the one deviation
```

The mixin also restates `preflight.initContainers` in full — all three checks, verbatim from the chart, with one addition: `defaultEnabled: false` on `preflight-nccl-allreduce`. The next section explains why.

Restating means AICR now pins that list's contents (both images, both bandwidth thresholds, and `DCGM_HOSTENGINE_ADDR`), so a chart bump cannot move them silently. `TestNVSentinelPreflightInitContainersMatchChart` renders the mixin's list against the chart's own and fails on any drift beyond the intended `defaultEnabled` line. That test runs weekly, not on every PR, so a chart bump can merge before it fires.

**`failurePolicy: Ignore` is deliberate.** The webhook sits in the pod-creation path, so the chart's `Fail` would turn a webhook outage into a pod-creation outage for every labeled namespace. `Ignore` trades a missed check for availability — the right default while this is new, and worth revisiting once it has field time. The cost is that a broken webhook is *silent*: pods are admitted unchecked, with no error anywhere. `recipes/checks/nvsentinel-preflight/health-check.yaml` detects exactly that, including the case where cert-manager has not injected the webhook's CA bundle — but **nothing runs it for you**. It is deliberately not registry-linked (the mixin is opt-in, so `make check-health-all` would run it against recipes that never deploy preflight), which is the same treatment `nvsentinel-observability` gets. Run it yourself after adopting the mixin:

```shell
make check-health COMPONENT=nvsentinel-preflight
```

Until you do, a webhook that never came up is indistinguishable from one that is working.

**`processingStrategy: EXECUTE_REMEDIATION` is the chart default, kept deliberately.** It is what makes the init container's exit code gate the pod. The obvious-looking alternative, `STORE_ONLY`, is a trap: each check converts its own failure to exit code 0 (upstream logs `Check failed (STORE_ONLY — not blocking pod)`), *and* `platform-connectors` filters `STORE_ONLY` events out before they become a NodeCondition or a Kubernetes Event. Since AICR deploys no datastore, that combination would ship the cost of the checks with no gate and no record — the only trace of a failure would be an init-container log that disappears with the pod.

**The name is misleading here: nothing is remediated.** The strategy controls whether the event is processable, not whether anything acts on it. All six NVSentinel remediation components (`faultQuarantine`, `nodeDrainer`, `faultRemediation`, `janitor`, `lifecycleManager`, `janitorProvider`) default off and AICR enables none, and `fault-quarantine` — the only consumer that would cordon or drain — is not deployed. The complete effect is: the pod is stranded, and the node gets either a NodeCondition (fatal) or a Kubernetes Event (non-fatal).

**What this means operationally:** a bad GPU now blocks the pods scheduled onto it. That is the intended behavior, but it is a real change in failure mode — budget for pods sitting in `Init:Error` rather than running slowly. `failurePolicy: Ignore` limits the blast radius of a *webhook* outage, not of a failing check.

**One asymmetry worth knowing:** the strategy affects *check* failures. A check that cannot load its own configuration exits non-zero regardless.

**Gang discovery points at KAI.** The chart's default (`{}`) relies on native Kubernetes gang-scheduling APIs that exist only on 1.35+/1.36+, while AICR's floor is 1.32 and managed control planes do not expose the alpha gates — so it is pointed at KAI's `PodGroup` CRs instead. `annotationKeys` is load-bearing, not decorative: upstream builds a PodGroup discoverer only when `name`, `annotationKeys` (or `labelKeys`), a full `podGroupGVR` and `minCountExpr` are all present, and anything short of that fails fast at controller startup. Because the controller validates the `PodGroup` CRD at startup and fails closed, the mixin adds `kai-scheduler` as a `dependencyRef` on `nvsentinel` so a DAG-stratified deployer applies the scheduler first. Every shipped recipe already carries `kai-scheduler`, and `TestMixinNVSentinelPreflight_ComposesOntoEveryLeaf` keeps it that way.

Gang coordination also makes the chart generate the `preflight-gang-discovery-builtin` ClusterRole from `podGroupGVR` and aggregate it into the role the controller binds — which is why the mixin ships no RBAC of its own.

**The multi-node check is configured but off by default.** `preflight-nccl-allreduce` needs gang context, and the webhook injects `POD_NAME` into it *only* when the pod already carries a gang annotation at creation time. A plain GPU pod would get the container without that variable, and the check exits on the missing variable — stranding a pod in `Init:Error` on perfectly healthy hardware. So the mixin ships it disabled and you request it per pod, with **both** annotations:

```yaml
metadata:
  annotations:
    pod-group-name: my-gang
    nvsentinel.nvidia.com/preflight-checks: "preflight-dcgm-diag,preflight-nccl-loopback,preflight-nccl-allreduce"
```

The entry stays in the list rather than being removed, because the webhook resolves an annotation-requested name against every configured check — deleting it would turn this opt-in into a pod-creation error. **This path is exercised only at admission time in CI**: the e2e asserts the container and its `POD_NAME` are injected, but nothing in AICR has ever run the check itself.

**The checks cost startup time.** Two init containers run in sequence before your workload's first container starts: a DCGM level-2 diagnostic (~2 min) and an NCCL loopback bandwidth test. Budget for this on every GPU pod in a labeled namespace, including short-lived ones — which is the main reason the namespace label exists rather than the mixin turning injection on cluster-wide.

**The check images are not counted in the BOM table.** The three `preflight-*` check images and the `preflight` controller image come from `ghcr.io/nvidia/nvsentinel/` at the chart's own version; see the opt-in image note in [container images](https://github.com/NVIDIA/aicr/blob/main/docs/user/container-images.md).

**Limitations.**

- **Rejected with `os-talos`, at bundle time.** That mixin relocates `gpu-operator` to `privileged-gpu-operator`, where `nvidia-dcgm.gpu-operator.svc:5555` does not resolve. The mixin pins that address and cannot vary it per composition — a second mixin setting the same allowlisted path collides at compose time. Rather than ship a DCGM check that cannot reach its hostengine, `CheckNVSentinelPreflightDCGMReachable` fails the bundle whenever preflight is enabled and gpu-operator is absent, disabled, relocated, or running with `dcgm.enabled: false`.
- **The multi-node check is unusable on Grove-scheduled recipes.** Grove's gang model is hierarchical while preflight assumes one flat PodGroup per gang ([NVIDIA/NVSentinel#1354](https://github.com/NVIDIA/NVSentinel/issues/1354)), so gang discovery finds nothing there. Because the check is off by default this costs nothing unless you request it — and if you do request it on such a recipe, the pod is stranded rather than merely uncoordinated. The two single-node checks are unaffected. Every `*-inference-dynamo` leaf is affected and no other leaf is -- today that is `b200-gke-cos`, `gb200-eks-ubuntu`, `gb200-oke-ubuntu`, `gb300-eks-ubuntu`, `h100-aks-ubuntu`, `h100-eks-ubuntu`, `h100-gke-cos`, `h100-kind`, `rtx-pro-6000-eks-ubuntu` and `vr200-rke2-ubuntu`. `TestGroveLeavesAreExactlyTheDynamoLeaves` pins the correspondence -- a Grove leaf under another name, or a dynamo leaf that moves off Grove, fails there -- but it does not read the names above, so re-check them when a dynamo leaf is added.
- **`kai-scheduler` must stay enabled, and the bundle now enforces it.** The mixin's dependency edge only orders the install — the bundler prunes an edge to a declared-but-disabled component as satisfied externally, so ordering alone would let a bundle look well-formed while `podgroups.scheduling.run.ai` never exists, crash-looping the controller and leaving `failurePolicy: Ignore` to admit every GPU pod unchecked. `CheckNVSentinelPreflightGangSchedulerRequired` blocks that at bundle time.
- **The loopback bandwidth threshold is not enforced.** The chart's 150 GB/s is calibrated for NVLink, while its own values note PCIe parts need roughly 15 — so on `l40`, `l40s` and `rtx-pro-6000` a healthy GPU would fail it. Because the checks now gate, that would be a pod-creation outage on those recipes, so the mixin sets `SKIP_BANDWIDTH_CHECK: "true"`. The loopback *connectivity* test still runs and still gates; only its bandwidth assertion is skipped. Revisit if upstream gains per-accelerator thresholds. The all-reduce check keeps its 100 GB/s threshold, but it is off by default.
- **A DCGM outage blocks GPU pods.** `preflight-dcgm-diag` treats an unreachable hostengine as a fatal result, so while DCGM is down every GPU pod in an opted-in namespace strands in `Init:Error`. `CheckNVSentinelPreflightDCGMReachable` catches the *configuration* cases at bundle time — gpu-operator absent, disabled, relocated, or running with `dcgm.enabled: false` (which the shipped Kind overlay does) — but it cannot catch a runtime outage. This is the main operational risk of adopting the mixin.


### Node Problem Detector

NVSentinel watches GPUs, drivers, NVLink and syslog. It does not watch the rest of the node, so a read-only root filesystem or a fatal CPU/memory/PCIe error leaves the node taking jobs that all fail — and the fault looks like a workload problem. [node-problem-detector](https://github.com/kubernetes/node-problem-detector) (NPD) already detects these and publishes them as Node Conditions; the Object Monitor policies above turn three of them into NVSentinel health events.

| Node Condition | Reason | Meaning |
|---|---|---|
| `XfsShutdown` | `XfsHasShutdown` | the XFS filesystem shut itself down |
| `CperHardwareErrorFatal` | `CperHardwareErrorFatal` | firmware reported a fatal UEFI CPER hardware error |
| `ReadonlyFilesystem` | `FilesystemIsReadOnly` | the root filesystem remounted read-only |

**AICR installs NPD nowhere by default.** No shipped recipe enables it; the `npd` mixin is opt-in, and `TestNoShippedRecipeAdoptsNPDMixins` keeps it that way. Platform-default adoption, starting with EKS, is deliberately deferred until the policies have been qualified on real clusters — they are still `STORE_ONLY`, and NPD runs privileged.

**Where it is safe to opt in depends on the platform, because a second copy is harmful.** Two NPD DaemonSets race to own the same Node Conditions and one silently loses its writes — no error, just conditions that flap.

**The mixin is supported only on EKS, Kind and RKE2. Every other platform fails closed.**

| Platform | Provider's own NPD | `npd` mixin |
|---|---|---|
| EKS | none | **supported** |
| Kind | none | **supported** |
| RKE2 | none — not in its packaged components | **supported** |
| GKE | enabled by default (COS and Ubuntu images, and as an addon) | blocked at bundle time |
| AKS | enabled by default via the AKS Linux Extension | blocked at bundle time |
| OKE | ships `oke-node-problem-detector`, disabled behind the `oci.oraclecloud.com/oke-node-problem-detector-enabled=true` node label | blocked — the label is operator-settable and invisible at bundle time, so a second instance cannot be ruled out |
| OCP | — | blocked — needs a SecurityContextConstraints binding AICR does not ship |
| LKE, BCM, Metal3, k0s, generic | unverified | blocked until someone confirms they run none |

The gate is an allowlist, not a denylist: a platform is permitted only once someone has checked it runs no NPD of its own. To qualify a new one, verify that, then add it to `npdQualifiedServices` in `pkg/bundler/validations/checks.go`.

`CheckNPDNotDuplicatingProviderNPD` enforces that at bundle time. It permits only the platforms verified to run no NPD of their own — `eks`, `kind`, `rke2` — and rejects everything else with a reason:

| Rejected | Why |
|---|---|
| `gke`, `aks` | the provider already runs its own |
| `oke` | Oracle ships one disabled behind a node label; that label is operator-settable and invisible at bundle time, so a second instance cannot be ruled out |
| `ocp` | the privileged DaemonSet needs a SecurityContextConstraints binding AICR does not ship — it would bundle cleanly, then fail admission |
| `os: talos` | `os-talos` relocates privileged components into `privileged-*` namespaces for Pod Security Admission, but not NPD, so it would land in a restricted namespace |
| `lke`, `bcm`, `metal3`, `k0s`, `generic` | unverified — nobody has checked whether they run their own |
| no criteria at all | the platform is unknown, and could be any of the above |

Each rejection names what would resolve it. Disabling the component explicitly (`--set node-problem-detector:enabled=false`) always skips the gate.

**Opt in via the `npd` mixin**, on your own leaf overlay:

```yaml
# your-leaf-overlay.yaml
spec:
  mixins:
    - npd
    - nvsentinel-object-monitor
```

The same catalog-registration rule applies as for the other mixins: the overlay must be committed to `recipes/overlays/` or supplied through `--data <dir>/overlays/`, because `aicr bundle -r` reads only `spec.criteria` from a directly passed file.

**The two mixins are independent, and which you need depends on the platform.** `nvsentinel-object-monitor` carries the policies; `npd` installs the detector that produces the conditions they read. On GKE and AKS the provider's own NPD publishes *some* of them, but not all: measured on live clusters, GKE publishes `XfsShutdown` and `CperHardwareErrorFatal` but names the third `ReadOnlyRootFileSystem`, while AKS publishes only `ReadonlyFilesystem`. Both run customised NPD configs rather than upstream's, so a policy whose condition that provider does not publish simply never fires, and nothing reports that. On OKE it depends on the cluster rather than the recipe: Oracle ships an NPD disabled behind an operator-settable node label, so whether the policies fire at all — and which of the three — is a property of how that cluster was configured, and is not visible at bundle time. Where no provider NPD runs at all (EKS, Kind, RKE2, and the unverified platforms), adopting the policies *without* `npd` gives no coverage whatsoever, for the same silent reason.

**All three NPD policies ship `processingStrategy: STORE_ONLY`**, unlike the operator-health policies alongside them. Upstream recommends `REPLACE_VM` for all three — the most destructive action in the pipeline — and that stays unvalidated until these have run on real clusters. `STORE_ONLY` records the event without acting on it. Two upstream caveats make that caution worth keeping: a `SystemLogMonitor` permanent condition **latches**, staying set after the underlying fault is repaired; and **restarting NPD resets its conditions**, which a consumer can read as recovery and use to cancel an active break-fix pipeline before recovery is confirmed.

**GKE auto-repair already acts on node conditions.** Before enabling anything beyond `STORE_ONLY` on GKE, decide which system owns remediation there — otherwise two systems act on one fault.

NPD runs as a privileged DaemonSet and patches Node status.

The pinned chart (`oci://ghcr.io/deliveryhero/helm-charts/node-problem-detector`) is the one upstream itself documents: the [node-problem-detector installation guide](https://github.com/kubernetes/node-problem-detector#installation) names it as the primary method and gives that exact OCI reference, with hand-applied manifests offered only as the alternative. The project publishes no chart of its own. It is still the only chart AICR pins that NVIDIA does not publish, which is worth stating plainly for supply-chain review — but it is the upstream-recommended path, not a substitute chosen here. The image it deploys (`registry.k8s.io/node-problem-detector/node-problem-detector`) is upstream Kubernetes' own.

### NIC and Fabric Fault Detection

A degraded InfiniBand or RoCE link is the failure this covers: the port stays UP and keeps passing traffic while the effective bandwidth for every GPU in a collective silently drops, so the job hangs or crashes with no obvious hardware error. NVSentinel splits the detection across three layers, and AICR ships them at two different scopes because they have different hardware requirements.

**Layer 3 — driver faults — is on everywhere.** `syslog-health-monitor` already runs on every GPU node, but AICR inherited the chart's default `enabledChecks`, which lists only the three GPU checks. `recipes/components/nvsentinel/values.yaml` adds `SysLogsNICDriverError` and enables all 11 `nicDriverDetection` patterns, which match `mlx5_core` kernel-log lines: firmware command timeouts, lost health-poll heartbeats, NAPI soft lockups. This adds no image, no component and no RBAC, and the patterns simply never fire on a node with no Mellanox driver loaded — so it is unconditional rather than platform-scoped. Note that `enabledChecks` replaces the chart's list rather than merging with it, so the values file restates all three GPU checks alongside the new one.

**Layers 1 and 2 — link state and link counters — are AKS and OKE only.** These come from the `nic-health-monitor` subchart, which reads sysfs and InfiniBand counters directly. Upstream's support matrix has exactly one row:

> Current scope: Mellanox/NVIDIA InfiniBand and RoCE devices only.

Mapped onto what AICR's overlays actually deploy:

| Platform | Fabric component | `nicHealthMonitor` |
|---|---|---|
| AKS | `network-operator` (ConnectX) | on |
| OKE | `network-operator` (ConnectX) | on |
| EKS | `aws-efa` | off — not Mellanox |
| GKE COS | `gke-nccl-tcpxo` | off — not Mellanox |
| Kind | `network-operator`, simulated | off — no real NICs |

The `nvsentinel-nic-health-monitor` mixin (`recipes/mixins/nvsentinel-nic-health-monitor.yaml`) carries the toggle, and the `aks` and `oke-ol` root overlays reference it. Mixins accumulate down the inheritance chain, so every AKS and OKE leaf gets it without restating it, and a newly added overlay in either family inherits it rather than silently missing it. Compose the mixin on your own leaf overlay to enable it elsewhere:

```yaml
# your-leaf-overlay.yaml
spec:
  mixins:
    - nvsentinel-nic-health-monitor
```

Upstream's validated-platform list covers DGX and OCI hardware and does **not** name Azure. AKS is included here because its GPU pools deploy `network-operator`/ConnectX, which is the actual hardware precondition — not because upstream qualified AKS specifically.

**Both layers ship `processingStrategy: STORE_ONLY`.** This is a deliberate downgrade from the chart's `EXECUTE_REMEDIATION` default, and more conservative than upstream's own example configuration, which reserves `STORE_ONLY` for a single pattern. Several counters — `link_downed` among them — treat any increment as fatal, and the remediation upstream recommends for them is `REPLACE_VM`, the most destructive action in the pipeline. Observation first; revisit once real coverage has been measured.

**`metadataCollector` is a hard dependency.** `nic-health-monitor` reads GPU-to-NIC topology from `/var/lib/nvsentinel/gpu_metadata.json` and has no devices to check without it. Because a missing dependency renders and deploys silently, `CheckNVSentinelNicHealthMonitorRequiresMetadataCollector` blocks the bundle instead: enabling `global.nicHealthMonitor.enabled` with `global.metadataCollector.enabled: false` fails unless `nic-health-monitor.nicInclusionRegexOverride` carries a value the monitor will actually accept. Set is not enough — the gate requires a string with at least one non-empty pattern, and every comma-separated pattern must compile, because the chart writes the value straight into the monitor's config and it refuses to start on one that does not. An override it rejects is not a bypass; it is the same missing inventory in a crash loop. That override is the documented bypass, and it forfeits the automatic management-NIC exclusion along with the dependency, so prefer enabling `metadataCollector`. No AKS or OKE overlay disables it; the overlays that do (VR200/RKE2, H200/k0s) are not in either family and never compose this mixin.

**Escalation needs the datastore.** The "three events in one hour escalates" behavior lives in the Health Events Analyzer, which needs MongoDB. Without it ([#1014](https://github.com/NVIDIA/aicr/issues/1014)) only fatal events surface.

### Enabling Remediation

**AICR does not support enabling remediation today, and this page does not carry a recipe for it.** [#1014](https://github.com/NVIDIA/aicr/issues/1014) tracks adding a qualified opt-in path.

Turning the components on is not a matter of flipping the six `enabled` flags. Those flags start the pipeline but leave `fault-remediation.maintenance.actions` at the subchart defaults, where `COMPONENT_RESET` maps to `kind: RebootNode` — so a recoverable GPU fault cordons, drains and reboots the whole node. Upstream's own remediation configuration maps that same action to `kind: GPUReset`, scoped to the affected GPU UUID, and resets in place instead. A partial enablement is therefore not a milder version of remediation; it is a more destructive one.

If you need remediation before #1014 lands, start from the chart's self-contained `values-remediation.yaml` (shipped inside the `nvsentinel` chart, pinned at the version in `recipes/registry.yaml`) rather than composing `--set` flags, and qualify the result on a cluster you can afford to have rebooted. Three further things apply whatever path you take:

- **The reboot path is privileged.** AICR pins `janitor-provider.csp.provider: generic`, so remediation reboots run as a privileged Job executing `chroot /host /sbin/reboot` on the target node. That avoids requiring cloud IAM credentials, but it is a broad grant. Cloud providers are selectable instead, and need the corresponding credentials.
- **The datastore is a real dependency.** `mongodbStore` deploys an in-cluster database. The chart also supports an external datastore and a `postgresql` provider; see the `global.datastore` block in the chart's values.
- **Check arm64 before enabling on ARM.** The chart's default MongoDB image has no `linux/arm64` manifest. arm64 works via the Percona path ([NVIDIA/NVSentinel#1328](https://github.com/NVIDIA/NVSentinel/issues/1328)).

**NVSentinel is included by default, not universally required.** `recipes/overlays/base.yaml` includes it unconditionally, so every recipe carries it unless something later removes it, and ADR-018 classifies it as `core` rather than `ops` on the rule that `ops` must be GPU-free and verifiable on CPU-only clusters. Two things can still remove it: an overlay that overrides `enabled` (the OCP overlay does), and a bundle-time exclusion on a platform whose presence is not profile-locked (EKS). Only the AKS, GKE-COS, and OKE `gpuStack` profiles make its presence mandatory — see [NVSentinel is mandatory on the profiled families](#nvsentinel-on-provider-installed-driver-platforms) below.

**The component values AICR sets are platform-correctness values, not remediation policy.** `labeler.assumeDriverInstalled` and `metadata-collector.runtimeClassName` describe facts about the target cluster that the chart cannot infer — who installs the driver, and what the RuntimeClass is named. The next section gives the per-platform values, when an explicit value is needed, and what each failure looks like when it is missing. Which components run is a different matter and stays upstream's call.

## NVSentinel on Provider-Installed-Driver Platforms

The recipes configure NVSentinel for you on every platform that needs it. This section explains what they set and why, so the values are recognizable in a generated bundle and the failure signatures are diagnosable if they ever reappear.

**Symptom.** NVSentinel's `metadata-collector` and both `syslog-health-monitor` DaemonSets report **0 desired pods** and never schedule, while everything else looks fine ([#2175](https://github.com/NVIDIA/aicr/issues/2175)).

This is easy to miss. A DaemonSet whose node selector matches no node is not unhealthy — it reports no error and emits no event — and `gpu-health-monitor` keeps running normally because it selects on the DCGM label instead. The stack presents as fully rolled out.

**Cause.** Those three DaemonSets select on the node label `nvsentinel.dgxc.nvidia.com/driver.installed`, which the NVSentinel labeler applies by watching for a GPU driver pod. Where the driver ships in the node image and no driver pod exists, the labeler never applies the label — so `labeler.assumeDriverInstalled` must be set to skip driver-pod detection and label GPU nodes unconditionally.

The recipes now carry that value wherever it is needed ([#2181](https://github.com/NVIDIA/aicr/issues/2181)):

| Platform | Driver pod the labeler can observe | `labeler.assumeDriverInstalled` | Supplied by |
|---|---|---|---|
| AKS `gpuStack=azure-managed` (default) | none — driver is in the node image | `true` | the `gpuStack` profile |
| AKS `gpuStack=operator-managed` | the operator's driver pod | `false` | the `gpuStack` profile |
| GKE COS `gpuStack=gke-default` (default) | none the labeler can observe — the driver is finalized by an init container of GKE's kube-system DaemonSet | `true` | the `gpuStack` profile |
| GKE COS `gpuStack=bundle-installer` | the bundle's `gcp-driver-installer` DaemonSet | `false` | the `gpuStack` profile |
| OKE `gpuStack=oci-managed` (default) | none — driver is in the node image; OKE's `NvidiaGpuPlugin` add-on advertises | `true` | the `gpuStack` profile |
| OKE `gpuStack=operator-managed` | the operator's driver pod | `false` | the `gpuStack` profile |
| EKS | the operator's driver pod | unset (chart default `false`) | — |
| Kind (nvkind) | none — driver is host-installed | `true` | the overlay (Kind has no profile) |

The explicit `false` on the operator-managed variants is deliberate rather than redundant: it keeps the path profile-owned, so it cannot be flipped into an unsafe hybrid later. Do **not** assume a preinstalled driver where the GPU Operator installs one — skipping detection there would keep the label applied across an unloaded or unhealthy driver.

**NVSentinel is mandatory on the profiled families.** Because the AKS, GKE-COS, and OKE `gpuStack` profiles name nvsentinel, its presence is profile-owned: `--set nv-sentinel:enabled=false` and a `bundlers=` list that omits it are both rejected on those platforms. That is intended — NVSentinel is a required component for these deployments. It remains optional on platforms with no `gpuStack` profile, such as EKS.

AKS, GKE-COS, and OKE get the install-time profile lock; Kind sets the value at overlay level, so a bundle-time or declared-dynamic change is still rejected by the gate below, but a manual post-generation edit to the rendered Helm values is not.

If you do need to set it yourself on an unlisted platform, it is an ordinary override:

```shell
aicr bundle -r recipe.yaml \
  --set nv-sentinel:labeler.assumeDriverInstalled=true \
  -o ./bundles
```

The value renders the labeler's `--assume-driver-installed` argument. That is the chart-level automation of the Manual Labeling Procedure documented in NVSentinel design 018. Upstream has settled the design question: it is the recommended, permanent mechanism for host-installed drivers — no automatic detection fallback will be added ([NVIDIA/NVSentinel#1583](https://github.com/NVIDIA/NVSentinel/issues/1583)).

**Do not label the nodes by hand.** `kubectl label node <node> nvsentinel.dgxc.nvidia.com/driver.installed=true` takes effect immediately — the DaemonSets roll out — and then silently reverts. With no driver pod to observe, the labeler computes an empty desired value and removes the label on its next reconcile. Because upstream design 018 documents manual labeling as the procedure for this case, an operator following it will see it work and later find the DaemonSets back at 0 desired.

**`aicr bundle` rejects a configuration that would reintroduce the gap.** A recipe that includes nvsentinel with no observable driver pod and without `labeler.assumeDriverInstalled` fails bundle generation with a blocking error (`CheckNVSentinelDriverLabelDetectable`). On the profiled families the value is also profile-owned, so a `--set` diverging from the selected `gpuStack` value is rejected before the gate even runs.

One exception, for completeness: the gate is silent if you disable *both* label consumers (`--set nv-sentinel:global.metadataCollector.enabled=false` **and** `--set nv-sentinel:global.syslogHealthMonitor.enabled=false`). Nothing then reads the label, so there is no gap to reintroduce. Disabling only one still requires the value, and no recipe disables either — both default to enabled, and the gate fails closed when it cannot prove otherwise. Everything above therefore applies to every shipped configuration.

**A second, distinct failure on AKS `azure-managed`: RuntimeClass mismatch.** The metadata-collector DaemonSet requests a RuntimeClass by name, and the GPU Operator's ClusterPolicy controller names that object after `operator.runtimeClass`. The AKS `azure-managed` profile retargets it to `nvidia-container-runtime`, so a metadata-collector left on its chart default `nvidia` finds no such RuntimeClass and the API server rejects every pod at admission (`pod rejected: RuntimeClass "nvidia" not found` — [#2176](https://github.com/NVIDIA/aicr/issues/2176)).

The two signatures differ: the label gap above shows **0 DESIRED** pods (never scheduled, no error, no event); the RuntimeClass mismatch shows **N desired / 0 CREATED** with a `FailedCreate` event on the DaemonSet and no pod object to describe.

The AKS `gpuStack` profile now owns both names — `gpu-operator.operator.runtimeClass` and `nvsentinel.metadata-collector.runtimeClassName` — in the same profile value, so they agree by construction under either value and no override is needed:

| AKS profile value | `operator.runtimeClass` | `metadata-collector.runtimeClassName` |
|---|---|---|
| `azure-managed` (default) | `nvidia-container-runtime` | `nvidia-container-runtime` |
| `operator-managed` | `nvidia` | `nvidia` |

Every other platform leaves `operator.runtimeClass` at the shared chart default `nvidia`, so neither side needs a value. `CheckNVSentinelRuntimeClassCoherence` still compares the two resolved names as defense in depth, treating either side unset as `nvidia`.

An AKS bundle therefore needs no NVSentinel overrides at all — only the keyed toleration AKS requires independently of NVSentinel (bundling an AKS recipe without one is itself a blocking error, `CheckWildcardAcceleratedToleration`):

```shell
aicr bundle -r recipe.yaml \
  --accelerated-node-toleration nvidia.com/gpu:NoSchedule \
  -o ./bundles
```

See [AKS GPU Setup](../integrator/aks-gpu-setup.md#default-use-the-aks-azure-managed-profile) for the per-profile guidance.

## Enabling NVCRE

**nvcre** is not on any shipped overlay, so enabling it means writing the `componentRef` yourself. Two requirements are easy to miss, and each one produces a different failure.

`dependencyRefs` only *orders* components that are already in `componentRefs` — it does not add one. Naming a component that is not present fails resolution outright with `component "nvcre" references unknown dependency "kubeflow-trainer"`. NVCRE drives its benchmarks through Kubeflow Trainer (`TrainJob` / `TrainingRuntime`) and the chart does not install Trainer, so Trainer has to come from somewhere else — the `platform-kubeflow` mixin is the cleanest source.

Component values are also never auto-discovered from the component name: a ref with no `valuesFile` and no inline overrides resolves to an empty map. Omit it and the chart defaults apply, which means a `ServiceMonitor` you did not ask for (requiring prometheus-operator CRDs) and a release-prefixed Deployment name such as `aicr-stack-nvcre-manager`, which the shipped health check cannot match.

Add this to an overlay that already inherits a stock AICR base:

```yaml
spec:
  mixins:
    - platform-kubeflow          # brings in kubeflow-trainer
  componentRefs:
    - name: nvcre
      type: Helm
      valuesFile: components/nvcre/values.yaml
      dependencyRefs:
        - kubeflow-trainer
```

Do not also declare `kubeflow-trainer` locally. The mixin's ref sets `type`, `valuesFile`, and `dependencyRefs`, all of which are prohibited collision fields, and overlay chains merge before mixins — so a local ref collides rather than overrides.

Prerequisites: NVIDIA GPU Operator and cert-manager, both inherited from `base.yaml` by every stock recipe, plus Kubeflow Trainer, which is not — see the fragment above.

NVCRE v0.2.0 expects Kubeflow Trainer **v2.2.1** — it pins `kubeflowTrainerVersion = "v2.2.1"` and its `setup status` reports the 2.2.0 that the registry defaults to as unsupported. No functional break is known between the two versions: the CRD delta is documentation text plus one embedded PodSpec field NVCRE does not set. Aligning the global Trainer default is tracked separately.

## NIM Workload Credentials

AICR installs the **k8s-nim-operator** only. It does not create a `NIMService` and does not create credentials — deploying a workload is an operator step, and there are two ways to supply the model.

Whichever path you take, `spec.authSecret` is required by the `NIMService` schema and must name an existing secret in the workload's namespace. `spec.image.pullSecrets` is optional; the image block requires only `repository` and `tag`.

### NGC path

Model artifacts come from NGC, so the secret must carry a valid `NGC_API_KEY`:

```bash
kubectl create secret generic ngc-api-secret \
  --from-literal=NGC_API_KEY="$NGC_API_KEY" -n nim-workload
```

Add a `docker-registry` secret and reference it from `image.pullSecrets` when the image lives in a private or authenticated registry path. See `demos/workloads/inference/nimservice-llama-3-2-1b.yaml` for a complete example.

### Credential-free path (Hugging Face)

Setting `NIM_MODEL_NAME` to an `hf://` URI puts the operator on its Hugging Face path, where it marks `NGC_API_KEY` optional and injects `HF_TOKEN` from the same `authSecret`. With an ungated Hugging Face model and a NIM image that pulls anonymously, no NGC credential is needed anywhere:

```bash
kubectl create secret generic hf-secret --from-literal=HF_TOKEN="" -n nim-workload
```

```yaml
spec:
  authSecret: hf-secret                                  # holds only HF_TOKEN
  image:
    repository: nvcr.io/nim/meta/llama-3.1-8b-instruct   # pulls anonymously; no pullSecrets
    tag: "2.0.10"                                        # pin a version; avoid the mutable latest
  env:
    - name: NIM_MODEL_NAME
      value: hf://Qwen/Qwen3-0.6B                        # ungated model
    - name: NIM_SERVED_MODEL_NAME
      value: Qwen/Qwen3-0.6B                             # the OpenAI-API `model` id
```

The `HF_TOKEN` key must exist in the secret — that reference is not optional — but an empty value is sufficient for an ungated model. A gated Hugging Face repository needs a real token here.

Model-specific NIM repositories (for example `nim/meta/llama-3.1-8b-instruct`) serve anonymous registry tokens; the generic Multi-LLM image `nim/nvidia/llm-nim` does not and requires a pull secret.

Note that pairing a model-specific image with an unrelated `hf://` model is off-label: the container runs its own profile against the downloaded weights. It works, but `nim/nvidia/llm-nim` is the image intended for arbitrary Hugging Face models — and because that repository is gated, choosing it trades the credential-free property for a supported pairing. Pin an image tag rather than `latest` so the pairing you validated is the one you ship.

See `demos/workloads/inference/nimservice-hf-nocred.yaml` for a complete example.

## Inference Gateway Network Exposure

Inference recipes include the **agentgateway** component, which deploys an `inference-gateway` Gateway. The agentgateway controller materializes that Gateway into a `Service` of type `LoadBalancer`, so on every cloud the platform provisions a load balancer for the (plaintext HTTP, unauthenticated) inference endpoint. Left unrestricted that load balancer is internet-facing, so `aicr bundle` scopes it to private networks by default — the opt-in path for public exposure and the validation behavior are described below.

`aicr bundle` is **private by default**: when a bundle includes `agentgateway` and `agentgateway.allowedSourceRanges` is empty or unset, the bundler injects the private RFC1918 ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`) into the generated Service's `spec.loadBalancerSourceRanges`. The deployed gateway is therefore reachable from inside the cluster/VPC (and from privately-routed peers) but **denied to the public internet** — it is never emitted open to `0.0.0.0/0` without an explicit opt-in. (Kubernetes treats an empty `loadBalancerSourceRanges` as allow-all, so a safe default has to be a real list, not an empty one.) A bundle note records when the default was applied.

To restrict it to specific trusted networks instead — for example to allow a corporate VPN, which egresses from a **public** IP and is therefore *not* covered by the RFC1918 default — set `agentgateway.allowedSourceRanges` to a list of CIDR (Classless Inter-Domain Routing) blocks. The values replace the default and are rendered into the generated Service's `spec.loadBalancerSourceRanges`, which the AWS, GCP, Azure, and OCI cloud load balancers all honor — so one setting locks the gateway down on every platform.

Do **not** use plain `--set` for this key. `--set agentgateway:allowedSourceRanges=<cidr>` writes `loadBalancerSourceRanges` as a bare string instead of a list; the bundler rejects that with `ErrCodeInvalidRequest` (a bare scalar would render a type-invalid Service). Use the list-aware [`--set-json` / `--set-file`](cli-reference.md#list-and-object-value-overrides) flags from the CLI:

```shell
aicr bundle -r recipe.yaml \
  --set-json agentgateway:allowedSourceRanges='["216.228.127.128/30"]'
```

or scope the gateway through a recipe overlay or `componentRef` override:

```yaml
componentRefs:
  - name: agentgateway
    type: Helm
    overrides:
      allowedSourceRanges:
        - 216.228.127.128/30   # e.g. corporate egress
```

The default is the generic RFC1918 private set rather than a fixed customer CIDR: a baked-in specific range would firewall every downstream deployment to one network and lock other operators out of their own gateway. RFC1918 is universal — it trusts only privately-routed traffic — so it is a safe default that still denies the public internet. Override it whenever you need to admit a specific public client.

If public exposure is genuinely intended, opt in **explicitly** with an any-source CIDR — bundle generation then succeeds but logs a loud warning that the gateway is open to the entire internet:

```shell
aicr bundle -r recipe.yaml \
  --set-json agentgateway:allowedSourceRanges='["0.0.0.0/0"]'
```

This setting filters by source IP only; it does not add TLS or authentication to the gateway listener.

### Exposure guardrails

AICR enforces and surfaces inference-gateway exposure in two places:

- **Bundle-time private-by-default.** When a bundle includes `agentgateway` and `allowedSourceRanges` is empty/unset, `aicr bundle` injects the RFC1918 private ranges so the deployed gateway denies the public internet, and records a bundle note. An invalid value (a bare-string `--set`, a non-list, an unparseable CIDR, or a non-canonical CIDR such as `1.2.3.4/24` that Kubernetes' strict validation would reject at apply time) is rejected with `ErrCodeInvalidRequest`. A scoped list passes silently; an explicit any-source CIDR (`0.0.0.0/0` or `::/0`) passes with a loud warning as a deliberate opt-in. See [#1373](https://github.com/NVIDIA/aicr/issues/1373).
- **Conformance check.** The `inference-gateway` conformance check (run during `aicr validate --phase conformance` on a live cluster) inspects the gateway's `LoadBalancer` Service and records its exposure as evidence — the source ranges if scoped, or an explicit "open to `0.0.0.0/0`" finding if not. Set `AICR_REQUIRE_SCOPED_INFERENCE_GATEWAY=true` on the validator environment to escalate an open gateway to a check **failure**.

## k8s-aibom Runtime Inventory

AICR qualifies k8s-aibom v1.3.0 as an optional Helm component. It is not in
the base or a mixin. Exactly one stock recipe installs it,
`h100-gke-cos-inference`, under [ADR-019](https://github.com/NVIDIA/aicr/blob/main/docs/design/019-k8s-aibom-runtime-inventory.md)'s
stock-adoption amendment. Decline it at generation time with
`aicr recipe --runtime-inventory disabled`, described below.

`h100-gke-cos-inference-dynamo` inherits from that recipe and deliberately
declines the component, so the Dynamo platform recipe deploys exactly what it
did before. Adoption beyond the one recipe is a later decision.

To enable it anywhere else, add this reference to a custom or external overlay
and keep that overlay's criteria as narrow as the intended rollout:

```yaml
spec:
  componentRefs:
    - name: k8s-aibom
      type: Helm
      valuesFile: components/k8s-aibom/values.yaml
```

A broad criteria overlay affects every matching recipe. In particular,
`intent: any` is universal across intents; do not use it unless that injection
is deliberate. The in-tree `recipes/overlays/monitoring-hpa.yaml` overlay shows
that broad reach: its `criteria: intent: any` attaches to every matching intent.
See [Recipe Development](../integrator/recipe-development.md) for external data
and criteria composition.

The qualified artifacts are source tag `v1.3.0` at commit
`30af41abbe0bed3c41a42289ccf294be8c4779bb`, OCI chart
`oci://ghcr.io/googlecloudplatform/charts/k8s-aibom:1.3.0`, and the controller
image pinned by digest in the component values. v1.3.0 is the API-graduation
release: both `v1alpha1` and `v1beta1` are served and CRD storage is on
`v1beta1`, while the chart still renders the `AIBOMControllerConfig` resource
itself at `v1alpha1`. Upstream states Kubernetes support as a policy rather
than a fixed range: stable APIs only, no known version ceiling, tested floor
1.27, backed by a weekly CI matrix. The authoritative statement is
[upstream's compatibility policy](https://github.com/GoogleCloudPlatform/k8s-aibom/blob/main/docs/compatibility.md),
which is linked rather than restated here so it cannot drift out of date on
our side. That link deliberately tracks `main`: the point is the current
policy, not a snapshot of it, which is the opposite of how this page cites
qualified artifacts.

AICR also observed the dedicated integration test passing on its Kind 1.36.1
node image; that is qualification evidence, not an extension of upstream's
support statement.

### Health and readiness

The deployment-phase check requires the controller Deployment to have at least
one desired replica and all desired replicas available. It also requires the
cluster-scoped `AIBOMControllerConfig/default` to report a current
`Ready=True` condition: both top-level status and the condition must have
observed the object's current generation. Missing or stale resources fail
closed. Zero `AIBOM` objects is healthy before any namespace opts in.

The check also requires both shipped CRDs, `aiboms.aibom.k8saibom.dev` and
`aibomcontrollerconfigs.aibom.k8saibom.dev`, to report the storage version of
the chart version pinned in the registry. It matters because Helm skips a
chart's `crds/` directory on upgrade, so a cluster that missed the CRD step can
run a new controller against the previous schema while the older version stays
served and the controller keeps working.

`k8s-aibom` is marked `ownsCRDs` in the registry, so the deployers update its
CRDs for you: Flux through `spec.upgrade.crds: CreateReplace`, `helm` and
`helmfile` through the generated `apply-crds.sh`, and Argo CD by applying them
as ordinary manifests each sync.

**That automation is tied to the registry-pinned coordinates, not to the
component.** `ownsCRDs` records an audit of one specific chart, so Flux, `helm`,
and `helmfile` all check that the componentRef still resolves to the registry's
`source`, `chart`, and `version` before acting, and do nothing when any of the
three is overridden. A recipe that overrides the version — including the
override described under [Overriding the chart version](#overriding-the-chart-version-requires-overriding-this-assertion)
below — therefore upgrades the controller with **no** CRD update on those three
deployers, silently. Such a recipe needs its own audit of the chart it points
at and its own CRD step; the fallback command below is the manual form. Argo CD
is unaffected, since it applies whatever CRDs the rendered chart contains
regardless of provenance. The assertion is still worth making on every
deployer, because it proves the deployed CRDs match the pinned chart rather
than merely that some deployer was expected to update them.

Both CRDs are asserted separately, so a failure names which one is stranded
and a partially applied CRD set cannot pass. If this check fails after a chart
bump, the CRD command in
[Upgrade, uninstall, and troubleshooting](#upgrade-uninstall-and-troubleshooting)
is the thing to run.

The assertion establishes that the storage-version contract matches the pinned
chart. It is not provenance: it reads one field, so it cannot show the CRDs
originated from that chart, and it cannot tell apart chart versions that share
a storage version. Charts 1.0.0, 1.1.0, and 1.2.0 all declare `v1alpha1` as
storage. So the check catches a stranded upgrade that crosses a
storage-version boundary, such as the 1.2.0 to 1.3.0 move this pin made, and
does not catch one within a boundary, such as 1.0.0 to 1.2.0.

**Declining the component.** `h100-gke-cos-inference` installs `k8s-aibom` by
default. Decline it at generation time:

```bash
aicr recipe --service gke --accelerator h100 --os cos --intent inference \
  --runtime-inventory disabled -o recipe.yaml
```

The same flag works for a recipe that adds the component through a custom
overlay — the shape shown above. Point `--data` at the directory holding that
overlay:

```bash
aicr recipe --service gke --accelerator h100 --os cos --intent inference \
  --data ./my-recipes --runtime-inventory disabled -o recipe.yaml
```

Passing the flag against a recipe that does not declare the component is an
error, not a silent no-op. Training recipes do not, so:

```console
$ aicr recipe --service gke --accelerator h100 --os cos --intent training \
    --runtime-inventory disabled
[INVALID_REQUEST] runtime inventory mode "disabled" requires the recipe to
declare component "k8s-aibom"; this recipe does not resolve it
```

The selection is recorded in the emitted recipe as
`configuration.runtimeInventory.mode`, and the component's ref carries
`install: false`, so the component and its health check are both absent from
the bundle and from deployment validation. A bundle-time
`--set k8s-aibom:enabled=false` is **not** equivalent and is not a supported
way to decline the component: it changes neither the recipe nor its health
checks, which is why [ADR-019](https://github.com/NVIDIA/aicr/blob/main/docs/design/019-k8s-aibom-runtime-inventory.md)
rejects it as a selection contract.

A wrong `--service` or a typo therefore surfaces instead of producing a recipe
that claims a decision it never applied.

The same selection is available in an `AICRConfig` document as
`spec.recipe.configuration.runtimeInventory.mode`.

**Overriding the chart version requires overriding this assertion.** Assert
content is static YAML with no templating, so the expected storage version is
a literal tied to the registry's pinned chart, currently `v1beta1` for chart
1.3.0. Charts 1.2.0 and earlier declare only `v1alpha1`. A recipe that sets
`version` on the `k8s-aibom` componentRef to a chart with a different storage
version will therefore fail this step even though the cluster is correct. Such
a recipe must supply matching inline `healthCheckAsserts` on the componentRef,
or set `healthCheckSkip: true` to drop the registry check entirely.

`readiness.strictConfig` is enabled, so invalid new configuration cannot
silently replace the controller's last-known-good configuration while the pod
continues to report ready.

### Security, privacy, and retention

Namespace discovery requires the label
`aibom.k8saibom.dev/enabled=true`. AICR does not apply it. No external sink,
endpoint, credential, or Secret access is configured by default. BOMs up to
262144 bytes are stored inline in `AIBOM.status`; larger output is summarized
and marked truncated when no sink is configured. Inline status consumes etcd
storage, so workload count and document size are part of the cluster control
plane footprint. The controller runs non-root with a read-only root filesystem,
RuntimeDefault seccomp, no privilege escalation, and no Linux capabilities.

The namespace label limits which workloads produce AIBOMs, not informer read
scope. The controller still reads workload and pod specifications
cluster-wide—including image references, arguments, and inline environment
values—into memory. With the default empty sink list that data does not leave
the cluster, but cluster-wide visibility remains part of the privacy boundary.
The controller is not read-only: its bounded RBAC permits writes to its own
AIBOM API resources and required status subresources, configuration status,
and Kubernetes Events. It has no Secret access while sinks are disabled.

An AIBOM is owned by its top-level workload and is garbage-collected when that
owner is deleted. Helm does not delete CRDs from a chart's `crds/` directory on
uninstall; consequently AIBOM resources for owners that still exist may remain
after controller removal. External sinks, if an operator configures one, have
their own retention policy outside AICR.

### Upgrade, uninstall, and troubleshooting

Upgrade the component by qualifying a new chart and image together, then
regenerate the custom recipe and bundle. Do not change only the controller
image: chart, CRDs, status API, and image are one qualified set. Quiesce
configuration changes during rollback and confirm that
`AIBOMControllerConfig/default` returns to a current `Ready=True` state.

**CRDs are applied for you; the manual command is a fallback.** The chart
ships its CRDs under `crds/`. Helm installs that directory on first install and
never touches it again on upgrade, so a chart bump whose CRDs changed would
leave the previous schema in place and the API server would silently prune the
new controller's writes to added fields.

Every deployer closes that on its own, by a different route; see the deployer
table below. `helm` and `helmfile` bundles carry an `apply-crds.sh` in the
component's folder, run automatically before the upgrade; `flux` and Argo CD
apply the CRDs through their own controllers.

Run the command below by hand only when you are upgrading outside a generated
bundle, or when `apply-crds.sh` failed and you are reproducing it:

```bash
CHART="oci://ghcr.io/googlecloudplatform/charts/k8s-aibom"
VERSION="1.3.0"   # replace with the version you are upgrading to

work="$(mktemp -d)"
helm pull "${CHART}" --version "${VERSION}" --destination "${work}"
tar -xzf "${work}"/*.tgz -C "${work}"

# One kubectl call per CRD file, create first and replace if it exists.
find "${work}" -type f -path '*/crds/*' \( -name '*.yaml' -o -name '*.yml' \) \
  | sort \
  | while read -r crd; do
      grep -q '[^[:space:]]' "${crd}" || continue
      kubectl create -f "${crd}" 2>/dev/null || kubectl replace -f "${crd}"
    done
```

Three details are load-bearing, and the obvious shorter forms fail on them:

- **Create-or-replace, not `kubectl apply`.** Server-side apply deletes a field
  the manifest omits only when no other manager owns it, and Helm created these
  CRDs. A schema field or `spec.versions` entry that the new chart *removes*
  therefore survives an apply that exits 0, leaving the controller and the
  schema out of step. Replace makes the chart authoritative for the whole
  object. This is why `ownsCRDs` requires that no CRD use
  `spec.conversion.strategy: Webhook`: replace discards a `caBundle` injected at
  runtime.
- **Read the CRDs from the chart archive, not from `helm show crds`.** That
  command's output shape differs by major version: Helm 4 prepends `---` before
  every CRD, Helm 3 prepends one only for `show all` and emits nothing between
  documents. Any separator-based filter silently yields nothing on Helm 3.
- **Pull once and work from that archive.** Repository, chart, and version are
  coordinates, not content. Reading CRDs through them and letting the upgrade
  resolve them again is two fetches, and a mutable tag does not promise the same
  bytes.

The generated `apply-crds.sh` does exactly this, with each call bounded; it is
the reference if you need the details.

Which deployers need that step differs, so check yours:

| Deployer | CRD behavior on upgrade | Manual step needed |
|---|---|---|
| `helm` | `helm upgrade` skips `crds/`, so the bundle emits `apply-crds.sh` for components the registry marks `ownsCRDs` and `install.sh` runs it first | Only for components without `ownsCRDs` |
| `helmfile` | Upgrades through Helm, so it skips `crds/` too; the release carries a `presync` hook running the same `apply-crds.sh` | Only for components without `ownsCRDs` |
| `flux` | The generated `HelmRelease` sets `spec.upgrade.crds: CreateReplace` for components the registry marks `ownsCRDs`, and leaves the helm-controller `Skip` default in place for the rest | Only for components without `ownsCRDs` |
| `argocd`, `argocd-helm` | Argo CD renders the chart with CRDs included and applies them as ordinary manifests each sync | No |

Argo CD is the one deployer that upgrades CRDs for *every* component rather
than only the opted-in ones, because including them is how it renders a Helm
source at all. Suppressing that per component is not available: `skipCrds`
would also drop the CRDs on first install.

`ownsCRDs` is opt-in, and narrow on purpose. Of the 15 registry components
that ship CRDs under `crds/`, 11 share at least one CRD with another
component: `nfd`, `gpu-operator`, and `network-operator` all ship the
NodeFeature CRDs, and `nfd`, `gpu-operator`, and `kai-scheduler` all appear
together in `base.yaml`. If every release replaced CRDs on upgrade, two or
three releases would rewrite the same CRD on every reconcile or redeploy,
each with the schema its own chart pins. Requiring the opt-in is what
prevents that, so it stays opt-in.

A component qualifies only if it solely owns every CRD it ships and ships none
using `spec.conversion.strategy: Webhook`, since replace discards a `caBundle`
injected at runtime. `kubeflow-trainer` is excluded for that second reason.
Currently `gatekeeper`, `k8s-aibom`, `nvcre`, and `nvsentinel` qualify; the
audited chart version for each is pinned in `pkg/recipe/ownscrds_audit_test.go`,
so bumping a pin without re-auditing fails CI.

Uninstall in this order. Removing the component from the overlay and applying a
regenerated bundle does **not** remove the previously installed release: the
`helm` and `helmfile` deployers install releases by name, and a release the new
bundle no longer mentions is simply left alone. Skipping the explicit uninstall
leaves the controller running while the next step deletes the CRs and CRDs
underneath it, so it reconciles against resources that are disappearing.

1. Remove the component reference from the custom overlay and regenerate the
   recipe and bundle.
2. Uninstall the release, scoped to this component only:

   ```bash
   # helm and helmfile bundles alike: helmfile installs through Helm, so the
   # release is an ordinary Helm release and this removes exactly one.
   helm uninstall k8s-aibom -n k8s-aibom-system
   ```

   For Argo CD, delete the owning `Application`; for Flux, the `HelmRelease`.
   Confirm the controller Deployment is gone before continuing.

   **Do not use `helmfile destroy` for this.** It tears down *every* release in
   the bundle in reverse dependency order, not just this component. It is also
   ineffective here: step 1 regenerated the bundle without `k8s-aibom`, so the
   release is no longer declared in it and `destroy` would not remove the one
   release you actually want gone while removing all the ones you do not. If
   you prefer a Helmfile-native command, run it against a bundle that still
   declares the component and scope it explicitly with
   `helmfile destroy --selector name=k8s-aibom`.
3. Only then delete retained AIBOMs and, last, the CRDs.

Deleting the CRDs cascades to every AIBOM stored cluster-wide, including any
belonging to a namespace or release you did not intend to touch. Enumerate
before deleting rather than passing `--all`:

```bash
# Review what exists and who owns it; delete only what this release should own.
kubectl get aiboms.aibom.k8saibom.dev --all-namespaces \
  -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,OWNER:.metadata.ownerReferences[0].name
kubectl -n <namespace> delete aiboms.aibom.k8saibom.dev <name>

# CRDs last, and only once no other release uses them — deletion removes every
# stored custom resource of these kinds cluster-wide.
kubectl delete crd \
  aiboms.aibom.k8saibom.dev \
  aibomcontrollerconfigs.aibom.k8saibom.dev
```

If health validation fails, inspect the Deployment and configuration before
looking for AIBOMs:

```bash
kubectl rollout status deployment/k8s-aibom -n k8s-aibom-system
kubectl get aibomcontrollerconfig default -o yaml
kubectl logs deployment/k8s-aibom -n k8s-aibom-system
```

A current `Ready=False` or stale `observedGeneration` means the active
configuration was not accepted. If the controller is healthy but produces no
inventory, verify the namespace label and the workload kind. A truncated BOM
with no sink is expected once its canonical document exceeds the inline
threshold; configure retention and credentials explicitly before enabling an
external sink.

## Adding Components

New components are added declaratively in `recipes/registry.yaml` — no Go code required. See the [Contributing Guide](https://github.com/NVIDIA/aicr/blob/main/CONTRIBUTING.md) and [Components](../contributor/component.md) docs for details.

## Upgrade Notes

Migration steps when upgrading from a prior AICR-generated bundle to a newer one that changes how a component delivers its Kubernetes resources.

A generated recipe is a point-in-time artifact of the AICR binary that produced it: the embedded registry, overlays, manifest paths, and chart pins are part of that binary's surface. When upgrading AICR, regenerate the recipe from scratch with the new binary (`aicr recipe ...`) before re-bundling. `aicr bundle --recipe <old-file>` against a newer binary may fail if the saved recipe references manifest paths the new release has moved or removed (see [Bundle Generation Fails](cli-reference.md#bundle-generation-fails) for the specific error).

**Regenerating also re-derives each component's namespace.** The namespace comes from the registry in the binary doing the regenerating, so if a component's default namespace moved between the two AICR releases, the new recipe names the new one. Helm cannot move a release between namespaces, so the resulting bundle installs a second copy of the component beside the one already running. Pass `aicr recipe --inherit-from <prior recipe or bundle directory>` to keep the namespaces the prior artifact deployed into, and run [`aicr upgrade-check`](upgrading.md#when-a-component-moves-namespace) to see whether any component moved in the first place.

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

### `grove`: `v0.1.0-alpha.8` (or earlier) to `v0.1.0-alpha.12`

alpha.8's `clustertopologies.grove.io` CRD (kind `ClusterTopology`, shortname
`ct`) is dropped by alpha.12 in favor of `clustertopologybindings.grove.io`,
which reuses shortname `ct`. `helm upgrade` never applies changed `crds/` (see
the CRD-upgrade caveat above), so a plain chart bump alone never installs the
new CRD and the operator crash-loops on `no matches for kind
"ClusterTopologyBinding"` regardless of deployer. If the new CRD is applied
before the old one is deleted, the shortname collision blocks it from ever
reaching `Established`.

Fresh installs are unaffected. To migrate an existing cluster:

1. Check for any live `ClusterTopology` resources — if any exist, **stop**
   for an explicit data migration before touching CRDs:
   ```bash
   kubectl get clustertopologies.grove.io -A
   ```
2. If zero exist, delete the obsolete CRD:
   ```bash
   kubectl delete crd clustertopologies.grove.io
   ```
3. Apply the new CRDs directly from the chart — this works from any
   directory, no local checkout needed:
   ```bash
   helm show crds oci://ghcr.io/ai-dynamo/grove/grove-charts --version v0.1.0-alpha.12 \
     | sed -n '/^---$/,$p' \
     | kubectl apply --server-side --force-conflicts -f -
   ```
4. Resume whichever deployer you use (`helm`/`helmfile`: re-run `install.sh`
   or `helmfile apply`; `flux`/`argocd`/`argocd-helm`: reconcile/sync as
   usual). AICR's generated `install.sh` already passes `--force-conflicts`
   on Helm 4.

Verified live end-to-end on a real EKS cluster, 2026-09-02: dynamo-operator,
grove-operator, and kai-scheduler gang-scheduling all confirmed healthy
together post-migration.

### `dynamo-platform`: opting back into bundled NATS on a standing cluster

Dynamo 1.4+ no longer installs bundled NATS by default (the request plane
defaults to TCP and the KV event plane to ZMQ). Re-enabling it needs to be
done by **regenerating the bundle**, not by hand-editing a generated
bundle's `values.yaml` or `cluster-values.yaml` -- both are covered by the
bundle's checksum manifest, and `aicr verify` fails closed on any modified
file:

```bash
aicr verify ./bundle
# ✓ Checksums verified (87 files)
# Bundle verification: PASSED

# Hand-edit 017-dynamo-platform/values.yaml, then:
aicr verify ./bundle
# ✗ Checksum verification failed
# - [INVALID_REQUEST] checksum mismatch for "017-dynamo-platform/values.yaml"
# Bundle verification: FAILED (exit 4)
```

A modified bundle also invalidates the binding of any existing attestation
to the original checksum digest. Regenerate instead, using the same
`aicr bundle` command and flags you used originally, with the NATS
overrides added:

```bash
aicr bundle --recipe recipe.yaml \
  <your original --accelerated-node-selector / --system-node-selector / etc. flags> \
  --set dynamoplatform:global.nats.install=true \
  --output ./bundle
```

If your system node group is tainted or the cluster has no default
StorageClass, add the scheduling/storage overrides a re-enabled NATS also
needs -- AICR no longer targets NATS with `--system-node-selector`,
`--system-node-toleration`, or `--storage-class` at bundle-generation time
(those registry paths were removed along with bundled-by-default NATS), so
pass them explicitly or the NATS pod/PVC can stay `Pending`. Use
`--set-json` for `nodeSelector` and `tolerations` -- the scalar `--set`
dot-path parser rejects qualified Kubernetes label keys that contain a `/`
(e.g. `eks.amazonaws.com/nodegroup`), and there is no scalar form for a
tolerations list at all:

```bash
  --set-json 'dynamoplatform:nats.podTemplate.merge.spec.nodeSelector={"<key>":"<value>"}' \
  --set-json 'dynamoplatform:nats.podTemplate.merge.spec.tolerations=[{"key":"<key>","operator":"Equal","value":"<value>","effect":"NoSchedule"}]' \
  --set dynamoplatform:nats.config.jetstream.fileStore.pvc.storageClassName=<name>
```

Verified against this exact sequence (generate → `aicr verify` passes →
hand-edit → `aicr verify` fails with exit 4 → regenerate with the overrides
above → rendered `nats.podTemplate.merge.spec` carries both the qualified
nodeSelector and the toleration → `aicr verify` passes again), 2026-09-02.
A scalar `--set …nodeSelector.eks.amazonaws.com/nodegroup=<value>` was
separately confirmed to fail with exit 2 (`INVALID_REQUEST: invalid path
segment "com/nodegroup"`), and `--system-node-toleration` alone was
confirmed to not reach NATS (`nats.podTemplate.merge.spec.tolerations`
rendered empty; only the operator's `controllerManager.tolerations` was
set) -- both are why the overrides above are required rather than optional.

On an in-place `helm upgrade` from the regenerated bundle, disabling NATS
removes its StatefulSet but does not delete its PVC; inspect
`kubectl get pvc -n dynamo-system -l app.kubernetes.io/name=nats` before
deciding whether to retain or delete it.

**Separately, and more importantly than the NATS opt-out above:** bumping
the operator without also bumping any standing `DynamoGraphDeployment`'s
runtime image is its own hazard, independent of NATS. An older runtime
under a newer operator is the same version-skew class that caused the
frontend discovery panic fixed in #1193 -- setting `DYN_EVENT_PLANE=zmq`
on the old workload is defense in depth, not a substitute for bumping its
image to match the operator.

### `dynamo-platform`: reusing an existing StorageClass for the GB200 model-weights cache

On GB200 (`a4x-highgpu-4g`) GKE leaves, `dynamo-platform` bundles a fixed
`a4x-compatible` StorageClass
(`recipes/components/dynamo-platform/manifests/a4x-storage-class.yaml`) so
the `inference-perf` model-weights cache PVC has somewhere Hyperdisk-backed
to bind, since those nodes can't attach Persistent Disk at all. See
[GKE GB200 networking](../integrator/gke-gb200-networking.md#storage-prerequisites).

Redirecting the cache PVC to a different, already-existing StorageClass,
via the recipe's `inference-model-cache-storage-class` constraint or the
`AICR_INFERENCE_PERF_MODEL_CACHE_STORAGE_CLASS` catalog env (see
[Validation](validation.md)), doesn't stop AICR from also rendering
`a4x-compatible`. If a StorageClass named `a4x-compatible` already exists on the
cluster under someone else's ownership, adopting it into this release's
Helm lifecycle either fails the install or takes over an object this bundle
doesn't need. Opt out of rendering it at bundle time with:

```bash
aicr bundle --recipe recipes/overlays/gb200-gke-cos-inference-dynamo.yaml \
  --set dynamo-platform:a4xStorageClass.create=false \
  --output ./bundle
```

`a4xStorageClass.create` is a bundling-time toggle read by AICR itself, not
an `ai-dynamo` chart value. It never reaches the rendered Helm values.

### `gpu-operator` and `nvidia-dra-driver-gpu`: ComputeDomain CRD ownership on Argo CD

`gpu-operator` and `nvidia-dra-driver-gpu` (and `nvidia-dra-driver-gpu-ocp`) both
ship the `computedomains.resource.nvidia.com` CRD. As of `gpu-operator`
v26.7.0 the two chart copies disagree on schema (`spec.numNodes` required vs.
optional with a default), so on `--deployer argocd` and `--deployer
argocd-helm` — where every generated `Application` syncs with
`automated.selfHeal: true` — Argo CD perpetually reconciles the CRD toward
whichever `Application` last synced.

When a bundle pairs a standalone DRA driver with `gpu-operator` **v26.7.0 or
newer**, AICR scopes an `ignoreDifferences` entry to the divergent fields on
the `gpu-operator` `Application`, so the DRA driver's copy stays the effective
owner and the reconcile loop stops. This is generated automatically — no flag
or override is needed. Bundles with only one of the two components, or with a
`gpu-operator` older than v26.7.0 (whose chart ships no `computedomains` CRD
to contend with), are unaffected and carry no such entry.

**One side effect worth knowing about.** The entry is paired with the
`RespectIgnoreDifferences=true` sync option, without which Argo CD would
exclude the fields from its diff but still re-apply them on every sync. That
option is *Application-wide*, not per-entry: Argo builds the sync-time
normalizer from this `Application`'s `ignoreDifferences` **plus** any
`resource.customizations.ignoreDifferences.*` configured cluster-wide in
`argocd-cm`. So on an Argo instance carrying global ignore rules (webhook
`caBundle`, HPA-managed `replicas`, aggregated ClusterRole rules), those
fields also stop being enforced at sync time for the `gpu-operator`
`Application` specifically — drift in them is preserved rather than corrected
by `selfHeal`. Argo CD offers no way to scope the option to a single entry.

This is a stopgap, not a durable fix. `Helm` and `Flux` bundles are not
affected (both install CRDs once and never re-apply them), and the
OLM-based OCP path (`gpu-operator-ocp`, `gpu-operator-ocp-olm`) is not
covered — those components install no chart `crds/` of their own, so their
CRDs come from the OLM `Subscription`/CSV and an `Application`-level
`ignoreDifferences` has nothing to arbitrate. That conflict is tracked
separately. See [NVIDIA/aicr#2546](https://github.com/NVIDIA/aicr/issues/2546).

### `mariadb-operator`: `26.6.0` (or earlier) to `26.10.0`

`26.10.0` changes the replication configuration rendered by the MariaDB init
container and the replication liveness probe served by the agent, so the data
plane has to move with the operator. `updateStrategy.autoUpdateDataPlane`
defaults to `false`, so an operator upgraded without setting it first runs
`26.10.0` against init and agent containers left at the prior version.

The init container and agent are the HA data plane, so they exist only where
Galera or replication is enabled. AICR's accounting database ships as a single
non-HA instance (`galera.enabled: false`, `replicas: 1`), so steps 1 and 5
below are inert for it: the field is accepted and does nothing. They matter for
any HA `MariaDB` the same operator manages. List what you have with:

```bash
kubectl get mariadb -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.image,GALERA:.spec.galera.enabled,REPLICATION:.spec.replication.enabled
```

Fresh installs are unaffected. To migrate an existing cluster, set the flag
**before** the operator moves, then upgrade CRDs first and the operator second:

1. Enable data-plane auto-update on every **HA** MariaDB the operator manages.
   AICR's own accounting database is `mariadb` in namespace `slurm` and is not
   HA, so this is a no-op for it:
   ```bash
   kubectl patch mariadb mariadb -n slurm --type merge \
     -p '{"spec":{"updateStrategy":{"autoUpdateDataPlane":true}}}'
   ```
2. Upgrade `mariadb-operator-crds` to `26.10.0` **in place**. Confirm which
   namespace the existing release is in first, because Helm scopes a release
   by namespace: without `--namespace` the request lands in whatever namespace
   the kubeconfig context points at, `--install` does not find the existing
   release, and Helm installs a second one that then fights the first for
   ownership of the cluster-scoped CRDs. `mariadb-system` is the registry
   default and no overlay overrides it, but an inherited bundle may differ.
   ```bash
   helm list -A | grep mariadb-operator-crds
   helm upgrade --install mariadb-operator-crds \
     oci://ghcr.io/mariadb-operator/charts/mariadb-operator-crds \
     --version 26.10.0 --namespace mariadb-system
   ```
   Never `helm uninstall` the CRD chart: that deletes the CRDs and
   cascade-deletes every `MariaDB`, `User`, `Database` and `Grant` with them.
3. Upgrade `mariadb-operator` to `26.10.0` (re-run `install.sh`, `helmfile
   apply`, or sync the release).
4. For each HA MariaDB patched in step 1, wait for the roll to finish before
   continuing. The operator applies the new init and agent images
   asynchronously, and reverting the flag mid-update strands that resource on
   the old data-plane version against a `26.10.0` operator:
   ```bash
   kubectl wait mariadb <name> -n <namespace> \
     --for=condition=Updated=True --timeout=15m
   kubectl wait mariadb <name> -n <namespace> \
     --for=condition=Ready=True --timeout=15m
   ```
   Name each resource rather than passing `--all`. The `Updated` condition is
   absent until an update is triggered, and `kubectl wait` does not return
   early on an absent condition, so a blanket wait burns the full timeout on
   every instance that had no roll to do.
5. Return the flag to `false` so a later operator bump does not update the data
   plane unattended. If the field is managed in git, set it there.

The same release changes the operator's default server image to
`mariadb:12.3.3`. AICR pins `mariadb:11.8.8` in
`recipes/components/slurm-accounting-mariadb/values.yaml`, so a cluster bundled
from this recipe stays on `11.8.8`; the pin also puts the server image into the
rendered `MariaDB` resource, where the BOM records it. Any `MariaDB` that omits
`spec.image` takes the operator default and will move to a new MariaDB major
version on its next reconcile. Check with:

```bash
kubectl get mariadb -A \
  -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.image
```

A blank `IMAGE` column means that cluster takes the default.

### `agentgateway`: upgrading across breaking releases

AICR pins the `agentgateway` and `agentgateway-crds` charts in the component
registry, and a pin bump can cross upstream releases that document breaking
changes — to JWT claim enforcement, LLM token accounting, policy merging,
cross-namespace route delegation, managed API-key metadata, Istio identity,
Gateway API and `TCPRoute` handling, MCP guardrails, standalone auth, and image
base. Whether any of that reaches you depends entirely on which agentgateway
resources exist in your cluster, and the answer differs sharply between what
AICR generates and what you author yourself.

**AICR-generated bundles are unaffected.** A bundle creates exactly two
agentgateway resources: an `AgentgatewayParameters` that carries deployment and
service shape only, and the `inference-gateway` `Gateway`. It ships no
`AgentgatewayPolicy`, `AgentgatewayBackend`, `AgentgatewayModel`, or
`HTTPRoute` — so the breaking changes land on surface AICR never populates.

**Resources you author yourself are exposed**, and AICR can neither detect nor
migrate them. Before applying a bundle whose agentgateway pin moved, check
whether you have any:

```bash
(
  if ! kinds=$(kubectl api-resources --api-group=agentgateway.dev -o name); then
    echo "discovery incomplete — retry; do not read this as clear" >&2; exit 1
  fi
  if [ -z "$kinds" ]; then
    echo "no agentgateway.dev kinds registered — the chart is not installed" >&2; exit 0
  fi
  for kind in $kinds; do
    kubectl get "$kind" -A || { echo "listing $kind failed — retry" >&2; exit 1; }
  done
)
```

The kinds are discovered rather than named because the API group grows across
chart versions — `AgentgatewayModel` only exists from v1.4.0 — so naming them
would fail with `the server doesn't have a resource type` on exactly the older
pins whose operators most need to run this. The status checks matter for the
same reason: an unhealthy aggregated APIService (a down metrics-server or
custom-metrics adapter, and `prometheus-adapter` is in AICR's own component
set) makes `kubectl api-resources` exit non-zero, the substitution yields an
empty list, and the loop would silently report clean. Empty output *plus* an
error means retry, not clear.

Routes you authored live in the Gateway API group rather than
`agentgateway.dev`, so the sweep above does not see them — and they are exactly
what the cross-namespace route delegation change affects:

```bash
(
  kubectl get httproutes,grpcroutes -A \
    -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,PKIND:.spec.parentRefs[*].kind,PNS:.spec.parentRefs[*].namespace,PARENTS:.spec.parentRefs[*].name' \
    || { echo "route listing failed — retry" >&2; exit 1; }
)
```

Read the rows by their parent:

- `PKIND: Gateway` naming `inference-gateway` — a route you attached to the
  AICR gateway.
- `PKIND: HTTPRoute` with a `PNS` that differs from `NS` — cross-namespace
  route-to-route delegation. From v1.5.0 this requires a `ReferenceGrant` in
  the child's namespace authorizing the parent's namespace, where previously
  none was needed. Confirm `kubectl get referencegrants -A` covers each one
  before upgrading, or the delegation stops being accepted.
- `PKIND: HTTPRoute` with an empty `PNS` — same-namespace delegation, which the
  change does not affect. `parentRefs[].namespace` is optional and defaults to
  the route's own namespace, so empty is the same-namespace signal.

One caveat on the command: a route with several `parentRefs` where only some
set `namespace` will have its `PNS` column misalign, because JSONPath omits the
missing entries rather than padding them. Describe those routes individually
with `kubectl get <httproute-or-grpcroute> <name> -n <ns> -o yaml` rather than
trusting the columns.

AICR's own `AgentgatewayParameters` named `system-proxy` in
`agentgateway-system` is expected. If nothing else appears here, nothing you
authored in the `agentgateway.dev` group is affected — but that is not the
whole check. A cluster with no custom agentgateway resources and no routes can
still hold an `agentgateway` Gateway outside `agentgateway-system`, which stops
reconciling once the namespace list is scoped. Finish with the Gateway
inventory in
[which namespaces the controller may provision Gateways in](#agentgateway-which-namespaces-the-controller-may-provision-gateways-in)
before concluding the upgrade needs nothing from you. Anything else returned means read the upstream release notes
for every version between the old and new pin and validate off-production
first — a multi-version jump has to absorb every breaking change in between,
not just the newest one. Use the
[component version matrix](https://github.com/NVIDIA/aicr/blob/main/docs/user/component-version-matrix.md) to find which versions
those are.

One limit worth stating plainly: AICR CI exercises **fresh installs** of a
pinned chart, not in-place upgrades from an older pin. A green release
validates that the new version deploys and passes its health checks. It is not
an in-place upgrade certification.

### `agentgateway`: which namespaces the controller may provision Gateways in

From chart v1.5.0 the controller's write permissions are scoped by
`rbac.gatewayNamespaces`, and AICR sets it to `agentgateway-system` — the one
namespace it provisions the `inference-gateway` Gateway in. The chart's own
default is an empty list, which binds the write role (Deployments, DaemonSets,
Secrets, ServiceAccounts, ConfigMaps, Services, HPAs, PDBs) with a
*ClusterRoleBinding*, letting a network-facing controller write those objects
in every namespace on the cluster.

Scoping narrows that **write** reach to the namespaces you name, and nothing
else. Two things it does not contain. The controller's read role is a separate
`ClusterRole` bound cluster-wide regardless of this value, and it carries
`get`/`list`/`watch` on Secrets — so a scoped controller can still read every
Secret in the cluster. And the write role still grants `daemonsets`, so a
DaemonSet created in a permitted namespace still schedules pods onto every
node. Treat the controller as privileged rather than contained; scoping is
worth doing, but it is not the control that keeps it away from your Secrets.

The consequence for you is that **only Gateways in the listed namespaces are
provisioned**. A Gateway elsewhere is accepted by the API server but never gets
an address — the controller cannot create its Deployment or Service there. The
Gateway reports it: v1.5.0 sets `Programmed=False` with reason
`DeploymentFailed` when those writes are denied, so
`kubectl get gateway <name> -n <ns> -o yaml` shows the cause in `status.conditions`,
with matching `forbidden` errors in the controller log. This is
about where the *Gateway* lives; routes are unaffected, and `HTTPRoute`s in any
namespace still attach to the AICR gateway.

Upgrading an existing cluster is where this bites. A Gateway that reconciles
today under the chart's unscoped default stops once the list is scoped without
its namespace, so inventory what you have before upgrading:

```bash
(
  kubectl get gateways.gateway.networking.k8s.io -A \
    -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,CLASS:.spec.gatewayClassName' \
    || { echo "Gateway listing failed — retry" >&2; exit 1; }
)
```

Every namespace holding a Gateway with `CLASS: agentgateway` belongs in the
list. To add them:

```bash
aicr bundle -r recipe.yaml \
  --set-json agentgateway:rbac.gatewayNamespaces='["agentgateway-system","my-gateways"]'
```

Two constraints on that edit. The namespaces **must already exist** — the chart
creates a `RoleBinding` in each, and naming one that has not been created fails
the install. And the key is a list, so it needs `--set-json`: a plain
`--set agentgateway:rbac.gatewayNamespaces=my-gateways` writes a bare string and
the chart's `range` over it fails at render, the same list-versus-string trap
described for `allowedSourceRanges` above.

### `kueue`: 0.18.x to 0.19.x

Upstream's 0.19 notes ask you to review the `.0` notes for every minor version
you cross. Coming from AICR's previous pin of 0.18.2 that is discharged, so
0.19.0 is the floor here. If you are upgrading from an AICR release older than
the 0.18.2 pin, read the 0.17.0 and 0.18.0 notes as well.

One change alters behavior on a default install. The rest apply only if you
author Kueue objects yourself or have re-enabled an integration AICR trims.

**`WaitForPodsReady` is on by default from 0.19.0.** The v1beta2 configuration
has no enable switch for it: the controller defaults the block
unconditionally, so an existing install that never set `waitForPodsReady`
inherits it on upgrade. The effective values come from the Kueue binary rather
than from the commented example in the chart, and a running 0.19.3 controller
reports them as:

```yaml
waitForPodsReady:
  blockAdmission: false
  recoveryTimeout: 30m0s
  requeuingStrategy:
    backoffBaseSeconds: 60
    backoffMaxSeconds: 3600
    timestamp: Eviction
  timeout: 30m0s
```

What changes for a running cluster: a workload whose pods do not all become
ready inside 30 minutes is evicted and requeued instead of holding its quota,
and a running workload that loses readiness for 30 minutes is evicted the same
way. On 0.18.2 the first case held its GPU quota while never running, so for
most clusters this is the better behavior. `blockAdmission` stays false, so an
evicted workload does not stall the queue behind it.

AICR inherits this rather than pinning it. If 30 minutes is wrong for your
workloads, set a different timeout rather than trying to switch the feature
off: the `DisableWaitForPodsReady` feature gate is already deprecated upstream
and is slated for removal in 0.21. Note that AICR pins
`managerConfig.controllerManagerConfigYaml` as a single string, and Helm does
not merge into a string, so changing one key means supplying the whole block
rather than overriding `waitForPodsReady` on its own.

**Rename any device-class mapping or resource transformation named `pods`.**
Kueue reserves that exact resource name for the request it synthesizes from the
PodSet count, and refuses it in four positions: `resources.transformations[].input`,
that entry's `multiplyBy`, any key of its `outputs`, and
`resources.deviceClassMappings[].name` (the last additionally requires
`KueueDRAIntegration`, which has defaulted on since 0.18). Through 0.19.2 an entry
using it was accepted and then silently discarded, or left the Workload pending
indefinitely. 0.19.3 adds a real
refusal and holds it behind the alpha `ReservedResourceNameValidation` feature
gate, which is off by default in 0.19 so that existing clusters can rename
first.

Upstream's release note says the controller-manager "will fail to start", which
holds only once that gate is on. Checked against 0.19.3 on kind with
`resources.transformations[0].input: pods` in the configuration: at the default
gate setting the controller came up `1/1 Running` with 0 restarts, and adding
`--feature-gates=ReservedResourceNameValidation=true` to the same
configuration crash-looped it at startup with

```text
Unable to validate the configuration
resources.transformations[0].input: Invalid value: "pods": the key is reserved for internal kueue use
```

So a 0.19.3 upgrade does not break on this by itself. Rename anyway, while the
gate is still off. The check applies to the controller Configuration, not to
ClusterQueue `coveredResources`. AICR ships neither block, so a default install
has nothing to rename, but both are reachable through typed overrides on the
`kueue` component:

```bash
kubectl get configmap kueue-manager-config -n kueue-system \
  -o jsonpath='{.data.controller_manager_config\.yaml}' \
  | grep -nE "^[[:space:]]*-?[[:space:]]*(input|multiplyBy|name)[[:space:]]*:[[:space:]]*[\"']?pods[\"']?([[:space:]]+#.*)?[[:space:]]*\$|^[[:space:]]*[\"']?pods[\"']?[[:space:]]*:"
```

Helm renders this ConfigMap through `fromYaml | toYaml`, so a value written as
`input: "pods" # rename me` reaches it normalized to `input: pods`, quotes and
comment dropped. The pattern accepts quotes, a trailing comment and arbitrary
spacing anyway, so it still reports a hit against a ConfigMap that was applied
directly or installed by another tool and never passed through that
normalization.

No output means nothing to do. Rename any hit to a qualified name such as
`example.com/pods`. A rename also means updating the matching ClusterQueue
`nominalQuota` entries in the same change, because the quota is keyed on the
name you just changed.

**Workloads you author yourself are validated more strictly from 0.19.1.**
Topology-aware scheduling is on by default (`TopologyAwareScheduling` since
0.14) and so is the new check (`TASValidateWorkloadSliceSize` at 0.19), so this
needs no change in AICR to take effect. It reaches only `Workload` objects you
create directly or through your own controller, not those Kueue builds from a
Job. After upgrading, a Workload is rejected unless `podSetSliceRequiredTopology`
is paired with a `podSetSliceSize` greater than zero, `podSetSliceSize` is
absent when that topology field is absent, and every
`podsetSliceRequiredTopologyConstraints` entry has a positive size. For a phased
rollout, disable `TASValidateWorkloadSliceSize`, clean up the invalid Workloads,
then re-enable it. While you are in there, fix any negative `subGroupCount` on a
Workload: 0.19 only warns about it, but 0.20 rejects it at the API level.

**If you turned topology-aware scheduling off, the gate the release note names
is not enough.** `TASRecomputeAssignmentWithinSchedulingCycle` is new in 0.19
and defaults on, and the 0.19.1 note tells you to set it false before upgrading
when TAS is disabled. That is necessary and not sufficient. Checked against
0.19.3 on kind: with `TopologyAwareScheduling=false` alone the manager exits at
startup with `conflicting feature gates detected` and eight causes, and the
Deployment crash-loops. Applying the release note's instruction on top of that,
so `TASRecomputeAssignmentWithinSchedulingCycle=false` as well, still
crash-loops. Seven sub-gates default on and each requires TAS
(`TASHandleOverlappingFlavors`, `TASFailedNodeReplacement`,
`TASFailedNodeReplacementFailFast`, `TASReplaceNodeOnPodTermination`,
`TASReplaceNodeOnNodeTaints`, `TASMultiLayerTopology`,
`TASRecomputeAssignmentWithinSchedulingCycle`), and `TASProfileMixed`, on by
default since 0.15, fails on its own with `cannot use a TAS profile with TAS
disabled`. The manager reached `1/1 Running` only with all nine set false
together. AICR sets no feature gates for kueue, so a default install is
unaffected and stays unaffected. This reaches only a cluster that disabled TAS
through an override on the `kueue` component, in either
`controllerManager.featureGates` or a `featureGates:` block inside
`managerConfig.controllerManagerConfigYaml`; both were checked and fail
identically, and kueue rejects setting the two at once. Given the cost, the
cheaper path is to drop the override and leave TAS on.

**If you re-enabled an integration AICR trims, check these too.** AICR pins
`integrations.frameworks` to `batch/job`, JobSet and TrainJob, so the following
are inert on a default install and matter only if you added the framework back
through an override:

- `ray.io/raycluster`: the autoscaler sidecar is now counted against quota.
  Budget an extra 500m CPU and 512Mi memory per head pod, or whatever
  `spec.autoscalerOptions.resources` sets, or admission starts failing.
- `ray.io/rayjob` with `submissionMode: SidecarMode`: the submitter sidecar is
  now counted against quota. Budget an extra 500m CPU and 200Mi memory per head
  pod.
- `leaderworkerset.x-k8s.io/leaderworkerset`: `spec.leaderWorkerTemplate.size`
  is immutable while Kueue manages the LeaderWorkerSet. Recreate at the new size
  rather than resizing in place. `spec.replicas` stays mutable.

If you add MultiKueue through an override and use `locationType: Path`, the
kubeconfig has to be mounted into the `kueue-controller-manager` pod under
`/etc/multikueue/kubeconfigs`. Moving the file on a node is not enough, because
the path is resolved inside the controller's own filesystem.
`MultiKueueKubeConfigPathValidation` is alpha and off by default in 0.19 and
upstream expects to turn it on later, so prefer `locationType: Secret` or
`ClusterProfile` rather than taking that dependency.

### `nodewright-operator`: `v0.18.0` renames `Skyhook` to `NodeWright`

Upstream `v0.18.0` renames the `skyhook.nvidia.com/v1alpha1 Skyhook` API to
`nodewright.nvidia.com/v1alpha1 NodeWright`, moves `DeploymentPolicy` to the
same group, and shifts the on-node annotation, label and finalizer prefix. An
operator-side mirror migrates each existing object for you, but completion
status is then written **only** on the new kind: the attempt to mirror it back
to the legacy object fails in a reconcile conflict loop, so `Skyhook.status`
stays empty on a cluster where tuning has genuinely finished.

The legacy object also becomes **read-only**. The post-rename admission webhook
rejects any spec, `pause` or `disable` change to a `Skyhook`, so the first
attempt to alter tuning after the upgrade fails outright rather than the
cluster merely reporting a stale status. Deletions and identical re-applies are
still accepted, which is why a steady-state sync keeps working and the break
surfaces only on a real edit. Operate the `NodeWright` instead.

AICR resolves the served API group by discovery rather than assuming either
one, so a bundle validates against an operator from either side of the rename.
The runtime-required taint is read from the operator's own Deployment for the
same reason: `v0.18.0` moved its default key from `skyhook.nvidia.com` to
`nodewright.nvidia.com`, and a gate that assumed the old key passed silently
instead of waiting for the taint to clear.

Upstream's own account of the rename is
[`docs/getting-started/migration.md`](https://github.com/NVIDIA/nodewright/blob/main/docs/getting-started/migration.md),
which sets a hard operational prerequisite for the upgrade itself:
[every `Skyhook` must be `complete` with no nodes in progress](https://github.com/NVIDIA/nodewright/blob/main/docs/getting-started/migration.md#prerequisite-all-skyhooks-must-be-complete)
before the operator is upgraded. It is a requirement rather than a
recommendation — the migration relabels the operator's package and per-node
ConfigMaps so the post-rename operator adopts them, and that flow assumes no
in-flight package work to disrupt. `paused` and `disabled` objects are fine to
leave as they are. Check with:

```bash
kubectl get skyhooks.skyhook.nvidia.com \
  -o custom-columns=NAME:.metadata.name,STATUS:.status.status,INPROGRESS:.status.nodesInProgress
```

`aicr upgrade-check` reports both boundaries. The transition record at
`recipes/components/nodewright-operator/upgrades.yaml` describes `v0.18.0` —
the rename, the prerequisite above, and per-deployer steps — and `v0.19.0`
separately, which is `safe`: it changes when a drain is considered complete but
asks nothing of an operator on upgrade. Crossing the rename is `manual`
whatever you land on, and you do **not** have to stop at `v0.18.0` to get past
it:

```console
$ aicr upgrade-check --from old.yaml --to new.yaml --deployer helm
COMPONENT            FROM     TO       VERDICT  NOTES
nodewright-operator  v0.17.1  v0.18.0  manual   1 minor, 4 steps

$ aicr upgrade-check --from older.yaml --to newer.yaml --deployer helm
COMPONENT            FROM     TO       VERDICT  NOTES
nodewright-operator  v0.16.0  v0.19.0  manual   3 minors, 4 steps

$ aicr upgrade-check --from cur.yaml --to newer.yaml --deployer helm
COMPONENT            FROM     TO       VERDICT  NOTES
nodewright-operator  v0.18.0  v0.19.0  safe     1 minor, verified
```

`v0.19.0` also changes drain timing: an interrupt now begins roughly the
longest `terminationGracePeriodSeconds` on the node later than before, and
`spec.drainConfig.timeout` — which has no default — bounds time-to-drain rather
than time-to-accept-evictions. AICR's tuning CRs declare interrupts and set no
timeout, so an undrainable pod holds its node in `in_progress` without bound.
Set a timeout on the CRs you author if you need that wait bounded.

The legacy `Skyhook` group is removed upstream in `v0.20.0`, so the CRs AICR
ships under `nodewright-customizations` still need renaming before a pin at or
above that is reachable. Tracked in
[#2594](https://github.com/NVIDIA/aicr/issues/2594).
