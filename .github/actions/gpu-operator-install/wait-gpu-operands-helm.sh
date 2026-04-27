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

echo "Waiting for device plugin to be ready..."
for i in $(seq 1 30); do
  if kubectl --request-timeout=10s --context="kind-${KIND_CLUSTER_NAME}" -n gpu-operator \
    get daemonset -l app=nvidia-device-plugin-daemonset --no-headers 2>/dev/null | grep -q .; then
    echo "Device plugin DaemonSet found."
    break
  fi
  if (( i == 30 )); then
    echo "::error::device plugin DaemonSet was not created within 300s"
    kubectl --request-timeout=10s --context="kind-${KIND_CLUSTER_NAME}" -n gpu-operator get pods || true
    exit 1
  fi
  echo "Waiting for device plugin DaemonSet to be created... (${i}/30)"
  sleep 10
done

kubectl --request-timeout=300s --context="kind-${KIND_CLUSTER_NAME}" -n gpu-operator \
  rollout status daemonset -l app=nvidia-device-plugin-daemonset --timeout=300s
echo "GPU Operator pods:"
kubectl --request-timeout=10s --context="kind-${KIND_CLUSTER_NAME}" -n gpu-operator get pods
