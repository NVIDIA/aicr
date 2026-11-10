# OKE GPU Setup

## GPU Stack Ownership

OKE installs NVIDIA's device plugin via the `NvidiaGpuPlugin` cluster
add-on, and Oracle's GPU node images preinstall the NVIDIA driver,
container toolkit, and host MOFED. Which of those a GPU node pool actually
has depends on how it was provisioned, and the AICR recipe must match it —
the `gpuStack` configuration profile on the OKE family names the two
qualified combinations:

| Value | Driver / toolkit | `nvidia.com/gpu` advertiser | Pool shape |
|---|---|---|---|
| `oci-managed` (default) | Oracle GPU node image | OKE's `NvidiaGpuPlugin` add-on | Oracle-managed cluster with Oracle GPU images |
| `operator-managed` | GPU Operator installs both | GPU Operator's device plugin | bring-your-own driverless image, add-on removed |

The hybrid shape (image driver + operator plugin) is deliberately not
declared: no supported consumer needs it, and the two-value model lets a
single control-plane signal qualify both ownership axes.

MOFED is host-supplied in every value — Oracle's GPU images and the common
bring-your-own images alike carry it, so `network-operator` never deploys
`ofedDriver` on OKE, and the device plugin runs with `MOFED_ENABLED=false`
(without it, k8s-device-plugin >= 0.19.0 with CDI floods every host RDMA
uverb into every GPU pod and breaks NCCL).

Select the mode at recipe generation; the paths it owns are locked at every
output boundary:

```shell
CLUSTER_ID="ocid1.cluster.oc1.iad.aaaaaaaaexample"  # replace with your OKE cluster OCID

# Capture the qualification signal alongside the snapshot: the
# NvidiaGpuPlugin add-on's control-plane state.
oci ce cluster list-addons --cluster-id "${CLUSTER_ID}" --all --output json > addons.json
aicr snapshot --oke-addons addons.json --output snapshot.yaml

# Oracle-managed cluster (the default) — add-on installed and ACTIVE
aicr recipe --snapshot snapshot.yaml --intent training -o recipe.yaml

# Bring-your-own driverless image, add-on removed
aicr recipe --snapshot snapshot.yaml --intent training \
  --profile gpuStack=operator-managed -o recipe.yaml
```

Always capture the dump with `--all`, never `--page`. A dump with
`opc-next-page` set is rejected, but a final page alone looks like a
complete list, so an add-on on an earlier page would read as `absent`.

## Default: Oracle-Managed (`oci-managed`)

On a default-provisioned OKE cluster with Oracle GPU images, the image
supplies the driver and toolkit, and OKE's `NvidiaGpuPlugin` add-on
advertises `nvidia.com/gpu`. The add-on must run with
`deviceListStrategy=volume-mounts` (see
[Container Toolkit Hardening](#container-toolkit-hardening)); GB300 adds the
prerequisites in [GB300](#gb300-bmgpugb3004). The GPU Operator manages the rest of
the stack with its own plugin disabled — running both plugins
double-advertises the same GPUs, which the #1327 exactly-one-advertiser
policy forbids.

Under `oci-managed`, the GPU Operator's operands, the NVSentinel metadata
collector and its GPU reset jobs run with the `runc` runtime class. OKE's node bootstrap wires GPUs
into containers through an always-on CRI-O hook on the default `runc` runtime
and registers no `nvidia` runtime handler. Under `operator-managed` they run
with the `nvidia` runtime class the GPU Operator's toolkit registers.

## Container Toolkit Hardening

OKE's node bootstrap runs the node image's NVIDIA container toolkit for every
container through an always-on CRI-O hook, and the toolkit's upstream default
trusts `NVIDIA_VISIBLE_DEVICES` from unprivileged containers: a pod whose
image sets `NVIDIA_VISIBLE_DEVICES=all`, as every CUDA base image does, gets
every GPU on the node without a Kubernetes allocation.

Every OKE recipe closes this with a NodeWright CR, `toolkit-hardening`, that
runs `nvidia-setup` as `oci-any` on every node OKE labels `nvidia.com/gpu=true`.
It sets `accept-nvidia-visible-devices-envvar-when-unprivileged = false` and
`accept-nvidia-visible-devices-as-volume-mounts = true` in
`/etc/nvidia-container-runtime/config.toml`, declares no interrupt, and
taint-gates new GPU nodes until it has run. A node without a host toolkit
config is left alone. Under `operator-managed`, the GPU Operator's own toolkit
is hardened with the same two keys through its `toolkit.env`.

With those keys set, a device plugin must hand GPUs to containers through CDI
or volume mounts. The `NvidiaGpuPlugin` add-on defaults to `envvar` and cannot
run the CDI strategies, so under `oci-managed` it must use `volume-mounts`,
or pods lose the GPUs they were allocated:

```shell
oci ce cluster update-addon --cluster-id "${CLUSTER_ID}" \
  --addon-name NvidiaGpuPlugin \
  --configurations '[{"key":"deviceListStrategy","value":"volume-mounts"}]'
```

The `oci-managed` profile enforces this: it constrains the
`K8s.oke-gpu-plugin.device-list-strategy` snapshot reading, taken from the
add-on's `kube-system/nvidia-gpu-device-plugin` DaemonSet, to `volume-mounts`,
so `aicr validate` readiness fails until the add-on is reconfigured. An
existing cluster on the add-on's default strategy needs that one change before
it deploys an OKE recipe.

Leaving `nodewright-customizations` out of a bundle (`--set
nodewrightcustomizations:enabled=false`, or a `bundlers` filter that omits it)
removes the `toolkit-hardening` CR along with every other nodewright
customization. Under `oci-managed` that removes the hardening, so `aicr bundle`
warns and `secure-accelerator-access` fails; under `operator-managed` the GPU
Operator's toolkit keeps the keys.

## Alternative: Bring-Your-Own Image (`operator-managed`)

Custom images (OKE Ubuntu pools are always custom images) may ship no NVIDIA
stack at all. Under `operator-managed` the GPU Operator installs the driver
and toolkit, its device plugin advertises, and the DRA kubelet plugin reads
the driver userspace from the operator install path
(`/run/nvidia/driver` — the profile moves `nvidiaDriverRoot` in lockstep;
see the driver-ownership coherence rules). **Remove the `NvidiaGpuPlugin`
cluster add-on** (Terraform `addons = { NvidiaGpuPlugin = { remove = true } }`,
or the add-on lifecycle API) — that removal IS the qualification
contract. Disabling the plugin only via the
`oci.oraclecloud.com/disable-gpu-device-plugin` node label is out of
contract: it leaves the add-on installed, which qualifies as `oci-managed`
and would deploy with no `nvidia.com/gpu` advertiser on those pools.
Clusters using the label route must migrate to add-on removal.

Custom images that DO bake a driver (Oracle publishes downloadable Ubuntu
GPU images with driver + CUDA + DOCA-OFED) are an unqualified hybrid under
this profile — the operator would install a second driver on top of the
image's.

## Validation

Each value carries the canonical distinguishing constraint over the
`K8s.oke-addons.nvidia-gpu-plugin` snapshot reading — the projection of the
`NvidiaGpuPlugin` add-on's control-plane state from an operator-supplied
`oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json` dump (`--oke-addons` on
both `aicr snapshot` and `aicr validate`, which re-captures live). The
constraint joins the recipe's constraint set: evaluated at snapshot-based
generation and re-evaluated by the `aicr validate` readiness pre-flight,
fail-closed both times. Criteria-only generation (no `--snapshot`) selects
explicitly and records the constraint without evaluating it.

| Add-on reading | Default (`oci-managed`) | `--profile gpuStack=operator-managed` |
|---|---|---|
| `installed` (present, ACTIVE) | ✅ resolves | ❌ fails closed: constraint expects `absent` |
| `absent` (not in the add-on list) | ❌ fails closed: constraint expects `installed` | ✅ resolves |
| any other lifecycle state (e.g. `addon-deleting`) | ❌ fails closed naming the observed state | ❌ fails closed |
| no reading (snapshot captured without `--oke-addons`) | ❌ fails closed: reading **unavailable** — recapture with the flag | ❌ same |

`operator-managed` additionally carries a constraint over the in-cluster
`K8s.oke-legacy-plugin.nvidia-gpu-device-plugin` reading (see the next
section):

| Legacy-plugin reading | Default (`oci-managed`) | `--profile gpuStack=operator-managed` |
|---|---|---|
| `none` (absent, or labeled and fully disabled) | not gated | ✅ resolves (add-on constraint permitting) |
| `active` (legacy DaemonSet targets ≥ 1 node) | not gated — when the add-on is installed it manages the same DaemonSet | ❌ fails closed: disable per pool or migrate to the add-on |
| `unknown` (snapshot could not consult the API, or a same-named DaemonSet without the `Reconcile` label) | not gated | ❌ fails closed |
| no reading (snapshot from an older aicr) | not gated | ❌ fails closed: reading **unavailable** — recapture |

## Legacy Device Plugin Detection

Older OKE clusters ship the device plugin through a second, pre-add-on
mechanism: a `nvidia-gpu-device-plugin` DaemonSet in `kube-system`,
reconciled by the legacy Kubernetes addon-manager
(`addonmanager.kubernetes.io/mode: Reconcile`). That DaemonSet is
**invisible to `oci ce cluster list-addons`**, so on such a cluster the
add-on reading is `absent` even though Oracle's plugin is still advertising
`nvidia.com/gpu`. Deploying `operator-managed` there would double-advertise
(#1327).

`aicr snapshot` therefore observes the DaemonSet directly (a read-only
`apps/daemonsets` get, part of the agent's default ClusterRole) and
records the collapsed `K8s.oke-legacy-plugin.nvidia-gpu-device-plugin`
reading — `none`, `active`, or `unknown` — plus the uncollapsed detail
under `K8s.oke-legacy-plugin.daemonset`. Only `none` qualifies
`operator-managed`; remediation is either disabling the legacy plugin on
every GPU node pool (`oci.oraclecloud.com/disable-gpu-device-plugin=true`
node label — for the *legacy* DaemonSet this label is the supported
mechanism, unlike the add-on route above) or migrating the cluster to the
managed `NvidiaGpuPlugin` add-on. A same-named DaemonSet without the
`Reconcile` label reads `unknown` (detail `unlabeled`) and fails closed,
because the label identifies the owner, not whether the DaemonSet advertises
`nvidia.com/gpu`. The node label does not change that reading. For an
unlabeled DaemonSet, resolve its ownership and remove it if it is the
conflicting workload, or migrate to the managed add-on. The tripwire
deliberately does not gate `oci-managed`: when the managed add-on is installed
it reconciles the same DaemonSet name, so a healthy `oci-managed` cluster
legitimately observes an active DaemonSet.

Qualification is defined for **enhanced** OKE clusters (the default cluster
type; add-on management does not exist on basic clusters, so
`list-addons` cannot produce a valid dump there and qualification fails
closed). Basic clusters should be
[upgraded to enhanced in place](https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/contengworkingwithenhancedclusters.htm).

## Oracle Add-on Interactions

Do **not** enable Oracle's `NvidiaGpuOperator` or `NvidiaNetworkOperator`
managed add-ons alongside AICR bundles — the bundle deploys both operators
itself, and two lifecycle managers fight over the same releases. The only
Oracle GPU add-on compatible with AICR bundles is the device plugin
(`NvidiaGpuPlugin`), and only under `oci-managed`.

The recipes that deploy `network-operator` (`gb200-oke-training`,
`l40s-oke-training`, their descendants, and `gb300-oke-ubuntu-training`)
enforce this for `NvidiaNetworkOperator`. They require the
`K8s.oke-addons.nvidia-network-operator` reading from the same
`--oke-addons` dump to be `absent`, so a cluster with the add-on fails
snapshot-based recipe generation and the `aicr validate` pre-flight.
Criteria-only generation evaluates no reading and only records the
constraint, so the gate fires at the pre-flight. Remove the add-on,
wait for the removal to finish, then recapture the snapshot. An add-on that
is still being removed reads `addon-deleting`, which also fails the gate.
A snapshot captured with `--oke-addons` by an earlier `aicr` has no
`nvidia-network-operator` reading, so these recipes reject it as not found
without printing a remediation. Recapture it with the current
`aicr snapshot --oke-addons`.

```shell
CLUSTER_ID="ocid1.cluster.oc1.iad.aaaaaaaaexample"  # replace with your OKE cluster OCID

oci ce cluster disable-addon --cluster-id "${CLUSTER_ID}" \
  --addon-name NvidiaNetworkOperator --is-remove-existing-add-on true

# After the removal finishes, recapture with a fresh add-on dump.
oci ce cluster list-addons --cluster-id "${CLUSTER_ID}" --all --output json > addons.json
aicr snapshot --oke-addons addons.json --output snapshot.yaml
```

Oracle's `CertManager` add-on also conflicts with AICR bundles, and no
recipe checks for it. It runs cert-manager in the `cert-manager`
namespace without a Helm release, so the bundle's `cert-manager` release fails
on objects Helm does not own. Check `oci ce cluster list-addons` for
`CertManager` before deploying, then either keep the add-on and leave
cert-manager out of the bundle, so the components that depend on it use
Oracle's, or remove the add-on so the bundle installs its own pinned version:

```shell
# Keep Oracle's cert-manager:
aicr bundle -r recipe.yaml --set certmanager:enabled=false -o ./bundles

# Or remove the add-on and let the bundle install cert-manager:
oci ce cluster disable-addon --cluster-id "${CLUSTER_ID}" \
  --addon-name CertManager --is-remove-existing-add-on true
```

## GB300 (`BM.GPU.GB300.4`)

`gb300-oke-ubuntu-training` targets GB300 NVL72 nodes running Ubuntu 24.04
with a 64k-page arm64 kernel and the DOCA host stack, under either `gpuStack`
value:

- **`oci-managed`** (default): Oracle's GB300 platform image, which also
  preinstalls the NVIDIA driver, with OKE's `NvidiaGpuPlugin` add-on. Use the
  image with the 6.17 kernel and the R595 driver from Oracle's
  [worker node images](https://github.com/oracle-quickstart/oci-hpc-oke/blob/main/docs/worker-node-images.md).
  The 6.8-kernel image with the 580 LTS driver is below the recipe's 6.14
  kernel floor.
- **`operator-managed`**: a driverless image with the add-on removed. The GPU
  Operator installs the R595 driver the recipe pins (`driver.version`), so both
  values run the same driver branch.

Both values pass deployment, conformance and NCCL performance validation on a
4-node GB300 pool: `oci-managed` on Oracle's 6.17-kernel image, which runs the
RDMA subsystem in shared network namespace mode, and `operator-managed` in both
shared and exclusive mode (see [Fabric](#fabric)).

Pass `--accelerated-node-selector` at bundle time with a label only the GB300
pool carries: the recipe's Nodewright CR reboots every node it matches, and
without a selector that includes the CPU pools.

```shell
GPU_POOL_LABEL="nodeGroup=gpu"  # replace with a label only your GB300 nodes carry

aicr recipe --snapshot snapshot.yaml --intent training -o recipe.yaml
# operator-managed (NvidiaGpuPlugin add-on removed): select the value explicitly,
# since an absent add-on fails the oci-managed default.
#   aicr recipe --snapshot snapshot.yaml --intent training \
#     --profile gpuStack=operator-managed -o recipe.yaml
aicr bundle -r recipe.yaml --accelerated-node-selector "${GPU_POOL_LABEL}" -o ./bundles
```

### Cluster prerequisites

- **Flannel pod networking.** Create the cluster with the `FLANNEL_OVERLAY`
  CNI, not VCN-native. VCN-native pod networking sets `disable_ipv6=1` in pod
  network namespaces. An RDMA VF moved into such a pod never gets its IPv6
  address, so GID index 3 (the RoCEv2 IPv6 GID) stays empty and NCCL fails to
  connect. The CNI cannot be changed after the cluster is created.
- **`NvidiaGpuPlugin` add-on matching the `gpuStack` value.** Under
  `oci-managed`, installed with `deviceListStrategy=volume-mounts`, as for every
  OKE recipe (see [Container Toolkit Hardening](#container-toolkit-hardening));
  under `operator-managed`, removed.
- **An imported image compatible with `BM.GPU.GB300.4`.** An image imported
  from Oracle's `.oci` file may list no GPU shapes, and launching a GB300 node
  from it fails with `Shape BM.GPU.GB300.4 is incompatible with image`. Add
  the shape to the imported image:

  ```shell
  oci compute image-shape-compatibility-entry add \
    --image-id "${IMAGE_OCID}" --shape-name BM.GPU.GB300.4
  ```

### Fabric

East-west traffic is RoCE over ConnectX SR-IOV VFs. `network-operator` runs
the SR-IOV device plugin, which advertises the RDMA VFs as
`nvidia.com/mlnxnics`, and Multus. The recipe also ships the `rdma-net`
`NetworkAttachmentDefinition` in the network-operator release namespace
(`nvidia-network-operator` by default). A workload attaches a VF by requesting
`nvidia.com/mlnxnics` and annotating
`k8s.v1.cni.cncf.io/networks: nvidia-network-operator/rdma-net` (one entry per
VF; a node has four, one per GPU).

The attachment uses the `host-device` CNI, which moves the VF's netdev into
the pod; `sriov-cni` is not used because GB300 NICs run in switchdev mode and
`sriov-cni` resolves the VF representor instead of the uplink
([sriov-cni#247](https://github.com/k8snetworkplumbingwg/sriov-cni/issues/247)).
The VFs take their addresses from OCI's IPv6 SLAAC fabric, so the attachment
needs no IPAM, and the recipe deploys no `nv-ipam`.

What else the attachment needs depends on the kernel's RDMA network namespace
mode on the GB300 nodes, set by the `ib_core` module parameter `netns_mode`:

| Mode | Images | Attachment | Bundle setting |
|---|---|---|---|
| shared (`netns_mode=1`, reads `Y`) | Oracle's GB300 images with the 6.17 kernel, and images that do not set the parameter | `host-device` alone; every namespace already sees the RDMA device | default (`rdmaCni.enabled: false`) |
| exclusive (`netns_mode=0`, reads `N`) | images configured with `options ib_core netns_mode=0` | `host-device`, then the `rdma` CNI, which moves the VF's RDMA device into the pod; the recipe installs it with a DaemonSet | `--set networkoperator:rdmaCni.enabled=true` |

The `rdma` CNI refuses to run in shared mode, and in exclusive mode
`host-device` alone leaves the RDMA device in the host namespace. Either
mismatch keeps NCCL pods that attach `rdma-net` from starting. Exclusive mode
is the more isolated of the two: a pod sees only the RDMA devices moved into
it. To use it, add `options ib_core netns_mode=0` to
`/etc/modprobe.d/ib_core.conf` on each GB300 node, rebuild the initramfs
(`update-initramfs -u`, since `ib_core` loads from it), reboot, and bundle with
`rdmaCni.enabled=true`.

`aicr snapshot` records the mode as
`OS.kmod-param./sys/module/ib_core/parameters/netns_mode`, and `aicr recipe`
copies it into the recipe as `metadata.rdmaNamespaceMode`. `aicr bundle` warns
when that mode and `rdmaCni.enabled` disagree, and names the setting that
fixes it; a recipe without the reading gets an informational note instead. The
reading comes from the one node the snapshot agent runs on, so keep every
GB300 node in the same mode: check each with
`cat /sys/module/ib_core/parameters/netns_mode`.

### Node preparation

Beside the OKE-wide `toolkit-hardening` CR, a GB300 tuning CR prepares each
GB300 node:

- A CRI-O drop-in sets `cni_default_network = "multus-cni-network"`, so CRI-O
  waits for Multus instead of starting pods on the primary network alone. Multus
  comes from `network-operator`, so `aicr bundle` rejects a GB300 bundle that
  leaves `network-operator` out.
- A systemd unit holds kubelet until the RDMA VFs have their driver bound and
  have stopped changing. Oracle Cloud Agent rebinds each VF seconds after
  creating it, and the SR-IOV device plugin scans once at start, so a scan
  inside a rebind would advertise fewer `nvidia.com/mlnxnics`. This is a
  workaround until `nvidia-tuned`'s VF gate waits for the VFs to settle
  ([nodewright-packages#186](https://github.com/NVIDIA/nodewright-packages/issues/186)).
- `nvidia-tuned` with `service: oci` corrects PCIe ACS on the RoCE root ports
  (required for DMA-BUF GPUDirect RDMA), holds boot until the RDMA VFs are
  ready, and writes the GB300 NCCL topology to `/etc/nccl/topo.xml`. Point
  NCCL workloads at it with `NCCL_TOPO_FILE`. Each node reboots once.

The CR reboots one node at a time, and a GB300 node takes about 8 minutes to
tune and reboot. `deploy.sh` waits on the CR within its retry budget for the
component (5 attempts of 10 minutes each), which covers about five nodes. For a
larger pool on an initial build, use the bring-up opt-out in
[Rollout pacing](components/nodewright.md#rollout-pacing).
`network-operator` deploys first, so each node comes back Ready as soon as its
reboot finishes.

### Requirements

Items marked *checked* fail `aicr validate` readiness when they do not hold;
the others are assumed:

| Requirement | Checked |
|---|---|
| Kubernetes 1.34 or later | checked |
| Ubuntu 24.04 node image | checked |
| 6.14 kernel line or later | checked |
| 64k-page arm64 kernel, which the GB300 tuning profile sizes hugepages for | assumed (bundle-time advisory only) |
| `NvidiaGpuPlugin` add-on installed and `ACTIVE` under `oci-managed`, absent under `operator-managed` | checked (via the `gpuStack` profile) |
| `NvidiaGpuPlugin` configured with `deviceListStrategy=volume-mounts` (`oci-managed`) | checked (`K8s.oke-gpu-plugin.device-list-strategy`, via the `gpuStack` profile) |
| `NvidiaNetworkOperator` add-on absent | checked |
| Flannel pod networking | assumed |
| RDMA network namespace mode matching `rdmaCni.enabled` (see [Fabric](#fabric)) | bundle-time advisory (`aicr bundle` warns on a mismatch) |
| DOCA (MOFED) host stack in the image | assumed |
| Oracle Cloud Agent 1.64 or later with the Compute HPC RDMA Authentication and Compute HPC RDMA Auto-Configuration plugins enabled, which bring up the RoCE VFs with IPv6 SLAAC addresses | assumed |
| `oci-hpc-network-device-names` 3.x, which names the four RoCE VFs `rdma_vf_rail0` to `rdma_vf_rail3` | assumed |
| CRI-O container runtime (the Nodewright drop-in targets `/etc/crio/crio.conf.d`) | assumed |
| `CertManager` add-on absent, or cert-manager left out of the bundle as described in [Oracle Add-on Interactions](#oracle-add-on-interactions) | assumed |

If the node pool pre-taints its nodes with the legacy
`skyhook.nvidia.com=runtime-required:NoSchedule` key, pass the same taint to
`aicr bundle --workload-gate` (see
[Nodewright](components/nodewright.md)).

## References

- [OKE: Running GPU Workloads](https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/contengrunninggpunodes.htm)
- [OKE cluster add-ons](https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/contengintroducingclusteraddons.htm)
- [oci-hpc-oke quickstart](https://github.com/oracle-quickstart/oci-hpc-oke) — worker images, RDMA manifests
- [AKS GPU Setup](aks-gpu-setup.md), [GKE GPU Setup](gke-gpu-setup.md) — the sibling `gpuStack` families
