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

if [[ -z "${GPU_OPERATOR_CHART_VERSION:-}" || "${GPU_OPERATOR_CHART_VERSION}" == "null" ]]; then
  echo "::error::GPU_OPERATOR_CHART_VERSION must be provided by the runtime-install action"
  exit 1
fi

helm repo add nvidia https://helm.ngc.nvidia.com/nvidia --force-update
helm repo update
helm upgrade -i \
  --kube-context="kind-${KIND_CLUSTER_NAME}" \
  --namespace gpu-operator \
  --create-namespace \
  --set driver.enabled=false \
  --set toolkit.enabled=false \
  --set dcgmExporter.enabled=false \
  --set nfd.enabled=true \
  --version="${GPU_OPERATOR_CHART_VERSION}" \
  --wait --timeout=600s \
  gpu-operator nvidia/gpu-operator
