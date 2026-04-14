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

# setup-gpu-mock.sh - Deploy nvml-mock DaemonSet for GPU simulation
#
# Usage:
#   ./setup-gpu-mock.sh
#   GPU_PROFILE=h100 GPU_COUNT=4 ./setup-gpu-mock.sh
#
# Deploys nvml-mock to simulate GPU hardware in Kind clusters.
# Tries OCI Helm chart first, falls back to kubectl apply with bundled manifest.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Source common utilities
# shellcheck source=../common
. "${REPO_ROOT}/tools/common"

has_tools kubectl yq

SETTINGS="${REPO_ROOT}/.settings.yaml"

NVML_MOCK_VERSION="${NVML_MOCK_VERSION:-$(yq -r '.testing.component_test.nvml_mock_version // "v0.1.0"' "$SETTINGS" 2>/dev/null)}"
NVML_MOCK_IMAGE="${NVML_MOCK_IMAGE:-$(yq -r '.testing.component_test.nvml_mock_image // "ghcr.io/nvidia/nvml-mock"' "$SETTINGS" 2>/dev/null)}"
GPU_PROFILE="${GPU_PROFILE:-$(yq -r '.testing.component_test.default_gpu_profile // "a100"' "$SETTINGS" 2>/dev/null)}"
GPU_COUNT="${GPU_COUNT:-$(yq -r '.testing.component_test.default_gpu_count // 8' "$SETTINGS" 2>/dev/null)}"
MOCK_READY_TIMEOUT="${MOCK_READY_TIMEOUT:-300s}"
MANIFEST_FILE="${SCRIPT_DIR}/manifests/nvml-mock.yaml"

# Map GPU profile to driver version (matches nvml-mock Helm chart defaults)
profile_to_driver_version() {
    case "$1" in
        a100|l40s|t4) echo "550.163.01" ;;
        h100|b200|gb200) echo "570.86.16" ;;
        *) echo "550.163.01" ;;
    esac
}
DRIVER_VERSION="${DRIVER_VERSION:-$(profile_to_driver_version "$GPU_PROFILE")}"

log_info "Setting up GPU mock: profile=${GPU_PROFILE}, count=${GPU_COUNT}, driver=${DRIVER_VERSION}"
log_info "Image: ${NVML_MOCK_IMAGE}:${NVML_MOCK_VERSION}"

# Check if nvml-mock is already deployed and healthy
if kubectl get daemonset nvml-mock -n nvml-mock &>/dev/null; then
    desired=$(kubectl get daemonset nvml-mock -n nvml-mock -o jsonpath='{.status.desiredNumberScheduled}' 2>/dev/null || echo "0")
    ready=$(kubectl get daemonset nvml-mock -n nvml-mock -o jsonpath='{.status.numberReady}' 2>/dev/null || echo "0")
    if [[ "$desired" -gt 0 ]] && [[ "$ready" -eq "$desired" ]]; then
        log_info "nvml-mock DaemonSet already running (${ready}/${desired} ready)"
        exit 0
    fi
    log_info "nvml-mock exists but not fully ready, redeploying..."
    kubectl delete daemonset nvml-mock -n nvml-mock --ignore-not-found 2>/dev/null || true
fi

# Try Helm chart first (preferred when OCI chart is published)
deploy_via_helm() {
    if ! command -v helm &>/dev/null; then
        return 1
    fi

    local chart_ref="oci://${NVML_MOCK_IMAGE}"
    log_info "Attempting Helm install from: ${chart_ref}"

    if helm install nvml-mock "$chart_ref" \
        --version "$NVML_MOCK_VERSION" \
        --namespace nvml-mock --create-namespace \
        --set gpu.profile="$GPU_PROFILE" \
        --set gpu.count="$GPU_COUNT" \
        --wait --timeout "$MOCK_READY_TIMEOUT" 2>/dev/null; then
        return 0
    fi

    log_info "Helm chart not available, falling back to manifest"
    # Clean up partial Helm install
    helm uninstall nvml-mock -n nvml-mock 2>/dev/null || true
    return 1
}

# Fallback: deploy via kubectl with bundled manifest
deploy_via_manifest() {
    if [[ ! -f "$MANIFEST_FILE" ]]; then
        log_error "Fallback manifest not found: $MANIFEST_FILE"
        exit 1
    fi

    log_info "Deploying nvml-mock via manifest: $MANIFEST_FILE"

    # Substitute placeholders in manifest
    sed \
        -e "s|NVML_MOCK_IMAGE_PLACEHOLDER|${NVML_MOCK_IMAGE}|g" \
        -e "s|NVML_MOCK_VERSION_PLACEHOLDER|${NVML_MOCK_VERSION}|g" \
        -e "s|GPU_PROFILE_PLACEHOLDER|${GPU_PROFILE}|g" \
        -e "s|GPU_COUNT_PLACEHOLDER|${GPU_COUNT}|g" \
        -e "s|DRIVER_VERSION_PLACEHOLDER|${DRIVER_VERSION}|g" \
        "$MANIFEST_FILE" | kubectl apply -f -
}

# Try Helm, fall back to manifest
if ! deploy_via_helm; then
    deploy_via_manifest
fi

# Wait for DaemonSet readiness
log_info "Waiting for nvml-mock DaemonSet to be ready (timeout: ${MOCK_READY_TIMEOUT})..."
kubectl rollout status daemonset/nvml-mock -n nvml-mock --timeout="$MOCK_READY_TIMEOUT"

# Verify nvml-mock labeled the nodes
log_info "Verifying nvml-mock node labels..."
gpu_nodes=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)

if [[ -z "$gpu_nodes" ]]; then
    log_warning "No nodes have nvidia.com/gpu.present=true label yet"
    log_warning "nvml-mock may need additional time to label nodes"
    log_warning "Check: kubectl get nodes --show-labels | grep nvidia"
else
    log_info "Nodes with nvml-mock GPU simulation:"
    echo "$gpu_nodes" | while IFS= read -r line; do
        log_info "  $line"
    done
fi

log_info "GPU mock setup complete"
