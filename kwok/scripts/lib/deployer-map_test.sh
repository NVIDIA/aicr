#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Unit harness for lib/deployer-map.sh (matrix deployer -> aicr --deployer).
# Run directly: bash kwok/scripts/lib/deployer-map_test.sh
# Wired into CI by the kwok-recipes discover job.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
# Resolve the subject SCRIPT_DIR-relative, never a deployed copy.
# shellcheck source=deployer-map.sh
source "${SCRIPT_DIR}/deployer-map.sh"

fails=0
check() { # <name> <want_rc> <want_stdout> <got_rc> <got_stdout>
    local name="$1" want_rc="$2" want_out="$3" got_rc="$4" got_out="$5"
    if [[ "${got_rc}" == "${want_rc}" && "${got_out}" == "${want_out}" ]]; then
        echo "PASS: ${name}"
    else
        echo "FAIL: ${name} (want rc=${want_rc} out='${want_out}'; got rc=${got_rc} out='${got_out}')"
        fails=$((fails + 1))
    fi
}

# 1-6. Every matrix deployer maps to the aicr deployer that installed it.
# argocd-helm-oci is the case no prefix rule gets right.
for pair in \
    "helm:helm" \
    "argocd-oci:argocd" \
    "argocd-git:argocd" \
    "argocd-helm-oci:argocd-helm" \
    "flux-oci:flux" \
    "flux-git:flux"; do
    matrix="${pair%%:*}"
    want="${pair##*:}"
    out=$(aicr_deployer_for "${matrix}" 2>/dev/null); rc=$?
    check "maps-${matrix}" 0 "${want}" "${rc}" "${out}"
done

# 7-9. An unmapped, empty, or absent name fails closed with empty stdout.
# A caller that used the output anyway would pass aicr a deployer whose
# release naming matches nothing, and get a report claiming a bare cluster.
out=$(aicr_deployer_for "flux-tarball" 2>/dev/null); rc=$?
check "unmapped-fails-closed" 1 "" "${rc}" "${out}"
out=$(aicr_deployer_for "" 2>/dev/null); rc=$?
check "empty-fails-closed" 1 "" "${rc}" "${out}"
out=$(aicr_deployer_for 2>/dev/null); rc=$?
check "missing-arg-fails-closed" 1 "" "${rc}" "${out}"

# 10. Domain coverage: the table must answer for exactly the deployers the
# matrix dispatches. The workflow's `deployers=` JSON array is the single
# source of truth for that list (kwok-recipes.yaml, "Classify recipes into
# tiers"); a new lane added there without a mapping here would run
# upgrade-check with no deployer and abort mid-lane.
workflow="${REPO_ROOT}/.github/workflows/kwok-recipes.yaml"
matrix_deployers=$(sed -n "s/^[[:space:]]*deployers='\(\[.*\]\)'[[:space:]]*$/\1/p" "${workflow}" \
    | head -1 | tr -d '[]"' | tr ',' '\n' | sed '/^$/d' | sort)
if [[ -z "${matrix_deployers}" ]]; then
    echo "FAIL: could not read the deployers list from ${workflow}"
    fails=$((fails + 1))
else
    unmapped=""
    while IFS= read -r matrix; do
        if ! aicr_deployer_for "${matrix}" >/dev/null 2>&1; then
            unmapped="${unmapped}${matrix} "
        fi
    done <<< "${matrix_deployers}"
    check "workflow-matrix-fully-mapped" 0 "" 0 "${unmapped% }"
fi

# 11. Range validity: every value the table prints must be a deployer aicr
# accepts. pkg/bundler/config/config.go is where that set is declared, so
# read it there rather than restating it; a renamed DeployerType constant
# has to fail here, not on a live matrix cell.
config="${REPO_ROOT}/pkg/bundler/config/config.go"
aicr_deployers=$(sed -n 's/^[[:space:]]*Deployer[A-Za-z]* DeployerType = "\([a-z-]*\)".*$/\1/p' "${config}" | sort -u)
if [[ -z "${aicr_deployers}" ]]; then
    echo "FAIL: could not read the DeployerType constants from ${config}"
    fails=$((fails + 1))
else
    unknown=""
    while IFS= read -r matrix; do
        mapped=$(aicr_deployer_for "${matrix}" 2>/dev/null) || continue
        if ! grep -qx -- "${mapped}" <<< "${aicr_deployers}"; then
            unknown="${unknown}${matrix}->${mapped} "
        fi
    done <<< "${matrix_deployers}"
    check "mapped-values-are-aicr-deployers" 0 "" 0 "${unknown% }"
fi

if ((fails > 0)); then
    echo "deployer-map_test.sh: ${fails} failure(s)"
    exit 1
fi
echo "deployer-map_test.sh: all checks passed"
