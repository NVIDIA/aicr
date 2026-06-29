# Air-Gapped Image Mirroring

Discover every container image and Helm chart a recipe needs, then mirror them
into a private registry for air-gapped deployment. For the per-flag reference,
see [CLI reference: aicr mirror list](cli-reference.md#aicr-mirror-list). For
the static image inventory across all registered components, see
[Container Image Inventory](container-images.md).

## Overview

`aicr mirror list` renders each component's Helm chart with recipe-resolved
values, scans referenced manifests, and produces a deduplicated list of
container images and chart references. Manifest reads honor the data source
resolved for the `mirror list` invocation itself: pass `--data <dir>` to
`aicr mirror list` and an overlay manifest shadowing an embedded path is used
in place of the embedded copy. The data directory is **not** remembered from
when the recipe was generated — a saved recipe carries no `--data` path, so
you must supply `--data` again on each `mirror list` run that needs the
overlay. The output is available in four formats — two general-purpose
(YAML, JSON) and two tool-specific (Hauler, Zarf).

> **Trust boundary:** Discovery shells out to `helm template`, which executes
> the full Go template engine (`tpl`, `include`, `lookup`). AICR recipes
> reference trusted, pinned charts from known repositories. Do not run
> `mirror list` against untrusted or unvetted recipe files — doing so executes
> arbitrary template code from those charts.

```text
                      ┌────────────────────-──┐
  aicr recipe ───────▶│  aicr mirror list     │
  (or query params)   │  --format hauler|zarf │
                      └──────────┬──────-─────┘
                                 │
              ┌──────────────────┼──────────────────┐
              ▼                                     ▼
     ┌────────────────┐                   ┌─────────--────────┐
     │  hauler store  │                   │  zarf package     │
     │  sync → copy   │                   │  create → mirror  │
     └───────┬────────┘                   └────────┬──────--──┘
             │                                     │
             ▼                                     ▼
     ┌────────────────────────────────────────────────────┐
     │              Private Registry                      │
     └────────────────────────────────────────────────────┘
```

**How this relates to chart vendoring:** `aicr bundle --vendor-charts` embeds
Helm chart tarballs into a bundle so `helm install` needs no registry egress.
`aicr mirror list` discovers the *container images* those charts reference plus
the chart coordinates themselves. For a fully air-gapped deployment, use both:
mirror images and charts into your private registry, then deploy the vendored
bundle.

## Prerequisites

- `aicr` CLI installed (see [Installation](installation.md))
- A recipe file (`aicr recipe --output recipe.yaml`) or query parameters
- `helm` v3+ on `$PATH` (required for chart rendering during discovery)
- For the Hauler workflow: `hauler` CLI
- For the Zarf workflow: `zarf` CLI
- YAML/JSON output works without any additional tools

## Output Formats

| Format | Description | Consumable By |
|--------|-------------|---------------|
| `yaml` | Full mirror list with per-component breakdown, global image list, chart refs, and metadata (default) | Any YAML parser, CI/CD pipelines, custom scripts |
| `json` | Same structure as YAML in JSON encoding | `jq`, programmatic tooling |
| `hauler` | Hauler content manifest (`content.hauler.cattle.io/v1`) with `Images` and `Charts` documents | `hauler store sync` |
| `zarf` | Zarf package config (`ZarfPackageConfig`) with images and charts in a single component | `zarf package create` |

## Quick Start

### Discover Images

From an existing recipe file:

```shell
aicr mirror list --recipe recipe.yaml
```

Or resolve a recipe from query parameters:

```shell
aicr mirror list --service eks --accelerator h100 --intent training --os ubuntu
```

### Save to File

```shell
aicr mirror list --recipe recipe.yaml --format hauler --output hauler-manifest.yaml
```

## Hauler Workflow

[Hauler](https://docs.hauler.dev/) syncs container images and Helm charts into
a local store, then copies them to a target registry.

### 1. Generate the Hauler manifest

```shell
aicr mirror list --recipe recipe.yaml --format hauler --output manifest.yaml
```

The output is a multi-document YAML with an `Images` document (and optionally a
`Charts` document):

```yaml
apiVersion: content.hauler.cattle.io/v1
kind: Images
metadata:
  name: aicr-images
spec:
  images:
    - name: nvcr.io/nvidia/cloud-native/gpu-operator:v25.3.0
    - name: registry.k8s.io/nfd/node-feature-discovery:v0.17.2
    # ...
---
apiVersion: content.hauler.cattle.io/v1
kind: Charts
metadata:
  name: aicr-charts
spec:
  charts:
    - name: gpu-operator
      repoURL: oci://ghcr.io/nvidia
      version: v25.3.0
    # ...
```

### 2. Sync into the Hauler store

```shell
hauler store sync \
  --store ./hauler-store \
  --filename manifest.yaml
```

To pull only a single platform (reduces download size):

```shell
hauler store sync \
  --store ./hauler-store \
  --platform linux/amd64 \
  --filename manifest.yaml
```

### 3. Copy to your private registry

```shell
hauler store copy \
  --store ./hauler-store \
  registry://my-registry.example.com:5000
```

For registries using plain HTTP (e.g., local test registries):

```shell
hauler store copy \
  --store ./hauler-store \
  --plain-http \
  registry://localhost:5001
```

## Zarf Workflow

[Zarf](https://docs.zarf.dev/) packages container images into a single
distributable tarball that can be carried to an air-gapped environment and
mirrored into a registry.

### 1. Generate the Zarf package config

```shell
aicr mirror list --recipe recipe.yaml --format zarf --output zarf.yaml
```

The output is a `ZarfPackageConfig`:

```yaml
apiVersion: zarf.dev/v1alpha1
kind: ZarfPackageConfig
metadata:
  name: aicr
  description: Container images and Helm charts for AICR recipe deployment
  version: 0.0.1
components:
  - name: aicr-images
    required: true
    images:
      - nvcr.io/nvidia/cloud-native/gpu-operator:v25.3.0
      - registry.k8s.io/nfd/node-feature-discovery:v0.17.2
      # ...
    charts:
      - name: gpu-operator
        url: oci://ghcr.io/nvidia/gpu-operator
        version: v25.3.0
        namespace: gpu-operator
      # ...
```

### 2. Create the Zarf package

Place `zarf.yaml` in its own directory and create the package:

```shell
mkdir -p zarf-pkg && cp zarf.yaml zarf-pkg/
cd zarf-pkg
zarf package create . --confirm
```

This pulls every listed image and produces a `zarf-package-*.tar.zst` file.

### 3. Mirror to your private registry

Transfer the tarball to the air-gapped environment, then mirror:

```shell
zarf package mirror-resources zarf-package-aicr-*.tar.zst \
  --registry-url my-registry.example.com:5000 \
  --confirm
```

For registries using plain HTTP:

```shell
zarf package mirror-resources zarf-package-aicr-*.tar.zst \
  --registry-url localhost:5001 \
  --plain-http \
  --confirm
```

## Overriding Component Values

The `--set` flag overrides Helm values at discovery time, changing which images
appear in the output. This is useful when you know certain sub-components will
be disabled in your deployment and want to exclude their images from the mirror
list.

```shell
# Exclude GPU driver images (pre-installed driver scenario)
aicr mirror list --recipe recipe.yaml \
  --set gpuoperator:driver.enabled=false

# Pin a specific driver version
aicr mirror list --recipe recipe.yaml \
  --set gpuoperator:driver.version=570.86.16
```

The override key format is `component:path.to.field=value`, where the component
name matches either the component name in the recipe or its `valueOverrideKeys`
alias from the registry (e.g., `gpuoperator` for `gpu-operator`).

## Relationship to Container Image Inventory

The [Container Image Inventory](container-images.md) is a static reference
generated from `recipes/registry.yaml` with default values. It lists every
image across all registered components regardless of recipe.

`aicr mirror list` is recipe-specific: it renders charts with the actual
resolved values for your target configuration (service, accelerator, intent,
OS). The resulting image list is typically a subset of the full inventory,
limited to the components and sub-components your recipe enables.

Use the inventory for security audit and compliance. Use `mirror list` to
generate the deployment-specific manifest for a specific deployment.

> **Completeness caveat:** discovery is best-effort, not all-or-nothing. A
> per-component failure to load values, render the chart (`helm template`), or
> extract images from a manifest is **non-fatal** — the component's images are
> omitted and the failure is recorded as a warning, but the command still
> exits 0. The YAML and JSON outputs carry these warnings in a `warnings`
> field; the Hauler and Zarf outputs **omit them entirely**. Treat a
> tool-specific manifest as authoritative only after confirming the YAML/JSON
> run reported no warnings — otherwise the mirrored set may be silently
> incomplete.
