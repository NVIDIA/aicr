# ADR-008: KWOK CI Deployer Matrix (Full Deploy Parity)

## Status

**Proposed** — 2026-05-11 (design-only; not implemented).

Implementation tracked under [#843](https://github.com/NVIDIA/aicr/issues/843).
The matrix dimensions, in-cluster topology, OCI flow, and acceptance
criteria described below are intent, not current behavior.

## Problem

The KWOK CI workflow validates bundle deployment for the default
`helm` deployer only. `aicr bundle --deployer argocd` and
`--deployer argocd-helm` emit Argo CD `Application` manifests that no
CI lane exercises end-to-end. A template regression in
`pkg/bundler/deployer/argocd/templates/application.yaml.tmpl` would
ship to `main` and only surface when a user attempts a release.

Existing coverage gaps:

| Layer | What it validates | Deployer coverage |
|---|---|---|
| KWOK recipes (`.github/workflows/kwok-recipes.yaml`, `kwok/scripts/validate-scheduling.sh`) | Bundle deploys and pods reach `Running` | `helm` only |
| Bundler unit tests (`pkg/bundler/bundler_test.go`) | File presence, sync-wave annotation strings | Structural only |
| Chainsaw CLI (`tests/chainsaw/cli/bundle-variants/chainsaw-test.yaml`) | `yamllint`; `helm template` for `argocd-helm` | Syntax + Helm render only |

Concrete risks that slip past today's CI:

- Invalid `apiVersion` / `kind` after an Argo CD CRD bump.
- Missing required fields (e.g., multi-source `spec.sources[].repoURL`).
- Broken Helm value references when `argocd-helm` is rendered through Argo CD.
- Sync-wave / finalizer annotations that pass `yamllint` but cause sync ordering bugs.

The production path the team depends on most is OCI: `aicr bundle
--output oci://registry/...` packages the bundle as an OCI artifact
that Argo CD pulls natively (`pkg/cli/bundle.go:165-173` already
auto-derives `repoURL` and `targetRevision` from the OCI reference).
Nothing in CI today exercises that push → pull → reconcile → deploy
round-trip.

## Goals

1. CI fails when any deployer emits a bundle that does not deploy successfully.
2. The OCI artifact path (push → Argo CD pull → reconcile → pods Running) is exercised on every PR and every nightly run.
3. Local repro matches CI: a single `make` target reproduces the same flow against a Kind cluster.

## Non-Goals

- Validating Argo CD sync-policy correctness (user-owned).
- Real upstream Helm chart pull fault injection (existing helm path covers it).
- Multi-cluster Argo CD topology testing.
- Coverage measurement for shell scripts.
- Static schema validation of `Application` CRDs (Option A from issue #843; tracked separately).
- Render-through-Argo CD on a Kind cluster without OCI (Option B from issue #843; subsumed by this ADR).

## Decision

Add a `deployer` dimension to the existing `kwok-recipes.yaml` matrix
with values `{helm, argocd-oci, argocd-helm-oci}`. Per matrix cell,
boot a shared in-cluster OCI registry and Argo CD; push the bundle
via OCI; observe Argo CD reconciliation to `Synced+Healthy`; then
reuse the existing pod-Running verification.

| Tier | Trigger | Deployer values |
|---|---|---|
| Tier 1 — generic overlays | every PR + push | `{helm, argocd-oci, argocd-helm-oci}` |
| Tier 2 — diff-aware accelerator overlays | PR only, conditional | `helm` only (unchanged) |
| Tier 3 — full overlay set | push to main + nightly | `{helm, argocd-oci, argocd-helm-oci}` |

**Rationale for tier scope.** Argo CD template regressions surface on
generic overlays (Tier 1) — that is where the issue's bug class
lives. Full accelerator-specific Argo CD coverage runs nightly and on
push to main without inflating PR latency. Tier 2 stays helm-only
because its purpose is diff-aware accelerator-config validation;
Argo CD shape is orthogonal.

**Rationale for OCI over Git source.** The user-confirmed production
path is OCI. A local Gitea / dumb-HTTP git server would test the
template shape but not the source most users actually use. The
in-cluster `registry:2` flow exercises the exact path real
deployments take.

## Alternatives Considered

| Alternative | Why rejected |
|---|---|
| Separate workflow (`kwok-deployers-oci.yaml`) for argocd lanes | Duplicates tier discovery, summary aggregation, kwok-test composite action wiring. Two places to keep in sync. |
| Single CI job loops `{helm, argocd, argocd-helm}` sequentially | Loses parallelism (~3× Tier 3 wall time). Per-deployer failure visibility buried in one job's logs. |
| Source bundle via in-cluster Gitea (full git protocol) | Heavier image (~300 MB), real Git push protocol unnecessary — Argo CD only needs to pull. |
| Source bundle via dumb-HTTP git server (alpine + git-daemon Pod) | Smaller than Gitea, but does not exercise the OCI path which is the production concern. |
| Push to a real branch on the workspace repo and point Argo CD at github.com | Does not work for fork PRs (no write token); pollutes the repo with throwaway refs. |
| Full cartesian `{helm, argocd, argocd-helm} × {dir, oci}` | 6 cells per recipe; deployer-template bugs surface regardless of OCI vs dir, so the extra columns buy little incremental signal. |

## Architecture

One Kind cluster per matrix cell. Inside each cluster:

```
┌─────────────────── Kind cluster (KWOK-enabled) ───────────────────┐
│                                                                   │
│  ┌─────────────────┐    ┌─────────────────────────────────────┐   │
│  │  aicr-registry  │    │             argocd                  │   │
│  │  (registry:2)   │◀──▶│  argocd-server, repo-server, etc.   │   │
│  │  Service:5000   │    │  configured w/ insecure local OCI   │   │
│  └────────▲────────┘    └──────────────┬──────────────────────┘   │
│           │                            │ syncs Applications        │
│           │                            ▼                           │
│           │             ┌─────────────────────────────────┐        │
│           │             │   workload namespaces           │        │
│           │             │   (KWOK fake pods → Running)    │        │
│           │             └─────────────────────────────────┘        │
└───────────┼───────────────────────────────────────────────────────┘
            │ kind extraPortMappings: host 5000 → node 30500
   ┌────────┴────────┐
   │   CI runner     │  aicr bundle --output oci://localhost:5000/...
   └─────────────────┘
```

- **`registry:2`** in `aicr-registry` namespace, plain HTTP, exposed
  to the runner via `extraPortMappings`, accessible inside the
  cluster via Service DNS `registry.aicr-registry.svc.cluster.local:5000`.
- **Argo CD** installed via Helm chart pinned in `.settings.yaml`,
  configured at install time with a `repo-creds` Secret for the local
  OCI registry (`insecure: true`, `enableOCI: true`).
- **KWOK controller + stage-fast** unchanged from current setup.
- **Both `aicr-registry` and `argocd` namespaces are allowlisted** in
  `cleanup_between_tests` / `cleanup_old_tests` so they persist
  across recipes within a single `run-all-recipes.sh` invocation.

**Reachability.** The runner reaches the registry via Kind's
`extraPortMappings`. Inside the cluster, Argo CD's `repo-server`
resolves `registry.aicr-registry.svc.cluster.local:5000`. Same
daemon, two access paths, same artifact digest.

**Cluster reuse.** All recipes in one matrix cell share one Kind
cluster (matches today's helm behavior). Different matrix cells get
separate clusters because they are separate GitHub Actions jobs.

## Data Flow

Per `(recipe, deployer)` matrix cell:

```
1. Cluster bootstrap (once per matrix cell, before first recipe)
   ├── kind create cluster (with extraPortMappings 5000→30500)
   ├── helm install kwok-controller + kwok-stage-fast
   ├── kubectl apply registry:2 Deployment + Service (NodePort 30500)
   ├── helm install argocd (pinned chart)
   └── kubectl apply repo-creds Secret
       (insecure: true, enableOCI: true,
        url: registry.aicr-registry.svc.cluster.local:5000)

2. Per-recipe (loop in run-all-recipes.sh)
   ├── cleanup_between_tests           [allowlist: argocd, aicr-registry]
   ├── apply-nodes.sh <recipe>         [KWOK fake nodes]
   └── validate-scheduling.sh --deployer <d> <recipe>
        │
        ├── if deployer == helm:
        │     [existing path unchanged]
        │     aicr bundle --recipe ... --output ./bundle
        │     bundle/deploy.sh --no-wait
        │     verify_pods
        │
        └── if deployer in {argocd-oci, argocd-helm-oci}:
              aicr bundle --recipe ... \
                          --deployer <d> \
                          --output oci://localhost:5000/aicr/<recipe>:<sha>-<deployer>
              kubectl apply -f bundle/app-of-apps.yaml          (argocd)
              # or: helm install <release> oci://...             (argocd-helm)
              wait_for_argocd_sync                              (new helper)
              verify_pods                                       (existing)

3. Per-recipe teardown
   └── cleanup_between_tests           [argocd/registry survive]
```

**`wait_for_argocd_sync` (new helper)** polls
`kubectl get application -n argocd -o json` until every `Application`
has `status.sync.status=Synced` and `status.health.status=Healthy`
(or `Progressing` after a bounded retry window — KWOK pods may take
a few stage-fast cycles to settle). On failure, dumps specific Argo
CD conditions for actionable logs.

**OCI tag convention:** `oci://localhost:5000/aicr/<recipe>:<short-sha>-<deployer>`
avoids stale-tag confusion when running the same recipe across
deployer values back-to-back in local development.

## Files Changing

### Definitely changing

| File | Change |
|---|---|
| `.settings.yaml` | Pin `argocd_chart` and `registry_image` under `versions:` |
| `kwok/kind-config.yaml` | Add `extraPortMappings: {containerPort: 30500, hostPort: 5000}` |
| `kwok/scripts/install-infra.sh` (new) | Idempotent install of registry + Argo CD + repo-creds Secret |
| `kwok/scripts/validate-scheduling.sh` | Add `--deployer <name>` flag; branch on deployer |
| `kwok/scripts/run-all-recipes.sh` | Accept deployer arg; allowlist `aicr-registry` / `argocd` in cleanup |
| `.github/actions/kwok-test/action.yml` | New input `deployer`; pass through to `run-all-recipes.sh` |
| `.github/workflows/kwok-recipes.yaml` | Add `deployer: [helm, argocd-oci, argocd-helm-oci]` to Tier 1 and Tier 3 matrices |
| `Makefile` | New `kwok-test-deployer RECIPE=... DEPLOYER=...` target |
| `docs/contributor/<area>.md` (KWOK testing page) | Document the matrix and local repro |

### Possibly changing (Phase 0 dependent)

| File | Trigger |
|---|---|
| `pkg/cli/bundle.go` (≈ line 167) | If Argo CD requires `oci://` scheme prefix in derived `repoURL` |
| `pkg/bundler/deployer/argocd/templates/app-of-apps.yaml.tmpl` | If `directory.recurse` does not work against OCI artifact sources |
| `pkg/bundler/deployer/argocd/argocd.go` | If Argo CD needs explicit OCI mode hint or alternate shape selection |

### Out of scope (will not touch)

- `pkg/bundler/deployer/argocd/templates/application.yaml.tmpl` — already supports single-source and multi-source shapes.
- `pkg/oci/push.go` — push logic verified to exist and work.
- `pkg/oci/reference.go` — parsing verified.

## Error Handling and Failure Modes

| # | Failure | Detection | Remediation |
|---|---|---|---|
| 1 | Registry push fails | `aicr bundle --output oci://...` exits non-zero | `wait_for_registry_ready` polls `curl -sf http://localhost:5000/v2/` before any bundle step; dump registry Pod logs |
| 2 | Argo CD install / CRD not ready | `kubectl wait --for=condition=Established crd/applications.argoproj.io --timeout=120s` | Bounded wait; fail fast with `kubectl describe deploy -n argocd` |
| 3 | Argo CD cannot pull from local OCI | `Application.status.conditions[].type=ComparisonError` | `dump_argocd_failures` runs `kubectl get applications -n argocd -o json \| jq '.items[].status.conditions'` + repo-server logs |
| 4 | Application stuck `OutOfSync` / `Progressing` past deadline | `wait_for_argocd_sync` deadline (default 300 s, override via `KWOK_ARGOCD_SYNC_TIMEOUT`) | Dump each Application's `status.operationState.message` and `status.health.message` |
| 5 | Pod scheduling fail | Existing `verify_pods` logic | Unchanged |

**Determinism**

- Registry and Argo CD installs are idempotent (`helm upgrade --install`, `kubectl apply`).
- Cluster-state cleanup is allowlist-based; `argocd` and `aicr-registry` are added to the allowlist explicitly so future system-namespace additions do not sweep our infra.
- OCI tag uniqueness per matrix cell prevents stale-tag confusion across deployer values.

**Artifact upload on failure** — extend the `kwok-test` composite
action to also collect:

- `kubectl get applications -n argocd -o yaml`
- `kubectl logs -n argocd deploy/argocd-repo-server --tail=500`
- `kubectl logs -n argocd deploy/argocd-application-controller --tail=500`
- `kubectl logs -n aicr-registry deploy/registry --tail=200`

**3-strike rule.** If `wait_for_argocd_sync` hits its deadline three
times across a `run-all-recipes.sh` invocation, bail the whole job
rather than continuing. Partial coverage is worse than a clear
failure.

## Testing Strategy

| Layer | Coverage |
|---|---|
| Unit (Go) | No new code on the recommended path. If Phase 0 surfaces a bundler change, that change ships with a table-driven test in `pkg/cli/bundle_test.go`. |
| Chainsaw (existing) | `tests/chainsaw/cli/bundle-variants` continues to gate schema-shape correctness. |
| KWOK matrix (new, this ADR) | Three deployer values × Tier 1 generic overlays + Tier 3 full overlay set. |
| Local repro | `make kwok-test-deployer RECIPE=eks-training DEPLOYER=argocd-oci` runs the same script the CI matrix uses against a local Kind cluster. |
| Negative test (one-shot, manual) | Before merging, deliberately break `application.yaml.tmpl`, confirm matrix turns red, revert. Document in PR description, not ongoing CI. |

## Acceptance Criteria

1. A deliberate regression in `pkg/bundler/deployer/argocd/templates/application.yaml.tmpl` causes Tier 1 to turn red.
2. All three deployer values pass on a representative generic overlay (e.g., `eks-training`).
3. `make qualify` passes locally.
4. Tier 3 nightly run on the feature branch completes within the existing 15-minute `timeout-minutes` per matrix cell.

## Phase 0 — Spike (before Phase 1)

The bundler's existing OCI emission for the `argocd` deployer has not
been exercised end-to-end against a real Argo CD. Two assumptions
need verification before locking the design:

1. **Derived `repoURL` includes the `oci://` scheme prefix** (or Argo CD detects OCI by some other mechanism). Current code at `pkg/cli/bundle.go:167-168` sets `opts.repoURL = opts.ociRef.Registry + "/" + opts.ociRef.Repository` — no scheme. Argo CD's OCI support typically requires the scheme prefix or a repo-credential annotation.
2. **`app-of-apps.yaml.tmpl` uses `directory: recurse: true, include: '*/application.yaml'`** — this is a git-native field. Argo CD's OCI-artifact source may or may not honor `directory.recurse`. If it does not, the template needs a different shape for OCI artifacts (likely `chart:` for OCI Helm, or a flat manifest list).

**Spike deliverable.** A 30-line test script that pushes a minimal
bundle to a `registry:2` Pod and applies the rendered
`app-of-apps.yaml` to a real Argo CD install, captures whether
reconciliation completes.

Outcomes:

- Both assumptions hold → Phase 1 proceeds with no bundler changes.
- (1) needs fixing → small `pkg/cli/bundle.go` patch + test in Phase 1.
- (2) needs fixing → template change in Phase 1; may also need a `pkg/bundler/deployer/argocd/argocd.go` flag to select OCI vs git shape.

## Open Questions

- **Argo CD chart version pin** — needs to be a version with stable OCI support. Latest 2.x as of this writing should suffice; the pin is set in `.settings.yaml` and bumped via Renovate.
- **Registry image pin** — `registry:2.8.x` or the latest 2.x. No write-side auth needed, plain HTTP.
- **`KWOK_ARGOCD_SYNC_TIMEOUT` default** — 300 s is a starting estimate. Calibrate during Phase 1 based on a representative recipe's actual sync time.

## References

- Issue [#843](https://github.com/NVIDIA/aicr/issues/843)
- Existing KWOK workflow `.github/workflows/kwok-recipes.yaml`
- Existing validate script `kwok/scripts/validate-scheduling.sh`
- Bundler OCI flow `pkg/cli/bundle.go:165-173`, `pkg/oci/push.go`
- Argo CD application template `pkg/bundler/deployer/argocd/templates/application.yaml.tmpl`
- Kind local-registry pattern: <https://kind.sigs.k8s.io/docs/user/local-registry/>
