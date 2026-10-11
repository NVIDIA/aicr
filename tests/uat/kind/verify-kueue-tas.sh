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
# Verifies Kueue topology-aware scheduling (TAS) end to end on the
# aicr-uat-slurm Kind cluster: submits a Job that cannot fit on one node and
# checks that its pods landed in a single Topograph accelerator domain, then
# submits a Job that cannot fit in one domain and checks Kueue refuses to
# admit it.
#
# WHY TWO JOBS, NOT ONE. A Job whose pods all fit on a single node proves
# nothing about TAS: it would land on one node under plain bin-packing with no
# topology constraint in effect at all. Observed on this cluster: a
# 2-pod/1-GPU-each Job landed on one node and its Workload reported
# topologyAssignment with domainCount 1 -- indistinguishable from TAS being
# absent. Only a Job that MUST span nodes (requests more GPU per pod than one
# node holds) exercises the constraint, and only a Job that cannot fit within
# one domain even though the cluster as a whole has the capacity proves
# admission is bounded BY DOMAIN rather than by raw cluster capacity.
#
# WHY THE EXPECTATION IS DERIVED FROM POD PLACEMENT AND NODE LABELS, NOT FROM
# KUEUE'S OWN topologyAssignment FIELD. Mirrors verify-topology.sh's rule for
# the same reason: topologyAssignment.valuesPerLevel uses a compact
# prefix/roots encoding for nodes sharing a name prefix, is not part of
# Kueue's documented API, and reading it back as the proof would make the
# check agree with whatever Kueue emits rather than with where the pods
# actually ran. The ground truth is each pod's spec.nodeName (who landed
# where) and each node's accelerator.topograph.run/domain label (which domain
# Topograph assigned it, via the dra provider reading
# nvidia.com/gpu.clique -- see h100-kind-training-kueue.yaml), exactly the
# two facts verify-topology.sh composes for Slurm. topologyAssignment is only
# checked for existence, confirming the Workload actually carries a topology
# decision and not a size-1 pass-through.
#
# Usage:
#   tests/uat/kind/verify-kueue-tas.sh [cluster-name]
#
# Exits 0 when both checks pass, 1 otherwise. Sourcing this file defines
# constants and functions only and touches no cluster, so verify-kueue-tas_test.sh
# can exercise the pure derivation hermetically.

# shellcheck source=./setup-gpu-sim.sh
source "$(dirname "${BASH_SOURCE[0]}")/setup-gpu-sim.sh"

# Set by AICR (recipes/overlays/h100-kind-training-kueue.yaml, the dra
# provider's engine.params.accelerator source and the k8s engine's default
# accelerator label; recipes/components/kueue/manifests/
# topology-accelerator.yaml, the Topology this leaf ships). Mirrored here
# rather than read from the recipe, the same choice verify-topology.sh makes
# for TOPOLOGY_CONFIGMAP et al.
ACCELERATOR_DOMAIN_LABEL="accelerator.topograph.run/domain"
LOCAL_QUEUE="default"
LOCAL_QUEUE_NAMESPACE="default"

# Convergence budget. TAS admission is near-immediate once Topograph has
# labelled the nodes, but that labelling is itself asynchronous (15s
# requestAggregationDelay in components/topograph/values.yaml), so a run
# started right after install needs to wait on it. Overridable for a shorter
# local loop.
VERIFY_TAS_TIMEOUT="${VERIFY_TAS_TIMEOUT:-180}"
VERIFY_TAS_INTERVAL="${VERIFY_TAS_INTERVAL:-10}"

# --- pure helpers ------------------------------------------------------------

# domains_used <pod-node-table> <node-domain-table>
#
# Prints the sorted, deduplicated set of domains the pods in <pod-node-table>
# actually ran in, one per line.
#
#   pod-node-table     one <pod> <kubernetes-node> row per pod
#   node-domain-table  one <kubernetes-node> <domain> row per node
#
# A pod on a node with no domain label returns 1 rather than being dropped:
# an unlabelled node silently shrinking the observed domain set is exactly
# the failure a domain-count assertion downstream would otherwise miss.
domains_used() {
    local pods="${1:-}" nodes="${2:-}"
    if [[ -z "${pods}" ]]; then
        echo "domains_used: no pods" >&2
        return 1
    fi

    local pod kube_node domain rc=0
    local seen=""
    while read -r pod kube_node; do
        [[ -n "${pod}" ]] || continue
        if [[ -z "${kube_node}" ]]; then
            echo "domains_used: pod ${pod} is not assigned to a node" >&2
            rc=1
            continue
        fi
        domain="$(printf '%s\n' "${nodes}" | awk -v n="${kube_node}" '$1 == n {print $2; exit}')"
        if [[ -z "${domain}" ]]; then
            echo "domains_used: node ${kube_node} (running ${pod}) carries no ${ACCELERATOR_DOMAIN_LABEL} label" >&2
            rc=1
            continue
        fi
        seen="${seen}${domain}
"
    done <<<"${pods}"

    if [[ -z "${seen}" ]]; then
        echo "domains_used: derived no domain membership" >&2
        return 1
    fi
    printf '%s' "${seen}" | sort -u
    return "${rc}"
}

# nodes_used <pod-node-table>
#
# Prints the sorted, deduplicated set of Kubernetes nodes in <pod-node-table>
# (one <pod> <kubernetes-node> row per pod), one per line. Pods with no node
# are skipped; domains_used is what rejects those.
nodes_used() {
    printf '%s\n' "${1:-}" | awk 'NF >= 2 {print $2}' | sort -u
}

# --- live-cluster entry point ------------------------------------------------

# delete_job_and_wait <context> <job-name>
#
# Removes a Job left by an interrupted run and waits for its pods and Workload
# to go. Jobs have fixed names, so without this a rerun's apply would leave the
# old completed Job in place, its succeeded count would already match, and the
# check would evaluate the previous run's pods and Workload.
delete_job_and_wait() {
    local context="$1" job="$2"
    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        delete job "${job}" -n "${LOCAL_QUEUE_NAMESPACE}" \
        --ignore-not-found --cascade=foreground --wait=true --timeout="${VERIFY_TAS_TIMEOUT}s" >&2
}

# cleanup_jobs <context> <job-name>...
#
# Best-effort removal of the Jobs this script creates, run from main's EXIT trap
# so an interrupt or an early return does not leave fixed-name Jobs behind. It
# never waits (a second Ctrl-C should not hang on a finalizer) and never
# changes the exit status: the verdict was already decided, and the next run's
# delete_job_and_wait is what waits for the Jobs to go.
cleanup_jobs() {
    local context="$1" job
    shift
    for job in "$@"; do
        kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
            delete job "${job}" -n "${LOCAL_QUEUE_NAMESPACE}" \
            --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done
    return 0
}

# job_pod_table <context> <job-name>
#
# Prints `<pod> <kubernetes-node>` per pod of <job-name>. Only
# Running/Succeeded pods are listed; a pod still Pending has no nodeName and
# including it would fail the derivation with a confusing "not assigned to a
# node" instead of the readiness message main prints first.
job_pod_table() {
    local context="$1" job="$2"
    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        get pods -n "${LOCAL_QUEUE_NAMESPACE}" -l "job-name=${job}" \
        --field-selector 'status.phase!=Pending,status.phase!=Unknown' \
        -o 'jsonpath={range .items[*]}{.metadata.name} {.spec.nodeName}{"\n"}{end}'
}

# node_domain_table <context>
node_domain_table() {
    local context="$1"
    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" get nodes \
        -o "jsonpath={range .items[*]}{.metadata.name} {.metadata.labels.accelerator\.topograph\.run/domain}{\"\n\"}{end}"
}

# workload_name_for_job <context> <job-name>
#
# Kueue names the Workload after the owning Job plus a hash suffix it does
# not expose as a predictable value, so this resolves it via the
# kueue.x-k8s.io/job-uid label Kueue sets on every Workload it creates for a
# batch/job.
workload_name_for_job() {
    local context="$1" job="$2" uid
    uid="$(kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        get job "${job}" -n "${LOCAL_QUEUE_NAMESPACE}" -o jsonpath='{.metadata.uid}')" || return 1
    [[ -n "${uid}" ]] || return 1
    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        get workloads -n "${LOCAL_QUEUE_NAMESPACE}" -l "kueue.x-k8s.io/job-uid=${uid}" \
        -o jsonpath='{.items[0].metadata.name}'
}

# apply_spread_job <context> <name> <gpu-per-pod> <pod-count>
#
# Submits an Indexed Job of <pod-count> pods each requesting <gpu-per-pod>
# nvidia.com/gpu, annotated for TAS on ACCELERATOR_DOMAIN_LABEL. suspend:
# true lets Kueue own admission; the chart's tas-flavor toleration is not
# needed since this cluster carries no nvidia.com/gpu taint.
apply_spread_job() {
    local context="$1" name="$2" gpu="$3" count="$4"
    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${name}
  namespace: ${LOCAL_QUEUE_NAMESPACE}
  labels:
    kueue.x-k8s.io/queue-name: ${LOCAL_QUEUE}
spec:
  parallelism: ${count}
  completions: ${count}
  completionMode: Indexed
  suspend: true
  template:
    metadata:
      annotations:
        kueue.x-k8s.io/podset-required-topology: ${ACCELERATOR_DOMAIN_LABEL}
    spec:
      restartPolicy: Never
      containers:
        - name: main
          image: busybox:1.37
          command: ["sleep", "20"]
          resources:
            requests:
              cpu: "100m"
              nvidia.com/gpu: "${gpu}"
            limits:
              nvidia.com/gpu: "${gpu}"
EOF
}

# check_admitted_within_domain <context> <nodes> <job-name> <want-count>
#
# Positive check: waits for <job-name> to reach <want-count> succeeded pods,
# then asserts they ran in exactly one domain and the Workload carries a
# non-empty topologyAssignment (confirming TAS, not plain scheduling, placed
# them).
#
# <want-count> is a caller-supplied argument, not inferred from the Job: the
# function has no other way to know what the caller's Job was sized to, and a
# hardcoded expectation here would silently stop matching if a caller ever
# submitted a differently-sized spread Job.
check_admitted_within_domain() {
    local context="$1" nodes="$2" job="$3" want_count="$4"

    local deadline=$(( SECONDS + VERIFY_TAS_TIMEOUT ))
    local succeeded
    while :; do
        succeeded="$(kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
            get job "${job}" -n "${LOCAL_QUEUE_NAMESPACE}" \
            -o jsonpath='{.status.succeeded}' 2>/dev/null || true)"
        [[ "${succeeded}" == "${want_count}" ]] && break
        if (( SECONDS >= deadline )); then
            echo "FAIL: ${job} did not reach ${want_count} succeeded pods within ${VERIFY_TAS_TIMEOUT}s" >&2
            kubectl --context "${context}" get pods -n "${LOCAL_QUEUE_NAMESPACE}" -l "job-name=${job}" -o wide >&2
            return 1
        fi
        sleep "${VERIFY_TAS_INTERVAL}"
    done

    local pods used n
    pods="$(job_pod_table "${context}" "${job}")" || return 1
    used="$(domains_used "${pods}" "${nodes}")" || return 1
    n="$(printf '%s\n' "${used}" | grep -c '[^[:space:]]')"
    if [[ "${n}" -ne 1 ]]; then
        echo "FAIL: ${job}'s pods ran across ${n} domain(s), want exactly 1:" >&2
        printf '%s\n' "${used}" | sed 's/^/  /' >&2
        echo "  pod placement:" >&2
        printf '%s\n' "${pods}" | sed 's/^/    /' >&2
        return 1
    fi

    # The Job is sized so no single node can hold it; pods on one node would
    # mean the spread this check exists to observe never happened.
    local node_count
    node_count="$(nodes_used "${pods}" | grep -c '[^[:space:]]')"
    if [[ "${node_count}" -lt 2 ]]; then
        echo "FAIL: ${job}'s pods ran on ${node_count} node(s), want at least 2:" >&2
        printf '%s\n' "${pods}" | sed 's/^/    /' >&2
        return 1
    fi

    local workload assignment
    workload="$(workload_name_for_job "${context}" "${job}")" || {
        echo "FAIL: could not resolve the Workload for ${job}" >&2
        return 1
    }
    assignment="$(kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        get workload "${workload}" -n "${LOCAL_QUEUE_NAMESPACE}" \
        -o jsonpath='{.status.admission.podSetAssignments[0].topologyAssignment}')" || return 1
    if [[ -z "${assignment}" ]]; then
        echo "FAIL: Workload ${workload} carries no topologyAssignment;" \
            "the pods landed together by coincidence, not by TAS" >&2
        return 1
    fi

    echo "ok: ${job}'s pods spanned ${node_count} nodes within a single domain" \
        "($(printf '%s' "${used}" | tr -d '\n'))"
}

# check_refused_across_domains <context> <job-name>
#
# Negative control: submits a Job sized to need 3 nodes, which no single
# 2-node domain on this cluster can satisfy even though the cluster as a
# whole has the combined capacity, and asserts Kueue's QuotaReserved
# condition stays False for the whole budget. A Job that instead gets
# admitted proves the required-topology annotation is not actually being
# enforced, however the positive check above came out.
check_refused_across_domains() {
    local context="$1" job="$2"

    local deadline=$(( SECONDS + VERIFY_TAS_TIMEOUT ))
    local workload status
    workload=""
    while [[ -z "${workload}" ]]; do
        workload="$(workload_name_for_job "${context}" "${job}" 2>/dev/null || true)"
        if [[ -z "${workload}" ]]; then
            if (( SECONDS >= deadline )); then
                echo "FAIL: no Workload ever appeared for ${job}" >&2
                return 1
            fi
            sleep "${VERIFY_TAS_INTERVAL}"
        fi
    done

    while :; do
        status="$(kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
            get workload "${workload}" -n "${LOCAL_QUEUE_NAMESPACE}" \
            -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].status}' 2>/dev/null || true)"
        if [[ "${status}" == "True" ]]; then
            echo "FAIL: Workload ${workload} (for ${job}, 3 pods needing 3 nodes) was admitted;" \
                "required-topology should have kept it Pending -- no 2-node domain can hold it" >&2
            return 1
        fi
        if (( SECONDS >= deadline )); then
            if [[ "${status}" == "False" ]]; then
                echo "ok: ${job} stayed un-admitted (QuotaReserved=False) for ${VERIFY_TAS_TIMEOUT}s," \
                    "as required-topology demands"
                return 0
            fi
            echo "FAIL: Workload ${workload}'s QuotaReserved never resolved (last: '${status:-<none>}')" >&2
            return 1
        fi
        sleep "${VERIFY_TAS_INTERVAL}"
    done
}

main() {
    local cluster context
    cluster="${1:-${DEFAULT_CLUSTER_NAME}}"
    context="kind-${cluster}"

    local nodes
    nodes="$(node_domain_table "${context}")" || {
        echo "FAIL: could not read nodes from ${context}" >&2
        return 1
    }
    if [[ "$(printf '%s\n' "${nodes}" | awk '$2 != "" {print $1}' | wc -l | tr -d ' ')" -lt 4 ]]; then
        echo "FAIL: fewer than 4 nodes carry ${ACCELERATOR_DOMAIN_LABEL};" \
            "Topograph has not labelled the cluster yet (dra provider, reads ${GPU_CLIQUE_LABEL})" >&2
        return 1
    fi

    local rc=0 spread_job="tas-verify-spread" toolarge_job="tas-verify-toolarge"
    local spread_count=2
    # context and the job names are locals of main, out of scope by the time an
    # EXIT trap fires, so their values are expanded into the trap string now.
    # INT and TERM exit explicitly so the EXIT trap runs on an interrupt.
    # shellcheck disable=SC2064
    trap "cleanup_jobs $(printf '%q ' "${context}" "${spread_job}" "${toolarge_job}")" EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    # Each worker holds 8 simulated GPUs (setup-gpu-sim.sh); 5 per pod means
    # 2 pods cannot both fit on one node, forcing the spread this check
    # exists to observe. 6 per pod across 3 pods needs 18 GPU, which exceeds
    # one domain's 16 (its two nodes' combined capacity) but not the
    # cluster's 32.
    delete_job_and_wait "${context}" "${spread_job}" || return 1
    apply_spread_job "${context}" "${spread_job}" 5 "${spread_count}" || return 1
    check_admitted_within_domain "${context}" "${nodes}" "${spread_job}" "${spread_count}" || rc=1
    delete_job_and_wait "${context}" "${spread_job}" || rc=1

    delete_job_and_wait "${context}" "${toolarge_job}" || return 1
    apply_spread_job "${context}" "${toolarge_job}" 6 3 || return 1
    check_refused_across_domains "${context}" "${toolarge_job}" || rc=1
    delete_job_and_wait "${context}" "${toolarge_job}" || rc=1

    return "${rc}"
}

# Source guard: sourcing defines constants and functions only.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    set -uo pipefail
    main "$@"
fi
