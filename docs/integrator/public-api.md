# Public API Surface

AICR is both a CLI and a Go library. This page documents the
stability contract for every exported Go package. External consumers
should prefer the `github.com/NVIDIA/aicr/pkg/aicr` facade described
in the [Go library integration guide](./go-library.md).

## Stability tiers

| Tier | Meaning |
|------|---------|
| **Public (stable)** | Covered by semver; breaking changes only in major bumps. |
| **Public (evolving)** | Exported today but may change in minor bumps. Pin and audit on upgrade. |
| **Internal** | Treated as implementation detail. May change without notice. |

## Package matrix

| Package | Tier | Purpose |
|---------|------|---------|
| `github.com/NVIDIA/aicr/pkg/aicr` | **Public (stable)** | Facade: `Client`, `NewClient`, request/result types, source constructors. |
| `pkg/recipe` | Public (evolving) | Recipe resolution, criteria, overlay system, component registry. |
| `pkg/bundler` | Public (evolving) | Per-component Helm/Kustomize bundle generation. |
| `pkg/validator` | Public (evolving) | Constraint evaluation, three-phase validation (Deployment, Performance, Conformance). |
| `pkg/collector` | Public (evolving) | Observed state collection from clusters. |
| `pkg/measurement` | Public (evolving) | Typed measurement model used by collectors and validators. |
| `pkg/version` | Public (evolving) | Semver constraint evaluation. |
| `pkg/errors` | Public (evolving) | Structured errors with error codes. Consumed at API boundaries. |
| `pkg/defaults` | Public (evolving) | Shared timeout and limit constants. |
| `pkg/component` | Internal | Bundler utilities and test helpers. |
| `pkg/constraints` | Internal | Constraint type definitions. |
| `pkg/snapshotter` | Public (evolving) | Snapshot orchestration. `Snapshot` and `AgentConfig` are aliased into the facade — see [Facade type aliases](#facade-type-aliases) below. |
| `pkg/serializer` | Internal | YAML/JSON serialization helpers. |
| `pkg/manifest` | Internal | Helm-compatible manifest rendering. |
| `pkg/evidence` | Internal | Conformance evidence capture. |
| `pkg/trust` | Internal | Sigstore / provenance integration. |
| `pkg/k8s` | Internal | Kubernetes client utilities. |
| `pkg/oci` | Internal | OCI registry helpers. |
| `pkg/logging` | Internal | Logging setup. |
| `pkg/header` | Internal | HTTP header helpers. |
| `pkg/build` | Internal | Build-time metadata. |
| `pkg/server` | Internal | HTTP API server implementation. |
| `pkg/api` | Internal | Server-side REST handlers (server-internal; consumers use the HTTP API, not the Go types). |
| `pkg/cli` | Internal | CLI command implementations. |

## Facade type aliases

The `github.com/NVIDIA/aicr/pkg/aicr` package is Public (stable), but a
handful of its types are transparent re-exports of types from
Public (evolving) packages:

| Facade symbol | Source | Source tier |
|---|---|---|
| `aicr.Snapshot` | `pkg/snapshotter.Snapshot` | Public (evolving) |
| `aicr.AgentConfig` | `pkg/snapshotter.AgentConfig` | Public (evolving) |
| `aicr.PhaseResult` | `pkg/validator.PhaseResult` | Public (evolving) |
| `aicr.Phase` | `pkg/validator.Phase` | Public (evolving) |
| `aicr.PhaseDeployment` / `PhasePerformance` / `PhaseConformance` | `pkg/validator.Phase*` | Public (evolving) |

These symbols inherit their source package's stability rather than the
facade's. Field-shape changes in `pkg/snapshotter.Snapshot` (or any of
the other backing types) propagate to facade consumers without notice.
Pin AICR to a patch version and audit minor-bump diffs when upgrading
if you use any of these symbols directly.

`aicr.ValidateOption` is **not** in this list — it's a facade-owned
functional-option type that captures into an internal struct and
translates to `pkg/validator` options at call time, insulating the
facade contract from changes to `pkg/validator.Option`.

Replacing the remaining aliases with facade-owned wrappers (so they
fully inherit the facade's stable tier) is tracked in
[NVIDIA/aicr#1078](https://github.com/NVIDIA/aicr/issues/1078).

## Recommended consumption pattern

1. Use `github.com/NVIDIA/aicr/pkg/aicr` for all library integration by default.
2. If the facade does not yet expose a feature you need, open an issue
   against [NVIDIA/aicr](https://github.com/NVIDIA/aicr) describing the
   missing capability — we'd rather extend the facade than have
   external consumers hard-couple to evolving subpackages.
3. If you must import a `Public (evolving)` subpackage, pin AICR to a
   patch version and audit diffs when upgrading.
4. Never import a package marked `Internal` — upgrades will break you.

## See also

- [Go library integration guide](./go-library.md)
