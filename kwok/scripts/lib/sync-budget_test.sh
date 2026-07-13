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

# 10-11. Margin-floor guard in the "Derive sync-gate deadline" step
# (action.yml): job_timeout_minutes must leave >= SYNC_BUDGET_FLOOR_SECONDS
# of usable budget after the 240s diagnostics margin, or the step must fail
# fast before toolchain setup + make build burn CI minutes. Extracted
# straight from action.yml (not reimplemented here) so this always exercises
# THIS checkout's actual CI logic, never a stale copy.
ACTION_YML="${REPO_ROOT}/.github/actions/kwok-test/action.yml"

# extract_derive_step <file> -> the dedented run: block of the "Derive
# sync-gate deadline" step.
extract_derive_step() {
    local file="$1"
    awk '
        /^    - name: Derive sync-gate deadline$/ { in_step=1; next }
        in_step && /^      run: \|$/ { in_run=1; next }
        in_run && /^    - name:/ { exit }
        in_run { sub(/^        /, ""); print }
    ' "${file}"
}

# run_derive_step <job_timeout_minutes> -> sets got_rc, got_env_file
# (caller-owned temp file, removed by caller). -eo pipefail mirrors the
# composite step's actual shell (`shell: bash` -> bash -eo pipefail) so a
# future set -e interaction cannot diverge between CI and this harness.
run_derive_step() {
    local jtm="$1"
    got_env_file=$(mktemp)
    JOB_TIMEOUT_MINUTES="${jtm}" GITHUB_ENV="${got_env_file}" \
        bash -eo pipefail -c "$(extract_derive_step "${ACTION_YML}")" > /dev/null 2>&1
    got_rc=$?
}

run_derive_step 5
check "derive-step-below-floor-fails" 1 "" "${got_rc}" ""
rm -f "${got_env_file}"

run_derive_step 6
env_has_deadline="no"
grep -q '^KWOK_SYNC_DEADLINE_EPOCH=' "${got_env_file}" && env_has_deadline="yes"
check "derive-step-at-floor-boundary-succeeds" 0 "yes" "${got_rc}" "${env_has_deadline}"
rm -f "${got_env_file}"

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All 11 tests passed"
