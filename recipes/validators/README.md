# Validator Catalog

The validator catalog (`catalog.yaml`) defines which validation checks are available, what phase they belong to, and how they run as Kubernetes Jobs.

## Catalog Structure

```yaml
apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: aicr-validators
  version: "1.0.0"
validators:
  - name: operator-health           # Unique identifier, used in Job names
    phase: deployment               # deployment | performance | conformance
    description: "Human-readable"   # Shown in CTRF report
    image: ghcr.io/.../img:latest   # OCI image reference
    timeout: 2m                     # Job activeDeadlineSeconds
    args: ["operator-health"]       # Container arguments
    env: []                         # Optional environment variables
    resources:                      # Optional (omit for defaults)
      cpu: "100m"
      memory: "128Mi"
```

## Image Tag Resolution

Applied by `catalog.Load` (`pkg/validator/catalog/catalog.go`) in order:

1. `:latest` is replaced with the CLI version (e.g., `:v0.9.5`) for release builds — locks validators to the CLI version. Dev builds keep `:latest`.
2. Explicit version tags (e.g., `:v1.2.3`) are never modified — use these to pin a validator independently.
3. `AICR_VALIDATOR_IMAGE_REGISTRY` overrides the registry prefix (e.g., `localhost:5001` replaces `ghcr.io/nvidia`).

## Validators

### Deployment Phase

| Name | Description | Timeout |
|------|-------------|---------|
| `operator-health` | Verify GPU operator pods are running and healthy | 2m |
| `expected-resources` | Verify expected Kubernetes resources exist and are healthy | 5m |
| `gpu-operator-version` | Validate GPU Operator version against recipe constraints | 2m |
| `check-nvidia-smi` | Verify nvidia-smi works on all GPU nodes | 10m |

### Performance Phase

| Name | Description | Timeout |
|------|-------------|---------|
| `nccl-all-reduce-bw` | Verify NCCL All Reduce Bus Bandwidth meets threshold | 30m |
| `nccl-all-reduce-bw-net` | Verify NCCL All Reduce Bus Bandwidth on the NET transport (EFA on EKS) | 30m |
| `nccl-all-reduce-bw-nvls` | Verify NCCL All Reduce Bus Bandwidth on the NVLS transport (MNNVL across an NVL72 IMEX domain) | 30m |

### Conformance Phase

| Name | Description | Timeout |
|------|-------------|---------|
| `dra-support` | Verify Dynamic Resource Allocation support | 5m |
| `gang-scheduling` | Verify gang scheduling with KAI scheduler using CPU-only workers | 10m |
| `accelerator-metrics` | Verify accelerator metrics from DCGM exporter | 5m |
| `ai-service-metrics` | Verify AI service metrics via Prometheus | 5m |
| `inference-gateway` | Verify inference gateway (agentgateway) is operational | 5m |
| `pod-autoscaling` | Verify HPA-driven pod autoscaling with GPU metrics | 10m |
| `cluster-autoscaling` | Verify cluster autoscaling with Karpenter | 10m |
| `robust-controller` | Verify Dynamo operator controller and webhooks | 5m |
| `secure-accelerator-access` | Verify secure GPU access via DRA (no host device mounts) | 10m |
| `gpu-operator-health` | Verify GPU operator health (conformance diagnostic) | 2m |
| `platform-health` | Verify platform component health (conformance diagnostic) | 5m |

## Extending the Catalog

Use the `--data` flag to add custom validators or override embedded ones. See the [Validator Extension Guide](../../docs/integrator/validator-extension.md).
