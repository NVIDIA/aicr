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
# to `timeout`, and the retry backoff is clamped to it as well, so the function
# cannot outlive it by more than scheduling noise no matter how many steps run.
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
# A spent budget reports "not cached", which is the safe reading: the caller
# then either bails on its own budget check or reports the pull as unsuccessful.
preload_have_image() {
    local image="$1" deadline="$2" remaining
    remaining=$(preload_remaining "${deadline}") || return 1
    timeout "${remaining}" docker image inspect "${image}" &>/dev/null
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

    # Retry until the BUDGET is spent, not for a fixed number of attempts.
    #
    # The fixed three-attempt schedule this replaces gave up in ~17s while
    # holding a 180s budget (#2483): each pull failed in about a second, the
    # backoff was 5s then 10s, and ~163s went unused. Against a per-IP registry
    # throttle that is close to the worst possible schedule — it retries fast
    # enough to stay inside the same throttle window, then stops long before the
    # window would reset.
    #
    # Evidence it is a throttle and not an outage: in the run that motivated
    # this, 125 of 127 concurrent matrix jobs pulled the same image successfully
    # at the same moment. The pull is not broken; a few callers are being shed.
    #
    # Backoff doubles from PRELOAD_BACKOFF_START and is capped, so a long budget
    # spends most of its time waiting rather than hammering.
    local attempt=0 remaining backoff="${PRELOAD_BACKOFF_START}" pull_err last_err=""
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
        if timeout "${remaining}" docker pull --quiet "${image}" >/dev/null 2>"${pull_err}"; then
            break
        fi
        # Capture the cause the moment it happens, so it survives every later
        # exit from this loop. Reading the file at the reporting site instead
        # would lose it on any path that breaks out and deletes the file first.
        last_err="$(tr '\n' ' ' < "${pull_err}" | tail -c 300)"

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
    if ! preload_have_image "${image}" "${deadline}"; then
        if [[ -n "${last_err}" ]]; then
            log_warn "${image} is not cached after ${attempt} attempt(s); last error: ${last_err}"
        else
            log_warn "${image} is not cached and no pull was attempted (the budget ran out first)"
        fi
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

