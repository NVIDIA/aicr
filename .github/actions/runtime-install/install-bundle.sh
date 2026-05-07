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

cd bundle
# The default keeps legacy bundle-mode behavior: do not wait on every
# Helm resource and keep deploying after component failures. H100
# qualification jobs override these inputs to hard-fail and wait.
chmod +x deploy.sh
AICR_DEPLOY_WAIT="${AICR_DEPLOY_WAIT:-false}"
AICR_DEPLOY_BEST_EFFORT="${AICR_DEPLOY_BEST_EFFORT:-true}"
for deploy_flag_name in AICR_DEPLOY_WAIT AICR_DEPLOY_BEST_EFFORT; do
  case "${!deploy_flag_name}" in
    true|false) ;;
    *)
      echo "::error::${deploy_flag_name} must be true or false, got '${!deploy_flag_name}'"
      exit 1
      ;;
  esac
done

DEPLOY_ARGS=()
if [[ "${AICR_DEPLOY_WAIT}" != "true" ]]; then
  DEPLOY_ARGS+=(--no-wait)
fi
if [[ "${AICR_DEPLOY_BEST_EFFORT}" == "true" ]]; then
  DEPLOY_ARGS+=(--best-effort)
fi
if [[ "${#DEPLOY_ARGS[@]}" -gt 0 ]]; then
  echo "Deploying bundle with args: ${DEPLOY_ARGS[*]}"
else
  echo "Deploying bundle with default args"
fi
./deploy.sh "${DEPLOY_ARGS[@]}"
