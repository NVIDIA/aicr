#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Run all KWOK recipe tests sequentially in a shared cluster.
# Usage:
#   ./run-all-recipes.sh                           # Run all testable recipes (helm)
#   ./run-all-recipes.sh recipe1                   # Run specific recipe(s)
#   ./run-all-recipes.sh --deployer <name>         # Select deployer for all recipes
#   ./run-all-recipes.sh --deployer=<name> recipe1 # `=` form supported
#
# Flags:
#   --deployer <name>   Deployer to exercise for every recipe. One of:
#                         helm             (default — original Helm path)
#                         argocd-oci       (in-cluster registry + Argo CD
#                                           app-of-apps)
#                         argocd-helm-oci  (in-cluster registry + Argo CD
#                                           via OCI Helm chart wrapper)
#                         argocd-git       (in-cluster registry + Argo CD +
#                                           Gitea; filesystem bundle pushed
#                                           to Git, app-of-apps cloned and
#                                           reconciled from Git; issue #963)
#                         flux-oci         (in-cluster registry + Flux 2
#                                           OCIRepository → Kustomization →
#                                           HelmRelease)
#                         flux-git         (in-cluster registry + Flux 2 +
#                                           Gitea; filesystem bundle pushed
#                                           to Git, GitRepository →
#                                           Kustomization → HelmRelease;
#                                           issue #963)
#                       When != helm, install-infra.sh is run ONCE before the
#                       recipe loop with DEPLOYER exported so it installs the
#                       shared in-cluster OCI registry plus the GitOps
#                       controllers the selected deployer needs (Argo CD or
#                       Flux, plus Gitea for flux-git / argocd-git). Their owning
#                       namespaces (`aicr-registry`, `argocd`, `flux-system`)
#                       survive across recipe iterations.
#
# Exit codes:
#    0  all recipes passed
#    1  one or more recipes failed (non-sync-timeout); install-infra.sh failed;
#       or invalid arguments
#   50  Argo CD sync deadline hit on 3 consecutive recipes (3-strike rule,
#       ADR-008 §"Error Handling and Failure Modes"). Distinct so CI can
#       distinguish infra/controller-wide failure from per-recipe issues.
#       Mirrors validate-scheduling.sh's EXIT_ARGOCD_SYNC_TIMEOUT.

set -euo pipefail

# Mirrors validate-scheduling.sh's EXIT_ARGOCD_SYNC_TIMEOUT. Kept in sync by
# the matching documentation in both script headers.
readonly EXIT_ARGOCD_SYNC_TIMEOUT=50
# Max consecutive sync timeouts before bailing the whole job (ADR-008
# §"Error Handling and Failure Modes"). Only applied for argocd-* deployers.
readonly ARGOCD_SYNC_STRIKE_LIMIT=3

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KWOK_DIR="${SCRIPT_DIR}/.."
REPO_ROOT="${KWOK_DIR}/.."
OVERLAYS_DIR="${REPO_ROOT}/recipes/overlays"

# Shared cleanup helpers (SYSTEM_NS_PATTERN, ensure_kwok_context). Same
# source for validate-scheduling.sh so the system-ns allowlist regex
# lives in one place.
# shellcheck source=lib/cleanup.sh
source "${SCRIPT_DIR}/lib/cleanup.sh"

# Shared profile-selection helpers (resolve_recipe_criteria,
# select_profiles). Batch mode uses these to skip recipes with no
# matching KWOK profile instead of hard-failing on them (#1997).
# shellcheck source=lib/profile-select.sh
source "${SCRIPT_DIR}/lib/profile-select.sh"

# CTRF emitter shared with the UAT phase library (#1806). Every (recipe,
# deployer) cell of the KWOK matrix is recorded as one CTRF test in
# KWOK_RESULTS_FILE so the six-deployer lane has structured, per-deployer
# results rather than only log lines and an exit code. The kwok-test action
# uploads the file on every outcome.
# shellcheck source=tools/ctrf
source "${SCRIPT_DIR}/../../tools/ctrf"
KWOK_RESULTS_FILE="${KWOK_RESULTS_FILE:-/tmp/kwok-debug-artifacts/kwok-results.json}"

# Set by run_recipe_test when a recipe is skipped (no KWOK profile in implicit
# batch mode) so the CTRF record says skipped rather than passed.
LAST_RECIPE_SKIPPED=false

# kwok_ctrf_finalize is the EXIT trap armed by main from CTRF init to the end
# of the run, so a report exists on every exit path: a set -e abort or exit 1
# during setup, a `return` from main, and TERM/INT (routed here through
# kwok_on_signal). On a non-zero exit with the report not yet final it records
# whichever unit was in flight as one CTRF entry with status "other" (setup ->
# kwok/setup/<deployer>; a recipe -> its cell, message "interrupted") and
# writes the report. Report-write failures are logged and never change the
# exit status. The original exit status is preserved either way.
KWOK_SETUP_STAGE=""
KWOK_SETUP_START=""
KWOK_ACTIVE_RECIPE=""
KWOK_ACTIVE_START=""
KWOK_REPORT_FINAL=false
KWOK_SIGNAL=""
kwok_ctrf_finalize() {
    local rc=$?
    trap - EXIT
    if [[ "${KWOK_REPORT_FINAL}" != true ]]; then
        if (( rc != 0 )); then
            local why="rc=${rc}"
            [[ -n "${KWOK_SIGNAL}" ]] && why="interrupted by SIG${KWOK_SIGNAL} (rc=${rc})"
            if [[ -n "${KWOK_ACTIVE_RECIPE}" ]]; then
                ctrf_add "kwok/${KWOK_ACTIVE_RECIPE}/${DEPLOYER}" other \
                    "$(ctrf_elapsed_ms "${KWOK_ACTIVE_START}")" "recipe test ${why}" || true
            elif [[ -n "${KWOK_SETUP_STAGE}" ]]; then
                ctrf_add "kwok/setup/${DEPLOYER}" other "$(ctrf_elapsed_ms "${KWOK_SETUP_START}")" \
                    "KWOK ${KWOK_SETUP_STAGE} setup failed (${why})" || true
            fi
        fi
        kwok_ctrf_flush
    fi
    exit "${rc}"
}

# kwok_ctrf_flush writes the accumulated report. A failed write (unwritable
# path, full disk, jq error) is a telemetry problem: it is logged and the
# caller's status is left alone, so it can never skip cleanup or replace the
# outcome of the run.
kwok_ctrf_flush() {
    if ctrf_write "${KWOK_RESULTS_FILE}"; then
        log_info "CTRF results written to ${KWOK_RESULTS_FILE}"
    else
        log_error "failed to write CTRF results to ${KWOK_RESULTS_FILE}; continuing"
    fi
}

# kwok_on_signal turns TERM/INT into an exit so kwok_ctrf_finalize runs (a
# fatal signal would otherwise skip the EXIT trap). It stops the recipe
# subprocess first so `wait` returns and the finalizer is not deferred until
# the child ends on its own. Exit status is 128 + signal, as the shell would
# have reported.
KWOK_CHILD_PID=""
kwok_on_signal() {
    local sig="$1" num="$2"
    trap - TERM INT
    KWOK_SIGNAL="${sig}"
    if [[ -n "${KWOK_CHILD_PID}" ]]; then
        kill -"${sig}" "${KWOK_CHILD_PID}" 2>/dev/null || true
    fi
    exit $(( 128 + num ))
}


CLUSTER_NAME="${KWOK_CLUSTER:-aicr-kwok-test}"
CONTEXT="kind-${CLUSTER_NAME}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

retry_command() {
    local description="$1"
    shift

    # 5 attempts with doubling backoff: 5+10+20+40 = 75s cumulative sleep.
    # The prior default (3 attempts → 15s cumulative) was not enough headroom
    # for transient GitHub releases CDN 502s on kwok-stage-fast chart fetch,
    # which observably persist 30-60s. Overridable via env for local tuning.
    local max_attempts="${KWOK_COMMAND_RETRIES:-5}"
    local delay="${KWOK_COMMAND_RETRY_DELAY:-5}"
    local attempt=1

    while true; do
        if "$@"; then
            return 0
        fi

        if ((attempt >= max_attempts)); then
            log_error "${description} failed after ${attempt} attempt(s)"
            return 1
        fi

        log_warn "${description} failed (attempt ${attempt}/${max_attempts}); retrying in ${delay}s..."
        sleep "${delay}"
        attempt=$((attempt + 1))
        delay=$((delay * 2))
    done
}

# Find recipes with service criteria (testable cloud configurations)
get_recipes() {
    for overlay in "${OVERLAYS_DIR}"/*.yaml; do
        local name service
        name=$(basename "$overlay" .yaml)
        service=$(yq eval '.spec.criteria.service // ""' "$overlay" 2>/dev/null)

        # Skip non-testable overlays (no service, or OCP — needs OpenShift operators)
        if [[ -n "$service" && "$service" != "null" && "$service" != "any" && "$service" != "ocp" ]]; then
            echo "$name"
        fi
    done | sort
}

ensure_cluster() {
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        log_info "Reusing existing cluster: ${CLUSTER_NAME}"
    else
        log_info "Creating cluster: ${CLUSTER_NAME}"
        kind create cluster \
            --name "${CLUSTER_NAME}" \
            --image "${KIND_NODE_IMAGE:-$(yq -r '.testing.kind_node_image' "${REPO_ROOT}/.settings.yaml" 2>/dev/null || echo "kindest/node:v1.32.0")}" \
            --config "${KWOK_DIR}/kind-config.yaml" \
            --wait 60s
    fi

    kubectl config use-context "${CONTEXT}"
    kubectl wait --for=condition=Ready node --all --timeout=60s

    if ! kubectl get deployment -n kube-system kwok-controller &>/dev/null; then
        log_info "Installing KWOK controller..."
        retry_command "Adding KWOK Helm repository" \
            helm repo add kwok https://kwok.sigs.k8s.io/charts/ --force-update
        retry_command "Installing KWOK controller" \
            helm upgrade --install kwok-controller kwok/kwok \
            --namespace kube-system --set hostNetwork=true --wait
        retry_command "Installing KWOK stage-fast" \
            helm upgrade --install kwok-stage-fast kwok/stage-fast --namespace kube-system
    fi

    # Patch kindnet to exclude KWOK nodes
    if kubectl get daemonset -n kube-system kindnet &>/dev/null; then
        kubectl patch daemonset -n kube-system kindnet --type=json -p='[
            {"op": "add", "path": "/spec/template/spec/affinity", "value": {
                "nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {
                    "nodeSelectorTerms": [{"matchExpressions": [{"key": "type", "operator": "NotIn", "values": ["kwok"]}]}]
                }}
            }}
        ]' 2>/dev/null || true
    fi
}

cleanup_between_tests() {
    log_info "Cleaning up for next test..."

    # Delete KWOK nodes (validate-scheduling.sh EXIT trap handles Helm/ns cleanup,
    # but nodes are managed by run-all-recipes.sh)
    kubectl delete nodes -l type=kwok --ignore-not-found --force --grace-period=0 2>/dev/null || true

    # Clean up orphaned CRDs from cert-manager (cluster-scoped, not cleaned by ns delete)
    kubectl delete crd -l app.kubernetes.io/instance=aicr-test --ignore-not-found 2>/dev/null || true

    # Force-finalize any still-terminating namespaces before the next recipe.
    # `argocd`, `flux-system`, and `aicr-registry` are owned by install-
    # infra.sh and MUST survive between recipes for the GitOps deployer
    # lanes — installing them once and reusing avoids 5+ minutes of Helm
    # install + CRD Established + controller-ready overhead per recipe.
    # The three namespaces are listed unconditionally (even under --deployer
    # helm) because they simply won't exist on the helm path, so the
    # allowlist is harmless there. Do not gate by DEPLOYER.
    #
    # See the long comment in validate-scheduling.sh cleanup() for the
    # rationale: KWOK cannot run real controller finalizers, so
    # `kubectl wait --for=delete` against a Terminating namespace blocks
    # the full --timeout=120s per namespace. Recipes touch ~10 namespaces;
    # the previous version spent ~20 minutes of cleanup between recipes
    # while doing zero useful work. Force-finalize bypasses the protocol
    # by PATCHing spec.finalizers to []; safe in the ephemeral KWOK
    # cluster (no real workloads to leak; cluster destroyed at job end).
    local system_ns="${SYSTEM_NS_PATTERN}"  # see SYSTEM_NS_PATTERN in lib/cleanup.sh

    # Two-phase cleanup, matching validate-scheduling.sh::cleanup_old_tests:
    # the previous version only iterated namespaces ALREADY in
    # status.phase=Terminating, which silently missed `Active` namespaces
    # carrying a `deletionTimestamp` (Helm hook resources mid-uninstall,
    # for example) and let them collide on the next recipe.
    #
    # Phase 1 — issue async deletes for every non-system namespace so the
    # controller starts tearing down. Phase 2 — sleep briefly, then
    # force-finalize anything still present. The two phases together
    # match the in-recipe cleanup so the between-test path can't drift
    # away from it over time.
    local test_namespaces
    test_namespaces=$(kubectl get ns -o jsonpath='{.items[*].metadata.name}' 2>/dev/null \
        | tr ' ' '\n' \
        | { grep -vE "^(${system_ns})$" || true; })
    if [[ -z "$test_namespaces" ]]; then
        return 0
    fi

    for ns in $test_namespaces; do
        kubectl delete ns "$ns" --ignore-not-found --wait=false 2>/dev/null || true
    done

    # Brief grace for graceful deletion (real finalizers run in well
    # under a second when there are no real workloads).
    sleep 2
    for ns in $test_namespaces; do
        if kubectl get ns "$ns" >/dev/null 2>&1; then
            log_info "Force-finalizing stuck namespace: $ns"
            kubectl get ns "$ns" -o json 2>/dev/null \
                | jq '.spec.finalizers = [] | .metadata.finalizers = []' \
                | kubectl replace --raw "/api/v1/namespaces/${ns}/finalize" -f - >/dev/null 2>&1 \
                || true
        fi
    done
}

run_recipe_test() {
    local recipe="$1"
    LAST_RECIPE_SKIPPED=false
    echo ""
    log_info "========================================"
    log_info "Testing recipe: ${recipe} (deployer=${DEPLOYER})"
    log_info "========================================"

    # Skip recipes whose (service, accelerator) has no matching KWOK
    # profile on disk. Direct `apply-nodes.sh <recipe>` still fails
    # closed with the full diagnostic (#1997).
    #
    # Only PROFILE_SELECT_RC_NO_MATCH (rc=2 from select_profiles) is
    # potentially skippable. Every other non-zero rc — ambiguous matches,
    # invalid profiles root, malformed args — is a real fault the tree
    # must surface; swallowing it would let a duplicate profile pass
    # batch CI green, which is the false-pass class this PR exists to
    # eliminate.
    #
    # For the no-match case the invocation mode decides. Implicit batch
    # mode (get_recipes(), used by `make kwok-test-all` locally) keeps
    # the historical SKIP-as-pass semantics so a partly-populated
    # profile tree doesn't turn every dev-loop invocation red. Explicit
    # invocation — the caller named this recipe on the command line, as
    # every CI matrix cell does — fails: reporting green for a recipe
    # the operator specifically asked about is the exact false-success
    # the CI discovery filter also targets, and this is defense in
    # depth if a caller ever bypasses that filter.
    local overlay_file="${OVERLAYS_DIR}/${recipe}.yaml"
    if [[ -f "${overlay_file}" ]]; then
        local criteria svc accel
        if criteria=$(resolve_recipe_criteria "${overlay_file}" 2>/dev/null); then
            read -r svc accel <<< "${criteria}"
            local sel_err select_rc=0
            sel_err=$(select_profiles "${svc}" "${accel}" "${KWOK_DIR}/profiles" 2>&1 >/dev/null) || select_rc=$?
            if (( select_rc == PROFILE_SELECT_RC_NO_MATCH )); then
                if is_explicit_recipe "${recipe}"; then
                    log_error "Recipe ${recipe} was explicitly requested but has no KWOK profile for service=${svc} accelerator=${accel}. Add a profile under kwok/profiles/${svc}/ or drop this recipe from the invocation (see #1997)."
                    return 1
                fi
                log_warn "SKIP ${recipe}: no KWOK profile for service=${svc} accelerator=${accel} (add one under kwok/profiles/${svc}/ — see #1997)"
                LAST_RECIPE_SKIPPED=true
                return 0
            fi
            if (( select_rc != 0 )); then
                log_error "Profile selection failed for ${recipe} (service=${svc} accelerator=${accel}, rc=${select_rc}):"
                [[ -n "${sel_err}" ]] && echo "${sel_err}" >&2
                return "${select_rc}"
            fi
        fi
    fi

    cleanup_between_tests

    # Create nodes (pass recipe name, script infers from overlay)
    bash "${SCRIPT_DIR}/apply-nodes.sh" "${recipe}" || return 1

    # Run validation. Preserve validate-scheduling.sh's exit code so callers
    # can distinguish EXIT_ARGOCD_SYNC_TIMEOUT (50) from generic failures (1)
    # for the 3-strike rule.
    # Background + wait (rather than a foreground child) so a TERM/INT to this
    # script is handled at once by kwok_on_signal instead of being deferred
    # until validate-scheduling.sh finishes on its own.
    local rc=0
    bash "${SCRIPT_DIR}/validate-scheduling.sh" --deployer "${DEPLOYER}" "${recipe}" &
    KWOK_CHILD_PID=$!
    wait "${KWOK_CHILD_PID}" || rc=$?
    KWOK_CHILD_PID=""
    return "$rc"
}

# Print usage to stderr.
usage() {
    cat >&2 <<'EOF'
Usage: run-all-recipes.sh [--deployer <name>] [recipe1 recipe2 ...]

Flags:
  --deployer <name>   Deployer to exercise for every recipe. One of:
                        helm (default), argocd-oci, argocd-helm-oci, argocd-git,
                        flux-oci, flux-git

Examples:
  run-all-recipes.sh
  run-all-recipes.sh eks-training
  run-all-recipes.sh --deployer argocd-oci eks-training
  run-all-recipes.sh --deployer=argocd-helm-oci
  run-all-recipes.sh --deployer argocd-git eks-training
  run-all-recipes.sh --deployer flux-oci eks-training
  run-all-recipes.sh --deployer flux-git eks-training
EOF
}

# Deployer selection (issue #843). Set by --deployer in main(); default helm.
# Under --deployer helm, this script's behavior is byte-identical to pre-#843
# except the cleanup_between_tests system_ns allowlist gains two harmless
# entries (argocd|aicr-registry) that never exist on the helm path.
DEPLOYER="helm"

# Space-separated list of recipes the caller explicitly named on the
# command line. Populated in main() from positional args. Empty when the
# recipe set came from get_recipes() (implicit batch mode). Used by
# run_recipe_test to decide whether an unmapped-profile SKIP is a
# recoverable batch outcome (rc=0) or a false-success that must be
# surfaced as a failure (#1997): CI matrix cells always name a specific
# recipe, so an unmapped recipe reaching them means every prior filter
# was bypassed and the run has no coverage to report green about.
EXPLICIT_RECIPES=""

is_explicit_recipe() {
    local recipe="$1"
    [[ " ${EXPLICIT_RECIPES} " == *" ${recipe} "* ]]
}

main() {
    local recipes failed=() passed=()
    local positional=()

    # Parse flags + positional recipe names. Supports `--deployer X` and
    # `--deployer=X`. Anything else with a leading `-` is rejected.
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --deployer)
                if [[ $# -lt 2 ]]; then
                    log_error "--deployer requires a value"
                    usage
                    return 1
                fi
                DEPLOYER="$2"
                shift 2
                ;;
            --deployer=*)
                DEPLOYER="${1#--deployer=}"
                shift
                ;;
            -h|--help)
                usage
                return 0
                ;;
            -*)
                log_error "Unknown option: $1"
                usage
                return 1
                ;;
            *)
                positional+=("$1")
                shift
                ;;
        esac
    done

    case "$DEPLOYER" in
        helm|argocd-oci|argocd-helm-oci|argocd-git|flux-oci|flux-git)
            ;;
        *)
            log_error "Invalid --deployer value: '${DEPLOYER}'"
            log_error "Must be one of: helm, argocd-oci, argocd-helm-oci, argocd-git, flux-oci, flux-git"
            usage
            return 1
            ;;
    esac

    if [[ ${#positional[@]} -gt 0 ]]; then
        recipes="${positional[*]}"
        EXPLICIT_RECIPES="${positional[*]}"
    else
        recipes=$(get_recipes)
    fi

    log_info "Found $(echo "${recipes}" | wc -w | tr -d ' ') recipe(s) to test (deployer=${DEPLOYER})"
    # jq is what tools/ctrf serializes with; fail fast here rather than 127 out
    # of the recipe loop with no results file.
    if ! command -v jq >/dev/null 2>&1; then
        log_error "jq is required to write ${KWOK_RESULTS_FILE}; install it and re-run"
        return 1
    fi
    # Never leave a previous run's results behind. Non-fatal: an unwritable
    # report path is a telemetry problem and must not decide the run.
    rm -f "${KWOK_RESULTS_FILE}" 2>/dev/null || true
    # The CTRF report starts before cluster/infra setup so a setup failure still
    # leaves a report: one kwok/setup/<deployer> entry with status "other"
    # (CTRF's "neither passed nor failed" bucket) and no per-recipe cells.
    #
    # Setup keeps its fail-fast semantics exactly as before: ensure_cluster's
    # commands abort the script under set -e and ensure_kwok_context_loose
    # exits 1 on a foreign context. Capturing either in an `|| rc=$?` list
    # would silence errexit INSIDE the function and let it return 0 after an
    # internal failure, so the record is written by an EXIT trap instead,
    # which fires on every one of those exit paths and preserves the exit
    # status. It is cleared once setup completes and the recipe loop owns
    # the report.
    ctrf_init "kwok-validate-scheduling"
    KWOK_SETUP_STAGE="cluster"
    KWOK_SETUP_START="$(ctrf_now_ms)"
    trap 'kwok_ctrf_finalize' EXIT
    trap 'kwok_on_signal TERM 15' TERM
    trap 'kwok_on_signal INT 2' INT

    ensure_cluster

    # Safety: refuse to start the cleanup sweep unless the kubectl
    # context points at a known KWOK Kind cluster. ensure_cluster has
    # just created/reused the cluster, but it does not validate the
    # context name — a developer running locally with a stale
    # KUBECONFIG could otherwise direct the upcoming force-finalize
    # sweep at a real cluster. Loose check only: kwok nodes don't
    # exist yet on the initial run (apply-nodes.sh runs per recipe
    # inside run_recipe_test).
    KWOK_SETUP_STAGE="context"
    ensure_kwok_context_loose

    # Clean up any stale resources from previous runs
    cleanup_between_tests
    KWOK_SETUP_STAGE="infra"

    # Install shared in-cluster registry + the controller(s) the selected
    # deployer needs (Argo CD for argocd-*, Flux for flux-*, plus Gitea for
    # the Git-source lanes flux-git / argocd-git). install-infra.sh is
    # idempotent but unnecessary work per
    # recipe; a failure here is fatal — the lane cannot run without it.
    # DEPLOYER is exported so install-infra.sh can branch on it. Its exit
    # code map: 10=yq/settings, 20=registry Deployment not Ready,
    # 21=registry not reachable on host port, 30=Argo CD Helm install
    # failed, 31=Application CRD not Established, 40=Repository secret
    # apply failed, 60=Flux install manifest apply failed, 61=Flux
    # controller not Ready, 62=Flux CRDs not Established, 70=Gitea
    # Deployment not Ready, 71=Gitea not reachable on host port, 72=Gitea
    # admin user bootstrap failed. Surface the raw rc in the log line so
    # an operator can map it.
    if [[ "${DEPLOYER}" != "helm" ]]; then
        log_info "Installing shared infra (in-cluster registry + controllers for ${DEPLOYER})..."
        local infra_rc=0
        DEPLOYER="${DEPLOYER}" bash "${SCRIPT_DIR}/install-infra.sh" || infra_rc=$?
        if (( infra_rc != 0 )); then
            log_error "install-infra.sh failed (exit code ${infra_rc}); cannot run ${DEPLOYER} deployer lane"
            log_error "See kwok/scripts/install-infra.sh header for exit-code taxonomy"
            KWOK_SETUP_STAGE="infra (install-infra.sh rc=${infra_rc}; see its header for the exit-code taxonomy)"
            return 1   # the EXIT trap writes the kwok/setup record
        fi
    fi
    KWOK_SETUP_STAGE=""   # setup complete; from here the active recipe is what an interruption records

    # 3-strike rule for Argo CD sync timeouts (ADR-008 §"Error Handling and
    # Failure Modes"). Tracks CONSECUTIVE EXIT_ARGOCD_SYNC_TIMEOUT failures.
    # The counter resets on any rc != 50 (pass OR generic failure) so only
    # genuinely consecutive sync timeouts trip the bail. A pass-fail-pass-
    # timeout-timeout-timeout sequence is 3 consecutive; a timeout-pass-
    # timeout-timeout sequence is 2 consecutive.
    # Only argocd-* deployers can hit 50; helm path never trips this.
    local consecutive_sync_timeouts=0

    for recipe in ${recipes}; do
        local rc=0 recipe_start
        recipe_start="$(ctrf_now_ms)"
        KWOK_ACTIVE_RECIPE="${recipe}"
        KWOK_ACTIVE_START="${recipe_start}"
        run_recipe_test "${recipe}" || rc=$?
        KWOK_ACTIVE_RECIPE=""
        if (( rc == 0 )); then
            passed+=("${recipe}")
            consecutive_sync_timeouts=0
            if [[ "${LAST_RECIPE_SKIPPED}" == true ]]; then
                ctrf_add "kwok/${recipe}/${DEPLOYER}" skipped "$(ctrf_elapsed_ms "${recipe_start}")" \
                    "no KWOK profile for this recipe (implicit batch mode)"
            else
                ctrf_add "kwok/${recipe}/${DEPLOYER}" passed "$(ctrf_elapsed_ms "${recipe_start}")"
            fi
        else
            failed+=("${recipe}")
            if (( rc == EXIT_ARGOCD_SYNC_TIMEOUT )); then
                ctrf_add "kwok/${recipe}/${DEPLOYER}" failed "$(ctrf_elapsed_ms "${recipe_start}")" \
                    "GitOps sync deadline exceeded (rc=${rc})"
            else
                ctrf_add "kwok/${recipe}/${DEPLOYER}" failed "$(ctrf_elapsed_ms "${recipe_start}")" \
                    "recipe test failed (rc=${rc}); see the log for the failing stage"
            fi
            # 3-strike rule is GitOps-only: helm path never returns 50, so
            # the gate is currently implicit. Make it explicit by checking
            # DEPLOYER too — keeps the contract auditable from this site
            # alone instead of a chain of "but only X can produce 50"
            # comments scattered across files.
            if (( rc == EXIT_ARGOCD_SYNC_TIMEOUT )) \
                    && [[ "$DEPLOYER" == argocd-* || "$DEPLOYER" == flux-* ]]; then
                consecutive_sync_timeouts=$(( consecutive_sync_timeouts + 1 ))
                log_warn "GitOps sync timeout strike ${consecutive_sync_timeouts}/${ARGOCD_SYNC_STRIKE_LIMIT} on recipe ${recipe}"
                if (( consecutive_sync_timeouts >= ARGOCD_SYNC_STRIKE_LIMIT )); then
                    log_error "========================================"
                    log_error "3-strike rule tripped: ${consecutive_sync_timeouts} consecutive GitOps sync timeouts"
                    log_error "Bailing remainder of recipe loop (partial coverage > no coverage is false here)"
                    log_error "Failed recipes so far:"
                    for r in "${failed[@]:-}"; do [[ -n "$r" ]] && log_error "  - ${r}"; done
                    log_error "========================================"
                    KWOK_REPORT_FINAL=true
                    kwok_ctrf_flush
                    cleanup_between_tests
                    return "$EXIT_ARGOCD_SYNC_TIMEOUT"
                fi
            else
                # Any other failure (including a hypothetical rc=50 from a
                # path that shouldn't produce one) breaks the streak.
                consecutive_sync_timeouts=0
            fi
        fi
        # Flush after every cell so completed cells survive a hard kill that
        # bypasses the traps (SIGKILL, runner teardown).
        kwok_ctrf_flush
    done

    echo ""
    log_info "========================================"
    log_info "Results"
    log_info "========================================"
    for r in "${passed[@]:-}"; do [[ -n "$r" ]] && echo -e "  ${GREEN}✓${NC} $r"; done
    for r in "${failed[@]:-}"; do [[ -n "$r" ]] && echo -e "  ${RED}✗${NC} $r"; done
    KWOK_REPORT_FINAL=true
    kwok_ctrf_flush

    cleanup_between_tests

    if [[ ${#failed[@]} -eq 0 ]]; then
        log_info "All ${#passed[@]} recipe(s) passed!"
        log_info "Cluster preserved. Delete with: kind delete cluster --name ${CLUSTER_NAME}"
        return 0
    else
        log_error "${#failed[@]} recipe(s) failed"
        return 1
    fi
}

main "$@"
