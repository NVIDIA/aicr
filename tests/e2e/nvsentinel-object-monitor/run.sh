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
# nvsentinel-object-monitor mixin: runtime e2e (#2612)
# =============================================================================
#
# Drives the real product path (aicr recipe -> aicr bundle -> the bundle's own
# install.sh) against a live Kind cluster, then asserts that an unhealthy
# operator DaemonSet pod produces both a published health event and a node
# condition, and that the same pod inside the grace period produces neither.
# The CEL unit tests cannot catch a break in chart enablement, policy
# serialization, RBAC wiring, or event delivery.
#
# Only the GPU Operator policy runs live; the network-operator policy differs
# solely in its namespaces, which are asserted against the catalog by
# TestMixinNVSentinelObjectMonitor_PolicyNamespacesMatchCatalog.
#
# BACKDATING CAVEAT (read before touching the unhealthy-pod step):
# kubelet re-syncs status.startTime, so a one-shot patch is overwritten within
# seconds, reopening the grace window and flapping the condition back to
# healthy. Hence the background re-patch loop. The flapping is an artifact of
# impersonating 30 minutes in seconds of wall-clock; in production startTime
# does not move and the condition is stable once it fires.
#
# PREREQUISITES: go, kind, kubectl, helm, yq
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-nvsentinel-object-monitor-e2e).
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

CLUSTER_NAME="${CLUSTER_NAME:-aicr-nvsentinel-object-monitor-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"

# The predicate's 30m floor plus margin, so a patched startTime doesn't race
# the `>` comparison against clock skew with the apiserver.
BACKDATE_MINUTES=40
BACKDATE_TS=$(date -u -v-"${BACKDATE_MINUTES}"M +%Y-%m-%dT%H:%M:%SZ 2>/dev/null ||
  date -u -d "${BACKDATE_MINUTES} minutes ago" +%Y-%m-%dT%H:%M:%SZ)

# The policy name verbatim -- NVSentinel does not PascalCase it the way it
# does built-in health-monitor check names.
GPU_POLICY_CONDITION="gpu-operator-pods-health"
MONITOR_SELECTOR="app.kubernetes.io/name=kubernetes-object-monitor"

# One value for both the positive and the in-grace-period assertions: a
# shorter negative window would pass on timing alone, before the monitor had a
# chance to fire at all.
CONDITION_WAIT_SECONDS=80
# The backdater must outlive the assertion window it supports.
PATCH_LOOP_SECONDS=$((CONDITION_WAIT_SECONDS + 20))

WORK=""
CREATED_CLUSTER=false
# Global so cleanup() can stop it on any error path.
PATCHER_PID=""
BUNDLE_DIR=""
CRDS_DIR=""
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

cleanup() {
  local rc=$?
  # Stop the backdater first: on an error path it would otherwise keep issuing
  # kubectl patches against a cluster being torn down.
  if [[ -n "${PATCHER_PID}" ]]; then
    kill "${PATCHER_PID}" 2>/dev/null || true
    wait "${PATCHER_PID}" 2>/dev/null || true
  fi
  # Cluster before workdir: $KUBECONFIG lives inside WORK, and deleting it
  # first would leave the teardown unable to reach the cluster, silently
  # leaking it (the delete is best-effort).
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
  msg "Composing a bundle with the nvsentinel-object-monitor mixin..."
  local data_dir="${WORK}/data"
  mkdir -p "${data_dir}/overlays"

  # --data requires a registry.yaml even when it adds no components of its own.
  cat >"${data_dir}/registry.yaml" <<'REGISTRY_EOF'
kind: ComponentRegistry
apiVersion: aicr.run/v1beta1
metadata:
  name: nvsentinel-object-monitor-e2e-registry
components: []
REGISTRY_EOF

  # Derived from the shipped overlay so its base, constraints and validation
  # cannot drift; the only difference is the composed mixin.
  yq '.spec.mixins = ["nvsentinel-object-monitor"]' \
    "${ROOT}/recipes/overlays/h100-kind-training.yaml" \
    >"${data_dir}/overlays/h100-kind-training.yaml"

  "${AICR_BIN}" recipe --service kind --accelerator h100 --intent training \
    --data "${data_dir}" --output "${WORK}/recipe.yaml" ||
    err "recipe generation failed"
  "${AICR_BIN}" bundle -r "${WORK}/recipe.yaml" -o "${WORK}/bundle" ||
    err "bundle generation failed"

  # -print -quit rather than `| head -1`: head closing the pipe SIGPIPEs find,
  # and under pipefail the bare assignment returns 141 and kills the script
  # before the err messages below can explain anything.
  BUNDLE_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-nvsentinel' -print -quit)
  CRDS_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-prometheus-operator-crds' -print -quit)
  [[ -n "${BUNDLE_DIR}" ]] || err "bundled nvsentinel component directory not found"
  [[ -n "${CRDS_DIR}" ]] || err "bundled prometheus-operator-crds directory not found"
  # Asserts the values, not just the key: `kubernetesObjectMonitor` alone
  # passes on `enabled: false` and with the policy list missing entirely, so a
  # one-character mixin regression would surface later as "monitor never became
  # Ready" and be blamed on the product.
  grep -q "kubernetesObjectMonitor" "${BUNDLE_DIR}/values.yaml" ||
    err "bundle's nvsentinel values.yaml lacks the mixin's kubernetesObjectMonitor override -- composition regressed"
  # The exact path: a bare `enabled: true` grep also matches each policy's own
  # enabled field, so it would pass with global.kubernetesObjectMonitor.enabled
  # absent or false.
  [[ "$(yq '.global.kubernetesObjectMonitor.enabled' "${BUNDLE_DIR}/values.yaml")" == "true" ]] ||
    err "bundle's nvsentinel values.yaml does not set global.kubernetesObjectMonitor.enabled: true -- composition regressed"
  for policy in gpu-operator-pods-health network-operator-pod-health; do
    grep -q "${policy}" "${BUNDLE_DIR}/values.yaml" ||
      err "bundle's nvsentinel values.yaml is missing the ${policy} policy -- composition regressed"
  done
}

ensure_cluster() {
  # Always recreated, never reused. A leftover cluster carries the previous
  # run's fixture DaemonSets, node conditions and monitor logs -- a stale
  # `True` condition plus its matching old log line would satisfy the positive
  # assertions without the monitor emitting anything new. This cluster name is
  # dedicated to this test, so deleting it costs nothing but the rebuild.
  if grep -qx "${CLUSTER_NAME}" <<<"$(kind get clusters 2>/dev/null)"; then
    msg "Deleting stale Kind cluster ${CLUSTER_NAME}..."
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi
  msg "Creating Kind cluster ${CLUSTER_NAME}..."
  kind create cluster --name "${CLUSTER_NAME}" || err "kind create cluster failed"
  CREATED_CLUSTER=true
  # kind switches the default kubeconfig's current-context as a side effect of
  # create/delete; export into a per-run kubeconfig so this never touches the
  # user's own. Needed on the reuse path too, since a reused cluster's
  # credentials live wherever it was originally created.
  kind export kubeconfig --name "${CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" ||
    err "failed to export kubeconfig for ${CLUSTER_NAME}"
  kubectl --context "${KUBE_CONTEXT}" wait --for=condition=Ready node --all --timeout=120s ||
    err "cluster nodes never became Ready"
}

# await_workload_exists polls until $1/$2 exists in the nvsentinel namespace.
# `kubectl rollout status` on a missing object fails instantly instead of
# waiting, so this closes the window between helm returning and the controller
# creating the workload.
await_workload_exists() {
  local kind="$1" name="$2" elapsed=0
  while [[ "${elapsed}" -lt 120 ]]; do
    if kubectl --context "${KUBE_CONTEXT}" -n nvsentinel get "${kind}" "${name}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  err "${kind}/${name} was never created in the nvsentinel namespace"
}

install_nvsentinel() {
  msg "Installing prometheus-operator-crds and nvsentinel from the bundle..."
  for ns in gpu-operator nvidia-network-operator; do
    kubectl --context "${KUBE_CONTEXT}" create namespace "${ns}" --dry-run=client -o yaml |
      kubectl --context "${KUBE_CONTEXT}" apply -f - >/dev/null
  done

  (cd "${CRDS_DIR}" && chmod +x install.sh &&
    KUBE_CONTEXT="${KUBE_CONTEXT}" ./install.sh) ||
    err "prometheus-operator-crds install failed"
  (cd "${BUNDLE_DIR}" && chmod +x install.sh &&
    KUBE_CONTEXT="${KUBE_CONTEXT}" ./install.sh) ||
    err "nvsentinel install failed"

  # install.sh runs `helm upgrade --install` without --wait (COMPONENT_WAIT_ARGS
  # is only exported by deploy.sh, which this bypasses), so helm returns before
  # the controllers have created anything. `kubectl wait`/`rollout status`
  # against a not-yet-existing object exits immediately with "no matching
  # resources found" rather than honouring --timeout, so poll for existence
  # first -- otherwise a slow scheduler reads as a product failure.
  await_workload_exists deployment kubernetes-object-monitor
  await_workload_exists daemonset platform-connectors

  # Generous: covers a cold pull of the whole NVSentinel image set, observed
  # to exceed 180s on a runner with an empty image cache.
  kubectl --context "${KUBE_CONTEXT}" -n nvsentinel rollout status deployment/kubernetes-object-monitor \
    --timeout=600s ||
    err "kubernetes-object-monitor never became Ready"

  # The monitor publishes through platform-connectors' socket, so its own
  # readiness does not mean an event can be delivered.
  kubectl --context "${KUBE_CONTEXT}" -n nvsentinel rollout status daemonset/platform-connectors \
    --timeout=600s ||
    err "platform-connectors never rolled out"
}

# ── Assertion helpers ──

# require_monitor_ready fails the run unless the monitor is Running and Ready.
# "No condition appeared" is indistinguishable from "nothing was watching", so
# both negative assertions gate on this.
require_monitor_ready() {
  local ready
  ready=$(kubectl --context "${KUBE_CONTEXT}" -n nvsentinel get pods -l "${MONITOR_SELECTOR}" \
    -o jsonpath='{.items[?(@.status.phase=="Running")].status.conditions[?(@.type=="Ready")].status}') ||
    err "could not query kubernetes-object-monitor readiness"
  [[ "${ready}" == *"True"* ]] ||
    err "kubernetes-object-monitor is not Running/Ready (got '${ready:-<none>}')"
}

# node_condition_status echoes the named condition's status, or the empty
# string when the condition is absent. A failed API call is fatal rather than
# empty: treating an apiserver error as "condition absent" would let every
# negative assertion below pass for the wrong reason.
node_condition_status() {
  local condition="$1" node="$2" out
  # Returns kubectl's exit status rather than calling err: this runs inside a
  # command substitution, where err would only kill the subshell. The caller
  # checks the status explicitly.
  out=$(kubectl --context "${KUBE_CONTEXT}" get node "${node}" \
    -o jsonpath="{.status.conditions[?(@.type==\"${condition}\")].status}") || return 1
  printf '%s' "${out}"
}

# wait_for_node_condition polls condition $1 on node $4 for status $2 until $3
# seconds elapse. The node is a parameter, not nodes[0]: the policy associates
# by resource.spec.nodeName, so on a reused multi-node cluster nodes[0] can be
# the wrong node, silently inverting every assertion here.
wait_for_node_condition() {
  local condition="$1" want="$2" timeout="$3" node="$4" elapsed=0 got rc
  [[ -n "${node}" ]] || err "wait_for_node_condition called without a node"
  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    # The API status is captured separately from the value. Inlining this as
    # `[[ "$(node_condition_status ...)" == "$want" ]]` would turn an apiserver
    # failure into an ordinary mismatch, so a negative assertion could "pass"
    # after every single poll errored.
    got=$(node_condition_status "${condition}" "${node}")
    rc=$?
    [[ "${rc}" -eq 0 ]] || err "querying node ${node} for condition ${condition} failed (exit ${rc})"
    [[ "${got}" == "${want}" ]] && return 0
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

# Proves the backdate mechanism works before the caller relies on it: without
# this, patch_start_time_loop (which ignores per-iteration errors by design)
# could fail every time and the caller would blame the monitor for never
# firing. Retried, because kubelet re-syncs startTime continuously and can
# overwrite the patch before the read-back -- checking a single attempt is
# itself racy, and was observed failing on a cold run.
backdate_pod_start_time() {
  local pod="$1" observed
  for _ in 1 2 3 4 5; do
    kubectl --context "${KUBE_CONTEXT}" -n gpu-operator patch pod "${pod}" \
      --subresource=status --type=merge \
      -p "{\"status\":{\"startTime\":\"${BACKDATE_TS}\"}}" >/dev/null ||
      err "backdating ${pod}'s status.startTime failed -- cannot impersonate the grace period"
    observed=$(kubectl --context "${KUBE_CONTEXT}" -n gpu-operator get pod "${pod}" \
      -o jsonpath='{.status.startTime}') || err "reading back ${pod}'s startTime failed"
    [[ "${observed}" == "${BACKDATE_TS}" ]] && return 0
    sleep 1
  done
  err "status.startTime never held the backdated value across 5 attempts (wanted ${BACKDATE_TS}, last saw ${observed:-<empty>}) -- the apiserver may no longer accept this mutation"
}

patch_start_time_loop() {
  local pod="$1" duration="$2" elapsed=0
  while [[ "${elapsed}" -lt "${duration}" ]]; do
    kubectl --context "${KUBE_CONTEXT}" -n gpu-operator patch pod "${pod}" \
      --subresource=status --type=merge \
      -p "{\"status\":{\"startTime\":\"${BACKDATE_TS}\"}}" >/dev/null 2>&1 || true
    sleep 2
    elapsed=$((elapsed + 2))
  done
}

# published_event_for echoes the monitor's last "Publishing health event"
# record naming $1, or nothing. Both the positive and negative assertions go
# through it so they cannot drift apart: matching in two stages (message, then
# a fixed-string pod match) rather than one `msg.*pod` regex keeps it
# independent of slog's attribute order. $2 bounds the query by time instead of
# a line count, so a chatty log cannot scroll the evidence out of view.
# Returns nonzero if the log query fails, rather than calling err: this runs
# inside a command substitution, where err would exit only the subshell and
# leave the caller reading empty output as "no event published" -- passing a
# negative assertion on an apiserver hiccup. Callers must test the status.
published_event_for() {
  local pod="$1" since="$2" logs
  logs=$(kubectl --context "${KUBE_CONTEXT}" -n nvsentinel logs -l "${MONITOR_SELECTOR}" \
    --since-time="${since}") || return 1
  grep '"msg":"Publishing health event"' <<<"${logs}" | grep -F "${pod}" | tail -n 1 || true
}

# Asserts the event payload, which the node condition alone does not cover.
assert_health_event() {
  local node="$1" pod="$2" since="$3" event missing=()
  if ! event=$(published_event_for "${pod}" "${since}"); then
    err "could not read kubernetes-object-monitor logs -- cannot verify the published event"
  fi
  if [[ -z "${event}" ]]; then
    fail "nvsentinel-object-monitor/health-event" "no published health event mentions pod ${pod}"
    return
  fi

  [[ "${event}" == *'"checkName":"'"${GPU_POLICY_CONDITION}"'"'* ]] || missing+=("checkName")
  [[ "${event}" == *'"GPU_OPERATOR_POD_UNHEALTHY"'* ]] || missing+=("errorCode")
  [[ "${event}" == *'"isFatal":true'* ]] || missing+=("isFatal")
  [[ "${event}" == *'"message":"GPU Operator DaemonSet pod is not healthy"'* ]] || missing+=("message")
  [[ "${event}" == *'"nodeName":"'"${node}"'"'* ]] || missing+=("nodeName")
  [[ "${event}" == *"gpu-operator/${pod}"* ]] || missing+=("entitiesImpacted")

  if [[ "${#missing[@]}" -eq 0 ]]; then
    pass "nvsentinel-object-monitor/health-event"
  else
    fail "nvsentinel-object-monitor/health-event" "published event has wrong/missing: ${missing[*]}"
    detail "event=${event}"
  fi
}

assert_no_health_event() {
  local pod="$1" since="$2" name="${3:-grace-period-no-event}" event
  # `if ! event=$(...)` rather than a bare assignment plus `rc=$?`: under
  # `set -e` a failing assignment aborts before the status could be read.
  if ! event=$(published_event_for "${pod}" "${since}"); then
    err "could not read kubernetes-object-monitor logs -- cannot prove no event was published"
  fi
  if [[ -n "${event}" ]]; then
    fail "nvsentinel-object-monitor/${name}" "a health event was published for ${pod} when none was expected"
  else
    pass "nvsentinel-object-monitor/${name}"
  fi
}

# apply_daemonset creates a DaemonSet in gpu-operator running $2. An
# unresolvable image yields a permanently unhealthy pod; a real one yields a
# healthy rollout. $3 selects
# whether its pods carry the GPU Operator operand identity the policy requires
# ("operand", the default) or not ("unrelated"). Without that label a real
# cluster's unrelated workloads must not raise a GPU Operator health event, and
# an "operand" fixture is what makes the positive cases represent a real operand
# rather than any pod that happens to live in the namespace.
apply_daemonset() {
  local name="$1" image="$2" identity="${3:-operand}" identity_label=""
  if [[ "${identity}" == "operand" ]]; then
    identity_label="
        app.kubernetes.io/managed-by: gpu-operator"
  fi
  kubectl --context "${KUBE_CONTEXT}" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ${name}
  namespace: gpu-operator
spec:
  selector:
    matchLabels:
      app: ${name}
  template:
    metadata:
      labels:
        app: ${name}${identity_label}
    spec:
      containers:
        - name: main
          image: ${image}
EOF
}

UNHEALTHY_IMAGE="registry.invalid/does-not-exist:latest"
HEALTHY_IMAGE="registry.k8s.io/pause:3.9"

# Echoes "<pod> <node>" once the DaemonSet has a live, scheduled pod.
#
# Filters on deletionTimestamp and nodeName rather than taking items[0]: a pod
# from a previous DaemonSet generation can still be listed while it drains, and
# picking it would attach every later assertion to a pod on its way out.
await_scheduled_pod() {
  local name="$1" elapsed=0 line="" pod="" node=""
  while [[ "${elapsed}" -lt 60 ]]; do
    line=$(kubectl --context "${KUBE_CONTEXT}" -n gpu-operator get pods -l "app=${name}" \
      -o go-template='{{range .items}}{{if and (not .metadata.deletionTimestamp) .spec.nodeName}}{{.metadata.name}} {{.spec.nodeName}}{{"\n"}}{{end}}{{end}}' \
      2>/dev/null | head -n 1 || true)
    if [[ -n "${line}" ]]; then
      read -r pod node <<<"${line}"
      # Trailing newline matters: `read` returns non-zero on EOF without one,
      # which aborts the caller under `set -e` despite assigning both vars.
      [[ -n "${pod}" && -n "${node}" ]] && { printf '%s %s\n' "${pod}" "${node}"; return 0; }
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  err "DaemonSet ${name} never produced a live scheduled pod"
}

# ── Tests ──

test_unhealthy_pod_fires() {
  msg "TEST: unhealthy GPU Operator DaemonSet pod past the grace period"
  local since
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  apply_daemonset gpu-operator-unhealthy-e2e "${UNHEALTHY_IMAGE}"
  local scheduled pod node
  scheduled=$(await_scheduled_pod gpu-operator-unhealthy-e2e) ||
    err "unhealthy DaemonSet never produced a scheduled pod"
  read -r pod node <<<"${scheduled}"
  detail "pod=${pod} node=${node}"

  # Prove the backdating mechanism works before relying on it, so a harness
  # failure cannot masquerade as the monitor failing to fire.
  backdate_pod_start_time "${pod}"
  patch_start_time_loop "${pod}" "${PATCH_LOOP_SECONDS}" &
  PATCHER_PID=$!

  if wait_for_node_condition "${GPU_POLICY_CONDITION}" "True" "${CONDITION_WAIT_SECONDS}" "${node}"; then
    pass "nvsentinel-object-monitor/unhealthy-condition"
    assert_health_event "${node}" "${pod}" "${since}"
  else
    fail "nvsentinel-object-monitor/unhealthy-condition" "${GPU_POLICY_CONDITION} never went True within ${CONDITION_WAIT_SECONDS}s"
  fi

  kill "${PATCHER_PID}" 2>/dev/null || true
  wait "${PATCHER_PID}" 2>/dev/null || true
  PATCHER_PID=""

  # Cleanup only, deliberately unasserted. Recovery is not in #2612's scope,
  # and it cannot be attributed here: the one deletion-specific signal -- a
  # healthy event republished on delete -- is published by upstream's
  # cleanupDeletedResource only when the resource is still matched at that
  # moment, which the backdating above makes intermittent. Asserting on the
  # condition alone would claim a causal link the harness cannot establish.
  kubectl --context "${KUBE_CONTEXT}" -n gpu-operator delete daemonset gpu-operator-unhealthy-e2e --wait=false >/dev/null
}

# The pod here is unhealthy, identical to the positive test, and differs only
# in that its startTime is left alone -- so the grace period is the sole thing
# keeping the condition from firing. A healthy pod would not test the gate at
# all: it already fails the predicate's health clause, so the test would stay
# green even with duration('30m') set to 0s.
test_inside_grace_period_stays_quiet() {
  msg "TEST: unhealthy pod INSIDE the grace period produces neither event nor condition"
  local since
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  apply_daemonset gpu-operator-fresh-e2e "${UNHEALTHY_IMAGE}"
  local scheduled pod node
  scheduled=$(await_scheduled_pod gpu-operator-fresh-e2e) ||
    err "fresh unhealthy DaemonSet never produced a scheduled pod"
  read -r pod node <<<"${scheduled}"
  detail "pod=${pod} node=${node}"

  require_monitor_ready
  if wait_for_node_condition "${GPU_POLICY_CONDITION}" "True" "${CONDITION_WAIT_SECONDS}" "${node}"; then
    fail "nvsentinel-object-monitor/grace-period-no-condition" "${GPU_POLICY_CONDITION} went True for a pod still inside the grace period"
  else
    pass "nvsentinel-object-monitor/grace-period-no-condition"
  fi
  assert_no_health_event "${pod}" "${since}"

  # A crash part-way through would leave the window silently unobserved.
  require_monitor_ready
  kubectl --context "${KUBE_CONTEXT}" -n gpu-operator delete daemonset gpu-operator-fresh-e2e --wait=false >/dev/null
}

# The policy's blast-radius guard, and the only case here that exercises the
# operand-identity clause at runtime. This DaemonSet is identical to the
# positive case -- same namespace, same DaemonSet ownership, same unresolvable
# image, same backdated startTime -- and differs ONLY in carrying no
# app.kubernetes.io/managed-by label. Without that clause in the predicate it
# raises a fatal "GPU Operator DaemonSet pod is not healthy" for a workload the
# GPU Operator has nothing to do with.
test_unrelated_daemonset_stays_quiet() {
  msg "TEST: unhealthy NON-operand DaemonSet past the grace period produces neither event nor condition"
  local since
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  apply_daemonset gpu-operator-unrelated-e2e "${UNHEALTHY_IMAGE}" unrelated
  local scheduled pod node
  scheduled=$(await_scheduled_pod gpu-operator-unrelated-e2e) ||
    err "unrelated DaemonSet never produced a scheduled pod"
  read -r pod node <<<"${scheduled}"
  detail "pod=${pod} node=${node}"

  # Backdated exactly like the positive case, so the grace period cannot be
  # what keeps this quiet -- the identity clause has to be.
  backdate_pod_start_time "${pod}"
  patch_start_time_loop "${pod}" "${PATCH_LOOP_SECONDS}" &
  PATCHER_PID=$!

  require_monitor_ready
  if wait_for_node_condition "${GPU_POLICY_CONDITION}" "True" "${CONDITION_WAIT_SECONDS}" "${node}"; then
    fail "nvsentinel-object-monitor/unrelated-daemonset-no-condition" \
      "${GPU_POLICY_CONDITION} went True for a DaemonSet with no GPU Operator identity"
  else
    pass "nvsentinel-object-monitor/unrelated-daemonset-no-condition"
  fi
  assert_no_health_event "${pod}" "${since}" "unrelated-daemonset-no-event"

  kill "${PATCHER_PID}" 2>/dev/null || true
  wait "${PATCHER_PID}" 2>/dev/null || true
  PATCHER_PID=""

  require_monitor_ready
  kubectl --context "${KUBE_CONTEXT}" -n gpu-operator delete daemonset gpu-operator-unrelated-e2e --wait=false >/dev/null
}

# #2612 asks specifically for a healthy rollout inside the grace period to
# produce no event. It is a weaker assertion than the one above -- a healthy
# pod already fails the predicate's health clause, so this would stay green
# even with the grace period removed -- but it is the stated criterion, and it
# covers a case the unhealthy variant does not: a normal rollout, the thing
# operators actually see every day, must stay quiet.
test_healthy_rollout_stays_quiet() {
  msg "TEST: healthy DaemonSet rollout inside the grace period -> no event, no condition"
  local since
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  apply_daemonset gpu-operator-healthy-e2e "${HEALTHY_IMAGE}"

  # Resolve the pod before waiting on it: `kubectl wait` against a selector
  # matching nothing exits immediately with "no matching resources found"
  # rather than honouring --timeout, so a slow DaemonSet controller would abort
  # the run (same race as install_nvsentinel's).
  local scheduled pod node
  scheduled=$(await_scheduled_pod gpu-operator-healthy-e2e) ||
    err "healthy DaemonSet never produced a scheduled pod"
  read -r pod node <<<"${scheduled}"

  kubectl --context "${KUBE_CONTEXT}" -n gpu-operator wait --for=condition=Ready \
    "pod/${pod}" --timeout=120s ||
    err "healthy test pod ${pod} never became Ready"
  detail "pod=${pod} node=${node}"

  require_monitor_ready
  if wait_for_node_condition "${GPU_POLICY_CONDITION}" "True" "${CONDITION_WAIT_SECONDS}" "${node}"; then
    fail "nvsentinel-object-monitor/healthy-rollout-no-condition" "${GPU_POLICY_CONDITION} went True for a healthy rollout"
  else
    pass "nvsentinel-object-monitor/healthy-rollout-no-condition"
  fi
  assert_no_health_event "${pod}" "${since}" "healthy-rollout-no-event"
  require_monitor_ready

  kubectl --context "${KUBE_CONTEXT}" -n gpu-operator delete daemonset gpu-operator-healthy-e2e --wait=false >/dev/null
}

# ── Main ──

msg "=========================================="
msg "nvsentinel-object-monitor mixin runtime e2e"
msg "=========================================="

has_tools go kind kubectl helm yq
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
compose_bundle
ensure_cluster
install_nvsentinel
# Negative tests first, condition-producing test last. test_unhealthy_pod_fires
# deletes its DaemonSet with --wait=false and no longer waits for the condition
# to clear (recovery is unasserted -- see that function), so running it earlier
# would let a stale True from it fail whichever negative test came next.
test_inside_grace_period_stays_quiet
test_healthy_rollout_stays_quiet
test_unrelated_daemonset_stays_quiet
test_unhealthy_pod_fires

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel-object-monitor e2e: all tests passed"
