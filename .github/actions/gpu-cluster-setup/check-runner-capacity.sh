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
free_disk_bytes=$(df -B1 --output=avail / | tail -1 | tr -dc '0-9')
min_free_disk_bytes=$((MIN_FREE_DISK_GB * 1024 * 1024 * 1024))
free_disk_gib=$((free_disk_bytes / 1024 / 1024 / 1024))
if (( free_disk_bytes < min_free_disk_bytes )); then
  echo "::error::free disk on / is ${free_disk_bytes} bytes (${free_disk_gib}GiB), need at least ${min_free_disk_bytes} bytes (${MIN_FREE_DISK_GB}GiB)"
  exit 1
fi

available_memory_gb=$(free -g | awk '/^Mem:/ {print $7}')
if (( available_memory_gb < MIN_AVAILABLE_MEMORY_GB )); then
  echo "::error::available memory is ${available_memory_gb}GiB, need at least ${MIN_AVAILABLE_MEMORY_GB}GiB"
  exit 1
fi

echo "Runner capacity is sufficient: disk=${free_disk_gib}GiB (${free_disk_bytes} bytes) memory=${available_memory_gb}GiB"
