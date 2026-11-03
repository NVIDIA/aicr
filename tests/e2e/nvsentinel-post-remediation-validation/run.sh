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
# NVSentinel post-remediation validation e2e (#3089)
# =============================================================================
#
# Bundle-only checks, then the remediation step with the analyzer and
# nvsentinel-node-validation installed through the bundle's own install.sh on
# Kind. Synthetic GPU faults are sent into platform-connectors' socket:
#
#   1. the gate refuses nvsentinel-remediation without
#      nvsentinel-node-validation, an empty validation rule set list, a rule
#      set naming a test nothing defines, a disabled rule set sharing the
#      post-reboot rule set's name, and a release taint whose value breaks
#      every test Job; it accepts the full
#      composition, whose values carry the mixin's post-reboot rule set;
#   2. a fatal RESTART_VM fault is quarantined, drained and repaired; its
#      healthy event makes fault-quarantine create a ValidationRequest and keep
#      the node cordoned while it runs; the pass uncordons it with
#      uncordon-reason=ValidationSucceeded;
#   3. a failing validation of a repaired RESTART_BM fault keeps the node
#      cordoned and labelled validation-failed; a passing retest of the same
#      tests releases it;
#   4. with lifecycle-manager down, fault-quarantine cannot create the request
#      and keeps the node quarantined; once lifecycle-manager is back, the
#      healthy event creates the request and the pass releases the node;
#   5. the documented recovery for such a node: fault-quarantine does not
#      retry on its own; under a NoSchedule hold taint, kubectl uncordon
#      cancels the quarantine, then a cordon and a hand-made request validate
#      it and the pass releases it;
#   6. a COMPONENT_RESET fault matches no rule set, so its healthy event
#      releases the node through fault-quarantine with no ValidationRequest.
#
# WHAT KIND CANNOT DO, AND WHAT STANDS IN FOR IT:
#   - No host to reboot. janitor-provider runs NVSentinel's simulated kind
#     provider, which completes a RebootNode without touching the node, so
#     cases 2 to 5 wait for fault-remediation's reboot request to complete
#     before the healthy event a rebooted node's monitors would send.
#   - No GPU to reset: janitor's reset Job needs the nvidia RuntimeClass and
#     waits for the GPU Operator's operands, holding the node's maintenance
#     lock meanwhile. So case 6 runs last, with fault-remediation scaled to 0;
#     what it checks is fault-quarantine's decision, not the reset.
#   - No GPUs and no DCGM hostengine, so dcgm-diag-test's command is a busybox
#     stub that sleeps, so the held cordon can be seen, then passes unless a
#     Service named FAIL_SERVICE exists in the test's namespace, which the
#     kubelet's service links expose to the pod as FAIL_ENV. Case 3 creates it
#     to fail one run and deletes it for the retest, so the same test can fail
#     and then pass without changing the bundle. Overriding the command is also
#     what lets the gates skip their DCGM rule.
#   - No nvidia.com/gpu allocatable, so readinessCriteria keeps only
#     not-under-quarantine, as tests/e2e/nvsentinel-node-validation does.
#   - nvidia.com/gpu.present=true is set by hand, as the GPU Operator would.
#
# TEST-ONLY SETTINGS, passed on the bundle: the stub and readinessCriteria
# above; the Job provider at 0 retries and a 180s timeout (chart: 5 and
# 1800s), so a failure is final at once; newNodeValidation set to null, which
# turns new-node discovery off: the Kind node is new, GPU-labelled and, once
# quarantined, cordoned, so lifecycle-manager would otherwise request a second
# validation of its own; janitor-provider.csp.provider=kind; and, as in
# tests/e2e/nvsentinel-remediation, node-drainer.systemNamespaces adding
# local-path-storage, the circuit breaker off, and nvsentinel-mongodb at one
# member with smaller requests, plus the analyzer at a smaller CPU request,
# so everything fits one runner node.
#
# PREREQUISITES: go, kind, kubectl, helm, yq, jq, curl
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-3089-kind).
#                  Always recreated, then deleted at the end unless
#                  KEEP_CLUSTER=true.
#   KEEP_CLUSTER   Skip cluster teardown on exit (default: false).
#   AICR_BIN       Path to a prebuilt aicr binary (default: built fresh).
# =============================================================================

# node_has and the stub command pass single-quoted text through to jq and sh.
# shellcheck disable=SC2016
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DIR}/../../.." && pwd)"
# shellcheck source=/dev/null
. "${ROOT}/tools/common"

CLUSTER_NAME="${CLUSTER_NAME:-aicr-3089-kind}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NODE="${CLUSTER_NAME}-control-plane"
NS="nvsentinel"

GRPCURL_IMAGE="docker.io/fullstorydev/grpcurl:v1.9.3@sha256:085e183ca334eb4e81ca81ee12cbb2b2737505d1d77f5e33dabc5d066593d998"
# The digest-pinned multi-arch busybox AICR already ships.
BUSYBOX_IMAGE="docker.io/library/busybox:1.38.0@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e"
DRAIN_EXTRA_NAMESPACES='^(local-path-storage)$'
# Each case faults its own GPU, so no case can match another's entries.
GPU_UUID_PREFIX="GPU-00000000-0000-0000-0000-00000000e3"
VR_RESOURCE="validationrequests.nvsentinel.nvidia.com"
REBOOT_RESOURCE="rebootnodes.janitor.dgxc.nvidia.com"
CREATED_BY_LABEL="nvsentinel.nvidia.com/created-by"
NEW_NODE_CONDITION="NewNodeValidationRequested"
STATE_LABEL="dgxc.nvidia.com/nvsentinel-state"
UNCORDON_REASON_LABEL="k8saas.nvidia.com/uncordon-reason"
VALIDATION_STATE_LABEL="nvsentinel.nvidia.com/validation-state"
QUARANTINE_ANNOTATION="quarantineHealthEvent"
VALIDATION_ANNOTATION="quarantineValidationHealthEvent"
POST_REBOOT_TEST="dcgm-diag-test"
UNDEFINED_TEST="aicr-e2e-undefined"
RETEST_VR="aicr-e2e-retest"
RECOVERY_VR="aicr-e2e-recovery"
HOLD_TAINT_KEY="aicr-e2e/validation-hold"
PROBE_VR="aicr-e2e-webhook-probe"
FAIL_SERVICE="aicr-e2e-fail-validation"
FAIL_ENV="$(tr 'a-z-' 'A-Z_' <<<"${FAIL_SERVICE}")_SERVICE_HOST"
CREATE_FAILED_LOG="Failed to create ValidationRequest for node, keeping node quarantined"
# Long enough that the held cordon is observable before the stub passes.
STUB_SECONDS=20
JOB_TIMEOUT_SECONDS=180
POLL_SECONDS=3
EVENT_WAIT_SECONDS=120
# Quarantine and drain each react to a change stream, so each hop takes
# seconds; the margin covers a cold informer cache.
QUARANTINE_WAIT_SECONDS=120
DRAIN_WAIT_SECONDS=120
VR_APPEAR_WAIT_SECONDS=90
# The simulated provider answers "not ready" about one time in twenty, and
# janitor then rechecks after 60s.
REPAIR_WAIT_SECONDS=180
JOB_APPEAR_WAIT_SECONDS=60
# Covers the stub image pull on a cold node as well as STUB_SECONDS.
VALIDATION_WAIT_SECONDS=240
RELEASE_WAIT_SECONDS=90
WEBHOOK_WAIT_SECONDS=120
SCALE_WAIT_SECONDS=120
LOG_WAIT_SECONDS=120
# How long a failed or uncreated validation is given to (wrongly) release the
# node.
SETTLE_SECONDS=30
CERT_MANAGER_WAIT_SECONDS=600
MONGODB_WAIT_SECONDS=900
NVSENTINEL_WAIT_SECONDS=900
LIFECYCLE_MANAGER_WAIT_SECONDS=180

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
  name: nvsentinel-post-remediation-validation-e2e-registry
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
  local recipe="$1" out="$2" stub_cmd stub readiness
  shift 2
  stub_cmd=$(printf 'sleep %s; if [ -n "${%s:-}" ]; then echo "Service %s exists: failing on request" >&2; exit 1; fi' \
    "${STUB_SECONDS}" "${FAIL_ENV}" "${FAIL_SERVICE}")
  stub=$(jq -cn --arg image "${BUSYBOX_IMAGE}" --arg cmd "${stub_cmd}" '{image: $image, command: ["sh", "-c", $cmd]}')
  readiness=$(jq -cn '[{
    name: "not-under-quarantine",
    expression: "!(has(node.metadata.annotations) && \"quarantineHealthEvent\" in node.metadata.annotations)"}]')
  rm -rf "${WORK}/bundle-${out}"
  "${AICR_BIN}" bundle -r "${WORK}/recipe-${recipe}.yaml" -o "${WORK}/bundle-${out}" \
    --set-json "nvsentinel:lifecycle-manager.config.tests.${POST_REBOOT_TEST}=${stub}" \
    --set-json "nvsentinel:lifecycle-manager.config.readinessCriteria=${readiness}" \
    --set-json "nvsentinel:lifecycle-manager.config.newNodeValidation=null" \
    --set-json "nvsentinel:lifecycle-manager.config.providers.k8s-job-provider.retries=0" \
    --set-json "nvsentinel:lifecycle-manager.config.providers.k8s-job-provider.timeoutSeconds=${JOB_TIMEOUT_SECONDS}" \
    --set "nvsentinel:janitor-provider.csp.provider=kind" \
    --set "nvsentinel:node-drainer.systemNamespaces=${DRAIN_EXTRA_NAMESPACES}" \
    --set "nvsentinel:fault-quarantine.circuitBreaker.enabled=false" \
    --set "nvsentinel:health-events-analyzer.resources.requests.cpu=250m" \
    --set "nvsentinelmongodb:unsafeFlags.replsetSize=true" \
    --set "nvsentinelmongodb:replsets.rs0.size=1" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.cpu=250m" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.memory=512Mi" \
    --system-node-selector kubernetes.io/os=linux \
    "$@" >"${WORK}/bundle-${out}.log" 2>&1
}

# expect_refusal asserts that bundling recipe $2 with the remaining arguments
# fails and names $3.
expect_refusal() {
  local name="$1" recipe="$2" want="$3"
  shift 3
  if bundle_to "${recipe}" refused "$@"; then
    fail "${name}" "the bundle was accepted"
  elif grep -qF -- "${want}" "${WORK}/bundle-refused.log"; then
    pass "${name}"
  else
    fail "${name}" "refused without naming '${want}': $(grep -m1 -i 'error' "${WORK}/bundle-refused.log" | cut -c1-400)"
  fi
}

# mixin_rule_sets prints the nvsentinel-remediation mixin's validation rule
# sets as compact JSON.
mixin_rule_sets() {
  yq -o=json '.spec.componentRefs[] | select(.name == "nvsentinel") | .overrides["fault-quarantine"].validation.ruleSets' \
    "${ROOT}/recipes/mixins/nvsentinel-remediation.yaml" | jq -c .
}

# test_gate covers case 1, and leaves the live bundle in ${WORK}/bundle-deploy.
test_gate() {
  msg "Case 1: the bundle gate..."
  compose_recipe without-nv nvsentinel-remediation nvsentinel-health-events-analyzer
  compose_recipe live nvsentinel-remediation nvsentinel-health-events-analyzer nvsentinel-node-validation

  local rule_sets undefined shadowed
  rule_sets=$(mixin_rule_sets)
  [[ "$(jq 'length' <<<"${rule_sets}")" -ge 1 ]] || err "could not read the remediation mixin's validation rule sets"
  undefined=$(jq -c --arg t "${UNDEFINED_TEST}" 'map(.tests = [$t])' <<<"${rule_sets}")
  # fault-quarantine looks tests up by rule set name, disabled sets included,
  # so this one would replace the post-reboot rule set's tests with none.
  shadowed=$(jq -c '. + [.[0] | .enabled = false | .tests = []]' <<<"${rule_sets}")

  expect_refusal "#1 gate refuses nvsentinel-remediation without nvsentinel-node-validation" \
    without-nv "compose the nvsentinel-node-validation mixin"
  expect_refusal "#1 gate refuses an empty validation rule set list" \
    live "fault-quarantine creates no ValidationRequest without a rule set" \
    --set-json "nvsentinel:fault-quarantine.validation.ruleSets=[]"
  expect_refusal "#1 gate refuses a rule set naming a test lifecycle-manager does not define" \
    live "requests the test \"${UNDEFINED_TEST}\", which lifecycle-manager's config does not define" \
    --set-json "nvsentinel:fault-quarantine.validation.ruleSets=${undefined}"
  expect_refusal "#1 gate refuses a disabled rule set sharing the post-reboot rule set's name" \
    live "has the name \"Post-reboot validation\"" \
    --set-json "nvsentinel:fault-quarantine.validation.ruleSets=${shadowed}"
  # lifecycle-manager writes the value unquoted into every test Job, where
  # "true" is a bool the API server rejects.
  expect_refusal "#1 gate refuses a release taint whose value breaks every test Job" \
    live "which lifecycle-manager's Job template writes unquoted" \
    --set-json 'nvsentinel:lifecycle-manager.config.schedulingGate.taints=[{"key":"example.com/hold","value":"true","effect":"NoSchedule","remove":true}]'

  bundle_to live deploy ||
    { tail -20 "${WORK}/bundle-deploy.log"; err "bundle generation failed for the full composition"; }
  pass "#1 gate accepts remediation, analyzer and node validation with the stubbed test"

  local bundle="${WORK}/bundle-deploy"
  BUNDLE_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-nvsentinel' -print -quit)
  CRDS_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-prometheus-operator-crds' -print -quit)
  CERT_MANAGER_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-cert-manager' -print -quit)
  PSMDB_OPERATOR_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-psmdb-operator' -print -quit)
  MONGODB_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-nvsentinel-mongodb' -print -quit)
  MONGODB_POST_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-nvsentinel-mongodb-post' -print -quit)
  [[ -n "${BUNDLE_DIR}" && -n "${CRDS_DIR}" && -n "${CERT_MANAGER_DIR}" ]] ||
    err "a bundled component directory is missing"
  [[ -n "${PSMDB_OPERATOR_DIR}" && -n "${MONGODB_DIR}" && -n "${MONGODB_POST_DIR}" ]] ||
    err "a bundled datastore component directory is missing"

  local values="${BUNDLE_DIR}/values.yaml" bundled
  bundled=$(yq -o=json '.["fault-quarantine"].validation.ruleSets' "${values}" | jq -c .)
  if [[ "$(yq '.["fault-quarantine"].validation.enabled' "${values}")" == "true" ]] &&
    jq -en --argjson a "${bundled}" --argjson b "${rule_sets}" '$a == $b' >/dev/null &&
    jq -e --arg t "${POST_REBOOT_TEST}" 'any(.[]; .name == "Post-reboot validation" and .tests == [$t])' \
      <<<"${bundled}" >/dev/null; then
    pass "#1 the bundle enables fault-quarantine.validation with the mixin's post-reboot rule set"
  else
    fail "#1 the bundle carries post-reboot validation" \
      "enabled $(yq '.["fault-quarantine"].validation.enabled' "${values}"), ruleSets ${bundled}"
  fi

  # Fail here, not later as "nothing happened": a composition or test-only
  # setting that did not reach the values would otherwise read as a
  # controller that never acted.
  [[ "$(yq '.global.dryRun' "${values}")" == "false" ]] ||
    err "bundle's nvsentinel values are not live (global.dryRun != false)"
  [[ "$(yq '.global.lifecycleManager.enabled' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not enable lifecycle-manager -- composition regressed"
  [[ "$(yq '.["lifecycle-manager"].config | has("newNodeValidation")' "${values}")" == "true" &&
    "$(yq '.["lifecycle-manager"].config.newNodeValidation' "${values}")" == "null" ]] ||
    err "newNodeValidation=null did not reach the bundle, so new-node validation would run beside the cases"
  [[ "$(yq ".[\"lifecycle-manager\"].config.tests[\"${POST_REBOOT_TEST}\"].image" "${values}")" == "${BUSYBOX_IMAGE}" ]] ||
    err "the ${POST_REBOOT_TEST} stub did not reach the bundle"
  [[ "$(yq '.["janitor-provider"].csp.provider' "${values}")" == "kind" ]] ||
    err "janitor-provider is not on the simulated kind provider"
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

deployment_scaled_down() {
  kc -n "${NS}" get deployment "$1" -o json | jq -e '(.status.replicas // 0) == 0' >/dev/null
}

install_bundle() {
  msg "Installing prometheus-operator-crds, cert-manager, the Percona datastore and nvsentinel from the bundle..."
  local dir d deadline remaining
  # CRDs first: cert-manager's chart renders a ServiceMonitor.
  for dir in "${CRDS_DIR}" "${CERT_MANAGER_DIR}"; do
    install_component "${dir}"
  done
  # The Percona operator and lifecycle-manager's webhook both take their
  # certificates from cert-manager.
  deadline=$((SECONDS + CERT_MANAGER_WAIT_SECONDS))
  for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
    wait_rollout deployment "${d}" cert-manager "${deadline}"
  done

  install_component "${PSMDB_OPERATOR_DIR}"
  deadline=$((SECONDS + MONGODB_WAIT_SECONDS))
  wait_rollout deployment psmdb-operator "${NS}" "${deadline}"
  install_component "${MONGODB_DIR}"
  install_component "${MONGODB_POST_DIR}"
  remaining=$((deadline - SECONDS))
  ((remaining > 0)) || err "nvsentinel-mongodb never became ready"
  kc -n "${NS}" wait --for=jsonpath='{.status.state}'=ready \
    perconaservermongodb/nvsentinel-mongodb --timeout="${remaining}s" ||
    err "nvsentinel-mongodb never became ready"
  remaining=$((deadline - SECONDS))
  ((remaining > 0)) || err "nvsentinel's MongoDB client certificate was never issued"
  kc -n "${NS}" wait --for=condition=Ready certificate/nvsentinel-mongodb-app-client \
    --timeout="${remaining}s" || err "nvsentinel's MongoDB client certificate was never issued"

  install_component "${BUNDLE_DIR}"

  deadline=$((SECONDS + NVSENTINEL_WAIT_SECONDS))
  for d in fault-quarantine node-drainer fault-remediation health-events-analyzer lifecycle-manager janitor janitor-provider; do
    wait_rollout deployment "${d}" "${NS}" "${deadline}"
  done
  wait_rollout daemonset platform-connectors "${NS}" "${deadline}"
  kc get crd "${VR_RESOURCE}" >/dev/null || err "the ValidationRequest CRD was not installed"
  kc get crd "${REBOOT_RESOURCE}" >/dev/null || err "the RebootNode CRD was not installed"
  # Helm drops a null subchart value's chart default; if it ever keeps it,
  # new-node validation would run beside the cases.
  [[ "$(kc -n "${NS}" get configmap lifecycle-manager -o jsonpath='{.data.config\.yaml}' |
    yq '.spec | has("newNodeValidation")')" == "false" ]] ||
    err "lifecycle-manager's config still carries newNodeValidation -- newNodeValidation=null did not take"
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
  kc -n "${NS}" create configmap e2e-health-event-proto \
    --from-file=health_event.proto="${WORK}/health_event.proto" --dry-run=client -o yaml |
    kc apply -f - >/dev/null
}

# ── Health events ──

# health_event prints one HealthEvents request for GPU $1: a fatal XID with
# recommended action $2, or, when $2 is "healthy", its recovery. The two share
# agent, check and entities, which is how fault-quarantine pairs them.
health_event() {
  local gpu="$1" action="$2"
  jq -cn --arg node "${NODE}" --arg gpu "${gpu}" --arg uuid "${GPU_UUID_PREFIX}$(printf '%02d' "${gpu}")" \
    --arg action "${action}" --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '
    ($action == "healthy") as $healthy | {
      version: 1,
      events: [{
        version: 1,
        agent: "gpu-health-monitor",
        componentClass: "GPU",
        checkName: "GpuXidError",
        isFatal: ($healthy | not),
        isHealthy: $healthy,
        message: (if $healthy then "aicr e2e synthetic recovery" else "aicr e2e synthetic XID 79" end),
        recommendedAction: (if $healthy then "NONE" else $action end),
        errorCode: (if $healthy then [] else ["79"] end),
        entitiesImpacted: [{entityType: "GPU", entityValue: $gpu}, {entityType: "GPU_UUID", entityValue: $uuid}],
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
  kc -n "${NS}" delete pod "${name}" --ignore-not-found >/dev/null
  jq -n --arg name "${name}" --arg ns "${NS}" --arg node "${NODE}" --arg image "${GRPCURL_IMAGE}" --arg data "${payload}" '{
    apiVersion: "v1", kind: "Pod",
    metadata: {name: $name, namespace: $ns},
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
  if ! kc -n "${NS}" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/${name}" \
    --timeout="${EVENT_WAIT_SECONDS}s" >/dev/null; then
    kc -n "${NS}" logs "pod/${name}" 2>&1 | tail -20 >&2 || true
    err "event ${name} was not accepted by platform-connectors"
  fi
}

# log_has succeeds when deployment $1 has logged, since $2, a line containing
# both $3 and the node's name.
log_has() {
  kc -n "${NS}" logs "deployment/$1" --since-time="$2" 2>/dev/null | grep -F -- "$3" | grep -qF -- "${NODE}"
}

# ── Node helpers ──

node_json() { kc get node "${NODE}" -o json; }

# node_has succeeds when jq, given the remaining arguments, finds the node
# matches.
node_has() { node_json | jq -e "$@" >/dev/null 2>&1; }

node_cordoned() { node_has '.spec.unschedulable == true'; }
node_schedulable() { node_has '(.spec.unschedulable // false) == false'; }
node_label_is() { node_has --arg k "$1" --arg v "$2" '.metadata.labels[$k] == $v'; }
node_label_absent() { node_has --arg k "$1" '.metadata.labels | has($k) | not'; }
node_annotated() { node_has --arg k "$1" '.metadata.annotations | has($k)'; }
# The absent forms are their own reads, not negations: a failed read fails
# them rather than reading as "absent".
node_annotation_absent() { node_has --arg k "$1" '(.metadata.annotations // {}) | has($k) | not'; }
node_condition_absent() { node_has --arg t "$1" 'all((.status.conditions // [])[]; .type != $t)'; }

# node_quarantine_cleared succeeds when the node carries none of
# fault-quarantine's active quarantine annotations (quarantineHealthEvent*,
# quarantineValidationHealthEvent). The quarantinedNode*Manually annotations
# a manual uncordon leaves are history, not an active quarantine.
node_quarantine_cleared() {
  node_has --arg v "${VALIDATION_ANNOTATION}" '(.metadata.annotations // {}) | keys |
    all(.[]; (startswith("quarantineHealthEvent") or . == $v) | not)'
}

# node_quarantine_cancelled succeeds once a manual uncordon has cancelled the
# quarantine: fault-quarantine removes the annotation lifecycle-manager's
# not-under-quarantine criterion reads, and the validation request it would
# have created, and records the uncordon in its own
# quarantinedNodeUncordonedManually annotation, which no criterion reads. One
# read, so a failed read fails rather than reading as "absent".
node_quarantine_cancelled() {
  node_has --arg q "${QUARANTINE_ANNOTATION}" --arg v "${VALIDATION_ANNOTATION}" \
    '((.spec.unschedulable // false) == false) and
     ((.metadata.annotations // {}) | (has($q) or has($v)) | not)'
}

node_quarantined() { node_cordoned && node_annotated "${QUARANTINE_ANNOTATION}"; }
node_held() {
  node_has --arg k "${HOLD_TAINT_KEY}" 'any(.spec.taints[]?; .key == $k and .effect == "NoSchedule")'
}
# fault-remediation moves the state label on from drain-succeeded as soon as
# it acts, so any later state also shows the drain finished.
node_drained() {
  node_has --arg k "${STATE_LABEL}" \
    '.metadata.labels[$k] | IN("drain-succeeded", "remediating", "remediation-succeeded")'
}

# validation_requested succeeds when the node's validation annotation records
# test $1 for a fault.
validation_requested() {
  node_has --arg k "${VALIDATION_ANNOTATION}" --arg t "$1" \
    '.metadata.annotations[$k] | fromjson | any(.[]; any((.tests // [])[]; . == $t))'
}

node_released_validated() {
  node_schedulable && node_label_is "${UNCORDON_REASON_LABEL}" ValidationSucceeded &&
    node_label_absent "${VALIDATION_STATE_LABEL}"
}

clear_uncordon_reason() {
  kc label node "${NODE}" "${UNCORDON_REASON_LABEL}-" >/dev/null 2>&1 || true
}

node_summary() {
  node_json | jq -c '{unschedulable: .spec.unschedulable, labels: .metadata.labels,
    annotations: ((.metadata.annotations // {}) | keys)}' | cut -c1-1200
}

# node_ready_for_case fails case $1 unless the node is schedulable and
# carries no quarantine, pipeline or validation state from an earlier case;
# node_drained relies on the state label starting absent.
node_ready_for_case() {
  if node_schedulable && node_quarantine_cleared && node_label_absent "${STATE_LABEL}" &&
    node_label_absent "${VALIDATION_STATE_LABEL}"; then
    clear_uncordon_reason
    return 0
  fi
  fail "$1" "precondition: the node still carries an earlier case's quarantine or validation: $(node_summary)"
  return 1
}

# quarantine_and_drain sends fault event $2 (GPU $3, action $4) for case $1,
# and waits for fault-quarantine to quarantine the node and node-drainer to
# finish draining it.
quarantine_and_drain() {
  local tc="$1" name="$2" gpu="$3" action="$4"
  send_event "${name}" "$(health_event "${gpu}" "${action}")"
  if ! poll "${QUARANTINE_WAIT_SECONDS}" node_quarantined; then
    fail "${tc} fault-quarantine quarantines the node for ${action}" \
      "no cordon and ${QUARANTINE_ANNOTATION} within ${QUARANTINE_WAIT_SECONDS}s: $(node_summary)"
    return 1
  fi
  pass "${tc} fault-quarantine cordons the node and records the ${action} fault"
  if ! poll "${DRAIN_WAIT_SECONDS}" node_drained; then
    fail "${tc} node-drainer drains the node" \
      "${STATE_LABEL} does not reach drain-succeeded within ${DRAIN_WAIT_SECONDS}s: $(node_summary)"
    return 1
  fi
  pass "${tc} node-drainer drains the node (${STATE_LABEL} reached drain-succeeded)"
}

# ── Repair helpers ──

# reboot_requests prints, as a JSON list oldest first, the RebootNodes for the
# node. A kubectl error fails rather than reading as "none".
reboot_requests() {
  local json
  json=$(kc get "${REBOOT_RESOURCE}" -o json) || return 1
  jq -c --arg n "${NODE}" '[.items[] | select(.spec.nodeName == $n)] |
    sort_by(.metadata.creationTimestamp) | map(.metadata.name)' <<<"${json}"
}

# new_reboot prints the newest RebootNode for the node not in the JSON list $1.
new_reboot() {
  local all
  all=$(reboot_requests) || return 1
  jq -r --argjson seen "$1" '[.[] | select(. as $r | any($seen[]; . == $r) | not)] | last // empty' <<<"${all}"
}

new_reboot_found() { [[ -n "$(new_reboot "$1")" ]]; }

reboot_completed() {
  kc get "${REBOOT_RESOURCE}" "$1" -o json |
    jq -e 'any(.status.conditions[]?; .type == "NodeReady" and .status == "True")' >/dev/null
}

# wait_for_repair waits, for case $1, for fault-remediation to request a
# reboot of the node that is not in the JSON list $2, and for janitor to
# complete it through the simulated provider.
wait_for_repair() {
  local tc="$1" baseline="$2" reboot
  if ! poll "${REPAIR_WAIT_SECONDS}" new_reboot_found "${baseline}"; then
    fail "${tc} fault-remediation requests a reboot" \
      "no new RebootNode for ${NODE} within ${REPAIR_WAIT_SECONDS}s: $(node_summary)"
    return 1
  fi
  reboot=$(new_reboot "${baseline}") || true
  pass "${tc} fault-remediation requests reboot ${reboot}"
  if ! poll "${REPAIR_WAIT_SECONDS}" reboot_completed "${reboot}"; then
    fail "${tc} janitor completes the reboot" \
      "${reboot} status: $(kc get "${REBOOT_RESOURCE}" "${reboot}" -o json | jq -c '.status' | cut -c1-600)"
    return 1
  fi
  pass "${tc} janitor completes ${reboot} (NodeReady=True)"
}

# ── ValidationRequest helpers ──

# fq_vrs prints, as a JSON list oldest first, the ValidationRequests
# fault-quarantine created for the node. A kubectl error fails rather than
# reading as "none".
fq_vrs() {
  local json
  json=$(kc get "${VR_RESOURCE}" -l "${CREATED_BY_LABEL}=fault-quarantine" -o json) || return 1
  jq -c --arg n "${NODE}" '[.items[] | select(any(.spec.nodes[]; .name == $n))] |
    sort_by(.metadata.creationTimestamp) | map(.metadata.name)' <<<"${json}"
}

# new_fq_vr prints the newest fault-quarantine ValidationRequest for the node
# that is not in the JSON list $1.
new_fq_vr() {
  local all
  all=$(fq_vrs) || return 1
  jq -r --argjson seen "$1" '[.[] | select(. as $n | any($seen[]; . == $n) | not)] | last // empty' <<<"${all}"
}

new_fq_vr_found() { [[ -n "$(new_fq_vr "$1")" ]]; }

vr_phase() { kc get "${VR_RESOURCE}" "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }

vr_terminal() {
  case "$(vr_phase "$1")" in
    Succeeded | Failed) return 0 ;;
  esac
  return 1
}

vr_tests_are() {
  kc get "${VR_RESOURCE}" "$1" -o json | jq -e --argjson want "$2" '.spec.tests == $want' >/dev/null
}

vr_status() { kc get "${VR_RESOURCE}" "$1" -o json | jq -c '.status' | cut -c1-600; }

# vr_job_found succeeds when lifecycle-manager has started a test Job for
# request $1 in the namespace the stub's service link is read from. The Job is
# matched by its controller reference, not its name: lifecycle-manager
# truncates Job names, so a long request name is not a prefix of its Job's.
vr_job_found() {
  local uid
  uid=$(kc get "${VR_RESOURCE}" "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null) && [[ -n "${uid}" ]] || return 1
  kc -n "${NS}" get jobs -o json |
    jq -e --arg uid "${uid}" 'any(.items[]; any(.metadata.ownerReferences[]?; .uid == $uid))' >/dev/null
}

# vr_manifest prints a ValidationRequest named $1 that runs the post-reboot
# test on the node.
vr_manifest() {
  jq -n --arg name "$1" --arg node "${NODE}" --arg test "${POST_REBOOT_TEST}" '{
    apiVersion: "nvsentinel.nvidia.com/v1alpha1", kind: "ValidationRequest",
    metadata: {name: $name},
    spec: {nodes: [{name: $node}], tests: [$test]}
  }'
}

webhook_admits() { vr_manifest "${PROBE_VR}" | kc create --dry-run=server -f - >/dev/null 2>&1; }
webhook_refuses() { ! webhook_admits; }

# create_vr creates request $1 for the post-reboot test on the node.
create_vr() {
  # A create the API server applied but whose response was lost must not fail
  # the retry with AlreadyExists.
  kc get "${VR_RESOURCE}" "$1" >/dev/null 2>&1 && return 0
  vr_manifest "$1" | kc create -f - >/dev/null 2>"${WORK}/vr-create.err"
}

# wait_validation_passes waits for request $2 to succeed and the node to be
# released by lifecycle-manager, reporting under case $1.
wait_validation_passes() {
  local tc="$1" vr="$2"
  if ! poll "${VALIDATION_WAIT_SECONDS}" vr_terminal "${vr}" || [[ "$(vr_phase "${vr}")" != "Succeeded" ]]; then
    fail "${tc} the validation succeeds" "${vr} status: $(vr_status "${vr}")"
    return 1
  fi
  pass "${tc} the validation succeeds (${vr})"
  if poll "${RELEASE_WAIT_SECONDS}" node_released_validated; then
    pass "${tc} the pass uncordons the node with ${UNCORDON_REASON_LABEL}=ValidationSucceeded"
  else
    fail "${tc} the pass uncordons the node" "$(node_summary)"
    return 1
  fi
}

# ── Tests ──

# vr_running succeeds when request $1 has not reached a terminal phase. A
# failed read fails.
vr_running() {
  local phase
  phase=$(vr_phase "$1") || return 1
  [[ "${phase}" != "Succeeded" && "${phase}" != "Failed" ]]
}

test_post_reboot_validation() {
  local tc="#2" baseline reboots vr
  msg "Case 2: a repaired RESTART_VM fault is validated before the node returns..."
  node_ready_for_case "${tc} post-reboot validation" || return 0
  baseline=$(fq_vrs) || err "could not list ValidationRequests"
  reboots=$(reboot_requests) || err "could not list RebootNodes"

  quarantine_and_drain "${tc}" e2e-prv-restart-vm 1 RESTART_VM || return 0
  if validation_requested "${POST_REBOOT_TEST}"; then
    pass "${tc} the post-reboot rule set records ${POST_REBOOT_TEST} in ${VALIDATION_ANNOTATION}"
  else
    fail "${tc} the post-reboot rule set matches RESTART_VM" \
      "${VALIDATION_ANNOTATION} does not name ${POST_REBOOT_TEST}: $(node_summary)"
  fi
  wait_for_repair "${tc}" "${reboots}" || return 0

  send_event e2e-prv-restart-vm-healthy "$(health_event 1 healthy)"
  if ! poll "${VR_APPEAR_WAIT_SECONDS}" new_fq_vr_found "${baseline}"; then
    fail "${tc} the healthy event creates a ValidationRequest" \
      "none labelled ${CREATED_BY_LABEL}=fault-quarantine names ${NODE} within ${VR_APPEAR_WAIT_SECONDS}s: $(node_summary)"
    return 0
  fi
  vr=$(new_fq_vr "${baseline}") || true
  pass "${tc} the healthy event creates ValidationRequest ${vr} labelled ${CREATED_BY_LABEL}=fault-quarantine"

  if poll "${RELEASE_WAIT_SECONDS}" node_quarantine_cleared; then
    pass "${tc} fault-quarantine hands the node over: its quarantine annotations are removed"
  else
    fail "${tc} fault-quarantine hands the node over" "quarantine annotations remain: $(node_summary)"
  fi
  # fault-quarantine creates the request before it hands the node over, so the
  # hold is checked after the hand-over, while the request still runs.
  if node_cordoned && vr_running "${vr}"; then
    pass "${tc} the node stays cordoned after the hand-over while its validation runs"
  else
    fail "${tc} the node stays cordoned after the hand-over while its validation runs" \
      "phase $(vr_phase "${vr}"), unschedulable $(node_json | jq -c '.spec.unschedulable')"
  fi
  if vr_tests_are "${vr}" "[\"${POST_REBOOT_TEST}\"]"; then
    pass "${tc} the request runs ${POST_REBOOT_TEST}"
  else
    fail "${tc} the request runs ${POST_REBOOT_TEST}" "spec.tests is $(kc get "${VR_RESOURCE}" "${vr}" -o jsonpath='{.spec.tests}')"
  fi
  # Case 3's failure switch is read from this namespace's service links.
  if poll "${JOB_APPEAR_WAIT_SECONDS}" vr_job_found "${vr}"; then
    pass "${tc} lifecycle-manager runs the test as a Job in ${NS}"
  else
    fail "${tc} lifecycle-manager runs the test as a Job in ${NS}" "no Job for ${vr} in ${NS} within ${JOB_APPEAR_WAIT_SECONDS}s"
  fi

  wait_validation_passes "${tc}" "${vr}" || return 0

  local lm_vrs
  if ! lm_vrs=$(kc get "${VR_RESOURCE}" -l "${CREATED_BY_LABEL}=lifecycle-manager" -o name); then
    fail "${tc} new-node validation stays off" "could not list ValidationRequests"
  elif [[ -n "${lm_vrs}" ]] || ! node_condition_absent "${NEW_NODE_CONDITION}"; then
    fail "${tc} new-node validation stays off" \
      "lifecycle-manager acted on the node as a new one (requests: ${lm_vrs:-none}), or the node could not be read -- newNodeValidation=null did not take"
  else
    pass "${tc} new-node validation stays off, so only fault-quarantine requested validation"
  fi
}

delete_fail_service() {
  kc -n "${NS}" delete service "${FAIL_SERVICE}" --ignore-not-found >/dev/null || true
}

test_failed_validation_holds_node() {
  local tc="#3" baseline reboots vr
  msg "Case 3: a failed validation holds the node; a passing retest releases it..."
  node_ready_for_case "${tc} failed validation" || return 0
  baseline=$(fq_vrs) || err "could not list ValidationRequests"
  reboots=$(reboot_requests) || err "could not list RebootNodes"

  kc -n "${NS}" create service clusterip "${FAIL_SERVICE}" --tcp=80:80 >/dev/null ||
    err "could not create Service ${FAIL_SERVICE}, which fails the stub"
  if ! quarantine_and_drain "${tc}" e2e-prv-restart-bm 4 RESTART_BM; then
    delete_fail_service
    return 0
  fi
  if validation_requested "${POST_REBOOT_TEST}"; then
    pass "${tc} the post-reboot rule set also matches RESTART_BM"
  else
    fail "${tc} the post-reboot rule set matches RESTART_BM" "$(node_summary)"
  fi
  if ! wait_for_repair "${tc}" "${reboots}"; then
    delete_fail_service
    return 0
  fi

  send_event e2e-prv-restart-bm-healthy "$(health_event 4 healthy)"
  if ! poll "${VR_APPEAR_WAIT_SECONDS}" new_fq_vr_found "${baseline}"; then
    fail "${tc} the healthy event creates a ValidationRequest" "none within ${VR_APPEAR_WAIT_SECONDS}s: $(node_summary)"
    delete_fail_service
    return 0
  fi
  vr=$(new_fq_vr "${baseline}") || true

  if poll "${VALIDATION_WAIT_SECONDS}" vr_terminal "${vr}" && [[ "$(vr_phase "${vr}")" == "Failed" ]] &&
    kc get "${VR_RESOURCE}" "${vr}" -o json |
    jq -e '.status.testGroups[0].attempts[-1].failureReason == "TestFailed"' >/dev/null; then
    pass "${tc} the failing test fails ${vr} with TestFailed"
  else
    fail "${tc} the failing test fails the request" \
      "${vr} status: $(vr_status "${vr}") -- if it Succeeded, the stub never saw ${FAIL_ENV}"
  fi
  delete_fail_service
  if poll "${RELEASE_WAIT_SECONDS}" node_label_is "${VALIDATION_STATE_LABEL}" validation-failed; then
    pass "${tc} the node is labelled ${VALIDATION_STATE_LABEL}=validation-failed"
  else
    fail "${tc} validation-failed label" "$(node_summary)"
  fi
  sleep "${SETTLE_SECONDS}"
  if node_cordoned && node_quarantine_cleared; then
    pass "${tc} the failed validation keeps the node cordoned, with nothing left for fault-quarantine to release"
  else
    fail "${tc} the failed validation keeps the node cordoned" "$(node_summary)"
  fi

  # The webhook's CA bundle is injected by cert-manager after rollout.
  if ! poll "${WEBHOOK_WAIT_SECONDS}" create_vr "${RETEST_VR}"; then
    fail "${tc} a retest is admitted" "$(cat "${WORK}/vr-create.err")"
    return 0
  fi
  # lifecycle-manager lets a passing request with the same tests clear a
  # failed one from the node's validation session.
  wait_validation_passes "${tc} retest:" "${RETEST_VR}" || true
}

restore_lifecycle_manager() {
  local deadline=$((SECONDS + LIFECYCLE_MANAGER_WAIT_SECONDS))
  kc -n "${NS}" scale deployment/lifecycle-manager --replicas=1 >/dev/null ||
    err "could not scale lifecycle-manager back to 1"
  wait_rollout deployment lifecycle-manager "${NS}" "${deadline}"
  poll "${WEBHOOK_WAIT_SECONDS}" webhook_admits
}

# stop_lifecycle_manager scales lifecycle-manager to 0 and waits until its
# webhook refuses requests, failing check $1 and restoring it otherwise.
stop_lifecycle_manager() {
  kc -n "${NS}" scale deployment/lifecycle-manager --replicas=0 >/dev/null ||
    err "could not scale lifecycle-manager to 0"
  if ! poll "${SCALE_WAIT_SECONDS}" deployment_scaled_down lifecycle-manager ||
    ! poll "${WEBHOOK_WAIT_SECONDS}" webhook_refuses; then
    fail "$1" "lifecycle-manager's webhook still admits requests after scaling it to 0"
    restore_lifecycle_manager || true
    return 1
  fi
}

# strand_node, for case $1 with lifecycle-manager down, quarantines, drains
# and repairs the node for event $2 on GPU $3, then sends the healthy event,
# which fault-quarantine cannot turn into a request. It restores
# lifecycle-manager on every failure.
strand_node() {
  local tc="$1" name="$2" gpu="$3" reboots since
  reboots=$(reboot_requests) || err "could not list RebootNodes"
  stop_lifecycle_manager "${tc} lifecycle-manager is stopped" || return 1
  if ! quarantine_and_drain "${tc}" "${name}" "${gpu}" RESTART_VM || ! wait_for_repair "${tc}" "${reboots}"; then
    restore_lifecycle_manager || true
    return 1
  fi
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  send_event "${name}-healthy" "$(health_event "${gpu}" healthy)"
  if poll "${LOG_WAIT_SECONDS}" log_has fault-quarantine "${since}" "${CREATE_FAILED_LOG}"; then
    pass "${tc} fault-quarantine logs that it could not create the request"
    return 0
  fi
  fail "${tc} fault-quarantine logs the creation failure" \
    "no '${CREATE_FAILED_LOG}' for ${NODE} within ${LOG_WAIT_SECONDS}s"
  restore_lifecycle_manager || true
  return 1
}

test_creation_failure_keeps_quarantine() {
  local tc="#4" baseline vr
  msg "Case 4: a request that cannot be created leaves the node quarantined..."
  node_ready_for_case "${tc} creation failure" || return 0
  baseline=$(fq_vrs) || err "could not list ValidationRequests"

  strand_node "${tc}" e2e-prv-no-webhook 5 || return 0
  sleep "${SETTLE_SECONDS}"
  if ! vr=$(new_fq_vr "${baseline}"); then
    fail "${tc} the node stays quarantined" "could not list ValidationRequests"
  elif [[ -z "${vr}" ]] && node_quarantined && node_annotated "${VALIDATION_ANNOTATION}"; then
    pass "${tc} the node stays cordoned with its quarantine and validation annotations"
  else
    fail "${tc} the node stays quarantined" "request ${vr:-none}; $(node_summary)"
  fi

  if ! restore_lifecycle_manager; then
    fail "${tc} lifecycle-manager returns" "its webhook does not admit requests ${WEBHOOK_WAIT_SECONDS}s after rollout"
    return 0
  fi
  # A repeat of the healthy event retries the creation; case 5 pins that
  # fault-quarantine does not retry on its own.
  send_event e2e-prv-no-webhook-healthy-again "$(health_event 5 healthy)"
  if ! poll "${VR_APPEAR_WAIT_SECONDS}" new_fq_vr_found "${baseline}"; then
    fail "${tc} the request is created once lifecycle-manager is back" \
      "none within ${VR_APPEAR_WAIT_SECONDS}s: $(node_summary)"
    return 0
  fi
  vr=$(new_fq_vr "${baseline}") || true
  pass "${tc} the request is created once lifecycle-manager is back (${vr})"
  wait_validation_passes "${tc}" "${vr}" || true
}

# test_stranded_node_recovery runs the documented recovery for a node whose
# request could not be created. It first pins that fault-quarantine does not
# retry once lifecycle-manager is back, so the recovery acts on a node that
# is really stranded. Then: taint it NoSchedule so it takes no pods while
# uncordoned, uncordon it, wait until fault-quarantine has removed its
# annotations, the one not-under-quarantine reads among them, cordon it, drop
# the taint and request the test by hand.
test_stranded_node_recovery() {
  local tc="#5" baseline vr
  msg "Case 5: an operator recovers a node whose request could not be created..."
  node_ready_for_case "${tc} stranded node recovery" || return 0
  baseline=$(fq_vrs) || err "could not list ValidationRequests"

  strand_node "${tc}" e2e-prv-recovery 6 || return 0
  if ! restore_lifecycle_manager; then
    fail "${tc} lifecycle-manager returns" "its webhook does not admit requests ${WEBHOOK_WAIT_SECONDS}s after rollout"
    return 0
  fi
  sleep "${SETTLE_SECONDS}"
  if ! vr=$(new_fq_vr "${baseline}"); then
    fail "${tc} fault-quarantine does not retry on its own" "could not list ValidationRequests"
    return 0
  elif [[ -n "${vr}" ]] || ! node_quarantined; then
    fail "${tc} fault-quarantine does not retry on its own" "request ${vr:-none}; $(node_summary)"
    return 0
  fi
  pass "${tc} with lifecycle-manager back, fault-quarantine does not retry on its own: no request, the node stays quarantined"

  kc taint node "${NODE}" "${HOLD_TAINT_KEY}=true:NoSchedule" --overwrite >/dev/null ||
    err "could not taint ${NODE}"
  kc uncordon "${NODE}" >/dev/null || err "could not uncordon ${NODE}"
  if poll "${RELEASE_WAIT_SECONDS}" node_quarantine_cancelled; then
    pass "${tc} kubectl uncordon cancels the quarantine and clears ${QUARANTINE_ANNOTATION} and ${VALIDATION_ANNOTATION}"
  else
    fail "${tc} kubectl uncordon cancels the quarantine" "$(node_summary)"
    kc taint node "${NODE}" "${HOLD_TAINT_KEY}:NoSchedule-" >/dev/null 2>&1 || true
    return 0
  fi
  if node_held; then
    pass "${tc} the hold taint keeps the uncordoned node closed to new pods"
  else
    fail "${tc} the hold taint survives the uncordon" "$(node_summary)"
  fi
  clear_uncordon_reason
  kc cordon "${NODE}" >/dev/null || err "could not cordon ${NODE}"
  kc taint node "${NODE}" "${HOLD_TAINT_KEY}:NoSchedule-" >/dev/null || err "could not remove the hold taint"
  if ! poll "${WEBHOOK_WAIT_SECONDS}" create_vr "${RECOVERY_VR}"; then
    fail "${tc} the operator's request is admitted" "$(cat "${WORK}/vr-create.err")"
    return 0
  fi
  wait_validation_passes "${tc} recovery:" "${RECOVERY_VR}" || true
}

# test_component_reset_not_validated runs last: it leaves fault-remediation at
# 0, since a GPU reset cannot run on Kind (see the header).
test_component_reset_not_validated() {
  local tc="#6" baseline vr reason
  msg "Case 6: a COMPONENT_RESET fault is released without validation..."
  node_ready_for_case "${tc} COMPONENT_RESET is not validated" || return 0
  baseline=$(fq_vrs) || err "could not list ValidationRequests"
  kc -n "${NS}" scale deployment/fault-remediation --replicas=0 >/dev/null ||
    err "could not scale fault-remediation to 0"
  poll "${SCALE_WAIT_SECONDS}" deployment_scaled_down fault-remediation ||
    err "fault-remediation still has pods ${SCALE_WAIT_SECONDS}s after scaling to 0"

  quarantine_and_drain "${tc}" e2e-prv-reset 2 COMPONENT_RESET || return 0
  if node_annotation_absent "${VALIDATION_ANNOTATION}"; then
    pass "${tc} no rule set matches COMPONENT_RESET: the node carries no ${VALIDATION_ANNOTATION}"
  else
    fail "${tc} no rule set matches COMPONENT_RESET" \
      "the node carries ${VALIDATION_ANNOTATION}, or could not be read: $(node_summary)"
  fi

  send_event e2e-prv-reset-healthy "$(health_event 2 healthy)"
  # fault-quarantine creates any request before it uncordons, so once the
  # node is released the absence of one is final.
  if ! poll "${RELEASE_WAIT_SECONDS}" node_schedulable; then
    fail "${tc} the healthy event releases the node" "still cordoned after ${RELEASE_WAIT_SECONDS}s: $(node_summary)"
    return 0
  fi
  if ! vr=$(new_fq_vr "${baseline}"); then
    fail "${tc} no ValidationRequest is created" "could not list ValidationRequests"
  elif [[ -n "${vr}" ]]; then
    fail "${tc} no ValidationRequest is created" "fault-quarantine created ${vr}"
  else
    pass "${tc} no ValidationRequest is created"
  fi
  reason=$(node_json | jq -r --arg k "${UNCORDON_REASON_LABEL}" '.metadata.labels[$k] // ""')
  if [[ "${reason}" != "ValidationSucceeded" ]] && node_quarantine_cleared; then
    pass "${tc} fault-quarantine releases the node itself (${UNCORDON_REASON_LABEL}=${reason:-<unset>})"
  else
    fail "${tc} fault-quarantine releases the node itself" "$(node_summary)"
  fi
}

# ── Main ──

msg "=========================================="
msg "nvsentinel post-remediation validation e2e"
msg "=========================================="

has_tools go kind kubectl helm yq jq curl
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
test_gate
ensure_cluster
install_bundle
publish_proto
test_post_reboot_validation
test_failed_validation_holds_node
test_creation_failure_keeps_quarantine
test_stranded_node_recovery
test_component_reset_not_validated

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel post-remediation validation e2e: all tests passed"
