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
# nvsentinel-observe mixin: dry-run e2e
# =============================================================================
#
# Installs the observe step through the bundle's own install.sh on Kind, sends
# synthetic GPU fault events into platform-connectors' socket, and asserts the
# dry-run pipeline carries each one to fault-remediation without cordoning or
# repairing anything.
#
# WHAT "RESOLVED ACTION" MEANS HERE. fault-remediation's dry run returns before
# it selects a CRD kind, so no log names GPUReset. What it does resolve is the
# COMPONENT_RESET action's equivalence group, and only the GPU-scoped mapping
# AICR ships requires a GPU_UUID there. So one event carries a GPU_UUID and must
# reach the dry-run skip; the other omits it and must be rejected for the
# missing GPU_UUID. The chart default (RebootNode, node-scoped) would accept
# both, so the second assertion fails if the mapping is lost.
#
# TEST-ONLY SETTINGS, passed as --set on the bundle:
#   node-drainer.systemNamespaces  replaced with a list that includes cert-manager
#       and local-path-storage.
#       In its chart-default AllowCompletion mode node-drainer evicts nothing
#       and waits for every non-DaemonSet pod outside systemNamespaces to
#       finish, so the drain would never end and fault-remediation would
#       never see the event. Kind has one node and both of those run on it.
#   fault-quarantine.circuitBreaker.enabled=false  the breaker trips once half
#       the GPU nodes are quarantined, which on a one-node cluster is the first
#       event.
#   nvsentinel-mongodb at one member with smaller requests, so it fits one
#       runner node. Percona refuses a one-member replica set without
#       unsafeFlags.replsetSize.
#
# PREREQUISITES: go, kind, kubectl, helm, yq, jq, curl
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-nvsentinel-remediation-e2e).
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

CLUSTER_NAME="${CLUSTER_NAME:-aicr-nvsentinel-remediation-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NODE="${CLUSTER_NAME}-control-plane"

GRPCURL_IMAGE="docker.io/fullstorydev/grpcurl:v1.9.3@sha256:085e183ca334eb4e81ca81ee12cbb2b2737505d1d77f5e33dabc5d066593d998"
DRAIN_SYSTEM_NAMESPACES='^(nvsentinel|kube-system|cert-manager|local-path-storage)$'
GPU_UUID="GPU-00000000-0000-0000-0000-00000000e2e0"
# Quarantine, drain and the dry-run remediation each react to a change stream,
# so the whole hop takes seconds; the margin covers a cold informer cache.
PIPELINE_WAIT_SECONDS=240
# One deadline per install, shared by its workloads because they pull in
# parallel.
CERT_MANAGER_WAIT_SECONDS=600
MONGODB_WAIT_SECONDS=900
NVSENTINEL_WAIT_SECONDS=900

WORK=""
CREATED_CLUSTER=false
BUNDLE_DIR=""
CRDS_DIR=""
CERT_MANAGER_DIR=""
PSMDB_OPERATOR_DIR=""
MONGODB_DIR=""
MONGODB_POST_DIR=""
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
  msg "Composing a bundle with the nvsentinel-observe mixin..."
  local data_dir="${WORK}/data"
  mkdir -p "${data_dir}/overlays"
  cat >"${data_dir}/registry.yaml" <<'REGISTRY_EOF'
kind: ComponentRegistry
apiVersion: aicr.run/v1beta1
metadata:
  name: nvsentinel-remediation-e2e-registry
components: []
REGISTRY_EOF
  yq '.spec.mixins = ["nvsentinel-observe"]' \
    "${ROOT}/recipes/overlays/h100-kind-training.yaml" \
    >"${data_dir}/overlays/h100-kind-training.yaml"

  "${AICR_BIN}" recipe --service kind --accelerator h100 --intent training \
    --data "${data_dir}" --output "${WORK}/recipe.yaml" ||
    err "recipe generation failed"
  "${AICR_BIN}" bundle -r "${WORK}/recipe.yaml" -o "${WORK}/bundle" \
    --set "nvsentinel:node-drainer.systemNamespaces=${DRAIN_SYSTEM_NAMESPACES}" \
    --set "nvsentinel:fault-quarantine.circuitBreaker.enabled=false" \
    --set "nvsentinelmongodb:unsafeFlags.replsetSize=true" \
    --set "nvsentinelmongodb:replsets.rs0.size=1" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.cpu=250m" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.memory=512Mi" \
    --system-node-selector kubernetes.io/os=linux ||
    err "bundle generation failed"

  BUNDLE_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-nvsentinel' -print -quit)
  CRDS_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-prometheus-operator-crds' -print -quit)
  CERT_MANAGER_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-cert-manager' -print -quit)
  PSMDB_OPERATOR_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-psmdb-operator' -print -quit)
  MONGODB_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-nvsentinel-mongodb' -print -quit)
  MONGODB_POST_DIR=$(find "${WORK}/bundle" -maxdepth 1 -type d -name '*-nvsentinel-mongodb-post' -print -quit)
  [[ -n "${BUNDLE_DIR}" ]] || err "bundled nvsentinel component directory not found"
  [[ -n "${CRDS_DIR}" ]] || err "bundled prometheus-operator-crds directory not found"
  [[ -n "${CERT_MANAGER_DIR}" ]] || err "bundled cert-manager directory not found"
  [[ -n "${PSMDB_OPERATOR_DIR}" ]] || err "bundled psmdb-operator directory not found"
  [[ -n "${MONGODB_DIR}" ]] || err "bundled nvsentinel-mongodb directory not found"
  [[ -n "${MONGODB_POST_DIR}" ]] || err "bundled nvsentinel-mongodb-post directory not found"

  # Fail here, not later as "nothing happened": a mixin that stopped composing
  # would otherwise read as a pipeline that never fired.
  local values="${BUNDLE_DIR}/values.yaml"
  [[ "$(yq '.global.dryRun' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not set global.dryRun: true -- composition regressed"
  [[ "$(yq '.global.faultRemediation.enabled' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not enable fault-remediation -- composition regressed"
  [[ "$(yq '.["fault-remediation"].maintenance.actions.COMPONENT_RESET.kind' "${values}")" == "GPUReset" ]] ||
    err "bundle's COMPONENT_RESET does not map to GPUReset"
  [[ "$(yq '.["node-drainer"].systemNamespaces' "${values}")" == "${DRAIN_SYSTEM_NAMESPACES}" ]] ||
    err "the test-only systemNamespaces override did not reach the bundle"
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

install_component() {
  (cd "$1" && chmod +x install.sh && KUBE_CONTEXT="${KUBE_CONTEXT}" ./install.sh) ||
    err "$(basename "$1") install failed"
}

install_bundle() {
  msg "Installing cert-manager, prometheus-operator-crds, the Percona datastore and nvsentinel from the bundle..."
  local dir d deadline
  for dir in "${CERT_MANAGER_DIR}" "${CRDS_DIR}"; do
    install_component "${dir}"
  done
  # The Percona operator issues the cluster's TLS through cert-manager.
  deadline=$((SECONDS + CERT_MANAGER_WAIT_SECONDS))
  for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
    wait_rollout deployment "${d}" cert-manager "${deadline}"
  done

  install_component "${PSMDB_OPERATOR_DIR}"
  deadline=$((SECONDS + MONGODB_WAIT_SECONDS))
  wait_rollout deployment psmdb-operator nvsentinel "${deadline}"
  install_component "${MONGODB_DIR}"
  install_component "${MONGODB_POST_DIR}"
  kc -n nvsentinel wait --for=jsonpath='{.status.state}'=ready \
    perconaservermongodb/nvsentinel-mongodb --timeout="$((deadline - SECONDS))s" ||
    err "nvsentinel-mongodb never became ready"
  kc -n nvsentinel wait --for=condition=Ready certificate/nvsentinel-mongodb-app-client \
    --timeout="$((deadline - SECONDS))s" || err "nvsentinel's MongoDB client certificate was never issued"

  install_component "${BUNDLE_DIR}"

  deadline=$((SECONDS + NVSENTINEL_WAIT_SECONDS))
  for d in fault-quarantine node-drainer fault-remediation; do
    wait_rollout deployment "${d}" nvsentinel "${deadline}"
  done
  wait_rollout daemonset platform-connectors nvsentinel "${deadline}"
}

# The pinned version's own proto, less the CRD-generator import and the one
# message that uses it; neither affects the wire format.
publish_proto() {
  local version
  version=$(yq '.components[] | select(.name == "nvsentinel") | .helm.defaultVersion' "${ROOT}/recipes/registry.yaml")
  [[ -n "${version}" && "${version}" != "null" ]] || err "could not read nvsentinel's defaultVersion"
  curl -fsSL --connect-timeout 10 --max-time 60 \
    "https://raw.githubusercontent.com/NVIDIA/NVSentinel/${version}/data-models/protobufs/health_event.proto" |
    awk '/protoc_gen_crd\/proto\/crd.proto/ {next} /^\/\/ HealthEventResource is the root type/ {exit} {print}' \
      >"${WORK}/health_event.proto" || err "failed to fetch health_event.proto at ${version}"
  grep -q "rpc HealthEventOccurredV1" "${WORK}/health_event.proto" ||
    err "health_event.proto at ${version} has no HealthEventOccurredV1 -- the platform-connector API moved"
  kc -n nvsentinel create configmap e2e-health-event-proto \
    --from-file=health_event.proto="${WORK}/health_event.proto" --dry-run=client -o yaml |
    kc apply -f - >/dev/null
}

# health_event prints one HealthEvents request. $1 is the check's XID code,
# $2 "with-uuid" or "without-uuid".
health_event() {
  local code="$1" uuid="$2" entities
  entities='[{"entityType":"GPU","entityValue":"0"}]'
  if [[ "${uuid}" == "with-uuid" ]]; then
    entities=$(jq -c --arg u "${GPU_UUID}" '. + [{"entityType":"GPU_UUID","entityValue":$u}]' <<<"${entities}")
  fi
  jq -cn --arg node "${NODE}" --arg code "${code}" --argjson entities "${entities}" \
    --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{
      version: 1,
      events: [{
        version: 1,
        agent: "gpu-health-monitor",
        componentClass: "GPU",
        checkName: "GpuXidError",
        isFatal: true,
        isHealthy: false,
        message: ("aicr e2e synthetic XID " + $code),
        recommendedAction: "COMPONENT_RESET",
        errorCode: [$code],
        entitiesImpacted: $entities,
        generatedTimestamp: $ts,
        nodeName: $node,
        processingStrategy: "EXECUTE_REMEDIATION"
      }]
    }'
}

# send_event delivers $2 through platform-connectors' socket from a pod on the
# node, which is what a node-local monitor does.
send_event() {
  local name="$1" payload="$2"
  jq -n --arg name "${name}" --arg node "${NODE}" --arg image "${GRPCURL_IMAGE}" --arg data "${payload}" '{
    apiVersion: "v1", kind: "Pod",
    metadata: {name: $name, namespace: "nvsentinel"},
    spec: {
      nodeName: $node,
      restartPolicy: "Never",
      tolerations: [{operator: "Exists"}],
      containers: [{
        name: "grpcurl", image: $image,
        # unix:// rather than -unix, which grpcurl v1.9.3 ignores and dials TCP.
        args: ["-plaintext", "-import-path", "/protos", "-proto", "health_event.proto",
               "-d", $data, "unix:///sock/nvsentinel.sock", "datamodels.PlatformConnector/HealthEventOccurredV1"],
        volumeMounts: [{name: "sock", mountPath: "/sock"}, {name: "protos", mountPath: "/protos"}]
      }],
      volumes: [
        {name: "sock", hostPath: {path: "/var/run/nvsentinel", type: "Directory"}},
        {name: "protos", configMap: {name: "e2e-health-event-proto"}}
      ]
    }
  }' | kc apply -f - >/dev/null
  if ! kc -n nvsentinel wait --for=jsonpath='{.status.phase}'=Succeeded "pod/${name}" --timeout=180s >/dev/null; then
    kc -n nvsentinel logs "pod/${name}" 2>&1 | tail -20 >&2 || true
    err "event ${name} was not accepted by platform-connectors"
  fi
}

# wait_for_log polls deployment $1's logs since $2 for a line containing both
# $3 and $4.
wait_for_log() {
  local deploy="$1" since="$2" needle="$3" also="$4" elapsed=0
  while [[ "${elapsed}" -lt "${PIPELINE_WAIT_SECONDS}" ]]; do
    if kc -n nvsentinel logs "deployment/${deploy}" --since-time="${since}" 2>/dev/null |
      grep -F -- "${needle}" | grep -qF -- "${also}"; then
      return 0
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

wait_for_condition() {
  local type="$1" elapsed=0
  while [[ "${elapsed}" -lt 60 ]]; do
    if kc get node "${NODE}" -o json |
      jq -e --arg t "${type}" '.status.conditions[] | select(.type == $t and .status == "True")' >/dev/null; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  return 1
}

node_untouched() {
  local unschedulable taints
  unschedulable=$(kc get node "${NODE}" -o jsonpath='{.spec.unschedulable}')
  taints=$(kc get node "${NODE}" -o jsonpath='{.spec.taints}')
  [[ "${unschedulable}" != "true" && "${taints}" != *nvidia* ]]
}

# ── Tests ──

test_gpu_scoped_event_reaches_dry_run() {
  local since
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  send_event e2e-event-with-uuid "$(health_event 119 with-uuid)"

  if wait_for_condition GpuXidError; then
    pass "event delivered: node condition GpuXidError is True"
  else
    fail "event delivered" "platform-connectors accepted the event but set no GpuXidError condition"
  fi

  if wait_for_log fault-quarantine "${since}" "Cordoning node" "${NODE}"; then
    pass "fault-quarantine matched the event and decided to cordon"
  else
    fail "fault-quarantine matched the event" "no 'Cordoning node' for ${NODE} within ${PIPELINE_WAIT_SECONDS}s"
  fi

  if wait_for_log fault-remediation "${since}" "DRY-RUN: Skipping custom resource creation" "${NODE}"; then
    pass "fault-remediation resolved the GPU-scoped action and skipped it in dry-run"
  else
    fail "fault-remediation dry-run" "no dry-run skip for ${NODE} within ${PIPELINE_WAIT_SECONDS}s"
  fi

  if node_untouched; then
    pass "dry-run left the node schedulable and untainted"
  else
    fail "dry-run left the node alone" "node is cordoned or tainted: $(kc get node "${NODE}" -o jsonpath='{.spec}')"
  fi

  # janitor's CRDs may or may not be installed in observe. A kind whose CRD is
  # absent can have no objects; any other failed lookup is inconclusive.
  local kind crd found repairs="" rc
  for kind in gpuresets rebootnodes; do
    crd="${kind}.janitor.dgxc.nvidia.com"
    rc=0
    found=$(kc get crd "${crd}" -o name 2>"${WORK}/repairs.err") || rc=$?
    if [[ "${rc}" -ne 0 ]]; then
      if grep -q NotFound "${WORK}/repairs.err"; then
        continue
      fi
      fail "no repair objects in observe" "looking up CRD ${crd} failed (rc=${rc}): $(cat "${WORK}/repairs.err")"
      return
    fi
    rc=0
    found=$(kc get "${crd}" -A -o name 2>"${WORK}/repairs.err") || rc=$?
    if [[ "${rc}" -ne 0 ]]; then
      fail "no repair objects in observe" "listing ${crd} failed (rc=${rc}): $(cat "${WORK}/repairs.err")"
      return
    fi
    [[ -z "${found}" ]] || repairs+="${found//$'\n'/ } "
  done
  if [[ -n "${repairs}" ]]; then
    fail "no repair objects in observe" "dry-run created repair objects: ${repairs}"
  else
    pass "no repair objects in observe"
  fi
}

test_component_reset_requires_gpu_uuid() {
  local since
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  send_event e2e-event-without-uuid "$(health_event 79 without-uuid)"

  # Only AICR's GPUReset mapping scopes COMPONENT_RESET to a GPU_UUID; the
  # chart's RebootNode default would take this event as readily as the first.
  if wait_for_log fault-remediation "${since}" \
    "missing impacted entity for GPU_UUID required by action COMPONENT_RESET" "error"; then
    pass "COMPONENT_RESET resolves to the GPU_UUID-scoped mapping"
  else
    fail "COMPONENT_RESET resolves to the GPU_UUID-scoped mapping" \
      "an event without a GPU_UUID was not rejected -- COMPONENT_RESET may have fallen back to the node-scoped chart default"
  fi

  if node_untouched; then
    pass "node still schedulable after the second event"
  else
    fail "node still schedulable" "node is cordoned or tainted after the second event"
  fi
}

# ── Main ──

msg "=========================================="
msg "nvsentinel-observe mixin dry-run e2e"
msg "=========================================="

has_tools go kind kubectl helm yq jq curl
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
compose_bundle
ensure_cluster
install_bundle
publish_proto
test_gpu_scoped_event_reaches_dry_run
test_component_reset_requires_gpu_uuid

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel-observe e2e: all tests passed"
