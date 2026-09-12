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

# Bound once as literals. Recipe values are validated as path components
# (separators rejected), not as shell words, so they are never interpolated
# bare into a command or into a double-quoted string, where $(...) would
# re-expand. Every later use is a plain parameter expansion, which does not.
RELEASE='k8s-aibom'
NAMESPACE='k8s-aibom-system'
RELEASE_FILTER='^k8s-aibom$'

if ! command -v kubectl >/dev/null 2>&1; then
  echo "ERROR: kubectl is required to apply ${RELEASE} CRDs before upgrade." >&2
  exit 1
fi

# Every helm and kubectl call below runs through run_bounded. This script runs
# inside the deploy path, where a command that never returns hangs the whole
# rollout rather than failing it: deploy.sh retries a component that exits
# non-zero but has no way to interrupt one that is still running. A wedged
# registry and a wedged apiserver both produce that, so the reads and the write
# are bounded alike.
#
# No unbounded fallback. Stock macOS ships no timeout(1), and running
# unbounded there would reintroduce exactly the hang this guards against on the
# one platform least likely to be exercised in CI. Failing closed with an
# actionable message is the safer trade: the operator can install coreutils, or
# apply the CRDs by hand with the command in the component catalog.
CRD_STEP_TIMEOUT="${AICR_CRD_STEP_TIMEOUT:-90}"
TIMEOUT_BIN=""
for candidate in timeout gtimeout; do
  if command -v "${candidate}" >/dev/null 2>&1; then
    TIMEOUT_BIN="${candidate}"
    break
  fi
done
if [[ -z "${TIMEOUT_BIN}" ]]; then
  echo "ERROR: neither timeout(1) nor gtimeout(1) is available, so the ${RELEASE} CRD" >&2
  echo "       step cannot be bounded and will not run unbounded inside a deploy." >&2
  echo "       Install GNU coreutils (macOS: brew install coreutils), or apply this" >&2
  echo "       chart's CRDs manually before upgrading; see the upgrade section of" >&2
  echo "       docs/user/component-catalog.md." >&2
  exit 1
fi

run_bounded() {
  "${TIMEOUT_BIN}" "${CRD_STEP_TIMEOUT}" "$@"
}

# Upgrades only. Helm installs a chart's crds/ directory itself on first
# install, which is the whole reason this script exists for upgrades and
# nowhere else. Running it on a fresh cluster would add a registry round-trip
# and a failure mode to the install path in order to apply CRDs helm is about
# to create a moment later.
#
# "Absent" and "cannot tell" are deliberately distinguished. An auth failure,
# an unreachable apiserver, or a broken helm must not read as a fresh install:
# the `helm upgrade` that follows can still succeed, and would then leave the
# previous CRDs in place. That is precisely the stranded-schema defect this
# script exists to prevent, so an indeterminate answer fails closed. `helm
# list` exits 0 whenever the query itself succeeded, whether or not it matched,
# which is what makes the two cases separable.
#
# The status flags are named rather than left to the default: Helm 4 lists every
# status by default but Helm 3 does not, and `--all` (which Helm 3 uses for
# that) was removed in Helm 4. These three exist in both and are exactly the
# set an upgrade would act on, so one spelling works against either binary.
if ! existing="$(run_bounded helm list --namespace "${NAMESPACE}" \
  --filter "${RELEASE_FILTER}" --short --deployed --failed --pending \
  ${KUBECONFIG_FLAG:-} 2>&1)"; then
  echo "ERROR: cannot determine whether release ${RELEASE} exists; refusing to" >&2
  echo "       skip the CRD step and risk leaving the previous schema in place: ${existing}" >&2
  exit 1
fi
if [[ -z "${existing//[[:space:]]/}" ]]; then
  echo "${RELEASE}: no existing release; helm install creates the chart's CRDs."
  exit 0
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
crds="$(run_bounded helm show crds "${CHART}" ${REPO:+--repo "${REPO}"} --version "${VERSION}" \
  | sed -n '/^---$/,$p')"

# An ownsCRDs component whose chart ships no CRDs is a no-op, not a failure:
# `kubectl apply` on an empty stream exits non-zero with "no objects passed to
# apply", which would abort the deploy over nothing.
if [[ -z "${crds//[[:space:]]/}" ]]; then
  echo "${RELEASE}: chart ships no CRDs; nothing to apply."
  exit 0
fi

# --server-side is required because these CRDs exceed the 262144-byte
# annotation cap client-side apply depends on. --force-conflicts is required
# because Helm created them on install and owns their fields.
printf '%s\n' "${crds}" \
  | run_bounded kubectl apply --server-side --force-conflicts ${KUBECONFIG_FLAG:-} -f -
