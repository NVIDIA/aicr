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
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

# Helm 4 uses server-side apply by default; --force-conflicts lets the
# upgrade overwrite fields that operators own on rotated webhook cert
# Secrets. Helm 3 uses client-side apply and does not recognize the flag.
HELM_MAJOR=$(helm version --template '{{.Version}}' 2>/dev/null | sed -nE 's/^v([0-9]+)\..*/\1/p')
FORCE_CONFLICTS_FLAG=""
if [[ "${HELM_MAJOR:-0}" -ge 4 ]]; then
  FORCE_CONFLICTS_FLAG="--force-conflicts"
fi

helm upgrade --install ${FORCE_CONFLICTS_FLAG} skyhook-customizations ./ \
  --namespace skyhook --create-namespace \
  -f values.yaml -f cluster-values.yaml \
  ${COMPONENT_WAIT_ARGS:-} ${DRY_RUN_FLAG:-} ${KUBECONFIG_FLAG:-} ${HELM_DEBUG_FLAG:-}
