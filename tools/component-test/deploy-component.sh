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

# deploy-component.sh - Bundle and deploy a single component
#
# Usage:
#   COMPONENT=cert-manager ./deploy-component.sh
#   COMPONENT=gpu-operator HELM_NAMESPACE=gpu-operator ./deploy-component.sh
#
# Generates a minimal single-component recipe, runs aicr bundle, and
# helm-installs the result into the test cluster.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Source common utilities
# shellcheck source=../common
. "${REPO_ROOT}/tools/common"

has_tools helm kubectl yq

COMPONENT="${COMPONENT:?COMPONENT is required}"
SETTINGS="${REPO_ROOT}/.settings.yaml"
REGISTRY="${REPO_ROOT}/recipes/registry.yaml"

HELM_TIMEOUT="${HELM_TIMEOUT:-$(yq -r '.testing.component_test.helm_timeout // "300s"' "$SETTINGS" 2>/dev/null)}"
HELM_NAMESPACE="${HELM_NAMESPACE:-}"
HELM_VALUES="${HELM_VALUES:-}"
HELM_SET="${HELM_SET:-}"

# Find aicr binary (same pattern as kwok/scripts/validate-scheduling.sh)
find_aicr_binary() {
    local aicr_bin="${AICR_BIN:-}"
    if [[ -n "$aicr_bin" ]] && [[ -x "$aicr_bin" ]]; then
        echo "$aicr_bin"
        return 0
    fi

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

    # Glob fallback
    local found
    found=$(find "${REPO_ROOT}/dist" -name "aicr" -type f -perm /111 2>/dev/null | head -1)
    if [[ -n "$found" ]]; then
        echo "$found"
        return 0
    fi

    return 1
}

AICR_BIN=$(find_aicr_binary) || {
    log_error "aicr binary not found in dist/"
    log_error "Run 'make build' first"
    exit 1
}
log_info "Using aicr binary: $AICR_BIN"

# Verify component exists in registry
component_entry=$(yq eval ".components[] | select(.name == \"${COMPONENT}\")" "$REGISTRY")
if [[ -z "$component_entry" ]]; then
    log_error "Component '$COMPONENT' not found in $REGISTRY"
    exit 1
fi

# Determine namespace: env override > registry defaultNamespace > component name
if [[ -z "$HELM_NAMESPACE" ]]; then
    HELM_NAMESPACE=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | .helm.defaultNamespace // .kustomize.defaultNamespace // \"${COMPONENT}\"" "$REGISTRY")
fi

# Create temp working directory
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# Generate a minimal single-component recipe
log_info "Generating single-component recipe for: $COMPONENT"

# Extract component details from registry — detect type by checking which key exists
has_helm=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | has(\"helm\")" "$REGISTRY")
has_kustomize=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | has(\"kustomize\")" "$REGISTRY")
has_manifest=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | has(\"manifest\")" "$REGISTRY")

if [[ "$has_manifest" == "true" ]]; then
    log_error "Component '$COMPONENT' uses manifest type, which is not supported by this harness"
    log_error "Manifest components are deployed via raw YAML, not bundled via Helm/Kustomize"
    exit 1
fi

if [[ "$has_helm" == "true" ]]; then
    chart_type="Helm"
elif [[ "$has_kustomize" == "true" ]]; then
    chart_type="Kustomize"
else
    log_error "Component '$COMPONENT' has no helm, kustomize, or manifest configuration in registry"
    exit 1
fi

chart_source=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | .helm.defaultRepository // .kustomize.defaultSource // \"\"" "$REGISTRY")
chart_name_raw=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | .helm.defaultChart // \"\"" "$REGISTRY")
# Strip repo prefix from chart name (e.g., "jetstack/cert-manager" → "cert-manager")
# Mirrors the Go recipe resolver logic in pkg/recipe/metadata.go
chart_name="${chart_name_raw##*/}"
chart_version=$(yq eval ".components[] | select(.name == \"${COMPONENT}\") | .helm.defaultVersion // .kustomize.defaultTag // \"\"" "$REGISTRY")

# Find the component's values file by searching overlays (base.yaml first, then others)
values_file=""
checked_base=false
for overlay in "${REPO_ROOT}"/recipes/overlays/base.yaml "${REPO_ROOT}"/recipes/overlays/*.yaml; do
    [[ -f "$overlay" ]] || continue
    # base.yaml appears in both the explicit path and the glob; skip the duplicate
    if [[ "$(basename "$overlay")" == "base.yaml" ]]; then
        if [[ "$checked_base" == "true" ]]; then continue; fi
        checked_base=true
    fi
    candidate=$(yq eval ".spec.componentRefs[] | select(.name == \"${COMPONENT}\") | .valuesFile // \"\"" "$overlay" 2>/dev/null)
    if [[ -n "$candidate" ]]; then
        values_file="$candidate"
        break
    fi
done
if [[ -z "$values_file" ]]; then
    # Try component default values
    if [[ -f "${REPO_ROOT}/recipes/components/${COMPONENT}/values.yaml" ]]; then
        values_file="components/${COMPONENT}/values.yaml"
    fi
fi

# Build a minimal resolved recipe (RecipeResult format, which aicr bundle expects)
cat > "${WORK_DIR}/recipe.yaml" <<EOF
kind: RecipeResult
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  version: component-test
componentRefs:
  - name: ${COMPONENT}
    namespace: ${HELM_NAMESPACE}
    type: ${chart_type}
    source: ${chart_source}
EOF

if [[ -n "$chart_name" ]]; then
    echo "    chart: ${chart_name}" >> "${WORK_DIR}/recipe.yaml"
fi

if [[ -n "$chart_version" ]]; then
    echo "    version: ${chart_version}" >> "${WORK_DIR}/recipe.yaml"
fi

if [[ -n "$values_file" ]]; then
    echo "    valuesFile: ${values_file}" >> "${WORK_DIR}/recipe.yaml"
fi

log_info "Recipe:"
cat "${WORK_DIR}/recipe.yaml"

# Generate bundle
log_info "Generating bundle..."
if ! "$AICR_BIN" bundle \
    --recipe "${WORK_DIR}/recipe.yaml" \
    --output "${WORK_DIR}/bundle" 2>&1; then
    log_error "Bundle generation failed"
    exit 1
fi

if [[ ! -d "${WORK_DIR}/bundle" ]]; then
    log_error "Bundle directory not created"
    exit 1
fi

log_info "Bundle contents:"
ls -1 "${WORK_DIR}/bundle"

# Deploy using the generated deploy.sh script (same approach as KWOK validation)
DEPLOY_SCRIPT="${WORK_DIR}/bundle/deploy.sh"
if [[ ! -f "$DEPLOY_SCRIPT" ]]; then
    log_error "deploy.sh not found in bundle directory"
    log_error "Bundle generation may have failed"
    exit 1
fi

chmod +x "$DEPLOY_SCRIPT"

# Pass --no-wait to deploy.sh; readiness is verified by the health check step
DEPLOY_ARGS="--no-wait"

log_info "Installing $COMPONENT into namespace $HELM_NAMESPACE..."

if ! "$DEPLOY_SCRIPT" $DEPLOY_ARGS 2>&1; then
    log_error "Deploy script failed"
    log_error "Debug with: kubectl -n $HELM_NAMESPACE get pods"
    log_error "            kubectl -n $HELM_NAMESPACE describe pods"
    exit 1
fi

log_info "Component '$COMPONENT' deployed successfully in namespace '$HELM_NAMESPACE'"
