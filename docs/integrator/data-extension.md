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
└── validators/               # Optional — validator catalog overrides
    └── catalog.yaml           # merged by validator name with the embedded catalog
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
apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components: []
```

External components in this file are merged with the embedded registry; on
name collision, the external definition wins. An entry replaces its embedded
counterpart **wholesale** — to override just `helm.defaultVersion`, copy the
full embedded entry and change that one field, or the other coordinates
(repository, chart, scheduling paths) are silently dropped.

Overriding a component's `defaultVersion` this way takes effect for every
embedded overlay that references the component: embedded overlays do not pin
versions that equal the registry default, so resolution falls back to your
merged default. The exception is an overlay with an explicit, intentionally
divergent pin — an explicit pin always wins over the registry default at
resolution. (The `versionPinExemptions` list is CI policy for recipes
embedded in the AICR repo, not a runtime input: the guard test that
consumes it never walks `--data` trees, so your external overlays may pin
freely without declaring anything or rebuilding AICR.) See issue
[#1616](https://github.com/NVIDIA/aicr/issues/1616) and
[Chart Version Pinning](recipe-development.md#chart-version-pinning).

## Catalog and binary compatibility

AICR validates every catalog header before it is used, so a catalog authored
against a binary you are not running fails loudly instead of resolving something
plausible and wrong. The accepted values per kind, and the releases in which
they change, are defined by
[ADR-022](https://github.com/NVIDIA/aicr/blob/main/docs/design/022-artifact-maturity-and-deprecation.md).

Each catalog kind has a current value and a target value:

| Kind | Current | Target |
|---|---|---|
| `ComponentRegistry` | `aicr.run/v1alpha2` | `aicr.run/v1beta1` |
| `RecipeMetadata`, `RecipeMixin` (ordinary) | `aicr.run/v1alpha2` | `aicr.run/v1beta1` |
| `RecipeMetadata` (profile-bearing) | `aicr.run/v1alpha3` | `aicr.run/v1beta2` |
| `AICRConfig` (not a catalog file; see [CLI config](../user/cli-config.md)) | `aicr.run/v1alpha2` | `aicr.run/v1beta1` |

Which binary accepts which catalog:

| AICR release | Accepts | Writes and documents |
|---|---|---|
| v0.20 and earlier | anything — catalog headers were ungated | current |
| v0.21 | current and target | current |
| v0.22 | current and target | target |
| v0.23 and later | target only | target |

**v0.20 and earlier cannot tell you whether your catalog is compatible.** Those
releases did not gate catalog headers at all: an external `registry.yaml` whose
`apiVersion` differed was merged anyway and restamped with the embedded value,
and `RecipeMetadata` and `RecipeMixin` headers were never checked. So a
target-stamped catalog does not fail on them — it loads silently and may resolve
something you did not intend. Closing that fail-open behavior is what ADR-022 §8
did in v0.21 (issue
[#1812](https://github.com/NVIDIA/aicr/issues/1812)). Treat v0.20 as unable to
validate your catalog rather than as a compatibility floor you can rely on.

Switch to the **target** value before v0.23, which stops accepting the current
one. Catalogs are authored inputs, so this is a manual edit in your tree. AICR
does not rewrite them, and there is no conversion layer.

Empty, unknown, or wrong-kind AICR catalog headers fail with `INVALID_REQUEST`
naming the value observed, the values expected for that kind, and the
remediation. AICR validates the raw external `registry.yaml` header **before**
merging it with the embedded registry, and checks metadata and mixin headers
before hydration, so an unaccepted external header is never replaced by an
embedded one and silently carried forward. Unrelated YAML in the tree keeps its
existing skip behavior, and `ValidatorCatalog` sits on a separate API domain
outside this contract.

This gate follows the document, not the entry point. Passing a single overlay
directly — `aicr bundle -r overlay.yaml`, `aicr validate -r overlay.yaml` —
applies the same check as a `--data` catalog scan, so a `RecipeMetadata`
with a missing or empty `apiVersion` is rejected on both paths. Through v0.20
the direct path accepted it and hydrated silently
([#2421](https://github.com/NVIDIA/aicr/issues/2421)); if you author overlays
outside a catalog tree, confirm each one carries a header. The empty-value
tolerance that remains is for hydrated `RecipeResult` inputs only, and it
retires in v0.23.

## Adding a criteria value

Criteria value validation (`service`, `accelerator`, `intent`, `os`,
`platform`) is data-driven: the static OSS list is the fast path, and the
runtime *criteria registry* picks up any value declared in a loaded overlay's
`spec.criteria`. So **adding a new value to an overlay automatically makes it
a valid CLI / API input.** No code change, no rebuild.

Example overlay for an internal NCP:

```yaml
apiVersion: aicr.run/v1alpha2
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

**Validating a recipe with a new criteria value.** Most validation checks
apply to an external recipe as-is: the deployment and conformance phases gate
on component presence and cluster state, not criteria. The NCCL performance
benchmarks additionally key their default applicability to embedded
`service` + `accelerator` pairs, so a recipe with a new **service or
accelerator** value would skip them (new `intent`, `os`, or `platform` values
alone do not affect NCCL applicability) — declare an `nccl-benchmark-profile`
performance constraint (e.g. `gb200/eks`) in the overlay's `validation` block
to opt into one of the embedded benchmarks. The profile selects the benchmark
template and fabric handling; node identification still follows the recipe's
own accelerator. See
[Opting external recipes into a benchmark profile](../user/validation.md#opting-external-recipes-into-a-benchmark-profile).
If the private service's fabric matches none of the embedded templates, ship the
benchmark itself: put a Kubeflow `TrainingRuntime` in your `--data` tree at
`validators/performance/testdata/{accelerator}/{service}/runtime.yaml` and point
the `nccl-benchmark-runtime-ref` constraint at it (`{accelerator}/{service}`).
Pass the same `--data <dir>` to `aicr validate` so it can resolve the referenced
file; validation then gates it keyed on the recipe's own criteria with no
compiled entry required, and the file is a drop-in should the runtime ever be
upstreamed.
See [Supplying a benchmark runtime for a private service](../user/validation.md#supplying-a-benchmark-runtime-for-a-private-service).

## Adding a component

`registry.yaml` declares the component's identity and source:

```yaml
apiVersion: aicr.run/v1alpha2
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

Component values are **not** auto-discovered by filename. For a Helm
component, a values file under `components/<name>/` is consumed only when
an overlay's `componentRef` names it via a
`valuesFile:` path relative to the data directory (a `componentRef` — and thus
`valuesFile` — lives on an overlay, not on the `registry.yaml` entry); the merge order is base
values → `valuesFile` → inline `overrides`. For a Kustomize component, the
deployable source is built from the registry entry's `defaultSource` /
`defaultPath` / `defaultTag` (overridable per `componentRef`) — there is no
implicit `kustomization.yaml` pickup.

Reference the component from an overlay's `componentRefs:` to include it in
recipes that match the overlay's criteria.

**Helm components must resolve with an effective chart version.** Declare
`helm.defaultVersion` in the registry entry (preferred — it keeps the version
in one place, matching the embedded catalog's convention; see
[Chart Version Pinning](recipe-development.md#chart-version-pinning)), or pin
`version:` on every `componentRef` that references the component. A Helm `componentRef` that
resolves without one is rejected at recipe resolution (`INVALID_REQUEST`)
rather than passed through — several deployers would otherwise emit the empty
version verbatim and Helm would silently install "latest" at deploy time.
Whitespace-only versions and a bare `v` count as absent — Flux and Argo CD
strip a leading `v` for non-OCI outputs (Helm resolves the empty remainder as
"latest"), non-vendored Helm/Helmfile and OCI outputs preserve it, and
vendored wrappers substitute a fabricated default; a bare `v` is rejected
uniformly to avoid output-dependent chart identities. Also,
`chart`, `source`, and `version` values carrying surrounding whitespace are
rejected outright, since deployers consume those fields verbatim. Manifest-only Helm components are exempt: a ref
whose chart and source are both empty and that ships at least one primary
`manifestFiles` entry has no chart version to pin. `preManifestFiles` alone
do not qualify — pre-manifests are auxiliary to a primary release, so a ref
with only pre-manifests is rejected as having no deployable primary.

## Precedence rules

| Resource | Behavior |
|---|---|
| `registry.yaml` | **Merged**: embedded + external component lists. On name collision, external wins. |
| `validators/catalog.yaml` | **Merged**: embedded + external validator lists, by validator name. A same-named external validator replaces the embedded one; new validators are appended. |
| Files in `components/`, `mixins/`, `overlays/` | **Replaced**: any external file at the same relative path completely replaces the embedded equivalent. No partial-content merge. |

When in doubt, `aicr --debug recipe ... --data <dir>` logs the resolved source
(`embedded` / `external` / `merged`) for every loaded file.

## Converting a family to a configuration profile

`recipes/overlays/aks.yaml` declares the `gpuStack` configuration profile
(`azure-managed` default, `operator-managed` alternative) over the GPU driver/toolkit
ownership paths, and `recipes/overlays/gke-cos.yaml` declares its own `gpuStack`
(`gke-default` default, `bundle-installer` alternative) over device-plugin ownership —
the GKE default value (`gke-default`) additionally declares
`advertiser: external`, and both GKE values trigger the #1327
allocation-policy closure, so their effective lock set is larger than the
declared paths (see
[Configuration Profiles](recipe-development.md#configuration-profiles)).
When an external data directory replaces a declaring overlay, or converts a
family to a profile, the replacement rules above interact with the profile
mechanics:

- **A same-path replacement replaces the declaration too.** An external
  `overlays/aks.yaml` completely replaces the embedded file — including its
  `spec.profile` block. Keep the declaration in the replacement — dropping it
  while keeping profile apiVersion `aicr.run/v1alpha3` or
  `aicr.run/v1beta2` fails catalog validation
  (the version⟺declaration cross-check). That guardrail protects an
  integrator *editing* a profile-track file: de-profiling one requires BOTH
  removing the declaration AND downgrading the overlay to the legacy
  apiVersion. It does NOT protect the upgrade path — a pre-existing legacy
  (`aicr.run/v1alpha2` or `aicr.run/v1beta1`) external `overlays/aks.yaml`, authored before the
  family's conversion, already satisfies both conditions. Upgrading AICR
  with such a catalog in `--data` silently preserves the unprofiled family:
  resolution succeeds with no error, no `selectedProfile` on the recipe, and
  no `K8s.aks-gpu-pools.gpu-driver` constraint. **On upgrade, diff each
  external overlay against its embedded counterpart** and port the
  `spec.profile` declaration plus the `apiVersion` bump (or consciously keep
  the fork unprofiled). A load-time detection of an external overlay
  shadowing an embedded profile declaration (with an explicit opt-out) is a
  candidate follow-up; it is not shipped.
- **Static assignments to now-owned paths are superseded.** Descendant or
  external overlays that statically assign profile-owned paths (for the AKS
  declaration: `gpu-operator` `driver.enabled`, `operator.runtimeClass`,
  `toolkit.enabled`; `nvidia-dra-driver-gpu` `nvidiaDriverRoot`) are
  overwritten by the selected value's fragment at resolution. The `enabled`
  entries in `ownedPaths` are different: they are the SYNTHETIC
  component-presence locks — fragments are forbidden from assigning
  `enabled`, and the lock instead rejects removing (or bundle-subsetting
  away) an owned component. Review those overlays when converting a family — a
  static assignment that used to take effect no longer does.
- **Owned paths lock per surface.** The enforcement matrix:
  1. *`aicr bundle` static overrides* (ANY static source — `--set`,
     `--set-json`, `--set-file`, or a config-file override) on an
     ordinary owned value path (e.g. `driver.enabled`,
     `operator.runtimeClass`): a divergent value fails closed; an
     identical value is accepted. For the synthetic `enabled` presence
     key, typed sources (`--set-json`, `--set-file`) are ALWAYS rejected
     — even when the value is identical to the selected one — because
     routing the toggle through a typed flag would write a stray literal
     `enabled:` chart value instead of toggling the component
     (`pkg/cli/bundle_config.go`). `aicr mirror list` exposes only the
     repeatable scalar `--set` (no typed flags), with the same
     identical-accepted / divergent-rejected rule; it does not apply a
     config file's `spec.bundle.deployment.set` overrides
     (`pkg/cli/mirror.go`).
  2. *`--dynamic` exports*: rejected on mere INTERSECTION with an owned
     path, regardless of value — install-time mutability of a locked path
     is itself the violation.
  3. *argocd-helm install-time values*: ANY install-time key whose path
     equals, contains, or is contained by an owned path fails closed at
     Helm render time, even when the value is identical — key presence
     alone trips the guard
     (`pkg/bundler/deployer/argocdhelm/argocdhelm.go`).
  4. *Component presence (the synthetic `enabled` owned path)*: not
     changeable by profile reselection — fragments cannot assign
     `enabled`, so no `--profile` choice adds or removes a component;
     the lock rejects removing (or bundle-subsetting away) an owned
     component. Presence changes are catalog/composition changes.
     Reselecting a profile changes owned VALUE paths only.
- **Selection is explicit.** Pick a value with `aicr recipe --profile
  name=value` (or `spec.recipe.profile` in `--config`); omission applies the
  declared default.
- **Evidence identity gains a profile path segment.** A profiled recipe's
  evidence identity appends a lowercase `-<name>-<value>` segment, so
  per-value evidence lands in distinct directories rather than colliding on
  the criteria-only path.

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
