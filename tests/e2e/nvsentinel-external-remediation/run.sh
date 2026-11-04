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
# nvsentinel-external-remediation mixin e2e (#3091)
# =============================================================================
#
# Installs the remediation step with the external remediation mixin, and the
# health events analyzer that live remediation requires, through the bundle's
# own install.sh on Kind. The leaf declares one fault: a kubernetes-object-monitor
# policy that maps the Node condition AICRE2EExternalRepair=True to CUSTOM /
# external-remediation. The test then drives the ExternalRemediationRequest
# (ERR) contract end to end, acting as the external system:
#
#   1. the declared fault cordons the node, fault-remediation creates one ERR,
#      and janitor releases the node (taint + managed=false); the labeler
#      strips driver.installed, so syslog-health-monitor leaves the node;
#   2. a fatal GPU XID sent while the node is released is stored STORE_ONLY
#      and acted on by nothing;
#   3. ExternalRemediationComplete=True returns the node: the taint and label
#      go, the ERR stays (preserve), the monitors come back, and the node stays
#      cordoned until the condition clears (upstream's known limitation);
#   4. a second fault parked at ExternalRemediationComplete=False keeps the node
#      released until `kubectl delete extrr` forces the release.
#
# fault-remediation labels the node remediation-succeeded when it creates the
# ERR, not when the repair finishes: it does not wait for the external system.
#
# TEST-ONLY SETTINGS, passed as --set on the bundle, as in
# tests/e2e/nvsentinel-log-collector:
#   janitor-provider.csp.provider=kind  NVSentinel's simulated provider, so
#       nothing can reboot the runner.
#   node-drainer.systemNamespaces  includes cert-manager and local-path-storage,
#       so the drain on Kind's one node finishes.
#   fault-quarantine.circuitBreaker.enabled=false  on one node the first
#       quarantine would trip it.
#   lifecycle-manager.config.tests.dcgm-diag-test  a busybox stub, as in
#       tests/e2e/nvsentinel-log-collector: Kind runs no DCGM, and a stubbed
#       command is what lets CheckNVSentinelPostRemediationValidationCoherent
#       skip its DCGM rule. No case here reboots, so nothing runs it.
#   lifecycle-manager.config.newNodeValidation=null  the node stays cordoned
#       after the handoff in case 3; on a Kind node younger than 30 minutes,
#       new-node validation would otherwise pick it up and uncordon it.
#   nvsentinel-mongodb at one member with smaller requests, and the analyzer
#       at a smaller CPU request, so everything fits one runner node beside
#       kubernetes-object-monitor.
#
# PREREQUISITES: go, kind, kubectl, helm, yq, jq, curl
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-3091-external-remediation-e2e).
#                  Always recreated, then deleted at the end unless
#                  KEEP_CLUSTER=true.
#   KEEP_CLUSTER   Skip cluster teardown on exit (default: false).
#   AICR_BIN       Path to a prebuilt aicr binary (default: built fresh).
# =============================================================================

# shellcheck disable=SC2016 # jq filters handed to node_matches and extrr_matches use jq's $vars.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DIR}/../../.." && pwd)"
# shellcheck source=/dev/null
. "${ROOT}/tools/common"

CLUSTER_NAME="${CLUSTER_NAME:-aicr-3091-external-remediation-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NODE="${CLUSTER_NAME}-control-plane"
# The digest-pinned busybox tests/e2e/nvsentinel-log-collector uses.
BUSYBOX_IMAGE="docker.io/library/busybox:1.38.0@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e"

GRPCURL_IMAGE="docker.io/fullstorydev/grpcurl:v1.9.3@sha256:085e183ca334eb4e81ca81ee12cbb2b2737505d1d77f5e33dabc5d066593d998"
DRAIN_SYSTEM_NAMESPACES='^(nvsentinel|kube-system|cert-manager|local-path-storage)$'
GPU_UUID="GPU-00000000-0000-0000-0000-00000000e2ea"

# The declared fault. The condition type must differ from the policy name:
# platform-connectors writes a Node condition named after the event's
# checkName, which is the policy name.
REPAIR_CONDITION="AICRE2EExternalRepair"
POLICY_NAME="aicr-e2e-external-repair"
POLICY_ERROR_CODE="AICR_E2E_EXTERNAL_REPAIR"

# The mixin's action key and its template's ERR name prefix.
ACTION_KEY="external-remediation"
EXTRR_NAME_PREFIX="extrr-"

# Upstream names, verified against NVSentinel v1.28.0:
#   CRD, kind, short name     distros/kubernetes/nvsentinel/charts/janitor/crds/external_remediation.crd.yaml:6,11,16
#   conditions, reason, taint janitor/pkg/controller/externalremediationrequest_controller.go:49-50,57,59
#   janitor's False log line  janitor/pkg/controller/externalremediationrequest_controller.go:622
#   managed label             commons/pkg/managed/managed.go:50,55
#   driver.installed label    labeler/pkg/labeler/labeler.go:48, stamped from nvidia.com/gpu.present at :748
#   DaemonSet name            charts/syslog-health-monitor/templates/_helpers.tpl:12,54 (suffix "regular")
#   gate log line, container  platform-connectors/pkg/transformers/metadata/transformer.go:157; templates/daemonset.yaml:60
#   cache TTL default         distros/kubernetes/nvsentinel/values.yaml:585
#   state label and value     commons/pkg/statemanager/statemanager.go:188,204
#   quarantine annotation     fault-quarantine/pkg/common/common.go:37
#   remediation annotation    fault-remediation/pkg/annotation/annotation_interface.go:26
#   preserve annotation       janitor/pkg/ttl/ttl.go:44
EXTRR_CRD="externalremediationrequests.nvsentinel.dgxc.nvidia.com"
EXTRR_KIND="ExternalRemediationRequest"
EXTRR_SHORT_NAME="extrr"
COND_RELEASED="NVSentinelOwnershipReleased"
COND_COMPLETE="ExternalRemediationComplete"
REASON_RELEASED="ReleaseTaintApplied"
RELEASE_TAINT_KEY="nvsentinel.dgxc.nvidia.com/external-remediation"
RELEASE_TAINT_EFFECT="NoSchedule"
JANITOR_FALSE_LOG="external system reported failure; node remains released until operator deletes ExtRR or external system retries"
MANAGED_LABEL="nvsentinel.dgxc.nvidia.com/managed"
MANAGED_FALSE="false"
DRIVER_INSTALLED_LABEL="nvsentinel.dgxc.nvidia.com/driver.installed"
GPU_PRESENT_LABEL="nvidia.com/gpu.present"
MONITOR_DAEMONSET="syslog-health-monitor-regular"
PC_DAEMONSET="platform-connectors"
PC_CONTAINER="platform-connector"
PC_GATED_LOG="Event gated to STORE_ONLY by managed-label check"
PC_CACHE_TTL_DEFAULT_SECONDS=60
STATE_LABEL="dgxc.nvidia.com/nvsentinel-state"
STATE_REMEDIATION_SUCCEEDED="remediation-succeeded"
QUARANTINE_ANNOTATION="quarantineHealthEvent"
FR_STATE_ANNOTATION="latestFaultRemediationState"
PRESERVE_ANNOTATION="nvsentinel.nvidia.com/preserve"
FR_CONFIGMAP="fault-remediation"
KOM_CONFIGMAP="kubernetes-object-monitor"
REPAIR_RESOURCES=(gpuresets.janitor.dgxc.nvidia.com rebootnodes.janitor.dgxc.nvidia.com terminatenodes.janitor.dgxc.nvidia.com)

NODE_READY_WAIT_SECONDS=120
CERT_MANAGER_WAIT_SECONDS=600
MONGODB_WAIT_SECONDS=900
NVSENTINEL_WAIT_SECONDS=900
# The labeler stamping driver.installed and the DaemonSet scheduling on it.
LABEL_WAIT_SECONDS=180
# Condition to cordon, drain and ERR; or a healthy event to the uncordon.
PIPELINE_WAIT_SECONDS=300
# janitor's apply or cleanup PATCH, the labeler's reaction, and a monitor pod
# terminating or starting.
RELEASE_WAIT_SECONDS=180
GATE_MARGIN_SECONDS=15
LOG_WAIT_SECONDS=60
# A health event pod running to completion.
EVENT_POD_WAIT_SECONDS=180
# How long a gated event is given to do something it should not.
GATED_SETTLE_SECONDS=60
# How long the node must stay released after Complete=False.
FALSE_HOLD_SECONDS=95
# How long the node must stay cordoned after Complete=True with the condition
# still True.
CORDON_HOLD_SECONDS=45
DELETE_WAIT_SECONDS=120
POLL_INTERVAL_SECONDS=3

WORK=""
CREATED_CLUSTER=false
BUNDLE_DIR=""
CRDS_DIR=""
CERT_MANAGER_DIR=""
PSMDB_OPERATOR_DIR=""
MONGODB_DIR=""
MONGODB_POST_DIR=""
PC_CACHE_TTL_SECONDS=""
FIRST_EXTRR=""
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
# fatal records a failure that every later case depends on, then stops.
fatal() {
  fail "$1" "$2"
  err "stopping at ${PASSED_TESTS}/${TOTAL_TESTS} passed: later cases depend on '$1'"
}

kc() { kubectl --context "${KUBE_CONTEXT}" "$@"; }

now_utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# wait_until runs "$2..." every POLL_INTERVAL_SECONDS until it succeeds, or
# returns 1 once SECONDS reaches the deadline $1. A failed query counts as
# "not yet", so every wait fails closed at its deadline.
wait_until() {
  local deadline="$1"
  shift
  while :; do
    "$@" && return 0
    ((SECONDS < deadline)) || return 1
    sleep "${POLL_INTERVAL_SECONDS}"
  done
}

# hold_for requires the command to stay true for $1 seconds, polling, and
# fails the first time it does not.
hold_for() {
  local deadline=$((SECONDS + $1))
  shift
  while ((SECONDS < deadline)); do
    "$@" || return 1
    sleep "${POLL_INTERVAL_SECONDS}"
  done
  "$@"
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
  msg "Composing a bundle with the nvsentinel-remediation, nvsentinel-health-events-analyzer, nvsentinel-node-validation and nvsentinel-external-remediation mixins..."
  local data_dir="${WORK}/data" leaf_refs="${WORK}/leaf-component-refs.yaml"
  mkdir -p "${data_dir}/overlays"
  cat >"${data_dir}/registry.yaml" <<'REGISTRY_EOF'
kind: ComponentRegistry
apiVersion: aicr.run/v1beta1
metadata:
  name: nvsentinel-external-remediation-e2e-registry
components: []
REGISTRY_EOF
  # The declared fault lives on the leaf, as an operator would write it; the
  # mixin carries only the routing.
  cat >"${leaf_refs}" <<LEAF_EOF
- name: nvsentinel
  type: Helm
  overrides:
    global:
      kubernetesObjectMonitor:
        enabled: true
    kubernetes-object-monitor:
      policies:
        - name: ${POLICY_NAME}
          enabled: true
          resource:
            group: ""
            version: v1
            kind: Node
          predicate:
            expression: |
              has(resource.status) && has(resource.status.conditions) &&
              resource.status.conditions.exists(c, c.type == '${REPAIR_CONDITION}' && c.status == 'True')
          nodeAssociation:
            expression: resource.metadata.name
          healthEvent:
            componentClass: Node
            isFatal: true
            message: "aicr e2e: an external system was asked to repair the node"
            recommendedAction: CUSTOM
            customRecommendedAction: ${ACTION_KEY}
            errorCode:
              - ${POLICY_ERROR_CODE}
LEAF_EOF
  LEAF_REFS="${leaf_refs}" yq '.spec.mixins = ["nvsentinel-remediation", "nvsentinel-health-events-analyzer", "nvsentinel-node-validation", "nvsentinel-external-remediation"] |
    .spec.componentRefs += load(strenv(LEAF_REFS))' \
    "${ROOT}/recipes/overlays/h100-kind-training.yaml" \
    >"${data_dir}/overlays/h100-kind-training.yaml" ||
    err "could not write the e2e leaf overlay"

  "${AICR_BIN}" recipe --service kind --accelerator h100 --intent training \
    --data "${data_dir}" --output "${WORK}/recipe.yaml" ||
    err "recipe generation failed"
  local diag_stub
  diag_stub=$(jq -cn --arg image "${BUSYBOX_IMAGE}" '{image: $image, command: ["sh", "-c", "exit 0"]}')
  "${AICR_BIN}" bundle -r "${WORK}/recipe.yaml" -o "${WORK}/bundle" \
    --set "nvsentinel:janitor-provider.csp.provider=kind" \
    --set-json "nvsentinel:lifecycle-manager.config.tests.dcgm-diag-test=${diag_stub}" \
    --set-json "nvsentinel:lifecycle-manager.config.newNodeValidation=null" \
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

  # Fail here, not later as "no ERR was created": a mixin or leaf policy that
  # stopped composing would otherwise read as a pipeline that never acted.
  local values="${BUNDLE_DIR}/values.yaml"
  [[ "$(yq '.global.dryRun' "${values}")" == "false" ]] ||
    err "bundle's nvsentinel values are not live (global.dryRun != false), so no ERR would be created"
  [[ "$(yq '.global.faultRemediation.enabled' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not enable fault-remediation -- composition regressed"
  [[ "$(ACTION="${ACTION_KEY}" yq '.["fault-remediation"].maintenance.actions[strenv(ACTION)].kind' "${values}")" == "${EXTRR_KIND}" ]] ||
    err "bundle's fault-remediation has no ${ACTION_KEY} action of kind ${EXTRR_KIND} -- the mixin did not compose"
  ACTION="${ACTION_KEY}" yq '.["fault-remediation"].maintenance.templates[strenv(ACTION)]' "${values}" |
    grep -qF "${PRESERVE_ANNOTATION}: \"true\"" ||
    err "bundle's ${ACTION_KEY} template does not carry ${PRESERVE_ANNOTATION}: \"true\" -- janitor's TTL would release the node"
  [[ "$(yq '.global.kubernetesObjectMonitor.enabled' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not enable kubernetes-object-monitor -- the leaf override was lost"
  [[ "$(POLICY="${POLICY_NAME}" yq '.["kubernetes-object-monitor"].policies[] | select(.name == strenv(POLICY)) |
    [.healthEvent.recommendedAction, .healthEvent.customRecommendedAction, .healthEvent.isFatal] | join(",")' "${values}")" == "CUSTOM,${ACTION_KEY},true" ]] ||
    err "bundle's kubernetes-object-monitor policies lack ${POLICY_NAME} as CUSTOM/${ACTION_KEY}/isFatal -- the leaf override was lost"

  # The gated-event case waits out the managed-label cache, so read its TTL
  # from the bundle in case a later AICR change overrides the chart default.
  local skip_label
  skip_label=$(yq '.platformConnector.transformers.MetadataAugmentor.skipNodeLabel' "${values}")
  [[ "${skip_label}" == "null" || "${skip_label}" == "${MANAGED_LABEL}=${MANAGED_FALSE}" ]] ||
    err "bundle overrides platform-connectors' skipNodeLabel to '${skip_label}'; the managed-label gate this test asserts would not apply"
  PC_CACHE_TTL_SECONDS=$(yq ".platformConnector.transformers.MetadataAugmentor.cacheTTLSeconds // ${PC_CACHE_TTL_DEFAULT_SECONDS}" "${values}")
  [[ "${PC_CACHE_TTL_SECONDS}" =~ ^[0-9]+$ ]] ||
    err "bundle's platform-connectors cacheTTLSeconds is not a number: ${PC_CACHE_TTL_SECONDS}"
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
  kc wait --for=condition=Ready node --all --timeout="${NODE_READY_WAIT_SECONDS}s" || err "cluster nodes never became Ready"
  # fault-quarantine counts GPU nodes by this label and refuses to act when
  # there are none; the labeler assumes the driver only on such a node.
  kc label node "${NODE}" "${GPU_PRESENT_LABEL}=true" --overwrite >/dev/null ||
    err "could not label ${NODE} ${GPU_PRESENT_LABEL}=true"
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

  install_component "${BUNDLE_DIR}"

  deadline=$((SECONDS + NVSENTINEL_WAIT_SECONDS))
  for d in fault-quarantine node-drainer fault-remediation janitor janitor-provider labeler \
    kubernetes-object-monitor health-events-analyzer; do
    wait_rollout deployment "${d}" nvsentinel "${deadline}"
  done
  wait_rollout daemonset "${PC_DAEMONSET}" nvsentinel "${deadline}"
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

# fault_event prints one fatal GPU XID HealthEvents request that, on a node
# NVSentinel owns, would quarantine it and request a GPU reset.
fault_event() {
  jq -cn --arg node "${NODE}" --arg uuid "${GPU_UUID}" --arg ts "$(now_utc)" '{
    version: 1,
    events: [{
      version: 1,
      agent: "gpu-health-monitor",
      componentClass: "GPU",
      checkName: "GpuXidError",
      isFatal: true,
      isHealthy: false,
      message: "aicr e2e synthetic XID 79 on a released node",
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
  if ! kc -n nvsentinel wait --for=jsonpath='{.status.phase}'=Succeeded "pod/${name}" --timeout="${EVENT_POD_WAIT_SECONDS}s" >/dev/null; then
    kc -n nvsentinel logs "pod/${name}" 2>&1 | tail -20 >&2 || true
    err "event ${name} was not accepted by platform-connectors"
  fi
}

# ── Cluster state helpers ──
#
# Each returns nonzero when its query fails, so a predicate polled by
# wait_until cannot be satisfied by an apiserver error.

# node_matches evaluates the jq filter $1 against the node, with the jq
# options that follow (e.g. --arg).
node_matches() {
  local filter="$1" json
  shift
  json=$(kc get node "${NODE}" -o json) || return 1
  jq -e "$@" "${filter}" <<<"${json}" >/dev/null
}

node_cordoned() { node_matches '.spec.unschedulable == true'; }
node_uncordoned() { node_matches '(.spec.unschedulable // false) == false'; }
node_label_is() { node_matches '.metadata.labels[$k] == $v' --arg k "$1" --arg v "$2"; }
node_label_absent() { node_matches '(.metadata.labels // {}) | has($k) | not' --arg k "$1"; }
node_annotation_absent() { node_matches '(.metadata.annotations // {}) | has($k) | not' --arg k "$1"; }

node_has_release_taint() {
  node_matches 'any(.spec.taints[]?; .key == $k and .value == $v and .effect == $e)' \
    --arg k "${RELEASE_TAINT_KEY}" --arg v "$1" --arg e "${RELEASE_TAINT_EFFECT}"
}
node_has_no_release_taint() { node_matches 'all(.spec.taints[]?; .key != $k)' --arg k "${RELEASE_TAINT_KEY}"; }

node_released_by() {
  node_has_release_taint "$1" && node_label_is "${MANAGED_LABEL}" "${MANAGED_FALSE}"
}
node_returned() {
  node_has_no_release_taint && node_label_absent "${MANAGED_LABEL}"
}

# The quarantine session is over: uncordoned, and fault-quarantine and
# fault-remediation have both dropped their state for the node.
node_session_ended() {
  node_uncordoned && node_label_absent "${STATE_LABEL}" &&
    node_annotation_absent "${QUARANTINE_ANNOTATION}" && node_annotation_absent "${FR_STATE_ANNOTATION}"
}

node_condition_count() {
  local json
  json=$(kc get node "${NODE}" -o json) || return 1
  jq --arg t "${REPAIR_CONDITION}" '[.status.conditions[]? | select(.type == $t)] | length' <<<"${json}"
}

# set_repair_condition plays the controller that owns the declared Node
# condition: it adds it as True, or removes it. A JSON patch with a test op,
# so a concurrent kubelet status write cannot shift the index under it.
set_repair_condition() {
  local want="$1" json idx patch ts attempt
  for attempt in 1 2 3; do
    json=$(kc get node "${NODE}" -o json) || err "could not read node ${NODE}"
    idx=$(jq --arg t "${REPAIR_CONDITION}" '[(.status.conditions // []) | to_entries[] | select(.value.type == $t) | .key]' <<<"${json}")
    if [[ "${want}" == "present" ]]; then
      [[ "$(jq 'length' <<<"${idx}")" == "0" ]] || err "${REPAIR_CONDITION} is already on ${NODE}"
      ts=$(now_utc)
      patch=$(jq -cn --arg t "${REPAIR_CONDITION}" --arg ts "${ts}" '[{op: "add", path: "/status/conditions/-", value: {
        type: $t, status: "True", reason: "ExternalRepairRequested",
        message: "aicr e2e: an external system owns this repair",
        lastHeartbeatTime: $ts, lastTransitionTime: $ts}}]')
    else
      [[ "$(jq 'length' <<<"${idx}")" == "1" ]] || err "expected exactly one ${REPAIR_CONDITION} on ${NODE}, found ${idx}"
      patch=$(jq -cn --arg t "${REPAIR_CONDITION}" --argjson i "$(jq '.[0]' <<<"${idx}")" \
        '[{op: "test", path: "/status/conditions/\($i)/type", value: $t}, {op: "remove", path: "/status/conditions/\($i)"}]')
    fi
    kc patch node "${NODE}" --subresource=status --type=json -p "${patch}" >/dev/null && break
    ((attempt < 3)) || err "could not set ${REPAIR_CONDITION} ${want} on ${NODE}"
    sleep "${POLL_INTERVAL_SECONDS}"
  done
  local count
  count=$(node_condition_count) || err "could not read back ${REPAIR_CONDITION} on ${NODE}"
  if [[ "${want}" == "present" ]]; then
    [[ "${count}" == "1" ]] || err "${REPAIR_CONDITION} did not persist on ${NODE} (count ${count})"
  else
    [[ "${count}" == "0" ]] || err "${REPAIR_CONDITION} is still on ${NODE} (count ${count})"
  fi
}

# monitor_state prints, for MONITOR_DAEMONSET, "<desiredNumberScheduled>
# <pods on the node> <live pods on the node> <Ready live pods on the node>
# <numberReady> <updatedNumberScheduled> <rolled out>". A pod being deleted
# still counts as on the node until it is gone; rolled out is 1 once the
# controller has observed the DaemonSet's current generation.
monitor_state() {
  local ds pods status counts field
  local -a fields
  ds=$(kc -n nvsentinel get daemonset "${MONITOR_DAEMONSET}" -o json) || return 1
  pods=$(kc -n nvsentinel get pods --field-selector "spec.nodeName=${NODE}" -o json) || return 1
  status=$(jq -r '"\(.status.desiredNumberScheduled // 0) \(.status.numberReady // 0)
    \(.status.updatedNumberScheduled // 0) \(if (.status.observedGeneration // 0) >= .metadata.generation then 1 else 0 end)"' \
    <<<"${ds}" | tr '\n' ' ') || return 1
  counts=$(jq -r --arg ds "${MONITOR_DAEMONSET}" '[.items[] |
      select(any(.metadata.ownerReferences[]?; .kind == "DaemonSet" and .name == $ds))] |
    (map(select(.metadata.deletionTimestamp == null))) as $live |
    "\(length) \($live | length) \($live | map(select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))) | length)"' \
    <<<"${pods}") || return 1
  read -r -a fields <<<"${status} ${counts}"
  ((${#fields[@]} == 7)) || return 1
  for field in "${fields[@]}"; do
    [[ "${field}" =~ ^[0-9]+$ ]] || return 1
  done
  echo "${fields[0]} ${fields[4]} ${fields[5]} ${fields[6]} ${fields[1]} ${fields[2]} ${fields[3]}"
}

# monitors_on_node requires the monitor to be running, not merely scheduled:
# a Ready pod on the node, and the DaemonSet rolled out with every desired pod
# ready and updated.
monitors_on_node() {
  local state desired on live ready ds_ready updated rolled
  state=$(monitor_state) || return 1
  read -r desired on live ready ds_ready updated rolled <<<"${state}"
  node_label_is "${DRIVER_INSTALLED_LABEL}" "true" && ((desired == 1 && ready >= 1 && ds_ready == desired &&
    updated == desired && rolled == 1))
}
monitors_off_node() {
  local state desired on live
  state=$(monitor_state) || return 1
  read -r desired on live _ <<<"${state}"
  node_label_absent "${DRIVER_INSTALLED_LABEL}" && ((desired == 0 && on == 0 && live == 0))
}

# extrrs prints every ERR as a JSON list of the fields the cases assert.
extrrs() {
  local json
  json=$(kc get "${EXTRR_SHORT_NAME}" -o json) || return 1
  jq -c --arg released "${COND_RELEASED}" --arg complete "${COND_COMPLETE}" --arg preserve "${PRESERVE_ANNOTATION}" '
    def cond($t): [(.status.conditions // [])[] | select(.type == $t)] | first // {};
    [.items[] | {
      name: .metadata.name,
      node: .spec.healthEvent.nodeName,
      action: .spec.healthEvent.recommendedAction,
      custom: .spec.healthEvent.customRecommendedAction,
      preserve: (.metadata.annotations[$preserve] // ""),
      released: cond($released),
      complete: cond($complete),
      completionTime: (.status.completionTime // ""),
      deleting: (.metadata.deletionTimestamp != null)
    }]' <<<"${json}"
}

extrr_count_is() {
  local list
  list=$(extrrs) || return 1
  [[ "$(jq 'length' <<<"${list}")" == "$1" ]]
}

# extrr_matches evaluates the jq filter $2 against ERR $1.
extrr_matches() {
  local name="$1" filter="$2" list
  shift 2
  list=$(extrrs) || return 1
  jq -e --arg name "${name}" "$@" "first(.[] | select(.name == \$name)) | ${filter}" <<<"${list}" >/dev/null
}

extrr_released() {
  extrr_matches "$1" '.released.status == "True" and .released.reason == $r' --arg r "${REASON_RELEASED}"
}
extrr_completed() { extrr_matches "$1" '.completionTime != ""'; }

# new_extrr_for_node prints the name of the one ERR for NODE that is not $1.
new_extrr_for_node() {
  local list
  list=$(extrrs) || return 1
  jq -er --arg node "${NODE}" --arg old "$1" \
    '[.[] | select(.node == $node and .name != $old)] | if length == 1 then .[0].name else empty end' <<<"${list}"
}

# set_extrr_complete writes ExternalRemediationComplete=$2 on ERR $1 as the
# external system must: through /status, replacing only that condition, with
# a test op so a reordered list fails the patch instead of overwriting the
# other condition.
set_extrr_complete() {
  local name="$1" status="$2" json idx patch
  json=$(kc get "${EXTRR_SHORT_NAME}" "${name}" -o json) || return 1
  idx=$(jq --arg t "${COND_COMPLETE}" '[(.status.conditions // []) | to_entries[] | select(.value.type == $t) | .key] |
    if length == 1 then .[0] else empty end' <<<"${json}")
  [[ "${idx}" =~ ^[0-9]+$ ]] || return 1
  patch=$(jq -cn --arg t "${COND_COMPLETE}" --arg s "${status}" --arg ts "$(now_utc)" --argjson i "${idx}" '[
    {op: "test", path: "/status/conditions/\($i)/type", value: $t},
    {op: "replace", path: "/status/conditions/\($i)", value: {
      type: $t, status: $s, reason: (if $s == "True" then "RepairSucceeded" else "RepairAbandoned" end),
      message: "aicr e2e external system", lastTransitionTime: $ts}}]')
  kc patch "${EXTRR_SHORT_NAME}" "${name}" --subresource=status --type=json -p "${patch}" >/dev/null
}

# logged checks whether $1/$2 (container $3) logged a line since $4 that
# contains both $5 and $6. The second grep is not -q: exiting early would
# SIGPIPE the first, which pipefail reports as no match.
logged() {
  local workload="$1" container="$3" since="$4" needle="$5" also="$6" logs
  logs=$(kc -n nvsentinel logs "${workload}/$2" -c "${container}" --since-time="${since}" --tail=-1) || return 1
  grep -F -- "${needle}" <<<"${logs}" | grep -F -- "${also}" >/dev/null
}

# repairs_requested prints the janitor repair objects that exist. A failed
# list fails rather than reading as "none".
repairs_requested() {
  local resource found out=""
  for resource in "${REPAIR_RESOURCES[@]}"; do
    found=$(kc get "${resource}" -A -o name) || return 1
    [[ -z "${found}" ]] || out+="${found//$'\n'/ } "
  done
  echo "${out}"
}

# ── Tests ──

# Step 2: preconditions. Each fails closed, so no later assertion can pass
# against a pipeline that was never wired.
test_preconditions() {
  local list cm deadline

  kc get crd "${EXTRR_CRD}" -o name >/dev/null ||
    fatal "ERR CRD installed" "crd/${EXTRR_CRD} is not served"
  list=$(extrrs) || fatal "ERR short name" "kubectl get ${EXTRR_SHORT_NAME} failed"
  [[ "$(jq 'length' <<<"${list}")" == "0" ]] || fatal "no ERR before any fault" "found ${list}"
  pass "crd/${EXTRR_CRD} is served as ${EXTRR_SHORT_NAME} and no ERR exists yet"

  cm=$(kc -n nvsentinel get configmap "${FR_CONFIGMAP}" -o json) ||
    fatal "fault-remediation config" "could not read configmap/${FR_CONFIGMAP}"
  if jq -e --arg hdr "[remediationActions.\"${ACTION_KEY}\"]" --arg kind "kind = \"${EXTRR_KIND}\"" \
    --arg key "${ACTION_KEY}" --arg preserve "${PRESERVE_ANNOTATION}: \"true\"" \
    '(.data["config.toml"] | contains($hdr) and contains($kind)) and (.data[$key] // "" | contains($preserve))' \
    <<<"${cm}" >/dev/null; then
    pass "fault-remediation's config routes ${ACTION_KEY} to ${EXTRR_KIND} with a preserved template"
  else
    fatal "fault-remediation config" "configmap/${FR_CONFIGMAP} lacks the ${ACTION_KEY} action or its preserved template"
  fi

  cm=$(kc -n nvsentinel get configmap "${KOM_CONFIGMAP}" -o json) ||
    fatal "kubernetes-object-monitor policy" "could not read configmap/${KOM_CONFIGMAP}"
  if jq -e --arg name "name = \"${POLICY_NAME}\"" --arg custom "customRecommendedAction = \"${ACTION_KEY}\"" \
    '.data["config.toml"] | contains($name) and contains($custom)' <<<"${cm}" >/dev/null; then
    pass "kubernetes-object-monitor loaded ${POLICY_NAME} with customRecommendedAction ${ACTION_KEY}"
  else
    fatal "kubernetes-object-monitor policy" "configmap/${KOM_CONFIGMAP} lacks ${POLICY_NAME} or its customRecommendedAction"
  fi

  deadline=$((SECONDS + LABEL_WAIT_SECONDS))
  if wait_until "${deadline}" monitors_on_node; then
    pass "labeler stamped ${DRIVER_INSTALLED_LABEL}=true and ${MONITOR_DAEMONSET} runs a pod on ${NODE}"
  else
    fatal "monitors on the node" "want ${DRIVER_INSTALLED_LABEL}=true and a Ready, rolled-out ${MONITOR_DAEMONSET} pod; got state '$(monitor_state || echo '?')'"
  fi

  if node_uncordoned && node_returned && node_label_absent "${STATE_LABEL}"; then
    pass "${NODE} starts schedulable, unreleased and with no NVSentinel state"
  else
    fatal "clean starting node" "$(kc get node "${NODE}" -o json | jq -c '{spec: .spec, labels: .metadata.labels}' || echo '?')"
  fi
}

# Step 3: a declared fault creates one ERR, janitor releases the node, the
# monitors leave, and a later fault is stored but not acted on.
test_declared_fault_releases_node() {
  local deadline list name released_at gated_since before after repairs

  set_repair_condition present
  msg "Set ${REPAIR_CONDITION}=True on ${NODE}"

  deadline=$((SECONDS + PIPELINE_WAIT_SECONDS))
  wait_until "${deadline}" node_cordoned ||
    fatal "declared fault cordons the node" "${NODE} not cordoned within ${PIPELINE_WAIT_SECONDS}s"
  pass "declared fault: fault-quarantine cordoned ${NODE}"

  # Set when fault-remediation creates the ERR; it does not mean repaired.
  if wait_until "${deadline}" node_label_is "${STATE_LABEL}" "${STATE_REMEDIATION_SUCCEEDED}"; then
    pass "fault-remediation labelled ${NODE} ${STATE_LABEL}=${STATE_REMEDIATION_SUCCEEDED} on creating the ERR"
  else
    fail "state label" "${STATE_LABEL} never reached ${STATE_REMEDIATION_SUCCEEDED}"
  fi

  wait_until "${deadline}" extrr_count_is 1 ||
    fatal "one ERR created" "want exactly one ${EXTRR_SHORT_NAME}; got $(extrrs || echo '?')"
  list=$(extrrs) || fatal "one ERR created" "could not list ${EXTRR_SHORT_NAME}"
  name=$(jq -r '.[0].name' <<<"${list}")
  FIRST_EXTRR="${name}"
  if jq -e --arg node "${NODE}" --arg custom "${ACTION_KEY}" --arg prefix "${EXTRR_NAME_PREFIX}" \
    '.[0] | .node == $node and .custom == $custom and (.action == "CUSTOM" or .action == 27) and
      .preserve == "true" and (.name | startswith($prefix))' <<<"${list}" >/dev/null; then
    pass "one ERR ${name} for ${NODE}, CUSTOM/${ACTION_KEY}, preserved"
  else
    fail "ERR content" "got $(jq -c '.[0]' <<<"${list}")"
  fi

  deadline=$((SECONDS + RELEASE_WAIT_SECONDS))
  if wait_until "${deadline}" extrr_released "${name}"; then
    pass "${COND_RELEASED}=True with reason ${REASON_RELEASED}"
  else
    fatal "ownership released" "${name} conditions: $(extrrs | jq -c --arg n "${name}" '.[] | select(.name == $n) | .released' || echo '?')"
  fi
  wait_until "${deadline}" node_has_release_taint "${name}" ||
    fatal "release taint" "${NODE} lacks ${RELEASE_TAINT_KEY}=${name}:${RELEASE_TAINT_EFFECT}"
  pass "${NODE} tainted ${RELEASE_TAINT_KEY}=${name}:${RELEASE_TAINT_EFFECT}"
  wait_until "${deadline}" node_label_is "${MANAGED_LABEL}" "${MANAGED_FALSE}" ||
    fatal "managed label" "${NODE} lacks ${MANAGED_LABEL}=${MANAGED_FALSE}"
  released_at=${SECONDS}
  pass "${NODE} labelled ${MANAGED_LABEL}=${MANAGED_FALSE}"

  if wait_until "${deadline}" monitors_off_node; then
    pass "monitors left: ${DRIVER_INSTALLED_LABEL} removed and ${MONITOR_DAEMONSET} desires 0 with no pod on ${NODE}"
  else
    fail "monitors leave the node" "state '$(monitor_state || echo '?')', driver label present: $(node_label_absent "${DRIVER_INSTALLED_LABEL}" && echo no || echo yes)"
  fi

  # platform-connectors caches a node's metadata for cacheTTLSeconds, so an
  # event inside that window after the label landed could read the
  # pre-release entry and pass ungated.
  local wait_for=$((released_at + PC_CACHE_TTL_SECONDS + GATE_MARGIN_SECONDS - SECONDS))
  if ((wait_for > 0)); then
    msg "Waiting ${wait_for}s for platform-connectors' ${PC_CACHE_TTL_SECONDS}s node metadata cache to expire..."
    sleep "${wait_for}"
  fi

  before=$(kc get node "${NODE}" -o json | jq -er --arg k "${QUARANTINE_ANNOTATION}" '.metadata.annotations[$k]') ||
    fatal "quarantine annotation" "${NODE} has no readable ${QUARANTINE_ANNOTATION} while quarantined"
  gated_since=$(now_utc)
  send_event e2e-extrr-gated-xid "$(fault_event)"

  deadline=$((SECONDS + LOG_WAIT_SECONDS))
  if wait_until "${deadline}" logged daemonset "${PC_DAEMONSET}" "${PC_CONTAINER}" "${gated_since}" "${PC_GATED_LOG}" "${NODE}"; then
    pass "platform-connectors logged '${PC_GATED_LOG}' for ${NODE}"
  else
    fail "event gated" "no '${PC_GATED_LOG}' for ${NODE} within ${LOG_WAIT_SECONDS}s"
  fi

  sleep "${GATED_SETTLE_SECONDS}"
  if repairs=$(repairs_requested); then
    if [[ -z "${repairs}" ]]; then
      pass "gated fault requested no GPUReset, RebootNode or TerminateNode"
    else
      fail "gated fault requests no repair" "found ${repairs}"
    fi
  else
    fail "gated fault requests no repair" "could not list the janitor repair resources"
  fi
  if extrr_count_is 1; then
    pass "gated fault created no second ERR"
  else
    fail "gated fault creates no ERR" "got $(extrrs || echo '?')"
  fi
  if after=$(kc get node "${NODE}" -o json | jq -er --arg k "${QUARANTINE_ANNOTATION}" '.metadata.annotations[$k]'); then
    if [[ "${after}" == "${before}" ]]; then
      pass "gated fault left ${QUARANTINE_ANNOTATION} unchanged"
    else
      fail "quarantine annotation unchanged" "before ${before}; after ${after}"
    fi
  else
    fail "quarantine annotation unchanged" "could not read ${QUARANTINE_ANNOTATION} after the gated fault"
  fi
}

# Step 4: Complete=True returns the node, which stays cordoned until the
# declared condition clears.
test_complete_true_returns_node() {
  local name="${FIRST_EXTRR}" deadline

  set_extrr_complete "${name}" True ||
    fatal "external system writes Complete=True" "could not patch ${name}'s ${COND_COMPLETE} through /status"
  msg "Set ${COND_COMPLETE}=True on ${name}"

  deadline=$((SECONDS + RELEASE_WAIT_SECONDS))
  wait_until "${deadline}" node_returned ||
    fatal "Complete=True removes taint and label" "${NODE} still carries ${RELEASE_TAINT_KEY} or ${MANAGED_LABEL}"
  pass "Complete=True: janitor removed the release taint and ${MANAGED_LABEL}"

  if wait_until "${deadline}" extrr_completed "${name}"; then
    pass "${name} has status.completionTime and is kept as a record"
  else
    fail "ERR kept as a completed record" "got $(extrrs || echo '?')"
  fi

  if wait_until "${deadline}" monitors_on_node; then
    pass "monitors returned: ${DRIVER_INSTALLED_LABEL}=true and a Ready, rolled-out ${MONITOR_DAEMONSET} pod on ${NODE}"
  else
    fail "monitors return" "state '$(monitor_state || echo '?')'"
  fi

  # Upstream's known limitation: nothing uncordons until a healthy event.
  msg "Holding ${CORDON_HOLD_SECONDS}s with ${REPAIR_CONDITION} still True..."
  if hold_for "${CORDON_HOLD_SECONDS}" node_cordoned; then
    pass "${NODE} stayed cordoned ${CORDON_HOLD_SECONDS}s after Complete=True, until the condition clears"
  else
    fail "node stays cordoned after Complete=True" "${NODE} was uncordoned, or could not be read, with the declared condition still True"
  fi

  set_repair_condition absent
  msg "Removed ${REPAIR_CONDITION} from ${NODE}"
  deadline=$((SECONDS + PIPELINE_WAIT_SECONDS))
  wait_until "${deadline}" node_session_ended ||
    fatal "healthy event uncordons" "${NODE} not uncordoned with its NVSentinel state cleared within ${PIPELINE_WAIT_SECONDS}s"
  pass "clearing the condition uncordoned ${NODE} and ended the quarantine session"
}

# Step 5: Complete=False keeps the node released until an operator deletes
# the ERR.
test_complete_false_holds_until_delete() {
  local deadline name since rc out

  set_repair_condition present
  msg "Set ${REPAIR_CONDITION}=True on ${NODE} again"

  deadline=$((SECONDS + PIPELINE_WAIT_SECONDS))
  wait_until "${deadline}" node_cordoned ||
    fatal "second fault cordons the node" "${NODE} not cordoned within ${PIPELINE_WAIT_SECONDS}s"
  wait_until "${deadline}" new_extrr_for_node "${FIRST_EXTRR}" >/dev/null ||
    fatal "second ERR created" "no new ${EXTRR_SHORT_NAME} beside ${FIRST_EXTRR}; got $(extrrs || echo '?')"
  name=$(new_extrr_for_node "${FIRST_EXTRR}") || fatal "second ERR created" "could not read the new ERR's name"
  pass "a new session created a second ERR ${name}"

  deadline=$((SECONDS + RELEASE_WAIT_SECONDS))
  wait_until "${deadline}" extrr_released "${name}" ||
    fatal "second ERR released" "${name} never reached ${COND_RELEASED}=True"
  wait_until "${deadline}" node_released_by "${name}" ||
    fatal "second ERR released" "${NODE} lacks the ${name} release taint or ${MANAGED_LABEL}=${MANAGED_FALSE}"
  pass "${name} released ${NODE}"

  since=$(now_utc)
  set_extrr_complete "${name}" False ||
    fatal "external system writes Complete=False" "could not patch ${name}'s ${COND_COMPLETE} through /status"
  msg "Set ${COND_COMPLETE}=False on ${name}"
  # Proves janitor reconciled the False, so the hold below is not vacuous.
  deadline=$((SECONDS + LOG_WAIT_SECONDS))
  if wait_until "${deadline}" logged deployment janitor janitor "${since}" "${JANITOR_FALSE_LOG}" "${name}"; then
    pass "janitor observed ${COND_COMPLETE}=False on ${name}"
  else
    fail "janitor observes Complete=False" "no '${JANITOR_FALSE_LOG}' for ${name} within ${LOG_WAIT_SECONDS}s"
  fi

  msg "Holding ${FALSE_HOLD_SECONDS}s with ${name} at ${COND_COMPLETE}=False..."
  sleep "${FALSE_HOLD_SECONDS}"
  if node_released_by "${name}"; then
    pass "Complete=False: ${NODE} still carries the ${name} taint and ${MANAGED_LABEL}=${MANAGED_FALSE}"
  else
    fail "node stays released after Complete=False" "the release taint or label was removed"
  fi
  if extrr_matches "${name}" '.completionTime == "" and .complete.status == "False"'; then
    pass "Complete=False: ${name} has no completionTime"
  else
    fail "no completionTime after Complete=False" "got $(extrrs || echo '?')"
  fi
  if extrr_count_is 2; then
    pass "Complete=False created no further ERR"
  else
    fail "no new ERR after Complete=False" "got $(extrrs || echo '?')"
  fi
  if node_cordoned; then
    pass "Complete=False: ${NODE} is still cordoned"
  else
    fail "node stays cordoned after Complete=False" "${NODE} was uncordoned"
  fi

  kc delete "${EXTRR_SHORT_NAME}" "${name}" --wait --timeout="${DELETE_WAIT_SECONDS}s" >/dev/null ||
    fatal "forced release" "kubectl delete ${EXTRR_SHORT_NAME} ${name} did not finish within ${DELETE_WAIT_SECONDS}s"
  rc=0
  out=$(kc get "${EXTRR_SHORT_NAME}" "${name}" -o name 2>&1) || rc=$?
  if ((rc != 0)) && [[ "${out}" == *NotFound* ]]; then
    pass "kubectl delete ${EXTRR_SHORT_NAME} ${name}: the finalizer ran and the object is gone"
  elif ((rc == 0)); then
    fatal "forced release removes the ERR" "${name} still exists after delete"
  else
    fatal "forced release removes the ERR" "could not confirm ${name} is gone (rc=${rc}): ${out}"
  fi
  deadline=$((SECONDS + RELEASE_WAIT_SECONDS))
  if wait_until "${deadline}" node_returned; then
    pass "forced release removed the release taint and ${MANAGED_LABEL} from ${NODE}"
  else
    fail "forced release returns the node" "${NODE} still carries ${RELEASE_TAINT_KEY} or ${MANAGED_LABEL}"
  fi

  set_repair_condition absent
  msg "Removed ${REPAIR_CONDITION} from ${NODE}"
  deadline=$((SECONDS + PIPELINE_WAIT_SECONDS))
  if wait_until "${deadline}" node_session_ended; then
    pass "clearing the condition after the forced release uncordoned ${NODE}"
  else
    fail "cleanup uncordons" "${NODE} not uncordoned with its NVSentinel state cleared within ${PIPELINE_WAIT_SECONDS}s"
  fi
}

# ── Main ──

msg "=========================================="
msg "nvsentinel-external-remediation mixin e2e"
msg "=========================================="

has_tools go kind kubectl helm yq jq curl
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
compose_bundle
ensure_cluster
install_bundle
publish_proto
test_preconditions
test_declared_fault_releases_node
test_complete_true_returns_node
test_complete_false_holds_until_delete

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel-external-remediation e2e: all tests passed"
