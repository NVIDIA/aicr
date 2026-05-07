# User Documentation

Documentation for platform operators deploying and operating GPU-accelerated Kubernetes clusters using AI Cluster Runtime (AICR).

## Audience

This section is for users who:
- Install and use the `aicr` CLI tool
- Deploy the AICR agent to capture cluster snapshots
- Generate recipes and bundles for their environments
- Use the API for programmatic configuration generation

## Documents

| Document | Description |
|----------|-------------|
| [Installation](installation.md) | Install the `aicr` CLI (automated script, manual, or build from source) |
| [CLI Reference](cli-reference.md) | Complete command reference with examples for all CLI operations |
| [API Reference](api-reference.md) | REST API quick start and endpoint documentation |
| [Agent Deployment](agent-deployment.md) | Deploy the Kubernetes agent for automated cluster snapshots |
| [Validation](validation.md) | Task-oriented walkthrough: validate a cluster against a recipe, both training and inference performance phases |
| [Component Catalog](component-catalog.md) | Every component that can appear in a recipe |
| [Container Images](container-images.md) | Container image inventory across all components (BOM) |

## Quick Start

```shell
# Install aicr CLI (Homebrew)
brew tap NVIDIA/aicr
brew install aicr

# Or use the install script
curl -sfL https://raw.githubusercontent.com/NVIDIA/aicr/main/install | bash -s --

# Generate a recipe for your environment
aicr recipe --service eks --accelerator h100 --intent training -o recipe.yaml

# Create deployment bundles
aicr bundle --recipe recipe.yaml -o ./bundles

# Deploy to your cluster
cd bundles && chmod +x deploy.sh && ./deploy.sh
```

**Validating a cluster instead of deploying?** See [Validation](validation.md)
for a task-oriented walkthrough covering deployment, performance (training and
inference), and conformance phases.

## Related Documentation

- **Integrators**: See [Integrator Documentation](../integrator/) for CI/CD integration and API server deployment
- **Contributors**: See [Contributor Documentation](../contributor/) for architecture and development guides
