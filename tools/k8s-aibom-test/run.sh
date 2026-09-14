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

# k8s-aibom behavior test (ADR-019 / #2241)
#
# The generic component harness stops after the read-only health check, so it
# proves the controller is up but never that it produces a correct AIBOM. This
# adds the behavioral half, and ONLY that half: cluster lifecycle, bundling,
# installation, and the health check are delegated to tools/component-test so
# there is one implementation of each.

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/../.." && pwd)

# shellcheck source=../common
. "${REPO_ROOT}/tools/common"

has_tools go jq kind kubectl yq

COMPONENT=k8s-aibom
FIXTURE_NS=aicr-aibom-test
FIXTURE_DEPLOYMENT=aicr-aibom-fixture
AIBOM_NAME=apps-deployment-aicr-aibom-fixture
CONTROLLER_NS=k8s-aibom-system

# Scheduling inputs. The node label is applied to every node below so the
# controller still schedules on a single-node Kind cluster; the point is not to
# constrain placement but to prove the registry's declared value paths are the
# ones the upstream chart actually reads.
SCHED_KEY=dedicated
SCHED_VALUE=aicr-system

OUTPUT_DIR=${OUTPUT_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/aicr-aibom-evidence.XXXXXX")}
mkdir -p "${OUTPUT_DIR}"

# The Kind context this test is allowed to touch. Every destructive step is
# gated on the active context matching it exactly.
CLUSTER_NAME=$(yq -r '.testing.component_test.cluster_name // "aicr-component-test"' \
    "${REPO_ROOT}/.settings.yaml")
EXPECTED_CONTEXT="kind-${CLUSTER_NAME}"

fail() {
    log_error "$*"
    exit 1
}

# require_expected_context refuses to proceed unless kubectl is pointed at the
# disposable Kind cluster. Both the test body and the cleanup trap call it:
# cleanup uninstalls a Helm release and deletes a namespace, and the shared
# harness switches the caller's kubeconfig context rather than using a private
# one, so an unguarded cleanup would run those deletions against whatever
# cluster the caller happened to be pointed at.
require_expected_context() {
    local current
    current=$(kubectl config current-context 2>/dev/null || true)
    [[ "${current}" == "${EXPECTED_CONTEXT}" ]] || return 1
}

cleanup() {
    local rc=$?
    # The evidence directory is never removed, on success or failure: the
    # AIBOM and BOM captures are the only record of why an assertion failed,
    # and a cleanup that deletes them makes a failed run undiagnosable. Say
    # where they are whenever the run did not succeed.
    if [[ "${rc}" -ne 0 ]]; then
        log_warning "Run failed; evidence retained in ${OUTPUT_DIR}"
    fi
    if [[ "${KEEP_CLUSTER:-false}" == "true" ]]; then
        log_warning "KEEP_CLUSTER=true; leaving the cluster and ${FIXTURE_NS} in place"
        log_warning "Evidence: ${OUTPUT_DIR}"
        return "${rc}"
    fi
    # Fail safe, not open: if the context is not the disposable Kind cluster,
    # skip teardown entirely and say so, rather than deleting from whatever
    # cluster is active.
    if ! require_expected_context; then
        log_warning "Active context is not ${EXPECTED_CONTEXT}; skipping teardown"
        log_warning "If the test cluster still exists, clean it up with: make component-cleanup COMPONENT=${COMPONENT}"
        return "${rc}"
    fi
    kubectl delete namespace "${FIXTURE_NS}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    COMPONENT="${COMPONENT}" bash "${REPO_ROOT}/tools/component-test/cleanup.sh" >/dev/null 2>&1 || true
    return "${rc}"
}

# wait_for_aibom_generation blocks until the AIBOM's status has observed the
# given workload generation. Polling on observedGeneration rather than on Ready
# alone is what makes the later assertions describe the change under test: a
# Ready condition left over from the previous generation would otherwise
# satisfy a naive wait immediately after an update.
wait_for_aibom_generation() {
    local target=$1
    local observed
    for _ in $(seq 1 180); do
        observed=$(kubectl -n "${FIXTURE_NS}" get "aibom/${AIBOM_NAME}" \
            -o jsonpath='{.status.observedGeneration}' 2>/dev/null || true)
        if [[ "${observed}" == "${target}" ]]; then
            return 0
        fi
        sleep 1
    done
    return 1
}

wait_for_aibom_ready() {
    for _ in $(seq 1 180); do
        if kubectl -n "${FIXTURE_NS}" get "aibom/${AIBOM_NAME}" -o json 2>/dev/null \
            | jq -e '.status.conditions[]? | select(.type == "Ready" and .status == "True")' >/dev/null; then
            return 0
        fi
        sleep 1
    done
    return 1
}

workload_generation() {
    kubectl -n "${FIXTURE_NS}" get "deployment/${FIXTURE_DEPLOYMENT}" \
        -o jsonpath='{.metadata.generation}'
}

# capture_aibom writes the AIBOM to the evidence dir, decodes its inline
# CycloneDX document, checks the document hash the controller published against
# the bytes it actually emitted, and validates the document. Echoes the
# inputHash so callers can compare across reconciles.
capture_aibom() {
    local label=$1
    local aibom_json=${OUTPUT_DIR}/aibom-${label}.json
    local bom_json=${OUTPUT_DIR}/bom-${label}.json

    kubectl -n "${FIXTURE_NS}" get "aibom/${AIBOM_NAME}" -o json >"${aibom_json}"

    jq -r '.status.bomDocument.inline.data' "${aibom_json}" | decode_base64 >"${bom_json}"

    local want_sha actual_sha
    want_sha=$(jq -r '.status.bomDocument.sha256' "${aibom_json}")
    actual_sha=$(sha256_file "${bom_json}")
    [[ "${actual_sha}" == "${want_sha}" ]] \
        || fail "${label}: canonical BOM SHA mismatch: got ${actual_sha}, want ${want_sha}"

    # Redirect the validator's stdout into the evidence dir. This function's
    # stdout IS its return value — the caller reads it through command
    # substitution — so anything the validator prints would be concatenated
    # ahead of the hash. That is not cosmetic: the validator's banner embeds
    # the component count, so the "digest change moved inputHash" assertion
    # could be satisfied by a differing banner while inputHash never moved.
    go run "${SCRIPT_DIR}/validate-bom" "${bom_json}" \
        >"${OUTPUT_DIR}/cyclonedx-${label}.txt" 2>&1 \
        || {
            cat "${OUTPUT_DIR}/cyclonedx-${label}.txt" >&2
            fail "${label}: AIBOM document failed CycloneDX validation"
        }

    jq -r '.status.inputHash' "${aibom_json}"
}

# ---------------------------------------------------------------------------
# 1. Cluster, install, and the read-only health check — all delegated.
# ---------------------------------------------------------------------------

# Pin every kubectl, helm, and kind call in this run — and in the helper
# scripts it invokes — to a private kubeconfig, BEFORE the helper runs.
#
# A one-time current-context check is not sufficient. The context is process-
# external mutable state: ensure-cluster.sh calls `kubectl config use-context`
# on the caller's shared kubeconfig, and any concurrent session that switches
# context mid-run would silently redirect the node labeling, the Helm install
# with its cluster-scoped CRDs and RBAC, the fixture apply, and the teardown
# to whatever cluster is active at that instant. A private KUBECONFIG closes
# the window for the whole run rather than at one instant, and satisfies
# #2241's "touches no other cluster context" literally.
#
# Seeding matters: when the cluster already exists the private file must be
# populated first or ensure-cluster.sh's `use-context` finds no such context.
# When it does not exist, `kind create` writes into $KUBECONFIG itself.
export KUBECONFIG="${OUTPUT_DIR}/kubeconfig"
if kind get clusters 2>/dev/null | grep -qxF "${CLUSTER_NAME}"; then
    kind export kubeconfig --name "${CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" >/dev/null
fi

log_info "Ensuring the component-test Kind cluster (private kubeconfig: ${KUBECONFIG})"
TIER=deploy bash "${REPO_ROOT}/tools/component-test/ensure-cluster.sh"

# Arm teardown only now. Before this point nothing has been created, and a trap
# armed earlier would run its deletions against a cluster this script never
# touched if setup failed.
require_expected_context \
    || fail "expected context ${EXPECTED_CONTEXT} after ensure-cluster.sh, got $(kubectl config current-context 2>/dev/null || echo none)"
trap cleanup EXIT

# Must precede the install: the bundle sets a nodeSelector, so an unlabeled
# node would leave the controller Pending and the health check would fail for
# a reason unrelated to what is under test.
kubectl label node --all "${SCHED_KEY}=${SCHED_VALUE}" --overwrite >/dev/null

log_info "Installing ${COMPONENT} through the AICR single-component bundle path"
COMPONENT="${COMPONENT}" \
    BUNDLE_EXTRA_ARGS="--system-node-selector ${SCHED_KEY}=${SCHED_VALUE} --system-node-toleration ${SCHED_KEY}=${SCHED_VALUE}:NoSchedule" \
    bash "${REPO_ROOT}/tools/component-test/deploy-component.sh"

log_info "Running the registry-declared health check"
COMPONENT="${COMPONENT}" bash "${REPO_ROOT}/tools/component-test/run-health-check.sh"

# ---------------------------------------------------------------------------
# 2. Scheduling path correctness against the REAL chart.
#
# The hermetic half of this claim — that the bundler injects system scheduling
# at the value paths recipes/registry.yaml declares — is asserted by
# TestK8sAIBOM_SystemSchedulingInjectionPaths. What a unit test cannot know is
# whether those paths are the ones the upstream chart reads. If the registry
# named a path the chart ignores, the values would render correctly and land
# nowhere; only the rendered Deployment can tell the difference.
# ---------------------------------------------------------------------------

log_info "Asserting system scheduling reached the controller Deployment"
kubectl -n "${CONTROLLER_NS}" get deployment/k8s-aibom -o json >"${OUTPUT_DIR}/controller-deployment.json"
jq -e --arg k "${SCHED_KEY}" --arg v "${SCHED_VALUE}" '
  (.spec.template.spec.nodeSelector[$k] == $v) and
  ([.spec.template.spec.tolerations[]? | select(.key == $k and .value == $v and .effect == "NoSchedule")] | length == 1)
' "${OUTPUT_DIR}/controller-deployment.json" >/dev/null \
    || fail "registry nodeScheduling paths did not reach the controller Deployment"

# ---------------------------------------------------------------------------
# 3. AIBOM generation, reconciliation, hashing, and garbage collection.
# ---------------------------------------------------------------------------

log_info "Creating the opted-in fixture namespace and workload"
kubectl apply -f "${SCRIPT_DIR}/workload-v1.yaml"

wait_for_aibom_ready || fail "initial AIBOM did not become Ready"

deployment_uid=$(kubectl -n "${FIXTURE_NS}" get "deployment/${FIXTURE_DEPLOYMENT}" \
    -o jsonpath='{.metadata.uid}')

input_hash_v1=$(capture_aibom v1)

jq -e --arg uid "${deployment_uid}" \
    '.metadata.ownerReferences | any(.uid == $uid and .controller == true)' \
    "${OUTPUT_DIR}/aibom-v1.json" >/dev/null \
    || fail "AIBOM controller ownerReference does not match the Deployment"

jq -e '
  .status.bomDocument.format == "CycloneDX" and
  .status.bomDocument.specVersion == "1.6" and
  .status.summary.workload.kind == "Deployment" and
  .status.summary.runtime.name == "triton"
' "${OUTPUT_DIR}/aibom-v1.json" >/dev/null \
    || fail "initial AIBOM status contract failed"

log_info "Reconciling a cosmetic change: inputHash must not move"
kubectl -n "${FIXTURE_NS}" patch "deployment/${FIXTURE_DEPLOYMENT}" --type merge \
    -p '{"spec":{"template":{"metadata":{"annotations":{"aicr.nvidia.com/test-reconcile":"cosmetic"}}}}}'
wait_for_aibom_generation "$(workload_generation)" \
    || fail "AIBOM did not observe the cosmetic workload generation"
input_hash_cosmetic=$(capture_aibom cosmetic)
[[ "${input_hash_cosmetic}" == "${input_hash_v1}" ]] \
    || fail "cosmetic workload change altered inputHash"

log_info "Applying a relevant change: inputHash must move"
kubectl apply -f "${SCRIPT_DIR}/workload-v2.yaml"
wait_for_aibom_generation "$(workload_generation)" \
    || fail "AIBOM did not observe the relevant workload generation"
wait_for_aibom_ready || fail "updated AIBOM did not become Ready"
input_hash_v2=$(capture_aibom v2)
[[ "${input_hash_v2}" != "${input_hash_v1}" ]] \
    || fail "digest-pinned image update did not alter inputHash"

log_info "Deleting the workload: its AIBOM must be garbage-collected"
kubectl -n "${FIXTURE_NS}" delete "deployment/${FIXTURE_DEPLOYMENT}" --wait=true
kubectl -n "${FIXTURE_NS}" wait --for=delete "aibom/${AIBOM_NAME}" --timeout=120s

{
    printf 'PASS\n'
    printf 'input_hash_v1=%s\n' "${input_hash_v1}"
    printf 'input_hash_cosmetic=%s\n' "${input_hash_cosmetic}"
    printf 'input_hash_v2=%s\n' "${input_hash_v2}"
} | tee "${OUTPUT_DIR}/result.txt"
