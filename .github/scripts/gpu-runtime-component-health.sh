#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "::error::Usage: $0 <training|inference>"
  exit 2
fi

mode="$1"
COMPONENT_HEALTH_TIMEOUT="${COMPONENT_HEALTH_TIMEOUT:-120s}"

duration_seconds() {
  local input_value="$1"
  local number="${input_value%[smh]}"
  local unit="${input_value: -1}"

  case "${unit}" in
    s) echo "$((10#${number}))" ;;
    m) echo "$((10#${number} * 60))" ;;
    h) echo "$((10#${number} * 3600))" ;;
    *)
      echo "::error::unsupported duration '${input_value}'" >&2
      exit 1
      ;;
  esac
}

kubectl_kind() {
  timeout 30s kubectl --request-timeout=10s --context="kind-${KIND_CLUSTER_NAME}" "$@"
}

kubectl_kind_wait() {
  # kubectl wait opens a watch that is already bounded by --timeout. Keep
  # request-timeout unset here so a slow API server does not cut the watch short.
  kubectl --context="kind-${KIND_CLUSTER_NAME}" "$@"
}

print_namespace_diagnostics() {
  local ns="$1"

  echo "=== ${ns} workloads ==="
  kubectl_kind -n "${ns}" get deployments,statefulsets,daemonsets,pods -o wide 2>/dev/null || true
  echo "=== Recent events (${ns}) ==="
  kubectl_kind -n "${ns}" get events --sort-by='.lastTimestamp' 2>/dev/null | tail -80 || true
}

wait_for_deployments() {
  local ns="$1"
  shift
  local deployments=("$@")

  echo "Waiting up to ${COMPONENT_HEALTH_TIMEOUT} for ${ns}: ${deployments[*]}"
  if kubectl_kind_wait -n "${ns}" wait \
    --for=condition=Available \
    --timeout="${COMPONENT_HEALTH_TIMEOUT}" \
    "${deployments[@]}"; then
    return 0
  fi

  echo "::error::One or more deployments in ${ns} did not become Available within ${COMPONENT_HEALTH_TIMEOUT}: ${deployments[*]}"
  print_namespace_diagnostics "${ns}"
  return 1
}

wait_for_required_object() {
  local resource="$1"
  local timeout_seconds
  local deadline

  timeout_seconds="$(duration_seconds "${COMPONENT_HEALTH_TIMEOUT}")"
  deadline=$((SECONDS + timeout_seconds))

  echo "Waiting up to ${COMPONENT_HEALTH_TIMEOUT} for ${resource}"
  while (( SECONDS <= deadline )); do
    if kubectl_kind get "${resource}" >/dev/null; then
      return 0
    fi
    sleep 2
  done

  echo "::error::Required object is missing: ${resource}"
  kubectl_kind get "${resource}" -o yaml 2>/dev/null || true
  kubectl_kind describe "${resource}" 2>/dev/null || true
  return 1
}

echo "=== Runtime component health (${mode}) ==="

wait_for_deployments monitoring \
  deployment/kube-prometheus-operator

wait_for_deployments kai-scheduler \
  deployment/kai-scheduler-default \
  deployment/admission \
  deployment/binder \
  deployment/kai-operator \
  deployment/pod-grouper \
  deployment/podgroup-controller \
  deployment/queue-controller

case "${mode}" in
  training)
    wait_for_deployments kubeflow \
      deployment/kubeflow-trainer-controller-manager
    wait_for_required_object validatingwebhookconfiguration/validator.trainer.kubeflow.org
    wait_for_required_object customresourcedefinition/trainjobs.trainer.kubeflow.org
    ;;
  inference)
    wait_for_deployments dynamo-system \
      deployment/dynamo-platform-dynamo-operator-controller-manager \
      deployment/grove-operator
    wait_for_deployments kgateway-system \
      deployment/kgateway \
      deployment/inference-gateway
    ;;
  *)
    echo "::error::unknown runtime component health mode: ${mode}"
    exit 2
    ;;
esac

echo "Runtime component health check passed."
