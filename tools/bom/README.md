# bom — AICR Container Image Bill-of-Materials

Generates an authoritative inventory of every container image AICR can deploy.
Renders each Helm chart in `recipes/registry.yaml` at its pinned version,
extracts every `image:` reference, walks embedded manifests under
`recipes/components/<name>/manifests/`, and emits:

- `bom.cdx.json` — CycloneDX 1.6 JSON (canonical, machine-readable)
- `bom.md` — human-readable Markdown summary

This is a **reference BOM**: it lists images that *would* be deployed at the
pinned chart versions, based on rendering. It does not pull or scan the images
themselves — that belongs to a separate provenance audit.

## Architecture

The reusable BOM logic lives in [`pkg/bom`](../../pkg/bom): YAML image
extraction, image reference parsing, CycloneDX assembly, and Markdown
rendering. This tool (`tools/bom`) is a thin CLI wrapper that adds:

- Loading `recipes/registry.yaml`
- Shelling out to `helm template` for each chart
- Walking embedded manifest directories

`pkg/bom` is also intended to be consumed by `pkg/bundler` to emit a
per-bundle SBOM during `aicr bundle`.

## Usage

From the repo root:

```bash
# Default — writes to dist/bom/
make bom

# Skip helm rendering (manifest-only, no network needed)
BOM_SKIP_HELM=1 make bom

# Fail on unpinned chart versions or render errors
BOM_STRICT=1 make bom

# Custom output directory
BOM_OUT_DIR=/tmp/aicr-bom make bom
```

Direct invocation:

```bash
GOFLAGS="-mod=vendor" go run ./tools/bom -repo-root . -out-dir /tmp/bom
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-repo-root` | `.` | Path to the AICR repo root |
| `-out-dir` | `dist/bom` | Output directory |
| `-aicr-version` | `dev` | AICR version embedded in the BOM |
| `-skip-helm` | `false` | Skip `helm template` rendering |
| `-strict` | `false` | Fail on unpinned charts or render errors |

## Output schema

`bom.cdx.json` is CycloneDX 1.6 with this modeling:

- `metadata.component` — AICR itself (`type: application`).
- `components[]` contains:
  - One entry per AICR component (`type: application`, `bom-ref: aicr/<name>`)
    with helm chart metadata in `properties[]`.
  - One entry per unique container image (`type: container`,
    `bom-ref: img:<image>`, `purl: pkg:oci/<name>@<version>?repository_url=...`).
- `dependencies[]` expresses the deployment graph:
  - `aicr` → all `aicr/<name>` refs
  - Each `aicr/<name>` → its image refs

This shape is consumable by Trivy, Grype, Cosign attestation, and most
supply-chain dashboards without conversion.

## Limitations

- Charts that fail to render (missing required values, network unreachable)
  emit a warning property on the component and contribute zero images. Use
  `-strict` to make these fatal.
- Image extraction looks for any `image:` scalar in the rendered YAML. False
  positives are possible but rare in valid Kubernetes manifests; CRDs that
  use `image` as an unrelated field would be flagged.
- Unrendered Go template directives (`{{ .Values.image }}`) are skipped — they
  appear when the chart's values don't supply a required override.
