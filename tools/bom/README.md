# bom — AICR Container Image Bill-of-Materials

Generates an authoritative inventory of every container image AICR can deploy.
Renders each Helm chart in `recipes/registry.yaml` at its pinned version,
extracts recognized container-image references, walks embedded manifests under
`recipes/components/<name>/manifests/`, adds the images each component declares
in `recipes/components/<name>/images.yaml`, and emits:

- `bom.cdx.json` — CycloneDX 1.6 JSON (canonical, machine-readable)
- `bom.md` — human-readable Markdown summary

This is a **reference BOM**: it lists images that *would* be deployed at the
pinned chart versions, based on rendering. It does not pull or scan the images
themselves — that belongs to a separate provenance audit.

## Architecture

Reusable BOM logic lives in [`pkg/bom`](../../pkg/bom): YAML image extraction, image reference parsing, CycloneDX assembly, Markdown rendering. This tool is a thin CLI wrapper that loads `recipes/registry.yaml`, shells out to `helm template` per chart, walks embedded manifest directories, and reads each component's `images.yaml`. `pkg/bom` is also intended for consumption by `pkg/bundler` to emit a per-bundle SBOM during `aicr bundle`.

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
go run ./tools/bom -repo-root . -out-dir /tmp/bom
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-repo-root` | `.` | Path to the AICR repo root |
| `-out-dir` | `dist/bom` | Output directory |
| `-aicr-version` | `dev` | AICR version embedded in the BOM |
| `-skip-helm` | `false` | Skip `helm template` rendering |
| `-strict` | `false` | Fail on unpinned charts, render errors, or a Helm chart that renders no images and is not in `expectedNoImages` |
| `-deterministic` | `false` | Suppress per-run metadata (timestamps, version churn) in Markdown for committable artifacts |
| `-no-title` | `false` | Omit the H1 title so the body can be embedded as a section |

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
  `-strict` to make these fatal. `make bom-docs` always runs strict. Render
  failures are retried up to three times before they count.
- A component's `images.yaml` with no entry for a surveyed chart version or
  operator image fails the run in every mode, as does a missing file for a
  component in `declaredRequiredComponents`. See
  [Declared Images](../../docs/contributor/component.md#declared-images).
- For each `enabledBy` path in a surveyed chart's `images.yaml` entry, the
  chart is rendered again with that path set to `true`. A gated image that
  does not appear in that render, or appears in the default render, fails
  the run in every mode. A failed gated render is a warning, so it fails
  only under `-strict`. `-skip-helm` skips the check.
- A Helm chart that legitimately renders no images (CRD-only charts) must be
  listed in `expectedNoImages` in `main.go`, with a reason. Under `-strict`,
  an unlisted chart with zero images fails, and a listed chart that starts
  rendering images fails, so a failed pull cannot look like an empty chart.
- `make bom-docs` requires the helm version pinned in `.settings.yaml`, and
  re-runs the committed-BOM freshness test so a dropped component row fails.
- Image extraction recognizes scalar `image:` values and mapping-valued
  `image:` or `scalingPodImage:` descriptors using the `name`, `repository`,
  and `tag` fields. A present field must be a non-null, non-empty scalar.
  Violations stop generation in all modes rather than publishing an incomplete
  reference. Other fields are ignored. Descriptors that rely on separate
  `registry` or `digest` fields are unsupported and can yield an incomplete or
  incorrectly resolved reference.
- False positives are possible but rare in valid Kubernetes manifests. CRDs
  that use `image` as an unrelated scalar field may be flagged.
- Unrendered Go template directives (`{{ .Values.image }}`) are skipped — they
  appear when the chart's values don't supply a required override.
