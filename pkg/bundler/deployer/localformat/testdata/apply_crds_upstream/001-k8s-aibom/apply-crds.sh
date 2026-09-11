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

# Apply k8s-aibom's CRDs from its pinned chart, ahead of `helm upgrade`.
#
# Helm installs a chart's crds/ directory on first install and never touches it
# again, so a chart bump whose CRDs changed would otherwise leave the previous
# schema in place: the API server then silently prunes the new controller's
# writes to fields the old schema does not know.
#
# AICR emits this script only for components the registry marks ownsCRDs whose
# ref still points at the registry-pinned chart. That flag records an audit of
# one specific chart: that the component solely owns every CRD it ships, and
# ships none using spec.conversion.strategy: Webhook. It says nothing about a
# chart an overriding ref points at.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

if ! command -v kubectl >/dev/null 2>&1; then
  echo "ERROR: kubectl is required to apply k8s-aibom CRDs before upgrade." >&2
  exit 1
fi

# shellcheck source=/dev/null
source ./upstream.env

# CHART carries the full OCI URI for OCI charts and just the chart name for
# HTTP/HTTPS charts; REPO is non-empty only for the latter.
#
# sed drops helm's own progress output: for an OCI chart `helm show crds`
# writes "Pulled:" and "Digest:" lines to stdout, and those two parse as a
# valid YAML mapping, so kubectl rejects the stream with
# "error validating data: [apiVersion not set, kind not set]".
crds="$(helm show crds "${CHART}" ${REPO:+--repo "${REPO}"} --version "${VERSION}" \
  | sed -n '/^---$/,$p')"

# An ownsCRDs component whose chart ships no CRDs is a no-op, not a failure:
# `kubectl apply` on an empty stream exits non-zero with "no objects passed to
# apply", which would abort the deploy over nothing.
if [[ -z "${crds//[[:space:]]/}" ]]; then
  echo "k8s-aibom: chart ships no CRDs; nothing to apply."
  exit 0
fi

# --server-side is required because these CRDs exceed the 262144-byte
# annotation cap client-side apply depends on. --force-conflicts is required
# because Helm created them on install and owns their fields.
printf '%s\n' "${crds}" \
  | kubectl apply --server-side --force-conflicts ${KUBECONFIG_FLAG:-} -f -
