# Validating a Cluster

Task-oriented walkthrough for running `aicr validate` against a GPU cluster — from
capturing a snapshot through interpreting results. Covers both training and
inference workloads and all three validation phases (deployment, conformance,
performance).

For per-flag reference, see [CLI reference: aicr validate](cli-reference.md#aicr-validate).
For the architectural view of how snapshot + recipe flow into the validator, see
[Data flow: Stage 3 Validate](../integrator/data-flow.md#stage-3-validate-constraint-checking).

## When to validate

| Phase | What it answers | Typical trigger |
|-------|-----------------|-----------------|
| `deployment` | Are the components the recipe asks for actually installed and healthy? | After `./deploy.sh` finishes, before running any workload |
| `conformance` | Does the cluster support workload-specific capabilities (DRA, gang scheduling, autoscaling, ...)? | Before opening the cluster to real workloads |
| `performance` | Does the cluster hit expected bandwidth / throughput thresholds? | After components are ready; before going to production |

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
| `nccl-all-reduce-bw` | Auto-detect (whatever NCCL picks) | Default for H100 on EKS/GKE, and for GB200/B200 on non-EKS services without a named-variant pairing (e.g. B200, or GB200 outside EKS/OKE). Preserves the pre-variant behavior. |
| `nccl-all-reduce-bw-net` | NET (EFA on EKS) | GB200 + EKS. Asserts EFA actually carried traffic — catches silent fallback to Socket when the NVIDIA driver is missing `NVreg_GrdmaPciTopoCheckOverride=1`. |
| `nccl-all-reduce-bw-nvls` | NVLS (MNNVL across an NVL72 IMEX domain) | GB200 + EKS, and GB200 + OKE. Asserts the NVLS communicator actually initialized — catches silent fallback to EFA (EKS) or Socket (OKE) when the IMEX domain is misconfigured. |

GB200/EKS recipes (both `training` and `inference` intents) enable `-net` and
`-nvls` together rather than the auto-detect variant, because those nodes
expose two inter-node fabrics simultaneously and a single auto-detect test
would only exercise one of them.

GB200/OKE recipes enable `-nvls` only: OKE NET/RDMA stays out of the support
matrix until the OCI testbed proves a non-Socket NCCL transport end to end, so
OKE validates the NVL72 IMEX fabric without an EFA/NET counterpart.

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
`DynamoGraphDeployment` with a vLLM-served model (Qwen/Qwen3-8B by default,
overridable per accelerator — see below) plus an AIPerf benchmark Job, and
measures end-to-end output-token throughput and time-to-first-token (TTFT) p99.

**Warm-up:** AIPerf sends a wave of warm-up requests *before* the measured run,
so vLLM's one-time CUDA-graph / JIT compilation (tens of seconds on a cold
worker) is excluded from the reported throughput and p99 TTFT — the numbers
reflect steady state, not cold start. Warm-up scales with concurrency and is
tunable via `AICR_INFERENCE_PERF_WARMUP_PER_CONCURRENCY` (see the
[validator reference](../contributor/validator.md#inference-perf-benchmark-tuning)).

**Determinism:** the benchmark is driven reproducibly so the verdict reflects the
deployment, not run-to-run RNG — a fixed random seed, fixed input/output token
counts (stddev 0), a pinned synthetic-prompt pool, and greedy decoding
(`temperature: 0`). Note that throughput (not the latency tail) is the stable,
discriminating signal at high concurrency; TTFT p99 near the saturation knee can
still vary with batching/scheduling, which is why the TTFT constraint is a
generous ceiling rather than a tight target.

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
      # Pass/fail thresholds (10% tolerance applied by the evaluator). Values
      # shown are the measured H100 gate at 8B/256; each overlay sets its own.
      - name: inference-throughput   # output tokens/sec
        value: ">= 50000"
      - name: inference-ttft-p99     # time-to-first-token p99 in ms
        value: "<= 2000"
      # Optional per-accelerator inputs (bare value, no comparator).
      # Precedence: recipe > AICR_INFERENCE_PERF_* env > compiled default.
      - name: inference-model                # HF model ID; default Qwen/Qwen3-8B
        value: Qwen/Qwen3-8B
      - name: inference-concurrency-per-gpu  # positive integer; default 256
        value: "256"
```

`inference-model` and `inference-concurrency-per-gpu` are optional: omit them to
use the compiled defaults (Qwen3-8B at 256 concurrent requests per GPU), set them
per overlay to tune model and load for each accelerator, or override globally
with the `AICR_INFERENCE_PERF_MODEL` / `AICR_INFERENCE_PERF_CONCURRENCY_PER_GPU`
catalog knobs (recipe wins over catalog env wins over default).

**Model-weights cache and `AICR_INFERENCE_PERF_MODEL_CACHE_STORAGE_CLASS`.** The benchmark downloads
the model **once** into a PVC and serves all workers from it (on by default;
avoids per-IP Hugging Face throttling). The cache PVC needs a StorageClass: it
uses the cluster's **default** StorageClass unless you set
`AICR_INFERENCE_PERF_MODEL_CACHE_STORAGE_CLASS`. On a cluster with **no default
StorageClass** (common on EKS — e.g. only a non-default `gp2`) and no value set,
the check **fails fast** in seconds with guidance rather than hanging; set
`AICR_INFERENCE_PERF_MODEL_CACHE_STORAGE_CLASS=<name>` (e.g. `gp2`/`gp3` on EKS,
`standard-rwo` on GKE) on the `inference-perf` catalog entry's `env` (or via a
catalog overlay in the `aicr validate --data <dir>` directory), or disable the cache with
`AICR_INFERENCE_PERF_MODEL_CACHE_SIZE=off`. Like the other
`AICR_INFERENCE_PERF_*` knobs, this is a **catalog/`--data`** setting — it is
**not** read from the shell environment of the process running `aicr validate`
(only `HF_TOKEN` is). AICR-deployed EKS clusters get a default `gp3` StorageClass
from the `aws-ebs-csi-driver` component, so the cache works there with no knob.

**Debugging a failed run with `AICR_INFERENCE_PERF_NO_CLEANUP`.** By default the
validator deletes the per-run namespace (DGD, workers, frontend, AIPerf Job) on
both success and failure. To investigate a failure — e.g. a `timed out waiting
for inference endpoint to serve requests` — set `AICR_INFERENCE_PERF_NO_CLEANUP=1`
and the validator leaves everything in place so you can `kubectl logs` the
frontend/workers and curl `/v1/models` and `/v1/chat/completions` live. Unlike the
other `AICR_INFERENCE_PERF_*` knobs, this one is read from the **shell
environment** of the process running `aicr validate` (forwarded to the
inference-perf pod, like `HF_TOKEN`), not from the catalog. Debug-only: you must
delete the `aicr-inference-perf-<suffix>` namespace manually afterward, or it
keeps GPU workers running.

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
explicit hostname override to avoid saturated nodes. The `inference-throughput`
gate is a full-node baseline, so when the benchmark runs on fewer than the
node's full GPU count the gate is scaled down by the same `freeGPUs / nodeGPUs`
fraction (throughput scales ~linearly at fixed concurrency-per-GPU) — a healthy
per-GPU result on a partially occupied node is not failed against a full-node
number. TTFT p99 is a per-request latency and is not scaled. Concurrent
`aicr validate` invocations are isolated from each other by the run-specific
suffix on both the namespace and the inner AIPerf Job name.

A passing CTRF entry (measured on EKS H100, 8 × H100 GPUs, Qwen/Qwen3-8B at 256 concurrency/GPU):

```json
{
  "name": "inference-perf",
  "status": "passed",
  "suite": ["performance"],
  "stdout": [
    "RESULT: Inference throughput: 108789.87 tokens/sec",
    "RESULT: Inference TTFT p99: 687.50 ms",
    "Throughput constraint: >= 50000 → PASS",
    "TTFT p99 constraint: <= 2000 → PASS"
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
# equivalent to: --phase deployment --phase conformance --phase performance
```

Phases run sequentially. By default all phases run and produce results
regardless of earlier failures. Pass `--fail-fast` to stop after the first
phase that fails (e.g., to skip a 65-minute inference-perf run when deployment
already failed).

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
| `inference-gateway` | Gateway API + Inference Extension installation; also records the gateway `LoadBalancer` network exposure (open `0.0.0.0/0` vs scoped source ranges). Fails on an open gateway only when `AICR_REQUIRE_SCOPED_INFERENCE_GATEWAY=true`. |
| `robust-operator` | Operator readiness and leader-election posture |
| `pod-autoscaling` | HPA / custom-metrics-driven pod autoscaling |
| `cluster-autoscaling` | Karpenter (preferred) or EKS managed node-group autoscaling fallback |

## Emitting recipe evidence for a PR

When a recipe PR targets hardware AICR maintainers cannot independently
re-run, the contributor needs to attach a signed **evidence bundle** so a
maintainer can verify the recipe offline. `aicr validate` produces the
bundle as a side effect when `--emit-attestation` is set; adding `--push`
signs it (cosign keyless via Sigstore) and uploads it to an OCI registry.
This is a different artifact from the CNCF-submission evidence above —
the two flag families produce independent outputs and may run from a
single `aicr validate` invocation.

```bash
aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --emit-attestation ./out \
  --push ghcr.io/<owner>/aicr-evidence
```

The `--push` tag is just a human-readable label — the `sha256:` digest is
what pins the bundle, so tag choice never affects verification (the verifier
pulls by digest). Omit the tag, as above, and aicr derives a unique
per-recipe one, `<recipe-slug>-<short-fingerprint>` (e.g.
`ghcr.io/<owner>/aicr-evidence:h100-eks-ubuntu-training-3f9a1c2b4d5e`), so
distinct attestations never collide on a shared tag. Pass an explicit tag to
override.

After the command finishes:

```text
./out
├── pointer.yaml                  # locator; copy into recipes/evidence/
└── summary-bundle/
    ├── recipe.yaml               # canonical post-resolution recipe
    ├── snapshot.yaml             # snapshot at validate-time
    ├── bom.cdx.json              # CycloneDX BOM (auto-generated from
    │                             #   recipe + validator catalog when
    │                             #   --bom is omitted)
    ├── ctrf/                     # per-phase test results
    ├── manifest.json             # per-file sha256 inventory
    ├── statement.intoto.json     # unsigned in-toto Statement
    └── attestation.intoto.jsonl  # signed (when --push is set)
```

Commit `pointer.yaml` to `recipes/evidence/<recipe>.yaml`; the bundle
itself lives in OCI. Then self-verify before opening the PR — the same
verifier runs against the committed pointer in the CI gate, so exit 0
locally means the gate will pass:

```bash
aicr evidence verify recipes/evidence/<recipe>.yaml
```

**Flag reference:**

| Flag | What it does |
|------|--------------|
| `--emit-attestation <dir>` | Write the bundle to `<dir>`. Required to produce evidence. |
| `--push <oci-ref>` | Sign via cosign keyless OIDC and push to the registry. The digest pins the bundle, so the tag is just a label; omit it and aicr derives a unique per-recipe tag (`<recipe-slug>-<short-fingerprint>`). Pass an explicit tag to override. Without `--push`, the bundle is unsigned (development/self-debug only). |
| `--bom <path>` | Embed an existing CycloneDX BOM instead of the auto-generated one. Pass `make bom` output for an exhaustive BOM that includes chart-default sub-images. |
| `--identity-token <token>` | Pre-fetched OIDC identity token, skipping the browser flow. Reads `COSIGN_IDENTITY_TOKEN`. |
| `--oidc-device-flow` | Use OAuth device-code flow instead of opening a browser. Reads `AICR_OIDC_DEVICE_FLOW`. |
| `--plain-http` | HTTP instead of HTTPS (local-registry tests only). |
| `--insecure-tls` | Skip TLS verification (self-signed registries). |

**Registry requirements:** the registry must support the OCI 1.1
Referrers API (or its tag-schema fallback) so the Sigstore Bundle can
be attached to the artifact. Known-good registries: GHCR, GitLab
Container Registry, Harbor (≥ 2.8), AWS ECR, Google Artifact Registry,
Azure Container Registry, JFrog Artifactory. Without referrer support
the bundle pushes but the signature is not discoverable, and the
verifier records signature-verify as "skipped (unsigned)" even on a
signed bundle.

**OIDC token resolution.** `--push` resolves an identity token through
this precedence chain: `--identity-token` (or `COSIGN_IDENTITY_TOKEN`)
→ ambient GitHub Actions OIDC (`ACTIONS_ID_TOKEN_REQUEST_URL`
present) → `--oidc-device-flow` (or `AICR_OIDC_DEVICE_FLOW=true`) →
interactive browser. CI pipelines typically rely on the ambient
GitHub Actions path; local workstations get the browser flow.

**Local-only mode (no registry access).** Omitting `--push` still
produces a complete bundle on disk — the verifier records the
signature step as "skipped (unsigned)" and the manifest-hash chain
becomes self-consistency only. Useful for catching accidental
corruption during development, but unsuitable for the CI gate, which
requires a signed bundle bound to a pointer.

For the full producer-and-consumer walkthrough — including OCI-only
verification, the tamper demo, and JSON output for CI gates — see
[Recipe Evidence Demo](https://github.com/NVIDIA/aicr/blob/main/demos/evidence.md).
For the bundle format and verifier semantics, see
[ADR-007](https://github.com/NVIDIA/aicr/blob/main/docs/design/007-recipe-evidence.md).
For the maintainer-side review checklist, see
[Maintaining Recipe Contributions](../contributor/maintaining.md).
For the per-flag reference on `aicr evidence verify`, see
[CLI reference](cli-reference.md#aicr-evidence-verify).

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
- **Phase-level skips** (the CheckFunc never ran — e.g., with `--fail-fast`, a prior phase failed so subsequent phases synthesize skip entries; also `--no-cluster` for checks that the runner marks skipped before dispatch): reason appears in `message`, not `stdout`.

Common reasons and their cause:

| Reason (excerpt) | Where it appears | Meaning | Fix |
|------------------|------------------|---------|-----|
| `no inference-throughput or inference-ttft-p99 constraint in recipe` | `stdout` | Check was invoked but recipe is missing the matching constraints | Re-generate the recipe or add the constraints |
| `dynamo-platform not in recipe components` | `stdout` | Inference check selected but `dynamo-platform` absent from `componentRefs` | Use `--platform dynamo` when generating the recipe |
| `DynamoGraphDeployment CRD not installed` | `stdout` | Recipe declares `dynamo-platform` but the operator is not deployed | Run `aicr bundle` + `./deploy.sh` first, or wait for bootstrap to complete |
| `skipped - no-cluster mode` | `message` | `--no-cluster` was passed — the runner short-circuits every phase before dispatching any Job | Remove the flag to run behavioral checks |
| `skipped due to previous phase failure` | `message` | `--fail-fast` was set and an earlier phase failed, so subsequent phases were skipped | Fix the earlier phase first, or drop `--fail-fast` to run all phases regardless |

### `ai-service-metrics` fails with "Prometheus unreachable"

On EKS clusters that split worker and system pods across separate security
groups (e.g. DGXC EKS with distinct customer/system ENI subnets), the
conformance check `ai-service-metrics` can fail non-deterministically with:

```text
[SERVICE_UNAVAILABLE] Prometheus unreachable at http://kube-prometheus-prometheus.monitoring.svc:9090 — verify network connectivity
```

The validator orchestrator Job tolerates every taint and has no node-affinity
toward Prometheus, so the kube-scheduler may place it on any worker node —
including one whose ENI is in a security group whose ingress to the
Prometheus-hosting SG is missing or asymmetric. The outcome is **not stable
across re-runs**: image-locality scoring tends to keep the pod on whatever
node won the first scheduling decision, so a passing run on a fresh cluster
does not prove the SG topology is correct.

This is a cluster-side prerequisite, not an AICR bug per se — see
[EKS Dynamo Networking Prerequisites](../integrator/eks-dynamo-networking.md#required-security-group-rules)
for the SG ingress rules required for Prometheus (`tcp/9090`). The underlying
issue is tracked at [#933](https://github.com/NVIDIA/aicr/issues/933).

Workaround when SG changes are not available: re-run the check until the
orchestrator lands on a node whose SG can reach Prometheus, then leave the
image cached there so image-locality keeps subsequent runs on the same node.
This is unreliable and should not be used as the steady-state validation
strategy.

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
