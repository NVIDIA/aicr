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
| **gke-nccl-tcpxo** | NCCL TCPXO network plugin for GKE. Provides optimized collective communication for multi-node GPU workloads on Google Kubernetes Engine. GKE-specific. | — |
| **gcp-driver-installer** | Google's cos-gpu-installer DaemonSet as an AICR-managed, values-gated component. Present in every GKE COS recipe; renders only under the `gpuStack=bundle-installer` profile value, where it installs the recipe-pinned NVIDIA driver on pools created with `gpu-driver-version=disabled`. GKE-specific. | — |
| **aws-efa** | Device plugin for AWS Elastic Fabric Adapter. Enables low-latency networking on EKS clusters with EFA-capable instances. EKS-specific. | [AWS EFA K8s Device Plugin](https://github.com/aws/eks-charts) |
| **cert-manager** | Automates TLS certificate management. Required by several operators for webhook and API server certificates. | [cert-manager](https://github.com/cert-manager/cert-manager) |
| **gatekeeper** | Admission controller for Kubernetes. Enforces policies and governance across the cluster using OPA (Open Policy Agent) ConstraintTemplates and Constraints. | [Open Policy Agent Gatekeeper](https://github.com/open-policy-agent/gatekeeper) |
| **nodewright-operator** | OS-level node tuning and configuration management. Applies kernel parameters, sysctl settings, and system-level optimizations to nodes. | [Nodewright](https://github.com/nvidia/nodewright) |
| **nodewright-customizations** | Environment-specific node tuning profiles applied via Nodewright. Extends the operator with kernel params, hugepages, and other host-level configurations. | — |
| **nvsentinel** | GPU health monitoring. Detects GPU errors and publishes health events; the components that cordon, drain, reboot or terminate a node are off by default — see [NVSentinel Deployment Posture](#nvsentinel-deployment-posture). On platforms where the provider installs the driver but no driver pod is observable by NVSentinel, the recipes set `labeler.assumeDriverInstalled` for you — see [NVSentinel on provider-installed-driver platforms](#nvsentinel-on-provider-installed-driver-platforms). | [NVSentinel](https://github.com/NVIDIA/nvsentinel) |
| **nvidia-dra-driver-gpu** | Dynamic Resource Allocation (DRA) driver. Advertises devices via the Kubernetes `resource.k8s.io` API (`v1` on 1.34+, `v1beta1`/`v1beta2` on 1.32/1.33) — ComputeDomain/IMEX channels for MNNVL platforms, and optionally whole GPUs. Stock recipes disable whole-GPU DRA advertisement (`resources.gpus.enabled: false`) — the device plugin is the production default whole-GPU advertiser, and DRA whole-GPU allocation is an experimental recipe-level opt-in ([#1327](https://github.com/NVIDIA/aicr/issues/1327)). Whole-GPU DRA and the GPU Operator device plugin (`nvidia.com/gpu`) are mutually exclusive per node: recipe-backed validation rejects a configuration that enables both (at policy-resolution time — skipping validation bypasses the check), because the two allocators keep independent ledgers and concurrent advertisement can double-allocate the same physical GPUs (see the guidance in `recipes/components/nvidia-dra-driver-gpu/values.yaml`). See [AKS GPU Setup](../integrator/aks-gpu-setup.md#dynamic-resource-allocation-dra) for details. CLI alias: `dradriver`. | [NVIDIA DRA Driver](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu) |
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
| **kueue** | Kubernetes-native job queuing system. Manages quotas and admits jobs for batch and AI workloads. Ships default quota CRs (ResourceFlavor `default-flavor`, ClusterQueue `cluster-queue`, LocalQueue `default` in the `default` namespace) so admission works out of the box — tune the ClusterQueue's nominal quotas to cluster capacity to enact real limits. Managed frameworks are pinned to batch/job, JobSet, and TrainJob. Upgrade note: the quota CRs are helm post-install/post-upgrade hooks with a delete-and-recreate policy — quiesce queues before upgrading the bundle (Kueue's resource-in-use finalizer on an active ClusterQueue/ResourceFlavor blocks the delete and can wedge the upgrade), and re-apply tuned quotas afterwards since upgrades reset them to the shipped defaults. Uninstalling leaves the hook-created CRs behind; delete them manually when removing Kueue. Overlays that override the component's `manifestFiles` (replacing the default quota CRs) must also override its health check — the shipped check asserts the default CR names above. | [Kueue](https://github.com/kubernetes-sigs/kueue) |
| **kubeflow-trainer** | Kubeflow Training Operator for distributed training jobs (PyTorch, etc.). Manages multi-node training job lifecycle with JobSet integration. | [Kubeflow Trainer](https://github.com/kubeflow/trainer) |
| **mariadb-operator-crds** | Official MariaDB Operator CRDs. Declared in every Slurm recipe but installed only for `accounting.mode: aicr-provided`. | [MariaDB Operator](https://github.com/mariadb-operator/mariadb-operator) |
| **mariadb-operator** | Official MariaDB Operator controller, webhook, and certificate controller. AICR installs it only for `accounting.mode: aicr-provided`. | [MariaDB Operator](https://github.com/mariadb-operator/mariadb-operator) |
| **slurm-accounting-mariadb** | Installation-managed MariaDB instance whose initial database, all-privileges accounting user, and generated Secret reference are configured atomically on the MariaDB resource. Declared in every Slurm recipe and rendered only for `accounting.mode: aicr-provided`. | [MariaDB Cluster chart](https://artifacthub.io/packages/helm/mariadb-operator/mariadb-cluster) |
| **slinky-slurm-operator-crds** | Custom Resource Definitions for the SchedMD Slinky Slurm operator. Installs the `slinky.slurm.net` CRDs (Controller, NodeSet, LoginSet, Accounting, RestApi, Token). Installed separately to support CRD lifecycle management. | [Slinky Slurm Operator](https://github.com/SlinkyProject/slurm-operator) |
| **slinky-slurm-operator** | SchedMD Slinky Slurm operator and admission webhook. Manages the lifecycle of Slurm clusters declared via Slinky CRs (Controller, NodeSet, LoginSet, Accounting, RestApi, Token). AICR's system node-selector and toleration bundle flags apply to both deployments; affinity remains available through component values or typed overrides. | [Slinky Slurm Operator](https://github.com/SlinkyProject/slurm-operator) |
| **slinky-slurm** | Slinky-managed Slurm cluster instance: Controller (slurmctld) + LoginSet (sackd/sshd) + NodeSet (slurmd) + RestApi (slurmrestd), with SlurmDBD derived from the recipe's typed accounting mode. Reconciled by `slinky-slurm-operator`. See [Slurm Accounting](slinky-slurm-accounting.md), [Slurm Enroot Configuration](slinky-slurm-enroot.md), and [Slurm Shared Storage](slinky-slurm-storage.md). | [Slinky Slurm Cluster Chart](https://github.com/SlinkyProject/slurm-operator/tree/main/helm/slurm) |
| **slinky-topograph** | Slinky/Slurm-scoped instance of Topograph — queries cloud provider topology APIs (GCP, AWS, OCI …) to generate Slurm `topology.conf`, enabling topology-aware placement decisions in the Slinky-managed scheduler. **Not installed by default**; leaf overlays opt in by adding an explicit `componentRef` entry for `slinky-topograph` — the `componentRef` is what schedules the release; `dependencyRefs` alone does not install anything. That `componentRef` declares `slinky-slurm` as a `dependencyRef` to deploy **after** it: `slinky-slurm` renders and owns the `slinky-slurm-config-extra` ConfigMap (from its `configFiles`, mounted into slurmctld via the Controller CR's `configFileRefs`), and Topograph patches only that ConfigMap's `topology.conf` key on each sync, preserving the chart-owned `cgroup.conf`/`gres.conf` keys — Helm has to own the ConfigMap first. `TopologyPlugin: topology/tree` is set per-leaf via `slinky-slurm`'s `controller.extraConfMap`. Includes the `node-observer` component, which watches the topograph API pod and regenerates topology on restarts or selected node/pod changes. Requires cloud provider IAM access (e.g. GCP `roles/compute.viewer` for Workload Identity). | [Topograph](https://github.com/NVIDIA/topograph) |
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

`recipes/components/nvsentinel/values.yaml` enables and disables no NVSentinel *component*. It carries deployment-shaping values only: `fullnameOverride`, tolerate-all scheduling so GPU-node DaemonSets land on tainted nodes, `networkPolicy.enabled: false` (the metrics policy otherwise blocks cert-manager webhook traffic in the same namespace — this is the one upstream default AICR overrides here), `platformConnector` resources, and `janitor-provider.csp.provider: generic`, which selects the reboot mechanism used *if* remediation is later enabled but does not enable it. Every component on/off default below is the chart's.

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

**The practical effect.** A stock AICR bundle surfaces GPU faults; it does not act on them. A node that needs a reboot is reported, not rebooted, and an operator intervenes. That is deliberate: `janitor` can reboot or terminate nodes, and enabling it without the operator having chosen to is not a safe default.

### Enabling Remediation

**AICR does not support enabling remediation today, and this page does not carry a recipe for it.** [#1014](https://github.com/NVIDIA/aicr/issues/1014) tracks adding a qualified opt-in path.

Turning the components on is not a matter of flipping the six `enabled` flags. Those flags start the pipeline but leave `fault-remediation.maintenance.actions` at the subchart defaults, where `COMPONENT_RESET` maps to `kind: RebootNode` — so a recoverable GPU fault cordons, drains and reboots the whole node. Upstream's own remediation configuration maps that same action to `kind: GPUReset`, scoped to the affected GPU UUID, and resets in place instead. A partial enablement is therefore not a milder version of remediation; it is a more destructive one.

If you need remediation before #1014 lands, start from the chart's self-contained `values-remediation.yaml` (shipped inside the `nvsentinel` chart, pinned at the version in `recipes/registry.yaml`) rather than composing `--set` flags, and qualify the result on a cluster you can afford to have rebooted. Three further things apply whatever path you take:

- **The reboot path is privileged.** AICR pins `janitor-provider.csp.provider: generic`, so remediation reboots run as a privileged Job executing `chroot /host /sbin/reboot` on the target node. That avoids requiring cloud IAM credentials, but it is a broad grant. Cloud providers are selectable instead, and need the corresponding credentials.
- **The datastore is a real dependency.** `mongodbStore` deploys an in-cluster database. The chart also supports an external datastore and a `postgresql` provider; see the `global.datastore` block in the chart's values.
- **Check arm64 before enabling on ARM.** The chart's default MongoDB image has no `linux/arm64` manifest. arm64 works via the Percona path ([NVIDIA/NVSentinel#1328](https://github.com/NVIDIA/NVSentinel/issues/1328)).

**NVSentinel is included by default, not universally required.** `recipes/overlays/base.yaml` includes it unconditionally, so every recipe carries it unless something later removes it, and ADR-018 classifies it as `core` rather than `ops` on the rule that `ops` must be GPU-free and verifiable on CPU-only clusters. Two things can still remove it: an overlay that overrides `enabled` (the OCP overlay does), and a bundle-time exclusion on a platform whose presence is not profile-locked (EKS and OKE). Only the AKS and GKE-COS `gpuStack` profiles make its presence mandatory — see [NVSentinel is mandatory on the profiled families](#nvsentinel-on-provider-installed-driver-platforms) below.

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
the chart version pinned in the registry. It matters because Helm and Helmfile
skip a chart's `crds/` directory on upgrade, so a cluster can run a new
controller against the previous schema while the older version stays served
and the controller keeps working.

Flux is the exception for this component: `k8s-aibom` is marked `ownsCRDs` in
the registry, so its generated `HelmRelease` sets
`spec.upgrade.crds: CreateReplace` and Flux applies the CRDs itself. Argo CD
applies them as ordinary manifests each sync. The assertion is still worth
making on every deployer, because it proves the deployed CRDs match the pinned
chart rather than merely that some deployer was expected to update them.

Both CRDs are asserted separately, so a failure names which one is stranded
and a partially applied CRD set cannot pass. If this check fails after a chart
bump, the pre-upgrade CRD step in
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

**Apply CRDs before the bundle upgrade — `helm` and `helmfile` only.** The
chart ships its CRDs under `crds/`. Helm installs that directory on first
install and never touches it again on upgrade, so a chart bump whose CRDs
changed leaves the previous schema in place and the API server silently prunes
the new controller's writes to added fields.

The `flux`, `argocd`, and `argocd-helm` bundles handle this themselves for this
component and need no manual step; see the deployer table below. For `helm` and
`helmfile`, apply the CRDs from the exact qualified chart first, then upgrade:

```bash
CHART="oci://ghcr.io/googlecloudplatform/charts/k8s-aibom"
VERSION="1.3.0"   # replace with the version you are upgrading to

helm show crds "${CHART}" --version "${VERSION}" \
  | sed -n '/^---$/,$p' \
  | kubectl apply --server-side --force-conflicts -f -
```

Three details in that command are load-bearing. The obvious shorter form —
piping `helm show crds` straight into `kubectl apply --server-side` — fails on
the first two:

- **`sed -n '/^---$/,$p'`** drops `helm`'s progress output. For an OCI chart,
  `helm show crds` writes `Pulled:` and `Digest:` lines to *stdout*, and those
  two lines parse as a valid YAML mapping, so `kubectl` rejects the stream with
  `error validating data: [apiVersion not set, kind not set]`.
- **`--force-conflicts`** is required because Helm created these CRDs on
  install and owns their fields. Without it, server-side apply refuses with a
  field-manager conflict.
- **`--server-side`** is required because the CRDs exceed the annotation size
  limit that client-side apply depends on.

Verified against a live GKE cluster across a 1.2.0 to 1.3.0 upgrade.

Which deployers need that step differs, so check yours:

| Deployer | CRD behavior on upgrade | Pre-upgrade step needed |
|---|---|---|
| `helm` | `helm upgrade` skips `crds/` | Yes |
| `helmfile` | `helmfile apply` upgrades through Helm, so it also skips `crds/` | Yes |
| `flux` | The generated `HelmRelease` sets `spec.upgrade.crds: CreateReplace` for components the registry marks `ownsCRDs`, and leaves the helm-controller `Skip` default in place for the rest | Only for components without `ownsCRDs` |
| `argocd`, `argocd-helm` | Argo CD renders the chart with CRDs included and applies them as ordinary manifests each sync | No |

`ownsCRDs` is opt-in, and narrow on purpose. Of the 15 registry components
that ship CRDs under `crds/`, 11 share at least one CRD with another
component: `nfd`, `gpu-operator`, and `network-operator` all ship the
NodeFeature CRDs, and `nfd`, `gpu-operator`, and `kai-scheduler` all appear
together in `base.yaml`. If every release replaced CRDs on upgrade, two or
three `HelmRelease` objects would rewrite the same CRD on every reconcile,
each with the schema its own chart pins. The `Skip` default is what prevents
that today, so it stays the default.

A component qualifies only if it solely owns every CRD it ships and ships none
using `spec.conversion.strategy: Webhook`, since replace discards a `caBundle`
injected at runtime. `kubeflow-trainer` is excluded for that second reason.
Currently `gatekeeper`, `k8s-aibom`, and `nvsentinel` qualify.

`helm` and `helmfile` always need the step, because skipping `crds/` on
upgrade is Helm's own behavior rather than something the generated bundle can
change.

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
