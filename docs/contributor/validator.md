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
| [**Chainsaw health check**](#chainsaw-health-checks) | Two surfaces with distinct runtimes: `make check-health` post-deploy locally (shells out to the `chainsaw` CLI installed on the developer's machine), AND `aicr validate --phase deployment` in-cluster (executes the Test format in-process via `pkg/chainsaw/inprocess.go` — no external binary in the deployment validator image) | `recipes/checks/<name>/health-check.yaml` | Chainsaw YAML (Test format on both surfaces; raw K8s YAML asserts use the chainsaw Go library inside `assertRawResources`) |

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

**Declared checks are resolved fail-closed.** Before the cluster is
prepared or any Job runs, `Validator.preflightDeclaredChecks`
(`pkg/validator/validator.go`) rejects the run with `ErrCodeInvalidRequest`
if any declared check name does not resolve to exactly one catalog validator
in its declared phase — a name matching nothing (typo or a check missing from
the loaded, possibly `--data`, catalog), a name that exists only under a
different phase (`nccl-all-reduce-bw` declared under `deployment`), or a name
declared more than once in one phase's `checks` list. All offenders across
every requested phase are aggregated into a single error. This closes the
fail-open path where an all-unmatched phase silently filtered to zero tests →
`skipped` → nonblocking, letting `aicr validate --fail-on-error` exit `0` on a
recipe whose required gate the catalog cannot supply
([#2121](https://github.com/NVIDIA/aicr/issues/2121)). The gate runs in
`--no-cluster` mode too, so typos are caught in offline recipe validation.
There is no opt-out: a declared name that resolves nowhere is always an
authoring error. A check that is *legitimately* not applicable at runtime
reports its own `skip` sentinel from inside the container — it is still
declared and still resolves to a catalog entry.

Top-level `constraints` — and any declared under
`validation.readiness.constraints` — are evaluated as a **pre-flight
gate** before phase checks run; other phases' `constraints` are
evaluated against each container check's reported metrics. Readiness
placement matters for gates that must not participate in
generation-time overlay filtering. (No shipped overlay currently
declares one: the GKE device-plugin ownership check, issue #1755,
briefly lived here before moving into the GKE `gpuStack` profile's
per-value constraints, which are verified at snapshot-based generation
(criteria-only generation has no snapshot evaluator and defers entirely
to the pre-flight) AND re-evaluated by the same readiness pre-flight.)

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

**One name bypasses the scalar flow entirely:** the node-set form
`NodeTopology.gpu-nodes.label` ([#1755](https://github.com/NVIDIA/aicr/issues/1755))
is dispatched by exact name in `constraints.Evaluate` *before*
`ParseConstraintPath`, uses its own value grammar
(`<label-key>=<value>` / `!<label-key>`, validated with the Kubernetes
label validators), and quantifies the predicate over the GPU-node set
synthesized from `NodeTopology.label` readings instead of comparing a
single reading. See `pkg/constraints/gpu_nodes.go` for its fail-closed
rules (truncation, empty universe, malformed or ambiguous encodings).

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
every GPU on the node and tears the DynamoGraphDeployment (and, in DRA
wiring mode, its DRA ResourceClaims) down asynchronously. Running it before
conformance starved conformance's GPU-needing checks (historically
`dra-support`, whose 1-GPU test pod failed to schedule with "cannot allocate
all claims" on single-node clusters; since #1620 that behavioral subtest runs
only where full-GPU DRA is usable — the GPU allocation checks are
capability-driven via the shared `validators/internal/allocmode` probe, with
inference-perf's worker wiring mode-dispatched per chosen node — but the
saturation-ordering rationale stands for every GPU-needing check).

The GPU allocation checks follow an **Inspect / Verify / Select** separation
(#1327): `allocmode.Detect` is the INSPECT step — it probes cluster facts
(usable full-GPU DRA, usable device plugin, per-node device counts) without
policy judgment; `allocmode.Verify` is the VERIFY step — it compares the
recipe-configured `ValidationInput.GPUAllocationPolicy` (resolved from
hydrated recipe values by `pkg/validator/v1.ResolveGPUAllocationPolicy`)
against those facts and fails closed with `ErrCodeInvalidRequest` on
mismatch; SELECT — the capability-preference dispatch inside each check —
applies only when the policy is `unspecified` (standalone runs without recipe
context). A configured policy forces the mechanism at
secure-accelerator-access, dra-support's behavioral subtest, and
inference-perf's worker wiring and node discovery.

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
| **stdout** | CTRF `stdout` (human-readable evidence) — use `fmt.Printf` |
| **stderr** | Streamed live to the user — use `slog.*` |
| `/dev/termination-log` | Failure reason (≤ 4096 bytes), written on `return error` |
| **stdout sentinel lines** | Structured/side-channel data — see [Stdout sentinels](#stdout-sentinels) |

### Capability applicability contract

A capability-gated conformance/performance check probes a live prerequisite
(a Deployment, CRD, served API group, or non-empty list) before it runs, and
turns that probe outcome into a verdict via `Capability.Require`
(`validators/applicability.go`). A `Skip` (exit 2) is a claim that the
capability is genuinely **inapplicable** — not a way to paper over a missing
prerequisite or an infrastructure error. The dividing line is whether the
resolved recipe *declares* the component that supplies the capability:
`RecipeDeclares(ctx, component)` reports true only when that componentRef is
present **and** enabled in `ctx.ValidationInput`.

`Require(ctx, probeErr, present)` resolves the fate from the probe outcome.
`present` is consulted only on a clean read (`probeErr == nil`) — e.g. a `List`
that returned zero items or a discovery call that did not serve the expected
group. Probe errors are classified by `classifyCapabilityProbeError`:

| Probe outcome | recipe DECLARES | recipe does NOT declare |
|---------------|-----------------|-------------------------|
| clean read, present | `nil` (proceed) | `nil` (proceed) |
| clean read, absent / empty | **FAIL** (`ErrCodeNotFound`) | **Skip** |
| probe err: `NotFound` (incl. group-not-served) | **FAIL** (`ErrCodeNotFound`) | **Skip** |
| probe err: `Forbidden` / `Unauthorized` (401/403) | **FAIL** (`ErrCodeUnauthorized`) — always | **FAIL** (`ErrCodeUnauthorized`) — always |
| probe err: timeout / deadline | **FAIL** (`ErrCodeTimeout`) — always | **FAIL** (`ErrCodeTimeout`) — always |
| probe err: transport (503 / conn reset / refused / HTTP2 lost) | **FAIL** (`ErrCodeUnavailable`) — always | **FAIL** (`ErrCodeUnavailable`) — always |
| probe err: other / aggregated API discovery | **FAIL** (`ErrCodeInternal`) — always | **FAIL** (`ErrCodeInternal`) — always |

**Infrastructure errors never Skip**, even when the recipe does not declare the
component: a missing RBAC grant, an apiserver timeout, a dropped connection, or
an aggregated discovery failure is not evidence that the capability is
inapplicable — it is evidence the validator *could not tell*, which must block
the gate. Only a clean `NotFound` / empty result on a **non-declared**
capability is Skip-eligible. (A `Forbidden` on a declared dependency maps to
`ErrCodeUnauthorized` rather than `ErrCodeInternal` precisely so operators read
"the validator cannot see it — grant it RBAC," not a blanket internal error.)

**Collection/list probes use `RequireList`.** When the probe is a `List` whose
*empty result* (not an error) is what signals inapplicability — e.g.
`detectPlatform` reading `Nodes().List` to classify the cloud — the caller passes
the List error through `Capability.RequireList(err)` instead of `Require`. A List
*error* is never Skip-eligible: even the rare `NotFound` shape on a collection
endpoint is an apiserver/aggregation-layer anomaly, not the clean absence of a
single object, so every List error blocks with a classified code and never a
Skip. The empty-result inapplicability case is handled by the caller
(`len(items) == 0`), keeping `Require`'s Skip path off the error branch entirely.

**#1327 standalone boundary.** A standalone validator run carries no recipe
context: `pkg/validator/v1.ToValidationInput` leaves `ComponentRefs` empty, so
`RecipeDeclares` returns false (it is nil-safe on a nil `Context` or nil
`ValidationInput`). On that path a clean-absent probe still `Skip`s, which
preserves the capability-driven automatic selection that #1327 introduced.
Fail-closed fires **only once the recipe actually declares** the dependency —
declaration is what converts "inapplicable, so Skip" into "promised but missing,
so fail."

This closes the #2122 false-PASS: before this contract, a recipe that declared a
capability whose prerequisite was absent — or whose probe hit an auth/timeout/
transport/discovery error — reported `passed` (via a Skip that read as
non-blocking), letting a broken or unauthorized cluster clear conformance.

### Stdout sentinels

A check runs inside a pod; the only channels that reach the orchestrator are the
exit code, `/dev/termination-log`, and the captured stdout (pod logs). To move
*more* than a pass/fail verdict across that boundary, checks emit **sentinel
lines** — stdout lines with a reserved prefix that a specific parser recognizes
and pulls out. Plain (non-prefixed) stdout is unaffected and flows into the CTRF
`stdout` array as before.

Two sentinels exist today, with deliberately different lifecycles:

| Prefix | Emitted by | Parsed by | Payload | Kept in CTRF `stdout`? | Survives minimal redaction? |
|--------|-----------|-----------|---------|------------------------|-----------------------------|
| `RESULT:` + one space | any check (`fmt.Printf`) | `extractResultSummaries` (`pkg/validator/validator.go`) | free-form human text (throughput, bandwidth, TTFT…) | **yes** — line stays; trailing text is *also* echoed to the live CLI at INFO | **no** — dies with `stdout` under the default policy |
| `##AICR-EXTRA##` + one space | `validators.EmitExtra` | `parseExtraSentinels` (`pkg/validator/job/result.go`) | one JSON object → `TestResult.Extra` (counts / enum codes) | **no** — stripped as transport, not evidence | **yes** — allowlisted keys are published (see below) |

Both are parsed with `strings.CutPrefix` and both parsers are pure, unit-tested
functions. They are separate channels on purpose: `RESULT:` surfaces live
metrics to a human watching a run, so it carries unbounded free-form text and is
correctly redacted with the rest of `stdout`; `##AICR-EXTRA##` carries structured
data that must *outlive* redaction, so it is low-cardinality, allowlisted, and
stripped from the human evidence. Do not route structured outcome data through
`RESULT:` (it would not survive publication) or human prose through
`##AICR-EXTRA##` (it would be dropped by the allowlist or leak identifiers).

### Structured evidence that survives redaction

`stdout` and `message` are free-form log text: they can leak node names, IPs,
DNS names, and secret/cert names, so the default **minimal** redaction policy
(`pkg/evidence/redact`) strips them from a published, signed evidence bundle —
only `--full` keeps them. A check that reports on `stdout` alone therefore shows
as `passed` with no detail in the artifact a downstream consumer verifies by
default: a 1-of-2-node pass is indistinguishable from 2-of-2, and a skip loses
its reason.

To carry outcome data that *does* survive minimal redaction, emit it into the
CTRF `extra` map (`ctrf.TestResult.Extra`, mirroring the CTRF spec's `extra`
object) via `validators.EmitExtra`:

```go
// Emit failures are non-fatal — warn, but never flip the check's verdict on a
// failed stdout write (checkNvidiaSMI wraps this as the emitExtraOrWarn helper):
emit := func(extra map[string]string) {
    if err := validators.EmitExtra(extra); err != nil {
        slog.Warn("failed to emit validator extra", "error", err)
    }
}
// Success path — coverage disclosure (counts only, never node names/IPs):
emit(map[string]string{"nodesValidated": "1", "nodesTotal": "2"})
// Skip path — reason enum code:
emit(map[string]string{"skipReason": "no-gpu-nodes"})
```

**Transport.** `EmitExtra` marshals the map to one JSON line prefixed with
`ctrf.ExtraLinePrefix` (`##AICR-EXTRA##` followed by one space) on stdout — the
only channel that crosses the pod boundary besides the exit code and termination
log. The orchestrator (`pkg/validator/job.ExtractResult`) parses each sentinel
line, keeps the **last valid non-empty** payload as `TestResult.Extra`, and
strips every sentinel line from the stored `stdout` (transport, not human
evidence). A malformed line is non-fatal: it is logged and skipped without
discarding an earlier valid payload — a garbled line never flips a pass to an
error, nor clears a coverage line that preceded it. Keep the human `fmt.Printf`
lines too; they still feed `--full` and live `aicr validate` output.

**Low-cardinality + allowlist contract.** `extra` values MUST be counts or enum
codes only (`"2"`, `"no-schedulable-gpu-nodes"`) — never node names, IPs, or hostnames.
`pkg/evidence/redact`'s `ctrfExtraAllowlist` enforces this at the **publication
boundary** (not just at emission, which raw prefixed stdout could bypass) with a
fail-closed **key _and_ value** check: only the listed keys (`nodesValidated`,
`nodesTotal`, `skipReason`) survive, and each surviving value must pass its key's
validator — a non-negative decimal count for the `nodes*` keys, and for
`skipReason` a **closed set** of known codes (`ctrfSkipReasons`, currently
`no-gpu-nodes`, `no-schedulable-gpu-nodes`, `nodes-busy`). A closed set rather
than a shape regex is deliberate: a kebab-case regex would still pass an
arbitrary low-cardinality identifier like `customer-prod-cluster`. A value that
is ill-shaped or unlisted (an IP under `nodesTotal`, a hostname or unminted code
under `skipReason`) is dropped even though its key is allowed. Every other key is
dropped too, and if nothing survives the map ships as absent (no empty
`extra: {}`). Adding a new key (with its value validator) or a new `skipReason`
code means adding it to that allowlist (and bumping `redact.PolicyVersion`) in
the same change; there is no CTRF schema in `api/`, so the `pkg/validator/ctrf`
godoc and this page are the contract.

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

**`:latest` is the last _stable_ release, never `main`.** The on-tag workflow
first builds every validator architecture under one run-unique candidate tag,
resolves one authoritative digest map, and scans both platforms and attests
those exact digests. Only then may version aliases move; a stable release
verifies all seven version aliases before it starts updating any `:latest`
alias. A pre-release promotes its version alias but never `:latest`, so a
validator change merged to `main` after the last stable release is absent from
`:latest` until the next one. Running
`AICR_VALIDATOR_IMAGE_TAG=latest` against a `main`-tracking recipe can
therefore silently run *older* validator behavior — e.g. a
`performance.constraints` pin such as `inference-model` /
`inference-concurrency-per-gpu` / `nccl-benchmark-profile` is only honored
by a validator new enough to read it; an older `:latest` validator ignores
the pin and runs its compiled default, which can surface as a misleading
result (for `nccl-benchmark-profile`, a silent skip) rather than a clear
version error.

The seven GHCR alias updates cannot be atomic across repositories. Promotion
performs an all-images, read-only preflight before its first write and accepts
each `:latest` only at that image's immediate-prior or current-candidate
digest. It also rejects an out-of-order stable release when the same or a newer
stable version is already public. If a later registry write fails, re-running
the failed workflow jobs with the same candidate converges the remaining
aliases without overwriting a conflicting version. Candidate tags are retained
for that recovery and for audit; cleanup is intentionally deferred to a
separate retention design.

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

`pkg/validator/validator.go::checkReadiness` evaluates the recipe's
top-level `constraints` plus any `validation.readiness.constraints`
*before* any phase runs. A parse error or a failing constraint returns
`ErrCodeInvalidRequest` and aborts the entire run. **Do not** `slog.Warn; continue` on an evaluator
error — that masquerades a broken validation YAML as a passing
constraint, which is an explicit anti-pattern in CLAUDE.md.

The `dependencyAffinity` pre-flight (validator catalog entries
declaring a required dependency) follows the same rule.

### Node-scoped checks: cordoned GPU nodes

A cordon (`Node.Spec.Unschedulable`) means "do not schedule new pods,"
not "exclude this node from GPU service" — it is commonly transient
maintenance, not deliberate exclusion. A node-scoped check that
enumerates only the schedulable cohort (e.g. via
`helper.FindSchedulableGpuNodes`) can pass while silently validating
fewer nodes than the cluster has, with nothing in the evidence
indicating reduced coverage. That is the spuriously-passing-check
failure mode CLAUDE.md's anti-pattern table calls out, applied to node
enumeration instead of a single boolean.

Checks that deploy a per-node test pod (`check-nvidia-smi`) must
instead call `helper.FindGpuNodes`, which returns every GPU node
tagged with its cordon state, and:

1. Validate only the schedulable subset. Note this is a **deliberate
   policy choice, not a technical necessity**: `check-nvidia-smi`'s
   probe pod is pinned via `nodeName` and tolerates all taints
   (`testdata/nvidia-smi-verify-pod.yaml`), so it bypasses the
   scheduler and *could* run on a cordoned node — `Unschedulable` is a
   scheduler-side predicate that kubelet admission of a directly-bound
   pod never consults. The check still excludes cordoned nodes because
   a cordon signals operator intent to keep workloads off the node
   during maintenance, even when a specific pod could technically slip
   past it. A future check with a different tradeoff should say so
   explicitly rather than assume cordons are unconditionally
   unschedulable.
2. Report cordoned nodes explicitly in the check's stdout evidence
   (`<node>: skipped (cordoned)`), never omit them from the node count.
3. Print a coverage line (`RESULT: nodesValidated: <validated>/<total>`)
   on every exit path (skip, failure, and success), not only the
   success path, so a pass *or* a failure on reduced scope is visible.
   `<validated>` is the count actually confirmed working, not the
   schedulable count: it is `0` on the all-cordoned and busy-skip
   paths (nothing was attempted yet) and the successful-node count on
   the failure path (a partial pass is not conflated with a full one).
   Exception: when the recipe declares `Deployment.gpu-driver.version`,
   those same all-cordoned and busy paths (and the no-GPU-nodes path)
   fail closed instead of Skip — a declared host-driver floor that
   cannot be measured must not PASS (#1995). Skip remains only when
   the floor constraint is absent.
   The `RESULT:` prefix is `pkg/validator/validator.go`'s
   `resultSummaryPrefix` convention: the validator runtime echoes the
   trailing text of any such stdout line into live CLI output via
   `slog.Info`, unconditionally — without it, the line is only visible
   after the run, inside the (possibly redacted) report.
   Caveat: the line still also lands in `TestResult.Stdout`/`.Message`
   in the CTRF report, which the default ("minimal") redaction policy
   (`pkg/evidence/redact`) strips from a signed evidence bundle. The
   `RESULT:` prefix makes the coverage figure visible during a live
   `aicr validate` run regardless of redaction, but it is not
   guaranteed to survive into the artifact a downstream consumer
   verifies by default. That is why the same counts are ALSO emitted
   through `validators.EmitExtra` (#1951) — see **Structured coverage
   survives redaction** below.

`expected-resources`' `rdmaFabricProbeCoverage` is itself node-scoped and now
follows this pattern too (#1952). It enumerates every GPU node via
`helper.FindGpuNodes`, validates only the schedulable Mellanox
RDMA-capable cohort for uniform allocatable fabric, but discloses each
cordoned RDMA-capable node explicitly (`<node>: skipped (cordoned)`),
counts it in `nodesTotal`, and never narrows the printed total. Because
the probe is re-run on every poll iteration
(`verifyRDMAFabricReady`/`pollUntilStable`), the stdout
enumeration/`RESULT:` line is printed **exactly once at the settled
terminal outcome** (ready or fail-closed), never per tick. The structured
Extra is emitted twice: an **eager floor** on the first observation that
enumerates any RDMA-candidate node (with `nodesValidated=0` — nothing is
certified mid-poll) and again at the terminal outcome.
`parseExtraSentinels` keeps the last valid sentinel, so the terminal emit
wins on a clean exit; the floor exists only so a cordoned-node narrowing
still reaches the signed bundle if the Job's `activeDeadlineSeconds`
SIGKILLs the process at the no-margin poll budget before the terminal emit
runs (#1952). The gate stays fail-closed: "could not observe the fabric"
reports `0` validated and never reads as ready.

Unlike `check-nvidia-smi`, the RDMA gate never *skips* — it either
certifies the cohort or fails closed — so it mints no `skipReason`
enum. Its coverage rides the existing `nodesValidated`/`nodesTotal`
allowlist keys unchanged (see below), so the redaction
`PolicyVersion` stays `v2`.

Cluster-aggregate checks that assert on an operator's aggregate status
(`gpu-operator-health`) remain unaffected — DaemonSet operands ignore
cordons.

**Structured coverage survives redaction.** The `RESULT:` stdout line
is echoed to the live CLI but is stripped from a signed bundle by the
default (`minimal`) redaction policy (`pkg/evidence/redact`), so both
`check-nvidia-smi` and the RDMA gate ALSO emit the coverage through
`validators.EmitExtra` as low-cardinality counts
(`nodesValidated`/`nodesTotal`) or a closed-set `skipReason` code. Those
keys are the only ones that clear the fail-closed `ctrfExtraAllowlist`
(a value that structurally looks like a node name or IP is dropped even
under an allowed key), so a signed bundle records reduced coverage —
e.g. a cordoned RDMA node narrowing the fabric cohort — without shipping
any operator-identifying text. Node names appear only in the redacted
stdout enumeration, never in the Extra channel. See #1951/#1952.

For *deliberate*, durable exclusion of a node from GPU service (as
opposed to transient cordon-for-maintenance), use the GPU Operator's
`nvidia.com/gpu.deploy.operands=false` node label instead of a cordon.
It removes the node from allocatable `nvidia.com/gpu` entirely, so the
node drops out of both node-scoped checks and ClusterPolicy
aggregation consistently — a cordon does neither reliably.

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

#### NCCL: recipe-driven applicability (`nccl-benchmark-profile`)

NCCL applicability defaults to the compiled `supportedNCCLCombinations`
matrix (variant → service → accelerators), keyed to the recipe's
`criteria`. The optional `nccl-benchmark-profile` performance constraint
(bare `{accelerator}/{service}` value, e.g. `gb200/eks`) overrides that
default so external `--data` recipes — new services, or embedded services
extended to new accelerators — can run the embedded benchmarks
([#1703](https://github.com/NVIDIA/aicr/issues/1703)). Resolution is
recipe-only (no env tier), and parsing fails closed: a malformed or
unknown profile aborts the check with `ErrCodeInvalidRequest` instead of
skipping. The profile keys template selection, service-specific fabric
plumbing (EFA / GKE NIC discovery, worker scheduling defaults), and
preflights; node identification (the GFD `gpu.product` filter) stays on
the recipe's own `criteria.accelerator`. Profiles are restricted to pairs
already present in the compiled matrix, so the
`TestSupportedNCCLCombinationsHaveRuntimeTemplates` wiring guard
(advertised tuple ⇒ parseable template) covers every profile a recipe can
name. See `validators/performance/nccl_benchmark_profile.go`.

#### NCCL: recipe-supplied runtime (`nccl-benchmark-runtime-ref`)

A profile still requires an embedded template to borrow, so it cannot cover a
private service+accelerator whose fabric matches none of the shipped templates
([#1792](https://github.com/NVIDIA/aicr/issues/1792)). The optional
`nccl-benchmark-runtime-ref` performance constraint closes that gap: its value is
a bare `{accelerator}/{service}` naming a Kubeflow `TrainingRuntime` the recipe
ships in its `--data` tree at
`validators/performance/testdata/{accelerator}/{service}/runtime.yaml` — the same
layout the embedded templates use, so an external runtime is a drop-in for
upstreaming.

Resolution is split across the two-stage design:

- **Orchestrator** (`pkg/validator/benchmark_runtime_ref.go`,
  `resolveBenchmarkRuntimeRef`, called at the top of `ValidatePhases` /
  `ValidatePhase`): reads the referenced file through the recipe `DataProvider`
  and lowers its content into the `nccl-benchmark-runtime` **carrier** constraint
  before the `/data/validation` ConfigMap is written. Fails closed
  (`ErrCodeInvalidRequest`) on a malformed/traversal ref, a missing/empty file,
  an absent `DataProvider`, or a ref set alongside an inline carrier. The two
  constraint names are defined once in `pkg/validator/v1` (`PerfConstraintNCCLBenchmarkRuntime*`)
  so the write side and the pod read side cannot drift.
- **Pod** (`validators/performance/nccl_benchmark_runtime.go`): reads the
  `nccl-benchmark-runtime` carrier and renders it — via the same `${VAR}`
  substitution as the baked-in templates, through the shared `renderYAMLTemplate`
  core — in place of `testdata/{accelerator}/{service}`.
  `validateNcclAllReduceBw` bypasses the `ncclCombinationSupported` applicability
  gate entirely (applicability is granted by the recipe's explicit opt-in, keyed
  on its own criteria) and `applyNCCLResources` skips every service-specific step
  — EFA/TCPXO/RDMA discovery, the GB200-NVreg / GKE-TCPXO preflights, RoCE claim
  application, and NVLS/IMEX provisioning — because the supplied runtime owns its
  fabric wiring. Transport verification still runs (the `-net` / `-nvls` markers
  are transport-internal and fabric-agnostic), so a runtime paired with a named
  variant must still prove its transport rather than pass on bandwidth alone; the
  worker cohort is also sized against the runtime's own `nodeSelector` so
  `WorkerCount` matches placement. It fails
  closed on a carrier that is not a `trainer.kubeflow.org/v1alpha1`
  `TrainingRuntime` with a `node` replicatedJob. The runtime is applied only
  through `trainingRuntimeGVR` with a force-set name/namespace, so a recipe can
  supply a `TrainingRuntime` and nothing else — not an arbitrary resource kind.
  It is mutually exclusive with `nccl-benchmark-profile`.

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
| `AICR_INFERENCE_PERF_ROUTER_MODE` | `least-loaded` | Dynamo frontend routing strategy (`DYN_ROUTER_MODE`): one of `kv`, `round-robin`, `random`, `power-of-two`, `least-loaded`, `device-aware-weighted` (upstream's `direct` is excluded — it needs per-request worker IDs the benchmark never sends). Lets a characterization run A/B strategies without rebuilding the image. Consumed only when `inference-routing-mode` is `dynamo-router`; in `gateway-epp` mode the EPP selects endpoints and the value is unused. A **set** value is validated in either mode — an unknown value fails closed with `ErrCodeInvalidRequest` rather than being silently ignored (the routing mode is a recipe decision, so a typo must not pass under one recipe and abort under another). |
| `AICR_INFERENCE_PERF_WORKLOAD_READY_TIMEOUT` | `10m` | Wait for the `DynamoGraphDeployment` to become ready (image pull + model load + worker health). Large models load slower — raise this **and** the catalog entry's `timeout` in tandem, or the parent deadline caps it. |
| `AICR_INFERENCE_PERF_HEALTH_TIMEOUT` | `5m` | Wait for the endpoint to serve a real chat-completion *after* the workload reports Ready. Concurrent first-load from one RWO cache PVC can push first-serve past 5m; raise it (bounded by the catalog `timeout`). |
| `AICR_INFERENCE_PERF_MODEL_CACHE_SIZE` | `100Gi` (on) | The PVC-backed model-weights cache is **on by default**. Set a different K8s quantity to resize, or a disable sentinel (`off`/`0`/`none`/`disabled`) to turn it off and download from HF directly. |
| `AICR_INFERENCE_PERF_MODEL_CACHE_STORAGE_CLASS` | cluster default | StorageClass for the cache PVC. On a cluster with **no default SC and no value here**, the check **fails fast** with guidance rather than leaving the PVC `Pending` until timeout. AICR-deployed EKS gets a default `gp3` SC from `aws-ebs-csi-driver`; GKE has `standard-rwo`. |
| `AICR_INFERENCE_PERF_MODEL_CACHE_POPULATE_TIMEOUT` | `13m` | Wait for the one-time model-cache populate Job (cold image pull + first-ever Hugging Face download into the PVC). Separate from — and larger than — `AICR_INFERENCE_PERF_WORKLOAD_READY_TIMEOUT` because the populate Job pays a cold pull *and* a multi-GB download; provide the optional HF-token secret to remove anonymous-download throttling. Raise it (and the catalog `timeout`) for very large models. **Migration:** the cache-populate wait no longer honors `AICR_INFERENCE_PERF_WORKLOAD_READY_TIMEOUT` (which now bounds only the DynamoGraphDeployment readiness wait) — set this knob instead to widen the populate budget. |

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
- **Routing matters.** The inference-perf workload defaults to Dynamo's
  load-aware least-loaded router (`DYN_ROUTER_MODE=least-loaded`; override via
  the `AICR_INFERENCE_PERF_ROUTER_MODE` knob above), which balances by each
  worker's active in-flight load so a transiently-slow worker stops receiving
  its full share — mitigating the stochastic EKS H100 worker-stall / throughput
  degradation at the saturation knee (issue #1197). Frontend-to-worker
  requests use Dynamo's request plane (Dynamo 1.4+ defaults to TCP; AICR does
  not set `DYN_REQUEST_PLANE=nats`). Workers publish KV-cache events directly
  over ZMQ; the KV router consumes them end-to-end with no NATS relay.
  Least-loaded routing does not consume those events.
  The `inference-routing-mode` recipe input defaults to `dynamo-router`; set
  `gateway-epp` to validate the GAIE/EPP path through agentgateway with worker
  frontend sidecars in direct mode. The direct-mode sidecars honor EPP routing
  headers; they do not relay KV events.
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
| `CheckWildcardAcceleratedToleration` | Accelerated-node tolerations carry no wildcard (keyless) entry — on AKS a wildcard deadlocks nodewright interrupt packages ([nodewright#296](https://github.com/NVIDIA/nodewright/issues/296)); wired at `severity: error`, skipped when the component is disabled via `--set` |
| `CheckDriverOwnershipCoherence` | GPU driver-ownership coherence on the final effective values (recipe merge + `--set`/`--set-json`/`--set-file` under canonical names and registry aliases): a recipe whose snapshot observed no NVIDIA driver (`metadata.gpuDriverState: absent`) must not bundle with the preinstalled-driver assumption. When GPU Operator manages the driver, `nvidia-dra-driver-gpu.nvidiaDriverRoot` must equal `gpu-operator hostPaths.driverInstallDir`; with a preinstalled driver, the DRA root must avoid the unpopulated operator container root and may intentionally differ from `hostPaths.driverInstallDir` ([#1087](https://github.com/NVIDIA/aicr/issues/1087), [#1757](https://github.com/NVIDIA/aicr/issues/1757)). Wired at `severity: error`. |
| `CheckMariaDBOperatorOwnershipCoherence` | MariaDB Operator installation safety for AICR-provided Slurm accounting: `metadata.mariaDBOperatorState` values `crs-detected` and `unknown` block bundling, `api-detected` or omitted evidence warns, and `absent` proceeds silently. Wired at `severity: warning` so warning results remain non-blocking while returned errors still fail the bundle. |

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
   `pkg/chainsaw/inprocess.go` unmarshals the
   `chainsaw.kyverno.io/v1alpha1` Test, walks `spec.steps[].try[]`, and
   dispatches `assert` / `error` to kyverno-json's `checks.Check` engine
   against live cluster state. No external binary is shipped in the
   deployment validator image. CLI output is source-tagged `[chainsaw]`
   vs `[expectedResources]` so operators can disambiguate when both
   paths report on the same component.

Only surface 1 still needs the `chainsaw` binary, and only on the
developer's machine — that is why `.settings.yaml` keeps the
`testing_tools.chainsaw` pin and `tools/update-chainsaw-checksums`.

The **readiness gate** (`cmd/gate`, image `ghcr.io/nvidia/aicr-gate`,
emitted by `aicr bundle --readiness-hooks`) is a third consumer of the
same executor. It shelled out to an embedded `chainsaw` binary until
PR #2038 removed it; that binary was the gate image's only source of
HIGH CVEs and upstream ships no release that fixes them. It calls `chainsaw.Run`
against a dynamic client built from the Job's read-only ServiceAccount,
so `pkg/chainsaw` is the single assertion engine for validator and gate
alike. The gate's `--namespace` flag survives as the default namespace
for resource blocks that omit one (`defaultNamespaceFetcher` in
`pkg/chainsawgate/runner`), preserving what `chainsaw --namespace` did.

A Test that declares no `assert`/`error` operation is rejected rather
than passing vacuously (#2040); a check that is intentionally a no-op —
today the three `*-ocp-olm` components, whose readiness is enforced by
the bundler's `--readiness-hooks` gate instead — must say so with the
`aicr/no-op-check: "true"` annotation on the Test.

Three sibling shapes are rejected for the same reason — each one would
otherwise report a component healthy without having evaluated what it
claims to (#2051):

- **Empty content.** A blank, whitespace-only, or comments-and-`---`-only
  entry has no Test to carry the no-op annotation, and used to reach
  `assertRawResources` and pass on zero documents.
- **A mixed stream.** Once any document is a chainsaw Test the whole
  stream routes to the in-process executor, and nothing else reads it, so
  a non-Test document alongside it was evaluated by no path at all. Only
  Test documents may share a stream; a trailing `---`, a comment-only
  document, and an explicit `null` are punctuation, not content.
- **Both `assert` and `error` on one operation.** Chainsaw evaluates one
  action per operation and the executor reaches `assert` first, so the
  negative check silently never ran. Split them into separate `try`
  entries.

**Indeterminate is not a verdict.** The gate reports `Unknown`, not
`Fail`, when a component's state was never established — a discovery
outage, an apiserver 5xx, a forbidden read. Those surface as
`ErrCodeUnavailable`, which `pkg/chainsaw` never treats as terminal, so
it retries for the whole budget and can only surface it once the budget
runs out. Exit codes are unchanged (`Fail` and `Unknown` gate
identically), but the distinction points the operator at a broken
cluster instead of a broken component.

A no-match from the RESTMapper is held to the same standard: it resolves
to `ErrCodeNotFound` — the immediate-pass path for a negative `error:`
assertion — only when the retry ran against discovery performed after the
lookup began *and* the kind's own API group was fully enumerated.
client-go drops `ErrGroupDiscoveryFailed` whenever partial results exist,
so a kind in an unreachable group arrives as a bare `NoKindMatchError`;
`resolveMapping` consults the same cached discovery client the mapper
resolves through to tell the two apart. The scope is deliberately one
group: real clusters routinely carry a broken aggregated APIService, and
treating any partial failure as global would strand every negative
assertion.

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
(`pkg/chainsaw/allowlist.go`). Any other operation (`script`,
`apply`, `create`, `delete`, `patch`, `update`, `wait`, `command`,
`sleep`, `podLogs`, `events`, `describe`, `get`) is rejected with
`ErrCodeInvalidRequest`, as is an operation that sets both `assert` and
`error`. PR #1223 will add the same enforcement at lint time so
violations are caught before they ever reach the validator.

**Value-gate awareness (#1844).** A registry assert file is static — it
cannot see the component's effective Helm values. That is a problem for a
manifest whose whole resource is gated behind a value: e.g.
`--set nodewrightcustomizations:tuningEnabled=false` on a single-package
tuning manifest (`tuning-gke.yaml` / `tuning-generic.yaml`) suppresses the
entire `tuning` Skyhook CR, so an unconditional
`assert status.status: complete` would fail on a deliberately-untuned
cluster. The deployment validator resolves this by rendering the
component's manifests with the effective values
(`recipe.GetComponentValues` → `manifest.Render`) and skipping the assert
when the render yields no matching CR — see `nodewrightHealthCheckSuppressed`
and `expectedNodewrightNames` in
`validators/deployment/expected_resources.go`. The skip is **fail-closed**:
whenever the values keep the CR, the render still lists it and the assert
runs, so only an intentionally-absent CR is tolerated (a CR that *should*
deploy but is missing on the cluster still fails). The same render drives
the Go readiness check `verifyNodewrightReady`, so both surfaces agree on
which CRs to expect. This skip is scoped to `nodewright-customizations`;
every other component's assert queues unconditionally.

The suppression must be expressed **in the recipe** — an overlay-declared
component `overrides:` (how `tuningEnabled: false` ships as the AKS default)
or an inline `overrides:` in the recipe file — because that is what the
render sees. Two boundaries follow from *where* the deployment validator
gets its inputs (it runs in-cluster inside a Job, reading the recipe from a
mounted ConfigMap with **no `DataProvider`** — only the embedded data baked
into the validator image):

- **A bundle-time-only `--set` is not honored.** `aicr bundle --set
  nodewrightcustomizations:tuningEnabled=false` is applied in the bundler and
  never persisted to the recipe; `aicr validate` has no `--set` flag and
  reads the recipe as-is, so it renders *with* the CR and still expects it.
  To suppress the assert on a deliberately-untuned cluster, the value must
  live in the recipe (overlay default or inline `overrides:`), not a
  transient bundle flag. (`aicr recipe` likewise has no value `--set`, so the
  overlay/inline override is the only channel.) `Overrides` resolved from a
  `--data` overlay *are* honored, because recipe resolution runs CLI-side and
  bakes them into the serialized recipe before the Job receives it.
- **`--data`-external files referenced by path are not readable in the Job.**
  A component whose `manifestFiles` or base `valuesFile` exist only in an
  external `--data` directory cannot be read by the embedded-only validator
  (a pre-existing constraint for `manifestFiles`; the render's `valuesFile`
  read shares the same embedded scope). It fails **closed** — an error, never
  a false pass. In practice `nodewright-customizations` ships no values file
  and uses inline `overrides:`, so there is no exposure here today.

**Running:**

```bash
make check-health COMPONENT=gpu-operator   # one component
make check-health-all                      # everything in recipes/checks/
make validate-local RECIPE=recipe.yaml     # full pipeline in Kind
```

### Timeout budgeting

During `aicr validate --phase deployment`, registry health checks in
`recipes/checks/<component>/health-check.yaml` run in-process inside
the `expected-resources` check (`pkg/chainsaw/inprocess.go`).

A Test's `spec.timeouts.assert` is the **whole-Test budget** — one
deadline shared across every step and retry. Slurm's
[`health-check.yaml`](https://github.com/NVIDIA/aicr/blob/main/recipes/checks/slinky-slurm/health-check.yaml)
uses `assert: 7m` so workload-readiness steps can converge before the
pod-phase guard runs.

The authored value can only **shorten** the caller's budget, never
extend it: the effective deadline is `min(spec.timeouts.assert,
caller budget)`. The caller budget is `defaults.ChainsawAssertTimeout`
for the deployment validator and the gate's `--timeout`
(`defaults.ReadinessGateExecTimeout`, 2m) for a readiness Job. This
restores what the removed chainsaw-exec path enforced by wrapping the
subprocess in an independent outer `context.WithTimeout`; running
in-process, the authored value had been substituting for the caller
budget outright, letting registry- or integrator-authored content
overrun `--timeout` by any factor it chose. No in-tree check changes
behavior — every authored value is at or below its caller's budget —
but a readiness test copied from a validator check (`assert: 5m`) is
now capped at the gate's 2m per evaluation rather than overrunning it.

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

0. **Name dispatch.** `constraints.Evaluate` first matches the
   constraint name against the node-set form
   `NodeTopology.gpu-nodes.label`
   ([#1755](https://github.com/NVIDIA/aicr/issues/1755)), which has its
   own value grammar and evaluator and never reaches the steps below.
   Every other name proceeds through the scalar flow.
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
