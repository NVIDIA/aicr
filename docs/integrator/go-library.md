# Using AICR as a Go library

AICR ships as both a CLI and a Go library. External projects that need
to resolve validated recipes, generate bundles, or collect observed
state can import AICR directly. This page is for those consumers.

## Which package to import

**Import the `github.com/NVIDIA/aicr/pkg/client/v1` package.** This is the
compatibility-reviewed facade and the surface AICR intends to stabilize at
v1.0.

```go
import aicr "github.com/NVIDIA/aicr/pkg/client/v1"
```

The facade provides a single `Client` type with constructors for the
supported recipe sources. Internally it delegates to the functional
packages under `pkg/*`.

You _may_ also import `pkg/*` subpackages directly, but their APIs are
not covered by the same stability guarantees — see the [public API
surface](./public-api.md) for the details.

## Installing

```bash
go get github.com/NVIDIA/aicr@latest
```

For reproducibility in downstream projects, pin a specific tag:

```bash
go get github.com/NVIDIA/aicr@v0.19.0
```

## Quick start

```go
package main

import (
	"context"
	"log"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

func main() {
	// FilesystemSource layers an external recipe directory over the
	// embedded recipe data. Use this in production today; OCISource
	// is reserved but not yet implemented (NewClient returns
	// ErrCodeUnavailable when given one — see the constructor's
	// godoc for the current state).
	client, err := aicr.NewClient(
		aicr.WithRecipeSource(
			aicr.FilesystemSource("/etc/aicr/recipes"),
		),
	)
	if err != nil {
		log.Fatal(err)
	}
	// Always Close when done — releases this Client's cached
	// metadata store and component registry from the recipe
	// package's per-DataProvider caches.
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := client.ResolveRecipe(ctx, aicr.RecipeRequest{
		Service:     "eks", // K8s flavour, not cloud vendor — map aws→eks etc. on your side
		Region:      "us-east-1",
		Accelerator: "h100",
		Nodes:       8, // worker-node count, not GPU count
		OS:          "ubuntu", // REQUIRED to reach the OS-pinned kubeflow overlay; see "Recipe sources" below
		Intent:      "training",
		Platform:    "kubeflow",
		// Profile:  "gpuStack=operator-managed", // only when the composition declares one (embedded adopter: AKS; values azure-managed [default] / operator-managed)
	})
	if err != nil {
		log.Fatalf("resolve recipe: %v", err)
	}

	log.Printf("resolved recipe %s (%d components)", result.Name, len(result.Components))
}
```

## Snapshotting and validation

Beyond recipe resolution, the facade exposes the rest of the
Snapshot → Validate workflow. Both methods are stateless w.r.t. the
Client's recipe source; they are surfaced through the Client only to
keep the facade uniform and leave room for future per-Client
telemetry hooks.

```go
// CollectSnapshot deploys a snapshotter Job to the target cluster and
// returns the resulting Snapshot. cfg is a facade-owned struct that
// mirrors pkg/snapshotter.AgentConfig field for field; the mirror is
// enforced by a test, so a field added upstream cannot silently stay at
// its zero value here.
//
// The returned Snapshot carries the parsed form plus Snapshot.Raw — the
// exact bytes the agent emitted. Persist Raw rather than re-serializing
// the parsed snapshot: a newer agent image can emit fields this module
// version does not model, and a typed round trip drops them silently.
//
// CollectSnapshot itself writes the snapshot nowhere unless AgentConfig.Output
// names a ConfigMap (cm://namespace/name), in which case the agent Job stages
// it there directly. To persist it anywhere else, hand Raw to
// snapshotter.DeliverSnapshot — a file, stdout, a ConfigMap, or a Go template
// render — which is what `aicr snapshot` does.
//
// On AKS, set AKSGPUPoolsPath to an `az aks nodepool list -o json` dump
// on the machine running this client: the pool projection is merged
// controller-side into the returned snapshot, and AKS profile-qualified
// resolution from that snapshot REQUIRES the resulting
// K8s.aks-gpu-pools.gpu-driver reading (a snapshot without it fails
// closed).
// Give the Job-backed snapshot its own deadline: contexts cap the
// configured timeouts from the parent side, so reusing the 30-second
// resolve ctx above would override the 5-minute AgentConfig.Timeout.
snapCtx, cancelSnap := context.WithTimeout(context.Background(), 10*time.Minute)
defer cancelSnap()
snap, err := client.CollectSnapshot(snapCtx, &aicr.AgentConfig{
	Kubeconfig:         "/path/to/target-kubeconfig",
	Namespace:          "aicr-snapshot",
	Image:              "ghcr.io/nvidia/aicr:v0.11.1",
	ServiceAccountName: "aicr-agent",
	Timeout:            5 * time.Minute,
	Cleanup:            true,
	AKSGPUPoolsPath:    "/path/to/aks-gpu-pools.json", // AKS only
})
if err != nil {
	log.Fatalf("collect snapshot: %v", err)
}

// NOTE: AgentConfig.AKSGPUPoolsPath and ResolveRecipeFromSnapshotWithProfile
// require the release containing the AKS gpuStack adoption (PR #1967) —
// newer than the module pin shown under Installation; update the pin to
// that release when reproducing this example.
// On AKS, resolve FROM the collected snapshot so the profile selection is
// verified against the recorded pool modes (ResolveRecipeFromSnapshot uses
// the declaration default, azure-managed, which requires pools reading
// Install; gpuStack=operator-managed as below requires a pool dump reading None —
// i.e. pools created with --gpu-driver none). A snapshot whose reading
// mismatches the selection — or that was collected without
// AKSGPUPoolsPath — fails closed.
resolveCtx, cancelResolve := context.WithTimeout(context.Background(), 30*time.Second)
defer cancelResolve()
aksResult, err := client.ResolveRecipeFromSnapshotWithProfile(resolveCtx,
	&aicr.Criteria{
		Service:     "aks",
		Accelerator: "h100",
		OS:          "ubuntu",
		Intent:      "training",
	}, snap, "gpuStack=operator-managed")
if err != nil {
	log.Fatalf("resolve from snapshot: %v", err)
}

// ValidateState runs the validation phases against the resolved recipe +
// observed snapshot. Pass the same kubeconfig you used for snapshot collection
// so that namespace, RBAC, ConfigMap, validator Job, and result operations all
// target that cluster. With no WithValidationPhases option it runs all three
// phases (Deployment, Conformance, Performance) in canonical order.
// Validation runs cluster Jobs per phase and can take well over an
// hour on the performance phase — bound it independently of the short
// resolve context (the SDK's own per-phase caps still apply inside).
valCtx, cancelVal := context.WithTimeout(context.Background(), 2*time.Hour)
defer cancelVal()
// WithValidationTimeout(0) removes the facade's default 75-minute
// operation cap (a per-check ordering guarantee, not a bound on a
// serial all-phase run) so valCtx above is the governing deadline.
results, err := client.ValidateState(valCtx, aksResult, snap,
	aicr.WithValidationKubeconfig("/path/to/target-kubeconfig"),
	aicr.WithValidationTimeout(0))
if err != nil {
	log.Fatalf("validate state: %v", err)
}
for _, r := range results {
	log.Printf("phase=%s status=%s duration=%s", r.Phase, r.Status, r.Duration)
}
```

When `WithValidationKubeconfig` is omitted or passed an empty string,
`ValidateState` uses the shared default Kubernetes client and its standard
discovery chain: `KUBECONFIG`, `~/.kube/config`, then in-cluster configuration.
When an explicit path is provided, the SDK reloads that kubeconfig and creates a
fresh client for each validation run. The run reuses that client for all of its
Kubernetes operations.

The `recipe` argument to `ValidateState` MUST be the `*RecipeResult`
returned by the same Client's `ResolveRecipe` (or `LoadRecipe`) call —
the unexported internal recipe state is required for constraint
evaluation.

To restrict the run to specific phases, pass `WithValidationPhases` in
the order you want them executed:

```go
results, err := client.ValidateState(ctx, result, snap,
	aicr.WithValidationPhases(aicr.PhaseDeployment, aicr.PhaseConformance))
```

Valid phase values are `PhaseDeployment`, `PhaseConformance`, and
`PhasePerformance` (canonical execution order). An unrecognized phase is rejected with
`ErrCodeInvalidRequest` before any cluster work, so a typo cannot
silently degrade to an empty run.

### Loading an existing recipe

When a recipe has already been resolved and persisted (for example a
recipe file checked into a GitOps repo, or a `cm://` ConfigMap URI), load
it back through the same Client with `LoadRecipe` instead of re-resolving
from criteria:

```go
result, err := client.LoadRecipe(ctx, "/etc/aicr/recipe.yaml", "")
if err != nil {
	log.Fatalf("load recipe: %v", err)
}
```

`LoadRecipe` hydrates overlay inputs (`kind: RecipeMetadata`) against the
Client's own data provider and returns a Client-owned `*RecipeResult`
ready for `ValidateState` / `BundleComponents` — it passes the same
ownership check as a `ResolveRecipe` result. An already-hydrated
`RecipeResult` file is returned with its provider bound to the Client. For a
profile-bearing overlay, the effective declaration resolved from that provider
must structurally match the file's declaration after JSON normalization;
otherwise loading fails rather than returning a recipe selected from a
different profile contract.
Note that bundle generation runs blocking preflight validations (for
example `CheckDriverOwnershipCoherence`, which rejects a recipe whose
snapshot recorded `gpuDriverState: absent` under a preinstalled-driver
profile). For recipes carrying `metadata.selectedProfile` (the AKS
family), the remedy is out-of-band: fix or recreate the GPU pools,
recapture the snapshot, and regenerate — the driver-ownership paths are
profile-owned, so `--set` overrides diverging from the selected value
are rejected. Only legacy pre-profile artifacts are remedied through
`--set` override flags, whose SDK surface is `MakeBundle` with
`BundleOptions.Config` — `BundleComponents` takes no overrides, so a
blocked legacy recipe must be bundled through `MakeBundle` (or
regenerated) rather than retried on the same call.
The kubeconfig argument (third parameter) is only needed when the recipe
path (first argument) is a `cm://` ConfigMap URI.

For unit tests that exercise the facade surface without a live
cluster, pass `aicr.WithValidationNoCluster(true)`: every check
reports as "skipped - no-cluster mode" and no Kubernetes resources
are created. Other facade options
(`WithValidationNamespace`, `WithValidationRunID`,
`WithValidationCleanup`, `WithValidationImagePullSecrets`,
`WithValidationTolerations`, `WithValidationNodeSelector`,
`WithValidationKubeconfig`) cover the production-controller knobs.

## Recipe sources

AICR exposes one production recipe source today; pick it via
`aicr.WithRecipeSource`:

| Source | Constructor | Status |
|--------|-------------|--------|
| Embedded | `aicr.EmbeddedSource()` | Production. Uses only AICR's built-in recipe data with no external overlay. |
| Local filesystem | `aicr.FilesystemSource(path)` | Production. Use a directory containing a `registry.yaml` (layered over the embedded recipe data). |
| OCI registry | `aicr.OCISource(registry, tag)` | **Reserved — not yet implemented.** `NewClient` returns `ErrCodeUnavailable` when this source is selected. |

`EmbeddedSource` resolves against the recipe data compiled into the
AICR binary — no filesystem path required. Use it when you want AICR's
bundled recipe data and no local overrides. `FilesystemSource`
layers an external directory over that same embedded data, so files in
the directory override their embedded equivalents.

## Client options

Beyond `WithRecipeSource`, `NewClient` accepts these functional options:

```go
allowLists, err := aicr.ParseAllowListsFromEnv()
if err != nil {
	log.Fatal(err)
}

client, err := aicr.NewClient(
	aicr.WithRecipeSource(aicr.EmbeddedSource()),
	aicr.WithVersion("1.2.3"),
	aicr.WithAllowLists(allowLists),
)
```

- **`WithVersion(version string)`** stamps the given version string into
  resolved recipe metadata (accessible via `result.Resolved().Metadata.Version`).
  Typically the consuming binary's build version.
- **`WithAllowLists(al *AllowLists)`** fences which criteria values the
  Client's resolve path accepts. A resolve whose criteria fall outside
  the allowlist is rejected before the recipe is built. Pass `nil` (or
  omit the option) to allow all values.
- **`ParseAllowListsFromEnv()`** builds an `AllowLists` from the
  `AICR_ALLOWED_ACCELERATORS`, `AICR_ALLOWED_SERVICES`,
  `AICR_ALLOWED_INTENTS`, and `AICR_ALLOWED_OS` environment variables.
  It returns `nil` when none are set — `WithAllowLists` treats a `nil`
  `AllowLists` as allow-all, so the result is always safe to pass straight
  to `WithAllowLists`.

`AllowLists` is a facade-owned struct whose `Accelerators`, `Services`,
`Intents`, and `OSTypes` fields are plain `[]string` slices, so callers
can construct one directly without depending on `pkg/recipe`'s enum
identifiers. When you already hold a `pkg/recipe.AllowLists`, use
`aicr.WrapAllowLists` to project it onto the facade shape.

## Resolving from criteria

`ResolveRecipe` takes the stable `RecipeRequest` shape and returns the
facade `RecipeResult` — a deliberately small struct exposing the
`Name`, `Version`, `Components`, and optional `SelectedProfile` of the
resolved recipe. Set `RecipeRequest.Profile` to the exact `name=value`
selection when the resolved composition declares a profile. Empty applies
the declaration's required default; a nonempty selection against an
unprofiled composition fails closed.

`Components` lists enabled (deployable) components only; disabled refs remain
visible via `Resolved().ComponentRefs`. When you
already hold an `*aicr.Criteria` value — for example, a REST handler
that parsed criteria from an incoming HTTP request and wrapped them with
`aicr.WrapCriteria` — use `ResolveRecipeFromCriteria`. Use
`ResolveRecipeFromCriteriaWithProfile` for an explicit selection and
`ResolveRecipeFromSnapshotWithProfile` for snapshot-filtered resolution.
These return the same facade `*RecipeResult`; call `result.Resolved()` when you need the
complete underlying `*pkg/recipe.RecipeResult` (constraints, deployment
order, validation config, metadata):

```go
rec, err := client.ResolveRecipeFromCriteria(ctx, aicr.WrapCriteria(criteria))
if err != nil {
	log.Fatalf("resolve recipe: %v", err)
}

// Facade surface — Name, Version, Components.
log.Printf("recipe %s components: %d", rec.Name, len(rec.Components))
if rec.SelectedProfile != nil {
	log.Printf("profile %s=%s", rec.SelectedProfile.Name, rec.SelectedProfile.Value)
}

// Full upstream shape, when needed.
resolved := rec.Resolved()
log.Printf("recipe constraints: %d", len(resolved.Constraints))
```

For a per-resolution Slurm accounting mode, use
`ResolveRecipeFromCriteriaWithOptions` or
`ResolveRecipeFromSnapshotWithOptions` with
`aicr.WithAccountingMode("customer-managed")`. The original criteria and
snapshot method signatures remain unchanged for source compatibility.

The returned `*RecipeResult` carries:

- `Name`, `Version`, `TranslatedAt` — stable identity
- `Components` — `[]ComponentRef` (Name, Kind, Version, Source, Chart, Namespace)
- `SelectedProfile` — selected name/value and declaration-wide `OwnedPaths`;
  nil for legacy recipes
- `Resolved()` — the upstream `*pkg/recipe.RecipeResult` for callers that
  need constraints, deployment order, validation config, or metadata
  (e.g., evidence emission). Do not mutate; do not retain past the
  facade `RecipeResult`'s lifetime — marshal first if persistence is
  needed.

`Criteria` is a facade-owned struct whose enum-typed fields project to
plain strings, decoupling the public surface from `pkg/recipe`'s enum
identifiers. Construct one directly or wrap an upstream
`*pkg/recipe.Criteria` via `aicr.WrapCriteria`. Allowlist enforcement
(`WithAllowLists`) applies here just as it does on `ResolveRecipe`; a
`nil` Client, `nil` context, or `nil` criteria each return
`ErrCodeInvalidRequest`, and the same facade-level timeout bounds the
resolve.

`ListCatalog` projects the effective inherited profile declaration on each
entry as `CatalogEntry.Profile`. The summary contains its name, description,
required default, and sorted value names; it is nil when the composition is
unprofiled.

To extract a single value from a resolved recipe, use
`SelectFromRecipeWithContext` with a dot-path selector. It hydrates the
recipe's component values and returns the value at the path; an empty
selector returns the entire hydrated structure, and a `nil` `*RecipeResult`
returns `ErrCodeInvalidRequest`. Hydration reads values files through the
recipe's `DataProvider`, so the context bounds real I/O — cancel it and the
hydration aborts. This is the same call the `aicr query` CLI command and the
REST query handler run:

```go
v, err := aicr.SelectFromRecipeWithContext(ctx, rec, "components.gpu-operator.values.driver.version")
if err != nil {
	log.Fatalf("select: %v", err)
}
log.Printf("driver version: %v", v)
```

`SelectFromRecipe` is the context-less form, kept for source compatibility.
It derives a `defaults.FileReadTimeout`-bounded context internally, so the
reads stay bounded but the caller cannot cancel them. Prefer the
context-aware form wherever a `context.Context` is available.

The **outermost** structured code distinguishes the two failure stages, so a
caller can shape a response without reimplementing hydrate-then-select:
`ErrCodeNotFound` means the selector path does not exist, and any other code
(`ErrCodeInternal`, `ErrCodeTimeout`, ...) means hydration failed. Match with
`errors.As` on the outermost error rather than `errors.Is` — `Is` walks the
wrap chain and would match an `ErrCodeNotFound` cause nested inside a
hydration failure.

### Delivering a collected snapshot

`snapshotter.DeliverSnapshot(ctx, raw, snapshotter.SnapshotDelivery{...})`
writes captured bytes to a destination independent of where the agent staged
them:

```go
err = snapshotter.DeliverSnapshot(snapCtx, snap.Raw, snapshotter.SnapshotDelivery{
	Output:     "snapshot.yaml",                 // file; "" or "-" for stdout; cm://ns/name for a ConfigMap
	Kubeconfig: "/path/to/target-kubeconfig",    // only used for a cm:// Output
})
```

A `cm://` destination is written, not assumed — including when it differs from
the `AgentConfig.Output` used at collection time. Set `TemplatePath` to render
through a Go template instead of copying bytes; `Output` then names the
rendered report.

`WrapResolved` turns a `*pkg/recipe.RecipeResult` — typically one taken from
`RecipeResult.Resolved()` and then projected by the caller — back into a
facade `*RecipeResult` that `SelectFromRecipeWithContext` accepts. The result
is queryable only: it carries no owning `Client`, so `MakeBundle`,
`BundleComponents`, and `ValidateState` reject it. Use `Client.AdoptRecipe`
when you need a bundle-able result.

## Errors

All errors returned by the facade are `*pkg/errors.StructuredError`
values carrying an `ErrorCode`. Use `errors.As` to inspect:

```go
import (
	stderrors "errors"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

_, err := client.ResolveRecipe(ctx, req)
var se *aicrerrors.StructuredError
if stderrors.As(err, &se) && se.Code == aicrerrors.ErrCodeInvalidRequest {
	// handle invalid input
}
```

## Context handling

`ResolveRecipe` (and every other context-aware facade method) honours
context cancellation. Each facade entry point unconditionally wraps the
caller's context with `context.WithTimeout` against its per-operation
cap. The effective deadline is the smaller of the caller's deadline
and the facade cap, per `context.WithTimeout` semantics — a caller
passing a tighter deadline keeps it; a caller passing
`context.Background()` gets the facade cap.

Per-operation caps:

- `ResolveRecipe` / `BundleComponents`: `defaults.RecipeOperationTimeout`
- `CollectSnapshot`: caller-controlled via `AgentConfig.Timeout` (falling
  back to `defaults.SnapshotOperationTimeout` when unset), plus
  `defaults.SnapshotOperationGrace`. The grace exists because
  `AgentConfig.Timeout` budgets Job *completion* only — deployment and
  result retrieval sit outside it, so a bare cap would silently shrink the
  completion budget you asked for.
- `ValidateState`: `defaults.ValidationOperationTimeout`
- `MakeBundle`: opt-in via `BundleOptions.Timeout`. When unset (`0`) the
  caller's context governs unchanged — large bundles, `--vendor-charts`,
  and attestation/signing can exceed any fixed cap. The REST `/v1/bundle`
  handler sets it to `defaults.BundleHandlerTimeout`; the CLI `bundle`
  command leaves it `0`.

Passing a `nil` `context.Context` returns `ErrCodeInvalidRequest`. Use
`context.Background()` (or a deadline-bounded child) for unbounded callers.

## Compatibility

Today AICR is pre-1.0. Under Go module versioning, a v0 minor release may
contain breaking API changes. The project mechanically detects and explicitly
records incompatible changes to the facade, but consumers must **pin a patch
version** in `go.mod` and audit upgrades.

Starting with v1.0, the facade's exported API follows [Semantic
Versioning][semver]:

- **Major** bumps may rename, remove, or change the shape of exported
  types and function signatures.
- **Minor** bumps may add new exported types, fields, or methods.
- **Patch** bumps contain compatible bug fixes.

## See also

- [Public API surface](./public-api.md) — stability matrix per package
- [Automation guide](./automation.md) — CI integration patterns
- [Recipe development](./recipe-development.md) — authoring recipes

[semver]: https://semver.org/spec/v2.0.0.html
