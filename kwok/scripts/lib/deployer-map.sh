#!/usr/bin/env bash
# shellcheck shell=bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Matrix-deployer name -> aicr `--deployer` value.
#
# The KWOK matrix names a {deployer, bundle source} pair (argocd-git,
# flux-oci) because the source is what those lanes exist to vary. aicr's
# --deployer names only the deployer. The transform is a table rather than a
# string operation: argocd-helm-oci maps to argocd-helm, not to argocd, and
# no prefix rule produces both that and argocd-git -> argocd.
#
# It lives here because several call sites need the same answer: `aicr
# bundle` in every generate_bundle branch, and both halves of the
# upgrade-check inventory read-back. A deployer name that disagrees between
# them is not a visible error: pkg/inventory maps release names per
# deployer, so the wrong one simply matches nothing and reports a bare
# cluster.
#
# Source guard: functions only, no side effects at source time (same
# contract as lib/cleanup.sh).

# aicr_deployer_for <matrix-deployer>
#
# Prints the aicr --deployer value for a matrix deployer name on stdout.
# Returns 1, printing a diagnostic to stderr and nothing to stdout, for a
# name the table does not cover: a new matrix entry has to be mapped here
# deliberately, because every wrong answer is the silent kind above.
aicr_deployer_for() {
    case "${1:-}" in
        helm)
            echo "helm"
            ;;
        argocd-oci|argocd-git)
            echo "argocd"
            ;;
        argocd-helm-oci)
            echo "argocd-helm"
            ;;
        flux-oci|flux-git)
            echo "flux"
            ;;
        *)
            echo "no aicr --deployer mapping for matrix deployer '${1:-}'" >&2
            return 1
            ;;
    esac
}
