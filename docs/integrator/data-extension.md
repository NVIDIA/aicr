# Data Extension via `--data`

Extend AICR's embedded recipe catalog with your own overlays, components, and
criteria values at runtime — no fork, no rebuild. This is how operators add
private/proprietary content (internal cloud providers, in-house GPU SKUs,
commercial platforms, customer-specific scheduling) on top of the OSS catalog
shipped with the binary.

The `--data <dir>` flag layers an external directory on top of the embedded
catalog. The embedded catalog is precedence-low; your directory is
precedence-high. Adding a file under the right path either supplements (for
catalog content) or overrides (for component files) the embedded equivalent.

## Use cases

| Need | How `--data` helps |
|---|---|
| Add a non-public Kubernetes service (`service=ncp-internal`) | Drop an overlay declaring that criteria value; the criteria registry admits it. |
| Add a proprietary platform (`platform=runai`, `platform=nvmesh`) | Same — an overlay's `spec.criteria.platform` registers the value. |
| Add a future GPU SKU before AICR's next release | Add `accelerator: <name>` in an overlay; CLI / API admit it on the fly. |
| Add an internal component (e.g., an in-house operator) | Add a component definition under `components/` and reference it in an overlay or mixin. |
| Override an embedded chart version / values file | Drop a same-path file under your `--data` dir; external takes precedence. |
| Customize per-deployment scheduling without an overlay | Use `--config aicr-config.yaml` with `bundle.scheduling.*` — `--data` is for catalog content, not per-cluster ephemera. |

## Folder layout

The external directory mirrors AICR's embedded `recipes/` tree. Drop only the
paths you need; AICR loads any subset.

```text
my-external-data/
├── registry.yaml             # REQUIRED — your component definitions
├── mixins/                   # Optional — composable overlay fragments
│   └── platform-internal.yaml
├── overlays/                 # Optional — your overlays (flat or subdirs ok)
│   └── ncp-internal-h100-training.yaml
├── components/               # Optional — Helm values, raw manifests, etc.
│   └── my-internal-operator/
│       ├── values.yaml
│       └── manifests/
│           ├── namespace.yaml
│           └── rbac.yaml
└── catalog/                  # Optional — validator catalog overrides
    └── validators-extra.yaml
```

The loader walks the tree recursively (`filepath.WalkDir`), so subdirectories
inside `overlays/` are supported and useful for organizing by service / customer
/ team:

```text
overlays/
├── ncp-customer-a/
│   ├── h100-training.yaml
│   └── h100-inference.yaml
├── ncp-customer-b/
│   └── gb200-training.yaml
└── runai/
    └── h100-eks-training.yaml
```

## `registry.yaml` is required

Even if your directory only adds overlays (no new components), AICR requires a
`registry.yaml` at the root. The minimal stub is:

```yaml
apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components: []
```

External components in this file are merged with the embedded registry; on
name collision, the external definition wins.

## Adding a criteria value

Criteria value validation (`service`, `accelerator`, `intent`, `os`,
`platform`) is data-driven: the static OSS list is the fast path, and the
runtime *criteria registry* picks up any value declared in a loaded overlay's
`spec.criteria`. So **adding a new value to an overlay automatically makes it
a valid CLI / API input.** No code change, no rebuild.

Example overlay for an internal NCP:

```yaml
apiVersion: aicr.nvidia.com/v1alpha1
kind: RecipeMetadata
metadata:
  name: ncp-internal-h100-training
spec:
  base: base
  criteria:
    service: ncp-internal       # NEW — registers as a valid --service value
    accelerator: h100
    intent: training
  componentRefs:
    - { name: gpu-operator }
    - { name: my-internal-operator }
```

Run it:

```shell
aicr recipe \
  --service ncp-internal \
  --accelerator h100 \
  --intent training \
  --data ./my-external-data \
  --output recipe.yaml
```

Without `--data`, `--service ncp-internal` is rejected (the value isn't in
the embedded catalog and the registry hasn't been seeded). With `--data`
pointing at the overlay above, the registry registers `ncp-internal` at
catalog-load time and the CLI admits it.

The same applies to `accelerator`, `intent`, `os`, and `platform` — any
field on a `RecipeMetadata`'s `spec.criteria`.

## Adding a component

`registry.yaml` declares the component's identity and source:

```yaml
apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: my-internal-operator
    displayName: My Internal Operator
    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/my-internal-operator
      defaultVersion: v1.2.3
```

…or, for a Kustomize-shipped component:

```yaml
  - name: my-kustomize-app
    displayName: My Kustomize App
    kustomize:
      defaultSource: https://github.com/example/my-app
      defaultPath: deploy/production
      defaultTag: v1.0.0
```

A `values.yaml` (Helm) or `kustomization.yaml` (Kustomize) at
`components/<name>/` is picked up automatically.

Reference the component from an overlay's `componentRefs:` to include it in
recipes that match the overlay's criteria.

## Precedence rules

| Resource | Behavior |
|---|---|
| `registry.yaml` | **Merged**: embedded + external. On name collision, external wins. |
| Files in `components/`, `mixins/`, `overlays/`, `catalog/` | **Replaced**: any external file at the same relative path completely replaces the embedded equivalent. No partial-content merge. |

When in doubt, `aicr --debug recipe ... --data <dir>` logs the resolved source
(`embedded` / `external` / `merged`) for every loaded file.

## Strict mode — gating the OSS catalog

`--criteria-strict` (or `AICR_CRITERIA_STRICT=1`, or
`spec.recipe.criteriaStrict: true` in `--config`) rejects any criteria value
not in the embedded OSS catalog, ignoring `--data` contributions entirely.

This is intended for **CI gates** in the OSS repo so the upstream catalog
cannot accidentally start depending on internal-only values during
development. Integrator workflows that legitimately need `--data`-supplied
values should leave it off.

```shell
# Internal: accepts ncp-internal because --data registered it.
aicr recipe --service ncp-internal --data ./internal -o /dev/null

# OSS CI: rejects ncp-internal even though --data registered it,
# because strict mode hides external contributions.
AICR_CRITERIA_STRICT=1 \
  aicr recipe --service ncp-internal --data ./internal -o /dev/null
# → error: invalid service type: ncp-internal
```

`make qualify` in the OSS repo runs unit tests with `AICR_CRITERIA_STRICT=1`
exported automatically.

## Verifying what loaded

Use `aicr --debug` to inspect external-data discovery and per-file source
resolution:

```shell
aicr --debug recipe --service eks --accelerator h100 --data ./my-external-data
```

Sample output (truncated):

```text
[cli] initializing external data provider: directory=./my-external-data
[cli] layered data provider initialized: external_dir=./my-external-data external_files=12
[cli] data provider set: generation=1
[cli] external data provider initialized successfully: directory=./my-external-data
[cli] building recipe from criteria: criteria=criteria(service=eks, accelerator=h100, intent=any, os=any)
[cli] recipe generation completed: output=stdout components=8 overlays=2
```

Tab-completion for `--service` / `--accelerator` / `--os` / `--intent` /
`--platform` reflects values from the registry at the moment the help text is
rendered. Run with `--data` early in the command line to populate it before
shell completion kicks in.

## Pinning your extension catalog

Treat your `--data` directory like any other artifact: tag it (git tag, OCI
tag, semver) and pin which AICR binary version it was tested against. The
overlay schema is the AICR YAML schema; bumping AICR may add new optional
fields but rarely changes existing ones, so backward compatibility is the
default — but check the AICR release notes when you upgrade the binary.

Typical organization patterns:

- **One repo per team / customer.** Each team owns its overlay catalog and
  releases it independently of AICR.
- **One central internal repo.** A single org-wide `--data` catalog with
  per-team subdirectories (`overlays/team-a/`, `overlays/team-b/`).
- **OCI distribution.** Package the directory into an OCI artifact and pull
  on demand; `aicr` itself doesn't care about source, only that the path
  contains a `registry.yaml` and the expected sub-tree.

## Related

- [Recipe Development](recipe-development.md) — overlay schema, criteria fields, mixins, base recipe
- [Data Architecture](../contributor/recipe.md) — internals of the layered data provider and criteria registry
- [CLI Reference](../user/cli-reference.md) — `--data`, `--criteria-strict`, `--debug` flag definitions
- [Component Catalog](../user/component-catalog.md) — embedded component list (the baseline you're extending)
- [Validator Extension](validator-extension.md) — adding custom validators (also via `--data`)
