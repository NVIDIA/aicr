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
# nvsentinel-slinky-drainer mixin: runtime e2e (#2615)
# =============================================================================
#
# Composes nvsentinel-quarantine and nvsentinel-slinky-drainer onto the
# shipped h100-kind-training-slurm leaf, installs the bundle's own install.sh
# for the components the drain path needs on Kind, sends a fatal GPU event into
# platform-connectors' socket, and asserts the node is drained through Slurm:
#
#   fault          -> fault-quarantine cordons the node, node-drainer creates a
#                     DrainRequest, and the plugin writes the cordon-reason
#                     annotation
#   job running    -> the slurmd pod is kept and the request stays incomplete,
#                     so nothing is evicted while Slurm is still busy
#   job finished   -> the plugin deletes the slurmd pod, marks the request
#                     DrainComplete, and node-drainer records the drain as
#                     done and deletes the request; pods outside the slurm
#                     namespace keep running
#
# node-drainer v1.28.0 records a completed custom drain as AlreadyDrained in
# the datastore, which fault-remediation reads as drained, and leaves the node
# labelled draining: only its eviction path relabels to drain-succeeded. So the
# completion is asserted from node-drainer's own log, not the label.
#
# SIMULATED SLINKY. Slinky is not installed: a pod carrying the slurm-operator
# v1.2.0 worker labels stands in for slurmd, and the test writes the
# SlurmNodeState* pod conditions the NodeSet controller would (Drain once the
# node is cordoned, Allocated while a job runs). The plugin reads exactly those
# conditions, so this exercises its contract with node-drainer and the pods
# without standing up slurmctld.
#
# TEST-ONLY SETTINGS, passed as --set on the bundle:
#   fault-quarantine.circuitBreaker.enabled=false  the breaker trips once half
#       the GPU nodes are quarantined, which on a one-node cluster is the first
#       event.
#   nvsentinel-mongodb at one member with smaller requests, so it fits one
#       runner node. Percona refuses a one-member replica set without
#       unsafeFlags.replsetSize.
#   slinky-slurm's controller, REST API, login and slurmd selectors, which
#       the Kind leaf leaves empty because it is one node. On one node those
#       pods share slurmd's node, and CheckNVSentinelSlurmCustomDrainCoherent
#       rejects that because the plugin would wait on them forever; it
#       accepts only selectors that give one key different values. Slinky is
#       not installed here, so the selectors only have to get the bundle past
#       the gate.
#
# PREREQUISITES: go, kind, kubectl, helm, yq, jq, curl
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-nvsentinel-slinky-drainer-e2e).
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

CLUSTER_NAME="${CLUSTER_NAME:-aicr-nvsentinel-slinky-drainer-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NODE="${CLUSTER_NAME}-control-plane"

GRPCURL_IMAGE="docker.io/fullstorydev/grpcurl:v1.9.3@sha256:085e183ca334eb4e81ca81ee12cbb2b2737505d1d77f5e33dabc5d066593d998"
WORKER_IMAGE="registry.k8s.io/pause:3.10"
SLURM_NAMESPACE="slurm"
SLURM_CONTROL_SELECTOR='{"aicr.test/slurm-role":"control"}'
SLURM_WORKER_SELECTOR='{"aicr.test/slurm-role":"worker"}'
WORKER_POD="slurmd-e2e"
XID="119"
CORDON_REASON_ANNOTATION="nodeset.slinky.slurm.net/node-cordon-reason"
# Quarantine, drain and the plugin each react to a change stream, so a hop
# takes seconds; the margin covers a cold informer cache.
PIPELINE_WAIT_SECONDS=240
# The --timeout deploy.sh passes with --wait (its HELM_TIMEOUT default).
CRD_HELM_TIMEOUT=10m
# How long the request must stay incomplete while the simulated job runs. The
# plugin rechecks every 5s (--pod-check-interval), so this spans several passes.
HOLD_SECONDS=45
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
PLUGIN_DIR=""
PLUGIN_CRDS_DIR=""
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

bundle_dir() {
  local dir
  # -print -quit rather than `| head -1`: head closing the pipe SIGPIPEs find.
  dir=$(find "${WORK}/bundle" -maxdepth 1 -type d -name "*-$1" -print -quit)
  [[ -n "${dir}" ]] || err "bundled $1 directory not found"
  printf '%s' "${dir}"
}

compose_bundle() {
  msg "Composing h100-kind-training-slurm with nvsentinel-quarantine and nvsentinel-slinky-drainer..."
  local data_dir="${WORK}/data"
  mkdir -p "${data_dir}/overlays"
  cat >"${data_dir}/registry.yaml" <<'REGISTRY_EOF'
kind: ComponentRegistry
apiVersion: aicr.run/v1beta1
metadata:
  name: nvsentinel-slinky-drainer-e2e-registry
components: []
REGISTRY_EOF
  # Appended to the leaf's own mixins, so nvsentinel-slurm-drain-monitor stays
  # composed as on every shipped Slurm leaf.
  yq '.spec.mixins += ["nvsentinel-quarantine", "nvsentinel-slinky-drainer"]' \
    "${ROOT}/recipes/overlays/h100-kind-training-slurm.yaml" \
    >"${data_dir}/overlays/h100-kind-training-slurm.yaml"

  # CheckNVSentinelSlurmCustomDrainCoherent runs here, so this also proves the
  # gate passes on the supported shape.
  "${AICR_BIN}" recipe --service kind --accelerator h100 --intent training --platform slurm \
    --data "${data_dir}" --output "${WORK}/recipe.yaml" ||
    err "recipe generation failed"
  "${AICR_BIN}" bundle -r "${WORK}/recipe.yaml" -o "${WORK}/bundle" --data "${data_dir}" \
    --set "nvsentinel:fault-quarantine.circuitBreaker.enabled=false" \
    --set "nvsentinelmongodb:unsafeFlags.replsetSize=true" \
    --set "nvsentinelmongodb:replsets.rs0.size=1" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.cpu=250m" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.memory=512Mi" \
    --set-json "slinkyslurm:controller.podSpec.nodeSelector=${SLURM_CONTROL_SELECTOR}" \
    --set-json "slinkyslurm:restapi.podSpec.nodeSelector=${SLURM_CONTROL_SELECTOR}" \
    --set-json "slinkyslurm:loginsets.slinky.podSpec.nodeSelector=${SLURM_CONTROL_SELECTOR}" \
    --set-json "slinkyslurm:nodesets.slinky.podSpec.nodeSelector=${SLURM_WORKER_SELECTOR}" \
    --system-node-selector kubernetes.io/os=linux ||
    err "bundle generation failed"

  BUNDLE_DIR=$(bundle_dir nvsentinel)
  CRDS_DIR=$(bundle_dir prometheus-operator-crds)
  CERT_MANAGER_DIR=$(bundle_dir cert-manager)
  PSMDB_OPERATOR_DIR=$(bundle_dir psmdb-operator)
  MONGODB_DIR=$(bundle_dir nvsentinel-mongodb)
  MONGODB_POST_DIR=$(bundle_dir nvsentinel-mongodb-post)
  PLUGIN_DIR=$(bundle_dir slinky-drainer)
  PLUGIN_CRDS_DIR=$(bundle_dir slinky-drainer-crds)
  # Deployers install bundle folders in order, so the CRD's must come first.
  local crds_order plugin_order sentinel_order
  crds_order=$(basename "${PLUGIN_CRDS_DIR}" | cut -d- -f1)
  plugin_order=$(basename "${PLUGIN_DIR}" | cut -d- -f1)
  sentinel_order=$(basename "${BUNDLE_DIR}" | cut -d- -f1)
  ((10#${crds_order} < 10#${plugin_order} && 10#${crds_order} < 10#${sentinel_order})) ||
    err "bundle installs slinky-drainer-crds (${crds_order}) after slinky-drainer (${plugin_order}) or nvsentinel (${sentinel_order})"

  # Fail here, not later as "nothing happened": a mixin that stopped composing
  # would otherwise read as a drain that never started.
  local values="${BUNDLE_DIR}/values.yaml"
  [[ "$(yq '.["node-drainer"].customDrain.enabled' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not enable node-drainer.customDrain -- composition regressed"
  [[ "$(yq '.["node-drainer"].userNamespaces | length' "${values}")" == "0" ]] ||
    err "bundle's nvsentinel values carry node-drainer.userNamespaces -- node-drainer would refuse to start"
  [[ "$(yq '.global.dryRun' "${values}")" == "false" ]] ||
    err "bundle's nvsentinel values are not live (global.dryRun != false)"
}

ensure_cluster() {
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

# install_component <dir> [helm wait args]: install.sh passes
# COMPONENT_WAIT_ARGS to helm, which deploy.sh sets per component; without it
# helm does not wait at all.
install_component() {
  (cd "$1" && chmod +x install.sh && KUBE_CONTEXT="${KUBE_CONTEXT}" COMPONENT_WAIT_ARGS="${2:-}" ./install.sh) ||
    err "$(basename "$1") install failed"
}

install_bundle() {
  msg "Installing prometheus-operator-crds, cert-manager, the Percona datastore, slinky-drainer-crds, slinky-drainer and nvsentinel from the bundle..."
  local dir d deadline remaining
  # The namespace slinky-slurm would create; the plugin and the drain monitor
  # watch it.
  kc create namespace "${SLURM_NAMESPACE}" --dry-run=client -o yaml | kc apply -f - >/dev/null

  # CRDs first: cert-manager's chart renders a ServiceMonitor.
  for dir in "${CRDS_DIR}" "${CERT_MANAGER_DIR}"; do
    install_component "${dir}"
  done
  deadline=$((SECONDS + CERT_MANAGER_WAIT_SECONDS))
  for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
    wait_rollout deployment "${d}" cert-manager "${deadline}"
  done

  install_component "${PSMDB_OPERATOR_DIR}"
  deadline=$((SECONDS + MONGODB_WAIT_SECONDS))
  wait_rollout deployment psmdb-operator nvsentinel "${deadline}"
  install_component "${MONGODB_DIR}"
  install_component "${MONGODB_POST_DIR}"
  remaining=$((deadline - SECONDS))
  ((remaining > 0)) || err "nvsentinel-mongodb never became ready"
  kc -n nvsentinel wait --for=jsonpath='{.status.state}'=ready \
    perconaservermongodb/nvsentinel-mongodb --timeout="${remaining}s" ||
    err "nvsentinel-mongodb never became ready"
  remaining=$((deadline - SECONDS))
  ((remaining > 0)) || err "nvsentinel's MongoDB client certificate was never issued"
  kc -n nvsentinel wait --for=condition=Ready certificate/nvsentinel-mongodb-app-client \
    --timeout="${remaining}s" || err "nvsentinel's MongoDB client certificate was never issued"

  # In bundle order: node-drainer and the plugin exit at startup without the
  # CRD, and node-drainer without the template ConfigMap. With the wait
  # deploy.sh gives it, helm returns only once the CRD is Established.
  install_component "${PLUGIN_CRDS_DIR}" "--wait --timeout ${CRD_HELM_TIMEOUT}"
  kc wait --for=condition=Established crd/drainrequests.nvsentinel.nvidia.com --timeout=1s ||
    err "DrainRequest CRD not Established after the slinky-drainer-crds install returned"
  install_component "${PLUGIN_DIR}"
  deadline=$((SECONDS + NVSENTINEL_WAIT_SECONDS))
  wait_rollout deployment slinky-drainer nvsentinel "${deadline}"
  local restarts
  restarts=$(kc -n nvsentinel get pods -l app=slinky-drainer \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[*].restartCount}{"\n"}{end}' | awk '{s += $1} END {print s + 0}')
  [[ "${restarts}" == "0" ]] ||
    err "slinky-drainer restarted ${restarts} time(s); it must start against an Established CRD"

  install_component "${BUNDLE_DIR}"
  for d in fault-quarantine node-drainer; do
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

# ── Fixtures ──

create_worker() {
  kc -n "${SLURM_NAMESPACE}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${WORKER_POD}
  labels:
    app.kubernetes.io/name: slurmd
    app.kubernetes.io/component: worker
spec:
  nodeName: ${NODE}
  containers:
    - name: slurmd
      image: ${WORKER_IMAGE}
EOF
  kc -n "${SLURM_NAMESPACE}" wait --for=condition=Ready "pod/${WORKER_POD}" --timeout=180s >/dev/null ||
    err "worker fixture ${WORKER_POD} never became Ready"
}

# set_slurm_state writes SlurmNodeState* conditions on the worker the way the
# Slinky NodeSet controller does. Each argument is TYPE=True or TYPE=delete. A
# strategic-merge patch keyed on the condition type touches only these, so it
# cannot race kubelet's own status writes, and kubelet preserves condition
# types it does not own.
set_slurm_state() {
  local patch
  patch=$(jq -nc --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '
    {status: {conditions: [$ARGS.positional[] | split("=") |
      if .[1] == "delete" then {type: .[0], "$patch": "delete"}
      else {type: .[0], status: .[1], lastTransitionTime: $now} end]}}' --args "$@") ||
    err "building the Slurm state patch failed"
  kc -n "${SLURM_NAMESPACE}" patch pod "${WORKER_POD}" --subresource=status --type=strategic \
    -p "${patch}" >/dev/null || err "patching Slurm state onto ${WORKER_POD} failed"
}

send_fault() {
  local payload
  payload=$(jq -cn --arg node "${NODE}" --arg code "${XID}" --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{
    version: 1,
    events: [{
      version: 1,
      agent: "gpu-health-monitor",
      componentClass: "GPU",
      checkName: "GpuXidError",
      isFatal: true,
      isHealthy: false,
      message: ("aicr e2e synthetic XID " + $code),
      recommendedAction: "RESTART_VM",
      errorCode: [$code],
      entitiesImpacted: [{entityType: "GPU", entityValue: "0"}],
      generatedTimestamp: $ts,
      nodeName: $node,
      processingStrategy: "EXECUTE_REMEDIATION"
    }]
  }')
  jq -n --arg node "${NODE}" --arg image "${GRPCURL_IMAGE}" --arg data "${payload}" '{
    apiVersion: "v1", kind: "Pod",
    metadata: {name: "e2e-fault", namespace: "nvsentinel"},
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
  if ! kc -n nvsentinel wait --for=jsonpath='{.status.phase}'=Succeeded pod/e2e-fault --timeout=180s >/dev/null; then
    kc -n nvsentinel logs pod/e2e-fault 2>&1 | tail -20 >&2 || true
    err "the fault event was not accepted by platform-connectors"
  fi
}

# ── Assertion helpers ──

# poll runs "$@" every 3s until it succeeds or PIPELINE_WAIT_SECONDS pass.
poll() {
  local elapsed=0
  while [[ "${elapsed}" -lt "${PIPELINE_WAIT_SECONDS}" ]]; do
    "$@" && return 0
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

node_cordoned() { [[ "$(kc get node "${NODE}" -o jsonpath='{.spec.unschedulable}')" == "true" ]]; }

# drain_request echoes the DrainRequest node-drainer created for the node.
# Fails, rather than echoing nothing, when the list itself fails, so an
# apiserver hiccup cannot read as "no request".
drain_request() {
  local out
  out=$(kc -n nvsentinel get drainrequests.nvsentinel.nvidia.com \
    -l "nvsentinel.nvidia.com/node-name=${NODE}" -o json) || return 2
  jq -c '.items[0] // empty' <<<"${out}"
}

drain_request_exists() { [[ -n "$(drain_request)" ]]; }

# drain_complete returns 0 when the request reports DrainComplete=True, 1 when
# it does not, and 2 when it cannot be read.
drain_complete() {
  local dr
  dr=$(drain_request) || return 2
  # jq -e exits 4, not 1, when the request has no conditions yet.
  jq -e '.status.conditions[]? | select(.type == "DrainComplete" and .status == "True")' <<<"${dr}" >/dev/null ||
    return 1
}

annotation_set() {
  local v
  v=$(kc get node "${NODE}" -o json | jq -r --arg k "${CORDON_REASON_ANNOTATION}" '.metadata.annotations[$k] // ""')
  [[ "${v}" == "[T] [NVSentinel]"*"${XID}"* ]]
}

# worker_gone returns 0 when the worker is deleted or deleting, 1 when it is
# still there, and 2 when it cannot be read.
worker_gone() {
  local out
  out=$(kc -n "${SLURM_NAMESPACE}" get pod "${WORKER_POD}" -o json 2>&1) && {
    jq -e '.metadata.deletionTimestamp != null' <<<"${out}" >/dev/null
    return
  }
  grep -q NotFound <<<"${out}" && return 0
  return 2
}

# drainer_recorded_drain succeeds once node-drainer has read the request's
# completion, recorded the node as drained and deleted the request.
drainer_recorded_drain() {
  local logs
  logs=$(kc -n nvsentinel logs deployment/node-drainer --tail=-1) || return 1
  grep -F '"msg":"Drain CR completed"' <<<"${logs}" | grep -qF "\"node\":\"${NODE}\"" &&
    grep -F '"msg":"Evaluated action for node"' <<<"${logs}" | grep -F "\"node\":\"${NODE}\"" |
      grep -qF '"action":"MarkAlreadyDrained"' &&
    grep -F '"msg":"Deleted custom drain CR"' <<<"${logs}" | grep -qF "\"node\":\"${NODE}\""
}

# bystander_uids echoes the UIDs of every pod outside the system and slurm
# namespaces, which a pod-eviction drain would have removed.
bystander_uids() {
  kc get pods -A -o json |
    jq -r '.items[] | select(.metadata.namespace | test("^(kube-system|nvsentinel|slurm)$") | not)
      | select(.metadata.ownerReferences[0].kind != "Job") | .metadata.uid' | sort
}

# ── Tests ──

test_drain_through_slurm() {
  local before after
  create_worker
  # A job is running on the node.
  set_slurm_state SlurmNodeStateAllocated=True
  before=$(bystander_uids) || err "listing bystander pods failed"
  [[ -n "${before}" ]] || err "no bystander pods found -- the eviction check below would prove nothing"

  send_fault

  if poll node_cordoned; then
    pass "fault-quarantine cordoned the node"
  else
    fail "fault-quarantine cordoned the node" "node ${NODE} not cordoned within ${PIPELINE_WAIT_SECONDS}s"
    return
  fi
  if poll drain_request_exists; then
    pass "node-drainer created a DrainRequest"
    detail "$(drain_request | jq -c '{name: .metadata.name, spec: {nodeName: .spec.nodeName, errorCode: .spec.errorCode}}')"
  else
    fail "node-drainer created a DrainRequest" "none for ${NODE} within ${PIPELINE_WAIT_SECONDS}s"
    kc -n nvsentinel logs deployment/node-drainer --tail=50 >&2 || true
    return
  fi
  if poll annotation_set; then
    pass "slinky-drainer wrote ${CORDON_REASON_ANNOTATION}"
  else
    fail "slinky-drainer wrote ${CORDON_REASON_ANNOTATION}" \
      "annotation missing or without XID ${XID}: $(kc get node "${NODE}" -o jsonpath='{.metadata.annotations}')"
  fi

  # What Slinky does on the cordon: Slurm DRAIN, with the job still running.
  set_slurm_state SlurmNodeStateDrain=True
  sleep "${HOLD_SECONDS}"
  # A read error must not pass as "kept": both checks run, and only a clean
  # 1 from each counts.
  local gone=0 complete=0
  worker_gone || gone=$?
  drain_complete || complete=$?
  if [[ "${gone}" -eq 0 ]]; then
    fail "slurmd kept while the job runs" "${WORKER_POD} was deleted while Slurm still reported the node allocated"
  elif [[ "${complete}" -eq 0 ]]; then
    fail "slurmd kept while the job runs" "the DrainRequest completed while Slurm still reported the node allocated"
  elif [[ "${gone}" -ne 1 || "${complete}" -ne 1 ]]; then
    err "could not read the worker pod or the DrainRequest while checking the hold"
  else
    pass "slurmd kept and the request pending while the job runs"
  fi

  # The job finishes: Slurm reports the node drained and idle.
  set_slurm_state SlurmNodeStateAllocated=delete SlurmNodeStateIdle=True
  if poll worker_gone; then
    pass "slinky-drainer deleted the slurmd pod once the node was drained and idle"
  else
    fail "slinky-drainer deleted the slurmd pod" "${WORKER_POD} still present after ${PIPELINE_WAIT_SECONDS}s"
  fi
  if poll drain_complete; then
    pass "the DrainRequest reports DrainComplete=True"
  else
    fail "the DrainRequest reports DrainComplete=True" "$(drain_request | jq -c '.status')"
  fi
  if poll drainer_recorded_drain; then
    pass "node-drainer recorded the node as drained and deleted the request"
  else
    fail "node-drainer recorded the node as drained" \
      "no Drain CR completed / MarkAlreadyDrained / Deleted custom drain CR for ${NODE}"
  fi

  after=$(bystander_uids) || err "listing bystander pods failed"
  if [[ "${before}" == "${after}" ]]; then
    pass "no pod outside the slurm namespace was evicted"
  else
    fail "no pod outside the slurm namespace was evicted" \
      "gone: $(comm -23 <(echo "${before}") <(echo "${after}") | tr '\n' ' ')"
  fi
}

# ── Main ──

msg "=========================================="
msg "nvsentinel-slinky-drainer mixin e2e"
msg "=========================================="

has_tools go kind kubectl helm yq jq curl
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
compose_bundle
ensure_cluster
install_bundle
publish_proto
test_drain_through_slurm

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel-slinky-drainer e2e: all tests passed"
