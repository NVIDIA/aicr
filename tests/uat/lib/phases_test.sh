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
# Tests for the snapshot CTRF record in tests/uat/lib/phases.sh (#1806).
# phase_prep is driven with a stub aicr on PATH: pass, fail, interruption by
# TERM while the agent runs, and an unwritable report path. Each case pins the
# phase's exit status and the record on disk, because a wrapper that changed
# the phase's verdict, or lost the record on interruption, would defeat the
# point of adding it.

set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if ! command -v jq >/dev/null 2>&1; then
    if [[ -n "${CI:-}" ]]; then
        echo "FAIL: jq is not installed; the snapshot CTRF record cannot be verified in CI" >&2
        exit 1
    fi
    echo "SKIP: jq is not installed"
    exit 0
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/phases-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT
fails=0
ran=0
pass() { ran=$((ran + 1)); echo "  ok   $1"; }
fail() { ran=$((ran + 1)); echo "FAIL: $1" >&2; fails=$((fails + 1)); }

# Stubs: yq answers the namespace lookup, kubectl is a no-op for the failure
# debug dump, aicr behaves per AICR_STUB_MODE. `recipe` exits 7 so a passing
# snapshot stops phase_prep right after the record, with a recognizable code.
STUB_BIN="${WORK}/bin"; mkdir -p "${STUB_BIN}"
printf '#!/usr/bin/env bash\necho aicr-validation\n' > "${STUB_BIN}/yq"
printf '#!/usr/bin/env bash\nexit 0\n' > "${STUB_BIN}/kubectl"
cat > "${STUB_BIN}/aicr" <<'STUB'
#!/usr/bin/env bash
case "$1" in
    snapshot)
        case "${AICR_STUB_MODE:-pass}" in
            pass) : > snapshot.yaml; exit 0 ;;
            fail) echo "stub: agent pod never became ready" >&2; exit 3 ;;
            nofile) exit 0 ;;
            # The sleep stays a CHILD of this stub, and `wait` keeps the stub
            # in the foreground, because that grandchild is the point: the
            # phase tears the tree down by signalling the snapshot's process
            # group (phases.sh, uat_snapshot_on_signal), and only a child can
            # show that the group -- not just the direct process -- was
            # reached. Backgrounding does not create a new process group here,
            # job control being off in a non-interactive shell, so the group
            # signal still reaches it.
            #
            # Its PID is recorded so the test can follow this exact process. A
            # command-line pattern cannot: it matches any process on the
            # machine, including one an earlier run left behind.
            hang) touch "${AICR_STUB_STARTED:?}"
                  sleep 2149 & echo $! > "${AICR_STUB_STARTED}.pid"; wait $! ;;
        esac ;;
    recipe) exit 7 ;;
    *) exit 0 ;;
esac
STUB
chmod +x "${STUB_BIN}"/*
export PATH="${STUB_BIN}:${PATH}"
export AICR_BIN="${STUB_BIN}/aicr"

# run_prep <case-dir> [env...]: runs phase_prep in a subshell from a fresh
# directory so snapshot-result.json lands there; prints nothing, returns rc.
# Callers must NOT invoke it as `run_prep ... || rc=$?`: a function in an ||
# list runs with errexit suppressed, subshell included, and the phase would
# then continue past a failing step exactly as it never does under the real
# runner. Call it plainly and read $? on the next line.
run_prep() {
    local dir="$1"; shift
    mkdir -p "${dir}"
    (
        cd "${dir}" || exit 99
        # This subshell is the process a signal must reach. Publish its PID so
        # the interruption case below need not go looking for it: deriving it
        # with `pgrep -P` fails wherever process enumeration is restricted
        # (macOS sandboxes return "Cannot get process list"), and the empty
        # result there silently retargets the signal at the wrong process.
        echo "${BASHPID}" > "${dir}/phase.pid"
        # The per-cloud runners run the phase library under set -euo pipefail.
        set -euo pipefail
        local kv; for kv in "$@"; do export "${kv?}"; done
        # shellcheck source=tests/uat/lib/phases.sh
        source "${SCRIPT_DIR}/phases.sh"
        config="${dir}/config.yaml"; : > "${config}"
        phase_prep
    ) > "${dir}/log" 2>&1
}

record_ok() { # <dir> <jq filter>
    jq -e "$2" "$1/snapshot-result.json" >/dev/null 2>&1
}

# --- pass ----------------------------------------------------------------
d="${WORK}/pass"; run_prep "${d}" AICR_STUB_MODE=pass; rc=$?
[[ "${rc}" == 7 ]] && pass "passing snapshot continues into the recipe step" || { fail "passing snapshot: want rc 7 (stub recipe), got ${rc}"; sed "s/^/    | /" "${d}/log" >&2; }
record_ok "${d}" '.results.summary.passed == 1 and .results.tests[0].name == "snapshot" and .results.tests[0].status == "passed"' \
    && pass "passing snapshot records passed" || fail "passing snapshot record wrong: $(cat "${d}/snapshot-result.json" 2>/dev/null)"

# --- fail ----------------------------------------------------------------
d="${WORK}/fail"; run_prep "${d}" AICR_STUB_MODE=fail; rc=$?
[[ "${rc}" == 1 ]] && pass "failing snapshot exits 1" || fail "failing snapshot: want rc 1, got ${rc}"
record_ok "${d}" '.results.summary.failed == 1 and (.results.tests[0].message | test("rc=3"))' \
    && pass "failing snapshot records failed with the agent rc" || fail "failing snapshot record wrong: $(cat "${d}/snapshot-result.json" 2>/dev/null)"
grep -q "Snapshot failure debug" "${d}/log" && pass "failing snapshot still runs the debug dump" || fail "debug dump missing on failure"

# --- exit 0 without snapshot.yaml ----------------------------------------
d="${WORK}/nofile"; run_prep "${d}" AICR_STUB_MODE=nofile; rc=$?
[[ "${rc}" == 1 ]] && pass "missing snapshot.yaml is a failure" || fail "missing snapshot.yaml: want rc 1, got ${rc}"
record_ok "${d}" '.results.summary.failed == 1' && pass "missing snapshot.yaml records failed" || fail "missing snapshot.yaml record wrong"

# --- interrupted by TERM -------------------------------------------------
d="${WORK}/hang"; mkdir -p "${d}"; started="${d}/started"
run_prep "${d}" AICR_STUB_MODE=hang AICR_STUB_STARTED="${started}" &
prep_pid=$!
for _ in $(seq 1 100); do [[ -f "${started}" ]] && break; sleep 0.2; done
if [[ ! -f "${started}" ]]; then
    fail "interrupted snapshot: stub never started"; kill "${prep_pid}" 2>/dev/null || true
else
    # run_prep is a function in this shell; the phase runs in its subshell,
    # which is the process that must receive the signal. Both PIDs are read
    # from files the processes wrote themselves. Deriving the phase's with
    # `pgrep -P` fails wherever process enumeration is restricted -- a macOS
    # sandbox answers "Cannot get process list" -- and because that only
    # produced an empty string, the signal silently went to this shell's child
    # instead, leaving the agent behind to fail some later run.
    phase_pid="$(cat "${d}/phase.pid" 2>/dev/null || true)"
    agent_pid="$(cat "${started}.pid" 2>/dev/null || true)"
    kill -TERM "${phase_pid:-${prep_pid}}"
    rc=0; wait "${prep_pid}" || rc=$?
    [[ "${rc}" == 143 ]] && pass "interrupted snapshot terminates with 143" || fail "interrupted snapshot: want rc 143, got ${rc}"
    # Wait for the teardown instead of sampling once after a fixed sleep:
    # signal delivery and reaping are asynchronous, so a fixed delay is a
    # guess. `kill -0` asks about the one process this run started, so a
    # stray sleep belonging to anything else cannot answer for it.
    if [[ -z "${agent_pid}" ]]; then
        fail "interrupted snapshot: agent never recorded its PID"
    else
        for _ in $(seq 1 100); do kill -0 "${agent_pid}" 2>/dev/null || break; sleep 0.2; done
        if kill -0 "${agent_pid}" 2>/dev/null; then
            fail "interrupted snapshot left the agent's child process running"
        else
            pass "interrupted snapshot stops the agent's process tree"
        fi
    fi
    # The record is written by the phase's trap, after the signal arrives.
    for _ in $(seq 1 100); do [[ -s "${d}/snapshot-result.json" ]] && break; sleep 0.2; done
    record_ok "${d}" '.results.summary.other == 1 and (.results.tests[0].message | test("interrupted by SIGTERM"))' \
        && pass "interrupted snapshot records other" || fail "interrupted snapshot record wrong: $(cat "${d}/snapshot-result.json" 2>/dev/null)"
fi
# Belt and braces if an assertion above failed. Scoped to this run's PID: the
# old `pkill -f "sleep 2149"` would also have killed another run's agent.
[[ -n "${agent_pid:-}" ]] && kill "${agent_pid}" 2>/dev/null || true

# --- unwritable report ---------------------------------------------------
d="${WORK}/unwritable"; mkdir -p "${d}/snapshot-result.json"   # a directory blocks the write
run_prep "${d}" AICR_STUB_MODE=pass; rc=$?
[[ "${rc}" == 7 ]] && pass "unwritable report does not change the phase outcome" || fail "unwritable report: want rc 7, got ${rc}"
grep -q "failed to write snapshot-result.json" "${d}/log" && pass "unwritable report is logged as a warning" || fail "unwritable report warning missing"

if (( fails > 0 )); then
    echo "${fails} test(s) failed (${ran} attempted)" >&2
    exit 1
fi
echo "All ${ran} phases_test cases passed"
