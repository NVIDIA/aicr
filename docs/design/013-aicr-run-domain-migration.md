# ADR-013: Migrate Artifact API Domain to `aicr.run`

## Status

Proposed. Confirms the migration tracked by the epic in
[#1486](https://github.com/NVIDIA/aicr/issues/1486) and its sub-issues.

## Problem

Every AICR artifact and on-cluster object stamps the reverse-DNS domain
`aicr.nvidia.com` — in the artifact `apiVersion` group
(`aicr.nvidia.com/v1alpha1`), in Kubernetes label and annotation keys, and in
the `https://aicr.nvidia.com/...` URIs that name our in-toto predicate and SLSA
build types. The string appears in ~470 places across the tree.

Tying the artifact contract to `nvidia.com` has two costs:

- **Vendor coupling.** The domain binds an otherwise vendor-neutral artifact
  format to a single company's brand and DNS. For an open-source project that
  may want CNCF-style neutrality — and that should outlive any one
  organization's domain — this is the wrong long-term identity.
- **Verbosity.** `aicr.nvidia.com` repeats in every `apiVersion`, label key,
  and attestation URI; `aicr.run` is shorter and reads cleanly.

Changing the group is a **breaking change**: any persisted artifact pinning the
old `apiVersion` would otherwise be rejected by the compatibility gate from
[ADR-011](011-artifact-apiversion-policy.md). Per that ADR's evolution rule,
breaking changes belong to a controlled transition. Doing this **before v1**,
while we are still on `v1alpha1`/`v1beta1`, is the cheapest possible moment —
there is no stable contract to honor yet, and the dual-accept machinery from
ADR-011 already exists to soften the cut.

The domain `aicr.run` is owned and controlled by the project maintainers, so
the `https://aicr.run/...` predicate and build-type URIs can resolve to real
documentation — a property the bare API group does not require but the URI
forms benefit from.

## Non-Goals

- **Bumping the version segment.** No schema change is being made; the version
  stays `v1alpha1` (artifacts) / `v1beta1` (build spec). Only the domain host
  changes. This is orthogonal to the ADR-011 evolution rule.
- **Reshaping attestation URI paths.** The path segments (`/bundle/v1`,
  `/recipe-evidence/v1`, `/recipe-catalog/v1`) are unchanged; only the host
  moves. A path or schema redesign is out of scope.
- **Changing the trust model.** The graded trust levels and the requirement
  that `verified` needs an NVIDIA-CI OIDC signing identity with no external
  data are unchanged by a host rename.
- **Re-signing or migrating already-published attestations.** Old attestations
  remain valid under their old predicate/build-type URIs. The only known
  external consumers are demo-only and are handled out of band by the
  maintainer.

## Decision

### 1. Adopt `aicr.run` as the artifact API domain

The reverse-DNS host changes from `aicr.nvidia.com` to `aicr.run` everywhere it
appears. Version segments and URI paths are untouched:

```
kind: RecipeResult
apiVersion: aicr.nvidia.com/v1alpha1   ──▶   apiVersion: aicr.run/v1alpha1
```

### 2. Single source of truth for the domain

Building on ADR-011's single-sourced `pkg/header`, introduce one domain knob and
derive every form from it:

```go
const Domain = "aicr.run" // single source of truth
const APIGroup = Domain    // aicr.run
// label/annotation keys:  Domain + "/job-type"
// attestation/build URIs:  "https://" + Domain + "/bundle/v1"
```

All currently-scattered literals (validator labels, bundler annotations,
attestation/provenance/build-type URIs, the build-spec `apiVersion`) are routed
through `header.Domain` so a future change is a one-line edit. This extends the
single-source-of-truth principle ADR-011 established for the artifact group to
the other four roles below.

### 3. Transition window (reuse ADR-011 §3–§4)

The cut uses the dual-accept mechanism ADR-011 already specifies for a version
bump, applied to the domain:

- `header.IsSupportedAPIVersion` accepts **both** `aicr.nvidia.com/v1alpha1`
  and `aicr.run/v1alpha1` (and the `v1beta1` build-spec equivalents) for one
  release.
- Artifact loaders accept either group on **read** and emit a single
  deprecation `slog.Warn` when the old group is seen.
- Writers always emit the new `aicr.run` group.
- The old group is retired only after the deprecation window closes, with a
  regression test asserting an out-of-window version is rejected.

### 4. Roles affected and their blast radius

The domain plays five semantic roles; the migration touches all of them, but
they differ in risk:

| Role | Examples | Risk |
|------|----------|------|
| Artifact API group | `header.APIGroup`, `pkg/build/spec.go` (`v1beta1`), `localformat/provenance.go` | Low — read fresh each run, already centralized |
| K8s label keys | `pkg/validator/labels` `aicr.nvidia.com/{job-type,run-id,…}` | Low — validation Jobs are created and torn down per run |
| Annotations | `bundler.go` `/gpu-operator-chart-version`, `validate.go` `/job` | Low — regenerated per bundle |
| Attestation predicate / build-type URIs | `https://aicr.nvidia.com/{bundle,recipe-evidence,recipe-catalog}/v1` | **High** — embedded in signed, published attestations |
| UUIDv5 namespace seed | `aicrBundleNamespace` derived from `https://aicr.nvidia.com/bundle/v1` | **High** — deterministic bundle IDs change |

## Consequences

### Positive

- The artifact contract is vendor-neutral and resolvable at a maintainer-owned
  domain.
- The domain literal exists exactly once (`header.Domain`); any future change is
  a one-line edit plus regenerated fixtures.
- Done pre-v1 with a transition window, the break is soft: old snapshots,
  recipes, and configs still load for one release.

### Negative

- **Deterministic bundle IDs reset.** The `aicrBundleNamespace` UUIDv5 seed
  moves to `https://aicr.run/bundle/v1`, so deterministic-mode bundle IDs
  change. This is a one-time, documented reset, not ongoing churn.
- **New predicate/build-type URIs.** Attestations produced after the cut carry
  `aicr.run` URIs; consumers pinned to the old URIs must add the new ones. The
  only known consumers are demo-only and handled offline.
- Signed-artifact goldens (attestation, BOM) and YAML/OpenAPI fixtures must be
  regenerated in lockstep with the flip.

### Neutral / Future direction

- ADR-011 remains the governing policy for `apiVersion` evolution; this ADR
  only changes the host value it single-sources. ADR-011's prose references to
  `aicr.nvidia.com` are updated as part of the docs sub-issue.
- The transition window closes in a later release; retiring the old group is a
  one-line removal plus a rejection test.

## Adoption plan

Tracked under epic [#1486](https://github.com/NVIDIA/aicr/issues/1486):

1. Centralize the domain into `header.Domain` (refactor only, value unchanged).
2. Flip the value to `aicr.run`; regenerate non-signed artifacts (apiVersion,
   labels/annotations, OpenAPI, fixtures).
3. Add the dual-accept transition window.
4. Migrate attestation/provenance URI hosts; regenerate signed artifacts and
   document the UUIDv5 namespace reset.
5. Update docs and this ADR series.
