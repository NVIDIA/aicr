---
name: analyzing-snapshots
description: |
  Use when analyzing an AICR snapshot YAML file, reviewing cluster state,
  comparing provider characteristics, extracting GPU/network topology insights,
  or generating a cluster assessment report from a snapshot.
  Triggers on: snapshot analysis, cluster review, provider comparison,
  GPU topology, node health, snapshot report.
---

# Analyzing AICR Snapshots

Systematic analysis of AICR snapshot YAML files to extract cluster identity,
provider characteristics, GPU topology, node health, software stack, and
operational signals. Produces a structured Markdown report.

## When to Use

- User provides a snapshot YAML file for review
- User asks about cluster characteristics or provider differentiation
- User wants to compare snapshots or extract specific insights
- User asks to generate a cluster assessment report

## Analysis Procedure

Snapshot files are large (50K-80K+ tokens). **Never read the whole file.**
Use `mcp__plugin_context-mode_context-mode__execute_file` with Python/YAML
parsing to extract sections, or use targeted `Read` with offset/limit on
specific line ranges found via `Grep`.

### Step 1: Extract Metadata and Structure

```python
import yaml
data = yaml.safe_load(FILE_CONTENT)
meta = data.get('metadata', {})
measurements = data.get('measurements', [])
print("=== METADATA ===")
for k, v in meta.items():
    print(f"  {k}: {v}")
print("\n=== MEASUREMENTS ===")
for m in measurements:
    subtypes = [s.get('subtype', s.get('name', '?')) for s in m.get('subtypes', [])]
    print(f"  {m['type']}: {subtypes}")
```

### Step 2: Extract K8s Server and Node Info

Key fields for provider identification:

| Field Path | What It Reveals |
|------------|----------------|
| `K8s.server.version` | K8s version + vendor suffix (`-eks-`, `-gke`, `-aks`, `+lke`) |
| `K8s.node.provider` | Mapped provider: eks, gke, aks, oke, metal3, kind |
| `K8s.node.provider-id` | Raw provider URI (`aws://`, `gce://`, `azure://`, `oci://`, `metal3://`) |
| `K8s.node.kernel-version` | Kernel + arch indicator (e.g., `-64k` = ARM 64K pages) |
| `K8s.node.container-runtime-*` | Runtime name and version |
| `K8s.node.kubelet-version` | Kubelet version |
| `K8s.node.os-image` | OS description string |

**Provider detection logic:**

| provider-id prefix | Service | Notes |
|-------------------|---------|-------|
| `aws://` | eks | Amazon EKS |
| `gce://` | gke | Google GKE |
| `azure://` | aks | Azure AKS |
| `oci://` | oke | Oracle OKE |
| `metal3://` | bare-metal | Metal3/Ironic, self-managed |
| `kind://` | kind | Local dev cluster |
| *(none/other)* | any | Self-managed, check version string |

If `provider-id` is absent, check `K8s.server.version` for vendor substrings.

### Step 3: Extract GPU Info

Key fields from `GPU.smi`:

| Field | Example | Significance |
|-------|---------|-------------|
| `gpu.model` | NVIDIA GB300 | Maps to accelerator criteria |
| `gpu.product-architecture` | Blackwell | GPU generation |
| `gpu-count` | 4 | GPUs per node |
| `driver` | 580.126.16 | NVIDIA driver version |
| `cuda-version` | 13.0 | CUDA toolkit version |
| `gpu.addressing-mode` | ATS | ATS = unified CPU-GPU memory (Grace) |
| `gpu.persistence-mode` | Disabled/Enabled | Should be Enabled for production |
| `gpu.vbios-version` | 97.10.4A.00.1A | Firmware version |
| `gpu.gsp-firmware-version` | 580.126.16 | GSP firmware |

**Accelerator mapping** (checked in order, case-insensitive):

| gpu.model contains | Accelerator |
|--------------------|-------------|
| `gb200` | gb200 (check before b200) |
| `gb300` | gb200 class (Blackwell NVL family) |
| `b200` | b200 |
| `h100` | h100 |
| `a100` | a100 |
| `l40` | l40 |
| `rtx pro 6000` | rtx-pro-6000 |

### Step 4: Extract OS Info

From `OS.release`: `ID`, `VERSION_ID`, `PRETTY_NAME`

From `OS.grub`: Boot parameters (check for `iommu`, `console`, `init_on_free`)

From `OS.kmod`: Loaded kernel modules (look for `nvidia*`, `nv_peer_mem`,
`gdrdrv`, `ib_*`, `mlx5_*` for RDMA/InfiniBand)

From `OS.sysctl` (key tuning parameters):

| Sysctl | Good Value for GPU | Why |
|--------|-------------------|-----|
| `vm.swappiness` | <= 10 | Minimize swapping for GPU workloads |
| `vm.overcommit_memory` | 1 | Allow overcommit for training |
| `vm.nr_hugepages` | > 0 (ideal) | Large page performance |
| `fs.file-max` | High (9223372036854775807) | Sufficient file descriptors |
| `kernel.threads-max` | > 1M | Sufficient threads |
| `vm.min_free_kbytes` | > 1M | Memory reserve |

### Step 5: Extract Node Topology

From `NodeTopology.summary`: `node-count`, `taint-count`, `label-count`

From `NodeTopology.taint`: Format is `effect|value|node1,node2,...`

From `NodeTopology.label`: Format is `value|node1,node2,...`

**High-value labels to extract** (skip `feature.node.kubernetes.io/cpu-cpuid.*`):

| Label Prefix | What It Reveals |
|-------------|-----------------|
| `kubernetes.io/arch.*` | CPU architecture (amd64 vs arm64 = heterogeneous) |
| `nvidia.com/gpu.*` | GPU product, family, memory, compute, count, MIG state |
| `nvidia.com/cuda.*` | CUDA driver/runtime versions |
| `nvidia.com/mig.*` | MIG capable/config/strategy |
| `nvidia.com/gpu.clique.*` | NVLink GPU cliques (multi-node NVLink domains) |
| `resource.nvidia.com/computeDomain` | Unified compute domain |
| `network.topology.nvidia.com/accelerator.*` | NVLink fabric blocks |
| `node-type.*` | Hardware type (gb300, standard) |
| `node-pool.*` | Pool assignment (gpu-pool, cpu-pool) |
| `node.dgxc.nvidia.com/*` | DGX Cloud node classification |
| `k8saas.nvidia.com/*` | K8SaaS management (NVSentinel cordon/uncordon) |
| `dgxc.nvidia.com/nvsentinel-state` | Health state (remediation-failed, healthy) |
| `nvsentinel.dgxc.nvidia.com/*` | NVSentinel component versions, driver state |
| `network.nvidia.com/operator.*` | Network operator MOFED/NIC config state |
| `metal3.io/uuid.*` | Metal3 bare-metal node UUIDs |
| `workload.*` | Workload type (gpu, general) |
| `feature.node.kubernetes.io/rdma.*` | RDMA available/capable |
| `feature.node.kubernetes.io/network-sriov.*` | SR-IOV capability |
| `feature.node.kubernetes.io/pci-15b3.*` | Mellanox ConnectX presence |
| `feature.node.kubernetes.io/pci-10de.*` | NVIDIA GPU PCI presence |
| `nvidia.com/dra-kubelet-plugin` | DRA (Dynamic Resource Allocation) |

### Step 6: Extract K8s Images and Policies

From `K8s.image`: All deployed container images and versions.

From `K8s.policy`: Flattened GPU Operator ClusterPolicy spec (dot-notation).

**Key policy fields:**

| Policy Field | What to Check |
|-------------|---------------|
| `driver.enabled` | GPU driver managed by operator |
| `driver.version` | Driver version in policy |
| `driver.rdma.enabled` | RDMA support |
| `toolkit.enabled` | Container toolkit |
| `devicePlugin.enabled` | Device plugin active |
| `dcgm.enabled` / `dcgmExporter.enabled` | GPU monitoring |
| `migManager.enabled` | MIG management |
| `ccManager.enabled` / `ccManager.defaultMode` | Confidential Computing |
| `sandboxWorkloads.enabled` | Sandbox/KubeVirt workloads |
| `psa.enabled` | Pod Security Admission |
| `vfioManager.enabled` | VFIO passthrough |

### Step 7: Check SystemD Services

From `SystemD.containerd.service`, `SystemD.kubelet.service`, `SystemD.docker.service`:

| Field | What to Check |
|-------|---------------|
| `ActiveState` | Should be `active` |
| `SubState` | Should be `running` |
| `LimitNOFILE` | File descriptor limits |
| `LimitMEMLOCK` | Memory lock limits (important for RDMA) |
| `KillMode` | `process` for containerd (graceful) |
| `Delegate` | `true` for containerd (cgroup delegation) |
| `CPUAccounting` | Resource accounting |

## Report Template

Structure the output as:

```
# Snapshot Analysis: {name}
> Source: {file} | Captured: {timestamp} | AICR: {version}

## Cluster Identity
Table: source-node, provider, K8s version, node count, GPU model, total GPUs

## Provider-Differentiating Insights
### 1. Provider Type (cloud vs bare-metal, managed vs self-managed)
### 2. CPU Architecture (homogeneous vs heterogeneous, ARM vs x86)
### 3. GPU Hardware (model, architecture, memory, driver, CUDA, MIG, persistence)
### 4. Network Topology (NVLink blocks, cliques, compute domains, RDMA, SR-IOV)
### 5. Management Layer (K8SaaS, NVSentinel health, cordon state)
### 6. Job Scheduling (Slurm/Slinky presence, HPC vs cloud-native)
### 7. Networking Stack (CNI, RDMA, SR-IOV, DOCA/MOFED)
### 8. Security (Confidential Computing, PSA, DRA)
### 9. Operational Signals (sysctl tuning, hugepages, persistence mode)

## Software Stack
### Key Container Images (table)
### OS and Kernel (table)

## Node Inventory
List nodes by rack/block/pool

## Operational Flags
Anything unusual: GPU health issues, disabled persistence mode,
missing hugepages, NVSentinel remediation failures, etc.
```

## What Makes Each Provider Unique

### Cloud Providers (EKS, GKE, AKS, OKE)

- Provider-id with cloud prefix
- Cloud-specific K8s version suffixes
- Managed node groups / auto-scaling
- No bare-metal labels (metal3.io)
- Typically x86_64 homogeneous
- No NVLink fabric topology labels
- No Slurm/Slinky stack

### Bare-Metal / DGX Cloud (Metal3, K8SaaS)

- `metal3://` provider-id with per-node UUIDs
- `k8saas.nvidia.com/*` management labels
- NVSentinel health monitoring (cordon/uncordon lifecycle)
- NVLink accelerator blocks and GPU cliques
- Compute domains spanning racks
- ARM64 Grace CPUs (heterogeneous with x86 head node)
- Slurm/Slinky HPC scheduling
- RDMA + SR-IOV networking with DOCA drivers
- ATS GPU addressing mode (unified memory)
- Liquid-cooled chassis machine types (LCC in machine name)

### Self-Managed / Kind

- Missing or generic provider-id
- No cloud or bare-metal management labels
- Simpler topology (single node or small cluster)
- Standard x86_64

## AICR Criteria Mapping

After analysis, map the snapshot to AICR recipe criteria:

```bash
aicr recipe \
  --service {detected_service} \
  --accelerator {detected_accelerator} \
  --os {detected_os} \
  --intent {training|inference} \
  --snapshot {snapshot_file}
```

| Criteria | Extracted From | Valid Values |
|----------|---------------|--------------|
| service | K8s.node.provider / K8s.server.version | eks, gke, aks, oke, kind, lke |
| accelerator | GPU.smi.gpu.model | h100, gb200, b200, a100, l40, rtx-pro-6000 |
| os | OS.release.ID | ubuntu, rhel, cos, amazonlinux |
| intent | User-specified | training, inference |
| platform | User-specified | kubeflow, dynamo, nim |
