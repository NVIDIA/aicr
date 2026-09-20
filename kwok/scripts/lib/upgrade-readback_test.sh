#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Unit harness for lib/upgrade-readback.sh (inventory read-back comparison).
# Run directly: bash kwok/scripts/lib/upgrade-readback_test.sh
# Wired into CI by the kwok-recipes discover job.
#
# The cases that matter here are the negative ones. A comparison of two
# reports can pass because they agree, or because neither says anything,
# and the second reads identically in CI while proving nothing, which is
# exactly the failure mode the assertions in the subject exist to prevent.
# Every degenerate shape below must come back non-zero.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Resolve the subject SCRIPT_DIR-relative, never a deployed copy.
# shellcheck source=upgrade-readback.sh
source "${SCRIPT_DIR}/upgrade-readback.sh"

for tool in jq yq diff comm; do
    command -v "${tool}" >/dev/null || {
        echo "FAIL: ${tool} is required by upgrade-readback_test.sh"
        exit 1
    }
done

WORK="$(mktemp -d "${TMPDIR:-/tmp}/aicr-readback-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

fails=0
check_rc() { # <name> <want_rc> <got_rc>
    if [[ "$3" == "$2" ]]; then
        echo "PASS: $1"
    else
        echo "FAIL: $1 (want rc=$2; got rc=$3)"
        fails=$((fails + 1))
    fi
}
check_eq() { # <name> <want> <got>
    if [[ "$3" == "$2" ]]; then
        echo "PASS: $1"
    else
        echo "FAIL: $1 (want '$2'; got '$3')"
        fails=$((fails + 1))
    fi
}

# report <out-file> <components-json> [<source-json>]
# Writes an upgrade-check report. Omitting <source-json> produces the
# artifact shape, which carries no source block at all.
report() {
    local out="$1" components="$2" source_block="${3:-}"
    {
        echo '{'
        [[ -n "${source_block}" ]] && printf '  "source": %s,\n' "${source_block}"
        printf '  "components": %s,\n' "${components}"
        printf '  "summary": {"components": %s, "failing": 0},\n' \
            "$(jq 'length' <<< "${components}")"
        echo '  "atRisk": {"scanned": false, "reason": "fixture"}'
        echo '}'
    } > "${out}"
}

SOURCE_OK='{"matched": 2, "helm": {"records": 4, "unattributed": 0, "unreadable": 0,
    "uninstalled": 0, "stampedUnmatched": 0}, "argo": {"applications": 0,
    "unattributed": 0, "unreadable": 0}}'
SOURCE_EMPTY='{"matched": 0, "helm": {"records": 0, "unattributed": 0, "unreadable": 0,
    "uninstalled": 0, "stampedUnmatched": 0}, "argo": {"applications": 0,
    "unattributed": 0, "unreadable": 0}}'
SOURCE_STAMPED='{"matched": 2, "helm": {"records": 4, "unattributed": 0, "unreadable": 0,
    "uninstalled": 0, "stampedUnmatched": 3}, "argo": {"applications": 0,
    "unattributed": 0, "unreadable": 0}}'

# One row for a component outside the bundle: a target-only component that
# renders no release (dra-node-labeler on every EKS recipe) is legitimately
# "added" on BOTH sides, so it must not be mistaken for a missed release.
AGREED='[{"component": "dra-node-labeler", "change": "added", "failsRun": false}]'
DIVERGED_VERSION='[{"component": "dra-node-labeler", "change": "added", "failsRun": false},
    {"component": "gpu-operator", "change": "version", "from": "26.7.0", "to": "v26.7.0",
     "verdict": "unknown", "reason": "no-record", "failsRun": true}]'
DIVERGED_VERDICT='[{"component": "dra-node-labeler", "change": "added", "failsRun": false},
    {"component": "gpu-operator", "change": "version", "from": "v26.6.0", "to": "v26.7.0",
     "verdict": "safe", "reason": "recorded", "failsRun": false}]'
ALL_ADDED='[{"component": "dra-node-labeler", "change": "added", "failsRun": false},
    {"component": "gpu-operator", "change": "added", "failsRun": false},
    {"component": "nfd", "change": "added", "failsRun": false}]'

printf '%s\n' gpu-operator nfd > "${WORK}/installed.txt"

# 1. Agreement: identical rows, source block present only on the cluster
# side, atRisk identical-but-irrelevant. Must pass.
report "${WORK}/from-artifact.json" "${AGREED}"
report "${WORK}/from-cluster.json" "${AGREED}" "${SOURCE_OK}"
compare_upgrade_readback "${WORK}/from-artifact.json" "${WORK}/from-cluster.json" \
    "${WORK}/installed.txt" "${WORK}" helm >/dev/null 2>&1
check_rc "agreeing-reports-pass" 0 "$?"

# 2. A version the cluster reads differently from the bundle. This is the
# shape an Argo CD lane produces when the Application's targetRevision is a
# normalized constraint rather than the pinned string.
report "${WORK}/from-cluster.json" "${DIVERGED_VERSION}" "${SOURCE_OK}"
out=$(compare_upgrade_readback "${WORK}/from-artifact.json" "${WORK}/from-cluster.json" \
    "${WORK}/installed.txt" "${WORK}" argocd 2>&1); rc=$?
check_rc "version-divergence-fails" 1 "${rc}"
case "${out}" in
    *gpu-operator*) echo "PASS: version-divergence-names-the-component" ;;
    *) echo "FAIL: version-divergence-names-the-component (diff did not mention gpu-operator)"
       fails=$((fails + 1)) ;;
esac

# 3. Same versions on both sides, different verdict. Verdicts are part of
# what is compared, so this must fail even though no version moved.
report "${WORK}/from-artifact.json" "${DIVERGED_VERDICT}"
report "${WORK}/from-cluster.json" \
    "$(jq -c '(.[] | select(.component == "gpu-operator") | .verdict) = "manual"' \
        <<< "${DIVERGED_VERDICT}")" "${SOURCE_OK}"
compare_upgrade_readback "${WORK}/from-artifact.json" "${WORK}/from-cluster.json" \
    "${WORK}/installed.txt" "${WORK}" helm >/dev/null 2>&1
check_rc "verdict-divergence-fails" 1 "$?"

# 4. The flux failure this whole lane exists to catch: the cluster read
# matches no release, so every installed component reads as new.
report "${WORK}/from-artifact.json" "${AGREED}"
report "${WORK}/from-cluster.json" "${ALL_ADDED}" "${SOURCE_OK}"
out=$(compare_upgrade_readback "${WORK}/from-artifact.json" "${WORK}/from-cluster.json" \
    "${WORK}/installed.txt" "${WORK}" flux 2>&1); rc=$?
check_rc "cluster-reading-installed-as-new-fails" 1 "${rc}"
case "${out}" in
    *"installed by this bundle but read as new"*) echo "PASS: added-row-diagnostic-is-specific" ;;
    *) echo "FAIL: added-row-diagnostic-is-specific"; fails=$((fails + 1)) ;;
esac

# 5. The ambiguous pass: BOTH sides degenerate the same way, so the diff is
# empty and agreement means nothing. Must still fail.
report "${WORK}/from-artifact.json" "${ALL_ADDED}"
report "${WORK}/from-cluster.json" "${ALL_ADDED}" "${SOURCE_OK}"
compare_upgrade_readback "${WORK}/from-artifact.json" "${WORK}/from-cluster.json" \
    "${WORK}/installed.txt" "${WORK}" flux >/dev/null 2>&1
check_rc "both-sides-degenerate-still-fails" 1 "$?"

# 6. A cluster read that matched nothing, even where the rows happen to
# agree: a bare cluster and a broken mapping are indistinguishable in the
# table, and only the source block tells them apart.
report "${WORK}/from-artifact.json" "${AGREED}"
report "${WORK}/from-cluster.json" "${AGREED}" "${SOURCE_EMPTY}"
compare_upgrade_readback "${WORK}/from-artifact.json" "${WORK}/from-cluster.json" \
    "${WORK}/installed.txt" "${WORK}" flux >/dev/null 2>&1
check_rc "zero-matched-fails" 1 "$?"

# 7. AICR-stamped records that map to no component: AICR wrote them and no
# longer recognizes their names.
report "${WORK}/from-cluster.json" "${AGREED}" "${SOURCE_STAMPED}"
compare_upgrade_readback "${WORK}/from-artifact.json" "${WORK}/from-cluster.json" \
    "${WORK}/installed.txt" "${WORK}" helm >/dev/null 2>&1
check_rc "stamped-unmatched-fails" 1 "$?"

# 8. A report with no source block is not a cluster read at all.
report "${WORK}/from-cluster.json" "${AGREED}"
compare_upgrade_readback "${WORK}/from-artifact.json" "${WORK}/from-cluster.json" \
    "${WORK}/installed.txt" "${WORK}" helm >/dev/null 2>&1
check_rc "missing-source-block-fails" 1 "$?"

# 9-12. readback_installed_components: the list every assertion above reads
# as "what both sides must have found".
cat > "${WORK}/recipe.yaml" <<'YAML'
apiVersion: aicr.run/v1
kind: Recipe
componentRefs:
  - name: nfd
    version: 0.19.0
  - name: gpu-operator
    version: v26.7.0
YAML
readback_installed_components "${WORK}/recipe.yaml" "${WORK}/out.txt" >/dev/null 2>&1
check_rc "installed-components-reads-recipe" 0 "$?"
check_eq "installed-components-sorted" "gpu-operator nfd" "$(tr '\n' ' ' < "${WORK}/out.txt" | sed 's/ $//')"

readback_installed_components "${WORK}/absent.yaml" "${WORK}/out.txt" >/dev/null 2>&1
check_rc "installed-components-missing-recipe-fails" 1 "$?"

printf 'apiVersion: aicr.run/v1\nkind: Recipe\ncomponentRefs: []\n' > "${WORK}/empty.yaml"
readback_installed_components "${WORK}/empty.yaml" "${WORK}/out.txt" >/dev/null 2>&1
check_rc "installed-components-empty-recipe-fails" 1 "$?"

if ((fails > 0)); then
    echo "upgrade-readback_test.sh: ${fails} failure(s)"
    exit 1
fi
echo "upgrade-readback_test.sh: all checks passed"
