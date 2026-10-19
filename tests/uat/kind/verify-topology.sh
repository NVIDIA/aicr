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
# Checks the topology.conf Topograph wrote against an expectation DERIVED FROM
# THE CLUSTER, not from Topograph.
#
# WHY THIS IS NOT A GOLDEN FILE. A block groups SLURM nodes, and a Slurm node
# is a slurmd POD, so the names in topology.conf encode which pod the scheduler
# put on which Kubernetes node. The leaf renders the NodeSet with an empty
# podSpec.nodeSelector, so nothing constrains that placement: the StatefulSet
# gives the pods stable NAMES and not stable NODES, and a rerun can legitimately
# swap which ordinals land in which clique.
#
# That is observed, not argued. Three runs of the same recipe against the same
# chart (topograph 1.0.0) and the same two-clique layout produced three
# different files:
#
#   cq0 = slinky-[2-3]   cq1 = slinky-[0-1]
#   cq0 = slinky-[0-1]   cq1 = slinky-[2-3]     (the inverse)
#   cq0 = slinky-[0,2]   cq1 = slinky-[1,3]     (and a different SYNTAX: two
#                                                non-adjacent ordinals pack as a
#                                                comma list, not a range)
#
# A committed byte-literal captured from any one of those fails on the other
# two, and the usual answer to a flaky assertion is to loosen it until it
# passes, which is how a gate turns into theater.
#
# So the expectation is recomputed on every run from two facts the cluster
# already holds:
#
#   1. each slurmd pod's spec.nodeName          (who landed where)
#   2. each node's nvidia.com/gpu.clique label  (which accelerator domain)
#
# Composing them gives clique -> {slurm nodes}, which is what a correct
# topology.conf must say. The ground truth is the LABELS the bootstrap applied
# (tests/uat/kind/setup-gpu-sim.sh), never Topograph's own output: an
# expectation captured from Topograph passes whatever Topograph emits, which is
# how the leaf came to assert the shape of an embedded fixture whose node names
# (I21, I22, I25, I34-I36) exist on no cluster at all.
#
# THE COMPARISON IS KEYED BY CLIQUE, not by block membership alone. Two blocks
# of two partition the four Slurm nodes the same way whichever clique each
# block belongs to, so comparing the partition as an unordered set of sets
# cannot see Topograph attributing the right sizes to the wrong domain. The
# clique for each block comes from the `# <block>=<clique>` comment Topograph
# writes above each BlockName line; a block with no such comment is a failure,
# not a pass, because without it the mapping cannot be checked at all.
#
# Usage:
#   tests/uat/kind/verify-topology.sh [cluster-name]
#
# Exits 0 when the live topology.conf matches the derived expectation, 1
# otherwise (with the two mappings printed side by side). Sourcing this file
# defines constants and functions only and touches no cluster, so
# topology-golden_test.sh can exercise the parsing and comparison hermetically.

# The cluster name default, the clique label and KUBECTL_TIMEOUT come from the
# GPU-simulation script rather than being restated here: the clique layout has
# exactly one definition, and the bootstrap sources the same file.
# shellcheck source=./setup-gpu-sim.sh
source "$(dirname "${BASH_SOURCE[0]}")/setup-gpu-sim.sh"

# Where Topograph publishes, and what it publishes into. Both are set by AICR
# (recipes/components/slinky-topograph/values.yaml engine params
# topologyConfigmapName / topologyConfigPath); the ConfigMap itself is rendered
# and owned by the slurm chart and mounted into slurmctld via configFileRefs.
TOPOLOGY_NAMESPACE="slurm"
TOPOLOGY_CONFIGMAP="slinky-slurm-config-extra"
TOPOLOGY_KEY="topology.conf"

# The slurmd pod selector, matching the engine's own podSelector. Topograph
# resolves Slurm node names through these pods, so the derivation has to read
# exactly the same set.
SLURMD_SELECTOR="app.kubernetes.io/name=slurmd"

# The marker the leaf's configFiles seed carries. Topograph overwrites the key
# on every successful sync, so the marker surviving means no sync has landed
# and the file describes nothing about this cluster.
TOPOLOGY_PRESEED_MARKER="aicr-preseed"

# Convergence budget for the comparison. Topograph publishes on a trigger after
# `config.requestAggregationDelay` (15s in this leaf's values), so the file
# trails the cluster by at least that much after the last slurmd goes Running,
# and by more when the operator is still rolling pods. 300s is generous against
# that and still fails closed. Overridable so a developer can shorten a local
# run without editing the script.
VERIFY_TOPOLOGY_TIMEOUT="${VERIFY_TOPOLOGY_TIMEOUT:-300}"
VERIFY_TOPOLOGY_INTERVAL="${VERIFY_TOPOLOGY_INTERVAL:-15}"

# --- pure helpers -----------------------------------------------------------

# expand_hostlist <spec>
#
# Expands one Slurm hostlist into one node name per line, in the order written.
#
# Slurm packs a set of names into a prefix plus a bracketed range list:
# `slinky-[0-1]`, `slinky-[0,2]`, `slinky-[0-1,3]`, and the degenerate
# `slinky-0`. A comma OUTSIDE the brackets separates independent specs, so
# splitting on every comma would cut `slinky-[0,2]` into `slinky-[0` and `2]`
# and silently report two nodes named nothing; the split below tracks bracket
# depth for that reason.
#
# Which form Topograph emits for a given pair is not fixed -- two adjacent
# ordinals collapse to a range and two non-adjacent ones do not -- so the
# comparison downstream works on expanded names and never on the packed text.
#
# Zero padding is preserved (`node[001-003]` yields node001..node003): dropping
# it would rename the node rather than fail, and a rename compares equal to
# nothing.
#
# Returns 1 on a spec it cannot parse (unbalanced brackets, reversed range)
# rather than emitting a partial expansion, so a caller cannot mistake a
# truncated list for a short block.
expand_hostlist() {
    local spec="${1:-}"
    if [[ -z "${spec}" ]]; then
        echo "expand_hostlist: empty hostlist" >&2
        return 1
    fi
    printf '%s\n' "${spec}" | awk '
    function emit(tok,   k, e, pfx, sfx, body, nr, ranges, r, dash, lo, hi, width, j) {
        k = index(tok, "[")
        if (k == 0) {
            if (index(tok, "]") != 0) { bad = "unbalanced brackets in " tok; return }
            if (tok == "") { bad = "empty element"; return }
            print tok
            return
        }
        e = index(tok, "]")
        if (e < k) { bad = "unbalanced brackets in " tok; return }
        pfx = substr(tok, 1, k - 1)
        body = substr(tok, k + 1, e - k - 1)
        sfx = substr(tok, e + 1)
        if (body == "") { bad = "empty range in " tok; return }
        nr = split(body, ranges, ",")
        for (r = 1; r <= nr; r++) {
            dash = index(ranges[r], "-")
            if (dash == 0) {
                if (ranges[r] !~ /^[0-9]+$/) { bad = "non-numeric index in " tok; return }
                print pfx ranges[r] sfx
                continue
            }
            lo = substr(ranges[r], 1, dash - 1)
            hi = substr(ranges[r], dash + 1)
            if (lo !~ /^[0-9]+$/ || hi !~ /^[0-9]+$/) { bad = "non-numeric range in " tok; return }
            width = length(lo)
            if (hi + 0 < lo + 0) { bad = "reversed range in " tok; return }
            for (j = lo + 0; j <= hi + 0; j++) {
                printf "%s%0*d%s\n", pfx, width, j, sfx
            }
        }
    }
    {
        depth = 0; part = ""; np = 0
        for (i = 1; i <= length($0); i++) {
            c = substr($0, i, 1)
            if (c == "[") depth++
            else if (c == "]") depth--
            if (c == "," && depth == 0) { parts[++np] = part; part = "" }
            else part = part c
        }
        parts[++np] = part
        for (p = 1; p <= np; p++) { emit(parts[p]); if (bad != "") break }
    }
    END {
        if (bad != "") { print "expand_hostlist: " bad > "/dev/stderr"; exit 1 }
    }'
}

# group_by_clique
#
# Reads `<clique> <slurm-node>` pairs on stdin and prints one
# `<clique> <space-separated sorted slurm nodes>` row per CLIQUE, sorted.
#
# Shared by both sides of the comparison so they cannot disagree on shape.
# `sort` is lexical on both, so slinky-10 orders before slinky-2 consistently.
group_by_clique() {
    sort | awk '
        { if ($1 != cur) { if (cur != "") print row; cur = $1; row = $1 } ; row = row " " $2 }
        END { if (cur != "") print row }'
}

# topology_blocks <topology.conf text>
#
# Prints one `<clique> <space-separated sorted slurm nodes>` row per CLIQUE,
# which is the shape expected_blocks emits, so the two compare as plain strings.
#
# AGGREGATED BY CLIQUE, NOT ONE ROW PER BLOCK. A clique larger than the engine's
# blockSizes is written as SEVERAL blocks: with a three-node cq0 and
# blockSizes: [2], Topograph emits `# block001=cq0` with two nodes and
# `# block002=cq0` with the third. Observed on a live cluster. One row per block
# would then have three rows facing two and report a mismatch on a file that
# agrees with the labels perfectly.
#
# Reads the clique from the `# <blockname>=<clique>` comment Topograph writes
# immediately above each BlockName line. A BlockName with no such comment
# returns 1: membership would be all that is left to compare, and a set-of-sets
# comparison is exactly the check that cannot see two blocks swapped between
# domains.
topology_blocks() {
    local conf="${1:-}"
    if [[ -z "${conf}" ]]; then
        echo "topology_blocks: empty topology.conf" >&2
        return 1
    fi

    local line block clique nodes expanded node rc=0
    local pending_block="" pending_clique=""
    local rows=""
    while IFS= read -r line; do
        # `# block001=cq0`: remember the mapping for the BlockName below it.
        if [[ "${line}" =~ ^[[:space:]]*#[[:space:]]*([A-Za-z0-9_.-]+)=([A-Za-z0-9_.-]+)[[:space:]]*$ ]]; then
            pending_block="${BASH_REMATCH[1]}"
            pending_clique="${BASH_REMATCH[2]}"
            continue
        fi
        [[ "${line}" == BlockName=* ]] || continue

        block="${line#BlockName=}"
        block="${block%% *}"
        nodes="${line#*Nodes=}"
        nodes="${nodes%% *}"
        if [[ "${nodes}" == "${line}" || -z "${nodes}" ]]; then
            echo "topology_blocks: BlockName=${block} has no Nodes= value" >&2
            rc=1
            continue
        fi
        if [[ "${pending_block}" != "${block}" ]]; then
            echo "topology_blocks: BlockName=${block} carries no '# ${block}=<clique>' comment;" \
                "without it the block cannot be attributed to an accelerator domain" >&2
            rc=1
            continue
        fi
        clique="${pending_clique}"

        expanded="$(expand_hostlist "${nodes}")" || {
            echo "topology_blocks: BlockName=${block}: cannot expand '${nodes}'" >&2
            rc=1
            continue
        }
        while IFS= read -r node; do
            [[ -n "${node}" ]] || continue
            rows="${rows}${clique} ${node}
"
        done <<<"${expanded}"
    done <<<"${conf}"

    if [[ -z "${rows}" ]]; then
        echo "topology_blocks: no BlockName lines found" >&2
        return 1
    fi
    printf '%s' "${rows}" | group_by_clique
    return "${rc}"
}

# expected_blocks <pod-table> <node-table>
#
# Derives the same `<clique> <sorted slurm nodes>` shape from the cluster.
#
#   pod-table   one `<slurm-node> <kubernetes-node>` row per slurmd pod
#   node-table  one `<kubernetes-node> <clique>` row per node
#
# Both are read straight from the API by main; passing them in keeps the
# derivation itself pure and testable.
#
# A pod on a node with no clique label returns 1 rather than being dropped:
# a slurmd with no accelerator domain is precisely the failure the control
# plane staying GPU-free exists to prevent, and skipping it would turn a
# four-node topology into a silently smaller one that still compares equal.
expected_blocks() {
    local pods="${1:-}" nodes="${2:-}"
    if [[ -z "${pods}" ]]; then
        echo "expected_blocks: no slurmd pods" >&2
        return 1
    fi

    local slurm_node kube_node clique rc=0
    local pairs=""
    while read -r slurm_node kube_node; do
        [[ -n "${slurm_node}" ]] || continue
        if [[ -z "${kube_node}" ]]; then
            echo "expected_blocks: slurmd pod ${slurm_node} is not assigned to a node" >&2
            rc=1
            continue
        fi
        clique="$(printf '%s\n' "${nodes}" | awk -v n="${kube_node}" '$1 == n {print $2; exit}')"
        if [[ -z "${clique}" ]]; then
            echo "expected_blocks: node ${kube_node} (hosting ${slurm_node}) carries no ${GPU_CLIQUE_LABEL} label" >&2
            rc=1
            continue
        fi
        pairs="${pairs}${clique} ${slurm_node}
"
    done <<<"${pods}"

    if [[ -z "${pairs}" ]]; then
        echo "expected_blocks: derived no clique membership" >&2
        return 1
    fi
    printf '%s' "${pairs}" | group_by_clique
    return "${rc}"
}

# --- live-cluster entry point ----------------------------------------------

# slurmd_pod_table <context>
#
# Prints `<slurm-node> <kubernetes-node>` per slurmd pod.
#
# THE SLURM NODE NAME IS THE POD'S spec.hostname, NOT ITS metadata.name. The
# Slinky operator gives each NodeSet pod a hostname of `<nodeset>-<ordinal>`
# and slurmd registers with slurmctld under that, while the pod itself is named
# `<release>-<nodeset>-<ordinal>`. Read from a cluster where Topograph had
# actually run:
#
#   metadata.name                            spec.hostname
#   slurm-worker-slinky-0                    slinky-0
#
# and the topology.conf it wrote says `Nodes=slinky-[0-1]`. Deriving the
# expectation from metadata.name would therefore compare `slurm-worker-slinky-0`
# against `slinky-0` and report every block wrong -- a check that fails on a
# healthy cluster, which gets loosened until it passes. The same value is
# carried on the `nodeset.slinky.slurm.net/pod-hostname` label.
#
# Only Running pods are listed. A Pending pod has no nodeName, so including it
# would fail the derivation with a confusing "not assigned to a node" instead
# of the readiness message main prints first.
slurmd_pod_table() {
    local context="$1"
    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        get pods -n "${TOPOLOGY_NAMESPACE}" -l "${SLURMD_SELECTOR}" \
        --field-selector=status.phase=Running \
        -o 'jsonpath={range .items[*]}{.spec.hostname} {.spec.nodeName}{"\n"}{end}'
}

# node_clique_table <context>
node_clique_table() {
    local context="$1"
    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" get nodes \
        -o "jsonpath={range .items[*]}{.metadata.name} {.metadata.labels.nvidia\.com/gpu\.clique}{\"\n\"}{end}"
}

# check_declared_layout <cluster> <node-table>
#
# Re-asserts that each worker still carries the clique WORKER_CLIQUE_MAP
# declares for it, before any expectation is derived from the labels.
#
# DO NOT REMOVE THIS AS REDUNDANT WITH THE COMPARISON BELOW. Without it the
# check is satisfied by any SELF-CONSISTENT cluster, and a relabelled cluster
# BECOMES self-consistent on its own: relabel a worker from cq1 to cq0 and
# Topograph resyncs, rewriting topology.conf as a clean 3/1 split that agrees
# with the new labels perfectly. Observed on a live cluster, which is the only
# way this surfaces:
#
#   # block001=cq0        <- two nodes
#   # block002=cq0        <- the third, because the clique now exceeds blockSizes
#   # block003=cq1        <- the one left
#
# The comparison below then goes GREEN on a cluster whose clique layout is
# wrong. Which of the two answers you get depends on whether Topograph has
# resynced yet, so the topology comparison alone makes the node-relabel
# mutation a RACE rather than a gate.
#
# The lane's premise is not "some clique layout" but the two-cliques-of-two
# layout the bootstrap applied, so WORKER_CLIQUE_MAP is the ground truth and
# the labels are what is checked against it.
#
# bootstrap-cluster.sh asserts the same thing at bootstrap. It is re-asserted
# here because this check runs a whole install later, and a premise that was
# true half an hour ago is not evidence about now.
check_declared_layout() {
    local cluster="$1" nodes="$2" index node want got rc=0
    for index in $(worker_indices); do
        node="$(kind_worker_node "${cluster}" "${index}")" || return 1
        want="$(worker_clique "${index}")" || return 1
        got="$(printf '%s\n' "${nodes}" | awk -v n="${node}" '$1 == n {print $2; exit}')"
        if [[ "${got}" != "${want}" ]]; then
            echo "FAIL: ${node} carries clique '${got:-<none>}', but the lane declares ${want}." \
                "Topograph resyncs to whatever the labels say, so a relabelled cluster is" \
                "self-consistent and the topology comparison alone cannot see this." >&2
            rc=1
        fi
    done
    return "${rc}"
}

# compare_once <context> <nodes>
#
# One attempt: read the slurmd placement, derive the expectation, parse the
# live topology.conf, compare. Returns 1 and prints the reason to stderr when
# any of that is not yet true, 0 and a line beginning "ok:" when it holds.
#
# THE EXIT CODE IS THE VERDICT, and every path here sets it deliberately. main
# captures both streams together so a retry loop can hold the last real reason
# and print it only if the deadline expires, but it decides on the code: the
# captured text also carries whatever kubectl chose to write to stderr, which
# is not evidence about the topology either way.
compare_once() {
    local context="$1" nodes="$2"

    local pods running want
    pods="$(slurmd_pod_table "${context}")" || return 1
    running="$(printf '%s\n' "${pods}" | grep -c '[^[:space:]]')"
    want="$(worker_indices | wc -l | tr -d ' ')"
    if [[ "${running}" -ne "${want}" ]]; then
        echo "${running} slurmd pods Running, want ${want}." \
            "The Slinky operator injects a hard per-hostname podAntiAffinity when" \
            "oversubscribeNode is false, so a short count that PERSISTS means a pod is" \
            "Pending for want of a schedulable node -- a readiness failure, not a" \
            "topology one." >&2
        return 1
    fi

    local conf
    conf="$(kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        get configmap "${TOPOLOGY_CONFIGMAP}" -n "${TOPOLOGY_NAMESPACE}" \
        -o "jsonpath={.data.${TOPOLOGY_KEY//./\\.}}")" || {
        echo "could not read ${TOPOLOGY_CONFIGMAP}/${TOPOLOGY_KEY} in ${TOPOLOGY_NAMESPACE}" >&2
        return 1
    }
    if [[ -z "${conf}" ]]; then
        echo "${TOPOLOGY_CONFIGMAP}/${TOPOLOGY_KEY} is empty" >&2
        return 1
    fi
    if [[ "${conf}" == *"${TOPOLOGY_PRESEED_MARKER}"* ]]; then
        echo "${TOPOLOGY_KEY} still carries the '${TOPOLOGY_PRESEED_MARKER}' seed;" \
            "Topograph has not completed a sync" >&2
        return 1
    fi

    local actual expected
    actual="$(topology_blocks "${conf}")" || {
        echo "could not read the block topology out of ${TOPOLOGY_KEY}:" >&2
        printf '%s\n' "${conf}" | sed 's/^/    /' >&2
        return 1
    }
    expected="$(expected_blocks "${pods}" "${nodes}")" || return 1

    # printf '%s\n' on a multi-line variable indents only the FIRST line, which
    # in a two-block failure reads as one block being mis-indented rather than
    # as two rows to compare. sed indents every row.
    if [[ "${actual}" != "${expected}" ]]; then
        echo "topology.conf does not match the clique layout the nodes carry." >&2
        echo "  expected (derived from ${GPU_CLIQUE_LABEL} labels and slurmd placement):" >&2
        printf '%s\n' "${expected}" | sed 's/^/    /' >&2
        echo "  actual (parsed from ${TOPOLOGY_CONFIGMAP}/${TOPOLOGY_KEY}):" >&2
        printf '%s\n' "${actual}" | sed 's/^/    /' >&2
        return 1
    fi

    echo "ok: topology.conf matches the derived clique layout"
    printf '%s\n' "${expected}" | sed 's/^/  /'
}

main() {
    local cluster context
    cluster="${1:-${DEFAULT_CLUSTER_NAME}}"
    context="kind-${cluster}"

    # THE DECLARED LAYOUT IS CHECKED ONCE, AND FAILS IMMEDIATELY. A worker in
    # the wrong clique is a configuration fault; waiting cannot fix it, and
    # retrying would only delay the message by the whole budget.
    local nodes
    nodes="$(node_clique_table "${context}")" || {
        echo "FAIL: could not read nodes from ${context}" >&2
        return 1
    }
    check_declared_layout "${cluster}" "${nodes}" || return 1

    # THE COMPARISON RETRIES, because Topograph's sync is asynchronous and
    # trails the cluster. It publishes on a trigger (the slurmd pods) after
    # `config.requestAggregationDelay`, so between the last slurmd going Running
    # and the next sync landing, topology.conf legitimately describes FEWER
    # nodes than the cluster has. Observed on a live run: four pods Running
    # against a topology.conf that still read
    #
    #   # block001=cq0
    #   BlockName=block001 Nodes=slinky-0
    #
    # A single-shot check fails there, and it fails in the direction that looks
    # like a topology defect. Retrying to a deadline distinguishes "has not
    # converged yet" from "converged wrong": the message printed on timeout is
    # the last real comparison, not a transient.
    #
    # THE VERDICT IS compare_once's EXIT CODE. Both streams are still merged
    # into `last`, because the reason a slow cluster has not converged is
    # written to stderr and is what has to be printed on timeout -- but the
    # merged TEXT decides nothing. kubectl writes to stderr on a healthy
    # cluster too (deprecation notices, client-side throttling, an API-group
    # discovery retry), so matching the text for an "ok:" prefix made any of
    # that chatter spend the whole convergence budget and then fail a cluster
    # whose topology is correct, on the step every later step is gated on.
    local deadline=$(( SECONDS + VERIFY_TOPOLOGY_TIMEOUT ))
    local attempt=0 last rc
    while :; do
        attempt=$(( attempt + 1 ))
        rc=0
        last="$(compare_once "${context}" "${nodes}" 2>&1)" || rc=$?
        if (( rc == 0 )); then
            printf '%s\n' "${last}"
            return 0
        fi
        if (( SECONDS >= deadline )); then
            echo "FAIL after ${VERIFY_TOPOLOGY_TIMEOUT}s and ${attempt} attempt(s):" >&2
            printf '%s\n' "${last}" >&2
            kubectl --context "${context}" get pods -n "${TOPOLOGY_NAMESPACE}" \
                -l "${SLURMD_SELECTOR}" -o wide >&2
            return 1
        fi
        sleep "${VERIFY_TOPOLOGY_INTERVAL}"
    done
}

# Source guard: sourcing defines constants and functions only.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    set -uo pipefail
    main "$@"
fi
