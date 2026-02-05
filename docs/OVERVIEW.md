# Eidos: An Overview

NVIDIA Eidos (Eidos) is a suite of tooling designed to automate the complexity of deploying GPU-accelerated Kubernetes infrastructure. By moving away from static documentation and toward automated configuration generation, Eidos ensures that AI/ML workloads run on infrastructure that is validated, optimized, and secure.

## Glossary

| Term | Description |
|------|-------------|
| **Snapshot** | A captured state of a system including OS, kernel, Kubernetes, GPU, and SystemD configuration. Created by `eidos snapshot` or the Kubernetes agent. |
| **Recipe** | A generated configuration recommendation containing component references, constraints, and deployment order. Created by `eidos recipe` based on criteria or snapshot analysis. |
| **Criteria** | Query parameters that define the target environment: `service` (eks/gke/aks/oke), `accelerator` (h100/gb200/a100/l40), `intent` (training/inference), `os` (ubuntu/rhel/cos), `platform` (kubeflow), and `nodes`. |
| **Overlay** | A recipe metadata file that extends the base recipe for specific environments. Overlays are matched against criteria using asymmetric matching. |
| **Bundle** | Deployment artifacts generated from a recipe: Helm values files, Kubernetes manifests, installation scripts, and checksums. |
| **Bundler** | A plugin that generates bundle artifacts for a specific component (e.g., GPU Operator bundler, Network Operator bundler). |
| **Deployer** | A plugin that transforms bundle artifacts into deployment-specific formats: `helm` (Helm umbrella charts, default), `argocd` (Applications with sync-waves). |
| **Component** | A deployable software package (e.g., GPU Operator, Network Operator, cert-manager). Components have versions, Helm sources, and configuration values. |
| **ComponentRef** | A reference to a component in a recipe, including version, source repository, values file, and dependency references. |
| **Constraint** | A validation rule in a recipe specifying required system conditions (e.g., `K8s.server.version >= 1.31`, `OS.release.ID == ubuntu`). Constraints can have severity (error/warning), remediation guidance, and units. |
| **Validation Phase** | A stage of validation in the deployment lifecycle: readiness (infrastructure), deployment (components), performance (system), conformance (workloads). |
| **ValidationConfig** | Configuration in a recipe defining phase-specific checks, constraints, expected resources, and node selection for validation. |
| **Measurement** | A captured data point from the system organized by type (K8s, OS, GPU, SystemD), subtype, and key-value readings. |
| **Specificity** | A score indicating how specific a recipe's criteria is (number of non-"any" fields). More specific recipes are applied later during merge. |
| **Asymmetric Matching** | The criteria matching algorithm where recipe "any" = wildcard (matches any query), but query "any" ≠ specific recipe (prevents overly-specific matches). |
| **ConfigMap URI** | A URI format (`cm://namespace/name`) for reading/writing snapshots and recipes directly to Kubernetes ConfigMaps. |
| **SLSA** | Supply-chain Levels for Software Artifacts. Eidos releases achieve SLSA Build Level 3 with provenance attestations. |
| **SBOM** | Software Bill of Materials. A complete inventory of dependencies provided for binaries (SPDX via GoReleaser) and containers (SPDX JSON via Syft). |

## Why Eidos?

Deploying high-performance AI infrastructure is historically complex. Administrators must navigate a "matrix" of dependencies, ensuring compatibility between the Operating System, Kubernetes version, GPU drivers, and container runtimes.

### The Challenge: The "Old Way"
Previously, administrators relied on static documentation and manual installation guides. This approach presented several significant challenges:
*   **Complexity:** Administrators had to manually track compatibility matrices across dozens of components (e.g., matching a specific GPU Operator version to a specific driver and K8s version).
*   **Human Error:** Manual copy-pasting of commands and flags often led to configuration drift or broken deployments.
*   **Documentation Drift:** Static guides (like Markdown files) quickly become outdated as new software versions are released, leading to "documentation drift".
*   **Lack of Optimization:** Generic installation guides rarely account for specific hardware differences (e.g., H100 vs. GB200) or workload intents (Training vs. Inference).

### The Solution: Automated Approach
Eidos replaces manual interpretation of documentation with a **automated approach**. It treats infrastructure configuration as code, providing a deterministic engine that generates the exact artifacts needed for a specific environment.

**Key Benefits:**
1.  **Deterministic & Validated:** The system guarantees that the inputs (your system state) always produce the same valid outputs, tested against NVIDIA hardware.
2.  **Hardware-Aware Optimization:** Eidos detects the specific GPU type (e.g., H100, A100, GB200) and OS to apply hardware-specific tuning automatically.
3.  **Speed:** Deployment preparation drops from hours of reading and configuration to minutes of automated generation.
4.  **Supply Chain Security:** All artifacts are backed by SLSA Build Level 3 attestations and Software Bill of Materials (SBOMs), ensuring the software stack is secure and verifiable.

## How Eidos Works

Eidos simplifies operations through a logical four-stage workflow handled by the `eidos` command-line tool. This workflow transforms a raw system state into a deployable package.

### Step 1: Snapshot (Capture Reality)

Before configuring anything, Eidos needs to understand the environment.
*   **What it does:** The system captures the state of the OS, SystemD services, Kubernetes version, and GPU hardware.
*   **How it helps:** It eliminates guesswork. Instead of assuming what hardware is present, Eidos measures it directly using the CLI or a Kubernetes Agent.
*   **Automation:** The agent can run as a Kubernetes Job, writing the snapshot directly to a ConfigMap, enabling fully automated auditing without manual intervention.

### Step 2: Recipe (Generate Recommendations)

Once the system state is known, Eidos generates a "Recipe"—a set of configuration recommendations.
*   **What it does:** It matches the snapshot against a database of validated rules (overlays). It selects the correct driver versions, kernel modules, and settings for that specific environment.
*   **Intent-Based Tuning:** Users can specify an "Intent" (e.g., `training` or `inference`). Eidos adjusts the recipe to optimize for throughput (training) or latency (inference).
*   **Asymmetric Matching:** The criteria matching algorithm ensures generic queries (e.g., `--service eks --intent training`) only match generic recipes, not hardware-specific ones. Recipe "any" = wildcard, query "any" ≠ specific recipe.
*   **How it helps:** It ensures version compatibility and applies expert-level optimizations automatically, acting as a dynamic compatibility matrix.

### Step 3: Validate (Check Compatibility)

Before deploying, Eidos can validate that a target cluster meets the recipe requirements using multi-phase validation.
*   **What it does:** It compares recipe constraints (version requirements, configuration settings) against actual measurements from a cluster snapshot across different validation phases.
*   **Validation Phases:**
    - **Readiness**: Validates infrastructure prerequisites (K8s version, OS, kernel, GPU hardware)
    - **Deployment**: Validates component deployment health and expected resources
    - **Performance**: Validates system performance and network fabric health
    - **Conformance**: Validates workload-specific requirements
*   **Constraint Types:** Supports version comparisons (`>=`, `<=`, `>`, `<`), equality (`==`, `!=`), and exact match for configuration values.
*   **How it helps:** It catches compatibility issues before deployment, validates component health after deployment, and ensures performance requirements are met. Ideal for CI/CD pipelines with `--fail-on-error` flag and phased deployment validation.

### Step 4: Bundle (Create Artifacts)

Finally, Eidos converts the abstract Recipe into concrete deployment files.
*   **What it does:** It generates a "Bundle" containing Helm values, Kubernetes manifests, installation scripts, and a custom README.
*   **Deployer Options:** Supports multiple deployment methods: `helm` (Helm umbrella chart, default), `argocd` (Applications with sync-wave ordering).
*   **How it helps:** Users receive ready-to-run scripts and manifests. For example, it generates a custom `install.sh` script that pre-validates the environment before running Helm commands.
*   **Parallel Execution:** Multiple "Bundlers" (e.g., GPU Operator, Network Operator) can run simultaneously to generate a full stack configuration in seconds.

## Key Capabilities

### Kubernetes-Native Integration
Eidos is designed to work natively within Kubernetes.
*   **ConfigMap Support:** You don't need to manage local files. You can read and write Snapshots and Recipes directly to Kubernetes ConfigMaps using the URI format `cm://namespace/name`.
*   **No Persistent Volumes:** The automated Agent writes data directly to the Kubernetes API, simplifying deployment in restricted environments.

### Integration & Automation
*   **CI/CD Ready:** The `eidos` CLI and API server are built for pipelines. Teams can use Eidos to detect "Configuration Drift" by periodically taking snapshots and comparing them to a baseline.
*   **API Server:** For programmatic access, Eidos provides a production-ready HTTP REST API to generate recipes dynamically.

### Security
Eidos prioritizes trust in the software supply chain.
*   **Verifiable Builds:** Every release includes provenance data showing exactly how and where it was built (SLSA Level 3).
*   **SBOMs:** Complete inventories of all dependencies are provided for both binaries and container images, enabling automated vulnerability scanning.

## Documentation

### User Guide

| Document | Description |
|----------|-------------|
| [Installation](user-guide/installation.md) | Installing the `eidos` CLI |
| [CLI Reference](user-guide/cli-reference.md) | Complete CLI command reference with examples |
| [API Reference](user-guide/api-reference.md) | Quick start for the REST API |
| [Agent Deployment](user-guide/agent-deployment.md) | Running the snapshot agent as a Kubernetes Job |

### Architecture

| Document | Description |
|----------|-------------|
| [Architecture Overview](architecture/README.md) | System design, patterns, and deployment topologies |
| [CLI Architecture](architecture/cli.md) | Detailed CLI implementation and workflow diagrams |
| [API Server Architecture](architecture/api-server.md) | HTTP server design, middleware, and endpoints |
| [Data Architecture](architecture/data.md) | Recipe metadata system, criteria matching, and inheritance |
| [Bundler Development](architecture/component.md) | Guide for creating new bundlers |

### Integration

| Document | Description |
|----------|-------------|
| [API Reference](integration/api-reference.md) | Complete REST API specification with examples |
| [Automation](integration/automation.md) | CI/CD integration patterns |
| [Data Flow](integration/data-flow.md) | Understanding recipe data architecture |
| [Kubernetes Deployment](integration/kubernetes-deployment.md) | Self-hosted API server deployment |
| [Recipe Development](integration/recipe-development.md) | Adding and modifying recipe metadata |

### Demos

| Document | Description |
|----------|-------------|
| [End-to-End Demo](demos/e2e.md) | Complete workflow demonstration (snapshot → recipe → bundle) |
| [Data Architecture Demo](demos/data.md) | Recipe metadata system walkthrough |
| [Supply Chain Security Demo](demos/s3c.md) | Verifying SLSA attestations and SBOMs |

## Quick Start

### Install CLI

```shell
curl -sfL https://raw.githubusercontent.com/NVIDIA/eidos/main/install | bash -s --
```

### Generate Recipe

```shell
# Query mode: direct parameters
eidos recipe --service eks --accelerator h100 --intent training --platform kubeflow

# Snapshot mode: analyze captured state
eidos snapshot -o snapshot.yaml
eidos recipe --snapshot snapshot.yaml --intent training --platform kubeflow
```

### Validate Configuration

```shell
# Validate readiness phase (default)
eidos validate --recipe recipe.yaml --snapshot snapshot.yaml

# Validate all phases
eidos validate --recipe recipe.yaml --snapshot snapshot.yaml --phase all
```

### Create Bundle

```shell
eidos bundle --recipe recipe.yaml --output ./bundles
```

### Deploy

```shell
cd bundles/gpu-operator
./scripts/install.sh
```

## Links

- **GitHub Repository:** [github.com/NVIDIA/eidos](https://github.com/NVIDIA/eidos)
- **Contributing:** [CONTRIBUTING.md](../CONTRIBUTING.md)
- **Security:** [SECURITY.md](../SECURITY.md)
