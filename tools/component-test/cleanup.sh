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

# cleanup.sh - Clean up after component testing
#
# Usage:
#   COMPONENT=cert-manager ./cleanup.sh                        # Uninstall component only
#   COMPONENT=cert-manager DELETE_CLUSTER=true ./cleanup.sh     # Delete entire cluster
#   KEEP_CLUSTER=true ./cleanup.sh                       # Skip cleanup (for debugging)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Source common utilities
# shellcheck source=../common
. "${REPO_ROOT}/tools/common"

has_tools helm kubectl yq

COMPONENT="${COMPONENT:-}"
SETTINGS="${REPO_ROOT}/.settings.yaml"
REGISTRY="${REPO_ROOT}/recipes/registry.yaml"

CLUSTER_NAME="${CLUSTER_NAME:-$(yq -r '.testing.component_test.cluster_name // "aicr-component-test"' "$SETTINGS" 2>/dev/null)}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
DELETE_CLUSTER="${DELETE_CLUSTER:-false}"
FORCE_CLEANUP="${FORCE_CLEANUP:-false}"

# If KEEP_CLUSTER is true, skip everything
if [[ "$KEEP_CLUSTER" == "true" ]]; then
    log_info "KEEP_CLUSTER=true, skipping cleanup"
    log_info "Cluster: $CLUSTER_NAME"
    if [[ -n "$COMPONENT" ]]; then
        log_info "Component '$COMPONENT' is still deployed"
        log_info "To inspect: kubectl -n <namespace> get pods"
    fi
    exit 0
fi

# Uninstall the component if specified
if [[ -n "$COMPONENT" ]]; then
    # Determine namespace
    helm_namespace=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | .helm.defaultNamespace // .kustomize.defaultNamespace // \"${COMPONENT}\"" "$REGISTRY" 2>/dev/null)

    log_info "Uninstalling component: $COMPONENT (namespace: $helm_namespace)"

    if helm status "$COMPONENT" -n "$helm_namespace" &>/dev/null; then
        helm uninstall "$COMPONENT" -n "$helm_namespace" --wait --timeout 120s 2>/dev/null || {
            log_warning "Helm uninstall timed out, force removing..."
            helm uninstall "$COMPONENT" -n "$helm_namespace" --no-hooks 2>/dev/null || true
        }
        log_info "Component '$COMPONENT' uninstalled"
    else
        log_info "Component '$COMPONENT' not found (already uninstalled?)"
    fi

    # Clean up nvml-mock if it was deployed
    if helm status nvml-mock -n nvml-mock &>/dev/null; then
        log_info "Uninstalling nvml-mock..."
        helm uninstall nvml-mock -n nvml-mock --wait 2>/dev/null || true
    elif kubectl get daemonset nvml-mock -n nvml-mock &>/dev/null; then
        log_info "Removing nvml-mock resources..."
        kubectl delete daemonset nvml-mock -n nvml-mock --ignore-not-found 2>/dev/null || true
        kubectl delete configmap nvml-mock-config -n nvml-mock --ignore-not-found 2>/dev/null || true
    fi

    # Delete component namespace if empty
    if kubectl get namespace "$helm_namespace" &>/dev/null; then
        pod_count=$(kubectl get pods -n "$helm_namespace" --no-headers 2>/dev/null | wc -l | tr -d ' ')
        if [[ "$pod_count" -eq 0 ]]; then
            log_info "Deleting empty namespace: $helm_namespace"
            kubectl delete namespace "$helm_namespace" --wait=true --timeout=60s 2>/dev/null || true
        fi
    fi

    # Delete nvml-mock namespace if it exists
    kubectl delete namespace nvml-mock --ignore-not-found --wait=true --timeout=60s 2>/dev/null || true
fi

# Delete the Kind cluster if requested
if [[ "$DELETE_CLUSTER" == "true" ]]; then
    if ! command -v kind &>/dev/null; then
        log_error "kind not installed, cannot delete cluster"
        exit 1
    fi

    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        if [[ "$FORCE_CLEANUP" != "true" ]]; then
            if [[ ! -t 0 ]]; then
                log_error "DELETE_CLUSTER=true requires FORCE_CLEANUP=true in non-interactive mode"
                exit 1
            fi
            log_info "About to delete Kind cluster: $CLUSTER_NAME"
            read -r -p "Continue? [y/N] " confirm
            if [[ "$confirm" != [yY] ]]; then
                log_info "Aborted"
                exit 0
            fi
        fi

        log_info "Deleting Kind cluster: $CLUSTER_NAME"
        kind delete cluster --name "$CLUSTER_NAME"
        log_info "Cluster deleted"
    else
        log_info "Cluster '$CLUSTER_NAME' not found (already deleted?)"
    fi
fi

log_info "Cleanup complete"
