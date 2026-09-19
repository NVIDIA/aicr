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
# NPD -> nvsentinel-object-monitor mixin: live runtime e2e test (#2614)
# =============================================================================
#
# PURPOSE:
# The three NPD-derived KOM policies (recipes/mixins/nvsentinel-object-
# monitor.yaml) are unit-tested at the CEL-predicate level
# (pkg/recipe/nvsentinel_object_monitor_npd_predicate_test.go), but CEL
# evaluation alone cannot catch a break in chart enablement, policy
# serialization, RBAC/cache wiring, or platform-connector delivery. This
# script closes that gap by driving the real product path (aicr recipe ->
# aicr bundle -> the bundle's own install.sh) against a live Kind cluster,
# mirroring #2612's tests/e2e/nvsentinel-object-monitor/run.sh (same repo
# conventions, same tools/common helpers).
#
# WHAT THIS SCRIPT DOES:
# 1. Builds the aicr binary
# 2. Composes a recipe carrying ONLY the nvsentinel-object-monitor mixin
#    (deliberately NOT the npd mixin -- see NPD-VS-MANUAL-CONDITION NOTE
#    below) and bundles it
# 3. Creates an ephemeral Kind cluster (unless CLUSTER_NAME already exists)
# 4. Installs prometheus-operator-crds and nvsentinel via the bundle's own
#    generated install.sh
# 5. Patches the Node's XfsShutdown condition to status:True,
#    reason:XfsHasShutdown (the exact triple NPD itself would write) and
#    asserts kubernetes-object-monitor's own log shows it published the
#    NPDXfsShutdown health event, and platform-connectors' log shows it
#    received and correctly skipped remediation for it under STORE_ONLY
# 6. Clears the condition and asserts a recovery event fires
#
# NPD-VS-MANUAL-CONDITION NOTE: the npd mixin's own DaemonSet-rollout
# behavior is already covered by recipes/checks/node-problem-detector/
# health-check.yaml (make check-health) and by #2614's own manual live
# kind-cluster testing. This script's job is
# specifically the KOM leg of the pipeline -- does patching a Node
# condition (Node.status.conditions is all KOM reads; it has no notion of
# who wrote it) actually produce a HealthEvent end to end. Deploying real
# NPD alongside would add a second moving part (NPD's own
# settings.heartBeatPeriod, default 5m, periodically re-asserts its
# internal condition state) that only risks flaking this specific
# assertion without adding coverage.
#
# STORE_ONLY NOTE: unlike #2612's policies (no processingStrategy
# override, so they inherit the chart-wide EXECUTE_REMEDIATION default and
# assertably flip a Node condition), every NPD policy here sets
# processingStrategy: STORE_ONLY deliberately (see the mixin file's own
# header comment) -- platform-connectors explicitly skips writing a Node
# condition for a STORE_ONLY event ("Skipping non-remediation health
# event"). The only externally observable signal under the SHIPPED default
# is therefore the two components' own logs, not a Node condition -- this
# matches the design doc's own documented trade-off, not a test gap.
#
# BACKDATING CAVEAT: NOT applicable here (unlike #2612's pod
# status.startTime, which kubelet periodically resyncs) -- a Node
# condition set via kubectl patch --subresource=status is not owned or
# resynced by any controller in this test (no real NPD is deployed, see
# above), so a single patch is stable for the duration of the test.
#
# PREREQUISITES:
# - make, go, kind, kubectl, helm
#
# USAGE:
#   ./tests/e2e/npd-nvsentinel-object-monitor/run.sh
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-npd-object-monitor-e2e).
#                  Reused if it already exists; otherwise created and torn
#                  down at the end unless KEEP_CLUSTER=true.
#   KEEP_CLUSTER   Skip cluster teardown on exit (default: false). Useful for
#                  local debugging.
#   AICR_BIN       Path to a prebuilt aicr binary (default: built fresh via
#                  `go build`).
#
# =============================================================================

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DIR}/../../.." && pwd)"
# shellcheck source=/dev/null
. "${ROOT}/tools/common"

CLUSTER_NAME="${CLUSTER_NAME:-aicr-npd-object-monitor-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"

WORK=""
CREATED_CLUSTER=false
PC_POD=""
# Set only on the reuse path, where the Node outlives the run: the name of the
# patched Node, and its XfsShutdown condition exactly as found. An empty
# ORIG_XFS_CONDITION with a non-empty PATCHED_NODE means the condition did not
# exist and must be removed rather than restored.
PATCHED_NODE=""
ORIG_XFS_CONDITION=""

cleanup() {
  local rc=$?
  # WORK is deleted last, not first: KUBECONFIG lives inside it, so removing
  # it here would leave every kubectl call below without a kubeconfig, and
  # their `|| true` would hide the failure -- silently restoring nothing.
  # A cluster this run created is about to be deleted, so only the reuse path
  # needs its Node put back: otherwise the injected XfsShutdown lingers, and a
  # condition that existed beforehand has been overwritten.
  if [[ "${CREATED_CLUSTER}" != "true" && -n "${PATCHED_NODE}" ]]; then
    if [[ -n "${ORIG_XFS_CONDITION}" ]]; then
      msg "Restoring the original XfsShutdown condition on ${PATCHED_NODE}..."
      kubectl --context "${KUBE_CONTEXT}" patch node "${PATCHED_NODE}" --subresource=status \
        --type=strategic -p "{\"status\":{\"conditions\":[${ORIG_XFS_CONDITION}]}}" >/dev/null 2>&1 || true
    else
      msg "Removing the e2e-injected XfsShutdown condition from ${PATCHED_NODE}..."
      # $patch: delete removes one list entry by its patchMergeKey (type),
      # leaving Ready and the pressure conditions untouched.
      kubectl --context "${KUBE_CONTEXT}" patch node "${PATCHED_NODE}" --subresource=status \
        --type=strategic -p \
        '{"status":{"conditions":[{"type":"XfsShutdown","$patch":"delete"}]}}' >/dev/null 2>&1 || true
    fi
  fi
  if [[ "${CREATED_CLUSTER}" == "true" && "${KEEP_CLUSTER}" != "true" ]]; then
    msg "Deleting Kind cluster ${CLUSTER_NAME}..."
    kind delete cluster --name "${CLUSTER_NAME}" &>/dev/null || true
  elif [[ "${CREATED_CLUSTER}" == "true" ]]; then
    msg "KEEP_CLUSTER=true: leaving cluster ${CLUSTER_NAME} running for inspection."
  fi
  if [[ -n "${WORK}" && -d "${WORK}" ]]; then
    rm -rf "${WORK}"
  fi
  exit "${rc}"
}
trap cleanup EXIT

# ── Functions ──

build_binary() {
  if [[ -n "${AICR_BIN}" ]]; then
    msg "Using prebuilt AICR_BIN=${AICR_BIN}"
    return
  fi
  msg "Building aicr binary..."
  AICR_BIN="${WORK}/aicr"
  (cd "${ROOT}" && go build -o "${AICR_BIN}" ./cmd/aicr) || err "failed to build aicr binary"
}

compose_bundle() {
  msg "Composing recipe with the nvsentinel-object-monitor mixin..."
  local data_dir="${WORK}/data"
  mkdir -p "${data_dir}/overlays"

  cat >"${data_dir}/registry.yaml" <<'REGISTRY_EOF'
kind: ComponentRegistry
apiVersion: aicr.run/v1beta1
metadata:
  name: npd-object-monitor-e2e-registry
spec:
  components: []
REGISTRY_EOF

  cat >"${data_dir}/overlays/npd-object-monitor-e2e.yaml" <<'OVERLAY_EOF'
kind: RecipeMetadata
apiVersion: aicr.run/v1beta1
metadata:
  name: npd-object-monitor-e2e

spec:
  base: h100-kind-training
  mixins:
    - nvsentinel-object-monitor
  criteria:
    service: kind
    accelerator: h100
    intent: training
    os: ubuntu
OVERLAY_EOF

  "${AICR_BIN}" recipe \
    --service kind --accelerator h100 --intent training --os ubuntu \
    --data "${data_dir}" \
    --output "${WORK}/recipe.yaml" || err "recipe generation failed"

  "${AICR_BIN}" bundle -r "${WORK}/recipe.yaml" -o "${WORK}/bundle" || err "bundle generation failed"

  BUNDLE_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-nvsentinel' | head -1)
  CRDS_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-prometheus-operator-crds' | head -1)
  [[ -n "${BUNDLE_DIR}" ]] || err "bundled nvsentinel component directory not found"
  [[ -n "${CRDS_DIR}" ]] || err "bundled prometheus-operator-crds component directory not found"
  grep -q "NPDXfsShutdown" "${BUNDLE_DIR}/values.yaml" ||
    err "bundle's nvsentinel values.yaml does not carry the mixin's NPDXfsShutdown policy -- composition regressed"
}

ensure_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    msg "Reusing existing Kind cluster ${CLUSTER_NAME}"
  else
    msg "Creating Kind cluster ${CLUSTER_NAME}..."
    kind create cluster --name "${CLUSTER_NAME}" || err "kind create cluster failed"
    CREATED_CLUSTER=true
  fi
  kind export kubeconfig --name "${CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" ||
    err "failed to export kubeconfig for ${CLUSTER_NAME}"
  kubectl --context "${KUBE_CONTEXT}" wait --for=condition=Ready node --all --timeout=120s ||
    err "cluster nodes never became Ready"
}

# await_workload_exists blocks until $1/$2 exists in namespace $3. `kubectl wait`
# treats a missing object as an immediate failure rather than something to wait
# for, so every readiness wait after a non-waiting install needs this first.
await_workload_exists() {
  local kind="$1" name="$2" ns="$3" elapsed=0
  while [[ "${elapsed}" -lt 180 ]]; do
    if kubectl --context "${KUBE_CONTEXT}" -n "${ns}" get "${kind}" "${name}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  err "${kind}/${name} was never created in the ${ns} namespace"
}

install_nvsentinel() {
  msg "Installing prometheus-operator-crds (nvsentinel's CRD dependency)..."
  (cd "${CRDS_DIR}" && chmod +x install.sh &&
    KUBE_CONTEXT="${KUBE_CONTEXT}" ./install.sh) ||
    err "prometheus-operator-crds install failed"

  msg "Installing nvsentinel (with nvsentinel-object-monitor mixin values)..."
  (cd "${BUNDLE_DIR}" && chmod +x install.sh &&
    KUBE_CONTEXT="${KUBE_CONTEXT}" ./install.sh) ||
    err "nvsentinel install failed"

  # install.sh runs `helm upgrade --install` without --wait, so it returns
  # before the controller has created anything. `kubectl wait` does NOT wait for
  # a matching object to appear -- with no pod yet it fails immediately on "no
  # matching resources found" -- so wait for the Deployment to exist first, then
  # let rollout status cover readiness.
  msg "Waiting for kubernetes-object-monitor to become Ready..."
  await_workload_exists deployment kubernetes-object-monitor nvsentinel
  kubectl --context "${KUBE_CONTEXT}" -n nvsentinel rollout status \
    deployment/kubernetes-object-monitor --timeout=600s ||
    err "kubernetes-object-monitor never became Ready"

  # platform-connectors is the root chart's own DaemonSet, hardcoded to
  # render under app.kubernetes.io/name: nvsentinel regardless of
  # fullnameOverride (templates/_helpers.tpl's nvsentinel.fullname ignores
  # fullnameOverride for the root chart's own resources -- confirmed
  # directly against the rendered manifest, `helm template ... | grep -A2
  # 'kind: DaemonSet'`). That label also matches other root-chart
  # resources, so select by pod name prefix instead of a label.
  msg "Waiting for platform-connectors to become Ready..."
  local pc_elapsed=0
  while [[ -z "${PC_POD}" && "${pc_elapsed}" -lt 180 ]]; do
    PC_POD=$(kubectl --context "${KUBE_CONTEXT}" -n nvsentinel get pods -o name 2>/dev/null |
      grep '^pod/platform-connectors' | head -1 | sed 's#^pod/##' || true)
    [[ -n "${PC_POD}" ]] || { sleep 3; pc_elapsed=$((pc_elapsed + 3)); }
  done
  [[ -n "${PC_POD}" ]] || err "platform-connectors pod never appeared"
  kubectl --context "${KUBE_CONTEXT}" -n nvsentinel wait --for=condition=Ready "pod/${PC_POD}" --timeout=600s ||
    err "platform-connectors never became Ready"
}

# patch_node_condition sets the named Node condition to the given
# status/reason on the (single) Kind node.
patch_node_condition() {
  local condition_type="$1" status="$2" reason="$3"
  local node now
  node=$(kubectl --context "${KUBE_CONTEXT}" get nodes -o jsonpath='{.items[0].metadata.name}')
  # Snapshot once, before the first write, so cleanup can put the Node back on
  # a cluster this run did not create. Captured for XfsShutdown only -- the one
  # condition this test patches.
  if [[ "${condition_type}" == "XfsShutdown" && -z "${PATCHED_NODE}" ]]; then
    PATCHED_NODE="${node}"
    ORIG_XFS_CONDITION=$(kubectl --context "${KUBE_CONTEXT}" get node "${node}" \
      -o jsonpath='{.status.conditions[?(@.type=="XfsShutdown")]}' 2>/dev/null || true)
    # kubectl's jsonpath printer JSON-marshals a non-scalar value, so this is
    # a JSON object ready to splice into the restore patch. Verified on the
    # pinned kubectl, but guarded rather than assumed: anything that is not an
    # object is discarded, which downgrades cleanup to removing the injected
    # condition instead of emitting a malformed patch that silently restores
    # nothing.
    if [[ "${ORIG_XFS_CONDITION}" != "{"*"}" ]]; then
      [[ -n "${ORIG_XFS_CONDITION}" ]] &&
        msg "WARN: unexpected XfsShutdown snapshot format; cleanup will remove the injected condition instead of restoring."
      ORIG_XFS_CONDITION=""
    fi
  fi
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  # strategic, not merge: NodeCondition carries patchMergeKey "type", so a
  # strategic patch merges this one condition in by type. A JSON merge patch
  # replaces the whole conditions array, briefly dropping Ready and the pressure
  # conditions until kubelet rewrites them -- visible to anything else watching
  # the Node.
  kubectl --context "${KUBE_CONTEXT}" patch node "${node}" --subresource=status --type=strategic -p \
    "{\"status\":{\"conditions\":[{\"type\":\"${condition_type}\",\"status\":\"${status}\",\"reason\":\"${reason}\",\"message\":\"e2e-injected\",\"lastHeartbeatTime\":\"${now}\",\"lastTransitionTime\":\"${now}\"}]}}" \
    >/dev/null
}

# wait_for_log_line polls a target's logs for a substring until $4 seconds
# elapse. target is passed through to `kubectl logs` verbatim, so it may be
# a pod name (e.g. "platform-connectors-xyz") or a "-l selector=value"
# pair, depending on which uniquely identifies the target (see
# install_nvsentinel's comment on why platform-connectors needs a pod name,
# not a label). Returns 0 on match.
# All trailing substrings must appear on THE SAME log record, not merely
# somewhere in the window. Records are one JSON object per line, so chaining
# greps narrows to lines carrying every field. Matching them independently
# would let an unrelated policy's event satisfy a check about this one --
# every NPD policy here is STORE_ONLY, so "some STORE_ONLY event happened"
# says nothing about which check produced it.
wait_for_log_line() {
  local namespace="$1" target="$2" timeout="$3" since="$4"
  shift 4
  local elapsed=0 out pattern
  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    # shellcheck disable=SC2086 # target intentionally word-splits (e.g. "-l foo=bar")
    out=$(kubectl --context "${KUBE_CONTEXT}" -n "${namespace}" logs ${target} \
      --since-time="${since}" --tail=-1 2>/dev/null) || out=""
    if [[ -n "${out}" ]]; then
      local remaining="${out}" matched=true
      for pattern in "$@"; do
        remaining=$(grep -F "${pattern}" <<<"${remaining}") || { matched=false; break; }
      done
      if [[ "${matched}" == "true" ]]; then
        return 0
      fi
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

# log_baseline echoes an RFC3339 timestamp to pass to `kubectl logs
# --since-time`. Every assertion below is scoped to a baseline captured
# immediately BEFORE its own mutation, so a log line from an earlier run
# cannot satisfy it. Without that, the cluster-reuse path this script
# supports (CLUSTER_NAME already exists, or KEEP_CLUSTER=true) would let all
# three checks pass on stale entries without the injected condition
# producing anything at all.
log_baseline() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

test_condition_produces_health_event() {
  msg "TEST: XfsShutdown Node condition -> NPDXfsShutdown health event (STORE_ONLY)"

  local since
  since=$(log_baseline)
  patch_node_condition "XfsShutdown" "True" "XfsHasShutdown"

  if wait_for_log_line nvsentinel "-l app.kubernetes.io/name=kubernetes-object-monitor" \
    60 "${since}" '"checkName":"NPDXfsShutdown"'; then
    pass "npd-object-monitor/kom-publishes-event"
  else
    fail "npd-object-monitor/kom-publishes-event" "kubernetes-object-monitor never logged publishing an NPDXfsShutdown health event within 60s"
  fi

  # Match the skip branch's own message, not checkName+processingStrategy:
  # PlatformConnectorServer.HealthEventOccurredV1 logs the whole HealthEvents
  # payload ("Health events received") on arrival, before any strategy is
  # applied, and that record carries both of those fields -- so asserting on
  # them alone passes even when the STORE_ONLY branch never runs. This string
  # is emitted only by filterProcessableEvents' skip path
  # (platform-connectors/pkg/connectors/kubernetes/process_node_events.go).
  if wait_for_log_line nvsentinel "${PC_POD}" \
    60 "${since}" 'Skipping non-remediation health event' '"checkName":"NPDXfsShutdown"'; then
    pass "npd-object-monitor/platform-connectors-store-only"
  else
    fail "npd-object-monitor/platform-connectors-store-only" "platform-connectors never logged skipping the STORE_ONLY event within 60s"
  fi

  # STORE_ONLY suppresses the node-condition side effect by design (see the
  # mixin file's header comment) -- assert that too, since it's the whole
  # point of choosing STORE_ONLY over the chart-wide EXECUTE_REMEDIATION
  # default.
  # The read's exit status is captured, not discarded: `|| true` would turn an
  # apiserver failure into empty output and record "no condition written" as a
  # PASS -- a negative assertion that passes on an ambiguous condition.
  #
  # The status is captured inside if/else, not after a bare assignment: under
  # set -e a failing command substitution aborts the shell immediately, so a
  # following `read_rc=$?` never runs and this failure would never be recorded.
  # `if ! cmd` would not work either -- $? would then be the status of `!`.
  local node kom_condition read_rc
  node=$(kubectl --context "${KUBE_CONTEXT}" get nodes -o jsonpath='{.items[0].metadata.name}')
  if kom_condition=$(kubectl --context "${KUBE_CONTEXT}" get node "${node}" \
    -o jsonpath='{.status.conditions[?(@.type=="NPDXfsShutdown")].status}' 2>/dev/null); then
    read_rc=0
  else
    read_rc=$?
  fi
  if [[ "${read_rc}" -ne 0 ]]; then
    fail "npd-object-monitor/store-only-suppresses-node-condition" "could not read node conditions (kubectl exit ${read_rc}); the absence of a condition cannot be confirmed"
  elif [[ -z "${kom_condition}" ]]; then
    pass "npd-object-monitor/store-only-suppresses-node-condition"
  else
    fail "npd-object-monitor/store-only-suppresses-node-condition" "a NPDXfsShutdown node condition was written despite STORE_ONLY (got status=${kom_condition})"
  fi
}

test_recovery_event_fires() {
  msg "TEST: clearing the condition -> healthy NPDXfsShutdown recovery event"

  # Two conditions on the same record, both required: the policy's own name and
  # isHealthy true. checkName alone is satisfied by any further unhealthy
  # publish for this policy -- the reconciler republishes on resync -- so only
  # isHealthy separates a real recovery from a repeat.
  #
  # isHealthy renders ONLY on the recovery event. Protobuf JSON omits zero
  # values, so the field is absent from every unhealthy record; its absence
  # there says nothing about the schema. Upstream publishes it from
  # handleHealthyTransition (kubernetes-object-monitor reconciler.go).
  local since
  since=$(log_baseline)
  patch_node_condition "XfsShutdown" "False" "XfsHasNotShutDown"

  if wait_for_log_line nvsentinel "-l app.kubernetes.io/name=kubernetes-object-monitor" \
    60 "${since}" '"checkName":"NPDXfsShutdown"' '"isHealthy":true'; then
    pass "npd-object-monitor/recovery-event"
  else
    fail "npd-object-monitor/recovery-event" "kubernetes-object-monitor published no healthy (isHealthy: true) NPDXfsShutdown event within 60s of clearing the condition"
  fi
}

# ── Test bookkeeping (mirrors tests/e2e/run.sh) ──

TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

pass() {
  TOTAL_TESTS=$((TOTAL_TESTS + 1))
  PASSED_TESTS=$((PASSED_TESTS + 1))
  msg "PASS: $1"
}

fail() {
  TOTAL_TESTS=$((TOTAL_TESTS + 1))
  FAILED_TESTS=$((FAILED_TESTS + 1))
  log_error "FAIL: $1: ${2:-}"
}

print_summary() {
  msg "=========================================="
  msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
  msg "=========================================="
  [[ "${FAILED_TESTS}" -eq 0 ]]
}

# ── Main ──

main() {
  has_tools go kind kubectl helm

  WORK=$(mktemp -d /tmp/aicr-npd-object-monitor-e2e-XXXXXX)

  KUBECONFIG="${WORK}/kubeconfig"
  export KUBECONFIG

  build_binary
  compose_bundle
  ensure_cluster
  install_nvsentinel
  test_condition_produces_health_event
  test_recovery_event_fires

  print_summary
}

main
