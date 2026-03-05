---
title: "Integrator Guide"
linkTitle: "Integrator Guide"
weight: 30
description: "Embed AICR in automation pipelines and CI/CD workflows"
---

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
| [Automation](/docs/integrator/automation/) | CI/CD integration patterns for GitHub Actions, GitLab CI, Jenkins, and Terraform |
| [Data Flow](/docs/integrator/data-flow/) | Understanding snapshots, recipes, validation, and bundles data transformations |
| [Kubernetes Deployment](/docs/integrator/kubernetes-deployment/) | Self-hosted API server deployment with Kubernetes manifests |
| [EKS Dynamo Networking](/docs/integrator/eks-dynamo-networking/) | Security group prerequisites for Dynamo overlays on EKS |
| [Recipe Development](/docs/integrator/recipe-development/) | Creating and modifying recipe metadata for custom environments |

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

- **Users**: See [User Documentation](/docs/user/) for CLI usage and installation
- **Contributors**: See [Contributor Documentation](/docs/contributor/) for architecture and development guides
