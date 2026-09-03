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
# Capture the qualification signal alongside the snapshot: the
# NvidiaGpuPlugin add-on's control-plane state.
oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json > addons.json
aicr snapshot --oke-addons addons.json --output snapshot.yaml

# Oracle-managed cluster (the default) — add-on installed and ACTIVE
aicr recipe --snapshot snapshot.yaml --intent training -o recipe.yaml

# Bring-your-own driverless image, add-on removed
aicr recipe --snapshot snapshot.yaml --intent training \
  --profile gpuStack=operator-managed -o recipe.yaml
```

## Default: Oracle-Managed (`oci-managed`)

A default-provisioned OKE cluster with Oracle GPU images needs zero setup:
the image supplies the driver and toolkit, and OKE's `NvidiaGpuPlugin`
add-on advertises `nvidia.com/gpu`. The GPU Operator manages the rest of
the stack with its own plugin disabled — running both plugins
double-advertises the same GPUs, which the #1327 exactly-one-advertiser
policy forbids.

## Alternative: Bring-Your-Own Image (`operator-managed`)

Custom images (OKE Ubuntu pools are always custom images) may ship no NVIDIA
stack at all. Under `operator-managed` the GPU Operator installs the driver
and toolkit, its device plugin advertises, and the DRA kubelet plugin reads
the driver userspace from the operator install path
(`/run/nvidia/driver` — the profile moves `nvidiaDriverRoot` in lockstep;
see the driver-ownership coherence rules). **Remove the `NvidiaGpuPlugin`
cluster add-on** (Terraform `addons = { NvidiaGpuPlugin = { remove = true }
}`, or the add-on lifecycle API) — that removal IS the qualification
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
| `none` (absent, unrelated same-name workload, or fully disabled) | not gated | ✅ resolves (add-on constraint permitting) |
| `active` (legacy DaemonSet targets ≥ 1 node) | not gated — when the add-on is installed it manages the same DaemonSet | ❌ fails closed: disable per pool or migrate to the add-on |
| `unknown` (snapshot could not consult the API) | not gated | ❌ fails closed |
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
managed `NvidiaGpuPlugin` add-on. The tripwire deliberately does not gate
`oci-managed`: when the managed add-on is installed it reconciles the same
DaemonSet name, so a healthy `oci-managed` cluster legitimately observes an
active DaemonSet.

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

## References

- [OKE: Running GPU Workloads](https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/contengrunninggpunodes.htm)
- [OKE cluster add-ons](https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/contengintroducingclusteraddons.htm)
- [oci-hpc-oke quickstart](https://github.com/oracle-quickstart/oci-hpc-oke) — worker images, RDMA manifests
- [AKS GPU Setup](aks-gpu-setup.md), [GKE GPU Setup](gke-gpu-setup.md) — the sibling `gpuStack` families
