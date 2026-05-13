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
# shellcheck source=/dev/null
source ./upstream.env

# CHART carries the full OCI URI for OCI charts and just the chart name for
# HTTP/HTTPS charts. REPO is non-empty only for HTTP/HTTPS charts; the
# ${REPO:+--repo "${REPO}"} expansion adds --repo iff REPO is set.
helm upgrade --install gpu-operator "${CHART}" \
  ${REPO:+--repo "${REPO}"} --version "${VERSION}" \
  --namespace privileged-gpu-operator --create-namespace \
  -f values.yaml -f cluster-values.yaml \
  ${COMPONENT_WAIT_ARGS:-} ${DRY_RUN_FLAG:-} ${KUBECONFIG_FLAG:-} ${HELM_DEBUG_FLAG:-}
