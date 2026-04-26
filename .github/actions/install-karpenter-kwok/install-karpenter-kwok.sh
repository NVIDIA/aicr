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
validate_duration_input() {
  local input_name="$1"
  local input_value="$2"

  if ! [[ "${input_value}" =~ ^[0-9]+[smh]$ ]]; then
    echo "::error::${input_name} must be a duration like 300s, 10m, or 1h; got '${input_value}'"
    exit 1
  fi
}

validate_seconds_input() {
  local input_name="$1"
  local input_value="$2"

  if ! [[ "${input_value}" =~ ^[0-9]+$ ]]; then
    echo "::error::${input_name} must be an integer number of seconds, got '${input_value}'"
    exit 1
  fi
  if (( input_value <= 0 )); then
    echo "::error::${input_name} must be greater than 0 seconds, got '${input_value}'"
    exit 1
  fi
}

validate_duration_input kwok_helm_timeout "${KWOK_HELM_TIMEOUT}"
validate_seconds_input ko_build_timeout "${KO_BUILD_TIMEOUT}"
validate_duration_input karpenter_helm_timeout "${KARPENTER_HELM_TIMEOUT}"
bash kwok/scripts/install-karpenter-kwok.sh
timeout 30s kubectl --request-timeout=10s \
  --context="kind-${KIND_CLUSTER_NAME}" \
  apply -f kwok/manifests/karpenter/nodepool.yaml
