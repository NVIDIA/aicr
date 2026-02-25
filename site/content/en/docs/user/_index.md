---
title: "User Guide"
linkTitle: "User Guide"
weight: 20
description: "Install, configure, and operate AICR"
---

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
| [Installation](/docs/user/installation/) | Install the `aicr` CLI (automated script, manual, or build from source) |
| [CLI Reference](/docs/user/cli-reference/) | Complete command reference with examples for all CLI operations |
| [API Reference](/docs/user/api-reference/) | REST API quick start and endpoint documentation |
| [Agent Deployment](/docs/user/agent-deployment/) | Deploy the Kubernetes agent for automated cluster snapshots |

## Quick Start

```shell
# Install aicr CLI
curl -sfL https://raw.githubusercontent.com/NVIDIA/aicr/main/install | bash -s --

# Generate a recipe for your environment
aicr recipe --service eks --accelerator h100 --intent training -o recipe.yaml

# Create deployment bundles
aicr bundle --recipe recipe.yaml -o ./bundles

# Deploy to your cluster
cd bundles && chmod +x deploy.sh && ./deploy.sh
```

## Related Documentation

- **Integrators**: See [Integrator Documentation](/docs/integrator/) for CI/CD integration and API server deployment
- **Contributors**: See [Contributor Documentation](/docs/contributor/) for architecture and development guides
