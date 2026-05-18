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
#   ./run-all-recipes.sh           # Run all testable recipes
#   ./run-all-recipes.sh recipe1   # Run specific recipe(s)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KWOK_DIR="${SCRIPT_DIR}/.."
REPO_ROOT="${KWOK_DIR}/.."
OVERLAYS_DIR="${REPO_ROOT}/recipes/overlays"

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

    local max_attempts="${KWOK_COMMAND_RETRIES:-3}"
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

        # Only include recipes with a service (eks, gke, etc.)
        if [[ -n "$service" && "$service" != "null" && "$service" != "any" ]]; then
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

    # Wait for any still-terminating namespaces before next recipe
    local system_ns="default|kube-node-lease|kube-public|kube-system|kwok-system|local-path-storage"
    local terminating
    terminating=$(kubectl get ns -o jsonpath='{range .items[?(@.status.phase=="Terminating")]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -vE "^(${system_ns})$" || true)
    for ns in $terminating; do
        log_info "Waiting for namespace $ns to terminate..."
        kubectl wait --for=delete "ns/$ns" --timeout=120s 2>/dev/null || true
    done
}

run_recipe_test() {
    local recipe="$1"
    echo ""
    log_info "========================================"
    log_info "Testing recipe: ${recipe}"
    log_info "========================================"

    cleanup_between_tests

    # Create nodes (pass recipe name, script infers from overlay)
    bash "${SCRIPT_DIR}/apply-nodes.sh" "${recipe}" || return 1

    # Run validation
    bash "${SCRIPT_DIR}/validate-scheduling.sh" "${recipe}" || return 1
}

main() {
    local recipes failed=() passed=()

    if [[ $# -gt 0 ]]; then
        recipes="$*"
    else
        recipes=$(get_recipes)
    fi

    log_info "Found $(echo "${recipes}" | wc -w | tr -d ' ') recipe(s) to test"
    ensure_cluster

    # Clean up any stale resources from previous runs
    cleanup_between_tests

    for recipe in ${recipes}; do
        if run_recipe_test "${recipe}"; then
            passed+=("${recipe}")
        else
            failed+=("${recipe}")
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
