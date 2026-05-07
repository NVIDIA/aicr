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
KUBECTL_REQUEST_TIMEOUT="${KUBECTL_REQUEST_TIMEOUT:-10s}"
POD_NAME_FILE="${POD_NAME_FILE:-/tmp/aicr-gpu-smoke-pod-name-${KIND_CLUSTER_NAME}}"
trap 'rm -f "${POD_NAME_FILE}"' EXIT

kubectl_kind() {
  kubectl --request-timeout="${KUBECTL_REQUEST_TIMEOUT}" --context="kind-${KIND_CLUSTER_NAME}" "$@"
}

pod_name=""
if [[ -f "${POD_NAME_FILE}" ]]; then
  pod_name="$(cat "${POD_NAME_FILE}")"
  if [[ -n "${pod_name}" ]] && ! kubectl_kind get pod "${pod_name}" >/dev/null 2>&1; then
    pod_name=""
  fi
fi

if [[ -z "${pod_name}" ]]; then
  pod_name=$(kubectl_kind get pods \
    -l app=gpu-smoke-test \
    --sort-by=.metadata.creationTimestamp \
    -o jsonpath='{.items[-1:].metadata.name}')
fi

if [[ -z "${pod_name}" ]]; then
  echo "::error::no gpu-smoke-test pod found"
  exit 1
fi

kubectl_kind logs "${pod_name}"
