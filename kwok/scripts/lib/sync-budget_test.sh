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

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All 6 tests passed"
