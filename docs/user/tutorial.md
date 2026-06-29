# End-to-End Tutorial

This tutorial walks the full AICR workflow once, end to end: install the CLI,
generate a recipe for your environment, render it into deployment bundles,
deploy them, and validate the running cluster against the recipe. It is a
learning path — follow it top to bottom on a non-production cluster to build a
mental model of how the four stages fit together.

For the conceptual overview of the four stages, see the
[documentation hub](../README.md#the-four-stage-workflow). For exhaustive flag
lists, see the [CLI Reference](cli-reference.md).

## Prerequisites

- A GPU-accelerated Kubernetes cluster you can deploy to (EKS, GKE, AKS, or a
  local Kind/KWOK cluster for a dry run). `kubectl` configured to reach it.
- The `helm` binary on your `PATH` (the default `helm` deployer emits Helm
  commands).
- About 15 minutes. No NVIDIA hardware is required to generate a recipe or a
  bundle — only the deploy and validate stages touch a real cluster.

## Step 1 — Install the CLI

```bash
# Homebrew
brew tap NVIDIA/aicr
brew install aicr

# Or the install script
curl -sfL https://get.aicr.run | bash -s --

aicr --version
```

For manual installation, container images, or building from source, see
[Installation](installation.md).

## Step 2 — Generate a recipe

A **recipe** is a version-locked configuration for a specific environment.
Describe your target with criteria flags and AICR matches it against its
library of validated overlays:

```bash
aicr recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent training \
  --platform kubeflow \
  --output recipe.yaml
```

Open `recipe.yaml` — it lists the components that will be deployed, their
pinned versions, the declarative constraints, and the deployment order. The
valid values for each criterion (services, accelerators, operating systems,
intents, platforms) are enumerated in the [CLI Reference](cli-reference.md) and
the [documentation hub glossary](../README.md#glossary).

> Prefer to start from your live cluster instead of criteria? Capture a
> snapshot first (`aicr snapshot --output snapshot.yaml`) and pass
> `--snapshot snapshot.yaml` to `aicr recipe`. See
> [Agent Deployment](agent-deployment.md) for in-cluster snapshot capture.

## Step 3 — Inspect a resolved value (optional)

Before bundling, you can query any hydrated value without rendering the whole
bundle — useful for scripting and sanity checks:

```bash
aicr query \
  --service eks --accelerator h100 --os ubuntu --intent training --platform kubeflow \
  --selector components.gpu-operator.values.driver.version
```

## Step 4 — Render deployment bundles

The **bundler** materializes the recipe into deployment-ready artifacts — one
folder per component, each with Helm values, checksums, and a README:

```bash
aicr bundle --recipe recipe.yaml --output ./bundles
```

With the default `helm` deployer, `./bundles` contains per-component folders
and a `deploy.sh` that runs the Helm installs in dependency order. To target a
GitOps tool instead (Argo CD, Flux, Helmfile), or to override values and
scheduling, see [Generating Bundles](bundling.md).

## Step 5 — Deploy to your cluster

```bash
cd bundles
chmod +x deploy.sh
./deploy.sh
```

This installs each component in order. Watch the GPU Operator and any platform
components come up with `kubectl get pods -A -w`.

## Step 6 — Validate the running cluster

The **validator** compares the recipe against the live cluster — first the
declarative constraints, then optional in-cluster phases (deployment,
performance, conformance):

```bash
aicr validate --recipe recipe.yaml
```

A clean run exits 0. For the phase model, performance testing, and emitting
signed evidence for a recipe PR, see [Validation](validation.md).

## Where to go next

- [Generating Bundles](bundling.md) — deployers, value overrides, node
  scheduling, offline/vendored charts, and readiness gates.
- [Validation](validation.md) — deployment, performance, and conformance phases.
- [Agent Deployment](agent-deployment.md) — run the snapshot agent in-cluster.
- [Component Catalog](component-catalog.md) — every component a recipe can include.
