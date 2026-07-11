#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Unit harness for lib/sync-budget.sh (deadline-derived sync-gate budgets).
# Run directly: bash kwok/scripts/lib/sync-budget_test.sh
# Wired into CI by the kwok-recipes discover job.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Resolve the subject SCRIPT_DIR-relative — never a deployed copy.
# shellcheck source=sync-budget.sh
source "${SCRIPT_DIR}/sync-budget.sh"

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

# 1. Env unset -> default passthrough (local runs keep fixed budgets).
unset KWOK_SYNC_DEADLINE_EPOCH
out=$(compute_sync_budget 500 1000000); rc=$?
check "env-unset-returns-default" 0 "500" "${rc}" "${out}"

# 2. Ample remaining (10000s) -> default wins the min().
export KWOK_SYNC_DEADLINE_EPOCH=1010000
out=$(compute_sync_budget 500 1000000); rc=$?
check "ample-remaining-returns-default" 0 "500" "${rc}" "${out}"

# 3. Tight remaining (300s < 500s default) -> budget shrinks to remaining.
export KWOK_SYNC_DEADLINE_EPOCH=1000300
out=$(compute_sync_budget 500 1000000); rc=$?
check "tight-remaining-shrinks-budget" 0 "300" "${rc}" "${out}"

# 4. Remaining exactly at the 120s floor -> still runs (boundary).
export KWOK_SYNC_DEADLINE_EPOCH=1000120
out=$(compute_sync_budget 500 1000000); rc=$?
check "floor-boundary-runs" 0 "120" "${rc}" "${out}"

# 5. Remaining below floor (119s) -> rc 1, no output (caller fails fast).
export KWOK_SYNC_DEADLINE_EPOCH=1000119
out=$(compute_sync_budget 500 1000000); rc=$?
check "below-floor-fails" 1 "" "${rc}" "${out}"

# 6. Deadline already in the past -> rc 1, no output.
export KWOK_SYNC_DEADLINE_EPOCH=999000
out=$(compute_sync_budget 500 1000000); rc=$?
check "deadline-past-fails" 1 "" "${rc}" "${out}"

# 7. Literal-sync guard: job_timeout_minutes must equal the SAME job's
# timeout-minutes in every caller workflow. This is a hand-synced literal
# (the composite action cannot read the calling job's own timeout-minutes);
# drift silently re-creates the CANCELLED-without-diagnostics failure this
# whole mechanism exists to prevent. Resolve workflows SCRIPT_DIR-relative
# so this always tests THIS checkout, never a deployed copy.
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# job_timeout_sync <workflow_file> -> one "<job>:jtm=<X>,tm=<Y>" line per
# job that sets job_timeout_minutes, where <X> is that value and <Y> is
# the nearest preceding job-level (4-space-indented) timeout-minutes
# ("unset" if none was seen for the current job).
job_timeout_sync() {
    local file="$1" job="" job_timeout=""
    while IFS= read -r line; do
        if [[ "${line}" =~ ^\ \ ([A-Za-z0-9_-]+):[[:space:]]*$ ]]; then
            job="${BASH_REMATCH[1]}"
            job_timeout=""
            continue
        fi
        if [[ "${line}" =~ ^\ \ \ \ timeout-minutes:\ *([0-9]+)[[:space:]]*$ ]]; then
            job_timeout="${BASH_REMATCH[1]}"
            continue
        fi
        if [[ "${line}" =~ job_timeout_minutes:\ *\'?([0-9]+)\'? ]]; then
            echo "${job}:jtm=${BASH_REMATCH[1]},tm=${job_timeout:-unset}"
        fi
    done < "${file}"
}

kwok_recipes_out=$(job_timeout_sync "${REPO_ROOT}/.github/workflows/kwok-recipes.yaml")
check "kwok-recipes-tier1-job-timeout-in-sync" 0 "test-tier1:jtm=18,tm=18" \
    0 "$(echo "${kwok_recipes_out}" | grep '^test-tier1:')"
check "kwok-recipes-tier2-job-timeout-in-sync" 0 "test-tier2:jtm=18,tm=18" \
    0 "$(echo "${kwok_recipes_out}" | grep '^test-tier2:')"

tier3_shard_out=$(job_timeout_sync "${REPO_ROOT}/.github/workflows/kwok-tier3-shard.yaml")
check "kwok-tier3-shard-job-timeout-in-sync" 0 "test:jtm=18,tm=18" \
    0 "$(echo "${tier3_shard_out}" | grep '^test:')"

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All 9 tests passed"
