#!/bin/bash
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

# Guards the CUJ phase selection used by uat_main's `all` arm.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Resolve the subject SCRIPT_DIR-relative, never via $HOME or a deployed copy,
# so this exercises the file in THIS worktree.
# shellcheck source=./cuj-dispatch.sh
source "${SCRIPT_DIR}/cuj-dispatch.sh"

fail=0
check() {
    local desc="$1" want="$2" got="$3"
    if [[ "$want" != "$got" ]]; then
        echo "FAIL: ${desc}: want '${want}', got '${got}'" >&2
        fail=1
    else
        echo "ok: ${desc}"
    fi
}

check "kubeflow training runs the TrainJob CUJ" \
    "train" "$(cuj_phase_for training kubeflow)"
check "dynamo inference runs the serve CUJ" \
    "serve" "$(cuj_phase_for inference dynamo)"
check "slurm training runs NO kubeflow CUJ" \
    "none" "$(cuj_phase_for training slurm)"
check "slurm inference also runs no CUJ" \
    "none" "$(cuj_phase_for inference slurm)"
check "unset platform still defaults to training behaviour" \
    "train" "$(cuj_phase_for training '')"
# The inference half of the same compatibility promise. Its sibling above only
# covers training, so without this an intent-only cell whose config predates
# the platform coordinate could regress to `train` unnoticed.
check "unset platform still defaults to inference behaviour" \
    "serve" "$(cuj_phase_for inference '')"

# Criteria values reach this function from a resolved recipe, where
# ParsePlatform and ParseIntent have already applied
# strings.ToLower(strings.TrimSpace(s)) (pkg/recipe/criteria.go). Fold case here
# too so the dispatcher agrees with the conformance gate no matter which source
# a caller reads: a cell declaring `platform: Slurm` must not miss the slurm arm,
# fall through to the intent switch, and apply a Kubeflow TrainJob on a cluster
# that has no TrainJob CRD.
check "a capitalised platform still routes to no CUJ" \
    "none" "$(cuj_phase_for training Slurm)"
check "an upper-case platform still routes to no CUJ" \
    "none" "$(cuj_phase_for training SLURM)"
check "a capitalised intent still routes to serve" \
    "serve" "$(cuj_phase_for Inference dynamo)"
# TrimSpace is the other half of the Go normalization. A quoted YAML scalar
# keeps its padding (`platform: " Slurm "` reaches a raw-config reader as
# ` Slurm `), so folding case alone would still miss the arm.
check "a padded platform still routes to no CUJ" \
    "none" "$(cuj_phase_for training ' Slurm ')"
check "a padded intent still routes to serve" \
    "serve" "$(cuj_phase_for ' Inference ' dynamo)"

# uat_main's dispatch must fail closed on a CUJ phase it does not handle.
# cuj_phase_for is total today, so the only way to reach that arm is to stub it
# (one layer deep) and drive the real uat_main. Every phase, plus yq, is stubbed
# as well, so this stays hermetic: no cluster, no aicr binary, no yq.
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# Only the config's EXISTENCE matters here (uat_main tests -f and hands it to
# the stubbed phase_prep), so no cluster state or parsing is involved.
DISPATCH_CONFIG="tests/uat/kind/tests/h100-training-config.yaml"

# Drives the real uat_main with cuj_phase_for stubbed to yield <phase-value>.
# stdout is discarded here; stderr is left on fd 2 so a caller can choose to
# capture it.
dispatch_run() {
    (
        cd "${REPO_ROOT}" || exit 9
        AICR_BIN=/bin/true RUN_ID=test bash -c '
            source tests/uat/lib/phases.sh
            phase_prep(){ :; }; phase_install(){ :; }; phase_conformance(){ :; }
            phase_train(){ echo "TRAIN RAN"; }; phase_serve(){ echo "SERVE RAN"; }
            phase_verify(){ echo "VERIFY RAN"; }
            yq(){ case "$*" in *intent*) echo training ;; *platform*) echo kubeflow ;; esac; }
            cuj_phase_for(){ printf "%s" "'"${1}"'"; }
            uat_main all "'"${DISPATCH_CONFIG}"'"
        ' >/dev/null
    )
}

dispatch_rc()     { dispatch_run "$1" 2>/dev/null; echo "$?"; }
dispatch_stderr() { dispatch_run "$1" 2>&1; }

check "an unhandled CUJ phase aborts the run rather than skipping it silently" \
    "2" "$(dispatch_rc sbatch)"
# The exit code alone leaves the operator-facing annotation unguarded: deleting
# the ::error:: line keeps rc=2 and the suite green. Pin the exact message,
# interpolated coordinates included, since that line is the only thing telling
# a human reading the CI log which cell tripped the abort.
check "the abort names the offending coordinates on stderr" \
    "::error::unhandled CUJ phase for intent=training platform=kubeflow" \
    "$(dispatch_stderr sbatch)"
# Discrimination control: proves the harness above reports 2 because of the
# unhandled value, not because the harness itself is broken.
check "a handled CUJ phase still runs to completion" \
    "0" "$(dispatch_rc train)"

exit "${fail}"
