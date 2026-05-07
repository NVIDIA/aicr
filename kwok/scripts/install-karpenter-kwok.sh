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

# Install Karpenter with KWOK cloud provider into a kind cluster.
#
# This script sets up Karpenter to use simulated (KWOK) nodes for testing
# GPU cluster autoscaling without real cloud infrastructure. It:
#   1. Installs KWOK controller via Helm
#   2. Clones Karpenter and builds the KWOK provider image via ko
#   3. Creates GPU instance types ConfigMap
#   4. Deploys Karpenter via Helm with the locally-built image and instance types
#
# Prerequisites: Go, ko, Helm, kubectl, kind cluster running
#
# Environment variables:
#   KIND_CLUSTER_NAME  (required) - Name of the kind cluster
#   KARPENTER_VERSION  (optional) - Karpenter version tag (default: v1.8.0)
#
# Usage:
#   export KIND_CLUSTER_NAME=gpu-inference-test
#   ./install-karpenter-kwok.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS_DIR="${SCRIPT_DIR}/../manifests/karpenter"

KARPENTER_VERSION="${KARPENTER_VERSION:-v1.8.0}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME must be set}"
KARPENTER_NAMESPACE="${KARPENTER_NAMESPACE:-karpenter}"
KARPENTER_CLONE_DIR="${KARPENTER_CLONE_DIR:-/tmp/karpenter}"
KWOK_HELM_TIMEOUT="${KWOK_HELM_TIMEOUT:-300s}"
KO_BUILD_TIMEOUT="${KO_BUILD_TIMEOUT:-900s}"  # 15 minutes
KARPENTER_HELM_TIMEOUT="${KARPENTER_HELM_TIMEOUT:-300s}"
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-${KIND_CLUSTER_NAME}}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

kubectl_kind() {
    kubectl --context="${KUBE_CONTEXT}" "$@"
}

helm_kind() {
    helm --kube-context "${KUBE_CONTEXT}" "$@"
}

# -------------------------------------------------------------------
# Step 1: Install KWOK controller
# Uses the same approach as kwok/scripts/run-all-recipes.sh
# -------------------------------------------------------------------
install_kwok() {
    log_info "Installing KWOK controller..."

    if kubectl_kind get deployment -n kube-system kwok-controller &>/dev/null; then
        log_info "KWOK controller already installed, skipping"
        return 0
    fi

    helm repo add kwok https://kwok.sigs.k8s.io/charts/ --force-update
    helm_kind upgrade --install kwok-controller kwok/kwok \
        --namespace kube-system \
        --set hostNetwork=true \
        --wait --timeout "${KWOK_HELM_TIMEOUT}"

    helm_kind upgrade --install kwok-stage-fast kwok/stage-fast \
        --namespace kube-system \
        --wait --timeout "${KWOK_HELM_TIMEOUT}"

    log_info "KWOK controller installed"
}

# -------------------------------------------------------------------
# Step 2: Clone and build Karpenter KWOK provider
# Uses ko to build the Go binary into a container image and
# side-load it directly into the kind cluster (kind.local).
# -------------------------------------------------------------------
build_karpenter() {
    log_info "Building Karpenter KWOK provider ${KARPENTER_VERSION}..."

    if [[ -d "${KARPENTER_CLONE_DIR}" ]]; then
        log_info "Removing previous Karpenter clone"
        rm -rf "${KARPENTER_CLONE_DIR}"
    fi

    git clone --depth 1 --branch "${KARPENTER_VERSION}" \
        https://github.com/kubernetes-sigs/karpenter.git "${KARPENTER_CLONE_DIR}"

    pushd "${KARPENTER_CLONE_DIR}" >/dev/null

    # ko build with kind.local side-loads the image directly into the kind cluster.
    # Redirect stderr to avoid Go compilation warnings corrupting the image reference.
    # Output format: kind.local/<name>:<content-hash>
    # Hard timeout prevents a slow/stuck compilation from consuming the entire job.
    local ko_stderr="${KARPENTER_CLONE_DIR}/ko-build.stderr"
    CONTROLLER_IMG=$(timeout "${KO_BUILD_TIMEOUT}" \
        env KO_DOCKER_REPO=kind.local \
        KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" \
        ko build sigs.k8s.io/karpenter/kwok 2>"${ko_stderr}") || {
        log_error "ko build failed or timed out after ${KO_BUILD_TIMEOUT}"
        if [[ -s "${ko_stderr}" ]]; then
            log_error "ko build stderr:"
            sed 's/^/  /' "${ko_stderr}" || true
        fi
        exit 1
    }

    popd >/dev/null

    # Validate the captured image reference
    if [[ ! "${CONTROLLER_IMG}" =~ ^kind\.local/ ]]; then
        log_error "ko build produced unexpected output: ${CONTROLLER_IMG}"
        exit 1
    fi

    # Extract repository and tag from the ko output.
    # ko outputs "kind.local/<name>:<hash>" — split on the first colon after the repo.
    if [[ "${CONTROLLER_IMG}" == *":"* ]]; then
        IMG_REPOSITORY="${CONTROLLER_IMG%%:*}"
        IMG_TAG="${CONTROLLER_IMG#*:}"
    else
        IMG_REPOSITORY="${CONTROLLER_IMG}"
        IMG_TAG=""
    fi

    log_info "Built image: ${CONTROLLER_IMG}"
    log_info "Repository: ${IMG_REPOSITORY}"
    log_info "Tag: ${IMG_TAG:-<none>}"

    # Export for use in deploy step
    export CONTROLLER_IMG IMG_REPOSITORY IMG_TAG
}

# -------------------------------------------------------------------
# Step 3: Deploy Karpenter via Helm
# Creates the instance types ConfigMap first, then deploys Karpenter
# with volume mounts and env vars configured via Helm values.
# -------------------------------------------------------------------
deploy_karpenter() {
    log_info "Deploying Karpenter to namespace ${KARPENTER_NAMESPACE}..."

    # Apply CRDs first
    kubectl_kind apply -f "${KARPENTER_CLONE_DIR}/kwok/charts/crds"

    # Create namespace and instance types ConfigMap before Helm install
    # so the volume mount can reference it immediately.
    kubectl_kind create namespace "${KARPENTER_NAMESPACE}" --dry-run=client -o yaml | kubectl_kind apply -f -

    local instance_types_file="${MANIFESTS_DIR}/instance-types.json"
    if [[ ! -f "${instance_types_file}" ]]; then
        log_error "Instance types file not found: ${instance_types_file}"
        exit 1
    fi
    kubectl_kind create configmap -n "${KARPENTER_NAMESPACE}" karpenter-instance-types \
        --from-file=instance-types.json="${instance_types_file}" \
        --dry-run=client -o yaml | kubectl_kind apply -f -

    # Build the image tag argument. If ko provided a tag, use it.
    # If not, omit it and let the chart default to its AppVersion.
    local tag_arg=""
    if [[ -n "${IMG_TAG}" ]]; then
        tag_arg="--set controller.image.tag=${IMG_TAG}"
    fi

    # Deploy via Helm with the locally-built image.
    # - imagePullPolicy=Never: image is side-loaded into kind, no registry to pull from
    # - staticCapacity=true: required by the deployment template but missing from chart defaults
    # - extraVolumes + extraVolumeMounts: mount the instance types ConfigMap
    # - controller.env: set INSTANCE_TYPES_FILE_PATH for the KWOK provider
    # shellcheck disable=SC2086
    helm_kind upgrade --install karpenter "${KARPENTER_CLONE_DIR}/kwok/charts" \
        --namespace "${KARPENTER_NAMESPACE}" --create-namespace \
        --set controller.image.repository="${IMG_REPOSITORY}" \
        ${tag_arg} \
        --set imagePullPolicy=Never \
        --set logLevel=info \
        --set settings.featureGates.staticCapacity=true \
        --set controller.resources.requests.cpu=500m \
        --set controller.resources.requests.memory=512Mi \
        --set controller.resources.limits.cpu=1 \
        --set controller.resources.limits.memory=1Gi \
        --set 'extraVolumes[0].name=instance-types' \
        --set 'extraVolumes[0].configMap.name=karpenter-instance-types' \
        --set 'controller.extraVolumeMounts[0].name=instance-types' \
        --set 'controller.extraVolumeMounts[0].mountPath=/etc/karpenter/instance-types' \
        --set 'controller.extraVolumeMounts[0].readOnly=true' \
        --set 'controller.env[0].name=INSTANCE_TYPES_FILE_PATH' \
        --set 'controller.env[0].value=/etc/karpenter/instance-types/instance-types.json' \
        --wait --timeout "${KARPENTER_HELM_TIMEOUT}" \
        || {
            log_error "Helm install failed. Diagnostics:"
            kubectl_kind -n "${KARPENTER_NAMESPACE}" get pods -o wide 2>/dev/null || true
            kubectl_kind -n "${KARPENTER_NAMESPACE}" describe deployment karpenter 2>/dev/null || true
            local POD
            POD=$(kubectl_kind -n "${KARPENTER_NAMESPACE}" get pods -l app.kubernetes.io/name=karpenter \
                -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
            if [[ -n "${POD}" ]]; then
                kubectl_kind -n "${KARPENTER_NAMESPACE}" describe pod "${POD}" 2>/dev/null || true
                kubectl_kind -n "${KARPENTER_NAMESPACE}" logs "${POD}" --tail=50 2>/dev/null || true
            fi
            exit 1
        }

    log_info "Karpenter deployed with GPU instance types configured"
}

# -------------------------------------------------------------------
# Main
# -------------------------------------------------------------------
main() {
    log_info "=== Karpenter KWOK Provider Installation ==="
    log_info "Karpenter version: ${KARPENTER_VERSION}"
    log_info "Kind cluster: ${KIND_CLUSTER_NAME}"
    log_info "Kube context: ${KUBE_CONTEXT}"
    log_info "Namespace: ${KARPENTER_NAMESPACE}"
    log_info "Timeouts: kwok=${KWOK_HELM_TIMEOUT} ko-build=${KO_BUILD_TIMEOUT} karpenter=${KARPENTER_HELM_TIMEOUT}"

    install_kwok
    build_karpenter
    deploy_karpenter

    log_info "=== Karpenter KWOK provider ready ==="
    log_info "Create a NodePool + KWOKNodeClass to start autoscaling"
}

main "$@"
