# Public API Surface

AICR is both a CLI and a Go library. This page documents the
stability contract for every exported Go package. External consumers
should prefer the `github.com/NVIDIA/aicr/pkg/client/v1` facade described
in the [Go library integration guide](./go-library.md).

## Stability tiers

| Tier | Meaning |
|------|---------|
| **Public (stable)** | Compatibility-reviewed facade. During v0, breaks are detected and explicitly recorded; starting with v1.0, breaking changes require a major bump. |
| **Public (evolving)** | Exported today but may change in minor bumps. Pin and audit on upgrade. |
| **Internal** | Treated as implementation detail. May change without notice. |

## Package matrix

| Package | Tier | Purpose |
|---------|------|---------|
| `github.com/NVIDIA/aicr/pkg/client/v1` | **Public (stable)** | Facade: `Client`, `NewClient`, `NewClientContext`, request/result types, source constructors. |
| `pkg/recipe` | Public (evolving) | Recipe resolution, criteria, overlay system, component registry. |
| `pkg/bundler` | Public (evolving) | Per-component Helm/Kustomize bundle generation. |
| `pkg/validator` | Public (evolving) | Constraint evaluation, three-phase validation (executed in order: Deployment, Conformance, Performance). |
| `pkg/collector` | Public (evolving) | Observed state collection from clusters. |
| `pkg/measurement` | Public (evolving) | Typed measurement model used by collectors and validators. |
| `pkg/version` | Public (evolving) | Semver constraint evaluation. |
| `pkg/errors` | Public (evolving) | Structured errors with error codes. Consumed at API boundaries. |
| `pkg/defaults` | Public (evolving) | Shared timeout and limit constants. |
| `pkg/component` | Internal | Bundler utilities and test helpers. |
| `pkg/constraints` | Internal | Constraint type definitions. |
| `pkg/bom` | Internal | Bill-of-materials / image inventory generation. |
| `pkg/config` | Internal | Config-file loading and flag/spec resolution. |
| `pkg/corroborate` | Internal | Cross-source corroboration of observed state. |
| `pkg/diff` | Internal | Structural snapshot comparison implementation. External consumers use `Client.DiffSnapshots` and `aicr.WriteSnapshotDiffTable`. |
| `pkg/fingerprint` | Internal | Cluster/provider fingerprint detection. |
| `pkg/health` | Internal | Health-check orchestration. |
| `pkg/helm` | Internal | Helm chart rendering helpers. |
| `pkg/mirror` | Internal | Chart/image mirroring to air-gapped registries. |
| `pkg/netutil` | Internal | Networking utilities. |
| `pkg/snapshotter` | Public (evolving) | Snapshot orchestration. The facade exposes its own `Snapshot` and `AgentConfig` types; `pkg/snapshotter` is the underlying implementation. |
| `pkg/serializer` | Internal | YAML/JSON serialization helpers. |
| `pkg/manifest` | Internal | Helm-compatible manifest rendering. |
| `pkg/evidence` | Internal | Conformance evidence capture. |
| `pkg/trust` | Internal | Sigstore / provenance integration. |
| `pkg/k8s` | Internal | Kubernetes client utilities. |
| `pkg/oci` | Internal | OCI registry helpers. |
| `pkg/logging` | Internal | Logging setup. |
| `pkg/header` | Internal | HTTP header helpers. |
| `pkg/server` | Internal | aicrd HTTP server: middleware chain and REST handlers (thin adapters over `pkg/client/v1`). Consumers use the HTTP API, not the Go types. |
| `pkg/cli` | Internal | CLI command implementations. |

## Facade type ownership

The `github.com/NVIDIA/aicr/pkg/client/v1` package is Public (stable). Types
reachable from this surface are either facade-owned structs or transparent
aliases — the table below documents which.

Transparent aliases extend that contract to their target types. They are kept
where identity and direct interoperability with an existing builder, interface,
result, or provider-scoped state are more valuable than a duplicative facade
wrapper. The API-diff gate scopes additional checks to the aliased definitions;
unrelated exports in their evolving packages remain free to change.

| Facade symbol | Translates to/from | Notes |
|---|---|---|
| `aicr.Snapshot` | `pkg/snapshotter.Snapshot` | **Facade-owned struct**. Public fields are identifying metadata; full measurement payload is preserved in an unexported field for round-trip through `ValidateState`. Obtain one from `Client.LoadSnapshot` (file, URL, or `cm://` ConfigMap) or `Client.CollectSnapshot` (live capture) — neither requires importing `pkg/snapshotter`. `aicr.WrapSnapshot` remains for the narrower case of lifting a `*snapshotter.Snapshot` you already hold from a direct `pkg/snapshotter` call. |
| `aicr.SnapshotDiff`, `aicr.SnapshotChange`, `aicr.SnapshotDiffSummary` | `pkg/diff` result shapes | **Facade-owned structs** returned by `Client.DiffSnapshots`. They preserve the CLI's JSON/YAML schema without exposing `pkg/diff` types. Drift is data (`SnapshotDiff.HasDrift`), while invalid or payload-less inputs and context cancellation are errors. |
| `aicr.SnapshotDiffOptions` | `Client.DiffSnapshots` input | **Facade-owned input struct** carrying optional baseline and target source labels. The labels are copied to output metadata and do not affect comparison semantics. |
| `aicr.SnapshotChangeKind` and its constants | `pkg/diff.ChangeKind` | **Facade-owned string enum** whose values describe added, removed, and modified readings. |
| `aicr.SnapshotChangeSeverity` and `aicr.SnapshotChangeSeverityInfo` | `pkg/diff.Severity` | **Facade-owned string enum** classifying change impact; informational is the currently defined severity. |
| `aicr.AgentConfig` | `pkg/snapshotter.AgentConfig` | **Facade-owned struct** covering the deployment-time agent fields. `Tolerations` keeps `k8s.io/api/core/v1.Toleration` since `k8s.io` is itself a stable contract. It does **not** mirror every `pkg/snapshotter.AgentConfig` field — the network-collector fields `ClusterConfigPath` and `DiscoverNetwork` are not surfaced on the facade type. `AKSGPUPoolsPath` **is** surfaced (controller-side pool projection input, required for AKS profile-qualified resolution from a collected snapshot). |
| `aicr.PhaseResult` | `pkg/validator.PhaseResult` | **Facade-owned struct**. Exposes `Summary` (CTRF counts) and `RawReport` (CTRF JSON bytes); `Report *ctrf.Report` is retained for in-tree consumers that merge per-phase reports. |
| `aicr.Phase`, `aicr.PhaseDeployment` / `PhasePerformance` / `PhaseConformance` | string consts | **Facade-owned**. Values match `pkg/validator/v1` constants verbatim for byte-identical wire round-trip. |
| `aicr.ReportSummary` | `pkg/validator/ctrf.Summary` | **Facade-owned struct** with the CTRF count fields. |
| `aicr.ValidateOption` | `pkg/validator.Option` | **Facade-owned** functional-option type that captures into an internal struct and translates at call time. |
| `aicr.RecipeResult` | `pkg/recipe.RecipeResult` | **Facade-owned struct** exposing `Name`, `Version`, `TranslatedAt`, `SelectedProfile` (the recorded ADR-015 selection, nil for unprofiled recipes), `Components` (enabled/deployable components only — disabled refs remain visible via `Resolved().ComponentRefs`), and `RelaxedDimensions` (criteria dimensions cleared by `WithSnapshotCriteriaRelaxation`; non-empty only when that option was passed, the first attempt failed the coverage post-condition on derived dimensions, **and** the retry succeeded — a refused or failed retry returns `nil, error` with no `RecipeResult`, so this field never signals failure). Call `Resolved()` for the full upstream `*pkg/recipe.RecipeResult` (constraints, deployment order, validation config, metadata). The previous `aicr.Recipe` alias was removed in #1115; `ResolveRecipeFromCriteria` and `ResolveRecipeFromSnapshot` now return `*RecipeResult`. |
| `aicr.AllowLists` | `pkg/recipe.AllowLists` | **Facade-owned struct** with `[]string` fields (Accelerators / Services / Intents / OSTypes). Use `aicr.WrapAllowLists` to lift a `*pkg/recipe.AllowLists`. |
| `aicr.Criteria` | `pkg/recipe.Criteria` | **Facade-owned struct** whose enum-typed fields (Service / Accelerator / Intent / OS / Platform) project to plain strings; Nodes stays an `int` per the facade's string/int contract. Use `aicr.WrapCriteria` to lift a `*pkg/recipe.Criteria`. |
| `aicr.BundleConfig` | `pkg/bundler/config.Config` | Deliberate transparent alias. It keeps the facade compatible with `config.NewConfig` and its functional options instead of duplicating the bundler's configuration builder. |
| `aicr.BundleAttester` | `pkg/bundler/attestation.Attester` | Deliberate transparent alias. Existing attester implementations pass directly into `BundleOptions` without an adapter. |
| `aicr.BundleArtifact` | `*pkg/bundler/result.Output` | Deliberate transparent alias. Callers receive the complete bundler result, including `HasErrors`, without a lossy projection. |
| `aicr.OIDCResolveOptions` | `pkg/bundler/attestation.ResolveOptions` | Deliberate transparent alias. CLI and server callers can pass the same late-bound signing inputs used by the attestation resolver. |
| `aicr.CriteriaRegistry` | `pkg/recipe.CriteriaRegistry` | Documented transparent alias. Kept as an alias intentionally because the registry is behavior-rich (`ParseService`, `SetStrict`, `Values`, ...) and carries mutable per-`DataProvider` state — wrapping would either break the per-Client identity coupling (copy) or add no isolation win over the alias (pointer). |
| `aicr.BundleVerifyReport` | `pkg/bundler/verifier.VerifyResult` | Deliberate transparent alias. Callers receive the verifier's complete report (`TrustLevel`, `TrustReason`, `Errors`, per-stage booleans) rather than a projection that would have to grow with every new check. |
| `aicr.EvidenceVerification` | `pkg/evidence/verifier.VerifyResult` | Deliberate transparent alias, for the same reason, and so `aicr.RenderEvidenceJSON` / `RenderEvidenceMarkdown` render the identical document `aicr evidence verify` emits. |
| `aicr.Config` | `pkg/config.AICRConfig` | **Facade-owned wrapper**, not an alias: Go cannot attach methods to another package's type through an alias, and the config document's ~30 nested types would otherwise freeze under the API-diff gate. Obtain one from `aicr.LoadConfig` (file or HTTP(S) URL) or `aicr.WrapConfig`. Its methods DERIVE options (`BundleVerifyOptions`, `RecipeSource`, `RecipeCriteria`, `RecipeResolveOptions`, ...) rather than applying them, so caller overrides stay explicit; `Unwrap()` reaches the raw document for fields the facade does not project. |
| `aicr.CriteriaDimension`, `aicr.DimensionService` / `DimensionAccelerator` / `DimensionIntent` / `DimensionOS` / `DimensionPlatform` | string consts | **Facade-owned.** The criteria dimensions subject to the coverage post-condition, and the values `WithSnapshotCriteriaRelaxation` accepts. Values match `pkg/recipe.CoverageDimensionNames` exactly, which a test asserts. `nodes` is absent: no overlay gates on it. |

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
