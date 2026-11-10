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
# nvsentinel-csp-health-monitor mixins e2e
# =============================================================================
#
# Bundles the EKS H100 leaf with the observe step and the
# nvsentinel-csp-health-monitor-aws mixin, installs the datastore and
# nvsentinel through the bundle's own install.sh on Kind, and checks:
#
#   - the bundle gate refuses the mixin without the per-cluster values, with
#     an accountId --set parsed as a number, on a recipe of another service,
#     and with processingStrategy EXECUTE_REMEDIATION;
#   - the monitor rolls out and keeps running without a cloud identity, as
#     docs/user/component-catalog.md#cloud-maintenance-events says;
#   - the ServiceAccount carries the IRSA role ARN built from the values, the
#     config selects aws, and the maintenance notifier runs STORE_ONLY;
#   - a maintenance event inside the lead time is published STORE_ONLY, its
#     completion is published too, and neither gives the node a
#     CSPMaintenance quarantine.
#
# EXECUTION SCENARIOS, run only with CSP_EXECUTION_SCENARIOS=true. The gate
# rejects EXECUTE_REMEDIATION because at NVSentinel v1.28.0 one maintenance
# notice can release a hold another notice still needs. These scenarios
# enable it anyway, by editing the bundle's values past the gate, on
# nvsentinel-quarantine, and assert the behavior execution needs: one notice
# cordons the node and its completion uncordons it; with two notices for the
# node, the first to complete leaves it cordoned; and a notice stored under
# STORE_ONLY that completes after the switch does not release a quarantine a
# newer notice set. The last two fail at v1.28.0. All must pass before the
# gate may allow EXECUTE_REMEDIATION.
#
# WHY THE EVENTS ARE INSERTED. Kind has no AWS Health API to poll, and the
# placeholder account holds no role, so the AWS poller never stores an event.
# The test writes one into the MaintenanceEvents collection the poller fills,
# with the fields the maintenance notifier reads, so everything downstream of
# the poller runs for real: the notifier's lead-time and post-maintenance
# queries, the platform connector, and fault-quarantine's CSPMaintenance rule.
#
# WHY THE EKS LEAF ON KIND. The gate accepts the aws mixin only on an eks
# recipe, and only the datastore and nvsentinel are installed from the
# bundle, so the EKS-specific components never reach the cluster.
#
# TEST-ONLY SETTINGS, passed as --set on the bundle: nvsentinel-mongodb at
# one member with smaller requests, so everything fits one runner node; for
# the execution scenarios' quarantine step, fault-quarantine's circuit breaker
# off, since cordoning Kind's only node trips it, and local-path-storage added
# to node-drainer's system namespaces, as in tests/e2e/nvsentinel-remediation.
#
# PREREQUISITES: go, kind, kubectl, helm, yq, jq, timeout
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-nvsentinel-csp-e2e).
#                  Always recreated, then deleted at the end unless
#                  KEEP_CLUSTER=true.
#   KEEP_CLUSTER   Skip cluster teardown on exit (default: false).
#   AICR_BIN       Path to a prebuilt aicr binary (default: built fresh).
#   CSP_EXECUTION_SCENARIOS
#                  Also run the execution scenarios (default: false).
# =============================================================================

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DIR}/../../.." && pwd)"
# shellcheck source=/dev/null
. "${ROOT}/tools/common"

CLUSTER_NAME="${CLUSTER_NAME:-aicr-nvsentinel-csp-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
CSP_EXECUTION_SCENARIOS="${CSP_EXECUTION_SCENARIOS:-false}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"

MONITOR="csp-health-monitor"
NODE="${CLUSTER_NAME}-control-plane"
MONGODB_POD="nvsentinel-mongodb-rs0-0"
DRAIN_EXTRA_NAMESPACES='^(local-path-storage)$'
# The notifier checks the datastore every maintenanceEventPollIntervalSeconds
# (60); each wait allows three checks.
EVENT_WAIT_SECONDS=180
CSP_CLUSTER_NAME="aicr-csp-e2e"
AWS_ACCOUNT_ID="000000000000"
AWS_REGION="us-east-1"
WANT_ROLE_ARN="arn:aws:iam::${AWS_ACCOUNT_ID}:role/${CSP_CLUSTER_NAME}-nvsentinel-health-monitor-assume-role-policy"
CERT_MANAGER_WAIT_SECONDS=600
MONGODB_WAIT_SECONDS=900
NVSENTINEL_WAIT_SECONDS=900
# How long the monitor must keep running without a cloud identity.
SETTLE_SECONDS=90

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
QUARANTINE_DIR=""
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

# compose_recipe writes ${WORK}/recipe-$1.yaml for service $2 from leaf $3,
# with remediation step $4 and the aws mixin composed and the remaining
# arguments passed to aicr recipe.
compose_recipe() {
  local name="$1" service="$2" leaf="$3" step="$4" data_dir="${WORK}/data-$1"
  shift 4
  mkdir -p "${data_dir}/overlays"
  cat >"${data_dir}/registry.yaml" <<'REGISTRY_EOF'
kind: ComponentRegistry
apiVersion: aicr.run/v1beta1
metadata:
  name: nvsentinel-csp-e2e-registry
components: []
REGISTRY_EOF
  STEP="${step}" yq '.spec.mixins += [strenv(STEP), "nvsentinel-csp-health-monitor-aws"]' \
    "${ROOT}/recipes/overlays/${leaf}.yaml" >"${data_dir}/overlays/${leaf}.yaml"
  "${AICR_BIN}" recipe --service "${service}" --accelerator h100 --intent training "$@" \
    --data "${data_dir}" --output "${WORK}/recipe-${name}.yaml" >/dev/null ||
    err "recipe generation failed for ${leaf}"
}

# bundle_recipe bundles ${WORK}/recipe-$1.yaml into ${WORK}/bundle-$2 with the
# remaining arguments, logging to ${WORK}/bundle-$2.log.
bundle_recipe() {
  local recipe="$1" out="$2"
  shift 2
  rm -rf "${WORK}/bundle-${out}"
  "${AICR_BIN}" bundle -r "${WORK}/recipe-${recipe}.yaml" -o "${WORK}/bundle-${out}" \
    --system-node-selector kubernetes.io/os=linux --accelerated-node-selector kubernetes.io/os=linux \
    --set "nvsentinelmongodb:unsafeFlags.replsetSize=true" \
    --set "nvsentinelmongodb:replsets.rs0.size=1" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.cpu=250m" \
    --set "nvsentinelmongodb:replsets.rs0.resources.requests.memory=512Mi" \
    "$@" >"${WORK}/bundle-${out}.log" 2>&1
}

# expect_refusal asserts that bundling with the remaining arguments fails and
# names $3.
expect_refusal() {
  local name="$1" recipe="$2" want="$3"
  shift 3
  if bundle_recipe "${recipe}" refused "$@"; then
    fail "${name}" "the bundle was accepted"
  elif grep -qF -- "${want}" "${WORK}/bundle-refused.log"; then
    pass "${name}"
  else
    fail "${name}" "refused without naming '${want}': $(grep -m1 'command failed' "${WORK}/bundle-refused.log" | cut -c1-300)"
  fi
}

test_gate_refusals() {
  msg "Checking the bundle gate..."
  local cluster_name=(--set "nvsentinel:${MONITOR}.configToml.clusterName=${CSP_CLUSTER_NAME}")
  local region=(--set "nvsentinel:${MONITOR}.configToml.aws.region=${AWS_REGION}")
  local account=(--set-json "nvsentinel:${MONITOR}.configToml.aws.accountId=\"${AWS_ACCOUNT_ID}\"")

  expect_refusal "gate refuses the aws mixin without per-cluster values" eks "configToml.clusterName is empty"
  expect_refusal "gate refuses an accountId --set as a number" eks "parses an all-digit value as a number" \
    "${cluster_name[@]}" "${region[@]}" --set "nvsentinel:${MONITOR}.configToml.aws.accountId=${AWS_ACCOUNT_ID}"

  expect_refusal "gate refuses EXECUTE_REMEDIATION" eks "but AICR supports only STORE_ONLY" \
    "${cluster_name[@]}" "${region[@]}" "${account[@]}" --set "nvsentinel:${MONITOR}.processingStrategy=EXECUTE_REMEDIATION"

  compose_recipe kind kind h100-kind-training nvsentinel-observe
  expect_refusal "gate refuses the aws mixin on a kind recipe" kind "but the recipe's service is \"kind\"" \
    "${cluster_name[@]}" "${region[@]}" "${account[@]}"

  bundle_recipe eks deploy "${cluster_name[@]}" "${region[@]}" "${account[@]}" ||
    { tail -20 "${WORK}/bundle-deploy.log"; err "bundle generation failed with every per-cluster value set"; }
  pass "gate accepts the aws mixin on the eks leaf with every per-cluster value"

  local bundle="${WORK}/bundle-deploy"
  BUNDLE_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-nvsentinel' -print -quit)
  CRDS_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-prometheus-operator-crds' -print -quit)
  CERT_MANAGER_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-cert-manager' -print -quit)
  PSMDB_OPERATOR_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-psmdb-operator' -print -quit)
  MONGODB_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-nvsentinel-mongodb' -print -quit)
  MONGODB_POST_DIR=$(find "${bundle}" -maxdepth 1 -type d -name '*-nvsentinel-mongodb-post' -print -quit)
  [[ -n "${BUNDLE_DIR}" && -n "${CRDS_DIR}" && -n "${CERT_MANAGER_DIR}" && -n "${PSMDB_OPERATOR_DIR}" &&
    -n "${MONGODB_DIR}" && -n "${MONGODB_POST_DIR}" ]] || err "a bundled component directory is missing"

  local values="${BUNDLE_DIR}/values.yaml"
  [[ "$(yq '.global.cspHealthMonitor.enabled' "${values}")" == "true" ]] ||
    err "bundle's nvsentinel values do not enable csp-health-monitor -- composition regressed"
  [[ "$(yq ".[\"${MONITOR}\"].processingStrategy" "${values}")" == "STORE_ONLY" ]] ||
    err "bundle's csp-health-monitor is not STORE_ONLY -- composition regressed"
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
  for d in fault-quarantine node-drainer fault-remediation; do
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

# ── Tests ──

test_monitor_runs_without_identity() {
  if ! kc -n nvsentinel rollout status "deployment/${MONITOR}" --timeout="${NVSENTINEL_WAIT_SECONDS}s"; then
    fail "${MONITOR} rolls out" "the Deployment never became Ready"
    return
  fi
  pass "${MONITOR} rolls out with both containers ready"

  # Compared with a baseline taken after the rollout: a restart while the
  # datastore was still bootstrapping is not a missing identity.
  local selector="app.kubernetes.io/name=${MONITOR}" counters baseline pods
  counters='[.items[] | {uid: .metadata.uid, restarts: [.status.containerStatuses[] | {name, restartCount}]}] | sort_by(.uid)'
  baseline=$(kc -n nvsentinel get pods -l "${selector}" -o json | jq -c "${counters}") ||
    { fail "${MONITOR} keeps running" "could not list its pods"; return; }
  msg "Watching ${MONITOR} for ${SETTLE_SECONDS}s without a cloud identity..."
  sleep "${SETTLE_SECONDS}"
  pods=$(kc -n nvsentinel get pods -l "${selector}" -o json) ||
    { fail "${MONITOR} keeps running" "could not list its pods"; return; }
  if [[ "$(jq -c "${counters}" <<<"${pods}")" == "${baseline}" ]] &&
    jq -e '(.items | length == 1) and all(.items[].status.containerStatuses[]; .ready)' <<<"${pods}" >/dev/null; then
    pass "${MONITOR} keeps running without a cloud identity (no new restarts in ${SETTLE_SECONDS}s)"
  else
    fail "${MONITOR} keeps running" "before: ${baseline}; after: $(jq -c "${counters}" <<<"${pods}")"
  fi
}

test_monitor_configuration() {
  local arn
  arn=$(kc -n nvsentinel get serviceaccount "${MONITOR}" -o jsonpath='{.metadata.annotations.eks\.amazonaws\.com/role-arn}')
  if [[ "${arn}" == "${WANT_ROLE_ARN}" ]]; then
    pass "ServiceAccount carries the IRSA role ${arn}"
  else
    fail "ServiceAccount IRSA role" "got '${arn}', want ${WANT_ROLE_ARN}"
  fi

  local toml
  toml=$(kc -n nvsentinel get configmap "${MONITOR}" -o jsonpath='{.data.config\.toml}')
  if grep -qF "clusterName = \"${CSP_CLUSTER_NAME}\"" <<<"${toml}" &&
    grep -A1 -F '[aws]' <<<"${toml}" | grep -qF 'enabled = true' &&
    grep -qF "region = \"${AWS_REGION}\"" <<<"${toml}"; then
    pass "config selects aws in ${AWS_REGION} for ${CSP_CLUSTER_NAME}"
  else
    fail "config selects aws" "config.toml: $(tr '\n' ' ' <<<"${toml}" | cut -c1-400)"
  fi

  local args
  args=$(kc -n nvsentinel get deployment "${MONITOR}" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="maintenance-notifier")].args}')
  if grep -qF -- '--processing-strategy=STORE_ONLY' <<<"${args}"; then
    pass "maintenance-notifier runs STORE_ONLY"
  else
    fail "maintenance-notifier STORE_ONLY" "args: ${args}"
  fi
}

# mongo_eval runs JavaScript $1 against NVSentinel's database and prints its
# last output line, failing when mongosh does or runs past 60 seconds, so a
# hung exec cannot outlast a poll's deadline. The datastore requires a client
# certificate even with password auth, so it presents the member's own
# (/tmp/tls.pem, which the Percona entrypoint assembles).
mongo_eval() {
  local out
  out=$(timeout 60 kubectl --context "${KUBE_CONTEXT}" -n nvsentinel exec "${MONGODB_POD}" -c mongod -- mongosh --quiet \
    --tls --tlsCAFile /etc/mongodb-ssl/ca.crt --tlsCertificateKeyFile /tmp/tls.pem --tlsAllowInvalidHostnames \
    "mongodb://localhost:27017/admin?directConnection=true" \
    -u "${MONGO_USER}" -p "${MONGO_PASSWORD}" \
    --eval "const hdb = db.getSiblingDB('HealthEventsDatabase'); $1" 2>&1) || return 1
  tail -1 <<<"${out}"
}

# insert_maintenance_event stores AWS maintenance event $1 for the node,
# starting in $2 minutes, as the AWS poller would.
insert_maintenance_event() {
  local id="$1" minutes="$2" got
  got=$(mongo_eval "const now = new Date();
    const r = hdb.MaintenanceEvents.insertOne({
      eventId: '${id}', csp: 'aws', clusterName: '${CSP_CLUSTER_NAME}',
      resourceType: 'EC2', resourceId: 'i-0123456789abcdef0', nodeName: '${NODE}',
      maintenanceType: 'SCHEDULED', status: 'DETECTED', cspStatus: 'PENDING',
      scheduledStartTime: new Date(now.getTime() + ${minutes} * 60000),
      scheduledEndTime: new Date(now.getTime() + (${minutes} + 60) * 60000),
      eventReceivedTimestamp: now, lastUpdatedTimestamp: now, recommendedAction: 'RESTART_VM'});
    print(r.acknowledged)") || err "could not insert maintenance event ${id}"
  [[ "${got}" == "true" ]] || err "maintenance event ${id} was not stored: ${got}"
}

# event_status prints the stored status of maintenance event $1.
event_status() {
  mongo_eval "const e = hdb.MaintenanceEvents.findOne({eventId: '$1'}); print(e ? e.status : 'MISSING')"
}

# health_events prints, as a JSON array, the CSPMaintenance health events
# published for the node.
health_events() {
  mongo_eval "print(EJSON.stringify(hdb.HealthEvents.find(
    {'healthevent.agent': 'csp-health-monitor', 'healthevent.checkname': 'CSPMaintenance',
     'healthevent.nodename': '${NODE}'},
    {_id: 0, 'healthevent.ishealthy': 1, 'healthevent.isfatal': 1,
     'healthevent.processingstrategy': 1}).toArray()))"
}

# Node predicates fail when the read fails, so a negative one cannot pass on
# an API error.
node_field() {
  local json
  json=$(kc get node "${NODE}" -o json) || return 1
  jq -r "$@" <<<"${json}"
}
cordoned() { [[ "$(node_field '.spec.unschedulable // false')" == "true" ]]; }
uncordoned() { [[ "$(node_field '.spec.unschedulable // false')" == "false" ]]; }
quarantined_for_maintenance() {
  # shellcheck disable=SC2016 # $check is a jq variable, bound by --arg.
  [[ "$(node_field --arg check CSPMaintenance \
    '.metadata.annotations.quarantineHealthEvent // "" | contains($check)')" == "true" ]]
}
not_quarantined_for_maintenance() {
  # shellcheck disable=SC2016 # $check is a jq variable, bound by --arg.
  [[ "$(node_field --arg check CSPMaintenance \
    '.metadata.annotations.quarantineHealthEvent // "" | contains($check)')" == "false" ]]
}
status_is() { [[ "$(event_status "$1")" == "$2" ]]; }

# poll runs the remaining arguments until they succeed or $1 seconds pass.
poll() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@"; do
    ((SECONDS < deadline)) || return 1
    sleep 5
  done
}

test_store_only_event() {
  msg "Injecting a maintenance event under STORE_ONLY..."
  insert_maintenance_event e2e-store-only 10
  if poll "${EVENT_WAIT_SECONDS}" status_is e2e-store-only QUARANTINE_TRIGGERED; then
    pass "the notifier published the event inside the lead time (QUARANTINE_TRIGGERED)"
  else
    fail "STORE_ONLY event published" "status: $(event_status e2e-store-only)"
    return
  fi
  local events
  events=$(health_events) || { fail "STORE_ONLY health event" "could not read HealthEvents"; return; }
  if jq -e 'length >= 1 and all(.[]; .healthevent.isfatal == true and .healthevent.ishealthy == false
      and (.healthevent.processingstrategy == 2 or .healthevent.processingstrategy == "STORE_ONLY"))' \
    <<<"${events}" >/dev/null; then
    pass "the CSPMaintenance health event is stored STORE_ONLY"
  else
    fail "STORE_ONLY health event" "events: ${events}"
  fi
  # The observe step's dry run would keep the node schedulable whatever the
  # strategy, but fault-quarantine still records the quarantine it would have
  # applied, so the annotation shows an event it consumed. It reacts within
  # seconds; one notifier period gives a wrong entry time to appear.
  sleep 60
  if not_quarantined_for_maintenance; then
    pass "a STORE_ONLY maintenance event gives the node no CSPMaintenance quarantine"
  else
    fail "STORE_ONLY gives no quarantine" "annotation=$(node_field '.metadata.annotations.quarantineHealthEvent // "none"' | cut -c1-200)"
  fi

  complete_maintenance e2e-store-only
  if poll "${EVENT_WAIT_SECONDS}" status_is e2e-store-only HEALTHY_TRIGGERED; then
    pass "the notifier published the completion after the post-maintenance delay"
  else
    fail "STORE_ONLY completion published" "status: $(event_status e2e-store-only)"
  fi
  if not_quarantined_for_maintenance; then
    pass "the STORE_ONLY completion gives the node no CSPMaintenance quarantine"
  else
    fail "STORE_ONLY completion gives no quarantine" "annotation=$(node_field '.metadata.annotations.quarantineHealthEvent // "none"' | cut -c1-200)"
  fi
}

# complete_maintenance marks maintenance event $1 complete, ended 20 minutes
# ago, past the post-maintenance delay.
complete_maintenance() {
  local got
  got=$(mongo_eval "const end = new Date(Date.now() - 20 * 60000);
    const r = hdb.MaintenanceEvents.updateOne({eventId: '$1'},
      {\$set: {status: 'MAINTENANCE_COMPLETE', cspStatus: 'COMPLETED', actualStartTime: new Date(end.getTime() - 60000),
       actualEndTime: end, lastUpdatedTimestamp: new Date()}});
    print(r.modifiedCount)") || err "could not complete maintenance event $1"
  [[ "${got}" == "1" ]] || err "maintenance event $1 was not updated: ${got}"
}

# install_with_strategy installs the quarantine bundle's nvsentinel with the
# monitor's processingStrategy set to $1 in its values, past the gate.
install_with_strategy() {
  STRATEGY="$1" yq -i '.["csp-health-monitor"].processingStrategy = strenv(STRATEGY)' "${QUARANTINE_DIR}/values.yaml" ||
    err "could not set processingStrategy in the quarantine bundle"
  install_component "${QUARANTINE_DIR}"
  local deadline=$((SECONDS + NVSENTINEL_WAIT_SECONDS)) args
  wait_rollout deployment fault-quarantine nvsentinel "${deadline}"
  wait_rollout deployment node-drainer nvsentinel "${deadline}"
  wait_rollout deployment "${MONITOR}" nvsentinel "${deadline}"
  args=$(kc -n nvsentinel get deployment "${MONITOR}" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="maintenance-notifier")].args}') ||
    err "could not read the maintenance notifier's args"
  grep -qF -- "--processing-strategy=$1" <<<"${args}" || err "maintenance-notifier is not $1: ${args}"
}

# move_up_to_quarantine bundles nvsentinel-quarantine, which the gate accepts
# only at STORE_ONLY, and installs it with EXECUTE_REMEDIATION.
move_up_to_quarantine() {
  msg "Moving up to nvsentinel-quarantine, with EXECUTE_REMEDIATION set past the gate..."
  compose_recipe eks-quarantine eks h100-eks-ubuntu-training nvsentinel-quarantine --os ubuntu
  bundle_recipe eks-quarantine quarantine \
    --set "nvsentinel:${MONITOR}.configToml.clusterName=${CSP_CLUSTER_NAME}" \
    --set "nvsentinel:${MONITOR}.configToml.aws.region=${AWS_REGION}" \
    --set-json "nvsentinel:${MONITOR}.configToml.aws.accountId=\"${AWS_ACCOUNT_ID}\"" \
    --set "nvsentinel:fault-quarantine.circuitBreaker.enabled=false" \
    --set "nvsentinel:node-drainer.systemNamespaces=${DRAIN_EXTRA_NAMESPACES}" ||
    { tail -20 "${WORK}/bundle-quarantine.log"; err "the quarantine bundle was refused"; }
  QUARANTINE_DIR=$(find "${WORK}/bundle-quarantine" -maxdepth 1 -type d -name '*-nvsentinel' -print -quit)
  [[ -n "${QUARANTINE_DIR}" ]] || err "the quarantine bundle has no nvsentinel directory"
  install_with_strategy EXECUTE_REMEDIATION
  local out
  if out=$(kc -n nvsentinel get deployment fault-remediation -o name 2>&1); then
    fail "quarantine step" "fault-remediation still runs after the upgrade"
  elif ! grep -q '(NotFound)' <<<"${out}"; then
    err "could not confirm fault-remediation was removed: ${out}"
  fi
}

# finish_notice completes maintenance event $1 and waits until its healthy
# event is published and the node is released, so no completion is still
# pending when the next scenario starts: one would release that scenario's
# hold.
finish_notice() {
  complete_maintenance "$1"
  poll "${EVENT_WAIT_SECONDS}" status_is "$1" HEALTHY_TRIGGERED ||
    err "the completion of $1 was never published: $(event_status "$1")"
  poll "${EVENT_WAIT_SECONDS}" uncordoned || err "the node was never released after $1 completed"
}

# expect_held_after asserts that once maintenance event $1 completes, the node
# stays cordoned for maintenance event $2.
expect_held_after() {
  local done="$1" open="$2" name="$3"
  complete_maintenance "${done}"
  poll "${EVENT_WAIT_SECONDS}" status_is "${done}" HEALTHY_TRIGGERED ||
    { fail "${name}" "the completion was never published: $(event_status "${done}")"; return; }
  # fault-quarantine applies a published event within seconds; give a wrong
  # release one notifier period to appear.
  sleep 60
  if cordoned && quarantined_for_maintenance; then
    pass "${name}"
  else
    fail "${name}" "the node was released while ${open} is $(event_status "${open}")"
  fi
}

# require_clean fails the run unless the node is schedulable with no
# CSPMaintenance quarantine, so a scenario cannot pass on a hold an earlier
# one left behind.
require_clean() {
  if ! { uncordoned && not_quarantined_for_maintenance; }; then
    err "node not clean before $1: $(node_field '.metadata.annotations.quarantineHealthEvent // "none"' | cut -c1-200)"
  fi
}

test_single_notice() {
  require_clean "the single-notice scenario"
  insert_maintenance_event e2e-single 10
  if poll "${EVENT_WAIT_SECONDS}" cordoned && quarantined_for_maintenance; then
    pass "a maintenance event inside the lead time cordons the node for CSPMaintenance"
  else
    fail "maintenance event cordons" "unschedulable=$(node_field '.spec.unschedulable') status=$(event_status e2e-single)"
    return
  fi
  complete_maintenance e2e-single
  if poll "${EVENT_WAIT_SECONDS}" status_is e2e-single HEALTHY_TRIGGERED && poll 120 uncordoned; then
    pass "the healthy event after the window uncordons the node"
  else
    fail "maintenance completion uncordons" "status=$(event_status e2e-single) unschedulable=$(node_field '.spec.unschedulable')"
  fi
}

test_overlapping_notices() {
  require_clean "the overlapping-notices scenario"
  insert_maintenance_event e2e-overlap-a 10
  insert_maintenance_event e2e-overlap-b 12
  if ! { poll "${EVENT_WAIT_SECONDS}" status_is e2e-overlap-b QUARANTINE_TRIGGERED && cordoned; }; then
    fail "overlapping notices" "not cordoned: a=$(event_status e2e-overlap-a) b=$(event_status e2e-overlap-b)"
    return
  fi
  expect_held_after e2e-overlap-a e2e-overlap-b "with two notices for the node, the first to complete leaves it cordoned"
  finish_notice e2e-overlap-b
}

test_stored_notice_after_switch() {
  require_clean "the stored-notice scenario"
  install_with_strategy STORE_ONLY
  insert_maintenance_event e2e-switch-old 10
  poll "${EVENT_WAIT_SECONDS}" status_is e2e-switch-old QUARANTINE_TRIGGERED ||
    { fail "stored notice after the switch" "the STORE_ONLY notice was never published"; return; }
  install_with_strategy EXECUTE_REMEDIATION
  insert_maintenance_event e2e-switch-new 10
  if ! { poll "${EVENT_WAIT_SECONDS}" cordoned && quarantined_for_maintenance; }; then
    fail "stored notice after the switch" "the new notice never cordoned the node"
    return
  fi
  expect_held_after e2e-switch-old e2e-switch-new \
    "a notice stored under STORE_ONLY that completes after the switch leaves the newer hold"
  finish_notice e2e-switch-new
}

# ── Main ──

msg "=========================================="
msg "nvsentinel-csp-health-monitor mixins e2e"
msg "=========================================="

has_tools go kind kubectl helm yq jq timeout
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
compose_recipe eks eks h100-eks-ubuntu-training nvsentinel-observe --os ubuntu
test_gate_refusals
ensure_cluster
install_bundle
test_monitor_runs_without_identity
test_monitor_configuration
test_store_only_event
if [[ "${CSP_EXECUTION_SCENARIOS}" == "true" ]]; then
  move_up_to_quarantine
  test_single_notice
  test_overlapping_notices
  test_stored_notice_after_switch
fi

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel-csp-health-monitor e2e: all tests passed"
