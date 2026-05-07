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

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "::error::Usage: $0 <test_dir>"
  exit 2
fi
test_dir="$1"
if [[ ! -d "${test_dir}" ]]; then
  echo "::error::Test directory not found: ${test_dir}"
  exit 1
fi

CHAINSAW_TEST_TIMEOUT="${CHAINSAW_TEST_TIMEOUT:-30m}"
if ! [[ "${CHAINSAW_TEST_TIMEOUT}" =~ ^[0-9]+[smh]$ ]]; then
  echo "::error::CHAINSAW_TEST_TIMEOUT must be a duration like 30m, 180s, or 1h; got '${CHAINSAW_TEST_TIMEOUT}'"
  exit 1
fi
MONITORING_READY_TIMEOUT="${MONITORING_READY_TIMEOUT:-180s}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME must be set}"
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-${KIND_CLUSTER_NAME}}"
KUBECTL_WAIT_GRACE_SECONDS="${KUBECTL_WAIT_GRACE_SECONDS:-30}"

if ! [[ "${MONITORING_READY_TIMEOUT}" =~ ^[0-9]+[smh]$ ]]; then
  echo "::error::MONITORING_READY_TIMEOUT must be a duration like 180s, 5m, or 1h; got '${MONITORING_READY_TIMEOUT}'"
  exit 1
fi

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

if ! [[ "${KUBECTL_WAIT_GRACE_SECONDS}" =~ ^[0-9]+$ ]]; then
  echo "::error::KUBECTL_WAIT_GRACE_SECONDS must be a non-negative integer, got '${KUBECTL_WAIT_GRACE_SECONDS}'"
  exit 1
fi
monitoring_ready_timeout_seconds="$(duration_seconds "${MONITORING_READY_TIMEOUT}")"
KUBECTL_WAIT_OUTER_TIMEOUT="${KUBECTL_WAIT_OUTER_TIMEOUT:-$((monitoring_ready_timeout_seconds + KUBECTL_WAIT_GRACE_SECONDS))s}"
KUBECTL_WAIT_REQUEST_TIMEOUT="${KUBECTL_WAIT_REQUEST_TIMEOUT:-${KUBECTL_WAIT_OUTER_TIMEOUT}}"

kubectl_kind() {
  timeout 30s kubectl --request-timeout=10s --context="${KUBE_CONTEXT}" "$@"
}

kubectl_kind_wait() {
  timeout "${KUBECTL_WAIT_OUTER_TIMEOUT}" kubectl \
    --request-timeout="${KUBECTL_WAIT_REQUEST_TIMEOUT}" \
    --context="${KUBE_CONTEXT}" "$@"
}

print_monitoring_diagnostics() {
  echo "=== Monitoring workloads ==="
  kubectl_kind -n monitoring get deployment,statefulset,daemonset,pods -o wide 2>/dev/null || true
  echo "=== kube-prometheus-operator deployment ==="
  kubectl_kind -n monitoring get deployment kube-prometheus-operator -o wide 2>/dev/null || true
  echo "=== kube-prometheus-operator deployment describe ==="
  kubectl_kind -n monitoring describe deployment kube-prometheus-operator 2>/dev/null || true
  echo "=== kube-prometheus-operator pods ==="
  kubectl_kind -n monitoring get pods -o wide 2>/dev/null \
    | grep -E '(^NAME|^kube-prometheus-operator-)' || true
  echo "=== kube-prometheus-operator logs ==="
  kubectl_kind -n monitoring logs deployment/kube-prometheus-operator --all-containers --tail=200 2>/dev/null || true
  echo "=== kube-prometheus-operator previous logs ==="
  kubectl_kind -n monitoring logs deployment/kube-prometheus-operator --all-containers --previous --tail=200 2>/dev/null || true
  echo "=== Recent events (monitoring) ==="
  kubectl_kind -n monitoring get events --sort-by='.lastTimestamp' 2>/dev/null | tail -100 || true
}

wait_for_monitoring_operator() {
  echo "Waiting for monitoring/kube-prometheus-operator before Chainsaw..."
  if kubectl_kind_wait -n monitoring rollout status deployment/kube-prometheus-operator \
    --timeout="${MONITORING_READY_TIMEOUT}"; then
    echo "monitoring/kube-prometheus-operator is rolled out."
    return 0
  fi

  echo "::error::monitoring/kube-prometheus-operator did not become available within ${MONITORING_READY_TIMEOUT}"
  print_monitoring_diagnostics
  return 1
}

wait_for_monitoring_operator

# --skip-delete: these tests assert the already-deployed runtime bundle. Letting
# Chainsaw delete asserted resources would tear down the system under test.
timeout "${CHAINSAW_TEST_TIMEOUT}" chainsaw test \
  --test-dir "${test_dir}" \
  --config tests/chainsaw/chainsaw-config.yaml \
  --skip-delete
