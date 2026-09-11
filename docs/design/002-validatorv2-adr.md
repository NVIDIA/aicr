# ADR-002: Replace Go Test Validation Engine with Container-Per-Validator Model

## Status

**Accepted and Implemented** — 2026-03-06

The migration is complete. The implementation lives in `pkg/validator/` with validator
containers in `validators/`. The v1 engine has been deleted.

> **Implementation drift (noted 2026-06-29).** Parts of the Decision below
> describe the original design and no longer match the shipped implementation.
> The historical text is preserved, but treat the following as current:
> - **Job input mounts:** validator Jobs mount `/data/validation/validation.yaml`
>   (env `AICR_VALIDATION_PATH`) and `/data/snapshot/snapshot.yaml` — not a
>   `/data/recipe/recipe.yaml` recipe ConfigMap (`pkg/validator/v1/job_plan_internal.go`).
> - **RBAC posture:** validation Jobs are bound to the built-in **`cluster-admin`**
>   ClusterRole (`pkg/validator/job/rbac.go`), not the minimal-purpose role this
>   ADR envisioned; see that file's rationale for why this is considered
>   acceptable for ephemeral, per-run Jobs.

## Context

AICR validates GPU-accelerated Kubernetes clusters through a multi-phase pipeline
(readiness, deployment, conformance, performance). The current implementation
(`pkg/validator`) uses Go's `testing.T` framework as a runtime execution engine
inside Kubernetes Jobs:

1. Validation checks are compiled into test binaries (`go test -c`)
2. A K8s Job runs `deployment.test -test.v -test.run '^TestOperatorHealth$' | test2json`
3. Results are extracted by parsing go test JSON output from pod logs
4. Custom protocols (`CONSTRAINT_RESULT:`, `ARTIFACT:`) are embedded in test log lines

This approach has several fundamental problems:

- **`testing.T` is not a public API.** The Go team explicitly documents that
  `testing.T` is designed for unit tests, not as a general-purpose execution
  framework. We depend on implementation details of `go test -json` output format.

- **Fragile IPC.** Results flow through a single channel (pod stdout) using a mix
  of go test JSON events and custom string markers. Any uncontrolled output to
  stdout/stderr corrupts the parsing pipeline.

- **Monolithic failure domain.** All checks in a phase (e.g., 10 deployment checks)
  run in one pod. An OOM kill or panic in one check loses all results.

- **Tight coupling.** Adding a new check requires writing Go code, registering via
  `init()`, writing a test wrapper function, and rebuilding the validator container
  image. The check code must live in this repository.

- **Non-standard output.** The custom `ValidationResult` type is internal to AICR.
  There is no interoperability with standard test reporting tools.

## Decision

We will replace the Go test-based validation engine with a **container-per-validator**
model where each validation is a standalone OCI container image, orchestrated as
individual Kubernetes Jobs. The orchestrator lives in `pkg/validator/`, validator containers in `validators/`.

### Result Protocol

Validators communicate results via standard Unix/Kubernetes mechanisms:

| Channel | Purpose | Captured in CTRF? |
|---------|---------|-------------------|
| **Exit code** | Pass/fail/skip signal | Yes — mapped to CTRF status |
| **`/dev/termination-log`** | Error context on failure | Yes — stored in `message` field |
| **stdout** | Evidence and conformance output | Yes — stored in `stdout` field as `[]string` |
| **stderr** | Test logs, debug output, diagnostics | No — visible via `kubectl logs` only |

Exit code mapping: `0` = passed, `1` = failed, `2` = skipped (not applicable).

No custom protocols, no log parsing, no JSON event streams. stdout is reserved
for structured evidence; stderr is for debug/test logs.

### Validator Catalog

A versioned, declarative YAML catalog embedded in `recipes/catalog.yaml`
defines all validators:

```yaml
apiVersion: validator.nvidia.com/v1alpha1
kind: ValidatorCatalog
metadata:
  name: aicr-validators
  version: "1.0.0"
validators:
  - name: gpu-operator-health
    phase: deployment
    description: "Verify GPU operator pods are running and healthy"
    image: ghcr.io/nvidia/aicr-validators/gpu-operator-health:v1.0.0
    timeout: 2m
    args: []
    env: []
```

### Validator Container Contract

Every validator container:
1. Reads `/data/snapshot/snapshot.yaml` and `/data/recipe/recipe.yaml` from mounted ConfigMaps
2. Self-selects: reads the recipe, exits `2` (skip) if not applicable
3. Exits `0` (pass), `1` (fail), or `2` (skip)
4. Writes error context to `/dev/termination-log` on failure (exit 1)
5. Writes evidence to **stdout** (captured in CTRF report)
6. Writes debug/test logs to **stderr** (not captured in CTRF)
7. Uses the mounted ServiceAccount (`aicr-validator` with scoped ClusterRole) for K8s API access
8. For GPU workloads: creates its own Pod on a GPU node (two-level scheduling)
9. Handles SIGTERM gracefully — writes partial results within `terminationGracePeriodSeconds` (30s)

### Job Specification

Each validator runs as a K8s Job with:
- `backoffLimit: 0` — no retries
- `activeDeadlineSeconds` — catalog `timeout` plus `defaults.ValidatorJobDeadlineHeadroom`,
  so the check's own budget expires first unless container startup outruns that
  headroom (see Timeout and Termination below)
- `terminationGracePeriodSeconds: 30` — time between SIGTERM and SIGKILL
- `ttlSecondsAfterFinished: 3600` — 1 hour retention for debugging
- `restartPolicy: Never`
- `imagePullPolicy: IfNotPresent` (version-locked tags)
- Resources: `requests/limits: {cpu: 1, memory: 1Gi}` (configurable per catalog entry)
- Soft CPU node affinity, tolerate-all tolerations
- ServiceAccount: `aicr-validator` (bound to purpose-built ClusterRole)
- Volumes: snapshot + recipe ConfigMaps (read-only)

Job naming: `aicr-{validatorName}-{hash}`

### RBAC

Three resources, using a purpose-built ClusterRole with minimum required permissions:

1. **ServiceAccount** `aicr-validator-<runID>` in validation namespace
2. **ClusterRole** `aicr-validator` with scoped read/write rules per resource type
3. **ClusterRoleBinding** `aicr-validator-<runID>` binding the SA to the ClusterRole

Created once per run via Server-Side Apply, cleaned up at end.

> **Implementation note (2026-05-14):** The ServiceAccount and ClusterRoleBinding
> names are suffixed with the per-run `runID` to prevent concurrent `aicr
> validate` invocations from clobbering each other's RBAC during end-of-run
> cleanup. The original design used fixed names (`aicr-validator`); operators
> ran into a race where run A's cleanup deleted the SA while run B was still
> deploying validator Jobs, causing `FailedCreate: serviceaccount … not found`.
> Resource discovery in tests and tooling should match by the stable
> `app.kubernetes.io/name=aicr-validator` label rather than the literal name.

### Timeout and Termination

Four timeout layers protect against hangs. The first three are ordered so the
check's own budget is the tightest — the ordering that closes issue #2473 (see
below). Clocks 1 and 3 start at different moments, so that ordering is not
unconditional: clock 1 begins when the validator container's first instruction
runs, while clock 3 begins at the Job's start time. Container startup —
scheduling plus image pull — therefore eats into the headroom, and a startup
slower than `defaults.ValidatorJobDeadlineHeadroom` lets clock 3 fire first.
Kubernetes then marks the Job `Failed/DeadlineExceeded` and deletes the still-active
pod, which is the pre-#2473 behaviour: the phase still fails closed, but the
check's own diagnosis is lost. The headroom is sized as
`defaults.K8sPodReadyTimeout` plus margin to cover ordinary startup; it is a
budget, not a guarantee.

1. **Check budget** (catalog `timeout`, published as `AICR_CHECK_TIMEOUT`): the
   validator's own parent context. A well-behaved check cancels and exits on
   this deadline before Kubernetes ever intervenes.

2. **Orchestrator wait timeout** (catalog timeout + `defaults.ValidatorWaitBuffer`,
   2m30s): if the Job hasn't reached a terminal state by then, the orchestrator
   captures whatever logs/status are available and moves to the next validator.
   The window is measured from the Job's **observed start time** — `status.startTime`
   when the apiserver has stamped it, otherwise `creationTimestamp` — not from the
   moment the create/apply response reached the CLI, so it shares an origin with
   clock 3 below. Anchored anywhere else, the real margin between clocks 2 and 3
   collapses to `defaults.JobEnvelopeMargin` (60s), because clock 3 has already
   been running for however long the apply response took: a response slower than
   that lets clock 3 fire first and delete the still-active pod. `v1.OrchestratorWaitFor`
   carries the derivation; it caps the result at the un-rebased budget (apiserver
   clock skew can place the start time in the CLI's future) and floors it at
   `defaults.ValidatorMinCompletionWait`.

3. **Job `activeDeadlineSeconds`** (catalog timeout + `defaults.ValidatorJobDeadlineHeadroom`,
   3m30s): K8s sends SIGTERM, then SIGKILL after `terminationGracePeriodSeconds` (30s),
   as a backstop for a check that never exits on its own. Because the headroom keeps
   this clock behind clocks 1 and 2, a self-terminated pod is no longer *active* by
   the time this deadline could fire, and the Job controller's `deleteActivePods`
   only deletes active pods — so the pod survives, `Failed`, with its logs and
   termination message intact for extraction. Before this ordering existed, all
   three clocks shared one catalog value and the Job's deadline — timed from Job
   creation, ahead of the check's own deadline, timed from container start — fired
   first and deleted the still-running pod, which is exactly what #2473 reported.

4. **Parent context timeout** (CLI flag): Cancels the entire phase if exceeded.

On any timeout, the orchestrator always attempts log capture with a fresh
`context.Background()` context before cleanup, ensuring partial results are never
silently lost.

### Failure Handling

Every failure mode produces a valid CTRF entry:

| Scenario | CTRF Status | Message Source |
|----------|-------------|----------------|
| Exit 0 | `passed` | Termination log (optional) |
| Exit 1 | `failed` | Termination log |
| Exit 2 | `skipped` | "Validator not applicable" |
| OOMKilled | `other` | "Container OOMKilled" |
| Timeout exceeded | `other` | "Exceeded timeout of {duration}" |
| Image pull failure | `other` | Pod waiting reason |
| Pod deleted externally | `other` | "Pod not found for Job" |
| Pod never scheduled | `other` | "Pod not scheduled within {timeout}" |

No finalizers are used. `TTLSecondsAfterFinished` provides safety-net cleanup for
abandoned resources.

### Report Format

Results aggregated into [CTRF](https://ctrf.io/) (spec version `0.0.1`), one report
per phase, stored in ConfigMaps (`aicr-ctrf-{runID}-{phase}`, key: `report.json`).

### Execution Model

```
ValidateAll(ctx, recipe, snapshot)
├── EnsureRBAC()                    # Once (SA + CRB)
├── ensureDataConfigMaps()          # Once (snapshot + recipe)
│
├── For phase in [deployment, conformance, performance]:
│   ├── Skip if previous phase failed
│   ├── For each validator (sequentially):
│   │   ├── Deploy Job
│   │   ├── Stream stderr (background, for live progress)
│   │   ├── WaitForCompletion() — until jobStart + timeout + ValidatorWaitBuffer
│   │   ├── ExtractResult() — exit code, termination msg, stdout
│   │   ├── Add to CTRF report
│   │   └── CleanupJob() unless --no-cleanup
│   └── Write CTRF ConfigMap
│
├── defer: CleanupRBAC()
└── defer: cleanupDataConfigMaps()
```

### Readiness Phase

Remains as-is in `pkg/validator` (inline constraint evaluation against snapshots).
Not containerized because readiness requires no cluster access and must work in
`--no-cluster` mode.

### Migration Strategy

Migration is complete. The v1 engine was replaced in a single PR:

1. Orchestrator in `pkg/validator/` (catalog, CTRF, job deployer, RBAC)
2. Validator containers in `validators/` (deployment, conformance, performance)
3. Catalog in `recipes/catalog.yaml` (embedded alongside recipe data)

## Consequences

### Positive

- **Decoupled validators.** Independent OCI images, different teams, any language.
- **Simple contract.** Exit code + termination log + stdout/stderr separation.
- **Fault isolation.** OOM/crash in one validator doesn't affect others.
- **Standard output.** CTRF works with GitHub Actions, Slack, dashboards.
- **No log parsing.** Exit codes and termination messages are K8s primitives.
- **Declarative extensibility.** Add validator = add YAML entry + publish image.
- **Robust failure handling.** Every failure mode produces a valid CTRF entry.

### Negative

- **More Jobs.** N validators = N sequential Jobs. More scheduling overhead.
- **Image management.** Each validator needs its own build/publish pipeline.
- **stdout size.** Truncated to 1000 lines to fit ConfigMap 1MB limit.
- **Migration period.** Two engines coexist until migration is complete.
- **Loss of in-process debugging.** Requires building containers vs `go test -run`.

### Neutral

- **RBAC is scoped but broad.** Purpose-built ClusterRole with minimum required permissions. Short-lived (created and deleted per run).
- **Data delivery unchanged.** Same ConfigMap approach. Same 1MB limitation.

## Alternatives Considered

### 1. gRPC sidecar for result reporting

**Rejected:** Adds complexity (sidecar image, proto definitions, lifecycle
management) for marginal benefit over exit codes.

### 2. CRD-based results (ValidatorResult custom resource)

**Rejected:** Requires CRD installation, adds operational complexity. ConfigMaps
are simpler and universally available.

### 3. Fix the Go test parsing pipeline

**Rejected:** Treats symptoms, not cause. `testing.T` is not a public API.
Monolithic pod problem remains. Tight coupling remains.

### 4. Parallel execution from day one

**Deferred:** Adds complexity. Sequential is simpler. Parallelism can be added
later without changing the architecture.

### 5. cluster-admin vs scoped ClusterRole

**Initially cluster-admin, later replaced with a purpose-built ClusterRole.**
The initial implementation used `cluster-admin` for simplicity. During review,
this was replaced with a scoped `aicr-validator` ClusterRole that grants only
the permissions validators actually need (read access to workloads/nodes,
create/delete for test pods, DRA/scheduling resources). The ClusterRole is
managed via Server-Side Apply and cleaned up after each run.

## References

- [CTRF Specification](https://ctrf.io/)
- [CTRF JSON Schema](https://github.com/ctrf-io/ctrf/blob/main/schema/ctrf.schema.json)
- [Kubernetes Termination Messages](https://kubernetes.io/docs/tasks/debug/debug-application/determine-reason-pod-failure/)
- [Current validator implementation](https://github.com/NVIDIA/aicr/tree/main/pkg/validator)
