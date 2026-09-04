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

## Runnable examples

Each facade entry point below has a compiled counterpart in
[`pkg/client/v1`](https://pkg.go.dev/github.com/NVIDIA/aicr/pkg/client/v1#pkg-examples).
They are ordinary Go example functions, so `go test` builds them on every
change — a facade change that breaks one of these fails in AICR's tree rather
than in yours.

| Example | Covers | Runs |
|---|---|---|
| `Example` | Quick start: client, resolve from criteria | yes |
| `Example_errorCodes` | Matching structured error codes | yes |
| `Example_bundleAndVerify` | Resolve → bundle → verify, hermetically | yes |
| `Example_workflow` | Full Snapshot → Recipe → Bundle: load, derive criteria, resolve, bundle, verify | yes |
| `Example_trustLevels` | The accepted trust levels, and their ordering trap | yes |
| `Example_criteriaDimensions` | The coverage dimensions | yes |
| `Example_committedConfig` | `AICRConfig` → source → catalog → criteria, in the required order | no |
| `Example_resolveFromSnapshot` | `LoadSnapshot` plus snapshot criteria relaxation | no |
| `ExampleClient_DiffSnapshots` | In-memory drift detection between two loaded snapshots | no |
| `ExampleClient_LoadRecipe` | Reading a previously emitted recipe | no |
| `ExampleClient_CollectSnapshot` | Capturing cluster state via the snapshotter Job | no |
| `ExampleClient_ValidateState` | Selecting validation phases, and `--no-cluster` mode | no |
| `ExampleClient_RecipeDigest` | The digest a CI staleness gate compares | no |
| `ExampleClient_VerifyEvidence` | Evidence verification and exit classes | no |
| `ExampleClient_VerifyCatalog` / `ExampleClient_SignCatalog` | Checking and producing the catalog signature | no |
| `ExampleClient_PublishEvidence` | Signing and pushing an evidence bundle | no |
| `ExampleVerifyBinaryAttestation` | Proving a binary came from NVIDIA CI | no |

**What "runs" means, and what it does not.** Examples marked *yes* print an
`Output:` block, so `go test` executes them and asserts the output. The rest
are **compiled but not executed** — they need a cluster, a registry, a signing
identity, or files that belong to your environment. Compilation still pins
every signature, field name, and option they touch, so a renamed method or a
dropped field breaks the build; it does not prove those flows behave
correctly at runtime.

The guarantee covers the examples, not this page. Prose here can still drift,
and short illustrative snippets outside the table are not compiled — prefer
copying from the examples, which are complete and known to build.

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
	// embedded recipe data. OCISource pulls a digest-pinned recipe
	// catalog from an OCI registry instead; see "Recipe sources" and
	// "Digest-pinned OCI recipe sources" below for both.
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
Snapshot → Validate workflow, including comparison of two snapshots for
configuration drift. These operations are stateless w.r.t. the Client's recipe
source; they are surfaced through the Client to keep the facade uniform and
leave room for future per-Client telemetry hooks.

### Loading a snapshot you already have

Most integrations do not capture a snapshot inline. One pipeline stage
records cluster state, a later stage resolves or validates against it —
or the snapshot is committed and replayed. `LoadSnapshot` is the entry
point for that case, and needs no cluster for a local file or an
HTTP(S) URL:

```go
snap, err := client.LoadSnapshot(ctx, "./snapshot.yaml", "")
if err != nil {
	log.Fatalf("load snapshot: %v", err)
}

results, err := client.ValidateState(ctx, recipe, snap,
	aicr.WithValidationNoCluster(true))
```

`path` takes a local file, an HTTP(S) URL, or a `cm://namespace/name`
ConfigMap URI; the `kubeconfig` argument resolves the `cm://` form and
is ignored for the other two.

**It fails closed on a document that is not a snapshot** — a wrong
`kind` (an `AICRConfig`, say) or an `apiVersion` this build does not
understand. That gate matters more than it looks: snapshot
deserialization is non-strict, so without it a typo'd path would decode
into an empty `Snapshot`, derive `criteria(any)`, and silently resolve
the generic fallback recipe with exit 0. Empty `kind` and `apiVersion`
are tolerated for snapshots that predate those fields.

`Snapshot.Raw` is **not** populated by `LoadSnapshot` — only
`CollectSnapshot` sets it. The source you loaded from is already the
durable artifact.

If you need the bytes, you can read the source again — but that returns
its **current** contents, which for a URL or a ConfigMap (or a file
someone rewrote) need not be what this call parsed. When byte-for-byte
identity with the loaded snapshot matters, such as hashing what you
validated, capture the source contents yourself and load from that
capture instead of re-reading afterward.

### Comparing snapshots for drift

`DiffSnapshots` compares the measurement payloads already held by two facade
snapshots. The comparison is in memory: it does not read a cluster or revisit
the file, URL, or ConfigMap the snapshots came from.

```go
baseline, err := client.LoadSnapshot(ctx, "before.yaml", "")
if err != nil {
	log.Fatalf("load baseline: %v", err)
}
target, err := client.LoadSnapshot(ctx, "after.yaml", "")
if err != nil {
	log.Fatalf("load target: %v", err)
}

result, err := client.DiffSnapshots(ctx, baseline, target, aicr.SnapshotDiffOptions{
	BaselineSource: "before.yaml",
	TargetSource:   "after.yaml",
})
if err != nil {
	log.Fatalf("diff snapshots: %v", err)
}
if result.HasDrift() {
	log.Printf("detected %d change(s)", result.Summary.Total)
}
```

Drift is returned as data, not as an error. `SnapshotDiff.Changes` preserves
added, removed, and modified values, while `Summary` provides aggregate counts.
The source labels are optional output metadata and do not affect comparison.
Use `aicr.WriteSnapshotDiffTable` for the same human-readable table format as
`aicr diff`; JSON and YAML serializers can consume the facade-owned result
directly.

Inputs must retain at least one typed measurement through `LoadSnapshot`,
`CollectSnapshot`, or `WrapSnapshot`. A hand-constructed `&aicr.Snapshot{}` or
a wrapped snapshot with no usable measurement is rejected instead of being
reported as no drift.

### Capturing a snapshot from a live cluster

```go
// CollectSnapshot deploys a snapshotter Job to the target cluster and
// returns the resulting Snapshot. cfg is a facade-owned struct that
// mirrors pkg/snapshotter.AgentConfig field for field; the mirror is
// enforced by TestAgentConfigMirrorsInternal, so a field added upstream
// cannot silently stay at its zero value here.
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
// closed). On OKE, OKEAddonsPath plays the same role from an
// `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json`
// dump, merged as the K8s.oke-addons.nvidia-gpu-plugin reading.
// Give the Job-backed snapshot its own deadline: contexts cap the
// configured timeouts from the parent side, so reusing the 30-second
// resolve ctx above would override the 5-minute AgentConfig.Timeout.
snapCtx, cancelSnap := context.WithTimeout(context.Background(), 10*time.Minute)
defer cancelSnap()
snap, err := client.CollectSnapshot(snapCtx, &aicr.AgentConfig{
	Kubeconfig: "/path/to/target-kubeconfig",
	// Namespace is the only required field: the Job, its RBAC, and the result
	// ConfigMap are created there, and an empty value is rejected with
	// ErrCodeInvalidRequest before any cluster access. Image is defaulted
	// when empty to the tag matching the Client's WithVersion; it is pinned
	// explicitly here because that is what a version-skew-sensitive or
	// air-gapped deployment wants.
	//
	// JobName is an optional name prefix; leaving it unset defaults to
	// "aicr" with a generated run ID appended, so every run gets its own
	// uniquely named Job without the caller managing that.
	//
	// ServiceAccountName carries two meanings and is EXACT-IF-EXISTS. When
	// a ServiceAccount of exactly that name already exists in Namespace it
	// is used verbatim and the run creates NO ServiceAccount, Role,
	// RoleBinding, ClusterRole or ClusterRoleBinding — and deletes none at
	// cleanup. Otherwise it is a prefix and the run creates and owns the
	// full run-scoped RBAC set. Leaving it unset, as here, keeps the
	// run-scoped default and never probes for an existing ServiceAccount.
	Namespace:       "aicr-snapshot",
	Image:           "ghcr.io/nvidia/aicr:v0.19.0",
	Timeout:         5 * time.Minute,
	Cleanup:         true,
	AKSGPUPoolsPath: "/path/to/aks-gpu-pools.json", // AKS only
	OKEAddonsPath:   "/path/to/oke-addons.json",    // OKE only
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

#### Running the agent as an existing ServiceAccount

`AgentConfig.ServiceAccountName` is **exact-if-exists**, so it carries two
meanings depending on the cluster:

- A ServiceAccount of exactly that name already exists in `Namespace`: the
  agent pod runs as it verbatim, and `CollectSnapshot` creates **no**
  ServiceAccount, Role, RoleBinding, ClusterRole, or ClusterRoleBinding, and
  deletes none at cleanup.
- Otherwise it is a name prefix and the run creates and owns the full
  run-scoped RBAC set, named `<prefix>-<run-id>`.

Leaving the field empty keeps the run-scoped default and never probes for an
existing ServiceAccount, so a stray ServiceAccount cannot capture a run.

Use the first form when the ServiceAccount must carry EKS IRSA or GKE Workload
Identity annotations: both providers pin trust to the ServiceAccount *name*, so
a run-scoped name can never be trusted by either. Grant it the agent's
permissions once — the objects it creates are permanent and no run cleanup
removes them:

```go
// Admin step, run once. Provisions and returns; it deploys no Job.
// Returns ErrCodeNotFound when the ServiceAccount does not exist.
res, err := snapshotter.ProvisionAgentRoles(ctx, &snapshotter.AgentRolesConfig{
	Kubeconfig:         "/path/to/target-kubeconfig",
	Namespace:          "gpu-operator",
	ServiceAccountName: "irsa-snapshotter",
	// DiscoverNetwork also grants the cluster-scoped MUTATING rules live
	// network discovery needs — permanently, not for one run's lifetime.
	DiscoverNetwork: false,
})
if err != nil {
	log.Fatalf("provision agent roles: %v", err)
}
log.Printf("granted via %s/%s and %s/%s",
	res.Role, res.RoleBinding, res.ClusterRole, res.ClusterRoleBinding)
```

Adopting one ServiceAccount across runs waives per-run permission isolation:
concurrent runs sharing it hold the same grants, and a `DiscoverNetwork`
provisioning leaves mutating cluster permissions in place until an operator
removes them. See
[Agent Deployment](../user/agent-deployment.md#using-an-existing-serviceaccount-irsa-and-workload-identity)
for the full migration path and teardown commands.

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

AICR exposes three production recipe sources; pick one via
`aicr.WithRecipeSource`:

| Source | Constructor | Status |
|--------|-------------|--------|
| Embedded | `aicr.EmbeddedSource()` | Production. Uses only AICR's built-in recipe data with no external overlay. |
| Local filesystem | `aicr.FilesystemSource(path)` | Production. Use a directory containing a `registry.yaml` (layered over the embedded recipe data). |
| OCI registry | `aicr.OCISource(repository, digest)` | Production. Pulls one immutable, digest-pinned recipe catalog into a private per-Client workspace. |

`EmbeddedSource` resolves against the recipe data compiled into the
AICR binary — no filesystem path required. Use it when you want AICR's
bundled recipe data and no local overrides. `FilesystemSource`
layers an external directory over that same embedded data, so files in
the directory override their embedded equivalents.

### Digest-pinned OCI recipe sources

`OCISource` keeps the repository and immutable selector separate. The
repository may start with `oci://`, but must not contain a tag or digest.
The selector must be a complete `sha256:<64-hex-character>` manifest
digest obtained through trusted configuration; tags and implicit `latest`
are rejected.

The accepted artifact is one OCI image manifest with the AICR artifact type,
the canonical empty config, and exactly one gzip-compressed layer. Downloads
and extraction are bounded, content digests are checked while streaming, and
archive traversal, links, devices, oversized content, and malformed catalogs
fail closed before the provider is activated.

OCI sources use credentials from the standard Docker configuration
(`~/.docker/config.json` or `$DOCKER_CONFIG`) and may invoke the configured
credential helper for the selected registry host.

Use `NewClientContext` so caller cancellation and tighter deadlines
propagate through registry authentication, download, extraction, and catalog
validation:

```go
import (
	"context"
	"errors"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

func useOCIRecipes(ctx context.Context, repository, manifestDigest, tempDir string) (retErr error) {
	client, err := aicr.NewClientContext(ctx,
		aicr.WithRecipeSource(aicr.OCISource(repository, manifestDigest)),
		aicr.WithOCISourceTempDir(tempDir),
	)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, client.Close()) }()

	return client.LoadCatalog(ctx)
}
```

`NewClient` remains a bounded compatibility wrapper. OCI construction
never exceeds `defaults.OCIRecipeConstructionTimeout` (eight minutes), while
`NewClientContext` also honors any shorter caller deadline. Registry staging
and materialization each retain the five-minute
`defaults.OCIRecipePullTimeout` phase ceiling; the larger construction
envelope reserves more than three minutes for materialization and catalog
validation after maximum-jitter pull retries.
`Client.Close` waits for in-flight reads, evicts provider-scoped caches,
and removes only the unique child workspace it owns.

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
- **`WithOCISourceTempDir(parent string)`** selects an existing writable
  parent for an OCI-backed Client's private workspace. It is rejected for
  embedded and filesystem sources. Budget capacity for up to a 64 MiB staged
  compressed layer plus a 128 MiB extracted tree, along with filesystem and
  manifest overhead.

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

### Criteria relaxation on the snapshot path

A snapshot resolve is strict by default — every criteria dimension you state
must be honored by an applied overlay, or resolution fails with
`ErrCodeInvalidRequest` and a `details.uncovered` payload.

That is right for criteria a user typed, but wrong for criteria you *derived*
from the snapshot. An overlay tree can be deliberately agnostic to a dimension
(Kind's overlays state no `os`) while `Client.CriteriaFromSnapshot` still
detects a concrete value on the node — nothing in the recipe distinguishes it,
so failing there rejects a legitimate query.

Pass `aicr.WithSnapshotCriteriaRelaxation` and name the dimensions **you
received explicitly**. Anything else is treated as derived, and a coverage
failure limited to derived dimensions is retried once with those cleared:

```go
// Every dimension the snapshot could not determine comes back as the "any"
// wildcard; nothing is guessed.
criteria, err := client.CriteriaFromSnapshot(snap)
if err != nil {
    log.Fatalf("derive criteria: %v", err)
}
criteria.Intent = "training" // the user asked for this one

result, err := client.ResolveRecipeFromSnapshotWithOptions(
    ctx, criteria, snap,
    aicr.WithSnapshotCriteriaRelaxation(aicr.DimensionIntent))
if err != nil {
    log.Fatalf("resolve: %v", err)
}
for _, dim := range result.RelaxedDimensions {
    log.Printf("resolved recipe is broader than requested: %s was relaxed", dim)
}
```

Runnable version: `Example_workflow` in `pkg/client/v1/example_test.go`, which
loads a snapshot, derives criteria from it, resolves, bundles and verifies —
and carries an `Output:` block, so it executes on every test run rather than
only compiling. `ExampleClient_CriteriaFromSnapshot` shows this step on its own,
but reads a snapshot path it does not ship and so is compile-only.

Relaxation is deliberately narrow. Three cases propagate the original coverage
error rather than retrying:

- **A dimension you named.** Relaxing a value the caller asked for would
  silently resolve a different recipe than requested.
- **A constraint-excluded dimension.** An overlay carrying it exists, but the
  observed cluster failed its constraints — a Kubernetes version below the
  overlay's floor, say. Relaxing there converts "your cluster does not meet
  this overlay's requirements" into a broader recipe that resolves cleanly,
  discarding the finding you most need.
- **A relaxation that would leave no stated coverage dimension.** Such criteria
  match every overlay and resolve the generic fallback recipe at exit 0 — the
  fail-open the pre-resolution specificity guard exists to prevent (#1888).
  Note this is not the same as "criteria is empty": a fingerprint-derived
  `nodes` value survives the clear, but no overlay gates on `nodes`, so it
  selects nothing.

The distinction in the second case is *why* the dimension is uncovered: no
overlay states it at all (safe to relax — nothing in the recipe distinguishes
the value) versus an overlay states it but was constraint-excluded (not safe).
The resolver reports which, per dimension, in the coverage error's
`details.uncovered[].constraintExcluded`.

Two more properties:

- **Passing no dimensions is meaningful,** not a no-op: it means every
  dimension was derived and all are relaxable. That is the common case for a
  pure fingerprint query. Presence of the option is what enables the policy, so
  omitting it entirely is how you keep strict behavior.
- **It is snapshot-only.** On `ResolveRecipeFromCriteria` there is no
  fingerprint, so the option is rejected with `ErrCodeInvalidRequest` rather
  than ignored.

Both attempts share the call's timeout budget, and relaxation happens at most
once.

The returned `*RecipeResult` carries:

- `Name`, `Version`, `TranslatedAt` — stable identity
- `Components` — `[]ComponentRef` (Name, Kind, Version, Source, Chart, Namespace)
- `SelectedProfile` — selected name/value and declaration-wide `OwnedPaths`;
  nil for legacy recipes
- `RelaxedDimensions` — criteria dimensions cleared by
  `WithSnapshotCriteriaRelaxation`. Non-empty only when the first attempt failed
  coverage on derived dimensions **and** the retry succeeded. Every other
  outcome — option not passed, first attempt succeeded, relaxation refused, or
  the retry itself failed — yields either an empty slice or `nil, error` with no
  `RecipeResult` at all, so this field is never the way to detect a failure
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
	Output:     "snapshot.json",                 // file; "" or "-" for stdout; cm://ns/name for a ConfigMap
	Kubeconfig: "/path/to/target-kubeconfig",    // only used for a cm:// Output
	Format:     serializer.FormatJSON,           // "" and FormatYAML deliver the agent's bytes verbatim to a file or stdout
})
```

A `cm://` destination is written, not assumed — including when it differs from
the `AgentConfig.Output` used at collection time. Set `TemplatePath` to render
through a Go template instead of copying bytes; `Output` then names the
rendered report, and `Format` is ignored.

The agent always stages YAML, so `Format` is where a JSON or table rendering
happens. YAML (and the zero value, for callers written before the field
existed) is a byte copy for file and stdout destinations — fields a newer agent
image emits than the calling binary models survive. `FormatJSON` re-encodes the
same keys through a generic map, preserving those fields; `FormatTable` renders
the typed `Snapshot` and is therefore the one format that can drop them.

A `cm://` destination is the exception to the byte copy: the writer derives the
`snapshot.<ext>` data key, the `format` and `timestamp` entries, and the
resource labels from the parsed document, so it re-serializes even for YAML. It
does so deterministically and through a generic map, so unmodeled fields still
survive; only the exact bytes do not. Deliver to a file or stdout when you need
byte-identical YAML.

`WrapResolved` turns a `*pkg/recipe.RecipeResult` — typically one taken from
`RecipeResult.Resolved()` and then projected by the caller — back into a
facade `*RecipeResult` that `SelectFromRecipeWithContext` accepts. The result
is queryable only: it carries no owning `Client`, so `MakeBundle`,
`BundleComponents`, and `ValidateState` reject it. Use `Client.AdoptRecipe`
when you need a bundle-able result.

`AdoptRecipe` canonicalizes the artifact `Kind` on the copy it returns: an
absent, empty, or legacy `Recipe` kind is stamped as `RecipeResult`, and any
other kind is rejected with `ErrCodeInvalidRequest`. This keeps a bundle
generated from an externally-decoded recipe reloadable by the file loader. The
caller's own `RecipeResult` is never mutated, and `APIVersion` is validated but
never rewritten.

## Using a committed AICRConfig

A team that has standardized on an `AICRConfig` — the version-controlled
document `aicr --config` reads — can consume the same file from their own
tooling, so the CLI and an embedding runtime agree on the settings by
construction rather than by convention.

```go
import (
	"context"
	"errors"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

func resolveCommittedConfig(ctx context.Context) (retErr error) {
	cfg, err := aicr.LoadConfig(ctx, "aicr-config.yaml") // path or HTTP(S) URL
	if err != nil {
		return err
	}

	// spec.recipe.data decides how the Client is constructed.
	source := aicr.EmbeddedSource()
	if configured, ok := cfg.RecipeSource(); ok {
		source = configured
	}
	client, err := aicr.NewClientContext(ctx, aicr.WithRecipeSource(source))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, client.Close()) }()

	// REQUIRED before deriving criteria: loading the catalog is what seeds this
	// Client's registry with the values its overlays contribute. Skip it and a
	// value defined only by spec.recipe.data is still unknown, so the derivation
	// below rejects it.
	if err = client.LoadCatalog(ctx); err != nil {
		return err
	}

	// spec.recipe.criteria, parsed against this Client's registry so a value
	// contributed by a --data overlay validates against the same catalog.
	criteria, err := cfg.RecipeCriteria(client.CriteriaRegistry())
	if err != nil {
		return err
	}
	opts, err := cfg.RecipeResolveOptions() // profile + accounting + runtime inventory
	if err != nil {
		return err
	}

	_, retErr = client.ResolveRecipeFromCriteriaWithOptions(ctx, criteria, opts...)
	return retErr
}
```

**Config derives options; it never applies them.** A `Config` does not attach
to a `Client` and is never consulted implicitly. Each method returns a
populated value you may then override:

```go
verifyOpts, err := cfg.BundleVerifyOptions()   // from spec.verify
verifyOpts.MinTrustLevel = "verified"          // caller wins, visibly
v, err := client.VerifyBundle(ctx, "./bundle", verifyOpts)
```

That is deliberate rather than incidental. The facade's options are plain
structs, so a field left at its zero value is indistinguishable from one set
to the zero value on purpose — there is no equivalent of the CLI's
`cmd.IsSet`. An implicit merge would have to guess, and would silently hand
the config's value back to a caller who deliberately cleared a setting.
Deriving keeps precedence to one readable line at the call site.

It also mirrors what the CLI does: build options from config, then let an
explicitly-set flag win. The flag half necessarily stays in `pkg/cli`, the
only layer that knows a flag was set.

**Every method is nil-safe.** A nil `*Config` — what you get when no document
was supplied — returns zero values rather than panicking, so derivations can
run unconditionally.

**Criteria values are validated at `RecipeCriteria`, not at `LoadConfig`.**
Whether a value is legal depends on the `CriteriaRegistry`, which is
per-`DataProvider` — so a value your own catalog contributes is unknown until
that provider is built and its catalog loaded. That is why the order in the
example above matters: load, construct, `LoadCatalog`, *then* derive criteria.
Loading checks structure only; a value in no catalog still fails, at the
derive step rather than the load step.

| Method | Reads |
|---|---|
| `BundleVerifyOptions()` | `spec.verify.policy` + `spec.verify.trust` |
| `BundleOptions()` | `spec.bundle.deployment` + `spec.bundle.scheduling` + `spec.bundle.attestation` |
| `BundleInputOptions()` | `spec.bundle.input` + `.output` + `.registry` |
| `ValidateSettings()` | `spec.validate.agent` + `.execution` |
| `ValidateInputOptions()` | `spec.validate.input` + `.execution.failOnError` |
| `EvidenceAttestationOptions()` | `spec.validate.evidence.attestation` |
| `CNCFEvidenceOptions()` | `spec.validate.evidence.cncf` |
| `SnapshotAgentConfig()` | `spec.snapshot.agent` + `.execution` (**not** `.output`) |
| `SnapshotOutputOptions()` | `spec.snapshot.output` |
| `RecipeSource()` | `spec.recipe.data` |
| `RecipeCriteria(reg)` | `spec.recipe.criteria` |
| `RecipeResolveOptions()` | `spec.recipe.profile`, `spec.recipe.configuration.slurm.accounting.mode`, `spec.recipe.configuration.runtimeInventory.mode` |
| `RecipeProfile()` / `RecipeAccountingMode()` / `RecipeRuntimeInventoryMode()` | the same three, raw, for callers applying their own precedence first |
| `RecipeOutputOptions()` | `spec.recipe.output` |
| `SnapshotPath()` | `spec.recipe.input.snapshot` |
| `IsCriteriaStrict()` | `spec.recipe.criteriaStrict` |

All five spec sections have a derivation, and every one of them has a
destination — including `spec.validate.evidence.cncf`, which
`CNCFEvidenceOptions()` carries even though no `Client` method consumes CNCF
AI Conformance evidence directly; the caller does (the CLI's
`validateFlagCombinations`, `cncf.New`, and `runCNCFSubmission`). That mirrors
`SnapshotOutputOptions()`, which projects `spec.snapshot.output` for the same
reason: `Client.CollectSnapshot` does not consume it either, but the caller
performing delivery does. Needing `Unwrap()` anywhere is worth reporting — it
means a derivation is missing, and `pkg/config` carries no stability
guarantee.

### What `BundleOptions()` does and does not carry

`BundleOptions()` returns the 18 bundler settings `spec.bundle.deployment` and
`spec.bundle.scheduling` configure (plus the two attestation flags the bundler
itself reads) as plain fields — not a built `Config` — so you read and
override individual settings directly, the same as `ValidateSettings()`. It
also returns `OIDCResolve` (the four signing settings that reach the attester
rather than the bundler):

```go
opts, err := cfg.BundleOptions()
if err != nil {
    return err
}
opts.Nodes = 32 // caller wins, visibly
```

`Config` is still on the struct, as an escape hatch: when you already have a
fully built `*BundleConfig` from something the flat fields cannot express (a
CLI-flag-only setting such as `Version` or `Serial`, or a config object you
mutate in place), set it and it wins outright over the 18 bundler-config flat
fields (`Deployer` through `AppName`) — `BundleOptions()` itself never
populates it. `Config` does NOT override `Attester`, `OIDCResolve`,
`BinaryAttestation`, `OutputDir` or `Timeout`: those five stay
caller-supplied regardless of `Config`, which is why the REST `/v1/bundle`
handler can set `Config` and `OutputDir`/`Timeout` in the same struct
literal and have all three take effect.

The six caller-side fields have their own type, `BundleInputOptions()`, not
because they are declined but because `MakeBundle` never reads them — see the
next section.

Signing follows the same derive-don't-apply rule as everything else: a non-nil
`Attester` wins over `OIDCResolve`, so an explicitly supplied signer is never
silently rebuilt from config. When `Attester` is nil, `MakeBundle` also
requires `Config.Attest()` (when `Config` is set) and `OIDCResolve.Attest` to
agree — a hand-built `BundleOptions` that sets both and disagrees (say,
`config.WithAttest(false)` baked into `Config` while `OIDCResolve.Attest` is
`true`) is rejected with `ErrCodeInvalidRequest` rather than silently
producing unsigned output that looks attested, or deriving a real signer
whose result `Config.Attest()` then discards. `BundleOptions()` itself always
keeps the two in agreement (`opts.Attest` and `opts.OIDCResolve.Attest` come
from the same `spec.bundle.attestation.enabled` value), so this only matters
for a caller who assembles `BundleOptions` by hand.

A KMS key and keyless OIDC are mutually exclusive. `BundleOptions()` rejects a
whitespace-only `signingKey` (must not be blank after trimming) and rejects
`signingKey` combined with `fulcioURL`. It deliberately does NOT reject
`signingKey` combined with `oidcDeviceFlow` at this layer: `BundleOptions()`
runs before the CLI's flag-over-config merge, so an eager rejection here would
make `--oidc-device-flow=false` unable to correct a document that sets both —
the error would fire before that flag is ever read. The CLI's
`validateSigningKeyExclusivity`, run on the flag-merged options, is what
catches the `signingKey` + `oidcDeviceFlow` combination for CLI invocations.
An SDK caller deriving `BundleOptions()` directly and calling `MakeBundle`
with no flag merge gets no equivalent guard for that specific pair — the
resulting bundle still signs with the KMS key (`ResolveAttesterLazy` picks KMS
whenever `SigningKey` is non-empty), matching the pre-#2245 behavior; avoid
setting both in a document consumed outside the CLI.

**Device flow needs a prompt writer.** `spec.bundle.attestation.oidcDeviceFlow`
sets `OIDCResolve.DeviceFlow`, but config cannot carry an `io.Writer`, so the
derived value leaves `PromptWriter` nil. Set one before signing or the
verification code the user has to enter goes nowhere:

```go
opts, err := cfg.BundleOptions()
if err != nil {
    return err
}
opts.OIDCResolve.PromptWriter = os.Stderr   // or any caller-owned writer
```

```go
opts, err := cfg.BundleOptions()       // from spec.bundle
if err != nil {
    return err                         // malformed spec.bundle, or mixed
}                                      // KMS + keyless signing settings
opts.OutputDir = "./bundles"           // caller wins, visibly
artifact, err := client.MakeBundle(ctx, rec, opts)
```

Check that first error rather than letting the second assignment overwrite it.
`BundleOptions()` returns a zero value alongside its error, so a swallowed
failure bundles with defaults — no deployer, no overrides, no attestation —
from a document that looked configured.

### What `BundleInputOptions()` does and does not carry

`MakeBundle` takes an already-resolved `RecipeResult` and does not push its
own result anywhere — the caller does, after it returns. `BundleInputOptions()`
carries the `spec.bundle` fields that describe that caller-side work instead:
which recipe to bundle, which image-refs file to write to, where to push the
finished bundle, and how to reach that registry:

```go
input, err := cfg.BundleInputOptions()
if err != nil {
    return err
}
rec, err := client.LoadRecipe(ctx, input.RecipePath, "")
```

| Field | Source |
|---|---|
| `RecipePath` | `spec.bundle.input.recipe` |
| `ImageRefsPath` | `spec.bundle.output.imageRefs` |
| `OutputTarget`, `OutputTargetRaw` | `spec.bundle.output.target`, parsed and raw |
| `InsecureTLS`, `PlainHTTP` | `spec.bundle.registry.insecureTLS`, `.plainHTTP` — the document already chose the push destination in `OutputTarget`, so it is trusted to describe how to reach it too, the same reasoning `EvidenceOptions` and `SignOptions` apply to their own transport fields |

None of these six reach `MakeBundle`; `BundleOptions()` carries what the
bundler itself reads.

One asymmetry worth knowing: `IgnoreTLog` has no config counterpart, so
`BundleVerifyOptions()` always leaves it false. It weakens the trust floor by
dropping the transparency-log requirement, and keeping it command-line-only
means a checked-in file can never silently disable that check.

## Verifying artifacts

Every artifact AICR produces can be checked back through the same facade,
so an integrator never has to reach into `pkg/bundler/verifier`,
`pkg/evidence/verifier`, or `pkg/recipe/catalog` to establish trust.

### Verifying a bundle

`VerifyBundle` checks a bundle's checksums and attestation chain, then
evaluates the policy assertions you supply:

```go
verification, err := client.VerifyBundle(ctx, "./my-bundle", aicr.BundleVerifyOptions{
	MinTrustLevel:  "verified",
	RequireCreator: "release@nvidia.com",
})
if err != nil {
	log.Fatal(err) // could not verify: missing bundle, bad trust root, bad options
}
if verification.PolicyFailure != "" {
	log.Fatalf("policy not met: %s", verification.PolicyFailure)
}
if len(verification.Report.Errors) > 0 {
	log.Fatalf("verification failed: %v", verification.Report.Errors)
}
log.Printf("trust level %s, created by %s",
	verification.Report.TrustLevel, verification.Report.BundleCreator)
```

A failed policy is **data, not an error**: the call still returns the
full `Report` so you can log or render why the bundle fell short. A
non-nil error means verification could not run at all.

`MinTrustLevel` is the one field whose empty value is not "no
constraint". Leaving it empty means `"max"` — auto-detect the highest
level this bundle could achieve and require it. Naming a level
explicitly (`aicr.TrustLevels()` returns the valid values) can *lower*
the floor as readily as raise it.

Verification is offline: the chain resolves against the locally cached
or embedded Sigstore trusted root. The one network path is a KMS URI in
`Key`, which makes a live `GetPublicKey` call.

`BundleVerifyOptions` mirrors the [`spec.verify`](../user/cli-config.md#specverify)
section of an `AICRConfig` field-for-field — the first three fields come from
`spec.verify.trust`, the next three from `spec.verify.policy` — so a
team that has standardized on a committed policy can populate this
struct without a translation table. `IgnoreTLog` deliberately has no
config counterpart: it drops the transparency-log requirement, and
keeping it out of the schema means a checked-in file can never silently
disable that check.

### Verifying evidence

`VerifyEvidence` checks a recipe-evidence bundle's signature and hash
chain. The input is auto-detected as a pointer file, an OCI reference,
or an unpacked directory:

```go
result, err := client.VerifyEvidence(ctx, aicr.EvidenceVerifyOptions{
	Input: "recipes/evidence/h100-eks-training/eks/sha256-abc.yaml",
})
if err != nil {
	log.Fatal(err) // verification could not be attempted
}

switch result.Exit {
case aicr.EvidenceExitValidPassed:
	log.Printf("valid: %s", result.RecipeName)
case aicr.EvidenceExitValidPhaseFailures:
	log.Printf("evidence sound, but recorded phases failed")
case aicr.EvidenceExitInvalid:
	log.Fatalf("bundle invalid")
case aicr.EvidenceExitIncomplete:
	if result.FailureCause != nil && result.FailureCause.Class == aicr.EvidenceCauseCanceled {
		log.Fatalf("run canceled before a verdict")
	}
	log.Fatalf("could not read the bundle (storage or registry fault)")
}

fmt.Print(aicr.RenderEvidenceMarkdown(result))
```

An invalid bundle is a verdict, not an error — branch on `Exit`. The
`EvidenceExitIncomplete` case is the one worth handling separately: it
means "we could not check this", which is different from "we checked it
and it failed".

Pair it with `RecipeDigest` to detect evidence that has gone stale
against the recipe on your branch:

```go
current, err := client.RecipeDigest(ctx, aicr.RecipeDigestOptions{
	Path: "recipes/overlays/h100-eks-training.yaml",
})
if err != nil {
	log.Fatal(err)
}
if result.Predicate.Recipe.Digest != current {
	log.Fatal("evidence is stale: the recipe changed since it was signed")
}
```

### Verifying the recipe catalog

`VerifyCatalog` recomputes the deterministic digest over the Client's
recipe catalog and verifies it against the Sigstore bundle shipped as
the `recipe-catalog.sigstore.json` release asset:

```go
catalog, err := client.VerifyCatalog(ctx, "./recipe-catalog.sigstore.json",
	aicr.CatalogVerifyOptions{})
if err != nil {
	log.Fatalf("catalog verification failed: %v", err)
}
log.Printf("catalog sha256:%s signed by %s", catalog.Digest, catalog.Identity)
```

The digest is computed over **this Client's** `DataProvider`, not the
process-wide embedded catalog. A Client built on `EmbeddedSource()`
verifies the catalog NVIDIA signed. A Client whose source layers
external data over the embedded tree is verifying different content, so
verification will not match the released signature — that is the
correct answer to "is the catalog I am resolving against the signed
one", not a bug.

### Verifying the binary

`VerifyBinaryAttestation` proves an `aicr` binary was built by NVIDIA
CI. It is package-level rather than a `Client` method because it
involves no recipe catalog and no configurable policy:

```go
identity, err := aicr.VerifyBinaryAttestation(ctx, aicr.BinaryAttestationVerifyOptions{
	Attestation:  attestationBytes, // raw Sigstore bundle
	BinaryDigest: rawSHA256,        // raw bytes, not hex
})
```

Passing bytes rather than a path is deliberate: it lets you verify the
exact content you are about to use, with no verify-then-reread window.
Override the pinned identity with `IdentityRegexp` (defaults to
`aicr.TrustedIdentityPattern`); `aicr.ValidateIdentityPattern`
pre-validates operator-supplied input against the same rule the verify
entry points apply internally.

An override must be *confined* to the NVIDIA repository, not merely
mention it. Two rules enforce that, and they are load-bearing together:
the pattern must **begin with** `https://github.com/NVIDIA/aicr/` (a
leading `^` is allowed, and `github\.com` is accepted too), and it must
not use **top-level alternation**. Both exist because the identity
matcher pins only the OIDC issuer beyond this pattern, so a widened
pattern silently degrades the gate to "any GitHub Actions workflow in
any repository" rather than failing visibly.

```go
// Rejected: begins with the prefix, but the second branch matches
// anything — only one branch of an alternation has to match.
aicr.ValidateIdentityPattern(`^https://github\.com/NVIDIA/aicr/.*|.*$`)

// Rejected: the alternation is nested in a group, so the pattern no
// longer begins with the prefix and one branch escapes the repository.
aicr.ValidateIdentityPattern(
	`(https://github.com/NVIDIA/aicr/.*|https://github.com/attacker/x/.*)`)

// Accepted: alternatives sit AFTER the prefix, so every branch is
// already behind the pin.
aicr.ValidateIdentityPattern(
	`^https://github\.com/NVIDIA/aicr/\.github/workflows/(on-tag|release)\.yaml@.*`)
```

## Signing artifacts

The producing half of the supply chain is on the facade too.
`EmitRecipeEvidence` builds a bundle from a completed validation run;
`PublishEvidence` signs and pushes one that already exists on disk:

```go
err := client.PublishEvidence(ctx, aicr.EvidencePublishOptions{
	BundleDir: "./out",
	Push:      "ghcr.io/myorg/aicr-evidence",
})
```

Splitting emit from publish lets the cluster-bound step run where the
cluster is reachable and the Sigstore-bound step run where Fulcio and
Rekor are. The result is content-identical to the one-shot path.

`SignCatalog` is the counterpart to `VerifyCatalog`, signing this
Client's catalog and returning the serialized Sigstore bundle.

**`SignCatalog` rejects the signing modes it can tell `VerifyCatalog`
will not verify.** Verification checks against the public-good Sigstore
root, requires a transparency-log entry, and accepts keyless GitHub OIDC
certificates only, so these four `OIDCResolve` settings are rejected
with `ErrCodeInvalidRequest` before any signing work runs:

| Setting | Why it is rejected |
|---|---|
| `SigningKey` | A key-signed catalog has no verification path at all. |
| `FulcioURL` | A private CA's certificate does not chain to the public-good root. |
| `RekorURL` | A private log's entries do not verify against the public-good root either. A public-good v1 URL would verify, but the two are indistinguishable from the URL alone, so this fails closed. |
| `DisableTLogUpload` | Verification requires a transparency-log entry. |

The point of the guard is that you should not be able to sign a catalog
successfully and then discover the documented counterpart refuses it;
if private catalog signing is ever needed, both halves move together.

**`SigningConfigPath` is checked rather than rejected.** It passes
through because the release path requires it — naming the public-good
Rekor v2 target is its normal use — but a Sigstore signing config can
itself name a private Fulcio or Rekor, which would have made the four
rejections above bypassable by moving the same endpoints into a file. So
the config is loaded and every Fulcio, Rekor, OIDC provider, and
timestamp-authority URL in it must sit under the `sigstore.dev` domain;
anything else fails with `ErrCodeInvalidRequest` naming the offending
endpoint. The check is on the domain, not a list of exact URLs, because
the public-good Rekor shards carry the year in their hostname
(`log2025-1.rekor.sigstore.dev`) and an exact-URL allowlist would start
rejecting legitimate signing at the next rotation. Matching is on a
label boundary, so a lookalike domain such as `evilsigstore.dev` is
rejected, and every URL must be HTTPS — the public-good services are
HTTPS-only, and these URLs are handed to the Rekor and timestamp
clients as-is, so `http://rekor.sigstore.dev` would pass a
hostname-only check while sending signing traffic in the clear.

The config that passes this check is the config signed with. `SignCatalog`
hands the parsed value to the signing path rather than letting it re-read
the file, so there is no window in which the file changes between the
check and the use.

Neither signing method imposes a facade timeout, unlike their
verification counterparts: keyless OIDC can block on a human completing
a browser or device-code flow. Pass a context with a deadline for
unattended use. Neither prompts — interactive signing disclosure is a UI
concern the caller owns, so both can run unattended from a server.

## Errors

All errors returned by the facade are `*pkg/errors.StructuredError`
values carrying an `ErrorCode`. Match on the code with `errors.Is` —
`StructuredError.Is` reports a match when the target is a `StructuredError`
with the same code, so this works through wrap chains:

```go
import (
	stderrors "errors"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

_, err := client.ResolveRecipe(ctx, req)
switch {
case stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")):
	// handle invalid input
case stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeNotFound, "")):
	// handle missing recipe
}
```

Runnable version: [`Example_errorCodes`](https://pkg.go.dev/github.com/NVIDIA/aicr/pkg/client/v1#example-package-ErrorCodes).

Reach for `errors.As` only when you need the error's *payload* rather than
its class — `se.Context`, which carries structured detail such as a coverage
failure's `uncovered` dimensions:

```go
var se *aicrerrors.StructuredError
if stderrors.As(err, &se) {
	uncovered := se.Context["uncovered"]
	_ = uncovered
}
```

## Context handling

`ResolveRecipe` (and every other context-aware facade method) honors
context cancellation. Capped entry points wrap the caller's context
with `context.WithTimeout` against their per-operation cap; the
effective deadline is then the smaller of the caller's deadline and the
facade cap, per `context.WithTimeout` semantics — a caller passing a
tighter deadline keeps it; a caller passing `context.Background()` gets
the facade cap.

Not every entry point is capped. `PublishEvidence` and `SignCatalog`
never are, and `MakeBundle` is not when `BundleOptions.Timeout` is `0`
(its default). Those run under the caller's context unchanged, so a
caller passing `context.Background()` gets no deadline at all.

Per-operation caps:

- `ResolveRecipe` / `BundleComponents`: `defaults.RecipeOperationTimeout`
- `LoadSnapshot`: `defaults.SnapshotLoadTimeout` — bounds the whole
  load whatever the source: a local file read, an HTTP(S) fetch, or a
  `cm://` ConfigMap read against the Kubernetes API. Distinct from
  `SnapshotOperationTimeout` below, which bounds deploying an agent Job.
- `DiffSnapshots`: **no facade cap** — comparison is in memory and the caller's
  context governs unchanged.
- `CollectSnapshot`: caller-controlled via `AgentConfig.Timeout` (falling
  back to `defaults.SnapshotOperationTimeout` when unset), plus
  `defaults.SnapshotOperationGrace`. The grace exists because
  `AgentConfig.Timeout` budgets Job *completion* only — deployment and
  result retrieval sit outside it, so a bare cap would silently shrink the
  completion budget you asked for.
- `ValidateState`: `defaults.ValidationOperationTimeout`
- `VerifyBundle` / `VerifyEvidence` / `VerifyCatalog` / `RecipeDigest`:
  `defaults.VerifyOperationTimeout` by default, overridable per call via the
  `Timeout` field on each options struct. `nil` (the zero value) keeps the
  default cap; a pointer to `0` imposes **no** facade cap and runs under the
  caller's context unchanged; a positive value sets an explicit cap.

  The pointer is what makes `0` mean uncapped here, matching
  `WithValidationTimeout(0)`, while leaving the zero value as today's
  behavior. Reach for it when a registry is slow rather than broken:
  `VerifyEvidence` pulls from the network, and a cap breach arrives as an
  **error** rather than the `EvidenceExitIncomplete` verdict a CI gate
  branches on — so a slow pull, which is exactly what that verdict describes,
  otherwise reports through the wrong channel. The override can only relax
  the facade ceiling; the caller's own deadline still applies.
- `PublishEvidence` / `SignCatalog`: **no facade cap** — the caller's
  context governs unchanged. Keyless OIDC can block on a human
  completing a browser or device-code flow, so a fixed cap would cut
  short a run that legitimately waits.
- `MakeBundle`: opt-in via `BundleOptions.Timeout`. When unset (`0`) the
  caller's context governs unchanged — large bundles, `--vendor-charts`,
  and attestation/signing can exceed any fixed cap. The REST `/v1/bundle`
  handler sets it to `defaults.BundleHandlerTimeout`; the CLI `bundle`
  command leaves it `0`.

Passing a `nil` `context.Context` returns `ErrCodeInvalidRequest`. Use
`context.Background()` (or a deadline-bounded child) for unbounded callers.

## The integrator contract

Four commitments, stated plainly, so you know what you are depending on.

**Import `pkg/client/v1`. That is the contract.** Everything else under
`pkg/*` stays importable, but only this package is compatibility-reviewed, and
only its exported surface is checked by the API-diff gate on every PR. The
[stability matrix](./public-api.md#stability-tiers) tiers each package;
`Internal` packages will break you on upgrade.

**When the facade is missing something, tell us instead of routing around
it.** [Open an issue](https://github.com/NVIDIA/aicr/issues/new/choose)
describing the capability. Reaching into an evolving subpackage works today
and is the thing most likely to break you later, and we would rather extend
the facade — that is how `LoadSnapshot`, `LoadConfig`,
`Client.CriteriaFromSnapshot`, and the verification surface all arrived.

**Breaking changes are detected, not merely intended.** `tools/api-diff`
compares the facade and its transparent-alias targets against the last release
on every PR; an incompatible change fails CI and requires a recorded, reviewed
exception. That is a mechanical guarantee, not a policy promise — but note
what it does *not* cover: behavior. A function keeping its signature while
changing what it does passes the gate.

**The examples are compiled.** Every entry in the [examples
table](#runnable-examples) builds in AICR's own test suite, so a facade change
that invalidates one fails here first. Scope that honestly: it covers those
examples, not this page's prose or its shorter inline snippets, and for the
majority it proves compilation rather than runtime behavior.

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

### How you learn something is going away

Nothing in `pkg/client/v1` is removed without first being marked deprecated for
the notice period in
[`RELEASING.md`](https://github.com/NVIDIA/aicr/blob/main/RELEASING.md#deprecation-policy)
— two minor releases before v1.0, and after v1.0 the next major.

The marker is a standard Go `// Deprecated:` godoc paragraph on the identifier:

```go
// ResolveRecipe returns a resolved recipe for the given criteria.
//
// Deprecated: use [Client.Resolve] instead. ResolveRecipe is removed in v0.25.
func (c *Client) ResolveRecipe(...) { ... }
```

This is deliberately not a runtime warning. `staticcheck` reports `SA1019` for
every use of a deprecated identifier, so the notice arrives in your build — at
the point you can act on it — rather than in a log line from a production run.
`go doc`, `gopls`, and every major Go IDE surface the same paragraph. If you run
`staticcheck` (or `golangci-lint` with the `staticcheck` linter enabled) in CI,
you get the deprecation channel for free with no AICR-specific tooling.

Each marker names the replacement and the removal release. The complete list of
active deprecations across all surfaces is in
[Deprecations](../user/deprecations.md).

## See also

- [Public API surface](./public-api.md) — stability matrix per package
- [Automation guide](./automation.md) — CI integration patterns
- [Recipe development](./recipe-development.md) — authoring recipes

[semver]: https://semver.org/spec/v2.0.0.html

### What `ValidateSettings()` does and does not carry

`spec.validate` is the one section that does not map to a single destination,
so `ValidateSettings()` carries settings from both `spec.validate.agent` and
`spec.validate.execution` as a plain value, not an option slice, so you read
and override individual fields directly — but not every field it carries
reaches `Client.ValidateState`.
Nine do, via a matching `WithValidation*` option: namespace, image pull
secrets, node selector, tolerations, no-cluster, cleanup, phases, fail-fast,
and timeout. Four do not: image, job name, service account name and
require-GPU have no `WithValidationImage`, `WithValidationJobName`,
`WithValidationServiceAccountName` or `WithValidationRequireGPU` for
`ValidateState` to accept them through. They ride on `ValidateSettings()`
anyway so a caller building its own validator agent config (the CLI's
`parseValidateAgentConfig`, in particular) can read them with its own
flag-over-config precedence instead of reaching for `Unwrap()` — see the
table below.

```go
opts, ok, err := cfg.ValidateSettings()
if err != nil {
    return err
}
if !ok {
    opts.Cleanup = true // no spec.validate at all: supply a safe default
}
results, err := client.ValidateState(ctx, rec, snap,
    aicr.WithValidationNamespace(opts.Namespace),
    aicr.WithValidationImagePullSecrets(opts.ImagePullSecrets),
    aicr.WithValidationNodeSelector(opts.NodeSelector),
    aicr.WithValidationTolerations(opts.Tolerations),
    aicr.WithValidationNoCluster(opts.NoCluster),
    aicr.WithValidationCleanup(opts.Cleanup),
)
```

**The second return is a presence signal, not decoration — skipping it is the
failure mode.** The zero value's `Cleanup: false` is not a safe default: it is
the opposite of the CLI's own default (clean up). A caller that cannot
distinguish "no `spec.validate` at all, supply your own defaults" from
"`spec.validate` is present but silent about cleanup", and always applies the
returned value as-is, leaves the cluster-admin `ClusterRoleBinding` and
validator Jobs active on a plain, no-config invocation — silently, since
nothing errors. That is why the sample above guards the default rather than
the override: `ok` must gate what `Cleanup` becomes when the section is
absent, not whether the caller's own choice is applied — the two look similar
but only one avoids the leak. `ok` is `true` when the section exists at all
(even if silent about every field) and `false` for a nil `Config`, a nil
internal document, or a document that omits the section entirely.
`SnapshotAgentConfig()` returns the same signal for the same reason, guarding
`Privileged` instead of `Cleanup`; see
[below](#what-snapshotagentconfig-does-and-does-not-carry).

The rest of the section has other homes, and knowing which saves a search:

| Field | Home |
|---|---|
| `spec.validate.agent.image`, `.jobName`, `.serviceAccountName`, `.requireGpu` | Still on `ValidateSettings()` (`Image`, `JobName`, `ServiceAccountName`, `RequireGPU`), but not passed to `ValidateState` — `pkg/validator` exposes no option for any of them, so a `WithValidation*` here would have nothing to translate into. The CLI reads them directly to build the validator's own agent Job. |
| `spec.validate.input.recipe`, `.snapshot`, `spec.validate.execution.failOnError` | `ValidateInputOptions()`, which targets the CALLER rather than `ValidateState` — see below. |
| `spec.validate.evidence.attestation` | `EvidenceAttestationOptions()`, which targets `EmitRecipeEvidence` rather than `ValidateState` — see below. |
| `spec.validate.evidence.cncf` | `CNCFEvidenceOptions()`, which targets the CALLER — there is no `Client.Emit*` for CNCF AI Conformance evidence — see below. |

One inversion worth knowing: config says `noCleanup`, the field says
`Cleanup`. `ValidateSettings()` flips it, so `noCleanup: true` becomes
`Cleanup: false`.

### What `ValidateInputOptions()` does and does not carry

`ValidateState` takes an already-resolved recipe and snapshot and reports
check results without acting on them, so the three `spec.validate` fields a
CALLER needs — which recipe and snapshot to validate, and whether a failed
check should fail the caller — have no home on `ValidateSettings()`.
`ValidateInputOptions()` carries them instead, so a caller applying its own
flag-over-config precedence does not need `Unwrap()` to read them:

```go
input, err := cfg.ValidateInputOptions()
if err != nil {
    return err
}
rec, err := client.LoadRecipe(ctx, input.RecipePath, "")
```

| Field | Source |
|---|---|
| `RecipePath` | `spec.validate.input.recipe` |
| `SnapshotPath` | `spec.validate.input.snapshot` |
| `FailOnError` | `spec.validate.execution.failOnError` — a pointer so "config said nothing" stays distinct from an explicit `false`, letting the caller's own default apply, the same pattern the CLI's `--fail-on-error` flag uses to win over a configured value |

None of these three reach `ValidateState`; `ValidateSettings()` carries what
the validator itself accepts.

### What `EvidenceAttestationOptions()` does and does not carry

`spec.validate.evidence` carries two kinds of evidence. This method covers one
of them, and the name says which, so it cannot quietly grow to imply both:

```go
opts, ok, err := cfg.EvidenceAttestationOptions()
if err != nil {
    return err
}
if ok {
    opts.Commit = buildCommit  // caller-owned, no config counterpart
    err = client.EmitRecipeEvidence(ctx, rec, snap, results, opts)
}
```

**`out` is the enable gate, not a zeroing gate.** An empty `out` leaves the
path off (`ok == false`) even when `bom`/`push`/`plainHTTP`/`insecureTLS` are
set, matching the spec field's own contract and what the CLI does — but those
four still populate on the returned `EvidenceOptions`; only `OutDir` stays
empty. `out` can come from somewhere other than this document — the CLI's
`--emit-attestation` flag can supply it while `bom`/`push` are configured —
so zeroing the whole struct whenever `out` is empty would silently drop that
half of the configuration on every run where `out` arrives another way. So
`ok == false` means "config didn't enable the path," never "misconfigured"
and never "config said nothing else" — a malformed section returns an error
instead. That is why there is a `bool` at all: `EmitRecipeEvidence` rejects an
empty `OutDir`, so a zero-value `EvidenceOptions` could not tell you which of
the two happened, and it also could not carry a partially-configured section
for a caller resolving `out` itself to finish.

Five fields project: `out`, `bom`, `push`, `plainHTTP`, `insecureTLS`. The
rest of `EvidenceOptions` stays yours, and the reasons differ:

| Field | Why it is not derived |
|---|---|
| `Commit` | Names the running binary, not the document. It selects the validator catalog the bundle's BOM is built against. Set it after deriving. |
| `OIDCResolve` | Excluded by the spec itself. A keyless-signing identity token is a short-lived secret and must not sit in a version-controlled file; resolve it at sign time. |
| `NoSign`, `Full` | Command-line-only, for the same reason as `IgnoreTLog` and `failOnError`. Both weaken the **artifact** — `NoSign` pushes an unsigned bundle, `Full` ships unredacted payloads — and a checked-in file that can silently disable signing is a supply-chain downgrade no reviewer would see in a diff. |

**Why `plainHTTP` and `insecureTLS` project anyway.** They weaken a run too, so
the rule above is not "config may never weaken anything" — stated that broadly
it would be contradicted by two of the five fields that do project. The line is
the *artifact* versus the *hop*.

Both configure the transport to a registry the same document already names in
`push`. A document trusted to choose the destination is trusted to describe how
to reach it, which is why `EvidenceOptions` and `SignOptions` carry these while
the bundler's own options do not — `MakeBundle` never reaches a registry (see
[`spec.bundle.registry`](#what-bundleoptions-does-and-does-not-carry)).

Neither field changes what the bundle attests or whether it is signed, and that
is structural rather than a promise: both reach only the OCI transport, never
the Fulcio/Rekor signing call and never predicate or redaction construction.

How the subject digest is pinned differs by path, and neither path reads it back
from the weakened hop. Emit-and-push binds the digest computed locally while
packaging, before any push begins. Signing an already-pushed artifact
(`aicr evidence sign`) resolves the digest at pull time instead, but the pull is
content-addressed and the materialized digest is checked for equality against
the value the original packaging run recorded, failing closed on mismatch.

So a tampered hop can corrupt or break the transfer; it cannot make the
signature vouch for content that was never packaged.

That is narrower than "harmless". A committed `plainHTTP` or `insecureTLS` does
weaken that hop, and it widens the threat model rather than just restating it:
redirecting `push` needs a malicious document, whereas downgrading TLS on a
destination the operator believes is protected only needs someone on the
network path. It is accepted because the destination is already the document's
call. Treat it as a reviewable transport decision, not as evidence that
excluding `NoSign` and `Full` is arbitrary.

### What `CNCFEvidenceOptions()` does and does not carry

`spec.validate.evidence` carries two kinds of evidence; `CNCFEvidenceOptions()`
covers the other one — CNCF AI Conformance markdown, gated behind
`--evidence-dir` / `--cncf-submission` / `--feature`:

```go
cncfOpts, err := cfg.CNCFEvidenceOptions()
if err != nil {
    return err
}
evidenceDir := cncfOpts.Dir // caller applies its own flag-over-config precedence
```

Unlike `EvidenceAttestationOptions()`, this one has no `Client.Emit*`
counterpart at all: there is no `Client.EmitCNCFEvidence` for
`CNCFEvidenceOptions()` to feed. The caller — the CLI's
`validateFlagCombinations`, `cncf.New`, and `runCNCFSubmission` — consumes the
three fields (`Dir`, `CNCFSubmission`, `Features`) directly, applying its own
flag-over-config precedence the same way `SnapshotOutputOptions()` and
`ValidateInputOptions()` already do for their own caller-consumed fields.
`CNCFEvidenceOptions()` returns the zero value (never an error) for an absent
section, and an error only when `spec.validate` is present but malformed.

### What `SnapshotAgentConfig()` does and does not carry

`AgentConfig`'s fields are exported, so unlike the bundle path there is no
options slice — derive it, then set any field directly. The returned
`*AgentConfig` is never nil, even for a nil `Config`:

```go
agent, ok, err := cfg.SnapshotAgentConfig()
if err != nil {
    return err
}
agent.Kubeconfig = kubeconfigPath // caller-owned, no config counterpart —
                                  // set unconditionally, not gated on ok
if !ok {
    agent.Privileged = true // no spec.snapshot at all: supply a safe default
}
snap, err := client.CollectSnapshot(ctx, agent)
```

Three mappings are transforms rather than copies, and two of them fail
silently if you reimplement them by hand:

| Field | Behavior |
|---|---|
| `noCleanup` → `Cleanup` | **Inverted**, same as `spec.validate` |
| `privileged` → `Privileged` | **Defaults to true** when config says nothing. The resolved field is a pointer so unset stays distinct from an explicit `false`; treating nil as `false` drops privileges the collector needs, and it surfaces as missing data rather than an error |
| `requests`, `limits` | Parsed from raw `name=quantity,...` strings. `Resolve()` deliberately leaves them unparsed, so a malformed value errors here instead of becoming an empty `ResourceList` |

The whole `spec.snapshot.output` section is **not** projected, and that is
deliberate. Output describes *delivery*; `AgentConfig` describes the collection
Job.

- `output.format` is applied at delivery. The Job always stages YAML in a
  ConfigMap, so a format routed through `AgentConfig` would be silently ignored
  (#2398).
- `output.path` and `output.template` are **not** `AgentConfig.Output` and
  `.TemplatePath`. Any `Output` value that is not a `cm://` URI stages to an
  internal ConfigMap and delivery becomes yours, so projecting a file path
  there would look configured and write nothing.

Deliver with `snapshotter.DeliverSnapshot`, passing `Snapshot.Raw`.

`OS` is parsed through the criteria registry rather than copied, matching what
the CLI does with `--os`. An unparsed `Talos` would miss the agent's exact
`talos` check and select incompatible host mounts, and an undocumented value
errors here instead of traveling.

`Kubeconfig`, `Debug`, `ClusterConfigPath`, `AKSGPUPoolsPath`,
`DiscoverNetwork`, `RunID` and `NameBase` have no config counterpart and stay
zero — they are per-invocation or caller-owned.

**A document with no `spec.snapshot` yields a zero value, which is not a
working configuration** — `Privileged` is false, which the collector generally
needs true. That is deliberate: defaults apply when the section exists and is
silent about a field, but a document that made no snapshot decisions at all
does not get decisions invented for it. Supply your own defaults in that case,
as the CLI does from its flag defaults.

**The second return, `ok`, is the presence signal that resolves this — skip it
and the failure mode is silent.** `ok` is `true` when `spec.snapshot` exists at
all (even if silent about every field) and `false` for a nil `Config`, a nil
internal document, or a document that omits the section. A caller that always
applies the returned `*AgentConfig` as-is, without checking `ok`, cannot tell
"no `spec.snapshot`, supply your own defaults" from "`spec.snapshot` decided
every field, apply them as-is" — both produce a populated, non-nil
`*AgentConfig` — and silently drops privileges the collector needs on the
common no-config case. `ValidateSettings()` returns the same signal for the
same reason, guarding `Cleanup` instead of `Privileged`; see
[above](#what-validatesettings-does-and-does-not-carry).

### What `SnapshotOutputOptions()` does and does not carry

`CollectSnapshot` never reads `spec.snapshot.output` — the whole section
describes *delivery*, which `SnapshotAgentConfig()` deliberately excludes (see
above): the Job always stages YAML in a ConfigMap, so a format routed through
`AgentConfig` would be silently ignored (#2398). `SnapshotOutputOptions()`
carries the three fields a caller needs AFTER `CollectSnapshot` returns, to
write the snapshot where the document asked:

```go
out, err := cfg.SnapshotOutputOptions()
if err != nil {
    return err
}
err = snapshotter.DeliverSnapshot(ctx, snap.Raw, snapshotter.SnapshotDelivery{
    Output:       out.Path,
    Format:       serializer.Format(out.Format),
    TemplatePath: out.Template,
})
```

| Field | Source |
|---|---|
| `Path` | `spec.snapshot.output.path` |
| `Format` | `spec.snapshot.output.format` (`yaml`, `json`, or `table`), validated by the loader |
| `Template` | `spec.snapshot.output.template`, a Go template rendered instead of the structured formats; requires `Format` `yaml` |

Returns the zero value — never an error — for a nil `Config`, an absent
`spec.snapshot`, or an absent `output` block, since delivery is optional. None
of these three reach `CollectSnapshot`; `SnapshotAgentConfig()` carries what
the collection Job itself reads.

### What `RecipeOutputOptions()` does and does not carry

`ResolveRecipe` and `LoadRecipe` never see `spec.recipe.output` either, for the
same reason `CollectSnapshot` never sees `spec.snapshot.output`: writing the
resolved recipe is a caller decision made AFTER resolution returns, not part
of resolving it. `RecipeOutputOptions()` carries the two fields:

```go
out := cfg.RecipeOutputOptions()
if out.Path != "" {
    // write rec to out.Path in out.Format, mirroring `aicr recipe --output`
}
```

| Field | Source |
|---|---|
| `Path` | `spec.recipe.output.path`. Empty when unset. |
| `Format` | `spec.recipe.output.format`. Empty when unset, leaving the caller's own default in place. |

Unlike every other derivation on `Config`, `RecipeOutputOptions()` returns no
error: the underlying accessors are nil-safe and perform no parsing, so
nothing here can fail. A nil `Config` or an absent `spec.recipe.output` each
yield the zero value.
