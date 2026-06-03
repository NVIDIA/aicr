# Snapshot Collectors

A **collector** captures one dimension of system state — Kubernetes API,
GPU hardware, OS release, systemd services, node topology — and emits a
single `*measurement.Measurement`. Collectors run during `aicr snapshot`
on a workstation, or inside the in-cluster snapshot agent Job.
The orchestrator (`pkg/snapshotter`) fans collectors out in parallel
under `errgroup.WithContext`; the result is a flat `[]*Measurement`
inside the resolved snapshot artifact.

The boundary is hard: **collectors are read-only**. They observe state;
they never `Create`, `Update`, `Delete`, `Apply`, `Patch`, exec into
pods, or mutate the host. Anything that mutates is a validator (see
[validator.md](validator.md)), not a collector.

This page is for contributors adding a new collector. End-user
snapshot semantics live in
[docs/user/cli-reference.md](../user/cli-reference.md#aicr-snapshot).

## Where Collectors Live

All collectors live under
[`pkg/collector/<kind>/`](https://github.com/NVIDIA/aicr/tree/main/pkg/collector).
Each subdirectory is one collector; one collector emits one
`measurement.Type`.

| Kind | Package | Emits | Notes |
|------|---------|-------|-------|
| GPU | `pkg/collector/gpu` | `TypeGPU` | Two subtypes: `hardware` (NFD/PCI) + `smi` (nvidia-smi XML). Degrades to zero-GPU when driver missing. |
| Kubernetes | `pkg/collector/k8s` | `TypeK8s` | Server version, image info, network policy, node-local info. Uses the singleton `pkg/k8s/client`. |
| OS | `pkg/collector/os` | `TypeOS` | Subtypes for `release` (`/etc/os-release`), `grub`, `kmod`, `sysctl`. |
| SystemD | `pkg/collector/systemd` | `TypeSystemD` | D-Bus probe of configured services. Routes to Talos via factory when `os: talos`. |
| Topology | `pkg/collector/topology` | `TypeNodeTopology` | Cluster-wide taints and labels across all nodes — see [Cross-cutting topology collector](#cross-cutting-topology-collector). |
| Talos | `pkg/collector/talos` | `TypeSystemD`, `TypeOS` | OS-specific override pair: a single shared config so one Node API fetch serves both collectors. |
| File (helper) | `pkg/collector/file` | — | Not a registered collector. A reusable parser for delimited key=value config files (used by the OS subcollectors). |

The mapping from collector to `measurement.Type` is one-to-one for
all collectors except Talos, which substitutes for systemd and os in
the factory when the OS criteria is `talos`.

## Collector Interface

The interface is in
[`pkg/collector/types.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/collector/types.go):

```go
type Collector interface {
    Collect(ctx context.Context) (*measurement.Measurement, error)
}
```

Two rules:

- **Context-cancellable.** Every `Collect` must honor `ctx`. Long
  loops check `ctx.Done()`. Outbound API calls take `ctx` directly.
- **One Measurement out.** Return `*measurement.Measurement` with
  `Type` set and `Subtypes` populated. Returning `nil` plus an error is
  fine on hard failure; returning a partial measurement with a logged
  warning is fine on graceful degradation (the GPU collector models
  this — when nvidia-smi is absent, it emits a zero-GPU `smi`
  subtype rather than failing).

## Registration via the Factory

Collectors are wired in
[`pkg/collector/factory.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/collector/factory.go).
`Factory` exposes one `Create...` method per collector kind; the
`DefaultFactory` constructs the production collector for each:

```go
type Factory interface {
    CreateSystemDCollector() Collector
    CreateOSCollector() Collector
    CreateKubernetesCollector() Collector
    CreateGPUCollector() Collector
    CreateNodeTopologyCollector() Collector
}
```

`pkg/snapshotter` calls these methods inside `errgroup.WithContext` —
it does not import collector subpackages directly. To add a new
collector kind, extend the `Factory` interface, add a constructor on
`DefaultFactory`, and add a `g.Go(collectSafe(..., factory.CreateXxx()))`
line in the snapshotter's `measure` function.

There is no `init()`-based self-registration. Adding a collector is
explicit — both factory and snapshotter must reference it, which is the
trade-off for making the parallel fan-out static and trivially testable.

## Context and Timeouts

Every collector must bound its own execution. The pattern at the top
of `Collect`:

```go
func (c *Collector) Collect(ctx context.Context) (*measurement.Measurement, error) {
    ctx, cancel := context.WithTimeout(ctx, defaults.CollectorTimeout)
    defer cancel()
    // ...
}
```

`defaults.CollectorTimeout` is **10s** — the default for any
host-local collector. Two collectors override:

| Collector | Constant | Value | Rationale |
|-----------|----------|-------|-----------|
| Kubernetes | `defaults.CollectorK8sTimeout` | 60s | API server round trips, in-cluster auth setup. |
| Topology | `defaults.CollectorTopologyTimeout` | 90s | Cluster-wide node pagination on large fleets. |

Use the parent deadline if it is sooner — the GPU collector shows the
pattern (`time.Until(deadline) < timeout`). Long-lived watches do not
belong in a collector: collectors are **one-shot**. If you need a
watch, you are writing a validator or a controller, not a collector.

## Adding a New Collector — Walkthrough

End-to-end, the smallest viable patch:

1. **Create the package.** `pkg/collector/<kind>/<kind>.go` with a
   `Collector` struct and any options as
   `pkg/defaults`-backed fields. Constructor returns the interface
   type, not the concrete struct.
2. **Implement `Collect`.** First line: `ctx, cancel :=
   context.WithTimeout(ctx, defaults.CollectorTimeout); defer cancel()`.
   Then read state and build subtypes. Use
   `measurement.NewSubtypeBuilder(name)` and
   `measurement.NewMeasurement(type).WithSubtypes(...).Build()` from
   [`pkg/measurement/builder.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/measurement/builder.go).
3. **Add a `measurement.Type` if the dimension is new.** Append the
   constant in
   [`pkg/measurement/types.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/measurement/types.go)
   (`TypeXxx`) and to the `Types` slice. Recipe constraints address
   measurements by type — leave this out and your data is unreachable.
4. **Extend the factory.** Add a `CreateXxxCollector() Collector`
   method on `Factory` and `DefaultFactory` in
   [`pkg/collector/factory.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/collector/factory.go).
5. **Wire into snapshotter.** Add one
   `g.Go(collectSafe(gctx, "<kind>", n.Factory.CreateXxxCollector()))` line in
   [`pkg/snapshotter/snapshot.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/snapshotter/snapshot.go).
6. **Test.** `<kind>_test.go` with table-driven tests. Use
   `k8s.io/client-go/kubernetes/fake` for K8s collectors. Cover the
   happy path, the missing-dependency degradation path, and a
   `context.Cancel` case.
7. **Update docs.** Add the row to
   [docs/user/cli-reference.md](../user/cli-reference.md) if the
   snapshot output schema gains a new top-level entry, and to this
   page's [Where Collectors Live](#where-collectors-live) table.

## Measurement Schema

```go
type Measurement struct {
    Type     Type
    Subtypes []Subtype
}

type Subtype struct {
    Name    string
    Data    map[string]Reading
    Context map[string]string
}
```

`Reading` is a typed-scalar interface implemented by
`Scalar[T]` (`Int`, `Int64`, `Uint`, `Uint64`, `Float64`, `Bool`,
`Str`). Use the helpers in
[`pkg/measurement/types.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/measurement/types.go)
— never store raw `any`.

**The `reading.Any()` JSON gotcha.** When a snapshot is read from
disk, JSON decoders deliver integer values as `float64`. Any
type-switch on `reading.Any()` must handle `int`, `int64`, **and**
`float64`. Forgetting `case float64` is a CLAUDE.md anti-pattern —
constraints break the moment the snapshot round-trips through
JSON.

## Boundary: Collectors Don't Mutate

Allowed K8s verbs from a collector: `Get`, `List`, `Watch` (one-shot
only — drain and return). Anything in this column is a review block:

| Forbidden in collectors | Belongs in |
|-------------------------|------------|
| `Create`, `Update`, `Patch`, `Delete`, `Apply` | Validator (job-runner phase) |
| Exec into pods | Container-per-validator check |
| Subprocess that mutates host state | Out of scope — AICR is design-time |
| Long-running watch loops | Validator or controller (AICR has neither today) |
| Polling for resource readiness | Use `pkg/k8s/pod.WaitForJobCompletion` from a validator |

If your check requires mutation to know the answer, the answer
belongs in `pkg/validator`, not `pkg/collector`.

## Concurrency Rules

- Collectors run in parallel under `errgroup.WithContext`. The order
  in the snapshot is the order results are appended under the
  snapshotter's mutex — **do not rely on it**.
- Collectors do not share state with each other. The Talos pair is
  the one exception, and it shares lazily-initialized config via the
  factory — not via globals.
- Do not block on another collector's output. If a dimension depends
  on another, fold both into the same collector or compose them at
  validation time.
- The snapshotter's `errgroup` is configured to cancel siblings on
  hard error today only structurally (`collectSafe` swallows errors
  and logs them). Returning a real error from `Collect` is reserved
  for future fail-closed cases — flag a discussion before flipping a
  collector to that mode.

## Error Wrapping

Use `pkg/errors` with codes — never `fmt.Errorf`:

```go
import (
    stderrors "errors"
    "github.com/NVIDIA/aicr/pkg/errors"
)

if err := api.Get(...); err != nil {
    return nil, errors.Wrap(errors.ErrCodeUnavailable, "k8s api unreachable", err)
}
```

Pick codes by intent: `ErrCodeUnavailable` for upstream/dependency
unreachable, `ErrCodeTimeout` for ctx deadline, `ErrCodeInternal` for
parse or invariant failures. Never swallow a non-context error
silently in a spawned goroutine — emit at least
`slog.Warn("...", "error", err)` (CLAUDE.md anti-pattern).

## Cross-Cutting Topology Collector

[`pkg/collector/topology`](https://github.com/NVIDIA/aicr/tree/main/pkg/collector/topology)
is the only collector that reads **cluster-wide** state rather than
the local node. It paginates `nodes.List`, aggregates taints and
labels into `taintID → []node` and `labelID → []node` maps, and emits
them as a single `TypeNodeTopology` measurement. Bound by
`CollectorTopologyTimeout` (90s) and the `MaxNodesPerEntry` cap from
the factory (caps the per-entry node list to keep snapshot size
sane).

Treat it as the template for any future cluster-scoped collector —
not for per-node ones.

## Testing

| What | How |
|------|-----|
| Constraint evaluation | `validator.WithNoCluster(true)` — see [Test Isolation in CLAUDE.md](https://github.com/NVIDIA/aicr/blob/main/.claude/CLAUDE.md#test-isolation) |
| K8s collector unit tests | `k8s.io/client-go/kubernetes/fake` — inject via collector option |
| GPU / OS host tooling | Inject a `commandRunner` or `HardwareDetector` (the GPU collector shows the pattern) |
| Timeout handling | `ctx, cancel := context.WithCancel(...); cancel(); _, err := c.Collect(ctx)` — assert wrapped `ErrCodeTimeout` |
| Table-driven cases | Required by CLAUDE.md when ≥ 2 input shapes — one case per shape, named |

Never write a test that hits a live cluster. CI runs without one.

## Common Pitfalls

| Pitfall | Symptom | Fix |
|---------|---------|-----|
| No `context.WithTimeout` at entry | Snapshot hangs on slow upstream | Add the timeout line; default is `defaults.CollectorTimeout` |
| Empty `Measurement.Type` | Constraints can't address it; resolver silently ignores | Set `Type` from a `measurement.TypeXxx` constant |
| Type-switch on `reading.Any()` missing `case float64` | Constraints pass live, fail after JSON round-trip | Add the `case float64` branch and reject truncation |
| Swallowed goroutine error | Operator sees "no data" with no clue why | `slog.Warn("...", "error", err)` before returning |
| Mutating K8s call | Review block; collector becomes a controller | Move to `pkg/validator` |
| Bare `return err` | Loses code on wrap chain | `errors.Wrap(errors.ErrCodeUnavailable, "<msg>", err)` |
| New `measurement.Type` not added to `Types` slice | `ParseType` rejects it; recipe constraints can't reference it | Append both the constant and the `Types` entry |
| `http.DefaultClient` for remote fetches | Unbounded timeout, snapshot can hang | `&http.Client{Timeout: defaults.HTTPClientTimeout}` |

## See Also

- [index.md](index.md) — overall architecture and package map
- [recipe.md](recipe.md) — recipe constraints address measurement
  values by `Type` / `Subtype` / key
- [validator.md](validator.md) — validators consume the snapshot
  measurements collectors produce, and are where mutation belongs
- [CLAUDE.md](https://github.com/NVIDIA/aicr/blob/main/.claude/CLAUDE.md)
  — error wrapping, context, K8s patterns, the `reading.Any()`
  anti-pattern entry
