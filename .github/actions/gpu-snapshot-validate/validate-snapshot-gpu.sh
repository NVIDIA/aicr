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

# Query by subtype field (not index) — #502 added a "hardware" subtype before "smi".
GPU_MODEL=$(yq eval '.measurements[] | select(.type == "GPU") | .subtypes[] | select(.subtype == "smi") | .data["gpu.model"]' snapshot.yaml)
GPU_COUNT=$(yq eval '.measurements[] | select(.type == "GPU") | .subtypes[] | select(.subtype == "smi") | .data["gpu-count"]' snapshot.yaml)
echo "GPU model: ${GPU_MODEL}"
echo "GPU count: ${GPU_COUNT}"
if ! [[ "${GPU_COUNT}" =~ ^[0-9]+$ ]]; then
  echo "::error::Expected numeric gpu-count in snapshot, got: ${GPU_COUNT}"
  exit 1
fi
if [[ "${GPU_MODEL}" != *"${EXPECTED_GPU_MODEL}"* ]]; then
  echo "::error::Expected ${EXPECTED_GPU_MODEL} GPU in snapshot, got: ${GPU_MODEL}"
  exit 1
fi
if [[ "${GPU_COUNT}" -lt ${MIN_GPU_COUNT} ]]; then
  echo "::error::Expected gpu-count >= ${MIN_GPU_COUNT}, got: ${GPU_COUNT}"
  exit 1
fi
echo "Snapshot correctly detected ${GPU_COUNT}x ${GPU_MODEL}"
