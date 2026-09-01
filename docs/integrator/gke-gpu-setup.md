# GKE GPU Setup

## GPU Device-Plugin Ownership

GKE has two mutually exclusive GPU **device-plugin ownership modes**, expressed
as an ADR-015 `gpuStack` configuration profile on the GKE recipe family. The
selected value is chosen at recipe generation (`--profile`, or the declaration
default) and recorded in `metadata.selectedProfile`; each value carries a
constraint that the snapshot and the `aicr validate` readiness pre-flight
verify against the cluster's GPU-node labels.

| Value | `nvidia.com/gpu` advertiser | Driver provisioning | Node-label requirement | Pool creation | Recipe effect |
|------|------|------|------|------|------|
| `gke-default` (default) | GKE's managed device plugin (recorded as `advertiser: external`) | GKE's managed driver install | **No** GPU node carries `gke-no-default-nvidia-gpu-device-plugin` | Normal pools with `gpu-driver-version=default` or `latest` — zero extra setup | `devicePlugin.enabled=false` (profile-owned) |
| `bundle-installer` | GPU Operator's device plugin (sole advertiser) | the bundle's `gcp-driver-installer` DaemonSet (recipe-pinned version) | **Every** GPU node carries `gke-no-default-nvidia-gpu-device-plugin=true` | Pools created with the label and `gpu-driver-version=disabled` | `devicePlugin.enabled=true` (profile-owned) |

Both values keep `driver.enabled=false` in the GPU Operator values — the GPU
Operator cannot install a driver on COS node images, so driver provisioning is
never the operator's in either mode.

Exactly **one** `nvidia.com/gpu` advertiser per node is required. Two plugins
registering the same resource name is not a benign overlap: kubelet's device
manager keys its endpoint and device inventory by resource name, so competing
registrations replace each other, ownership becomes nondeterministic, and one
plugin's device IDs can reach the other plugin's `Allocate`. See
[Component Catalog › GKE Device-Plugin Ownership](../user/component-catalog.md#gke-device-plugin-ownership)
for the ownership model and the override-locking rules.

**Recording the ownership mode in snapshots.** Unlike AKS — whose ownership
signal is the Azure control-plane AgentPool `gpuProfile.driver` property and
therefore needs a provider pool projection
(`aicr snapshot --aks-gpu-pools <az dump>`) — the GKE signal is an ordinary
Kubernetes node label, so **no extra snapshot flag is needed**: a plain
`aicr snapshot` captures everything the constraint reads. Each value's
constraint is the `NodeTopology.gpu-nodes.label` node-set form
([#1755](https://github.com/NVIDIA/aicr/issues/1755)): the evaluator
synthesizes the GPU-node universe from the snapshot's `NodeTopology.label`
readings (nodes carrying `cloud.google.com/gke-accelerator`) and quantifies a
label predicate over it, in both directions — the positive form
`gke-no-default-nvidia-gpu-device-plugin=true` (every GPU node carries the
label) qualifies `bundle-installer`, and the negated form
`!gke-no-default-nvidia-gpu-device-plugin` (no GPU node carries the key)
qualifies `gke-default`.

**End-to-end flow.** Three steps; the snapshot carries the label readings
from step 1 on (recipe takes the snapshot, bundle takes the recipe):

```shell
# 1. Snapshot — no provider dump or extra flag needed on GKE.
aicr snapshot -o snapshot.yaml

# 2. Generate the recipe with the profile value your pools call for,
#    then bundle. Selection is explicit; the reading VERIFIES it.
aicr recipe --service gke --accelerator h100 --os cos --intent training \
  --platform kubeflow \
  --snapshot snapshot.yaml -o recipe.yaml                 # gke-default default
#   ... or, for labeled pools (bundle-carried driver installer):
#   --profile gpuStack=bundle-installer

# 3. Bundle. NVSentinel needs no overrides: the gpuStack profile sets its
#    labeler flag per value. See the NVSentinel note below.
aicr bundle -r recipe.yaml -o ./bundles
```

The reading qualifies the selection — it does not choose for you. Every
combination is deterministic:

| GPU-node labels read | Default (`gke-default`) | `--profile gpuStack=bundle-installer` |
|---|---|---|
| all GPU nodes label-absent | ✅ resolves | ❌ fails closed: constraint expects the label on every GPU node |
| all GPU nodes `gke-no-default-nvidia-gpu-device-plugin=true` | ❌ fails closed: constraint expects no labeled GPU node | ✅ resolves |
| mixed (some labeled, some not) | ❌ fails closed naming the observed state | ❌ fails closed |
| no identifiable GPU nodes (nothing carries `cloud.google.com/gke-accelerator`) | ❌ fails closed: empty GPU-node set | ❌ same |
| truncated reading (`--max-nodes-per-entry` actually cut a participating label reading) | ❌ fails closed — a truncated node list cannot prove set membership; recapture without the cap (a cap larger than the node count truncates nothing and validates normally) | ❌ same |

A wrong selection can never silently produce a mismatched recipe — the error
names the observed label state, and fixing it means changing the selection or
the pools, never overriding the values by hand.

**Selection and verification are independent axes.** `--profile` (or its
absence) decides the selection; `--snapshot` (or its absence) decides whether
the selection is verified now or later. The selection is NEVER derived from
the snapshot, and the check is NEVER skipped when a snapshot is present:

| Invocation | Selected value | Node-label check |
|---|---|---|
| no `--profile`, no `--snapshot` | declaration default (`gke-default`) | none possible (no cluster data) — the constraint is still recorded in the recipe and enforced at `aicr validate` readiness |
| `--profile gpuStack=bundle-installer`, no `--snapshot` | `bundle-installer` | same — deferred to validate |
| no `--profile`, `--snapshot` | default (`gke-default`) | checked at generation: no GPU node may carry the opt-out label, else generation fails closed naming the observed state |
| `--profile gpuStack=bundle-installer`, `--snapshot` | `bundle-installer` | checked at generation: every GPU node must carry `gke-no-default-nvidia-gpu-device-plugin=true`, else fails closed |

If you need an unverified recipe deliberately, generate criteria-only (drop
`--snapshot`): the artifact is honest about being unqualified, and the
`aicr validate` readiness pre-flight re-checks the constraint against a live
snapshot before any check Job deploys (see [Validation](../user/validation.md)).

### Default: Use the GKE-Default Profile

Create GPU node pools normally — no opt-out label, with GKE's managed driver
install (`gpu-driver-version=default` or `latest`):

```shell
gcloud container node-pools create POOL_NAME \
  --cluster CLUSTER_NAME \
  --location=LOCATION \
  --node-locations=ZONE \
  --num-nodes=1 \
  --machine-type=a3-highgpu-8g \
  --accelerator type=nvidia-h100-80gb,count=8,gpu-driver-version=default \
```

Two flags deserve care:

- `--machine-type` must match the accelerator (H100 GPUs are exclusive to the
  A3 series — `a3-highgpu-8g` for `nvidia-h100-80gb`, `a3-megagpu-8g` for
  `nvidia-h100-mega-80gb`); without the flag, `gcloud` defaults to `e2-medium`
  and pool creation fails.
- `--num-nodes` is **per zone**, defaults to 3, and an unrestricted pool on a
  regional cluster inherits every cluster zone — the defaults on a three-zone
  cluster would attempt nine 8-GPU nodes (72 H100s). Set `--num-nodes`
  explicitly and narrow `--node-locations` to the zones you intend.

No changes to AICR recipes are needed — this is the GKE family's `gpuStack`
configuration profile at its default value, `gke-default` (the resolved recipe
records `metadata.selectedProfile: gpuStack=gke-default` with
`advertiser: external`). GKE's managed device plugin advertises
`nvidia.com/gpu` and GKE's managed install provisions the driver, so the
recipe disables the GPU Operator's device plugin (`devicePlugin.enabled=false`,
profile-owned) and keeps `driver.enabled=false`. AICR's GPU Operator still
deploys and owns the rest of the GPU stack: the container toolkit, DCGM
(the host engine), the DCGM exporter, GPU Feature Discovery, the MIG
manager, and the operator validator — six DaemonSets on the GPU nodes.
Under `gke-default` **no device-plugin DaemonSet is rendered at all**
(`devicePlugin.enabled=false`): GKE's kube-system plugin is the sole
`nvidia.com/gpu` advertiser.

`aicr validate` verifies the value's constraint at readiness: **no** GPU node
(the nodes carrying `cloud.google.com/gke-accelerator`) may carry the opt-out
label `gke-no-default-nvidia-gpu-device-plugin`. A labeled node fails the
pre-flight closed (exit 2) before any check Job deploys.

**NVSentinel's driver label is configured by the profile.** The NVSentinel
labeler decides the node label `nvsentinel.dgxc.nvidia.com/driver.installed` by
watching for a GPU driver pod. Under `gke-default` the driver is provisioned at
pool creation and finalized by an init container of GKE's kube-system
DaemonSet, so there is **no driver pod the labeler can observe**. Left unset the
label is never applied, and the three DaemonSets that select on it —
`metadata-collector`, `syslog-health-monitor-regular`, and
`syslog-health-monitor-kata` — come up with **0 desired pods**
([#2175](https://github.com/NVIDIA/aicr/issues/2175)).

This is easy to miss: a DaemonSet matching no node is not unhealthy, so it
reports no error and emits no event, and `gpu-health-monitor` keeps running
because it selects on the DCGM label instead. The stack presents as fully
rolled out.

The `gpuStack` profile sets `nvsentinel.labeler.assumeDriverInstalled` per
value ([#2181](https://github.com/NVIDIA/aicr/issues/2181)), so no bundle-time
override is needed:

| `gpuStack` value | `labeler.assumeDriverInstalled` | Why |
|---|---|---|
| `gke-default` (default) | `true` | no driver pod exists to observe |
| `bundle-installer` | `false` | the bundle's `gcp-driver-installer` DaemonSet supplies one |

Because the path is profile-owned, a bundle-time `--set` diverging from the
selected value is **rejected** rather than silently applied. The explicit
`false` under `bundle-installer` is deliberate: skipping detection there would
keep the label applied across an unloaded driver.

The value renders the labeler's `--assume-driver-installed` argument — the
chart-level automation of the Manual Labeling Procedure in NVSentinel design
018 — and, per the upstream decision in
[NVIDIA/NVSentinel#1583](https://github.com/NVIDIA/NVSentinel/issues/1583), the
recommended, permanent mechanism for host-installed drivers (no automatic
detection fallback will be added). Under `gke-default` a recipe that reaches
bundle generation without it is a **blocking error**
(`CheckNVSentinelDriverLabelDetectable`), so the silent half-rollout cannot
ship. Under `bundle-installer` the gate does not fire: the bundle's installer
supplies an observable driver pod.

**Labeling the nodes by hand does not persist.** Applying the label manually:

```shell
kubectl label node <node> nvsentinel.dgxc.nvidia.com/driver.installed=true
```

takes effect immediately and then silently reverts — with no driver pod to
observe, the labeler computes an empty desired value and removes the label on
its next reconcile. Design 018 documents manual labeling as the procedure for
this case, so an operator following it will see it work and later find the
DaemonSets back at 0 desired.

### Alternative: Let the Bundle Own the GPU Stack

If you prefer the GPU Operator's device plugin to own `nvidia.com/gpu`
advertisement, select the mode at recipe generation:

```shell
aicr recipe --service gke --accelerator h100 --os cos --intent training \
  --platform kubeflow \
  --profile gpuStack=bundle-installer -o recipe.yaml
aicr bundle -r recipe.yaml -o ./bundles
```

This value has real pool prerequisites. The opt-out label forfeits GKE's
managed driver install: the managed install (`gpu-driver-version=default` or
`latest`) is finalized by an init container of the **same** kube-system
DaemonSet the label disables, so a labeled pool paired with the managed
install comes up **driverless** — never combine the label with
`gpu-driver-version=default`/`latest`. Pools for the `bundle-installer` value
must instead be created with `gpu-driver-version=disabled`. Driver
provisioning is carried **inside the bundle**: the `gcp-driver-installer`
component ships Google's cos-gpu-installer DaemonSet
([#1716](https://github.com/NVIDIA/aicr/issues/1716)), ordered ahead of the
GPU Operator, with the driver version pinned in the recipe
(`gcp-driver-installer.driverVersion`). The pin must be COS-qualified: the installer validates
the request against the COS build's curated per-GPU-type list and rejects
unqualified versions. Version bumps take effect on replaced or rebooted
nodes only (the installer skips nodes with a loaded nvidia module).

Set the label when you create the GPU node pool, alongside the disabled
managed install:

```bash
gcloud container node-pools create POOL_NAME \
  --cluster CLUSTER_NAME \
  --location=LOCATION \
  --node-locations=ZONE \
  --num-nodes=1 \
  --machine-type=a3-highgpu-8g \
  --accelerator type=nvidia-h100-80gb,count=8,gpu-driver-version=disabled \
  --node-labels="gke-no-default-nvidia-gpu-device-plugin=true"
```

The `--machine-type` and `--num-nodes` cautions from the default-profile
section above apply here unchanged.

#### Retrofitting an existing pool

For a GPU node pool that already exists, do the retrofit in this order:
driver mode first, then the opt-out label, then the bundle. The bundle
carries the driver installer, so — unlike the hand-applied arrangement this
replaces — there is nothing to apply out-of-band, but the installer only
arrives with the bundle in step 3: between step 1 and step 3 the pool has a
scheduling gap, described per step below. Plan the retrofit as one sitting
with the bundle generated in advance.

**Step 0 — if migrating from a hand-applied installer, delete it first.**
The bundle's DaemonSet shares the name `nvidia-driver-installer` in
`kube-system`, and Helm will not adopt a pre-existing object — step 3 would
fail. Nodes keep their loaded drivers; only the provisioning workload is
replaced.

```bash
kubectl delete daemonset -n kube-system nvidia-driver-installer --ignore-not-found
```

**Step 1 — switch the pool to `gpu-driver-version=disabled`** (restate the
pool's actual accelerator type and count):

```bash
gcloud container node-pools update POOL_NAME \
  --cluster CLUSTER_NAME \
  --location=LOCATION \
  --accelerator type=nvidia-h100-80gb,count=8,gpu-driver-version=disabled
```

The driver-mode update may re-create the pool's nodes. Until step 3's
installer runs, re-created nodes come up **driverless while GKE's plugin
still advertises** `nvidia.com/gpu` on them — GPU pods scheduled there will
fail. Avoid scheduling GPU work from here until the handoff completes
(cordon the pool's nodes for GPU workloads if the cluster is busy).

**Step 2 — apply the opt-out label.** This evicts GKE's managed plugin, so
from this point until step 3's Operator plugin registers, the pool has
**no** `nvidia.com/gpu` advertiser — which also stops the driverless nodes
from being advertised. Do **not** invert the order by deploying the bundle
onto a still-unlabeled pool: that would put two advertisers on the same
nodes (the dual-advertisement state the
[allocation-policy gates](#the-three-bundle-installer-settings) exist to
prevent).
Note that `--node-labels` on update **replaces** the pool's full user-label
set: first list the labels the pool already carries, then pass the complete
set with the new label appended:

```bash
gcloud container node-pools describe POOL_NAME \
  --cluster CLUSTER_NAME \
  --location=LOCATION \
  --format='value[delimiter=","](config.labels)'

gcloud container node-pools update POOL_NAME \
  --cluster CLUSTER_NAME \
  --location=LOCATION \
  --node-labels="EXISTING_KEY_1=EXISTING_VALUE_1,gke-no-default-nvidia-gpu-device-plugin=true"
```

Replace `EXISTING_KEY_…=EXISTING_VALUE_…` with every label the `describe`
command returned (drop it entirely if the pool has none). The `delimiter=","`
attribute makes the output comma-separated, matching what `--node-labels`
expects — without it, `value(config.labels)` joins entries with semicolons,
which the update rejects. Omitting an existing label removes it from the
pool's nodes, which can break scheduling that depends on it.

**Step 3 — deploy the bundle.** Deploy the AICR bundle generated with
`--profile gpuStack=bundle-installer`. Its `gcp-driver-installer` DaemonSet
installs the pinned driver on the labeled, driver-mode-disabled nodes
(nodes that already have a loaded driver are skipped), and the GPU
Operator's device plugin registers once the driver is ready — closing the
window opened in steps 1–2. Wait until every GPU node again reports
non-zero allocatable `nvidia.com/gpu`, then confirm the full result with
the checks in [Verifying the handoff](#verifying-the-handoff).

**Rollback:** if the Operator's device plugin fails to come up after the
label lands, remove the label (another `--node-labels` update passing the
full set with the opt-out label omitted) — GKE's plugin returns and the
pool resumes advertising GPUs. Nodes the bundle's installer already
provisioned keep their driver; nodes re-created before step 3 ran are
driverless until the pool's driver mode is restored
(`gpu-driver-version=default`) or the bundle deploys.

#### Verifying the handoff

The update applies the label to the pool's existing Node objects in place — it
does not re-create or replace nodes — and nodes created later inherit it. Once
the label lands, the DaemonSet controller reconciles asynchronously and evicts
GKE's managed plugin pods from the labeled nodes, so allow a short delay (pods
may show `Terminating` at first) before reading the checks below as failures.

Verify all three parts of the result — every GPU node shows both labels, GKE's
managed plugin pods (kube-system, `k8s-app=nvidia-gpu-device-plugin`) are gone
from those nodes, and the GPU Operator's plugin has actually taken ownership
(its device-plugin pods are Running and every GPU node reports non-zero
allocatable `nvidia.com/gpu`):

```bash
kubectl get nodes -l cloud.google.com/gke-accelerator \
  -L gke-no-default-nvidia-gpu-device-plugin
kubectl get pods -n kube-system -l k8s-app=nvidia-gpu-device-plugin -o wide
kubectl get pods -n gpu-operator -l app=nvidia-device-plugin-daemonset -o wide
kubectl get nodes -l cloud.google.com/gke-accelerator \
  -o custom-columns='NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu'
```

The second list should be empty (or show pods only on GPU nodes you have not
labeled). The third check matters because the label only removes GKE's
advertiser — if the GPU Operator is not yet deployed (or its plugin is not
Ready), labeling leaves the node with **no** `nvidia.com/gpu` advertiser at
all, and GPU pods will not schedule until the Operator's plugin comes up.

#### The three bundle-installer settings

The three settings cover different parts of the GPU stack:

- `gke-no-default-nvidia-gpu-device-plugin=true` disables **GKE's** device
  plugin so the Operator's plugin owns `nvidia.com/gpu` — and, as a side
  effect, forfeits GKE's managed driver install (the installer rides the
  DaemonSet the label disables).
- `gpu-driver-version=disabled` records that GKE does not own driver
  provisioning on the pool — and is what makes the bundle's installer act
  on its nodes (it ignores automatic-install pools). Never pair the label
  with `gpu-driver-version=default` — labeled pools come up driverless.
- The bundle's `gcp-driver-installer` DaemonSet supplies the driver.
  AICR's GKE-COS overlays keep `driver.enabled: false` because the GPU
  Operator cannot install a driver on COS node images.

#### Migrating from a hand-applied installer DaemonSet

Earlier arrangements (including AICR's replaced `driver-installer` profile
value, shipped in v0.19.0) supplied the driver by applying Google's standalone
[`nvidia-driver-installer` DaemonSet](https://cloud.google.com/kubernetes-engine/docs/how-to/gpus#installing_drivers)
by hand on the same pool shape. Do not run that alongside a
`bundle-installer` bundle: the bundle's DaemonSet shares the name
`nvidia-driver-installer` in `kube-system`, and Helm will not adopt the
pre-existing object. To migrate: delete the hand-applied DaemonSet,
regenerate with `--profile gpuStack=bundle-installer`, and deploy the
bundle. Nodes with a loaded driver are untouched (the installer's fast path
skips them); the bundle takes over provisioning for new and rebooted nodes.

## DRA kubelet plugin node labels are not required on GKE

DRA eviction coordination is opt-in, and by default it does nothing on
supported GKE recipes. Every GKE overlay is COS, and both the default and
`gpuStack=bundle-installer` profiles keep GPU Operator's `driver.enabled=false`
— `bundle-installer` provisions the driver through a separate
`gcp-driver-installer` DaemonSet, not a GPU Operator driver pod. With no GPU
Operator driver pod there is no Driver Manager to coordinate with.

So bundles generated for GKE add no eviction node selector, and the DRA kubelet
plugin runs on every accelerated node with no extra label. Nothing in the node
pool commands below needs `nvidia.com/dra-kubelet-plugin`.

If you opt in anyway, AICR renders the selector and every GPU node must carry
the **same `key=value` pair** you passed, set in the node pool definition:

```bash
aicr bundle --recipe recipe.yaml \
  --dra-eviction-node-label nvidia.com/dra-kubelet-plugin=true \
  --output bundle

gcloud container node-pools create <pool> --cluster <cluster> \
  --node-labels="nvidia.com/dra-kubelet-plugin=true"
```

See [DRA Driver Upgrade Eviction](../user/cli-reference.md#dra-driver-upgrade-eviction).
This applies to existing clusters too — adding the selector during an
upgrade removes a plugin that was previously working.
See [Prepare DRA nodes when opting in to eviction coordination](../user/bundling.md#prepare-dra-nodes-when-opting-in-to-eviction-coordination).

## Troubleshooting

### Labeled pool comes up driverless

**Symptom:** on a GPU pool that carries
`gke-no-default-nvidia-gpu-device-plugin=true` but was created with
`gpu-driver-version=default` or `latest`, nodes come up with the driver
installer's `.run` package staged on disk but never executed — no `nvidia`
kernel module is loaded, `/dev/nvidia*` device nodes do not exist, the node
reports **zero** allocatable `nvidia.com/gpu`, and the GPU Operator stack
blocks with its toolkit / driver-validation init containers looping (they wait
for a driver that never arrives).

**Cause:** the managed driver install is finalized by an init container of the
same kube-system DaemonSet the opt-out label disables. Labeling the pool
disabled the whole DaemonSet — device plugin *and* driver finalization — so
the pairing "label + managed driver install" is never functional.

**Fix — pick one exit:**

- **Stay on the default `gke-default` value:** remove the label from the
  labeled pools (a pool update passing the full label set with the opt-out
  label omitted — see the replacement caveat in
  [Retrofitting an existing pool](#retrofitting-an-existing-pool)) so GKE's
  DaemonSet returns and finalizes the managed install, and generate (or keep)
  recipes with the default selection.
- **Commit to `bundle-installer`:** recreate the pools with
  `gpu-driver-version=disabled` (or update their driver mode in place — see
  [Retrofitting an existing pool](#retrofitting-an-existing-pool)), then
  generate recipes with `--profile gpuStack=bundle-installer`.

### No advertiser at all

**Symptom:** GPU nodes report zero allocatable `nvidia.com/gpu` and GPU pods
stay `Pending`, even though the driver is present and healthy.

**Cause:** the pool is labeled but the GPU Operator's device plugin has not
(yet) registered. The label immediately evicts GKE's managed plugin, so
until the Operator's plugin comes up, the node has no `nvidia.com/gpu`
advertiser at all. A brief window in this state is the expected
intermediate step of the retrofit handoff (step 2 of
[Retrofitting an existing pool](#retrofitting-an-existing-pool)); it is a
problem only when nothing closes it.

**Fix:** deploy the AICR bundle (or at least the GPU Operator) and wait for
its device-plugin pods to be Running on the labeled nodes; allocatable GPU
counts return once the plugin registers. Keep the handoff order — label
first, then the Operator — in future rollouts too: deploying the Operator's
plugin onto a still-unlabeled pool puts two advertisers on the same nodes
(the dual-advertisement state the allocation-policy gates reject). Keep the
window short instead: have the bundle generated before labeling, deploy
immediately after, and avoid scheduling GPU work in between. Run the
three-part verification in [Verifying the handoff](#verifying-the-handoff) to
confirm exactly which advertiser owns each node.

## References

- [GKE GPU node-pool guide](https://cloud.google.com/kubernetes-engine/docs/how-to/gpus)
- [NVIDIA GPU Operator on Google GKE](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/google-gke.html)
- [Component Catalog › GKE Device-Plugin Ownership](../user/component-catalog.md#gke-device-plugin-ownership)
- [Validation readiness gate](../user/validation.md)
- [GKE TCPXO Networking](gke-tcpxo-networking.md)
