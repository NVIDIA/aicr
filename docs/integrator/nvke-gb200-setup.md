# NVKE GB200 Setup

The `gb200-nvke-ubuntu-training` recipe targets NVKE (NVIDIA Kubernetes Engine,
formerly NKE): bare-metal GB200 NVL trays provisioned by Forge/NICo, running
Ubuntu 24.04 with NVIDIA's `-nvidia-64k` kernel. It is a Preview coordinate.

## Cluster prerequisites

- Kubernetes 1.34 or newer (DRA `resource.k8s.io/v1` for ComputeDomains).
- Nodes carry `providerID: nico://<uuid>`. `aicr snapshot` maps that scheme to
  `service: nvke`, so `aicr recipe --snapshot` selects this recipe without
  `--service`.
- GPU nodes are untainted by the provisioner and run nothing GPU- or
  fabric-related: GPU Operator installs the driver, toolkit and GDRCopy.
- Control-plane nodes carry GPU-node labels they do not earn
  (`nvidia.com/gpu.present=true`, `nvidia.com/gpu.deploy.driver=true`,
  `feature.node.kubernetes.io/pci-15b3.present=true` from the BlueField-3)
  while having no GPU. Anything that selects GPU nodes by those labels alone
  also selects the control plane; the recipe excludes control-plane nodes
  explicitly where that matters (`rdma-modules`), and `deploy.sh`'s DRA
  driver-migration gate waits its full timeout on them before continuing.
- The primary CNI is OVN-Kubernetes on the BlueField-3, bound to the host's
  inbox `mlx5_core`. Multus is already present on the nodes.
- Every pod on the pod network is given one BlueField-3 VF: the `sdn-mutator`
  webhook adds a `nvidia.com/asap2_vf: 1` request, and a node advertises 28.
  That caps non-hostNetwork pods at 28 per node, platform pods included; the
  recipe's own DaemonSets take eight slots per GPU node (GPU Operator) plus
  nvsentinel, NFD, DRA and monitoring. Keep a system node pool for the
  cluster-scoped charts or expect pods to stay Pending on
  `Insufficient nvidia.com/asap2_vf`.
- East-west fabric: four ConnectX-7 InfiniBand rails per tray (`mlx5_0`,
  `mlx5_1`, `mlx5_3`, `mlx5_4`), active NDR, no IPoIB netdevs. `mlx5_2` is the
  BlueField-3 host representor.
- `rdma_ucm` and `ib_umad` (which create `/dev/infiniband/rdma_cm` and the
  `umad`/`issm` nodes the RDMA device plugin requires) are not loaded by the
  NVKE image and `rdma-core` is not installed; the recipe's `rdma-modules`
  component loads them (see below). A modules-load.d entry for both in the
  image makes that component a no-op. Installing `rdma-core` instead also
  works but is not equivalent: its udev rules tag every `mlx5_core` RDMA
  device as InfiniBand and load `/etc/rdma/modules/infiniband.conf`
  (`ib_ipoib`, `ib_umad`) and `rdma.conf` (`ib_iser`, `ib_uverbs`, `rdma_ucm`,
  `xprtrdma`, `svcrdma`), so IPoIB netdevs appear on all four rails.

## Never install DOCA-OFED on these nodes

The Network Operator in this recipe ships **no `ofedDriver`**. The host runs
the inbox `mlx5_core` and OVN-Kubernetes rides on it. A DOCA driver container
unloads `mlx5_core` to replace it, and the node's primary address disappears
with it; a reboot restores the inbox driver and the operator applies the
DOCA driver again. This was observed on YTL GB200 and Firmus GB300 clusters
with recipes that carried an `ofedDriver` block. Do not add one back for NVKE,
and do not combine this recipe with another Network Operator installation.

The same posture applies to GPUDirect RDMA: it runs over DMA-BUF. The recipe
sets no `driver.rdma` value (no `nvidia-peermem`, which has no build for the
64k-page Grace kernel and needs OFED symbols) and installs the
`NVreg_GrdmaPciTopoCheckOverride=1` module parameter through the
`kernel-module-params` ConfigMap, which the `nccl-all-reduce-bw-net` preflight
requires.

## Shared RDMA

The NicClusterPolicy exposes the four IB HCAs as one shared resource,
`nvidia.com/mlnxnics`, with 63 allocations per node, selecting by PCI vendor
and device (`15b3`/`1021`) because the IB ports have no netdevs and the
BlueField-3 shares the vendor. An allocation exposes all four HCAs; it does
not reserve a NIC. No nv-ipam, IPPool, multus or IPoIB network is deployed.
NCCL picks the rails directly (`NCCL_IB_HCA`). An SR-IOV variant (8 IB VFs per
rail, `SriovIBNetwork` + nv-ipam) is the planned follow-up; shared RDMA is
qualified first because it does not depend on the primary CNI's handling of
VF netdevs.

## rdma-modules and node tainting

`rdma-modules` is a manifest-only component carrying one `NodeWright` CR that
loads `rdma_ucm` and `ib_umad` and writes `/etc/modules-load.d/aicr-rdma-ulp.conf` on
every `pci-15b3.present` node that is not a control-plane node (the control
plane also carries a BlueField-3; an auto-tainted node whose taints the
package cannot tolerate would otherwise stay tainted). The package has no
interrupt (no cordon, drain
or reboot). `runtimeRequired: true` with `autoTaintNewNodes` lets nodewright
taint GPU nodes until the module is loaded, so the RDMA device plugin and GPU
workloads never start against an empty `/dev/infiniband`. Pass the accelerated
node selector and toleration at bundle time so the CR, the NicClusterPolicy
DaemonSets and GPU Operator agree on the cohort:

```bash
aicr bundle --recipe recipe.yaml \
  --accelerated-node-selector feature.node.kubernetes.io/pci-15b3.present=true \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  --output bundle
```

Disable the taint with `--set rdmamodules:autoTaintNewNodes=false`, or the
whole component with `--set rdmamodules:enabled=false` on images that already
load both modules.

## Generate and validate

```bash
aicr snapshot --node-selector kubernetes.io/hostname='<gpu-node>' --privileged \
  --output snapshot.yaml
aicr recipe --snapshot snapshot.yaml --intent training --output recipe.yaml
aicr bundle --recipe recipe.yaml --output bundle
aicr validate --recipe bundle/recipe.yaml
```

Performance thresholds are copied from the GB200 OKE recipe until an NVKE
baseline is measured; the benchmark runs the `gb200/oke` IB shared-HCA runtime
through the `nccl-benchmark-profile` constraint. Record the measured peak in
qualification evidence before re-flooring.

## Known gaps

- Shared RDMA only; SR-IOV and IPoIB secondary networks are not yet provided.
- `nccl-all-reduce-bw-nvls` is declared but unproven on NVL4 trays without an
  NVSwitch; drop it from the recipe if the cluster has no NVLink fabric.
- No published evidence yet.
- KWOK profiles model scheduling only; they cannot validate RDMA traffic.
