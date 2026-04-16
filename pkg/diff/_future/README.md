## Recipe-vs-Snapshot Drift Detection (deferred)

This directory preserves the recipe-vs-snapshot implementation that was
removed from the initial `aicr diff` PR per review feedback (NVIDIA/aicr#582).

Reviewers agreed that recipe-vs-snapshot drift belongs under
`aicr validate --drift-only` to keep constraint evaluation in one code path.

### Why recipe-vs-snapshot is worth preserving

Three tools answer three different questions. Recipe-vs-snapshot is not
redundant with either neighbor:

| Tool | Question answered | Cluster access | Side effects |
|------|-------------------|----------------|--------------|
| `aicr diff` (snapshot-vs-snapshot) | What changed between T1 and T2? | None (two files) | None |
| recipe-vs-snapshot (this code) | Did the cluster drift from the declared recipe? | None (file + snapshot) | None |
| `aicr validate` | Does the cluster satisfy the recipe *right now*? | Required | Creates RBAC, deploys Jobs, runs health checks |

### Where recipe-vs-snapshot is uniquely valuable

1. **Airgapped / disconnected audit** — an auditor receives a `snapshot.yaml`
   and the `recipe.yaml`, reproduces drift analysis with zero cluster access.
   `aicr validate` cannot do this because it needs to deploy Jobs.

2. **Historical compliance** — "Was cluster X compliant on 2026-03-15?"
   Given the snapshot captured that day, recipe-vs-snapshot answers
   definitively. `aicr validate` only answers *now*.

3. **Fleet-scale drift monitoring** — gather snapshots from 500 clusters
   via CronJob, run recipe-vs-snapshot offline on all of them to find
   divergent clusters. Round-tripping `aicr validate` against every
   cluster does not scale to this shape of workload.

4. **CI/CD drift gating** — a PR that changes a recipe? Compare the new
   recipe against a cached production snapshot in CI, no cluster
   spin-up required. Sub-second feedback in pipelines.

5. **Pre-flight before `aicr validate`** — cheap read-only check before
   the heavy Job-deploying validate run. Catches obvious drift fast
   and avoids paying for a full validate when the answer is already no.

6. **Evidence / attestation trail** — "here's recipe hash A, snapshot
   hash B, here's the signed drift report." Reproducible forensic
   artifact for audit and compliance workflows.

The reviewers' concern was that constraint evaluation should live in
one code path — they are right. The suggested architecture
(`aicr validate --drift-only` that skips Job execution) unifies the
code path while keeping both use cases first-class.

### Files

| File | Original location | Contains |
|------|-------------------|----------|
| `recipe_diff.go.bak` | `pkg/diff/diff.go` | `RecipeVsSnapshot()`, `evaluateConstraint()`, component drift, validation phases |
| `metrics.go.bak` | `pkg/diff/metrics.go` | Prometheus metrics (constraint gauges, component drift, check counters) |
| `metrics_test.go.bak` | `pkg/diff/metrics_test.go` | Metrics test coverage |
| `recipe_diff_test.go.bak` | `pkg/diff/diff_test.go` | Recipe constraint, component drift, validation phase tests |
| `cli_diff.go.bak` | `pkg/cli/diff.go` | CLI with both recipe and snapshot modes |
| `table.go.bak` | `pkg/diff/table.go` | Table renderer with recipe constraint/component output |

### Reuse plan for `validate --drift-only`

1. Extract `evaluateConstraint` as a shared function in `pkg/constraints`
   so both active validate and drift-only share one implementation.
2. Replace heuristic component-name matching with a structured registry
   mapping between recipe `ComponentRef` and deployed container images
   (surfaces confidence when a match is best-effort).
3. Add `--drift-only` flag to `aicr validate` that skips Job deployment
   and RBAC creation, evaluates constraints inline against the supplied
   snapshot, and reports the same structured result.
4. Wire Prometheus metrics from `metrics.go.bak` into the validator's
   execution path — behind a flag, so the CLI invocation stays quiet
   and long-running controllers opt in.
