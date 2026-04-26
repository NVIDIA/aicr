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

pod_name=$(cat <<'EOF' | kubectl --context="kind-${KIND_CLUSTER_NAME}" create -f - -o jsonpath='{.metadata.name}'
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
    image: ubuntu:22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 1
EOF
)

echo "${pod_name}" > /tmp/aicr-gpu-smoke-pod-name

echo "Waiting for ${pod_name} pod to complete..."
kubectl --context="kind-${KIND_CLUSTER_NAME}" wait "pod/${pod_name}" \
  --for=condition=Ready --timeout=120s || true
kubectl --context="kind-${KIND_CLUSTER_NAME}" wait "pod/${pod_name}" \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s
