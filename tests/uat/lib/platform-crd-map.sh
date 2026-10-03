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
# shellcheck shell=bash
#
# Maps a recipe's declared criteria.platform to the workload CRD that proves
# the matching operator is installed. Source guard: constants and functions
# only, no side effects at source time (same contract as
# kwok/scripts/lib/sync-budget.sh).
#
# Two consumers read this map: phase_conformance's platform-coordinate
# cross-check (a gate) and collect_cluster_debug's workload-CR capture (a
# diagnostic). Both derive from PLATFORM_WORKLOAD_CRD_MAP below rather than
# carrying their own literal list, so they cannot drift apart. Adding a
# platform is a one-line edit here.
#
# Deliberately not `readonly`: phases.sh and collect-debug.sh each source this
# file, so one shell sources it twice and a readonly reassignment would abort
# the run.
PLATFORM_WORKLOAD_CRD_MAP="dynamo:dynamographdeployments.nvidia.com
kubeflow:trainjobs.trainer.kubeflow.org
slurm:nodesets.slinky.slurm.net"

# platform_workload_crd <platform>
#
# Prints the workload CRD name for <platform> and returns 0.
#
# Fails closed: an unrecognised platform returns 1 with empty stdout so the
# caller can distinguish "no mapping wired" from "mapped to nothing".
platform_workload_crd() {
    local platform="${1:-}" entry
    while IFS= read -r entry; do
        if [[ "${entry%%:*}" == "${platform}" ]]; then
            printf '%s' "${entry#*:}"
            return 0
        fi
    done <<<"${PLATFORM_WORKLOAD_CRD_MAP}"
    return 1
}

# platform_workload_platforms
#
# Prints every platform platform_workload_crd knows, one per line, in map
# order. A caller that must sweep all platforms (the debug-bundle collector)
# iterates this and resolves each name back through platform_workload_crd,
# so the sweep stays in step with the gate.
platform_workload_platforms() {
    local entry
    while IFS= read -r entry; do
        printf '%s\n' "${entry%%:*}"
    done <<<"${PLATFORM_WORKLOAD_CRD_MAP}"
}
