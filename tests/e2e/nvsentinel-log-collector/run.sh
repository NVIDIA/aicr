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
# nvsentinel-log-collector mixin e2e
# =============================================================================
#
# Installs the remediation step with the log collector mixin, and the health
# events analyzer that live remediation requires, through the bundle's own
# install.sh on Kind, sends one synthetic GPU fault into
# platform-connectors' socket, and checks that diagnostics are captured before
# the repair:
#
#   - the file server's volume binds through the storage class --storage-class
#     names, which reaches it through nvsentinel's storageClassPaths;
#   - fault-remediation runs the collector Job on the faulty node, and the Job
#     completes;
#   - the bug report it uploads is listed by the file server at
#     /upload/<node>/<timestamp>/, fetched the way the docs say to, through a
#     port-forward;
#   - fault-remediation creates the GPU reset only after the Job finished.
#
# TEST-ONLY SETTINGS, passed as --set on the bundle, as in
# tests/e2e/nvsentinel-remediation:
#   fault-remediation.logCollector.env.MOCK_MODE=true  a Kind node has no GPU
#       driver, so the collector uploads its synthetic bug report instead.
#       CheckNVSentinelLogCollectorCoherent accepts mock mode in place of a
#       reachable driver.
#   janitor-provider.csp.provider=kind  NVSentinel's simulated provider, so
#       nothing can reboot the runner if an action escalates to RESTART_VM.
#   node-drainer.systemNamespaces  includes cert-manager and local-path-storage,
#       so the AllowCompletion drain on Kind's one node finishes.
#   fault-quarantine.circuitBreaker.enabled=false  on one node the first
#       quarantine would trip it.
#   nvsentinel-mongodb at one member with smaller requests, so everything fits
#       one runner node.
#
# The GPU reset itself cannot run on Kind (no nvidia RuntimeClass); the test
# reads only that it is requested after collection.
#
# PREREQUISITES: go, kind, kubectl, helm, yq, jq, curl
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-nvsentinel-lc-e2e).
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

CLUSTER_NAME="${CLUSTER_NAME:-aicr-nvsentinel-lc-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NODE="${CLUSTER_NAME}-control-plane"

GRPCURL_IMAGE="docker.io/fullstorydev/grpcurl:v1.9.3@sha256:085e183ca334eb4e81ca81ee12cbb2b2737505d1d77f5e33dabc5d066593d998"
DRAIN_SYSTEM_NAMESPACES='^(nvsentinel|kube-system|cert-manager|local-path-storage)$'
GPU_UUID="GPU-00000000-0000-0000-0000-00000000e2e9"
# A non-default copy of Kind's local-path class: a claim on it proves
# --storage-class reached it, since the default-class admission plugin would
# otherwise fill in "standard".
STORAGE_CLASS="aicr-lc-e2e"
FILE_SERVER="nvsentinel-incluster-file-server"
GPU_RESETS="gpuresets.janitor.dgxc.nvidia.com"
CERT_MANAGER_WAIT_SECONDS=600
MONGODB_WAIT_SECONDS=900
NVSENTINEL_WAIT_SECONDS=900
# Event delivery through fault-quarantine and the drain to fault-remediation.
PIPELINE_WAIT_SECONDS=300
# The mixin's collector timeout, which bounds how long remediation waits.
COLLECTOR_WAIT_SECONDS=600

WORK=""
CREATED_CLUSTER=false
BUNDLE_DIR=""
CRDS_DIR=""
CERT_MANAGER_DIR=""
PSMDB_OPERATOR_DIR=""
MONGODB_DIR=""
MONGODB_POST_DIR=""
PORT_FORWARD_PID=""
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

cleanup() {
  local rc=$?
  [[ -n "${PORT_FORWARD_PID}" ]] && kill "${PORT_FORWARD_PID}" 2>/dev/null || true
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
  msg "Composing a bundle with the nvsentinel-remediation, nvsentinel-health-events-analyzer and nvsentinel-log-collector mixins..."
  local data_dir="${WORK}/data"
  mkdir -p "${data_dir}/overlays"
  cat >"${data_dir}/registry.yaml" <<'REGISTRY_EOF'
kind: ComponentRegistry
apiVersion: aicr.run/v1beta1
metadata:
  name: nvsentinel-lc-e2e-registry
components: []
REGISTRY_EOF
  yq '.spec.mixins = ["nvsentinel-remediation", "nvsentinel-health-events-analyzer", "nvsentinel-log-collector"]' \
    "${ROOT}/recipes/overlays/h100-kind-training.yaml" \
    >"${data_dir}/overlays/h100-kind-training.yaml"

  "${AICR_BIN}" recipe --service kind --accelerator h100 --intent training \
    --data "${data_dir}" --output "${WORK}/recipe.yaml" ||
    err "recipe generation failed"
  "${AICR_BIN}" bundle -r "${WORK}/recipe.yaml" -o "${WORK}/bundle" \
    --storage-class "${STORAGE_CLASS}" \
    --set "nvsentinel:fault-remediation.logCollector.env.MOCK_MODE=true" \
    --set "nvsentinel:janitor-provider.csp.provider=kind" \
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

  # Fail here, not later as "nothing uploaded": a mixin that stopped composing
  # would otherwise read as a collector that never ran.
  local values="${BUNDLE_DIR}/values.yaml"
  [[ "$(yq '.global.inclusterFileServer.enabled' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not enable the file server -- composition regressed"
  [[ "$(yq '.["fault-remediation"].logCollector.enabled' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not enable the log collector -- composition regressed"
  [[ "$(yq '.global.dryRun' "${values}")" == "false" ]] ||
    err "bundle's nvsentinel values are not live (global.dryRun != false), which skips the collector"
  [[ "$(yq '.["incluster-file-server"].persistence.storageClassName' "${values}")" == "${STORAGE_CLASS}" ]] ||
    err "--storage-class did not reach the file server's claim -- storageClassPaths regressed"
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
  kc get storageclass standard -o json |
    jq --arg name "${STORAGE_CLASS}" '.metadata = {name: $name}' |
    kc apply -f - >/dev/null || err "could not create StorageClass ${STORAGE_CLASS}"
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

# render_nvsentinel_bundle renders the bundle's nvsentinel chart with the
# values its install.sh passes. It runs in a subshell, so upstream.env's
# variables do not leak.
render_nvsentinel_bundle() (
  cd "${BUNDLE_DIR}" || exit 1
  # shellcheck source=/dev/null
  . ./upstream.env
  # shellcheck disable=SC2153 # CHART, REPO and VERSION come from upstream.env.
  helm template nvsentinel "${CHART}" ${REPO:+--repo "${REPO}"} --version "${VERSION}" -n nvsentinel \
    -f values.yaml -f cluster-values.yaml
)

# preload_file_server_images pulls the file server's images on the host, with
# retries, and imports them into the node: anonymous registry pulls from inside
# Kind on shared CI runners are throttled into ImagePullBackOff. The images come
# from the bundle's own render, so they track the chart pin. Not kind load:
# with Docker's containerd image store it exports an index whose other
# platforms have no layers, which its --all-platforms import rejects. docker
# save does not record a digest-pinned image's name, so each image is saved
# under a temporary local tag, which classic (manifest.json) and OCI
# (index.json) archives both carry and the import keeps, then named as the
# kubelet requests it and checked through CRI.
preload_file_server_images() {
  local images image attempt arch staged index=0 repo tag digest
  arch=$(docker version -f '{{.Server.Arch}}') || err "could not read the Docker host architecture"
  images=$(render_nvsentinel_bundle |
    yq -N "select(.kind == \"Deployment\" and .metadata.name == \"${FILE_SERVER}\") |
      .spec.template.spec | (.initContainers // []) + .containers | .[].image" | sort -u) ||
    err "could not render the file server's images from the bundle"
  [[ -n "${images}" ]] || err "the bundle renders no ${FILE_SERVER} Deployment"
  for image in ${images}; do
    for attempt in 1 2 3; do
      docker pull -q --platform "linux/${arch}" "${image}" >/dev/null && break
      ((attempt < 3)) || err "could not pull ${image}"
      sleep $((attempt * 10))
    done
    index=$((index + 1))
    # Unique per run, so concurrent local runs cannot remove each other's tag.
    staged="localhost/aicr-e2e-preload:${CLUSTER_NAME}-$$-${index}"
    docker tag "${image}" "${staged}" || err "could not tag ${image} for import"
    docker save "${staged}" |
      docker exec -i "${NODE}" ctr -n k8s.io images import --platform "linux/${arch}" \
        --snapshotter=overlayfs - >/dev/null || err "could not import ${image} into ${NODE}"
    docker rmi "${staged}" >/dev/null 2>&1 || true
    digest=""
    [[ "${image}" == *@* ]] && digest="${image#*@}"
    repo=$(normalize_repo "${image%@*}")
    tag="${repo##*/}"
    if [[ "${tag}" == *:* ]]; then
      tag="${tag##*:}"
      repo="${repo%:*}"
    else
      tag=latest
    fi
    docker exec "${NODE}" ctr -n k8s.io images tag --force "${staged}" "${repo}:${tag}" >/dev/null ||
      err "could not name ${image} in ${NODE}"
    if [[ -n "${digest}" ]]; then
      docker exec "${NODE}" ctr -n k8s.io images tag --force "${staged}" "${repo}@${digest}" >/dev/null ||
        err "could not name ${image} in ${NODE}"
    fi
    docker exec "${NODE}" ctr -n k8s.io images rm "${staged}" >/dev/null 2>&1 || true
    docker exec "${NODE}" crictl inspecti "${image}" >/dev/null 2>&1 ||
      err "${image} was imported into ${NODE} but the kubelet cannot see it under that name"
  done
  msg "Preloaded the file server's images: $(tr '\n' ' ' <<<"${images}")"
}

# normalize_repo prefixes a Docker Hub reference with docker.io/ (and
# library/ for an official image), as the kubelet resolves it.
normalize_repo() {
  local ref="$1" first="${1%%/*}"
  if [[ "${ref}" != */* ]]; then
    ref="docker.io/library/${ref}"
  elif [[ "${first}" != *.* && "${first}" != *:* && "${first}" != localhost ]]; then
    ref="docker.io/${ref}"
  fi
  echo "${ref}"
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

  preload_file_server_images
  install_component "${BUNDLE_DIR}"

  deadline=$((SECONDS + NVSENTINEL_WAIT_SECONDS))
  for d in fault-quarantine node-drainer fault-remediation janitor janitor-provider "${FILE_SERVER}"; do
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

# fault_event prints one fatal GPU XID HealthEvents request whose action is a
# GPU reset, the action the collector runs before.
fault_event() {
  jq -cn --arg node "${NODE}" --arg uuid "${GPU_UUID}" --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{
    version: 1,
    events: [{
      version: 1,
      agent: "gpu-health-monitor",
      componentClass: "GPU",
      checkName: "GpuXidError",
      isFatal: true,
      isHealthy: false,
      message: "aicr e2e synthetic XID 79",
      recommendedAction: "COMPONENT_RESET",
      errorCode: ["79"],
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

# collector_jobs prints the collector Jobs as a JSON list. A kubectl error
# fails rather than reading as "none".
collector_jobs() {
  local json
  json=$(kc -n nvsentinel get jobs -l app=log-collector -o json) || return 1
  jq -c '[.items[] | {name: .metadata.name, node: .spec.template.spec.nodeName,
    succeeded: (.status.succeeded // 0), failed: (.status.failed // 0), completion: .status.completionTime,
    complete: any(.status.conditions[]?; .type == "Complete" and .status == "True"),
    done: any(.status.conditions[]?; (.type == "Complete" or .type == "Failed") and .status == "True")}]' <<<"${json}"
}

# wait_for_job_done polls until a collector Job is Complete or Failed, then
# prints that Job. It waits on the Job's condition rather than its pod counts:
# a failed first pod is retried (backoffLimit defaults to 6), and
# completionTime is set only once the Job is Complete.
wait_for_job_done() {
  local elapsed=0 jobs
  while [[ "${elapsed}" -lt "$((PIPELINE_WAIT_SECONDS + COLLECTOR_WAIT_SECONDS))" ]]; do
    if jobs=$(collector_jobs) && jq -e 'any(.[]; .done)' <<<"${jobs}" >/dev/null; then
      jq -c 'first(.[] | select(.done))' <<<"${jobs}"
      return 0
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done
  return 1
}

# wait_for_reset polls for the GPU reset fault-remediation requests and prints
# its creation time.
wait_for_reset() {
  local elapsed=0 created
  while [[ "${elapsed}" -lt "${PIPELINE_WAIT_SECONDS}" ]]; do
    created=$(kc get "${GPU_RESETS}" -A -o json 2>/dev/null |
      jq -r '[.items[].metadata.creationTimestamp] | sort | first // empty')
    if [[ -n "${created}" ]]; then
      echo "${created}"
      return 0
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done
  return 1
}

# start_file_server_forward opens the port-forward the component catalog
# documents for retrieving reports. Call it in the parent shell: started inside
# $(...), PORT_FORWARD_PID would never reach cleanup.
start_file_server_forward() {
  [[ -n "${PORT_FORWARD_PID}" ]] && return 0
  kc -n nvsentinel port-forward "svc/${FILE_SERVER}" 18080:80 >"${WORK}/port-forward.log" 2>&1 &
  PORT_FORWARD_PID=$!
  local i
  for ((i = 0; i < 30; i++)); do
    curl -fsS --max-time 5 "http://127.0.0.1:18080/healthz" >/dev/null 2>&1 && return 0
    sleep 1
  done
}

# file_server_get fetches path $1 through the forward start_file_server_forward
# opened.
file_server_get() {
  curl -fsS --max-time 30 "http://127.0.0.1:18080$1"
}

# ── Tests ──

test_file_server_volume_bound() {
  local claim
  if ! claim=$(kc -n nvsentinel get pvc "${FILE_SERVER}-data" -o json); then
    fail "file server volume" "could not read PVC ${FILE_SERVER}-data"
    return
  fi
  if jq -e --arg sc "${STORAGE_CLASS}" '.spec.storageClassName == $sc and .status.phase == "Bound"' <<<"${claim}" >/dev/null; then
    pass "file server volume is Bound on storage class ${STORAGE_CLASS} from --storage-class"
  else
    fail "file server volume" "want Bound on ${STORAGE_CLASS}; got $(jq -c '{sc: .spec.storageClassName, phase: .status.phase}' <<<"${claim}")"
  fi
}

test_fault_is_collected_before_remediation() {
  local jobs
  jobs=$(collector_jobs) || err "could not list collector Jobs"
  [[ "$(jq 'length' <<<"${jobs}")" == "0" ]] || err "collector Jobs exist before any fault: ${jobs}"

  send_event e2e-lc-fault "$(fault_event)"

  local job
  if ! job=$(wait_for_job_done); then
    fail "collector Job runs" "no collector Job finished within $((PIPELINE_WAIT_SECONDS + COLLECTOR_WAIT_SECONDS))s; jobs: $(collector_jobs || echo '?')"
    return
  fi
  msg "Collector Job: ${job}"
  if jq -e --arg node "${NODE}" '.node == $node' <<<"${job}" >/dev/null; then
    pass "collector Job ran on the faulty node"
  else
    fail "collector Job ran on the faulty node" "got ${job}"
  fi
  if jq -e '.complete' <<<"${job}" >/dev/null; then
    pass "collector Job completed"
  else
    fail "collector Job completed" "it failed: ${job}; logs: $(kc -n nvsentinel logs "job/$(jq -r .name <<<"${job}")" --tail=30 2>&1 || true)"
    return
  fi

  local listing report page
  start_file_server_forward
  # The listing also links ../, so match the collector's timestamp directory.
  # Each page is captured before grep so grep -q cannot cut curl off mid-write.
  if listing=$(file_server_get "/upload/${NODE}/") &&
    report=$(grep -oE 'href="[0-9]{8}-[0-9]{6}/"' <<<"${listing}" | head -1 | sed -E 's/href="|\/"//g') &&
    [[ -n "${report}" ]] && page=$(file_server_get "/upload/${NODE}/${report}/") &&
    grep -q "nvidia-bug-report-${NODE}-" <<<"${page}"; then
    pass "bug report uploaded and listed at /upload/${NODE}/${report}/"
  else
    fail "bug report uploaded" "the file server lists none under /upload/${NODE}/: ${listing:-<no response>}"
  fi

  local reset_created
  if ! reset_created=$(wait_for_reset); then
    fail "GPU reset requested after collection" "no ${GPU_RESETS} within ${PIPELINE_WAIT_SECONDS}s"
    return
  fi
  if jq -en --arg finished "$(jq -r .completion <<<"${job}")" --arg reset "${reset_created}" \
    '($finished | fromdateiso8601) <= ($reset | fromdateiso8601)' >/dev/null; then
    pass "GPU reset requested at ${reset_created}, after the collector finished"
  else
    fail "GPU reset requested after collection" "reset at ${reset_created}, collector finished $(jq -r .completion <<<"${job}")"
  fi
}

# ── Main ──

msg "=========================================="
msg "nvsentinel-log-collector mixin e2e"
msg "=========================================="

has_tools go kind kubectl helm yq jq curl
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
compose_bundle
ensure_cluster
install_bundle
publish_proto
test_file_server_volume_bound
test_fault_is_collected_before_remediation

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel-log-collector e2e: all tests passed"
