# corroborate

`corroborate` computes the recipe **corroboration consensus model** and emits the
deterministic interim-evidence dashboard (GP4). Given a source-keyed evidence tree —
per signer, per run: a verified `meta.json` (recipe coordinate, signer identity +
class, versions, `attestedAt`) beside its `ctrf/<phase>.json` reports — it writes
`index.json` + per-recipe `series/<recipe>.json` and a self-contained static renderer
that reads them.

The model counts **distinct verified signers, never builds** — N nightly runs from
one CI loop are one source, so a sybil cannot manufacture a `CONFIRMED` cell. See
`pkg/corroborate` for the consensus rules.

## Usage

```sh
go run ./tools/corroborate -in <evidence-dir> -out <output-dir> [-allowlist <allowlist.yaml>]
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-in` | _(required)_ | Root of the source-keyed evidence tree (the GCS layout synced to disk, e.g. via `gcloud storage rsync`). Every `meta.json` beneath it is read with its sibling `ctrf/<phase>.json` reports. |
| `-out` | `dist/corroborate` | Output directory. |
| `-allowlist` | _(none)_ | Optional signer allowlist (`recipes/evidence/allowlist.yaml`). When set, each source's class is **re-derived** from its verified `(issuer, identity)` instead of trusting the `signer.class` baked into `meta.json`. |

### Output layout

```text
<out>/
  index.html              # self-contained renderer (fetches the data below)
  data/
    index.json            # catalog + sources + criteria + per-recipe grid (baked consensus)
    series/<recipe>.json  # per-source build history, lazy-loaded on a drilldown
```

`fetch()` cannot read `file://`, so serve the output over HTTP to view it:

```sh
cd <out> && python3 -m http.server 8000   # then open http://localhost:8000/
```

On GitHub Pages the files are served over HTTP and work directly.

### All versions vs. a specific version

By default the dashboard shows the **all versions** view: each source's single
latest run, folded into one grid, so every source that has ever attested a recipe
is visible even when their latest runs are at different AICR releases. Corroboration
here counts agreement *across* versions.

Selecting a specific release in **FILTER EVIDENCE → AICR VERSION** switches to that
version's **strict** grid, where consensus counts agreement only among runs at the
*same* version (cross-version agreement is not a reproduction). Both grids are baked
in Go (`Tab.Combined` and `Tab.Versions`), so the renderer only chooses which to show.

## Reading from GCS

There is no embedded cloud client. Sync the bucket to a directory first, then point
`-in` at it:

```sh
gcloud storage rsync -r gs://<bucket>/results ./evidence/results
go run ./tools/corroborate -in ./evidence -out ./dist/corroborate
```

## Determinism

The JSON and HTML are **byte-identical across runs** from the same inputs: no
`time.Now()`, no random, no UUID on the emit path. Every timestamp comes from the
bundle predicate's `attestedAt` (carried in `meta.json`), and every collection is
sorted (coordinate, `PhaseOrder`, CTRF name, signer-id-hash, JSON map keys). This
mirrors `tools/bom -deterministic` / `pkg/serializer.MarshalYAMLDeterministic`, so
the output is safe to commit and to publish from CI.
