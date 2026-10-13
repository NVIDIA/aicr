#!/bin/bash
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

# Unit tests for the #2625 stage gates: uat_nvsentinel_remediation_mode
# (sxid-fault-inject.sh), which decides from the deployed recipe whether the
# sxidfault phase runs Stages 2 and 3, and uat_compose_nvsentinel_mixin
# (phases.sh), which composes the mixin a manual dispatch asks for.
#
# Hermetic: aicr is a stub that writes a fixture recipe. A missing yq is a
# FAILURE, not a skip, because both functions are yq programs.

set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

command -v yq >/dev/null 2>&1 || { echo "FAIL: yq is not on PATH; the functions under test are yq programs" >&2; exit 1; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/sxid-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT
fails=0
ran=0
pass() { ran=$((ran + 1)); echo "  ok   $1"; }
fail() { ran=$((ran + 1)); echo "FAIL: $1" >&2; fails=$((fails + 1)); }

# recipe <file> <nvsentinel global overrides as YAML flow, or "" for no nvsentinel>
recipe() {
    local f="$1" global="$2"
    {
        echo 'metadata:'
        echo '  appliedOverlays: [base, h100-eks-ubuntu-training]'
        echo 'componentRefs:'
        echo '  - name: gpu-operator'
        if [[ -n "${global}" ]]; then
            echo '  - name: nvsentinel'
            echo "    overrides: {global: ${global}}"
        fi
    } > "${f}"
}

# shellcheck source=tests/uat/lib/sxid-fault-inject.sh
source "${SCRIPT_DIR}/sxid-fault-inject.sh"

mode_case() { # <desc> <want> <global>
    local f="${WORK}/recipe-${ran}.yaml" got
    recipe "${f}" "$3"
    got="$(uat_nvsentinel_remediation_mode "${f}" 2>/dev/null)"
    [[ "${got}" == "$2" ]] && pass "$1" || fail "$1: want '$2', got '${got}'"
}

QUARANTINE='{dryRun: false, faultQuarantine: {enabled: true}, nodeDrainer: {enabled: true}}'
REMEDIATION='{dryRun: false, faultQuarantine: {enabled: true}, faultRemediation: {enabled: true}, janitor: {enabled: true}, janitorProvider: {enabled: true}}'

mode_case "a recipe without nvsentinel deploys no remediation" none ""
mode_case "monitoring-only nvsentinel deploys no remediation" none '{}'
mode_case "nvsentinel-observe is dry-run, so it cordons nothing" none \
    '{dryRun: true, faultQuarantine: {enabled: true}, faultRemediation: {enabled: true}}'
mode_case "nvsentinel-quarantine reads as quarantine" quarantine "${QUARANTINE}"
mode_case "nvsentinel-remediation reads as remediation" remediation "${REMEDIATION}"
mode_case "an absent dryRun is the chart's live default" quarantine '{faultQuarantine: {enabled: true}}'
mode_case "remediation without janitor cannot repair, so it is quarantine" quarantine \
    '{dryRun: false, faultQuarantine: {enabled: true}, faultRemediation: {enabled: true}}'

uat_nvsentinel_remediation_mode "${WORK}/missing.yaml" >/dev/null 2>&1 \
    && fail "a missing recipe must fail closed" || pass "a missing recipe fails closed"
echo 'componentRefs: [unterminated' > "${WORK}/bad.yaml"
uat_nvsentinel_remediation_mode "${WORK}/bad.yaml" >/dev/null 2>&1 \
    && fail "an unparseable recipe must fail closed" || pass "an unparseable recipe fails closed"

# --- uat_compose_nvsentinel_mixin ---------------------------------------
# The stub re-resolves by copying STUB_RECIPE over recipe.yaml when called with
# --data, and records every call so a no-op can be told from a re-resolve.
STUB_BIN="${WORK}/bin"; mkdir -p "${STUB_BIN}"
cat > "${STUB_BIN}/aicr" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "${PWD}/aicr-calls"
[[ " $* " == *" --data "* ]] && cp "${STUB_RECIPE:?}" recipe.yaml
exit 0
STUB
chmod +x "${STUB_BIN}/aicr"

# compose <dir> <mixin> <recipe the re-resolve returns> -> rc
compose() {
    local dir="$1"; mkdir -p "${dir}"
    recipe "${dir}/recipe.yaml" ""
    (
        cd "${dir}" || exit 99
        set -euo pipefail
        # shellcheck source=tests/uat/lib/phases.sh
        source "${SCRIPT_DIR}/phases.sh"
        config="${dir}/config.yaml"; : > "${config}"
        AICR_BIN="${STUB_BIN}/aicr" UAT_NVSENTINEL_MIXIN="$2" STUB_RECIPE="$3" uat_compose_nvsentinel_mixin
    ) > "${dir}/log" 2>&1
}

recipe "${WORK}/q.yaml" "${QUARANTINE}"
recipe "${WORK}/r.yaml" "${REMEDIATION}"
recipe "${WORK}/none.yaml" '{}'

d="${WORK}/unset"; compose "${d}" "" "${WORK}/q.yaml"; rc=$?
[[ "${rc}" == 0 && ! -e "${d}/aicr-calls" ]] && pass "an unset knob re-resolves nothing" \
    || fail "unset knob: rc=${rc}, calls=$(cat "${d}/aicr-calls" 2>/dev/null)"

d="${WORK}/quarantine"; compose "${d}" nvsentinel-quarantine "${WORK}/q.yaml"; rc=$?
[[ "${rc}" == 0 ]] && pass "quarantine composes and verifies" || { fail "quarantine: rc=${rc}"; sed 's/^/    | /' "${d}/log" >&2; }
[[ "$(yq -r '.spec.mixins[-1]' "${d}/uat-data/overlays/h100-eks-ubuntu-training.yaml" 2>/dev/null)" == nvsentinel-quarantine ]] \
    && pass "the mixin is appended to the leaf overlay" || fail "leaf overlay lacks the mixin"
grep -q -- '--data uat-data' "${d}/aicr-calls" 2>/dev/null \
    && pass "the recipe is re-resolved with --data" || fail "no --data re-resolve: $(cat "${d}/aicr-calls" 2>/dev/null)"

d="${WORK}/remediation"; compose "${d}" nvsentinel-remediation "${WORK}/r.yaml"; rc=$?
[[ "${rc}" == 0 ]] && pass "remediation composes and verifies" || { fail "remediation: rc=${rc}"; sed 's/^/    | /' "${d}/log" >&2; }

# A run asked to prove Stage 2 or 3 must not go green on Stage 1 alone.
d="${WORK}/not-landed"; compose "${d}" nvsentinel-quarantine "${WORK}/none.yaml"; rc=$?
[[ "${rc}" != 0 ]] && grep -q "want 'quarantine'" "${d}/log" \
    && pass "a mixin that did not land fails closed" || fail "not-landed: rc=${rc}"

d="${WORK}/wrong-step"; compose "${d}" nvsentinel-remediation "${WORK}/q.yaml"; rc=$?
[[ "${rc}" != 0 ]] && pass "remediation that resolves to quarantine fails closed" || fail "wrong-step: rc=${rc}"

d="${WORK}/unknown"; compose "${d}" nvsentinel-observe "${WORK}/q.yaml"; rc=$?
[[ "${rc}" != 0 && ! -e "${d}/aicr-calls" ]] && pass "an unknown mixin is refused before any re-resolve" \
    || fail "unknown mixin: rc=${rc}"

if (( fails > 0 )); then
    echo "${fails} test(s) failed (${ran} attempted)" >&2
    exit 1
fi
echo "All ${ran} sxid-fault-inject_test cases passed"
