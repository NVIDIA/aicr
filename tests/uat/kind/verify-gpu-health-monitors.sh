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
# omits these DaemonSets: it excludes the 3.x one by name and never lists the
# 4.x one, because both legitimately sit at desiredNumberScheduled 0 on any
# lane that supplies no DCGM host engine. The upstream
# gpu-operator chart ships its standalone host engine off (dcgm.enabled: false);
# AICR's values turn it on (recipes/components/gpu-operator/values.yaml:46-47)
# and recipes/overlays/kind.yaml turns it off again. Asserting them in the
# shared check would fail those lanes for being correctly configured. This lane
# SUPPLIES the host engine, so it can demand that the monitor is Ready and
# connected to it. Same split, and the same reason, as verify-topology.sh.
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
# No image pull happens in this budget. The monitor image is 2.5GB beyond what
# it shares with the host engine, and its pull cannot wait for this gate: the
# labeler stamps the label during install, so the pull would land inside
# nvsentinel's 600s helm --wait. setup-gpu-sim.sh pre-pulls it in bootstrap
# under MONITOR_PREPULL_TIMEOUT instead, so by the time install schedules the
# monitor, the node already has the image.
#
# What is left: the label (normally written during install), scheduling and
# starting the container from the node's own store, and the first connect one
# 15s poll later, with room for the kubelet's first four restart back-offs
# (10s, 20s, 40s, 80s) if a monitor started before its host engine answered.
MONITOR_TIMEOUT="${MONITOR_TIMEOUT:-300}"
MONITOR_INTERVAL=10
# Long enough for the first health check after any init line a read has seen
# to be logged, even when it hangs. At v1.25.0 it starts one 15s poll after
# the init (pollIntervalSeconds, gpu-health-monitor values.yaml:46; AICR does
# not override it), and a hung check logs nothing until the probe watchdog's
# deadline of 3 x pollIntervalSeconds = 45s has passed
# (templates/configmap.yaml:27-34), on its next 1s tick (dcgm.py:45): 61s
# after the init. 75s is that plus one more poll of margin.
MONITOR_CONFIRM_WAIT=75
# The chart names the monitor container after itself.
MONITOR_CONTAINER="gpu-health-monitor"
MONITOR_INIT_PATTERN='dcgm gpu_id are \[[0-9, ]*\]'
# What the monitor logs at v1.25.0 when it loses DCGM, all in
# gpu_health_monitor/dcgm_watcher/dcgm.py: a hung probe (:195), a failed or
# timed-out health check (:735, :740, :1259), a failed connect (:1064), and a
# failed or rolled-back initialisation (:1142, :1246).
DCGM_FAILURE_PATTERN='treating the DCGM probe as unresponsive|Indicating connectivity failure|DCGM connectivity failure detected|Error creating DCGM handle|DCGM monitoring initialization failed|Error getting DCGM handle'

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

# monitor_gpu_count <log>
#
# Prints how many GPUs the monitor's latest DCGM initialisation watches, read
# from its "dcgm gpu_id are [...]" line (gpu_health_monitor/dcgm_watcher/
# dcgm.py:1098 at v1.25.0). The monitor logs it only after it has connected,
# grouped every supported GPU DCGM discovered and set its health watches. That
# makes it the evidence numberReady is not: readiness is GET /metrics, served
# once the HTTP server starts, and the watch loop stays alive while it retries
# a host engine it cannot reach. Fails with no output when there is no line.
monitor_gpu_count() {
    local line count
    line="$(sed -nE 's/.*dcgm gpu_id are \[([0-9, ]*)\].*/ids:\1/p' <<<"$1" | tail -1)"
    [[ -n "${line}" ]] || return 1
    # grep -c exits 1 on a count of 0, which is still an answer.
    count="$(tr ',' '\n' <<<"${line#ids:}" | grep -c '[0-9]')"
    printf '%s' "${count}"
}

# monitor_failed_after_init <log>
#
# Succeeds when a DCGM failure line follows the latest init line. The init
# line is logged before the first health check, so a monitor that connected
# and then lost its host engine keeps that line and shows the loss only after
# it. Failures before it are a monitor that started before its host engine.
monitor_failed_after_init() {
    INIT="${MONITOR_INIT_PATTERN}" FAILURE="${DCGM_FAILURE_PATTERN}" awk '
        $0 ~ ENVIRON["INIT"] { failed = 0; next }
        $0 ~ ENVIRON["FAILURE"] { failed = 1 }
        END { exit !failed }
    ' <<<"$1"
}

# monitor_changes <before> <after>
#
# Names each monitor in <after> that is not the one <before> read: another
# pod, a restarted container, or a new DCGM initialisation. Both are lists of
# monitor_evidence "ok" lines.
monitor_changes() {
    awk '
        NR == FNR { pod[$2] = $3; restarts[$2] = $4; inits[$2] = $5; next }
        $3 != pod[$2] { print $2 " (pod replaced between reads)"; next }
        $4 != restarts[$2] { print $2 " (restarted between reads)"; next }
        $5 != inits[$2] { print $2 " (re-initialised DCGM between reads)" }
    ' <(printf '%s\n' "$1") <(printf '%s\n' "$2")
}

# worker_nodes <cluster>
#
# The workers setup-gpu-sim.sh runs a host engine on, one name per line, which
# is every node this lane expects a monitor on.
worker_nodes() {
    local cluster="$1" index
    for index in $(worker_indices); do
        kind_worker_node "${cluster}" "${index}" || return 1
        printf '\n'
    done
}

# unlabelled_workers <workers> <labelled>
#
# Prints each of <workers> that <labelled> does not name, one per line; both
# are newline-separated node names. By name rather than by count, so a labelled
# node that is not a worker cannot stand in for a worker that is missing.
unlabelled_workers() {
    local workers="$1" labelled="$2" node
    while IFS= read -r node; do
        [[ -n "${node}" ]] || continue
        grep -qxF -- "${node}" <<<"${labelled}" || printf '%s\n' "${node}"
    done <<<"${workers}"
}

# --- live -------------------------------------------------------------------

# monitor_pods <context> <daemonset>
#
# Prints "<node> <pod> <restartCount> <ready>" for each pod <daemonset> owns,
# one per line, the last two for the monitor container. A pod with no
# container status yet prints neither, which reads as not ready.
monitor_pods() {
    local context="$1" ds="$2"
    local status=".status.containerStatuses[?(@.name==\"${MONITOR_CONTAINER}\")]"
    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        get pods -n "${NVSENTINEL_NAMESPACE}" \
        -o "jsonpath={range .items[?(@.metadata.ownerReferences[0].name==\"${ds}\")]}{.spec.nodeName}{\" \"}{.metadata.name}{\" \"}{${status}.restartCount}{\" \"}{${status}.ready}{\"\\n\"}{end}" \
        2>/dev/null
}

# monitor_evidence <context> <daemonset> <workers>
#
# Reads each worker's monitor once. Prints "ok <node> <pod> <restarts> <inits>"
# for a Ready monitor whose latest init line lists GPUS_PER_WORKER GPUs with no
# DCGM failure after it, and "fail <node> (<reason>)" for any other.
monitor_evidence() {
    local context="$1" ds="$2" workers="$3"
    local pods node pod restarts ready logs count
    pods="$(monitor_pods "${context}" "${ds}")"
    while IFS= read -r node; do
        [[ -n "${node}" ]] || continue
        read -r pod restarts ready <<<"$(awk -v n="${node}" '$1 == n { print $2, $3, $4; exit }' <<<"${pods}")"
        if [[ -z "${pod}" ]]; then
            echo "fail ${node} (no monitor pod)"
            continue
        fi
        # kubectl logs serves a crash-looping container's last terminated
        # instance, whose log can end on a clean init.
        if [[ "${ready}" != "true" ]]; then
            echo "fail ${node} (monitor container not ready)"
            continue
        fi
        if ! logs="$(kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
            logs -n "${NVSENTINEL_NAMESPACE}" "${pod}" -c "${MONITOR_CONTAINER}" 2>/dev/null)"; then
            echo "fail ${node} (logs unreadable)"
            continue
        fi
        if ! count="$(monitor_gpu_count "${logs}")"; then
            echo "fail ${node} (no 'dcgm gpu_id are' line)"
        elif [[ "${count}" != "${GPUS_PER_WORKER}" ]]; then
            echo "fail ${node} (${count} GPU(s))"
        elif monitor_failed_after_init "${logs}"; then
            echo "fail ${node} (DCGM failure after its latest 'dcgm gpu_id are' line)"
        else
            echo "ok ${node} ${pod} ${restarts} $(grep -cE "${MONITOR_INIT_PATTERN}" <<<"${logs}")"
        fi
    done <<<"${workers}"
}

ds_field() {
    local context="$1" name="$2" field="$3"
    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        get daemonset "${name}" -n "${NVSENTINEL_NAMESPACE}" \
        -o "jsonpath={.status.${field}}" 2>/dev/null
}

main() {
    local cluster="${1:-${DEFAULT_CLUSTER_NAME}}"
    local context="kind-${cluster}" version ds sibling desired ready
    local workers expected labelled missing deadline
    local evidence baseline problems

    version="$(expected_dcgm_major "$(dcgm_image_ref)")" || return 1
    ds="$(monitor_daemonset "${version}")" || return 1
    sibling="$(monitor_sibling "${version}")" || return 1
    workers="$(worker_nodes "${cluster}")" || return 1
    expected="$(grep -c . <<<"${workers}")"
    echo "expecting ${DCGM_VERSION_LABEL}=${version} on ${expected} worker(s) and ${ds} ready on each"

    # One budget for both waits: the DaemonSet cannot schedule before the label
    # exists, so they are one sequence, and each loop reads before it checks
    # the deadline so a label that lands late still gets the DaemonSet read.
    deadline=$((SECONDS + MONITOR_TIMEOUT))

    # The labeler writes the node label from its own event handlers, once a
    # DCGM pod is Ready and its caches have synced. Nothing before this step
    # waits for that, so a worker without it is retried; one that stays
    # unlabelled points at its host engine rather than at NVSentinel.
    while :; do
        labelled="$(kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
            get nodes -l "${DCGM_VERSION_LABEL}=${version}" -o name 2>/dev/null | sed 's|^node/||')"
        missing="$(unlabelled_workers "${workers}" "${labelled}")"
        [[ -z "${missing}" ]] && break
        if ((SECONDS >= deadline)); then
            echo "error: $(grep -c . <<<"${missing}") of ${expected} worker(s) lack" \
                "${DCGM_VERSION_LABEL}=${version} after ${MONITOR_TIMEOUT}s:" \
                "$(paste -sd ' ' - <<<"${missing}")" >&2
            echo "       the labeler writes it from a Ready pod labelled app=${DCGM_NAME}" >&2
            echo "       whose image matches dcgm:<major>. Check the host engine first." >&2
            return 1
        fi
        sleep "${MONITOR_INTERVAL}"
    done
    echo "all ${expected} worker(s) carry ${DCGM_VERSION_LABEL}=${version}"

    # Every worker is labelled by now, so fewer scheduled pods than workers is
    # a worker left without a monitor.
    while :; do
        desired="$(ds_field "${context}" "${ds}" desiredNumberScheduled)"
        ready="$(ds_field "${context}" "${ds}" numberReady)"
        daemonset_is_ready "${desired:-0}" "${ready:-0}" && [[ "${desired}" == "${expected}" ]] && break
        if ((SECONDS >= deadline)); then
            echo "error: ${ds} is not ready on all ${expected} worker(s) after ${MONITOR_TIMEOUT}s" \
                "(desired=${desired:-0} ready=${ready:-0})" >&2
            return 1
        fi
        sleep "${MONITOR_INTERVAL}"
    done
    echo "${ds} is ready (desired=${desired} ready=${ready})"

    # Ready is not connected, so every worker's monitor must also show that it
    # reached a host engine, is watching that worker's GPUs, and has not lost
    # the engine since. The monitor waits one poll interval before its first
    # connect, so the evidence can trail readiness and is polled for in the
    # same budget. A clean read is only a sample: it can fall between an init
    # and the health check that fails or hangs. So it is read again
    # MONITOR_CONFIRM_WAIT later and must show the same pods, with no restart,
    # no new init and still no failure after the latest one.
    baseline=""
    while :; do
        evidence="$(monitor_evidence "${context}" "${ds}" "${workers}")"
        problems="$(sed -n 's/^fail //p' <<<"${evidence}")"
        if [[ -z "${problems}" && -n "${baseline}" ]]; then
            problems="$(monitor_changes "${baseline}" "${evidence}")"
            [[ -z "${problems}" ]] && break
        fi
        if [[ -z "${problems}" ]]; then
            baseline="${evidence}"
            echo "all ${expected} monitor(s) show a DCGM connection; reading them again in ${MONITOR_CONFIRM_WAIT}s"
            sleep "${MONITOR_CONFIRM_WAIT}"
            continue
        fi
        baseline=""
        if ((SECONDS >= deadline)); then
            echo "error: $(grep -c . <<<"${problems}") of ${expected} monitor(s) show no DCGM connection" \
                "with ${GPUS_PER_WORKER} GPU(s) after ${MONITOR_TIMEOUT}s:" \
                "$(grep . <<<"${problems}" | paste -sd ' ' -)" >&2
            echo "       a connected monitor logs 'dcgm gpu_id are [...]' and no DCGM failure after it;" >&2
            echo "       read that worker's monitor log and check the host engine on the same node." >&2
            return 1
        fi
        sleep "${MONITOR_INTERVAL}"
    done
    echo "all ${expected} monitor(s) connected to DCGM and watch ${GPUS_PER_WORKER} GPU(s) each"

    # The sibling must stay at 0. If both ever schedule, the labeler wrote two
    # versions and one monitor is reading a host engine that is not there. The
    # chart renders both DaemonSets, so a read that fails or yields no count is
    # an error, never a 0: defaulting it would pass the gate on a lookup that
    # never happened.
    if ! desired="$(ds_field "${context}" "${sibling}" desiredNumberScheduled)"; then
        echo "error: could not read ${sibling}; a failed lookup does not prove it inactive" >&2
        return 1
    fi
    if [[ ! "${desired}" =~ ^[0-9]+$ ]]; then
        echo "error: ${sibling} reported desiredNumberScheduled '${desired}', not a count" >&2
        return 1
    fi
    if [[ "${desired}" != "0" ]]; then
        echo "error: ${sibling} scheduled ${desired} pod(s); the two are mutually exclusive" >&2
        return 1
    fi
    echo "${sibling} is correctly inactive"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
