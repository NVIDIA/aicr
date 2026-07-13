#!/usr/bin/env bash
# shellcheck shell=bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Deadline-derived sync-gate budgets for the KWOK deployer-matrix CI lane.
#
# Sourced by validate-scheduling.sh. CI (.github/actions/kwok-test) exports
# KWOK_SYNC_DEADLINE_EPOCH — the absolute epoch after which the job has no
# time left for sync-gate work (job timeout minus a diagnostics margin).
# Each chainsaw sync gate derives its budget as min(<gate default>,
# deadline − now), bounding that gate's assert/error operation so it
# finishes — and prints its catch-block diagnostics — before GitHub's
# job timeout kills the runner in the expected single-gate-dominates
# case (this bounds each gate operation, not the job's whole wall time).
#
# Source guard: constants and functions only, no side effects at source
# time (same contract as lib/cleanup.sh).

# Below this floor a shrunken budget cannot produce a meaningful gate run;
# callers fail fast with an explicit error instead — which is itself the
# diagnosis a silent CANCELLED would have destroyed.
readonly SYNC_BUDGET_FLOOR_SECONDS=120

# compute_sync_budget <default_seconds> [<now_epoch>]
#
# Prints the effective sync-gate budget (seconds) to stdout:
#   - KWOK_SYNC_DEADLINE_EPOCH unset/empty: <default_seconds> unchanged
#     (local runs keep today's fixed budgets).
#   - Set: min(<default_seconds>, KWOK_SYNC_DEADLINE_EPOCH − now).
# Returns 1 (printing nothing) when the derived budget is below
# SYNC_BUDGET_FLOOR_SECONDS — callers must fail fast (exit code 50).
# <now_epoch> is injectable for tests; defaults to $(date +%s).
compute_sync_budget() {
    local default_seconds="$1"
    local now="${2:-$(date +%s)}"
    if [[ -z "${KWOK_SYNC_DEADLINE_EPOCH:-}" ]]; then
        echo "${default_seconds}"
        return 0
    fi
    local remaining=$(( KWOK_SYNC_DEADLINE_EPOCH - now ))
    local budget="${default_seconds}"
    if (( remaining < budget )); then
        budget="${remaining}"
    fi
    if (( budget < SYNC_BUDGET_FLOOR_SECONDS )); then
        return 1
    fi
    echo "${budget}"
}
