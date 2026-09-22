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
#
# Gates what this lane exists to prove: NVSentinel's GPU health monitor runs
# and reports with no GPU hardware, given a DCGM host engine over a mocked NVML
# driver.
#
# WHY THIS IS NOT IN THE RECIPE'S HEALTH CHECK. recipes/checks/nvsentinel
# asserts these DaemonSets tolerantly, because desiredNumberScheduled is
# legitimately 0 wherever no DCGM pod exists, which is every lane but this one.
# Tightening the shared check would fail those lanes for being correctly
# configured. This lane SUPPLIES the host engine, so it is the only place that
# can demand the monitor actually be Ready. Same split, and the same reason, as
# verify-topology.sh.
#
# IT MUST FAIL, NOT SKIP. Every failure mode here is silent: the monitor sits
# at desiredNumberScheduled 0 and nothing errors. A check that reports "no GPU
# node, skipped" would be green in exactly the state this lane is meant to
# catch, so every path below exits non-zero instead.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./setup-gpu-sim.sh
source "${SCRIPT_DIR}/setup-gpu-sim.sh"

NVSENTINEL_NAMESPACE="nvsentinel"
DCGM_VERSION_LABEL="nvsentinel.dgxc.nvidia.com/dcgm.version"
# The monitor image is comparable in size to the host engine's and is pulled
# after it, so it gets the same budget for the same reason: a wait that expires
# on a cold pull fails the lane for being slow rather than wrong. Measured on
# this hardware, a 905MB image took 338s cold, and the same image took over
# 16 minutes while another large pull was in flight on the node.
MONITOR_TIMEOUT="${MONITOR_TIMEOUT:-900}"

# --- pure -------------------------------------------------------------------

# expected_dcgm_major <image-reference>
#
# Derives the dcgm.version the labeler will write, using ITS rule rather than a
# hardcoded answer: labeler/pkg/labeler/labeler.go matches `.*dcgm:<major>\..*`
# against the image string and maps 3 and 4 to "3.x" and "4.x". Deriving it
# here means a pin that the labeler cannot parse fails in this lane, at the
# point the pin changes, instead of surfacing as an absent DaemonSet later.
expected_dcgm_major() {
    local ref="$1" major
    major="$(sed -nE 's|.*dcgm:([0-9]+)\..*|\1|p' <<<"${ref}")"
    case "${major}" in
        3 | 4) printf '%s.x' "${major}" ;;
        *)
            echo "error: no labeler-parseable dcgm:<major>. in '${ref}'" >&2
            return 1
            ;;
    esac
}

# monitor_daemonset <dcgm-version>
#
# The DaemonSet that version activates. Its sibling stays at 0 by design: the
# labeler writes exactly one version, so asserting both would fail forever.
monitor_daemonset() {
    local version="$1"
    case "${version}" in
        3.x | 4.x) printf 'gpu-health-monitor-dcgm-%s' "${version}" ;;
        *)
            echo "error: '${version}' is not a dcgm.version the chart renders" >&2
            return 1
            ;;
    esac
}

# monitor_sibling <dcgm-version>
monitor_sibling() {
    local version="$1"
    case "${version}" in
        4.x) printf 'gpu-health-monitor-dcgm-3.x' ;;
        3.x) printf 'gpu-health-monitor-dcgm-4.x' ;;
        *)
            echo "error: '${version}' is not a dcgm.version the chart renders" >&2
            return 1
            ;;
    esac
}

# daemonset_is_ready <desired> <ready>
#
# Ready means every scheduled pod is ready AND at least one was scheduled. The
# second half is the one that matters: desired=0/ready=0 satisfies
# "ready == desired" while proving the monitor never ran.
daemonset_is_ready() {
    local desired="${1:-0}" ready="${2:-0}"
    [[ "${desired}" =~ ^[0-9]+$ && "${ready}" =~ ^[0-9]+$ ]] || return 1
    ((desired > 0)) && ((ready == desired))
}

# --- live -------------------------------------------------------------------

ds_field() {
    local context="$1" name="$2" field="$3"
    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        get daemonset "${name}" -n "${NVSENTINEL_NAMESPACE}" \
        -o "jsonpath={.status.${field}}" 2>/dev/null
}

main() {
    local cluster="${1:-${DEFAULT_CLUSTER_NAME}}"
    local context="kind-${cluster}" version ds sibling desired ready labelled deadline

    version="$(expected_dcgm_major "$(dcgm_image_ref)")" || return 1
    ds="$(monitor_daemonset "${version}")" || return 1
    sibling="$(monitor_sibling "${version}")" || return 1
    echo "expecting ${DCGM_VERSION_LABEL}=${version} and ${ds} to be ready"

    # The labeler writes the node label only once a DCGM pod is Ready, so this
    # failing points at the host engine rather than at NVSentinel.
    labelled="$(kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        get nodes -l "${DCGM_VERSION_LABEL}=${version}" -o name 2>/dev/null | wc -l | tr -d ' ')"
    if [[ "${labelled}" == "0" ]]; then
        echo "error: no node carries ${DCGM_VERSION_LABEL}=${version}." >&2
        echo "       the labeler writes it from a Ready pod labelled app=${DCGM_NAME}" >&2
        echo "       whose image matches dcgm:<major>. Check the host engine first." >&2
        return 1
    fi
    echo "${labelled} node(s) carry ${DCGM_VERSION_LABEL}=${version}"

    deadline=$(( $(date +%s) + MONITOR_TIMEOUT ))
    while [[ "$(date +%s)" -lt "${deadline}" ]]; do
        desired="$(ds_field "${context}" "${ds}" desiredNumberScheduled)"
        ready="$(ds_field "${context}" "${ds}" numberReady)"
        daemonset_is_ready "${desired:-0}" "${ready:-0}" && break
        sleep 10
    done
    if ! daemonset_is_ready "${desired:-0}" "${ready:-0}"; then
        echo "error: ${ds} is not ready after ${MONITOR_TIMEOUT}s" \
            "(desired=${desired:-0} ready=${ready:-0})" >&2
        return 1
    fi
    echo "${ds} is ready (desired=${desired} ready=${ready})"

    # The sibling must stay at 0. If both ever schedule, the labeler wrote two
    # versions and one monitor is reading a host engine that is not there.
    desired="$(ds_field "${context}" "${sibling}" desiredNumberScheduled)"
    if [[ "${desired:-0}" != "0" ]]; then
        echo "error: ${sibling} scheduled ${desired} pod(s); the two are mutually exclusive" >&2
        return 1
    fi
    echo "${sibling} is correctly inactive"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
