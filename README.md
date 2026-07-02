# NVIDIA AI Cluster Runtime

[![On Push CI](https://github.com/NVIDIA/aicr/actions/workflows/on-push.yaml/badge.svg)](https://github.com/NVIDIA/aicr/actions/workflows/on-push.yaml)
[![On Tag Release](https://github.com/NVIDIA/aicr/actions/workflows/on-tag.yaml/badge.svg)](https://github.com/NVIDIA/aicr/actions/workflows/on-tag.yaml)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

AI Cluster Runtime (AICR) makes it easy to stand up GPU-accelerated Kubernetes clusters. It captures known-good combinations of drivers, operators, kernels, and system configurations and publishes them as version-locked **recipes** — reproducible artifacts for Helm, Argo CD, Flux, and Helmfile.

> **Full documentation:** [docs.nvidia.com/aicr](https://docs.nvidia.com/aicr)

## Why We Built This

Running GPU-accelerated Kubernetes clusters reliably is hard. Small differences in kernel versions, drivers, container runtimes, operators, and Kubernetes releases can cause failures that are difficult to diagnose and expensive to reproduce.

Historically, this knowledge has lived in internal validation pipelines and runbooks. AI Cluster Runtime makes it available to everyone.

Every AICR recipe is:

- **Optimized** — Tuned for a specific combination of hardware, cloud, OS, and workload intent.
- **Validated** — Passes automated constraint and compatibility checks before publishing.
- **Reproducible** — Same inputs produce identical deployments every time.

Every AICR recipe also carries two kinds of cryptographic proof: **where it came from** (provenance — signed by NVIDIA CI, verifiable offline) and, **for recipes with published evidence, what their validation recorded** (validity — a signer-bound, tamper-evident attestation that binds an identity to a recorded `aicr validate` result, from contributors with cluster access NVIDIA doesn't have). See [SECURITY.md](SECURITY.md) and the [bundle attestation](demos/bundle-attestation.md), [recipe evidence](demos/evidence.md), and [build provenance](demos/provenance.md) demos for the full chain.

## Quick Start

```bash
# Install the CLI (Homebrew)
brew tap NVIDIA/aicr
brew install aicr

# Or use the install script
curl -sfL https://get.aicr.run | bash -s --

# Generate a recipe for your environment
aicr recipe --service eks --accelerator h100 --os ubuntu \
  --intent training --platform kubeflow -o recipe.yaml

# Render it into deployment-ready bundles (helm, argocd, flux, or helmfile)
aicr bundle --recipe recipe.yaml --deployer argocd --output ./bundles

# After deploying the bundle, validate the running cluster against the recipe
aicr validate --recipe recipe.yaml

# Select hydrated config value (e.g., the resolved GPU driver version)
aicr query --service eks --accelerator h100 --os ubuntu \
  --intent training --platform kubeflow \
  --selector components.gpu-operator.values.driver.version
```

The contents of the `bundles/` directory depend on the chosen `--deployer`: Argo CD `Application` manifests for `argocd`, a Helm chart app-of-apps for `argocd-helm`, `HelmRelease` and `Kustomization` manifests for `flux`, `helmfile.yaml` release graph for `helmfile`, or simple Helm commands for `helm`.

See the [Installation Guide](docs/user/installation.md) for manual installation, building from source, and container images.

## Features

| Feature | Description |
|---------|-------------|
| **`aicr` CLI** | Single binary for the full workflow: snapshot, recipe, bundle, validate, verify, diff, and trust management. |
| **API Server (`aicrd`)** | REST API exposing the same capabilities as the CLI. Run in-cluster for CI/CD integration or air-gapped environments. |
| **Go Library (`github.com/NVIDIA/aicr/pkg/client/v1`)** | Stable Go SDK facade for in-process consumers — same workflow (resolve, bundle, snapshot, validate) callable from any Go program without a subprocess or REST hop. Per-Client isolation supports multi-tenant use. |
| **Snapshot Agent** | Kubernetes Job that captures live cluster state (GPU hardware, drivers, kernel, OS, operators, K8s config) into a ConfigMap for validation against recipes. |
| **Multi-Deployer Bundles** | Render the same recipe into Helm, Argo CD (App of Apps or Helm chart variant), Flux, or Helmfile artifacts — pick whichever fits your GitOps pipeline. |
| **Multi-Phase Validation** | Deployment, performance (training and inference), and conformance phases — run all or one at a time. |
| **Drift Detection** | `aicr diff` compares two snapshots to surface configuration drift between clusters or over time. |
| **Supply Chain Security** | SLSA Build Level 3 image provenance, signed image SBOMs, image attestations (Cosign / Sigstore), and `aicr verify` for offline bundle verification. |

## Supported Components

AICR recipes compose components from the following groups:

| Group | Examples |
|-------|----------|
| **GPU stack** | GPU Operator, DRA GPU Driver, Network Operator, NFD, NVSentinel |
| **Cloud integration** | AWS EFA, AWS EBS CSI, GKE NCCL TCPxO |
| **Node tuning** | Nodewright Operator and customizations, cert-manager |
| **Observability** | kube-prometheus-stack, Prometheus Operator CRDs, Prometheus Adapter, ephemeral-storage metrics |
| **Training platforms** | Kubeflow Trainer, Slinky Slurm Operator, KAI Scheduler, Kueue |
| **Inference platforms** | Dynamo, Grove, NIM Operator, Agent Gateway |

See the full [Component Catalog](docs/user/component-catalog.md) for every component, pinned version, and source. Don't see what you need? [Open an issue](https://github.com/NVIDIA/aicr/issues) — feedback helps inform future validation priorities.

### Supported Environments

| Dimension | Values |
|-----------|--------|
| **Services** | AKS, BCM, EKS, GKE, Kind, LKE, OCP, OKE |
| **Accelerators** | A100, B200, GB200, H100, H200, L40, RTX PRO 6000 |
| **Operating systems** | Amazon Linux, COS, RHEL, Talos, Ubuntu |
| **Workload intents** | Inference, Training |
| **Platforms** | Dynamo, Kubeflow, NIM, Run:ai, Slurm (Slinky) |

## How It Works

A **recipe** is a version-locked configuration for a specific environment. You describe your target (cloud, GPU, OS, workload intent, optional platform), and the recipe engine matches it against a library of validated **overlays** — layered configurations that compose bottom-up from base defaults through cloud, accelerator, OS, and workload-specific tuning. Composable **mixins** carry shared fragments (OS constraints, platform components) so a leaf overlay only declares what is unique to it.

The **bundler** materializes a recipe into deployment-ready artifacts: one folder per component, each with Helm values, checksums, and a README. The **validator** compares a recipe against a live cluster snapshot — first checking declarative constraints, then optionally running deployment, performance, and conformance phases inside the cluster.

This separation means the same validated configuration works whether you deploy with Helm, Argo CD, Flux, Helmfile, or a custom pipeline.

## What AI Cluster Runtime Is Not

- Not a Kubernetes distribution
- Not a cluster provisioner or lifecycle management system
- Not a managed control plane or hosted service
- Not a replacement for your cloud provider or OEM platform
- Not a generic configuration management platform

At its core, AICR is a cluster configuration generator. You bring your GPU-accelerated Kubernetes cluster and your deployment tooling; AICR generates the runtime configuration artifacts your tools deploy to the cluster. AICR can also validate that the configuration was correctly materialized and that it delivers the expected performance characteristics.

## Documentation

Full documentation lives at **[docs.nvidia.com/aicr](https://docs.nvidia.com/aicr)**. Key entry points:

- **[Installation](docs/user/installation.md)** — Install the `aicr` CLI (script, manual, or build from source)
- **[CLI Reference](docs/user/cli-reference.md)** — Every command, flag, and example
- **[API Reference](docs/user/api-reference.md)** — REST API endpoints for `aicrd`
- **[Agent Deployment](docs/user/agent-deployment.md)** — Run the snapshot agent in your cluster
- **[Validation](docs/user/validation.md)** — Deployment, performance, and conformance phases
- **[Component Catalog](docs/user/component-catalog.md)** — Every component that can appear in a recipe
- **[Recipe Development](docs/integrator/recipe-development.md)** — Add or modify recipe metadata
- **[Automation Guide](docs/integrator/automation.md)** — CI/CD integration patterns

For contributors:

- **[Contributing Guide](CONTRIBUTING.md)** — Development setup, testing, and PR process
- **[Code of Conduct](CODE_OF_CONDUCT.md)** — Community standards and enforcement
- **[Development Guide](DEVELOPMENT.md)** — Local development, Make targets, and tooling
- **[Architecture Overview](docs/contributor/index.md)** — System design and packages

## Resources

- **[Roadmap](ROADMAP.md)** — Feature priorities and development timeline
- **[Adopters](ADOPTERS.md)** — Organizations and projects using or building on AICR
- **[Security](SECURITY.md)** — Supply chain security, vulnerability reporting, and verification
- **[Releases](https://github.com/NVIDIA/aicr/releases)** — Binaries, SBOMs, and attestations
- **[Issues](https://github.com/NVIDIA/aicr/issues)** — Bugs, feature requests, and questions
- **Slack** — Join [Kubernetes Slack](https://kubernetes.slack.com) and visit the [#aicr](https://kubernetes.slack.com/archives/C0AQMPP1BK7) channel

## Contributing

AI Cluster Runtime is under Apache 2.0 [LICENSE](LICENSE). Contributions are welcome: new recipes for environments we haven't covered, additional bundler formats, validation checks, or bug reports. See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and the PR process.
