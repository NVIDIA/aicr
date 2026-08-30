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
preload_image() {
    local image="$1"

    if ! command -v docker &>/dev/null || ! command -v kind &>/dev/null; then
        log_debug "docker or kind unavailable — leaving ${image} to the kubelet"
        return 0
    fi

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

    if ! kind get clusters 2>/dev/null | grep -qx "${cluster}"; then
        log_debug "Kind cluster ${cluster} not found — leaving ${image} to the kubelet"
        return 0
    fi

    # Three attempts with linear backoff. The upstream failures seen in CI are
    # transient resets and timeouts, which a second attempt seconds later
    # clears; a longer schedule would just delay the kubelet fallback.
    local attempt
    for attempt in 1 2 3; do
        if docker image inspect "${image}" &>/dev/null; then
            break
        fi
        log_info "Pulling ${image} on the host (attempt ${attempt}/3)..."
        if docker pull --quiet "${image}" >/dev/null 2>&1; then
            break
        fi
        if (( attempt < 3 )); then
            sleep $(( attempt * 5 ))
        fi
    done

    if ! docker image inspect "${image}" &>/dev/null; then
        log_warn "Could not pull ${image} on the host; the kubelet will retry in-cluster"
        return 0
    fi

    if kind load docker-image "${image}" --name "${cluster}" >/dev/null 2>&1; then
        log_info "Preloaded ${image} into Kind cluster ${cluster}"
    else
        log_warn "Could not side-load ${image} into ${cluster}; the kubelet will pull it"
    fi
    return 0
}

