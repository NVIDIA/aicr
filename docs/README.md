# AI Cluster Runtime (AICR)

NVIDIA AI Cluster Runtime (AICR) generates validated, reproducible
configuration artifacts for GPU-accelerated Kubernetes clusters.
Given a description of your environment — cloud, accelerator, OS,
intent — AICR emits the Helm, Argo CD, Flux, or Helmfile artifacts
your deployment tool consumes. The output is hardware-aware,
version-locked, and backed by SLSA Build Level 3 image provenance.

For the project pitch, supported environments, and a feature
overview, see the [repository README](https://github.com/NVIDIA/aicr).

## Find Your Path

| If you are a... | Start here |
|-----------------|-----------|
| **User** — operator deploying AICR to provision or validate a cluster | [User Guide](user/index.md) |
| **Integrator** — engineer embedding AICR in a CI/CD pipeline, GitOps flow, or larger platform | [Integrator Guide](integrator/automation.md) |
| **Contributor** — developer extending AICR or shipping recipes | [Contributor Guide](contributor/index.md) |

### User Guide

For operators running `aicr` against real clusters.

| Topic | Doc |
|-------|-----|
| Install the CLI | [Installation](user/installation.md) |
| Full workflow, start to finish | [End-to-End Tutorial](user/tutorial.md) |
| Every command and flag | [CLI Reference](user/cli-reference.md) |
| Render a recipe into deployment artifacts | [Generating Bundles](user/bundling.md) |
| REST API for `aicrd` | [API Reference](user/api-reference.md) |
| Run the snapshot agent in-cluster | [Agent Deployment](user/agent-deployment.md) |
| Validate a recipe against a live cluster | [Validation](user/validation.md) |
| Components that can appear in a recipe | [Component Catalog](user/component-catalog.md) |
| Air-gapped mirroring | [Air-Gap Mirror](user/air-gap-mirror.md) |

### Integrator Guide

For pipelines and platforms that call AICR programmatically or host
`aicrd`.

| Topic | Doc |
|-------|-----|
| CI/CD integration patterns | [Automation](integrator/automation.md) |
| Self-host `aicrd` on Kubernetes | [Kubernetes Deployment](integrator/kubernetes-deployment.md) |
| Add or modify recipe metadata | [Recipe Development](integrator/recipe-development.md) |
| Verify artifacts (SLSA, SBOM, attestations) | [Supply Chain Verification](integrator/supply-chain-verification.md) |
| Ship custom validators via `--data` | [Validator Extension](integrator/validator-extension.md) |
| Cloud-specific GPU setup | [AKS](integrator/aks-gpu-setup.md), [EKS networking](integrator/eks-dynamo-networking.md), [GKE networking](integrator/gke-tcpxo-networking.md), [Talos](integrator/talos-integration.md) |

### Contributor Guide

For developers working on AICR itself.

| Topic | Doc |
|-------|-----|
| Architecture, boundaries, package map | [Architecture Overview](contributor/index.md) |
| Recipes, overlays, mixins | [Recipes](contributor/recipe.md) |
| Adding a component | [Components](contributor/component.md) |
| Adding a snapshot collector | [Collectors](contributor/collector.md) |
| All four validation surfaces | [Validators](contributor/validator.md) |
| CLI internals | [CLI](contributor/cli.md) |
| API server internals | [API Server](contributor/api-server.md) |
| Testing surfaces and the `make qualify` gate | [Testing](contributor/tests.md) |
| Release runbook | [Maintaining AICR](contributor/maintaining.md) |

## The Four-Stage Workflow

```text
┌──────────┐    ┌────────┐    ┌──────────┐    ┌────────┐
│ Snapshot │───▶│ Recipe │───▶│ Validate │───▶│ Bundle │
└──────────┘    └────────┘    └──────────┘    └────────┘
   capture       generate       check          emit
   cluster       optimized      constraints    deployment
   state         config         vs. actual     artifacts
```

Each stage produces a serializable artifact and is independently
invocable. Stages can be chained or run standalone, and inputs and
outputs flow through files, stdout, or Kubernetes ConfigMaps
(`cm://namespace/name`). For the CLI walkthrough see
[CLI Reference](user/cli-reference.md); for the architecture see
[contributor/index.md](contributor/index.md).

## Glossary

Reference for the terms used across the docs site.

| Term | Definition |
|------|------------|
| **Snapshot** | Captured state of a target system (OS, kernel, Kubernetes, GPU, SystemD). Produced by `aicr snapshot` or the in-cluster snapshot Job. |
| **Recipe** | Resolved configuration spec — component refs, constraints, deployment order — produced by `aicr recipe` from criteria or from a snapshot. |
| **Criteria** | Query parameters that select a recipe: `service`, `accelerator`, `intent`, `os`, `platform`, `nodes`. |
| **Overlay** | A recipe metadata file (`kind: RecipeMetadata`) under `recipes/overlays/` matched by criteria. Composes via single-parent inheritance (`spec.base`). |
| **Mixin** | A composable fragment (`kind: RecipeMixin`) under `recipes/mixins/` carrying only `constraints` and `componentRefs`, referenced via `spec.mixins`. |
| **Bundle** | Deployment artifacts emitted by `aicr bundle`: Helm values, manifests, install scripts, checksums. |
| **Bundler** | A per-component generator that emits the bundle inputs (e.g., GPU Operator bundler). |
| **Deployer** | An output adapter that serializes a bundle in a tool-specific format: `helm`, `helmfile`, `argocd`, `argocd-helm`, `flux`. |
| **Component** | A deployable software package (e.g., GPU Operator, Network Operator). Lives in `recipes/registry.yaml`. |
| **ComponentRef** | A reference to a component inside a recipe — version, source, values file, dependencies. |
| **Constraint** | A declarative validation rule on a recipe (e.g., `K8s.server.version >= 1.32.4`). |
| **Validation Phase** | A stage of `aicr validate`: readiness (always implicit), deployment, performance, conformance. |
| **Measurement** | A snapshot data point keyed by type (K8s, OS, GPU, SystemD, NodeTopology, NetworkTopology), subtype, and reading. |
| **Specificity** | A score counting non-`any` criteria fields. More-specific overlays merge later. |
| **Asymmetric matching** | Criteria-matching rule: recipe `any` is a wildcard; query `any` does not match a specific recipe. |
| **ConfigMap URI** | `cm://namespace/name` — read or write snapshots and recipes directly to Kubernetes ConfigMaps. |
| **SLSA / SBOM** | Supply-chain Levels for Software Artifacts (release images reach Build Level 3) and Software Bill of Materials shipped with binaries and images. |

## Links

- [GitHub](https://github.com/NVIDIA/aicr) · [Releases](https://github.com/NVIDIA/aicr/releases) · [Issues](https://github.com/NVIDIA/aicr/issues)
- [Contributing](https://github.com/NVIDIA/aicr/blob/main/CONTRIBUTING.md) · [Security](https://github.com/NVIDIA/aicr/blob/main/SECURITY.md) · [Roadmap](https://github.com/NVIDIA/aicr/blob/main/ROADMAP.md)
- Slack: [#aicr](https://kubernetes.slack.com/archives/C0AQMPP1BK7) on Kubernetes Slack
