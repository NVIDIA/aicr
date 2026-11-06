# ADR-030: Artifact Inventory and Catalog Bindings

## Status

**Proposed** — 2026-11-06. Requires the Terraform and Fleet GitRepo chart
archive verification fix in [#3653](https://github.com/NVIDIA/aicr/issues/3653)
before claiming verification across vendoring deployers. This ADR describes
proposed behavior; it does not introduce an implemented command or schema.

## Decision Summary

The embedded catalog may publish `artifacts/bindings.yaml`; a `--data`
catalog can add or override bindings by artifact name. Checks activate only
when the user supplies an inventory, through `artifacts/inventory.yaml` in
`--data` or the existing config file. AICR applies matching bindings in memory
during catalog/provider loading, before recipe resolution.
The producer lists release artifacts without knowing AICR component names.
The catalog author maps those artifacts to chart coordinates and image values.
The user controls an optional inventory path override and coverage exceptions
through the existing `AICRConfig`, under `spec.recipe.artifacts`.

Every selected Helm component needs a matched chart binding and matches for
all image bindings targeting it, unless the user lists that component in
`allowCatalogPins`. Bindings may cover more artifacts than the inventory;
unmatched bindings remain inactive. Recipe provenance records the
input digests and exceptions. There is no `aicr bind`, materialized catalog,
lock sidecar, new configuration kind, or new flag.

## Problem

A release changes chart versions, chart archive digests, image references and
image digests. Rewriting an integration catalog for each release mixes those
changes with component policy, values layout, scheduling and health checks.
Embedding a decoder for each producer's export format would couple AICR to
those producers and make their release formats part of AICR's maintenance.

A neutral inventory separates release identities from integration knowledge.
It must also prevent partial adoption: applying an inventory to a recipe while
silently retaining unrelated catalog pins would make the recipe's artifact
selection depend on omissions. Requiring the inventory to cover every catalog
component goes too far, because recipes select different parts of the catalog.
Coverage must be evaluated after recipe selection, with exceptions owned by
the user who chooses the inventory.

## Context

The following behavior was checked against origin `main` at
[`fe2618f4696b181a4936e8dcf1d0f013512c16e0`](https://github.com/NVIDIA/aicr/tree/fe2618f4696b181a4936e8dcf1d0f013512c16e0).
It describes the implementation this proposal extends. The cited code is
unchanged on origin `main` at
[`97e9c289a47429da5153eea185c25b0d0ce78c4e`](https://github.com/NVIDIA/aicr/tree/97e9c289a47429da5153eea185c25b0d0ce78c4e).

| Current behavior | Consequence for this design |
|---|---|
| `pkg/config` defines `AICRConfig` on the `aicr.run/v1beta1` authoring track, with `spec.recipe.data` and strict field decoding. [Config schema][config], [loader][config-loader] | Add user choices to that schema and its normal resolution path. |
| A filesystem provider requires an external `registry.yaml` and layers external data over embedded data. Matching external registry entries replace whole component entries. [Provider][provider], [merge][merge] | Binding preserves complete effective entries and their policy. It does not replace the embedded catalog. |
| The client constructs its provider before its recipe builder. [Client construction][client] | Compose before builder/provider caches are populated, through a shared functional path. |
| Registry defaults fill absent recipe coordinates. Values files participate according to the component ref's `valuesFile`; a filename alone does not enable loading. [Defaults][defaults], [values][values] | Reject conflicting explicit coordinates and require each image target file to participate in the selected component's values. |
| `helm.defaultDigest` identifies the chart `.tgz`, including for OCI charts. `ResolveChartDigests` returns a pin only when source, chart and version still match the registry. [Helm schema][helm-schema], [digest resolution][chart-digests] | Bind coordinates and archive digest together, then reject identity changes after composition. |
| `RecipeResultMetadata` has no artifact-binding record. Loading a saved result attaches the caller's provider. [Metadata][metadata], [recipe loader][recipe-loader] | Serialized provenance and checks on reload are new work, not guarantees of the existing provider association. |

## Goals

- The same catalog, inventory, bindings, user choices and AICR binary produce
  the same resolved pins.
- Every inventory artifact has an explicit binding or ignore reason.
- Every selected Helm component has matches for its chart and all declared
  image bindings, or a user-owned exception.
- Recipe, validation, mirror discovery and bundle generation consume the same
  composition and reject later changes to bound identities.
- AICR consumes one neutral contract without producer-specific types,
  decoders, dependencies, names or theming.

## Non-Goals

- Replacing the embedded catalog with an external catalog.
- Fetching an inventory from an OCI reference or adding artifact transport.
- Non-OCI inventory references, templating, arbitrary patches or scripts.
- Chart pins addressed by OCI manifest digest.
- Proving runtime image completeness, release compatibility or cluster health
  from an inventory.

## Decision

### Ownership and activation

An external catalog may carry bindings and a default inventory:

```text
release-catalog/
  registry.yaml
  components/...
  overlays/...
  artifacts/
    bindings.yaml    # Catalog author: integration targets and ignore reasons.
    inventory.yaml   # Producer: release artifact identities.
```

A release pipeline or mirror tool can produce the inventory without importing
AICR code. Any translation from its private export format happens outside
AICR. Bindings are the catalog's published contract: stable, descriptive
artifact names tell producers how to name inventory entries without exposing
component names or values paths in the inventory. Renaming a binding key is a
breaking change, versioned with the catalog. The catalog author owns bindings,
values contracts and component policy. The user chooses the inventory and any
components permitted to retain catalog pins.

Bindings are an ordinary layered catalog file. The embedded catalog may ship
`artifacts/bindings.yaml`, and an external catalog's file layers over it.
An external entry replaces the entire disposition for the same artifact name;
other embedded entries remain. Validate disjoint dispositions and at most one
chart binding per component on the merged set. There is no removal mechanism.

An inventory activates composition and coverage checks. Read it from
`<data>/artifacts/inventory.yaml` when present, or use
`spec.recipe.artifacts.inventory` when supplied. The explicit path also works
with only the embedded catalog; `--data` is not required. An explicitly named
missing file or an invalid inventory fails loading. Without an inventory,
bindings are ignored and catalog behavior is unchanged, whether either layer
has bindings or not.

If an inventory is supplied but neither layer has bindings, use an empty
binding set. Each inventory entry then fails ordinary accounting with
`no binding for X`; there is no separate missing-bindings special case.

### Composition and consumer checks

1. When an inventory is supplied, read bounded snapshots of it and the
   catalog's binding layers. Merge bindings and validate document shapes and
   inventory accounting before publishing a provider. Every inventory entry
   must be bound or explicitly ignored. An unmatched binding is inactive and
   leaves its target fields at catalog pins. Reads and later bundle capture
   must use the same snapshot.
2. Load the ordinary layered catalog. Validate targets only for active bindings:
   component existence, values-file participation, JSON Pointer existence and
   platform agreement. Inactive bindings receive schema/shape validation only;
   a stale target in an unused embedded binding must not break loading. Clone
   effective entries and update only active chart fields and image values.
   Preserve class, namespace, dependencies, scheduling, checks, manifests and
   other integration policy. Do not mutate shared caches.
3. For each bound chart, split its OCI reference at the final path component
   into `helm.defaultRepository` and `helm.defaultChart`. Set
   `helm.defaultVersion` from `version` and `helm.defaultDigest` from
   `archiveDigest` together. A missing archive digest fails; an old pin must
   never survive a coordinate change. Record `manifestDigest` as provenance
   only. No `helm.defaultManifestDigest` is introduced.
4. Resolve the recipe normally. Bindings neither add nor enable components.
   A selected Helm component is covered only when its chart binding and every
   image binding targeting it have inventory matches. Missing or partial
   coverage fails unless the user lists it in `allowCatalogPins`. Unselected
   components do not require coverage. An image binding alone cannot cover a
   chart, and a chart match alone cannot cover declared but unmatched images.
5. Apply a shared identity guard after final value composition on every
   consumer path. Explicit recipe coordinates, overlays, profiles and user
   overrides must agree with bound chart coordinates and image identities.
   Equal values are allowed; changes to a bound repository, version, chart,
   digest or required tag clearing fail. Dynamic declarations that overlap
   bound identity paths fail. An exception permits retaining catalog pins for
   unmatched bindings, including on a partially matched component. It does not
   suppress active bindings or disable their identity guards.
6. Record provenance in the recipe. Loading that recipe later requires matching
   inventory, bindings, base catalog and exceptions. Missing inputs or a
   mismatch fail before validation, discovery or bundle output.

The shared implementation belongs below the CLI, so SDK and server callers
receive the same checks. `recipe`, `validate`, `mirror list` and `bundle` must
all pass artifact configuration into provider construction and run the guard
where their final values are known. Recipe selection coverage cannot be
completed solely during provider loading.

A binding does not renew version-specific integration evidence. A changed
chart must not inherit an `ownsCRDs` audit for different chart coordinates or
archive bytes; the reviewed catalog must first name the incoming identity.
Health checks and upgrade guidance remain the catalog author's responsibility.

## Proposal

### Document rules

`ArtifactInventory` and `ArtifactBindings` use
`apiVersion: artifacts.aicr.run/v1alpha1`. This is a separate, explicitly
versioned contract; it does not add an `aicr.run/v1alpha1` track. Its parser
accepts only supported kind/version pairs. Implementation must register and
document those gates consistently with
[ADR-011](011-artifact-apiversion-policy.md) and
[ADR-022](022-artifact-maturity-and-deprecation.md).

Each file contains one UTF-8 YAML or JSON document with string mapping keys.
Reject unknown fields, duplicate keys, anchors, aliases, explicit YAML tags,
merge keys and unsupported versions. Optional fields are omitted rather than
set to null. Bound input sizes and nesting; the initial limits are 8 MiB per
inventory or bindings file, depth 32 and 10,000 inventory artifacts.

Names match `^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$` and contain at most 128
characters. Digests are `sha256:` followed by 64 lowercase hexadecimal digits.
References contain no credentials. Documents must contain no secret values.

### Inventory schema

| Field | Required | Contract |
|---|---|---|
| `apiVersion`, `kind` | Yes | `artifacts.aicr.run/v1alpha1`, `ArtifactInventory`. |
| `metadata.name` | No | Human-readable label using the name grammar. |
| `artifacts` | Yes | List of at most 10,000 artifacts. May be empty; order has no meaning. |
| `artifacts[].name` | Yes | Unique name, stable across releases. No component-name semantics. |
| `artifacts[].type` | Yes | `chart`, `image` or `other`; all are OCI-hosted. |
| `artifacts[].reference` | Yes | Lowercase `oci://host[:port]/repo/path`, without a tag, digest or userinfo. |
| `artifacts[].version` | Yes | Concrete SemVer without a `v` prefix for charts. An opaque label for images and other artifacts, never interpreted as a tag. |
| `artifacts[].manifestDigest` | Yes | Identity of the exact OCI manifest bytes, or image-index bytes. Recorded, not checked during loading. |
| `artifacts[].archiveDigest` | Bound charts | Identity of the exact chart `.tgz` blob. Allowed only for charts and mandatory for a chart binding. |
| `artifacts[].platform` | No | Image-only object with `os`, `architecture` and optional `variant`. Describes a single-platform manifest; omit for a multi-platform index. |

The inventory may be partial relative to the catalog. Every artifact it does
contain must be accounted for, including `other`, which can only be explicitly
ignored in this version.

```yaml
apiVersion: artifacts.aicr.run/v1alpha1
kind: ArtifactInventory
metadata:
  name: release-2.4.1
artifacts:
  - name: task-runner-chart
    type: chart
    reference: oci://registry.example.test/charts/task-runner
    version: "2.4.1"
    manifestDigest: sha256:1111111111111111111111111111111111111111111111111111111111111111
    archiveDigest: sha256:2222222222222222222222222222222222222222222222222222222222222222
  - name: task-runner-controller
    type: image
    reference: oci://registry.example.test/images/task-runner-controller
    version: "2.4.1"
    manifestDigest: sha256:3333333333333333333333333333333333333333333333333333333333333333
    platform:
      os: linux
      architecture: amd64
  - name: release-notes
    type: other
    reference: oci://registry.example.test/docs/release-notes
    version: "2.4.1"
    manifestDigest: sha256:4444444444444444444444444444444444444444444444444444444444444444
```

### Bindings schema

The document requires `apiVersion`, `kind`, `charts` and `images`.
`ignoredArtifacts` defaults to an empty map. The three maps use artifact names
as keys. Their key sets must be disjoint after layering, and together must
cover every inventory name. They may be a superset of the inventory. A binding
or ignore entry with no matching inventory entry is inactive. A type mismatch
or unaccounted inventory artifact fails. A misspelling on either side of an
intended match leaves the inventory entry unaccounted, preserving typo safety
without requiring per-release binding files.

The target requirements below apply only to active bindings. Inactive entries
must have valid schema and field shapes, but do not require target existence,
values-file participation, JSON Pointer existence or a platform match. The
merged set still obeys disjointness and one-chart-per-component rules.

| Field | Contract |
|---|---|
| `charts.<artifact>.component` | Existing Helm component. Artifact must be a chart. At most one chart per component and one component per chart binding. |
| `images.<artifact>.targets` | Nonempty list of targets for an image artifact. Duplicate or overlapping writes fail. |
| `targets[].component` | Existing Helm component; does not enable it. |
| `targets[].valuesFile` | Catalog-relative `.yaml`, `.yml` or `.json` regular file that participates in the component's values composition. Reject absolute paths, `..`, symlinks and catalog escapes. |
| `targets[].platform` | Optional assertion with the inventory platform shape. Must match exactly, including variant presence. Do not infer it from the machine running AICR. |
| `targets[].write` | Exactly one of `{reference}` or `{repository, digest, clearTag?}`. Values are JSON Pointers to existing string leaves. |
| `ignoredArtifacts.<artifact>.reason` | Nonempty reason for not binding the artifact. Does not exempt a component from recipe coverage. |

JSON Pointers use `~0` and `~1` escapes. All bindings require nonempty,
syntactically valid pointers. Active writes require existing object-map
parents and string leaves; reject array traversal, missing paths and equal or
ancestor/descendant overlaps, including tag-clearing targets.

The `reference` form writes the scheme-less repository followed by `@` and
`manifestDigest`. The split form writes the scheme-less repository and full
`sha256:` digest separately. Optional `clearTag` writes an empty string to the
named tag field. The catalog author must verify that the chart uses those
values as intended. Unsupported image-value layouts require a future typed
form, not arbitrary string templates.

```yaml
apiVersion: artifacts.aicr.run/v1alpha1
kind: ArtifactBindings
charts:
  task-runner-chart:
    component: task-runner
images:
  task-runner-controller:
    targets:
      - component: task-runner
        valuesFile: components/task-runner/values.yaml
        platform:
          os: linux
          architecture: amd64
        write:
          reference: /controller/image
ignoredArtifacts:
  release-notes:
    reason: Release documentation has no deployment target.
```

### AICRConfig additions

The following proposed block extends `pkg/config`'s existing schema. There
are no artifact-specific flags. Existing flag-over-config precedence still
applies where a flag exists, such as `--data`.

```yaml
apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  recipe:
    data: ./release-catalog
    artifacts:
      inventory: ./release-inventory.yaml
      allowCatalogPins: [cert-manager]
```

| Field | Default | Contract |
|---|---|---|
| `spec.recipe.artifacts.inventory` | `<data>/artifacts/inventory.yaml` when present | Optional local path, including with only the embedded catalog. Must identify an existing regular file, not a symlink. No OCI reference. Without either source, checks stay inactive. |
| `spec.recipe.artifacts.allowCatalogPins` | `[]` | Selected components the user permits to retain catalog pins for missing chart or image matches. Active bindings still apply. Reject unknown names and duplicates; record the effective set in provenance. |

A user can retain `cert-manager` from the catalog while supplying both
`task-runner-chart` and `task-runner-controller` for the selected `task-runner`.
Supplying only its chart leaves that component partially covered and fails
unless `task-runner` is also listed in `allowCatalogPins`; with that exception,
the chart binding applies and the controller keeps its catalog image pin.
Other unselected components do not need exceptions. Selecting another
uncovered Helm component makes resolution fail. An exception neither enables
a component nor authorizes a producer or catalog author to waive coverage on
the user's behalf.

### Recipe provenance additions

The proposed location is `RecipeResult.metadata.artifacts`. Final wire names
and the recipe schema version must be settled before implementation; an older
reader must reject an artifact-bearing recipe rather than discard its
association. This extends serialized metadata, not the existing in-process
provider pointer.

| Field under `metadata.artifacts` | Contract |
|---|---|
| `inventoryDigest` | SHA-256 of the exact inventory bytes consumed. |
| `bindingsDigest` | SHA-256 of the canonical merged bindings document, including inactive entries. |
| `baseCatalogDigest` | Digest of the external base catalog file inventory plus the embedded catalog identity, before binding. |
| `allowCatalogPins` | Sorted effective exception set from the user config. |
| `verification` | `metadata-only` at composition time. |

Compute `bindingsDigest` after external entries replace same-name embedded
entries. Encode the merged document as compact UTF-8 JSON with recursively
sorted object keys and no insignificant whitespace; preserve array order.
Include every effective chart, image and ignored-artifact entry. Per-layer
formatting or a shadowed entry does not change this digest. When neither layer
has bindings, hash the canonical empty document with empty `charts`, `images`
and `ignoredArtifacts` maps and the normal kind/version header.

For `baseCatalogDigest`, enumerate external base files excluding the
`artifacts/` directory, whose effective inputs have separate digests. Sort
relative paths and encode each entry as `[path, size, sha256]`. Hash a documented
canonical encoding of that list and the embedded catalog identity. The definition must
exclude mtimes and absolute paths and include every base file consumed.
Record the AICR version/build identity alongside this record using recipe
metadata conventions.

The provenance must also retain each applied artifact's name, reference,
version, manifest digest and, where applicable, archive digest or platform,
with its binding targets. Ignored artifacts retain their identities and
reasons. Thus a chart's manifest digest remains available for audit without
becoming an enforced chart pin. The exact representation of those records is
part of the recipe-schema work.

Recompute input digests on reload and compare the effective exception set.
Changing inventory formatting changes its digest even when pins are unchanged;
bindings use the canonical merged representation above. Bundle capture must
preserve the consumed inventory and merged bindings, including an inventory
read through the override, and propagate the recipe's provenance.
There is no generated lock file or resolved-catalog directory to reconcile.

## Alternatives considered

| Alternative | Disposition and reason |
|---|---|
| `aicr bind` materializes a resolved catalog and `binding-lock.json` | Rejected. The original design made the result easy to diff, but added a command, flags, duplicate configuration and a separate output directory that later commands had to select. Load-time composition uses the existing workflow; recipe provenance carries the association. |
| Standalone `ArtifactComposition` configuration kind | Rejected. It duplicates `AICRConfig` and creates a second place for user choices and precedence rules. |
| Coverage exceptions in catalog-author-owned bindings | Rejected. The user swapping pins owns the coverage decision. A catalog author or producer must not silently permit fallback on that user's behalf. `ignoredArtifacts` accounts for inventory entries and is not a coverage exemption. |
| Require a full-catalog inventory | Rejected. The catalog is sliced across recipes; requiring artifacts for unselected components couples independent releases. Check selected Helm components after resolution. |
| Exact bindings = inventory match | Rejected. It forces per-release bindings even when the catalog contract is unchanged. Superset bindings plus inventory-side accounting preserve typo safety. |
| Binding removal in overlays | Rejected. There is no identified case that entry replacement does not cover. Unmatched bindings are already inactive. |
| Producer-specific decoders inside AICR | Rejected. AICR would acquire producer dependencies and release-format compatibility obligations. Producers emit or translate to the neutral inventory outside AICR. |
| Manifest-digest chart pins or `helm.defaultManifestDigest` | Rejected for this proposal. The enforced chart identity remains the archive digest expected by existing vendoring. A chart manifest digest is recorded only; it cannot substitute for `.tgz` identity. |
| Full catalog replacement | Deferred as a separate feature. Dropping embedded data changes catalog composition and compatibility independently of artifact binding. This proposal preserves layered loading. |

## Consequences

Catalog policy and release artifacts can evolve separately, but bindings still
need maintenance when chart values change. A catalog can publish bindings for
its whole component set while a user supplies only the inventory needed by a
recipe. Inventory-side accounting catches misspelled matches; selected-component
coverage catches omitted chart or image entries. Coverage exceptions remain
visible in user configuration and saved recipe provenance. A future optional
helper could list the artifact names a recipe needs from the merged bindings;
this proposal does not add that command.

Older binaries can ignore `artifacts/` and silently use catalog pins. A
catalog intended for use with an inventory therefore needs a minimum AICR
version declaration, checked before composition. Adding a field alone cannot
make an already-shipped binary enforce it. Release design must pair that
declaration with a catalog schema/version that unsupported readers already
reject, or an equivalent
mandatory compatibility gate. The declaration's location, first supporting
release and compatibility matrix remain implementation prerequisites. The
same reject-unknown requirement applies to artifact-bearing saved recipes.

There is no materialized catalog diff. An optional future
`aicr catalog render --data …` could print the effective registry and values
for inspection using the same provider. It is a review aid, not a required
composition step or a lock-file workflow.

Loading artifact metadata fetches no chart or image bytes. Inventory hashes
record inputs, chart archive pins express expectations, and image values
select manifest/index identities. None of these verifies registry availability,
publisher authority or runtime image coverage. Existing catalog signatures and
validation evidence must not be presented as signatures or qualification of a
new composition.

Archive verification remains a vendoring responsibility. At the inspected
commit, Terraform and Fleet GitRepo construct `localformat.Component` without
`ChartDigest`; an empty expected digest skips verification.
[Terraform][terraform], [Fleet][fleet], [verification][verify]. Fix
[#3653](https://github.com/NVIDIA/aicr/issues/3653) before claiming archive
verification for all vendoring deployers. This prerequisite does not make
non-vendored installation verify an archive.

Mirror discovery must apply the identity guard. Its current render input has
no archive digest, so a version-fetched render does not prove that the pinned
archive was rendered. [Render input][render], [discovery][discovery].
Render failures and missing late-created workload images prevent a completeness
claim. A shared verified archive fetch/render path and runtime coverage
evidence need separate work.

## Adoption plan

1. Settle the minimum-version declaration and recipe schema gate, including
   rejection by unsupported readers. Fix the Terraform/Fleet prerequisite.
2. Add neutral strict parsers, in-memory provider composition and config
   plumbing. Preserve behavior whenever no inventory is supplied, including
   catalogs that publish bindings.
3. Add selected-recipe coverage, final-value guards, saved-recipe comparison
   and bundle provenance capture across CLI, SDK and server paths.
4. Qualify full-reference and split repository/digest image forms with real
   charts. Exercise embedded-only bindings, external same-name replacements,
   superset bindings, inactive stale targets, partial component coverage,
   missing active targets, explicit exceptions and changed inputs on reload.
   Check repeatability and unchanged behavior without an inventory. Verify
   mismatching chart archives are rejected by every supported vendoring path.

Before implementation, resolve how platform assertions are checked for mixed
architectures, the exact per-artifact provenance representation, and the
verified-render boundary for mirror discovery. Runtime image coverage and
relocation evidence remain separate from metadata composition.

## References

- [ADR-011: Artifact apiVersion policy](011-artifact-apiversion-policy.md)
- [ADR-022: Artifact maturity and deprecation](022-artifact-maturity-and-deprecation.md)
- [External data extension](../integrator/data-extension.md)
- [Terraform and Fleet GitRepo ChartDigest prerequisite](https://github.com/NVIDIA/aicr/issues/3653)

[config]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/config/config.go#L25
[config-loader]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/config/loader.go#L208
[provider]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/recipe/provider.go#L306
[merge]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/recipe/provider.go#L723
[client]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/client/v1/aicr.go#L362
[defaults]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/recipe/metadata.go#L270
[values]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/recipe/adapter.go#L328
[helm-schema]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/recipe/components.go#L342
[chart-digests]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/bundler/deployer/chartdigest.go#L35
[metadata]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/recipe/metadata.go#L1249
[recipe-loader]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/recipe/loader.go#L183
[terraform]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/bundler/deployer/terraform/terraform.go#L476
[fleet]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/bundler/deployer/fleet/fleet.go#L624
[verify]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/bundler/deployer/localformat/vendor.go#L124
[render]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/helm/render.go#L31
[discovery]: https://github.com/NVIDIA/aicr/blob/fe2618f4696b181a4936e8dcf1d0f013512c16e0/pkg/mirror/discover.go#L212
