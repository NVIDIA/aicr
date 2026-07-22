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

reject_newline() {
  local name="$1" value="$2"
  if [[ -z "${value}" || "${value}" == *$'\n'* || "${value}" == *$'\r'* ]]; then
    echo "::error::${name} must be a non-empty single-line value"
    exit 1
  fi
}

require_release_image() {
  case "$1" in
    ghcr.io/nvidia/aicr | \
      ghcr.io/nvidia/aicrd | \
      ghcr.io/nvidia/aicr-validators/deployment | \
      ghcr.io/nvidia/aicr-validators/performance | \
      ghcr.io/nvidia/aicr-validators/conformance | \
      ghcr.io/nvidia/aicr-validators/aiperf-bench | \
      ghcr.io/nvidia/aicr-gate) ;;
    *)
      echo "::error::image_name is not a fixed AICR release image"
      exit 1
      ;;
  esac
}
