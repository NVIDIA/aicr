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
| **nfd** | Node Feature Discovery. Labels nodes with hardware features (PCI device IDs, kernel modules, CPU capabilities). Both gpu-operator and network-operator consume these labels. Uninstalling nfd leaves its node labels in place because the chart's post-delete prune hook is disabled (`postDeleteCleanup: false`), so the GPU, network and DRA DaemonSets that select on them keep running. To clear them when retiring nfd for good, see [NFD Permanent Removal](#nfd-permanent-removal). On production GPU recipes, the Topology Updater publishes per-node `NodeResourceTopology` CRDs describing NUMA zones and GPU/NIC affinity for downstream NUMA-aware schedulers. | [Node Feature Discovery](https://github.com/kubernetes-sigs/node-feature-discovery) |
| **node-problem-detector** | Detects node-level faults the GPU stack does not watch — XFS shutdown, fatal UEFI CPER hardware errors, a read-only root filesystem — and publishes them as Node Conditions for NVSentinel's Object Monitor to consume. Opt-in via the `npd` mixin, and only where the platform does not already run its own. | [node-problem-detector](https://github.com/kubernetes/node-problem-detector) |
| **dranet** | DRA network driver ([kubernetes-sigs/dranet](https://github.com/kubernetes-sigs/dranet)) for the ConnectX-9 RDMA fabric on VR200 (Vera Rubin NVL72) RKE2 clusters. Publishes the mlx5 NICs as `ResourceSlice`s (driver `dra.net`) and injects them into pods via NRI under `DeviceClass` `mlnx-cx9`. Manifest-only (vendored `dranet.yaml`), evaluated as a lean alternative to `network-operator`. Ships only in the VR200 RKE2 overlays, which pair it with host RDMA "exclusive" netns mode (`ib_core` `netns_mode=0`, applied by `nodewright-customizations` with `rdmaNetnsExclusive: true` and a node reboot) so a pod sees only its DRA-allocated HCA. | [DraNet](https://github.com/kubernetes-sigs/dranet) |
| **gke-nccl-tcpxo** | NCCL TCPXO network plugin for GKE. Provides optimized collective communication for multi-node GPU workloads on Google Kubernetes Engine. GKE-specific. | — |
| **gke-gb200-rdma** | NCCL gIB (GPUDirect-RDMA over RoCE) plugin installer for GB200 (A4X, ARM64) GKE nodes. Installs RDMA binaries and the NCCL library so workloads select `NCCL_NET=gIB` over the cluster's `gvnic-1`/`rdma-0..rdma-3` `Network` objects. GKE-specific — see [GKE GB200 Networking](../integrator/gke-gb200-networking.md). | — |
| **gcp-driver-installer** | Google's cos-gpu-installer DaemonSet as an AICR-managed, values-gated component. Present in every GKE COS recipe; renders only under the `gpuStack=bundle-installer` profile value, where it installs the recipe-pinned NVIDIA driver on pools created with `gpu-driver-version=disabled`. GKE-specific. | — |
| **aws-efa** | Device plugin for AWS Elastic Fabric Adapter. Enables low-latency networking on EKS clusters with EFA-capable instances. EKS-specific. | [AWS EFA K8s Device Plugin](https://github.com/aws/eks-charts) |
| **cert-manager** | Automates TLS certificate management. Required by several operators for webhook and API server certificates. | [cert-manager](https://github.com/cert-manager/cert-manager) |
| **gatekeeper** | Admission controller for Kubernetes. Enforces policies and governance across the cluster using OPA (Open Policy Agent) ConstraintTemplates and Constraints. | [Open Policy Agent Gatekeeper](https://github.com/open-policy-agent/gatekeeper) |
| **nodewright-operator** | OS-level node tuning and configuration management. Applies kernel parameters, sysctl settings, and system-level optimizations to nodes. `v0.18.0` renamed the `Skyhook` API to `NodeWright`; crossing that boundary needs operator steps — see [Upgrade Notes](#nodewright-operator-v0180-renames-skyhook-to-nodewright) below. | [Nodewright](https://github.com/NVIDIA/nodewright) |
| **nodewright-customizations** | Environment-specific node tuning profiles applied via Nodewright. Extends the operator with kernel params, hugepages, and other host-level configurations. | — |
| **nvsentinel** | GPU health monitoring. Detects GPU errors and publishes health events; the components that cordon, drain, reboot or terminate a node are off by default — see [NVSentinel Deployment Posture](#nvsentinel-deployment-posture). On platforms where the provider installs the driver but no driver pod is observable by NVSentinel, the recipes set `labeler.assumeDriverInstalled` for you — see [NVSentinel on provider-installed-driver platforms](#nvsentinel-on-provider-installed-driver-platforms). | [NVSentinel](https://github.com/NVIDIA/nvsentinel) |
| **psmdb-operator** | Percona Operator for MongoDB, reconciling `nvsentinel-mongodb`. Watches only the `nvsentinel` namespace; sole owner of the `psmdb.percona.com` CRDs. Added only by the NVSentinel [remediation step mixins](#graded-remediation). CLI alias: `psmdboperator`. | [Percona Operator for MongoDB](https://github.com/percona/percona-server-mongodb-operator) |
| **nvsentinel-mongodb** | NVSentinel's health-event datastore: a three-member Percona Server for MongoDB replica set with TLS required and x509 client auth. Added only by the NVSentinel [remediation step mixins](#graded-remediation); requires `--system-node-selector`. CLI alias: `nvsentinelmongodb`. | [Percona Helm charts](https://github.com/percona/percona-helm-charts) |
| **slinky-drainer** | NVSentinel's slinky-drainer plugin: drains a quarantined Slurm node through Slurm by reconciling the `DrainRequest`s `node-drainer` creates, instead of evicting the slurmd pod. Added only by the opt-in `nvsentinel-slinky-drainer` mixin; see [Slurm Drain Through Slinky](#slurm-drain-through-slinky). CLI alias: `slinkydrainer`. | [NVSentinel](https://github.com/NVIDIA/NVSentinel/tree/v1.28.0/plugins/slinky-drainer) |
| **slinky-drainer-crds** | The `drainrequests.nvsentinel.nvidia.com` CRD that `node-drainer` and the slinky-drainer plugin need at startup, as its own release so deployers wait for it to be Established before either starts. Added only by the opt-in `nvsentinel-slinky-drainer` mixin. CLI alias: `slinkydrainercrds`. | [NVSentinel](https://github.com/NVIDIA/NVSentinel/tree/v1.28.0/plugins/slinky-drainer/config/crd) |
| **nvidia-dra-driver-gpu** | Dynamic Resource Allocation (DRA) driver. Advertises devices via the Kubernetes `resource.k8s.io` API (`v1` on 1.34+, `v1beta1`/`v1beta2` on 1.32/1.33) — ComputeDomain/IMEX channels for MNNVL platforms, and optionally whole GPUs. Stock recipes disable whole-GPU DRA advertisement (`resources.gpus.enabled: false`) — the device plugin is the production default whole-GPU advertiser, and DRA whole-GPU allocation is an experimental recipe-level opt-in ([#1327](https://github.com/NVIDIA/aicr/issues/1327)). Whole-GPU DRA and the GPU Operator device plugin (`nvidia.com/gpu`) are mutually exclusive per node: recipe-backed validation rejects a configuration that enables both (at policy-resolution time — skipping validation bypasses the check), because the two allocators keep independent ledgers and concurrent advertisement can double-allocate the same physical GPUs (see the guidance in `recipes/components/nvidia-dra-driver-gpu/values.yaml`). See [AKS GPU Setup](../integrator/aks-gpu-setup.md#dynamic-resource-allocation-dra) for details. CLI alias: `dradriver`. | [NVIDIA DRA Driver](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu) |
| **dra-node-labeler** | Applies the DRA eviction node label (`--dra-eviction-node-label`) to every node GFD reports as `nvidia.com/gpu.present=true`, so the DRA kubelet plugin's node selector and GPU Operator's Driver Manager eviction hook need no node-pool labeling. Write-once: an existing value, including the Driver Manager's `paused-for-driver-upgrade`, is never rewritten. Present in the bundle only when the eviction label is configured. | — |
| **prometheus-operator-crds** | Custom Resource Definitions for the prometheus-operator (`Alertmanager`, `AlertmanagerConfig`, `PodMonitor`, `Probe`, `Prometheus`, `PrometheusRule`, `ServiceMonitor`, `ThanosRuler`). Shipped as a separate release so the CRDs land before any chart that creates monitoring CRs; this breaks the helm-diff self-reference that otherwise blocks `helmfile apply` on a fresh cluster. | [prometheus-operator-crds](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus-operator-crds) |
| **kube-prometheus-stack** | Cluster monitoring: Prometheus, Grafana, Alertmanager, and node exporters. Provides GPU and cluster metrics collection and dashboards. CRDs are installed by the sibling `prometheus-operator-crds` release (this chart runs with `crds.enabled: false`). Prometheus selects every ServiceMonitor and PodMonitor in every namespace, with no `release` label required. Most components whose charts offer a monitor ship it enabled (NVCRE keeps its ServiceMonitor off because it is on no shipped overlay), and each one that renders its monitor at install declares a dependency on `prometheus-operator-crds`. | [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts) |
| **prometheus-adapter** | Exposes custom metrics from Prometheus to the Kubernetes metrics API. Enables HPA scaling based on GPU utilization and other custom metrics. | [prometheus-adapter](https://github.com/kubernetes-sigs/prometheus-adapter) |
| **aws-ebs-csi-driver** | CSI driver for Amazon EBS volumes. Provides persistent storage for workloads on EKS. EKS-specific. **Cluster-wide default StorageClass:** AICR enables `defaultStorageClass.enabled`, so this component provisions a **cluster-default** gp3 StorageClass (`ebs-csi-default-sc`) on **every** EKS cluster that includes it — not just inference recipes; training overlays inherit it too. EKS ships no default SC of its own, so this makes dynamic provisioning (e.g. the inference-perf model cache) work without naming a class. **The driver still needs AWS credentials, which AICR does not configure** — see [EBS CSI Driver Credentials](#ebs-csi-driver-credentials). Two consequences to note: (1) if the cluster already has a default SC, Kubernetes treats multiple defaults as ambiguous — unset the other; (2) a PVC that previously failed-fast on "no default SC" will now silently bind gp3, which can mask a misconfiguration. | [AWS EBS CSI Driver](https://github.com/kubernetes-sigs/aws-ebs-csi-driver) |
| **k8s-ephemeral-storage-metrics** | Exports ephemeral storage usage metrics per pod. Useful for monitoring scratch space consumption on GPU nodes. | [k8s-ephemeral-storage-metrics](https://github.com/jmcgrath207/k8s-ephemeral-storage-metrics) |
| **k8s-aibom** | Optional runtime AI workload inventory. Produces namespace-scoped CycloneDX 1.6 ML-BOM resources for explicitly opted-in namespaces. Ships by default in two stock recipes, `h100-gke-cos-inference` and its Dynamo descendant `h100-gke-cos-inference-dynamo`; available by opt-in on any other GKE recipe with `aicr recipe --runtime-inventory enabled`, including the `b200`, `gb200`, and `gb300` GKE Dynamo recipes. On the two recipes that ship it by default, decline it with `aicr recipe --runtime-inventory disabled`. CLI aliases: `k8saibom`, `aibom`. See [k8s-aibom Runtime Inventory](#k8s-aibom-runtime-inventory). | [k8s-aibom](https://github.com/GoogleCloudPlatform/k8s-aibom) |
| **kai-scheduler** | Gang scheduler with hierarchical queues and topology-aware placement; works with device-plugin (`nvidia.com/gpu`) and DRA GPU allocation alike. Ensures distributed training jobs land on nodes with optimal interconnect topology. AICR pins `defaultQueue.createDefaultQueue: true`, so the chart creates the `default-parent-queue`/`default-queue` hierarchy on install. The `gang-scheduling` conformance check submits its synthetic test PodGroup to `default-queue` by name, so that queue is a hard dependency of validation, not an optional extra. Note the chart creates the queues only on first install and annotates them `helm.sh/resource-policy: keep` — a `helm upgrade` will not recreate them if they are deleted, so restore them manually (or reinstall the release) if that happens. Workloads are not restricted to this queue: Dynamo submits to its own `dynamo`/`dynamo-default` hierarchy, which its chart creates via post-install and post-upgrade hooks. | [KAI Scheduler](https://github.com/kai-scheduler/KAI-Scheduler) |
| **grove** | Pod lifecycle management for Dynamo inference platform. Installed as a standalone component. Upgrading from `v0.1.0-alpha.8` (or earlier) requires a CRD migration step — see [Upgrade Notes](#grove-v010-alpha8-or-earlier-to-v010-alpha12) below. Upgrading from `v0.1.0-alpha.12` requires a CRD apply — see [Upgrade Notes](#grove-v010-alpha12-to-v010-alpha13) below. | [Grove](https://github.com/ai-dynamo/grove) |
| **dynamo-platform** | NVIDIA Dynamo inference serving platform with bundled CRDs. Distributed inference with KV-cache-aware routing, Dynamo request-plane traffic, a ZMQ-based KV-cache event plane, and disaggregated prefill/decode. | [Dynamo](https://github.com/ai-dynamo/dynamo) |
| **agentgateway-crds** | Custom Resource Definitions for agentgateway (Kubernetes Gateway API implementation for AI/ML inference). | [agentgateway](https://github.com/agentgateway/agentgateway) |
| **agentgateway** | Kubernetes Gateway API implementation for AI/ML inference. Implements the Gateway API Inference Extension for model-aware ingress routing to InferencePool backends. | [agentgateway](https://github.com/agentgateway/agentgateway) |
| **k8s-nim-operator** | NVIDIA NIM Operator for managing NIM (NVIDIA Inference Microservices) deployments on Kubernetes. AICR installs the operator only — it creates no `NIMService` and no credentials; see [NIM workload credentials](#nim-workload-credentials). | [K8s NIM Operator](https://github.com/NVIDIA/k8s-nim-operator) |
| **kueue** | Kubernetes-native job queuing system. Manages quotas and admits jobs for batch and AI workloads. Ships default quota CRs (ResourceFlavor `default-flavor`, ClusterQueue `cluster-queue`, LocalQueue `default` in the `default` namespace) so admission works out of the box — tune the ClusterQueue's nominal quotas to cluster capacity to enact real limits. Managed frameworks are pinned to batch/job, JobSet, and TrainJob. Upgrade note: the quota CRs are helm post-install/post-upgrade hooks with a delete-and-recreate policy — quiesce queues before upgrading the bundle (Kueue's resource-in-use finalizer on an active ClusterQueue/ResourceFlavor blocks the delete and can wedge the upgrade), and re-apply tuned quotas afterwards since upgrades reset them to the shipped defaults. Uninstalling leaves the hook-created CRs behind; delete them manually when removing Kueue. Overlays that override the component's `manifestFiles` (replacing the default quota CRs) must also override its health check — the shipped check asserts the default CR names above. The `platform=kueue` leaves (`h100-kind-training-kueue`, `h100-gke-cos-training-kueue`, `h100-eks-ubuntu-training-kueue`) do exactly that to enable topology-aware scheduling; see [Topology-aware scheduling](#topology-aware-scheduling-with-kueue-and-topograph). Upgrading from a 0.18.x bundle needs two checks first: see [Upgrade Notes](#kueue-018x-to-019x) below. | [Kueue](https://github.com/kubernetes-sigs/kueue) |
| **kubeflow-trainer** | Kubeflow Training Operator for distributed training jobs (PyTorch, etc.). Manages multi-node training job lifecycle with JobSet integration. | [Kubeflow Trainer](https://github.com/kubeflow/trainer) |
| **nvcre** | NVIDIA Cluster Readiness Engine, a GPU cluster burn-in certification controller. Runs training and NCCL workloads, measures goodput and bandwidth. **Not installed by default.** Enabling it takes both a `valuesFile` and a Trainer source. See [Enabling NVCRE](#enabling-nvcre) for a fragment that resolves. With that values file referenced, `metrics.serviceMonitor.enabled` is **false** so install does not require prometheus-operator CRDs. Turn it on with `--set cre:metrics.serviceMonitor.enabled=true` only after those CRDs exist, and add `prometheus-operator-crds` to `dependencyRefs`. The chart has no manager `nodeSelector`. For hard placement, set `manager.affinity` in `recipes/components/nvcre/values.yaml` or a complete JSON object, for example `--set-json cre:manager.affinity='{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"nvidia.com/gpu.present","operator":"Exists"}]}]}}}'` (scalar `--set cre:manager.affinity=...` renders an invalid string). CLI aliases: `cre`, `cluster-readiness-engine`. Shipped EKS H100 training still uses the TrainJob NCCL check. Opt-in AICR validators drive CRE with `Certification` (create, wait, delete), not `WorkloadRun`. **Disconnected installs.** The certification workloads pull images the chart does not render, because the workload catalog is compiled into the manager binary. Mirror the set in `recipes/components/nvcre/workload-images.yaml`, regenerated with `make nvcre-closure`. Each image is listed by tag with the digest that tag resolved to when the file was generated, and `sourceCommit` records the upstream commit it was derived from. The workloads pull by tag, so the mirror has to serve each tag, and serving it at the recorded digest keeps the cluster on the bytes the file describes. That committed set is scoped, not universal. It resolves the catalog for `platform: aws`, `gpuArchitecture: h100`, and the two certification entries `communication/nccl-all-reduce` and `training/nemotron5-8b`. The NCCL image it names is an EFA build, so another platform or accelerator pulls a different set and mirroring this one leaves those images unmirrored. Derive the set you need instead of reusing this one, writing it somewhere other than the committed path, for example `go run ./tools/nvcre-closure -platform gcp -arch h100 -entries communication/nccl-all-reduce -out /tmp/nvcre-gcp.yaml`. The training path additionally needs Megatron-LM at pod start, and the clone recorded in the closure is its last resort. The init container uses an already-populated workspace if one is mounted, else copies from `/opt/megatron-lm` in the image, and only clones when neither is there. So it can run disconnected, but only once you have provided the source by one of those two routes. The NCCL path needs nothing beyond the mirrored images. | [Cluster Readiness Engine](https://github.com/NVIDIA/cluster-readiness-engine) |
| **mariadb-operator-crds** | Official MariaDB Operator CRDs. Declared in every Slurm recipe but installed only for `accounting.mode: aicr-provided`. | [MariaDB Operator](https://github.com/mariadb-operator/mariadb-operator) |
| **mariadb-operator** | Official MariaDB Operator controller, webhook, and certificate controller. AICR installs it only for `accounting.mode: aicr-provided`. | [MariaDB Operator](https://github.com/mariadb-operator/mariadb-operator) |
| **slurm-accounting-mariadb** | Installation-managed MariaDB instance whose initial database, all-privileges accounting user, and generated Secret reference are configured atomically on the MariaDB resource. Declared in every Slurm recipe and rendered only for `accounting.mode: aicr-provided`. | [MariaDB Cluster chart](https://artifacthub.io/packages/helm/mariadb-operator/mariadb-cluster) |
| **slinky-slurm-operator-crds** | Custom Resource Definitions for the SchedMD Slinky Slurm operator. Installs the `slinky.slurm.net` CRDs (Controller, NodeSet, LoginSet, Accounting, RestApi, Token). Installed separately to support CRD lifecycle management. | [Slinky Slurm Operator](https://github.com/SlinkyProject/slurm-operator) |
| **slinky-slurm-operator** | SchedMD Slinky Slurm operator and admission webhook. Manages the lifecycle of Slurm clusters declared via Slinky CRs (Controller, NodeSet, LoginSet, Accounting, RestApi, Token). AICR's system node-selector and toleration bundle flags apply to both deployments; affinity remains available through component values or typed overrides. | [Slinky Slurm Operator](https://github.com/SlinkyProject/slurm-operator) |
| **slinky-slurm** | Slinky-managed Slurm cluster instance: Controller (slurmctld) + LoginSet (sackd/sshd) + NodeSet (slurmd) + RestApi (slurmrestd), with SlurmDBD derived from the recipe's typed accounting mode. Reconciled by `slinky-slurm-operator`. See [Slurm Accounting](slinky-slurm-accounting.md), [Slurm Enroot Configuration](slinky-slurm-enroot.md), and [Slurm Shared Storage](slinky-slurm-storage.md). | [Slinky Slurm Cluster Chart](https://github.com/SlinkyProject/slurm-operator/tree/main/helm/slurm) |
| **topograph** | Kubernetes-scoped Topograph: the `k8s` engine writes network-topology node labels (`fabric.topograph.run/tier-N`, closest switch tier first, and `accelerator.topograph.run/domain`) that topology-aware schedulers such as Kueue TAS read. Separate from `slinky-topograph`, which patches a Slurm `topology.conf` and depends on `slinky-slurm`; shares its chart pin. **Not installed by default**; the `platform=kueue` leaves install it as a `dependencyRef` of `kueue`. Labels are written asynchronously, so its health check asserts the Deployments only, not labels. Provider per leaf: `gcp` (GKE), `aws` (EKS), `dra` reading the `nvidia.com/gpu.clique` node label (Kind). Cloud providers require IAM access — see [Topology-aware scheduling](#topology-aware-scheduling-with-kueue-and-topograph). | [Topograph](https://github.com/dsx-ai-factory/topograph) |
| **slinky-topograph** | Slinky/Slurm-scoped instance of Topograph — derives topology from cloud provider APIs (GCP, AWS, OCI …) or, with the `dra` provider, from Kubernetes node labels, and generates Slurm `topology.conf`, enabling topology-aware placement decisions in the Slinky-managed scheduler. **Not installed by default**; leaf overlays opt in by adding an explicit `componentRef` entry for `slinky-topograph` — the `componentRef` is what schedules the release; `dependencyRefs` alone does not install anything. That `componentRef` declares `slinky-slurm` as a `dependencyRef` to deploy **after** it: `slinky-slurm` renders and owns the `slinky-slurm-config-extra` ConfigMap (from its `configFiles`, mounted into slurmctld via the Controller CR's `configFileRefs`), and Topograph patches only that ConfigMap's `topology.conf` key on each sync, preserving the chart-owned `cgroup.conf`/`gres.conf` keys — Helm has to own the ConfigMap first. `TopologyPlugin` is set per-leaf via `slinky-slurm`'s `controller.extraConfMap` and must match the `plugin` that leaf gives Topograph's engine: leaves run `topology/tree` or `topology/block` depending on the shape their provider yields, and slurmctld refuses to start on a `topology.conf` whose body does not match the configured plugin. Includes the `node-observer` component, which watches the topograph API pod and regenerates topology on restarts or selected node/pod changes. A cloud provider requires IAM access (e.g. GCP `roles/compute.viewer` for Workload Identity); the `dra` provider reads Node labels and needs none. | [Topograph](https://github.com/dsx-ai-factory/topograph) |
| **nfd-ocp-olm** | OLM installer for Node Feature Discovery on OpenShift. Creates the OperatorGroup and Subscription resources that install NFD via the Operator Lifecycle Manager. Paired with `nfd-ocp`. OCP-specific. | [Node Feature Discovery (Certified)](https://catalog.redhat.com/software/container-stacks/detail/5ec53e8c110f56bd24f5f8db) |
| **nfd-ocp** | Node Feature Discovery CR for OpenShift. Configures NFD's operand (worker, topology updater) via a NodeFeatureDiscovery custom resource. Deployed after `nfd-ocp-olm`. OCP-specific. | [Node Feature Discovery](https://github.com/kubernetes-sigs/node-feature-discovery) |
| **gpu-operator-ocp-olm** | OLM installer for the GPU Operator on OpenShift. Creates the OperatorGroup and Subscription resources that install the certified GPU Operator via the Operator Lifecycle Manager. Paired with `gpu-operator-ocp`. OCP-specific. | [NVIDIA GPU Operator (Certified)](https://catalog.redhat.com/software/container-stacks/detail/5e7b210b8a3c1e00013d636d) |
| **gpu-operator-ocp** | GPU Operator ClusterPolicy CR for OpenShift. Configures the GPU Operator's runtime behavior (driver, toolkit, DCGM, device plugin, MIG manager) via a ClusterPolicy custom resource. Deployed after `gpu-operator-ocp-olm`. OCP-specific. | [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) |
| **network-operator-ocp-olm** | OLM installer for the Network Operator on OpenShift. Creates the OperatorGroup and Subscription resources that install the certified Network Operator via the Operator Lifecycle Manager. Paired with `network-operator-ocp`. OCP-specific. | [NVIDIA Network Operator (Certified)](https://catalog.redhat.com/software/container-stacks/detail/60bfbc14e1207e67e9e29585) |
| **network-operator-ocp** | Network Operator NicClusterPolicy CR for OpenShift. Configures RDMA, MOFED driver, shared device plugin, and NV-IPAM via a NicClusterPolicy custom resource. Deployed after `network-operator-ocp-olm`. OCP-specific. | [NVIDIA Network Operator](https://github.com/Mellanox/network-operator) |
| **cert-manager-ocp-olm** | OLM installer for cert-manager on OpenShift. Creates the OperatorGroup and Subscription resources that install the certified cert-manager Operator via the Operator Lifecycle Manager. Paired with `cert-manager-ocp`. OCP-specific. | [cert-manager (Certified)](https://catalog.redhat.com/software/container-stacks/detail/5ec3f5a5eebc3d6acb0ee71c) |
| **cert-manager-ocp** | cert-manager CertManager CR for OpenShift. The operand Deployments (controller, cainjector, webhook) land in a hardcoded `cert-manager` namespace regardless of the operator's own namespace. Deployed after `cert-manager-ocp-olm`. OCP-specific. | [cert-manager](https://github.com/cert-manager/cert-manager) |
| **prometheus-adapter-ocp** | Prometheus Adapter for OpenShift. Reuses the same upstream chart as `prometheus-adapter`, pointed at OCP's built-in Thanos Querier instead of kube-prometheus-stack (which stays disabled on OCP). No certified OCP operator exists for this component. OCP-specific. | [prometheus-adapter](https://github.com/kubernetes-sigs/prometheus-adapter) |
| **nvidia-dra-driver-gpu-ocp** | NVIDIA DRA GPU driver for OpenShift. Reuses the same upstream chart as `nvidia-dra-driver-gpu`, with an added SCC RoleBinding granting the kubelet-plugin DaemonSet the host device access OCP's default restricted-v2 SCC forbids. No certified OCP operator exists for this component. OCP-specific. Known limitation: the driver-version annotation injected onto the DRA pod templates falls back to the `gpu-operator-ocp-olm` Subscription channel, which changes on a channel re-pin but not on every in-channel OLM auto-upgrade — so the stale-NVML rollout gate (#973) can still miss an in-channel driver bump on OCP; tracked in [#2135](https://github.com/NVIDIA/aicr/issues/2135). | [NVIDIA DRA Driver](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu) |
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
- **Topology-aware optional components** (`topograph`, `slinky-topograph`) are not installed by default. `topograph` comes with the `platform=kueue` leaves (see [Topology-aware scheduling](#topology-aware-scheduling-with-kueue-and-topograph)); for `slinky-topograph`: Opting in requires an explicit `componentRef` entry for `slinky-topograph` in the leaf overlay — the `componentRef` is what installs it; `dependencyRefs` alone does not. That `componentRef` declares `slinky-slurm` as a `dependencyRef`, so Topograph deploys after the Slurm cluster chart, which owns the ConfigMap Topograph patches. See the wiring example in the [Recipe Development Guide](../integrator/recipe-development.md#slinky-slurm-inline-components).
- **Accelerator/OS-specific tuning** (nodewright-customizations, nvidia-dra-driver-gpu) varies by hardware and OS combination.

### NFD Topology Updater

Production GPU leaf recipes (H100, GB200, RTX Pro 6000 on EKS / AKS / GKE / OKE / LKE) enable the NFD Topology Updater. It publishes per-node `NodeResourceTopology` CRDs that describe NUMA zones, GPU-to-NUMA affinity, and NIC-to-NUMA affinity. Runtime consumers (NUMA-aware schedulers, debugging via `kubectl get noderesourcetopologies`) can read these CRDs without further configuration.

The Topology Updater requires the kubelet `podResources` gRPC socket. The `KubeletPodResources` feature gate has been on by default since Kubernetes 1.15 (Beta) and reached GA in Kubernetes 1.28; AICR's recipe constraints require K8s ≥ 1.32, so this is satisfied in practice. Recipes targeting Kubernetes `< 1.15` must enable the feature gate explicitly. Kind / KWOK simulated clusters do not run a real kubelet and therefore leave the Topology Updater disabled — kind-based recipes will not see `NodeResourceTopology` CRDs.

See the upstream [Topology Updater docs](https://kubernetes-sigs.github.io/node-feature-discovery/stable/usage/nfd-topology-updater.html) for runtime consumer examples.

### NFD Feature Publishing

nfd-worker publishes node labels, not raw hardware features. Every recipe that deploys the `nfd` component (all but the OpenShift recipes, which deploy `nfd-ocp`) sets `worker.config.core.noPublishFeatures` (`recipes/components/nfd/values.yaml`), so each node's `NodeFeature` object carries its labels and only the `kernel.loadedmodule` feature. Workers still discover every feature and compute every label, including their own `sources.custom` rules, from the full set: node labels do not change.

nfd-master caches one `NodeFeature` per node. With raw features published, each object was about 334 KB on GB300 nodes, mostly the kernel config, and nfd-master ran out of memory on a 1720-node cluster. On a kind node the published spec drops from 64,709 bytes to 2,979 bytes; `kernel.config` and `kernel.enabledmodule` alone were 82% of it.

The trade-off is that, among the features nfd-worker publishes, cluster-side `NodeFeatureRule` and `NodeFeatureGroup` objects can only match `kernel.loadedmodule`; features that other agents publish in their own `NodeFeature` objects (for example slinky-topograph with `engine.name: nfd`) are unaffected. AICR's own rules run in nfd-worker instead: recipes that ship network-operator add an nfd-worker rule (`components/nfd/values-nvidia-nics.yaml`, or `values-nvidia-nics-aks.yaml` on AKS) that labels NVIDIA networking nodes `feature.node.kubernetes.io/pci-15b3.present=true`. network-operator's own rule (on AKS, AICR's `nfd-network-rule` manifest instead) stays for one release so the label is not dropped during an upgrade. Once a node's worker stops publishing `pci.*`, those rules match nothing on that node, and a later release removes them. Upgrade through this release before taking that later one, or let the nfd-worker rollout finish before network-operator syncs; otherwise nodes whose nfd-worker has not restarted lose the label.

- **Your own NodeFeatureRules:** publish the features they read again, for example `--set-json 'nfd:worker.config.core.noPublishFeatures=["kernel.config","kernel.enabledmodule"]'` keeps everything except the two largest keys.
- **Your own nfd-worker rules:** `worker.config.sources.custom` is a list, and a list override replaces it. Include the NIC rule from the values file above when you set it on a recipe that ships network-operator.
- **Custom overlays that add network-operator:** select `components/nfd/values-nvidia-nics.yaml` (`values-nvidia-nics-aks.yaml` on AKS) as nfd's `valuesFile` in the same overlay, include its rule in a `worker.config.sources.custom` override, or publish `pci.*` again; with none of these, nothing labels the NIC nodes once nfd-worker restarts. `aicr bundle` warns in both directions: network-operator without the rule, and the rule without network-operator, for example after `--set networkoperator:enabled=false` without `--set-json 'nfd:worker.config.sources.custom=[]'`.
- **k8s-nim-operator `nfd.nodeFeatureRules.deviceID`:** reads `pci.device`, so it labels nothing unless `pci.*` is published again.
- **Upgrading a running cluster:** the label stays on every node during the upgrade, whatever order the deployer applies components in, because this release keeps the cluster-side rules. Until a node's nfd-worker restarts, the old rule labels the node; the worker's first write after the restart drops `pci.device` and adds the label in the same update. Earlier bundles shipped the AKS `nfd-network-rule` manifest as a Helm hook that Flux deleted and recreated on every `network-operator-post` upgrade; that is gone (see the `network-operator` upgrade note below), and a Flux cluster coming from such a bundle follows that note.

### NFD Permanent Removal

Uninstalling nfd does not clean up the nodes. The `feature.node.kubernetes.io/*` labels, the labels NodeFeatureRules created, NFD-managed extended resources and taints, and the `nfd.node.kubernetes.io/*` annotations stay on every node indefinitely, because AICR disables the chart's post-delete prune hook.

To retire nfd permanently, first uninstall every component that selects on those labels (gpu-operator, network-operator, nvidia-dra-driver-gpu). Then re-enable the hook for the final uninstall, so its Job runs `nfd-master -prune` against every node. Regenerate the bundle with every flag the original bundle used, adding only `--set nfd:postDeleteCleanup=true`. A dropped flag falls back to its default, so for example a bundle regenerated without its `--system-node-selector` no longer pins nfd-master to the system nodes. If the original bundle used `--config`, add `nfd:postDeleteCleanup=true` to the config's `spec.bundle.deployment.set` list instead. A `--set` flag replaces that list rather than adding to it, which drops the config's component overrides, `enabled=false` exclusions and `deployer:` options. Write the bundle to a local directory even if the original went to an `oci://` reference, because an `oci://` output pushes the whole bundle. nfd's values files do not depend on the output target:

```bash
RECIPE="recipe.yaml"  # replace with the recipe the bundle was generated from
DEPLOYER="helm"       # replace with the deployer the bundle was generated with, e.g. argocd
# Also repeat every other flag the original bundle used, such as --system-node-selector and --set.
# With --config, add nfd:postDeleteCleanup=true to its set list and drop the --set below.
aicr bundle -r "${RECIPE}" --deployer "${DEPLOYER}" --set nfd:postDeleteCleanup=true -o ./bundles
```

Redeploy only the nfd component from the regenerated bundle, then uninstall it so the hook runs. The regenerated bundle still contains every component the recipe deploys, including the ones you just uninstalled. The steps depend on the deployer.

With `argocd`, `argocd-helm` and `flux`, do not publish the regenerated bundle whole. The parent Application or the Flux root `kustomization.yaml` would redeploy the uninstalled components, and the prune would then strip the labels they select on. Copy only nfd's values files over the source the cluster syncs from, so the new revision changes nothing else:

- `argocd`: `NNN-nfd/values.yaml`
- `argocd-helm`: `NNN-nfd/values.yaml` and `static/nfd.yaml`
- `flux`: `nfd/helmrelease.yaml`

Diff each file against the deployed copy first. The only change should be `postDeleteCleanup: true`. Keep the uninstalled components out of the source from then on, including after auto-sync is turned back on.

With `helm`, run the nfd install script, then uninstall the release:

```bash
./bundles/NNN-nfd/install.sh  # replace NNN with the nfd folder's prefix in the bundle
helm uninstall nfd -n node-feature-discovery
```

With `argocd` or `argocd-helm`, the hook needs Argo CD 3.4.1 or later, because earlier versions never finish deleting an Application whose post-delete hooks include cluster-scoped objects. On an earlier version, do not publish that revision. Run the commands below with the hook still disabled, then run the Helm prune that follows them.

On Argo CD 3.4.1 or later, publish that revision the same way as the original and let the nfd Application sync. With `argocd-helm`, publish the chart under a new version and set the parent's `targetRevision` to it. Argo CD caches a chart by version, so republishing the same version leaves the nfd child on its old values. Before deleting the child, confirm `kubectl get application -n argocd "${CHILD}" -o jsonpath='{.spec.source.helm.values}'` shows `postDeleteCleanup: true`. The parent Application syncs every child with automated prune and self-heal, so it recreates a deleted nfd child. Generated children also carry no `resources-finalizer.argocd.argoproj.io` finalizer unless the bundle set `deployer:cascadeDelete=true`, so a plain delete leaves nfd running. Turn off the parent's auto-sync, then delete the child with cascade:

```bash
APP="nvidia-stack"  # replace with the parent Application: --app-name, default nvidia-stack (argocd) or aicr-stack (argocd-helm)
CHILD="nfd"         # prepend the deployer namePrefix, if the bundle set one
# With argocd-helm, a helm install --set appName=... or --set deployer.namePrefix=... overrides both names.
argocd app set "${APP}" --sync-policy none
argocd app delete "${CHILD}" --cascade
```

Remove nfd from the bundle source before turning the parent's auto-sync back on.

A completed deletion does not prove the prune ran. Through at least Argo CD 3.5.5, a [race in delete-hook handling](https://github.com/argoproj/argo-cd/issues/29100) can delete the prune Job before it starts and still finish deleting the Application, and the Job deletes itself when it succeeds. Check the nodes instead. This prints every node that still carries an NFD label or annotation, and prints nothing once the prune has run:

```bash
kubectl get nodes -o json | jq -r '.items[] | select((.metadata.labels // {}) + (.metadata.annotations // {}) | keys | any(startswith("feature.node.kubernetes.io/") or startswith("nfd.node.kubernetes.io/"))) | .metadata.name'
```

If it prints any node, run the prune through Helm, which waits for the hook Job to finish. Do this before turning the parent's auto-sync back on:

```bash
VALUES="./bundles/NNN-nfd/values.yaml"  # replace NNN with the nfd folder's prefix in the regenerated bundle
VERSION="x.y.z"                         # replace with the nfd chart version the bundle pins
helm install nfd node-feature-discovery --repo https://kubernetes-sigs.github.io/node-feature-discovery/charts \
  --version "${VERSION}" -n node-feature-discovery --create-namespace -f "${VALUES}"
helm uninstall nfd -n node-feature-discovery
```

With `flux`, commit that revision and let Flux upgrade the nfd release. The Flux Kustomization that applies the bundle recreates a HelmRelease deleted from the cluster, so remove the `nfd/` entries from the bundle's root `kustomization.yaml` and commit instead. With `prune: true`, as the bundle README configures, Flux deletes the `nfd` HelmRelease and helm-controller runs `helm uninstall`, which runs the hook.

With `helmfile`, run from the regenerated bundle directory:

```bash
helmfile -f helmfile.yaml -l name=nfd apply
helmfile -f helmfile.yaml -l name=nfd destroy
```

### GPU Operator Driver Auto-Detect

When a recipe is resolved from a snapshot (`aicr recipe --snapshot snap.yaml`, or the `ResolveRecipeFromSnapshot` SDK entry point), AICR reads the sampled GPU node's `driver-loaded` measurement and, when the NVIDIA kernel module is already loaded, injects `components.gpu-operator.overrides.driver.enabled=false` into the resolved recipe. **On recipes whose ADR-015 profile owns `driver.enabled` (the AKS family), the injector is subordinated**: the profile fragment owns the path, the injector skips it without mutating (logging the skip), and the fragment's value is authoritative. Subordination follows path ownership, not mere profile presence — the GKE family is also profiled (`gpuStack` owns `devicePlugin.enabled`, not `driver.enabled`), so the injection/teardown discussion in this section applies to GKE-COS exactly as to unprofiled compositions (OKE, legacy AKS artifacts). The override lands at the top of the merge chain (`base values.yaml → ValuesFile → Overrides`), so the rendered Helm values a deployer installs carry `driver.enabled: false` regardless of what the resolved overlay's values file would default to. This prevents the GPU Operator from installing a second driver on top of one the platform has already provisioned. Explicit `--set` flags at bundle generation (`aicr bundle --set gpuoperator:driver.enabled=true`) retain higher precedence and can supersede the injection **unless the path is profile-owned** — on recipes carrying `metadata.selectedProfile` (the AKS family's `gpuStack`), a `--set` diverging from the selected value on an owned path fails closed at bundle time. `--set` is a bundle-time flag, not an `aicr recipe` flag.

Injection is gated on the resolved overlay already declaring `driver.enabled=false` in its merged base+valuesFile. That marker check inspects `driver.enabled` alone; the shipped preinstalled-driver overlays additionally carry coordinated ownership settings (AKS and OKE set `toolkit.enabled=false`; GKE-COS keeps the toolkit enabled with the COS-specific `toolkit.installDir` under the host-managed driver root) plus `hostPaths.driverInstallDir`, and the bundle-time `CheckDriverOwnershipCoherence` validation enforces full ownership coherence for any recipe. That scopes auto-detect to overlays like AKS, GKE-COS, and OKE where every dependent setting is already aligned. Bare EKS overlays lack the marker; the auto-detect **skips them and logs a warning** (`gpu-operator driver auto-detect: pre-installed driver observed …`) telling the operator to use a preinstalled-profile overlay rather than land a half-configured Operator (driver off, toolkit and gdrcopy still enabled with no operator-managed driver root). The case is still tracked as separate work:

- **EKS** — GPU-optimized AMIs that ship an NVIDIA driver preinstalled on the AMI itself. Bare EKS overlays still warn. The `*-eks-amazonlinux-inference` leaves (`os=amazonlinux`) are the preinstalled-driver overlays: they require the NVIDIA AL2023 AMI and set `driver.enabled=false` and `toolkit.enabled=false` themselves.

On preinstalled-driver overlays whose profile does not own `driver.enabled` (GKE-COS) or that carry no profile (OKE, legacy AKS artifacts), the injection is **semantically idempotent** — the rendered `driver.enabled` value is unchanged; the resolved recipe records the override explicitly (visible in `aicr recipe -o recipe.yaml`) so the reason for the value is auditable end-to-end. On profiled AKS there is no injection at all: the `gpuStack` fragment writes the value and the injector skips the owned path (see above). The AKS default is the [AKS azure-managed profile](../integrator/aks-gpu-setup.md#default-use-the-aks-azure-managed-profile) (`driver.enabled=false`, `toolkit.enabled=false`, `operator.runtimeClass=nvidia-container-runtime` — the Azure default, where the node image preinstalls driver and toolkit); a `--gpu-driver none` pool selects the operator-managed profile value at recipe time (`--profile gpuStack=operator-managed`, see the [GPU Operator-managed profile](../integrator/aks-gpu-setup.md#alternative-let-gpu-operator-manage-the-driver)).

The inverse mismatch (no NVIDIA driver loaded on the sampled GPU node while the resolved overlay declares the preinstalled-driver profile) is handled differently depending on whether the family carries an ADR-015 configuration profile:

- **On AKS (profiled), a pool reading that mismatches the SELECTED value fails closed at resolution.** Selection comes from `--profile` (or the `azure-managed` default); the `K8s.aks-gpu-pools.gpu-driver` reading then verifies it before any driver-state post-processing. Pools reading `None` fail the azure-managed default but **qualify `--profile gpuStack=operator-managed` — rerun with that selection against the same snapshot; no pool change or recapture is needed**. `Mixed` and `Managed` reject either selection naming the observed state — fix the pools and recapture; a *missing* reading (snapshot captured without `--aks-gpu-pools`) rejects with "reading unavailable" — recapture with the pool dump (see the [AKS GPU Operator-managed profile](../integrator/aks-gpu-setup.md#alternative-let-gpu-operator-manage-the-driver)).
- **On AKS (profiled), Install-mode pools whose sampled node has no driver loaded still enter the record-and-gate flow.** Pool mode is the ownership contract, not live state: `gpu-driver: Install` satisfies the azure-managed constraint even while a failed AKS driver install or a mid-reimage node samples no loaded driver. Resolution succeeds, records `metadata.gpuDriverState: absent`, and the bundle-time `CheckDriverOwnershipCoherence` gate blocks `aicr bundle` with the AKS remedy (repair the pools and recapture, or switch pools to `--gpu-driver none`, recapture, and regenerate with `--profile gpuStack=operator-managed`). The driver-ownership paths are profile-owned, so the pre-profile per-path `--set` override tuple is rejected at bundle time.
- **On families without a profile (and legacy pre-profile AKS artifacts), every inverse mismatch takes that same warn-record-gate path**: resolution logs a warning, records `metadata.gpuDriverState: absent` in the recipe, and the bundle-time `CheckDriverOwnershipCoherence` validation blocks `aicr bundle` ([#1757](https://github.com/NVIDIA/aicr/issues/1757)) unless the values are flipped to operator-managed mode or the GPU pools are reprovisioned with the platform's default driver install and re-snapshotted. Ownership overrides are bundle-time flags there, so resolution itself cannot fail hard without cutting off the supported override path — bundle generation is the first point where the final effective values are known.

One per-OS limitation is intentionally out of the check's scope: the check verifies value *coherence* (driver ownership and driver-root lockstep), not per-OS install capability. On GKE COS node images a deliberate `--set gpuoperator:driver.enabled=true` clears the gate — the values are internally coherent — but the GPU Operator cannot install a driver on COS, so the deployment fails at deploy time, where the `gpu-operator-health` conformance-phase check is the backstop. The check's GKE remedy therefore points COS clusters at the GKE-managed driver install (`gpu-driver-version`) rather than the override tuple.

The policy is **only-false**: the auto-detect never forces `driver.enabled=true`, so recipes resolved without a snapshot (or targeting a node without a loaded driver) fall back to today's static defaults. Two operational consequences:

- Criteria-only resolves (`aicr recipe --service ... --accelerator ...`) and no-cluster mode see zero behavior change — no snapshot, no override.
- A stale snapshot from an older CLI that omits the `driver-loaded` reading is treated as *unknown*, not *absent*, so it cannot flip a hardened overlay.

**Capture the snapshot BEFORE deploying the GPU Operator** (unprofiled compositions; on profiled AKS the pool reading, not `driver-loaded`, gates resolution, and a post-deploy snapshot cannot flip an owned path). The `driver-loaded` reading is installer-agnostic — it reports whether the `nvidia` kernel module is currently loaded, not who loaded it. A snapshot taken after a prior AICR deploy has run the operator's driver container will still report `driver-loaded=true`, and a re-resolve from that post-deploy snapshot would flip a working overlay toward `driver.enabled=false`, tearing the operator-managed driver DaemonSet down and leaving new or rebooted GPU nodes driverless. AICR emits a `gpu-operator driver auto-detect: driver-loaded=true AND a ClusterPolicy is already present…` warning when both signals appear together in the same snapshot, but the guard is observability, not prevention: a pre-deploy snapshot is the intended workflow.

The signal is a single-node sample: the snapshotter Job runs on one `nvidia.com/gpu.present=true` node, so its `driver-loaded` reading is representative only when every GPU pool is in the same driver state. Mixed-pool clusters (some nodes with a preinstalled driver, some without) are out of scope for the auto-detect and tracked in [#464](https://github.com/NVIDIA/aicr/issues/464); AICR emits a `topology reports non-uniform GPU labels…` warning when the snapshot's node-topology labels indicate divergent GPU nodes so the fail-direction (some non-preinstalled pools may come up driverless) is at least observable.

#### Opt-in NVIDIADriver CR ownership

The `gpu-operator-nvidia-driver-cr` mixin adds a post-manifest that renders a
separately managed `NVIDIADriver/default` resource. The mixin itself hard-codes
no driver version or rollout policy: the consuming overlay must enable
`driver.nvidiaDriverCRD.enabled`, explicitly set `deployDefaultCR: false`, clear
the legacy ClusterPolicy driver identity and `upgradePolicy`, and provide a
complete `nvidiaDriver` value tree. The required keys are `repository`, `image`,
`version`, `manager`, and every `upgradePolicy` member: `autoUpgrade`,
`maxParallelUpgrades`, `maxUnavailable`, `waitForCompletion` (`timeoutSeconds`
and `podSelector`), `gpuPodDeletion` (`force`, `timeoutSeconds`, and
`deleteEmptyDir`), and `drain` (`enable`, `force`, `podSelector`,
`timeoutSeconds`, and `deleteEmptyDir`). `gpuPodDeletion` is intentionally
mapped to `spec.upgradePolicy.podDeletion`. `imagePullPolicy`,
`imagePullSecrets`, `driverType`, `kernelModuleType`, and `usePrecompiled` are
optional. Omitting `deployDefaultCR` does not opt in. The manifest renders only
when the GPU Operator still manages the driver and all three guards are
satisfied.

The post-manifest resolves in one Helm pass for every deployer. Driver identity,
pull secrets, and rollout policy come from `nvidiaDriver`; the existing `gds`
and `gdrcopy` trees are projected into the CR so enabling CR ownership does not
silently drop those driver sidecars. Their image identities are not pinned in
the shared GPU Operator values; an overlay adopting this mixin must supply each
complete tree it uses (`enabled`, `repository`, `image`, `version`,
`imagePullPolicy`, `imagePullSecrets`, `env`, and `args`). The resource is the
cluster-wide default, so it intentionally omits `nodeSelector`: the upstream
CRD forbids a default `NVIDIADriver` from selecting nodes. Deployments that need
pool-specific driver resources must attach separate non-default manifests
instead of this mixin.

The following replacement for the stock H100/EKS/Ubuntu training leaf is a
complete example. Put it at
`<data-dir>/overlays/h100-eks-ubuntu-training.yaml` and resolve with
`--data <data-dir>` (including that data tree's required `registry.yaml`):

```yaml
kind: RecipeMetadata
apiVersion: aicr.run/v1beta1
metadata:
  name: h100-eks-ubuntu-training
spec:
  base: h100-eks-training
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
    intent: training
  mixins:
    - os-ubuntu
    - gpu-operator-nvidia-driver-cr
  componentRefs:
    - name: gpu-operator
      overrides:
        driver:
          nvidiaDriverCRD:
            enabled: true
            deployDefaultCR: false
          # Remove the legacy ClusterPolicy-owned driver settings inherited
          # from the base values; the NVIDIADriver tree below owns them.
          repository: null
          image: null
          version: null
          upgradePolicy: null
        nvidiaDriver:
          repository: nvcr.io/nvidia
          image: driver
          version: "580.173.02"
          imagePullPolicy: IfNotPresent
          imagePullSecrets: []
          driverType: gpu
          kernelModuleType: auto
          usePrecompiled: false
          manager:
            repository: nvcr.io/nvidia/cloud-native
            image: k8s-driver-manager
            version: v0.12.1
            imagePullPolicy: IfNotPresent
            env: []
          upgradePolicy:
            autoUpgrade: true
            maxParallelUpgrades: 5
            maxUnavailable: 25%
            waitForCompletion:
              timeoutSeconds: 0
              podSelector: ""
            gpuPodDeletion:
              force: false
              timeoutSeconds: 300
              deleteEmptyDir: false
            drain:
              enable: false
              force: false
              podSelector: ""
              timeoutSeconds: 300
              deleteEmptyDir: false
        gds:
          enabled: false
          repository: nvcr.io/nvidia/cloud-native
          image: nvidia-fs
          version: "2.29.4"
          imagePullPolicy: IfNotPresent
          imagePullSecrets: []
          env: []
          args: []
        gdrcopy:
          enabled: true
          repository: nvcr.io/nvidia/cloud-native
          image: gdrdrv
          version: v2.6
          imagePullPolicy: IfNotPresent
          imagePullSecrets: []
          env: []
          args: []
```

This is an advanced ownership mode, not a stock recipe default. Do not enable
it alongside the chart-generated default CR or leave the legacy
`ClusterPolicy.spec.driver` identity populated; either combination creates two
sources of truth for the same driver rollout.

To see exactly which components appear in a given recipe, generate one:

```bash
aicr recipe --service eks --accelerator h100 --os ubuntu --intent training -o recipe.yaml
```

The output lists every component with its pinned version and configuration values.

## GKE Device-Plugin Ownership

**Device-plugin ownership is a configuration profile.** The GKE recipes declare an ADR-015 `gpuStack` profile with two qualified values, selected at recipe generation and recorded in `metadata.selectedProfile`:

- **`gke-default` (the default)** — GKE's managed device plugin is the `nvidia.com/gpu` advertiser (recorded as `advertiser: external`), and the recipe disables the GPU Operator's plugin (`devicePlugin.enabled: false`, profile-owned). Its constraint requires that **no** GPU node — identified by its `cloud.google.com/gke-accelerator` label — carries the opt-out label `gke-no-default-nvidia-gpu-device-plugin`. This is the default GKE cluster shape: create GPU node pools normally (with `gpu-driver-version=default` or `latest` for GKE's managed driver install) and **no further cluster setup is required**.
- **`bundle-installer`** (`aicr recipe ... --profile gpuStack=bundle-installer`) — the GPU Operator's device plugin is the sole advertiser (`devicePlugin.enabled: true`, profile-owned), and the constraint inverts: every GPU node must carry `gke-no-default-nvidia-gpu-device-plugin=true` on pools created `gpu-driver-version=disabled`. A second constraint on `K8s.gke-gpu-pools.gpu-driver-installation` (supplied via `--gke-gpu-pools <gcloud node-pools dump>`) corroborates that the pools were actually created that way, since the label alone only proves device-plugin ownership. A snapshot without that reading fails this value closed. The bundle's `gcp-driver-installer` component supplies the driver — the version is pinned in the recipe and upgrades roll with the bundle; nothing is applied by hand.

For the end-to-end setup flow (snapshot → recipe → validate), the qualification and selection-vs-verification matrices, and troubleshooting, see [GKE GPU Setup](../integrator/gke-gpu-setup.md).

Why exactly one advertiser: two plugins registering `nvidia.com/gpu` on one node is not a benign overlap. Kubelet's device manager keys its endpoint and device inventory by resource name, so competing registrations and `ListAndWatch` updates replace each other. Ownership becomes nondeterministic, and one plugin's device IDs (GKE uses `nvidia0`-style names, NVIDIA uses GPU UUIDs) can reach the other plugin's `Allocate`. Expect intermittent allocation and runtime failures.

**Bundle-installer cluster setup.** The opt-out label forfeits GKE's managed driver install: the managed install (`gpu-driver-version=default`/`latest`) is finalized by an init container of the **same** kube-system DaemonSet the label disables, so a labeled pool paired with `gpu-driver-version=default` comes up **driverless** — never combine the label with the managed driver install. Pools for the `bundle-installer` value must be created with `gpu-driver-version=disabled`; the bundle's `gcp-driver-installer` component carries the installer DaemonSet, so there is nothing to apply out-of-band. (AICR's GKE-COS overlays keep `driver.enabled: false` in either mode — the GPU Operator cannot install a driver on COS node images.) A previously hand-applied standalone `nvidia-driver-installer` DaemonSet must be deleted before deploying the bundle: the bundle's DaemonSet shares its name in `kube-system` and Helm will not adopt the pre-existing object. The operational procedures live in [GKE GPU Setup](../integrator/gke-gpu-setup.md#alternative-let-the-bundle-own-the-gpu-stack).

**`aicr validate` enforces the selected value deterministically, before any phase runs.** The selected value's `NodeTopology.gpu-nodes.label` constraint ([#1755](https://github.com/NVIDIA/aicr/issues/1755)) is verified against the snapshot at recipe generation and re-evaluated by the validate readiness pre-flight. The check fails closed: labels contradicting the selected value, mixed labels, an empty GPU-node set, and readings that `--max-nodes-per-entry` actually truncated (a cap larger than the node count truncates nothing and validates normally) all fail with exit 2 and remediation text pointing back at this section, before any check Jobs deploy. See [Validation](validation.md) for the readiness-gate mechanics.

The profile also locks the ownership tuple: `devicePlugin.enabled` is profile-owned, and because both values govern advertisement, the #1327 allocation-policy paths (`devicePlugin.enabled`, DRA `resources.gpus.enabled` / `gpuResourcesEnabledOverride`) are closure-locked — a bundle- or install-time override diverging at any of them is rejected rather than warned. Switching modes is a recipe-generation decision (`--profile`), never a `--set`.

The selected constraint is the only deterministic detection point. `aicr bundle` is offline by design and cannot read node labels. The operator-health deployment check passes under the conflict because it verifies only that GPU Operator controller pods are Running — it never inspects the device plugin. Allocation probes such as `check-nvidia-smi` schedule a pod requesting `nvidia.com/gpu` on each schedulable GPU node, but skip cordoned nodes and nodes with unfinished (Pending or Running) pods that request `nvidia.com/gpu`; when they do run, they may fail nondeterministically without identifying the missing label as the cause.

See GKE's [GPU node-pool guide](https://cloud.google.com/kubernetes-engine/docs/how-to/gpus) for the authoritative pool-creation procedures. The [NVIDIA GPU Operator GKE guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/google-gke.html) documents the same `gpu-driver-version=disabled` + installer-DaemonSet combination the `bundle-installer` value builds on ([#1716](https://github.com/NVIDIA/aicr/issues/1716)) — with AICR, that DaemonSet ships inside the bundle rather than being applied by hand, and the two values are distinguished at generation time by the opt-out pool label alone (positive vs negated), which resolved ADR-015 Deferred Decision 5 by construction.

## OKE Device-Plugin Ownership

**Same profile mechanism as GKE, one control-plane signal.** The OKE recipes declare an ADR-015 `gpuStack` profile with two qualified values; because both ownership axes (driver and device plugin) move together on OKE, a single signal — the `NvidiaGpuPlugin` cluster add-on's control-plane state — qualifies a selection:

- **`oci-managed` (the default)** — Oracle's GPU node image supplies the driver and toolkit, and the `NvidiaGpuPlugin` add-on is the `nvidia.com/gpu` advertiser (recorded as `advertiser: external`); the GPU Operator's driver, toolkit, and plugin are all off (profile-owned). Its constraint requires the add-on **installed and ACTIVE**.
- **`operator-managed`** (`aicr recipe ... --profile gpuStack=operator-managed`) — bring-your-own driverless image with the add-on **removed**: the GPU Operator owns driver, toolkit, and plugin, and the DRA driver root moves to the operator install path in lockstep. Its constraint requires the add-on **absent**.

The signal is supplied as an `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json` dump via `--oke-addons` on `aicr snapshot` and `aicr validate` (projected into `K8s.oke-addons.nvidia-gpu-plugin`); it is evaluated at snapshot-based generation and re-evaluated by the validate readiness pre-flight, failing closed on any other add-on lifecycle state or a missing reading. Per-node label disablement (`oci.oraclecloud.com/disable-gpu-device-plugin`) is out of contract — it leaves the add-on installed. The exactly-one-advertiser rationale is the same as [GKE's](#gke-device-plugin-ownership), and the profile closure-locks the ownership tuple the same way — switching modes is a recipe-generation decision, never a `--set`.

**Legacy device-plugin tripwire.** Older OKE clusters ship the plugin through a second, pre-add-on mechanism, a `kube-system/nvidia-gpu-device-plugin` DaemonSet reconciled by the legacy Kubernetes addon-manager, which `list-addons` cannot see. The snapshot observes that DaemonSet directly (`K8s.oke-legacy-plugin.nvidia-gpu-device-plugin`), and `operator-managed` additionally requires it `none` (absent or fully disabled), so a legacy cluster whose add-on reads `absent` still fails closed instead of double-advertising; remediation is the per-pool `oci.oraclecloud.com/disable-gpu-device-plugin=true` label or migration to the managed add-on. A same-named DaemonSet without the addon-manager `Reconcile` label reads `unknown` and also fails closed, and the node label does not change that reading. Resolve the DaemonSet's ownership and remove it if it is the conflicting workload, or migrate to the managed add-on. `oci-managed` is deliberately not gated on it. The installed add-on reconciles the same DaemonSet. For the end-to-end flow and the qualification matrices, see [OKE GPU Setup](../integrator/oke-gpu-setup.md).

## NVSentinel Deployment Posture

AICR ships NVSentinel in the upstream chart's **monitoring-only** configuration: it detects GPU and node faults and publishes health events, but takes no automatic action on a node. AICR does not disable remediation — the upstream chart ships it off, and AICR inherits that default rather than overriding it.

`recipes/components/nvsentinel/values.yaml` enables and disables no NVSentinel *component*. It carries deployment-shaping values: `fullnameOverride`, tolerate-all scheduling so GPU-node DaemonSets land on tainted nodes, `networkPolicy.enabled: false` (the metrics policy otherwise blocks cert-manager webhook traffic in the same namespace), `platformConnector` resources, and `janitor-provider.csp.provider: generic`, which selects the reboot mechanism used *if* remediation is later enabled but does not enable it. The GPU-reset action map and the pinned reboot Job image come with the [remediation steps](#graded-remediation), not this file. It also tunes which *checks* an already-on component runs: `syslog-health-monitor.enabledChecks` adds `SysLogsNICDriverError` to the three GPU checks the chart enables by default — see [NIC and fabric fault detection](#nic-and-fabric-fault-detection). Every component on/off default below is the chart's.

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
| `mongodbStore` | deploys the chart's embedded datastore; AICR's remediation steps leave it off and use `nvsentinel-mongodb` instead |
| `faultQuarantine` | cordons a node on a qualifying fault |
| `nodeDrainer` | drains a quarantined node; at the chart default it waits for workloads to finish rather than evicting them |
| `faultRemediation` | decides the remediation action |
| `janitor` / `janitorProvider` | executes it — GPU reset, reboot or terminate |

Also off: `healthEventsAnalyzer`, `lifecycleManager`, `cspHealthMonitor`, `kubernetesObjectMonitor`, `nvcreCertificationMonitor`, `nicHealthMonitor`, `slurmDrainMonitor`, `preflight`, `eventExporter`, `inclusterFileServer`, `k8sdatastoreCrds`. Verified against chart `v1.28.0`, the version pinned in `recipes/registry.yaml`.

Two entries above are turned back on by AICR's shipped recipes: `nicHealthMonitor` on AKS and OKE (see [NIC and fabric fault detection](#nic-and-fabric-fault-detection)), and `slurmDrainMonitor` on every `platform: slurm` recipe (see [Slurm Drain Monitor](#slurm-drain-monitor)). The remediation components are turned on only by the opt-in [remediation steps](#graded-remediation), which bring their own datastore rather than `mongodbStore`, `healthEventsAnalyzer` only by the opt-in [analyzer mixin](#health-events-analyzer), and `eventExporter` only by the opt-in [event exporter mixin](#health-event-export).

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
RECIPE="recipe.yaml"  # replace with your recipe file
aicr bundle -r "${RECIPE}" \
  --set nv-sentinel:global.tracing.endpoint=otel-collector.example:4317 \
  -o ./bundles
```

If your leaf needs a different value for something the mixin already sets (a different retention policy, for instance), you cannot set it on your leaf *and* adopt the mixin: the retention paths are allowlisted, but the mixin already owns them, so a leaf setting them too is rejected as a collision rather than silently overwritten. Set them directly on your own leaf's `componentRefs` *instead of* adopting the mixin -- the same pattern `recipes/overlays/vr200-rke2-ubuntu-training.yaml` uses for other `nvsentinel` values -- or override at bundle time with `--set`/`--set-json`.

**Audit logging** (`global.auditLogging.enabled`) writes a durable, rotated JSON record of every write NVSentinel makes to the Kubernetes API or a cloud API, to `/var/log/nvsentinel/{POD_NAME}-audit.log` on `platform-connectors` (the root chart's DaemonSet — it renders under that name regardless of `fullnameOverride`) and `labeler`. Retention is set explicitly (`maxSizeMB: 100`, `maxBackups: 7`, `maxAgeDays: 30`, `compress: true`) rather than inherited from the chart default, so a future upstream default change can't silently alter it. `logRequestBody` stays `false`: request bodies may carry sensitive data. The mount is a hostPath (`DirectoryOrCreate`) — any process with host filesystem access can read the file once it exists, independent of Kubernetes RBAC. AICR ships no log forwarder; collecting the file off the node is the operator's responsibility.

**Disk cost, precisely.** The filename embeds the pod's own name (`{POD_NAME}-audit.log`), and lumberjack's rotation only knows about the *current* process's own filename — it has no way to find or clean up files a previous pod instance left behind. So each live pod costs up to 100 MB (current file) plus 7 compressed backups before rotation catches up, and that's a **per-pod-identity** cost, not a bounded per-node cost: `platform-connectors` is a DaemonSet (one pod per node, so this recurs on every node, but a given node's pod identity is comparatively stable), while `labeler` is a Deployment whose pod gets a new name on every restart or reschedule — each restart starts a fresh rotation set, and the previous pod's files are never rotated away or deleted by NVSentinel itself. Left unmanaged, `/var/log/nvsentinel/` accumulates stale files from every past pod identity. Operators enabling this need their own retention or cleanup policy for the mount path, not just the mixin's built-in rotation numbers.

**Tracing** (`global.tracing.enabled`) emits OpenTelemetry traces to an OTLP collector at `global.tracing.endpoint`. At the pinned chart version, under AICR's current base configuration, the exporter env (`OTEL_EXPORTER_OTLP_ENDPOINT`/`_INSECURE`) renders on the `platform-connectors` DaemonSet only — so enabling tracing does not instrument the other workloads AICR deploys today (including `labeler`, which does receive audit logging). The chart instruments additional workloads (event-exporter, fault-remediation, node-drainer) when those optional subcharts are enabled, which AICR's base values do not do. `TestNVSentinelObservabilityChartRender` asserts that env so a chart bump that drops it is caught. The chart has no `required` guard on that value — `global.tracing.enabled: true` with no endpoint renders and deploys without error, and the exporter fails silently at runtime. AICR closes that gap at bundle time: `CheckNVSentinelTracingEndpointRequired` fails the bundle unless an endpoint is supplied, e.g. `--set nv-sentinel:global.tracing.endpoint=<host:port>`. `insecure` defaults to `false` (the endpoint is expected to use TLS); override with `--set nv-sentinel:global.tracing.insecure=true` for a non-TLS collector.

### Kubernetes Object Monitor

`kubernetesObjectMonitor` is off by default (see the table above). It evaluates CEL predicates against live Kubernetes objects on a resync loop and, when a predicate turns true, emits a health event that the platform connector turns into a node condition — cordon and drain follow only when the quarantine or remediation step is composed (see [Graded remediation](#graded-remediation)). It is the runtime complement to `aicr validate`'s install-time DaemonSet checks: `aicr validate` catches a broken rollout once, at install; the Object Monitor keeps watching afterward, so a driver pod that starts crashlooping weeks later still produces a signal.

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

**Each policy matches only its own operator's operands.** Namespace plus "owned by a DaemonSet" would not be enough: an unrelated DaemonSet an administrator happens to run in `gpu-operator` or `nvidia-network-operator` would, once unhealthy past the grace period, raise a *fatal* event and node condition blaming the operator. Each predicate therefore also requires the label that operator stamps on the DaemonSet pods it owns. The two are not the same label, and neither is a documented API. The GPU Operator label was confirmed on live deployments; the Network Operator label was verified from the pinned release's source and documentation, with the prior release exercised on Kind:

| Operator | Required label | Coverage |
|---|---|---|
| `gpu-operator` (v26.7.2) | `app.kubernetes.io/managed-by: gpu-operator` | Confirmed on a live H100 cluster at v26.7.0 and re-read on a live GB300 cluster at v26.7.1 and v26.7.2: all nine operand DaemonSets (driver, toolkit, device-plugin, DCGM, DCGM exporter, validator, GFD, MIG manager, MPS control), and the running Pods inherit it. The bundled node-feature-discovery subchart does not carry it and is out of scope. |
| `network-operator` (26.7.0) | `ds-owner: NicClusterPolicy` | Verified in the v26.7.0 source and docs; the earlier 26.4.1 behavior was also exercised on Kind. The label is applied per-operand, not uniformly, so coverage depends on which `NicClusterPolicy` a recipe ships -- see below. |

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

**Event handling is the chart's default, `processingStrategy: EXECUTE_REMEDIATION`.** The mixin does not set it, so events flow through NVSentinel's normal path (a node condition, plus cordon and drain when the quarantine or remediation step is composed; see [Graded remediation](#graded-remediation)). The subchart also offers `STORE_ONLY`, which records events without acting on them; it is not on `nvsentinel`'s `mixinSafeOverridePaths` allowlist, so set it on your own leaf's `componentRefs` if you want it.

**RBAC worth naming:** the subchart's ClusterRole grants `nodes: get/list/watch/patch/update` unconditionally, independent of which policies you configure. That is not read-only -- it is how the node condition gets written.

**Watch load is the chart's defaults, inherited deliberately.** Because each policy must match two namespaces, `resource.namespace` cannot be set, so the monitor keeps a cluster-wide Pod informer -- the same shape as upstream's own policies. The mixin leaves `resyncPeriod` (5m) and `maxConcurrentReconciles` (1) at the subchart defaults; on the clusters AICR targets that is a single informer over Pods, not a per-policy one. Neither is on `nvsentinel`'s `mixinSafeOverridePaths`, so tuning them means setting them on your own leaf's `componentRefs` rather than through a mixin. [#2430](https://github.com/NVIDIA/aicr/issues/2430) owns requalifying those defaults against real cluster sizes.

Both policies set `isFatal: true` and leave `quarantineOverrides`/`drainOverrides` unset -- deliberately, not by omission. No shipped recipe enables a quarantine component, so `isFatal: true` produces only a node condition there. With the quarantine or remediation step composed (see [Graded remediation](#graded-remediation)), the same policies drive an actual cordon/drain when an operator DaemonSet pod stays unhealthy past the grace period -- which is the correct behavior for a fault that means the node can no longer safely run GPU or RDMA workloads, not an accident of inheriting upstream's default.

**`node-not-ready` is deliberately dropped, not inherited.** The `kubernetes-object-monitor` subchart ships a third policy by default, `node-not-ready` (watches `Node` for `Ready=False`); setting `kubernetes-object-monitor.policies` replaces that default list wholesale (Helm values do not merge lists), so this mixin does not carry it forward. It is a general node-readiness signal unrelated to this mixin's scope (operator DaemonSet pod health) and would enable a new class of node-cordon behavior nobody asked for here. Adopt it explicitly, with its own deliberate `isFatal`/quarantine decision, via your own leaf overlay's `componentRefs` if you want it.

### Preflight Checks

Off by default. NVSentinel's preflight is a mutating admission webhook that appends init containers to GPU pods, so the node runs hardware checks *before* your workload's own containers start.

**These checks gate.** A node that fails one leaves the pod in `Init:Error` — the workload's own containers never start — and the failure is recorded on the node: a **fatal** result becomes a NodeCondition named after the check, while an **unhealthy but non-fatal** result becomes a Kubernetes Event instead. (They are alternatives, not both.) That is the point: the job fails in seconds rather than hanging minutes into training. Nothing is cordoned, drained or rebooted unless the quarantine or remediation step is also composed (see [Graded remediation](#graded-remediation)).

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
NAMESPACE="my-namespace"  # replace with a namespace whose GPU pods should be checked
kubectl label namespace "${NAMESPACE}" nvsentinel.nvidia.com/preflight=enabled
```

Within a labeled namespace, only pods that request a GPU resource (`nvidia.com/gpu`) are mutated; everything else passes through untouched.

**Adopting this on a cluster that already runs NVSentinel needs one manual step.** The preflight subchart ships its `PreflightConfig` CRD under `crds/`, and Helm installs a `crds/` directory only on `helm install`, never on `helm upgrade`. A cluster that installed NVSentinel *before* adopting this mixin had the subchart — and therefore its CRD — pruned by the `global.preflight.enabled` condition, so enabling the mixin later cannot backfill it. The controller still admits and injects correctly, but its `preflightconfig` controller never starts and it logs `no matches for kind "PreflightConfig"` every 10 seconds. Apply the CRD once, from the chart:

```shell
CHART="path/to/nvsentinel"  # replace with the path to the unpacked nvsentinel chart
kubectl apply -f "${CHART}/charts/preflight/crds/preflight.nvsentinel.nvidia.com_preflightconfigs.yaml"
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
    - name: preflight-dcgm-diag         #   one DCGM address, not the chart's list
    - name: preflight-nccl-loopback     #   adds SKIP_BANDWIDTH_CHECK
    - name: preflight-nccl-allreduce
      defaultEnabled: false
```

The mixin also restates `preflight.initContainers` in full — all three checks, taken from the chart with three deviations:

- `defaultEnabled: false` on `preflight-nccl-allreduce`. The next section explains why.
- `SKIP_BANDWIDTH_CHECK: "true"` on `preflight-nccl-loopback`.
- A single `DCGM_HOSTENGINE_ADDR` on `preflight-dcgm-diag`: `nvidia-dcgm.gpu-operator.svc:5555`, the GPU Operator's ClusterPolicy-mode hostengine. From v1.25.0 the chart ships a candidate list that also names the GPUCluster-mode `nvidia-dcgm-dra` Service; the bundle gate verifies one address and rejects a list.

Restating means AICR now pins that list's contents (both images, both bandwidth thresholds, and `DCGM_HOSTENGINE_ADDR`), so a chart bump cannot move them silently. `TestNVSentinelPreflightInitContainersMatchChart` renders the mixin's list against the chart's own and fails on any drift beyond those three deviations. That test runs weekly, not on every PR, so a chart bump can merge before it fires.

**`failurePolicy: Ignore` is deliberate.** The webhook sits in the pod-creation path, so the chart's `Fail` would turn a webhook outage into a pod-creation outage for every labeled namespace. `Ignore` trades a missed check for availability — the right default while this is new, and worth revisiting once it has field time. The cost is that a broken webhook is *silent*: pods are admitted unchecked, with no error anywhere. `recipes/checks/nvsentinel-preflight/health-check.yaml` detects exactly that, including the case where cert-manager has not injected the webhook's CA bundle — but **nothing runs it for you**. It is deliberately not registry-linked (the mixin is opt-in, so `make check-health-all` would run it against recipes that never deploy preflight), which is the same treatment `nvsentinel-observability` gets. Run it yourself after adopting the mixin:

```shell
make check-health COMPONENT=nvsentinel-preflight
```

Until you do, a webhook that never came up is indistinguishable from one that is working.

**`processingStrategy: EXECUTE_REMEDIATION` is the chart default, kept deliberately.** It is what makes the init container's exit code gate the pod. The obvious-looking alternative, `STORE_ONLY`, is a trap: each check converts its own failure to exit code 0 (upstream logs `Check failed (STORE_ONLY — not blocking pod)`), *and* `platform-connectors` filters `STORE_ONLY` events out before they become a NodeCondition or a Kubernetes Event. Without a datastore, which AICR deploys only with a [remediation step](#graded-remediation), that combination would ship the cost of the checks with no gate and no record — the only trace of a failure would be an init-container log that disappears with the pod.

**The name is misleading here: nothing is remediated.** The strategy controls whether the event is processable, not whether anything acts on it. All six NVSentinel remediation components (`faultQuarantine`, `nodeDrainer`, `faultRemediation`, `janitor`, `lifecycleManager`, `janitorProvider`) default off and no shipped recipe enables any. `fault-quarantine`, the only consumer that would cordon or drain, acts only when the quarantine or remediation step is composed (see [Graded remediation](#graded-remediation)). Without those steps the complete effect is: the pod is stranded, and the node gets either a NodeCondition (fatal) or a Kubernetes Event (non-fatal).

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
- **The multi-node check is unusable on Grove-scheduled recipes.** Grove's gang model is hierarchical while preflight assumes one flat PodGroup per gang ([NVIDIA/NVSentinel#1354](https://github.com/NVIDIA/NVSentinel/issues/1354)), so gang discovery finds nothing there. Because the check is off by default this costs nothing unless you request it — and if you do request it on such a recipe, the pod is stranded rather than merely uncoordinated. The two single-node checks are unaffected. Every `*-inference-dynamo` leaf is affected and no other leaf is -- today that is `b200-gke-cos`, `gb200-eks-ubuntu`, `gb200-oke-ubuntu`, `gb300-eks-ubuntu`, `gb300-gke-cos`, `h100-aks-ubuntu`, `h100-eks-ubuntu`, `h100-gke-cos`, `h100-kind`, `rtx-pro-6000-eks-ubuntu` and `vr200-rke2-ubuntu`. `TestGroveLeavesAreExactlyTheDynamoLeaves` pins the correspondence -- a Grove leaf under another name, or a dynamo leaf that moves off Grove, fails there -- but it does not read the names above, so re-check them when a dynamo leaf is added.
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
| LKE, BCM, Metal3, k0s, MK8s, generic | unverified | blocked until someone confirms they run none |

The gate is an allowlist, not a denylist: a platform is permitted only once someone has checked it runs no NPD of its own. To qualify a new one, verify that, then add it to `npdQualifiedServices` in `pkg/bundler/validations/checks.go`.

`CheckNPDNotDuplicatingProviderNPD` enforces that at bundle time. It permits only the platforms verified to run no NPD of their own — `eks`, `kind`, `rke2` — and rejects everything else with a reason:

| Rejected | Why |
|---|---|
| `gke`, `aks` | the provider already runs its own |
| `oke` | Oracle ships one disabled behind a node label; that label is operator-settable and invisible at bundle time, so a second instance cannot be ruled out |
| `ocp` | the privileged DaemonSet needs a SecurityContextConstraints binding AICR does not ship — it would bundle cleanly, then fail admission |
| `os: talos` | `os-talos` relocates privileged components into `privileged-*` namespaces for Pod Security Admission, but not NPD, so it would land in a restricted namespace |
| `lke`, `bcm`, `metal3`, `k0s`, `mk8s`, `generic` | unverified — nobody has checked whether they run their own |
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

**`metadataCollector` is a hard dependency.** `nic-health-monitor` reads GPU-to-NIC topology from `/var/lib/nvsentinel/gpu_metadata.json` and has no devices to check without it. Because a missing dependency renders and deploys silently, `CheckNVSentinelNicHealthMonitorRequiresMetadataCollector` blocks the bundle instead: enabling `global.nicHealthMonitor.enabled` with `global.metadataCollector.enabled: false` fails unless `nic-health-monitor.nicInclusionRegexOverride` carries a value the monitor will actually accept. Set is not enough — the gate requires a string with at least one non-empty pattern, and every comma-separated pattern must compile, because the chart writes the value straight into the monitor's config and it refuses to start on one that does not. An override it rejects is not a bypass; it is the same missing inventory in a crash loop. That override is the documented bypass, and it forfeits the automatic management-NIC exclusion along with the dependency, so prefer enabling `metadataCollector`. No shipped overlay disables it.

**Escalation needs the Health Events Analyzer.** The "three events in one hour escalates" behavior lives there, and it needs MongoDB. The [remediation steps](#graded-remediation) deploy MongoDB but no AICR recipe or mixin enables the analyzer, so only fatal events surface.

### Slurm Drain Monitor

When a Slurm health check drains a node, that fact stays inside Slurm: Kubernetes, NVSentinel and AICR's health checks all keep reporting the node as fine. `slurmDrainMonitor` closes that gap on Slinky clusters. The Slinky NodeSet controller mirrors each Slurm node's state onto its worker pod as a `SlurmNodeStateDrain` pod condition, whose message is the Slurm drain reason; the monitor reads it and turns a matching reason into an NVSentinel health event.

**It is on for every `platform: slurm` recipe.** Each `platform: slurm` leaf composes the `nvsentinel-slurm-drain-monitor` mixin (`recipes/mixins/nvsentinel-slurm-drain-monitor.yaml`), and no other leaf does; `TestSlurmDrainMonitorScopedToSlurmLeaves` keeps it that way. A generated Slurm recipe carries `global.slurmDrainMonitor.enabled: true` on its `nvsentinel` componentRef, and lists the monitor's Deployment in that componentRef's `expectedResources` (see below).

**Opting out.** Supply a copy of the Slurm leaf through `--data` with both the `nvsentinel-slurm-drain-monitor` entry removed from `spec.mixins` and the `nvsentinel` `componentRefs` entry removed (it carries only the `expectedResources` line), and use the same `--data` directory for `aicr recipe`, `aicr bundle` and `aicr validate`. A bundle-time `--set nv-sentinel:global.slurmDrainMonitor.enabled=false` on its own is **not** a supported opt-out: it stops the monitor from rendering, but `aicr validate` resolves values from the recipe rather than the bundle, so deployment validation still requires the `slurm-drain-monitor` Deployment and fails. `TestSlurmDrainMonitorOptOutLeaf` covers the supported path.

**It requires `slinky-slurm`.** The monitor watches worker pods in the namespace `slinky-slurm` deploys to (`slurm`), and only `slinky-slurm` creates them. `CheckNVSentinelSlurmDrainMonitorRequiresSlinky` blocks the bundle when the mixin is used on a recipe without `slinky-slurm`, with it disabled, or with the two namespaces out of step, because each of those deploys a monitor that runs and never sees a pod.

**What it reads.** Worker pods are selected with `app.kubernetes.io/name=slurmd,app.kubernetes.io/component=worker`, the labels the Slinky slurm-operator's NodeSet controller sets. Those labels, the condition name, and the `.Message` field were verified against the slurm-operator v1.2.0 source this repo pins; `TestSlurmDrainMonitorSelectorPinnedToVerifiedSlinkyVersion` fails when `slinky-slurm-operator` is bumped, so the next version is re-checked rather than assumed. A reason that carries several causes (`a; b`) is split on `; `, and each part is matched on its own.

| Drain reason | Result |
|---|---|
| Starts with `[HC]` (a Slurm health check) | Health event `SlurmHealthCheck`, `isFatal: false`, recommended action `CONTACT_SUPPORT` |
| Starts with `slurm-operator:` (Slinky's own drains) | Ignored, so routine operator activity creates no noise |
| Anything else | Ignored |
| The drain clears, or the pod is deleted | Recovery (healthy) event |

**Events are `STORE_ONLY`, and today that means you will not see them in Kubernetes.** NVSentinel's platform connector creates no node condition and no Kubernetes Event for a `STORE_ONLY` event, and no shipped recipe deploys an NVSentinel datastore to keep it (only the opt-in [remediation steps](#graded-remediation) add one). The only trace is a log line from the `platform-connectors` pod on the node the monitor runs on. `platform-connectors` runs one pod per node and `kubectl logs daemonset/...` reads only one of them, so read them all:

```shell
kubectl -n nvsentinel logs -l app.kubernetes.io/name=nvsentinel \
  --all-containers --max-log-requests=50 --tail=-1 | \
  grep '"agent":"slurm-drain-monitor"'
```

The selector can also match NVSentinel's short-lived post-upgrade cleanup Job; the `grep` keeps only the monitor's events.

This is deliberate while pattern coverage is validated: the chart's default, `EXECUTE_REMEDIATION`, would surface each drain as a node Event (the pattern is non-fatal, so not a node condition), and would also hand it to any remediation a recipe enables later. No shipped recipe remediates (remediation is opt-in, see [Graded Remediation](#graded-remediation)), but that coupling is why the step is left to you. To change it, pass `--set nv-sentinel:slurm-drain-monitor.processingStrategy=EXECUTE_REMEDIATION` at bundle time; the mixin owns that path, so setting it in a leaf overlay is rejected as a collision.

**Privileges.** The chart grants a ClusterRole with `pods: get, list, watch` and nothing else; the monitor cannot cordon, evict, label or delete anything. The grant is cluster-wide, but the monitor limits its own watch to the `slurm` namespace. It runs as a single-replica Deployment on a system node and publishes through the platform-connector socket, which exists on every node because `platform-connectors` tolerates every taint.

**Failure behavior.** The monitor keeps no state beyond memory. If it restarts while a node is drained, it publishes the drain again; if a drain clears while it is down, no recovery event is sent. A platform connector that rejects an event (an unauthenticated service account, for example) is logged and not retried.

**Deployment validation checks it.** Each `platform: slurm` leaf lists the `slurm-drain-monitor` Deployment in `nvsentinel`'s `expectedResources`, so `aicr validate --phase deployment` fails when the monitor is missing or not ready, alongside the registry's `nvsentinel` check. A deeper check, which also asserts that the monitor's RBAC is read-only, runs on demand with `make check-health COMPONENT=nvsentinel-slurm-drain-monitor`; it is not registry-linked, because the registry's `nvsentinel` check runs on every recipe and the monitor exists only on Slurm ones.

**Qualification.** `make nvsentinel-slurm-drain-monitor-e2e` bundles the `h100-kind-training-slurm` recipe, installs NVSentinel on Kind, and drives the drain condition on pods carrying Slinky's worker labels: an `[HC]` drain reaches the platform connector as `STORE_ONLY` with no node condition or Event, the undrain publishes the recovery, and `slurm-operator:` and unmatched drains publish nothing. It simulates Slinky's condition write rather than running slurmctld. The real path was also exercised by hand, with `scontrol update NodeName=<node> State=DRAIN Reason="..."` and `State=RESUME` against a full bundle: on Kind with simulated H100s from the `uat-kind-sim` setup, and on EKS with two `p5.48xlarge` nodes, where Slurm reported `gpu:h100:8`. In both, `[HC]` and compound `[HC]` reasons published and recovered, unmatched reasons published nothing, and Slinky itself immediately reverted a `slurm-operator:` drain it had not issued. A drain raised by a Slurm health-check program reacting to a real GPU fault has not been exercised.

### Slurm Drain Through Slinky

On a `platform: slurm` cluster, `node-drainer`'s default drain never finishes. Slinky runs a long-lived slurmd pod on each worker node, so `AllowCompletion` waits on it forever and the node never reaches repair, while a mode that evicts it would kill the node's running Slurm jobs. The `nvsentinel-slinky-drainer` mixin drains the node through Slurm instead. Compose it with `nvsentinel-quarantine` or `nvsentinel-remediation` (see [Graded Remediation](#graded-remediation)) on a `platform: slurm` leaf; no shipped recipe composes it.

```yaml
# your-leaf-overlay.yaml (a platform: slurm leaf)
spec:
  mixins:
    - nvsentinel-quarantine
    - nvsentinel-slinky-drainer
```

The mixin adds the `slinky-drainer` component, NVSentinel's slinky-drainer plugin at v1.28.0, and turns on `node-drainer.customDrain`. The plugin ships upstream only as Kustomize, so AICR vendors its manifests into the `nvsentinel` namespace: the `slinky-drain-template` ConfigMap `node-drainer` renders each request from and the controller Deployment, pinned by digest. Its `drainrequests.nvsentinel.nvidia.com` CRD is a separate component, `slinky-drainer-crds`, that both `slinky-drainer` and `nvsentinel` depend on, so the deployers install it, and wait for it to be Established, before the plugin or `node-drainer` starts. For each quarantined node:

1. `fault-quarantine` cordons the node, and Slinky's operator marks a cordoned node DRAIN in Slurm, so Slurm starts no new job there.
2. `node-drainer` creates a `DrainRequest` for the node instead of evicting its pods.
3. The plugin annotates the node `nodeset.slinky.slurm.net/node-cordon-reason` with the fault, then waits until every Ready pod in the `slurm` namespace on the node reports Slurm DRAIN and idle: until the node's running jobs finish.
4. It deletes every pod in `slurm` on the node, not-Ready ones without waiting, and marks the request `DrainComplete`. `node-drainer` records the drain as done and deletes the request, and the node moves on, to repair under `nvsentinel-remediation`.

At v1.28.0 `node-drainer` records a completed custom drain as `AlreadyDrained`, which `fault-remediation` reads as drained, and leaves the node's `dgxc.nvidia.com/nvsentinel-state` label at `draining` rather than `drain-succeeded`, so under `nvsentinel-quarantine` a drained Slurm node keeps reading `draining`.

Slinky's operator drains the Slurm node with the annotation as its reason, so `scontrol show node` shows the fault, for example `Reason=slurm-operator: [T] [NVSentinel] 119 GPU:0,... - <message>`. The `slurm-operator:` prefix keeps the [Slurm Drain Monitor](#slurm-drain-monitor) from reporting the drain back to NVSentinel.

**Every quarantined node goes through the plugin.** AICR sets no label that marks Slurm worker nodes, so `customDrain.nodeSelector` stays empty, and the gate rejects any scope, because a slurmd node outside it would have its jobs evicted. With it empty `node-drainer` refuses to start unless `userNamespaces` and `podDrainPolicies` are empty too. The mixin therefore sets `userNamespaces: []`, and the per-namespace modes under [Graded Remediation](#graded-remediation) do not apply. Only pods in `slurm` are drained: on a node with no slurmd pod the plugin completes the request at once, and pods in other namespaces keep running into the repair.

**Slurm's control pods must not share slurmd's nodes.** The plugin treats every Ready pod in `slurm` on the drained node as a Slurm worker and waits for it to report DRAIN, which `slurmctld`, `slurmrestd`, the login pod, `slurmdbd` and MariaDB never do, so one of them on a GPU node stalls every drain there. Node labels are not known at bundle time, so the gate accepts only selectors that give one label key different values, such as `--system-node-selector nodeGroup=system --accelerated-node-selector nodeGroup=gpu`; selectors on different keys are rejected, because one node can carry both labels. The cloud Slurm leaves already require both flags. The Kind Slurm leaf leaves slurmd and the control pods unpinned because it is one node, so the gate rejects the mixin there. On a multi-node Kind cluster, label the nodes and pin slurmd and every control pod with `--set-json`, since the leaf's empty selectors also opt them out of `--system-node-selector` and `--accelerated-node-selector`. `--system-node-selector` is still needed for the NVSentinel datastore:

```shell
aicr bundle -r recipe.yaml -o ./bundles \
  --system-node-selector nodeGroup=system \
  --set-json 'slinkyslurm:nodesets.slinky.podSpec.nodeSelector={"nodeGroup":"gpu"}' \
  --set-json 'slinkyslurm:controller.podSpec.nodeSelector={"nodeGroup":"system"}' \
  --set-json 'slinkyslurm:restapi.podSpec.nodeSelector={"nodeGroup":"system"}' \
  --set-json 'slinkyslurm:loginsets.slinky.podSpec.nodeSelector={"nodeGroup":"system"}'
```

**Returning the node to Slurm.** Deleting slurmd makes Slinky's operator mark the Slurm node DOWN, `Reason=slurm-operator: Pod is terminating`, and the NodeSet recreates slurmd on the node. AICR leaves Slurm's `ReturnToService` at its default, `0`, so the node stays DOWN after it is repaired and uncordoned. Return it with `scontrol update NodeName=<slurm-node> State=RESUME` from the controller pod.

**No time limit.** Neither the plugin nor `node-drainer` enforces one; the `customDrain.timeout` the mixin sets, `"1800"`, is there because `node-drainer` fails its config load on the chart default `"30m"`. A job that never ends holds its node in `draining`. End the job, or run `kubectl uncordon` to cancel the workflow as for any quarantined node.

**Not with `nvsentinel-observe`.** `node-drainer` creates `DrainRequest`s even in dry run, so under observe the plugin would wait out the node's jobs and delete its slurmd pods.

**Privileges.** The plugin's ClusterRole can update and patch every node and delete any pod in the cluster; it uses the pod grant only in `slurm`. It runs as non-root with a read-only root filesystem and every capability dropped.

**Bundle-time gate.** `CheckNVSentinelSlurmCustomDrainCoherent` blocks a recipe with `slinky-slurm` and a live `fault-remediation` that does not compose the mixin, so a Slurm recipe that composes `nvsentinel-remediation` without it fails to bundle: the node would never finish draining and so never be repaired. `nvsentinel-quarantine` alone still bundles on Slurm; it evicts no slurmd, and the node stays cordoned in `draining` as the step describes. It also blocks `customDrain` with no live `node-drainer`, in dry run or with a `global.dryRun` that is not a boolean, without `slinky-slurm`, the plugin or its CRD component, with the plugin outside NVSentinel's namespace or watching a namespace other than the one `slinky-slurm` deploys to (with any other the plugin finds no pods and completes every request at once), values that break the plugin's contract (a different kind, resource, completion condition or template, a `timeout` that is not a whole number of seconds, a non-empty `customDrain.nodeSelector`, or a non-empty `userNamespaces` or `podDrainPolicies`), and a Slurm control pod or slurmd whose rendered node selector is empty, or a control pod whose selector gives no label key a different value from slurmd's, so it can land on a slurmd node. It rejects `--dynamic` on the paths it checks.

**Health check.** The `slinky-drainer-crds` check asserts the CRD is Established, and the `slinky-drainer` check asserts that again, the template ConfigMap carries `drain-template.yaml`, and the plugin's Deployment has rolled out.

**Uninstalling.** The plugin puts a finalizer, `nvsentinel.nvidia.com/slinky-drainer`, on each request and removes it only after the node leaves quarantine, so a node still quarantined at uninstall keeps its request, whichever component goes first. Uncordon such nodes first; otherwise a request left behind blocks deleting the CRD until its finalizer is cleared: `kubectl -n nvsentinel patch drainrequest <name> --type=merge -p '{"metadata":{"finalizers":null}}'`.

**What is qualified.** The render tests pin `node-drainer`'s rendered `[customDrain]` table, mount and RBAC, and the plugin's manifests; a KWOK lane bundles and deploys the mixin with `nvsentinel-quarantine` onto a Slurm leaf; and a Kind e2e (`tests/e2e/nvsentinel-slinky-drainer/`) drives a fatal fault through cordon, `DrainRequest`, the simulated Slurm drain and slurmd pod deletion to `DrainComplete`. The path was also run by hand against slurm-operator 1.2.0 on a two-node Kind cluster with simulated H100s (nvml-mock 0.4.0) and `nvsentinel-remediation`: the cordon drained the Slurm node with the fault as its reason, a running job finished, slurmd was deleted, the request completed, a `GPUReset` was created, and after recovery and `State=RESUME` a new job ran. No CI job runs real Slinky.

### Graded Remediation

Remediation is opt-in, in three steps. Each is a mixin you compose on your own leaf overlay; no shipped recipe composes any of them. Adopt them in order, and stay on a step until its behavior on your cluster is what you expect.

| Mixin | Turns on | Effect |
|---|---|---|
| *(none)* | nothing | monitoring only, as above |
| `nvsentinel-observe` | `faultQuarantine`, `nodeDrainer`, `faultRemediation`, with `global.dryRun: true` | stores fault events and walks them through the pipeline; each stage logs what it would do, and nothing is cordoned, drained or repaired |
| `nvsentinel-quarantine` | `faultQuarantine`, `nodeDrainer` | cordons a node that reports a fatal fault and waits for its workloads to finish; the node is not repaired |
| `nvsentinel-remediation` | all of the above plus `faultRemediation`, `janitor`, `janitorProvider` | cordons, waits for the drain, then repairs the node |

Every step also adds the `psmdb-operator` and `nvsentinel-mongodb` components as NVSentinel's datastore (see **Datastore** below). The remediation step also needs the [`nvsentinel-health-events-analyzer`](#health-events-analyzer) mixin composed beside it, and the bundle fails without it.

```yaml
# your-leaf-overlay.yaml
spec:
  mixins:
    - nvsentinel-observe
```

The steps are mutually exclusive: they set the same toggles, so composing two fails at recipe resolution. To move up a step, replace the mixin name; moving to `nvsentinel-remediation` also requires adding `nvsentinel-health-events-analyzer`, or the bundle fails.

On a `platform: slurm` recipe, `nvsentinel-remediation` also needs `nvsentinel-slinky-drainer`; without it the bundle fails, because the drain could never finish and the node would never be repaired. `nvsentinel-quarantine` works without it and leaves a faulty Slurm node cordoned in `draining`. See [Slurm Drain Through Slinky](#slurm-drain-through-slinky).

**What "repair" means.** A recoverable GPU fault (`COMPONENT_RESET`) resets the faulted GPU in place, scoped to its GPU UUID. `RESTART_VM` and `RESTART_BM` reboot the node. `REPLACE_VM` creates a `TerminateNode`, which janitor fails under the `generic` provider AICR pins, so the node stays cordoned. Actions with no mapping, such as `CONTACT_SUPPORT`, are cordoned and drained but not repaired, and the node is labeled `remediation-failed`. `fault-remediation` labels a node `remediation-succeeded` when it creates the repair request, not when the repair finishes, so a node whose `TerminateNode` or `GPUReset` failed still reads `remediation-succeeded`; the request object's status carries the outcome. At v1.28.0 janitor also sets a failed `GPUReset`'s `Complete` condition to `True`, the condition upstream's action map (`completeConditionType: Complete`) tells `fault-remediation` to wait on, so `fault-remediation` reads a failed reset as finished. Check `kubectl get gpuresets` and its `Failed` phase rather than the node label; the repair cap still applies, because it counts attempts, not outcomes.

The chart's own default maps `COMPONENT_RESET` to `RebootNode`, which drains and reboots the whole node for a fault that a GPU reset clears. The `nvsentinel-observe` and `nvsentinel-remediation` mixins therefore carry upstream's `GPUReset` mapping from the chart's `values-remediation.yaml`, so recipes without a step are unchanged. Enabling `faultRemediation` any other way, such as with `--set`, gets the chart's `RebootNode` default. `TestMixinNVSentinelRemediation_ComposesOntoEveryLeaf` and `TestNVSentinelRemediationChartRender` fail if the mapping is lost.

**Why quarantine leaves `faultRemediation` off.** Without `janitor`, nothing acts on the repair requests `fault-remediation` creates, and because it labels the node when it creates one, every drained node would read `remediation-succeeded` with nothing repaired. (A Helm bundle applies janitor's CRDs even with janitor off, so creating the requests succeeds.)

**Drain waits; it does not evict.** AICR keeps the chart's `node-drainer.userNamespaces` default, `[{name: "*", mode: AllowCompletion}]`, and no step changes it; on Slurm the `nvsentinel-slinky-drainer` mixin replaces this drain (see [Slurm Drain Through Slinky](#slurm-drain-through-slinky)). After a node is cordoned, `node-drainer` waits, with no time limit, for every running, Ready, non-DaemonSet pod outside its `systemNamespaces` (`^(nvsentinel|kube-system|gpu-operator|gmp-system|network-operator|skyhook)$`) to finish on its own. A node running long-lived pods therefore stays cordoned in state `draining` and never reaches repair until those pods exit or an operator runs `kubectl uncordon`. AICR's own namespaces, such as `cert-manager` and `monitoring`, are not in that list, so an AICR pod that lands on a GPU node holds the drain too. NVSentinel's documentation recommends `DeleteAfterTimeout` for training namespaces: `node-drainer` waits `deleteAfterTimeoutMinutes` (default 60) from the fault, then force-deletes the pods still running. Choose the mode per namespace, for example in the `nvsentinel` component's `cluster-values.yaml`. The list replaces the chart's, so restate the `"*"` entry:

```yaml
node-drainer:
  userNamespaces:
    - name: "*"
      mode: AllowCompletion
    - name: <training-namespace>
      mode: DeleteAfterTimeout
```

**Datastore.** The steps leave the chart's embedded `mongodbStore` off. Instead they add two components and point NVSentinel at them as an external MongoDB (`global.datastore.*`) with x509 client authentication, so AICR, not NVSentinel's chart, pins the Percona version and steps its upgrades.

- `psmdb-operator` is the Percona Operator for MongoDB (chart `psmdb-operator` 1.23.2, operator 1.23.1, a patch release above the 1.23.0 that NVSentinel's own embedded copy runs) in the `nvsentinel` namespace. It watches only that namespace, through a Role rather than a ClusterRole, runs with telemetry disabled, and is the sole owner of the four `psmdb.percona.com` CRDs.
- `nvsentinel-mongodb` (chart `psmdb-db` 1.23.3, MongoDB 8.0.32) is replica set `rs0`: three members, each requesting 1 CPU and 1.5Gi (limits 1.5 CPU and 2Gi) and an 8Gi volume. Hostname anti-affinity is preferred, not required, so members may share a node on a small cluster. TLS is `requireTLS`, and the operator creates NVSentinel's `$external` x509 user with `readWrite` on `HealthEventsDatabase` and the custom role `nvsentinelTTLIndex`, which grants only `collMod` on that database so NVSentinel's setup Job can change an existing TTL index. Sharding, backup, log collection and PMM are off, though their images are still pinned. After the chart it applies NVSentinel's client certificate, `nvsentinel-mongodb-app-client`, issued by the operator-created Issuer `nvsentinel-mongodb-psmdb-issuer`, and `nvsentinel-mongodb-uri`, a Secret whose `MONGODB_URI` carries no credentials.

NVSentinel's setup Job, `<release>-external-mongodb-setup-<ttl>-<hash>`, runs the Percona `mongod` image as NVSentinel's x509 user to create the collections, indexes and TTLs; Kubernetes deletes it 24 hours after it finishes. From v1.26.0 it is an ordinary resource of the release rather than a Helm hook, so it starts alongside the pipeline pods, and on first install a pipeline pod may restart once before the Job has created the `HealthEvents` collection. It fails if MongoDB is not ready within 10 minutes (`activeDeadlineSeconds: 600`). Re-running a Helm-based deploy does not rerun a failed Job, because its spec is unchanged, so delete it first: `kubectl -n nvsentinel delete job -l app.kubernetes.io/component=mongodb-init`. Under Argo CD the Job is a synced resource that NVSentinel marks `Replace=true`, so a sync recreates a failed Job, and once Kubernetes deletes a finished Job, self-heal recreates it and the setup runs again; it is idempotent. Every Percona image is multi-arch, so the datastore runs on arm64 system nodes too.

Percona Server for MongoDB is licensed under the Server Side Public License (SSPL); the operator is Apache 2.0. Check that SSPL is acceptable for your deployment before you compose a step.

With a step composed, the datastore is in NVSentinel's event path, not only its remediation path. `platform-connectors` does not start until it can reach MongoDB and NVSentinel's setup Job has created the indexes, and an event it cannot store is returned to the reporting monitor as a failure for the monitor to retry. While `nvsentinel-mongodb` is down or not yet ready, expect health events, and the node conditions derived from them, to be delayed or missing. Recipes without a step do not use MongoDB.

**Prerequisites.**

- **A default StorageClass**, or `--storage-class`. Each of the three members claims an 8Gi `ReadWriteOnce` volume. Without a StorageClass the claims stay `Pending` and the pipeline crash-loops waiting for its datastore. AICR provisions no storage. A recipe generated from a snapshot records whether the cluster has a default StorageClass, and `aicr bundle` fails when it has none and no class is named.
- **`--system-node-selector`.** `nvsentinel-mongodb` requires it, so a bundle that composes a step fails without it. Each member's volume binds to the zone of the node the member first lands on, and a member scheduled onto a GPU node would strand its data there.
- **cert-manager.** The datastore's TLS certificates and NVSentinel's client certificate are issued through cert-manager, which every recipe already deploys.
- **metadata-collector.** The GPU reset is scoped to the GPU UUID it supplies. Without it, the GPU and syslog monitors downgrade `COMPONENT_RESET` to `RESTART_VM`, so a recoverable fault reboots the node. Every shipped leaf runs it; a bundle that disables it with `fault-remediation` on fails (see **Bundle-time gates**).
- **No other Percona operator.** A cluster-wide operator would also reconcile AICR's cluster, and `psmdb-operator`'s CRD step force-applies its CRDs over the existing ones. A recipe generated from a snapshot records what the cluster runs, and `aicr bundle` fails when it shows a PerconaServerMongoDB other than AICR's own, a Percona operator AICR did not install, or evidence it could not read. The `psmdb.percona.com` CRDs alone only warn: helm-deployed clusters that ran NVSentinel before these steps already carry them, because NVSentinel's CRD step applied its embedded Percona CRDs until it began excluding them, and `psmdb-operator` replaces them. An operator installed with a renamed `app.kubernetes.io/name` label is not recognized.

**Not supported.**

- Talos: `os-talos` moves `nvsentinel` to the `privileged-nvsentinel` namespace, away from the Percona components, and the bundle gate fails closed on the mismatch.
- Custom cluster domains: the connection URI names `svc.cluster.local`.
- The remediation step on GKE COS, under either `gpuStack` profile (`gke-default` or `bundle-installer`). In both, the driver comes from outside the GPU Operator and lives in `/home/kubernetes/bin/nvidia`. The GPU reset Job runs `nvidia-smi` by chrooting into the driver root, and that directory holds only driver files, not a complete filesystem; the `gpu-reset` image ships no `nvidia-smi` of its own. The bundle fails. The observe and quarantine steps work there.

**Bundle-time gates.** `CheckNVSentinelRemediationPipelineCoherent` blocks a bundle whose pipeline cannot work: quarantine, drain or remediation with the embedded `mongodbStore` on, with no `global.datastore.provider`, or with one other than `mongodb` or `postgresql`; a stage without the one before it (`node-drainer` without `fault-quarantine`, `fault-remediation` without `node-drainer`); a live `fault-remediation` without `janitor`, or without the [health events analyzer](#health-events-analyzer) and its `MultipleRemediations` rule; `janitor` without `janitor-provider`; janitor's GPU reset Job requesting a RuntimeClass other than the GPU Operator's, or any RuntimeClass where the GPU Operator runs CDI with the NRI plugin; the reset Job reading the driver from a path the GPU Operator does not populate (`/run/nvidia/driver`, or its `hostPaths.driverInstallDir`, when it installs the driver; `/` when the node image does), a reset `driverRoot` that is not an absolute, clean path, or is `/`, or the reset on GKE COS under either `gpuStack` profile; a reset service manager that waits for an operand the GPU Operator does not run, in a namespace it does not use, or with a custom spec missing its timeouts; `fault-remediation` without `metadata-collector`. When NVSentinel reads the `nvsentinel-mongodb-uri` Secret, it also requires x509 auth with the `nvsentinel-mongodb-app-client` certificate, and both Percona components deployed, enabled and in NVSentinel's namespace. `CheckNVSentinelMongoDBCoherent` blocks an `nvsentinel-mongodb` with no NVSentinel to use it, TLS below `requireTLS`, no `$external` app user or one without `readWrite` on `HealthEventsDatabase` and the `nvsentinelTTLIndex` role, sharding on, no enabled `psmdb-operator`, a `psmdb-operator` with `watchAllNamespaces` on or `disableTelemetry` off, or a `crVersion` or `initImage` off the operator image's minor. `CheckNVSentinelDatastorePrerequisites` reads the snapshot evidence above: it blocks no default StorageClass (unless one is named), another Percona cluster or operator, or unreadable evidence, and warns when the recipe carries no evidence, as a criteria-only recipe does. The first two gates also reject `--dynamic` on any path they verify, since install-time edits to `cluster-values.yaml` would bypass them. The mixins satisfy those two by construction, so they catch `--set` edits and overlays that disable a dependency; the third depends on the cluster. On a Slurm recipe `CheckNVSentinelSlurmCustomDrainCoherent` also applies; see [Slurm Drain Through Slinky](#slurm-drain-through-slinky).

**Health checks.** Each step gives `nvsentinel-mongodb` its own check. It asserts that the Percona cluster is `ready`, every `nvsentinel-mongodb-rs0` member is ready, the client certificate is `Ready`, the URI Secret exists, and every Deployment the step enables is available: `fault-quarantine` and `node-drainer`, plus `fault-remediation` in observe, plus `fault-remediation`, `janitor` and `janitor-provider` in remediation. The component exists only when a step is composed, so `aicr validate` fails a step whose pipeline did not come up instead of passing on its absence. `psmdb-operator`'s check asserts the operator Deployment and its CRD.

**Privileges.** `janitor-provider.csp.provider: generic` reboots by running a privileged Job, on the busybox the `nvsentinel-remediation` mixin pins in `janitor-provider.csp.generic.rebootImage`, that executes `chroot /host reboot` on the target node. The GPU reset runs as a privileged Job that mounts the host's `/dev`, `/sys`, the journal socket and the driver root, sets `NVIDIA_VISIBLE_DEVICES=void` so the container runtime injects nothing, and pulls the `gpu-reset` image on every run. That avoids cloud IAM credentials but is a broad grant.

The reset Job runs `nvidia-smi` by chrooting into the host's driver root, so where it reads the driver from and which RuntimeClass it requests depend on the platform. The overlays set both next to the matching DRA driver root, and `CheckNVSentinelRemediationPipelineCoherent` rejects a mismatch:

| Platform | `resetJob.hostDriverRootPath` | `resetJob.runtimeClassName` | Set by |
|---|---|---|---|
| GPU Operator installs the driver (EKS except os=amazonlinux, BCM, LKE, OCP, k0s, generic) | `/run/nvidia/driver` (chart default) | `nvidia` (chart default) | — |
| AKS `gpuStack=azure-managed` (default) | `/` | `nvidia-container-runtime` | the `gpuStack` profile |
| AKS `gpuStack=operator-managed` | `/run/nvidia/driver` | `nvidia` | the `gpuStack` profile |
| OKE `gpuStack=oci-managed` (default) | `/` | `nvidia` (chart default) | the `gpuStack` profile |
| OKE `gpuStack=operator-managed` | `/run/nvidia/driver` | `nvidia` (chart default) | the `gpuStack` profile |
| EKS `os=amazonlinux` leaves (require the NVIDIA AL2023 AMI) | `/` | `nvidia` (chart default) | the leaf overlay |
| Kind (nvkind), H200 k0s | `/` | `nvidia` (chart default) | the overlay |
| RKE2 (CDI with NRI) | `/` on VR200, the chart default elsewhere | none (`""`) | the `rke2` and VR200 overlays |
| GKE COS (`gpuStack=gke-default` or `bundle-installer`) | not supported (see **Not supported**) | — | — |

Around each reset janitor stops the GPU Operator's operands on the node and then waits up to 10 minutes for them to come back; it waits for every operand it lists, deployed or not, so the list must match what the GPU Operator runs. Its built-in list is the device plugin, `nvidia-dcgm`, the DCGM exporter and GPU feature discovery. Kind runs no standalone DCGM and OKE's `oci-managed` profile runs no GPU Operator device plugin, so those overlays set `janitor.config.controllers.gpuReset.serviceManager.spec` to the operands they run; a reset with the built-in list there would always end in `RestoreTimeoutExceeded`.

Every value is inert until the remediation step enables janitor.

**Retained data.** Health events persist on the three `nvsentinel-mongodb` volumes. MongoDB expires a health event 30 days after it is created and a maintenance event 30 days after it ends. Both come from `mongodb-store.collectionExpirySeconds` (default `2592000`), which NVSentinel's chart reads even with the embedded store off; set it on the `nvsentinel` component to change both. NVSentinel's setup Job creates the TTL indexes on install and changes them with `collMod` on upgrade, which the `nvsentinelTTLIndex` role permits. The volume claims outlive an uninstall, so delete them by hand to discard the history. Completed `GPUReset` and `RebootNode` objects are removed by janitor after 14 days.

**Datastore CA renewal.** The operator manages the datastore's TLS through cert-manager, the same model as NVSentinel's embedded Percona path. Its CA is valid for 365 days and renews 730 hours before it expires. NVSentinel reads the CA from its client certificate Secret. `certificateRotationEnabled` reloads the client certificate when it renews, but whether NVSentinel reloads a renewed CA is not verified. The CA is the cert-manager Certificate `nvsentinel-mongodb-ca-cert`; `kubectl -n nvsentinel get certificate nvsentinel-mongodb-ca-cert -o jsonpath='{.status.renewalTime}'` shows when it next renews. If a consumer kept the old CA, its logs show TLS verification errors against MongoDB (`certificate signed by unknown authority`) and new health events stop being stored. After the CA renews, restart the datastore consumers so they load it (omit `fault-remediation` on the quarantine step, which does not deploy it):

```bash
kubectl -n nvsentinel rollout restart daemonset/platform-connectors \
  deployment/fault-quarantine deployment/node-drainer deployment/fault-remediation
```

AICR leaves `tls.certManagementPolicy` at Percona's default, `auto`, so the operator owns the CA. Percona 1.23's `userProvidedOnly` would let AICR supply the CA and own its lifetime; AICR does not use it yet.

**Upgrading the datastore.** Percona upgrades the operator one minor at a time, CRDs first. AICR keeps `nvsentinel-mongodb`'s `crVersion` at the operator's minor (`CheckNVSentinelMongoDBCoherent`), so move `psmdb-operator` and `nvsentinel-mongodb` together, one minor per upgrade; the dependency order upgrades the operator first. See [Upgrade records](../contributor/upgrade-records.md) for how AICR records a component's upgrade boundaries.

**Uninstalling.** Remove `nvsentinel` first, so nothing is writing to the datastore, then `nvsentinel-mongodb`, then `psmdb-operator`. `nvsentinel-mongodb` drops the chart's `percona.com/delete-psmdb-pods-in-order` finalizer, so deleting it never waits on the operator: Kubernetes garbage-collects the replica set it owns, and the volume claims remain (see **Retained data**).

**Failure behavior and recovery.**

- In the observe step nothing is cordoned or tainted, but the node is not left untouched: `fault-quarantine` writes its quarantine annotations, and `node-drainer` and `fault-remediation` still move the `dgxc.nvidia.com/nvsentinel-state` label through `draining` to `remediation-succeeded`. `node-drainer` still waits for the node's pods to finish, so on a node running any non-DaemonSet pod outside `systemNamespaces` the drain does not finish until those pods exit, and until then the event never reaches `fault-remediation`.
- Moving from observe to a live step discards the faults observed so far. Each dry-run quarantine carries a `quarantineHealthEventDryRun: "True"` annotation, and when dry run is switched off `fault-quarantine` removes the dry-run annotations and cancels their events at startup, so `node-drainer` does not act on them. The cancel is best-effort and not retried: if it fails, the events stay `Quarantined` and `node-drainer` can still drain and repair the node. After switching dry run off, check `fault_quarantine_processing_errors_total{error_type="mongodb_cancel_quarantine_error"}`; if it moved, set `healtheventstatus.nodequarantined` to `Cancelled` on that node's remaining `Quarantined` events before the live step acts on them. Faults raised after the move are acted on normally. Annotations written by the observe step before v1.27.0 carry no marker, and `fault-quarantine` reads each of them once as a manual uncordon instead, annotating the node `quarantinedNodeUncordonedManually`.
- A quarantined node stays cordoned until NVSentinel receives a healthy event for the fault. `kubectl uncordon` cancels the workflow: NVSentinel clears its taints, labels and annotations and marks the events cancelled. To keep NVSentinel off a node entirely, label it `k8saas.nvidia.com/ManagedByNVSentinel=false` or `nvsentinel.dgxc.nvidia.com/managed=false`; every fault-quarantine rule set honors both. Both are upstream's documented procedure ([Cancelling Break-Fix Workflows](https://github.com/NVIDIA/NVSentinel/blob/v1.28.0/docs/cancelling-breakfix.md)).
- The remediation step caps repairs at three attempts per equivalence group (for example `reset` or `restart`) within one quarantine (`fault-remediation.maxRemediationAttempts: 3`; the chart default `0` retries without limit). At the cap, NVSentinel labels the node `dgxc.nvidia.com/nvsentinel-state=remediation-failed` and leaves it cordoned for an operator; the count resets when the node leaves quarantine. Watch for that label, repair the node by hand, then `kubectl uncordon` it. A node repaired again and again across quarantines is reported by the analyzer's `MultipleRemediations` rule, which the remediation step requires (see [Health Events Analyzer](#health-events-analyzer)).
- The GPU reset Job has a 300-second `activeDeadlineSeconds` that includes pulling the `gpu-reset` image, and the Job always re-checks the tag against the registry. A slow or unreachable registry can time the reset out, so mirror the image close to the cluster.
- Only fatal events drive the pipeline. Every step sets `processingStrategy: EXECUTE_REMEDIATION` explicitly on `gpu-health-monitor` and `syslog-health-monitor`, the event sources it acts on, so a chart default change cannot alter that; the syslog NIC patterns, the NIC checks and the NPD object-monitor policies ship `STORE_ONLY` and are not acted on. With [preflight](#preflight-checks) composed as well, a fatal preflight result cordons the node.
- `fault-quarantine`'s circuit breaker stays at the chart defaults: it trips when 50% of GPU nodes (`nvidia.com/gpu.present=true`) are cordoned within 5 minutes (`maxNodes: 0`, no absolute cap). Tripped, it halts every health event, healthy ones included, so no node is cordoned or uncordoned until an operator resets it; on a small cluster the first cordon can trip it. Dry run feeds no breaker, but before v1.27.0 the observe step did, so a cluster that ran observe on an earlier version can carry a persisted trip into its first live step: check the breaker ConfigMap and reset it before moving. Set `fault-quarantine.circuitBreaker.maxNodes` or `percentage` to bound the blast radius for your fleet, and see upstream's [circuit breaker](https://github.com/NVIDIA/NVSentinel/blob/v1.27.0/docs/circuit-breaker.md) for the reset procedure.

**What is qualified.** The render tests pin the resolved action map, each step's `--dry-run`, the Deployments it renders and the images it pulls, a KWOK lane bundles and deploys each step onto an EKS leaf with the helm deployer, and the remediation step with Argo CD and Flux too, and a Kind e2e (`tests/e2e/nvsentinel-remediation/`) drives synthetic faults through the observe step's dry-run pipeline. `fault-remediation` returns on dry-run before it selects a repair kind, so no log names `GPUReset`; the e2e instead asserts what only the GPU-scoped mapping does, rejecting a `COMPONENT_RESET` event that carries no GPU UUID. Outside Slurm, nothing in CI runs the quarantine or remediation step live: the only CI job that cordons and drains a node is the [Slurm drain e2e](#slurm-drain-through-slinky), which runs the quarantine step with the `nvsentinel-slinky-drainer` mixin, and no CI job resets a GPU or reboots a node. Treat both steps as qualified up to the decision, not the execution, and trial them on a cluster you can afford to have cordoned and rebooted.

**NVSentinel is included by default, not universally required.** `recipes/overlays/base.yaml` includes it unconditionally, so every recipe carries it unless something later removes it, and ADR-018 classifies it as `core` as a named exception: a GPU pod runs without it, but its remediation cordons, drains, and reboots GPU nodes, which makes it part of keeping the GPU runtime healthy. Two things can still remove it: an overlay that overrides `enabled` (the OCP overlay does), and a bundle-time exclusion on a platform whose presence is not profile-locked (EKS, and GKE-COS and OKE, whose profiles reference it as optional). Only the AKS `gpuStack` profile makes its presence mandatory — see [NVSentinel presence on the profiled families](#nvsentinel-on-provider-installed-driver-platforms) below.

**The per-platform values AICR sets are platform-correctness values, not remediation policy.** `labeler.assumeDriverInstalled`, the RuntimeClass names (`metadata-collector.runtimeClassName` and janitor's reset Job) and the reset Job's driver root describe facts about the target cluster that the chart cannot infer — who installs the driver, where it lives, and what the RuntimeClass is named. The next section gives the per-platform values, when an explicit value is needed, and what each failure looks like when it is missing. Which components run is a different matter and stays upstream's call.

### Health Events Analyzer

NVSentinel's `health-events-analyzer` looks for patterns across stored health events that no single event shows: an XID repeating on one GPU, XID 13 clustering on one GPC and TPC, an XID 74 whose NVLink registers point to a hardware fault, or a node repaired again and again. When a rule matches, it publishes a derived health event whose `checkName` is the rule name. It is opt-in through the `nvsentinel-health-events-analyzer` mixin (`recipes/mixins/nvsentinel-health-events-analyzer.yaml`), which no shipped recipe composes. It reads NVSentinel's datastore, so compose it with a [remediation step](#graded-remediation), which adds one:

```yaml
# your-leaf-overlay.yaml
spec:
  mixins:
    - nvsentinel-remediation
    - nvsentinel-health-events-analyzer
```

`CheckNVSentinelRemediationPipelineCoherent` fails a bundle that turns on the analyzer without a datastore, with the same datastore rules as the remediation stages. The analyzer also needs MongoDB: NVSentinel's PostgreSQL client cannot run the aggregation stages several of its XID 74 rules use, so a `postgresql` datastore is rejected.

**Required with the remediation step.** `fault-remediation.maxRemediationAttempts` caps repairs within one quarantine, and the count resets when the node leaves quarantine. A node that is repaired, returns healthy, and fails again next week is never stopped by that cap. The analyzer's `MultipleRemediations` rule is what reports it, so `CheckNVSentinelRemediationPipelineCoherent` fails a bundle that runs `fault-remediation` live (`global.dryRun: false`) without the analyzer, or with `health-events-analyzer.enableMultipleRemediationsRule` off, or with any `health-events-analyzer.config` override. That override is a Helm template whose rules exist only after rendering, so the gate cannot verify that it still evaluates the rule; choose rules with the chart's `enable<Rule>Rule` toggles instead. The observe step's dry run makes no repairs, so it does not need the analyzer, and the quarantine step does not run `fault-remediation`. With the analyzer composed on the observe step, `MultipleRemediations` still counts its dry-run remediations: in dry run, `fault-remediation` marks each event whose repair it would have created as remediated.

**Rules.** The mixin restates every rule the pinned chart (v1.28.0) ships, all on, so the rule set is chosen in the mixin rather than inherited; none is disabled. `TestNVSentinelHealthEventsAnalyzerChartRender`, in the weekly render check, fails when a chart bump adds a rule the mixin does not name.

| Group | Rules | Recommended action |
|---|---|---|
| Remediation feedback | `MultipleRemediations`: five or more remediated events on a node within 7 days. It counts the repair requests `fault-remediation` created, whether or not the repair then succeeded | `CONTACT_SUPPORT` |
| Repeated and clustered XIDs | `RepeatedXIDErrorOnSameGPU`, `RepeatedXID31OnSameGPU`, `RepeatedXID31OnDifferentGPU`, `RepeatedXID13OnSameGPCAndTPC`, `RepeatedXID13OnDifferentGPCAndTPC`, `XIDErrorSoloNoBurst` | `CONTACT_SUPPORT`, `RUN_DCGMEUD` or `NONE`, per rule |
| XID 74 (NVLink) register decode | `XID74Reg0SoloNVLinkError`, `XID74Reg0ECCParityError`, `RepeatedXID74Reg0HardwareIssue`, `XID74Reg0SignalIntegrityError`, `XID74Reg0Bit27Or29Set`, `RepeatedXID74Reg2HardwareIssue`, `XID74Reg2Bit13Set`, `RepeatedXID74Reg2Bit16Or19Set`, `RepeatedXID74Reg2Bit17Or18Set`, `XID74Reg3UnexpectedError`, `XID74Reg3Bit18Set`, `XID74Reg4HardwareIssue`, `XID74Reg4ECCError` | `CONTACT_SUPPORT`, except `XID74Reg4ECCError` (`NONE`) |
| Repeated NIC errors | `RepeatedNICDriverError`, `RepeatedNICDegradation` | `CONTACT_SUPPORT` |

The analyzer skips source events that are `STORE_ONLY`. Every remediation step sets the GPU and syslog monitors to `EXECUTE_REMEDIATION`, so the XID rules see their events. AICR ships the syslog NIC patterns and the [NIC health monitor](#nic-and-fabric-fault-detection) at `STORE_ONLY`, so the two NIC rules do not fire until those sources are moved to `EXECUTE_REMEDIATION` or `STORE_AND_ANALYSE`. `STORE_AND_ANALYSE` feeds the analyzer while, like `STORE_ONLY`, setting no node condition and no Kubernetes Event for the source event. A derived event with action `NONE` is non-fatal; every other one is fatal.

**Derived events are `STORE_ONLY`.** The mixin sets `health-events-analyzer.processingStrategy: STORE_ONLY`, as a first step to observe what the rules match. Derived events are written to the datastore and counted in the analyzer's `rule_matched_total{rule_name,node_name}` metric, which the chart's PodMonitor scrapes. The platform connector sets no node condition and creates no Kubernetes Event for them. The analyzer cordons nothing at either strategy: none of `fault-quarantine`'s rule sets matches events from `health-events-analyzer`.

To surface matches on the node, pass `--set nv-sentinel:health-events-analyzer.processingStrategy=EXECUTE_REMEDIATION` at bundle time. The mixin owns that path, so setting it in a leaf overlay is rejected as a collision. A fatal derived event then sets a node condition named after the rule, and a non-fatal one creates a Kubernetes Event.

**Node conditions do not clear themselves.** The analyzer never publishes a healthy event, so a condition it sets, `MultipleRemediations` included, stays on the node until an operator removes it. After the hardware is fixed, remove it:

```shell
NODE="gpu-node-1"            # replace with the node name
RULE="MultipleRemediations"  # replace with the condition to clear
kubectl get node "${NODE}" -o json | \
  jq --arg rule "${RULE}" '.status.conditions |= map(select(.type != $rule))' | \
  kubectl replace -f - --subresource=status
```

**Resources and privileges.** One replica on a system node, requesting and limited to 1 CPU and 1Gi. With a MongoDB datastore the chart runs it as root (`runAsUser: 0`), and it has a namespaced Role to read and write its own `resume-control` ConfigMap. It publishes through the platform-connector socket; the chart admits its service account for events about any node. For each unhealthy event the analyzer receives, it runs one datastore query per enabled rule whose `when` condition matches the event. `MultipleRemediations` has no `when`, so it queries on every event, and the datastore load grows with the event rate.

**Deployment validation.** The registry's `nvsentinel` check asserts that the `health-events-analyzer` Deployment is fully rolled out wherever it renders, and passes where it does not.

### Health Event Export

NVSentinel's Event Exporter sends health events off the cluster: it watches the datastore and posts each event to an HTTP endpoint you run as a [CloudEvent](https://cloudevents.io/) (`type: com.nvidia.nvsentinel.health.v1`, `source: nvsentinel://<cluster>/healthevents`), authenticated with an OAuth 2.0 client-credentials token from your identity provider. Use it for a view across clusters, long-term failure-trend storage, or a feed into ticketing. The `nvsentinel-event-exporter` mixin enables it, and no shipped recipe composes it, because the sink, the identity provider and the cluster's identity are yours. It reads the datastore, so compose it with a [remediation step](#graded-remediation):

```yaml
# your-leaf-overlay.yaml
spec:
  mixins:
    - nvsentinel-observe
    - nvsentinel-event-exporter
```

**Per-deployment values come from the bundle command.** `CheckNVSentinelEventExporterCoherent` fails the bundle until each is set. The `.example` hosts below are placeholders, which the gate rejects:

```shell
aicr bundle -r recipe.yaml -o ./bundle \
  --set nv-sentinel:event-exporter.exporter.sink.endpoint=https://events.corp.example/v1/events \
  --set nv-sentinel:event-exporter.exporter.oidc.tokenUrl=https://idp.corp.example/oauth2/token \
  --set nv-sentinel:event-exporter.exporter.oidc.clientId=nvsentinel-exporter \
  --set nv-sentinel:event-exporter.exporter.oidc.scope=events:write \
  --set nv-sentinel:event-exporter.exporter.metadata.cluster=gpu-train-1 \
  --set nv-sentinel:event-exporter.exporter.metadata.environment=production
```

Any other `event-exporter.exporter.metadata.<key>`, such as `region`, is added to every event. A value `--set` would read as a number or boolean, such as an all-digit client ID, goes through `--set-json` as a quoted string instead.

**Create the client secret before deploying.** The chart mounts the OIDC client secret from a Secret it does not create, and the exporter does not start without it:

```shell
kubectl create namespace nvsentinel --dry-run=client -o yaml | kubectl apply -f -
kubectl -n nvsentinel create secret generic event-exporter-oidc-secret \
  --from-literal=oidc-client-secret="${OIDC_CLIENT_SECRET}"
```

The name is `event-exporter.oidcSecretName` (the mixin sets `event-exporter-oidc-secret`) and the key is always `oidc-client-secret`. The exporter sends the client ID and secret to `tokenUrl` with HTTP Basic authentication, requests `grant_type=client_credentials` with the configured scope, and sends the returned token as `Authorization: Bearer` on each event.

**What is exported.** The mixin sets the filter `event.recommendedAction != 'NONE' || event.isHealthy == true`: every event that recommends an action, and every recovery. It drops unhealthy events that recommend no action, which are most of the stream. `TestNVSentinelEventExporterFilter` pins that choice. The expression is CEL over the event's fields (`agent`, `checkName`, `componentClass`, `errorCode` as a list, `isFatal`, `isHealthy`, `recommendedAction`, `nodeName`, `metadata`, `message`); to export everything to a data lake and filter there, pass `--set-json 'nv-sentinel:event-exporter.exporter.filter.expression=""'` at bundle time (`--set` rejects an empty value). A filter that fails to evaluate exports the event and counts `health_events_exporter_filter_errors_total`. The mixin owns the filter path, so setting it in a leaf overlay is rejected as a collision.

**Delivery is at-least-once.** The exporter stores its resume token in the datastore, in `HealthEventsDatabase`'s `ResumeTokens` collection beside the other NVSentinel components' (client name `event-exporter`), and continues from it after a restart, so an event can arrive twice. The CloudEvent `id` is generated per publish, so a redelivery after a restart carries a new one. Deduplicate on a hash of the CloudEvent `source` and the whole `data.healthEvent` object instead, which a redelivery repeats exactly: a subset of its fields is not unique, since a monitor stamps one `generatedTimestamp` on every event of a polling batch, so faults on two GPUs of one node differ only in `entitiesImpacted`, and keying on fewer fields would drop one of them. Whenever it starts without a resume token, as on first start, it backfills up to 30 days of stored events (at most a million, at 1000 a second), so a new sink receives recent history. Ten workers publish in parallel; a failed publish is retried with exponential backoff from one second, which ends once the next delay would exceed the five-minute cap: about nine attempts over roughly nine minutes. (The chart's comment says about 30 minutes; the cap stops it sooner.) If it still fails, the exporter exits and Kubernetes restarts it from its last resume token, so a sink outage stalls export and restarts the pod rather than losing events.

**Events leave the cluster.** They carry node names, error codes and the metadata above. The mixin sets `insecureSkipVerify: false` for the sink and the token endpoint, and the gate rejects turning either check off. Over plain `http` the client secret and the bearer token cross the network unencrypted, so the gate accepts `http` only for a host inside the cluster and requires `https` everywhere else.

**Bundle-time gates.** `CheckNVSentinelEventExporterCoherent` blocks a bundle whose `sink.endpoint` or `oidc.tokenUrl` is not an `http` or `https` URL with a host, names an `example.com`, `.org` or `.net` host as the chart's placeholders do, or a host under the reserved `.example` TLD, or uses plain `http` to a host outside the cluster (anything but a single-label name, `*.svc` or `*.svc.cluster.local`); that does not set `oidc.clientId`, `oidc.scope`, `metadata.cluster` (or leaves it at the chart's `my-cluster`) or `metadata.environment`; with a double quote, backslash or control character other than tab in any of those or another metadata value, or a metadata key that is not a TOML bare key, which the chart writes into its config unescaped; with either `insecureSkipVerify` not `false`; or with an invalid `oidcSecretName`. With the exporter on, it also rejects `--dynamic` on any `event-exporter` value. `CheckNVSentinelRemediationPipelineCoherent` applies the remediation steps' datastore rules. Neither gate can confirm that the Secret exists or that the sink and identity provider accept the credentials: a missing Secret keeps the pod from starting, and a rejected token or sink shows in the exporter's log and its retry metrics.

**Resources and privileges.** One replica on a system node, requesting 500m and 512Mi and limited to 1 CPU and 1Gi. With a MongoDB datastore the chart runs it as root (`runAsUser: 0`). It holds no Kubernetes RBAC of its own; it reads the datastore with the remediation step's x509 client certificate and makes outbound HTTP calls only.

**Deployment validation.** The registry's `nvsentinel` check asserts that the `event-exporter` Deployment is fully rolled out wherever it renders, and passes where it does not. To stop exporting, regenerate the bundle without the mixin and redeploy; events already delivered stay at the sink.

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
| EKS (except os=amazonlinux) | the operator's driver pod | unset (chart default `false`) | — |
| EKS `os=amazonlinux` leaves (require the NVIDIA AL2023 AMI) | none — driver is in the AMI | `true` | the leaf overlay (these leaves have no profile) |
| Kind (nvkind) | none — driver is host-installed | `true` | the overlay (Kind has no profile) |
| k0s (H200) | none — driver is host-installed | `true` | the leaf overlay (k0s has no profile) |
| RKE2 (VR200) | none — driver is host-installed | `true` | the leaf overlays (RKE2 has no profile) |

The explicit `false` on the operator-managed variants is deliberate rather than redundant: it keeps the path profile-owned, so it cannot be flipped into an unsafe hybrid later. Do **not** assume a preinstalled driver where the GPU Operator installs one — skipping detection there would keep the label applied across an unloaded or unhealthy driver.

**NVSentinel presence on the profiled families.** The AKS `gpuStack` profile names nvsentinel, so its presence is profile-owned there: `--set nv-sentinel:enabled=false` and a `bundlers=` list that omits it are both rejected. That is intended — NVSentinel is a required component for AKS deployments.

The GKE-COS and OKE `gpuStack` profiles reference nvsentinel as `optional` ([#2978](https://github.com/NVIDIA/aicr/issues/2978)). They own `labeler.assumeDriverInstalled` (and, on OKE, the janitor GPU-reset settings) but not presence, so `--set nv-sentinel:enabled=false` removes the component — for example when NVSentinel is deployed outside AICR. When the bundle does include NVSentinel, the owned values are locked as on AKS. An externally deployed NVSentinel must set `labeler.assumeDriverInstalled` to match the cluster's `gpuStack` value itself; AICR does not verify it. A recipe generated before this change still locks presence, so regenerate it with `aicr recipe` before bundling without NVSentinel. NVSentinel is also optional on platforms with no `gpuStack` profile, such as EKS.

AKS, GKE-COS, and OKE get the install-time profile lock; Kind, k0s, RKE2, and the EKS `os=amazonlinux` leaves set the value at overlay level, so a bundle-time or declared-dynamic change is still rejected by the gate below, but a manual post-generation edit to the rendered Helm values is not.

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

The AKS `gpuStack` profile now owns these names — `gpu-operator.operator.runtimeClass`, `nvsentinel.metadata-collector.runtimeClassName`, and the GPU reset Job's `nvsentinel.janitor.config.controllers.gpuReset.resetJob.runtimeClassName` (inert until the [remediation step](#graded-remediation) enables janitor) — in the same profile value, so they agree by construction under either value and no override is needed:

| AKS profile value | `operator.runtimeClass` | `metadata-collector.runtimeClassName` | janitor `resetJob.runtimeClassName` |
|---|---|---|---|
| `azure-managed` (default) | `nvidia-container-runtime` | `nvidia-container-runtime` | `nvidia-container-runtime` |
| `operator-managed` | `nvidia` | `nvidia` | `nvidia` |

Every other platform leaves `operator.runtimeClass` at the shared chart default `nvidia`, so no side needs a value. The exception is RKE2: its GPU Operator runs CDI with the NRI plugin, which registers no RuntimeClass.

- **Generic RKE2 recipes** set `metadata-collector.nriPlugin.enabled: true`, so the collector requests the GPU Operator's management CDI device instead ([NVIDIA/NVSentinel#1949](https://github.com/NVIDIA/NVSentinel/pull/1949)). This works with a host-installed driver or the operator's driver container. It needs the nvsentinel namespace in the toolkit's `NRI_MANAGEMENT_CDI_DEVICE_NAMESPACES`, which the `rke2` root sets. The two settings are paired: a leaf that sets `cdi.nriPluginEnabled: false` must also set `metadata-collector.nriPlugin.enabled: false`. The allowlist entry lets any pod in the nvsentinel namespace request every GPU on the node, so a bundle that drops NVSentinel (`--set nv-sentinel:enabled=false`) should drop it too, with `--set-json 'gpuoperator:toolkit.env=[]'`. `aicr bundle` warns when the allowlist names a namespace that no component in the bundle deploys into; a subset bundle (the REST `bundlers=` parameter or the SDK's `config.WithBundlers`) stays silent, since the component may be deployed outside it.
- **The VR200 overlays** keep their earlier wiring: `nriPlugin` off, `metadata-collector.runtimeClassName` cleared, the host's NVML library mounted directly, and the toolkit allowlist cleared (`toolkit.env: []`), since nothing there needs the management device ([NVIDIA/NVSentinel#1717](https://github.com/NVIDIA/NVSentinel/issues/1717)).

`CheckNVSentinelRuntimeClassCoherence` still compares `operator.runtimeClass` with metadata-collector's name as defense in depth, treating either side unset as `nvidia`.

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

NVCRE v0.8.2 expects Kubeflow Trainer **v2.2.1** — it pins `kubeflowTrainerVersion = "v2.2.1"` and its `setup status` reports the 2.2.0 that the registry defaults to as unsupported. The expectation did not move between v0.2.0 and v0.8.2. No functional break is known between the two versions: the CRD delta is documentation text plus one embedded PodSpec field NVCRE does not set. Aligning the global Trainer default is tracked separately.

### Cluster-scoped objects and release ownership

The chart creates cluster-scoped objects with fixed names: four `LogProfile` resources (`megatron-bridge`, `megatron-training`, `nccl-bandwidth`, `nccl-loopback`), 24 `ClusterRole`s, and two `ClusterRoleBinding`s. `fullnameOverride: nvcre` in the shipped values is what fixes the RBAC names, and `LogProfile` names are fixed by the chart regardless. Every one carries Helm ownership annotations for the release `nvcre` and none carries `helm.sh/resource-policy`. Because of that, a Helm release owns them for its whole lifetime:

- **Upgrade** keeps them in place. A revision 1 to revision 2 upgrade left the same four `LogProfile`s, 26 RBAC objects, and seven CRDs.
- **A second release** with the shipped values cannot coexist, even in another namespace. Installing it fails before creating anything, with Helm reporting that the first cluster-scoped object it reaches (`ClusterRole nvcre-bandwidthmeasurement-admin-role`) already belongs to release `nvcre`. The `LogProfile` names carry no release prefix, so changing `fullnameOverride` to rename the RBAC objects would leave those names to collide; that case was not tested.
- **Uninstall** deletes the `LogProfile`s and all RBAC objects, and leaves the seven CRDs behind, as Helm does for anything in a chart's `crds/` directory. The `nccl-bandwidth` `LogProfile` is what the NCCL check's parse rules come from, so a cluster that has uninstalled the release has the CRDs but no profile.
- **Editing a shipped `LogProfile`** changes what the next upgrade does, and the answer depends on the Helm client. The v0.7.0 chart adds a `networkTransport` pattern to `nccl-bandwidth`, which the NCCL check's transport assertion relies on. With Helm 3.17.1, an edit to `spec.patterns.bandwidthResult.example` survived a v0.6.0 to v0.7.0 upgrade and the new pattern was still added. With Helm 4.3.0, which applies server-side, the same upgrade failed with `UPGRADE FAILED: conflict occurred while applying object /nccl-bandwidth` naming the editing field manager, and the profile was left without `networkTransport`. Re-running with `helm upgrade --force-conflicts` succeeded, added `networkTransport`, and reverted the edit to the chart's value. The `LogProfile` schema is closed: a pattern name the chart does not define is rejected at edit time with `unknown field`, so only the fields the chart sets can be edited.
- **Reinstall** after an uninstall works through the bundle's `apply-crds.sh`, which detects the remaining CRDs ("no release but CRDs remain") and updates them before the install. The `LogProfile`s and RBAC objects come back identical to their pre-uninstall state.

This was observed on EKS (Kubernetes v1.35) with the Helm deployer, chart v0.7.0, and Helm 4.3.0. The CRD behavior on the `helmfile` deployer was checked separately on a Kind cluster (Kubernetes v1.36, helmfile 1.7.2, Helm 3.17.1): upgrading the release from v0.6.0 to v0.7.0 with `helmfile sync` succeeded and left the v0.6.0 CRDs in place, so `certifications.nvcre.nvidia.com` still lacked `startupStallTimeoutSeconds`. Running the CRD-apply command below with `CHART=oci://ghcr.io/nvidia/cluster-readiness-engine`, `VERSION=v0.7.0`, and `MANAGER=helm` added the field, and the controller stayed Running throughout. The `LogProfile` edit test ran on the same Kind cluster with the chart installed directly by Helm, not through a generated bundle, and an upgrade across Helm clients (installed with Helm 3, upgraded with Helm 4) was not tried. Not exercised: the `terraform`, `argocd`, `argocd-helm`, and `flux` deployers, and an uninstall while a `Certification` is still running.

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

## EBS CSI Driver Credentials

The `aws-ebs-csi-driver` controller calls the Amazon EBS API to create, attach, and delete volumes. AICR installs the driver but does not give it AWS credentials: the stock values leave `ebs-csi-controller-sa` in `kube-system` without an IAM role annotation. The cluster must supply EBS permissions through one of these paths:

- **EKS Pod Identity.** Create a Pod Identity association for `kube-system/ebs-csi-controller-sa`. The association lives in EKS, outside the Helm release, so it needs no bundle override. It delivers credentials only through the [EKS Pod Identity Agent](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-agent-setup.html), which AICR does not install, so install or verify the agent first.
- **IAM roles for service accounts (IRSA).** Annotate the controller ServiceAccount with the role ARN at bundle time. The annotation key contains dots, so pass it as a JSON object:

  ```shell
  aicr bundle -r recipe.yaml \
    --set-json awsebscsidriver:controller.serviceAccount.annotations='{"eks.amazonaws.com/role-arn":"arn:aws:iam::<account>:role/<ebs-csi-role>"}'
  ```

- **Node instance role.** Attach the permissions to the node IAM role. The controller must then reach the instance metadata service for credentials, and because it does not use host networking, IMDSv2 needs a hop limit of at least 2. Upstream does not recommend this path for production, because every pod that can reach IMDS inherits the permissions.

The default EKS node role does not include EBS permissions, so a cluster with none of these paths installs the driver cleanly and then cannot provision any volume. For the required permissions, use the upstream [driver permissions guide](https://github.com/kubernetes-sigs/aws-ebs-csi-driver/blob/v1.59.0/docs/install.md#set-up-driver-permissions) and the AWS [Amazon EBS CSI driver](https://docs.aws.amazon.com/eks/latest/userguide/ebs-csi.html) page rather than a policy name copied from here; AWS maintains more than one managed policy for the driver.

The `aws-ebs-csi-driver` health check verifies only that the controller Deployment has an available replica and that no driver pod is in an unhealthy state such as `Pending` or `CrashLoopBackOff`. It does not exercise the EBS API, so it passes on a cluster with no credential path.

### Troubleshooting provisioning failures

On the stock EKS overlay, `kube-prometheus-stack` creates a PVC for Prometheus, so a missing credential path usually surfaces there first as a failing `kube-prometheus-stack` health check: the Prometheus StatefulSet never becomes ready because its PVC stays `Pending`. Any other PVC bound to `ebs-csi-default-sc` fails the same way.

A `Pending` PVC alone does not identify the cause. Confirm it from the driver's own diagnostics:

```shell
NAMESPACE="monitoring"  # replace with the namespace of the Pending claim
NAME="my-claim"         # replace with the Pending claim's name

# Provisioning events on the stuck claim
kubectl describe pvc "${NAME}" -n "${NAMESPACE}"

# The sidecar that issues CreateVolume, and the driver that calls the EBS API.
# The controller runs two replicas and only the leader provisions, so read both pods.
kubectl logs -n kube-system deploy/ebs-csi-controller -c csi-provisioner --all-pods=true --prefix --tail=100
kubectl logs -n kube-system deploy/ebs-csi-controller -c ebs-plugin --all-pods=true --prefix --tail=100
```

Credential errors in the `ebs-plugin` log — no credential provider found, an unauthorized operation, or a failed role assumption — confirm a missing or insufficient credential path. Fix the path using one of the options above, then let the provisioner retry; the PVC binds once `CreateVolume` succeeds.

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

AICR qualifies k8s-aibom v1.6.2 as an optional Helm component. It is not in
the base or a mixin. It ships by default in two stock recipes:
`h100-gke-cos-inference`, under [ADR-019](https://github.com/NVIDIA/aicr/blob/main/docs/design/019-k8s-aibom-runtime-inventory.md)'s
stock-adoption amendment, and `h100-gke-cos-inference-dynamo`, which inherits
it from that recipe.

`aicr recipe --runtime-inventory <mode>` selects the component at generation
time and records the choice in the emitted recipe as
`configuration.runtimeInventory.mode`:

| Resolved recipe, before the flag | `--runtime-inventory enabled` | `--runtime-inventory disabled` |
|---|---|---|
| Declares the component | Confirms the existing selection | Removes it (`install: false`) |
| Declares it with `install: false` — an explicit decline | **Rejected**, on any service — a decline is not overridable from the CLI | Confirms the existing decline (no-op) |
| Neither declares nor declines it, service `gke` | **Grants** the component ([#2962](https://github.com/NVIDIA/aicr/issues/2962)) | Rejected — nothing to disable |
| Neither declares nor declines it, any other service | Rejected — nothing to enable | Rejected — nothing to disable |

Grant it on any GKE recipe that does not already mention it:

```bash
aicr recipe --service gke --accelerator a100 --os cos --intent training \
  --runtime-inventory enabled -o recipe.yaml
```

The grant adds the same componentRef `h100-gke-cos-inference` declares by
hand — name, type, and `valuesFile: components/k8s-aibom/values.yaml` — with
chart, repository, and version filled in from the registry, so a granted
recipe and a declaring one render the same artifact. When the recipe enables
`dynamo-platform` or `k8s-nim-operator`, the granted ref also depends on it,
for the ordering reason below. It is scoped to `gke`
because that is the footprint qualified for the widened adoption; the same
flag against a non-GKE recipe that does not already declare the component is
rejected exactly as it always has been.

The GKE Dynamo recipes carry it as well. `h100-gke-cos-inference-dynamo`
inherits it from `h100-gke-cos-inference`, so it ships there by default;
`b200-gke-cos-inference-dynamo`, `gb200-gke-cos-inference-dynamo`, and
`gb300-gke-cos-inference-dynamo` receive it through the opt-in. The pairing
with `grove` and `dynamo-platform` rests on upstream's v1.6.1 run, with the
topology caveats noted in the evidence below.

On the GKE Dynamo recipes, by default or by opt-in, k8s-aibom deploys after
`dynamo-platform`. The
controller discovers the workload kinds it watches when it starts, so started
before the `DynamoGraphDeployment` CRDs exist it would not inventory graphs
until it restarted. Upstream's run reached the same state by restarting the
controller after installing Dynamo; AICR gets it from the deployment order.

The order covers a fresh install. If k8s-aibom was already running when
`dynamo-platform` was first installed, for example on a cluster moving from
`h100-gke-cos-inference` to its Dynamo variant, Helm does not restart the
controller, so restart it once after `dynamo-platform` is ready:

```bash
kubectl rollout restart deployment/k8s-aibom -n k8s-aibom-system
```

A recipe that declines the component with `install: false` outranks the
opt-in flag rather than the other way around. No stock recipe declines it,
but a custom overlay can:

```console
$ aicr recipe --service gke --accelerator h100 --os cos --intent inference \
    --data ./my-recipes --runtime-inventory enabled
[INVALID_REQUEST] component "k8s-aibom" is disabled by the recipe and cannot
be re-enabled with --runtime-inventory enabled; remove the override in the
recipe or select a recipe that enables it
```

To add it to a non-GKE recipe, or without depending on
`--runtime-inventory` at all, add this reference to a custom or external
overlay instead and keep that overlay's criteria as narrow as the intended
rollout:

```yaml
spec:
  componentRefs:
    - name: k8s-aibom
      type: Helm
      valuesFile: components/k8s-aibom/values.yaml
      # Only when the recipe also carries an operator whose CRDs k8s-aibom
      # watches; naming a component the recipe lacks fails resolution.
      dependencyRefs:
        - dynamo-platform
```

A broad criteria overlay affects every matching recipe. In particular,
`intent: any` is universal across intents; do not use it unless that injection
is deliberate. The in-tree `recipes/overlays/monitoring-hpa.yaml` overlay shows
that broad reach: its `criteria: intent: any` attaches to every matching intent.
See [Recipe Development](../integrator/recipe-development.md) for external data
and criteria composition.

Decline it at generation time with `aicr recipe --runtime-inventory disabled`,
described below.

The qualified artifacts are source tag `v1.6.2` at commit
`47bf6418e40eee22905973528b879dc075181b3f`, OCI chart
`oci://ghcr.io/googlecloudplatform/charts/k8s-aibom:1.6.2` at
`sha256:26563c1a6450ba084a8c2e9d0ce3246d0b3c9974ed331105c573df6d058a442f`,
and the controller image pinned by digest in the component values
(`sha256:5353a40010c093d31e7c8b7dc099ed54d19bc79b76f4ba71963fb44c5db7a2f3`).
v1.6.2 is v1.6.1 plus eight fixes, with the same CRDs and RBAC. Two of them
change what the controller reports: its schema-skew check now covers nested
fields, and its watch probe now sees a conversion-webhook outage. The v1.6
line inventories `DynamoGraphDeployment`, `DynamoComponentDeployment`, `NIMService`,
`LeaderWorkerSet`, `DisaggregatedSet` and `CronJob` workloads, and rolls a
workload owned by another tracked kind into its owner's AIBOM instead of
reporting it separately. Its ClusterRole gains read-only access to those kinds
and to the Grove kinds the Dynamo roll-up walks. Those kinds are discovered
when the controller starts: a kind whose CRD is absent is skipped, not an
error, and a CRD installed later is not watched until the controller restarts.
The companion `k8s-aibom-crds` chart that the v1.6
releases also publish is not used: AICR keeps the main chart's `crds/`
directory and its own CRD handling. v1.3.0 remains the
API-graduation release: both `v1alpha1` and `v1beta1` are served and CRD
storage is on `v1beta1`, a configuration unchanged through v1.6.2. What did
change: the chart's own rendered `AIBOMControllerConfig` resource moved from
`v1alpha1` (v1.3.0) to `v1beta1` (v1.5.1 onward); both versions stay served
throughout, so this is a template change, not an API break — see
[Health and readiness](#health-and-readiness) below for what that means for
the health check's own assertion. Upstream states Kubernetes support as a
policy rather than a fixed range: stable APIs only, no known version ceiling,
tested floor 1.27, backed by a weekly CI matrix. The authoritative statement
is
[upstream's compatibility policy](https://github.com/GoogleCloudPlatform/k8s-aibom/blob/main/docs/compatibility.md),
which is linked rather than restated here so it cannot drift out of date on
our side. That link deliberately tracks `main`: the point is the current
policy, not a snapshot of it, which is the opposite of how this page cites
qualified artifacts.

AICR observed the dedicated integration test (`make k8s-aibom-test`) passing
against v1.3.0 on its Kind 1.36.1 node image at initial qualification; that is
qualification evidence for the original ADR-019 adoption, not an extension of
upstream's support statement. The v1.3.0 → v1.5.1 transition itself is
verified by CRD schema diff and a clean `aicr bundle` render — an
additive-only CRD change with served/storage configuration unchanged — and
was then executed end to end on a live GKE cluster (`v1.35.6-gke.1250000`):
install v1.3.0, upgrade to v1.5.1, with the shipped health check passing
against the upgraded cluster. See the component's
[upgrade record](https://github.com/NVIDIA/aicr/blob/main/recipes/components/k8s-aibom/upgrades.yaml)
for that evidence and the steps it produced.

Upstream then measured what that run did not, on a regional GKE cluster
(`v1.35.8`) at 1,002 tracked workloads, 1,001 of them Deployments scaled to
zero replicas: API-server cost, rollback, and the resource envelope
([evidence](https://github.com/GoogleCloudPlatform/k8s-aibom/blob/c08e9ac2fda11a339865abdf2a7339a5a34c59e2/docs/evidence/v1.5.1-gke-upgrade-rollback-apiserver.md)).
With that idle inventory the controller made about one API-server request per
minute at steady state, all watch reconnects and no writes, at 48MiB working
set. A cluster with running pods and pod churn was not measured. A `helm rollback`
from v1.5.1 to v1.3.0 reached Ready in 17-19s with the CRDs in either state;
with the v1.5.1 CRDs left in place, every AIBOM kept its input and BOM hashes
across the round trip.

The v1.5.1 → v1.6.1 transition is verified by CRD schema diff and a rendered
chart diff: one additive field nested under the controller config
(`spec.discovery.workloadKinds`), served and storage configuration unchanged,
and only read-only ClusterRole additions. Upstream then ran it on GKE with
AICR's values, at AICR's request
([evidence](https://github.com/GoogleCloudPlatform/k8s-aibom/blob/d2b5b683d58ee9e71cc95bd4d7272676c4b54d11/docs/evidence/v1.6.1-gke-adr019-dynamo.md)):

- **Upgrade and rollback.** `helm upgrade` from v1.5.1 with the v1.5.1 CRDs
  left in place deployed in 24s with the controller Ready and not `Degraded`.
  `helm rollback` to v1.5.1 with the v1.6.1 CRDs left in place reached Ready in
  18s. Each AIBOM regenerates once in each direction.
- **Dynamo.** With dynamo-platform 1.4.2 (the version AICR pins, whose
  `DynamoGraphDeployment` storage is still `v1alpha1` behind a conversion
  webhook), one multinode graph produced exactly one AIBOM, owned by the
  graph, with its Grove `PodCliqueSet`, cliques and pods rolled up into it.
  The run used the chart's bundled Grove and etcd on CPU-only nodes, not
  AICR's standalone `grove` component or GPU topology.
- **Dynamo operator down.** With the operator scaled to zero, the API server
  kept serving `DynamoGraphDeployment` lists and watches from its cache, so
  existing AIBOMs stayed correct and new Deployments were still inventoried.
  v1.6.1 reported nothing: no `Degraded` condition and no event. Graph
  writes are refused while the webhook is down, so there is no change to miss.
  v1.6.2's watch probe also reads one object by name, which the API server
  serves through the webhook, so upstream reports the outage now shows as
  `Degraded=True` within one two-minute probe interval. AICR has not
  reproduced that with Dynamo.
- **Cost.** With the Dynamo watches on, two graphs whose pods could not
  schedule, and `metrics.enabled=true` to read the counters: 3.9 API requests
  per minute, 2.2m CPU and 28-34MiB working set.
  11 of the 59 requests in the 15-minute window were status writes, re-enqueued
  by the Dynamo operator updating graph status while the pods stayed Pending.

The upgrade record for this transition makes the CRD step unconditional on
`helmfile`, `terraform` and `fleet`, because the v1.6 controller reports `Degraded=True` with reason
`SchemaPredatesController` against CRDs still left over from v1.3.0. A jump
from v1.3.0 straight to v1.6.x crosses both `manual` records, so `aicr upgrade-check`
tells it to stop at v1.5.1 first.

v1.6.2 ships v1.6.1's CRDs unchanged, and its chart rendered with AICR's
values differs only in version labels and the image digest. No AICR release
pinned v1.6.1, so the record for that boundary reaches v1.6.2: moving from a
release that pinned v1.5.1 is one `manual` step with the same CRD step, not a
stop at v1.6.1. A cluster already on v1.6.1 moves to v1.6.2 under a `safe`
record. AICR ran both on Kind (Kubernetes v1.37.0) with AICR's values:

- **From v1.6.1.** `helm upgrade` took 16s; the controller came back Ready and
  not `Degraded`, the fixture AIBOM kept its input and BOM hashes, and the
  health check passed. `helm rollback` to v1.6.1 took 11s; the controller came
  back Ready and not `Degraded`, and the AIBOM kept its hashes.
- **From v1.5.1, CRDs applied first.** The upgrade took 17s, Ready and not
  `Degraded`. `helm rollback` to v1.5.1 with the v1.6.2 CRDs left in place
  took 13s and regenerated the AIBOM back to its v1.5.1 input hash.
- **From v1.5.1, CRD step skipped.** The upgrade still succeeded, the pod stayed
  Ready and the health check passed, but `AIBOMControllerConfig/default`
  reported `Degraded=True` with reason `SchemaPredatesController` naming
  `discovery.workloadKinds`. Applying the v1.6.2 CRDs and restarting the
  controller cleared it.

One thing is deliberately **not** covered. The health check's
`AIBOMControllerConfig` assertion still targets `v1alpha1` rather than the
`v1beta1` the chart now renders (see [Health and readiness](#health-and-readiness)).

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

**That assertion only catches a missed CRD step when the transition actually
moves the storage version.** v1.3.0 through v1.6.2 all store `v1beta1`, so
a cluster stranded on older CRDs after a bump passes the check while missing
the new schema — measured on GKE after a v1.5.1 bump, such a cluster reports
the release deployed, the controller `1/1` Ready on the new image, and
`storedVersions` `v1beta1` on both CRDs. To confirm the v1.6 CRDs
specifically, look for the field they add rather than the storage version:

```bash
kubectl get crd aibomcontrollerconfigs.aibom.k8saibom.dev \
  -o jsonpath='{.spec.versions[?(@.name=="v1beta1")].schema.openAPIV3Schema.properties.spec.properties.discovery.properties.workloadKinds.type}'
# expect: array   (empty means an older schema is still in place)
```

From v1.6.0 the controller also reports a stale schema itself:
`AIBOMControllerConfig/default` carries `Degraded=True` with reason
`SchemaPredatesController` while a field it knows is missing from the served
schema, and stays Ready. v1.6.0 and v1.6.1 check top-level fields only, so they
catch a missing `spec.verification` (added in v1.5.0) but not the nested
`workloadKinds`. v1.6.2 checks nested fields too and names
`discovery.workloadKinds` on CRDs left over from v1.5.1. The command above
works on every version.

`k8s-aibom` is marked `ownsCRDs` in the registry, so most deployers update its
CRDs for you: Flux through `spec.upgrade.crds: CreateReplace`, `helm` through
the generated `apply-crds.sh`, and Argo CD by applying them as ordinary
manifests each sync. `helmfile`, `terraform` and `fleet` have no equivalent automation — see
[Upgrade, uninstall, and troubleshooting](#upgrade-uninstall-and-troubleshooting)
for the manual step. Whether a given bump needs that step is answered per
transition by the upgrade record. A v1.3.0 to v1.5.1 bump on default values
upgrades cleanly on `helmfile`, `terraform` and `fleet` against the old CRDs,
but the same bump setting `config.verification` aborts the release with
`.spec.verification: field not declared in schema` after the Deployment has
already advanced. The v1.5.1 to v1.6.x bump needs the step unconditionally:
on CRDs still from v1.3.0 the new controller reports `Degraded` (on v1.6.2,
also on CRDs from v1.5.1), and setting `config.discovery.workloadKinds` needs
the v1.6 schema. v1.6.1 to v1.6.2 changes no CRD. When in doubt, apply
the CRDs first; it is never harmful.

**That automation is tied to the registry-pinned coordinates, not to the
component.** `ownsCRDs` records an audit of one specific chart, so Flux and
`helm` both check that the componentRef still resolves to the registry's
`source`, `chart`, and `version` before acting, and do nothing when either is
overridden. A recipe that overrides the version — including the override
described under [Overriding the chart version](#overriding-the-chart-version-requires-overriding-this-assertion)
below — therefore upgrades the controller with **no** CRD update on those two
deployers, silently. Such a recipe needs its own audit of the chart it points
at and its own CRD step; the fallback command below is the manual form. Argo CD
is unaffected, since it applies whatever CRDs the rendered chart contains
regardless of provenance. `helmfile`, `terraform` and `fleet` are also unaffected
by this particular caveat, in the sense that there is nothing to disable: none
of them acts on `ownsCRDs`, checked or not, so no version override can take
their automation away. Whether a given bump actually needs the manual step is a separate
question, answered per transition by the upgrade record.
The assertion is still worth making on every deployer, because it proves the
deployed CRDs match the pinned chart rather than merely that some deployer was
expected to update them.

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
storage-version boundary, such as the 1.2.0 to 1.3.0 move — the last one this
pin crossed — and does not catch a stranded upgrade within one, such as 1.0.0
to 1.2.0, or the 1.3.0 to 1.5.1 and 1.5.1 to 1.6.1 moves this pin later made,
which stayed on the `v1beta1` side of that boundary.

**The `AIBOMControllerConfig` assertion is deliberately asymmetric with the
CRD one above.** The controller-configuration check asserts
`apiVersion: aibom.k8saibom.dev/v1alpha1` on `AIBOMControllerConfig/default`,
even though the pinned v1.6.2 chart itself renders that object at `v1beta1`
(v1.3.0 rendered it at `v1alpha1`; see [qualified artifacts](#k8s-aibom-runtime-inventory)
above). That is not stale: `v1alpha1` remains served, so the assertion still
resolves — but now through the API server's conversion path rather than by
matching the object's actual rendered version. Moving the assertion to
`v1beta1` needs live-cluster confirmation that the conversion path behaves
identically to serving the rendered version directly; until that evidence
lands from a GKE UAT lane, the assertion stays at `v1alpha1`.

**Declining the component.** `h100-gke-cos-inference` and
`h100-gke-cos-inference-dynamo` install `k8s-aibom` by default. Decline it at
generation time:

```bash
aicr recipe --service gke --accelerator h100 --os cos --intent inference \
  --runtime-inventory disabled -o recipe.yaml

# The Dynamo recipe, which inherits the component:
aicr recipe --service gke --accelerator h100 --os cos --intent inference \
  --platform dynamo --runtime-inventory disabled -o recipe.yaml
```

The same flag works for a recipe that adds the component through a custom
overlay — the shape shown above. Point `--data` at the directory holding that
overlay:

```bash
aicr recipe --service gke --accelerator h100 --os cos --intent inference \
  --data ./my-recipes --runtime-inventory disabled -o recipe.yaml
```

Passing `--runtime-inventory disabled` against a recipe that does not declare
the component is an error, not a silent no-op. Training recipes do not, so:

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

#### Overriding the chart version requires overriding this assertion

Assert content is static YAML with no templating, so the expected storage version is
a literal tied to the registry's pinned chart, currently `v1beta1` for chart
1.6.2. Charts 1.2.0 and earlier declare only `v1alpha1`. A recipe that sets
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
cluster-wide, and from v1.6 also `CronJob`, `DynamoGraphDeployment`,
`DynamoComponentDeployment`, `NIMService`, `LeaderWorkerSet`,
`DisaggregatedSet` and Grove objects wherever their CRDs exist—including
image references, arguments, and inline environment
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

**CRDs are applied for you on every deployer except `helmfile`, `terraform` and `fleet`.** The chart
ships its CRDs under `crds/`. Helm installs that directory on first install and
never touches it again on upgrade, so a chart bump whose CRDs changed would
leave the previous schema in place and the API server would silently prune the
new controller's writes to added fields.

Every other deployer closes that on its own, by a different route; see the
deployer table below. `helm` bundles carry an `apply-crds.sh` in the
component's folder, run automatically before the upgrade; `flux` and Argo CD
apply the CRDs through their own controllers. `helmfile`, `terraform` and
`fleet` have no automated equivalent: the manual command below is always required for their
`ownsCRDs` components.

When you run the command below depends on your deployer, because only some of
them do it for you:

- **`helmfile`, `terraform` and `fleet`: always, including from a generated bundle.**
  Nothing runs it for you on these deployers, so it is a required step before
  every `ownsCRDs` upgrade, not a fallback. Under `terraform`, run it before
  the `terraform apply` of the regenerated bundle. Under `fleet`, run it
  against every downstream cluster the bundle targets before applying the
  regenerated bundle.
- **`helm`: only** when you are upgrading outside a generated bundle, or when
  the bundle's `apply-crds.sh` failed and you are reproducing it by hand.
- **`flux`, `argocd`, `argocd-helm`: not needed.** Their controllers apply the
  CRDs themselves.

```bash
set -euo pipefail

CHART="oci://ghcr.io/googlecloudplatform/charts/k8s-aibom"
VERSION="1.6.2"   # replace with the version you are upgrading to
# The classic Helm repository the chart is served from. Leave it empty for an
# oci:// CHART. For psmdb-operator:
# REPO="https://percona.github.io/percona-helm-charts/"
REPO=""
# The field manager that installed the CRDs: "helm" under the helm and
# helmfile deployers (both run the helm CLI), and "terraform-provider-helm"
# under terraform, where the provider installs the release. Check with:
#   kubectl get crd <name> --show-managed-fields -o jsonpath='{.metadata.managedFields[*].manager}'
MANAGER="helm"
# The component's ownsCRDsExcludeSubcharts, space-separated. For nvsentinel:
# EXCLUDE_SUBCHARTS="mongodb-store", whose Percona CRDs psmdb-operator owns.
EXCLUDE_SUBCHARTS=""

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# Every step below is checked explicitly rather than left to `set -e`. This
# block gets pasted into a shell, often only in part, and a copy that loses
# the `set -e` line would otherwise carry on past a failure in silence.
if ! helm pull "${CHART}" ${REPO:+--repo "${REPO}"} --version "${VERSION}" --destination "${work}"; then
  echo "ERROR: helm pull failed for ${CHART} ${VERSION}" >&2
  exit 1
fi
if ! tar -xzf "${work}"/*.tgz -C "${work}"; then
  echo "ERROR: could not extract the chart archive" >&2
  exit 1
fi

# Collect first, so discovering nothing is an error rather than a loop that
# runs zero times and exits 0.
excluded=()
for sub in ${EXCLUDE_SUBCHARTS}; do
  excluded+=(-not -path "*/charts/${sub}/*")
done
crds=()
while IFS= read -r crd; do
  crds+=("${crd}")
done < <(find "${work}" -type f -path '*/crds/*' ${excluded[@]+"${excluded[@]}"} \( -name '*.yaml' -o -name '*.yml' \) | sort)

if [ ${#crds[@]} -eq 0 ]; then
  echo "ERROR: no CRDs found under crds/ in ${CHART} ${VERSION}" >&2
  exit 1
fi

# One kubectl call per CRD file, server-side applied under the installer's
# field manager. The notes below say which removed fields this prunes.
for crd in "${crds[@]}"; do
  grep -q '[^[:space:]]' "${crd}" || continue
  if ! kubectl apply --server-side --force-conflicts --field-manager="${MANAGER}" -f "${crd}"; then
    echo "ERROR: failed to apply ${crd}." >&2
    echo "       Stopping: the remaining CRDs are unapplied, so do not upgrade" >&2
    echo "       the controller until this is resolved." >&2
    exit 1
  fi
done
```

Four details are load-bearing, and each obvious alternative loses something:

- **Server-side apply under the field manager that installed the CRDs, not
  `kubectl replace` or a `kubectl apply --server-side` under kubectl's own
  default manager.** `kubectl replace` does update an existing CRD, because
  kubectl copies the live `metadata.resourceVersion` into the manifest before
  sending it, but it overwrites the whole object: anything another manager
  set on the CRD after install, such as a GitOps label, is discarded.
  Server-side apply keeps a field another manager set unless the chart's CRD
  sets that same field, in which case `--force-conflicts` takes it over. What
  the apply removes depends on where the new chart dropped something:
  - **Inside `spec.versions`**, which holds every version's schema, a dropped
    version or schema property disappears under any field manager once the
    apply succeeds: the list is atomic, so a forced apply replaces it whole.
    Without `--force-conflicts`, an apply under any manager other than the
    list's owner fails with a conflict on `.spec.versions` and changes
    nothing. The exception is a version still listed in the CRD's
    `status.storedVersions`. The API server rejects that apply and the
    command stops, until a storage migration removes the version from the
    list.
  - **Outside `spec.versions`**, such as `spec.names.shortNames` or an
    annotation, a dropped field disappears only when the applying manager
    owns it through an Apply operation and no other manager owns it too.
    Another owner, even one that set the same value, keeps the field in
    place, and `--force-conflicts` does not change that. Helm 4 installs
    CRDs through Apply, as manager `helm`, so `MANAGER="helm"` removes such a
    field where a forced apply under the default manager would leave it.
    Helm 3 and `terraform-provider-helm` (measured on a kind cluster with
    provider 3.3.0) install with an Update operation, which server-side apply
    tracks as a separate owner even under the same manager name, so after
    either a field the original install set stays behind whatever `MANAGER`
    says. A field that an earlier run of this command added is owned through
    Apply and is removed normally, as long as every run uses the same
    `MANAGER`. If the chart's release notes depend on a removal, check the
    field's owners with `--show-managed-fields` and delete it by hand if it
    remains.

  `--force-conflicts` is required because other managers may hold individual
  fields. On a CRD using `spec.conversion.strategy: Webhook`, a chart that
  sets `caBundle`, even to a placeholder, would overwrite the one injected at
  runtime, and a replace discards it whatever the chart sets. That is why
  `ownsCRDs` requires that no CRD use webhook conversion.
- **Read the CRDs from the chart archive, not from `helm show crds`.** That
  command's output shape differs by major version: Helm 4 prepends `---` before
  every CRD, Helm 3 prepends one only for `show all` and emits nothing between
  documents. Any separator-based filter silently yields nothing on Helm 3.
- **Pull once and work from that archive.** Repository, chart, and version are
  coordinates, not content. Reading CRDs through them and letting the upgrade
  resolve them again is two fetches, and a mutable tag does not promise the same
  bytes.
- **Fail closed.** `set -euo pipefail` plus collecting the file list before the
  loop is what makes a failure stop the upgrade instead of reading as success.
  A `find ... | while read` pipeline reports only the last iteration's status,
  so an earlier failed apply is masked by a later one that succeeds, and a
  chart whose `crds/` is empty or moved runs the loop zero times and exits 0.
  Either way you would proceed to upgrade the controller against a stale or
  partial schema — the exact failure this command exists to prevent, and it
  matters most on `helmfile`, `terraform` and `fleet`, where nothing else covers it.

The generated `apply-crds.sh` does exactly this, with each call bounded; it is
the reference if you need the details.

Which deployers need that step differs, so check yours:

| Deployer | CRD behavior on upgrade | Manual step needed |
|---|---|---|
| `helm` | `helm upgrade` skips `crds/`, so the bundle emits `apply-crds.sh` for components the registry marks `ownsCRDs` and `install.sh` runs it first | Only for components without `ownsCRDs` |
| `helmfile` | Upgrades through Helm, so it skips `crds/` too; no automation exists, because a `presync` hook fires only for releases `helmfile apply` decides to sync, so it would hold on a chart bump and silently not hold on an unchanged rerun | Assume yes — see note below |
| `terraform` | Upgrades through Helm (`helm_release`), so it skips `crds/` too; the bundle emits no CRD step, because the provider has no CRD-upgrade option and a `local-exec` step would run outside the plan; run the manual command with `MANAGER=terraform-provider-helm`, the manager the provider installed the CRDs under | Assume yes — see note below |
| `flux` | The generated `HelmRelease` sets `spec.upgrade.crds: CreateReplace` for components the registry marks `ownsCRDs`, and leaves the helm-controller `Skip` default in place for the rest | Only for components without `ownsCRDs` |
| `argocd`, `argocd-helm` | Argo CD renders the chart with CRDs included and applies them as ordinary manifests each sync | No |
| `fleet` | Fleet's agent upgrades through Helm, so it skips `crds/` too, and a Fleet bundle cannot run `apply-crds.sh`; the bundle README and deployment notes name the `ownsCRDs` components instead; run the manual command with `MANAGER` set to the manager the managed-fields check reports for the installed CRDs | Assume yes — see note below |

**On the `helmfile`, `terraform` and `fleet` "assume yes".** None of these
deployers ever updates CRDs for you, so the deployer itself can never narrow the step — that much is unconditional. Whether
a *particular* version bump actually needs it is a different question, and the
only thing that can answer it is the component's ADR-021 upgrade record for
that transition. Some transitions genuinely do not need it: the k8s-aibom
v1.3.0 to v1.5.1 bump upgrades cleanly on stale CRDs at default values, and
needs the manual apply only when the same upgrade sets `config.verification`.
Absent a record that says so, assume the step is required — applying CRDs that
were already current is harmless, while skipping a needed apply fails the
release partway through, after the workload has already rolled.

Argo CD is the one deployer that upgrades CRDs for *every* component rather
than only the opted-in ones, because including them is how it renders a Helm
source at all. Suppressing that per component is not available: `skipCrds`
would also drop the CRDs on first install.

`ownsCRDs` is opt-in, and narrow on purpose. Most registry components
that ship CRDs under `crds/` share at least one CRD with another
component: `nfd`, `gpu-operator`, and `network-operator` all ship the
NodeFeature CRDs, and `nfd`, `gpu-operator`, and `kai-scheduler` all appear
together in `base.yaml`. If every release replaced CRDs on upgrade, two or
three releases would rewrite the same CRD on every reconcile or redeploy,
each with the schema its own chart pins. Requiring the opt-in is what
prevents that, so it stays opt-in.

A component qualifies only if it solely owns every CRD it ships and ships none
using `spec.conversion.strategy: Webhook`, since forcing or replacing the CRD
can overwrite a `caBundle` injected at runtime. `kubeflow-trainer` is
excluded for that second reason.
Currently `gatekeeper`, `k8s-aibom`, `nvcre`, `nvsentinel`, and
`psmdb-operator` qualify; the audited chart version for each is pinned in
`pkg/recipe/ownscrds_audit_test.go`, so bumping a pin without re-auditing fails
CI. `nvsentinel` excludes the Percona CRDs its `mongodb-store` subchart embeds
(`ownsCRDsExcludeSubcharts`), since `psmdb-operator` owns them.

Uninstall in this order. Removing the component from the overlay and applying a
regenerated bundle does **not** remove the previously installed release: the
`helm` and `helmfile` deployers install releases by name, and a release the new
bundle no longer mentions is simply left alone. `terraform` and Fleet in
`gitrepo` mode are the exceptions: Terraform holds the release in state and
Fleet deletes a Bundle its repository no longer produces, so applying the
regenerated bundle uninstalls the release, and step 2 relies on that. Skipping the explicit uninstall
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

   For Argo CD, delete the owning `Application`; for Flux, the `HelmRelease`;
   under Terraform, `terraform apply` of the regenerated bundle already removed it.

   For Fleet, do not `helm uninstall` the release: Fleet still declares it and
   reinstalls it on the next reconcile. Remove it from Fleet instead, and leave
   the rest of the bundle in place:

   - `--fleet-mode gitrepo`: push the regenerated bundle from step 1 and apply
     its `gitrepo.yaml`. Fleet deletes the Bundle the repository no longer
     produces (`<app-name>-k8s-aibom`), and the agent uninstalls its release on
     every targeted cluster.
   - `--fleet-mode helmop`: `kubectl apply` never deletes, so apply the
     regenerated `helmops.yaml` first (it re-points the next component's
     `dependsOn`), then delete only the removed HelmOp on the management
     cluster:

     ```bash
     # FLEET_NAMESPACE and APP_NAME are the bundle's --fleet-namespace and --app-name.
     kubectl delete helmops.fleet.cattle.io -n "$FLEET_NAMESPACE" "$APP_NAME-k8s-aibom"
     ```

   Do not delete the `GitRepo` or run `kubectl delete -f helmops.yaml`; either
   uninstalls every release in the bundle.

   Confirm the controller Deployment is gone before continuing; for Fleet,
   check each downstream cluster the bundle targets.

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

NAMESPACE="my-namespace"  # replace with the namespace from the listing above
NAME="my-aibom"           # replace with the AIBOM to delete
kubectl -n "${NAMESPACE}" delete aiboms.aibom.k8saibom.dev "${NAME}"

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

## Topology-aware scheduling with Kueue and Topograph

Kueue topology-aware scheduling (TAS) is on by default in the shipped Kueue (0.19.3) but only reads node labels; it does not create them. The `platform=kueue` leaves install [Topograph](https://github.com/dsx-ai-factory/topograph) with the `k8s` engine to write the labels, and wire Kueue's `Topology` and `ResourceFlavor` to them. Recipes without `--platform kueue` are unchanged. `platform` takes a single value, so these leaves cannot be combined with `--platform kubeflow`: there is no recipe that installs Kubeflow Trainer together with Kueue TAS. Kueue itself can queue `TrainJob`s (Trainer v2.0.0 or later, `trainer.kubeflow.org/trainjob` is already enabled in the shipped Kueue config), but topology-aware placement of `TrainJob`s is not covered by Kueue's TrainJob documentation and has not been verified here.

| Leaf | Provider | Topology levels (widest first) |
|------|----------|--------------------------------|
| `h100-kind-training-kueue` | `dra` (reads `nvidia.com/gpu.clique`) | `accelerator.topograph.run/domain`, `kubernetes.io/hostname` |
| `h100-gke-cos-training-kueue` | `gcp` | `tier-2`, `tier-1`, `tier-0`, `kubernetes.io/hostname` |
| `h100-eks-ubuntu-training-kueue` | `aws` | `tier-2`, `tier-1`, `tier-0`, `kubernetes.io/hostname` |

A `Topology` has a fixed level list while Topograph's fabric depth varies per cluster, so each leaf ships the `Topology` (`topology-accelerator.yaml` or `topology-tiers-3.yaml`) matching its provider. A node missing any level label is not usable by TAS. The GKE depth follows the provider docs and has not yet been confirmed against a live cluster.

The leaves ship a `tas-flavor` (`topologyName`, `nvidia.com/gpu.present: "true"` node selector, `nvidia.com/gpu` toleration) and a ClusterQueue backed by it; `default-flavor` stays topology-free. Overriding `manifestFiles` also replaces the health check (inline `healthCheckAsserts`), which asserts the `topologies.kueue.x-k8s.io` CRD, the `Topology` and `tas-flavor`, and an `Active` ClusterQueue — never node labels, because Topograph labels nodes asynchronously after install.

**Kind:** there is no cloud topology API, so the `dra` provider reads each node's existing `nvidia.com/gpu.clique` label and the `k8s` engine publishes it as `accelerator.topograph.run/domain`. The provider also needs the `topograph.run/instance` (the node's own name) and `topograph.run/region` (any consistent value) node annotations: the leaf sets `nodeDataBroker.enabled: false`, so nothing in the recipe writes them, and without them Topograph answers `502 no matching nodes found`, writes no domain labels, and TAS admits nothing while the health checks still pass. The simulated-GPU cluster built by `tests/uat/kind/bootstrap-cluster.sh` (through `setup-gpu-sim.sh`, which holds the labelling code) sets the clique label and both annotations on its four workers (two cliques of two); a stock Kind cluster has none of them. Nodes without the label, such as the control plane, are not TAS-eligible. The chart's `test` provider cannot be used on Kind: Topograph 1.0.0 accepts only a bare built-in fixture name, not a model naming real nodes.

**IAM:** cloud providers call the provider's topology API.

- GKE: grant the Topograph ServiceAccount `roles/compute.viewer` (`compute.instances.list`), for example through Workload Identity (`iam.gke.io/gcp-service-account` annotation on `serviceAccount.annotations`).
- EKS: grant `ec2:DescribeInstanceTopology` through IRSA or EKS Pod Identity.

Without access no labels are written and TAS admits nothing.

Verify the labels, then submit a Job that must land inside one domain. On GKE and EKS every `Topology` level must be present on the GPU nodes (a node missing one is not TAS-eligible); on Kind check `accelerator.topograph.run/domain` instead:

```bash
# GKE / EKS
kubectl get nodes -L fabric.topograph.run/tier-0,fabric.topograph.run/tier-1,fabric.topograph.run/tier-2
# Kind
kubectl get nodes -L accelerator.topograph.run/domain
```

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  generateName: tas-sample-
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: default
spec:
  parallelism: 2
  completions: 2
  completionMode: Indexed
  suspend: true
  template:
    metadata:
      annotations:
        kueue.x-k8s.io/podset-required-topology: fabric.topograph.run/tier-0
    spec:
      restartPolicy: Never
      containers:
        - name: main
          image: docker.io/library/busybox:1.37
          command: ["sleep", "30"]
          resources:
            requests:
              cpu: "1"
              nvidia.com/gpu: "1"
            limits:
              nvidia.com/gpu: "1"
```

The admitted Workload records the placement in `status.admission.podSetAssignments[].topologyAssignment`.

The annotation value must be one of the leaf's `Topology` levels: use `accelerator.topograph.run/domain` on the Kind leaf, and `fabric.topograph.run/tier-0` (or a wider tier) on GKE and EKS. `Topology.spec.levels` is immutable in Kueue, so changing a leaf's levels on a running cluster needs the old objects removed first: delete the LocalQueue, ClusterQueue, `tas-flavor` and `tas-topology` (uninstalling the release leaves them behind), then reinstall the `kueue-post` release.

`tas-flavor` tolerates only the `nvidia.com/gpu` taint, and Kueue injects flavor tolerations only. If your GPU nodes carry another taint (a managed node group labelled `nodeGroup=gpu:NoSchedule`, for example), the Workload is admitted but its pods stay `Pending`; add a matching toleration to the Job's pod template. Check with `kubectl describe node <gpu-node> | grep Taints`.

**Automated verification (Kind):** `tests/uat/kind/verify-kueue-tas.sh` runs the same check against the `aicr-uat-slurm` cluster. It submits a Job sized so no single node can hold it (forcing a spread across nodes) and asserts every pod landed in one Topograph domain from the pods' own `spec.nodeName` and the nodes' `accelerator.topograph.run/domain` labels — never from Kueue's own `topologyAssignment`, so the check cannot pass merely because Kueue says so. It then submits a Job sized so no single domain can hold it and asserts Kueue's `QuotaReserved` condition stays `False`: without this negative control, a passing positive check could mean TAS is unconstrained rather than working, since an unconstrained Job can also land in one domain by coincidence. `tests/uat/kind/verify-kueue-tas_test.sh` covers the pod-to-domain derivation hermetically, with no cluster, and runs under `make test-shell`.

## Upgrade Notes

Migration steps when upgrading from a prior AICR-generated bundle to a newer one that changes how a component delivers its Kubernetes resources.

A generated recipe is a point-in-time artifact of the AICR binary that produced it: the embedded registry, overlays, manifest paths, and chart pins are part of that binary's surface. When upgrading AICR, regenerate the recipe from scratch with the new binary (`aicr recipe ...`) before re-bundling. `aicr bundle --recipe <old-file>` against a newer binary may fail if the saved recipe references manifest paths the new release has moved or removed (see [Bundle Generation Fails](cli-reference.md#bundle-generation-fails) for the specific error).

**Regenerating also re-derives each component's namespace.** The namespace comes from the registry in the binary doing the regenerating, so if a component's default namespace moved between two AICR releases, the new recipe names the new one. Helm cannot move a release between namespaces, so the resulting bundle installs a second copy of the component beside the one already running. Pass `aicr recipe --inherit-from <prior recipe or bundle directory> --inherit=namespace` to preserve populated namespaces while using the new release's chart, source, kustomize path, manifest files and pre-manifest files. Omitting `--inherit`, or setting it to `all`, also restores those identity fields from the prior artifact. A component whose deployment type changed always keeps only its namespace. Empty prior fields leave the current registry defaults unchanged. `--inherit-from` does not compare the artifact with the live deployment, so supply the recipe or bundle directory that matches the deployed release. Then run [`aicr upgrade-check`](upgrading.md#when-a-component-moves-namespace) to review the remaining changes before deployment.

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

### `network-operator`, `network-operator-ocp` and `nfd-ocp`: fabric CRs are no longer Helm hooks (Flux only)

Earlier bundles declared the AKS and OKE `NicClusterPolicy` manifests, the AKS
`NodeFeatureRule`, and the OpenShift `NicClusterPolicy` and
`NodeFeatureDiscovery` as `helm.sh/hook: post-install,post-upgrade` resources
with `helm.sh/hook-delete-policy: before-hook-creation`. Newer bundles ship them
as ordinary release members that Helm updates in place, because deleting a
`NicClusterPolicy` is how the network-operator tears down its OFED and RDMA
pods, and a hook with that delete policy did exactly that on every upgrade.

Only the `flux` deployer crosses a boundary here. `helm`, `helmfile`, `argocd`
and `argocd-helm` strip hook annotations from every local chart they write
(see [Bundling](bundling.md)), so on those deployers the live objects were
release members all along and nothing changes. The `flux` deployer copies recipe
manifests into its generated charts unchanged, so a Flux cluster deployed from an
earlier bundle runs these CRs as hooks today. The hook-created object is not in
the release manifest, and the first upgrade across this change has to adopt it:

| `HelmRelease` | Live objects to adopt | Recipes |
|---|---|---|
| `network-operator-post` | `NicClusterPolicy/nic-cluster-policy`; on AKS also `NodeFeatureRule/nfd-network-rule` | AKS, OKE GB200, OKE L40S |
| `network-operator-ocp` | `NicClusterPolicy/nic-cluster-policy` | OpenShift |
| `nfd-ocp` | `NodeFeatureDiscovery/nfd-instance` in `openshift-nfd` | OpenShift |

The `HelmRelease` objects live in the namespace given by `--flux-namespace`
(`flux-system` by default); substitute it in the commands below. This is not
a component version boundary, so `aicr upgrade-check` does not report it.

Fresh installs apply the new spec directly, and the change does not make the
release wait on a `NicClusterPolicy`. helm-controller waits for release
members to be Ready under `spec.timeout` (5m by default) and remediates a
failed install only by uninstalling, while network-operator v26.7.0 and later
hold `NicClusterPolicy` at `Ready=False` until every component, OFED included,
is up. The `NicClusterPolicy` manifests therefore carry
`aicr/disable-wait: "true"`, and the flux deployer sets `install.disableWait` and
`upgrade.disableWait` on a generated chart that holds such a manifest:
`network-operator-post` and `network-operator-ocp` succeed once the policy is
applied, and `gpu-operator`, which depends on the first, is not held back.
Every other generated chart keeps its wait; `NodeFeatureRule` and the
OpenShift `NodeFeatureDiscovery` report no `Ready` condition, so the wait on
`nfd-ocp` returns at once. Hooks were never waited on, so this matches the
previous behavior; watch the policy itself, not the release, to know when the
fabric is ready:

```bash
kubectl get nicclusterpolicy nic-cluster-policy -o jsonpath='{.status.state}{"\n"}'
```

Whether the adoption applies the new spec depends on how helm-controller
applies the release. `spec.upgrade.serverSideApply` defaults to `auto`, which
reuses the apply method of the previous release:

- A release first installed by helm-controller v1.5.0 or later is server-side
  when its install used server-side apply, which is the default unless
  `spec.install.serverSideApply: false` or `UseHelm3Defaults` is set. Its
  upgrade adopts the object (take-ownership is on unless
  `spec.upgrade.disableTakeOwnership: true`), forces field conflicts, and
  rewrites the live spec from the chart. Nothing to do.
- A release first installed client-side stays client-side under `auto`. For
  custom resources, Helm's client-side upgrade computes a two-way merge patch
  between the recorded release manifest and the new one. The adopted object has
  no recorded manifest to differ from, so the patch is empty: the `HelmRelease`
  reports success while the live `NicClusterPolicy` keeps its old spec,
  including any DOCA/OFED driver version pin, and later upgrades never correct
  it because the object is now in the release manifest and looks current.

For the second case, set `spec.upgrade.serverSideApply: enabled` on each
affected `HelmRelease` before reconciling the first bundle without the hooks.
The field exists from helm-controller v1.5.0, which Flux v2.8.0 ships; Flux
v2.7 ships v1.4.0 and rejects it. A cluster on Flux v2.7 upgrades Flux first,
then reconciles the first bundle without the hooks, with take-ownership left
at its default. AICR does not emit the field, and the bundle's own files are generated
and checksummed, so do not edit them: `aicr verify` fails closed on a modified
bundle, OCI-pushed bundles have no file to edit, and the next bundle regenerates
them anyway. Add the patch to the Flux `Kustomization` you own that applies the
bundle (`aicr-stack` in the bundle's README), which rewrites the `HelmRelease`
as Flux applies it:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: aicr-stack
  namespace: flux-system   # your --flux-namespace
spec:
  # ...
  patches:
    - target:
        kind: HelmRelease
        name: network-operator-post   # or network-operator-ocp, nfd-ocp
      patch: |
        apiVersion: helm.toolkit.fluxcd.io/v2
        kind: HelmRelease
        metadata:
          name: network-operator-post
        spec:
          upgrade:
            serverSideApply: enabled
```

Changing the `HelmRelease` spec alone does not re-run Helm: helm-controller
treats a deployed release as in sync unless its chart, values or
post-renderers change. If the crossing upgrade already ran client-side, force
a one-off upgrade after the patch is in place:

```bash
flux reconcile helmrelease network-operator-post -n flux-system --force
```

The setting is needed once. After an upgrade has applied server-side, `auto`
resolves to server-side apply for that release from then on, so the patch can
be dropped on a later bundle. Confirm the upgrade applied the spec by comparing
a field the template pins against the bundle; a stale value means the upgrade
ran client-side:

| Recipe | Live object | Field | Pinned in |
|---|---|---|---|
| AKS | `NicClusterPolicy/nic-cluster-policy` | `.spec.ofedDriver.version` | `network-operator-post` chart, `nic-cluster-policy-aks.yaml` |
| OKE GB200 | `NicClusterPolicy/nic-cluster-policy` | `.spec.rdmaSharedDevicePlugin.version` | `network-operator-post` chart, `nic-cluster-policy-oke-gb200.yaml` |
| OKE L40S | `NicClusterPolicy/nic-cluster-policy` | `.spec.sriovDevicePlugin.version` | `network-operator-post` chart, `nic-cluster-policy-oke-l40s.yaml` |
| AKS | `NodeFeatureRule/nfd-network-rule` | `.spec.rules[0].matchFeatures[0].matchExpressions.device.value` | `network-operator-post` chart, `nfd-network-rule.yaml` (no version pin; compare the device ID list) |
| OpenShift | `NicClusterPolicy/nic-cluster-policy` | `.spec.ofedDriver.version` | `network-operator-ocp` `HelmRelease`, `spec.values.network-operator-ocp.ofedDriver.version` |
| OpenShift | `NodeFeatureDiscovery/nfd-instance` (`openshift-nfd`) | `.spec.workerConfig.configData` | `nfd-ocp` `HelmRelease`, `spec.values.nfd-ocp.workerConfig.configData` |

For example, on AKS:

```bash
kubectl get nicclusterpolicy nic-cluster-policy \
  -o jsonpath='{.spec.ofedDriver.version}{"\n"}'
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
   or `helmfile apply`; `terraform`: `terraform apply` on a bundle regenerated
   at the new pin; `flux`/`argocd`/`argocd-helm`: reconcile/sync as
   usual). AICR's generated `install.sh` already passes `--force-conflicts`
   on Helm 4.

Verified live end-to-end on a real EKS cluster, 2026-09-02: dynamo-operator,
grove-operator, and kai-scheduler gang-scheduling all confirmed healthy
together post-migration.

### `grove`: `v0.1.0-alpha.12` to `v0.1.0-alpha.13`

alpha.13 fixes DynamoGraphDeployments that stay `SchedulingGated` forever
after scaling up from zero
([ai-dynamo/grove#809](https://github.com/ai-dynamo/grove/issues/809)). The
fix reads a new `status.lastScheduled` field on the `podcliques.grove.io`
CRD. The release also renames the `clustertopologybindings.grove.io` short
name from `ct` to `ctb`.

`helm upgrade` never applies changed `crds/`, and the alpha.12 PodClique
status schema is structural. A plain chart bump therefore keeps the old CRD,
and the API server silently drops `lastScheduled` from every status write.
Apply the new CRDs before upgrading:

1. Apply the alpha.13 CRDs directly from the chart:
   ```bash
   helm show crds oci://ghcr.io/ai-dynamo/grove/grove-charts --version v0.1.0-alpha.13 \
     | sed -n '/^---$/,$p' \
     | kubectl apply --server-side --force-conflicts -f -
   ```
2. Resume whichever deployer you use, as in the section above.

No resource migration is needed, and fresh installs are unaffected.

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
# Replace with every flag from your original aicr bundle command.
ORIGINAL_FLAGS=(--system-node-selector nodeGroup=system-worker)

aicr bundle --recipe recipe.yaml \
  "${ORIGINAL_FLAGS[@]}" \
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

```text
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
image to match the operator. AICR's own inference-perf workload tags its
runtime images with the recipe's `dynamo-platform` chart version, so it
moves with the operator.

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

### `gpu-operator` and `nvidia-dra-driver-gpu`: shared ComputeDomain CRD

`gpu-operator` and `nvidia-dra-driver-gpu` (and `nvidia-dra-driver-gpu-ocp`)
both ship the `computedomains.resource.nvidia.com` CRD. In `gpu-operator`
v26.7.0 the two copies disagreed on schema: the operator's copy required
`spec.numNodes` and carried no default. `gpu-operator` v26.7.1 ships a copy
identical to the DRA driver 0.5.0 chart's, so the two charts no longer
contend.

**Argo CD.** Bundles built with `gpu-operator` v26.7.0 carried an
`ignoreDifferences` entry and `RespectIgnoreDifferences=true` on the
`gpu-operator` `Application` to stop Argo CD reconciling the CRD back and
forth. AICR no longer emits either. An existing Argo CD deployment converges
once the `gpu-operator` `Application` syncs v26.7.1, because both
`Application`s then apply the same CRD. An external `--data` layer that pins
`gpu-operator` back to v26.7.0 alongside a DRA driver brings the reconcile
loop back on Argo CD; move that pin to v26.7.1.

**Helm and Flux.** AICR's Helm and Flux deployments leave an installed
`computedomains` CRD unchanged on upgrade. `gpu-operator` is not marked
`ownsCRDs`, so its Flux `HelmRelease` keeps helm-controller's default
`spec.upgrade.crds: Skip` and the `helm` deployer generates no CRD step for it;
the chart's own CRD upgrade hook (`operator.upgradeCRD`) does not cover
`computedomains`. A cluster first installed with `gpu-operator` v26.7.0
therefore keeps that release's stricter copy after upgrading. Set `spec.numNodes`
explicitly on every `ComputeDomain`, as AICR's own manifests do; `0` is valid
under both copies.

**OpenShift (OLM).** The `gpu-operator-ocp-olm` Subscription tracks the
certified `v26.7` channel, whose v26.7.1 bundle ships the same
`computedomains` schema as the DRA driver chart. Its CSV declares ownership of
`computedomains` and `computedomaincliques`, while `nvidia-dra-driver-gpu-ocp`
also installs `computedomains` from its chart. This pairing has not yet been
verified on a live OpenShift cluster; see
[NVIDIA/aicr#2969](https://github.com/NVIDIA/aicr/issues/2969).

### `mariadb-operator`: `26.3.0` or `26.6.0` to `26.10.1`

`26.10.0` changes the replication configuration rendered by the MariaDB init
container and the replication liveness probe served by the agent, so the data
plane has to move with the operator, and `26.10.1` keeps that requirement.
`updateStrategy.autoUpdateDataPlane` defaults to `false`, so an operator
upgraded without setting it first runs `26.10.1` against init and agent
containers left at the prior version.

The init container and agent are the HA data plane, so they exist only where
Galera or replication is enabled. AICR's accounting database ships as a single
non-HA instance (`galera.enabled: false`, `replicas: 1`), so steps 1 and 5
below are inert for it: the field is accepted and does nothing. They matter for
any HA `MariaDB` the same operator manages. List what you have with:

```bash
kubectl get mariadb -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,GALERA:.spec.galera.enabled,REPLICATION:.spec.replication.enabled
```

Fresh installs are unaffected, and so is a cluster already on `26.10.0`:
upstream's `26.10.1` guide applies only when coming from before `26.10.x`, and
`26.10.1` adds only optional CRD fields. A cluster on a `25.x` release or
earlier is not covered by the steps below: upstream's `25.10.0` guide runs a
replication migration script, and it and the `25.08.0` guide forbid skipping
intermediate versions, so follow upstream's guides up to `26.3.0` first. To
migrate an existing cluster from `26.3.0` or `26.6.0`, set the flag **before**
the operator moves, then upgrade CRDs first and the operator second:

1. Enable data-plane auto-update on every **HA** MariaDB the operator manages,
   substituting each one's name and namespace. AICR's own accounting database
   (`mariadb` in namespace `slurm`) is not HA, so it needs no patch:
   ```bash
   NAME="my-mariadb"      # replace with the HA MariaDB's name
   NAMESPACE="databases"  # replace with its namespace
   kubectl patch mariadb "${NAME}" -n "${NAMESPACE}" --type merge \
     -p '{"spec":{"updateStrategy":{"autoUpdateDataPlane":true}}}'
   ```
   With Argo CD or Flux, do not patch the live resource. Set
   `spec.updateStrategy.autoUpdateDataPlane: true` in the desired configuration
   the application syncs, commit it, and sync it before step 2, then confirm
   the live value with
   `kubectl get mariadb <name> -n <namespace> -o jsonpath='{.spec.updateStrategy.autoUpdateDataPlane}'`.
   A pruning or self-healing sync restores whatever git says, and if git still
   says `false` when the upgraded operator first reconciles an HA `MariaDB`,
   the operator's Galera and replication defaulting keep the old init and
   agent images: the data-plane upgrade is skipped and the waits in step 4
   time out. Keep `true` in git until step 4 passes.
2. Upgrade `mariadb-operator-crds` to `26.10.1` **in place**. With Helm or
   helmfile, confirm which namespace the existing release is in first, because
   Helm scopes a release by namespace: without `--namespace` the request lands
   in whatever namespace the kubeconfig context points at and `--install` does
   not find the existing release. The install it attempts instead fails Helm's
   ownership check, because the existing CRDs carry the original release's
   `meta.helm.sh/release-namespace` annotation. `mariadb-system` is the
   registry default and no overlay overrides it, but an inherited bundle may
   differ.
   ```bash
   # Helm and helmfile only.
   helm list -A | grep mariadb-operator-crds
   helm upgrade --install mariadb-operator-crds \
     oci://ghcr.io/mariadb-operator/charts/mariadb-operator-crds \
     --version 26.10.1 --namespace mariadb-system
   ```
   With Argo CD or Flux, sync the `mariadb-operator-crds` application or
   HelmRelease to `26.10.1` before the operator instead. With Terraform, steps 2
   and 3 are one `terraform apply` of the regenerated bundle: the chart ships its
   CRDs as templates, so the in-place upgrade applies them, and the module graph
   moves the operator only after this release. Never `helm uninstall`, prune,
   delete or `terraform destroy` the CRD release: that deletes the CRDs and
   cascade-deletes every `MariaDB`, `User`, `Database` and `Grant` with them.
3. Upgrade `mariadb-operator` to `26.10.1` (re-run `install.sh`, `helmfile
   apply`, `terraform apply` on the regenerated bundle, or sync the release).
4. For each HA MariaDB enabled in step 1, wait until the `26.10.1` data plane
   is running before continuing. The `Updated` and `Ready` conditions cannot
   show this: the operator computes both against whatever StatefulSet exists,
   so they are already `True` before `26.10.1` first reconciles the resource,
   and a flag reverted at that point leaves `26.10.1` keeping the old init and
   agent images. The waits below cannot pass on that old state. Set the name,
   namespace and replica count, and list one pod per replica, `${NAME}-0` up to
   `${NAME}-<replicas-1>`:
   ```bash
   NAME="my-mariadb"      # replace with the HA MariaDB's name
   NAMESPACE="databases"  # replace with its namespace
   REPLICAS=3             # replace with its replica count
   IMG="ghcr.io/mariadb-operator/mariadb-operator:26.10.1"
   # The StatefulSet renders the new data plane.
   kubectl wait sts "${NAME}" -n "${NAMESPACE}" --timeout=10m \
     --for=jsonpath='{.spec.template.spec.initContainers[?(@.name=="init")].image}'="${IMG}"
   kubectl wait sts "${NAME}" -n "${NAMESPACE}" --timeout=10m \
     --for=jsonpath='{.spec.template.spec.containers[?(@.name=="agent")].image}'="${IMG}"
   # Every pod runs it. One name per replica.
   kubectl wait pod "${NAME}-0" "${NAME}-1" "${NAME}-2" -n "${NAMESPACE}" --timeout=15m \
     --for=jsonpath='{.status.initContainerStatuses[?(@.name=="init")].image}'="${IMG}"
   kubectl wait pod "${NAME}-0" "${NAME}-1" "${NAME}-2" -n "${NAMESPACE}" --timeout=15m \
     --for=jsonpath='{.status.containerStatuses[?(@.name=="agent")].image}'="${IMG}"
   # The roll has finished.
   kubectl wait sts "${NAME}" -n "${NAMESPACE}" --timeout=15m \
     --for=jsonpath='{.status.updatedReplicas}'="${REPLICAS}"
   kubectl wait sts "${NAME}" -n "${NAMESPACE}" --timeout=15m \
     --for=jsonpath='{.status.readyReplicas}'="${REPLICAS}"
   ```
   The operator writes the new images into the `MariaDB` spec before it
   renders the StatefulSet, so once the template carries them step 5 can no
   longer take them back; the pod and replica waits confirm the roll itself.
   A resource that names its own init or agent image, such as a mirror, keeps
   that repository and takes only the `26.10.1` tag, so set `IMG` to match.
   Name the pods rather than selecting them with `-l`: a label selector
   resolves the pod list once, so a pod being recreated when the command
   starts is never waited on, while a missing named pod fails the command and
   you rerun it. The replica counts mean something only after the image waits,
   because before the new template exists they already equal the replica
   count for the old revision. Skip non-HA instances: they have no init or
   agent container, so these waits run to their timeout.
5. Return the flag to `false` on every instance enabled in step 1, so a later
   operator bump does not update the data plane unattended. With Argo CD or Flux, set it back to `false` in the
   desired configuration, commit it, and sync, only after step 4 passes.

`26.10.0` also changes the operator's default server image to
`mariadb:12.3.3`, and `26.10.1` keeps it. AICR pins `mariadb:11.8.8` by tag
and index digest in `recipes/components/slurm-accounting-mariadb/values.yaml`,
so a cluster bundled from this recipe stays on `11.8.8`; the pin also puts the
server image into the rendered `MariaDB` resource, where the BOM records it.
Where the accounting database already runs, the first bundle that carries the
digest changes that resource's `spec.image` string, so the operator restarts the
database pod once and slurmdbd loses its database for the length of that
restart. Plan the bundle upgrade for a quiet window. The pod then runs the build
the digest names, which can be a newer rebuild of `11.8.8` than the one it last
pulled. A database first reconciled by operator `26.3.0`, which AICR never
shipped, was given that release's default,
`docker-registry1.mariadb.com/library/mariadb:11.8.5`, so it also moves registry
and patch version.

Existing `MariaDB` resources keep their image. The operator writes its default
into `spec.image` the first time it reconciles a resource, so every resource it
already manages carries an image, and upgrading the operator does not change
it. Only a `MariaDB` created after the upgrade from a manifest that omits
`spec.image` comes up on `12.3.3`, such as one re-created on a rebuilt cluster.
Live objects cannot show that exposure, so check the manifests they are created
from instead: every `MariaDB` in your Helm values, Kustomize trees and GitOps
repositories should set `spec.image`. For one a Helm release creates, print the
manifest as rendered, before the operator filled anything in:

```bash
RELEASE="my-release"       # replace with the Helm release name
NAMESPACE="my-namespace"   # replace with its namespace
helm get manifest "${RELEASE}" -n "${NAMESPACE}"
```

A `kind: MariaDB` document there with no `image:` under `spec` comes up on
`12.3.3` the next time it is created.

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
together.

AICR now pins 0.19.6, which adds a tenth: `TASPartialSlices` is new in 0.19.6,
defaults on, and requires TAS, so at 0.19.6 the nine above still fail with
`TASPartialSlices requires TopologyAwareScheduling to be enabled`. Checked
against 0.19.6 by running kueue's own feature-gate validation:
`TopologyAwareScheduling=false` alone fails with nine causes, the nine gates
above fail with that one, and all ten set false together pass. Releases before
0.19.6 do not know the gate and reject it with
`unrecognized feature gate: TASPartialSlices`, so set the tenth in the same
change that moves kueue to 0.19.6, not ahead of it. `TASGroupedPodSetSlicing`
also requires TAS from 0.19.6 but defaults off, so it matters only if you turned
it on. AICR sets no feature gates for kueue, so a default install is
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

`aicr upgrade-check` reports every boundary. The transition record at
`recipes/components/nodewright-operator/upgrades.yaml` describes `v0.18.0` (the
rename, the prerequisite above, and per-deployer steps), then `v0.19.0`,
`v0.19.1` and `v0.20.0` separately, each `safe`. Crossing the rename is
`manual` whatever you land on, and you do **not** have to stop at `v0.18.0` to
get past it:

```console
$ aicr upgrade-check --from older.yaml --to new.yaml --deployer helm
COMPONENT            FROM     TO       VERDICT  NOTES
nodewright-operator  v0.16.0  v0.20.0  manual   4 minors, 5 steps

$ aicr upgrade-check --from cur.yaml --to new.yaml --deployer helm
COMPONENT            FROM     TO       VERDICT  NOTES
nodewright-operator  v0.18.0  v0.20.0  safe     2 minors, verified
```

`v0.19.0` also changes drain timing: an interrupt now begins roughly the
longest `terminationGracePeriodSeconds` on the node later than before, and
`spec.drainConfig.timeout` — which has no default — bounds time-to-drain rather
than time-to-accept-evictions. AICR's tuning CRs declare interrupts and set no
timeout, so an undrainable pod holds its node in `in_progress` without bound.
Set a timeout on the CRs you author if you need that wait bounded.

`v0.20.0` refuses to start on a runtime-required taint key with a DNS prefix or
longer than 53 characters, and `aicr bundle` rejects such a `--workload-gate`
key, so a bundle that builds carries a valid one. It also no longer uncordons a
node it did not cordon.

The legacy `Skyhook` group, its `runtime-required` taint key and the
`skyhook_*` metrics are served through `v0.20.x` and removed upstream in
`v0.21.0`. A pin at or above that is not reachable until the remaining
`skyhook.nvidia.com` references in the `nodewright-operator` health check and
the `vr200-rke2-ubuntu-*` overlays are migrated.
