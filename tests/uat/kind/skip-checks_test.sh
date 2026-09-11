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

# Guards the simulated lane's conformance skip list.
#
# WHY THIS LIVES HERE AND NOT IN THE VALIDATOR. `aicr validate` rejects a skip
# list that empties a requested phase, and that guard is deliberately generic:
# it fires only when NOTHING is left to run. Leave one check standing and any
# list is accepted, however much else it withholds. So the validator cannot know
# that a particular survivor is the one a caller depends on, and a feature-level
# rule naming one check would be wrong for every other caller.
#
# The lane does know. `slinky-slurm-health` is the check that validates the
# Slurm path this lane exists for; withholding it would leave the lane green
# while testing nothing it was built to test, and the generic guard would not
# notice, because gang-scheduling and cluster-autoscaling would still be there
# to keep the phase non-empty. Verified rather than assumed: with the shipped
# seven skips PLUS slinky-slurm-health, `aicr validate` returns 0.
#
# Hermetic: reads the two committed YAML files, resolves both SCRIPT_DIR-relative
# so it exercises THIS worktree. No cluster, no binary.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

LANE_CONFIG="${SCRIPT_DIR}/tests/h100-training-slurm-config.yaml"
LEAF_OVERLAY="${REPO_ROOT}/recipes/overlays/h100-kind-training-slurm.yaml"

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

for f in "${LANE_CONFIG}" "${LEAF_OVERLAY}"; do
    if [[ ! -f "${f}" ]]; then
        echo "FAIL: ${f} does not exist; this suite is asserting against nothing" >&2
        exit 1
    fi
done

# The checks the lane's scope statement says still run and still gate. Named
# here rather than derived, so that the file a reader meets and the guard that
# enforces it are two independent statements of the same intent: deriving the
# list from the config would make the test agree with whatever the config says.
SURVIVORS=(slinky-slurm-health gang-scheduling cluster-autoscaling)

skip_list="$(yq -r '.spec.validate.execution.skipChecks[]?' "${LANE_CONFIG}")"

# Guard against the assertion below passing because the read returned nothing:
# a renamed field, a moved section or a yq that failed would make every
# "is not skipped" check trivially true.
check "the lane declares a skip list" "1" \
    "$([[ -n "${skip_list}" ]] && echo 1 || echo 0)"

# THE LOAD-BEARING ASSERTION. slinky-slurm-health first, because it is the one
# whose loss would be invisible: the lane would still deploy, still converge,
# still pass its remaining checks, and still emit a signed bundle.
for survivor in "${SURVIVORS[@]}"; do
    check "the lane does not skip ${survivor}" "0" \
        "$(printf '%s\n' "${skip_list}" | grep -cx -- "${survivor}" | tr -d ' ')"
done

# The other half of "it still runs": a check the leaf never declares cannot run
# whether it is skipped or not, so absence from the skip list means nothing on
# its own.
leaf_checks="$(yq -r '.spec.validation.conformance.checks[]?' "${LEAF_OVERLAY}")"
check "the leaf declares slinky-slurm-health, so it is in the run to begin with" "1" \
    "$(printf '%s\n' "${leaf_checks}" | grep -cx -- "slinky-slurm-health" | tr -d ' ')"

# The lane's header calls this out as SEVEN skipped checks and lists what each
# costs. A count that drifts from the prose leaves a reader trusting a number
# that is no longer true, which is the failure this branch exists to remove.
check "the skip list still holds the seven the header describes" "7" \
    "$(printf '%s\n' "${skip_list}" | grep -c '[^[:space:]]' | tr -d ' ')"

if [[ "${fail}" -ne 0 ]]; then
    echo "skip-checks_test.sh: FAILED" >&2
    exit 1
fi
echo "skip-checks_test.sh: all checks passed"
