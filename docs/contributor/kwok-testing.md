# KWOK Deployer Matrix Testing

KWOK (Kubernetes WithOut Kubelet) simulates a GPU cluster without real
hardware so CI can validate scheduling shape — node selectors,
tolerations, resource requests — for every recipe. The **deployer
matrix** extends that coverage by re-running the same recipes through
three additional output adapters (`argocd-oci`, `argocd-helm-oci`,
`flux-oci`) in addition to the existing `helm` path. This catches
Argo CD / Flux template regressions and OCI-source compatibility
breaks that the `helm`-only lane could never see.

For the design rationale and the spike findings that justify the chart
pin and Repository-secret shape, see
[ADR-008](https://github.com/NVIDIA/aicr/blob/main/docs/design/008-kwok-deployer-matrix.md).
For the cluster-level KWOK setup (node profiles, recipe
auto-discovery), see [kwok/README.md](https://github.com/NVIDIA/aicr/blob/main/kwok/README.md).

## Deployer Coverage Matrix

| Tier | Trigger | Deployers exercised |
|------|---------|----------------------|
| Tier 1 — generic overlays | every PR + push | `helm`, `argocd-oci`, `argocd-helm-oci`, `flux-oci` |
| Tier 2 — diff-aware accelerator overlays | PR only, conditional on changed files | `helm` only |
| Tier 3 — full overlay set | push to `main` + nightly schedule | `helm`, `argocd-oci`, `argocd-helm-oci`, `flux-oci` |

### What each lane validates

| Lane | Pull artifact | Apply manifests | Reconcile to Ready |
|---|---|---|---|
| `helm` | n/a (filesystem) | ✅ (`helm install`) | ✅ pods scheduled |
| `argocd-oci` | ✅ (repo-server OCI pull) | ✅ (Argo CD sync) | ✅ `Synced+Healthy` (or documented partial-pass states) |
| `argocd-helm-oci` | ✅ (`helm pull` OCI) | ✅ (wrapper chart install) | ✅ `Synced+Healthy` (or documented partial-pass states) |
| `flux-oci` | ✅ (source-controller OCI pull) | ✅ (kustomize-controller apply) | ⏳ **deferred** — see [#964](https://github.com/NVIDIA/aicr/issues/964) |

`flux-oci` intentionally stops at "bundle applied + HelmRelease CRs created" because the bundler's flux deployer emits local-chart HelmReleases with `GitRepository` sources pointing at the placeholder URL `https://github.com/YOUR_ORG/YOUR_REPO.git`. Under OCI consumption helm-controller cannot fetch these charts, and Flux's `dependsOn` cascade then stalls every downstream HelmRelease. Issue [#964](https://github.com/NVIDIA/aicr/issues/964) tracks the proper fix (render local-chart manifests at bundle time OR push each local chart as its own OCI helm artifact).

For filesystem (Git-source) round-trip coverage of `argocd` / `flux`, see [#963](https://github.com/NVIDIA/aicr/issues/963).

Tier 2 stays `helm`-only because its job is to verify that an
accelerator-specific overlay still produces a correct bundle when the
diff touches that overlay's inputs. The deployer shape is orthogonal
to that question — re-running the same recipe under Argo CD would only
re-exercise template rendering, which Tier 1 and Tier 3 already cover
on the generic overlays.

## Running Locally

`make kwok-test-deployer` is the single entry point that mirrors what
CI runs per matrix cell. It expects a KWOK cluster to already exist.

```bash
unset GITLAB_TOKEN
make build
make kwok-cluster

# Single recipe + single deployer
make kwok-test-deployer RECIPE=eks-training DEPLOYER=argocd-oci
```

Valid `DEPLOYER` values: `helm`, `argocd-oci`, `argocd-helm-oci`,
`flux-oci`. The target invokes `kwok/scripts/run-all-recipes.sh
--deployer <name> <recipe>`, which calls `install-infra.sh` once with
`DEPLOYER` exported (in-cluster `registry:2` always; Argo CD for
`argocd-*`; Flux 2 controllers for `flux-oci`), then runs
`validate-scheduling.sh` for the recipe.

### Registry host port

The Kind cluster exposes the in-cluster `registry:2` Service on **host
port 5500** (`kwok/kind-config.yaml`'s `extraPortMappings`). The
unconventional choice avoids Apple ControlCenter (AirPlay / Handoff),
which listens on host port 5000 by default on macOS and would otherwise
fail `kind create cluster` with a port-bind error. Linux CI runners have
5500 free as well, so the same default works everywhere.

The in-cluster NodePort (`30500`) and the Service `containerPort`
(`5000`) are hardcoded and independent of the host-side mapping —
Argo CD's repo-server reaches the registry via Service DNS
(`registry.aicr-registry.svc.cluster.local:5000`) regardless.

Override `KWOK_REGISTRY_HOST_PORT` only when running against a
non-standard cluster topology (e.g., port 5500 is already in use on
your host). The variable adjusts the reachability probe; you still need
to update `kwok/kind-config.yaml`'s `hostPort` to match.

## Sweeping All Deployers Locally

`make kwok-test-all` still defaults to `helm` and there is no
matrix-aware make target — CI does the fan-out via the workflow
matrix. To reproduce the matrix locally, loop in shell:

```bash
for d in helm argocd-oci argocd-helm-oci flux-oci; do
  make kwok-test-deployer RECIPE=eks-training DEPLOYER="$d" || break
done
```

For the full recipe set under a single deployer, call the script
directly:

```bash
bash kwok/scripts/run-all-recipes.sh --deployer argocd-oci
```

## Failure Modes and Exit Codes

The three scripts emit distinct exit codes so callers (CI, the Make
target, local loops) can branch on failure mode rather than parsing
logs.

| Script | Code | Meaning |
|--------|------|---------|
| `install-infra.sh` | 10 | `yq` missing or required `.settings.yaml` field absent |
| `install-infra.sh` | 20 | Registry Deployment not Ready within 120 s |
| `install-infra.sh` | 21 | Registry not reachable on host port within 60 s |
| `install-infra.sh` | 30 | Argo CD Helm install failed |
| `install-infra.sh` | 31 | `applications.argoproj.io` CRD not Established within 120 s |
| `install-infra.sh` | 40 | Repository secret apply failed |
| `install-infra.sh` | 60 | Flux install manifest apply failed |
| `install-infra.sh` | 61 | Flux controller (source/kustomize/helm) not Ready within 180 s |
| `install-infra.sh` | 62 | Flux CRDs (OCIRepository/Kustomization/HelmRelease) not Established within 60 s |
| `validate-scheduling.sh` | 50 | GitOps sync deadline hit (`KWOK_ARGOCD_SYNC_TIMEOUT` for `argocd-*`, `KWOK_FLUX_SYNC_TIMEOUT` for `flux-oci`); only GitOps lanes can return 50 |
| `run-all-recipes.sh` | 50 | Three consecutive GitOps sync timeouts; tripped the ADR-008 3-strike rule and bailed the whole job |

Exit code 50 from `validate-scheduling.sh` is intentionally distinct
from generic non-zero exits: a sync-deadline timeout is qualitatively
different from a bundle-render failure or a scheduling-shape mismatch,
and the 3-strike rule in `run-all-recipes.sh` only counts code-50
strikes.

## Tuning the Sync Deadline

Four environment variables shape how long the GitOps lanes wait before
declaring a sync timeout. The Argo CD pair is independent of the Flux
pair so the two GitOps lanes can be tuned separately when a recipe has
deployer-specific reconciliation overhead.

| Variable | Default | Purpose |
|----------|---------|---------|
| `KWOK_ARGOCD_SYNC_TIMEOUT` | `300` (seconds) | Total deadline for all child Argo CD Applications to reach `Synced` + `Healthy` |
| `KWOK_ARGOCD_ROOT_GRACE` | `30` (seconds) | Grace period for the root Application to appear before deadline accounting starts |
| `KWOK_FLUX_SYNC_TIMEOUT` | `300` (seconds) | Total deadline for the outer `OCIRepository` to fetch + `Kustomization` to apply + `HelmRelease` CRs to be created. `flux-oci` does NOT wait for `HelmRelease` reconciliation to terminal state — see [issue #964](https://github.com/NVIDIA/aicr/issues/964) |
| `KWOK_FLUX_ROOT_GRACE` | `30` (seconds) | Grace period for the outer Kustomization to appear before deadline accounting starts |

On a clean local Kind cluster the Phase-0 spike observed
Synced+Healthy in roughly 30 seconds. CI runners are slower and
contended, so the 300-second default exists to absorb that variance.
If a local run trips code 50 but the cluster is otherwise healthy,
raise `KWOK_ARGOCD_SYNC_TIMEOUT` / `KWOK_FLUX_SYNC_TIMEOUT` before
assuming the recipe is broken — image pulls on a cold cluster are the
most common cause.

## Debugging CI Failures

When the `kwok-test` composite action fails, it uploads an artifact
named `kwok-debug-<recipe>-<deployer>-<run_id>` containing:

- `<cluster>-resources.txt` — `kubectl get all --all-namespaces`
- `<cluster>-nodes.txt` — `kubectl get nodes -o wide`
- `<cluster>-events.txt` — events sorted by `lastTimestamp`
- `<cluster>-pods.txt` — pod listing across all namespaces
- `<cluster>-argo-apps.yaml` — `applications.argoproj.io` YAML in `argocd` namespace (argocd lanes only)
- `<cluster>-argo-reposerver.log` — last 500 lines of `argocd-repo-server`
- `<cluster>-argo-appcontroller.log` — last 500 lines of `argocd-application-controller`
- `<cluster>-flux-resources.yaml` — YAML for `ocirepositories`, `kustomizations`, and `helmreleases` across all namespaces (flux-oci lane only)
- `<cluster>-flux-source-controller.log` — last 500 lines of `source-controller`
- `<cluster>-flux-kustomize-controller.log` — last 500 lines of `kustomize-controller`
- `<cluster>-flux-helm-controller.log` — last 500 lines of `helm-controller`
- `<cluster>-registry.log` — last 200 lines of the in-cluster `registry:2` Deployment

The repo-server log (Argo CD) and source-controller log (Flux) are
the first places to look for OCI-pull failures (plain-HTTP scheme
errors, mediaType mismatches). The application-controller (Argo CD)
and kustomize-controller (Flux) logs show reconciliation decisions
and prune behavior; helm-controller logs surface per-`HelmRelease`
install and upgrade outcomes.

## Adding a New Deployer Value

The deployer set is intentionally finite and matches what
`pkg/bundler` emits. To add a new value (say, `argocd-git`):

1. Add a `case` branch in `kwok/scripts/validate-scheduling.sh`'s
   `resolve_argocd_root_app()` (or the equivalent `resolve_flux_root_names()`
   if the new lane reconciles via Flux) mapping the new deployer to its
   root-resource name, plus any new branches in `generate_bundle` and
   `deploy_bundle`. Reuse the existing `argocd-oci` / `flux-oci` branches
   as templates.
2. Extend the `DEPLOYER` allowlist in `kwok/scripts/run-all-recipes.sh`
   (the `case "$DEPLOYER" in` block in `main()`).
3. Extend the `case "${DEPLOYER}"` branches in
   `kwok/scripts/install-infra.sh`'s `main()` so the right controller
   stack is installed for the new deployer.
4. Extend the `deployer:` input description in
   `.github/actions/kwok-test/action.yml` so callers see the new
   value in the input docs.
5. Add the value to the `deployer:` matrix in Tier 1 and Tier 3 of
   `.github/workflows/kwok-recipes.yaml`. Leave Tier 2 alone — the
   orthogonality rationale above still applies.
6. Add a row to the [Deployer Coverage Matrix](#deployer-coverage-matrix)
   table on this page so contributors can discover the new lane.

If the new value requires changes to the in-cluster infra (a different
registry, a different Argo CD chart, additional CRDs), update
`install-infra.sh` and pin any new versions in `.settings.yaml` rather
than hardcoding. The exit-code taxonomy in
[Failure Modes and Exit Codes](#failure-modes-and-exit-codes) is
contiguous — pick the next free code if a new distinct failure mode
appears.
