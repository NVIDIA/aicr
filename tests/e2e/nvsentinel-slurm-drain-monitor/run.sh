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
# nvsentinel-slurm-drain-monitor mixin: runtime e2e (#2611)
# =============================================================================
#
# Drives the real product path (aicr recipe -> aicr bundle on the shipped
# h100-kind-training-slurm leaf, which composes the mixin -> the bundle's own
# install.sh for nvsentinel) against a live Kind cluster, then asserts what a
# Slurm drain does:
#
#   [HC] drain            -> the monitor publishes an event, the platform
#                            connector receives it as STORE_ONLY, and no node
#                            condition or Kubernetes Event is created
#   drain cleared         -> the monitor publishes the recovery
#   slurm-operator: drain -> nothing is published
#   unmatched reason      -> nothing is published
#
# SIMULATED SLINKY. Slinky itself is not installed: the test creates pods with
# the labels the Slinky slurm-operator v1.2.0 NodeSet controller puts on worker
# pods and patches the SlurmNodeStateDrain condition onto them, which is
# exactly the write that controller makes (condition .Message = the Slurm
# reason). That keeps the test about the monitor's contract rather than about
# standing up slurmctld on Kind. TestSlurmDrainMonitorSelectorPinnedToVerifiedSlinkyVersion
# binds those labels to the operator version they were verified against.
#
# PREREQUISITES: go, kind, kubectl, helm, yq, jq
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-nvsentinel-slurm-drain-monitor-e2e).
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

CLUSTER_NAME="${CLUSTER_NAME:-aicr-nvsentinel-slurm-drain-monitor-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"

SLURM_NAMESPACE="slurm"
MONITOR_SELECTOR="app.kubernetes.io/name=slurm-drain-monitor"
CONNECTOR_SELECTOR="app.kubernetes.io/name=nvsentinel,app.kubernetes.io/instance=nvsentinel"
CHECK_NAME="SlurmHealthCheck"
# Multi-arch, tiny, and never exits: the pods only need to exist and be
# scheduled so the condition has a node to name.
WORKER_IMAGE="registry.k8s.io/pause:3.10"

# How long a positive assertion waits for its log line, and how long a negative
# one waits before declaring absence. The same value for both: a shorter
# negative window would pass on timing alone.
EVENT_WAIT_SECONDS=60

WORK=""
CREATED_CLUSTER=false
BUNDLE_DIR=""
CRDS_DIR=""
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

detail() { msg "  $*"; }
pass() { TOTAL_TESTS=$((TOTAL_TESTS + 1)); PASSED_TESTS=$((PASSED_TESTS + 1)); msg "PASS: $1"; }
fail() {
  TOTAL_TESTS=$((TOTAL_TESTS + 1))
  FAILED_TESTS=$((FAILED_TESTS + 1))
  msg "FAIL: $1 -- $2"
}
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
kc() { kubectl --context "${KUBE_CONTEXT}" "$@"; }

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

compose_bundle() {
  msg "Composing the shipped h100-kind-training-slurm bundle..."
  # The shipped Slurm leaf composes the mixin itself, so this is the bundle an
  # operator gets. CheckNVSentinelSlurmDrainMonitorRequiresSlinky runs here too,
  # proving the gate passes on that shape.
  "${AICR_BIN}" recipe --service kind --accelerator h100 --intent training --platform slurm \
    --output "${WORK}/recipe.yaml" ||
    err "recipe generation failed"
  "${AICR_BIN}" bundle -r "${WORK}/recipe.yaml" -o "${WORK}/bundle" ||
    err "bundle generation failed"

  # -print -quit rather than `| head -1`: head closing the pipe SIGPIPEs find.
  BUNDLE_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-nvsentinel' -print -quit)
  CRDS_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-prometheus-operator-crds' -print -quit)
  [[ -n "${BUNDLE_DIR}" ]] || err "bundled nvsentinel component directory not found"
  [[ -n "${CRDS_DIR}" ]] || err "bundled prometheus-operator-crds directory not found"

  # Exact paths, not key greps: a regression to enabled: false or a dropped
  # strategy would otherwise surface later as a confusing runtime failure.
  local values="${BUNDLE_DIR}/values.yaml"
  [[ "$(yq '.global.slurmDrainMonitor.enabled' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values.yaml does not set global.slurmDrainMonitor.enabled: true -- composition regressed"
  [[ "$(yq '.slurm-drain-monitor.processingStrategy' "${values}")" == "STORE_ONLY" ]] ||
    err "bundle's nvsentinel values.yaml does not set slurm-drain-monitor.processingStrategy: STORE_ONLY -- composition regressed"
  [[ "$(yq '.slurm-drain-monitor.namespace' "${values}")" == "${SLURM_NAMESPACE}" ]] ||
    err "bundle's nvsentinel values.yaml does not watch namespace ${SLURM_NAMESPACE} -- composition regressed"
}

ensure_cluster() {
  # Always recreated, never reused: a leftover cluster carries the previous
  # run's monitor logs, and an old log line would satisfy a positive assertion
  # without the monitor emitting anything new.
  if grep -qx "${CLUSTER_NAME}" <<<"$(kind get clusters 2>/dev/null)"; then
    msg "Deleting stale Kind cluster ${CLUSTER_NAME}..."
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi
  msg "Creating Kind cluster ${CLUSTER_NAME}..."
  kind create cluster --name "${CLUSTER_NAME}" || err "kind create cluster failed"
  CREATED_CLUSTER=true
  # A per-run kubeconfig, so this never touches the user's own.
  kind export kubeconfig --name "${CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" ||
    err "failed to export kubeconfig for ${CLUSTER_NAME}"
  kc wait --for=condition=Ready node --all --timeout=120s || err "cluster nodes never became Ready"
}

# await_workload_exists polls until $1/$2 exists in the nvsentinel namespace.
# `kubectl rollout status` on a missing object fails instantly instead of
# waiting, so this closes the window between helm returning and the controller
# creating the workload.
await_workload_exists() {
  local kind="$1" name="$2" elapsed=0
  while [[ "${elapsed}" -lt 120 ]]; do
    if kc -n nvsentinel get "${kind}" "${name}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  err "${kind}/${name} was never created in the nvsentinel namespace"
}

install_nvsentinel() {
  msg "Installing prometheus-operator-crds and nvsentinel from the bundle..."
  # The namespace slinky-slurm would create. The monitor's watch is
  # namespace-scoped, so it must exist before the fixtures do.
  kc create namespace "${SLURM_NAMESPACE}" --dry-run=client -o yaml | kc apply -f - >/dev/null

  (cd "${CRDS_DIR}" && chmod +x install.sh && KUBE_CONTEXT="${KUBE_CONTEXT}" ./install.sh) ||
    err "prometheus-operator-crds install failed"
  (cd "${BUNDLE_DIR}" && chmod +x install.sh && KUBE_CONTEXT="${KUBE_CONTEXT}" ./install.sh) ||
    err "nvsentinel install failed"

  await_workload_exists deployment slurm-drain-monitor
  await_workload_exists daemonset platform-connectors
  # Generous: covers a cold pull of the NVSentinel image set.
  kc -n nvsentinel rollout status deployment/slurm-drain-monitor --timeout=600s ||
    err "slurm-drain-monitor never became Ready"
  # The monitor publishes through platform-connectors' socket, so its own
  # readiness does not mean an event can be delivered.
  kc -n nvsentinel rollout status daemonset/platform-connectors --timeout=600s ||
    err "platform-connectors never rolled out"
}

# ── Fixtures ──

# create_worker creates a pod carrying the Slinky v1.2.0 worker labels and
# echoes the node it was scheduled to.
create_worker() {
  local pod="$1" node
  kc -n "${SLURM_NAMESPACE}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  labels:
    app.kubernetes.io/name: slurmd
    app.kubernetes.io/component: worker
spec:
  containers:
    - name: slurmd
      image: ${WORKER_IMAGE}
EOF
  kc -n "${SLURM_NAMESPACE}" wait --for=condition=Ready "pod/${pod}" --timeout=180s >/dev/null ||
    err "worker fixture ${pod} never became Ready"
  node=$(kc -n "${SLURM_NAMESPACE}" get pod "${pod}" -o jsonpath='{.spec.nodeName}') ||
    err "reading ${pod}'s node failed"
  [[ -n "${node}" ]] || err "worker fixture ${pod} has no node"
  printf '%s' "${node}"
}

# set_drain writes the SlurmNodeStateDrain condition the way the Slinky
# NodeSet controller does: Status True with .Message carrying the Slurm
# reason. An empty reason removes the condition, which is what the controller
# does when Slurm undrains the node. A strategic-merge patch keyed on the
# condition type touches only this condition, so it cannot race kubelet's own
# status writes, and kubelet preserves condition types it does not own.
set_drain() {
  local pod="$1" reason="$2" patch
  patch=$(jq -nc --arg reason "${reason}" '
    {status: {conditions: [
      if $reason == "" then {type: "SlurmNodeStateDrain", "$patch": "delete"}
      else {type: "SlurmNodeStateDrain", status: "True", message: $reason,
            lastTransitionTime: (now | strftime("%Y-%m-%dT%H:%M:%SZ"))}
      end]}}') || err "building ${pod}'s drain patch failed"
  kc -n "${SLURM_NAMESPACE}" patch pod "${pod}" --subresource=status --type=strategic \
    -p "${patch}" >/dev/null ||
    err "patching SlurmNodeStateDrain on ${pod} failed"
}

# ── Assertion helpers ──

# require_monitor_ready fails the run unless the monitor is Running and Ready.
# "Nothing was published" is indistinguishable from "nothing was watching", so
# every negative assertion gates on this.
require_monitor_ready() {
  local ready
  ready=$(kc -n nvsentinel get pods -l "${MONITOR_SELECTOR}" \
    -o jsonpath='{.items[?(@.status.phase=="Running")].status.conditions[?(@.type=="Ready")].status}') ||
    err "could not query slurm-drain-monitor readiness"
  [[ "${ready}" == *"True"* ]] || err "slurm-drain-monitor is not Running/Ready (got '${ready:-<none>}')"
}

# monitor_lines echoes the monitor's log lines since $2 that carry message $1.
# Returns nonzero if the log query fails, rather than calling err: this runs in
# a command substitution, and reading a failed query as "no line" would pass a
# negative assertion on an apiserver hiccup. --tail=-1 because the default cap
# for a selector lets a line scroll out, which reads as absence.
monitor_lines() {
  local message="$1" since="$2" logs
  logs=$(kc -n nvsentinel logs -l "${MONITOR_SELECTOR}" --since-time="${since}" --tail=-1) || return 1
  grep -F "\"msg\":\"${message}\"" <<<"${logs}" || true
}

# connector_skip_lines echoes the platform connector's lines since $1 recording a
# STORE_ONLY event from this monitor. That log line is the only trace a
# STORE_ONLY event leaves without an NVSentinel datastore.
connector_skip_lines() {
  local since="$1" logs
  # The selector is shared with other nvsentinel workloads and spans one
  # platform-connectors pod per node, so lift kubectl's default 5-pod cap.
  logs=$(kc -n nvsentinel logs -l "${CONNECTOR_SELECTOR}" --all-containers --prefix=false \
    --max-log-requests=50 --since-time="${since}" --tail=-1) || return 1
  grep -F '"msg":"Skipping non-remediation health event' <<<"${logs}" |
    grep -F '"agent":"slurm-drain-monitor"' || true
}

# wait_for_line polls $1 (a function name) with the remaining args until it
# yields a line containing every pattern after "--", or EVENT_WAIT_SECONDS pass.
# Echoes the matching line. Returns 1 on timeout and 2 when the logs cannot be
# read: it runs in a command substitution, where err would exit only the
# subshell and the caller would report a missing line instead.
wait_for_line() {
  local fn="$1" elapsed=0 out line
  shift
  local args=() patterns=()
  while [[ "$#" -gt 0 && "$1" != "--" ]]; do args+=("$1"); shift; done
  [[ "${1:-}" == "--" ]] && shift
  patterns=("$@")
  while [[ "${elapsed}" -le "${EVENT_WAIT_SECONDS}" ]]; do
    out=$("${fn}" "${args[@]}") || return 2
    while IFS= read -r line; do
      local ok=true p
      for p in "${patterns[@]}"; do
        [[ "${line}" == *"${p}"* ]] || { ok=false; break; }
      done
      if [[ "${ok}" == "true" && -n "${line}" ]]; then
        printf '%s' "${line}"
        return 0
      fi
    done <<<"${out}"
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

# expect_line records test $1 as passed when wait_for_line (the remaining
# args) finds its line, failed with reason $2 on a timeout, and aborts the run
# when the logs cannot be read at all.
expect_line() {
  local name="$1" why="$2" line rc=0
  shift 2
  line=$(wait_for_line "$@") || rc=$?
  case "${rc}" in
    0) pass "nvsentinel-slurm-drain-monitor/${name}"; detail "${line}" ;;
    1) fail "nvsentinel-slurm-drain-monitor/${name}" "${why}" ;;
    *) err "could not read nvsentinel logs while checking ${name}" ;;
  esac
}

# assert_store_only checks that no node condition and no Kubernetes Event was
# created for the check -- the property STORE_ONLY promises.
assert_store_only() {
  local node="$1" name="$2" cond events
  cond=$(kc get node "${node}" -o jsonpath="{.status.conditions[?(@.type==\"${CHECK_NAME}\")].status}") ||
    err "querying node ${node}'s conditions failed"
  events=$(kc get events -A --field-selector "involvedObject.kind=Node,involvedObject.name=${node}" \
    -o jsonpath='{range .items[*]}{.reason}{" "}{.message}{"\n"}{end}') ||
    err "querying node ${node}'s events failed"
  if [[ -n "${cond}" ]]; then
    fail "nvsentinel-slurm-drain-monitor/${name}" "node ${node} has a ${CHECK_NAME} condition (${cond}); STORE_ONLY must not create one"
  elif grep -qF "${CHECK_NAME}" <<<"${events}"; then
    fail "nvsentinel-slurm-drain-monitor/${name}" "node ${node} has a ${CHECK_NAME} Kubernetes Event; STORE_ONLY must not create one"
  else
    pass "nvsentinel-slurm-drain-monitor/${name}"
  fi
}

# ── Tests ──

# Negative cases run first and in one window, so the positive case's events
# cannot be mistaken for theirs: the connector's log line names the node and
# check, not the pod.
test_ignored_drains_stay_quiet() {
  msg "TEST: operator-owned and unmatched drains -> nothing published"
  local since node
  since=$(now)
  node=$(create_worker slurmd-operator-drain-e2e)
  create_worker slurmd-unmatched-drain-e2e >/dev/null
  detail "node=${node}"

  require_monitor_ready
  # Slinky prefixes its own drains with this. The second segment would match
  # [HC] once split on "; ", so only the prefix skip keeps this quiet.
  set_drain slurmd-operator-drain-e2e "slurm-operator: node maintenance; [HC] GPU check failed"
  set_drain slurmd-unmatched-drain-e2e "admin: scheduled firmware update"
  sleep "${EVENT_WAIT_SECONDS}"
  require_monitor_ready

  local published skipped
  published=$(monitor_lines "Published unhealthy events for external drain" "${since}") ||
    err "could not read slurm-drain-monitor logs"
  skipped=$(connector_skip_lines "${since}") || err "could not read platform-connector logs"
  if [[ -n "${published}" ]]; then
    fail "nvsentinel-slurm-drain-monitor/ignored-drains" "the monitor published for an operator-owned or unmatched drain: ${published}"
  elif [[ -n "${skipped}" ]]; then
    fail "nvsentinel-slurm-drain-monitor/ignored-drains" "the platform connector received an event from the monitor: ${skipped}"
  else
    pass "nvsentinel-slurm-drain-monitor/ignored-drains"
  fi

  kc -n "${SLURM_NAMESPACE}" delete pod slurmd-operator-drain-e2e slurmd-unmatched-drain-e2e --wait=true >/dev/null
}

test_hc_drain_publishes_and_recovers() {
  msg "TEST: [HC] drain -> event published as STORE_ONLY; undrain -> recovery published"
  local pod="slurmd-hc-drain-e2e" since node
  node=$(create_worker "${pod}")
  detail "pod=${pod} node=${node}"
  require_monitor_ready

  since=$(now)
  set_drain "${pod}" "[HC] e2e GPU health check failed"
  expect_line hc-drain-published "no publish line for ${pod} within ${EVENT_WAIT_SECONDS}s" \
    monitor_lines "Published unhealthy events for external drain" "${since}" -- \
    "${SLURM_NAMESPACE}/${pod}" "\"node\":\"${node}\"" "[HC] e2e GPU health check failed"
  expect_line hc-drain-received-store-only \
    "the platform connector logged no STORE_ONLY ${CHECK_NAME} event for ${node} within ${EVENT_WAIT_SECONDS}s" \
    connector_skip_lines "${since}" -- \
    "\"checkName\":\"${CHECK_NAME}\"" "\"node\":\"${node}\"" '"processingStrategy":"STORE_ONLY"'
  assert_store_only "${node}" "hc-drain-no-condition-or-event"

  since=$(now)
  set_drain "${pod}" ""
  expect_line undrain-recovery-published "no recovery publish line for ${pod} within ${EVENT_WAIT_SECONDS}s" \
    monitor_lines "Published healthy event, drain cleared" "${since}" -- \
    "${SLURM_NAMESPACE}/${pod}" "\"node\":\"${node}\""
  assert_store_only "${node}" "undrain-no-condition-or-event"

  kc -n "${SLURM_NAMESPACE}" delete pod "${pod}" --wait=false >/dev/null
}

main() {
  for tool in go kind kubectl helm yq jq; do
    has_tools "${tool}" || err "${tool} is required"
  done
  WORK=$(mktemp -d)
  export KUBECONFIG="${WORK}/kubeconfig"

  build_binary
  compose_bundle
  ensure_cluster
  install_nvsentinel

  test_ignored_drains_stay_quiet
  test_hc_drain_publishes_and_recovers

  msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed, ${FAILED_TESTS} failed"
  [[ "${FAILED_TESTS}" -eq 0 ]] || err "nvsentinel-slurm-drain-monitor e2e failed"
}

main "$@"
