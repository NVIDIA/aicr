# AI Cluster Runtime (AICR)

[![On Push CI](https://github.com/NVIDIA/aicr/actions/workflows/on-push.yaml/badge.svg)](https://github.com/NVIDIA/aicr/actions/workflows/on-push.yaml)
[![On Tag Release](https://github.com/NVIDIA/aicr/actions/workflows/on-tag.yaml/badge.svg)](https://github.com/NVIDIA/aicr/actions/workflows/on-tag.yaml)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

AICR provides tooling for deploying optimized and validated GPU-accelerated AI runtime in Kubernetes. It captures known-good combinations of drivers, operators, kernels, and system configurations to create a reproducible artifacts for common Kubernetes deployment frameworks like Helm and ArgoCD.

## Why We Built This

Running GPU-accelerated Kubernetes clusters reliably is hard. Small differences in kernel versions, drivers, container runtimes, operators, and Kubernetes releases can cause failures that are difficult to diagnose and expensive to reproduce.

Historically, this knowledge has lived in internal validation pipelines, playbooks, and tribal knowledge. AICR exists to externalize that experience. Its goal is to make validated configurations visible, repeatable, and reusable across environments.

## What AICR Is (and Is Not)

AICR is a **source of validated configuration knowledge** for NVIDIA-accelerated Kubernetes environments.

It **is**:
- A curated set of tested and validated component combinations
- A reference for how NVIDIA-accelerated Kubernetes clusters are expected to be configured
- A foundation for generating reproducible deployment artifacts
- Designed to integrate with existing provisioning, CI/CD, and GitOps workflows

It **is not**:
- A Kubernetes distribution
- A cluster provisioning or lifecycle management system
- A managed control plane or hosted service
- A replacement for cloud provider or OEM platforms

## How It Works

AICR separates **validated configuration knowledge** from **how that knowledge is consumed**.

- Human-readable documentation lives under `docs/`.
- Version-locked configuration definitions (“recipes”) capture known-good system states.
- Those definitions can be rendered into concrete artifacts such as Helm values, Kubernetes manifests, or install scripts.- Recipes can be validated against actual system configurations to verify compatibility.

This separation allows the same validated configuration to be applied consistently across different environments and automation systems.

*For example, a configuration validated for GB200 on Ubuntu 22.04 with Kubernetes 1.34 can be rendered into Helm values and manifests suitable for use in an existing GitOps pipeline.*

## Get Started

> Some tooling and APIs are under active development; documentation reflects current and near-term capabilities.

### Installation

Install the latest version using the installation script:

```shell
curl -sfL https://raw.githubusercontent.com/NVIDIA/aicr/main/install | bash -s --
```

See [Installation Guide](docs/user/installation.md) for manual installation, building from source, and container images.

### Quick Start

Get started quickly with AICR:
1. Review the documentation under `docs/` to understand supported platforms and required components.
2. Identify your target environment:
   - GPU architecture
   - Operating system and kernel
   - Kubernetes distribution and version
   - Workload intent (for example, training or inference)
3. Apply the validated configuration guidance using your existing tools (Helm, kubectl, CI/CD, or GitOps).
4. Validate and iterate as platforms and workloads evolve.

**Example:** Generate a validated configuration for GB200 on EKS with Ubuntu, optimized for Kubeflow training:

```bash
# Generate a recipe for your environment
aicr recipe --service eks --accelerator gb200 --os ubuntu --intent training --platform kubeflow -o recipe.yaml

# Render the recipe into Helm values for your GitOps pipeline
aicr bundle --recipe recipe.yaml -o ./bundles
```

The generated `bundles/` directory contains a Helm per-component bundle ready to deploy or commit to your GitOps repository. See [CLI Reference](docs/user/cli-reference.md) for more options.

### Get Started by Use Case

Choose the documentation path that matches how you'll use AICR.

<details>
<summary><strong>User</strong> – Platform and Infrastructure Operators</summary>

You deploy and operate GPU-accelerated Kubernetes clusters using validated configurations.

- **[Installation Guide](docs/user/installation.md)** – Install the aicr CLI (automated script, manual, or build from source)
- **[CLI Reference](docs/user/cli-reference.md)** – Complete command reference with examples
- **[API Reference](docs/user/api-reference.md)** – REST API quick start
- **[Agent Deployment](docs/user/agent-deployment.md)** – Deploy the Kubernetes agent for automated snapshots
</details>

<details>
<summary><strong>Contributor</strong> – Developers and Maintainers</summary>

You contribute code, extend functionality, or work on AICR internals.

- **[Agent Instructions](AGENTS.md)** – Core coding-agent guidance for Codex/Copilot (mirrors `.claude/CLAUDE.md`)
- **[Contributing Guide](CONTRIBUTING.md)** – Development setup, testing, and PR process
- **[Development Guide](DEVELOPMENT.md)** – Local development, Make targets, and tooling
- **[Architecture Overview](docs/contributor/README.md)** – System design and components
- **[Bundler Development](docs/contributor/component.md)** – How to create new bundlers
- **[Data Architecture](docs/contributor/data.md)** – Recipe data model and query matching
</details>

<details>
<summary><strong>Integrator</strong> – Automation and Platform Engineers</summary>

You integrate AICR into CI/CD pipelines, GitOps workflows, or larger platforms.

- **[API Reference](docs/user/api-reference.md)** – REST API endpoints and usage examples
- **[Data Flow](docs/integrator/data-flow.md)** – Understanding snapshots, recipes, and bundles
- **[Automation Guide](docs/integrator/automation.md)** – CI/CD integration patterns
- **[Kubernetes Deployment](docs/integrator/kubernetes-deployment.md)** – Self-hosted API server setup
- **[Recipe Development](docs/integrator/recipe-development.md)** – Adding and modifying recipe metadata
</details>

## Documentation & Resources

- **[Documentation](docs/README.md)** – Documentation, guides, and examples.
- **[Roadmap](ROADMAP.md)** – Feature priorities and development timeline
- **[Overview](docs/README.md)** - Detailed system overview and glossary
- **[Security](SECURITY.md)** - Security-related resources 
- **[Releases](https://github.com/NVIDIA/aicr/releases)** - Binaries, SBOMs, and other artifacts
- **[Issues](https://github.com/NVIDIA/aicr/issues)** - Bugs, feature requests, and questions

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, contribution guidelines, and the pull request process.
