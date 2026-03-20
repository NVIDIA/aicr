# Integrator Documentation


Documentation for engineers integrating AI Cluster Runtime (AICR) into CI/CD pipelines, GitOps workflows, or larger platforms.

## Audience

This section is for integrators who:
- Build automation pipelines using the AICR API
- Deploy and operate the AICR API server in Kubernetes
- Create custom recipes for their environments
- Integrate AICR into GitOps workflows (ArgoCD, Flux)

## Documents

| Document | Description |
|----------|-------------|
| [Automation](automation.md) | CI/CD integration patterns for GitHub Actions, GitLab CI, Jenkins, and Terraform |
| [Data Flow](data-flow.md) | Understanding snapshots, recipes, validation, and bundles data transformations |
| [Kubernetes Deployment](kubernetes-deployment.md) | Self-hosted API server deployment with Kubernetes manifests |
| [EKS Dynamo Networking](eks-dynamo-networking.md) | Security group prerequisites for Dynamo overlays on EKS |
| [AKS GPU Setup](aks-gpu-setup.md) | AKS prerequisites: Kubernetes 1.34+ (DRA GA), GPU driver setup, DRA configuration |
| [Recipe Development](recipe-development.md) | Creating and modifying recipe metadata for custom environments |
| [Validator Extension](validator-extension.md) | Adding custom validators and overriding embedded ones via `--data` |

## Quick Start

### API Server Deployment

```shell
# Deploy API server to Kubernetes
kubectl apply -k https://github.com/NVIDIA/aicr/deploy/aicrd

# Generate recipe via API
curl "http://aicrd.aicr.svc/v1/recipe?service=eks&accelerator=h100"
```

### CI/CD Integration

```yaml
# GitHub Actions example
- name: Generate recipe
  run: |
    curl -s "http://aicrd.aicr.svc/v1/recipe?service=eks&accelerator=h100" \
      -o recipe.json

- name: Generate bundles
  run: |
    curl -X POST "http://aicrd.aicr.svc/v1/bundle?bundlers=gpu-operator" \
      -H "Content-Type: application/json" \
      -d @recipe.json \
      -o bundles.zip
```

## Related Documentation

- **Users**: See [User Documentation](../user/) for CLI usage and installation
- **Contributors**: See [Contributor Documentation](../contributor/) for architecture and development guides
