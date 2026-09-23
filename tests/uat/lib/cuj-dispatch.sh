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
# Selects the CUJ phase for a cell. The CUJ is a function of BOTH intent and
# platform: platform decides whether a workload CUJ exists at all, intent
# decides which one. Slurm leaves have no K8s-native CUJ (a TrainJob would
# bypass slurmd), so they run conformance and health checks only.
# Source guard: constants and functions only, no side effects at source time
# (same contract as kwok/scripts/lib/sync-budget.sh).

# cuj_normalize_criteria <value>
#
# Lowercases <value> and strips leading and trailing whitespace: the shell
# counterpart of the strings.ToLower(strings.TrimSpace(s)) that ParseIntent
# (pkg/recipe/criteria.go:198) and ParsePlatform (:299) key on.
#
# `tr` and `sed` rather than ${value,,} and the ${value##[[:space:]]*} family
# because macOS ships bash 3.2, where the case-modifying expansion is a syntax
# error.
#
# Whitespace here means the ASCII class. Go's TrimSpace also covers the Unicode
# set; a criteria value carrying, say, a non-breaking space would normalize
# differently in the two languages. That gap is not worth a UTF-8-aware trim in
# shell, and it fails safe: the value simply misses its arm, the same outcome as
# any unrecognised platform.
cuj_normalize_criteria() {
    printf '%s' "${1:-}" \
        | tr '[:upper:]' '[:lower:]' \
        | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}

# cuj_phase_for <intent> <platform>
#
# Prints `train`, `serve`, or `none` and returns 0.
#
# An unset platform keeps the pre-existing intent-only behaviour, which is the
# backward-compatibility guarantee for every cell whose config predates the
# platform coordinate.
cuj_phase_for() {
    local intent="${1:-training}" platform="${2:-}"

    # Normalize both coordinates the way pkg/recipe/criteria.go does before it
    # matches. Callers read a resolved recipe, where ParseIntent and
    # ParsePlatform have already applied this, so the call below is defence in
    # depth: a caller reading a RAW config instead would see `Slurm` (or
    # ` slurm `, since a quoted YAML scalar keeps its padding), miss the arm
    # below, fall through to the intent switch, and apply a TrainJob to a
    # cluster with no TrainJob CRD.
    intent="$(cuj_normalize_criteria "${intent}")"
    platform="$(cuj_normalize_criteria "${platform}")"

    case "${platform}" in
        slurm) printf '%s' "none"; return 0 ;;
    esac
    case "${intent}" in
        inference) printf '%s' "serve" ;;
        *)         printf '%s' "train" ;;
    esac
    return 0
}
