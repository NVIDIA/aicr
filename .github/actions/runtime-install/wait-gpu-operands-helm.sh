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

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME must be set}"
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-${KIND_CLUSTER_NAME}}"
DEVICE_PLUGIN_WAIT_TIMEOUT="${DEVICE_PLUGIN_WAIT_TIMEOUT:-300s}"
KUBECTL_WAIT_OUTER_TIMEOUT="${KUBECTL_WAIT_OUTER_TIMEOUT:-330s}"
KUBECTL_WAIT_REQUEST_TIMEOUT="${KUBECTL_WAIT_REQUEST_TIMEOUT:-${KUBECTL_WAIT_OUTER_TIMEOUT}}"

kubectl_kind() {
  timeout 30s kubectl --request-timeout=10s --context="${KUBE_CONTEXT}" "$@"
}

kubectl_kind_wait() {
  timeout "${KUBECTL_WAIT_OUTER_TIMEOUT}" kubectl \
    --request-timeout="${KUBECTL_WAIT_REQUEST_TIMEOUT}" \
    --context="${KUBE_CONTEXT}" "$@"
}

echo "Waiting for device plugin to be ready..."
if ! kubectl_kind_wait -n gpu-operator wait --for=create \
  daemonset/nvidia-device-plugin-daemonset \
  --timeout="${DEVICE_PLUGIN_WAIT_TIMEOUT}"; then
  echo "::error::device plugin DaemonSet was not created within ${DEVICE_PLUGIN_WAIT_TIMEOUT}"
  kubectl_kind -n gpu-operator get pods || true
  exit 1
fi
echo "Device plugin DaemonSet found."

if ! kubectl_kind_wait -n gpu-operator rollout status daemonset/nvidia-device-plugin-daemonset \
  --timeout="${DEVICE_PLUGIN_WAIT_TIMEOUT}"; then
  echo "::error::device plugin DaemonSet did not roll out within ${DEVICE_PLUGIN_WAIT_TIMEOUT}"
  kubectl_kind -n gpu-operator get pods -o wide || true
  kubectl_kind -n gpu-operator describe daemonset/nvidia-device-plugin-daemonset || true
  kubectl_kind -n gpu-operator get events --sort-by='.lastTimestamp' || true
  exit 1
fi
echo "GPU Operator pods:"
kubectl_kind -n gpu-operator get pods
