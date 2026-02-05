#!/usr/bin/env bash
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

# Run all KWOK recipe tests in parallel across multiple Kind clusters.
# Usage:
#   ./run-all-recipes-parallel.sh              # Auto-detect parallelism
#   ./run-all-recipes-parallel.sh 4            # Use 4 clusters
#   PARALLEL=4 ./run-all-recipes-parallel.sh   # Same as above

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KWOK_DIR="${SCRIPT_DIR}/.."
REPO_ROOT="${KWOK_DIR}/.."
OVERLAYS_DIR="${REPO_ROOT}/pkg/recipe/data/overlays"

CLUSTER_PREFIX="${KWOK_CLUSTER_PREFIX:-eidos-kwok-test}"
KWOK_VERSION="${KWOK_VERSION:-$(yq -r '.testing_tools.kwok' "${REPO_ROOT}/.versions.yaml" 2>/dev/null || echo "v0.7.0")}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.32.0}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }
log_debug() { echo -e "${BLUE}[DEBUG]${NC} $*"; }

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

# Check dependencies
check_deps() {
    local missing=()
    for cmd in kubectl helm yq kind docker; do
        if ! command -v "$cmd" &>/dev/null; then
            missing+=("$cmd")
        fi
    done

    if [[ ${#missing[@]} -gt 0 ]]; then
        log_error "Missing required tools: ${missing[*]}"
        exit 1
    fi

    # Check if docker is running
    if ! docker info &>/dev/null; then
        log_error "Docker is not running or not accessible"
        log_error "Start Docker Desktop or check permissions"
        exit 1
    fi
}

# Create a single KWOK cluster
create_cluster() {
    local cluster_name="$1"
    local context="kind-${cluster_name}"
    local log_file="${WORK_DIR}/${cluster_name}-create.log"

    log_info "[$cluster_name] Creating cluster..."

    # Use Kind directly instead of ctlptl for better compatibility
    # Create Kind config on the fly
    local kind_config
    kind_config=$(mktemp)
    trap "rm -f '$kind_config'" RETURN

    cat > "$kind_config" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  DynamicResourceAllocation: true
runtimeConfig:
  "resource.k8s.io/v1beta1": "true"
nodes:
  - role: control-plane
    image: ${KIND_NODE_IMAGE}
    kubeadmConfigPatches:
      - |
        kind: InitConfiguration
        skipPhases:
          - addon/coredns
        nodeRegistration:
          ignorePreflightErrors:
            - SystemVerification
          taints:
            - key: "node-role.kubernetes.io/control-plane"
              effect: "NoSchedule"
EOF

    # Create cluster with kind
    if ! kind create cluster --name "$cluster_name" --config "$kind_config" --wait 60s &>"$log_file"; then
        log_error "[$cluster_name] Failed to create cluster. Check: $log_file"
        return 1
    fi

    # Wait for nodes
    if ! kubectl --context="$context" wait --for=condition=Ready node --all --timeout=60s &>>"$log_file"; then
        log_error "[$cluster_name] Nodes not ready. Check: $log_file"
        return 1
    fi

    # Install KWOK controller
    log_info "[$cluster_name] Installing KWOK controller..."
    if ! kubectl --context="$context" apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwok.yaml" &>>"$log_file"; then
        log_error "[$cluster_name] Failed to install KWOK. Check: $log_file"
        return 1
    fi

    if ! kubectl --context="$context" apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/stage-fast.yaml" &>>"$log_file"; then
        log_error "[$cluster_name] Failed to install KWOK stages. Check: $log_file"
        return 1
    fi

    # Wait for KWOK controller with longer timeout for parallel creation
    if ! kubectl --context="$context" wait --for=condition=Available deployment/kwok-controller -n kube-system --timeout=300s &>>"$log_file"; then
        log_error "[$cluster_name] KWOK controller not ready. Check: $log_file"
        # Show last 10 lines of log for immediate debugging
        echo "  Last 10 lines of $log_file:" >&2
        tail -10 "$log_file" | sed 's/^/    /' >&2
        return 1
    fi

    # Taint control-plane to prevent ANY workload from scheduling there
    # Use a custom taint that no component tolerates, since some operators tolerate control-plane taint
    # TODO: The bundler should apply --system-node-selector to ALL system components, not just some.
    #       Once fixed, this extra taint can be removed.
    kubectl --context="$context" taint nodes -l node-role.kubernetes.io/control-plane \
        node-role.kubernetes.io/control-plane:NoSchedule \
        eidos.nvidia.com/kwok-test=true:NoSchedule \
        --overwrite &>>"$log_file" || true

    log_info "[$cluster_name] Cluster ready"
}

# Delete a single cluster
delete_cluster() {
    local cluster_name="$1"

    # Check if cluster exists before trying to delete
    if kind get clusters 2>/dev/null | grep -q "^${cluster_name}$"; then
        log_debug "[$cluster_name] Deleting cluster..."
        kind delete cluster --name "$cluster_name" 2>/dev/null || true
    fi
}

# Run test on a specific cluster
run_test_on_cluster() {
    local cluster_name="$1"
    local recipe="$2"
    local context="kind-${cluster_name}"
    local log_file="${WORK_DIR}/${cluster_name}-${recipe}.log"

    log_info "[$cluster_name] Testing recipe: ${recipe}"

    # Create a unique kubeconfig for this test to avoid context race conditions
    local test_kubeconfig="${WORK_DIR}/${cluster_name}.kubeconfig"
    kind get kubeconfig --name "$cluster_name" > "$test_kubeconfig" 2>/dev/null

    # Export environment for child scripts
    export KUBECONFIG="$test_kubeconfig"
    export KWOK_CLUSTER="${cluster_name}"
    export KWOK_NAMESPACE="eidos-kwok-test-${recipe}"
    export KWOK_RELEASE="eidos-test-${recipe}"

    # Create nodes
    if ! bash "${SCRIPT_DIR}/apply-nodes.sh" "${recipe}" &>"$log_file"; then
        log_error "[$cluster_name] Failed to create nodes for ${recipe}"
        echo "FAILED" > "${WORK_DIR}/${cluster_name}-${recipe}.result"
        return 1
    fi

    # Run validation
    if ! bash "${SCRIPT_DIR}/validate-scheduling.sh" "${recipe}" &>>"$log_file"; then
        log_error "[$cluster_name] Validation failed for ${recipe}"
        echo "FAILED" > "${WORK_DIR}/${cluster_name}-${recipe}.result"
        return 1
    fi

    log_info "[$cluster_name] ✓ ${recipe} passed"
    echo "PASSED" > "${WORK_DIR}/${cluster_name}-${recipe}.result"
}

# Global flag to prevent duplicate cleanup
CLEANUP_DONE=false

# Cleanup function
cleanup() {
    # Prevent duplicate cleanups on multiple signals
    if [[ "$CLEANUP_DONE" == "true" ]]; then
        exit 1
    fi
    CLEANUP_DONE=true

    local exit_code=$?

    # Kill any background jobs (helm, kubectl, etc.) to prevent hanging
    # Note: jobs -p only shows jobs from this shell, use pkill for child processes
    local job_pids
    job_pids=$(jobs -p 2>/dev/null) || true
    if [[ -n "$job_pids" ]]; then
        echo "$job_pids" | xargs kill -9 2>/dev/null || true
    fi

    # Only clean up if num_clusters is set and > 0 (means we have clusters to clean)
    if [[ ${num_clusters:-0} -gt 0 ]]; then
        if [[ "${KEEP_CLUSTERS:-false}" == "true" ]]; then
            log_info "Preserving clusters (KEEP_CLUSTERS=true)"
            log_info "Delete with: kind delete clusters ${CLUSTER_PREFIX}-{1..${num_clusters}}"
        else
            log_info "Cleaning up remaining clusters..."
            # Get actual running clusters with our prefix
            local existing_clusters
            existing_clusters=$(kind get clusters 2>/dev/null | grep "^${CLUSTER_PREFIX}-" || true)

            if [[ -n "$existing_clusters" ]]; then
                echo "$existing_clusters" | while read -r cluster; do
                    delete_cluster "$cluster" &
                done
                # Wait briefly for deletions, but don't hang forever
                sleep 2
            fi
        fi
    fi

    # Preserve work directory on failure for debugging
    if [[ -n "${WORK_DIR:-}" ]] && [[ -d "$WORK_DIR" ]]; then
        if [[ $exit_code -ne 0 ]]; then
            log_info "Preserving logs for debugging: $WORK_DIR"
            log_info "Delete with: rm -rf $WORK_DIR"
        else
            rm -rf "$WORK_DIR"
        fi
    fi

    exit $exit_code
}

main() {
    local recipes parallelism

    # Determine parallelism (max concurrent clusters)
    if [[ $# -gt 0 ]]; then
        parallelism="$1"
    elif [[ -n "${PARALLEL:-}" ]]; then
        parallelism="${PARALLEL}"
    else
        # Auto-detect: use number of CPUs, minimum 2, maximum 8
        parallelism=$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
        [[ $parallelism -lt 2 ]] && parallelism=2
        [[ $parallelism -gt 16 ]] && parallelism=16
    fi

    # Get recipes to test
    recipes=($(get_recipes))
    local num_recipes=${#recipes[@]}

    if [[ $num_recipes -eq 0 ]]; then
        log_error "No recipes found to test"
        exit 1
    fi

    log_info "Found ${num_recipes} recipe(s) to test with max ${parallelism} parallel cluster(s)"
    log_info "Each recipe gets its own dedicated cluster"

    check_deps

    # Create work directory first (needed for logs)
    WORK_DIR=$(mktemp -d)
    log_debug "Work directory: $WORK_DIR"
    trap cleanup EXIT INT TERM

    # Track all clusters created (for cleanup)
    declare -a all_clusters=()

    # Process recipes in batches of $parallelism
    local passed=() failed=()
    local recipe_idx=0

    while [[ $recipe_idx -lt $num_recipes ]]; do
        local batch_size=$parallelism
        local remaining=$((num_recipes - recipe_idx))
        [[ $remaining -lt $batch_size ]] && batch_size=$remaining

        log_info ""
        log_info "========================================"
        log_info "Processing batch: recipes $((recipe_idx + 1))-$((recipe_idx + batch_size)) of ${num_recipes}"
        log_info "========================================"

        # Create clusters for this batch
        local batch_clusters=()
        local cluster_pids=()

        for ((i=0; i<batch_size; i++)); do
            local cluster_name="${CLUSTER_PREFIX}-$((recipe_idx + i + 1))"
            batch_clusters+=("$cluster_name")
            all_clusters+=("$cluster_name")

            create_cluster "$cluster_name" &
            cluster_pids+=($!)

            # Stagger cluster creation by 2 seconds to reduce resource contention
            [[ $i -lt $((batch_size - 1)) ]] && sleep 2
        done

        # Wait for all clusters in this batch to be created
        local failed_clusters=0
        for pid in "${cluster_pids[@]}"; do
            if ! wait "$pid"; then
                ((failed_clusters++))
            fi
        done

        if [[ $failed_clusters -gt 0 ]]; then
            log_error "Failed to create $failed_clusters clusters in this batch"
            log_error "Try reducing parallelism: PARALLEL=$((parallelism / 2)) make kwok-test-all-parallel"

            # Clean up clusters from this batch
            num_clusters=${#all_clusters[@]}
            exit 1
        fi

        # Run tests in parallel for this batch (each recipe on its own cluster)
        local test_pids=()
        for ((i=0; i<batch_size; i++)); do
            local recipe="${recipes[$((recipe_idx + i))]}"
            local cluster_name="${batch_clusters[$i]}"

            run_test_on_cluster "$cluster_name" "$recipe" &
            test_pids+=($!)
        done

        # Wait for all tests in this batch to complete
        for pid in "${test_pids[@]}"; do
            wait "$pid" || true  # Don't exit on test failure
        done

        # Collect results for this batch
        for ((i=0; i<batch_size; i++)); do
            local recipe="${recipes[$((recipe_idx + i))]}"
            local cluster_name="${batch_clusters[$i]}"
            local result_file="${WORK_DIR}/${cluster_name}-${recipe}.result"

            if [[ -f "$result_file" ]]; then
                local result
                result=$(cat "$result_file")
                if [[ "$result" == "PASSED" ]]; then
                    passed+=("$recipe")
                    log_info "✓ $recipe"
                else
                    failed+=("$recipe")
                    log_error "✗ $recipe"
                fi
            else
                failed+=("$recipe")
                log_error "✗ $recipe (no result file)"
            fi
        done

        # Delete clusters from this batch before starting next batch
        log_info "Cleaning up batch clusters..."
        for cluster_name in "${batch_clusters[@]}"; do
            delete_cluster "$cluster_name" &
        done
        wait

        recipe_idx=$((recipe_idx + batch_size))
    done

    # Final results
    echo ""
    log_info "========================================"
    log_info "Final Results"
    log_info "========================================"

    for recipe in "${passed[@]}"; do
        echo -e "  ${GREEN}✓${NC} $recipe"
    done

    for recipe in "${failed[@]}"; do
        echo -e "  ${RED}✗${NC} $recipe"

        # Show log excerpt on failure
        local result_files=("${WORK_DIR}"/*-"${recipe}.result")
        if [[ -f "${result_files[0]}" ]]; then
            local log_file="${result_files[0]%.result}.log"
            if [[ -f "$log_file" ]]; then
                echo -e "    ${YELLOW}Last 20 lines:${NC}"
                tail -20 "$log_file" | sed 's/^/      /'
            fi
        fi
    done

    # Set num_clusters to 0 since we already cleaned up
    num_clusters=0

    echo ""
    if [[ ${#failed[@]} -eq 0 ]]; then
        log_info "All ${#passed[@]} recipe(s) passed!"
        exit 0
    else
        log_error "${#failed[@]} recipe(s) failed, ${#passed[@]} passed"
        exit 1
    fi
}

main "$@"
