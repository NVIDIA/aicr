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
| `driver-installer` | GPU Operator's device plugin (sole advertiser) | Google's standalone `nvidia-driver-installer` DaemonSet | **Every** GPU node carries `gke-no-default-nvidia-gpu-device-plugin=true` | Pools created with the label and `gpu-driver-version=disabled` | `devicePlugin.enabled=true` (profile-owned) |

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
label) qualifies `driver-installer`, and the negated form
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
#   ... or, for labeled pools with the standalone driver installer:
#   --profile gpuStack=driver-installer

# 3. Bundle.
aicr bundle -r recipe.yaml -o ./bundles
```

The reading qualifies the selection — it does not choose for you. Every
combination is deterministic:

| GPU-node labels read | Default (`gke-default`) | `--profile gpuStack=driver-installer` |
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
| `--profile gpuStack=driver-installer`, no `--snapshot` | `driver-installer` | same — deferred to validate |
| no `--profile`, `--snapshot` | default (`gke-default`) | checked at generation: no GPU node may carry the opt-out label, else generation fails closed naming the observed state |
| `--profile gpuStack=driver-installer`, `--snapshot` | `driver-installer` | checked at generation: every GPU node must carry `gke-no-default-nvidia-gpu-device-plugin=true`, else fails closed |

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
  --accelerator type=nvidia-h100-80gb,count=8,gpu-driver-version=default
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

### Alternative: Let GPU Operator Manage the Device Plugin

If you prefer the GPU Operator's device plugin to own `nvidia.com/gpu`
advertisement, select the mode at recipe generation:

```shell
aicr recipe --service gke --accelerator h100 --os cos --intent training \
  --platform kubeflow \
  --profile gpuStack=driver-installer -o recipe.yaml
aicr bundle -r recipe.yaml -o ./bundles
```

This value has real cluster prerequisites. The opt-out label forfeits GKE's
managed driver install: the managed install (`gpu-driver-version=default` or
`latest`) is finalized by an init container of the **same** kube-system
DaemonSet the label disables, so a labeled pool paired with the managed
install comes up **driverless** — never combine the label with
`gpu-driver-version=default`/`latest`. Pools for the `driver-installer` value
must instead be created with `gpu-driver-version=disabled`, with driver
provisioning supplied by Google's standalone
[`nvidia-driver-installer` DaemonSet](https://cloud.google.com/kubernetes-engine/docs/how-to/gpus#installing_drivers)
applied to the cluster.

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
standalone driver ready **first**, then the opt-out label, then the GPU
Operator. The label takes effect the moment it lands, disabling the
kube-system DaemonSet whose init container finalizes GKE's managed driver
install — so on a labeled pool still set to `gpu-driver-version=default` (or
`latest`), every node created in the interim (autoscaling, upgrade,
auto-repair) comes up **driverless**. Deploying the standalone installer
alone does not close that gap either — Google's `nvidia-driver-installer`
DaemonSet
[ignores nodes configured for automatic driver installation](https://cloud.google.com/kubernetes-engine/docs/troubleshooting/gpus#gpu_device_plugins_fail_with_crashloopbackoff_errors),
so it skips every node of a pool whose driver mode is still `default`. Only
the sequence below keeps the pool functional at every step.

**Step 1 — apply the standalone
[`nvidia-driver-installer` DaemonSet](https://cloud.google.com/kubernetes-engine/docs/how-to/gpus#installing_drivers).**
Applying it early is safe: it skips automatic-install nodes, so it is a
no-op until step 2 flips the pool's driver mode.

**Step 2 — switch the pool to `gpu-driver-version=disabled`** (restate the
pool's actual accelerator type and count):

```bash
gcloud container node-pools update POOL_NAME \
  --cluster CLUSTER_NAME \
  --location=LOCATION \
  --accelerator type=nvidia-h100-80gb,count=8,gpu-driver-version=disabled
```

The driver-mode update may re-create the pool's nodes; with the standalone
installer already applied, re-created and future nodes come up with a
driver, and GKE's device plugin (not yet disabled) keeps advertising
`nvidia.com/gpu` — the pool stays schedulable throughout.

**Step 3 — verify the driver before touching the label.** Every GPU node
should be running the installer's pods and still report non-zero allocatable
`nvidia.com/gpu` (advertised, for now, by GKE's plugin):

```bash
kubectl get pods -n kube-system -l k8s-app=nvidia-driver-installer -o wide
kubectl get nodes -l cloud.google.com/gke-accelerator \
  -o custom-columns='NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu'
```

**Step 4 — apply the opt-out label.** This begins the handoff: the label
immediately evicts GKE's managed plugin, so from this point until step 5's
Operator plugin registers, the pool has **no** `nvidia.com/gpu` advertiser
and GPU pods will not schedule. That brief advertiser-free window is the
accepted cost of the handoff direction — do **not** invert it by deploying
the Operator's plugin onto a still-unlabeled pool, which would put two
advertisers on the same nodes (the dual-advertisement state the
[allocation-policy gates](#the-three-driver-installer-settings) exist to
prevent). Have the bundle from step 5 generated in advance to keep the
window short, and avoid scheduling GPU work during it.
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

**Step 5 — deploy the GPU Operator and wait for its plugin.** Deploy the
AICR bundle generated with `--profile gpuStack=driver-installer`, then wait
until the Operator's device-plugin pods are Running on the labeled nodes and
every GPU node again reports non-zero allocatable `nvidia.com/gpu` — that
closes the advertiser-free window opened in step 4. Confirm the full result
with the checks in [Verifying the handoff](#verifying-the-handoff).

**Rollback:** if the Operator's device plugin fails to come up after the
label lands, remove the label (another `--node-labels` update passing the
full set with the opt-out label omitted) — GKE's plugin returns and the pool
resumes advertising GPUs, with the driver still supplied by the standalone
installer.

#### Verifying the handoff

The update applies the label to the pool's existing Node objects in place — it
does not re-create or replace nodes — and nodes created later inherit it. Once
the label lands, the DaemonSet controller reconciles asynchronously and evicts
GKE's managed plugin pods from the labeled nodes, so allow a short delay (pods
may show `Terminating` at first) before reading the checks below as failures.

Verify all three parts of the result — every GPU node shows the label, GKE's
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

#### The three driver-installer settings

The three settings cover different parts of the GPU stack:

- `gke-no-default-nvidia-gpu-device-plugin=true` disables **GKE's** device
  plugin so the Operator's plugin owns `nvidia.com/gpu` — and, as a side
  effect, forfeits GKE's managed driver install (the installer rides the
  DaemonSet the label disables).
- `gpu-driver-version=disabled` records that GKE does not own driver
  provisioning on the pool — and is what makes the standalone installer act
  on its nodes (the installer ignores automatic-install pools). Never pair
  the label with `gpu-driver-version=default` — labeled pools come up
  driverless.
- The standalone `nvidia-driver-installer` DaemonSet supplies the driver.
  AICR's GKE-COS overlays keep `driver.enabled: false` because the GPU
  Operator cannot install a driver on COS node images.

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
- **Commit to `driver-installer`:** apply Google's standalone
  `nvidia-driver-installer` DaemonSet and recreate the pools with
  `gpu-driver-version=disabled` (or update their driver mode in place — see
  [Retrofitting an existing pool](#retrofitting-an-existing-pool)), then
  generate recipes with `--profile gpuStack=driver-installer`.

### No advertiser at all

**Symptom:** GPU nodes report zero allocatable `nvidia.com/gpu` and GPU pods
stay `Pending`, even though the driver is present and healthy.

**Cause:** the pool is labeled but the GPU Operator's device plugin has not
(yet) registered. The label immediately evicts GKE's managed plugin, so
until the Operator's plugin comes up, the node has no `nvidia.com/gpu`
advertiser at all. A brief window in this state is the expected
intermediate step of the retrofit handoff (step 4 of
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
