# AKS GPU Setup

## Kubernetes Version Requirement

AICR requires **Kubernetes 1.34 or later** on AKS. This is driven by DRA (Dynamic
Resource Allocation), which is included in every AICR recipe.

The core DRA APIs (`resource.k8s.io`) **graduated to GA (stable `v1`)** in
Kubernetes 1.34. No AKS-specific feature flag is needed — DRA is enabled out of
the box once you're on 1.34+.

```shell
# Create a cluster on 1.34
az aks create \
  --resource-group <rg> \
  --name <cluster> \
  --kubernetes-version 1.34 \
  --enable-oidc-issuer \
  --enable-workload-identity \
  --enable-managed-identity \
  --generate-ssh-keys

# Upgrade an existing cluster to 1.34
az aks upgrade \
  --resource-group <rg> \
  --name <cluster> \
  --kubernetes-version 1.34
```

You can verify DRA is available after the upgrade:

```shell
kubectl api-resources --api-group=resource.k8s.io
```

Expected output includes `deviceclasses`, `resourceclaims`, `resourceclaimtemplates`,
and `resourceslices`.

> **Note:** Kubernetes version skipping is not allowed. If your cluster is on 1.32,
> you must upgrade to 1.33 first, then to 1.34.

## Dynamic Resource Allocation (DRA)

All AKS GPU recipes include the `nvidia-dra-driver-gpu` component, which exposes
GPU resources via the Kubernetes DRA API. In the production default,
whole-GPU allocation goes through the device plugin (`nvidia.com/gpu` limits),
while DRA serves ComputeDomain/IMEX channels and other structured resources —
claim-based allocation, structured device advertisement, and gang-scheduling
integration.

### Feature Gate Details

| Kubernetes Version | DRA Status | Feature Gate |
|--------------------|-----------|--------------|
| 1.26–1.29 | Alpha | `DynamicResourceAllocation` — off by default |
| 1.30–1.33 | Beta | `DynamicResourceAllocation` — on by default |
| 1.34+ | **GA / Stable** | `resource.k8s.io/v1` — always enabled, no feature gate needed |

On AKS 1.34, DRA is GA. You do not need to pass any custom API server flags or
register an AKS preview feature.

### Configuring the allocation mode

The whole-GPU allocation mode is an **allocation policy** that validators
resolve from the recipe's hydrated values and verify against the cluster,
failing closed on mismatch
([#1327](https://github.com/NVIDIA/aicr/issues/1327)) — so configure it in a
recipe overlay, not at bundle time. Bundle-time `--set` / `--set-json` /
`--set-file` overrides of the nested policy keys
(`dradriver:resources.gpus.enabled`, `dradriver:gpuResourcesEnabledOverride`,
`gpuoperator:devicePlugin.enabled`, and the same key on `gpu-operator-ocp`)
still work but are **deprecated** and log a warning; the component-level
`enabled` toggle of those components — disabling an advertiser changes the
policy exactly like the nested keys — is honored only via scalar `--set`
(the typed `--set-json`/`--set-file` path rejects `enabled` for every
component) and likewise warns when used this way. In every case:
validators verify the recipe-resolved policy, so a bundle-time change
surfaces as recipe/cluster drift at validation time. `--dynamic` declarations
on these keys are rejected outright — the value would be unknowable when the
policy is resolved.

### Device Plugin vs DRA

Exactly **one whole-GPU advertiser per node** is required. Enabling both
concurrently causes GPU over-admission — the two allocators keep independent
ledgers, so dual advertisement can double-allocate or contend for the same
physical GPU — and **recipe-backed validation rejects** a dual-advertised
configuration with an invalid-request error at policy-resolution time
(running `aicr validate` is what enforces this; recipe/bundle generation
alone does not invoke the resolver, so skipping validation bypasses the
check).

**Device-plugin whole-GPU allocation is the production default** — stock
recipes ship it out of the box (`resources.gpus.enabled: false` and
`gpuResourcesEnabledOverride: false` in the `nvidia-dra-driver-gpu` component
values, `devicePlugin.enabled: true` in the `gpu-operator` values), so no
overlay or `--set` is needed. The DRA driver stays active for
ComputeDomain/IMEX and other non-GPU resources; only its full-GPU
advertisement is disabled.

For DRA-only (experimental — the validators exercise ResourceClaims and
discover DRA-only nodes from the allocation probe under this policy, but full
`aicr validate` is not guaranteed until the
[#1327](https://github.com/NVIDIA/aicr/issues/1327) graduation checklist
passes; the stock demo/workload manifests request scalar `nvidia.com/gpu`
and are likewise unschedulable there), the opt-in overlay must change all
three values together — a partial flip is rejected at resolution time (dual
advertisement, an inert waiver, or no advertiser at all):

```yaml
spec:
  componentRefs:
    - name: nvidia-dra-driver-gpu
      overrides:
        gpuResourcesEnabledOverride: true
        resources:
          gpus:
            enabled: true
    - name: gpu-operator
      overrides:
        devicePlugin:
          enabled: false
```

## GPU Driver Setup

AKS has two mutually exclusive GPU **ownership modes**. Each is a complete
provisioning profile — the nodepool creation flags and the GPU Operator values
must come from the *same* mode. Mixing them (for example, a `--gpu-driver none`
pool with `toolkit.enabled=false`) leaves containerd without a working `nvidia`
runtime handler: every GPU Operator operand fails with
`FailedCreatePodSandBox: no runtime for "nvidia" is configured` and GPU nodes
advertise zero `nvidia.com/gpu`.

| Mode | Nodepool | GPU Operator values |
|------|----------|---------------------|
| AKS azure-managed (default) | AKS "Driver only" install profile (`--enable-managed-gpu=false`, the AKS default) | `driver.enabled=false`, `toolkit.enabled=false`, `operator.runtimeClass=nvidia-container-runtime` (recipe defaults) |
| GPU Operator-managed | `--gpu-driver none` (AKS "None/BYO" install profile) | `driver.enabled=true`, `toolkit.enabled=true`, `operator.runtimeClass=nvidia`, plus `dradriver:nvidiaDriverRoot=/run/nvidia/driver` (all together) |

AICR's default follows the CSP default: an `az aks nodepool add` without GPU
driver flags preinstalls the NVIDIA driver and container toolkit from the AKS
node image, and the AKS recipes ship the matching GPU Operator values.

Both modes use an *unmanaged* GPU node pool. Do not combine AICR with
AKS-managed GPU node pools (`--enable-managed-gpu=true`, preview —
`gpuProfile.nvidia.managementMode: Managed`): that profile makes AKS install
its own device plugin, DCGM exporter, and GPU health tooling, which duplicate
and conflict with the GPU Operator operands AICR deploys. See
[AKS install profiles](https://learn.microsoft.com/en-us/azure/aks/aks-managed-gpu-nodes#install-profiles).

**Recording the pool mode in snapshots.** The ownership mode lives in the
Azure control plane (AgentPool `gpuProfile.driver`), not in any Kubernetes
API object, so a snapshot cannot observe it from inside the cluster. To
record it, dump the node pools and pass the file to `aicr snapshot`:

```shell
az aks nodepool list \
  --cluster-name <cluster> \
  --resource-group <rg> \
  -o json > pools.json

aicr snapshot --aks-gpu-pools pools.json -o snapshot.yaml
```

The GPU pools' driver modes are projected into the snapshot's
`K8s.aks-gpu-pools.gpu-driver` reading (`Install` for azure-managed pools,
`None` for `--gpu-driver none` pools; disagreeing or AKS-managed pools
project a value that fails recipe qualification closed). `aicr validate`
accepts the same `--aks-gpu-pools` flag when it captures a live snapshot.
The projection runs on the machine invoking the CLI — the file never
enters the cluster — and a malformed or missing file fails the command
before any cluster work.

**End-to-end flow.** Three steps; the pool dump is consumed only at step 2
(the snapshot carries the reading from then on — recipe takes the snapshot,
bundle takes the recipe):

```shell
# 1. Dump the node pools (the mode lives in the Azure control plane).
az aks nodepool list -g <rg> --cluster-name <cluster> -o json > pools.json

# 2. Snapshot with the projection (agent Job or local mode).
aicr snapshot --aks-gpu-pools pools.json -o snapshot.yaml

# 3. Generate the recipe with the profile value your pools call for,
#    then bundle. Selection is explicit; the reading VERIFIES it.
aicr recipe --service aks --accelerator h100 --os ubuntu --intent training \
  --snapshot snapshot.yaml -o recipe.yaml                 # azure-managed default
#   ... or, for --gpu-driver none pools:
#   --profile gpuStack=operator-managed
aicr bundle -r recipe.yaml -o ./bundles
```

The reading qualifies the selection — it does not choose for you. Every
combination is deterministic:

| Pools read | Default (`azure-managed`) | `--profile gpuStack=operator-managed` |
|---|---|---|
| `Install` (AKS installs the driver) | ✅ resolves | ❌ fails closed: constraint expects `None` |
| `None` (`--gpu-driver none`) | ❌ fails closed: constraint expects `Install` | ✅ resolves |
| `Mixed` / `Managed` | ❌ fails closed naming the observed state | ❌ fails closed |
| no reading (snapshot captured without `--aks-gpu-pools`) | ❌ fails closed: reading **unavailable** — recapture the snapshot with the pool dump | ❌ same |

A wrong selection can never silently produce a mismatched recipe — the
error names the observed pool state, and fixing it means changing the
selection, the pools, or recapturing, never overriding the values by hand.

**Selection and verification are independent axes.** `--profile` (or its
absence) decides the selection; `--snapshot` (or its absence) decides
whether the selection is verified now or later. The selection is NEVER
derived from the snapshot, and the check is NEVER skipped when a snapshot
is present:

| Invocation | Selected value | Pool-mode check |
|---|---|---|
| no `--profile`, no `--snapshot` | declaration default (`azure-managed`) | none possible (no cluster data) — the constraint is still recorded in the recipe and enforced at `aicr validate` readiness |
| `--profile gpuStack=operator-managed`, no `--snapshot` | `operator-managed` | same — deferred to validate |
| no `--profile`, `--snapshot` | default (`azure-managed`) | checked at generation: pools must read `Install`, else generation fails closed naming the observed state |
| `--profile gpuStack=operator-managed`, `--snapshot` | `operator-managed` | checked at generation: pools must read `None`, else fails closed |

A snapshot without the pool reading (captured without `--aks-gpu-pools`)
fails closed for either selection — never a silent skip. If you need an
unverified recipe deliberately, generate criteria-only (drop
`--snapshot`): the artifact is honest about being unqualified, and
validate re-checks when a snapshot exists.

### Default: Use the AKS Azure-Managed Profile

Create nodepools with the AKS **Driver only** install profile — the AKS
default, so simply omit `--gpu-driver none`:

```shell
az aks nodepool add \
  --cluster-name <cluster> \
  --resource-group <rg> \
  --name gpupool \
  --node-vm-size Standard_ND96isr_H100_v5 \
  --node-count 1
```

No changes to AICR recipes are needed — this is the AKS family's `gpuStack`
configuration profile at its default value, `azure-managed` (the resolved
recipe records `metadata.selectedProfile: gpuStack=azure-managed`).
The AKS node image preinstalls the NVIDIA driver and container toolkit and
preconfigures containerd, so the recipe defaults (`driver.enabled=false`,
`toolkit.enabled=false`, `operator.runtimeClass=nvidia-container-runtime`)
leave driver and container-runtime ownership with AKS. AICR's GPU Operator
still deploys and owns the rest of the GPU stack: device plugin, DCGM
exporter, GPU Feature Discovery, and the validator.

Driver lifecycle and compatibility stay with Microsoft's node-image QA —
driver versions follow the AKS node-image release cadence rather than the
AICR recipe's pins. `operator.runtimeClass` must match the runtime handler
preconfigured on the AKS node image; NVIDIA's AKS example uses
`nvidia-container-runtime` (see
[GPU Operator on Microsoft AKS](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/microsoft-aks.html)).

`driver.rdma.useHostMofed` remains `false` in this mode — it is inert while
`driver.enabled=false`, but keeping it correct prevents a later ownership-mode
change from reviving the `nvidia_peermem` symbol-mismatch bug (see the comment
in `recipes/components/gpu-operator/values-aks.yaml`).

**GPUDirect RDMA (`nvidia-peermem`) under the azure-managed profile.** With
`driver.enabled=false` there is no GPU Operator driver DaemonSet to load the
`nvidia-peermem` kernel module (`driver.rdma.enabled` is inert), and the
AKS-managed driver install does not load it automatically. Azure's RDMA
guidance requires a dedicated reloader in this mode, so the AKS recipes ship
Azure's `nvidia-peermem-reloader` DaemonSet as a `network-operator` manifest
(`recipes/components/network-operator/manifests/nvidia-peermem-reloader.yaml`):
a retry loop keeps re-asserting `modprobe nvidia-peermem` (via the node's own
kmod) until the driver and the OFED `ib_core` stack are both present, and
re-loads it if a driver update unloads it. The pod deliberately reports Ready
immediately — deployers helm-wait on the network-operator manifests before
gpu-operator installs, so a modprobe-gated readiness probe would deadlock the
GPU-Operator-managed alternative profile below, where the driver only appears
after gpu-operator deploys (and where the operator's driver pod loads
`nvidia-peermem` itself, making the reloader a harmless no-op). It targets
IB-capable nodes (the same `pci-15b3.present` NFD label as the
NicClusterPolicy) and is removed together with the RDMA stack by the
documented opt-out (`--set networkoperator:enabled=false`). See
[Azure's GPU driver guidance](https://azure.github.io/aks-rdma-infiniband/configurations/gpu-drivers).

**Stub `nvidia-peermem` builds and the DMA-BUF path.** The reloader
re-asserts the module where the module is loadable — it cannot make an
unloadable module load. On some AKS node images (observed on
`AKSUbuntu-2404gen2containerd-202606.19.0`, kernel `6.8.0-1059-azure`,
preinstalled driver 580.126.09) the preinstalled
`/lib/modules/<kernel>/updates/dkms/nvidia-peermem.ko` is a stub DKMS build:
`modprobe nvidia-peermem` fails with `Invalid argument` and no dmesg trace —
the signature of NVIDIA's conftest finding no OFED-style headers at image
bake and compiling the peer-memory support out — even though the running
kernel exports `ib_register_peer_memory_client` and the IB stack (`ib_core`,
`mlx5_ib`) is loaded after nodewright host prep. On such images the
reloader's retry loop is the expected steady state ("not loadable yet",
indefinitely), and GPUDirect RDMA is carried by the kernel's DMA-BUF path
instead (driver 580 + kernel 6.8 + mlx5 + a DMA-BUF-capable NCCL) — that
path delivered the validated NCCL bandwidth below. Only when neither path is
available — no loadable `nvidia-peermem` and no DMA-BUF support — is
GPUDirect RDMA lost; NCCL then falls back to host-memory staging and the
calibrated all-reduce gate will regress or fail.

**Validation status.** This profile is validated end-to-end on AKS H100
(2x `Standard_ND96isr_H100_v5`, node image
`AKSUbuntu-2404gen2containerd-202606.19.0`, preinstalled driver 580.126.09)
for both intents: the deployment phase passes all four checks
(operator-health, expected-resources, gpu-operator-version,
check-nvidia-smi) for training and inference, and the training performance
phase passes the calibrated NCCL all-reduce gate at 157.29 GB/s (16 GiB
message size across 2 nodes, gate >= 150). The Dynamo inference counterpart
(`h100-aks-ubuntu-inference-dynamo`) passes `inference-perf` at
148,004 tok/s throughput (gate `>= 50,000`) with 579.70 ms TTFT p99
(gate `<= 2,000`) — Qwen/Qwen3-8B at 256 concurrency per GPU via
dynamo-router on a single ND96isr node. Other SKUs and node images have
not been exercised — run `aicr validate` after deployment and report gaps.

**Inference-perf model-cache cold load on the default StorageClass.** The
`inference-perf` check stages the model checkpoint on a model-cache PVC; on
AKS the cluster-default Azure disk StorageClass can be slow enough that a
cold load — several decode workers reading the checkpoint shards
concurrently from one RWO PVC — exceeds the check's default 10-minute
workload-ready window. The check then fails with
`DynamoGraphDeployment not ready: TIMEOUT` while the workers are healthily
loading (not a performance failure — the run above passed once the window
was raised). Two remedies: override the catalog entry via `--data` (see
[Validator Extension](validator-extension.md)) setting the
`AICR_INFERENCE_PERF_WORKLOAD_READY_TIMEOUT` and
`AICR_INFERENCE_PERF_HEALTH_TIMEOUT` envs and raising the entry `timeout`
in tandem, or point `AICR_INFERENCE_PERF_MODEL_CACHE_STORAGE_CLASS` at a
faster class (for example premium SSD v2).

**Device-isolation hardening under the azure-managed profile.** The AKS node
image preinstalls the NVIDIA container toolkit with the upstream permissive
defaults in `/etc/nvidia-container-runtime/config.toml`
(`accept-nvidia-visible-devices-envvar-when-unprivileged = true`,
`accept-nvidia-visible-devices-as-volume-mounts = false`). Because
containerd's default runtime on GPU nodes is `nvidia-container-runtime`, every
container passes through the toolkit, so an unprivileged pod whose image sets
`NVIDIA_VISIBLE_DEVICES=all` (the default in most CUDA base images) would be
handed all host GPUs with no Kubernetes allocation — breaking container-level
GPU isolation and failing the `secure-accelerator-access` conformance check.
With `driver.enabled=false` and `toolkit.enabled=false` there is no operator
toolkit DaemonSet to harden the config, so the AKS recipes ship a small
`nvidia-toolkit-hardening` DaemonSet (a `gpu-operator` manifest) that re-asserts
`accept-nvidia-visible-devices-envvar-when-unprivileged = false` and
`accept-nvidia-visible-devices-as-volume-mounts = true` via `nvidia-ctk config
--in-place`. The toolkit re-reads its config at every container start, so no
containerd restart is needed and running pods are unaffected. Its readiness
probe is fail-closed — the pod reports Ready only while the current assert
succeeds — so it surfaces a node's **inability to harden** (`nvidia-ctk`
missing or incompatible, so the keys can never be set) rather than passing
green. It does not detect a silent permissive revert as a readiness
transition: a transient permissive-but-valid rewrite of `config.toml` is simply
repaired by the next `--in-place` re-assert within ≤60s (the re-assert
succeeds, so Ready never flips — the config is corrected, not flagged as
drift).

The DaemonSet renders **only** in the azure-managed profile
(`toolkit.enabled=false`). Under the GPU-Operator-managed fallback
(`--gpu-driver none` + `toolkit.enabled=true`) it is omitted, because the
operator owns the toolkit there — the AKS values set the same hardened keys via
`gpu-operator.toolkit.env`
(`ACCEPT_NVIDIA_VISIBLE_DEVICES_ENVVAR_WHEN_UNPRIVILEGED=false`,
`ACCEPT_NVIDIA_VISIBLE_DEVICES_AS_VOLUME_MOUNTS=true`), which the toolkit
installer writes into the config, and gating the DaemonSet out avoids a
helm-wait deadlock on nodes where `nvidia-ctk` is not preinstalled.

**Scope of the hardening.** Setting the env-var key to `false` closes the
`NVIDIA_VISIBLE_DEVICES` env-var path — the path `secure-accelerator-access`
exercises. Setting `accept-nvidia-visible-devices-as-volume-mounts = true` is
required so the device plugin's volume-mounts allocation strategy still works
for legitimately allocated pods, but it leaves the **volume-mounts
device-request path open** (a pod that declares a `/dev/null`-backed mount whose
destination is under `/var/run/nvidia-container-devices` can still select
devices — the pinned toolkit v1.19.1 accepts the volume-mount device request
only when the mount *source* is `/dev/null`). This is the same posture as
GPU-Operator-managed mode — not a regression — but it means full multi-tenant
isolation additionally requires an admission policy restricting a
`/dev/null`-backed mount whose destination is under
`/var/run/nvidia-container-devices`. A
green `secure-accelerator-access` result confirms the env-var path is closed,
not that isolation is complete. Migrating this host-config hardening into the
nodewright `nvidia-setup` package — the canonical host-configuration channel on
AKS — and retiring the DaemonSet is tracked in
[#1839](https://github.com/NVIDIA/aicr/issues/1839).

**GPU-Operator-managed override tuple.** The documented fallback override set
(`driver.enabled=true`, `toolkit.enabled=true`, `operator.runtimeClass=nvidia`,
`dradriver:nvidiaDriverRoot=/run/nvidia/driver`) does not need an explicit
`gpuoperator:hostPaths.driverInstallDir` — the base `values.yaml` already pins
`hostPaths.driverInstallDir=/run/nvidia/driver`, so it stays in lockstep with
the DRA driver root.

**Migration note for pre-existing AKS recipes:** a recipe file references the
catalog's values files by path, so an AKS recipe generated *before* this
default flip resolves the new `driver.enabled=false` / `toolkit.enabled=false`
values on its next `aicr bundle` while retaining its old baked overrides —
in particular, the DRA driver root stays at the operator container path and
the resulting bundle is incoherent. **Regenerate pre-flip AKS recipes**
(`aicr recipe ...`) before bundling with this AICR version — for GPU pools
created with `--gpu-driver none`, regenerate with
`--profile gpuStack=operator-managed`. (A legacy pre-profile artifact may instead
supply the complete GPU Operator-managed override set:
`--set gpuoperator:driver.enabled=true --set gpuoperator:toolkit.enabled=true
--set gpuoperator:operator.runtimeClass=nvidia
--set dradriver:nvidiaDriverRoot=/run/nvidia/driver` — rejected on profiled
recipes when it diverges from the selected value; the ownership lock
accepts only identical values.) `aicr bundle` now fails closed on this combination:
the `CheckDriverOwnershipCoherence` validation detects the incoherent
driver root (`driver.enabled=false` with the DRA driver root still at the
operator container path) on the final effective values and blocks the
bundle until the recipe is regenerated (profiled recipes select the other
`gpuStack` value; legacy pre-profile artifacts may instead supply the full
override set) ([#1757](https://github.com/NVIDIA/aicr/issues/1757)).

**Driver-absent mismatch (profiled recipes):** resolving an AKS recipe from
a snapshot now takes one of two fail-closed paths, neither of which is a
bundle-time `--set` flip:

- Pools reading `None` under the azure-managed default: the constraint
  rejects the recipe at resolution, before any driver-state recording —
  but `None` **qualifies** `--profile gpuStack=operator-managed`, so simply rerun
  with that selection against the same snapshot (no pool change or
  recapture needed). `Mixed`, `Managed`, or a missing reading reject
  either selection; fix the pools (or capture with `--aks-gpu-pools`) and
  recapture.
- Pools reading `Install` while the sampled node has no loaded driver
  (failed AKS driver install, mid-reimage): the constraint passes — pool
  mode is the ownership contract, not live state — and resolution records
  `metadata.gpuDriverState: absent`; `aicr bundle` then fails closed
  (`CheckDriverOwnershipCoherence`). Repair the AKS-managed install and
  recapture, or switch the pools to `--gpu-driver none`, recapture, and
  regenerate with `--profile gpuStack=operator-managed`. The driver-ownership paths
  are profile-owned, so the pre-profile per-path `--set` tuple is rejected.

Legacy pre-profile AKS artifacts (no `metadata.selectedProfile`) keep the
old behavior: warn at resolution, record `absent`, and unblock at bundle
time with the complete legacy override set (`--set gpuoperator:driver.enabled=true
--set gpuoperator:toolkit.enabled=true --set gpuoperator:operator.runtimeClass=nvidia
--set dradriver:nvidiaDriverRoot=/run/nvidia/driver` — accepted only on
legacy artifacts; on profiled recipes these paths are owned and flags
diverging from the selected value are rejected). Criteria-only resolves
(`aicr recipe --service aks ...`) have no cluster signal and record no state —
the deployment-phase `gpu-operator-health` validation is the backstop.

`Standard_ND96isr_H100_v5` is the 8-GPU ND H100 v5 SKU. The AKS Dynamo
inference throughput gate (`inference-throughput`) is a fixed absolute
**full-node** floor calibrated on an 8-GPU H100 node, so this SKU is the
supported happy path for that gate. The same applies to the AKS H100 training
NCCL gate (`nccl-all-reduce-bw >= 150`): its floor is calibrated on full
ND96isr nodes using the Network Operator's RDMA shared device pool
(`rdma/hca_shared_devices_a`) over the SKU's multi-HCA InfiniBand fabric.
Smaller NCads H100 SKUs (`Standard_NC80adis_H100_v5` = 2 GPUs,
`Standard_NC40ads_H100_v5` = 1 GPU) run fine for deployment but will
false-fail both full-node floors — they lack the GPU count and IB fabric the
calibrations assume; gate on `inference-ttft-p99` only on those until the
per-GPU normalization in
[#1254](https://github.com/NVIDIA/aicr/issues/1254) lands.

The NCCL gate's benchmark pods pull the `nccl-tests` image from
`public.ecr.aws` (AWS's public registry) — a cross-cloud pull when running on
Azure. Private or egress-restricted AKS clusters must allow registry egress to
`public.ecr.aws` or mirror the image into an Azure-reachable registry (e.g.
ACR) before running the performance phase; otherwise the benchmark workers
fail at image pull and the check fails without measuring anything.

**GDRCopy in the azure-managed profile.** The AKS recipes set
`gdrcopy.enabled: false` because the operator deploys GDRCopy as a sidecar
container in the operator-managed driver pod; with `driver.enabled=false` that
pod is never created, so the setting is inert against the preinstalled driver.
Note that using GDRCopy requires both the `gdrdrv` kernel module on the node
and the userspace library in the workload image; whether the Azure-managed
node image ships `gdrdrv` remains unverified. To get operator-deployed
GDRCopy, switch to the GPU-Operator-managed profile below (regenerate with
`--profile gpuStack=operator-managed`) and add `--set gpuoperator:gdrcopy.enabled=true`
at bundle time — `gdrcopy.enabled` is not a profile-owned path, so the
override is accepted.

### Alternative: Let GPU Operator Manage the Driver

If you prefer the GPU Operator to install the driver (e.g., to pin the driver
version through the AICR recipe rather than follow the AKS node-image
cadence), create nodepools with `--gpu-driver none` so AKS skips its driver
installation:

```shell
az aks nodepool add \
  --cluster-name <cluster> \
  --resource-group <rg> \
  --name gpupool \
  --node-vm-size Standard_ND96isr_H100_v5 \
  --gpu-driver none \
  --node-count 1
```

Then select the mode at recipe generation time with the `gpuStack`
configuration profile — one flag flips every ownership path together:

```shell
aicr recipe --service aks --accelerator h100 --os ubuntu --intent training \
  --profile gpuStack=operator-managed -o recipe.yaml
aicr bundle -r recipe.yaml -o ./bundles
```

The `operator-managed` value sets `driver.enabled=true`, `toolkit.enabled=true`,
`operator.runtimeClass=nvidia`, and retargets `nvidia-dra-driver-gpu`'s
`nvidiaDriverRoot` from the AKS default (`/`, the host-installed driver
location) to the GPU Operator driver container's install root
(`/run/nvidia/driver`) — nothing installs a driver at the host root in this
mode. A partial configuration is impossible by construction: the profile
owns all four paths, so per-path `--set` overrides that diverge from the
selected value are rejected at bundle time (identical values are accepted;
legacy pre-profile recipes without `metadata.selectedProfile` still take
the old four-flag `--set` tuple).

When generating from a snapshot, capture the pool mode first (see
"Recording the pool mode in snapshots" above): the `operator-managed` value's
recorded constraint requires the pools to read `gpu-driver: None`, so
resolution fails closed if the pools were not actually created with
`--gpu-driver none`.

This gives the GPU Operator ownership of the full stack: it installs the
driver and configures the containerd `nvidia` runtime handler through its
container-toolkit DaemonSet. `--gpu-driver none` shifts driver lifecycle and
compatibility responsibility from Microsoft's node-image QA to the GPU
Operator (and the AICR recipe's pinned versions), and node bring-up now
includes driver installation time. See
[Skip GPU driver installation](https://learn.microsoft.com/en-us/azure/aks/use-nvidia-gpu#skip-gpu-driver-install).

`driver.rdma.useHostMofed` remains `false` in this mode too — the recipe
supplies MOFED through the Network Operator's OFED container, not a
host-preinstalled MOFED (see the comment in
`recipes/components/gpu-operator/values-aks.yaml`).

## InfiniBand RDMA Host Setup (nodewright)

AKS recipes deliver the InfiniBand RDMA host configuration (persistent
`ib_umad`/`rdma_ucm` module loading, `LimitMEMLOCK=infinity` for containerd and
kubelet) through nodewright: the `nodewright-customizations` component applies
`nvidia-setup` packages to GPU nodes, with reboots handled as nodewright
interrupts. This replaces the earlier privileged `ib-node-config` DaemonSet.

**The `nvidia-tuned` package is disabled by default on AKS.** Under the
Azure-managed driver profile the AKS overlays set
`nodewright-customizations` `tuningEnabled: false`, which omits `nvidia-tuned`
from the Skyhook packages (no tuning-triggered node reboot) and chains
`nvidia-setup-full` directly off `nvidia-setup-kernel`. The RDMA host prep
above still runs — it lives in `nvidia-setup`, not `nvidia-tuned`. An A/B
comparison on an AKS H100 cluster also measured the tuned inference profile
(isolcpus/hugepages) regressing inference TTFT from ~550 ms to ~6 s, so
untuned is the better default today. To re-enable the tuning profile:

```shell
aicr bundle -r recipe.yaml \
  --set nodewrightcustomizations:tuningEnabled=true \
  -o ./bundles
```

**Upgrading in place does not revert previously applied tuning.** The
`tuningEnabled: false` gate only controls whether `nvidia-tuned` renders into
the Skyhook CR — nodewright package uninstall defaults off and the tuning
customization declares no uninstall block, so a cluster that already applied
`nvidia-tuned` keeps its host state (isolcpus, hugepages, GRUB changes) after
upgrading to a `tuningEnabled: false` bundle. That state remains until the
nodes are reimaged or recreated; recreating the GPU node pool is the clean
path. Automated remediation is tracked in
[#1820](https://github.com/NVIDIA/aicr/issues/1820).

**Pass a keyed toleration on AKS.** AKS admission collapses a pod's toleration
list to just the wildcard (`operator: Exists`, no key) when one is present,
which defeats the nodewright operator's drain exemption for its own package
pods and deadlocks packages that declare interrupts on first install
([nodewright#296](https://github.com/NVIDIA/nodewright/issues/296)). Recovering
from that deadlock requires manually cordoning and rebooting the node. Because
`aicr bundle` injects a wildcard toleration by default when
`--accelerated-node-toleration` is not set, always bundle AKS recipes with a
keyed toleration matching your GPU node taint:

```shell
aicr bundle -r recipe.yaml \
  --accelerated-node-toleration nvidia.com/gpu:NoSchedule \
  -o ./bundles
```

Bundling an AKS recipe without a keyed toleration is a **blocking error**
(`CheckWildcardAcceleratedToleration`): the bundle is not produced until you
supply one, so the deadlock cannot ship silently.

To opt out of the RDMA stack entirely (e.g., on non-InfiniBand SKUs) — this
disables `nodewright-customizations`, so no keyed toleration is required:

```shell
aicr bundle -r recipe.yaml \
  --set networkoperator:enabled=false \
  --set gpuoperator:driver.rdma.useHostMofed=false \
  --set nodewrightcustomizations:enabled=false \
  -o ./bundles
```

## References

- [GPU Operator on AKS](https://learn.microsoft.com/en-us/azure/aks/nvidia-gpu-operator)
- [AKS GPU Node Pools](https://learn.microsoft.com/en-us/azure/aks/gpu-cluster)
- [Kubernetes DRA Documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
- [NVIDIA DRA Driver](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu)
- [AKS Supported Kubernetes Versions](https://learn.microsoft.com/en-us/azure/aks/supported-kubernetes-versions)
- [Kubernetes 1.34 DRA Updates (blog)](https://kubernetes.io/blog/2025/09/01/kubernetes-v1-34-dra-updates/)
