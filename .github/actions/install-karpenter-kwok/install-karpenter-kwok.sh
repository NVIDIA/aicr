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
if [[ -z "${KIND_CLUSTER_NAME:-}" ]]; then
  echo "::error::KIND_CLUSTER_NAME is required"
  exit 1
fi
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-${KIND_CLUSTER_NAME}}"

validate_duration_input() {
  local input_name="$1"
  local input_value="$2"

  if ! [[ "${input_value}" =~ ^[0-9]+[smh]$ ]]; then
    echo "::error::${input_name} must be a duration like 300s, 10m, or 1h; got '${input_value}'"
    exit 1
  fi
}

validate_duration_input kwok_helm_timeout "${KWOK_HELM_TIMEOUT}"
validate_duration_input ko_build_timeout "${KO_BUILD_TIMEOUT}"
validate_duration_input karpenter_helm_timeout "${KARPENTER_HELM_TIMEOUT}"
bash kwok/scripts/install-karpenter-kwok.sh
timeout 30s kubectl --request-timeout=10s \
  --context="${KUBE_CONTEXT}" \
  apply -f kwok/manifests/karpenter/nodepool.yaml
