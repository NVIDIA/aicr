# Integrator Documentation

Documentation for engineers integrating Eidos into CI/CD pipelines, GitOps workflows, or larger platforms.

## Audience

This section is for integrators who:
- Build automation pipelines using the Eidos API
- Deploy and operate the Eidos API server in Kubernetes
- Create custom recipes for their environments
- Integrate Eidos into GitOps workflows (ArgoCD, Flux)

## Documents

| Document | Description |
|----------|-------------|
| [Automation](automation.md) | CI/CD integration patterns for GitHub Actions, GitLab CI, Jenkins, and Terraform |
| [Data Flow](data-flow.md) | Understanding snapshots, recipes, validation, and bundles data transformations |
| [Kubernetes Deployment](kubernetes-deployment.md) | Self-hosted API server deployment with Kubernetes manifests |
| [Recipe Development](recipe-development.md) | Creating and modifying recipe metadata for custom environments |

## Quick Start

### API Server Deployment

```shell
# Deploy API server to Kubernetes
kubectl apply -k https://github.com/NVIDIA/eidos/deployments/eidosd

# Generate recipe via API
curl "http://eidosd.eidos.svc/v1/recipe?service=eks&accelerator=h100"
```

### CI/CD Integration

```yaml
# GitHub Actions example
- name: Generate recipe
  run: |
    curl -s "http://eidosd.eidos.svc/v1/recipe?service=eks&accelerator=h100" \
      -o recipe.json

- name: Generate bundles
  run: |
    curl -X POST "http://eidosd.eidos.svc/v1/bundle?bundlers=gpu-operator" \
      -H "Content-Type: application/json" \
      -d @recipe.json \
      -o bundles.zip
```

## Related Documentation

- **Users**: See [User Documentation](../user/README.md) for CLI usage and installation
- **Contributors**: See [Contributor Documentation](../contributor/README.md) for architecture and development guides
