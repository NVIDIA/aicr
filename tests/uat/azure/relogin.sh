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
#
# Federated az session refresh, sourced by tests/uat/azure/run (readiness gate,
# debug phase) and the Azure reap in .github/scripts/uat-janitor.sh; sourcing it
# runs nothing. Executed directly (the Destroy Cluster step in
# .github/workflows/uat-azure.yaml, which may not `source`), it refreshes once.

# Refresh the federated az session mid-phase. In CI the session minted by
# azure/login cannot self-refresh (client-credentials grant, no refresh
# token): once its access tokens expire, kubectl/aicr calls fail with
# AADSTS700024 and NO amount of retrying recovers — a fresh GitHub OIDC
# assertion must be redeemed. GitHub permits minting ID tokens repeatedly, so
# re-login in place: mint a fresh ACTIONS_ID_TOKEN, az login with it, then
# EAGERLY warm the kubelogin audience (the AKS AAD server app) — the
# assertion is only ~5-minute valid, so the token must be minted now, not
# lazily when kubelogin next misses its cache. No-op outside CI (local user
# az sessions self-refresh) and when the AZURE_* identifiers are absent.
AKS_SERVER_APP_ID="6dae42f8-4368-4678-94ff-3960e28e3630"
az_federated_relogin() {
  if [[ -z "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" || -z "${AZURE_CLIENT_ID:-}" \
    || -z "${AZURE_TENANT_ID:-}" || -z "${AZURE_SUBSCRIPTION_ID:-}" ]]; then
    return 0
  fi
  # Explicit `|| return 1` on every step: the caller invokes this inside an
  # `if` condition, which suspends errexit for the whole function body — a
  # failed mint/login/warm-up would otherwise fall through to the success
  # echo and return 0, silently advancing the caller's re-login timer.
  local oidc_token
  oidc_token="$(curl -fsSL --retry 3 \
    -H "Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
    "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=api://AzureADTokenExchange" | jq -r .value)" || return 1
  if [[ -z "${oidc_token}" || "${oidc_token}" == "null" ]]; then
    echo "federated re-login: token endpoint returned no value" >&2
    return 1
  fi
  az login --service-principal \
    --username "${AZURE_CLIENT_ID}" \
    --tenant "${AZURE_TENANT_ID}" \
    --federated-token "${oidc_token}" \
    --output none || return 1
  az account set --subscription "${AZURE_SUBSCRIPTION_ID}" --output none || return 1
  az account get-access-token --resource "${AKS_SERVER_APP_ID}" --output none || return 1
  echo "federated az session refreshed (ARM + AKS audiences)"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  set -euo pipefail
  az_federated_relogin
fi
