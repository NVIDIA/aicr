# Cloud Native Stack Deployment

Generated: 2026-01-23T04:15:56-08:00
Recipe Version: v0.22.0-next
Bundler Version: v0.22.0-next

This is a Helm umbrella chart that deploys NVIDIA Cloud Native Stack components
for GPU-accelerated Kubernetes workloads.

## Configuration


**Target Environment:**

- **Service**: eks

- **Accelerator**: gb200

- **Intent**: training

- **OS**: ubuntu



## Components

The following components are included (deployed in order):

| Component | Version | Repository |
|-----------|---------|------------|
| cert-manager | v1.17.2 | https://charts.jetstack.io |
| gpu-operator | v25.3.3 | https://helm.ngc.nvidia.com/nvidia |
| nvidia-dra-driver-gpu | 25.8.1 | https://helm.ngc.nvidia.com/nvidia |
| nvsentinel | v0.6.0 | oci://ghcr.io/nvidia |
| skyhook-operator | 0.11.1 | https://helm.ngc.nvidia.com/nvidia/skyhook |



## Constraints

The following constraints must be satisfied:

| Constraint | Value |
|------------|-------|
| K8s.server.version | >= 1.32.4 |
| OS.release.ID | ubuntu |
| OS.release.VERSION_ID | 24.04 |
| OS.sysctl./proc/sys/kernel/osrelease | >= 6.8 |



## Quick Start

1. **Add Helm repositories** (if not already added):

```bash
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
helm repo update
```

2. **Update dependencies**:

```bash
helm dependency update
```

3. **Review and customize values** (optional):

```bash
# Edit values.yaml to customize component configuration
vim values.yaml
```

4. **Install the chart**:

```bash
helm install eidos-stack . --wait --timeout 10m -f values.yaml
```

> **Note**: The `--wait --timeout 10m` flags ensure Helm waits for all resources to be ready
> and that Helm hooks (used for CRD-dependent resources) execute in the correct order.

## Customization

### Disabling Components

To skip installing a specific component, set `<component>.enabled=false`:

```bash
helm install eidos-stack . --wait --timeout 10m \
  --set cert-manager.enabled=false
```

### Overriding Values

Override specific values using `--set`:

```bash
helm install eidos-stack . --wait --timeout 10m \
  --set gpu-operator.driver.enabled=false \
  --set gpu-operator.toolkit.enabled=true
```

### Using a Custom Values File

Create a custom values file and merge it:

```bash
helm install eidos-stack . --wait --timeout 10m -f values.yaml -f custom-values.yaml
```

## Upgrade

To upgrade an existing installation:

```bash
helm upgrade eidos-stack . --wait --timeout 10m -f values.yaml
```

## Uninstall

To remove the deployment:

```bash
helm uninstall eidos-stack
```

## Troubleshooting

### Check deployment status

```bash
helm status eidos-stack
kubectl get pods -n nvidia-operators
```

### View component logs

```bash
kubectl logs -n nvidia-operators -l app=gpu-operator
kubectl logs -n nvidia-operators -l app=network-operator
```

### Verify GPU access

```bash
kubectl get nodes -o jsonpath='{.items[*].status.allocatable}' | jq '.["nvidia.com/gpu"]'
```

## References

- [GPU Operator Documentation](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/)
- [Network Operator Documentation](https://docs.nvidia.com/networking/display/cokan10/network+operator)
- [Helm Dependencies](https://helm.sh/docs/helm/helm_dependency/)
