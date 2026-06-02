# Testing AICR

AICR's test pyramid has five layers. Unit tests are the broad base —
table-driven, hermetic, `--no-cluster`. Above them sit integration
tests against a real Kubernetes API (Kind), Chainsaw post-deploy
health checks, KWOK matrix tests that exercise scheduling shape and
deployer output without GPU hardware, and a thin top of E2E tests
against real cloud accounts.

> **What to use when.** If the code path can be exercised without
> the Kubernetes API, write a unit test. If it cannot, prefer KWOK
> or Chainsaw over Kind, and Kind over an E2E.

The pre-push gate is **`make qualify`**. It runs tests with the race
detector and coverage threshold, lints (golangci-lint + yamllint),
e2e, vulnerability scan, BOM regen check (opt-in flag elsewhere),
and license check. CI runs the equivalent — if `make qualify` passes
locally, CI will pass.

## Test Surfaces

| Surface | When to use | Lives in | Run locally | Gated by |
|---|---|---|---|---|
| **Unit tests (Go)** | Logic exercisable without K8s API | `*_test.go` next to source | `make test` | `make qualify`, push CI |
| **Integration tests (Go)** | Logic touching the K8s API | `*_test.go` with envtest / fake client | `make test` (Kind for live cases) | `make qualify`, push CI |
| **Chainsaw health checks** | Component-level post-deploy health | `recipes/checks/<name>/health-check.yaml` | `make check-health COMPONENT=<name>` | Bundle-validate workflow |
| **KWOK matrix tests** | Recipe scheduling shape + deployer output without GPUs | `kwok/scripts/*`, `recipes/overlays/*` | `make kwok-test-deployer RECIPE=… DEPLOYER=…` | `kwok-recipes.yaml` workflow |
| **E2E tests** | Full pipeline against real cloud accounts | `tools/e2e` | `unset GITLAB_TOKEN && ./tools/e2e` | `make qualify`, e2e workflow |

The rest of this page covers each surface in the order a typical
change touches them — unit, integration, chainsaw, KWOK, E2E — plus
the `make qualify` gate and common gotchas.

## Unit Tests

Unit tests are the default. Required patterns from
[CLAUDE.md](https://github.com/NVIDIA/aicr/blob/main/.claude/CLAUDE.md):
table-driven cases, race detector enabled, no live cluster access.

**Table-driven** (mandatory for multiple cases):

```go
func TestParseCriteria(t *testing.T) {
    tests := []struct {
        name    string
        input   string
        want    string
        wantErr bool
    }{
        {"valid h100", "h100", "h100", false},
        {"empty rejected", "", "", true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got, err := Parse(tt.input)
            if (err != nil) != tt.wantErr {
                t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
            }
            if got != tt.want {
                t.Errorf("got=%q want=%q", got, tt.want)
            }
        })
    }
}
```

**Race detector** is always on:

```bash
go test -race ./...                          # full module
go test -race -v ./pkg/recipe/...            # single package
go test -race -v ./pkg/recipe -run TestX     # single test
```

**CLI tests capture `cmd.Writer`**, not stdout. CLI commands write
through `cmd.Root().Writer` so tests can intercept output:

```go
buf := &bytes.Buffer{}
cmd := newRecipeCmd(client)
cmd.SetOut(buf)
cmd.SetArgs([]string{"--service", "eks", "--accelerator", "h100"})
if err := cmd.Execute(); err != nil {
    t.Fatalf("execute: %v", err)
}
```

Direct `fmt.Println` / `fmt.Printf` to stdout in `pkg/cli` breaks
this pattern and is a review-blocker.

**Coverage floor: 75%** (project-wide, from `.settings.yaml`
`quality.coverage_threshold`). `make test-coverage` enforces it.
Per-package decreases > 0.5% are flagged for justification.

## Test Isolation (`--no-cluster`)

Any test that touches the validator or a collector **must** run with
`--no-cluster` set. Two reasons:

1. **Hermeticity.** A test that happens to have a kubeconfig pointed
   at a real cluster will connect to it and create resources. CI
   runners and laptops both fail this way silently otherwise.
2. **Speed.** No RBAC creation, no Job deployment, no waiting on pods.

**How to set it:**

```go
// Go unit/integration tests
v := validator.New(
    validator.WithNoCluster(true),
    validator.WithVersion(version),
)
```

```bash
# CLI invocations in tests, scripts, and chainsaw
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml --no-cluster
```

```yaml
# Chainsaw scripts: always include --no-cluster on aicr invocations
- script:
    content: |
      ${AICR_BIN} validate -r recipe.yaml -s snapshot.yaml --no-cluster
```

**Behavior with `NoCluster=true`:** the validator skips ServiceAccount /
Role / ClusterRole creation, skips Job deployment for container-per-validator
checks, and reports each check as `"skipped — no-cluster mode (test
mode)"`. **Constraints are still evaluated inline**, because constraint
evaluation reads the snapshot rather than the API server. A test that
asserts on validator output must therefore assert on the constraint
results, not on check results.

The option lives in [`pkg/validator/options.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/validator/options.go)
(`WithNoCluster`). Adding a new validator entry point that talks to
the API must respect this flag — the anti-patterns table treats live
cluster access in tests as a review-blocker.

## Coverage Gate Workflow

Before pushing a Go change, verify the per-package coverage delta on
the narrowest directory root your change touches. `$pkg/...` includes
descendants — pick the narrowest root that covers the diff.

```bash
# 1. Profile the working tree (changes must be committed first).
GOFLAGS="-mod=vendor" go test -coverprofile=cover.out ./pkg/recipe/...

# 2. Profile origin/main from a clean worktree, outside the source tree.
git worktree add $TMPDIR/baseline origin/main \
  && (cd $TMPDIR/baseline && GOFLAGS="-mod=vendor" go test \
        -coverprofile=$TMPDIR/base.out ./pkg/recipe/...); \
  rc=$?; git worktree remove --force $TMPDIR/baseline; \
  (exit $rc)

# 3. Compare totals.
go tool cover -func=$TMPDIR/base.out | tail -1
go tool cover -func=cover.out          | tail -1
```

Writing the baseline profile to `$TMPDIR/base.out` is deliberate: a
profile inside the worktree disappears with `worktree remove --force`.

**Gates:**

- **Block** if `make test-coverage` fails (project-wide 75% floor).
- **Block** if any new exported function or method has 0% coverage
  in the diff — add tests before pushing.
- **Flag** any per-package decrease > 0.5% and explain in the PR.

Report the delta in the PR Testing section, e.g., `pkg/recipe:
90.4% → 90.3% (-0.1%)`. CI also posts per-package deltas via
`go-coverage-report` after push, but the local gate catches
regressions before push.

## Integration Tests (Kind)

When a test needs a real Kubernetes API — controller logic, RBAC
behavior, watch semantics — use Kind. `make dev-env` spins up a Kind
cluster and starts Tilt; `make dev-env-clean` tears it down. Prefer
`controller-runtime`'s `envtest` (local apiserver/etcd, no Kind) for
unit-scope controller tests; reserve Kind for cross-package or
deployer-output flows that need a full cluster.

```bash
make dev-env                    # Kind + Tilt running
make dev-env-clean              # delete cluster, stop Tilt
```

Live-cluster tests still set `--no-cluster` when the call path
includes the validator — the flag suppresses validator RBAC and Job
deployment; cluster-touching code under test is unaffected.

## Chainsaw Health Checks

Each component in the registry can carry an optional Chainsaw assert
YAML that runs after the bundle deploys to confirm the component is
healthy: pods Ready, services resolve, CRDs `Established`, custom
resources reach a known phase. The asserts live alongside the
component:

```text
recipes/checks/<component>/health-check.yaml
```

Discover them and run locally against a Kind cluster:

```bash
make check-health COMPONENT=gpu-operator       # single component
make check-health-all                           # all components
make validate-local RECIPE=recipe.yaml          # full pipeline
```

**When to add one:** the contributor wants to verify, after deploy,
that the component's pods reach Ready, its CRDs are `Established`,
its operator deploys its custom resources, or its services resolve.
Anything subtler than "is it alive" belongs in a container-per-validator
check (see [validator.md](validator.md)) so the assertion runs as part
of a tracked validation phase rather than a one-shot Chainsaw step.

**Always include `--no-cluster` on `aicr` invocations inside the
chainsaw script.** A chainsaw test that runs `aicr validate` without
the flag will attempt to install RBAC into the test cluster, which
diverges from the assertion's intent (the validator job is the unit
under test elsewhere).

Components with an assert file today include `gpu-operator`,
`network-operator`, `nfd`, `cert-manager`, `nvsentinel`, `kueue`,
`kubeflow-trainer`, `slinky-slurm`, and more — browse
[`recipes/checks/`](https://github.com/NVIDIA/aicr/tree/main/recipes/checks)
for the full list.

## KWOK Matrix Testing

KWOK (Kubernetes WithOut Kubelet) simulates a GPU cluster without
real hardware. CI uses it to validate two things per recipe:

1. **Scheduling shape.** Node selectors, tolerations, and resource
   requests render correctly and land pods on the simulated nodes
   the overlay expects.
2. **Deployer output correctness.** The same recipe is re-rendered
   through every output adapter — `helm`, `argocd-oci`,
   `argocd-helm-oci`, `flux-oci` — and each renders to a working
   bundle the GitOps controllers can reconcile.

KWOK nodes have no kubelet, so pods never actually run. KWOK testing
**does not exercise** runtime validators (NCCL, inference-perf) or
component health — for those, see [validator.md](validator.md). If a
recipe's constraints reference dimensions the KWOK node profiles do
not provide (e.g., a GPU model no profile registers), the bundle
renders but pods stay Pending or land on the wrong nodes. Extend
`kwok/profiles/` rather than relax the recipe — KWOK is the
simulated reflection of production shape, not a relaxed substitute.

For the design rationale and the spike findings that justify the
chart pin and Repository-secret shape, see
[ADR-008](https://github.com/NVIDIA/aicr/blob/main/docs/design/008-kwok-deployer-matrix.md).
For cluster-level KWOK setup (node profiles, recipe auto-discovery),
see [kwok/README.md](https://github.com/NVIDIA/aicr/blob/main/kwok/README.md).

### Deployer Coverage Matrix

| Tier | Trigger | Deployers exercised |
|------|---------|----------------------|
| Tier 1 — generic overlays | every PR + push | `helm`, `argocd-oci`, `argocd-helm-oci`, `flux-oci` |
| Tier 2 — diff-aware accelerator overlays | PR only, conditional on changed files | `helm` only |
| Tier 3 — full overlay set | push to `main` + nightly schedule | `helm`, `argocd-oci`, `argocd-helm-oci`, `flux-oci` |

| Lane | Pull artifact | Apply manifests | Reconcile to Ready |
|---|---|---|---|
| `helm` | n/a (filesystem) | `helm install` | pods scheduled |
| `argocd-oci` | repo-server OCI pull | Argo CD sync | `Synced+Healthy` |
| `argocd-helm-oci` | `helm pull` OCI | wrapper chart install | `Synced+Healthy` |
| `flux-oci` | source-controller OCI pull | kustomize-controller apply | all HelmReleases `Ready=True` + ArtifactGenerators Ready |

Tier 2 stays `helm`-only because its job is to verify accelerator-specific
overlays still render correctly when their inputs change. The deployer
shape is orthogonal — re-running through Argo CD would only re-exercise
template rendering, which Tier 1 and Tier 3 already cover on the generic
overlays. For filesystem (Git-source) round-trip coverage of `argocd` /
`flux`, see [#963](https://github.com/NVIDIA/aicr/issues/963).

### Running KWOK Locally

```bash
unset GITLAB_TOKEN
make build
make kwok-cluster

# Single recipe + single deployer
make kwok-test-deployer RECIPE=eks-training DEPLOYER=argocd-oci
```

Valid `DEPLOYER` values: `helm`, `argocd-oci`, `argocd-helm-oci`,
`flux-oci`. The target invokes
`kwok/scripts/run-all-recipes.sh --deployer <name> <recipe>`, which
calls `install-infra.sh` once with `DEPLOYER` exported (in-cluster
`registry:2` always; Argo CD for `argocd-*`; Flux 2 controllers for
`flux-oci`), then runs `validate-scheduling.sh` for the recipe.

**Registry host port.** The Kind cluster exposes the in-cluster
`registry:2` Service on **host port 5500** (`kwok/kind-config.yaml`'s
`extraPortMappings`). 5500 avoids Apple ControlCenter
(AirPlay / Handoff) which listens on host port 5000 by default on
macOS. Linux runners have 5500 free too. The in-cluster NodePort
(`30500`) and Service `containerPort` (`5000`) are hardcoded and
independent — Argo CD's repo-server reaches the registry via Service
DNS (`registry.aicr-registry.svc.cluster.local:5000`) regardless.

**Sweeping all deployers locally.** `make kwok-test-all` defaults to
`helm`; there is no matrix-aware make target. Loop in shell:

```bash
for d in helm argocd-oci argocd-helm-oci flux-oci; do
  make kwok-test-deployer RECIPE=eks-training DEPLOYER="$d" || break
done

# Full recipe set under a single deployer:
bash kwok/scripts/run-all-recipes.sh --deployer argocd-oci
```

### Failure Modes and Exit Codes

The three scripts emit distinct exit codes so CI, the Make target,
and local loops can branch on failure mode without parsing logs.

| Script | Code | Meaning |
|--------|------|---------|
| `install-infra.sh` | 10 | `yq` missing or `.settings.yaml` field absent |
| `install-infra.sh` | 20 | Registry Deployment not Ready within 120 s |
| `install-infra.sh` | 21 | Registry not reachable on host port within 60 s |
| `install-infra.sh` | 30 | Argo CD Helm install failed |
| `install-infra.sh` | 31 | `applications.argoproj.io` CRD not Established within 120 s |
| `install-infra.sh` | 40 | Repository secret apply failed |
| `install-infra.sh` | 60 | Flux install manifest apply failed |
| `install-infra.sh` | 61 | Flux controller not Ready within 180 s |
| `install-infra.sh` | 62 | Flux CRDs not Established within 60 s |
| `validate-scheduling.sh` | 50 | GitOps sync deadline hit |
| `run-all-recipes.sh` | 50 | Three consecutive GitOps sync timeouts; ADR-008 3-strike rule tripped |

Exit code 50 is distinct so the 3-strike rule in `run-all-recipes.sh`
counts only sync-deadline strikes, not bundle-render or
scheduling-shape failures.

### Tuning the Sync Deadline

Four environment variables shape how long the GitOps lanes wait
before declaring a sync timeout. Argo CD and Flux pairs are
independent.

| Variable | Default | Purpose |
|----------|---------|---------|
| `KWOK_ARGOCD_SYNC_TIMEOUT` | `300` s | Deadline for all child Argo CD Applications to reach `Synced+Healthy` |
| `KWOK_ARGOCD_ROOT_GRACE` | `30` s | Grace period for the root Application before deadline counting starts |
| `KWOK_FLUX_SYNC_TIMEOUT` | `300` s | Deadline for OCIRepository fetch + Kustomization apply + HelmReleases `Ready=True` + ArtifactGenerators Ready |
| `KWOK_FLUX_ROOT_GRACE` | `30` s | Grace period for the outer Kustomization before deadline counting starts |

On a clean local Kind cluster `Synced+Healthy` lands in ~30 s; the
300-second default exists to absorb CI variance. If a local run trips
code 50 but the cluster is otherwise healthy, raise the relevant
timeout before assuming the recipe is broken — cold-cluster image
pulls are the most common cause.

### Debugging CI Failures

When `kwok-test` fails, it uploads an artifact named
`kwok-debug-<recipe>-<deployer>-<run_id>` containing:

- `<cluster>-resources.txt`, `<cluster>-nodes.txt`, `<cluster>-pods.txt`, `<cluster>-events.txt`
- `<cluster>-argo-apps.yaml` plus the repo-server and application-controller logs (argocd lanes)
- `<cluster>-flux-resources.yaml` (OCIRepositories, Kustomizations, HelmReleases, ArtifactGenerators, ExternalArtifacts) plus source-, kustomize-, and helm-controller logs (flux-oci lane)
- `<cluster>-registry.log` — last 200 lines of the in-cluster `registry:2`

Start with the repo-server log (Argo CD) or source-controller log
(Flux) for OCI-pull failures. Application-controller / kustomize-controller
logs show reconciliation decisions and prune behavior;
helm-controller logs surface per-`HelmRelease` install outcomes.

### Adding a New Deployer Value

The deployer set is finite and matches what `pkg/bundler` emits. To
add a new value (say, `argocd-git`):

1. Add a `case` branch in `kwok/scripts/validate-scheduling.sh`'s
   `resolve_argocd_root_app()` (or `resolve_flux_root_names()` for
   Flux-reconciled lanes), plus branches in `generate_bundle` and
   `deploy_bundle`. Reuse the existing `argocd-oci` / `flux-oci`
   branches as templates.
2. Extend the `DEPLOYER` allowlist in
   `kwok/scripts/run-all-recipes.sh` (`case "$DEPLOYER" in` in `main()`).
3. Extend the `case "${DEPLOYER}"` branches in `install-infra.sh`'s
   `main()` so the right controller stack is installed.
4. Extend the `deployer:` input description in
   `.github/actions/kwok-test/action.yml`.
5. Add the value to the `deployer:` matrix in Tier 1 and Tier 3 of
   `.github/workflows/kwok-recipes.yaml`. Leave Tier 2 alone — the
   orthogonality rationale above still applies.
6. Add a row to the [Deployer Coverage Matrix](#deployer-coverage-matrix)
   above so contributors can discover the new lane.

If the new value requires changes to in-cluster infra (different
registry, different Argo CD chart, additional CRDs), update
`install-infra.sh` and pin any new versions in `.settings.yaml`. The
exit-code taxonomy in [Failure Modes and Exit Codes](#failure-modes-and-exit-codes)
is contiguous — pick the next free code if a new distinct failure
mode appears.

## E2E Tests

`./tools/e2e` is the end-to-end pipeline runner. It builds, snapshots,
generates recipes, validates, bundles, and (when credentials are
available) deploys against real cloud accounts.

```bash
unset GITLAB_TOKEN
./tools/e2e
```

`make qualify` invokes the e2e step as part of the pre-push gate.
CI runs the same script in the push workflow. Cloud credentials are
optional — without them, the e2e exercises the artifact-generation
half of the pipeline and skips deploy-side assertions.

## The `make qualify` Gate

`make qualify` is the canonical pre-push command. It runs:

- `test-coverage` — `go test -race ./...` plus the 75% coverage floor.
- `lint` — golangci-lint with `.golangci.yaml` plus yamllint.
- `e2e` — the end-to-end pipeline runner.
- `scan` — Grype vulnerability scan.
- `license-check` — license header / dependency-license sweep.

CI runs the equivalent. If `make qualify` passes locally on the
current branch, push CI will pass.

**Branch lint gate for Go changes.** If a PR changes any `.go` file,
you must also run:

```bash
golangci-lint run -c .golangci.yaml ./pkg/<affected>/...
golangci-lint run -c .golangci.yaml ./...           # full sweep
```

This applies even to PRs labeled `documentation` when they include
incidental Go changes. Do not rely on CI to surface lint failures —
the pre-push gate is local.

## Common Gotchas

- **`goreleaser` fails when both `GITLAB_TOKEN` and `GITHUB_TOKEN`
  are set.** `make build`, `make qualify`, and `./tools/e2e` all
  invoke goreleaser indirectly. Always `unset GITLAB_TOKEN` in the
  shell first. This is one of the most common local-only CI-passes-fine
  failure modes.
- **Forgetting `make bom-docs`** after a `recipes/registry.yaml`,
  component values, or chart-pin change. `docs/user/container-images.md`
  goes stale silently — `make bom-check` is **opt-in only** and not
  wired into `make qualify`, `make lint`, or the merge gate today.
  CI does not catch this. Run `make bom-docs` locally any time the
  change touches charts.
- **Coverage decrease > 0.5%** blocks the PR. Add tests rather than
  reaching for `// nolint` or `t.Skip` — both are review-blockers
  under the no-skip-tests rule in CLAUDE.md.
- **Live-cluster connections from unit tests.** A test that forgets
  `--no-cluster` will attach to whichever kubeconfig is current and
  create RBAC against it. Always pass `WithNoCluster(true)` (Go) or
  `--no-cluster` (CLI / chainsaw) on the validator path.
- **CLI tests asserting on stdout.** `pkg/cli` writes through
  `cmd.Root().Writer`. A test that captures `os.Stdout` will see
  nothing. Use `cmd.SetOut(buf)` and assert on `buf.String()`.

## See Also

- [contributor index](index.md) — package layout and the scope boundary
- [recipe.md](recipe.md) — recipe-level constraints and merge tests
- [validator.md](validator.md) — validator engine, chainsaw checks, container-per-validator pattern
- [CLAUDE.md](https://github.com/NVIDIA/aicr/blob/main/.claude/CLAUDE.md) — coding rules and anti-patterns table
- [ADR-008](https://github.com/NVIDIA/aicr/blob/main/docs/design/008-kwok-deployer-matrix.md) — KWOK deployer matrix rationale
- [kwok/README.md](https://github.com/NVIDIA/aicr/blob/main/kwok/README.md) — KWOK cluster setup and node profiles
