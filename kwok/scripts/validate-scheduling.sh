#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
#   ./validate-scheduling.sh h100-eks-ubuntu-training-kubeflow
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
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
log_debug() { echo -e "${BLUE}[DEBUG]${NC} $*"; }

# Use consistent namespace/release names so Helm can upgrade existing resources
NAMESPACE="${KWOK_NAMESPACE:-aicr-kwok-test}"
RELEASE_NAME="${KWOK_RELEASE:-aicr-test}"
WORK_DIR=""
AICR_BIN=""
KEEP_NAMESPACE=false

# Cleanup function
cleanup() {
    local exit_code=$?
    log_info "Cleaning up..."

    if [[ -n "$WORK_DIR" ]] && [[ -d "$WORK_DIR" ]]; then
        rm -rf "$WORK_DIR"
    fi

    if [[ "$KEEP_NAMESPACE" == "true" ]]; then
        log_info "Preserving releases for inspection"
        log_info "Clean up with: helm list -A (then uninstall each release)"
    else
        # Uninstall all Helm releases from component namespaces
        local releases
        releases=$(helm list -A -o json 2>/dev/null | jq -r '.[] | select(.namespace != "kube-system") | "\(.name) \(.namespace)"' || true)
        if [[ -n "$releases" ]]; then
            while IFS=' ' read -r name ns; do
                if [[ -n "$name" ]]; then
                    log_info "Uninstalling release $name from $ns..."
                    timeout 60s helm uninstall "$name" -n "$ns" --wait 2>/dev/null || true
                fi
            done <<< "$releases"
        fi
        # Clean up stale APIServices before namespace deletion to prevent hangs
        cleanup_stale_apiservices
        # Delete all non-system namespaces (dynamically covers any recipe)
        local system_ns="default|kube-node-lease|kube-public|kube-system|kwok-system|local-path-storage"
        local test_namespaces
        test_namespaces=$(kubectl get ns -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -vE "^(${system_ns})$" || true)
        for ns in $test_namespaces; do
            log_info "Deleting namespace $ns..."
            kubectl delete ns "$ns" --ignore-not-found --wait=true --timeout=120s 2>/dev/null || true
        done
    fi

    exit $exit_code
}

# Find aicr binary (goreleaser puts it in platform-specific dirs)
find_aicr_binary() {
    # Check common locations in order of preference
    local candidates=(
        "${REPO_ROOT}/dist/aicr"
        "${REPO_ROOT}/dist/aicr_darwin_arm64_v8.0/aicr"
        "${REPO_ROOT}/dist/aicr_darwin_all/aicr"
        "${REPO_ROOT}/dist/aicr_linux_amd64_v1/aicr"
    )

    for candidate in "${candidates[@]}"; do
        if [[ -x "$candidate" ]]; then
            echo "$candidate"
            return 0
        fi
    done

    # Try glob pattern as fallback
    local found
    found=$(find "${REPO_ROOT}/dist" -name "aicr" -type f -perm /111 2>/dev/null | head -1)
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

    # Check for aicr binary
    AICR_BIN=$(find_aicr_binary) || {
        log_error "aicr binary not found in dist/"
        log_error "Run 'make build' first"
        exit 1
    }
    log_info "Using aicr binary: $AICR_BIN"

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

    # Find and uninstall old Helm releases from component namespaces
    local releases
    releases=$(helm list -A -o json 2>/dev/null | jq -r '.[] | select(.namespace != "kube-system") | "\(.namespace) \(.name)"' || true)
    if [[ -n "$releases" ]]; then
        log_info "Uninstalling old releases..."
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

    # Delete all non-system namespaces (dynamically covers any recipe)
    local system_ns="default|kube-node-lease|kube-public|kube-system|kwok-system|local-path-storage"
    local test_namespaces
    test_namespaces=$(kubectl get ns -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -vE "^(${system_ns})$" || true)
    for ns in $test_namespaces; do
        log_info "Removing namespace $ns..."
        kubectl delete ns "$ns" --ignore-not-found --wait=true --timeout=120s 2>/dev/null || true
    done

    # Also clean up legacy aicr-kwok-test namespaces
    local old_namespaces
    old_namespaces=$(kubectl get ns -o name 2>/dev/null | grep "namespace/aicr-kwok-test" || true)
    if [[ -n "$old_namespaces" ]]; then
        log_info "Removing old test namespaces..."
        echo "$old_namespaces" | xargs kubectl delete --wait=true --timeout=120s 2>/dev/null || true
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

    log_debug "Checking for KWOK nodes (expected: $expected_total)..."

    # Check if kubectl can connect to cluster
    if ! kubectl cluster-info &>/dev/null; then
        log_error "Cannot connect to Kubernetes cluster"
        log_error "Make sure a KWOK cluster is running: make kwok-cluster"
        exit 1
    fi

    local actual_count
    actual_count=$(kubectl get nodes -l type=kwok --no-headers 2>/dev/null | wc -l | tr -d ' ')

    log_debug "Found $actual_count KWOK nodes"

    if [[ "$actual_count" -lt "$expected_total" ]]; then
        log_error "Expected $expected_total KWOK nodes, found $actual_count"
        log_error ""
        log_error "To fix this, run:"
        log_error "  make kwok-cluster              # Create cluster"
        log_error "  make kwok-nodes RECIPE=$recipe # Create nodes"
        log_error ""
        log_error "Or run the full e2e workflow:"
        log_error "  make kwok-e2e RECIPE=$recipe"
        exit 1
    fi

    log_info "Verified $actual_count KWOK nodes exist"
}

# Generate recipe and bundle
generate_bundle() {
    local recipe="$1"

    log_info "Generating bundle for recipe: $recipe"

    # Read criteria from the recipe overlay file
    local recipe_overlay="${REPO_ROOT}/recipes/overlays/${recipe}.yaml"
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
    log_debug "Running: $AICR_BIN recipe ${recipe_args[*]} --output ${WORK_DIR}/recipe.yaml"

    if ! "$AICR_BIN" recipe "${recipe_args[@]}" --output "${WORK_DIR}/recipe.yaml" 2>&1; then
        log_error "Recipe generation failed"
        return 1
    fi

    if [[ ! -f "${WORK_DIR}/recipe.yaml" ]]; then
        log_error "Recipe file not created: ${WORK_DIR}/recipe.yaml"
        return 1
    fi

    log_debug "Generated recipe:"
    head -20 "${WORK_DIR}/recipe.yaml"

    # Generate bundle with node scheduling flags for KWOK
    # Disable features not needed for scheduling validation:
    # - PrometheusRules and AlertManager (slow to create)
    # - Skyhook customization (creates CRs that depend on operator CRDs)
    log_info "Generating bundle..."

    local bundle_output
    if ! bundle_output=$("$AICR_BIN" bundle \
        --recipe "${WORK_DIR}/recipe.yaml" \
        --output "${WORK_DIR}/bundle" \
        --system-node-selector "aicr.nvidia.com/node-type=system" \
        --accelerated-node-selector "aicr.nvidia.com/node-type=accelerated" \
        --system-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
        --accelerated-node-toleration "nvidia.com/gpu=present:NoSchedule" \
        --accelerated-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
        --set "certmanager:startupapicheck.enabled=false" \
        --set "kubeprometheusstack:defaultRules.create=false" \
        --set "kubeprometheusstack:alertmanager.enabled=false" \
        --set "skyhook-customizations:enabled=false" \
        --set "networkoperator:operator.tolerations[2].key=aicr.nvidia.com/kwok-test" \
        --set "networkoperator:operator.tolerations[2].operator=Equal" \
        --set "networkoperator:operator.tolerations[2].value=true" \
        --set "networkoperator:operator.tolerations[2].effect=NoSchedule" \
        --set "dynamoplatform:etcd.persistence.enabled=false" \
        --set "dynamoplatform:nats.config.jetstream.fileStore.enabled=false" 2>&1); then
        log_error "Bundle generation failed"
        log_error "Bundle command output:"
        echo "$bundle_output"
        return 1
    fi

    if [[ ! -d "${WORK_DIR}/bundle" ]]; then
        log_error "Bundle directory not created: ${WORK_DIR}/bundle"
        return 1
    fi

    log_info "Bundle generated at ${WORK_DIR}/bundle"

    # KWOK clusters use emptyDir for Prometheus storage (no PVC/StorageClass).
    # Cloud overlays (EKS, AKS) set emptyDir: null + volumeClaimTemplate which
    # the Prometheus CRD rejects. Restore emptyDir and remove PVC for KWOK.
    local prom_values="${WORK_DIR}/bundle/kube-prometheus-stack/values.yaml"
    if [[ -f "$prom_values" ]] && yq eval '.prometheus.prometheusSpec.storageSpec.emptyDir' "$prom_values" 2>/dev/null | grep -q 'null'; then
        log_info "Fixing kube-prometheus-stack storageSpec for KWOK (emptyDir instead of PVC)"
        yq eval -i '
            .prometheus.prometheusSpec.storageSpec.emptyDir = {"medium": "", "sizeLimit": "10Gi"} |
            del(.prometheus.prometheusSpec.storageSpec.volumeClaimTemplate)
        ' "$prom_values"
    fi

    log_debug "Bundle contents:"
    ls -1 "${WORK_DIR}/bundle" | head -10
}

# Deploy bundle to cluster using the generated deploy.sh
deploy_bundle() {
    log_info "Deploying per-component bundle..."

    local bundle_dir="${WORK_DIR}/bundle"

    if [[ ! -f "${bundle_dir}/deploy.sh" ]]; then
        log_error "deploy.sh not found in bundle directory: $bundle_dir"
        log_error "Bundle generation may have failed"
        return 1
    fi

    log_debug "Bundle directory: $bundle_dir"
    log_debug "Components in bundle:"
    ls -1 "$bundle_dir" | grep -v "deploy.sh" | head -10

    # Run the generated deploy script without --wait since KWOK clusters
    # only validate scheduling, not pod readiness
    chmod +x "${bundle_dir}/deploy.sh"
    log_info "Running deploy.sh --no-wait..."

    local deploy_output
    if ! deploy_output=$("${bundle_dir}/deploy.sh" --no-wait 2>&1); then
        log_error "Deploy script failed"
        log_error "Last 50 lines of deploy output:"
        echo "$deploy_output" | tail -50
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

    # Get pod status across all component namespaces (per-component deployment)
    # Exclude system namespaces that aren't part of our bundle
    local ns_filter="--all-namespaces"
    local exclude_ns="kube-system|kube-node-lease|kube-public|local-path-storage|kwok-system"

    local total_pods pending_pods failed_pods running_pods unscheduled_pods
    total_pods=$(kubectl get pods ${ns_filter} --no-headers 2>/dev/null | { grep -vE "^(${exclude_ns})\s" || true; } | wc -l | tr -d ' ')
    pending_pods=$(kubectl get pods ${ns_filter} --field-selector=status.phase=Pending --no-headers 2>/dev/null | { grep -vE "^(${exclude_ns})\s" || true; } | wc -l | tr -d ' ')
    failed_pods=$(kubectl get pods ${ns_filter} --field-selector=status.phase=Failed --no-headers 2>/dev/null | { grep -vE "^(${exclude_ns})\s" || true; } | wc -l | tr -d ' ')
    running_pods=$(kubectl get pods ${ns_filter} --field-selector=status.phase=Running --no-headers 2>/dev/null | { grep -vE "^(${exclude_ns})\s" || true; } | wc -l | tr -d ' ')

    # Count truly unscheduled pods (Pending with no node assigned)
    # Pods in ContainerCreating are Pending but scheduled - they have a node
    # Exclude cleanup/webhook Jobs - these are Helm hooks that may not have proper tolerations
    # Use awk to count lines, avoiding issues with empty output or newlines
    unscheduled_pods=$(kubectl get pods ${ns_filter} --field-selector=status.phase=Pending \
        -o json 2>/dev/null | \
        jq -r '.items[] | select(.metadata.namespace as $ns | "'${exclude_ns}'" | split("|") | map(. == $ns) | any | not) | select(.spec.nodeName == null or .spec.nodeName == "") | select(.metadata.ownerReferences == null or (.metadata.ownerReferences | map(.kind) | contains(["Job"]) | not)) | .metadata.name' | \
        awk 'NF {count++} END {print count+0}')

    log_info "Pod status: $total_pods total, $running_pods running, $pending_pods pending ($unscheduled_pods unscheduled), $failed_pods failed"

    # Show pod distribution across nodes
    log_info "Pod distribution:"
    kubectl get pods --all-namespaces -o wide --no-headers 2>/dev/null | \
        grep -vE "^(${exclude_ns})\s" | \
        awk '{print $8}' | sort | uniq -c | \
        while read -r count node; do
            echo "  $node: $count pods"
        done

    # Check for scheduling failures - only fail if pods are truly unscheduled (no node assigned)
    # Pods in ContainerCreating on real nodes are scheduled but waiting for container start
    if [[ "$unscheduled_pods" -gt 0 ]]; then
        log_error "=========================================="
        log_error "Scheduling validation FAILED"
        log_error "=========================================="
        log_error "$unscheduled_pods pods could not be scheduled to nodes"
        log_error ""
        log_error "Unscheduled pods:"
        kubectl get pods --all-namespaces --field-selector=status.phase=Pending -o wide | \
            awk 'NR==1 || $8=="<none>"'
        log_error ""
        log_error "Events for unscheduled pods:"
        kubectl get events --all-namespaces --field-selector reason=FailedScheduling --sort-by='.lastTimestamp'
        log_error ""
        log_error "Common causes:"
        log_error "  - Node selectors don't match available nodes"
        log_error "  - Tolerations missing for node taints"
        log_error "  - Insufficient resources on nodes"
        log_error "=========================================="
        return 1
    fi

    if [[ "$failed_pods" -gt 0 ]]; then
        log_error "=========================================="
        log_error "Scheduling validation FAILED"
        log_error "=========================================="
        log_error "$failed_pods pods are in Failed state"
        log_error ""
        kubectl get pods --all-namespaces --field-selector=status.phase=Failed -o wide
        log_error ""
        log_error "Pod failure details:"
        kubectl get pods --all-namespaces --field-selector=status.phase=Failed -o json | \
            jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name): \(.status.containerStatuses[0].state.terminated.reason // "Unknown") - \(.status.containerStatuses[0].state.terminated.message // "")"'
        log_error "=========================================="
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
                echo "Example: $0 h100-eks-ubuntu-training-kubeflow"
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
        echo "Example: $0 h100-eks-ubuntu-training-kubeflow"
        echo "         $0 --keep-namespace eks-training"
        exit 1
    fi

    # Set up cleanup trap
    trap cleanup EXIT

    log_info "=========================================="
    log_info "Starting validation for recipe: $recipe"
    log_info "=========================================="

    log_debug "Step 1: Checking dependencies..."
    check_deps

    log_debug "Step 2: Cleaning up old test artifacts..."
    cleanup_old_tests

    # Create temp work directory
    WORK_DIR=$(mktemp -d)
    log_info "Work directory: $WORK_DIR"
    log_info "Test namespace: $NAMESPACE"
    log_info "Helm release: $RELEASE_NAME"

    log_debug "Step 3: Verifying KWOK nodes..."
    verify_kwok_nodes "$recipe"

    log_debug "Step 4: Generating bundle..."
    generate_bundle "$recipe"

    log_debug "Step 5: Deploying bundle..."
    deploy_bundle

    log_debug "Step 6: Verifying pod scheduling..."
    verify_pods

    log_info "=========================================="
    log_info "✓ Validation PASSED for recipe: $recipe"
    log_info "=========================================="
}

main "$@"
