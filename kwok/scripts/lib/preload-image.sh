#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# preload-image.sh - side-load a public image into the Kind node so the
# kubelet never has to pull it mid-rollout.
#
# Sourced by install-infra.sh. Kept as its own lib (like sync-budget.sh and
# profile-select.sh) so the retry and cluster-resolution logic can be unit
# tested with stubbed docker/kind instead of only in a live CI lane.
#
# Callers must provide log_info / log_warn / log_debug; install-infra.sh
# defines them before sourcing this file.

# Total wall-clock budget for preloading ONE image, across every retry and the
# side-load. Overridable for tests.
#
# A per-operation timeout would not be enough: three stalled pulls plus a stalled
# load still multiply out. The budget is checked before each operation and passed
# to `timeout`, and the retry backoff is clamped to it as well, so no number of
# retries can make the function outlive it.
#
# The one deliberate exception is the final "is it cached" probe, which is given
# at least PRELOAD_PROBE_TIMEOUT even once the deadline is spent (see
# preload_image_cached). The real ceiling is therefore the budget plus at most
# one probe floor. That is one probe per call, not per retry, so it does not
# scale with the retry count.
# 180s is generous for two small images and leaves the 20-minute KWOK job budget
# essentially intact even if both preloads time out completely.
#
# Read at CALL time, not source time, so a caller (or a test) can change it
# between invocations.
KWOK_PRELOAD_BUDGET_DEFAULT=180

# Retry backoff schedule, in seconds: doubles from START, capped at MAX. The cap
# keeps a long budget from spending itself on one enormous sleep, which would
# leave no attempt near the end of the window.
PRELOAD_BACKOFF_START=5
PRELOAD_BACKOFF_MAX=30

# FLOOR for a single "is this image cached" probe (see preload_image_cached).
# A probe gets at least this long even when the run budget is spent, and more
# when the budget still has more to give. It is a floor and not a cap precisely
# so it cannot narrow the allowance a busy daemon would otherwise have had.
#
# It therefore bounds a wedged Docker Engine only on the budget-spent path;
# while budget remains the probe may wait for most of it, exactly as the
# deadline-bound check it replaced always did. The ceiling that matters to a
# lane is the run budget, and this constant is what keeps a spent one from
# collapsing the probe to zero.
PRELOAD_PROBE_TIMEOUT=5

# Side-load an image into the Kind node so the kubelet never pulls it.
#
# The registry and Gitea Deployments are the only two workloads in the KWOK
# lanes whose images come from a public registry, and both used to be pulled by
# the kubelet inside a 120s rollout budget with no retry. That made every lane
# depend on a single upstream reach at exactly the wrong moment: an ECR blip
# surfaced as "Registry Deployment did not become Ready within 120s", exit 20,
# and a red matrix cell with nothing wrong in the repo. It was the single
# largest source of KWOK-lane failures.
#
# Pulling on the host instead has three advantages: `docker pull` can be
# retried without holding a rollout budget open, the runner's Docker cache
# survives across lanes in a job, and both Deployments already set
# imagePullPolicy: IfNotPresent, so a preloaded image is used as-is.
#
# BEST EFFORT BY DESIGN. Every failure path here logs and returns 0, leaving
# the kubelet pull as the fallback exactly as before. This function must not
# become a new way for the lane to fail: it removes a failure mode or it does
# nothing. That is why it never propagates an error, even when docker or kind
# is missing entirely (a non-Kind cluster, or a dev box driving a remote one).
# preload_remaining prints the seconds left before the deadline, and returns 1
# when the budget is spent so callers can bail instead of starting an operation
# they cannot finish.
preload_remaining() {
    local deadline="$1" left
    left=$(( deadline - $(date +%s) ))
    if (( left <= 0 )); then
        return 1
    fi
    echo "${left}"
}

# preload_have_image reports whether the image is already in the host's Docker
# cache, bounded by the remaining budget.
#
# Bounded because `docker image inspect` is a request to the Docker Engine, not
# a local file read: a wedged daemon stalls it like any other call. Inside the
# retry loop it would stall once per attempt before the kubelet fallback could
# run — the same unbounded-wait failure this function exists to remove.
#
# A spent budget reports "not cached", which is the right reading INSIDE the
# loop: there is no time left to pull, so the loop must stop. It is the wrong
# reading for a final verdict — see preload_image_cached.
preload_have_image() {
    local image="$1" deadline="$2" remaining
    remaining=$(preload_remaining "${deadline}") || return 1
    timeout "${remaining}" docker image inspect "${image}" &>/dev/null
}

# preload_image_cached answers "is the image on this host", and unlike
# preload_have_image a spent DEADLINE does not make it answer no.
#
# The budget governs how long we spend TRYING to get the image, not how long we
# may take to OBSERVE the result. Gating a verdict on it conflates the two: a
# pull that succeeds with the last of the budget leaves the image cached and the
# deadline passed, and a budget-bounded check then reports the image as missing,
# so the log blames a pull that actually worked.
#
# Bounded by the LARGER of the remaining budget and PRELOAD_PROBE_TIMEOUT. The
# constant is a floor, not a cap: it guarantees a fair chance to answer once the
# deadline is spent, without narrowing the allowance a busy daemon would
# otherwise have had. Capping at the constant would trade the budget-spent false
# miss for a slow-daemon one. So this still reads the budget -- it just cannot
# be silenced by an exhausted one.
#
# Callers that need to know whether there is time left to do more WORK ask
# preload_remaining instead.
preload_image_cached() {
    local image="$1" deadline="$2" bound="${PRELOAD_PROBE_TIMEOUT}" remaining
    if remaining=$(preload_remaining "${deadline}"); then
        if (( remaining > bound )); then
            bound="${remaining}"
        fi
    fi
    timeout "${bound}" docker image inspect "${image}" &>/dev/null
}

# preload_pull_retry gets IMAGE into the host's Docker cache before DEADLINE,
# retrying a failed pull until the budget is spent. Returns 0 when the image is
# cached on return, 1 when it is not — and logs the reason in that case.
#
# Split out from preload_image so the CI image-cache priming step
# (kwok/scripts/lib/image-cache.sh) reuses this exact retry schedule instead of
# growing a second one. The two callers differ only in what a failure means:
# preload_image degrades to the kubelet pull, priming fails its job.
#
# Retry until the BUDGET is spent, not for a fixed number of attempts.
#
# The fixed three-attempt schedule this replaces gave up in ~17s while holding a
# 180s budget (#2483): each pull failed in about a second, the backoff was 5s
# then 10s, and ~163s went unused. Against a per-IP registry throttle that is
# close to the worst possible schedule — it retries fast enough to stay inside
# the same throttle window, then stops long before the window would reset.
#
# Evidence it is a throttle and not an outage: in the run that motivated this,
# 125 of 127 concurrent matrix jobs pulled the same image successfully at the
# same moment. The pull is not broken; a few callers are being shed.
#
# Backoff doubles from PRELOAD_BACKOFF_START and is capped, so a long budget
# spends most of its time waiting rather than hammering.
preload_pull_retry() {
    local image="$1" deadline="$2"

    local attempt=0 remaining backoff="${PRELOAD_BACKOFF_START}" pull_err last_err=""
    local last_rc=0
    pull_err="$(mktemp)"
    while :; do
        if preload_have_image "${image}" "${deadline}"; then
            break
        fi
        if ! remaining=$(preload_remaining "${deadline}"); then
            break
        fi

        attempt=$(( attempt + 1 ))
        log_info "Pulling ${image} on the host (attempt ${attempt}, ${remaining}s left)..."
        # Capture the exit code rather than branching on it directly: `timeout`
        # reports 124 when it killed the command, which is the only reliable way
        # to tell a budget kill from a pull that simply failed. Inferring it
        # from empty stderr is wrong -- docker can exit nonzero and silent.
        local pull_rc=0
        timeout "${remaining}" docker pull --quiet "${image}" >/dev/null 2>"${pull_err}" || pull_rc=$?
        if (( pull_rc == 0 )); then
            # Clear the carried cause. A failed attempt that a later attempt
            # superseded must not be reported as the reason the run ended --
            # that is the same misattribution as reading a timeout kill out of
            # empty stderr, just one loop iteration further back.
            last_err=""
            last_rc=0
            break
        fi
        # Capture the cause the moment it happens, so it survives every later
        # exit from this loop. Reading the file at the reporting site instead
        # would lose it on any path that breaks out and deletes the file first.
        last_err="$(tr '\n' ' ' < "${pull_err}" | tail -c 300)"
        last_rc="${pull_rc}"

        # Clamp the backoff to the budget. Sleeping past the deadline is pure
        # dead time: it cannot buy another attempt, and it delays the kubelet
        # fallback by exactly as long as it oversleeps.
        remaining=$(preload_remaining "${deadline}") || break
        if (( backoff > remaining )); then
            backoff="${remaining}"
        fi
        sleep "${backoff}"

        backoff=$(( backoff * 2 ))
        if (( backoff > PRELOAD_BACKOFF_MAX )); then
            backoff="${PRELOAD_BACKOFF_MAX}"
        fi
    done
    rm -f "${pull_err}"

    # ONE reporting site for every way the pull can end unsuccessfully. The
    # loop has four exits (image already cached, budget spent before an
    # attempt, pull succeeded, budget spent after a failed attempt) and an
    # earlier version reported the cause on only one of them -- so a pull that
    # consumed the last of the budget lost its own error message. Reporting
    # here, from a variable captured at failure time, is what makes that
    # impossible rather than merely fixed.
    #
    # The verdict asks only whether the image is on the host. Asking it through
    # the budget as well made a pull that succeeded with the last of the budget
    # report as a miss, so the log blamed a pull that had worked.
    #
    # This corrects the REPORT, not the outcome: preload_image's next step is
    # its own budget check, so a genuinely exhausted budget still ends in the
    # kubelet pull. Side-loading a ~250MB image with no time left is not
    # something this function can or should attempt (#2502 is diagnostics-only).
    if preload_image_cached "${image}" "${deadline}"; then
        return 0
    fi
    # Select the message on what was actually observed -- the attempt counter
    # and the exit code -- never on inference. A pull killed by `timeout` writes
    # nothing to stderr, so keying on empty stderr printed "no pull was
    # attempted" beneath the line announcing the attempt; keying the opposite
    # way would claim a budget kill for any silent nonzero exit, which docker
    # can produce on its own. Only rc 124 actually means `timeout` killed it.
    #
    # Ordered by how much each condition tells the reader. A real cause from
    # docker outranks any exit code -- a pull can report "toomanyrequests" and
    # THEN be killed, and that cause is the useful half. The exit code only has
    # to answer when no cause was captured.
    #
    # Emptiness is tested on a whitespace-stripped copy: `tr` above turns a
    # killed docker's partial or blank line into a space, which is non-empty,
    # so testing the raw string would print an empty "last error:" and hide the
    # rc-based diagnosis behind it.
    local last_err_trimmed="${last_err//[[:space:]]/}"
    if (( attempt == 0 )); then
        log_warn "${image} is not cached and no pull was attempted (the budget ran out first)"
    elif (( last_rc == 0 )); then
        log_warn "${image} is not cached after ${attempt} attempt(s); the last reported success but the image is not present"
    elif [[ -n "${last_err_trimmed}" ]]; then
        log_warn "${image} is not cached after ${attempt} attempt(s); last error: ${last_err}"
    elif (( last_rc == 124 )); then
        log_warn "${image} is not cached after ${attempt} attempt(s); the last was killed by the budget timeout before it reported a cause"
    else
        log_warn "${image} is not cached after ${attempt} attempt(s); the last exited ${last_rc} without writing an error"
    fi
    return 1
}

preload_image() {
    local image="$1"

    # `timeout` is required, not optional. Without it every docker/kind call is
    # unbounded, and a stalled pull would hold the lane for the whole job — the
    # precise failure this function exists to prevent, in a new place. If it is
    # missing, skip preloading rather than risk that.
    if ! command -v docker &>/dev/null || ! command -v kind &>/dev/null ||
        ! command -v timeout &>/dev/null; then
        log_debug "docker, kind, or timeout unavailable — leaving ${image} to the kubelet"
        return 0
    fi

    # One deadline for the whole function; every operation below draws from it.
    local budget="${KWOK_PRELOAD_BUDGET_SECONDS:-${KWOK_PRELOAD_BUDGET_DEFAULT}}"
    local deadline=$(( $(date +%s) + budget ))

    # Kind names the context "kind-<cluster>", so the cluster name is the
    # context with that prefix stripped. Fall back to the same default
    # run-all-recipes.sh uses when no context is pinned.
    local cluster="${KWOK_CLUSTER:-aicr-kwok-test}"
    if [[ -n "${KUBECTL_CONTEXT:-}" ]]; then
        if [[ "${KUBECTL_CONTEXT}" != kind-* ]]; then
            log_debug "context ${KUBECTL_CONTEXT} is not a Kind cluster — leaving ${image} to the kubelet"
            return 0
        fi
        cluster="${KUBECTL_CONTEXT#kind-}"
    fi

    if ! preload_remaining "${deadline}" >/dev/null; then
        log_warn "Preload budget exhausted before resolving the cluster; the kubelet will pull ${image}"
        return 0
    fi

    if ! timeout "$(preload_remaining "${deadline}")" kind get clusters 2>/dev/null | grep -qx "${cluster}"; then
        log_debug "Kind cluster ${cluster} not found — leaving ${image} to the kubelet"
        return 0
    fi

    # The retry schedule lives in preload_pull_retry, which also owns the single
    # reporting site for a pull that never lands. Here a failure is not fatal:
    # the kubelet pull is still the fallback, exactly as before.
    if ! preload_pull_retry "${image}" "${deadline}"; then
        log_warn "The kubelet will retry ${image} in-cluster"
        return 0
    fi

    if ! remaining=$(preload_remaining "${deadline}"); then
        log_warn "Preload budget exhausted before side-loading ${image}; the kubelet will pull it"
        return 0
    fi

    if timeout "${remaining}" kind load docker-image "${image}" --name "${cluster}" >/dev/null 2>&1; then
        log_info "Preloaded ${image} into Kind cluster ${cluster}"
    else
        log_warn "Could not side-load ${image} into ${cluster}; the kubelet will pull it"
    fi
    return 0
}

