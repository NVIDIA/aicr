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

if [[ -z "${SNAPSHOT_AGENT_BASE_IMAGE:-}" || "${SNAPSHOT_AGENT_BASE_IMAGE}" == "null" ]]; then
  echo "::error::SNAPSHOT_AGENT_BASE_IMAGE must be provided by the aicr-build action"
  exit 1
fi

if [[ ! -f dist/aicr ]]; then
  echo "::error::dist/aicr not found; build the AICR CLI before building the snapshot agent image"
  exit 1
fi

if [[ -z "${KIND_CLUSTER_NAME:-}" || "${KIND_CLUSTER_NAME}" == "null" ]]; then
  echo "::error::KIND_CLUSTER_NAME must be provided by the aicr-build action"
  exit 1
fi

# Build the snapshot agent image on a static, distroless base. The agent binary
# is static Go and detects GPUs driver-free via NFD/PCI (sysfs), so it needs
# neither the NVIDIA driver nor nvidia-smi — and no CUDA base image.
timeout 900s docker build \
  --build-arg SNAPSHOT_AGENT_BASE_IMAGE="${SNAPSHOT_AGENT_BASE_IMAGE}" \
  -t ko.local:smoke-test -f - . <<'DOCKERFILE'
ARG SNAPSHOT_AGENT_BASE_IMAGE
FROM ${SNAPSHOT_AGENT_BASE_IMAGE}
COPY dist/aicr /usr/local/bin/aicr
ENTRYPOINT ["/usr/local/bin/aicr"]
DOCKERFILE

# Load onto all nodes. The snapshot agent has no node selector, so it can land
# on any node including the control-plane in the smoke test; PCI enumeration
# reads sysfs and needs no GPU resource/runtime-class.
timeout 900 kind load docker-image ko.local:smoke-test --name "${KIND_CLUSTER_NAME}" || {
  echo "::warning::kind load attempt 1 failed for ko.local:smoke-test, retrying..."
  timeout 900 kind load docker-image ko.local:smoke-test --name "${KIND_CLUSTER_NAME}"
}
