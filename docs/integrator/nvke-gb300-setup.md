# NVKE GB300 Setup

The `gb300-nvke-ubuntu-training` recipe targets NVKE (NVIDIA Kubernetes Engine,
formerly NKE): bare-metal GB300 NVL trays provisioned by Forge/NICo, running
Ubuntu 24.04 with NVIDIA's `-nvidia-64k` kernel. It is a Preview coordinate.

It is the bare-metal `gb300-generic-ubuntu-training` component shape (GPU
Operator with an operator-installed driver, nvidia-tuned GB300 tuning through
nodewright, NFD, Network Operator with shared-HCA RDMA) with one difference:
the Network Operator ships **no `ofedDriver`**, because the Mellanox driver
stack is already on the host. The NVKE platform prerequisites are the same as
for GB200; see [NVKE GB200 Setup](nvke-gb200-setup.md) for the `nico://`
providerID mapping, the control-plane label caveat, the BlueField-3 VF cap on
pod count, and the fabric P_Key full-membership requirement.

## Never install DOCA-OFED on these nodes

The host runs the Mellanox driver that OVN-Kubernetes (the primary CNI, on
BlueField-3) is bound to. A DOCA driver container unloads `mlx5_core` to
replace it, and the node's primary address disappears with it; a reboot
restores the host driver and the operator applies the DOCA driver again. This
was observed on Firmus GB300 clusters with recipes that carried an
`ofedDriver` block. Do not add one back for NVKE, and do not combine this
recipe with another Network Operator installation.

GPUDirect RDMA runs over DMA-BUF: the recipe sets no `driver.rdma` value
(no `nvidia-peermem`, which has no build for the 64k-page Grace kernel).

## Shared-HCA InfiniBand

The NicClusterPolicy carries only `rdmaSharedDevicePlugin`, which pools the
InfiniBand ConnectX PFs (device IDs `1021` ConnectX-7 and `1023` ConnectX-8;
the BlueField-3 is excluded) into one `rdma/ib` node resource, up to 64
allocations per node. There is no IPoIB, no nv-ipam and no SR-IOV. The
`nicConfigurationOperator` of the generic GB300 recipe is left out: NIC
firmware (IB link type, SR-IOV settings) is a host prerequisite on NVKE, whose
trays reject operator-driven firmware resets.

The plugin's DaemonSet is pinned to `pci-15b3.present` nodes that are not
control-plane nodes, because on NVKE the control plane also carries a
BlueField-3 and therefore the label, without any IB rail.

## Generate and validate

```bash
aicr snapshot --node-selector kubernetes.io/hostname='<gpu-node>' --privileged \
  --output snapshot.yaml
aicr recipe --snapshot snapshot.yaml --intent training --output recipe.yaml
aicr bundle --recipe recipe.yaml --output bundle
aicr validate --recipe bundle/recipe.yaml
```

The `nccl-all-reduce-bw-nvls` floor is copied from
`gb300-generic-ubuntu-training` until an NVKE GB300 baseline is measured.
Record the measured peak in qualification evidence before re-flooring.

## Known gaps

- No NCCL evidence on an NVKE GB300 cluster yet; the performance floor is
  inherited, not measured.
- If the host image does not load `rdma_ucm` (the GB200 NVKE image did not),
  the `rdma-modules` component from the GB200 recipe can be added to this leaf;
  it is not wired in until a GB300 host reports the gap.
- No published evidence yet.
- The KWOK profile models scheduling only; CPU and memory are carried over
  from the GB200 tray profile.
