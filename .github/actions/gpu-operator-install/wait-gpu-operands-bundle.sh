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

echo "Waiting for GPU operator controller to deploy operands..."
# The GPU operator controller watches ClusterPolicy and creates
# DaemonSets for device-plugin, NFD, GFD, etc. This happens
# asynchronously after the helm install completes.
for i in $(seq 1 30); do
  count=$(kubectl --context="kind-${KIND_CLUSTER_NAME}" -n gpu-operator \
    get daemonset -l app=nvidia-device-plugin-daemonset --no-headers 2>/dev/null | wc -l)
  if [[ "$count" -gt 0 ]]; then
    echo "Device plugin DaemonSet found."
    break
  fi
  echo "Waiting for device plugin DaemonSet to be created... (${i}/30)"
  sleep 10
done
echo "Waiting for device plugin rollout..."
# Operands are excluded from control-plane nodes via nodeAffinity in
# the kind overlay, so all scheduled pods should become ready.
kubectl --context="kind-${KIND_CLUSTER_NAME}" -n gpu-operator \
  rollout status daemonset -l app=nvidia-device-plugin-daemonset --timeout=300s
echo "GPU Operator pods:"
kubectl --context="kind-${KIND_CLUSTER_NAME}" -n gpu-operator get pods
