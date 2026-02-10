# User Documentation

Documentation for platform operators deploying and operating GPU-accelerated Kubernetes clusters using Eidos.

## Audience

This section is for users who:
- Install and use the `eidos` CLI tool
- Deploy the Eidos agent to capture cluster snapshots
- Generate recipes and bundles for their environments
- Use the API for programmatic configuration generation

## Documents

| Document | Description |
|----------|-------------|
| [Installation](installation.md) | Install the `eidos` CLI (automated script, manual, or build from source) |
| [CLI Reference](cli-reference.md) | Complete command reference with examples for all CLI operations |
| [API Reference](api-reference.md) | REST API quick start and endpoint documentation |
| [Agent Deployment](agent-deployment.md) | Deploy the Kubernetes agent for automated cluster snapshots |

## Quick Start

```shell
# Install eidos CLI
curl -sfL https://raw.githubusercontent.com/NVIDIA/eidos/main/install | bash -s --

# Generate a recipe for your environment
eidos recipe --service eks --accelerator h100 --intent training -o recipe.yaml

# Create deployment bundles
eidos bundle --recipe recipe.yaml -o ./bundles

# Deploy to your cluster
cd bundles && chmod +x deploy.sh && ./deploy.sh
```

## Related Documentation

- **Integrators**: See [Integrator Documentation](../integrator/README.md) for CI/CD integration and API server deployment
- **Contributors**: See [Contributor Documentation](../contributor/README.md) for architecture and development guides
