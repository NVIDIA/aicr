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

# Build snapshot agent image with CUDA base (provides nvidia-smi for GPU detection).
# Uses cuda:base (~250MB) instead of cuda:runtime (~1.8GB) because only nvidia-smi is needed.
timeout 900s docker build -t ko.local:smoke-test -f - . <<'DOCKERFILE'
FROM nvcr.io/nvidia/cuda:13.1.0-base-ubuntu24.04
COPY dist/aicr /usr/local/bin/aicr
ENTRYPOINT ["/usr/local/bin/aicr"]
DOCKERFILE

# Load onto all nodes. The snapshot agent requests nvidia.com/gpu but does not
# set a node selector, so it can land on any GPU-capable node including the
# control-plane in the L40G smoke test.
timeout 900 kind load docker-image ko.local:smoke-test --name "${KIND_CLUSTER_NAME}" || {
  echo "::warning::kind load attempt 1 failed for ko.local:smoke-test, retrying..."
  timeout 900 kind load docker-image ko.local:smoke-test --name "${KIND_CLUSTER_NAME}"
}
