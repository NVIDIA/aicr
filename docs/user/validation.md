# Validating a Cluster

Task-oriented walkthrough for running `aicr validate` against a GPU cluster — from
capturing a snapshot through interpreting results. Covers both training and
inference workloads and all three validation phases (deployment, performance,
conformance).

For per-flag reference, see [CLI reference: aicr validate](cli-reference.md#aicr-validate).
For the architectural view of how snapshot + recipe flow into the validator, see
[Data flow: Stage 3 Validate](../integrator/data-flow.md#stage-3-validate-constraint-checking).

## When to validate

| Phase | What it answers | Typical trigger |
|-------|-----------------|-----------------|
| `deployment` | Are the components the recipe asks for actually installed and healthy? | After `./deploy.sh` finishes, before running any workload |
| `performance` | Does the cluster hit expected bandwidth / throughput thresholds? | After components are ready; before going to production |
| `conformance` | Does the cluster support workload-specific capabilities (DRA, gang scheduling, autoscaling, ...)? | Before opening the cluster to real workloads |

Readiness pre-flight constraints (K8s version, OS, kernel) run implicitly before
any phase. If pre-flight fails, no validator Jobs are deployed.

## The workflow

```text
  aicr snapshot ─┐
                 ├─▶ aicr validate ─▶ CTRF report
  aicr recipe ───┘                    (passed / failed / skipped per check)
```

1. **Snapshot** — capture current cluster state (K8s / OS / GPU / topology) once.
2. **Recipe** — generate the target configuration for your workload (training vs inference, platform, accelerator).
3. **Validate** — run one or all phases against the snapshot and live cluster.

## Prerequisites

- `aicr` CLI installed (see [installation](installation.md)).
- `kubectl` configured for the target cluster (validator dispatches K8s Jobs; pre-flight only needs the snapshot).
- Cluster service account with RBAC to create Jobs, ConfigMaps, and read cluster state (AICR creates its own `aicr-validation` namespace on first run).

## Training performance validation

Training performance runs an NCCL all-reduce benchmark — a Kubeflow `TrainJob`
that runs `all_reduce_perf` across GPU nodes and measures aggregate bus
bandwidth. Three check variants are available; the recipe picks the one (or
ones) that match the target fabric:

| Check | Transport | When it's selected |
|---|---|---|
| `nccl-all-reduce-bw` | Auto-detect (whatever NCCL picks) | Default for H100 on EKS/GKE, and for GB200/B200 on non-EKS services. Preserves the pre-variant behavior. |
| `nccl-all-reduce-bw-net` | NET (EFA on EKS) | GB200 + EKS. Asserts EFA actually carried traffic — catches silent fallback to Socket when the NVIDIA driver is missing `NVreg_GrdmaPciTopoCheckOverride=1`. |
| `nccl-all-reduce-bw-nvls` | NVLS (MNNVL across an NVL72 IMEX domain) | GB200 + EKS. Asserts the NVLS communicator actually initialized — catches silent fallback to EFA when the IMEX domain is misconfigured. |

GB200/EKS recipes (both `training` and `inference` intents) enable `-net` and
`-nvls` together rather than the auto-detect variant, because those nodes
expose two inter-node fabrics simultaneously and a single auto-detect test
would only exercise one of them.

```bash
# Capture snapshot, generate training recipe, validate the performance phase.
aicr snapshot --output snapshot.yaml

aicr recipe --service eks --accelerator h100 --os ubuntu \
            --intent training --platform kubeflow \
            --output recipe.yaml

aicr validate --recipe recipe.yaml --snapshot snapshot.yaml --phase performance
```

The generated recipe lists the selected variant(s) under
`validation.performance.checks` with a platform-tuned bandwidth constraint
(example: `>= 300 GB/s` for H100 + EFA; `>= 40 GB/s` NET and `>= 500 GB/s`
NVLS for GB200 + EFA, each sized for a 2-node pair).

Expected flow (~5–10 min per variant): readiness pre-flight → deploy
`TrainingRuntime` + `TrainJob` in `aicr-validation` → worker pods reach
`Running` → run `all_reduce_perf` → parse peak bus bandwidth → verify the
intended transport actually carried traffic (for `-net` / `-nvls`) → compare
to recipe constraint (10 % tolerance) → cleanup.

A passing CTRF entry:

```json
{
  "name": "nccl-all-reduce-bw-net",
  "status": "passed",
  "suite": ["performance"],
  "stdout": [
    "NCCL All Reduce bandwidth (nccl-all-reduce-bw-net): <actual> GB/s",
    "Constraint: >= <threshold> → true"
  ]
}
```

> **Note:** this guide does not yet list per-platform expected-bandwidth
> baselines (EKS + EFA, GKE + TCPXO, AKS, etc.). The recipe's constraint
> value is the current pass/fail floor; measured values above that floor
> are treated as passing regardless of platform.

To run deployment validation first (recommended — verifies GPU Operator, DRA
driver, and Kubeflow Trainer are installed and healthy before the benchmark):

```bash
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml --phase deployment
```

## Inference performance validation

Inference performance runs the `inference-perf` check — deploys a
`DynamoGraphDeployment` with a small vLLM-served model (Qwen/Qwen3-0.6B by
default) plus an AIPerf benchmark Job, and measures end-to-end output-token
throughput and time-to-first-token (TTFT) p99.

```bash
# Capture snapshot, generate inference recipe, validate the performance phase.
aicr snapshot --output snapshot.yaml

aicr recipe --service eks --accelerator h100 --os ubuntu \
            --intent inference --platform dynamo \
            --output recipe.yaml

aicr validate --recipe recipe.yaml --snapshot snapshot.yaml --phase performance
```

The generated recipe includes `dynamo-platform` in `componentRefs` and lists
`inference-perf` under `validation.performance.checks` with two constraints
— one per metric the check produces:

```yaml
validation:
  performance:
    checks: [inference-perf]
    constraints:
      - name: inference-throughput   # output tokens/sec
        value: ">= 5000"
      - name: inference-ttft-p99     # time-to-first-token p99 in ms
        value: "<= 200"
```

Expected flow (~5–7 min on H100): readiness pre-flight → deploy
`ResourceClaimTemplate` + `DynamoGraphDeployment` in a per-run namespace
`aicr-inference-perf-<8-hex-suffix>` → wait for `state=successful` (image pull
+ model load) → `/health` probe → AIPerf benchmark Job parses throughput +
TTFT p99 → compare to recipe constraints (10 % tolerance) → cleanup.

All Dynamo Frontend and worker pods pin to a single GPU node via
`kubernetes.io/hostname` for a stable per-node baseline. On a shared cluster
where some GPUs on a candidate node are already held by another workload's
DRA `ResourceClaim`, the validator picks the candidate with the most free
GPUs and sizes the benchmark to that count — so the check does not need an
explicit hostname override to avoid saturated nodes. Concurrent
`aicr validate` invocations are isolated from each other by the run-specific
suffix on both the namespace and the inner AIPerf Job name.

A passing CTRF entry (measured on EKS H100, 8 × H100 GPUs, Qwen/Qwen3-0.6B):

```json
{
  "name": "inference-perf",
  "status": "passed",
  "suite": ["performance"],
  "stdout": [
    "RESULT: Inference throughput: 38367.28 tokens/sec",
    "RESULT: Inference TTFT p99: 127.90 ms",
    "Throughput constraint: >= 5000 → PASS",
    "TTFT p99 constraint: <= 200 → PASS"
  ]
}
```

The `RESULT: ` prefix on the first two lines is the contract documented in
`pkg/validator/validator.go` — any check that wants its summary lines echoed
to the CLI's own output (not just the CTRF report) opts in by emitting that
prefix. The validator runtime strips the prefix when echoing; the full
prefixed line stays in `stdout[]`.

To run deployment validation first (recommended — verifies GPU Operator, DRA
driver, Dynamo operator, KAI scheduler, and supporting components are installed
and healthy):

```bash
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml --phase deployment
```

### Skip scenarios

The inference validator has three explicit skip guards so it never runs where
it can't succeed. Each produces a `status: skipped` CTRF entry with a specific
reason. Skipped checks are **not** failures: the validator container exits
with code 2 internally (mapped to CTRF `skipped`), but `aicr validate` itself
exits **0** for skipped/passed/other phases — a skipped inference check never
drives a non-zero CLI exit on its own.

| Guard | Trigger | Skip message |
|-------|---------|--------------|
| **A** | Recipe lists `inference-perf` in `checks:` but no matching `inference-throughput` / `inference-ttft-p99` constraints | `no inference-throughput or inference-ttft-p99 constraint in recipe` |
| **B** | `inference-perf` is selected but `dynamo-platform` is not in recipe `componentRefs` | `skipped - dynamo-platform not in recipe components` |
| **C** | `dynamo-platform` is declared but the `DynamoGraphDeployment` CRD is not installed on the cluster (operator not deployed yet) | `skipped - DynamoGraphDeployment CRD not installed on cluster (dynamo-platform component declared but operator not deployed yet)` |

Guards fire before any cluster mutation, so skips are cheap (typically < 10 s).

## Running all phases

```bash
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml
# equivalent to: --phase deployment --phase performance --phase conformance
```

Phases run sequentially. If any phase fails, subsequent phases are skipped.

## Scoping CNCF submission evidence to specific features

The `--feature` flag scopes which CNCF AI conformance features get behavioral
evidence collected. It only applies to the CNCF-submission evidence collector
and is rejected by the CLI unless `--cncf-submission` is also set (which in
turn requires `--evidence-dir`). It does **not** scope the regular
`--phase conformance` validator run — that one always evaluates every check
defined in the recipe.

```bash
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml \
  --phase conformance \
  --cncf-submission \
  --evidence-dir ./evidence \
  --feature dra-support --feature gang-scheduling
```

Empty `--feature` (the default) collects evidence for every feature.

Valid feature names (from `pkg/evidence/cncf/collector.go`):

| Name | What it checks |
|------|----------------|
| `dra-support` | Dynamic Resource Allocation driver and ResourceSlices |
| `gang-scheduling` | Gang-scheduler presence and PodGroup support |
| `secure-access` | Cluster authn/authz posture for AI workloads |
| `accelerator-metrics` | GPU metrics exporter and Prometheus scrape config |
| `ai-service-metrics` | Inference-service metrics via custom-metrics API |
| `inference-gateway` | Gateway API + Inference Extension installation |
| `robust-operator` | Operator readiness and leader-election posture |
| `pod-autoscaling` | HPA / custom-metrics-driven pod autoscaling |
| `cluster-autoscaling` | Karpenter (preferred) or EKS managed node-group autoscaling fallback |

## Input modes

Snapshot and recipe can come from a file, an HTTPS URL, or a Kubernetes ConfigMap:

```bash
# File (default)
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml

# HTTPS URL
aicr validate \
  --recipe https://artifacts.example.com/recipes/h100-eks-inference.yaml \
  --snapshot https://artifacts.example.com/snapshots/prod-cluster.yaml

# Kubernetes ConfigMap (for in-cluster operators)
aicr validate \
  --recipe cm://gpu-operator/aicr-recipe \
  --snapshot cm://gpu-operator/aicr-snapshot
```

The ConfigMap form is useful when the snapshot is captured by an in-cluster
agent — see [agent deployment](agent-deployment.md).

## Dry-run mode

`--no-cluster` runs the validator against the snapshot alone, skipping all
Kubernetes API calls. Declarative constraints still evaluate; behavioral checks
report `skipped - no-cluster mode (test mode)`.

```bash
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml --no-cluster
```

Useful for CI pipelines that validate a recipe against a captured snapshot
without needing cluster access.

## CI/CD integration

`aicr validate` exits non-zero when any phase fails. CTRF JSON is emitted to
stdout (or to `--output <file>`), so a pipeline can gate promotion on both the
exit code and the structured report:

```bash
aicr validate \
  --recipe recipe.yaml \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --output ctrf.json
```

Exit codes follow Unix conventions and are derived from the CLI's structured
error codes (see [`pkg/errors/exitcode.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/errors/exitcode.go)):

| Code | Meaning |
|------|---------|
| `0` | All phases reported status `passed`, `skipped`, or `other` |
| `2` | Invalid input or request (`ErrCodeInvalidRequest`) — bad CLI flag, malformed argument, or a validator rejecting a recipe value (e.g., an inference constraint that uses the wrong comparator direction) |
| `5` | CLI-layer timeout *before* a check runs — snapshot-agent Job never completes within `--timeout`, or the validator Job as a whole exceeds its wait deadline |
| `8` | One or more phases reported status `failed`, **including per-check internal timeouts** (e.g., DynamoGraphDeployment not ready within `InferenceWorkloadReadyTimeout`) |

> **Important:** two quirks to be aware of when gating a pipeline on exit code:
>
> 1. Only phase status `failed` drives a non-zero exit. A phase whose status is `other` (check crashed, pod OOM, `activeDeadlineSeconds` exceeded) **still produces exit 0**. Pipelines that need to catch those outcomes must inspect the CTRF report and look at per-phase status or the `summary.other` count, not rely on exit code alone.
> 2. Exit 5 is narrower than it sounds. A timeout **inside** a check's own logic (DynamoGraphDeployment not ready, inference endpoint never healthy, AIPerf Job pod-wait deadline) surfaces as a failed phase, not as a structured `ErrCodeTimeout`, so the CLI exits **8**. Only timeouts at the CLI-to-cluster layer (snapshot-agent wait, validator-Job wait) retain their `ErrCodeTimeout` classification all the way through to exit 5.

Scripts that gate on validation outcome should treat **any non-zero code** as
failure rather than branching on specific values, and should additionally
check CTRF `summary.failed` and `summary.other` for a complete picture.

For informational-only runs (report results without failing the build):

```bash
aicr validate ... --fail-on-error=false
```

## Troubleshooting

### Readiness pre-flight fails

The CLI logs each readiness constraint comparison before any phase runs:

```text
readiness constraint failed: name=K8s.server.version expected=">= 1.34" actual=v1.33.0-eks-abc
```

Fix: upgrade the cluster, or pick a recipe whose readiness constraints match
the cluster's actual versions.

### Non-standard GPU labels or taints

Default GPU-node discovery looks for `nodeGroup`, `node.kubernetes.io/instance-type`, or GPU-related label substrings. If your cluster uses custom labels, override the scheduling of inner workloads with `--node-selector` and `--toleration`:

```bash
aicr validate \
  --recipe recipe.yaml --snapshot snapshot.yaml --phase performance \
  --node-selector my-org/gpu-pool=h100 \
  --toleration dedicated=worker-workload:NoSchedule \
  --toleration dedicated=worker-workload:NoExecute
```

These flags affect the inner benchmark pods that run on GPU nodes (NCCL workers, Dynamo workers), not the validator orchestrator Job itself. For `inference-perf` specifically, `--node-selector` narrows the pool of candidate GPU nodes — the validator then picks the candidate with the most free GPUs (after accounting for in-use DRA allocations) and pins all Dynamo Frontend + worker pods to that node via `kubernetes.io/hostname`. The AIPerf benchmark runner pod is CPU-only, uses a tolerate-all / no-nodeSelector pod spec, and is unaffected by these flags.

### A check reports `skipped` unexpectedly

Skips are always deliberate and always carry a reason, but the location of the
reason in the CTRF entry depends on how the skip happened:

- **Check-level skips** (the CheckFunc ran and returned `validators.Skip(reason)` — e.g., Guards A/B/C on inference, `--no-cluster` from inside a check): reason appears in `stdout` as `level=INFO msg=SKIP reason="…"`.
- **Phase-level skips** (the CheckFunc never ran — e.g., a prior phase failed, so subsequent phases synthesize skip entries; also `--no-cluster` for checks that the runner marks skipped before dispatch): reason appears in `message`, not `stdout`.

Common reasons and their cause:

| Reason (excerpt) | Where it appears | Meaning | Fix |
|------------------|------------------|---------|-----|
| `no inference-throughput or inference-ttft-p99 constraint in recipe` | `stdout` | Check was invoked but recipe is missing the matching constraints | Re-generate the recipe or add the constraints |
| `dynamo-platform not in recipe components` | `stdout` | Inference check selected but `dynamo-platform` absent from `componentRefs` | Use `--platform dynamo` when generating the recipe |
| `DynamoGraphDeployment CRD not installed` | `stdout` | Recipe declares `dynamo-platform` but the operator is not deployed | Run `aicr bundle` + `./deploy.sh` first, or wait for bootstrap to complete |
| `skipped - no-cluster mode` | `message` | `--no-cluster` was passed — the runner short-circuits every phase before dispatching any Job | Remove the flag to run behavioral checks |
| `skipped due to previous phase failure` | `message` | An earlier phase failed and subsequent phases are skipped | Fix the earlier phase first, then re-run |

### Benchmark Job stuck or timed out

Each performance check has a Job-level `activeDeadlineSeconds` set by the catalog's `timeout:`. For `inference-perf`, the full pipeline (workload ready → endpoint health → benchmark) can take up to 30 min on cold-start clusters. If it still times out:

```bash
# validator orchestrator Job + AIPerf benchmark Job both live in aicr-validation.
# The orchestrator is named aicr-inference-perf-<hex> (random suffix per run);
# the AIPerf Job is named aicr-aiperf-<run-id-hash>.
kubectl -n aicr-validation get jobs | grep -E 'aicr-inference-perf-|aicr-aiperf-'

# tail each by full job name (label selectors require exact match)
kubectl -n aicr-validation logs -l job-name=aicr-inference-perf-<hash> --tail=200
kubectl -n aicr-validation logs -l job-name=aicr-aiperf-<run-id-hash>  --tail=200

# the Dynamo workload (DynamoGraphDeployment, Frontend, worker pods,
# ResourceClaimTemplate) lives in a separate per-run namespace:
kubectl get ns | grep aicr-inference-perf-
kubectl -n aicr-inference-perf-<suffix> get dynamographdeployments,pods,svc
```

Common causes: image pull throttling, vLLM model load slowness, and every
candidate GPU node being fully saturated by existing DRA (`ResourceClaim`)
allocations. In the saturated case the validator fails fast with a message
like `no candidate GPU node has free GPUs — all N matched node(s) are
saturated by existing DRA ResourceClaim allocations`; the fix is to free
GPUs on one of the candidate nodes, or to pass
`--node-selector kubernetes.io/hostname=<node>` to target a specific node
you know is free. On clusters where the DRA API is not installed or the
validator's service account cannot list `resourceclaims`, the check falls
back to sizing purely from `Status.Allocatable["nvidia.com/gpu"]` — which
does not account for in-use DRA devices and can leave the benchmark
Pending until timeout on a partially-occupied node.

## Related

- [CLI reference: `aicr validate`](cli-reference.md#aicr-validate) — full flag reference and per-command examples
- [CLI reference: `aicr snapshot`](cli-reference.md#aicr-snapshot) — snapshot capture options
- [CLI reference: `aicr recipe`](cli-reference.md#aicr-recipe) — recipe generation flags
- [Agent deployment](agent-deployment.md) — capture snapshots via an in-cluster Job
- [Data flow: Stage 3 Validate](../integrator/data-flow.md#stage-3-validate-constraint-checking) — how the validator engine is built
- [Validator Development Guide](../contributor/validator.md) — add a new validator (contributor-facing)
