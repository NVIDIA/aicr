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
    echo ""
    log_info "========================================"
    log_info "Testing recipe: ${recipe} (deployer=${DEPLOYER})"
    log_info "========================================"

    cleanup_between_tests

    # Create nodes (pass recipe name, script infers from overlay)
    bash "${SCRIPT_DIR}/apply-nodes.sh" "${recipe}" || return 1

    # Run validation. Preserve validate-scheduling.sh's exit code so callers
    # can distinguish EXIT_ARGOCD_SYNC_TIMEOUT (50) from generic failures (1)
    # for the 3-strike rule.
    local rc=0
    bash "${SCRIPT_DIR}/validate-scheduling.sh" --deployer "${DEPLOYER}" "${recipe}" || rc=$?
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
    else
        recipes=$(get_recipes)
    fi

    log_info "Found $(echo "${recipes}" | wc -w | tr -d ' ') recipe(s) to test (deployer=${DEPLOYER})"
    ensure_cluster

    # Safety: refuse to start the cleanup sweep unless the kubectl
    # context points at a known KWOK Kind cluster. ensure_cluster has
    # just created/reused the cluster, but it does not validate the
    # context name — a developer running locally with a stale
    # KUBECONFIG could otherwise direct the upcoming force-finalize
    # sweep at a real cluster. Loose check only: kwok nodes don't
    # exist yet on the initial run (apply-nodes.sh runs per recipe
    # inside run_recipe_test).
    ensure_kwok_context_loose

    # Clean up any stale resources from previous runs
    cleanup_between_tests

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
            return 1
        fi
    fi

    # 3-strike rule for Argo CD sync timeouts (ADR-008 §"Error Handling and
    # Failure Modes"). Tracks CONSECUTIVE EXIT_ARGOCD_SYNC_TIMEOUT failures.
    # The counter resets on any rc != 50 (pass OR generic failure) so only
    # genuinely consecutive sync timeouts trip the bail. A pass-fail-pass-
    # timeout-timeout-timeout sequence is 3 consecutive; a timeout-pass-
    # timeout-timeout sequence is 2 consecutive.
    # Only argocd-* deployers can hit 50; helm path never trips this.
    local consecutive_sync_timeouts=0

    for recipe in ${recipes}; do
        local rc=0
        run_recipe_test "${recipe}" || rc=$?
        if (( rc == 0 )); then
            passed+=("${recipe}")
            consecutive_sync_timeouts=0
        else
            failed+=("${recipe}")
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
                    cleanup_between_tests
                    return "$EXIT_ARGOCD_SYNC_TIMEOUT"
                fi
            else
                # Any other failure (including a hypothetical rc=50 from a
                # path that shouldn't produce one) breaks the streak.
                consecutive_sync_timeouts=0
            fi
        fi
    done

    echo ""
    log_info "========================================"
    log_info "Results"
    log_info "========================================"
    for r in "${passed[@]:-}"; do [[ -n "$r" ]] && echo -e "  ${GREEN}✓${NC} $r"; done
    for r in "${failed[@]:-}"; do [[ -n "$r" ]] && echo -e "  ${RED}✗${NC} $r"; done

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
