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
3. Exits `0` (pass), `1` (fail), `2` (skip), or `3` (SIGTERM interruption, CTRF `other`)
4. Writes error context to `/dev/termination-log` on failure (exit 1)
5. Writes evidence to **stdout** (captured in CTRF report)
6. Writes debug/test logs to **stderr** (not captured in CTRF)
7. Uses the mounted ServiceAccount (`aicr-validator` with scoped ClusterRole) for K8s API access
8. For GPU workloads: creates its own Pod on a GPU node (two-level scheduling)
9. Cancels the check context on SIGTERM. Cooperative checks run deferred cleanup,
   then write a bounded interruption message and exit `3`, even if the check returns
   nil. Cleanup and message flushing are best effort within
   `terminationGracePeriodSeconds` (60s: a 30s cleanup budget plus a 30s result
   flush margin); blocking or long teardown can still be killed before the message
   is written.

### Job Specification

Each validator runs as a K8s Job with:
- `backoffLimit: 0` — no retries
- `activeDeadlineSeconds` — catalog `timeout` plus `defaults.ValidatorJobDeadlineHeadroom`
  (3m: startup grace + cleanup grace + result flush margin), so the check's own
  budget expires first and its deferred cleanup runs before the Job deadline
  unless container startup outruns the startup grace (see Timeout and Termination below)
- `terminationGracePeriodSeconds: 60` — time between SIGTERM and SIGKILL
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
check's own budget is tightest, the Job deadline is a backstop behind it, and the
orchestrator outlives both (issues #2473 and #1632).

| Constant (`pkg/defaults`) | Value | Role |
|---|---|---|
| `ValidatorStartupGrace` (= `K8sPodReadyTimeout`) | 2m | Scheduling and image pull consume the Job clock before the check's clock starts. |
| `ValidatorCleanupGrace` (= `K8sCleanupTimeout`) | 30s | Deferred cleanup after the check's own context expires. |
| `ValidatorResultFlushMargin` | 30s | Writing the termination message and flushing stdout before SIGKILL. |
| `ValidatorJobDeadlineHeadroom` | 3m | Startup + cleanup + flush; `activeDeadlineSeconds` is the catalog timeout plus this. |
| `ValidatorTerminationGracePeriod` | 60s | Cleanup + flush; `terminationGracePeriodSeconds`. |
| `ValidatorTerminationWaitMargin` | 5s | Delivery margin on the orchestrator's wait and on the interrupted-run pod wait. |
| `K8sPodTerminationWaitTimeout` | 90s | Normal post-delete pod wait; covers the grace plus margin. |
| `ValidationCancellationBudget` | 125s | Interrupted-run cleanup: 2 × 30s + 60s grace + 5s margin (`validationCancellationBudget` in `pkg/cli`). |

With `c` the catalog timeout, `D = c + 3m` the Job deadline and `W = D + 60s + 5s`
the orchestrator's wait envelope (`v1.WaitEnvelopeFor`), the invariants pinned
against `pkg/defaults` in tests are:

- `ValidatorJobDeadlineHeadroom = ValidatorStartupGrace + ValidatorCleanupGrace + ValidatorResultFlushMargin`.
- `ValidatorTerminationGracePeriod > ValidatorCleanupGrace`, and covers cleanup plus flush.
- Every in-pod cleanup bound (`K8sCleanupTimeout`, `PreflightCleanupTimeout`) is at most `ValidatorCleanupGrace`.
- `W >= D + ValidatorTerminationGracePeriod`, so the orchestrator never classifies a run as
  timed out while the pod is still finishing inside its SIGTERM window.
- `K8sPodTerminationWaitTimeout >= ValidatorTerminationGracePeriod + ValidatorTerminationWaitMargin`.
- For every catalog entry, `W + K8sCleanupTimeout + K8sPodTerminationWaitTimeout <
  ValidationOperationTimeout` (75m), the facade cap for callers without a deadline.
- The operator's deployment-phase Job (60m `ValidationJobDeadline`) holds every
  deployment check's `W + K8sCleanupTimeout`, and its pod's
  `terminationGracePeriodSeconds` exceeds `ValidationCancellationBudget`.

The clocks do not start at the same moment. Clock 1 begins when the validator
container's first instruction runs, while clock 3 begins at the Job's start time,
so a pod that starts as late as the startup grace still has `c` for the check and
the full cleanup and flush grace before clock 3 fires. A startup slower than the
startup grace spends that grace out of the cleanup budget; the headroom is a
budget, not a guarantee.

1. **Check budget** (catalog `timeout`, published bare as `AICR_CHECK_TIMEOUT`): the
   validator's own parent context. A well-behaved check cancels, runs its deferred
   cleanup under a fresh bounded context, and exits on this deadline before
   Kubernetes ever intervenes.

2. **Job `activeDeadlineSeconds`** (catalog timeout + `defaults.ValidatorJobDeadlineHeadroom`,
   3m): the Job controller SIGTERMs the pod, then SIGKILLs it after
   `terminationGracePeriodSeconds` (60s), as a backstop for a check that never exits on
   its own. A self-terminated pod is no longer *active* by then, and the controller's
   `deleteActivePods` only deletes active pods, so it survives `Failed` with its logs and
   termination message intact. A pod that is still running is deleted by the controller,
   and a SIGTERM-aware check writes its interruption message (exit `3`) inside the
   grace window.

3. **Orchestrator wait** (`v1.WaitEnvelopeFor`: Job deadline + `defaults.ValidatorTerminationGracePeriod`
   + `defaults.ValidatorTerminationWaitMargin`, i.e. catalog timeout + 4m05s): the
   orchestrator waits for the Job to reach a terminal state, so a deadline-killed check
   reports before the run is classified. The window is measured from the Job's **observed
   start time** (`status.startTime` when the apiserver has stamped it, otherwise
   `creationTimestamp`), not from the moment the create/apply response reached the CLI, so
   it shares an origin with clock 2. `v1.OrchestratorWaitFor` carries the derivation; it caps
   the result at the un-rebased envelope (apiserver clock skew can place the start time in
   the CLI's future) and floors it at `defaults.ValidatorMinCompletionWait`.
   While waiting, the orchestrator also observes the Job's pods (`pod.ObserveJobPods`) and
   keeps the last pod seen with a terminated validator container. When the Job reports
   `Failed/DeadlineExceeded` and no live pod remains, `ExtractResult` reads the check's exit
   code and termination message from a pod still in its deletion grace window or from that
   last observed pod, and reports it appended to the deadline message. The verdict stays
   `failed` (exit 1); only its message gains the check's own account. When no terminated
   status was observed, the generic deadline message stands.

4. **Parent context timeout** (CLI flag): Cancels the entire phase if exceeded.

SIGTERM from deadline enforcement, eviction or drain cancels the in-pod check
context. Checks must honor cancellation and use fresh, bounded cleanup contexts.
The grace period is not a guarantee that teardown finishes: existing CRE teardown
and NCCL namespace/Trainer cleanup can exceed the 30s cleanup grace. SIGKILL, OOM and
crashes cannot run deferred cleanup. Orphaned per-run resources — the
`aicr.run/gpu-test-run`-labeled test namespaces and Run:ai residue, plus
run-scoped RBAC and ConfigMaps — are reclaimed out of band by `aicr sweep`,
the label-based sweeper shipped for this issue.

Operator SIGINT/SIGTERM preserves completed and partial current-phase CTRF results,
records unstarted checks in that phase as skipped, and returns `ErrCodeCanceled`
unless privileged RBAC revocation also fails. When no check entry already blocks
the phase, a synthetic `validation-interrupted` entry with CTRF status `other`
keeps the report itself from reading as passing. That combined case returns
`ErrCodeInternal`, joining both causes and warning that the cluster-admin
binding may be orphaned, without discarding partial results.
With cleanup enabled, post-wait capture, Job deletion and report persistence
share one fresh 30s budget once cancellation is observed, with persistence before
the pod-termination wait. That wait keeps the shared deadline but extends to at
least Job deletion plus the pod's 60s SIGTERM grace and a 5s delivery margin, so
the per-run RBAC lifetime covers the pod's full grace. An uninterrupted run keeps
the caller's context for capture and deletion.
The existing per-run RBAC/data cleanup has a separate 30s budget (typically about
65s total cleanup, about 125s plus overhead in the worst case). Partial CLI output
can use only the remainder of a 125s aggregate deadline recorded on first observed
cancellation, capped by its existing 30s I/O allowance; it cannot start a further
window. Normal validation time does not consume this budget. Context deadlines cannot physically interrupt
arbitrary blocking file/stdout writes. ConfigMap persistence is best effort and
does not replace the partial return value. Neither interrupted nor fatal cleanup
paths publish evidence. A second CLI signal restores force-exit behavior and
can interrupt cleanup or blocked output. `--no-cleanup` continues to opt out of deletion.

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
│   │   ├── WaitForCompletion() — until jobStart + WaitEnvelopeFor(timeout)
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
