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

# run-health-check.sh - Execute a component's health check
#
# Usage:
#   COMPONENT=cert-manager ./run-health-check.sh
#   COMPONENT=gpu-operator HEALTH_CHECK_TIMEOUT=10m ./run-health-check.sh
#
# Runs chainsaw test against the Kind cluster (not --no-cluster) to validate
# the component is actually healthy.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Source common utilities
# shellcheck source=../common
. "${REPO_ROOT}/tools/common"

has_tools yq

COMPONENT="${COMPONENT:?COMPONENT is required}"
SETTINGS="${REPO_ROOT}/.settings.yaml"
REGISTRY="${REPO_ROOT}/recipes/registry.yaml"

HEALTH_CHECK_TIMEOUT="${HEALTH_CHECK_TIMEOUT:-$(yq -r '.testing.component_test.health_check_timeout // "5m"' "$SETTINGS" 2>/dev/null)}"
CHAINSAW_BIN="${CHAINSAW_BIN:-chainsaw}"

# Resolve health check file path
if [[ -n "${HEALTH_CHECK_FILE:-}" ]]; then
    HEALTH_CHECK="$HEALTH_CHECK_FILE"
else
    # Try registry.yaml reference first
    health_ref=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | .healthCheck.assertFile // \"\"" "$REGISTRY")
    if [[ -n "$health_ref" ]]; then
        HEALTH_CHECK="${REPO_ROOT}/recipes/${health_ref}"
    else
        HEALTH_CHECK="${REPO_ROOT}/recipes/checks/${COMPONENT}/health-check.yaml"
    fi
fi

if [[ ! -f "$HEALTH_CHECK" ]]; then
    log_error "Health check file not found: $HEALTH_CHECK"
    log_error "Component '$COMPONENT' may not have a health check defined"
    exit 1
fi

log_info "Running health check for: $COMPONENT"
log_info "Health check file: $HEALTH_CHECK"
log_info "Timeout: $HEALTH_CHECK_TIMEOUT"

# Verify chainsaw is available
if ! command -v "$CHAINSAW_BIN" &>/dev/null; then
    log_error "chainsaw not found: $CHAINSAW_BIN"
    log_error "Install with: make tools-setup"
    exit 1
fi

# Run chainsaw against the live cluster (no --no-cluster flag)
health_check_dir=$(dirname "$HEALTH_CHECK")

if "$CHAINSAW_BIN" test "$health_check_dir" \
    --test-file "$(basename "$HEALTH_CHECK")" \
    --assert-timeout "$HEALTH_CHECK_TIMEOUT" \
    --no-color=false 2>&1; then
    log_info "Health check PASSED for: $COMPONENT"
else
    log_error "Health check FAILED for: $COMPONENT"
    log_error ""
    log_error "Debug with:"
    log_error "  kubectl get pods -A"
    log_error "  kubectl describe pods -n <namespace>"
    log_error "  kubectl logs -n <namespace> <pod>"
    exit 1
fi
