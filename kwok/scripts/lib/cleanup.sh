#!/usr/bin/env bash
# shellcheck shell=bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Shared cleanup helpers for the KWOK deployer-matrix CI lane.
#
# Sourced by validate-scheduling.sh and run-all-recipes.sh so the
# system-namespace allowlist (the only thing standing between the
# force-finalize sweep and the install-infra-owned ns argocd /
# flux-system / aicr-registry) and the KWOK-context safety guard
# live in exactly one place. Drift in either across scripts has
# silently broken the test infra before — keep it centralized.
#
# Source guard: this file uses constants and functions only, no
# side effects at source time.

# SYSTEM_NS_PATTERN matches the namespaces that MUST NOT be touched by
# the test cleanup paths:
#   - default, kube-*, local-path-storage, kwok-system: cluster
#     control-plane and CNI / storage plumbing. Wiping any of them
#     bricks the cluster.
#   - argocd, flux-system, aicr-registry: owned by install-infra.sh,
#     reused across recipes. Wiping any of them forces a full re-
#     install (~3-5 min) on every subsequent recipe.
#
# Used in `grep -vE "^(${SYSTEM_NS_PATTERN})$"` and
# `grep -vE "^(${SYSTEM_NS_PATTERN})\s"` call sites. Anchors are NOT
# baked in here so callers can choose space-anchor vs eol-anchor
# depending on the kubectl output they're filtering.
# shellcheck disable=SC2034  # consumed by sourcing scripts
readonly SYSTEM_NS_PATTERN="default|kube-node-lease|kube-public|kube-system|kwok-system|local-path-storage|argocd|aicr-registry|flux-system"

# ensure_kwok_context_loose checks that the kubectl context is a known
# KWOK Kind cluster name. Cheap check, no node lookup. Use this from
# cold-start paths where kwok nodes may not exist yet (e.g. run-all-
# recipes.sh's initial cleanup before any recipe has applied nodes).
#
# Hard-fail on mismatch — the cleanup paths the caller is about to run
# force-finalize namespaces, and we'd rather refuse to act than risk
# wiping a real cluster.
ensure_kwok_context_loose() {
    local ctx
    ctx=$(kubectl config current-context 2>/dev/null || true)
    case "$ctx" in
        kind-aicr-kwok|kind-aicr-kwok-test)
            return 0
            ;;
        *)
            echo "[ERROR] ensure_kwok_context: refusing to run — current kubectl context '${ctx:-<none>}' is not a known KWOK Kind cluster (expected kind-aicr-kwok or kind-aicr-kwok-test)" >&2
            echo "[ERROR] If you intended to run against a different cluster, double-check KUBECONFIG and the cluster's purpose — this script force-finalizes namespaces" >&2
            exit 1
            ;;
    esac
}

# ensure_kwok_context strict-checks BOTH the context name AND that at
# least one node carries `type=kwok` label. Use this from in-recipe
# paths where apply-nodes.sh has already populated the cluster. The
# node-label check is load-bearing: a developer could `kind create
# cluster --name aicr-kwok-test` and then point at a different cluster
# whose context name happens to be the same — only the kwok-typed
# nodes prove apply-nodes.sh ran against THIS cluster.
#
# The label-selector check is retried with bounded backoff to ride out
# a sub-second visibility race in the kube-apiserver's label-index
# path: `apply-nodes.sh`'s `kubectl wait --for=condition=Ready
# -l type=kwok` matches via the watch cache and returns before the
# follow-up `kubectl get -l type=kwok` from a fresh subshell can see
# the same labels (observed ~100 ms gap on KWOK Tier-1 CI). 10 tries
# at 0.5 s gives the apiserver up to 5 s to converge — well past any
# real race window, still tight enough to surface a genuinely empty
# cluster within a normal CI step.
ensure_kwok_context() {
    ensure_kwok_context_loose

    local i
    for i in $(seq 1 10); do
        if kubectl get nodes -l type=kwok -o name 2>/dev/null | grep -q . ; then
            return 0
        fi
        sleep 0.5
    done

    echo "[ERROR] ensure_kwok_context: refusing to run — current context has no kwok-typed nodes (checked 10× over 5s)" >&2
    echo "[ERROR] Run kwok/scripts/apply-nodes.sh <recipe> first, or verify you're against the right cluster" >&2
    exit 1
}
