# Eidos

[![On Push CI](https://github.com/NVIDIA/eidos/actions/workflows/on-push.yaml/badge.svg)](https://github.com/NVIDIA/eidos/actions/workflows/on-push.yaml)
[![On Tag Release](https://github.com/NVIDIA/eidos/actions/workflows/on-tag.yaml/badge.svg)](https://github.com/NVIDIA/eidos/actions/workflows/on-tag.yaml)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

Eidos (Eidos) provides validated configuration guidance for deploying GPU-accelerated Kubernetes infrastructure. It captures known-good combinations of software, configuration, and system requirements and makes them consumable as documentation and generated deployment artifacts.

## Why We Built This

Running GPU-accelerated Kubernetes clusters reliably is hard. Small differences in kernel versions, drivers, container runtimes, operators, and Kubernetes releases can cause failures that are difficult to diagnose and expensive to reproduce.

Historically, this knowledge has lived in internal validation pipelines, playbooks, and tribal knowledge. Eidos exists to externalize that experience. Its goal is to make validated configurations visible, repeatable, and reusable across environments.

## What Eidos Is (and Is Not)

Eidos is a **source of validated configuration knowledge** for NVIDIA-accelerated Kubernetes environments.

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

Eidos separates **validated configuration knowledge** from **how that knowledge is consumed**.

- Human-readable documentation lives under `docs/`.
- Version-locked configuration definitions (“recipes”) capture known-good system states.
- Those definitions can be rendered into concrete artifacts such as Helm values, Kubernetes manifests, or install scripts.- Recipes can be validated against actual system configurations to verify compatibility.

This separation allows the same validated configuration to be applied consistently across different environments and automation systems.

*For example, a configuration validated for gb200 on Ubuntu 22.04 with Kubernetes 1.29 can be rendered into Helm values and manifests suitable for use in an existing GitOps pipeline.*

## Get Started

> Some tooling and APIs are under active development; documentation reflects current and near-term capabilities.

### Installation

```shell
# Public repository
curl -sfL https://raw.githubusercontent.com/NVIDIA/eidos/main/install | bash -s --

# Private repository (requires GitHub token with repo scope)
curl -sfL -H "Authorization: token $GITHUB_TOKEN" \
  https://raw.githubusercontent.com/NVIDIA/eidos/main/install | bash -s --
```

See [Installation Guide](docs/user-guide/installation.md) for manual installation, building from source, and container images.

### Quick Start

Get started quickly with Eidos:
1. Review the documentation under `docs/` to understand supported platforms and required components.
2. Identify your target environment:
   - GPU architecture
   - Operating system and kernel
   - Kubernetes distribution and version
   - Workload intent (for example, training or inference)
3. Apply the validated configuration guidance using your existing tools (Helm, kubectl, CI/CD, or GitOps).
4. Validate and iterate as platforms and workloads evolve.

### Get Started by Use Case

These use cases reflect common ways teams interact with Eidos.

<details>
<summary><strong>Platform and Infrastructure Operators</strong></summary>

You are responsible for deploying and operating GPU-accelerated Kubernetes clusters. 
- **[Installation Guide](docs/user-guide/installation.md)** – Install the eidos CLI (automated script, manual, or build from source)
- **[CLI Reference](docs/user-guide/cli-reference.md)** – Complete command reference with examples
- **[API Reference](docs/user-guide/api-reference.md)** – Complete API reference with examples
- **[Agent Deployment](docs/user-guide/agent-deployment.md)** – Deploy the Kubernetes agent to get automated configuration snapshots
</details>

<details>
<summary><strong>Developers and Contributors</strong></summary>

You are contributing code, extending functionality, or working on Eidos internals. 

- **[Contributing Guide](CONTRIBUTING.md)** – Development setup, testing, and PR process
- **[Architecture Overview](docs/architecture/README.md)** – System design and components
- **[Bundler Development](docs/architecture/component.md)** – How to create new bundlers
- **[Data Architecture](docs/architecture/data.md)** – Recipe data model and query matching
</details>

<details>
<summary><strong>Integrators and Automation Engineers</strong></summary>

You are integrating Eidos into CI/CD pipelines, GitOps workflows, or a larger product or service. 

- **[API Reference](docs/integration/api-reference.md)** – REST API endpoints and usage examples
- **[Data Flow](docs/integration/data-flow.md)** – Understanding snapshots, recipes, and bundles
- **[Automation Guide](docs/integration/automation.md)** – CI/CD integration patterns
- **[Kubernetes Deployment](docs/integration/kubernetes-deployment.md)** – Self-hosted API server setup
</details>

## Project Structure

- `api/` — OpenAPI specifications for the REST API
- `cmd/` — Entry points for CLI (`eidos`) and API server (`eidosd`)
- `deployments/` — Kubernetes manifests for agent deployment
- `docs/` — User-facing documentation, guides, and architecture docs
- `examples/` — Example snapshots, recipes, and comparisons
- `infra/` — Infrastructure as code (Terraform) for deployments
- `pkg/` — Core Go packages (collectors, recipe engine, bundlers, serializers)
- `tools/` — Build scripts, E2E testing, and utilities

## Documentation & Resources

- **[Documentation](/docs)** – Documentation, guides, and examples.
- **[Roadmap](ROADMAP.md)** – Feature priorities and development timeline
- **[Overview](docs/OVERVIEW.md)** - Detailed system overview and glossary
- **[Security](SECURITY.md)** - Security-related resources 
- **[Releases](https://github.com/NVIDIA/eidos/releases)** - Binaries, SBOMs, and other artifacts
- **[Issues](https://github.com/NVIDIA/eidos/issues)** - Bugs, feature requests, and questions

## Contributing

Contributions are welcome. See [contributing](/CONTRIBUTING.md) for development setup, contribution guidelines, and the pull request process.