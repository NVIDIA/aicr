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
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:?KIND_NODE_IMAGE must be set}"
MIN_FREE_DISK_GB="${MIN_FREE_DISK_GB:?MIN_FREE_DISK_GB must be set}"
if ! [[ "${MIN_FREE_DISK_GB}" =~ ^[0-9]+$ ]]; then
  echo "::error::MIN_FREE_DISK_GB must be an integer, got '${MIN_FREE_DISK_GB}'"
  exit 1
fi

echo "=== Kind node image cache ==="
if docker image inspect "${KIND_NODE_IMAGE}" >/dev/null 2>&1; then
  echo "Kind node image already cached: ${KIND_NODE_IMAGE}"
else
  echo "Pulling kind node image: ${KIND_NODE_IMAGE}"
  timeout 600s docker pull "${KIND_NODE_IMAGE}"
fi
free_disk_bytes=$(df -B1 --output=avail / | tail -1 | tr -dc '0-9')
min_free_disk_bytes=$((MIN_FREE_DISK_GB * 1024 * 1024 * 1024))
free_disk_gib=$((free_disk_bytes / 1024 / 1024 / 1024))
if (( free_disk_bytes < min_free_disk_bytes )); then
  echo "::error::free disk on / is ${free_disk_bytes} bytes (${free_disk_gib}GiB) after warming ${KIND_NODE_IMAGE}, need at least ${min_free_disk_bytes} bytes (${MIN_FREE_DISK_GB}GiB)"
  exit 1
fi
echo "Runner disk remains sufficient after kind image warm-up: ${free_disk_gib}GiB (${free_disk_bytes} bytes)"
