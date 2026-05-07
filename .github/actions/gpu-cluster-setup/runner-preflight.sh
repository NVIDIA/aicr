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

echo "=== Runner baseline ==="
date -u
hostname
uptime
nproc
free -h
df -h /
df -ih /

for value_name in MIN_GPU_COUNT MIN_FREE_DISK_GB MIN_AVAILABLE_MEMORY_GB; do
  value="${!value_name}"
  if ! [[ "${value}" =~ ^[0-9]+$ ]]; then
    echo "::error::${value_name} must be an integer, got '${value}'"
    exit 1
  fi
done

echo "=== Docker health ==="
docker info >/dev/null
docker version

echo "=== Host GPUs ==="
nvidia-smi -L
nvidia-smi

mapfile -t gpu_names < <(nvidia-smi --query-gpu=name --format=csv,noheader)
if [[ -n "${GPU_MODEL_PATTERN}" ]]; then
  set +e
  gpu_count=$(printf '%s\n' "${gpu_names[@]}" | grep -Eic -- "${GPU_MODEL_PATTERN}")
  grep_status=$?
  set -e
  if (( grep_status == 2 )); then
    echo "::error::invalid gpu_model_pattern regex: ${GPU_MODEL_PATTERN}"
    exit 1
  fi
  if (( grep_status != 0 )); then
    gpu_count=0
  fi
  echo "Visible GPUs matching '${GPU_MODEL_PATTERN}': ${gpu_count}"
else
  gpu_count="${#gpu_names[@]}"
  echo "Visible GPUs: ${gpu_count}"
fi

if (( gpu_count < MIN_GPU_COUNT )); then
  echo "::error::visible GPU count ${gpu_count} is below required minimum ${MIN_GPU_COUNT}"
  exit 1
fi

echo "=== Existing kind state ==="
kind get clusters || true
docker ps -a --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}" || true
