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
| GPU Operator-managed (default, recommended) | `--gpu-driver none` (AKS "None/BYO" install profile) | `driver.enabled=true`, `toolkit.enabled=true` (recipe defaults) |
| AKS driver-only | AKS "Driver only" install profile (`--enable-managed-gpu=false`, the AKS default) | `driver.enabled=false`, `toolkit.enabled=false`, `operator.runtimeClass=nvidia-container-runtime` (all three together) |

Both modes use an *unmanaged* GPU node pool. Do not combine AICR with
AKS-managed GPU node pools (`--enable-managed-gpu=true`, preview —
`gpuProfile.nvidia.managementMode: Managed`): that profile makes AKS install
its own device plugin, DCGM exporter, and GPU health tooling, which duplicate
and conflict with the GPU Operator operands AICR deploys. See
[AKS install profiles](https://learn.microsoft.com/en-us/azure/aks/aks-managed-gpu-nodes#install-profiles).

### Recommended: Let GPU Operator Manage the Driver

Create nodepools with `--gpu-driver none` so AKS skips its driver installation
and the GPU Operator handles it:

```shell
az aks nodepool add \
  --cluster-name <cluster> \
  --resource-group <rg> \
  --name gpupool \
  --node-vm-size Standard_ND96isr_H100_v5 \
  --gpu-driver none \
  --node-count 1
```

No changes to AICR recipes are needed — this is the default configuration. The
recipe defaults (`driver.enabled=true`, `toolkit.enabled=true`) give the GPU
Operator ownership of the full stack: it installs the driver and configures the
containerd `nvidia` runtime handler through its container-toolkit DaemonSet.

Note that `--gpu-driver none` shifts driver lifecycle and compatibility
responsibility from Microsoft's node-image QA to the GPU Operator (and the AICR
recipe's pinned versions), and node bring-up now includes driver installation
time. See
[Skip GPU driver installation](https://learn.microsoft.com/en-us/azure/aks/use-nvidia-gpu#skip-gpu-driver-install).

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

### Alternative: Use the AKS Driver-Only Profile

If you prefer AKS to install the driver (e.g., for driver version pinning by
AKS), create the nodepool with the AKS **Driver only** install profile
(`--enable-managed-gpu=false`, the AKS default — simply omit
`--gpu-driver none`). Only driver and container-runtime ownership transfers to
AKS; AICR's GPU Operator still deploys and owns the device plugin, DCGM
exporter, and the rest of the GPU stack. This requires **all three** overrides
together — never set only one side, because a partial configuration either
leaves containerd without a working `nvidia` runtime or conflicts with the
preinstalled driver:

```shell
aicr bundle -r recipe.yaml \
  --set gpuoperator:driver.enabled=false \
  --set gpuoperator:toolkit.enabled=false \
  --set gpuoperator:operator.runtimeClass=nvidia-container-runtime
```

Or add to your values override file:

```yaml
driver:
  enabled: false
toolkit:
  enabled: false
operator:
  runtimeClass: nvidia-container-runtime
```

`operator.runtimeClass` must match the runtime handler preconfigured on the AKS
node image; NVIDIA's AKS example uses `nvidia-container-runtime` (see
[GPU Operator on Microsoft AKS](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/microsoft-aks.html)).

`driver.rdma.useHostMofed` remains `false` in this mode too — it is inert while
`driver.enabled=false`, but keeping it correct prevents a later ownership-mode
change from reviving the `nvidia_peermem` symbol-mismatch bug (see the comment
in `recipes/components/gpu-operator/values-aks.yaml`).

This profile has not yet been validated end-to-end with network-operator/RDMA.

## References

- [GPU Operator on AKS](https://learn.microsoft.com/en-us/azure/aks/nvidia-gpu-operator)
- [AKS GPU Node Pools](https://learn.microsoft.com/en-us/azure/aks/gpu-cluster)
- [Kubernetes DRA Documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
- [NVIDIA DRA Driver](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu)
- [AKS Supported Kubernetes Versions](https://learn.microsoft.com/en-us/azure/aks/supported-kubernetes-versions)
- [Kubernetes 1.34 DRA Updates (blog)](https://kubernetes.io/blog/2025/09/01/kubernetes-v1-34-dra-updates/)
