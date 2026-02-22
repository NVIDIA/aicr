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

# Validate Cluster Autoscaling (Karpenter + KWOK)
#
# This script validates the full metrics-driven GPU autoscaling chain:
#   GPU workload → DCGM metrics → Prometheus → prometheus-adapter (external metric)
#   → HPA scales Deployment → pending pods → Karpenter → KWOK nodes provisioned
#
# It installs Karpenter with the KWOK cloud provider, creates a NodePool,
# verifies external GPU metrics are available, deploys an HPA-driven test
# workload, and confirms that Karpenter provisions KWOK nodes for overflow
# pods. Finally, it tests scale-down consolidation.
#
# Prerequisites: kind cluster with GPU operator, prometheus, prometheus-adapter
#
# Environment variables:
#   KIND_CLUSTER_NAME  (required) - Name of the kind cluster
#   KARPENTER_VERSION  (optional) - Override version from .settings.yaml
#
# Usage:
#   export KIND_CLUSTER_NAME=gpu-inference-test
#   ./validate-cluster-autoscaling.sh              # Run everything (default)
#   ./validate-cluster-autoscaling.sh --setup      # Install Karpenter + create NodePool only
#   ./validate-cluster-autoscaling.sh --exercise   # Run metrics + autoscaling exercise only

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
MANIFESTS_DIR="${SCRIPT_DIR}/../manifests/karpenter"

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME must be set}"
KUBE_CTX="kind-${KIND_CLUSTER_NAME}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

# -------------------------------------------------------------------
# Step 1: Install Karpenter with KWOK provider
# -------------------------------------------------------------------
install_karpenter() {
    log_info "=== Installing Karpenter with KWOK provider ==="
    export KIND_CLUSTER_NAME
    export KARPENTER_VERSION="${KARPENTER_VERSION:-$(yq eval '.testing_tools.karpenter' "${REPO_ROOT}/.settings.yaml")}"
    bash "${SCRIPT_DIR}/install-karpenter-kwok.sh"
}

# -------------------------------------------------------------------
# Step 2: Create NodePool and KWOKNodeClass
# -------------------------------------------------------------------
create_nodepool() {
    log_info "=== Creating NodePool and KWOKNodeClass ==="
    kubectl --context="${KUBE_CTX}" apply -f "${MANIFESTS_DIR}/nodepool.yaml"
}

# -------------------------------------------------------------------
# Step 3: Verify external metrics API has GPU metrics
# -------------------------------------------------------------------
verify_external_metrics() {
    log_info "=== Verifying external metrics API has GPU metrics ==="

    # Phase 1: Wait for the metric name to be registered in the external metrics API.
    local ext_available=false
    for i in $(seq 1 12); do
        local ext_metrics
        ext_metrics=$(kubectl --context="${KUBE_CTX}" get --raw \
            /apis/external.metrics.k8s.io/v1beta1 2>/dev/null || true)
        if [[ -n "${ext_metrics}" ]] && echo "${ext_metrics}" | \
                jq -e '.resources[]? | select(.name=="dcgm_gpu_power_usage")' >/dev/null 2>&1; then
            log_info "External metric dcgm_gpu_power_usage is registered"
            ext_available=true
            break
        fi
        echo "Waiting for external metrics API... (${i}/12)"
        sleep 10
    done

    if [[ "${ext_available}" != "true" ]]; then
        log_error "External metric dcgm_gpu_power_usage not available"
        kubectl --context="${KUBE_CTX}" get --raw \
            /apis/external.metrics.k8s.io/v1beta1 2>/dev/null | jq . || true
        exit 1
    fi

    # Phase 2: Wait for the metric to have actual data in Prometheus.
    # The metric name can be registered by prometheus-adapter before Prometheus
    # has scraped enough DCGM exporter samples. The HPA will fail with
    # "unable to fetch metrics" if we proceed before data exists.
    log_info "Waiting for external metric to have data..."
    local has_data=false
    for i in $(seq 1 24); do
        local ext_value
        ext_value=$(kubectl --context="${KUBE_CTX}" get --raw \
            "/apis/external.metrics.k8s.io/v1beta1/namespaces/default/dcgm_gpu_power_usage" 2>/dev/null || true)
        local metric_val
        metric_val=$(echo "${ext_value}" | jq -r '.items[0].value // empty' 2>/dev/null || true)
        if [[ -n "${metric_val}" ]]; then
            log_info "External metric has data: dcgm_gpu_power_usage=${metric_val}"
            has_data=true
            break
        fi
        echo "Waiting for Prometheus to collect DCGM metrics... (${i}/24)"
        sleep 10
    done

    if [[ "${has_data}" != "true" ]]; then
        log_error "External metric dcgm_gpu_power_usage has no data after 4 minutes"

        # Diagnostic 1: Raw external metrics API responses for BOTH metrics
        log_warn "--- External API: dcgm_gpu_power_usage ---"
        kubectl --context="${KUBE_CTX}" get --raw \
            "/apis/external.metrics.k8s.io/v1beta1/namespaces/default/dcgm_gpu_power_usage" 2>&1 || true
        log_warn "--- External API: dcgm_gpu_utilization ---"
        kubectl --context="${KUBE_CTX}" get --raw \
            "/apis/external.metrics.k8s.io/v1beta1/namespaces/default/dcgm_gpu_utilization" 2>&1 || true

        # Diagnostic 2: Run the exact PromQL the adapter would execute
        local prom_svc="http://kube-prometheus-prometheus.monitoring.svc:9090"
        log_warn "--- Prometheus: exact adapter PromQL ---"
        kubectl --context="${KUBE_CTX}" -n monitoring run prom-diag --rm -i --restart=Never \
            --image=curlimages/curl:latest -- sh -c "
                echo 'Raw series count:';
                curl -sf '${prom_svc}/api/v1/query?query=DCGM_FI_DEV_POWER_USAGE' | head -c 500;
                echo;
                echo 'Adapter metricsQuery result:';
                curl -sf '${prom_svc}/api/v1/query?query=avg(avg_over_time(DCGM_FI_DEV_POWER_USAGE%5B2m%5D))' | head -c 500;
                echo;
            " 2>/dev/null || true

        # Diagnostic 3: Full externalRules from ConfigMap (not truncated)
        log_warn "--- prometheus-adapter externalRules (full) ---"
        kubectl --context="${KUBE_CTX}" -n monitoring get configmap -l app.kubernetes.io/name=prometheus-adapter \
            -o jsonpath='{.items[0].data.config\.yaml}' 2>/dev/null \
            | python3 -c "import sys,yaml; cfg=yaml.safe_load(sys.stdin); print(yaml.dump(cfg.get('externalRules', 'MISSING')))" 2>/dev/null \
            || kubectl --context="${KUBE_CTX}" -n monitoring get configmap -l app.kubernetes.io/name=prometheus-adapter \
                -o jsonpath='{.items[0].data.config\.yaml}' 2>/dev/null | grep -A20 'externalRules' || echo "No externalRules found"

        # Diagnostic 4: Adapter deployment args (verify metricsRelistInterval)
        log_warn "--- prometheus-adapter container args ---"
        kubectl --context="${KUBE_CTX}" -n monitoring get deployment -l app.kubernetes.io/name=prometheus-adapter \
            -o jsonpath='{.items[0].spec.template.spec.containers[0].args}' 2>/dev/null || true
        echo

        # Diagnostic 5: prometheus-adapter logs (last 30 lines)
        log_warn "--- prometheus-adapter logs (tail) ---"
        kubectl --context="${KUBE_CTX}" -n monitoring logs deployment/prometheus-adapter --tail=30 2>/dev/null || true

        exit 1
    fi
}

# -------------------------------------------------------------------
# Step 4: Deploy HPA-driven autoscaling test
# -------------------------------------------------------------------
deploy_test_workload() {
    log_info "=== Deploying HPA-driven autoscaling test ==="
    kubectl --context="${KUBE_CTX}" create namespace autoscaling-test
    kubectl --context="${KUBE_CTX}" apply -f "${MANIFESTS_DIR}/hpa-gpu-scale-test.yaml"
}

# -------------------------------------------------------------------
# Step 5: Wait for HPA to read metrics and scale
# -------------------------------------------------------------------
wait_for_hpa_scale() {
    log_info "=== Waiting for HPA to read metrics and scale ==="
    local hpa_scaled=false
    for i in $(seq 1 20); do
        local desired current metrics
        desired=$(kubectl --context="${KUBE_CTX}" -n autoscaling-test \
            get hpa gpu-overflow-hpa -o jsonpath='{.status.desiredReplicas}' 2>/dev/null || true)
        current=$(kubectl --context="${KUBE_CTX}" -n autoscaling-test \
            get hpa gpu-overflow-hpa -o jsonpath='{.status.currentReplicas}' 2>/dev/null || true)
        metrics=$(kubectl --context="${KUBE_CTX}" -n autoscaling-test \
            get hpa gpu-overflow-hpa -o jsonpath='{.status.currentMetrics}' 2>/dev/null || true)

        if [[ -n "${desired}" && "${desired}" -gt 1 ]]; then
            log_info "HPA scaled: desired=${desired} current=${current}"
            log_info "HPA metrics: ${metrics}"
            hpa_scaled=true
            break
        fi
        echo "Waiting for HPA to compute scaling decision... desired=${desired:-?} (${i}/20)"
        sleep 15
    done

    if [[ "${hpa_scaled}" != "true" ]]; then
        log_error "HPA did not scale beyond 1 replica"
        kubectl --context="${KUBE_CTX}" -n autoscaling-test describe hpa gpu-overflow-hpa 2>/dev/null || true
        exit 1
    fi
}

# -------------------------------------------------------------------
# Step 6: Wait for Karpenter to provision KWOK nodes
# -------------------------------------------------------------------
wait_for_kwok_nodes() {
    log_info "=== Waiting for Karpenter to provision KWOK nodes ==="
    KWOK_NODES=0
    for i in $(seq 1 30); do
        KWOK_NODES=$(kubectl --context="${KUBE_CTX}" get nodes \
            -l karpenter.sh/nodepool=gpu-autoscaling-test --no-headers 2>/dev/null | wc -l | tr -d ' ')
        if [[ "${KWOK_NODES}" -gt 0 ]]; then
            log_info "Karpenter provisioned ${KWOK_NODES} KWOK GPU node(s)"
            break
        fi
        echo "Waiting for Karpenter to provision nodes... (${i}/30)"
        sleep 10
    done

    if [[ "${KWOK_NODES}" -eq 0 ]]; then
        log_error "Karpenter did not provision GPU nodes"
        kubectl --context="${KUBE_CTX}" -n karpenter logs deployment/karpenter --tail=50 2>/dev/null || true
        exit 1
    fi

    log_info "=== Verifying nodes have GPU capacity ==="
    kubectl --context="${KUBE_CTX}" get nodes \
        -l karpenter.sh/nodepool=gpu-autoscaling-test \
        -o jsonpath='{range .items[*]}{.metadata.name}: nvidia.com/gpu={.status.capacity.nvidia\.com/gpu}{"\n"}{end}'
}

# -------------------------------------------------------------------
# Step 7: Verify pods scheduled onto KWOK nodes
# -------------------------------------------------------------------
verify_pods_scheduled() {
    log_info "=== Verifying pods scheduled onto KWOK nodes ==="
    local scheduled=0
    local total=0
    for i in $(seq 1 20); do
        scheduled=$(kubectl --context="${KUBE_CTX}" get pods -n autoscaling-test \
            --field-selector=status.phase!=Pending --no-headers 2>/dev/null | wc -l | tr -d ' ')
        total=$(kubectl --context="${KUBE_CTX}" get pods -n autoscaling-test \
            --no-headers 2>/dev/null | wc -l | tr -d ' ')
        if [[ "${scheduled}" -eq "${total}" && "${total}" -gt 1 ]]; then
            log_info "All ${total} GPU pods scheduled successfully (HPA-driven)"
            break
        fi
        echo "Waiting for pods to schedule... (${scheduled}/${total}, attempt ${i}/20)"
        sleep 5
    done

    if [[ "${total}" -le 1 ]]; then
        log_error "HPA did not create additional replicas"
        kubectl --context="${KUBE_CTX}" -n autoscaling-test describe hpa gpu-overflow-hpa 2>/dev/null || true
        exit 1
    fi

    log_info "=== Full chain verified ==="
    echo "  GPU metrics → Prometheus → external metrics API → HPA → Deployment scaled"
    echo "  → pending pods → Karpenter → ${KWOK_NODES} KWOK node(s) → ${total} pods scheduled"
}

# -------------------------------------------------------------------
# Step 8: Test scale-down (consolidation)
# -------------------------------------------------------------------
test_consolidation() {
    log_info "=== Testing scale-down (consolidation) ==="
    kubectl --context="${KUBE_CTX}" delete namespace autoscaling-test --wait=false
    sleep 15
    local kwok_remaining=0
    for i in $(seq 1 12); do
        kwok_remaining=$(kubectl --context="${KUBE_CTX}" get nodes \
            -l karpenter.sh/nodepool=gpu-autoscaling-test --no-headers 2>/dev/null | wc -l | tr -d ' ')
        if [[ "${kwok_remaining}" -eq 0 ]]; then
            log_info "Karpenter consolidated all KWOK nodes (scale to zero)"
            break
        fi
        echo "Waiting for consolidation... (${kwok_remaining} nodes remaining, ${i}/12)"
        sleep 10
    done

    if [[ "${kwok_remaining}" -gt 0 ]]; then
        log_warn "Karpenter did not consolidate all nodes (${kwok_remaining} remaining)"
    fi
}

# -------------------------------------------------------------------
# Main
# -------------------------------------------------------------------
main() {
    local mode="all"
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --setup)    mode="setup"; shift ;;
            --exercise) mode="exercise"; shift ;;
            *)          echo "Unknown option: $1"; exit 1 ;;
        esac
    done

    log_info "=== Cluster Autoscaling (mode=${mode}) ==="
    log_info "Kind cluster: ${KIND_CLUSTER_NAME}"

    if [[ "${mode}" == "all" || "${mode}" == "setup" ]]; then
        install_karpenter
        create_nodepool
    fi

    if [[ "${mode}" == "all" || "${mode}" == "exercise" ]]; then
        verify_external_metrics
        deploy_test_workload
        wait_for_hpa_scale
        wait_for_kwok_nodes
        verify_pods_scheduled
        test_consolidation
    fi

    log_info "=== Cluster autoscaling validation PASSED ==="
}

main "$@"
