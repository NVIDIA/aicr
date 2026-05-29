# Using AICR as a Go library

AICR ships as both a CLI and a Go library. External projects that need
to resolve validated recipes, generate bundles, or collect observed
state can import AICR directly. This page is for those consumers.

## Which package to import

**Import the `github.com/NVIDIA/aicr/pkg/aicr` package.** This is the
stable facade.

```go
import aicr "github.com/NVIDIA/aicr/pkg/aicr"
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
go get github.com/NVIDIA/aicr@v0.11.1
```

## Quick start

```go
package main

import (
	"context"
	"log"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/aicr"
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
// returns the resulting Snapshot. cfg is a transparent alias of
// pkg/snapshotter.AgentConfig.
snap, err := client.CollectSnapshot(ctx, &aicr.AgentConfig{
	Kubeconfig:         "/path/to/target-kubeconfig",
	Namespace:          "aicr-snapshot",
	Image:              "nvcr.io/nvidia/aicr-agent:v0.11.1",
	ServiceAccountName: "aicr-agent",
	Timeout:            5 * time.Minute,
})
if err != nil {
	log.Fatalf("collect snapshot: %v", err)
}

// ValidateState runs every validation phase (Deployment, Performance,
// Conformance) against the resolved recipe + observed snapshot.
results, err := client.ValidateState(ctx, result, snap)
if err != nil {
	log.Fatalf("validate state: %v", err)
}
for _, r := range results {
	log.Printf("phase=%s status=%s duration=%s", r.Phase, r.Status, r.Duration)
}
```

The `recipe` argument to `ValidateState` MUST be the `*RecipeResult`
returned by the same Client's `ResolveRecipe` call — the unexported
internal recipe state is required for constraint evaluation.

For unit tests that exercise the facade surface without a live
cluster, pass `aicr.WithValidationNoCluster(true)`: every check
reports as "skipped - no-cluster mode" and no Kubernetes resources
are created. Other facade options
(`WithValidationNamespace`, `WithValidationRunID`,
`WithValidationCleanup`, `WithValidationImagePullSecrets`,
`WithValidationTolerations`, `WithValidationNodeSelector`) cover the
production-controller knobs.

## Recipe sources

AICR exposes one production recipe source today; pick it via
`aicr.WithRecipeSource`:

| Source | Constructor | Status |
|--------|-------------|--------|
| Local filesystem | `aicr.FilesystemSource(path)` | Production. Use a directory containing a `registry.yaml` (layered over the embedded recipe data). |
| OCI registry | `aicr.OCISource(registry, tag)` | **Reserved — not yet implemented.** `NewClient` returns `ErrCodeUnavailable` when this source is selected. |

`FilesystemSource` is the only production-ready source today.

## Errors

All errors returned by the facade are `*pkg/errors.StructuredError`
values carrying an `ErrorCode`. Use `errors.As` to inspect:

```go
import (
	stderrors "errors"
	aicr "github.com/NVIDIA/aicr/pkg/aicr"
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
- `CollectSnapshot`: caller-controlled via `AgentConfig.Timeout`,
  falling back to `defaults.SnapshotOperationTimeout` when unset
- `ValidateState`: `defaults.ValidationOperationTimeout`

Passing a `nil` `context.Context` returns `ErrCodeInvalidRequest`. Use
`context.Background()` (or a deadline-bounded child) for unbounded callers.

## Compatibility

The facade's exported API follows [Semantic Versioning][semver]:

- **Major** bumps may rename, remove, or change the shape of exported
  types and function signatures.
- **Minor** bumps may add new exported types, fields, or methods.
- **Patch** bumps are bug-fix-only.

Today AICR is pre-1.0. **Pin a patch version** in your `go.mod` and
audit diffs on upgrade.

## See also

- [Public API surface](./public-api.md) — stability matrix per package
- [Automation guide](./automation.md) — CI integration patterns
- [Recipe development](./recipe-development.md) — authoring recipes

[semver]: https://semver.org/spec/v2.0.0.html
