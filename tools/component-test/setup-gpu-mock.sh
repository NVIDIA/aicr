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
NVML_MOCK_CHART="${NVML_MOCK_CHART:-$(yq -r '.testing.component_test.nvml_mock_chart // "ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock"' "$SETTINGS" 2>/dev/null)}"
NVML_MOCK_CHART_VERSION="${NVML_MOCK_CHART_VERSION:-$(yq -r '.testing.component_test.nvml_mock_chart_version // "0.3.0"' "$SETTINGS" 2>/dev/null)}"
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

# Verify the mock staged what its consumers actually read.
#
# This previously looked for nvidia.com/gpu.present=true and only warned when
# it was absent. nvml-mock does not write node labels: that one comes from
# NFD/GFD, which this single-component harness never deploys, so the check
# could not pass on a passing run and could not fail on a broken one. It then
# printed "setup complete" either way.
#
# What nvml-mock does produce is the mock driver tree that the device plugin
# and GFD read through NVIDIA_DRIVER_ROOT, plus the NFD feature file. Assert
# both and fail closed: a DaemonSet that rolls out with nothing staged leaves
# a cluster reporting zero GPUs with no error anywhere. Called on the reuse
# path too, so an already-running but empty mock cannot pass unexamined.
verify_mock_staged() {
    log_info "Verifying nvml-mock staged the mock driver..."
    local mock_pod
    mock_pod=$(kubectl get pods -n nvml-mock -l app.kubernetes.io/name=nvml-mock \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [[ -z "$mock_pod" ]]; then
        log_error "nvml-mock DaemonSet present but no pod matches app.kubernetes.io/name=nvml-mock"
        exit 1
    fi

    if ! kubectl exec -n nvml-mock "$mock_pod" -c node-agent -- \
        sh -c 'ls /host/var/lib/nvml-mock/driver/usr/lib*/libnvidia-ml.so.1' >/dev/null 2>&1; then
        log_error "mock libnvidia-ml.so.1 is not staged under /var/lib/nvml-mock/driver on the node."
        log_error "Consumers pointed at NVIDIA_DRIVER_ROOT would find no driver and advertise no GPUs."
        exit 1
    fi
    log_info "  mock driver staged: /var/lib/nvml-mock/driver/usr/lib*/libnvidia-ml.so.1"

    if ! kubectl exec -n nvml-mock "$mock_pod" -c node-agent -- \
        cat /host/etc/kubernetes/node-feature-discovery/features.d/nvml-mock.features >/dev/null 2>&1; then
        log_error "NFD feature file missing; NFD cannot derive the PCI vendor label."
        exit 1
    fi
    log_info "  NFD feature file written"

    # Node labels are NFD's to create, so report rather than assert.
    local gpu_nodes
    gpu_nodes=$(kubectl get nodes -l nvidia.com/gpu.present=true \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
    if [[ -z "$gpu_nodes" ]]; then
        log_info "No node carries nvidia.com/gpu.present=true. Expected unless NFD and GFD are"
        log_info "deployed: nvml-mock supplies the feature file, they create the label."
    else
        log_info "Nodes labelled by NFD/GFD from the mock:"
        echo "$gpu_nodes" | while IFS= read -r line; do
            log_info "  $line"
        done
    fi
}

# Check if nvml-mock is already deployed and healthy
if kubectl get daemonset nvml-mock -n nvml-mock &>/dev/null; then
    desired=$(kubectl get daemonset nvml-mock -n nvml-mock -o jsonpath='{.status.desiredNumberScheduled}' 2>/dev/null || echo "0")
    ready=$(kubectl get daemonset nvml-mock -n nvml-mock -o jsonpath='{.status.numberReady}' 2>/dev/null || echo "0")
    if [[ "$desired" -gt 0 ]] && [[ "$ready" -eq "$desired" ]]; then
        log_info "nvml-mock DaemonSet already running (${ready}/${desired} ready)"
        verify_mock_staged
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

    # The chart lives at a different registry path from the image; see the
    # component_test block in .settings.yaml. The image tag is deliberately
    # NOT forced here, because the chart pins its own.
    local chart_ref="oci://${NVML_MOCK_CHART}"
    log_info "Attempting Helm install from: ${chart_ref} (version ${NVML_MOCK_CHART_VERSION})"

    local helm_err
    helm_err=$(mktemp)
    if helm install nvml-mock "$chart_ref" \
        --version "$NVML_MOCK_CHART_VERSION" \
        --namespace nvml-mock --create-namespace \
        --set gpu.profile="$GPU_PROFILE" \
        --set gpu.count="$GPU_COUNT" \
        --wait --timeout "$MOCK_READY_TIMEOUT" 2>"$helm_err"; then
        rm -f "$helm_err"
        return 0
    fi

    # Report why. Discarding this is what let a chart reference that could
    # never resolve look like "the chart is not published yet" for months.
    log_warning "Helm install failed, falling back to manifest. helm said:"
    sed 's/^/    /' "$helm_err" >&2
    rm -f "$helm_err"
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

verify_mock_staged

log_info "GPU mock setup complete"
