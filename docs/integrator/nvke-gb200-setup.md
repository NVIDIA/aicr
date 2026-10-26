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
  `mlx5_1`, `mlx5_3`, `mlx5_4`), active NDR, no IPoIB netdevs until the recipe
  loads `ib_ipoib`. `mlx5_2` is the BlueField-3 host representor. Firmware
  enables SR-IOV with `NUM_OF_VFS` 16 per PF (`sriov_totalvfs`), which is
  the count the recipe requests (see below).
- The fabric Subnet Manager must make the GPU host ports **full members** of
  their partition. On the reference cluster every rail's P_Key table held
  only `0x7fff` (limited membership), which leaves links up and LIDs assigned
  while every RDMA connection between hosts fails with retry-exceeded, even
  same-tray loopback through the switch. Check
  `/sys/class/infiniband/mlx5_0/ports/1/pkeys/0`: `0xffff` is required. VFs
  inherit the PF's membership, so this applies to the SR-IOV VFs too.
- `rdma_ucm`, `ib_umad` and `ib_ipoib` are not loaded by the NVKE image and
  `rdma-core` is not installed (the modules ship in
  `linux-modules-<kernel>`); the recipe's `rdma-modules` component loads them
  (see below). A modules-load.d entry for all three in the image makes that
  component a no-op. Installing `rdma-core` instead also works: its udev rules
  load `/etc/rdma/modules/infiniband.conf` (`ib_ipoib`, `ib_umad`) and
  `rdma.conf` (`ib_iser`, `ib_uverbs`, `rdma_ucm`, `xprtrdma`, `svcrdma`), a
  superset of what the recipe loads.
- Multus and the reference CNI plugins are preinstalled by NVKE
  (`kube-system/multus-ds`, `/opt/cni/bin`), as is a `sriov-device-plugin`
  that partitions the BlueField-3 VFs. The recipe installs neither; the
  SR-IOV operator's NetworkAttachmentDefinitions are served by the platform
  multus, and its policies select only the ConnectX-7 root devices.

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

## Containerd configuration

The recipe sets `gpu-operator.toolkit.env.RUNTIME_CONFIG_SOURCE=file` so the
NVIDIA toolkit reads `/etc/containerd/config.toml` directly. NVKE v0.22.0's
qualified Ubuntu image ships containerd 2.3.0 with a version 3 configuration.
The default command source reads `containerd config dump`, which migrates the
effective configuration to version 4. A version 4 NVIDIA drop-in imported by
the version 3 root prevents containerd from starting. Reading the file keeps
the NVIDIA drop-in at the host's configuration version without changing the
node image or migrating its root configuration. The same toolkit defect took
down AKS nodes with a version 2 root (NVIDIA/aicr#1688).

The toolkit also refuses `NVIDIA_VISIBLE_DEVICES` from unprivileged containers
(`ACCEPT_NVIDIA_VISIBLE_DEVICES_ENVVAR_WHEN_UNPRIVILEGED=false`). GPUs reach a
workload only through a `nvidia.com/gpu` request or a DRA claim; an unprivileged
pod that selects the `nvidia` RuntimeClass and sets `NVIDIA_VISIBLE_DEVICES=all`
sees none. Privileged management pods (the GPU Operator operands, NVSentinel's
metadata-collector) keep the env-var path, because the toolkit always honors it for
privileged containers. Unlike the AKS and OpenShift recipes, this one leaves
`ACCEPT_NVIDIA_VISIBLE_DEVICES_AS_VOLUME_MOUNTS` at its default of `false`: the
device plugin injects over `cdi-cri` here, so the volume-mount device list is never
produced, and accepting it would only add a `hostPath`-gated request path.

## SR-IOV InfiniBand

The Network Operator chart enables the SR-IOV Network Operator and the
Maintenance Operator (external drainer, one node at a time). The
NicClusterPolicy carries only `nvIpam`. Per rail, the `sriov-ib-networks`
component (manifest-only, ordered after `network-operator` because the
nv-ipam CRDs appear only once the NicClusterPolicy is reconciled) supplies
one nv-ipam `IPPool`
(`192.168.<4n>.0/22`), one `SriovNetworkNodePolicy` (`linkType: IB`,
`isRdma: true`, `deviceType: netdevice`, `numVfs: 16`, selected by the rail's
PCI root device) and one `SriovIBNetwork` whose NetworkAttachmentDefinition
lands in `default`. Nodes advertise `nvidia.com/sriov_resource_rail_0` to
`_3`, 16 each. A pod takes one VF per rail and the matching attachments:

```yaml
metadata:
  annotations:
    k8s.v1.cni.cncf.io/networks: default/sriov-network-rail-0,default/sriov-network-rail-1,default/sriov-network-rail-2,default/sriov-network-rail-3
spec:
  containers:
    - resources:
        limits:
          nvidia.com/sriov_resource_rail_0: 1
          nvidia.com/sriov_resource_rail_1: 1
          nvidia.com/sriov_resource_rail_2: 1
          nvidia.com/sriov_resource_rail_3: 1
```

This is the shape k8s-launch-kit generates for
`--deployment-type sriov --fabric infiniband` on this topology, minus its
`secondaryNetwork.multus`/`cniPlugins` (preinstalled on NVKE, see above) and
with the Maintenance Operator budget set to one node, with `numVfs` raised
from l8k's 8 to the firmware's 16, and with the SR-IOV config daemon's
`mellanox` plugin disabled (`sriovOperatorConfig.disablePlugins`). That
plugin rewrites firmware `NUM_OF_VFS` to exactly `numVfs` and then runs
`mstfwreset -l 3`, which this tray's firmware rejects ("not supported in the
current state of this device"); with it enabled, a policy that differed from
the firmware count drained the node and then looped on the reset with the
node cordoned. With the plugin off, firmware is a host prerequisite (SR-IOV
enabled, `NUM_OF_VFS`, IB link type) and VF creation is a sysfs write with no
drain or reboot, so `numVfs` must not exceed the firmware count. Check
`mstconfig -d <pf> q NUM_OF_VFS` before changing `numVfs`. The policies and pools select nodes by
`feature.node.kubernetes.io/rdma.available=true` rather than
`pci-15b3.present`: the control plane carries that label (and
`pci-0302_10de.present`) without any ConnectX-7, and when it was selected
the operator placed a device-plugin pod there whose init container waited
forever, which the `expected-resources` check flags. Enabling the
SR-IOV subchart on a Network Operator release that was installed without it
needs `charts/sriov-network-operator/crds` applied first: Helm installs a
subchart's CRDs only on a fresh install, and validates the rendered
`SriovOperatorConfig` before the chart's own CRD-upgrade hook runs.

A shared-HCA variant (`rdmaSharedDevicePlugin`, one pooled `nvidia.com/mlnxnics`
resource over the PFs, no IPoIB) was qualified first on the same cluster and
passed the deployment and conformance phases; it lives in this recipe's
history before the SR-IOV switch for anyone who needs a VF-free fallback.

## rdma-modules and node tainting

`rdma-modules` is a manifest-only component carrying one `NodeWright` CR that
loads `rdma_ucm`, `ib_umad` and `ib_ipoib` and writes
`/etc/modules-load.d/aicr-rdma-ulp.conf` on every `pci-15b3.present` node that
is not a control-plane node (the control plane also carries a BlueField-3; an
auto-tainted node whose taints the package cannot tolerate would otherwise
stay tainted). The package has no interrupt (no cordon, drain or reboot).
`runtimeRequired: true` with `autoTaintNewNodes` lets nodewright taint GPU
nodes until the modules are loaded, so the SR-IOV config daemon and GPU
workloads never start against an empty `/dev/infiniband` or VFs without
IPoIB netdevs. Pass the accelerated
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
load all three modules.

## Generate and validate

```bash
aicr snapshot --node-selector kubernetes.io/hostname='<gpu-node>' --privileged \
  --output snapshot.yaml
aicr recipe --snapshot snapshot.yaml --intent training --output recipe.yaml
aicr bundle --recipe recipe.yaml --output bundle
aicr validate --recipe bundle/recipe.yaml
```

Performance thresholds are copied from the GB200 OKE recipe until an NVKE
baseline is measured. The `nccl-benchmark-profile` constraint still names the
`gb200/oke` runtime, whose workers request `nvidia.com/mlnxnics`; this leaf
no longer exposes that resource, so the performance phase cannot schedule
until a `gb200/nvke` runtime requesting the per-rail VFs and attachments is
added to the validator. Record the measured peak in qualification evidence
before re-flooring.

## Known gaps

- No `gb200/nvke` NCCL runtime yet; the performance phase does not run on this
  leaf.
- The reference fabric has not passed RDMA traffic (limited P_Key membership,
  see prerequisites), so neither the SR-IOV nor the earlier shared-HCA shape
  has NCCL evidence.
- `nccl-all-reduce-bw-nvls` is declared but unproven on NVL4 trays without an
  NVSwitch; drop it from the recipe if the cluster has no NVLink fabric.
- No published evidence yet.
- KWOK profiles model scheduling only; they cannot validate RDMA traffic.
