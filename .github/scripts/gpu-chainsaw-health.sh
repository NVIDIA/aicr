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
  echo "::error::Usage: $0 <test_dir>"
  exit 2
fi
test_dir="$1"
if [[ ! -d "${test_dir}" ]]; then
  echo "::error::Test directory not found: ${test_dir}"
  exit 1
fi

CHAINSAW_TEST_TIMEOUT="${CHAINSAW_TEST_TIMEOUT:-30m}"
MONITORING_READY_TIMEOUT="${MONITORING_READY_TIMEOUT:-180s}"

kubectl_kind() {
  timeout 30s kubectl --request-timeout=10s --context="kind-${KIND_CLUSTER_NAME}" "$@"
}

kubectl_kind_wait() {
  # Rollout status opens a watch that is already bounded by --timeout. Keep
  # request-timeout unset here so a slow API server does not cut the watch short.
  kubectl --context="kind-${KIND_CLUSTER_NAME}" "$@"
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
  print_monitoring_diagnostics
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

timeout "${CHAINSAW_TEST_TIMEOUT}" chainsaw test \
  --test-dir "${test_dir}" \
  --config tests/chainsaw/chainsaw-config.yaml \
  --skip-delete
