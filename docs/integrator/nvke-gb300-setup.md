# NVKE GB300 Setup

The `gb300-nvke-ubuntu-training` recipe targets NVKE (NVIDIA Kubernetes Engine,
formerly NKE): bare-metal GB300 trays provisioned by Forge/NICo, running
Ubuntu 24.04 with NVIDIA's `-nvidia-64k` kernel. It is a Preview coordinate.
The reference tray is a Supermicro `ARS-121GL-NB3-LCC-FG001` (Firmus CBR01):
4x GB300, 144 Grace cores, two dual ConnectX-8 SuperNIC mezzanines for the
east-west fabric and a BlueField-3 for north-south.

It is the bare-metal `gb300-generic-ubuntu-training` component shape (GPU
Operator with an operator-installed driver, nvidia-tuned GB300 tuning through
nodewright, NFD, Network Operator with shared-HCA RDMA) with the fabric
changed: no `ofedDriver` (the host keeps its inbox driver), RoCE over
per-rail macvlan attachments instead of InfiniBand, and the `rdma-modules`
NodeWright from the GB200 leaf in its RoCE form. The NVKE platform prerequisites are the same as for GB200; see
[NVKE GB200 Setup](nvke-gb200-setup.md) for the `nico://` providerID mapping,
the control-plane label caveat, and the BlueField-3 VF cap on pod count.

## Cluster prerequisites

- Kubernetes 1.34 or newer (DRA `resource.k8s.io/v1` for ComputeDomains).
- Nodes carry `providerID: nico://<uuid>`, which `aicr snapshot` maps to
  `service: nvke`.
- The node image has no DOCA-OFED: `mlx5_core`, `mlx5_ib`, `ib_core` and
  `ib_uverbs` load from the kernel tree and userspace is Ubuntu's
  `libibverbs1`. The recipe runs on that inbox stack and never installs
  OFED (see below). An image that ships DOCA-OFED also works.
- `rdma_ucm` is not loaded by the image (`/dev/infiniband/rdma_cm` is
  missing) and nothing loads it without OFED; the recipe's `rdma-modules`
  component does (see below). A modules-load.d entry for it in the image
  makes that component a no-op.
- The primary CNI is OVN-Kubernetes on the BlueField-3 (`15b3:a2dc`, in
  zero-trust host-restricted mode), bound to the host's inbox `mlx5_core`.
  The OVN pod network is `192.168.0.0/16`; the recipe's rail subnets stay
  out of it.
- East-west fabric: four ConnectX-8 rails per tray (`mlx5_0`, `mlx5_2`,
  `mlx5_4`, `mlx5_6` at `0000/0002/0010/0012:03:00.0`; each NIC's second plane
  is the `.1` function and is not a separate rail), Ethernet link type, 400G.
  The rails must be linked up with the host PF MTU set as the fabric expects;
  on the reference tray they came up with no carrier and MTU 1500, which blocks
  any data-plane validation until the switch side is configured.
- ConnectX-8 firmware exposes one VF per PF (`sriov_totalvfs` 1). The recipe
  therefore uses shared-HCA RDMA; see below before trying SR-IOV.
- Whether the fabric is Spectrum-X is a switch-side fact the recipe cannot
  detect. If it is, the Network Operator needs the Spectrum-X profile
  (`l8k generate --spectrum-x RA2.x`), which this recipe does not carry.
- On a tenant whose only node is a tainted control plane that is also the
  GPU node, pass the control-plane toleration at bundle time so the GPU
  Operator and nodewright agree with the fabric DaemonSets, which tolerate it
  already:

```bash
aicr bundle --recipe recipe.yaml \
  --accelerated-node-selector feature.node.kubernetes.io/pci-15b3.present=true \
  --accelerated-node-toleration node-role.kubernetes.io/control-plane=:NoSchedule \
  --output bundle
```

## Never install DOCA-OFED on these nodes

The Network Operator in this recipe ships **no `ofedDriver`**. The host runs
the inbox `mlx5_core` and OVN-Kubernetes rides on it through the BlueField-3.
A DOCA driver container unloads `mlx5_core` to replace it, and the node's
primary address disappears with it; a reboot restores the inbox driver and
the operator applies the DOCA driver again. This was observed on Firmus GB300
clusters with recipes that carried an `ofedDriver` block. A tenant whose only
node is the control plane also cannot be drained for a driver rollout. Do not
add an `ofedDriver` back for NVKE, and do not combine this recipe with
another Network Operator installation.

GPUDirect RDMA runs over DMA-BUF: the recipe sets no `driver.rdma` value
(no `nvidia-peermem`, which has no build for the 64k-page Grace kernel).

## Shared-HCA RoCE with per-rail macvlan

The Network Operator chart deploys only the operator. The NicClusterPolicy
carries `nvIpam` and `rdmaSharedDevicePlugin`, which exposes one pooled
resource per rail, `rdma/rdma_shared_resource_rail_0` to `_3`, each selected
by the rail's PCI address (`deviceID 1023`, link type `ether`; the BlueField-3
is excluded). The `macvlan-rdma-networks` component (manifest-only, ordered
after `network-operator` because the nv-ipam CRDs appear only once the
NicClusterPolicy is reconciled) supplies per rail one nv-ipam `IPPool`
(`10.255.<4n>.0/22`) and one `MacvlanNetwork` in bridge mode over the rail's
PF netdev, with `mtu: 0` so the attachment inherits the PF's MTU. The
NetworkAttachmentDefinitions land in `default`. A pod takes one RDMA
allocation and one attachment per rail:

```yaml
metadata:
  annotations:
    k8s.v1.cni.cncf.io/networks: default/macvlan-network-rail-0,default/macvlan-network-rail-1,default/macvlan-network-rail-2,default/macvlan-network-rail-3
spec:
  containers:
    - resources:
        limits:
          rdma/rdma_shared_resource_rail_0: 1
          rdma/rdma_shared_resource_rail_1: 1
          rdma/rdma_shared_resource_rail_2: 1
          rdma/rdma_shared_resource_rail_3: 1
```

This is the shape k8s-launch-kit generates for
`--deployment-type rdma_shared --fabric ethernet --multirail` on this
topology, minus its `secondaryNetwork.multus`/`cniPlugins` (preinstalled on
NVKE), its `nfd.enabled: true` (the recipe's own NFD labels the nodes), and
its `NicConfigurationTemplate` (NIC firmware is a host prerequisite on NVKE;
the trays reject operator-driven firmware resets). `MacvlanNetwork.master`
names the PF netdev, so the manifest is specific to the reference tray's
kernel interface names; a different board needs a copy with its own names.

## rdma-modules and node tainting

`rdma-modules` is a manifest-only component carrying one `NodeWright` CR that
loads `rdma_ucm` and `ib_umad` and writes
`/etc/modules-load.d/aicr-rdma-ulp.conf` on every `pci-15b3.present` node.
This is the RoCE form of the GB200 leaf's manifest: no `ib_ipoib`, since
pods get a macvlan netdev rather than an IPoIB one, and no control-plane
exclusion, since on an NVKE GB300 tenant the control plane can be the only
GPU tray. Without it the rdma-shared device plugin finds no
`/dev/infiniband/rdma_cm`, advertises no rail resource, and the deployment
gate's expected-resources check never passes. The package has no interrupt
(no cordon, drain or reboot). `runtimeRequired: true` with
`autoTaintNewNodes` lets nodewright taint GPU nodes until the modules are
loaded. Disable the taint with `--set rdmamodules:autoTaintNewNodes=false`,
or the whole component with `--set rdmamodules:enabled=false` on images that
already load `rdma_ucm`.

Why not SR-IOV like the NVKE GB200 leaf: `l8k discover` defaults this tray to
SR-IOV with `numVfs: 8`, but the firmware exposes one VF per PF and nothing in
the recipe may rewrite firmware. Raise `NUM_OF_VFS` on the host first if you
need VFs; then the GB200 leaf's `sriov-ib-networks` component is the model,
with `linkType: ETH` and `SriovNetwork` in place of `SriovIBNetwork`.

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

- The reference tray already ran a GPU Operator (driver 580.173.02), NFD and
  the NVIDIA DRA driver before this recipe existed. Who installs the GPU
  stack on an NVKE tenant, the platform or the operator, decides whether this
  recipe's `gpu-operator`, `nfd` and `nvidia-dra-driver-gpu` components take
  over an existing install or must be disabled; not yet confirmed.
- Multus is assumed preinstalled, as on NVKE GB200; not yet confirmed on a
  GB300 tenant. If absent, the macvlan attachments need `secondaryNetwork`
  enabled in the NicClusterPolicy.
- The inbox-driver and missing-`rdma_ucm` findings come from one node on one
  image build (`nke-node-non-fde-ubuntu-24.04.5-arm64`, checked 2026-10-23).
  A later NVKE image may load the module or ship DOCA-OFED; either makes
  `rdma-modules` a no-op rather than a conflict.
- No NCCL evidence on an NVKE GB300 cluster yet; the east-west rails on the
  reference tray are not linked up.
- No published evidence yet.
- The KWOK profile models scheduling only.
