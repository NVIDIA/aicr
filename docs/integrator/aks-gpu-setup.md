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
GPU resources via the Kubernetes DRA API. In the supported configuration,
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

Both device-plugin and DRA are enabled by default, but **only one should be used
per node**. Using both concurrently causes GPU over-admission — both systems
advertise all GPUs independently, so the scheduler may admit more GPU pods than
physical GPUs available.

For device-plugin whole-GPU allocation (recommended — matches the NVIDIA DRA
driver's supported configuration; the DRA driver stays active for
ComputeDomain/IMEX and other non-GPU resources, only its full-GPU
advertisement is disabled), set the policy tuple in a recipe overlay:

```yaml
spec:
  componentRefs:
    - name: nvidia-dra-driver-gpu
      overrides:
        gpuResourcesEnabledOverride: false
        resources:
          gpus:
            enabled: false
    - name: gpu-operator
      overrides:
        devicePlugin:
          enabled: true
```

For DRA-only (experimental — the validators exercise ResourceClaims and
discover DRA-only nodes from the allocation probe under this policy, but full
`aicr validate` is not guaranteed until the
[#1327](https://github.com/NVIDIA/aicr/issues/1327) graduation checklist
passes; the one device-plugin-converted demo manifest,
`vllm-metrics-test.yaml`, is likewise unschedulable there), the opt-in
overlay must change all three values:

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

AKS GPU nodepools install NVIDIA drivers by default. This conflicts with the
GPU Operator, which also installs drivers by default. Use one of the approaches
below to avoid the conflict.

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

No changes to AICR recipes are needed — this is the default configuration.

`Standard_ND96isr_H100_v5` is the 8-GPU ND H100 v5 SKU. The AKS Dynamo
inference throughput gate (`inference-throughput`) is a fixed absolute
**full-node** floor calibrated on an 8-GPU H100 node, so this SKU is the
supported happy path for that gate. Smaller NCads H100 SKUs
(`Standard_NC80adis_H100_v5` = 2 GPUs, `Standard_NC40ads_H100_v5` = 1 GPU) run
fine for deployment but will false-fail the throughput floor; gate on
`inference-ttft-p99` only on those until the per-GPU normalization in
[#1254](https://github.com/NVIDIA/aicr/issues/1254) lands.

### Alternative: Use the AKS-Managed Driver

If you prefer the AKS-managed driver (e.g., for driver version pinning by AKS),
disable the GPU Operator driver:

```shell
aicr bundle -r recipe.yaml --set gpuoperator:driver.enabled=false
```

Or add to your values override file:

```yaml
driver:
  enabled: false
```

## References

- [GPU Operator on AKS](https://learn.microsoft.com/en-us/azure/aks/nvidia-gpu-operator)
- [AKS GPU Node Pools](https://learn.microsoft.com/en-us/azure/aks/gpu-cluster)
- [Kubernetes DRA Documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
- [NVIDIA DRA Driver](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu)
- [AKS Supported Kubernetes Versions](https://learn.microsoft.com/en-us/azure/aks/supported-kubernetes-versions)
- [Kubernetes 1.34 DRA Updates (blog)](https://kubernetes.io/blog/2025/09/01/kubernetes-v1-34-dra-updates/)
