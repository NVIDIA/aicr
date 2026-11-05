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
# nvsentinel-maintenance-request mixin e2e (#3092)
# =============================================================================
#
# Bundle-only checks, then the quarantine step with nvsentinel-node-validation
# and nvsentinel-maintenance-request installed through the bundle's own
# install.sh on Kind:
#
#   1. the gate refuses the mixin without lifecycle-manager and beside the
#      observe step's dry run; it accepts the composition with new-node
#      validation turned off, which needs no DCGM;
#   2. the webhook rejects a request for a node that does not exist, and one
#      whose startTime is in the past;
#   3. a request cordons its node and drains it: the drain waits for the
#      workload pod on the node to finish, then reports drain-succeeded;
#   4. the webhook rejects a second request for the same node;
#   5. deleting the request uncordons the node and clears its NVSentinel
#      state.
#
# The quarantine step runs no remediation, so the request's NONE action ends
# at the drain. That is the path an external requester takes.
#
# TEST-ONLY SETTINGS, passed on the bundle:
#   lifecycle-manager.config.newNodeValidation=null  Kind has no DCGM
#       hostengine; this is the documented opt-out for maintenance-only use.
#   node-drainer.systemNamespaces  adds local-path-storage, Kind's provisioner,
#       which would otherwise hold the drain.
#   fault-quarantine.circuitBreaker.enabled=false  on one node the first
#       cordon trips it.
#   nvsentinel-mongodb at one member with smaller requests, so it fits one
#       runner node.
#
# PREREQUISITES: go, kind, kubectl, helm, yq, jq
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-3092-kind).
#                  Always recreated, then deleted at the end unless
#                  KEEP_CLUSTER=true.
#   KEEP_CLUSTER   Skip cluster teardown on exit (default: false).
#   AICR_BIN       Path to a prebuilt aicr binary (default: built fresh).
# =============================================================================

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DIR}/../../.." && pwd)"
# shellcheck source=/dev/null
. "${ROOT}/tools/common"

CLUSTER_NAME="${CLUSTER_NAME:-aicr-3092-kind}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NODE="${CLUSTER_NAME}-control-plane"

DRAIN_EXTRA_NAMESPACES='^(local-path-storage)$'
# The digest-pinned multi-arch busybox AICR already ships.
BUSYBOX_IMAGE="docker.io/library/busybox:1.38.0@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e"
WORKLOAD_NAMESPACE="maintenance-e2e"
# Long enough that the drain is seen waiting for the pod before it finishes.
WORKLOAD_SECONDS=60
REQUEST="maintenance-e2e"
CHECK_NAME="PlannedMaintenance"
# Emission, cordon and uncordon each react to a change stream, so the hop takes
# seconds; the margin covers a cold informer cache.
PIPELINE_WAIT_SECONDS=240
# The workload's own run plus the drain's next pass.
DRAIN_WAIT_SECONDS=$((WORKLOAD_SECONDS + PIPELINE_WAIT_SECONDS))
CERT_MANAGER_WAIT_SECONDS=600
MONGODB_WAIT_SECONDS=900
NVSENTINEL_WAIT_SECONDS=900
POLL_SECONDS=3

WORK=""
CREATED_CLUSTER=false
BUNDLE=""
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
  name: nvsentinel-maintenance-request-e2e-registry
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
  local recipe="$1" out="$2"
  shift 2
  rm -rf "${WORK}/bundle-${out}"
  "${AICR_BIN}" bundle -r "${WORK}/recipe-${recipe}.yaml" -o "${WORK}/bundle-${out}" \
    --set-json "nvsentinel:lifecycle-manager.config.newNodeValidation=null" \
    --set "nvsentinel:node-drainer.systemNamespaces=${DRAIN_EXTRA_NAMESPACES}" \
    --set "nvsentinel:fault-quarantine.circuitBreaker.enabled=false" \
    --set "nvsentinelmongodb:unsafeFlags.replsetSize=true" \
    --set "nvsentinelmongodb:replsets.rs0.size=1" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.cpu=250m" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.memory=512Mi" \
    --system-node-selector kubernetes.io/os=linux \
    "$@" >"${WORK}/bundle-${out}.log" 2>&1
}

# expect_refusal asserts that bundling recipe $2 fails and names $3.
expect_refusal() {
  local name="$1" recipe="$2" want="$3"
  if bundle_to "${recipe}" refused; then
    fail "${name}" "the bundle was accepted"
  elif grep -qF -- "${want}" "${WORK}/bundle-refused.log"; then
    pass "${name}"
  else
    fail "${name}" "refused without naming '${want}': $(grep -m1 -i 'error' "${WORK}/bundle-refused.log" | cut -c1-400)"
  fi
}

# test_gate covers case 1 and leaves the live bundle in ${WORK}/bundle-deploy.
test_gate() {
  msg "Case 1: the bundle gate..."
  compose_recipe alone nvsentinel-quarantine nvsentinel-maintenance-request
  compose_recipe observe nvsentinel-observe nvsentinel-node-validation nvsentinel-maintenance-request
  compose_recipe live nvsentinel-quarantine nvsentinel-node-validation nvsentinel-maintenance-request

  expect_refusal "#1 gate refuses the mixin without lifecycle-manager" alone \
    "compose the nvsentinel-node-validation mixin"
  expect_refusal "#1 gate refuses the mixin beside the observe step's dry run" observe \
    "global.dryRun is true, so fault-quarantine only logs the cordon"

  bundle_to live deploy ||
    { tail -20 "${WORK}/bundle-deploy.log"; err "bundle generation failed for the maintenance-request composition"; }
  pass "#1 gate accepts the composition with new-node validation off, without DCGM"

  BUNDLE="${WORK}/bundle-deploy"
  local values
  values=$(find "${BUNDLE}" -maxdepth 2 -path '*-nvsentinel/values.yaml' -print -quit)
  [[ -n "${values}" ]] || err "bundled nvsentinel values not found"
  # Fail here, not later as "nothing happened": a composition that regressed
  # would otherwise read as a request that never fired.
  [[ "$(yq '.["lifecycle-manager"].controllers.maintenanceRequest.enabled' "${values}")" == "true" ]] ||
    err "the bundle does not turn the MaintenanceRequest controller on"
  [[ "$(yq '.global.lifecycleManager.enabled' "${values}")" == "true" ]] ||
    err "the bundle does not turn lifecycle-manager on"
  [[ "$(yq '.global.dryRun' "${values}")" == "false" ]] || err "the bundle is not live"
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
  # fault-quarantine counts GPU nodes by this label and refuses to act when
  # there are none.
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

component_dir() {
  local dir
  dir=$(find "${BUNDLE}" -maxdepth 1 -type d -name "*-$1" -print -quit)
  [[ -n "${dir}" ]] || err "bundled $1 directory not found"
  echo "${dir}"
}

install_component() {
  local dir
  dir=$(component_dir "$1")
  (cd "${dir}" && chmod +x install.sh && KUBE_CONTEXT="${KUBE_CONTEXT}" ./install.sh) ||
    err "$1 install failed"
}

install_bundle() {
  msg "Installing prometheus-operator-crds, cert-manager, the Percona datastore and nvsentinel from the bundle..."
  local d deadline remaining
  # CRDs first: cert-manager's chart renders a ServiceMonitor.
  install_component prometheus-operator-crds
  install_component cert-manager
  # The Percona operator issues the cluster's TLS through cert-manager, and
  # lifecycle-manager's webhook certificate comes from it.
  deadline=$((SECONDS + CERT_MANAGER_WAIT_SECONDS))
  for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
    wait_rollout deployment "${d}" cert-manager "${deadline}"
  done

  install_component psmdb-operator
  deadline=$((SECONDS + MONGODB_WAIT_SECONDS))
  wait_rollout deployment psmdb-operator nvsentinel "${deadline}"
  install_component nvsentinel-mongodb
  install_component nvsentinel-mongodb-post
  remaining=$((deadline - SECONDS))
  ((remaining > 0)) || err "nvsentinel-mongodb never became ready"
  kc -n nvsentinel wait --for=jsonpath='{.status.state}'=ready \
    perconaservermongodb/nvsentinel-mongodb --timeout="${remaining}s" ||
    err "nvsentinel-mongodb never became ready"
  remaining=$((deadline - SECONDS))
  ((remaining > 0)) || err "nvsentinel's MongoDB client certificate was never issued"
  kc -n nvsentinel wait --for=condition=Ready certificate/nvsentinel-mongodb-app-client \
    --timeout="${remaining}s" || err "nvsentinel's MongoDB client certificate was never issued"

  install_component nvsentinel

  deadline=$((SECONDS + NVSENTINEL_WAIT_SECONDS))
  for d in fault-quarantine node-drainer lifecycle-manager; do
    wait_rollout deployment "${d}" nvsentinel "${deadline}"
  done
  wait_rollout daemonset platform-connectors nvsentinel "${deadline}"
}

# ── Helpers ──

# request_manifest prints a MaintenanceRequest named $1 for node $2, with
# startTime $3 when given.
request_manifest() {
  local name="$1" node="$2" start="${3:-}"
  jq -n --arg name "${name}" --arg node "${node}" --arg check "${CHECK_NAME}" --arg start "${start}" '{
    apiVersion: "nvsentinel.dgxc.nvidia.com/v1", kind: "MaintenanceRequest", metadata: {name: $name},
    spec: ({healthEvent: {version: 1, agent: "maintenance-e2e", componentClass: "Node", checkName: $check,
      nodeName: $node, isHealthy: false, isFatal: false, recommendedAction: "NONE",
      message: "aicr maintenance-request e2e"}} + (if $start == "" then {} else {startTime: $start} end))}'
}

# expect_rejected asserts the API server refuses manifest $2 with a webhook
# message containing $3.
expect_rejected() {
  local name="$1" manifest="$2" want="$3" out
  if out=$(kc create -f - <<<"${manifest}" 2>&1); then
    fail "${name}" "the request was accepted"
    kc delete maintenancerequest "$(jq -r .metadata.name <<<"${manifest}")" --wait=false >/dev/null 2>&1 || true
  elif grep -qF -- "${want}" <<<"${out}"; then
    pass "${name}"
  else
    fail "${name}" "rejected without naming '${want}': ${out}"
  fi
}

# Every predicate below fails when its read fails, so a negative one
# (uncordoned, state_is_not, state_cleared, request_gone) cannot pass on an
# API error.
node_json() { kc get node "${NODE}" -o json; }
node_field() {
  local json
  json=$(node_json) || return 1
  jq -r "$@" <<<"${json}"
}
node_state() { node_field '.metadata.labels["dgxc.nvidia.com/nvsentinel-state"] // ""'; }
cordoned() { [[ "$(node_field '.spec.unschedulable // false')" == "true" ]]; }
uncordoned() { [[ "$(node_field '.spec.unschedulable // false')" == "false" ]]; }
state_is() { local state; state=$(node_state) && [[ "${state}" == "$1" ]]; }
state_is_not() { local state; state=$(node_state) && [[ "${state}" != "$1" ]]; }
state_cleared() { state_is ""; }
quarantined_for_request() {
  # shellcheck disable=SC2016 # $check is a jq variable, bound by --arg.
  [[ "$(node_field --arg check "${CHECK_NAME}" \
    '.metadata.annotations.quarantineHealthEvent // "" | contains($check)')" == "true" ]]
}
# request_gone passes only on NotFound; any other error is a failed read.
request_gone() {
  local out
  if out=$(kc get maintenancerequest "${REQUEST}" -o name 2>&1); then
    return 1
  fi
  grep -q '(NotFound)' <<<"${out}"
}
workload_phase() { kc -n "${WORKLOAD_NAMESPACE}" get pod workload -o jsonpath='{.status.phase}' 2>/dev/null; }
workload_running() { [[ "$(workload_phase)" == "Running" ]]; }

# ── Tests ──

test_admission() {
  msg "Case 2: the webhook..."
  expect_rejected "#2 webhook rejects a request for a missing node" \
    "$(request_manifest maintenance-e2e-missing no-such-node)" "references non-existent node"
  expect_rejected "#2 webhook rejects a request whose startTime is past" \
    "$(request_manifest maintenance-e2e-past "${NODE}" "2020-01-01T00:00:00Z")" "spec.startTime must be in the future"
}

test_request_cordons_and_drains() {
  msg "Case 3: a request cordons and drains its node..."
  kc create namespace "${WORKLOAD_NAMESPACE}" >/dev/null
  kc -n "${WORKLOAD_NAMESPACE}" run workload --image="${BUSYBOX_IMAGE}" --restart=Never \
    --overrides="{\"spec\":{\"nodeName\":\"${NODE}\"}}" -- sleep "${WORKLOAD_SECONDS}" >/dev/null
  poll 120 workload_running || err "the workload pod never started"

  kc create -f - <<<"$(request_manifest "${REQUEST}" "${NODE}")" >/dev/null || err "the request was rejected"
  if kc wait maintenancerequest "${REQUEST}" --for=condition=HealthEventEmitted \
    --timeout="${PIPELINE_WAIT_SECONDS}s" >/dev/null 2>&1; then
    pass "#3 lifecycle-manager published the request's event (HealthEventEmitted)"
  else
    fail "#3 HealthEventEmitted" "$(kc get maintenancerequest "${REQUEST}" -o jsonpath='{.status.conditions}')"
    return
  fi
  if poll "${PIPELINE_WAIT_SECONDS}" cordoned && quarantined_for_request; then
    pass "#3 fault-quarantine cordoned the node for ${CHECK_NAME}"
  else
    fail "#3 cordon" "unschedulable=$(node_json | jq -r .spec.unschedulable) annotation=$(node_json | jq -r '.metadata.annotations.quarantineHealthEvent // "none"' | cut -c1-200)"
    return
  fi
  if workload_running && state_is_not drain-succeeded; then
    pass "#3 the drain waits while the workload pod still runs"
  else
    fail "#3 drain waits" "workload=$(workload_phase) state=$(node_json | jq -r '.metadata.labels["dgxc.nvidia.com/nvsentinel-state"] // ""')"
  fi
  if poll "${DRAIN_WAIT_SECONDS}" state_is drain-succeeded; then
    pass "#3 node-drainer reported drain-succeeded after the workload finished (pod $(workload_phase))"
  else
    fail "#3 drain" "state=$(node_json | jq -r '.metadata.labels["dgxc.nvidia.com/nvsentinel-state"] // ""') workload=$(workload_phase)"
  fi
}

test_second_request_rejected() {
  msg "Case 4: one open request per node..."
  expect_rejected "#4 webhook rejects a second request for the same node" \
    "$(request_manifest maintenance-e2e-second "${NODE}")" "already has an open MaintenanceRequest"
}

test_delete_uncordons() {
  msg "Case 5: deleting the request uncordons the node..."
  kc delete maintenancerequest "${REQUEST}" --timeout="${PIPELINE_WAIT_SECONDS}s" >/dev/null ||
    { fail "#5 delete" "the request was not removed: $(kc get maintenancerequest "${REQUEST}" -o jsonpath='{.metadata.finalizers}')"; return; }
  if poll "${PIPELINE_WAIT_SECONDS}" request_gone; then
    pass "#5 lifecycle-manager published the clearing event and released the request"
  else
    fail "#5 request removal" "still present"
  fi
  if poll "${PIPELINE_WAIT_SECONDS}" uncordoned && poll "${PIPELINE_WAIT_SECONDS}" state_cleared; then
    pass "#5 the node is uncordoned and its NVSentinel state cleared"
  else
    fail "#5 uncordon" "unschedulable=$(node_json | jq -r .spec.unschedulable) state=$(node_json | jq -r '.metadata.labels["dgxc.nvidia.com/nvsentinel-state"] // ""')"
  fi
}

has_tools go kind kubectl helm yq jq
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
test_gate
ensure_cluster
install_bundle
test_admission
test_request_cordons_and_drains
test_second_request_rejected
test_delete_uncordons

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel-maintenance-request e2e: all tests passed"
