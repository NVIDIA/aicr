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

# Unit harness for the footprint GATE in .github/scripts/record-footprint.sh.
# Run directly: bash tools/record-footprint_test.sh
# Wired into CI via `make test` (test-shell target, runs tools/*_test.sh).
#
# WHY A GATE NEEDS A HARNESS. The script's first job is to record numbers, and
# a recorder cannot be wrong in a way anyone notices. Its second job is to fail
# the step when the runner is about to run out of room, and that job is only
# real if it can go RED. The assertions below drive each arm over its floor by
# injecting figures through a PATH stub, so the disk arm is exercised without
# ever filling a disk and the memory arm without a Linux host.
#
# Every expectation names the EXACT documented exit code, never merely
# non-zero: 0 within floors, 1 usage or bad configuration, 2 disk below floor,
# 3 memory below floor, 4 memory arm required but unmeasurable, 5 df figures
# unparseable. "Non-zero" would let a syntax error masquerade as a caught
# over-budget run.
#
# Hermetic with one deliberate exception, the "real machine" case below, which
# is the only proof that the host's own df and uname work at all; it asserts
# what holds on any host rather than that the host has headroom. No cluster, no
# network, no real disk pressure anywhere. The subject is
# resolved relative to THIS file so the harness exercises this worktree rather
# than whatever copy happens to be on PATH.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SUBJECT="${REPO_ROOT}/.github/scripts/record-footprint.sh"

if [[ ! -f "${SUBJECT}" ]]; then
    echo "FAIL: ${SUBJECT} does not exist; this suite is asserting against nothing" >&2
    exit 1
fi

# Template form rather than a bare `mktemp -d`: BSD mktemp only honours
# TMPDIR when given one, and a harness that cannot place its stubs where the
# caller asked is a harness that silently runs with no stubs at all.
STUB_DIR="$(mktemp -d "${TMPDIR:-/tmp}/record-footprint-test.XXXXXX")"
trap 'rm -rf "${STUB_DIR}"' EXIT

# A stub directory that was never created leaves every stubbed case running
# UNSTUBBED against the host, where most of them pass for the wrong reason.
if [[ -z "${STUB_DIR}" || ! -d "${STUB_DIR}" ]]; then
    echo "FAIL: could not create a stub directory under ${TMPDIR:-/tmp}" >&2
    exit 1
fi

REAL_DF="$(command -v df)"
REAL_UNAME="$(command -v uname)"
export REAL_DF REAL_UNAME

# --- Stub `df` on PATH --------------------------------------------------------
# Inert unless DF_STUB is set, so the "real machine" case below runs against the
# host's actual filesystem. Figures are 1K blocks, the unit `df -Pk` reports.
# The floor is 10% of the reported total, so with a 77000000K (73.4 GiB) total
# the floor sits at 7700000K (7.3 GiB).
cat >"${STUB_DIR}/df" <<'STUB'
#!/usr/bin/env bash
if [[ -z "${DF_STUB:-}" ]]; then exec "${REAL_DF}" "$@"; fi
echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
case "${DF_STUB}" in
  low)          echo "/dev/fake 77000000 74000000 3000000 97% /" ;;
  healthy)      echo "/dev/fake 77000000 47000000 30000000 62% /" ;;
  at_floor)     echo "/dev/fake 77000000 69300000 7700000 90% /" ;;
  below_floor)  echo "/dev/fake 77000000 69300001 7699999 90% /" ;;
  garbage)      echo "/dev/fake - - - - /" ;;
  # Numeric and well formed, but a total of zero. This is the ONLY fixture that
  # reaches the `total_k -le 0` clause: `garbage` is rejected by the regex arm
  # one clause earlier, so without this the zero-total guard has no case at all.
  zero_total)   echo "/dev/fake 0 0 0 100% /" ;;
  # One unreadable field each, the other sound. `garbage` sets BOTH, so it fires
  # whichever clause is asked first and proves only that the disjunction works,
  # never that either half of it does.
  garbage_total) echo "/dev/fake - 47000000 30000000 62% /" ;;
  garbage_avail) echo "/dev/fake 77000000 47000000 - 62% /" ;;
  *) echo "df stub: unknown DF_STUB '${DF_STUB}'" >&2; exit 64 ;;
esac
STUB
chmod +x "${STUB_DIR}/df"

# --- Stub `uname` on PATH -----------------------------------------------------
# The memory arm's applicability hinges on whether this host is Linux. Stubbing
# `uname` lets both branches of that decision be asserted on any host, instead
# of one of them being whatever the developer happens to be sitting at.
cat >"${STUB_DIR}/uname" <<'STUB'
#!/usr/bin/env bash
if [[ -z "${UNAME_STUB:-}" ]]; then exec "${REAL_UNAME}" "$@"; fi
echo "${UNAME_STUB}"
STUB
chmod +x "${STUB_DIR}/uname"

# --- /proc/meminfo fixtures ---------------------------------------------------
# Totals chosen to avoid a %.2f rounding tie, so the asserted strings are the
# same on every libc. 16000000K total => 15.26 GiB; the 10% floor is 1600000K
# => 1.53 GiB.
cat >"${STUB_DIR}/meminfo-low" <<'FIXTURE'
MemTotal:       16000000 kB
MemFree:          400000 kB
MemAvailable:    1000000 kB
FIXTURE

cat >"${STUB_DIR}/meminfo-healthy" <<'FIXTURE'
MemTotal:       16000000 kB
MemFree:         6000000 kB
MemAvailable:    8000000 kB
FIXTURE

# Present and readable, but the field the gate needs is absent. This is the
# ambiguous condition the project's anti-pattern list is about: it must fail,
# not pass, and not be reported as "n/a, arm skipped".
cat >"${STUB_DIR}/meminfo-no-available" <<'FIXTURE'
MemTotal:       16000000 kB
MemFree:          400000 kB
FIXTURE

# Both fields present and numeric, and the total is zero. The sibling above is
# caught by the regex arm on an EMPTY capture, so it never exercises the
# `mem_total_k -gt 0` clause; this one does, and it is also the input that makes
# the floor arithmetic degenerate (a 10% floor of zero, which nothing is below).
cat >"${STUB_DIR}/meminfo-zero-total" <<'FIXTURE'
MemTotal:              0 kB
MemFree:               0 kB
MemAvailable:          0 kB
FIXTURE

# One kB either side of the memory floor, and exactly on it. 16000000K total
# puts the 10% floor at 1600000K, so these three pin the comparison the way the
# at_floor/below_floor df fixtures pin the disk one.
cat >"${STUB_DIR}/meminfo-at-floor" <<'FIXTURE'
MemTotal:       16000000 kB
MemFree:          400000 kB
MemAvailable:    1600000 kB
FIXTURE

cat >"${STUB_DIR}/meminfo-under-floor" <<'FIXTURE'
MemTotal:       16000000 kB
MemFree:          400000 kB
MemAvailable:    1599999 kB
FIXTURE

cat >"${STUB_DIR}/meminfo-over-floor" <<'FIXTURE'
MemTotal:       16000000 kB
MemFree:          400000 kB
MemAvailable:    1600001 kB
FIXTURE

# The stubs are the whole instrument. If PATH resolution does not reach them,
# the cases below measure the host and report a cheerful green.
if [[ "$(UNAME_STUB=Linux PATH="${STUB_DIR}:${PATH}" uname -s)" != "Linux" ]]; then
    echo "FAIL: the PATH stubs are inert; every assertion below would test the host" >&2
    exit 1
fi

fail=0
check() {
    local desc="$1" want="$2" got="$3"
    if [[ "${want}" != "${got}" ]]; then
        echo "FAIL: ${desc}: want '${want}', got '${got}'" >&2
        fail=1
    else
        echo "ok: ${desc}"
    fi
}

contains() {
    local desc="$1" needle="$2" haystack="$3"
    case "${haystack}" in
        *"${needle}"*) echo "ok: ${desc}" ;;
        *) echo "FAIL: ${desc}: output does not contain '${needle}'" >&2
           echo "  --- captured output ---" >&2
           printf '%s\n' "${haystack}" >&2
           fail=1 ;;
    esac
}

RC=0
OUT=""
run_subject() {
    OUT="$(PATH="${STUB_DIR}:${PATH}" bash "${SUBJECT}" "$@" 2>&1)"
    RC=$?
}

reset_env() {
    unset DF_STUB UNAME_STUB AICR_FOOTPRINT_MEMINFO \
        AICR_FOOTPRINT_MIN_DISK_PCT AICR_FOOTPRINT_MIN_MEM_PCT GITHUB_STEP_SUMMARY
}

# --- Usage --------------------------------------------------------------------
reset_env
run_subject
check "no stage label is a usage error (exit 1)" "1" "${RC}"

# --- The developer machine must keep working ----------------------------------
# The header promises this script runs unchanged on a laptop. A gate that fails
# there would be reverted within a day, so this is a load-bearing assertion and
# not a smoke test.
# THE ONE CASE THAT KEEPS ITS STUBS OFF, and what it may therefore assert.
# Everything else here stubs df, so this is the only place that proves the
# HOST's own df accepts `-Pk` at all (BSD and GNU df are different binaries) and
# that the script survives end to end with nothing injected.
#
# What it may NOT assert is exit 0. That is a property of the machine, not of
# the script: a runner already under its 10% disk floor exits 2, correctly, and
# the suite would call the script broken. Asserted instead is what holds on ANY
# host: df's output parsed into three figures, and the exit code is one the
# script reaches by MEASURING. 5 says this host's df output could not be read,
# 1 says the stage label never arrived, and 4 says a required memory arm had
# nothing readable to measure; none can be true here, so all three are failures.
# 0, 2 and 3 are all honest answers about the host's own free space: 3 is the
# memory arm's 2, reached on a Linux host under its memory floor, and rejecting
# it would put back the same host dependency in the other arm.
assert_real_measurement() {
    local desc="$1" stage="$2" rc="$3" out="$4"
    local re="footprint \\[${stage}\\] disk: ([0-9]+\\.[0-9]) GiB used, ([0-9]+\\.[0-9]) GiB free of ([0-9]+\\.[0-9]) GiB"
    if ! [[ "${out}" =~ $re ]]; then
        echo "FAIL: ${desc}: no recording line with three parsed disk figures" >&2
        echo "  --- captured output ---" >&2
        printf '%s\n' "${out}" >&2
        fail=1
        return
    fi
    # A total of 0.0 GiB is what an unread df looks like after formatting: the
    # figures are well formed and mean nothing. No real root filesystem is that
    # small, so this separates "parsed" from "parsed something".
    if [[ "${BASH_REMATCH[3]}" == "0.0" ]]; then
        echo "FAIL: ${desc}: the reported total is 0.0 GiB, so df was not actually read" >&2
        fail=1
        return
    fi
    case "${rc}" in
        0|2|3) echo "ok: ${desc} (exit ${rc}, total ${BASH_REMATCH[3]} GiB)" ;;
        *)   echo "FAIL: ${desc}: exit ${rc}, which is neither of the codes a measured run reaches" >&2
             printf '%s\n' "${out}" >&2
             fail=1 ;;
    esac
}

reset_env
run_subject "real machine"
assert_real_measurement "real df + real uname on this host parses and measures" \
    "real machine" "${RC}" "${OUT}"

# --- Disk arm -----------------------------------------------------------------
# EVERY CASE IN THIS SECTION PINS THE MEMORY ARM TO A HEALTHY FIXTURE, which is
# the mirror of the memory section stubbing df healthy. `reset_env` clears
# AICR_FOOTPRINT_MEMINFO, so without this the arm falls back to /proc/meminfo
# and, on Linux, `uname` alone makes it mandatory: a runner already under its
# 10% memory floor then returns 3 from a case whose assertion reads as a
# statement about the disk arm. Measured rather than supposed: with a
# /proc/meminfo injected at 6.25% available, ubuntu:24.04 gave 7 FAILs, four of
# them here, and 0 with this pinned. Nothing is lost, because the memory arm
# keeps its own fixtures below.
#
# The override also makes the memory arm MANDATORY (the script says so in the
# log), so these cases cannot pass by the arm quietly not applying either.
reset_env
export DF_STUB=low
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "cluster + GPU simulation"
check "disk free below the floor exits 2" "2" "${RC}"
contains "the disk error names the stage" "[cluster + GPU simulation]" "${OUT}"
contains "the disk error names the shortfall" "short by 4.5 GiB" "${OUT}"
contains "the disk error names the measured free space" "disk free 2.9 GiB" "${OUT}"
contains "the disk error names the floor it missed" "floor of 7.3 GiB" "${OUT}"
# Constraint 3: the gate is additive. Losing the diagnostic on the one run that
# matters would be a net loss even though the gate itself worked.
contains "the recording line survives a failing gate" "footprint [cluster + GPU simulation] disk:" "${OUT}"

reset_env
export DF_STUB=healthy
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "healthy disk"
check "disk free above the floor exits 0" "0" "${RC}"
# The PASS line is the only output a healthy run produces beyond the recording,
# and an exit code alone cannot tell a gate that passed from a gate that went
# silent. Its memory clause is host-dependent, so only the stable half is named.
contains "the passing gate announces itself" "footprint gate [healthy disk] passed:" "${OUT}"
contains "the passing gate names the free space and the floor it cleared" \
    "disk free 28.6 GiB is at or above the 10% floor of 7.3 GiB" "${OUT}"

# The two assertions that make the green above worth something: one block
# either side of the floor, so a comparison that drifts by one is caught.
reset_env
export DF_STUB=at_floor
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "disk exactly at the floor"
check "disk free exactly at the floor exits 0" "0" "${RC}"

reset_env
export DF_STUB=below_floor
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "disk one block under the floor"
check "disk free one block under the floor exits 2" "2" "${RC}"

reset_env
export DF_STUB=garbage
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "unparseable df"
check "df figures that will not parse exit 5, not 0" "5" "${RC}"
contains "the unparseable-df error refuses to claim a pass" "unmeasured" "${OUT}"

# A total of zero parses as a number, so only the explicit `-le 0` clause stands
# between it and a calm "0.0 GiB free of 0.0 GiB" pass: the floor works out to
# zero and nothing is below zero, so the gate would report success on a
# filesystem it never measured.
reset_env
export DF_STUB=zero_total
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "df reporting a zero total"
check "a zero df total exits 5, not a pass on a 0.0 GiB filesystem" "5" "${RC}"
contains "the zero-total error quotes the figures it rejected" "total='0'" "${OUT}"

# One field at a time, because the guard is a three-clause disjunction and the
# `garbage` fixture above satisfies all three at once: delete any single clause
# and `garbage` is still caught by the others, so each clause individually was
# unasserted. What each one is holding back, measured by deleting it:
#   total unreadable  -> the arithmetic below aborts on the operand, exit 1
#   avail unreadable  -> "disk free 0.0 GiB is at or above the 10% floor", EXIT 0
# The second is the one that matters: a pass reported on a filesystem the script
# never measured, which is the exact sentence the guard's own comment predicts.
reset_env
export DF_STUB=garbage_total
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "df total that will not parse"
check "an unreadable df total exits 5" "5" "${RC}"
contains "the error names the total it could not read" "total='-'" "${OUT}"

reset_env
export DF_STUB=garbage_avail
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "df available that will not parse"
check "an unreadable df available exits 5, not a pass on 0.0 GiB free" "5" "${RC}"
contains "the error names the available figure it could not read" "avail='-'" "${OUT}"

# --- the stub's own integrity -------------------------------------------------
#
# Every case above trusts that its DF_STUB value names a real fixture. The stub
# emitted its header BEFORE the `case` that validates the name, so a mistyped
# value still printed a well-formed header on stdout and exited 64. The subject
# reads `df -Pk / | tail -1`, so it got the HEADER and reported exit 5, figures
# unparseable: the same code the four cases above assert on purpose. A typo in
# any of their fixture names would have passed, for entirely the wrong reason.
#
# The first check is the property directly: an unknown name produces no stdout
# at all. The second is the consequence, that such a run stays distinguishable
# from the unparseable-figures case it used to impersonate.
reset_env
stub_out="$(PATH="${STUB_DIR}:${PATH}" DF_STUB=not_a_real_fixture df -Pk / 2>/dev/null)"
stub_rc=$?
check "a mistyped DF_STUB writes nothing to stdout" "" "${stub_out}"
check "a mistyped DF_STUB fails closed with the stub's own code" "64" "${stub_rc}"

reset_env
export DF_STUB=not_a_real_fixture
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "mistyped df fixture"
check "a mistyped DF_STUB is not mistakable for the unparseable-figures case" \
    "distinct-from-5" \
    "$([[ "${RC}" -ne 5 ]] && echo distinct-from-5 || echo "reported-exit-5-for-a-harness-typo")"

# --- Memory arm: applicability ------------------------------------------------
# Not Linux and no explicit source: the arm does not apply. It must record n/a
# and pass, which is the developer case, and the n/a is the evidence that it was
# SKIPPED rather than silently judged.
#
# EVERY CASE FROM HERE ON STUBS df HEALTHY. The disk arm takes precedence in the
# exit code, so on a machine already under its own 10% disk floor each of these
# returns 2 and a memory assertion reads as a failure of the memory arm.
# Measured rather than supposed: with the meminfo-low fixture two sections down,
# a healthy host gives the asserted 3 and a host under its floor gives 2.
# Nothing is lost by stubbing, because the disk arm keeps its own fixtures above.
reset_env
export UNAME_STUB=Darwin
export DF_STUB=healthy
run_subject "not linux"
check "a non-Linux host skips the memory arm and exits 0" "0" "${RC}"
contains "the skipped memory arm records n/a" "memory: n/a GiB used, n/a GiB available" "${OUT}"

# Linux: the arm is mandatory. Either the host really can read /proc/meminfo, in
# which case the arm must RUN (no n/a), or it cannot, in which case the gate must
# refuse to pass. Both branches are asserted; neither is a silent skip.
reset_env
export UNAME_STUB=Linux
export DF_STUB=healthy
run_subject "linux host"
if [[ -r /proc/meminfo ]]; then
    # Not exit 0: this case reads the HOST's real /proc/meminfo, so 0 would be a
    # statement about the runner's free memory rather than about the arm. 3 is
    # the same arm reporting a host under its floor, and is equally a measured
    # answer; 4 is the one code that would mean it did not measure, so it stays
    # a failure and the else-branch below asserts it directly.
    case "${RC}" in
        0|3) echo "ok: on a Linux host with readable meminfo the memory arm measures (exit ${RC})" ;;
        *)   echo "FAIL: on a Linux host with readable meminfo the memory arm runs: exit ${RC}, which is neither code a measured arm reaches" >&2
             printf '%s\n' "${OUT}" >&2
             fail=1 ;;
    esac
    case "${OUT}" in
        *"memory: n/a"*) echo "FAIL: Linux host reported memory n/a; the arm did not run" >&2; fail=1 ;;
        *) echo "ok: on a Linux host the memory arm actually measured something" ;;
    esac
else
    check "Linux without a readable /proc/meminfo fails closed (exit 4)" "4" "${RC}"
    contains "the fail-closed error refuses to claim a pass" "unmeasured" "${OUT}"
fi

# --- Memory arm: the floor ----------------------------------------------------
reset_env
export DF_STUB=healthy
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-low"
run_subject "AICR bundle installed"
check "memory available below the floor exits 3" "3" "${RC}"
contains "the memory error names the stage" "[AICR bundle installed]" "${OUT}"
contains "the memory error names the measured availability" "memory available 0.95 GiB" "${OUT}"
contains "the memory error names the floor it missed" "floor of 1.53 GiB" "${OUT}"
contains "the memory error names the shortfall" "short by 0.57 GiB" "${OUT}"

reset_env
export DF_STUB=healthy
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "healthy memory"
check "memory available above the floor exits 0" "0" "${RC}"
contains "the healthy memory arm records the measured figure" "7.63 GiB available" "${OUT}"

# THE ANTI-PATTERN GUARD. A meminfo that is readable but missing MemAvailable
# makes awk yield an empty string, which arithmetic would happily read as zero.
# Zero available would trip the floor and look like a caught over-budget run,
# or, if the check were written the other way, would sail through as n/a. Both
# are wrong: the honest answer is that the arm did not measure anything.
reset_env
export DF_STUB=healthy
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-no-available"
run_subject "meminfo without MemAvailable"
check "readable meminfo missing MemAvailable fails closed (exit 4)" "4" "${RC}"
contains "the unparseable-meminfo error refuses to claim a pass" "unmeasured" "${OUT}"

# The same guard's other half. A MemTotal of zero is numeric, so the regex arm
# passes it through; only the `-gt 0` clause stops it. Left unguarded the floor
# becomes zero, the subtraction yields a confident "0.00 GiB used, 0.00 GiB
# available", and the run is reported as a pass, which is the anti-pattern the
# block above is named for.
reset_env
export DF_STUB=healthy
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-zero-total"
run_subject "meminfo reporting a zero MemTotal"
check "a zero MemTotal fails closed (exit 4), not a pass on a 0.00 GiB total" "4" "${RC}"
contains "the zero-MemTotal guard records n/a instead of inventing 0.00" \
    "memory: n/a GiB used, n/a GiB available" "${OUT}"

reset_env
export DF_STUB=healthy
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/does-not-exist"
run_subject "meminfo path that is not there"
check "an unreadable meminfo source fails closed (exit 4)" "4" "${RC}"

# One kB either side of the memory floor and exactly on it, the same treatment
# the disk floor already gets. Without these a comparison that drifts by one
# (`-le` for `-lt`) fails every arm sitting exactly on its floor and no
# assertion notices. df is stubbed healthy so the disk arm cannot pre-empt the
# exit code with the host's own free space.
reset_env
export DF_STUB=healthy
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-at-floor"
run_subject "memory exactly at the floor"
check "memory available exactly at the floor exits 0" "0" "${RC}"

reset_env
export DF_STUB=healthy
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-under-floor"
run_subject "memory one kB under the floor"
check "memory available one kB under the floor exits 3" "3" "${RC}"

reset_env
export DF_STUB=healthy
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-over-floor"
run_subject "memory one kB over the floor"
check "memory available one kB over the floor exits 0" "0" "${RC}"

# --- The recording and the verdict must agree ---------------------------------
# `n/a` in the recording line is the ONLY evidence a reader has that the memory
# arm was skipped rather than judged, and the two lines carrying that claim come
# from two separate conditions in the subject. When those drifted apart the
# script printed real memory figures one line above a verdict announcing that
# the memory arm did not apply on this host, and the contradiction was invisible
# to every assertion that read one line at a time.
#
# Which direction of the invariant is live depends on the host. A skipped arm
# recording real figures needs a host whose default meminfo is readable while
# `uname` says otherwise, so it is live on Linux with UNAME_STUB and vacuous on
# macOS; a judged arm recording n/a is live everywhere, through an explicit
# source. The third run below leaves `uname` real, so it asserts whichever of
# the two THIS host can reach. Its df is stubbed: the invariant is about the
# memory arm, and an exit 2 from the host's own disk would only stop the
# comparison from being made at all.
#
# Only ever applied to a run that passed: the "does not apply" clause is printed
# by the pass path alone, so on a failing run its absence would say nothing and
# the comparison below would fire for the wrong reason. The rc is asserted here
# rather than assumed.
assert_record_matches_verdict() {
    local desc="$1" rc="$2" out="$3"
    local recorded_na=0 verdict_skipped=0
    if [[ "${rc}" -ne 0 ]]; then
        echo "FAIL: ${desc}: expected a passing run to compare, got exit ${rc}" >&2
        printf '%s\n' "${out}" >&2
        fail=1
        return
    fi
    case "${out}" in
        *"memory: n/a GiB used, n/a GiB available"*) recorded_na=1 ;;
    esac
    case "${out}" in
        *"the memory arm does not apply"*) verdict_skipped=1 ;;
    esac
    if [[ "${recorded_na}" -ne "${verdict_skipped}" ]]; then
        echo "FAIL: ${desc}: the recorded figures and the verdict disagree about whether the memory arm applied (recorded n/a=${recorded_na}, verdict says skipped=${verdict_skipped})" >&2
        echo "  --- captured output ---" >&2
        printf '%s\n' "${out}" >&2
        fail=1
    else
        echo "ok: ${desc}"
    fi
}

reset_env
export UNAME_STUB=Darwin
export DF_STUB=healthy
run_subject "arm skipped"
assert_record_matches_verdict "a skipped memory arm records n/a and the verdict says skipped" "${RC}" "${OUT}"

reset_env
export DF_STUB=healthy
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "arm judged"
assert_record_matches_verdict "a judged memory arm records figures and the verdict says judged" "${RC}" "${OUT}"

# The floor is dropped to its minimum for this one case, and NOT its meminfo
# stubbed. Setting AICR_FOOTPRINT_MEMINFO would make the arm mandatory by the
# env branch and collapse this into the case above, losing the only coverage of
# the `uname` branch that decides applicability. But the comparison can only be
# made on a run that PASSED, and a Linux host under its own 10% memory floor
# exits 3, so the floor is what has to give. 1% is the lowest the script accepts
# and leaves the real /proc/meminfo being really measured.
reset_env
export DF_STUB=healthy
export AICR_FOOTPRINT_MIN_MEM_PCT=1
run_subject "real uname"
assert_record_matches_verdict "the real-uname run agrees with itself on whichever host it lands" "${RC}" "${OUT}"

# --- The floors cannot be configured away -------------------------------------
# An env knob that accepts 0 is a gate with an off switch. Reject anything
# outside 1..99 rather than quietly disabling the arm.
reset_env
export AICR_FOOTPRINT_MIN_DISK_PCT=0
export DF_STUB=low
run_subject "zeroed disk floor"
check "a disk floor of 0 is rejected (exit 1), not honoured" "1" "${RC}"

reset_env
export AICR_FOOTPRINT_MIN_MEM_PCT=abc
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/meminfo-healthy"
run_subject "non-numeric memory floor"
check "a non-numeric memory floor is rejected (exit 1)" "1" "${RC}"

reset_env
export AICR_FOOTPRINT_MIN_DISK_PCT="1 2"
run_subject "floor carrying whitespace"
check "a floor that is not a single integer is rejected (exit 1)" "1" "${RC}"

# The upper bound, which the three cases above never reach. A floor of 100 asks
# for the whole filesystem to be free and would fail every run that ever
# succeeded, so it is rejected as configuration rather than honoured as a gate.
reset_env
export AICR_FOOTPRINT_MIN_DISK_PCT=100
export DF_STUB=healthy
run_subject "disk floor of 100"
check "a disk floor of 100 is rejected (exit 1), not honoured" "1" "${RC}"

# And the value immediately inside it, so the bound cannot quietly slide to 98.
# 99% of the stubbed 73.4 GiB total is far above its 28.6 GiB free, so exit 2 is
# the gate running on an ACCEPTED floor; exit 1 here would mean 99 was refused.
reset_env
export AICR_FOOTPRINT_MIN_DISK_PCT=99
export DF_STUB=healthy
run_subject "disk floor of 99"
check "a disk floor of 99 is accepted and enforced (exit 2, not 1)" "2" "${RC}"

# --- Both arms failing at once ------------------------------------------------
# The exit code can only name one arm, so the documented precedence is disk
# first. What must NOT happen is either error going unprinted: the log is where
# an operator learns that memory was also gone, and a run debugged from the exit
# code alone would chase the disk and never see it.
reset_env
export DF_STUB=low
export AICR_FOOTPRINT_MEMINFO="${STUB_DIR}/does-not-exist"
run_subject "both arms down"
check "disk takes precedence in the exit code when both arms fail" "2" "${RC}"
contains "the disk error is still printed when memory also failed" "disk free 2.9 GiB" "${OUT}"
contains "the memory error is still printed when disk also failed" "refusing to report a pass on an unmeasured arm" "${OUT}"

# --- The job summary still gets the failing stage's row -----------------------
# The summary table is how an operator finds WHERE the space went. It has to be
# written for the stage that blew the budget, which is the row that gets read.
reset_env
export DF_STUB=low
export GITHUB_STEP_SUMMARY="${STUB_DIR}/summary.md"
: >"${GITHUB_STEP_SUMMARY}"
run_subject "validator + agent images"
check "the gate still fails with a summary configured" "2" "${RC}"
summary="$(cat "${GITHUB_STEP_SUMMARY}")"
contains "the summary table header is written" "| Stage | Disk used" "${summary}"
contains "the failing stage gets its summary row" "| validator + agent images |" "${summary}"

reset_env
if [[ "${fail}" -ne 0 ]]; then
    echo "record-footprint_test.sh: FAILED" >&2
    exit 1
fi
echo "record-footprint_test.sh: all checks passed"
