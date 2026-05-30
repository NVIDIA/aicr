# Public API Surface

AICR is both a CLI and a Go library. This page documents the
stability contract for every exported Go package. External consumers
should prefer the `github.com/NVIDIA/aicr/pkg/client/v1` facade described
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
| `github.com/NVIDIA/aicr/pkg/client/v1` | **Public (stable)** | Facade: `Client`, `NewClient`, request/result types, source constructors. |
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
| `pkg/snapshotter` | Public (evolving) | Snapshot orchestration. The facade exposes its own `Snapshot` and `AgentConfig` types; `pkg/snapshotter` is the underlying implementation. |
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

## Facade type ownership

The `github.com/NVIDIA/aicr/pkg/client/v1` package is Public (stable). Types
reachable from this surface are either facade-owned structs or transparent
aliases — the table below documents which.

| Facade symbol | Translates to/from | Notes |
|---|---|---|
| `aicr.Snapshot` | `pkg/snapshotter.Snapshot` | **Facade-owned struct**. Public fields are identifying metadata; full measurement payload is preserved in an unexported field for round-trip through `ValidateState`. Use `aicr.WrapSnapshot` to lift a `*snapshotter.Snapshot` loaded externally. |
| `aicr.AgentConfig` | `pkg/snapshotter.AgentConfig` | **Facade-owned struct** mirroring every field. `Tolerations` keeps `k8s.io/api/core/v1.Toleration` since `k8s.io` is itself a stable contract. |
| `aicr.PhaseResult` | `pkg/validator.PhaseResult` | **Facade-owned struct**. Exposes `Summary` (CTRF counts) and `RawReport` (CTRF JSON bytes); `Report *ctrf.Report` is retained for in-tree consumers that merge per-phase reports. |
| `aicr.Phase`, `aicr.PhaseDeployment` / `PhasePerformance` / `PhaseConformance` | string consts | **Facade-owned**. Values match `pkg/validator/v1` constants verbatim for byte-identical wire round-trip. |
| `aicr.ReportSummary` | `pkg/validator/ctrf.Summary` | **Facade-owned struct** with the CTRF count fields. |
| `aicr.ValidateOption` | `pkg/validator.Option` | **Facade-owned** functional-option type that captures into an internal struct and translates at call time. |
| `aicr.RecipeResult` | `pkg/recipe.RecipeResult` | **Facade-owned struct** exposing `Name`, `Version`, `TranslatedAt`, and `Components`. Call `Resolved()` for the full upstream `*pkg/recipe.RecipeResult` (constraints, deployment order, validation config, metadata). The previous `aicr.Recipe` alias was removed in #1115; `ResolveRecipeFromCriteria` and `ResolveRecipeFromSnapshot` now return `*RecipeResult`. |
| `aicr.AllowLists` | `pkg/recipe.AllowLists` | **Facade-owned struct** with `[]string` fields (Accelerators / Services / Intents / OSTypes). Use `aicr.WrapAllowLists` to lift a `*pkg/recipe.AllowLists`. |
| `aicr.Criteria` | `pkg/recipe.Criteria` | **Facade-owned struct** whose enum-typed fields (Service / Accelerator / Intent / OS / Platform) project to plain strings; Nodes stays an `int` per the facade's string/int contract. Use `aicr.WrapCriteria` to lift a `*pkg/recipe.Criteria`. |
| `aicr.CriteriaRegistry` | `pkg/recipe.CriteriaRegistry` | Documented transparent alias. Kept as an alias intentionally because the registry is behavior-rich (`ParseService`, `SetStrict`, `Values`, ...) and carries mutable per-`DataProvider` state — wrapping would either break the per-Client identity coupling (copy) or add no isolation win over the alias (pointer). |

## Recommended consumption pattern

1. Use `github.com/NVIDIA/aicr/pkg/client/v1` for all library integration by default.
2. If the facade does not yet expose a feature you need, open an issue
   against [NVIDIA/aicr](https://github.com/NVIDIA/aicr) describing the
   missing capability — we'd rather extend the facade than have
   external consumers hard-couple to evolving subpackages.
3. If you must import a `Public (evolving)` subpackage, pin AICR to a
   patch version and audit diffs when upgrading.
4. Never import a package marked `Internal` — upgrades will break you.

## See also

- [Go library integration guide](./go-library.md)
