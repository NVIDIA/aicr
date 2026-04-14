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

# ensure-cluster.sh - Ensure a suitable Kind cluster is running
#
# Usage:
#   TIER=deploy ./ensure-cluster.sh
#   TIER=scheduling COMPONENT=cert-manager ./ensure-cluster.sh
#
# For scheduling tier: delegates to existing KWOK infrastructure (make kwok-cluster)
# For deploy/gpu-aware: creates/reuses an aicr-component-test Kind cluster

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Source common utilities
# shellcheck source=../common
. "${REPO_ROOT}/tools/common"

has_tools kind kubectl yq

TIER="${TIER:-deploy}"
SETTINGS="${REPO_ROOT}/.settings.yaml"

CLUSTER_NAME="${CLUSTER_NAME:-$(yq -r '.testing.component_test.cluster_name // "aicr-component-test"' "$SETTINGS" 2>/dev/null)}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-$(yq -r '.testing.kind_node_image // "kindest/node:v1.32.0"' "$SETTINGS" 2>/dev/null)}"
KIND_CONFIG="${KIND_CONFIG:-${SCRIPT_DIR}/kind-config.yaml}"
CLUSTER_WAIT_TIMEOUT="${CLUSTER_WAIT_TIMEOUT:-120s}"

CONTEXT="kind-${CLUSTER_NAME}"

# For scheduling tier, delegate to KWOK cluster infrastructure
if [[ "$TIER" == "scheduling" ]]; then
    log_info "Scheduling tier detected"
    log_info "Component scheduling validation uses the KWOK infrastructure, not this harness"
    log_info ""
    log_info "To validate scheduling, use:"
    log_info "  make kwok-e2e RECIPE=<recipe-name>"
    log_info ""
    log_info "See: kwok/README.md for details"
    exit 0
fi

# For deploy/gpu-aware: create or reuse the component-test cluster
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    log_info "Reusing existing cluster: ${CLUSTER_NAME}"
    kubectl config use-context "$CONTEXT"
else
    log_info "Creating Kind cluster: ${CLUSTER_NAME}"
    kind create cluster \
        --name "$CLUSTER_NAME" \
        --image "$KIND_NODE_IMAGE" \
        --config "$KIND_CONFIG" \
        --wait 60s

    kubectl config use-context "$CONTEXT"
fi

# Wait for all nodes to be ready
log_info "Waiting for nodes to be ready (timeout: ${CLUSTER_WAIT_TIMEOUT})..."
kubectl wait --for=condition=Ready node --all --timeout="$CLUSTER_WAIT_TIMEOUT"

log_info "Cluster '${CLUSTER_NAME}' is ready"
log_info "Context: ${CONTEXT}"
