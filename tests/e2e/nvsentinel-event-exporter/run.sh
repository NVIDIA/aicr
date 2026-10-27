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
# nvsentinel-event-exporter mixin e2e
# =============================================================================
#
# Installs the observe step with the event exporter mixin through the bundle's
# own install.sh on Kind, with an in-cluster mock OAuth token endpoint and
# CloudEvents sink, sends synthetic health events into platform-connectors'
# socket, and reads what reaches the sink:
#
#   - the bundle gate refuses the mixin without a sink, with the chart's
#     placeholder sink and with a certificate check turned off;
#   - the exporter takes a client-credentials token with the Secret's client
#     secret and posts CloudEvents with it as a bearer token;
#   - the mixin's filter exports an actionable fault and a recovery and drops
#     an event that recommends no action;
#   - each CloudEvent carries the cluster's metadata and the health event;
#   - the exporter's resume token is stored in the datastore, and an event
#     stored while the exporter is down is delivered once it restarts.
#
# TEST-ONLY SETTINGS, passed as --set on the bundle, as in
# tests/e2e/nvsentinel-health-events-analyzer: node-drainer.systemNamespaces,
# fault-quarantine.circuitBreaker.enabled=false and nvsentinel-mongodb at one
# member, so the observe step settles on Kind's one node, and
# event-exporter.exporter.backfill.enabled=false, so the restart test passes
# only through the resume token. The sink and token
# endpoint are plain http, which the gate allows; a real sink should be https.
#
# PREREQUISITES: go, kind, kubectl, helm, yq, jq, curl
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-nvsentinel-ee-e2e).
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

CLUSTER_NAME="${CLUSTER_NAME:-aicr-nvsentinel-ee-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NODE="${CLUSTER_NAME}-control-plane"

GRPCURL_IMAGE="docker.io/fullstorydev/grpcurl:v1.9.3@sha256:085e183ca334eb4e81ca81ee12cbb2b2737505d1d77f5e33dabc5d066593d998"
MOCK_IMAGE="docker.io/library/python:3.13-alpine@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a"
MOCK_NS="ee-mock"
MOCK_URL="http://ee-mock.${MOCK_NS}.svc.cluster.local:8080"
DRAIN_SYSTEM_NAMESPACES='^(local-path-storage|ee-mock)$'
GPU_UUID="GPU-00000000-0000-0000-0000-00000000e2e5"
MONGODB_POD="nvsentinel-mongodb-rs0-0"
EXPORTER="event-exporter"
OIDC_SECRET="event-exporter-oidc-secret"
CLIENT_ID="aicr-e2e"
CLIENT_SECRET="aicr-e2e-secret"
SCOPE="events:write"
SINK_CLUSTER="aicr-ee-e2e"
SINK_ENVIRONMENT="e2e"
SINK_REGION="kind"
DELIVERY_WAIT_SECONDS=240
# How long an event the filter drops is given to (wrongly) arrive.
FILTER_SETTLE_SECONDS=45
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

compose_recipe() {
  local data_dir="${WORK}/data"
  mkdir -p "${data_dir}/overlays"
  cat >"${data_dir}/registry.yaml" <<'REGISTRY_EOF'
kind: ComponentRegistry
apiVersion: aicr.run/v1beta1
metadata:
  name: nvsentinel-ee-e2e-registry
components: []
REGISTRY_EOF
  yq '.spec.mixins = ["nvsentinel-observe", "nvsentinel-event-exporter"]' \
    "${ROOT}/recipes/overlays/h100-kind-training.yaml" \
    >"${data_dir}/overlays/h100-kind-training.yaml"
  "${AICR_BIN}" recipe --service kind --accelerator h100 --intent training \
    --data "${data_dir}" --output "${WORK}/recipe.yaml" >/dev/null || err "recipe generation failed"
}

# bundle_to bundles the recipe into ${WORK}/bundle-$1 with the test-only
# settings and the remaining arguments, logging to ${WORK}/bundle-$1.log.
bundle_to() {
  local out="$1"
  shift
  rm -rf "${WORK}/bundle-${out}"
  "${AICR_BIN}" bundle -r "${WORK}/recipe.yaml" -o "${WORK}/bundle-${out}" \
    --set "nvsentinel:node-drainer.systemNamespaces=${DRAIN_SYSTEM_NAMESPACES}" \
    --set "nvsentinel:fault-quarantine.circuitBreaker.enabled=false" \
    --set "nvsentinel:event-exporter.exporter.backfill.enabled=false" \
    --set "nvsentinelmongodb:unsafeFlags.replsetSize=true" \
    --set "nvsentinelmongodb:replsets.rs0.size=1" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.cpu=250m" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.memory=512Mi" \
    --system-node-selector kubernetes.io/os=linux \
    "$@" >"${WORK}/bundle-${out}.log" 2>&1
}

# expect_refusal asserts that bundling with the remaining arguments fails and
# names $2.
expect_refusal() {
  local name="$1" want="$2"
  shift 2
  if bundle_to refused "$@"; then
    fail "${name}" "the bundle was accepted"
  elif grep -qF -- "${want}" "${WORK}/bundle-refused.log"; then
    pass "${name}"
  else
    fail "${name}" "refused without naming '${want}': $(grep -m1 'command failed' "${WORK}/bundle-refused.log" | cut -c1-300)"
  fi
}

test_gate_and_bundle() {
  msg "Checking the bundle gate..."
  local p="nvsentinel:event-exporter.exporter"
  local deployment=(
    --set "${p}.oidc.tokenUrl=${MOCK_URL}/token"
    --set "${p}.oidc.clientId=${CLIENT_ID}"
    --set "${p}.oidc.scope=${SCOPE}"
    --set "${p}.metadata.cluster=${SINK_CLUSTER}"
    --set "${p}.metadata.environment=${SINK_ENVIRONMENT}"
    --set "${p}.metadata.region=${SINK_REGION}"
  )
  expect_refusal "gate refuses the exporter without a sink" "sink.endpoint is not set" "${deployment[@]}"
  expect_refusal "gate refuses the chart's placeholder sink" "a placeholder host" "${deployment[@]}" \
    --set "${p}.sink.endpoint=https://events.example.com/api/v1/events"
  expect_refusal "gate refuses an unchecked sink certificate" "it must be the boolean false" "${deployment[@]}" \
    --set "${p}.sink.endpoint=${MOCK_URL}/events" --set "${p}.sink.insecureSkipVerify=true"

  bundle_to deploy "${deployment[@]}" --set "${p}.sink.endpoint=${MOCK_URL}/events" ||
    { tail -20 "${WORK}/bundle-deploy.log"; err "bundle generation failed with every per-deployment value set"; }
  pass "gate accepts the exporter with every per-deployment value"

  local bundle="${WORK}/bundle-deploy"
  BUNDLE_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-nvsentinel' -print -quit)
  CRDS_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-prometheus-operator-crds' -print -quit)
  CERT_MANAGER_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-cert-manager' -print -quit)
  PSMDB_OPERATOR_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-psmdb-operator' -print -quit)
  MONGODB_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-nvsentinel-mongodb' -print -quit)
  MONGODB_POST_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-nvsentinel-mongodb-post' -print -quit)
  [[ -n "${BUNDLE_DIR}" && -n "${CRDS_DIR}" && -n "${CERT_MANAGER_DIR}" && -n "${PSMDB_OPERATOR_DIR}" &&
    -n "${MONGODB_DIR}" && -n "${MONGODB_POST_DIR}" ]] || err "a bundled component directory is missing"
  [[ "$(yq '.global.eventExporter.enabled' "${BUNDLE_DIR}/values.yaml")" == "true" ]] ||
    err "bundle's nvsentinel values do not enable event-exporter -- composition regressed"
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

# deploy_mock runs an OAuth token endpoint and a CloudEvents sink in one pod.
# The token endpoint answers only the client ID and secret the exporter is
# given, and the sink only that token, so delivery proves the credentials.
deploy_mock() {
  msg "Deploying the mock token endpoint and sink..."
  kc create namespace "${MOCK_NS}" --dry-run=client -o yaml | kc apply -f - >/dev/null
  kc -n "${MOCK_NS}" create configmap ee-mock --dry-run=client -o yaml --from-literal=server.py="$(cat <<'PY'
import base64, json, os, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs

TOKEN = "aicr-e2e-token"
BASIC = "Basic " + base64.b64encode(f"{os.environ['CLIENT_ID']}:{os.environ['CLIENT_SECRET']}".encode()).decode()
lock = threading.Lock()
events, tokens, rejected = [], [], []

class Handler(BaseHTTPRequestHandler):
    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path == "/token":
            form = parse_qs(body.decode())
            ok = (self.headers.get("Authorization") == BASIC
                  and form.get("grant_type") == ["client_credentials"]
                  and form.get("scope") == [os.environ["SCOPE"]])
            with lock:
                (tokens if ok else rejected).append(self.path)
            if ok:
                self.reply(200, {"access_token": TOKEN, "token_type": "Bearer", "expires_in": 3600})
            else:
                self.reply(401, {"error": "invalid_client"})
        elif self.path == "/events":
            ok = (self.headers.get("Authorization") == "Bearer " + TOKEN
                  and self.headers.get("Content-Type") == "application/cloudevents+json")
            with lock:
                if ok:
                    events.append(json.loads(body))
                else:
                    rejected.append(self.path)
            self.reply(202 if ok else 401, {})
        else:
            self.reply(404, {})

    def do_GET(self):
        with lock:
            self.reply(200, {"events": events, "tokens": len(tokens), "rejected": rejected})

    def log_message(self, *args):
        pass

ThreadingHTTPServer(("", 8080), Handler).serve_forever()
PY
)" | kc apply -f - >/dev/null
  jq -n --arg image "${MOCK_IMAGE}" --arg id "${CLIENT_ID}" --arg secret "${CLIENT_SECRET}" --arg scope "${SCOPE}" '{
    apiVersion: "v1", kind: "List", items: [
      {apiVersion: "apps/v1", kind: "Deployment", metadata: {name: "ee-mock"},
       spec: {replicas: 1, selector: {matchLabels: {app: "ee-mock"}},
         template: {metadata: {labels: {app: "ee-mock"}}, spec: {
           containers: [{name: "mock", image: $image, command: ["python3", "-u", "/app/server.py"],
             env: [{name: "CLIENT_ID", value: $id}, {name: "CLIENT_SECRET", value: $secret}, {name: "SCOPE", value: $scope}],
             ports: [{containerPort: 8080}],
             readinessProbe: {tcpSocket: {port: 8080}},
             volumeMounts: [{name: "app", mountPath: "/app"}]}],
           volumes: [{name: "app", configMap: {name: "ee-mock"}}]}}}},
      {apiVersion: "v1", kind: "Service", metadata: {name: "ee-mock"},
       spec: {selector: {app: "ee-mock"}, ports: [{port: 8080, targetPort: 8080}]}}
    ]}' | kc -n "${MOCK_NS}" apply -f - >/dev/null
  kc -n "${MOCK_NS}" rollout status deployment/ee-mock --timeout=300s || err "the mock token endpoint and sink never became ready"
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
  msg "Installing prometheus-operator-crds, cert-manager, the Percona datastore and nvsentinel from the bundle..."
  local dir d deadline remaining
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

  # The chart mounts the client secret and does not create it.
  kc -n nvsentinel create secret generic "${OIDC_SECRET}" --from-literal=oidc-client-secret="${CLIENT_SECRET}" \
    --dry-run=client -o yaml | kc apply -f - >/dev/null
  install_component "${BUNDLE_DIR}"

  deadline=$((SECONDS + NVSENTINEL_WAIT_SECONDS))
  for d in fault-quarantine node-drainer fault-remediation "${EXPORTER}"; do
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
  # Download before trimming: awk exits at the marker, and a curl still writing
  # into the closed pipe fails with "(23) Failed writing body".
  curl -fsSL --connect-timeout 10 --max-time 60 -o "${WORK}/health_event.proto.raw" \
    "https://raw.githubusercontent.com/NVIDIA/NVSentinel/${version}/data-models/protobufs/health_event.proto" ||
    err "failed to fetch health_event.proto at ${version}"
  awk '/protoc_gen_crd\/proto\/crd.proto/ {next} /^\/\/ HealthEventResource is the root type/ {exit} {print}' \
    "${WORK}/health_event.proto.raw" >"${WORK}/health_event.proto"
  grep -q "rpc HealthEventOccurredV1" "${WORK}/health_event.proto" ||
    err "health_event.proto at ${version} has no HealthEventOccurredV1 -- the platform-connector API moved"
  kc -n nvsentinel create configmap e2e-health-event-proto \
    --from-file=health_event.proto="${WORK}/health_event.proto" --dry-run=client -o yaml |
    kc apply -f - >/dev/null
}

# health_event prints one HealthEvents request: check $1, healthy $2, fatal $3,
# recommended action $4. The check name is the event's identity at the sink.
health_event() {
  local check="$1" healthy="$2" fatal="$3" action="$4"
  jq -cn --arg node "${NODE}" --arg check "${check}" --argjson healthy "${healthy}" --argjson fatal "${fatal}" \
    --arg action "${action}" --arg uuid "${GPU_UUID}" --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{
      version: 1,
      events: [{
        version: 1,
        agent: "gpu-health-monitor",
        componentClass: "GPU",
        checkName: $check,
        isFatal: $fatal,
        isHealthy: $healthy,
        message: ("aicr event exporter e2e " + $check),
        recommendedAction: $action,
        errorCode: (if $healthy then [] else ["79"] end),
        entitiesImpacted: [{entityType: "GPU", entityValue: "0"}, {entityType: "GPU_UUID", entityValue: $uuid}],
        generatedTimestamp: $ts,
        nodeName: $node,
        processingStrategy: "STORE_ONLY"
      }]
    }'
}

# send_event delivers $2 through platform-connectors' socket from a pod on the
# node, which is what a node-local monitor does.
send_event() {
  local name="$1" payload="$2"
  kc -n nvsentinel delete pod "${name}" --ignore-not-found >/dev/null
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

# sink_state prints the mock's JSON state: received events, token grants and
# rejected requests.
sink_state() {
  kc -n "${MOCK_NS}" exec deploy/ee-mock -- python3 -c \
    'import urllib.request; print(urllib.request.urlopen("http://localhost:8080/").read().decode())'
}

# delivered prints, one per line, the checkName of every event the sink has,
# and fails when the sink cannot be read, so an unreadable sink never reads as
# an absent event.
delivered() {
  local state
  state=$(sink_state) || return 1
  jq -er '.events | map(.data.healthEvent.checkName) | .[]?' <<<"${state}" 2>/dev/null ||
    jq -e '.events | length == 0' <<<"${state}" >/dev/null 2>&1
}

# wait_for_delivery waits until checkName $1 reaches the sink.
wait_for_delivery() {
  local deadline=$((SECONDS + DELIVERY_WAIT_SECONDS))
  while ((SECONDS < deadline)); do
    delivered 2>/dev/null | grep -qx "$1" && return 0
    sleep 5
  done
  return 1
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

# ── Tests ──

test_delivery_and_filter() {
  send_event ee-actionable "$(health_event EeActionable false true COMPONENT_RESET)"
  send_event ee-informational "$(health_event EeInformational false false NONE)"
  send_event ee-recovery "$(health_event EeRecovery true false NONE)"

  if wait_for_delivery EeActionable; then
    pass "an actionable fault reaches the sink"
  else
    fail "actionable fault delivered" "not at the sink after ${DELIVERY_WAIT_SECONDS}s: $(sink_state | jq -c '{tokens, rejected, n: (.events | length)}')"
  fi
  if wait_for_delivery EeRecovery; then
    pass "a recovery reaches the sink"
  else
    fail "recovery delivered" "not at the sink after ${DELIVERY_WAIT_SECONDS}s"
  fi
  sleep "${FILTER_SETTLE_SECONDS}"
  local names
  if ! names=$(delivered); then
    fail "filter drops an event recommending no action" "the sink could not be read"
  elif grep -qx EeInformational <<<"${names}"; then
    fail "filter drops an event recommending no action" "EeInformational reached the sink"
  else
    pass "filter drops an event recommending no action"
  fi

  local state
  if ! state=$(sink_state); then
    fail "sink readable" "the mock token endpoint and sink could not be read"
    return
  fi
  if jq -e '.tokens >= 1 and (.rejected | length) == 0' <<<"${state}" >/dev/null; then
    pass "token taken with the Secret's client secret; every post carried that bearer token"
  else
    fail "OIDC and bearer token" "$(jq -c '{tokens, rejected}' <<<"${state}")"
  fi
  if jq -e --arg c "${SINK_CLUSTER}" --arg e "${SINK_ENVIRONMENT}" --arg r "${SINK_REGION}" --arg n "${NODE}" '
      [.events[] | select(.data.healthEvent.checkName == "EeActionable")][0] as $ev |
      $ev.specversion == "1.0" and $ev.type == "com.nvidia.nvsentinel.health.v1" and
      $ev.source == ("nvsentinel://" + $c + "/healthevents") and
      $ev.data.metadata.cluster == $c and $ev.data.metadata.environment == $e and $ev.data.metadata.region == $r and
      $ev.data.healthEvent.nodeName == $n and $ev.data.healthEvent.recommendedAction == "COMPONENT_RESET" and
      ($ev.data.healthEvent.errorCode | index("79")) != null' <<<"${state}" >/dev/null; then
    pass "CloudEvent carries the type, source, cluster metadata and health event"
  else
    fail "CloudEvent shape" "$(jq -c '[.events[] | select(.data.healthEvent.checkName == "EeActionable")][0]' <<<"${state}" | cut -c1-600)"
  fi
}

test_resume_after_restart() {
  local tokens
  tokens=$(mongo_eval "print(hdb.ResumeTokens.countDocuments({clientName: 'event-exporter'}))") || tokens=""
  if [[ "${tokens}" =~ ^[0-9]+$ ]] && ((tokens >= 1)); then
    pass "resume token stored in HealthEventsDatabase.ResumeTokens for event-exporter"
  else
    fail "resume token stored" "HealthEventsDatabase.ResumeTokens holds '${tokens}' for event-exporter"
  fi

  # Sent while the exporter is down, with backfill off for this run: only its
  # resume token, not a fresh change stream or a backfill, carries the event
  # across the restart.
  kc -n nvsentinel scale "deployment/${EXPORTER}" --replicas=0 >/dev/null
  kc -n nvsentinel wait --for=delete pod -l "app.kubernetes.io/name=${EXPORTER}" --timeout=300s >/dev/null ||
    { fail "export resumes after a restart" "the exporter did not stop"; return; }
  send_event ee-while-down "$(health_event EeWhileDown false true RESTART_VM)"
  local stored
  stored=$(mongo_eval "print(hdb.HealthEvents.countDocuments({'healthevent.checkname': 'EeWhileDown'}))") || stored=""
  [[ "${stored}" =~ ^[1-9][0-9]*$ ]] ||
    { fail "export resumes after a restart" "EeWhileDown was not stored while the exporter was down ('${stored}')"; return; }
  kc -n nvsentinel scale "deployment/${EXPORTER}" --replicas=1 >/dev/null
  kc -n nvsentinel rollout status "deployment/${EXPORTER}" --timeout=300s >/dev/null ||
    { fail "export resumes after a restart" "the exporter did not roll out again"; return; }
  if wait_for_delivery EeWhileDown; then
    pass "an event stored while the exporter was down is delivered after it restarts"
  else
    fail "export resumes after a restart" "EeWhileDown not at the sink after ${DELIVERY_WAIT_SECONDS}s"
  fi
}

# ── Main ──

msg "=========================================="
msg "nvsentinel-event-exporter mixin e2e"
msg "=========================================="

has_tools go kind kubectl helm yq jq curl
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
compose_recipe
test_gate_and_bundle
ensure_cluster
deploy_mock
install_bundle
publish_proto
test_delivery_and_filter
test_resume_after_restart

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel-event-exporter e2e: all tests passed"
