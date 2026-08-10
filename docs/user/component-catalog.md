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
| **gke-nccl-tcpxo** | NCCL TCPxO network plugin for GKE. Provides optimized collective communication for multi-node GPU workloads on Google Kubernetes Engine. GKE-specific. | — |
| **aws-efa** | Device plugin for AWS Elastic Fabric Adapter. Enables low-latency networking on EKS clusters with EFA-capable instances. EKS-specific. | [AWS EFA K8s Device Plugin](https://github.com/aws/eks-charts) |
| **cert-manager** | Automates TLS certificate management. Required by several operators for webhook and API server certificates. | [cert-manager](https://github.com/cert-manager/cert-manager) |
| **gatekeeper** | Admission controller for Kubernetes. Enforces policies and governance across the cluster using OPA (Open Policy Agent) ConstraintTemplates and Constraints. | [Open Policy Agent Gatekeeper](https://github.com/open-policy-agent/gatekeeper) |
| **nodewright-operator** | OS-level node tuning and configuration management. Applies kernel parameters, sysctl settings, and system-level optimizations to nodes. | [Nodewright](https://github.com/nvidia/nodewright) |
| **nodewright-customizations** | Environment-specific node tuning profiles applied via Nodewright. Extends the operator with kernel params, hugepages, and other host-level configurations. | — |
| **nvsentinel** | GPU health monitoring and automated remediation. Detects GPU errors and can cordon or drain affected nodes. | [NVSentinel](https://github.com/NVIDIA/nvsentinel) |
| **nvidia-dra-driver-gpu** | Dynamic Resource Allocation (DRA) driver. Advertises devices via the Kubernetes `resource.k8s.io` API (`v1` on 1.34+, `v1beta1`/`v1beta2` on 1.32/1.33) — ComputeDomain/IMEX channels for MNNVL platforms, and optionally whole GPUs. Stock recipes disable whole-GPU DRA advertisement (`resources.gpus.enabled: false`) — the device plugin is the production default whole-GPU advertiser, and DRA whole-GPU allocation is an experimental recipe-level opt-in ([#1327](https://github.com/NVIDIA/aicr/issues/1327)). Whole-GPU DRA and the GPU Operator device plugin (`nvidia.com/gpu`) are mutually exclusive per node: recipe-backed validation rejects a configuration that enables both (at policy-resolution time — skipping validation bypasses the check), because the two allocators keep independent ledgers and concurrent advertisement can double-allocate the same physical GPUs (see the guidance in `recipes/components/nvidia-dra-driver-gpu/values.yaml`). See [AKS GPU Setup](../integrator/aks-gpu-setup.md#dynamic-resource-allocation-dra) for details. CLI alias: `dradriver`. | [NVIDIA DRA Driver](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu) |
| **prometheus-operator-crds** | Custom Resource Definitions for the prometheus-operator (`Alertmanager`, `AlertmanagerConfig`, `PodMonitor`, `Probe`, `Prometheus`, `PrometheusRule`, `ServiceMonitor`, `ThanosRuler`). Shipped as a separate release so the CRDs land before any chart that creates monitoring CRs; this breaks the helm-diff self-reference that otherwise blocks `helmfile apply` on a fresh cluster. | [prometheus-operator-crds](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus-operator-crds) |
| **kube-prometheus-stack** | Cluster monitoring: Prometheus, Grafana, Alertmanager, and node exporters. Provides GPU and cluster metrics collection and dashboards. CRDs are installed by the sibling `prometheus-operator-crds` release (this chart runs with `crds.enabled: false`). | [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts) |
| **prometheus-adapter** | Exposes custom metrics from Prometheus to the Kubernetes metrics API. Enables HPA scaling based on GPU utilization and other custom metrics. | [prometheus-adapter](https://github.com/kubernetes-sigs/prometheus-adapter) |
| **aws-ebs-csi-driver** | CSI driver for Amazon EBS volumes. Provides persistent storage for workloads on EKS. EKS-specific. **Cluster-wide default StorageClass:** AICR enables `defaultStorageClass.enabled`, so this component provisions a **cluster-default** gp3 StorageClass (`ebs-csi-default-sc`) on **every** EKS cluster that includes it — not just inference recipes; training overlays inherit it too. EKS ships no default SC of its own, so this makes dynamic provisioning (e.g. the inference-perf model cache) work zero-config. Two consequences to note: (1) if the cluster already has a default SC, Kubernetes treats multiple defaults as ambiguous — unset the other; (2) a PVC that previously failed-fast on "no default SC" will now silently bind gp3, which can mask a misconfiguration. | [AWS EBS CSI Driver](https://github.com/kubernetes-sigs/aws-ebs-csi-driver) |
| **k8s-ephemeral-storage-metrics** | Exports ephemeral storage usage metrics per pod. Useful for monitoring scratch space consumption on GPU nodes. | [k8s-ephemeral-storage-metrics](https://github.com/jmcgrath207/k8s-ephemeral-storage-metrics) |
| **kai-scheduler** | Gang scheduler with hierarchical queues and topology-aware placement; works with device-plugin (`nvidia.com/gpu`) and DRA GPU allocation alike. Ensures distributed training jobs land on nodes with optimal interconnect topology. | [KAI Scheduler](https://github.com/kai-scheduler/KAI-Scheduler) |
| **grove** | Pod lifecycle management for Dynamo inference platform. Installed as a standalone component. | [Grove](https://github.com/ai-dynamo/grove) |
| **dynamo-platform** | NVIDIA Dynamo inference serving platform with bundled CRDs. Distributed inference with KV-cache-aware routing, Dynamo request-plane traffic, a NATS-backed Kubernetes event plane for KV-cache events, and disaggregated prefill/decode. | [Dynamo](https://github.com/ai-dynamo/dynamo) |
| **agentgateway-crds** | Custom Resource Definitions for agentgateway (Kubernetes Gateway API implementation for AI/ML inference). | [agentgateway](https://github.com/agentgateway/agentgateway) |
| **agentgateway** | Kubernetes Gateway API implementation for AI/ML inference. Implements the Gateway API Inference Extension for model-aware ingress routing to InferencePool backends. | [agentgateway](https://github.com/agentgateway/agentgateway) |
| **k8s-nim-operator** | NVIDIA NIM Operator for managing NIM (NVIDIA Inference Microservices) deployments on Kubernetes. | [K8s NIM Operator](https://github.com/NVIDIA/k8s-nim-operator) |
| **kueue** | Kubernetes-native job queuing system. Manages quotas and admits jobs for batch and AI workloads. Ships default quota CRs (ResourceFlavor `default-flavor`, ClusterQueue `cluster-queue`, LocalQueue `default` in the `default` namespace) so admission works out of the box — tune the ClusterQueue's nominal quotas to cluster capacity to enact real limits. Managed frameworks are pinned to batch/job, JobSet, and TrainJob. Upgrade note: the quota CRs are helm post-install/post-upgrade hooks with a delete-and-recreate policy — quiesce queues before upgrading the bundle (Kueue's resource-in-use finalizer on an active ClusterQueue/ResourceFlavor blocks the delete and can wedge the upgrade), and re-apply tuned quotas afterwards since upgrades reset them to the shipped defaults. Uninstalling leaves the hook-created CRs behind; delete them manually when removing Kueue. Overlays that override the component's `manifestFiles` (replacing the default quota CRs) must also override its health check — the shipped check asserts the default CR names above. | [Kueue](https://github.com/kubernetes-sigs/kueue) |
| **kubeflow-trainer** | Kubeflow Training Operator for distributed training jobs (PyTorch, etc.). Manages multi-node training job lifecycle with JobSet integration. | [Kubeflow Trainer](https://github.com/kubeflow/trainer) |
| **mariadb-operator-crds** | Official MariaDB Operator CRDs. Declared in every Slurm recipe but installed only for `accounting.mode: aicr-provided`. | [MariaDB Operator](https://github.com/mariadb-operator/mariadb-operator) |
| **mariadb-operator** | Official MariaDB Operator controller, webhook, and certificate controller. AICR installs it only for `accounting.mode: aicr-provided`. | [MariaDB Operator](https://github.com/mariadb-operator/mariadb-operator) |
| **slurm-accounting-mariadb** | Installation-managed MariaDB instance whose initial database, all-privileges accounting user, and generated Secret reference are configured atomically on the MariaDB resource. Declared in every Slurm recipe and rendered only for `accounting.mode: aicr-provided`. | [MariaDB Cluster chart](https://artifacthub.io/packages/helm/mariadb-operator/mariadb-cluster) |
| **slinky-slurm-operator-crds** | Custom Resource Definitions for the SchedMD Slinky Slurm operator. Installs the `slinky.slurm.net` CRDs (Controller, NodeSet, LoginSet, Accounting, RestApi, Token). Installed separately to support CRD lifecycle management. | [Slinky Slurm Operator](https://github.com/SlinkyProject/slurm-operator) |
| **slinky-slurm-operator** | SchedMD Slinky Slurm operator and admission webhook. Manages the lifecycle of Slurm clusters declared via Slinky CRs (Controller, NodeSet, LoginSet, Accounting, RestApi, Token). AICR's system node-selector and toleration bundle flags apply to both deployments; affinity remains available through component values or typed overrides. | [Slinky Slurm Operator](https://github.com/SlinkyProject/slurm-operator) |
| **slinky-slurm** | Slinky-managed Slurm cluster instance: Controller (slurmctld) + LoginSet (sackd/sshd) + NodeSet (slurmd) + RestApi (slurmrestd), with SlurmDBD derived from the recipe's typed accounting mode. Reconciled by `slinky-slurm-operator`. See [Slurm Accounting](slinky-slurm-accounting.md) and [Slurm Shared Storage](slinky-slurm-storage.md). | [Slinky Slurm Cluster Chart](https://github.com/SlinkyProject/slurm-operator/tree/main/helm/slurm) |
| **slinky-topograph** | Slinky/Slurm-scoped instance of Topograph — queries cloud provider topology APIs (GCP, AWS, OCI …) to generate Slurm `topology.conf`, enabling topology-aware placement decisions in the Slinky-managed scheduler. **Not installed by default**; leaf overlays opt in by adding an explicit `componentRef` entry for `slinky-topograph` — the `componentRef` is what schedules the release; `dependencyRefs` alone does not install anything. That `componentRef` declares `slinky-slurm` as a `dependencyRef` to deploy **after** it: `slinky-slurm` renders and owns the `slinky-slurm-config-extra` ConfigMap (from its `configFiles`, mounted into slurmctld via the Controller CR's `configFileRefs`), and Topograph patches only that ConfigMap's `topology.conf` key on each sync, preserving the chart-owned `cgroup.conf`/`gres.conf` keys — Helm has to own the ConfigMap first. `TopologyPlugin: topology/tree` is set per-leaf via `slinky-slurm`'s `controller.extraConfMap`. Includes the `node-observer` sub-chart, which watches the topograph API pod and regenerates topology on restarts or selected node/pod changes. Requires cloud provider IAM access (e.g. GCP `roles/compute.viewer` for Workload Identity). | [Topograph](https://github.com/NVIDIA/topograph) |
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

The Topology Updater requires the kubelet `podResources` gRPC socket. The `KubeletPodResources` feature gate has been on by default since Kubernetes 1.15 (Beta) and reached GA in Kubernetes 1.28; AICR's recipe constraints on the affected leaves require K8s ≥ 1.30 or higher, so this is satisfied in practice. Recipes targeting Kubernetes `< 1.15` must enable the feature gate explicitly. Kind / KWOK simulated clusters do not run a real kubelet and therefore leave the Topology Updater disabled — kind-based recipes will not see `NodeResourceTopology` CRDs.

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
- **`driver-installer`** (`aicr recipe ... --profile gpuStack=driver-installer`) — the GPU Operator's device plugin is the sole advertiser (`devicePlugin.enabled: true`, profile-owned), and the constraint inverts: every GPU node must carry `gke-no-default-nvidia-gpu-device-plugin=true`. This value has real cluster prerequisites — the label on every GPU pool at creation **plus** Google's standalone `nvidia-driver-installer` DaemonSet, with pools created `gpu-driver-version=disabled`.

For the end-to-end setup flow (snapshot → recipe → validate), the qualification and selection-vs-verification matrices, and troubleshooting, see [GKE GPU Setup](../integrator/gke-gpu-setup.md).

Why exactly one advertiser: two plugins registering `nvidia.com/gpu` on one node is not a benign overlap. Kubelet's device manager keys its endpoint and device inventory by resource name, so competing registrations and `ListAndWatch` updates replace each other. Ownership becomes nondeterministic, and one plugin's device IDs (GKE uses `nvidia0`-style names, NVIDIA uses GPU UUIDs) can reach the other plugin's `Allocate`. Expect intermittent allocation and runtime failures.

**Driver-installer cluster setup.** The opt-out label forfeits GKE's managed driver install: the managed install (`gpu-driver-version=default`/`latest`) is finalized by an init container of the **same** kube-system DaemonSet the label disables, so a labeled pool paired with `gpu-driver-version=default` comes up **driverless** — never combine the label with the managed driver install. Pools for the `driver-installer` value must instead be created with `gpu-driver-version=disabled`, with driver provisioning supplied by Google's standalone [`nvidia-driver-installer` DaemonSet](https://cloud.google.com/kubernetes-engine/docs/how-to/gpus#installing_drivers) applied to the cluster. (AICR's GKE-COS overlays keep `driver.enabled: false` in either mode — the GPU Operator cannot install a driver on COS node images.) The operational procedures live in [GKE GPU Setup](../integrator/gke-gpu-setup.md#alternative-let-gpu-operator-manage-the-device-plugin): pool creation with the label, [retrofitting an existing pool](../integrator/gke-gpu-setup.md#retrofitting-an-existing-pool) (a `--node-labels` update replaces the pool's full label set), [verifying the handoff](../integrator/gke-gpu-setup.md#verifying-the-handoff) after the asynchronous plugin eviction, and [the three driver-installer settings](../integrator/gke-gpu-setup.md#the-three-driver-installer-settings) and what each covers.

**`aicr validate` enforces the selected value deterministically, before any phase runs.** The selected value's `NodeTopology.gpu-nodes.label` constraint ([#1755](https://github.com/NVIDIA/aicr/issues/1755)) is verified against the snapshot at recipe generation and re-evaluated by the validate readiness pre-flight. The check fails closed: labels contradicting the selected value, mixed labels, an empty GPU-node set, and readings that `--max-nodes-per-entry` actually truncated (a cap larger than the node count truncates nothing and validates normally) all fail with exit 2 and remediation text pointing back at this section, before any check Jobs deploy. See [Validation](validation.md) for the readiness-gate mechanics.

The profile also locks the ownership tuple: `devicePlugin.enabled` is profile-owned, and because both values govern advertisement, the #1327 allocation-policy paths (`devicePlugin.enabled`, DRA `resources.gpus.enabled` / `gpuResourcesEnabledOverride`) are closure-locked — a bundle- or install-time override diverging at any of them is rejected rather than warned. Switching modes is a recipe-generation decision (`--profile`), never a `--set`.

The selected constraint is the only deterministic detection point. `aicr bundle` is offline by design and cannot read node labels. The operator-health deployment check passes under the conflict because it verifies only that GPU Operator controller pods are Running — it never inspects the device plugin. Allocation probes such as `check-nvidia-smi` schedule a pod requesting `nvidia.com/gpu` on each schedulable GPU node, but skip cordoned nodes and skip entirely when any schedulable GPU node is busy; when they do run, they may fail nondeterministically without identifying the missing label as the cause.

See GKE's [GPU node-pool guide](https://cloud.google.com/kubernetes-engine/docs/how-to/gpus) for the authoritative pool-creation and driver-installer procedures. The [NVIDIA GPU Operator GKE guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/google-gke.html) documents the same `gpu-driver-version=disabled` + driver-installer DaemonSet combination the `driver-installer` value builds on. Shipping the installer as an AICR-managed component (the ADR-015 `operator-selfdriver` direction) is tracked separately in [#1716](https://github.com/NVIDIA/aicr/issues/1716).

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
