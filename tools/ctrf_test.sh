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
# Tests for tools/ctrf, the shell CTRF emitter shared by the UAT phase library
# and the KWOK batch driver. A report whose summary disagrees with its tests, or
# an emitter that accepts a bogus status, would let a harness publish a green
# artifact for a red run, so every case pins the exact shape.

set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if ! command -v jq >/dev/null 2>&1; then
    if [[ -n "${CI:-}" ]]; then
        echo "FAIL: jq is not installed; tools/ctrf cannot be verified in CI" >&2
        exit 1
    fi
    echo "SKIP: jq is not installed"
    exit 0
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/ctrf-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT
fails=0

pass() { echo "  ok   $1"; }
fail() { echo "FAIL: $1" >&2; fails=$((fails + 1)); }

# shellcheck source=tools/ctrf
. "${SCRIPT_DIR}/ctrf"

# --- a full report -----------------------------------------------------------
ctrf_init "ctrf-test" "v0-test"
ctrf_add "alpha" passed 1200 || fail "ctrf_add alpha returned non-zero"
ctrf_add "beta" failed 300 "boom: exit 2" || fail "ctrf_add beta returned non-zero"
ctrf_add "gamma" skipped 0 || fail "ctrf_add gamma returned non-zero"
ctrf_write "${WORK}/nested/dir/report.json" || fail "ctrf_write returned non-zero"

if [[ -f "${WORK}/nested/dir/report.json" ]]; then
    pass "ctrf_write creates the parent directory"
else
    fail "ctrf_write did not create ${WORK}/nested/dir/report.json"
fi

check_jq() { # <description> <jq filter that must evaluate to true>
    if jq -e "$2" "${WORK}/nested/dir/report.json" >/dev/null 2>&1; then
        pass "$1"
    else
        fail "$1 -- filter: $2"
    fi
}
check_jq "reportFormat is CTRF"                '.reportFormat == "CTRF"'
check_jq "specVersion matches pkg/validator/ctrf" '.specVersion == "0.0.1"'
check_jq "generatedBy and tool.name carry the tool" '.generatedBy == "ctrf-test" and .results.tool.name == "ctrf-test"'
check_jq "tool.version is recorded when given"  '.results.tool.version == "v0-test"'
check_jq "timestamp is RFC 3339 UTC"            '.timestamp | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")'
check_jq "summary.tests equals the tests array" '.results.summary.tests == (.results.tests | length) and .results.summary.tests == 3'
check_jq "summary counts each status"           '.results.summary.passed == 1 and .results.summary.failed == 1 and .results.summary.skipped == 1 and .results.summary.pending == 0 and .results.summary.other == 0'
check_jq "summary.start is not after summary.stop" '.results.summary.start <= .results.summary.stop and (.results.summary.start | type) == "number"'
check_jq "duration is an integer"               '.results.tests[0].duration == 1200 and (.results.tests[0].duration | type) == "number"'
check_jq "message is kept when given"           '.results.tests[1].message == "boom: exit 2"'
check_jq "message is omitted when empty"        '.results.tests[0] | has("message") | not'
check_jq "test order is insertion order"        '[.results.tests[].name] == ["alpha","beta","gamma"]'

# --- rewriting the same path replaces, never appends ---------------------------
ctrf_add "delta" passed 5 || fail "ctrf_add delta returned non-zero"
ctrf_write "${WORK}/nested/dir/report.json" || fail "second ctrf_write returned non-zero"
check_jq "rewrite reflects every test recorded since init" '.results.summary.tests == 4 and .results.summary.passed == 2'

# --- tool.version is optional ---------------------------------------------------
ctrf_init "ctrf-test"
ctrf_add "only" passed 1
ctrf_write "${WORK}/noversion.json"
if jq -e '.results.tool | has("version") | not' "${WORK}/noversion.json" >/dev/null; then
    pass "tool.version is omitted when not given"
else
    fail "tool.version should be omitted when ctrf_init gets no version"
fi
if jq -e '.results.summary.tests == 1' "${WORK}/noversion.json" >/dev/null; then
    pass "ctrf_init discards tests from the previous report"
else
    fail "ctrf_init did not reset the recorded tests"
fi

# --- rejections ----------------------------------------------------------------
ctrf_init "ctrf-test"
if ctrf_add "bad" flaky 1 2>/dev/null; then
    fail "ctrf_add accepted an invalid status"
else
    pass "ctrf_add rejects an invalid status"
fi
if ctrf_add "bad" passed 1.5 2>/dev/null; then
    fail "ctrf_add accepted a non-integer duration"
else
    pass "ctrf_add rejects a non-integer duration"
fi
if ctrf_add "bad" passed -1 2>/dev/null; then
    fail "ctrf_add accepted a negative duration"
else
    pass "ctrf_add rejects a negative duration"
fi
if jq -e '. == []' <<<"${CTRF_TESTS}" >/dev/null; then
    pass "rejected results are not recorded"
else
    fail "a rejected ctrf_add mutated the recorded tests"
fi

CTRF_START_MS=""
if ctrf_add "early" passed 1 2>/dev/null; then
    fail "ctrf_add succeeded before ctrf_init"
else
    pass "ctrf_add fails before ctrf_init"
fi
if ctrf_write "${WORK}/early.json" 2>/dev/null; then
    fail "ctrf_write succeeded before ctrf_init"
else
    pass "ctrf_write fails before ctrf_init"
fi

# --- helpers -----------------------------------------------------------------
start="$(ctrf_now_ms)"
elapsed="$(ctrf_elapsed_ms "${start}")"
if [[ "${elapsed}" =~ ^[0-9]+$ ]]; then
    pass "ctrf_elapsed_ms returns a non-negative integer"
else
    fail "ctrf_elapsed_ms returned '${elapsed}'"
fi

if (( fails > 0 )); then
    echo "${fails} test(s) failed" >&2
    exit 1
fi
echo "All tools/ctrf tests passed"
