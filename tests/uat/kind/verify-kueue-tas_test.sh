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
# Guards verify-kueue-tas.sh's pure domain derivation.
#
# domains_used is what decides pass/fail for the positive check in
# verify-kueue-tas.sh: it is what notices a Job's pods spread across domains
# instead of staying within one. A regression there would still look correct
# against a live cluster most of the time (TAS usually does pack pods
# correctly), so the cases below replay the two ways it can go wrong -- a pod
# on an unlabelled node, and pods actually split across domains -- on every
# `make test`, with no cluster.
#
# Hermetic: sources the script (which touches no cluster when sourced) and
# calls only its pure helper.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Resolve the subject SCRIPT_DIR-relative so this exercises the file in THIS
# worktree, never a deployed copy.
# shellcheck source=./verify-kueue-tas.sh
source "${SCRIPT_DIR}/verify-kueue-tas.sh"

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

# --- the cluster this suite models -------------------------------------------
#
# Two pods on the two workers the bootstrap labels cq0, matching the
# accelerator.topograph.run/domain value the dra provider derives from each
# node's nvidia.com/gpu.clique (h100-kind-training-kueue.yaml).
NODES="aicr-uat-slurm-control-plane
aicr-uat-slurm-worker cq0
aicr-uat-slurm-worker2 cq0
aicr-uat-slurm-worker3 cq1
aicr-uat-slurm-worker4 cq1"

PODS_SAME_DOMAIN="tas-verify-spread-0 aicr-uat-slurm-worker
tas-verify-spread-1 aicr-uat-slurm-worker2"

PODS_SPLIT_DOMAINS="tas-verify-spread-0 aicr-uat-slurm-worker
tas-verify-spread-1 aicr-uat-slurm-worker3"

# --- domains_used -------------------------------------------------------------

check "pods within one domain collapse to one row" \
    "cq0" \
    "$(domains_used "${PODS_SAME_DOMAIN}" "${NODES}")"

check "pods split across domains report both, sorted" \
    "$(printf 'cq0\ncq1')" \
    "$(domains_used "${PODS_SPLIT_DOMAINS}" "${NODES}")"

# A pod on the control-plane (no domain label at all) must fail the
# derivation rather than being silently excluded -- excluding it would let a
# 2-pod Job where one pod landed off-domain report a single-element domain
# set from the one remaining pod, passing the very check this guards.
out="$(domains_used "tas-verify-spread-0 aicr-uat-slurm-control-plane" "${NODES}" 2>&1)"
rc=$?
check "pod on an unlabelled node fails closed" "1" "${rc}"
case "${out}" in
    *"carries no ${ACCELERATOR_DOMAIN_LABEL} label"*) echo "ok: names the missing label" ;;
    *)
        echo "FAIL: error does not name the missing label: ${out}" >&2
        fail=1
        ;;
esac

# A pod with no nodeName (still Pending, reached this function anyway) fails
# closed rather than being silently excluded, for the same reason.
out="$(domains_used "tas-verify-spread-0 " "${NODES}" 2>&1)"
rc=$?
check "pod with no assigned node fails closed" "1" "${rc}"

# Empty input is a caller bug (job_pod_table found nothing), not an empty
# domain set -- an empty set would make the "exactly 1 domain" check in
# check_admitted_within_domain pass on zero evidence.
out="$(domains_used "" "${NODES}" 2>&1)"
rc=$?
check "no pods fails closed rather than returning an empty set" "1" "${rc}"

# --- nodes_used ---------------------------------------------------------------
#
# The positive check requires at least two distinct nodes. Counting pod rows
# instead would report two pods on one node as a two-node spread.

check "pods on two nodes report both, sorted" \
    "$(printf 'aicr-uat-slurm-worker\naicr-uat-slurm-worker2')" \
    "$(nodes_used "${PODS_SAME_DOMAIN}")"

check "two pods on one node collapse to one node" \
    "aicr-uat-slurm-worker" \
    "$(nodes_used "tas-verify-spread-0 aicr-uat-slurm-worker
tas-verify-spread-1 aicr-uat-slurm-worker")"

check "a pod with no node is not counted" \
    "aicr-uat-slurm-worker" \
    "$(nodes_used "tas-verify-spread-0 aicr-uat-slurm-worker
tas-verify-spread-1 ")"

# --- cleanup_jobs -------------------------------------------------------------
#
# main's EXIT trap calls this on every exit path, interrupts included, so it
# must delete every named Job in the right context and must not turn a failed
# delete into a changed exit status.
KUBECTL_CALLS=""
kubectl() {
    KUBECTL_CALLS="${KUBECTL_CALLS}$*;"
    return 1
}
cleanup_jobs ctx-x job-a job-b
rc=$?
check "cleanup_jobs succeeds even when every delete fails" "0" "${rc}"
case "${KUBECTL_CALLS}" in
    *"--context ctx-x"*"delete job job-a"*"delete job job-b"*) echo "ok: deletes both Jobs in the given context" ;;
    *)
        echo "FAIL: cleanup_jobs did not delete both Jobs in ctx-x: ${KUBECTL_CALLS}" >&2
        fail=1
        ;;
esac
case "${KUBECTL_CALLS}" in
    *"--wait=false"*) echo "ok: does not wait on the delete" ;;
    *)
        echo "FAIL: cleanup_jobs waits on delete; a second interrupt would hang: ${KUBECTL_CALLS}" >&2
        fail=1
        ;;
esac
unset -f kubectl

# --- main's cleanup verdict ---------------------------------------------------
#
# Drives main with every cluster-touching function stubbed, so only the
# delete_job_and_wait calls decide the result. The Jobs have fixed names, so a
# cleanup that fails must fail the run rather than leave a Job behind a green
# exit. Calls 1 and 3 are the pre-apply deletes, 2 and 4 the post-check ones.
main_with_failing_delete() {
    FAIL_ON="$1"
    (
        DELETE_CALLS=0
        node_domain_table() { printf 'n1 cq0\nn2 cq0\nn3 cq1\nn4 cq1\n'; }
        apply_spread_job() { return 0; }
        check_admitted_within_domain() { return 0; }
        check_refused_across_domains() { return 0; }
        kubectl() { return 0; }
        delete_job_and_wait() {
            DELETE_CALLS=$((DELETE_CALLS + 1))
            [[ "${DELETE_CALLS}" -ne "${FAIL_ON}" ]]
        }
        main aicr-uat-slurm >/dev/null 2>&1
    )
}
main_with_failing_delete 0
check "main passes when every delete succeeds" "0" "$?"
main_with_failing_delete 2
check "main fails when the spread Job cannot be cleaned up" "1" "$?"
main_with_failing_delete 4
check "main fails when the too-large Job cannot be cleaned up" "1" "$?"

if [[ "${fail}" -ne 0 ]]; then
    echo "FAILED" >&2
    exit 1
fi
echo "All verify-kueue-tas.sh unit tests passed"
