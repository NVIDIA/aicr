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

# validate-scheduling.sh - Validate bundle scheduling on KWOK cluster
#
# Usage:
#   ./validate-scheduling.sh <recipe-name>
#   ./validate-scheduling.sh gb200-eks-training
#
# This script:
# 1. Generates a recipe from the cluster config
# 2. Generates a bundle from the recipe
# 3. Deploys the bundle to the KWOK cluster
# 4. Verifies all pods reach Running state (KWOK auto-transitions them)
# 5. Reports success/failure

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KWOK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${KWOK_DIR}/.." && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }

# Use consistent namespace/release names so Helm can upgrade existing resources
NAMESPACE="${KWOK_NAMESPACE:-eidos-kwok-test}"
RELEASE_NAME="${KWOK_RELEASE:-eidos-test}"
WORK_DIR=""
EIDOS_BIN=""
KEEP_NAMESPACE=false

# Cleanup function
cleanup() {
    local exit_code=$?
    log_info "Cleaning up..."

    if [[ -n "$WORK_DIR" ]] && [[ -d "$WORK_DIR" ]]; then
        rm -rf "$WORK_DIR"
    fi

    if [[ "$KEEP_NAMESPACE" == "true" ]]; then
        log_info "Preserving namespace $NAMESPACE for inspection"
        log_info "Clean up with: helm uninstall $RELEASE_NAME -n $NAMESPACE && kubectl delete ns $NAMESPACE"
    else
        # Uninstall Helm release first (cleans up CRDs properly)
        # Add timeout to prevent hanging when cluster is gone
        timeout 60s helm uninstall "$RELEASE_NAME" -n "$NAMESPACE" --wait 2>/dev/null || true
        # Clean up stale APIServices before namespace deletion to prevent hangs
        cleanup_stale_apiservices
        # Wait for namespace deletion to complete to avoid conflicts with subsequent tests
        kubectl delete ns "$NAMESPACE" --ignore-not-found --wait=true --timeout=120s 2>/dev/null || true
    fi

    exit $exit_code
}

# Find eidos binary (goreleaser puts it in platform-specific dirs)
find_eidos_binary() {
    # Check common locations in order of preference
    local candidates=(
        "${REPO_ROOT}/dist/eidos"
        "${REPO_ROOT}/dist/eidos_darwin_arm64_v8.0/eidos"
        "${REPO_ROOT}/dist/eidos_darwin_all/eidos"
        "${REPO_ROOT}/dist/eidos_linux_amd64_v1/eidos"
    )

    for candidate in "${candidates[@]}"; do
        if [[ -x "$candidate" ]]; then
            echo "$candidate"
            return 0
        fi
    done

    # Try glob pattern as fallback
    local found
    found=$(find "${REPO_ROOT}/dist" -name "eidos" -type f -perm /111 2>/dev/null | head -1)
    if [[ -n "$found" ]]; then
        echo "$found"
        return 0
    fi

    return 1
}

# Check dependencies
check_deps() {
    local missing=()
    for cmd in kubectl helm yq jq; do
        if ! command -v "$cmd" &>/dev/null; then
            missing+=("$cmd")
        fi
    done

    # Check for eidos binary
    EIDOS_BIN=$(find_eidos_binary) || {
        log_error "eidos binary not found in dist/"
        log_error "Run 'make build' first"
        exit 1
    }
    log_info "Using eidos binary: $EIDOS_BIN"

    if [[ ${#missing[@]} -gt 0 ]]; then
        log_error "Missing required tools: ${missing[*]}"
        exit 1
    fi
}

# Force-remove finalizers from a stuck namespace
force_delete_namespace() {
    local ns="$1"
    log_warn "Force-removing finalizers from stuck namespace $ns"
    kubectl get ns "$ns" -o json 2>/dev/null | \
        jq '.spec.finalizers = []' | \
        kubectl replace --raw "/api/v1/namespaces/${ns}/finalize" -f - >/dev/null 2>&1 || true
}

# Clean up stale APIServices that can cause namespace deletion to hang
# prometheus-adapter creates these and they become stale when the adapter is deleted
cleanup_stale_apiservices() {
    local stale_apis=(
        "v1beta1.custom.metrics.k8s.io"
        "v1beta1.external.metrics.k8s.io"
    )

    for api in "${stale_apis[@]}"; do
        if kubectl get apiservice "$api" &>/dev/null; then
            # Check if the APIService is unavailable (stale)
            local available
            available=$(kubectl get apiservice "$api" -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null || echo "Unknown")
            if [[ "$available" != "True" ]]; then
                log_info "Removing stale APIService: $api"
                kubectl delete apiservice "$api" --ignore-not-found 2>/dev/null || true
            fi
        fi
    done
}

# Wait for a namespace to fully terminate
wait_for_namespace_gone() {
    local ns="$1"
    local max_wait="${2:-120}"
    local force_after="${3:-60}"  # Force-delete after this many seconds
    local waited=0
    local force_attempted=false

    while kubectl get ns "$ns" &>/dev/null; do
        if [[ $waited -ge $max_wait ]]; then
            log_warn "Timeout waiting for namespace $ns to terminate"
            return 1
        fi

        # After force_after seconds, try force-deleting if namespace is stuck
        if [[ $waited -ge $force_after ]] && [[ "$force_attempted" == "false" ]]; then
            local ns_status
            ns_status=$(kubectl get ns "$ns" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
            if [[ "$ns_status" == "Terminating" ]]; then
                # Check if namespace is empty (stuck on finalizer with no resources)
                local resource_count
                resource_count=$(kubectl api-resources --verbs=list --namespaced -o name 2>/dev/null | \
                    xargs -n 1 kubectl get -n "$ns" --ignore-not-found --no-headers 2>/dev/null | wc -l | tr -d ' ')
                if [[ "$resource_count" -eq 0 ]]; then
                    force_delete_namespace "$ns"
                    force_attempted=true
                fi
            fi
        fi

        log_info "Waiting for namespace $ns to terminate ($waited/${max_wait}s)..."
        sleep 5
        waited=$((waited + 5))
    done
    return 0
}

# Cleanup old test artifacts from previous runs
cleanup_old_tests() {
    log_info "Cleaning up old test artifacts..."

    # First, wait for any currently terminating namespaces to finish
    # This handles the case where a previous run's cleanup trap is still in progress
    if kubectl get ns "$NAMESPACE" &>/dev/null; then
        local ns_status
        ns_status=$(kubectl get ns "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
        if [[ "$ns_status" == "Terminating" ]]; then
            log_info "Namespace $NAMESPACE is terminating from previous run, waiting..."
            wait_for_namespace_gone "$NAMESPACE" 120
        fi
    fi

    # Find and uninstall old Helm releases first (this should clean up their CRDs)
    local releases
    releases=$(helm list -A -o json 2>/dev/null | jq -r '.[] | select(.namespace | startswith("eidos-kwok-test")) | "\(.namespace) \(.name)"' || true)
    if [[ -n "$releases" ]]; then
        log_info "Uninstalling old test releases..."
        echo "$releases" | while read -r ns release; do
            if [[ -n "$release" ]]; then
                log_info "  Uninstalling $release from $ns..."
                helm uninstall "$release" -n "$ns" --wait 2>/dev/null || true
            fi
        done
    fi

    # Clean up stale APIServices left by prometheus-adapter
    # These can cause namespace deletion to hang with "stale GroupVersion discovery" errors
    cleanup_stale_apiservices

    # Delete old test namespaces and wait for them to fully terminate
    local old_namespaces
    old_namespaces=$(kubectl get ns -o name 2>/dev/null | grep "namespace/eidos-kwok-test" || true)
    if [[ -n "$old_namespaces" ]]; then
        log_info "Removing old test namespaces..."
        echo "$old_namespaces" | xargs kubectl delete --wait=true --timeout=120s 2>/dev/null || true

        # Wait for the specific namespace we'll use to fully disappear
        wait_for_namespace_gone "$NAMESPACE" 120
    fi

    # Final check: ensure our namespace is gone before proceeding
    if kubectl get ns "$NAMESPACE" &>/dev/null; then
        log_info "Waiting for namespace $NAMESPACE to fully terminate..."
        wait_for_namespace_gone "$NAMESPACE" 180
    fi

    log_info "Cleanup complete"
}

# Fixed defaults matching apply-nodes.sh
SYSTEM_NODE_COUNT=2
GPU_NODE_COUNT=4

# Verify KWOK nodes exist
verify_kwok_nodes() {
    local recipe="$1"
    local expected_total=$((SYSTEM_NODE_COUNT + GPU_NODE_COUNT))

    local actual_count
    actual_count=$(kubectl get nodes -l type=kwok --no-headers 2>/dev/null | wc -l | tr -d ' ')

    if [[ "$actual_count" -lt "$expected_total" ]]; then
        log_error "Expected $expected_total KWOK nodes, found $actual_count"
        log_error "Run 'make kwok-nodes RECIPE=$recipe' first"
        exit 1
    fi

    log_info "Verified $actual_count KWOK nodes exist"
}

# Generate recipe and bundle
generate_bundle() {
    local recipe="$1"

    log_info "Generating bundle for recipe: $recipe"

    # Read criteria from the recipe overlay file
    local recipe_overlay="${REPO_ROOT}/pkg/recipe/data/overlays/${recipe}.yaml"
    if [[ ! -f "$recipe_overlay" ]]; then
        log_error "Recipe overlay not found: $recipe_overlay"
        exit 1
    fi

    # Extract criteria from overlay
    local service accelerator intent os
    service=$(yq eval '.spec.criteria.service // ""' "$recipe_overlay")
    accelerator=$(yq eval '.spec.criteria.accelerator // ""' "$recipe_overlay")
    intent=$(yq eval '.spec.criteria.intent // ""' "$recipe_overlay")
    os=$(yq eval '.spec.criteria.os // ""' "$recipe_overlay")

    log_info "Criteria: service=$service accelerator=$accelerator intent=$intent os=$os"

    # Build recipe command with available criteria
    local recipe_args=()
    [[ -n "$service" ]] && recipe_args+=(--service "$service")
    [[ -n "$accelerator" ]] && recipe_args+=(--accelerator "$accelerator")
    [[ -n "$intent" ]] && recipe_args+=(--intent "$intent")
    [[ -n "$os" ]] && recipe_args+=(--os "$os")

    # Generate resolved recipe from criteria
    log_info "Generating resolved recipe..."
    "$EIDOS_BIN" recipe "${recipe_args[@]}" --output "${WORK_DIR}/recipe.yaml"

    # Generate bundle with node scheduling flags for KWOK
    # Disable features not needed for scheduling validation:
    # - PrometheusRules and AlertManager (slow to create)
    # - Skyhook customization (creates CRs that depend on operator CRDs)
    log_info "Generating bundle..."
    "$EIDOS_BIN" bundle \
        --recipe "${WORK_DIR}/recipe.yaml" \
        --output "${WORK_DIR}/bundle" \
        --system-node-selector "eidos.nvidia.com/node-type=system" \
        --accelerated-node-selector "eidos.nvidia.com/node-type=accelerated" \
        --system-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
        --accelerated-node-toleration "nvidia.com/gpu=present:NoSchedule" \
        --accelerated-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
        --set "kubeprometheusstack:defaultRules.create=false" \
        --set "kubeprometheusstack:alertmanager.enabled=false" \
        --set "skyhook-operator:customization=disabled" \
        --set "networkoperator:operator.tolerations[2].key=eidos.nvidia.com/kwok-test" \
        --set "networkoperator:operator.tolerations[2].operator=Equal" \
        --set "networkoperator:operator.tolerations[2].value=true" \
        --set "networkoperator:operator.tolerations[2].effect=NoSchedule"

    log_info "Bundle generated at ${WORK_DIR}/bundle"
}

# Deploy bundle to cluster
deploy_bundle() {
    log_info "Deploying bundle to namespace: $NAMESPACE"

    # Create namespace
    kubectl create ns "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

    # Fetch chart dependencies
    log_info "Fetching Helm chart dependencies..."
    if ! helm dependency update "${WORK_DIR}/bundle" 2>&1; then
        log_error "Helm dependency update failed"
        return 1
    fi

    # Deploy bundle using Helm (upgrade --install is idempotent)
    # Skip --wait since we only care about scheduling, not readiness probes
    # KWOK stage-fast will transition pods to Running
    log_info "Installing Helm chart with release name: $RELEASE_NAME..."
    if ! helm upgrade --install "$RELEASE_NAME" "${WORK_DIR}/bundle" \
        --namespace "$NAMESPACE" \
        --timeout 300s 2>&1; then
        log_error "Helm install failed"
        return 1
    fi

    # Brief wait for scheduler to place pods
    log_info "Waiting for pods to be scheduled..."
    sleep 5

    log_info "Bundle deployed successfully"
}

# Verify pod scheduling
verify_pods() {
    log_info "Verifying pod scheduling..."

    # Get pod status
    local total_pods pending_pods failed_pods running_pods unscheduled_pods
    total_pods=$(kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    pending_pods=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Pending --no-headers 2>/dev/null | wc -l | tr -d ' ')
    failed_pods=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Failed --no-headers 2>/dev/null | wc -l | tr -d ' ')
    running_pods=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l | tr -d ' ')

    # Count truly unscheduled pods (Pending with no node assigned)
    # Pods in ContainerCreating are Pending but scheduled - they have a node
    # Exclude cleanup/webhook Jobs - these are Helm hooks that may not have proper tolerations
    # Use awk to count lines, avoiding issues with empty output or newlines
    unscheduled_pods=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Pending \
        -o json 2>/dev/null | \
        jq -r '.items[] | select(.spec.nodeName == null or .spec.nodeName == "") | select(.metadata.ownerReferences == null or (.metadata.ownerReferences | map(.kind) | contains(["Job"]) | not)) | .metadata.name' | \
        awk 'NF {count++} END {print count+0}')

    log_info "Pod status: $total_pods total, $running_pods running, $pending_pods pending ($unscheduled_pods unscheduled), $failed_pods failed"

    # Show pod distribution across nodes
    log_info "Pod distribution:"
    kubectl get pods -n "$NAMESPACE" -o wide --no-headers 2>/dev/null | \
        awk '{print $7}' | sort | uniq -c | \
        while read -r count node; do
            echo "  $node: $count pods"
        done

    # Check for scheduling failures - only fail if pods are truly unscheduled (no node assigned)
    # Pods in ContainerCreating on real nodes are scheduled but waiting for container start
    if [[ "$unscheduled_pods" -gt 0 ]]; then
        log_error "Scheduling validation FAILED: $unscheduled_pods pods could not be scheduled"
        log_error "Unscheduled pods:"
        kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Pending -o wide | \
            awk 'NR==1 || $7=="<none>"'
        log_error "Events for unscheduled pods:"
        kubectl get events -n "$NAMESPACE" --field-selector reason=FailedScheduling
        return 1
    fi

    if [[ "$failed_pods" -gt 0 ]]; then
        log_error "Scheduling validation FAILED: $failed_pods pods Failed"
        kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Failed -o wide
        return 1
    fi

    if [[ "$total_pods" -eq 0 ]]; then
        log_warn "No pods were created - bundle may be empty"
        return 0
    fi

    log_info "Scheduling validation PASSED: all $total_pods pods scheduled successfully"
    return 0
}

# Main
main() {
    local recipe=""

    # Parse arguments
    while [[ $# -gt 0 ]]; do
        case $1 in
            --keep-namespace)
                KEEP_NAMESPACE=true
                shift
                ;;
            -*)
                echo "Unknown option: $1"
                echo "Usage: $0 [--keep-namespace] <recipe-name>"
                echo "Example: $0 gb200-eks-training"
                echo "         $0 --keep-namespace eks-training"
                exit 1
                ;;
            *)
                recipe="$1"
                shift
                ;;
        esac
    done

    if [[ -z "$recipe" ]]; then
        echo "Usage: $0 [--keep-namespace] <recipe-name>"
        echo "Example: $0 gb200-eks-training"
        echo "         $0 --keep-namespace eks-training"
        exit 1
    fi

    # Set up cleanup trap
    trap cleanup EXIT

    check_deps
    cleanup_old_tests

    # Create temp work directory
    WORK_DIR=$(mktemp -d)
    log_info "Work directory: $WORK_DIR"
    log_info "Test namespace: $NAMESPACE"
    log_info "Helm release: $RELEASE_NAME"

    verify_kwok_nodes "$recipe"
    generate_bundle "$recipe"
    deploy_bundle
    verify_pods

    log_info "Validation complete for recipe: $recipe"
}

main "$@"
