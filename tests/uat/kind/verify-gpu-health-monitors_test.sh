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

# Guards the pure decisions in verify-gpu-health-monitors.sh.
# Hermetic: sources the script and calls its pure functions, never a cluster.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Resolve the subject SCRIPT_DIR-relative so this exercises the file in THIS
# worktree, never a deployed copy.
# shellcheck source=./verify-gpu-health-monitors.sh
source "${SCRIPT_DIR}/verify-gpu-health-monitors.sh"

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

# --- deriving the label from the pin ---------------------------------------
#
# The lane derives the expected dcgm.version from its own image reference using
# the labeler's rule, so a pin the labeler cannot parse fails here rather than
# as an absent DaemonSet an hour later.
check "a tag+digest reference yields the major" "4.x" \
    "$(expected_dcgm_major 'nvcr.io/nvidia/cloud-native/dcgm:4.6.0-1-ubuntu24.04@sha256:aaaa')"
check "a plain tag yields the major" "3.x" \
    "$(expected_dcgm_major 'nvcr.io/nvidia/cloud-native/dcgm:3.3.9-1-ubuntu22.04')"

# THE REGRESSION THIS EXISTS FOR. Pinning by bare digest, the way every other
# image in this lane is pinned, removes the tag the labeler parses. Nothing
# errors: the label is never written and the monitor never schedules.
out="$(expected_dcgm_major 'nvcr.io/nvidia/cloud-native/dcgm@sha256:aaaa' 2>/dev/null)"; rc=$?
check "a bare digest reference returns empty stdout" "" "${out}"
check "a bare digest reference fails closed" "1" "${rc}"

out="$(expected_dcgm_major 'nvcr.io/nvidia/k8s/dcgm-exporter:4.6.0-4.8.3' 2>/dev/null)"; rc=$?
check "the dcgm-exporter image is not mistaken for a host engine" "1" "${rc}"

out="$(expected_dcgm_major 'nvcr.io/nvidia/cloud-native/dcgm:9.0.0' 2>/dev/null)"; rc=$?
check "an unrenderable major fails closed" "1" "${rc}"

# --- the mutually exclusive pair -------------------------------------------
check "4.x selects the 4.x DaemonSet" "gpu-health-monitor-dcgm-4.x" "$(monitor_daemonset 4.x)"
check "3.x selects the 3.x DaemonSet" "gpu-health-monitor-dcgm-3.x" "$(monitor_daemonset 3.x)"
check "4.x's sibling is 3.x" "gpu-health-monitor-dcgm-3.x" "$(monitor_sibling 4.x)"
check "3.x's sibling is 4.x" "gpu-health-monitor-dcgm-4.x" "$(monitor_sibling 3.x)"
monitor_daemonset 5.x >/dev/null 2>&1; check "an unknown version fails closed" "1" "$?"

# --- readiness, and the vacuous pass it must reject ------------------------
#
# desired=0/ready=0 satisfies "ready equals desired" while proving the monitor
# never ran. That is the exact state this lane exists to catch, so it must read
# as NOT ready.
daemonset_is_ready 1 1; check "one of one is ready" "0" "$?"
daemonset_is_ready 4 4; check "four of four is ready" "0" "$?"
daemonset_is_ready 0 0; check "zero of zero is NOT ready (the vacuous pass)" "1" "$?"
daemonset_is_ready 4 3; check "a partial rollout is not ready" "1" "$?"
daemonset_is_ready 0 1; check "ready exceeding desired is not ready" "1" "$?"
daemonset_is_ready "" ""; check "empty fields are not ready" "1" "$?"
daemonset_is_ready notanumber 1; check "a non-numeric field fails closed" "1" "$?"

# --- the lane must actually run this, and gate on it -----------------------
#
# A guard nobody runs is not a guard. The same reasoning topology-golden_test.sh
# applies to verify-topology.sh applies here: a step whose failure nothing
# depends on lets the run publish signed evidence claiming GPU health coverage
# the lane never demonstrated.
WORKFLOW="${SCRIPT_DIR}/../../../.github/workflows/uat-kind-sim.yaml"
check "the CI lane calls verify-gpu-health-monitors.sh" "1" \
    "$(grep -c 'tests/uat/kind/verify-gpu-health-monitors\.sh' "${WORKFLOW}" | tr -d ' ')"
check "the step has an id the conformance job can depend on" "1" \
    "$(grep -cE '^        id: gpu_health$' "${WORKFLOW}" | tr -d ' ')"
check "conformance is gated on this step" "1" \
    "$(grep -c "steps.gpu_health.outcome == 'success'" "${WORKFLOW}" | tr -d ' ')"
# And it must not have displaced the gate that was already there.
check "conformance is still gated on the topology step" "1" \
    "$(grep -c "steps.topology.outcome == 'success' && steps.gpu_health.outcome == 'success'" "${WORKFLOW}" | tr -d ' ')"

exit "${fail}"
