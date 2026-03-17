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

All AICR recipes include the `nvidia-dra-driver-gpu` component, which advertises
GPUs via the Kubernetes DRA API instead of the legacy device plugin. DRA provides
structured GPU device advertisement, claim-based allocation, and integration with
gang scheduling.

### Feature Gate Details

| Kubernetes Version | DRA Status | Feature Gate |
|--------------------|-----------|--------------|
| 1.26–1.29 | Alpha | `DynamicResourceAllocation` — off by default |
| 1.30–1.33 | Beta | `DynamicResourceAllocation` — on by default |
| 1.34+ | **GA / Stable** | `resource.k8s.io/v1` — always enabled, no feature gate needed |

On AKS 1.34, DRA is GA. You do not need to pass any custom API server flags or
register an AKS preview feature.

### CLI Override

You can control DRA settings when bundling:

```shell
# Enable GPU resource advertisement (default)
aicr bundle -r recipe.yaml --set dradriver:gpuResourcesEnabledOverride=true

# Disable DRA GPU allocation (fall back to device plugin)
aicr bundle -r recipe.yaml \
  --set dradriver:gpuResourcesEnabledOverride=false \
  --set dradriver:resources.gpus.enabled=false
```

### Device Plugin vs DRA (Important)

Both device-plugin and DRA are enabled by default, but **only one should be used
per node**. Using both concurrently causes GPU over-admission — both systems
advertise all GPUs independently, so the scheduler may admit more GPU pods than
physical GPUs available.

For DRA-only (recommended):

```shell
aicr bundle -r recipe.yaml --set gpuoperator:devicePlugin.enabled=false
```

For device-plugin-only (legacy):

```shell
aicr bundle -r recipe.yaml \
  --set dradriver:gpuResourcesEnabledOverride=false \
  --set dradriver:resources.gpus.enabled=false
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
  --node-vm-size Standard_NC80adis_H100_v5 \
  --gpu-driver none \
  --node-count 1
```

No changes to AICR recipes are needed — this is the default configuration.

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
- [NVIDIA DRA Driver](https://github.com/NVIDIA/k8s-dra-driver-gpu)
- [AKS Supported Kubernetes Versions](https://learn.microsoft.com/en-us/azure/aks/supported-kubernetes-versions)
- [Kubernetes 1.34 DRA Updates (blog)](https://kubernetes.io/blog/2025/09/01/kubernetes-v1-34-dra-updates/)
