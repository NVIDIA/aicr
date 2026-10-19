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
# nvsentinel-health-events-analyzer mixin e2e
# =============================================================================
#
# Installs the observe step with the analyzer mixin through the bundle's own
# install.sh on Kind, sends synthetic GPU fault events into platform-connectors'
# socket, and reads NVSentinel's datastore for the events the analyzer derives:
#
#   - one XID 13 event matches XIDErrorSoloNoBurst;
#   - repeated faults, each carried through the observe step's dry-run
#     remediation, match MultipleRemediations;
#   - each derived event is stored with the rule name as its checkName, the
#     rule's action and fatality, and STORE_ONLY, and the fatal one sets no node
#     condition although the source events visibly do.
#
# WHY THE OBSERVE STEP. fault-remediation's dry run creates no repair, but it
# still marks each event remediated in the datastore, and that mark is what
# MultipleRemediations counts. So the rule can be driven on Kind without
# resetting a GPU.
#
# TEST-ONLY SETTINGS, passed as --set on the bundle, as in
# tests/e2e/nvsentinel-remediation:
#   node-drainer.systemNamespaces  includes cert-manager and local-path-storage,
#       so the AllowCompletion drain on Kind's one node finishes.
#   fault-quarantine.circuitBreaker.enabled=false  on one node the first
#       quarantine would trip it.
#   nvsentinel-mongodb at one member with smaller requests, and the analyzer
#       at a smaller CPU request, so everything fits one runner node.
#
# PREREQUISITES: go, kind, kubectl, helm, yq, jq, curl
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-nvsentinel-hea-e2e).
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

CLUSTER_NAME="${CLUSTER_NAME:-aicr-nvsentinel-hea-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NODE="${CLUSTER_NAME}-control-plane"

GRPCURL_IMAGE="docker.io/fullstorydev/grpcurl:v1.9.3@sha256:085e183ca334eb4e81ca81ee12cbb2b2737505d1d77f5e33dabc5d066593d998"
DRAIN_SYSTEM_NAMESPACES='^(nvsentinel|kube-system|cert-manager|local-path-storage)$'
GPU_UUID="GPU-00000000-0000-0000-0000-00000000e2e1"
MONGODB_POD="nvsentinel-mongodb-rs0-0"
# MultipleRemediations fires at five remediated events on a node. A few more
# are allowed, since an event can reach fault-remediation after the analyzer
# has already evaluated the next one.
MAX_REMEDIATED_EVENTS=8
PIPELINE_WAIT_SECONDS=240
# How long a STORE_ONLY derived event is given to (wrongly) set a condition.
CONDITION_SETTLE_SECONDS=30
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
MONGO_USER=""
MONGO_PASSWORD=""
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
  msg "Composing a bundle with the nvsentinel-observe and nvsentinel-health-events-analyzer mixins..."
  local data_dir="${WORK}/data"
  mkdir -p "${data_dir}/overlays"
  cat >"${data_dir}/registry.yaml" <<'REGISTRY_EOF'
kind: ComponentRegistry
apiVersion: aicr.run/v1beta1
metadata:
  name: nvsentinel-hea-e2e-registry
components: []
REGISTRY_EOF
  yq '.spec.mixins = ["nvsentinel-observe", "nvsentinel-health-events-analyzer"]' \
    "${ROOT}/recipes/overlays/h100-kind-training.yaml" \
    >"${data_dir}/overlays/h100-kind-training.yaml"

  "${AICR_BIN}" recipe --service kind --accelerator h100 --intent training \
    --data "${data_dir}" --output "${WORK}/recipe.yaml" ||
    err "recipe generation failed"
  "${AICR_BIN}" bundle -r "${WORK}/recipe.yaml" -o "${WORK}/bundle" \
    --set "nvsentinel:node-drainer.systemNamespaces=${DRAIN_SYSTEM_NAMESPACES}" \
    --set "nvsentinel:fault-quarantine.circuitBreaker.enabled=false" \
    --set "nvsentinel:health-events-analyzer.resources.requests.cpu=250m" \
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

  # Fail here, not later as "nothing matched": a mixin that stopped composing
  # would otherwise read as rules that never fired.
  local values="${BUNDLE_DIR}/values.yaml"
  [[ "$(yq '.global.healthEventsAnalyzer.enabled' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not enable health-events-analyzer -- composition regressed"
  [[ "$(yq '.["health-events-analyzer"].processingStrategy' "${values}")" == "STORE_ONLY" ]] ||
    err "bundle's health-events-analyzer is not STORE_ONLY -- composition regressed"
  [[ "$(yq '.global.dryRun' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not set global.dryRun: true -- composition regressed"
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
  local dir d deadline remaining
  for dir in "${CERT_MANAGER_DIR}" "${CRDS_DIR}"; do
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

  install_component "${BUNDLE_DIR}"

  deadline=$((SECONDS + NVSENTINEL_WAIT_SECONDS))
  for d in fault-quarantine node-drainer fault-remediation health-events-analyzer; do
    wait_rollout deployment "${d}" nvsentinel "${deadline}"
  done
  wait_rollout daemonset platform-connectors nvsentinel "${deadline}"

  MONGO_USER=$(kc -n nvsentinel get secret internal-nvsentinel-mongodb-users \
    -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_USER}' | base64 -d) ||
    err "could not read the datastore's admin user"
  MONGO_PASSWORD=$(kc -n nvsentinel get secret internal-nvsentinel-mongodb-users \
    -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d) ||
    err "could not read the datastore's admin password"
  [[ -n "${MONGO_USER}" && -n "${MONGO_PASSWORD}" ]] || err "the datastore's admin credentials are empty"
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

# health_event prints one fatal GPU XID HealthEvents request for XID $1.
health_event() {
  local code="$1"
  jq -cn --arg node "${NODE}" --arg code "${code}" --arg uuid "${GPU_UUID}" \
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
        entitiesImpacted: [{entityType: "GPU", entityValue: "0"}, {entityType: "GPU_UUID", entityValue: $uuid}],
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

# mongo_eval runs JavaScript $1 against NVSentinel's database and prints its
# last output line. The datastore requires a client certificate even with
# password auth, so it presents the member's own (/tmp/tls.pem, which the
# Percona entrypoint assembles); localhost is not in that certificate's names.
mongo_eval() {
  kc -n nvsentinel exec "${MONGODB_POD}" -c mongod -- mongosh --quiet \
    --tls --tlsCAFile /etc/mongodb-ssl/ca.crt --tlsCertificateKeyFile /tmp/tls.pem --tlsAllowInvalidHostnames \
    "mongodb://localhost:27017/admin?directConnection=true" \
    -u "${MONGO_USER}" -p "${MONGO_PASSWORD}" \
    --eval "const hdb = db.getSiblingDB('HealthEventsDatabase'); $1" 2>/dev/null | tail -1
}

# derived_events prints, as a JSON array, the events the analyzer derived for
# rule $1 on the node.
derived_events() {
  mongo_eval "print(EJSON.stringify(hdb.HealthEvents.find(
    {'healthevent.agent': 'health-events-analyzer', 'healthevent.nodename': '${NODE}', 'healthevent.checkname': '$1'},
    {_id: 0, 'healthevent.agent': 1, 'healthevent.checkname': 1, 'healthevent.recommendedaction': 1,
     'healthevent.isfatal': 1, 'healthevent.processingstrategy': 1}).toArray()))"
}

# remediated_count prints how many of the node's events fault-remediation has
# marked remediated.
remediated_count() {
  mongo_eval "print(hdb.HealthEvents.countDocuments(
    {'healthevent.nodename': '${NODE}', 'healtheventstatus.faultremediated.value': true}))"
}

# wait_for_derived polls for at least one derived event for rule $1 and prints
# them.
wait_for_derived() {
  local rule="$1" elapsed=0 out
  while [[ "${elapsed}" -lt "${PIPELINE_WAIT_SECONDS}" ]]; do
    out=$(derived_events "${rule}" || true)
    if jq -e 'type == "array" and length > 0' <<<"${out}" >/dev/null 2>&1; then
      echo "${out}"
      return 0
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

# wait_for_remediated polls until the node has at least $1 remediated events.
wait_for_remediated() {
  local want="$1" elapsed=0 got
  while [[ "${elapsed}" -lt "${PIPELINE_WAIT_SECONDS}" ]]; do
    got=$(remediated_count || true)
    if [[ "${got}" =~ ^[0-9]+$ ]] && ((got >= want)); then
      return 0
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

# node_conditions prints the node's condition types, one per line, and fails
# when the node cannot be read, so a kubectl error never reads as "absent".
node_conditions() {
  local json
  json=$(kc get node "${NODE}" -o json) || return 1
  jq -r '.status.conditions[].type' <<<"${json}"
}

wait_for_condition() {
  local type="$1" elapsed=0 types
  while [[ "${elapsed}" -lt 60 ]]; do
    if types=$(node_conditions) && grep -qx "${type}" <<<"${types}"; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  return 1
}

# assert_derived checks the derived events $2 for rule $1. The query already
# selects on agent and checkName, so a non-empty result is what shows the rule
# name is the checkName; this checks the fields it does not select on: the
# rule's action ($3, enum number and name), its fatality ($4) and STORE_ONLY.
assert_derived() {
  local rule="$1" events="$2" action_num="$3" action_name="$4" fatal="$5"
  msg "Derived ${rule} events: ${events}"
  pass "${rule}: a derived event is stored with checkName ${rule}"
  if jq -e --argjson n "${action_num}" --arg a "${action_name}" --argjson f "${fatal}" 'all(.[];
      (.healthevent.recommendedaction == $n or .healthevent.recommendedaction == $a)
      and ((.healthevent.isfatal // false) == $f)
      and (.healthevent.processingstrategy == 2 or .healthevent.processingstrategy == "STORE_ONLY"))' \
    <<<"${events}" >/dev/null; then
    pass "${rule}: ${action_name}, isFatal ${fatal}, STORE_ONLY"
  else
    fail "${rule}: action, fatality and strategy" "want ${action_name}, isFatal ${fatal}, STORE_ONLY; got ${events}"
  fi
}

# ── Tests ──

test_xid13_matches_solo_rule() {
  send_event e2e-hea-xid13 "$(health_event 13)"
  # Positive control for the condition check below: the EXECUTE_REMEDIATION
  # source event does set its condition.
  if wait_for_condition GpuXidError; then
    pass "source event delivered: node condition GpuXidError is set"
  else
    fail "source event delivered" "platform-connectors accepted the event but set no GpuXidError condition"
  fi
  local events
  if events=$(wait_for_derived XIDErrorSoloNoBurst); then
    assert_derived XIDErrorSoloNoBurst "${events}" 0 NONE false
  else
    fail "XIDErrorSoloNoBurst matched" "no derived event within ${PIPELINE_WAIT_SECONDS}s"
  fi
}

test_repeated_remediation_matches_multiple_remediations() {
  local i events="" baseline
  baseline=$(remediated_count) || err "could not read the node's remediated events"
  [[ "${baseline}" =~ ^[0-9]+$ ]] || err "unexpected remediated count: ${baseline}"
  for ((i = 1; i <= MAX_REMEDIATED_EVENTS; i++)); do
    if events=$(derived_events MultipleRemediations) &&
      jq -e 'type == "array" and length > 0' <<<"${events}" >/dev/null 2>&1; then
      break
    fi
    send_event "e2e-hea-repeat-${i}" "$(health_event 79)"
    if ! wait_for_remediated "$((baseline + i))"; then
      fail "dry-run remediation marks each repeat remediated" \
        "the node has $(remediated_count || echo '?') remediated events, want $((baseline + i)), within ${PIPELINE_WAIT_SECONDS}s"
      return
    fi
  done
  msg "Remediated events on ${NODE}: $(remediated_count || echo '?')"

  if ! events=$(wait_for_derived MultipleRemediations); then
    fail "MultipleRemediations matched" "no derived event after $(remediated_count || echo '?') remediated events"
    return
  fi
  assert_derived MultipleRemediations "${events}" 5 CONTACT_SUPPORT true

  # Fatal, so EXECUTE_REMEDIATION would set a MultipleRemediations condition.
  sleep "${CONDITION_SETTLE_SECONDS}"
  local types
  if ! types=$(node_conditions); then
    fail "MultipleRemediations: no node condition" "could not read node ${NODE}"
  elif grep -qx MultipleRemediations <<<"${types}"; then
    fail "MultipleRemediations: no node condition" "the node has one, which a STORE_ONLY event must not set"
  else
    pass "MultipleRemediations: STORE_ONLY left no node condition"
  fi
}

# ── Main ──

msg "=========================================="
msg "nvsentinel-health-events-analyzer mixin e2e"
msg "=========================================="

has_tools go kind kubectl helm yq jq curl
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
compose_bundle
ensure_cluster
install_bundle
publish_proto
test_xid13_matches_solo_rule
test_repeated_remediation_matches_multiple_remediations

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel-health-events-analyzer e2e: all tests passed"
