# Validator Development Guide

AICR has **four** distinct validation surfaces. Picking the wrong one
is the single most common source of wasted PRs. Read the table first,
then jump to the matching section. The rest of this page is the
contributor view for all four.

| Surface | When it runs | Where it lives | Mechanism |
|---------|-------------|----------------|-----------|
| [**Constraint**](#constraints-declarative) (declarative) | `aicr validate` against a snapshot | Recipe overlay `validation:` block | `pkg/constraints` evaluator (in-process) |
| [**Container-per-validator check**](#container-per-validator-checks) | `aicr validate` against a live cluster | `validators/<phase>/` + `recipes/validators/catalog.yaml` | One K8s Job per check |
| [**Component validation**](#component-validations-bundle-time) (bundle-time) | `aicr bundle` | `pkg/bundler/validations/checks.go` + `registry.yaml` `validations:` | In-process Go `ValidationFunc` |
| [**Chainsaw health check**](#chainsaw-health-checks) | Two surfaces with distinct runtimes: `make check-health` post-deploy locally (shells out to the `chainsaw` CLI installed on the developer's machine), AND `aicr validate --phase deployment` in-cluster (executes the Test format in-process via `validators/chainsaw/inprocess.go` — no external binary in the deployment validator image) | `recipes/checks/<name>/health-check.yaml` | Chainsaw YAML (Test format on both surfaces; raw K8s YAML asserts use the chainsaw Go library inside `assertRawResources`) |

Rule of thumb: declarative constraint against a snapshot value → surface 1.
Active probe of a live cluster → surface 2 or 4. Pre-deployment sanity
gate on the resolved recipe → surface 3.

## Constraints (declarative)

A **constraint** is a declarative expression — `K8s.server.version >=
1.32.4` — declared in a recipe overlay's `validation:` block and
evaluated by `pkg/constraints` against a measurement from a snapshot.
No code change is needed to add a constraint to an existing recipe;
only to add a new **operator**.

**Where they live in YAML:**

```yaml
# recipes/overlays/<name>.yaml
spec:
  validation:
    constraints:
      - name: K8s.server.version
        value: ">= 1.32.4"
      - name: OS.name
        value: "ubuntu"
    deployment:
      checks: [operator-health, expected-resources]
    performance:
      checks: [nccl-all-reduce-bw]
      constraints:
        - name: nccl-all-reduce-bw
          value: ">= 450"            # GB/s
```

Top-level `constraints` are evaluated as a **pre-flight gate** before
phase checks run; phase-specific `constraints` are evaluated against
each container check's reported metrics.

**Supported operators** (`pkg/constraints/constraint.go`):

| Operator | Use | Notes |
|----------|-----|-------|
| `>=`, `<=`, `>`, `<` | Version / numeric comparison | Always treated as a version comparison; parsed via `pkg/version` |
| `==`, `!=` | Explicit equality / inequality | Version compare if either side parses as version, else string |
| *(none)* | `OperatorExact` | Case-sensitive string equality — `value: "ubuntu"` |

The parser is operator-prefix-longest-first so `>=` wins over `>`.
Anything matching the version heuristic (starts with digit, contains a
dot, optional `v` prefix) is parsed via `pkg/version`. Anything else
falls back to string comparison.

**Evaluation flow:** `ParseConstraintExpression(expr)` →
`ParsedConstraint{Operator, Value, IsVersionComparison}` →
`pc.Evaluate(actual)` returns `(bool, error)`. The evaluator returns
an error (not `false`) when a value claimed to be a version fails to
parse — callers in `pkg/validator/validator.go::checkReadiness` treat
parse errors as `ErrCodeInvalidRequest`, fail-closed.

**Adding a new operator:**

1. Add an `Operator` constant in `pkg/constraints/constraint.go`.
2. Insert it in the operator slice in `ParseConstraintExpression` —
   **longest prefix first** (e.g. `~=` before `~`).
3. Add a `case` arm in `(*ParsedConstraint).Evaluate`. Return an
   `errors.WrapWithContext(ErrCodeInvalidRequest, ...)` for malformed
   inputs; never fall back to string compare silently.
4. Extend the `TestParseConstraintExpression` / `TestEvaluate` table
   in `constraint_test.go`. Both happy path and parse-error path.
5. If the operator implies a numeric range or tolerance, the
   *interpretation* lives in the validator phase (e.g.
   `validators/performance` evaluates NCCL bandwidth with a 10%
   tolerance baked into the check, not the operator).

## Container-per-validator checks

A **check** is a Go function that runs inside a Kubernetes Job spawned
by `aicr validate` against a live cluster. One Job per check, isolated
per run. Per-phase containers are built from
`validators/<phase>/main.go`; the catalog in
`recipes/validators/catalog.yaml` is the authoritative list.

**Three phases**, evaluated in this fixed order
(`pkg/validator/phases.go`): **deployment → conformance → performance**.

| Phase | Purpose | Example |
|-------|---------|---------|
| `deployment` | Components installed and healthy | GPU operator pods running |
| `conformance` | Workload-specific requirements | DRA, gang scheduling, autoscaling |
| `performance` | Cluster meets perf thresholds | NCCL bandwidth, AIPerf TTFT p99 |

Performance runs **last** on purpose: its inference-perf benchmark saturates
every GPU on the node and tears the DynamoGraphDeployment (and its DRA
ResourceClaims) down asynchronously. Running it before conformance starved
conformance's GPU-needing checks (notably `dra-support`, whose 1-GPU test pod
failed to schedule with "cannot allocate all claims" on single-node clusters).

`PhaseAll` (the string `"all"`) is the CLI / recipe wildcard;
`ParsePhaseSelection` collapses it to nil-meaning-everything. It is
**exclusive** — combining `all` with any other phase is rejected.

By default all phases run and produce results regardless of earlier failures —
a performance threshold miss no longer silences conformance results. Pass
`--fail-fast` (or set `spec.validate.execution.failFast: true` in config) to
restore stop-on-first-failure behavior for cost-sensitive runs.

`readiness` is also a field on `ValidationConfig` (see
`pkg/recipe/validation.go`) and appears in overlay examples, but it
is **not** a container-per-validator phase. Readiness runs as
inline constraint evaluation in
`pkg/validator/validator.go::checkReadiness` before any phase
container is scheduled — see [Constraints](#constraints-declarative)
above for how the evaluator works.

### Quick start

Three steps to add a check to an existing validator container.

**1. Implement** in `validators/<phase>/my_check.go`:

```go
func checkMyComponent(ctx *validators.Context) error {
    slog.Info("checking my-component")
    pods, err := ctx.Clientset.CoreV1().Pods("my-namespace").List(
        ctx.Ctx, metav1.ListOptions{LabelSelector: "app=my-component"})
    if err != nil {
        return errors.Wrap(errors.ErrCodeInternal, "failed to list pods", err)
    }
    if len(pods.Items) == 0 {
        return errors.New(errors.ErrCodeNotFound, "no my-component pods found")
    }
    fmt.Printf("Found %d my-component pod(s)\n", len(pods.Items)) // → CTRF evidence
    return nil
}
```

**2. Register** in `validators/<phase>/main.go`:

```go
validators.Run(map[string]validators.CheckFunc{
    "my-component": checkMyComponent,
})
```

**3. Add a catalog entry** in `recipes/validators/catalog.yaml`:

```yaml
- name: my-component
  phase: deployment
  description: "Verify my-component pods are running"
  image: ghcr.io/nvidia/aicr-validators/deployment:latest
  timeout: 2m
  args: ["my-component"]   # must match the registered dispatch key
```

### Container contract

| Exit code | Meaning | CTRF |
|-----------|---------|------|
| `0` | passed | `passed` |
| `1` | failed | `failed` |
| `2` | skipped | `skipped` — return `validators.Skip(reason)` |

| Channel | Captured as |
|---------|-------------|
| **stdout** | CTRF `message` (human-readable evidence) — use `fmt.Printf` |
| **stderr** | Streamed live to the user — use `slog.*` |
| `/dev/termination-log` | Failure reason (≤ 4096 bytes), written on `return error` |

**Mounted data:** `/data/snapshot/snapshot.yaml`, `/data/validation/validation.yaml`
(override via `AICR_SNAPSHOT_PATH`, `AICR_VALIDATION_PATH`).

**Environment** (set by the Job deployer from the catalog entry):

| Variable | Purpose |
|----------|---------|
| `AICR_NAMESPACE` | Validation namespace (fallback) |
| `AICR_CHECK_TIMEOUT` | Go-duration timeout for the check; honored by `ctx.Ctx`. Falls back to `defaults.CheckExecutionTimeout` if unset or malformed (logged WARN). |
| `AICR_VALIDATOR_IMAGE_REGISTRY` | Override the image registry prefix (CLI passes through to inner workloads). |
| `AICR_VALIDATOR_IMAGE_TAG` | Override the resolved tag when the binary's stamped commit has no published image (e.g. `edge` or `sha-<commit>`). See [Validator image tags](#validator-image-tags). Forwarded to inner workloads (including `aiperf-bench`). |
| `AICR_NODE_SELECTOR` | Comma-separated `key=value`; read via `ctx.NodeSelector` |
| `AICR_TOLERATIONS` | Comma-separated `key=value:effect`; read via `ctx.Tolerations` |
| `AICR_REQUIRE_SCOPED_INFERENCE_GATEWAY` | When truthy, the `inference-gateway` check fails if the gateway's `LoadBalancer` Service is open to `0.0.0.0/0` — its `spec.loadBalancerSourceRanges` is empty or includes an any-source CIDR (`0.0.0.0/0` or `::/0`). Default (unset): the open exposure is recorded and warned but the check still passes. |

**RBAC.** The engine creates a per-run ServiceAccount and
ClusterRoleBinding named `aicr-validator-<runID>`. Per-run naming
prevents concurrent runs from clobbering each other's RBAC. External
tooling selects by label `app.kubernetes.io/name=aicr-validator`, not
literal name.

**Image-pull policy** is computed by `v1.ImagePullPolicy(image,
imageTagOverride)` in `pkg/validator/v1/job_plan.go`:
side-loaded (`ko.local/*`, `kind.local/*`) → `Never`;
digest-pinned (`name@sha256:…`) → `IfNotPresent`;
`AICR_VALIDATOR_IMAGE_TAG` set or `:latest` suffix → `Always`;
otherwise → `IfNotPresent`. Both the outer validator Job and any
inner workload Job share this helper so policy cannot drift.

### Validator image tags

The catalog declares every validator image as `…:latest`;
`catalog.ResolveImage` (`pkg/validator/catalog/catalog.go`) rewrites that
tag at runtime so the validators match the `aicr` binary that launched
them:

1. **Stamped build** — the binary's version + commit resolve the tag.
   `ResolveImage` checks the version first: a **release** build → that
   release's version tag (`:vX.Y.Z`, or `:vX.Y.Z-rc…` for a pre-release);
   otherwise a dev/`main` build →
   `:sha-<commit>`, the immutable per-commit image CI publishes for `main`
   pushes (only — see the caveat below the table).
2. **`AICR_VALIDATOR_IMAGE_TAG` set** — overrides step 1 for *all* catalog
   images uniformly, including the inner `aiperf-bench` runner the
   `performance` validator launches (so both must exist at that tag).

What CI publishes:

| Trigger | Tags built (`on-push.yaml` / `on-tag.yaml`) |
|---------|----------------------------------------------|
| Push to `main`, not docs-only | `:sha-<full-commit>` (immutable) **and** `:edge` (moving → latest validator-image build) |
| Stable release `vX.Y.Z` | `:vX.Y.Z` **and** `:latest` |
| Pre-release `vX.Y.Z-rc…` | `:vX.Y.Z-rc…` only — **not** `:latest` |

`on-push.yaml` runs **only on `main`** and is skipped when a push touches
*only* docs (`paths-ignore: **.md`, `docs/**`, `LICENSE`). So no
`:sha-<commit>` is built — and `:edge` is not advanced — for a docs-only
`main` commit, nor for any feature-branch / PR commit (the build job is
gated to `refs/heads/main`). `:edge` therefore tracks the last `main`
commit that ran the image build, *not necessarily* HEAD, and
`sha-$(git rev-parse origin/main)` can 404 right after a docs-only merge.
Confirm the tag exists (see below) and fall back to `:edge` or the last
published SHA.

**`:latest` is the last _stable_ release, never `main`.** It is moved only
by the on-tag release pipeline for stable tags (the `:latest` step is gated
on a non-pre-release tag), so a validator change merged to `main` after the
last stable release is absent from `:latest` until the next one. Running
`AICR_VALIDATOR_IMAGE_TAG=latest` against a `main`-tracking recipe can
therefore silently run *older* validator behavior — e.g. a
`performance.constraints` pin such as `inference-model` /
`inference-concurrency-per-gpu` is only honored by a validator new enough
to read it; an older `:latest` validator ignores the pin and runs its
compiled default, which can surface as a misleading result rather than a
clear version error.

**To run the validator built on `main`** (e.g. testing a recipe whose pins
are not yet in a release), point at `:edge` or a published `main` commit —
*not* `:latest`:

```shell
# Moving tag — latest main validator-image build:
AICR_VALIDATOR_IMAGE_TAG=edge aicr validate -r recipe.yaml -s snapshot.yaml --phase performance

# Immutable pin (reproducible) — use a published main commit, not blindly HEAD
# (a docs-only HEAD has no image; verify with the registry check below):
AICR_VALIDATOR_IMAGE_TAG=sha-<published-main-commit> aicr validate -r recipe.yaml -s snapshot.yaml ...
```

A bare `go build` stamps `commit: unknown`, so step 1 can't resolve a
`:sha-<commit>` tag and the override is required. `make build` stamps the
commit — but CI publishes `:sha-<commit>` images **only for `main`** (the
build job is gated to `refs/heads/main`), so auto-resolution works only
when you build from a `main` commit whose image exists. Any feature-branch,
fork, or PR build (pushed or not) stamps a SHA with **no** published image
and still needs `AICR_VALIDATOR_IMAGE_TAG=edge` (or a published `main`
SHA) — `:edge` is the closest tag to your branch.

Find or trace the `main` tag against GitHub Container Registry (GHCR) —
public read:

```shell
REPO=nvidia/aicr-validators/performance
SHA=$(git rev-parse origin/main)
TOKEN=$(curl -s "https://ghcr.io/token?scope=repository:${REPO}:pull" | jq -r .token)

# Does the image for this main commit exist? (200 = yes)
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" \
  -H 'Accept: application/vnd.oci.image.index.v1+json' \
  "https://ghcr.io/v2/${REPO}/manifests/sha-${SHA}"
```

To go the other way — which commit built a given image — read the OCI
labels baked in by CI: `org.opencontainers.image.revision=<commit>` and
`org.opencontainers.image.version=main-<commit>`.

### `validators.Context` API

`LoadContext()` builds it from the container environment and returns
the only struct a `CheckFunc` ever sees:

```go
type Context struct {
    Ctx             context.Context
    Cancel          context.CancelFunc
    Clientset       kubernetes.Interface
    RESTConfig      *rest.Config
    DynamicClient   dynamic.Interface
    Snapshot        *snapshotter.Snapshot
    ValidationInput *v1.ValidationInput
    Namespace       string
    NodeSelector    map[string]string   // nil = use defaults
    Tolerations     []corev1.Toleration // nil = use defaults
}
```

`ctx.Timeout(d)` returns a child context with a shorter deadline.
`validators.Run(map)` is the container entry point; it dispatches by
`os.Args[1]`, maps `Skip` → exit 2, errors → exit 1, nil → exit 0.

**Scheduling overrides.** When creating inner workloads, check
`ctx.NodeSelector` and `ctx.Tolerations` before applying hardcoded
platform selectors. `nodeName` pinning (e.g. nvidia-smi, DRA
isolation) bypasses the scheduler and should not apply
`ctx.NodeSelector`.

### `PodLifecycle` helper

For checks that deploy a single test pod (training NCCL, conformance
DRA isolation, nvidia-smi probes), use `validators/helper/pod.go`
rather than reimplementing watch/cleanup:

```go
lc := &helper.PodLifecycle{Clientset: ctx.Clientset, Namespace: ctx.Namespace}
pod, err := lc.CreatePodFromTemplate(ctx.Ctx, "testdata/probe.yaml.tmpl", subs)
if err != nil { return errors.Wrap(...) }
defer func() { _ = lc.CleanupPod(context.Background(), pod) }() // deferred cleanup uses fresh ctx

if err := lc.WaitForPodSuccess(ctx.Ctx, pod, defaults.PodSuccessTimeout); err != nil {
    logs, _ := lc.GetPodLogs(context.Background(), pod)
    return errors.WrapWithContext(errors.ErrCodeInternal, "probe failed", err,
        map[string]any{"logs": logs})
}
```

`WaitForPodSuccess`/`WaitForPodRunning` use the watch API
(`pkg/k8s/pod`) — no polling, no sleep loops. The cleanup goroutine
must use `context.Background()` because the parent is canceled on
return; this is one of the two CLAUDE.md-sanctioned uses of `Background()`.

### Pre-flight gates are fail-closed

`pkg/validator/validator.go::checkReadiness` evaluates top-level
`validation.constraints` *before* any phase runs. A parse error or a
failing constraint returns `ErrCodeInvalidRequest` and aborts the
entire run. **Do not** `slog.Warn; continue` on an evaluator
error — that masquerades a broken validation YAML as a passing
constraint, which is an explicit anti-pattern in CLAUDE.md.

The `dependencyAffinity` pre-flight (validator catalog entries
declaring a required dependency) follows the same rule.

### Performance benchmark tuning

Performance checks ship validation *methodology* knobs as env vars on
the catalog entry (overridable via `aicr validate ... --data`).
Pass/fail thresholds live in the recipe overlay constraints; methodology
lives with the validator. A value that fails to parse fails the check
with `ErrCodeInvalidRequest` *before* any workload deploys — never
silently fall back.

Full list (defaults, semantics) is in the `validators/performance`
package godoc. NCCL variants exposed today: `nccl-all-reduce-bw`,
`nccl-all-reduce-bw-net`, `nccl-all-reduce-bw-nvls`. Inference:
`inference-perf` (Dynamo + AIPerf).

> **Constraint-name contract.** Each NCCL variant looks up a
> constraint with the *exact* same name as the check. A recipe
> running the `-net` or `-nvls` variant **must** declare a same-named
> constraint; the variant will Skip if only the generic
> `nccl-all-reduce-bw` constraint is present.

#### `inference-perf`: model, concurrency, and weights cache

The `inference-perf` check warms vLLM before measuring, so the one-time
CUDA-graph/JIT compile cost is excluded from the reported throughput and
p99 TTFT. Its knobs are read by the in-cluster validator from the
`inference-perf` catalog entry's `env` (override per run with a catalog
overlay in the `aicr validate --data <dir>` directory). Unlike `HF_TOKEN`,
they are **not** forwarded from the orchestrator shell, so
`export AICR_INFERENCE_PERF_…` before `aicr validate` has no effect.

The **model** and **per-GPU concurrency** can also be set per accelerator in
the recipe overlay's `performance.constraints`, symmetric with the
throughput / TTFT thresholds:

```yaml
validation:
  performance:
    constraints:
      - name: inference-model
        value: Qwen/Qwen3-8B          # HF model ID (bare value, no comparator)
      - name: inference-concurrency-per-gpu
        value: "256"                  # positive integer
      - name: inference-throughput
        value: ">= 50000"
      - name: inference-ttft-p99
        value: "<= 2000"
```

Resolution precedence is **recipe constraint > catalog env knob > compiled
default** (`Qwen/Qwen3-8B` at 256/GPU). A non-positive / non-integer
`inference-concurrency-per-gpu` fails closed with `ErrCodeInvalidRequest`.

| Variable | Default | Effect |
|----------|---------|--------|
| `AICR_INFERENCE_PERF_CONCURRENCY_PER_GPU` | `256` | Concurrent requests per GPU; total is this × free GPUs on the chosen node. Prefer the per-accelerator `inference-concurrency-per-gpu` recipe constraint over this global knob. |
| `AICR_INFERENCE_PERF_MODEL` | `Qwen/Qwen3-8B` | Hugging Face model ID to benchmark. Override per accelerator via the `inference-model` recipe constraint. |
| `AICR_INFERENCE_PERF_WORKLOAD_READY_TIMEOUT` | `10m` | Wait for the `DynamoGraphDeployment` to become ready (image pull + model load + worker health). Large models load slower — raise this **and** the catalog entry's `timeout` in tandem, or the parent deadline caps it. |
| `AICR_INFERENCE_PERF_HEALTH_TIMEOUT` | `5m` | Wait for the endpoint to serve a real chat-completion *after* the workload reports Ready. Concurrent first-load from one RWO cache PVC can push first-serve past 5m; raise it (bounded by the catalog `timeout`). |
| `AICR_INFERENCE_PERF_MODEL_CACHE_SIZE` | `100Gi` (on) | The PVC-backed model-weights cache is **on by default**. Set a different K8s quantity to resize, or a disable sentinel (`off`/`0`/`none`/`disabled`) to turn it off and download from HF directly. |
| `AICR_INFERENCE_PERF_MODEL_CACHE_STORAGE_CLASS` | cluster default | StorageClass for the cache PVC. On a cluster with **no default SC and no value here**, the check **fails fast** with guidance rather than leaving the PVC `Pending` until timeout. AICR-deployed EKS gets a default `gp3` SC from `aws-ebs-csi-driver`; GKE has `standard-rwo`. |

For gated models, or to lift Hugging Face rate limits on large downloads,
set `HF_TOKEN` in the orchestrator environment: it is forwarded only to the
`inference-perf` validator, which provisions an optional `aicr-hf-token`
Secret the benchmark workers reference via `secretKeyRef`. A token raises
*per-account* limits but does not bypass Hugging Face *per-IP* throttling —
large models pulled by many workers benefit most from the shared cache.

**Model-weights cache (PVC).** Many workers re-downloading a large model (and
re-downloading on every crash-restart) repeatedly trips Hugging Face's
**per-IP** throttle, so the cache is **on by default**:

1. The validator creates an `aicr-model-cache` **PVC** (ReadWriteOnce) in the
   per-run namespace.
2. A one-time **populate Job** — pinned to the same node the workers use (so
   the `WaitForFirstConsumer` RWO volume binds there) — downloads
   `config.model` into the PVC via `huggingface_hub` (using `HF_TOKEN` if
   present). The validator blocks on it before deploying. The populate
   container carries CPU/memory **requests** but no memory **limit** — a limit
   OOMKills large-model downloads via page cache on cgroup v2.
3. Workers mount the PVC **read-only** at `HF_HOME` with `HF_HUB_OFFLINE=1`,
   loading weights locally and never reaching HF (failing closed if the cache
   is incomplete).

The PVC lives in the per-run namespace and is torn down on cleanup, so the
cache is **intra-run** (one download shared by the run's N workers), not
persisted across runs. Because it is RWO, all workers co-locate on one node —
which the validator already enforces for a stable per-node baseline. Multi-node
would require RWX storage (e.g. EFS); for at-scale serving, Dynamo's
ModelExpress server is the alternative (see #1116).

> **Throughput-gate scaling.** `buildInferenceConfig` sizes the workload to the
> **free** GPUs on the chosen node, which on a shared node is fewer than the
> full allocatable count. The `inference-throughput` gate is therefore scaled
> by `freeGPUs / nodeGPUs` (throughput is ~linear in GPU count at fixed
> per-GPU concurrency) so a healthy per-GPU result on a partially occupied node
> is not failed against a full-node number. TTFT is a per-request latency and
> is **not** scaled.

#### Methodology: a baseline gate, and reading run-to-run fluctuation

`inference-perf` is a **conformance baseline**, not a tuned peak-throughput
benchmark — pass/fail answers *"is this deployment serving acceptably,"* not
*"what is the maximum."* Read the numbers as a health floor, not a leaderboard.
Design choices follow from that, and from what we measured debugging
run-to-run TTFT fluctuation (see NVIDIA/aicr#1192):

- **Throughput is the stable, discriminating signal; TTFT p99 is noisy at high
  concurrency.** Near the saturation knee the p99 curve is steep, so batching /
  scheduling timing produces large run-to-run swings on an otherwise healthy
  deployment. That is why the `inference-ttft-p99` constraint is a **generous
  ceiling** (catches gross stalls — real ones ran 9–45 s — while tolerating
  normal knee jitter), not a tight target.
- **The verdict should reflect the deployment, not RNG.** The AIPerf workload is
  pinned for reproducibility — fixed random seed, fixed input/output token
  counts (stddev 0), a pinned prompt pool, and greedy decoding
  (`temperature: 0`). Input determinism stabilizes *throughput*; it does not
  remove system-side p99 jitter at the knee.
- **Routing matters.** The inference-perf workload uses Dynamo's load-aware
  least-loaded router (`DYN_ROUTER_MODE=least-loaded`), which balances by each
  worker's active in-flight load so a transiently-slow worker stops receiving
  its full share — mitigating the stochastic EKS H100 worker-stall / throughput
  degradation at the saturation knee (issue #1197). Frontend-to-worker
  requests use Dynamo's request plane (Dynamo 1.2 defaults to TCP; AICR does
  not set `DYN_REQUEST_PLANE=nats`). Workers still publish local vLLM KV-cache
  events through their ZMQ publisher (relayed onto the NATS event plane), but
  least-loaded routing does not consume them.
  The `inference-routing-mode` recipe input defaults to `dynamo-router`; set
  `gateway-epp` to validate the GAIE/EPP path through agentgateway with worker
  frontend sidecars in direct mode. The direct-mode sidecars honor EPP routing
  headers; they do not perform the ZMQ-to-NATS KV-event relay.
- **The AIPerf load generator co-locates with the GPU workers, but that is not
  resource contention.** It is CPU-only and the GPU node has ample CPU headroom
  (measured node CPU pressure ≈ 0 across runs); co-location does not starve the
  workers. Do not add worker CPU/memory requests to "fix" contention that the
  data does not show.
- **Triaging an anomalous run:** the severe stalls we saw were **stochastic and
  often not reproducible** — re-run before concluding. Verify GPU health
  (clocks, ECC, throttle reasons, XID) to rule out hardware. And note
  `nvidia-smi` *utilization* is a duty-cycle metric (kernel-present time), **not**
  compute saturation — a worker can read 100% util while under-fed; cross-check
  **power draw and achieved throughput**, not utilization alone.
- **A GPU driver restart needs a DRA plugin restart.** If you restart the GPU
  driver pod (`nvidia-driver-daemonset-*`) on a node — e.g. to clear suspected
  driver state between runs — also restart the NVIDIA DRA kubelet-plugin
  (`nvidia-dra-driver-gpu-kubelet-plugin-*`) on that node. Otherwise it serves
  stale CDI specs and every worker `ResourceClaim` fails with
  `FailedPrepareDynamicResources: … empty device edits`, leaving the decode
  workers stuck in `ContainerCreating` until the phase times out.
- **The serve-readiness probe tolerates cold-start first-token latency.** A fresh
  worker's first inference captures CUDA graphs / JIT-warms kernels — measured at
  ~42 s on RTX PRO 6000. The readiness probe (`waitForEndpointReady`) therefore
  uses a generous **120 s** per-request timeout (`InferenceEndpointProbeTimeout`),
  not the generic 30 s `HTTPClientTimeout`; the latter cancelled the legitimate
  first request mid-warmup and failed healthy deployments with
  `timed out waiting for inference endpoint to serve requests` — the *same* outer
  symptom as the (fixed) #1192 discovery panic but a different root cause. AIPerf's
  own warmup absorbs steady-state once the probe passes.
- **Inspecting a failed run.** `AICR_INFERENCE_PERF_NO_CLEANUP=1` leaves the
  namespace, DGD, workers, frontend, and AIPerf Job in place after the run so a
  serve-wait / generate hang can be examined live (`kubectl logs` the frontend,
  ping `/v1/models` and `/v1/chat/completions`). Debug-only — delete the namespace
  manually afterward.

### Code walkthrough

```go
// validators/deployment/operator_health.go
func checkOperatorHealth(ctx *validators.Context) error {
    slog.Info("listing pods", "namespace", gpuOperatorNamespace)            // → stderr
    pods, err := ctx.Clientset.CoreV1().Pods(gpuOperatorNamespace).List(
        ctx.Ctx, metav1.ListOptions{LabelSelector: gpuOperatorLabel})
    if err != nil {
        return errors.Wrap(errors.ErrCodeInternal, "failed to list pods", err)
    }
    fmt.Printf("Found %d gpu-operator pod(s):\n", len(pods.Items))          // → CTRF evidence
    for _, p := range pods.Items {
        fmt.Printf("  %s: %s\n", p.Name, p.Status.Phase)
    }
    if runningCount == 0 {
        return errors.New(errors.ErrCodeInternal, "no pods in Running state")
    }
    return nil
}
```

`slog.*` → stderr → streamed live. `fmt.Printf` → stdout → captured
as CTRF evidence. `return nil` → 0, `return error` → 1,
`return validators.Skip(reason)` → 2.

### Directory layout

```text
validators/
├── context.go                # LoadContext, Context type
├── runner.go                 # Run() entry, exit-code mapping
├── helper/pod.go             # PodLifecycle (watch, logs, cleanup)
├── deployment/               # phase image: deployment
├── performance/              # phase image: performance (+ aiperf-bench.Dockerfile)
└── conformance/              # phase image: conformance
```

Each phase directory compiles to one container image; multiple checks
share the binary, selected by `os.Args[1]`.

## Component validations (bundle-time)

A **component validation** is an in-process Go function that runs
during `aicr bundle` to catch component misconfigurations the recipe
parser and Helm chart won't catch on their own — required flags
unset, incompatible host-resource requests, missing dependency
components.

Runs **in-process**, no network, no Kubernetes. Anything requiring a
real cluster belongs in a container-per-validator check or chainsaw
health check, not here.

### Declaring a validation

Add a `validations:` block to the component entry in
`recipes/registry.yaml`:

```yaml
components:
  - name: nodewright-customizations
    validations:
      - function: CheckWorkloadSelectorMissing
        severity: warning              # warning (non-blocking) | error (blocking)
        conditions:
          intent: [training]           # AND across keys, OR within a key
        message: "May cause nodewright to evict running training jobs."
```

| Field | Required | Notes |
|-------|----------|-------|
| `function` | yes | Must match a name registered in `pkg/bundler/validations/checks.go::init()` |
| `severity` | yes | `warning` appends to report; `error` stops the bundle |
| `conditions` | no | Keys are criteria fields from `pkg/recipe/criteria.go`. Empty = always runs |
| `message` | no | Actionable detail appended to function output |

Conditions are evaluated via `checkConditions(recipeResult, conditions)`.
Keys = AND across, values within a key = OR. When a new accelerator,
service, OS, intent, or platform is added to `pkg/recipe/criteria.go`,
audit existing condition blocks per CLAUDE.md's enum-expansion rule.

### Shipping functions

| Function | Checks |
|----------|--------|
| `CheckWorkloadSelectorMissing` | nodewright `--workload-selector` set when conditions match |
| `CheckAcceleratedSelectorMissing` | nodewright `--accelerated-node-selector` set |
| `CheckHostMofedWithoutNetworkOperator` | Host-mode MOFED component paired with `network-operator` |

Registered in `pkg/bundler/validations/checks.go::init()`.

### ValidationFunc signature

Fixed (`pkg/bundler/validations/interface.go`):

```go
type ValidationFunc func(
    ctx context.Context,
    componentName string,
    recipeResult *recipe.RecipeResult,
    bundlerConfig *config.Config,
    conditions map[string][]string,
) (warnings []string, errors []error)
```

- `componentName` is the registry name; resolve component refs via `recipeResult.ComponentRefs`.
- `bundlerConfig` exposes CLI flags and merged values.
- `conditions` is the YAML block, not the resolved criteria — use `checkConditions(recipeResult, conditions)` to gate.

### Adding a new function

1. Implement in `pkg/bundler/validations/checks.go` matching `ValidationFunc`.
2. Register: `registerCheck("CheckMyCondition", CheckMyCondition)` in `init()`.
3. Wire into a component's `validations:` block in `registry.yaml`.
4. Add a table-driven test in `checks_test.go` exercising every condition branch with synthetic `RecipeResult` and `bundlerConfig`. No cluster, no network.

### Common pitfalls

- **Function name typo in YAML.** Fails closed — `RunValidations` raises
  `ErrCodeInvalidRequest` ("unknown validation function") rather than
  skipping the check. Add a test that calls `Get("...")` for every
  shipping check.
- **Returning an error when you mean a warning.** Errors stop the
  bundle. If the user can ship through it, return a warning.
- **Network or K8s calls.** Bundle must work offline. Push cluster
  probes to surface 2 or 4.

## Chainsaw health checks

A **chainsaw health check** is a YAML test in
`recipes/checks/<component>/health-check.yaml` that asserts a
deployed component's state. Runs against a real cluster (typically a
Kind cluster after `aicr bundle` + `helm install`) via the
[Chainsaw](https://kyverno.github.io/chainsaw/) test runner.

The same assertion file now powers TWO surfaces:

1. **`make check-health` / `make check-health-all`** — local Kind-cluster
   sanity invoked manually by chart authors.
2. **`aicr validate --phase deployment`** — registry-declared content is
   loaded into `ComponentRef.HealthCheckAsserts` during recipe
   resolution (PR #1219) and executed by the deployment validator's
   chainsaw runner (PR #1220). Since #1236 the runner is **pure Go**:
   `validators/chainsaw/inprocess.go` unmarshals the
   `chainsaw.kyverno.io/v1alpha1` Test, walks `spec.steps[].try[]`, and
   dispatches `assert` / `error` to kyverno-json's `checks.Check` engine
   against live cluster state. No external binary is shipped in the
   deployment validator image. CLI output is source-tagged `[chainsaw]`
   vs `[expectedResources]` so operators can disambiguate when both
   paths report on the same component.

**Registration.** A component opts in by declaring
`healthCheck.assertFile` in `recipes/registry.yaml`:

```yaml
components:
  - name: nfd
    healthCheck:
      assertFile: checks/nfd/health-check.yaml
```

The path is relative to `recipes/`. `make check-health COMPONENT=<name>`
invokes Chainsaw against
`recipes/checks/<name>/health-check.yaml` (no-cluster flag has no
effect here — chainsaw always needs a real cluster).

**Assertion file** is plain Chainsaw:

```yaml
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-health-check
spec:
  timeouts: { assert: 5m }
  steps:
    - name: validate-deployment-exists
      try:
        - assert:
            resource:
              apiVersion: apps/v1
              kind: Deployment
              metadata: { name: gpu-operator, namespace: gpu-operator }
              status: { (availableReplicas > `0`): true }
```

Use Chainsaw's `assert` (expected match) and `error` (unexpected match
must not exist). Always include an existence guard before phase
assertions so an empty namespace can't yield a vacuous pass. See the
[Chainsaw assert reference](https://kyverno.github.io/chainsaw/latest/operations/assert/)
for the full operator list.

**Read-only allowlist.** Registry-declared assert files MUST use only
`assert` and `error` operations. The deployment validator Job runs
under a ServiceAccount bound to cluster-admin, so registry content is
restricted at runtime to read-only Chainsaw operations
(`validators/chainsaw/allowlist.go`). Any other operation (`script`,
`apply`, `create`, `delete`, `patch`, `update`, `wait`, `command`,
`sleep`, `podLogs`, `events`, `describe`, `get`) is rejected with
`ErrCodeInvalidRequest`. PR #1223 will add the same enforcement at
lint time so violations are caught before they ever reach the
validator.

**Running:**

```bash
make check-health COMPONENT=gpu-operator   # one component
make check-health-all                      # everything in recipes/checks/
make validate-local RECIPE=recipe.yaml     # full pipeline in Kind
```

### Timeout budgeting

During `aicr validate --phase deployment`, registry health checks in
`recipes/checks/<component>/health-check.yaml` run in-process inside
the `expected-resources` check (`validators/chainsaw/inprocess.go`).

A Test's `spec.timeouts.assert` is the **whole-Test budget** — one
deadline shared across every step and retry. Slurm's
[`health-check.yaml`](https://github.com/NVIDIA/aicr/blob/main/recipes/checks/slinky-slurm/health-check.yaml)
uses `assert: 7m` so workload-readiness steps can converge before the
pod-phase guard runs.

The `expected-resources` catalog timeout (8m in
`recipes/validators/catalog.yaml`) is the **outer** envelope. It must
exceed the longest in-tree `assert` value plus headroom for
pre-chainsaw work, chainsaw teardown, and log flush
(`defaults.JobEnvelopeMargin`). If assert runs too close to that
catalog deadline, the Job can SIGKILL the pod before chainsaw reports
the failing step — operators see truncated output instead of a useful
failure. Raise the catalog `timeout` in tandem when you need a longer
assert budget (`TestExpectedResourcesCatalogEnvelope` guards this).

## Constraint evaluation algorithm

`pkg/constraints` is shared by surface 1, surface 2's recipe
constraints, and the readiness pre-flight gate. The evaluation flow:

1. **Parse.** `ParseConstraintExpression(expr)` strips whitespace,
   finds the **longest** matching operator prefix (so `>=` wins over
   `>`), splits into `{Operator, Value}`. Empty value → `ErrCodeInvalidRequest`.
2. **Classify.** Operators other than `Exact`/`EQ`/`NE` are always
   version comparisons. `EQ`/`NE` are version comparisons iff the
   value passes `looksLikeVersion` (starts with digit, has a dot,
   optional `v` prefix). Everything else is string.
3. **Evaluate** against the snapshot measurement. Version compares
   route through `pkg/version.Compare` (semver-aware). String
   compares are case-sensitive equality.
4. **Errors propagate, not bools.** A value declared as `>= 1.32.4`
   that fails to parse as a version returns
   `errors.WrapWithContext(ErrCodeInvalidRequest, "cannot parse
   actual version", err, ...)` — not `false`. The caller (validator
   pre-flight gate) must surface this as a failed constraint, not a
   passing one. This is the fail-closed invariant.

Tolerance and range semantics (e.g. NCCL's 10% slack) live in the
**check** that produces the measurement, not in the operator. The
operator vocabulary stays minimal on purpose.

## Testing checklist

Patterns common to all four surfaces.

- **`--no-cluster` is mandatory** for any test that touches
  `pkg/validator` or `aicr validate` outside an explicit live-cluster
  fixture. `validator.New(validator.WithNoCluster(true))` for unit
  tests; the `--no-cluster` CLI flag for e2e and chainsaw. When
  `NoCluster` is true, RBAC and Jobs are skipped, all checks report
  `skipped - no-cluster mode`, but constraints still evaluate.
- **Table-driven tests.** Required for multi-case logic per CLAUDE.md.
  See `pkg/constraints/constraint_test.go` and
  `pkg/bundler/validations/checks_test.go` for the canonical shapes.
- **Synthetic inputs.** Component validations take a hand-built
  `RecipeResult` and `bundlerConfig`. Container checks take a
  `validators.Context` with `fake.NewClientset(...)`.
- **Chainsaw against Kind.** `make check-health COMPONENT=<name>`
  runs against the local Kind cluster set up by `make dev-env`. KWOK
  cannot host chainsaw checks that need real workloads — see
  [tests.md](tests.md#kwok-matrix-testing) for what KWOK does and doesn't
  cover.
- **CTRF output.** Container checks emit JSON via the runner. Assert
  on status/message in integration tests, not raw stdout.

## Common pitfalls

- **`slog.Warn; continue` on a constraint or `ValidationFunc` parse
  error.** Masquerades broken YAML as passing. Fail closed — return
  `ErrCodeInvalidRequest`. (CLAUDE.md anti-pattern.)
- **Function-name typo in `registry.yaml` `validations:` block.**
  Fails closed — `RunValidations` raises `ErrCodeInvalidRequest`
  ("unknown validation function"). Add a registry-lookup test for every
  shipping function.
- **`yaml.Marshal` on `map[string]any` for output that feeds CTRF or
  a digest.** `yaml.v3` walks randomized Go map order. Use
  `serializer.MarshalYAMLDeterministic`.
- **Container check that requires a real GPU node profile.** KWOK
  fakes labels and topology but not GPU runtime. Gate such checks
  behind a `nvidia.com/gpu` resource check that lets KWOK runs Skip
  cleanly.
- **Network calls in a component validation.** Bundle must work
  offline. Push to a container check or chainsaw check instead.
- **Re-pushing the same image tag during dev (`:dev`).** K8s default
  `IfNotPresent` keeps the stale image on previously-pulled nodes.
  Suffix per iteration (`:dev-v1`, `:dev-$(git rev-parse --short HEAD)`).

## See Also

- [recipe.md](recipe.md) — recipe overlays and the `validation:` block
- [tests.md](tests.md#kwok-matrix-testing) — recipe matrix tests without GPU hardware
- [Validator Extension Guide](../integrator/validator-extension.md) — external validators via `--data`
- [CLAUDE.md](https://github.com/NVIDIA/aicr/blob/main/.claude/CLAUDE.md) — anti-patterns: fail-closed gates, `slog.Warn; continue`, watch-over-poll, `--no-cluster`
- [Validator V2 ADR](https://github.com/NVIDIA/aicr/blob/main/docs/design/002-validatorv2-adr.md) — container-per-validator architecture decision
- [Validator Catalog](https://github.com/NVIDIA/aicr/tree/main/recipes/validators) — authoritative `catalog.yaml`
