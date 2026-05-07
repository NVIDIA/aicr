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

: "${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME must be set}"
KUBECTL_REQUEST_TIMEOUT="${KUBECTL_REQUEST_TIMEOUT:-10s}"
KUBECTL_WAIT_REQUEST_TIMEOUT="${KUBECTL_WAIT_REQUEST_TIMEOUT:-130s}"
POD_NAME_FILE="${POD_NAME_FILE:-/tmp/aicr-gpu-smoke-pod-name-${KIND_CLUSTER_NAME}}"

kubectl_kind() {
  kubectl --request-timeout="${KUBECTL_REQUEST_TIMEOUT}" --context="kind-${KIND_CLUSTER_NAME}" "$@"
}

kubectl_kind_wait() {
  timeout 150s kubectl --request-timeout="${KUBECTL_WAIT_REQUEST_TIMEOUT}" --context="kind-${KIND_CLUSTER_NAME}" "$@"
}

pod_name=$(cat <<'EOF' | kubectl_kind create -f - -o jsonpath='{.metadata.name}'
apiVersion: v1
kind: Pod
metadata:
  generateName: gpu-smoke-test-
  labels:
    app: gpu-smoke-test
spec:
  restartPolicy: Never
  containers:
  - name: nvidia-smi
    # Intentionally use a small base image: NVIDIA Container Toolkit should
    # inject nvidia-smi into GPU containers. This smoke test should fail if it
    # does not.
    image: ubuntu:22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 1
EOF
)

mkdir -p "$(dirname "${POD_NAME_FILE}")"
echo "${pod_name}" > "${POD_NAME_FILE}"

echo "Waiting for ${pod_name} pod to complete..."
kubectl_kind_wait wait "pod/${pod_name}" \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s
