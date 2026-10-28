# NVKE GB200 Setup

The `gb200-nvke-ubuntu-training` recipe targets NVKE (NVIDIA Kubernetes Engine,
formerly NKE): bare-metal GB200 NVL trays provisioned by Forge/NICo, running
Ubuntu 24.04 with NVIDIA's `-nvidia-64k` kernel. It is a Preview coordinate.
`gb200-nvke-ubuntu-training-kubeflow` (`--platform kubeflow`) is the same leaf
plus Kubeflow Trainer through the `platform-kubeflow` mixin.

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
DOCA driver again. This was observed on NVKE GB200 and bare-metal GB300 clusters
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
`ACCEPT_NVIDIA_VISIBLE_DEVICES_AS_VOLUME_MOUNTS` at its default of `false`:
`DEVICE_LIST_STRATEGY` still emits the volume-mount device list beside CDI, but
the toolkit ignores it while the flag is `false`, and CDI is what injects
devices here.

## SR-IOV InfiniBand

The Network Operator chart enables the SR-IOV Network Operator and the
Maintenance Operator (external drainer, one node at a time). The
NicClusterPolicy carries only `nvIpam`. Per rail, the `sriov-ib-networks`
component (manifest-only, ordered after `network-operator`) supplies
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

The IPPool CRD is created by the operator while it reconciles the
NicClusterPolicy, not by the chart, so `sriov-ib-networks` can be applied
before the CRD exists. `deploy.sh` retries the release until it does. With
Argo CD or Terraform, build with `--readiness-hooks`: the `network-operator`
readiness gate waits for the NicClusterPolicy to reach `ready`, which the
operator reports only after its nv-ipam state, CRDs included, is applied, so
the pools follow it. Helmfile cannot take `--readiness-hooks` and its bundle
has no CRD wait, so install it in two stages. `kubectl wait` fails at once on
an object that does not exist yet, so wait for the CRD to be created before
waiting for it to be Established (`--for=create` needs kubectl 1.31 or later):

```bash
cd bundle
helmfile -l 'name!=sriov-ib-networks' apply
kubectl wait --for=create crd/ippools.nv-ipam.nvidia.com --timeout=10m
kubectl wait --for=condition=Established crd/ippools.nv-ipam.nvidia.com --timeout=2m
helmfile -l name=sriov-ib-networks apply
```

Flux does not support readiness hooks either. If `sriov-ib-networks`
exhausts its install retries on the missing CRD, wait for the CRD to be
created and Established, then reset the release:

```bash
kubectl wait --for=create crd/ippools.nv-ipam.nvidia.com --timeout=10m
kubectl wait --for=condition=Established crd/ippools.nv-ipam.nvidia.com --timeout=2m
flux reconcile helmrelease sriov-ib-networks -n flux-system --reset
```

The `sriov-ib-networks` health check asserts the CRD is Established, so
`aicr validate` records that it landed. Measured on a reference tenant after
a full teardown: the CRD appeared five seconds after the NicClusterPolicy
and the policy was `ready` four seconds later; the gate held
`sriov-ib-networks` until then.

When tearing the fabric down, uninstall `sriov-ib-networks` and wait for
every `SriovNetworkNodeState` to return to `Succeeded` before uninstalling
`network-operator`: removing the policies makes the config daemon request a
drain through the Maintenance Operator, and uninstalling the operators while
that drain is in flight orphans the `NodeMaintenance`, which leaves the node
cordoned with nothing left to release it. The Maintenance Operator's
controller is what removes the `maintenance.nvidia.com/finalizer` from a
`NodeMaintenance` and uncordons its node, so with the operator gone either
reinstall `network-operator` and let it finish the cleanup, or recover by hand:

```bash
kubectl get nodemaintenances.maintenance.nvidia.com -A
kubectl delete nodemaintenance '<name>' -n '<namespace>' --wait=false
# Only if the object stays pending on its finalizer:
kubectl patch nodemaintenance '<name>' -n '<namespace>' --type=merge \
  -p '{"metadata":{"finalizers":null}}'
kubectl uncordon '<node>'
```

A shared-HCA variant (`rdmaSharedDevicePlugin`, one pooled `nvidia.com/mlnxnics`
resource over the PFs, no IPoIB) was qualified first on the same cluster and
passed the deployment and conformance phases. It is not selectable from this
recipe; it survives only in the history of
[NVIDIA/aicr#3295](https://github.com/NVIDIA/aicr/pull/3295) before the SR-IOV
switch.

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
IPoIB netdevs. The CR selects `feature.node.kubernetes.io/pci-15b3.present=true`
by default, the cohort the NicClusterPolicy pins its DaemonSets to.
`--accelerated-node-selector` and `--accelerated-node-toleration` reach only
this component: the NicClusterPolicy keeps its fixed affinity and GPU Operator
selects nodes by its own labels, so a different selector must still cover every
node the fabric DaemonSets land on, or those nodes never get the modules and are
never gated.

Disable the taint with `--set rdmamodules:autoTaintNewNodes=false`. On images
that already load all three modules, disable the component in the recipe rather
than at bundle time: set `overrides.enabled: false` on the `rdma-modules`
componentRef in `recipe.yaml` (or in a `--data` overlay), which drops it from
the bundle and from the deployment phase's expected resources alike. A
bundle-time `--set rdmamodules:enabled=false` is not persisted, so
`aicr validate` would still assert the CR.

## Generate and validate

```bash
aicr snapshot --node-selector kubernetes.io/hostname='<gpu-node>' --privileged \
  --output snapshot.yaml
aicr recipe --snapshot snapshot.yaml --intent training --output recipe.yaml
aicr bundle --recipe recipe.yaml --readiness-hooks --output bundle
./bundle/deploy.sh
aicr validate --recipe bundle/recipe.yaml --phase deployment --phase conformance
```

`aicr validate` runs the deployment and conformance phases against the live
cluster, so the bundle must be deployed before it.

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
