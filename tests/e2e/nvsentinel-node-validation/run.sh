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

# =============================================================================
# nvsentinel-node-validation mixin e2e (#3088)
# =============================================================================
#
# Bundle-only checks, then nvsentinel with the nvsentinel-node-validation mixin
# installed through the bundle's own install.sh on Kind:
#
#   1. the gate refuses new-node criteria that drop the cordoned criterion,
#      and newNodeTests naming a test nothing defines; it accepts the mixin
#      with the test stubbed;
#   2. nvsentinel-observe with nvsentinel-node-validation bundles, with the
#      gate's note that lifecycle-manager ignores dryRun;
#   3. cordoning the new GPU node gets it a ValidationRequest from
#      lifecycle-manager; it stays cordoned while the test runs, and the pass
#      uncordons it with uncordon-reason=ValidationSucceeded;
#   4. a failing test keeps the node cordoned and labels it validation-failed;
#      deleting that ValidationRequest uncordons the node untested, which is the
#      operator's escape hatch.
#
# The mixin needs no datastore, so only prometheus-operator-crds, cert-manager
# (lifecycle-manager's webhook certificate) and nvsentinel are installed.
#
# WHAT KIND CANNOT DO, AND WHAT STANDS IN FOR IT:
#   - No GPUs and no DCGM hostengine (the kind overlay sets gpu-operator
#     dcgm.enabled: false, and this run does not install gpu-operator). So
#     dcgm-diag-test's command is replaced with a busybox stub that sleeps, so
#     the cordon can be seen held while it runs, then exits 0; stub-fail exits
#     1. Overriding the command is also what lets the gate skip its DCGM rule.
#   - No nvidia.com/gpu allocatable, so readinessCriteria keeps only
#     not-under-quarantine (as upstream's values-tilt.yaml does). Patching
#     allocatable into node status would not hold: the kubelet rewrites it.
#   - nvidia.com/gpu.present=true is set by hand, as the GPU Operator would.
#
# TEST-ONLY SETTINGS, passed on the bundle: the stubs and readinessCriteria
# above; newNodeValidation.batchPeriodSeconds=5 (chart: 600); and the Job
# provider at 0 retries and a 180s timeout (chart: 5 and 1800s), so a failure
# is final at once.
#
# ORDER MATTERS. NewNodeValidationRequested is set once and never reset, and a
# node older than the mixin's 30m recently-joined window is never a new node.
# Case 3 therefore runs first; case 4 cordons the node again and needs the
# new-node controller to have finished with it.
#
# PREREQUISITES: go, kind, kubectl, helm, yq, jq
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-3088-kind).
#                  Always recreated, then deleted at the end unless
#                  KEEP_CLUSTER=true.
#   KEEP_CLUSTER   Skip cluster teardown on exit (default: false).
#   AICR_BIN       Path to a prebuilt aicr binary (default: built fresh).
# =============================================================================

# node_has passes its single-quoted arguments to jq as filters.
# shellcheck disable=SC2016
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DIR}/../../.." && pwd)"
# shellcheck source=/dev/null
. "${ROOT}/tools/common"

CLUSTER_NAME="${CLUSTER_NAME:-aicr-3088-kind}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NODE="${CLUSTER_NAME}-control-plane"

# The digest-pinned multi-arch busybox AICR already ships.
BUSYBOX_IMAGE="docker.io/library/busybox:1.38.0@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e"
VR_RESOURCE="validationrequests.nvsentinel.nvidia.com"
CREATED_BY_LABEL="nvsentinel.nvidia.com/created-by"
NEW_NODE_CONDITION="NewNodeValidationRequested"
UNCORDON_REASON_LABEL="k8saas.nvidia.com/uncordon-reason"
VALIDATION_STATE_LABEL="nvsentinel.nvidia.com/validation-state"
STUB_FAIL_VR="aicr-e2e-stub-fail"
UNDEFINED_TEST="aicr-e2e-undefined"
# Long enough that the held cordon is observable before the stub passes.
STUB_SECONDS=20
JOB_TIMEOUT_SECONDS=180
NEW_NODE_BATCH_SECONDS=5
# The mixin's recently-joined criterion, and how much of it must remain when
# case 3 cordons the node.
NEW_NODE_WINDOW_SECONDS=1800
NEW_NODE_MARGIN_SECONDS=300
POLL_SECONDS=3
VR_APPEAR_WAIT_SECONDS=180
# Covers the stub image pull on a cold node as well as STUB_SECONDS.
VALIDATION_WAIT_SECONDS=300
RELEASE_WAIT_SECONDS=60
WEBHOOK_WAIT_SECONDS=120
FAILED_SETTLE_SECONDS=30
CERT_MANAGER_WAIT_SECONDS=600
NVSENTINEL_WAIT_SECONDS=600

WORK=""
CREATED_CLUSTER=false
BUNDLE_DIR=""
CRDS_DIR=""
CERT_MANAGER_DIR=""
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

cleanup() {
  local rc=$?
  # Cluster before workdir: $KUBECONFIG lives inside WORK.
  if [[ "${CREATED_CLUSTER}" == "true" && "${KEEP_CLUSTER}" != "true" ]]; then
    msg "Deleting Kind cluster ${CLUSTER_NAME}..."
    kind delete cluster --name "${CLUSTER_NAME}" &>/dev/null || true
  fi
  [[ -n "${WORK}" && -d "${WORK}" ]] && rm -rf "${WORK}"
  exit "${rc}"
}
trap cleanup EXIT

pass() { TOTAL_TESTS=$((TOTAL_TESTS + 1)); PASSED_TESTS=$((PASSED_TESTS + 1)); msg "PASS: $1"; }
fail() {
  TOTAL_TESTS=$((TOTAL_TESTS + 1))
  FAILED_TESTS=$((FAILED_TESTS + 1))
  msg "FAIL: $1 -- $2"
}

kc() { kubectl --context "${KUBE_CONTEXT}" "$@"; }

# poll runs the remaining arguments every POLL_SECONDS until they succeed,
# failing after $1 seconds.
poll() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@"; do
    ((SECONDS < deadline)) || return 1
    sleep "${POLL_SECONDS}"
  done
}

# ── Setup ──

build_binary() {
  if [[ -n "${AICR_BIN}" ]]; then
    msg "Using prebuilt AICR_BIN=${AICR_BIN}"
    return
  fi
  AICR_BIN="${WORK}/aicr"
  msg "Building aicr binary..."
  (cd "${ROOT}" && go build -o "${AICR_BIN}" ./cmd/aicr) || err "failed to build aicr binary"
}

# compose_recipe writes ${WORK}/recipe-$1.yaml: h100-kind-training with the
# remaining arguments as its mixins.
compose_recipe() {
  local name="$1" data_dir="${WORK}/data-$1" mixins
  shift
  mixins=$(jq -cn '$ARGS.positional' --args "$@")
  mkdir -p "${data_dir}/overlays"
  cat >"${data_dir}/registry.yaml" <<'REGISTRY_EOF'
kind: ComponentRegistry
apiVersion: aicr.run/v1beta1
metadata:
  name: nvsentinel-node-validation-e2e-registry
components: []
REGISTRY_EOF
  MIXINS="${mixins}" yq '.spec.mixins = env(MIXINS)' \
    "${ROOT}/recipes/overlays/h100-kind-training.yaml" \
    >"${data_dir}/overlays/h100-kind-training.yaml"
  "${AICR_BIN}" recipe --service kind --accelerator h100 --intent training \
    --data "${data_dir}" --output "${WORK}/recipe-${name}.yaml" >/dev/null ||
    err "recipe generation failed for mixins ${mixins}"
}

# bundle_to bundles recipe $1 into ${WORK}/bundle-$2 with the test-only
# settings and the remaining arguments, logging to ${WORK}/bundle-$2.log.
bundle_to() {
  local recipe="$1" out="$2" stub_pass stub_fail readiness
  shift 2
  stub_pass=$(jq -cn --arg image "${BUSYBOX_IMAGE}" --arg cmd "sleep ${STUB_SECONDS}; exit 0" \
    '{image: $image, command: ["sh", "-c", $cmd]}')
  stub_fail=$(jq -cn --arg image "${BUSYBOX_IMAGE}" '{
    provider: "k8s-job-provider", image: $image, command: ["sh", "-c", "exit 1"],
    supportsBatchingNodes: false, minimumNodesPerBatch: 1, batchFailurePolicy: "fail"}')
  readiness=$(jq -cn '[{
    name: "not-under-quarantine",
    expression: "!(has(node.metadata.annotations) && \"quarantineHealthEvent\" in node.metadata.annotations)"}]')
  rm -rf "${WORK}/bundle-${out}"
  "${AICR_BIN}" bundle -r "${WORK}/recipe-${recipe}.yaml" -o "${WORK}/bundle-${out}" \
    --set-json "nvsentinel:lifecycle-manager.config.tests.dcgm-diag-test=${stub_pass}" \
    --set-json "nvsentinel:lifecycle-manager.config.tests.stub-fail=${stub_fail}" \
    --set-json "nvsentinel:lifecycle-manager.config.readinessCriteria=${readiness}" \
    --set-json "nvsentinel:lifecycle-manager.config.newNodeValidation.batchPeriodSeconds=${NEW_NODE_BATCH_SECONDS}" \
    --set-json "nvsentinel:lifecycle-manager.config.providers.k8s-job-provider.retries=0" \
    --set-json "nvsentinel:lifecycle-manager.config.providers.k8s-job-provider.timeoutSeconds=${JOB_TIMEOUT_SECONDS}" \
    --system-node-selector kubernetes.io/os=linux \
    "$@" >"${WORK}/bundle-${out}.log" 2>&1
}

# expect_refusal asserts that bundling recipe $2 with the remaining arguments
# fails and names $3.
expect_refusal() {
  local name="$1" recipe="$2" want="$3"
  shift 3
  if bundle_to "${recipe}" refused "$@"; then
    fail "${name}" "the bundle was accepted"
  elif grep -qF -- "${want}" "${WORK}/bundle-refused.log"; then
    pass "${name}"
  else
    fail "${name}" "refused without naming '${want}': $(grep -m1 -i 'error' "${WORK}/bundle-refused.log" | cut -c1-400)"
  fi
}

# test_gate covers cases 1 and 2, and leaves the live bundle in
# ${WORK}/bundle-deploy.
test_gate() {
  msg "Cases 1-2: the bundle gate..."
  compose_recipe live nvsentinel-node-validation
  compose_recipe observe nvsentinel-observe nvsentinel-node-validation

  local criteria
  criteria=$(yq -o=json '.spec.componentRefs[0].overrides["lifecycle-manager"].config.newNodeValidation.criteria |
    map(select(.name != "cordoned"))' "${ROOT}/recipes/mixins/nvsentinel-node-validation.yaml" | jq -c .)
  [[ "$(jq length <<<"${criteria}")" -ge 1 ]] || err "could not read the mixin's new-node criteria"
  expect_refusal "#1 gate refuses new-node criteria without the cordoned criterion" \
    live "does not include a criterion named \"cordoned\" with the chart's expression" \
    --set-json "nvsentinel:lifecycle-manager.config.newNodeValidation.criteria=${criteria}"
  expect_refusal "#1 gate refuses newNodeTests naming an undefined test" \
    live "names the test \"${UNDEFINED_TEST}\", which is not defined" \
    --set-json "nvsentinel:lifecycle-manager.config.newNodeValidation.newNodeTests=[\"${UNDEFINED_TEST}\"]"

  bundle_to live deploy ||
    { tail -20 "${WORK}/bundle-deploy.log"; err "bundle generation failed for nvsentinel-node-validation"; }
  pass "#1 gate accepts nvsentinel-node-validation with stubbed tests"

  if bundle_to observe observe; then
    pass "#2 nvsentinel-observe with nvsentinel-node-validation bundles"
    if grep -qF "global.dryRun is true, but lifecycle-manager does not read it" "${WORK}/bundle-observe.log"; then
      pass "#2 the bundle warns that lifecycle-manager runs real diagnostics under dryRun"
    else
      fail "#2 dry-run warning" "the bundle log carries no dryRun warning from the gate"
    fi
  else
    fail "#2 nvsentinel-observe with nvsentinel-node-validation bundles" \
      "$(grep -m1 -i 'error' "${WORK}/bundle-observe.log" | cut -c1-400)"
  fi

  local bundle="${WORK}/bundle-deploy"
  BUNDLE_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-nvsentinel' -print -quit)
  CRDS_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-prometheus-operator-crds' -print -quit)
  CERT_MANAGER_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-cert-manager' -print -quit)
  [[ -n "${BUNDLE_DIR}" && -n "${CRDS_DIR}" && -n "${CERT_MANAGER_DIR}" ]] ||
    err "a bundled component directory is missing"

  # Fail here, not later as "nothing happened": a mixin that stopped composing
  # would otherwise read as a controller that never acted.
  local values="${BUNDLE_DIR}/values.yaml"
  [[ "$(yq '.global.lifecycleManager.enabled' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not enable lifecycle-manager -- composition regressed"
  [[ "$(yq '.["lifecycle-manager"].config.newNodeValidation.criteria[] | select(.name == "cordoned") | .name' "${values}")" == "cordoned" ]] ||
    err "bundle's new-node criteria lack the cordoned criterion -- composition regressed"
  [[ "$(yq '.["lifecycle-manager"].config.tests["dcgm-diag-test"].image' "${values}")" == "${BUSYBOX_IMAGE}" ]] ||
    err "the dcgm-diag-test stub did not reach the bundle"
  [[ "$(yq '.["lifecycle-manager"].config.tests["stub-fail"].command[2]' "${values}")" == "exit 1" ]] ||
    err "the stub-fail test did not reach the bundle"
}

ensure_cluster() {
  if grep -qx "${CLUSTER_NAME}" <<<"$(kind get clusters 2>/dev/null)"; then
    msg "Deleting stale Kind cluster ${CLUSTER_NAME}..."
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi
  msg "Creating Kind cluster ${CLUSTER_NAME}..."
  kind create cluster --name "${CLUSTER_NAME}" || err "kind create cluster failed"
  CREATED_CLUSTER=true
  kind export kubeconfig --name "${CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" ||
    err "failed to export kubeconfig for ${CLUSTER_NAME}"
  kc wait --for=condition=Ready node --all --timeout=120s || err "cluster nodes never became Ready"
  # Read by the mixin's gpu-present new-node criterion.
  kc label node "${NODE}" nvidia.com/gpu.present=true --overwrite >/dev/null
}

# wait_rollout waits until $1/$2 in namespace $3 has rolled out, failing at
# $4, a deadline in bash SECONDS.
wait_rollout() {
  local kind="$1" name="$2" ns="$3" deadline="$4" remaining
  until kc -n "${ns}" get "${kind}" "${name}" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || err "${kind}/${name} was never created in ${ns}"
    sleep 2
  done
  # kubectl reads --timeout=0s as "wait forever".
  remaining=$((deadline - SECONDS))
  ((remaining > 0)) || err "${kind}/${name} never became Ready in ${ns}"
  kc -n "${ns}" rollout status "${kind}/${name}" --timeout="${remaining}s" ||
    err "${kind}/${name} never became Ready in ${ns}"
}

install_component() {
  (cd "$1" && chmod +x install.sh && KUBE_CONTEXT="${KUBE_CONTEXT}" ./install.sh) ||
    err "$(basename "$1") install failed"
}

install_bundle() {
  msg "Installing prometheus-operator-crds, cert-manager and nvsentinel from the bundle..."
  local dir d deadline
  # CRDs first: cert-manager's chart renders a ServiceMonitor, nvsentinel's a
  # PodMonitor.
  for dir in "${CRDS_DIR}" "${CERT_MANAGER_DIR}"; do
    install_component "${dir}"
  done
  # lifecycle-manager's webhook certificate is issued by cert-manager.
  deadline=$((SECONDS + CERT_MANAGER_WAIT_SECONDS))
  for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
    wait_rollout deployment "${d}" cert-manager "${deadline}"
  done

  install_component "${BUNDLE_DIR}"
  deadline=$((SECONDS + NVSENTINEL_WAIT_SECONDS))
  wait_rollout deployment lifecycle-manager nvsentinel "${deadline}"
  kc get crd "${VR_RESOURCE}" >/dev/null || err "the ValidationRequest CRD was not installed"
}

# ── Node and ValidationRequest helpers ──

node_json() { kc get node "${NODE}" -o json; }

# node_has succeeds when jq, given the remaining arguments, finds the node
# matches.
node_has() { node_json | jq -e "$@" >/dev/null 2>&1; }

node_cordoned() { node_has '.spec.unschedulable == true'; }
node_schedulable() { node_has '(.spec.unschedulable // false) == false'; }
node_label_is() { node_has --arg k "$1" --arg v "$2" '.metadata.labels[$k] == $v'; }
node_label_absent() { node_has --arg k "$1" '.metadata.labels | has($k) | not'; }
node_condition_true() { node_has --arg t "$1" 'any(.status.conditions[]; .type == $t and .status == "True")'; }

clear_uncordon_reason() {
  kc label node "${NODE}" "${UNCORDON_REASON_LABEL}-" >/dev/null 2>&1 || true
}

node_age_seconds() {
  node_json | jq -r 'now - (.metadata.creationTimestamp | fromdateiso8601) | floor'
}

# new_node_settled succeeds when the new-node controller will not act on the
# node again: it has already requested validation, or the node is past the
# recently-joined window.
new_node_settled() {
  local age
  node_condition_true "${NEW_NODE_CONDITION}" && return 0
  age=$(node_age_seconds) || return 1
  [[ "${age}" =~ ^[0-9]+$ ]] && ((age >= NEW_NODE_WINDOW_SECONDS))
}

# vr_for_node prints the newest ValidationRequest labelled created-by=$1 that
# names the node.
vr_for_node() {
  kc get "${VR_RESOURCE}" -l "${CREATED_BY_LABEL}=$1" -o json 2>/dev/null |
    jq -r --arg n "${NODE}" '[.items[] | select(any(.spec.nodes[]; .name == $n))] |
      sort_by(.metadata.creationTimestamp) | reverse | .[0].metadata.name // empty'
}

vr_found() { [[ -n "$(vr_for_node "$1")" ]]; }

vr_phase() { kc get "${VR_RESOURCE}" "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }

vr_terminal() {
  case "$(vr_phase "$1")" in
    Succeeded | Failed) return 0 ;;
  esac
  return 1
}

vr_tests_are() {
  kc get "${VR_RESOURCE}" "$1" -o json | jq -e --argjson want "$2" '.spec.tests == $want' >/dev/null
}

vr_status() { kc get "${VR_RESOURCE}" "$1" -o json | jq -c '.status' | cut -c1-600; }

create_stub_fail_vr() {
  # A create the API server applied but whose response was lost must not fail
  # the retry with AlreadyExists.
  kc get "${VR_RESOURCE}" "${STUB_FAIL_VR}" >/dev/null 2>&1 && return 0
  jq -n --arg name "${STUB_FAIL_VR}" --arg node "${NODE}" '{
    apiVersion: "nvsentinel.nvidia.com/v1alpha1", kind: "ValidationRequest",
    metadata: {name: $name},
    spec: {nodes: [{name: $node}], tests: ["stub-fail"]}
  }' | kc create -f - >/dev/null 2>"${WORK}/vr-create.err"
}

# ── Tests ──

test_new_node_validation() {
  msg "Case 3: new-node validation passes..."
  local age vr
  age=$(node_age_seconds) || age=""
  if ! [[ "${age}" =~ ^[0-9]+$ ]]; then
    fail "#3 new-node validation" "could not read the node's age"
    return
  fi
  if ((age + NEW_NODE_MARGIN_SECONDS >= NEW_NODE_WINDOW_SECONDS)); then
    fail "#3 new-node validation" "the node is ${age}s old and the mixin's recently-joined criterion admits only nodes under ${NEW_NODE_WINDOW_SECONDS}s -- the install ran too long to exercise it"
    return
  fi
  if node_has --arg c "${NEW_NODE_CONDITION}" 'any(.status.conditions[]; .type == $c)' || ! node_schedulable; then
    fail "#3 new-node validation" "precondition: the node is already cordoned or carries ${NEW_NODE_CONDITION}"
    return
  fi

  clear_uncordon_reason
  kc cordon "${NODE}" >/dev/null
  if ! poll "${VR_APPEAR_WAIT_SECONDS}" vr_found lifecycle-manager; then
    fail "#3 lifecycle-manager requests validation of the cordoned new GPU node" \
      "no ValidationRequest labelled ${CREATED_BY_LABEL}=lifecycle-manager names ${NODE} within ${VR_APPEAR_WAIT_SECONDS}s"
    kc uncordon "${NODE}" >/dev/null
    return
  fi
  vr=$(vr_for_node lifecycle-manager) || true
  pass "#3 lifecycle-manager requests validation of the cordoned new GPU node (${vr})"

  if node_cordoned && ! vr_terminal "${vr}"; then
    pass "#3 the node stays cordoned while its validation runs"
  else
    fail "#3 the node stays cordoned while its validation runs" \
      "phase $(vr_phase "${vr}"), unschedulable $(node_json | jq -c '.spec.unschedulable')"
  fi
  if vr_tests_are "${vr}" '["dcgm-diag-test"]'; then
    pass "#3 the request runs the mixin's newNodeTests"
  else
    fail "#3 the request runs the mixin's newNodeTests" "spec.tests is $(kc get "${VR_RESOURCE}" "${vr}" -o jsonpath='{.spec.tests}')"
  fi
  if poll "${RELEASE_WAIT_SECONDS}" node_condition_true "${NEW_NODE_CONDITION}"; then
    pass "#3 ${NEW_NODE_CONDITION} is True"
  else
    fail "#3 ${NEW_NODE_CONDITION} is True" "condition not set within ${RELEASE_WAIT_SECONDS}s"
  fi

  if ! poll "${VALIDATION_WAIT_SECONDS}" vr_terminal "${vr}" || [[ "$(vr_phase "${vr}")" != "Succeeded" ]]; then
    fail "#3 the validation succeeds" "${vr} status: $(vr_status "${vr}")"
    kc uncordon "${NODE}" >/dev/null
    return
  fi
  pass "#3 the validation succeeds"
  if poll "${RELEASE_WAIT_SECONDS}" node_schedulable && node_label_is "${UNCORDON_REASON_LABEL}" ValidationSucceeded; then
    pass "#3 a pass uncordons the node with ${UNCORDON_REASON_LABEL}=ValidationSucceeded"
  else
    fail "#3 a pass uncordons the node" "labels: $(node_json | jq -c '.metadata.labels')"
  fi
}

test_failed_validation_holds_node() {
  msg "Case 4: a failed validation holds the node; deleting it releases the node..."
  if ! new_node_settled || ! node_schedulable; then
    fail "#4 failed validation" "precondition: the node is cordoned, or the new-node controller could still act on it"
    return
  fi

  clear_uncordon_reason
  kc cordon "${NODE}" >/dev/null
  # The webhook's CA bundle is injected by cert-manager after rollout.
  if ! poll "${WEBHOOK_WAIT_SECONDS}" create_stub_fail_vr; then
    fail "#4 failed validation" "the ValidationRequest was not admitted: $(cat "${WORK}/vr-create.err")"
    kc uncordon "${NODE}" >/dev/null
    return
  fi

  if poll "${VALIDATION_WAIT_SECONDS}" vr_terminal "${STUB_FAIL_VR}" && [[ "$(vr_phase "${STUB_FAIL_VR}")" == "Failed" ]] &&
    kc get "${VR_RESOURCE}" "${STUB_FAIL_VR}" -o json |
    jq -e '.status.testGroups[0].attempts[-1].failureReason == "TestFailed"' >/dev/null; then
    pass "#4 a failing test fails the request with TestFailed"
  else
    fail "#4 a failing test fails the request" "${STUB_FAIL_VR} status: $(vr_status "${STUB_FAIL_VR}")"
  fi
  if poll "${RELEASE_WAIT_SECONDS}" node_label_is "${VALIDATION_STATE_LABEL}" validation-failed; then
    pass "#4 the node is labelled ${VALIDATION_STATE_LABEL}=validation-failed"
  else
    fail "#4 validation-failed label" "labels: $(node_json | jq -c '.metadata.labels')"
  fi
  sleep "${FAILED_SETTLE_SECONDS}"
  if node_cordoned; then
    pass "#4 a failed validation keeps the node cordoned"
  else
    fail "#4 a failed validation keeps the node cordoned" "the node was uncordoned"
  fi

  if ! kc delete "${VR_RESOURCE}" "${STUB_FAIL_VR}" --timeout="${RELEASE_WAIT_SECONDS}s" >/dev/null; then
    fail "#4 deleting the failed request" "${STUB_FAIL_VR} was not deleted within ${RELEASE_WAIT_SECONDS}s"
    return
  fi
  if poll "${RELEASE_WAIT_SECONDS}" node_schedulable && node_label_absent "${VALIDATION_STATE_LABEL}"; then
    # lifecycle-manager writes this reason on every release, deletion included,
    # so it is not proof the node passed.
    pass "#4 deleting the failed request returns the node to service untested (uncordon-reason $(node_json | jq -c --arg k "${UNCORDON_REASON_LABEL}" '.metadata.labels[$k]'))"
  else
    fail "#4 deleting the failed request releases the node" "unschedulable $(node_json | jq -c '.spec.unschedulable'), labels $(node_json | jq -c '.metadata.labels')"
  fi
}

# ── Main ──

msg "=========================================="
msg "nvsentinel-node-validation mixin e2e"
msg "=========================================="

has_tools go kind kubectl helm yq jq
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
test_gate
ensure_cluster
install_bundle
test_new_node_validation
test_failed_validation_holds_node

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel-node-validation e2e: all tests passed"
