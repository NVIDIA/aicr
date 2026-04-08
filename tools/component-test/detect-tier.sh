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

# detect-tier.sh - Determine the test tier for a component
#
# Usage:
#   ./detect-tier.sh <component-name>
#   TIER=deploy ./detect-tier.sh <component-name>
#
# Output: Single word to stdout: scheduling, deploy, or gpu-aware
#
# Detection matrix:
#   Has health check? | GPU references? | Detected tier
#   No                | No              | scheduling
#   Yes               | No              | deploy
#   Yes               | Yes             | gpu-aware
#   No                | Yes             | gpu-aware (warn: no health check)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Source common utilities
# shellcheck source=../common
. "${REPO_ROOT}/tools/common"

has_tools yq

COMPONENT="${1:-${COMPONENT:-}}"
if [[ -z "$COMPONENT" ]]; then
    log_error "COMPONENT is required"
    echo "Usage: $0 <component-name>" >&2
    exit 1
fi

REGISTRY="${REPO_ROOT}/recipes/registry.yaml"

# 1. If TIER env var is set, use it (contributor override)
if [[ -n "${TIER:-}" ]]; then
    case "$TIER" in
        scheduling|deploy|gpu-aware)
            echo "$TIER"
            exit 0
            ;;
        *)
            log_error "Invalid TIER: $TIER (must be scheduling, deploy, or gpu-aware)"
            exit 1
            ;;
    esac
fi

# 2. If testTier field exists in registry.yaml, use it
test_tier=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | .testTier // \"\"" "$REGISTRY")
if [[ -n "$test_tier" ]]; then
    case "$test_tier" in
        scheduling|deploy|gpu-aware)
            echo "$test_tier"
            exit 0
            ;;
        *)
            log_error "Invalid testTier in registry.yaml for $COMPONENT: $test_tier"
            exit 1
            ;;
    esac
fi

# Verify component exists in registry
component_exists=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | .name" "$REGISTRY")
if [[ -z "$component_exists" ]]; then
    log_error "Component '$COMPONENT' not found in $REGISTRY"
    exit 1
fi

# 3. Check if health check exists
has_health_check=false
health_check_ref=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | .healthCheck.assertFile // \"\"" "$REGISTRY")
if [[ -n "$health_check_ref" ]] && [[ -f "${REPO_ROOT}/recipes/${health_check_ref}" ]]; then
    has_health_check=true
elif [[ -f "${REPO_ROOT}/recipes/checks/${COMPONENT}/health-check.yaml" ]]; then
    has_health_check=true
fi

# 4. Check for GPU resource references
has_gpu_refs=false

# Check component values.yaml for nvidia.com/gpu
values_file="${REPO_ROOT}/recipes/components/${COMPONENT}/values.yaml"
if [[ -f "$values_file" ]] && grep -q 'nvidia\.com/gpu' "$values_file" 2>/dev/null; then
    has_gpu_refs=true
fi

# Check registry entry for GPU-related nodeScheduling paths
if [[ "$has_gpu_refs" == "false" ]]; then
    gpu_scheduling=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | .nodeScheduling.accelerated // \"\"" "$REGISTRY")
    if [[ -n "$gpu_scheduling" ]]; then
        # Check if the component's values reference GPU resources
        if [[ -f "$values_file" ]] && grep -qE '(gpu|nvidia|cuda)' "$values_file" 2>/dev/null; then
            has_gpu_refs=true
        fi
    fi
fi

# 5. Apply decision matrix
if [[ "$has_gpu_refs" == "true" ]]; then
    if [[ "$has_health_check" == "false" ]]; then
        log_warning "Component '$COMPONENT' has GPU references but no health check" >&2
    fi
    echo "gpu-aware"
elif [[ "$has_health_check" == "true" ]]; then
    echo "deploy"
else
    echo "scheduling"
fi
