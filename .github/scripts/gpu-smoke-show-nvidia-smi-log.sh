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

pod_name=""
if [[ -f /tmp/aicr-gpu-smoke-pod-name ]]; then
  pod_name="$(cat /tmp/aicr-gpu-smoke-pod-name)"
fi

if [[ -z "${pod_name}" ]]; then
  pod_name=$(kubectl --context="kind-${KIND_CLUSTER_NAME}" get pods \
    -l app=gpu-smoke-test \
    --sort-by=.metadata.creationTimestamp \
    -o jsonpath='{.items[-1:].metadata.name}')
fi

if [[ -z "${pod_name}" ]]; then
  echo "::error::no gpu-smoke-test pod found"
  exit 1
fi

kubectl --context="kind-${KIND_CLUSTER_NAME}" logs "${pod_name}"
