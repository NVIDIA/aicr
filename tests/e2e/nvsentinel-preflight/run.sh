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
# nvsentinel-preflight mixin: runtime e2e (#2610)
# =============================================================================
#
# Drives the real product path (aicr recipe -> aicr bundle -> the bundle's own
# install.sh) against a live Kind cluster, then asserts the admission webhook's
# actual behavior: a GPU pod in an opted-in namespace gets the two default-on
# preflight init containers appended; pods that should be left alone -- a GPU
# pod in a namespace that never opted in, and a non-GPU pod in one that did --
# come back untouched; and a pod that opts into the multi-node check by
# annotation gets it WITH the gang env it needs to run.
#
# That injection decision is the feature, and nothing else in this repo can
# observe it. TestNVSentinelPreflightChartRender proves the webhook is
# CONFIGURED correctly; recipes/checks/nvsentinel-preflight/health-check.yaml
# proves it is DEPLOYED and reachable. Only a live admission request proves the
# controller actually mutates the right pods and only those.
#
# No GPU hardware is needed: injection happens at admission time, so the
# assertions read the created pod's spec and never wait for it to schedule or
# run. The pods deliberately stay Pending.
#
# PREREQUISITES: go, kind, kubectl, helm, yq
#
# ENVIRONMENT VARIABLES:
#   CLUSTER_NAME   Kind cluster name (default: aicr-nvsentinel-preflight-e2e).
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

CLUSTER_NAME="${CLUSTER_NAME:-aicr-nvsentinel-preflight-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"

# The namespace opt-in gate -- the mixin deploys the webhook, this label is
# what actually turns injection on. Kept verbatim in lockstep with
# docs/user/component-catalog.md and the chart's namespaceSelector.
PREFLIGHT_NS_LABEL="nvsentinel.nvidia.com/preflight=enabled"

# What a plain GPU pod must get. preflight-nccl-allreduce is deliberately NOT
# here: the mixin ships it defaultEnabled: false because it needs gang context
# a plain pod does not have. test_gang_annotated_pod_gets_allreduce covers it.
EXPECTED_INIT_CONTAINERS="preflight-dcgm-diag preflight-nccl-loopback"

NS_OPTED_IN="preflight-optin-e2e"
NS_NOT_OPTED_IN="preflight-no-optin-e2e"

WORK=""
CREATED_CLUSTER=false
BUNDLE_DIR=""
CERT_MANAGER_DIR=""
CRDS_DIR=""
KAI_DIR=""
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

cleanup() {
  local rc=$?
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

# find_bundle_dir echoes the bundled component directory whose name ends in $1.
# -print -quit rather than `| head -1`: head closing the pipe SIGPIPEs find, and
# under pipefail the bare assignment returns 141 and kills the script before the
# err message below can explain anything.
find_bundle_dir() {
  local suffix="$1" found
  found=$(find "${WORK}/bundle" -maxdepth 1 -type d -name "*-${suffix}" -print -quit)
  [[ -n "${found}" ]] || err "bundled ${suffix} component directory not found"
  printf '%s' "${found}"
}

compose_bundle() {
  msg "Composing a bundle with the nvsentinel-preflight mixin..."
  local data_dir="${WORK}/data"
  mkdir -p "${data_dir}/overlays"

  # --data requires a registry.yaml even when it adds no components of its own.
  cat >"${data_dir}/registry.yaml" <<'REGISTRY_EOF'
kind: ComponentRegistry
apiVersion: aicr.run/v1beta1
metadata:
  name: nvsentinel-preflight-e2e-registry
components: []
REGISTRY_EOF

  # Derived from the shipped overlay so its base, constraints and validation
  # cannot drift; the only difference is the composed mixin.
  yq '.spec.mixins = ["nvsentinel-preflight"]' \
    "${ROOT}/recipes/overlays/h100-kind-training.yaml" \
    >"${data_dir}/overlays/h100-kind-training.yaml"

  "${AICR_BIN}" recipe --service kind --accelerator h100 --intent training \
    --data "${data_dir}" --output "${WORK}/recipe.yaml" ||
    err "recipe generation failed"
  # The Kind overlay disables the standalone DCGM hostengine, which
  # CheckNVSentinelPreflightDCGMReachable now rejects alongside the preflight
  # mixin -- correctly, since the injected check could never reach it. Kind
  # cannot run DCGM either way; this test only exercises admission-time
  # injection and never starts the init containers, so the value is set to get
  # past a gate that is about runtime reachability.
  "${AICR_BIN}" bundle -r "${WORK}/recipe.yaml" --set gpuoperator:dcgm.enabled=true \
    -o "${WORK}/bundle" ||
    err "bundle generation failed"

  BUNDLE_DIR=$(find_bundle_dir nvsentinel)
  CERT_MANAGER_DIR=$(find_bundle_dir cert-manager)
  CRDS_DIR=$(find_bundle_dir prometheus-operator-crds)
  KAI_DIR=$(find_bundle_dir kai-scheduler)

  # Asserts the composed values by exact path. A bare `grep preflight` would
  # also match the chart's own default keys, and a bare `enabled: true` grep
  # matches unrelated components' fields -- both would pass with the mixin
  # silently absent.
  [[ "$(yq '.global.preflight.enabled' "${BUNDLE_DIR}/values.yaml")" == "true" ]] ||
    err "bundle's nvsentinel values.yaml does not set global.preflight.enabled: true -- composition regressed"
  [[ "$(yq '.preflight.webhook.failurePolicy' "${BUNDLE_DIR}/values.yaml")" == "Ignore" ]] ||
    err "bundle's nvsentinel values.yaml does not set preflight.webhook.failurePolicy: Ignore -- composition regressed"
  [[ "$(yq '.preflight.processingStrategy' "${BUNDLE_DIR}/values.yaml")" == "EXECUTE_REMEDIATION" ]] ||
    err "bundle's nvsentinel values.yaml does not set preflight.processingStrategy: EXECUTE_REMEDIATION -- a failed check would not gate the pod"
  [[ "$(yq '.preflight.gangDiscovery.name' "${BUNDLE_DIR}/values.yaml")" == "kai" ]] ||
    err "bundle's nvsentinel values.yaml does not set preflight.gangDiscovery.name: kai -- composition regressed"
  # The mixin owns the whole initContainers list. A partial restatement would
  # silently drop checks, and losing defaultEnabled would strand every non-gang
  # GPU pod in Init:Error -- neither is visible from the values file otherwise.
  [[ "$(yq '.preflight.initContainers | length' "${BUNDLE_DIR}/values.yaml")" == "3" ]] ||
    err "bundle's nvsentinel values.yaml does not carry all 3 preflight.initContainers -- a partial list drops checks"
  [[ "$(yq '.preflight.initContainers[] | select(.name=="preflight-nccl-allreduce") | .defaultEnabled' "${BUNDLE_DIR}/values.yaml")" == "false" ]] ||
    err "bundle's nvsentinel values.yaml does not disable preflight-nccl-allreduce -- non-gang GPU pods would be stranded"
}

ensure_cluster() {
  # Always recreated, never reused. A leftover cluster carries the previous
  # run's namespaces and pods -- an already-injected pod would satisfy the
  # positive assertion without the webhook doing anything this run. This
  # cluster name is dedicated to this test, so deleting it costs nothing but
  # the rebuild.
  if grep -qx "${CLUSTER_NAME}" <<<"$(kind get clusters 2>/dev/null)"; then
    msg "Deleting stale Kind cluster ${CLUSTER_NAME}..."
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi
  msg "Creating Kind cluster ${CLUSTER_NAME}..."
  kind create cluster --name "${CLUSTER_NAME}" || err "kind create cluster failed"
  CREATED_CLUSTER=true
  # kind switches the default kubeconfig's current-context as a side effect of
  # create/delete; export into a per-run kubeconfig so this never touches the
  # user's own.
  kind export kubeconfig --name "${CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" ||
    err "failed to export kubeconfig for ${CLUSTER_NAME}"
  kubectl --context "${KUBE_CONTEXT}" wait --for=condition=Ready node --all --timeout=120s ||
    err "cluster nodes never became Ready"
}

install_component() {
  local dir="$1" label="$2"
  (cd "${dir}" && chmod +x install.sh &&
    KUBE_CONTEXT="${KUBE_CONTEXT}" ./install.sh) ||
    err "${label} install failed"
}

# await_workload_exists polls until $1/$2 exists in namespace $3.
# `kubectl rollout status` on a missing object fails instantly instead of
# waiting, so this closes the window between helm returning and the controller
# creating the workload.
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

install_stack() {
  msg "Installing cert-manager, prometheus-operator-crds, kai-scheduler and nvsentinel from the bundle..."

  # cert-manager first and waited on: the preflight chart creates Certificate
  # and Issuer CRs, and its webhook is unusable until ca-injector has filled
  # in the caBundle. kai-scheduler next: the preflight controller validates the
  # PodGroup CRD at startup and fails closed, which is exactly why the mixin
  # declares kai-scheduler as a dependencyRef.
  install_component "${CERT_MANAGER_DIR}" cert-manager
  for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
    await_workload_exists deployment "${d}" cert-manager
    kubectl --context "${KUBE_CONTEXT}" -n cert-manager rollout status "deployment/${d}" --timeout=300s ||
      err "cert-manager component ${d} never became Ready"
  done

  install_component "${CRDS_DIR}" prometheus-operator-crds
  install_component "${KAI_DIR}" kai-scheduler
  kubectl --context "${KUBE_CONTEXT}" wait --for=condition=Established \
    crd/podgroups.scheduling.run.ai --timeout=180s ||
    err "kai-scheduler's PodGroup CRD was never established -- the preflight controller fails closed without it"

  install_component "${BUNDLE_DIR}" nvsentinel

  # install.sh runs `helm upgrade --install` without --wait (COMPONENT_WAIT_ARGS
  # is only exported by deploy.sh, which this bypasses), so helm returns before
  # the controllers have created anything.
  await_workload_exists deployment preflight nvsentinel
  # Generous: covers a cold pull of the whole NVSentinel image set, observed to
  # exceed 180s on a runner with an empty image cache.
  kubectl --context "${KUBE_CONTEXT}" -n nvsentinel rollout status deployment/preflight \
    --timeout=600s ||
    err "preflight never became Ready"

  await_webhook_ca_injected
}

# await_webhook_ca_injected blocks until cert-manager's ca-injector has filled
# in the webhook's caBundle. This gate is the whole reason the negative tests
# below mean anything: failurePolicy is Ignore, so before the CA lands the API
# server silently skips the webhook and EVERY pod comes back uninjected --
# which would pass both negative assertions for entirely the wrong reason.
await_webhook_ca_injected() {
  local elapsed=0 bundle
  while [[ "${elapsed}" -lt 180 ]]; do
    # Status captured separately from the value: inlining this would turn an
    # apiserver failure into an ordinary "not yet injected" and spin here until
    # the timeout with no explanation.
    if bundle=$(kubectl --context "${KUBE_CONTEXT}" get mutatingwebhookconfiguration preflight \
      -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null); then
      [[ -n "${bundle}" ]] && { msg "webhook caBundle injected"; return 0; }
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  err "cert-manager never injected a caBundle into the preflight webhook -- with failurePolicy Ignore the webhook would be skipped and every assertion below would be vacuous"
}

# await_webhook_serving blocks until the webhook actually mutates a pod.
#
# A completed rollout and a populated caBundle are both necessary and neither is
# sufficient: the apiserver caches the webhook configuration, so there is a
# window after the CA lands where it still dials with the old one. failurePolicy
# is Ignore, so that window admits every pod unmutated and silently -- the
# positive test below then fails, and the negative tests pass for the wrong
# reason. Creating a real pod is the only signal that the whole admission path
# works, so probe until it does.
await_webhook_serving() {
  local elapsed=0 probe="preflight-webhook-probe" injected
  while [[ "${elapsed}" -lt 120 ]]; do
    kubectl --context "${KUBE_CONTEXT}" -n "${NS_OPTED_IN}" delete pod "${probe}" \
      --ignore-not-found >/dev/null 2>&1
    create_pod "${NS_OPTED_IN}" "${probe}" gpu
    if injected=$(injected_init_containers "${NS_OPTED_IN}" "${probe}") && [[ -n "${injected}" ]]; then
      kubectl --context "${KUBE_CONTEXT}" -n "${NS_OPTED_IN}" delete pod "${probe}" \
        --ignore-not-found >/dev/null 2>&1
      msg "webhook is serving (probe got: ${injected})"
      return 0
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  err "the preflight webhook never mutated a probe pod in 120s -- with failurePolicy Ignore every assertion below would be vacuous"
}

# ── Assertion helpers ──

# create_pod creates a Pending pod named $2 in namespace $1. $3 selects whether
# it requests a GPU. It is never expected to schedule: Kind has no GPUs, and
# injection is an admission-time decision that is already visible on the
# created object.
create_pod() {
  local ns="$1" name="$2" gpu="$3" annotations="${4:-}" resources="" annotation_block=""
  if [[ "${gpu}" == "gpu" ]]; then
    resources='
      resources:
        limits:
          nvidia.com/gpu: "1"'
  fi
  if [[ -n "${annotations}" ]]; then
    annotation_block="
  annotations:
${annotations}"
  fi
  kubectl --context "${KUBE_CONTEXT}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${ns}${annotation_block}
spec:
  restartPolicy: Never
  containers:
    - name: main
      image: registry.k8s.io/pause:3.10${resources}
EOF
}

# injected_init_containers echoes the sorted names of the pod's preflight init
# containers, or nothing. Returns nonzero if the query fails rather than
# calling err: this runs inside a command substitution, where err would exit
# only the subshell and leave the caller reading empty output as "nothing was
# injected" -- passing a negative assertion on an apiserver hiccup.
injected_init_containers() {
  local ns="$1" name="$2" out
  out=$(kubectl --context "${KUBE_CONTEXT}" -n "${ns}" get pod "${name}" \
    -o jsonpath='{range .spec.initContainers[*]}{.name}{"\n"}{end}') || return 1
  grep '^preflight-' <<<"${out}" | sort | tr '\n' ' ' | sed 's/ $//' || true
}

# init_container_env echoes the env var NAMES on init container $3 of pod $1/$2.
# Returns nonzero on a failed query rather than an empty string, so a caller
# cannot read an apiserver hiccup as "the variable is missing".
init_container_env() {
  local ns="$1" name="$2" container="$3" out
  out=$(kubectl --context "${KUBE_CONTEXT}" -n "${ns}" get pod "${name}" \
    -o jsonpath="{.spec.initContainers[?(@.name==\"${container}\")].env[*].name}") || return 1
  printf '%s' "${out}"
}

# init_container_env_field_path echoes the valueFrom.fieldRef.fieldPath of env
# var $4 on init container $3. A name-only check would accept POD_NAME wired to
# an empty or literal value, which the check reads as "no pod name" and exits
# on -- the exact failure this test exists to catch.
init_container_env_field_path() {
  local ns="$1" name="$2" container="$3" var="$4" out
  out=$(kubectl --context "${KUBE_CONTEXT}" -n "${ns}" get pod "${name}" \
    -o jsonpath="{.spec.initContainers[?(@.name==\"${container}\")].env[?(@.name==\"${var}\")].valueFrom.fieldRef.fieldPath}") || return 1
  printf '%s' "${out}"
}

# ── Tests ──

test_opted_in_namespace_gets_injected() {
  msg "TEST: GPU pod in an opted-in namespace gets the preflight init containers"
  create_pod "${NS_OPTED_IN}" preflight-gpu-pod gpu

  local got want
  if ! got=$(injected_init_containers "${NS_OPTED_IN}" preflight-gpu-pod); then
    err "could not read the created pod -- cannot verify injection"
  fi
  # Sorted for comparison; the chart appends them in its own order, which is
  # not part of this mixin's contract.
  want=$(tr ' ' '\n' <<<"${EXPECTED_INIT_CONTAINERS}" | sort | tr '\n' ' ' | sed 's/ $//')
  detail "injected=[${got}]"
  # err, not fail: this is the only test that proves the webhook is reachable
  # at all. Continuing past it would print PASS for both negative tests in
  # exactly the situation where they mean nothing.
  [[ "${got}" == "${want}" ]] ||
    err "preflight init containers were [${got}], want [${want}] -- the webhook did not inject, so the negative tests below would pass vacuously"
  pass "nvsentinel-preflight/injects-into-opted-in-namespace"
}

test_namespace_without_optin_is_untouched() {
  msg "TEST: GPU pod in a namespace that never opted in is untouched"
  create_pod "${NS_NOT_OPTED_IN}" preflight-gpu-pod-no-optin gpu

  local got
  if ! got=$(injected_init_containers "${NS_NOT_OPTED_IN}" preflight-gpu-pod-no-optin); then
    err "could not read the created pod -- cannot prove nothing was injected"
  fi
  if [[ -z "${got}" ]]; then
    pass "nvsentinel-preflight/skips-namespace-without-optin"
  else
    fail "nvsentinel-preflight/skips-namespace-without-optin" \
      "the webhook injected [${got}] into a namespace that never opted in"
  fi
}

test_non_gpu_pod_is_untouched() {
  msg "TEST: non-GPU pod in an opted-in namespace is untouched"
  create_pod "${NS_OPTED_IN}" preflight-cpu-pod cpu

  local got
  if ! got=$(injected_init_containers "${NS_OPTED_IN}" preflight-cpu-pod); then
    err "could not read the created pod -- cannot prove nothing was injected"
  fi
  if [[ -z "${got}" ]]; then
    pass "nvsentinel-preflight/skips-non-gpu-pod"
  else
    fail "nvsentinel-preflight/skips-non-gpu-pod" \
      "the webhook injected [${got}] into a pod that requests no GPU"
  fi
}

# The opt-in path for the multi-node check. This is the case the mixin turns off
# by default, and the ONLY one where the all-reduce container is correct: the
# webhook injects POD_NAME solely when the pod carries a gang annotation at
# CREATE, and the check's entrypoint exits on the missing variable
# without it. Asserting the env var, not just the container name, is the point
# -- the name-only helper above is what let that defect through review.
test_gang_annotated_pod_gets_allreduce() {
  msg "TEST: gang-annotated GPU pod opting in gets all three checks, with POD_NAME"
  create_pod "${NS_OPTED_IN}" preflight-gang-pod gpu \
    "    pod-group-name: preflight-e2e-gang
    nvsentinel.nvidia.com/preflight-checks: \"preflight-dcgm-diag,preflight-nccl-loopback,preflight-nccl-allreduce\""

  local got want
  if ! got=$(injected_init_containers "${NS_OPTED_IN}" preflight-gang-pod); then
    err "could not read the created pod -- cannot verify the opt-in path"
  fi
  want=$(tr ' ' '\n' <<<"preflight-dcgm-diag preflight-nccl-allreduce preflight-nccl-loopback" | sort | tr '\n' ' ' | sed 's/ $//')
  detail "injected=[${got}]"
  if [[ "${got}" != "${want}" ]]; then
    fail "nvsentinel-preflight/gang-optin-injects-allreduce" "init containers were [${got}], want [${want}]"
    return
  fi
  pass "nvsentinel-preflight/gang-optin-injects-allreduce"

  local env_names
  if ! env_names=$(init_container_env "${NS_OPTED_IN}" preflight-gang-pod preflight-nccl-allreduce); then
    err "could not read the all-reduce container env -- cannot verify gang wiring"
  fi
  detail "allreduce env=[${env_names}]"
  if ! grep -qw "POD_NAME" <<<"${env_names}"; then
    fail "nvsentinel-preflight/gang-optin-injects-pod-name" \
      "POD_NAME absent from preflight-nccl-allreduce env; the check would exit on the missing variable and strand the pod"
    return
  fi

  # Presence is not enough: POD_NAME must resolve to the pod's own name via the
  # downward API. A literal or empty value satisfies a name-only check while
  # still failing the check at runtime.
  local field_path
  if ! field_path=$(init_container_env_field_path "${NS_OPTED_IN}" preflight-gang-pod preflight-nccl-allreduce POD_NAME); then
    err "could not read POD_NAME's valueFrom on the all-reduce container"
  fi
  detail "POD_NAME fieldPath=[${field_path}]"
  if [[ "${field_path}" == "metadata.name" ]]; then
    pass "nvsentinel-preflight/gang-optin-injects-pod-name"
  else
    fail "nvsentinel-preflight/gang-optin-injects-pod-name" \
      "POD_NAME resolves via fieldPath '${field_path}', want 'metadata.name' -- a literal or empty value leaves the check with no pod identity"
  fi
}

# ── Main ──

msg "=========================================="
msg "nvsentinel-preflight mixin runtime e2e"
msg "=========================================="

has_tools go kind kubectl helm yq
WORK=$(mktemp -d)
export KUBECONFIG="${WORK}/kubeconfig"

build_binary
compose_bundle
ensure_cluster
install_stack

kubectl --context "${KUBE_CONTEXT}" create namespace "${NS_OPTED_IN}" >/dev/null
kubectl --context "${KUBE_CONTEXT}" label namespace "${NS_OPTED_IN}" "${PREFLIGHT_NS_LABEL}" >/dev/null
kubectl --context "${KUBE_CONTEXT}" create namespace "${NS_NOT_OPTED_IN}" >/dev/null

await_webhook_serving

# The positive test runs first and aborts the run on failure: it is the only one
# that proves the webhook is reachable at all, so a negative test that ran
# without it would report a green "nothing was injected" against a webhook that
# was never consulted.
test_opted_in_namespace_gets_injected
test_namespace_without_optin_is_untouched
test_non_gpu_pod_is_untouched
test_gang_annotated_pod_gets_allreduce

msg "=========================================="
msg "Results: ${PASSED_TESTS}/${TOTAL_TESTS} passed"
[[ "${FAILED_TESTS}" -eq 0 ]] || err "${FAILED_TESTS} test(s) failed"
msg "nvsentinel-preflight e2e: all tests passed"
