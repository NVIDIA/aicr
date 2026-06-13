# ADR-010: KWOK Git-Source Deployer Lanes (Gitea Filesystem-Bundle Round-Trip)

## Status

**Implemented (flux-git)** — 2026-06-10.

Extends [ADR-008](008-kwok-deployer-matrix.md) (KWOK CI Deployer
Matrix). Implementation tracked under
[#963](https://github.com/NVIDIA/aicr/issues/963). The `flux-git` lane
ships first; the `argocd-git` lane described in the issue follows the
same infrastructure and is a follow-up.

## Problem

ADR-008 added OCI round-trip lanes (`argocd-oci`, `argocd-helm-oci`,
`flux-oci`, PR [#956](https://github.com/NVIDIA/aicr/issues/956)): the bundle is pushed to an in-cluster registry
and a GitOps controller pulls and reconciles it. What remains
uncovered is the **filesystem-bundle round-trip**: `aicr bundle
--output ./dir` → Git server → Flux `GitRepository` (or Argo CD
`Application` with a Git `repoURL`) → reconcile.

This matters because Git is the GitOps source many production users
choose — it is the workflow the flux deployer itself documents
("Push this bundle to your Git repository", `pkg/bundler/deployer/
flux/flux.go` `DeploymentSteps`). The Git output shape is *different
code* from the OCI shape:

- With a filesystem `--output`, the flux deployer renders
  `sources/gitrepo-<sanitized-url>.yaml` `GitRepository` source CRs
  from `--repo` (`collectGitSources()` in
  `pkg/bundler/deployer/flux/sources.go`), and local-chart
  HelmReleases reference that source via
  `sourceRef: {kind: GitRepository}` with `chart: ./<dir>`.
- With an OCI `--output`, none of that renders — HelmReleases
  reference the `aicr-bundle` OCI source / ExternalArtifacts instead.

A regression in the `gitrepo-*.yaml` template, the URL sanitization,
the default `branch: main` ref, or the kustomization's inclusion of
the source CRs would ship to `main` undetected by every existing lane.

ADR-008 explicitly rejected Gitea — for the *OCI-first* scope ("does
not exercise the OCI path which is the production concern"). That
choice was about which transport to validate first, not a permanent
exclusion. This ADR supplements it: the OCI lanes remain unchanged;
the Git lanes cover the second documented product surface.

## Goals

1. CI fails when the flux deployer's filesystem/Git bundle shape does
   not reconcile end-to-end (clone → kustomize apply → HelmRelease
   terminal state → pods Running).
2. The Git path is exercised with a real Git server and the real
   `git push` → `source-controller` clone protocol, not a shape check.
3. Local repro matches CI:
   `make kwok-test-deployer RECIPE=<r> DEPLOYER=flux-git`.

## Non-Goals

- `argocd-git` lane (same Gitea infra; follow-up under [#963](https://github.com/NVIDIA/aicr/issues/963)).
- Git auth flows (`GitRepository.spec.secretRef`, deploy keys). The
  bundler's gitrepo template emits no `secretRef`, so the contract
  under test is anonymous read access (see Decision).
- Tier 2 coverage (stays helm-only per ADR-008's tier rationale).
- Replacing the OCI lanes — both transports stay in the matrix.

## Decision

Add a `flux-git` value to the deployer matrix (Tier 1 + Tier 3). The
lane installs **Gitea** in the existing `aicr-registry` namespace via
`install-infra.sh`, bundles to the filesystem, pushes the bundle tree
to Gitea over the Kind host-port mapping, applies an outer
`GitRepository` + `Kustomization` wrapper pair, and reuses the
existing chainsaw sync-gate framing and pod verification.

Key choices, in dependency order:

**Anonymous public repos, no secretRef.** The bundler's
`gitrepo-source.yaml.tmpl` has no `secretRef`, so the repo MUST be
anonymously clonable for the rendered bundle to reconcile at all.
Gitea is configured with
`GITEA__repository__DEFAULT_PUSH_CREATE_PRIVATE=false` so pushed
repos are public; Flux's source-controller clones with no credentials
— exactly what the bundle's own source CR requires. This is a
product-contract test, not a CI convenience.

**Push-to-create, no per-recipe API calls.**
`GITEA__repository__ENABLE_PUSH_CREATE_USER=true` means the first
`git push` creates `aicr/<recipe>.git`. Re-runs `git push --force`,
which resets repo state idempotently (Gitea state is
emptyDir-ephemeral per cluster anyway).

**Dual-view URLs (same pattern as the OCI lanes).** The runner pushes
to `http://aicr:<pw>@localhost:3300/aicr/<recipe>.git` (Kind
`extraPortMappings` host 3300 → NodePort 30300); the in-cluster URL
baked into `--repo` and the outer `GitRepository` is
`http://gitea.aicr-registry.svc.cluster.local:3000/aicr/<recipe>.git`.
Host 3300, not Gitea's default 3000, for the same collision-avoidance
reason as the registry's 5500-vs-5000 (port 3000 is commonly held by
Grafana / local dev servers).

**Branch `main` is a hard requirement.** The bundler defaults
`GitRepository.ref.branch` to `main` (`resolveTargetRevision()`), and
there is no CLI flag to change it for filesystem output, so the test
driver pushes `main`.

**Two GitRepository objects, accepted duplication.** The driver
applies an outer `aicr-bundle` GitRepository (the Kustomization's
source); the bundle itself carries an inner sanitized-name
GitRepository (`gitea-aicr-registry-svc-cluster-local-3000-aicr-
<recipe>`) that local-chart HelmReleases reference. Both point at the
same public repo. Merging them would require reproducing the Go
`sanitizeSourceName()` 63-char truncation logic in bash — fragile for
zero coverage gain.

**Sibling chainsaw test, byte-identical predicates.**
`tests/chainsaw/kwok/flux-git-sync/` differs from `flux-sync/` only
in the source-Ready step (GitRepository instead of OCIRepository —
chainsaw cannot template the resource `kind:` of an assert) plus an
error-polarity "no GitRepository non-Ready" step that covers the
inner source without naming it. The Kustomization, HelmRelease
terminal-pass, and ArtifactGenerator steps are byte-identical copies;
SYNC NOTE headers in both files bind them.

**Tier scope** mirrors ADR-008: Tier 1 (every PR) + Tier 3 (push to
main + nightly); Tier 2 stays helm-only.

## Alternatives Considered

| Alternative | Why rejected |
|---|---|
| Dumb-HTTP git server (alpine + git-daemon Pod) | No push-to-create, no smart-HTTP push without extra CGI wiring; Gitea's rootless image is one Deployment with env-only config and a real API for debugging. |
| Gitea Helm chart | Heavier (PVC, postgres options, ingress); a plain Deployment + NodePort mirrors the established `install_registry()` pattern in the same file. |
| Private repo + `secretRef` Secret | The bundler's gitrepo template emits no `secretRef`, so a private repo would test infrastructure the product cannot use — and mask the real contract (anonymous read). |
| Per-recipe repo creation via Gitea API | Push-to-create does it in zero extra calls; API bootstrap is one more failure mode and one more place to leak the credential. |
| Parameterizing `flux-sync` over the source kind | Chainsaw bindings interpolate values, not the resource `kind:` of an assert; a single test would need duplicated steps behind conditionals — worse than a sibling with a sync contract. |
| Merging outer/inner GitRepository objects | Requires reproducing Go's `sanitizeSourceName()` truncation in bash; duplication is harmless (two pulls of the same public repo at 1m/10m intervals). |

## Architecture

```
┌─────────────────── Kind cluster (KWOK-enabled) ───────────────────┐
│                                                                   │
│  ┌──────────────────────────┐      ┌───────────────────────────┐  │
│  │      aicr-registry       │      │        flux-system        │  │
│  │  registry:3 Service:5000 │      │  source-controller        │  │
│  │  gitea     Service:3000  │◀─────│  kustomize-controller     │  │
│  │  (sqlite3, emptyDir,     │clone │  helm-controller          │  │
│  │   push-create, public)   │      │  source-watcher           │  │
│  └────────▲─────────────────┘      └─────────────┬─────────────┘  │
│           │                                      │ applies bundle │
│           │                                      ▼                │
│           │                       ┌──────────────────────────┐    │
│           │                       │   workload namespaces    │    │
│           │                       │ (KWOK fake pods→Running) │    │
│           │                       └──────────────────────────┘    │
└───────────┼───────────────────────────────────────────────────────┘
            │ kind extraPortMappings: host 3300 → node 30300 (gitea)
            │                        host 5500 → node 30500 (registry)
   ┌────────┴────────┐
   │   CI runner     │  aicr bundle --output ./bundle --repo http://gitea...
   └─────────────────┘  git push --force http://aicr:…@localhost:3300/... main
```

- **Gitea** (`gitea/gitea:<pin>-rootless`, `.settings.yaml::
  testing_tools.gitea_image`) in the `aicr-registry` namespace —
  already on the cleanup allowlist, so it survives recipe-to-recipe
  like the registry. Rootless variant so the admin-user bootstrap is
  a plain `kubectl exec deploy/gitea -- gitea admin user create`.
- **Config is env-only** (`GITEA__section__KEY`): `INSTALL_LOCK`,
  sqlite3 on emptyDir, `ROOT_URL` = Service DNS, push-create user,
  public push-create, registration disabled.
- **Admin user** `aicr` / `aicr-kwok-ci` (overridable via
  `KWOK_GITEA_USER` / `KWOK_GITEA_PASSWORD`). A CI-only credential
  for an ephemeral in-cluster service — not a secret.
- **Flux install is unchanged** from the flux-oci lane
  (`install_flux()`, version pinned in `.settings.yaml`).

## Data Flow

Per `(recipe, flux-git)` matrix cell:

```
1. Infra (once per cell): install-infra.sh DEPLOYER=flux-git
   ├── install_registry   (unconditional, harmless if unused)
   ├── install_flux       (same as flux-oci)
   └── install_gitea      (Deployment+Service, healthz waits,
                           admin user bootstrap; exit codes 70/71/72)

2. Per-recipe: validate-scheduling.sh --deployer flux-git <recipe>
   ├── aicr bundle --recipe ... --deployer flux --output $WORK/bundle \
   │       --repo http://gitea.aicr-registry.svc.cluster.local:3000/aicr/<recipe>.git
   ├── assert kustomization.yaml AND sources/gitrepo-*.yaml exist
   │       (proves the bundler's git mode engaged — the lane's raison d'être)
   ├── git init -b main; add -A; commit; push --force \
   │       http://aicr:…@localhost:3300/aicr/<recipe>.git main
   ├── kubectl apply GitRepository aicr-bundle   (flux-system, branch main)
   ├── kubectl apply Kustomization aicr-<recipe> (sourceRef kind: GitRepository)
   ├── wait_for_flux_sync → chainsaw tests/chainsaw/kwok/flux-git-sync
   │       (--assert-timeout AND --error-timeout from KWOK_FLUX_SYNC_TIMEOUT)
   └── verify_pods (existing)

3. Cleanup: delete Kustomization (prune cascade) → outer GitRepository
   → best-effort sweep of remaining GitRepositories in flux-system
   (the inner sanitized-name source, if prune raced) → existing
   HelmRelease finalizer sweep + namespace force-finalize.
```

## Sync Gate: All-Resources Semantics

Implementing this lane surfaced a latent bug in the flux sync gate
(inherited from the #962 chainsaw migration): a chainsaw `assert` on
a resource **without `metadata.name` uses exists-semantics** — it
passes when ANY resource matches. The "all HelmReleases terminal"
step therefore passed the moment the first HelmRelease in a
`dependsOn` chain went Ready, while the other 12 were still blocked
(observed live on `eks-training`).

Fix (applied to `flux-sync` and `flux-git-sync` identically):

- A bare existence `assert` (≥1 HelmRelease must exist) so an empty
  bundle cannot pass vacuously, followed by
- an **`error:` op with the De Morgan negation** of the terminal-pass
  predicate. `error` polls until NO resource matches the bad state —
  the actual "all resources converged" contract.
- `spec.timeouts.error: 8m` in both tests and `--error-timeout` in
  the driver: chainsaw's error timeout is separate from assert and
  defaults to 30s, far below what a dependsOn chain needs.

Known follow-up: `argocd-sync`'s `assert-all-applications-pass` has
the same any-match weakness (its `operationState.phase=='Succeeded'`
gate from #1061 narrows but does not close it). Tracked separately —
changing it alters the behavior of existing argocd lanes.

## Files Changed

| File | Change |
|---|---|
| `.settings.yaml` | Pin `testing_tools.gitea_image` |
| `kwok/kind-config.yaml` | `extraPortMappings` host 3300 → NodePort 30300 |
| `kwok/scripts/install-infra.sh` | `install_gitea()`, `dump_gitea_diagnostics()`, `flux-git` case, exit codes 70/71/72 |
| `kwok/scripts/validate-scheduling.sh` | `flux-git` lane: bundle+push in `generate_bundle`, GitRepository/Kustomization wrappers in `deploy_bundle`, per-deployer chainsaw routing in `wait_for_flux_sync`, `flux-*` cleanup |
| `kwok/scripts/run-all-recipes.sh` | Allowlist `flux-git`; 3-strike rule widened to `flux-*`; exit-code docs |
| `tests/chainsaw/kwok/flux-git-sync/chainsaw-test.yaml` | New sibling sync gate (GitRepository source steps) |
| `tests/chainsaw/kwok/flux-sync/chainsaw-test.yaml` | SYNC NOTE header; all-HelmReleases gate fixed to error-polarity |
| `.github/workflows/kwok-recipes.yaml` | `flux-git` in Tier 1 matrix + Tier 3 deployer list |
| `.github/actions/kwok-test/action.yml` | `gitrepositories` + Gitea log in debug artifacts |
| `Makefile` | `kwok-test-deployer` help text |

No Go changes: the bundler's Git output shape
(`collectGitSources()`, `gitrepo-source.yaml.tmpl`, GitRepository
`sourceRef` on local-chart HelmReleases) already existed; this ADR
adds the CI lane that protects it.

## Error Handling and Failure Modes

| # | Failure | Detection | Remediation |
|---|---|---|---|
| 1 | Gitea Deployment not Ready | `kubectl rollout status` 120s → exit 70 | `dump_gitea_diagnostics` (describe + pod logs) |
| 2 | Gitea unreachable on host port | `curl /api/healthz` 60s → exit 71 | Almost always a cluster created before the 3300 mapping — recreate the Kind cluster |
| 3 | Admin user bootstrap failed | `gitea admin user create` non-zero and not "already exists" → exit 72 | Dump + fail fast; "already exists" is success (idempotent re-runs) |
| 4 | Bundler silently took OCI path | `sources/gitrepo-*.yaml` glob assert in `generate_bundle` | Fail fast — the lane must not pass without exercising the Git shape |
| 5 | `git push` fails | Captured combined output, non-zero exit | Logged verbatim; push-to-create + force-push make re-runs self-healing |
| 6 | Clone/reconcile failure | chainsaw `flux-git-sync` steps (GitRepository Ready first) | Per-step `catch` dumps sources, controller logs, Gitea log; exit 50 feeds the 3-strike rule |

## Acceptance Criteria

1. `make kwok-test-deployer RECIPE=eks-training DEPLOYER=flux-git`
   passes against a fresh Kind cluster, and a re-run of the same
   recipe passes (force-push idempotency).
2. The sync gate holds until **every** HelmRelease reaches the
   terminal-pass state (verified live: 13-release dependsOn chain no
   longer short-circuits on the first Ready release).
3. A deliberate regression in `gitrepo-source.yaml.tmpl` (e.g. wrong
   branch) turns the Tier 1 `flux-git` cell red.
4. `flux-oci` lane behavior is unchanged except for the
   strictly-stronger HelmRelease gate.

## References

- Issue [#963](https://github.com/NVIDIA/aicr/issues/963); parent [#843](https://github.com/NVIDIA/aicr/issues/843); PR #956 (OCI lanes)
- [ADR-008](008-kwok-deployer-matrix.md) — deployer matrix, OCI-first rationale this ADR extends
- Bundler Git sources: `pkg/bundler/deployer/flux/sources.go`, `pkg/bundler/deployer/flux/templates/gitrepo-source.yaml.tmpl`
- Sync gates: `tests/chainsaw/kwok/flux-sync/`, `tests/chainsaw/kwok/flux-git-sync/`
- Gitea push-to-create: <https://docs.gitea.com/administration/config-cheat-sheet#repository-repository>
