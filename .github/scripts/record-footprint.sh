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
#
# Records root-filesystem and memory use at a named point in a job, to the log
# and to the job summary, and FAILS THE STEP when the headroom left is below a
# floor.
#
# Written for the simulated-GPU UAT lane (issue #2358), where whether a
# five-node kind cluster plus the AICR bundle fits a GitHub-hosted runner is an
# OPEN question: kind gives each node its own containerd store, so images are
# unshared, and the only honest answer comes from measuring a real run rather
# than citing a documented runner size. Called at each stage so the summary
# shows where the space went, not just whether the job survived.
#
# WHY IT GATES AND DOES NOT MERELY RECORD. The claim this lane makes is "this
# fits a GitHub-hosted runner". A number printed to a log cannot make that claim
# false; it can only be read afterwards by somebody who already suspects the
# answer. Without a gate the lane dies the way capacity problems always die, as
# an OOM-killed pod or an out-of-disk helm error three steps later that nobody
# attributes to capacity. The gate converts that into a named failure at the
# stage that consumed the room.
#
# WHY A HEADROOM FLOOR AND NOT A BUDGET. Nobody has measured this lane yet, so
# any absolute "must stay under N GiB" would be invented. The floor is a
# percentage of the totals the runner itself reports, which self-calibrates to
# whatever runner the job lands on, survives a runner-size change, and goes red
# at the moment the run is actually about to die rather than at a number
# somebody guessed. On the ubuntu-latest root filesystem (about 73 GiB) the
# default 10% floor sits near 7.3 GiB, roughly the room one more kind node image
# plus an operator's images needs.
#
# Usage:
#   .github/scripts/record-footprint.sh "<stage label>"
#
# Exit codes (asserted by tools/record-footprint_test.sh; callers and that
# harness both depend on the exact value, so do not renumber):
#   0  every applicable arm is above its floor
#   1  usage error, or a floor configured outside 1..99
#   2  disk free is below its floor
#   3  memory available is below its floor
#   4  the memory arm is required but its headroom could not be measured
#   5  `df -Pk /` returned figures that cannot be read as 1K blocks
# Disk takes precedence in the exit code when both arms fail; each failing arm
# prints its own ::error line, so the log names every one of them.
#
# Environment:
#   AICR_FOOTPRINT_MIN_DISK_PCT  disk floor, percent of the reported total (default 10)
#   AICR_FOOTPRINT_MIN_MEM_PCT   memory floor, percent of MemTotal (default 10)
#   AICR_FOOTPRINT_MEMINFO       meminfo source (default /proc/meminfo). Setting
#                                it asserts that this host HAS a meminfo, so the
#                                memory arm becomes mandatory; the script says so
#                                in the log when it is set.
# Both percentages are rejected outside 1..99: a floor of 0 is an off switch,
# and a gate with an off switch is the defect this script exists to remove.
#
# `df -Pk` is POSIX (portable output, 1K blocks), so this runs unchanged on a
# developer machine. Memory comes from /proc/meminfo and is recorded as `n/a`
# wherever the memory arm does not apply.

set -euo pipefail

STAGE="${1:?usage: record-footprint.sh <stage label>}"

MIN_DISK_PCT="${AICR_FOOTPRINT_MIN_DISK_PCT:-10}"
MIN_MEM_PCT="${AICR_FOOTPRINT_MIN_MEM_PCT:-10}"
MEMINFO="${AICR_FOOTPRINT_MEMINFO:-/proc/meminfo}"

# A floor outside 1..99 is rejected rather than clamped: 0 would disable the
# arm outright and 100 would fail every run, and neither is something a caller
# can have meant. The value is passed as a single argument, so a floor carrying
# whitespace is caught here rather than splitting into a fragment that happens
# to validate.
require_pct() {
    local name="$1" value="$2"
    if ! [[ "${value}" =~ ^[0-9]+$ ]] || [[ "${value}" -lt 1 ]] || [[ "${value}" -gt 99 ]]; then
        echo "::error::${name} must be an integer from 1 through 99; got '${value}'" >&2
        exit 1
    fi
}
require_pct AICR_FOOTPRINT_MIN_DISK_PCT "${MIN_DISK_PCT}"
require_pct AICR_FOOTPRINT_MIN_MEM_PCT "${MIN_MEM_PCT}"

# Field 2 is the total, 3 the used, 4 the available, all in 1K blocks. -P
# guarantees one record per filesystem even when the device name is long.
read -r _ total_k used_k avail_k _ <<<"$(df -Pk / | tail -1)"

to_gib() { awk -v k="$1" 'BEGIN { printf "%.1f", k / 1048576 }'; }
to_gib2() { awk -v k="$1" 'BEGIN { printf "%.2f", k / 1048576 }'; }

# The disk arm is never optional: `df -Pk` is POSIX and present everywhere this
# runs. Figures that will not parse therefore mean the measurement failed, not
# that there is nothing to measure. awk would read "-" as 0 and report a
# perfectly calm "0.0 GiB free of 0.0 GiB", which is why this is checked here
# and not left to the arithmetic below.
if ! [[ "${total_k}" =~ ^[0-9]+$ ]] || ! [[ "${avail_k}" =~ ^[0-9]+$ ]] || [[ "${total_k}" -le 0 ]]; then
    echo "::error::footprint gate [${STAGE}]: \`df -Pk /\` returned figures this script cannot read as 1K blocks (total='${total_k}' avail='${avail_k}'); refusing to report a pass on an unmeasured arm" >&2
    exit 5
fi

disk_used="$(to_gib "${used_k}")"
disk_avail="$(to_gib "${avail_k}")"
disk_total="$(to_gib "${total_k}")"

# WHETHER THE MEMORY ARM APPLIES, AND THE TRAP IN DECIDING IT. /proc/meminfo is
# absent on a developer macOS machine, and this script is documented as running
# there unchanged, so "fail when unreadable" would break local use on day one.
# But "skip when unreadable" is worse: a Linux runner whose /proc is not mounted
# would then pass the memory arm without measuring anything, which is the
# project's own anti-pattern about a negative check that passes on an ambiguous
# condition. The two cases are distinguished by the OS, not by the file: on
# Linux the arm is mandatory and an unreadable source is a failure, off Linux
# there is nothing to read and the arm does not apply.
mem_required=0
mem_required_why=""
if [[ -n "${AICR_FOOTPRINT_MEMINFO:-}" ]]; then
    mem_required=1
    mem_required_why="AICR_FOOTPRINT_MEMINFO is set to ${MEMINFO}"
    echo "footprint [${STAGE}] memory source overridden to ${MEMINFO}; the memory arm is mandatory"
elif [[ "$(uname -s)" == "Linux" ]]; then
    mem_required=1
    mem_required_why="uname -s reports Linux"
fi

mem_used="n/a"
mem_avail="n/a"
mem_total_k=""
mem_avail_k=""
mem_unmeasured=""
# MEASURED EXACTLY WHEN THE ARM APPLIES, not merely when the source happens to
# be readable. `uname` is what decides applicability, but /proc/meminfo is
# readable on every Linux host, so gating this read on readability alone
# recorded real figures for a host whose memory arm the verdict below then
# reported as inapplicable. The log and the verdict then disagreed about whether
# the arm had run, and `n/a` stopped being evidence that it was skipped. One
# condition over both keeps them in agreement by construction.
if [[ "${mem_required}" -eq 1 ]]; then
    if [[ -r "${MEMINFO}" ]]; then
        # MemAvailable is the kernel's own estimate of what a new workload could
        # claim, which is the number that decides whether the next helm release
        # schedules. MemTotal - MemAvailable is therefore the meaningful "used".
        mem_total_k="$(awk '/^MemTotal:/ { print $2; exit }' "${MEMINFO}")"
        mem_avail_k="$(awk '/^MemAvailable:/ { print $2; exit }' "${MEMINFO}")"
        # An empty capture is what a missing field looks like, and arithmetic
        # reads it as zero. Zero available would either trip the floor and be
        # reported as a caught over-budget run, or be formatted into a confident
        # "0.00 GiB available"; both are the script inventing a measurement it
        # does not have.
        if [[ "${mem_total_k}" =~ ^[0-9]+$ ]] && [[ "${mem_avail_k}" =~ ^[0-9]+$ ]] && [[ "${mem_total_k}" -gt 0 ]]; then
            mem_used="$(awk -v t="${mem_total_k}" -v a="${mem_avail_k}" \
                'BEGIN { printf "%.2f", (t - a) / 1048576 }')"
            mem_avail="$(to_gib2 "${mem_avail_k}")"
        else
            mem_unmeasured="MemTotal or MemAvailable is missing or not a number in ${MEMINFO}"
        fi
    else
        mem_unmeasured="${MEMINFO} is not readable"
    fi
fi

printf 'footprint [%s] disk: %s GiB used, %s GiB free of %s GiB | memory: %s GiB used, %s GiB available\n' \
    "${STAGE}" "${disk_used}" "${disk_avail}" "${disk_total}" "${mem_used}" "${mem_avail}"

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    # Emit the table header once. Checking the summary file rather than
    # tracking "first call" in the workflow keeps every call site identical.
    if ! grep -qF '| Stage | Disk used' "${GITHUB_STEP_SUMMARY}" 2>/dev/null; then
        {
            echo ""
            echo "### Runner footprint (measured)"
            echo ""
            echo "| Stage | Disk used | Disk free | Memory used | Memory available |"
            echo "|-------|-----------|-----------|-------------|------------------|"
        } >> "${GITHUB_STEP_SUMMARY}"
    fi
    printf '| %s | %s GiB | %s GiB | %s GiB | %s GiB |\n' \
        "${STAGE}" "${disk_used}" "${disk_avail}" "${mem_used}" "${mem_avail}" \
        >> "${GITHUB_STEP_SUMMARY}"
fi

# THE GATE. Deliberately after the recording above, so the stage that blew the
# budget still contributes its log line and its summary row: the table is how an
# operator finds where the space went, and the failing row is the one worth
# reading.
gate_rc=0

disk_floor_k=$(( total_k * MIN_DISK_PCT / 100 ))
if [[ "${avail_k}" -lt "${disk_floor_k}" ]]; then
    echo "::error::footprint gate [${STAGE}]: disk free ${disk_avail} GiB is below the floor of $(to_gib "${disk_floor_k}") GiB (${MIN_DISK_PCT}% of the ${disk_total} GiB root filesystem); short by $(to_gib "$(( disk_floor_k - avail_k ))") GiB" >&2
    gate_rc=2
fi

if [[ "${mem_required}" -eq 1 ]]; then
    if [[ -n "${mem_unmeasured}" ]]; then
        echo "::error::footprint gate [${STAGE}]: the memory arm is required (${mem_required_why}) but ${mem_unmeasured}; refusing to report a pass on an unmeasured arm" >&2
        if [[ "${gate_rc}" -eq 0 ]]; then
            gate_rc=4
        fi
    else
        mem_floor_k=$(( mem_total_k * MIN_MEM_PCT / 100 ))
        if [[ "${mem_avail_k}" -lt "${mem_floor_k}" ]]; then
            echo "::error::footprint gate [${STAGE}]: memory available ${mem_avail} GiB is below the floor of $(to_gib2 "${mem_floor_k}") GiB (${MIN_MEM_PCT}% of the $(to_gib2 "${mem_total_k}") GiB reported total); short by $(to_gib2 "$(( mem_floor_k - mem_avail_k ))") GiB" >&2
            if [[ "${gate_rc}" -eq 0 ]]; then
                gate_rc=3
            fi
        fi
    fi
fi

if [[ "${gate_rc}" -eq 0 ]]; then
    if [[ "${mem_required}" -eq 1 ]]; then
        mem_note="memory available ${mem_avail} GiB is at or above its ${MIN_MEM_PCT}% floor"
    else
        mem_note="the memory arm does not apply on this host, which does not report Linux"
    fi
    echo "footprint gate [${STAGE}] passed: disk free ${disk_avail} GiB is at or above the ${MIN_DISK_PCT}% floor of $(to_gib "${disk_floor_k}") GiB; ${mem_note}"
fi

exit "${gate_rc}"
