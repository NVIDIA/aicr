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

# Unit harness for tests/e2e/run.sh's suite-failure plumbing (run_stage).
# Run directly: bash tools/e2e-run_test.sh
# Wired into CI via `make test` (test-shell target).
#
# Hermetic: sources run.sh for its helpers -- the `main` guard at the bottom
# keeps that inert -- and drives run_stage against stub stages. No cluster is
# contacted and no kubectl is invoked; every stage here is a local function.
#
# What is under test is the FAILING path. run.sh runs under `set -e` and
# test_snapshot_run_isolation returns its assertion status, so an unguarded
# call would abort the script at that line: every later stage and, critically,
# cleanup_e2e would be skipped, stranding this run's Jobs, RBAC and fake-GPU
# fixtures on a cluster shared with other CI runs. A harness that only
# exercised the passing path would never see that.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
RUN_SH="${REPO_ROOT}/tests/e2e/run.sh"

if [ ! -f "${RUN_SH}" ]; then
    echo "FAIL: ${RUN_SH} not found" >&2
    exit 1
fi

# Point OUTPUT_DIR at our own temp dir so sourcing run.sh does not mktemp a
# stray directory, and export it so the sourced `${OUTPUT_DIR:-...}` default
# does not fire.
OUTPUT_DIR="$(mktemp -d)"
export OUTPUT_DIR
trap 'rm -rf "${OUTPUT_DIR}"' EXIT

# Source run.sh: the `main` guard keeps it inert, and every top-level statement
# in it is a plain variable assignment.
# shellcheck source=/dev/null
source "${RUN_SH}"
set +e   # run.sh's `set -e` would abort this harness on a failed assertion

FAILED=0
check() { # check <label> <expected> <actual>
    if [ "$2" = "$3" ]; then
        echo "  ok   $1"
    else
        echo "  FAIL $1: expected '$2', got '$3'"
        FAILED=1
    fi
}

# reset_counters puts the sourced suite counters back to a known state so each
# case below asserts an absolute number rather than a running total.
reset_counters() {
    # shellcheck disable=SC2034  # read by pass/fail/print_summary in the sourced run.sh
    TOTAL_TESTS=0
    # shellcheck disable=SC2034  # read by pass/fail/print_summary in the sourced run.sh
    PASSED_TESTS=0
    FAILED_TESTS=0
}

# --- Stub stages ------------------------------------------------------------
# stage_fails_after_recording is the shape test_snapshot_run_isolation has:
# a failed assertion is reported through fail(), then the status is returned.
stage_fails_after_recording() {
    fail "stub/assertion" "simulated assertion failure"
    return 1
}

# stage_fails_silently is the shape an unexpected error takes: nonzero out,
# nothing recorded. run_stage must not let this pass as a green suite.
stage_fails_silently() {
    return 3
}

stage_passes() {
    pass "stub/assertion"
    return 0
}

echo "run_stage, failing stage that recorded its own failure:"
reset_counters
run_stage stage_fails_after_recording >/dev/null
rc=$?
check "returns 0 so \`set -e\` does not unwind main" "0" "${rc}"
check "counts the failure exactly once (no double count)" "1" "${FAILED_TESTS}"
print_summary >/dev/null
check "print_summary reports the suite as failed" "1" "$?"

echo "run_stage, failing stage that recorded nothing:"
reset_counters
run_stage stage_fails_silently >/dev/null
rc=$?
check "returns 0 so \`set -e\` does not unwind main" "0" "${rc}"
check "records a failure under the stage name" "1" "${FAILED_TESTS}"
print_summary >/dev/null
check "print_summary reports the suite as failed" "1" "$?"

echo "run_stage, passing stage:"
reset_counters
run_stage stage_passes >/dev/null
rc=$?
check "returns 0" "0" "${rc}"
check "records no failure" "0" "${FAILED_TESTS}"
print_summary >/dev/null
check "print_summary reports the suite as passed" "0" "$?"

# --- The regression itself --------------------------------------------------
# Reproduce main's shape under `set -e`: a failing stage, then cleanup_e2e.
# Guarded, cleanup must still run. The unguarded control below proves this
# harness can actually see the defect -- without it, both cases would pass on
# a shell where `set -e` never fired.
echo "cleanup reachability under \`set -e\`:"
reset_counters

guarded=$(
    set -e
    run_stage stage_fails_after_recording >/dev/null
    echo "cleanup_e2e-reached"
)
check "a guarded failing stage still reaches cleanup" "cleanup_e2e-reached" "${guarded}"

unguarded=$(
    set -e
    stage_fails_after_recording >/dev/null
    echo "cleanup_e2e-reached"
)
check "control: an unguarded failing stage does NOT" "" "${unguarded}"

# --- Pin the call site ------------------------------------------------------
# The helper is only half the fix; the guard has to be ON the call. Assert it
# straight out of run.sh so an edit that reinstates the bare call fails here.
echo "call site in tests/e2e/run.sh:"
isolation_call="$(grep -c '^ *run_stage test_snapshot_run_isolation$' "${RUN_SH}")"
check "test_snapshot_run_isolation is invoked via run_stage" "1" "${isolation_call}"

bare_call="$(grep -c '^ *test_snapshot_run_isolation$' "${RUN_SH}")"
check "it is never invoked bare" "0" "${bare_call}"

# cleanup_e2e must still be the last thing in that stage list, so the guard
# above has something to reach.
cleanup_call="$(grep -c '^ *cleanup_e2e$' "${RUN_SH}")"
check "cleanup_e2e is still called from main" "1" "${cleanup_call}"

echo ""
if [ "${FAILED}" -ne 0 ]; then
    echo "FAILED"
    exit 1
fi
echo "PASSED"
