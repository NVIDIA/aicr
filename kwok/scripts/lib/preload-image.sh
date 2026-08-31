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

    # Three attempts with linear backoff. The upstream failures seen in CI are
    # transient resets and timeouts, which a second attempt seconds later
    # clears; a longer schedule would just delay the kubelet fallback.
    local attempt remaining
    for attempt in 1 2 3; do
        if preload_have_image "${image}" "${deadline}"; then
            break
        fi
        if ! remaining=$(preload_remaining "${deadline}"); then
            log_warn "Preload budget exhausted pulling ${image}; the kubelet will retry in-cluster"
            return 0
        fi
        log_info "Pulling ${image} on the host (attempt ${attempt}/3, ${remaining}s left)..."
        if timeout "${remaining}" docker pull --quiet "${image}" >/dev/null 2>&1; then
            break
        fi
        if (( attempt < 3 )); then
            # Clamp the backoff to the budget. Sleeping past the deadline is
            # pure dead time: it cannot buy another attempt, and it delays the
            # kubelet fallback by exactly as long as it oversleeps. Without this
            # the function's real ceiling is the budget PLUS the whole backoff
            # schedule, not the budget.
            local backoff=$(( attempt * 5 ))
            remaining=$(preload_remaining "${deadline}") || break
            if (( backoff > remaining )); then
                backoff="${remaining}"
            fi
            sleep "${backoff}"
        fi
    done

    if ! preload_have_image "${image}" "${deadline}"; then
        log_warn "${image} is not cached after pulling (failed, or the budget ran out); the kubelet will retry in-cluster"
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

