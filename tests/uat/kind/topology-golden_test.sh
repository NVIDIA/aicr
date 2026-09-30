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

# Guards verify-topology.sh, the runtime-derived topology check.
#
# THIS FILE EXISTS SO THE DISCRIMINATION IS RE-VERIFIABLE. Moving a node
# between cliques on a live cluster and watching the check go red proves it
# once, on one afternoon. The cases below replay that mutation -- and the
# three others that matter -- against the same functions, on every `make test`,
# with no cluster.
#
# Each negative case names the specific defect it stands for, and every one of
# them produces a topology.conf that is internally consistent and that
# slurmctld would start on. That is the point: the failures this check exists
# for do not announce themselves.
#
# Hermetic: sources the script (which touches no cluster when sourced) and
# calls only its pure helpers.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Resolve the subject SCRIPT_DIR-relative so this exercises the file in THIS
# worktree, never a deployed copy.
# shellcheck source=./verify-topology.sh
source "${SCRIPT_DIR}/verify-topology.sh"

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

# --- the cluster this suite models -----------------------------------------
#
# Four slurmd pods on the four workers the bootstrap labels cq0/cq0/cq1/cq1.
# The pod-to-node mapping is deliberately INVERTED relative to node ordering
# (slinky-2 and slinky-3 on the cq0 pair), reproducing what the reference
# cluster actually scheduled: the derivation must follow placement, not
# ordinal order, or it would agree with the file for the wrong reason.
PODS="slinky-0 aicr-uat-slurm-worker3
slinky-1 aicr-uat-slurm-worker4
slinky-2 aicr-uat-slurm-worker
slinky-3 aicr-uat-slurm-worker2"

NODES="aicr-uat-slurm-control-plane
aicr-uat-slurm-worker cq0
aicr-uat-slurm-worker2 cq0
aicr-uat-slurm-worker3 cq1
aicr-uat-slurm-worker4 cq1"

EXPECTED="cq0 slinky-2 slinky-3
cq1 slinky-0 slinky-1"

# The cluster name the node names above are kind-derived from, and the same
# table with worker3 moved from cq1 to cq0 -- the live mutation this task ran,
# and the layout WORKER_CLIQUE_MAP does not declare.
CLUSTER="aicr-uat-slurm"
NODES_RELABELLED="aicr-uat-slurm-control-plane
aicr-uat-slurm-worker cq0
aicr-uat-slurm-worker2 cq0
aicr-uat-slurm-worker3 cq0
aicr-uat-slurm-worker4 cq1"

# What Topograph writes for that cluster when everything is right.
CONF_GOOD="# block001=cq0
BlockName=block001 Nodes=slinky-[2-3]
# block002=cq1
BlockName=block002 Nodes=slinky-[0-1]
BlockSizes=2"

# --- hostlist expansion ----------------------------------------------------
#
# The comparison downstream works on expanded names, so every packing Slurm
# may emit for the same pair has to expand to the same two nodes. Which
# packing appears is a property of which ordinals the scheduler happened to
# co-locate -- adjacent ones collapse to a range, non-adjacent ones do not --
# so a check that understood only `[a-b]` would fail on a legal placement.
check "a contiguous range expands" "slinky-2 slinky-3" \
    "$(expand_hostlist 'slinky-[2-3]' | tr '\n' ' ' | sed 's/ *$//')"
check "a comma list expands" "slinky-0 slinky-2" \
    "$(expand_hostlist 'slinky-[0,2]' | tr '\n' ' ' | sed 's/ *$//')"
check "a mixed range and list expands" "slinky-0 slinky-1 slinky-3" \
    "$(expand_hostlist 'slinky-[0-1,3]' | tr '\n' ' ' | sed 's/ *$//')"
check "a top-level comma does not split inside brackets" "slinky-0 slinky-2 slinky-5" \
    "$(expand_hostlist 'slinky-[0,2],slinky-5' | tr '\n' ' ' | sed 's/ *$//')"
check "a bare name expands to itself" "slinky-0" "$(expand_hostlist 'slinky-0')"
# Zero padding is part of the NAME. Dropping it renames the node, and a
# renamed node compares equal to nothing rather than failing loudly here.
check "zero padding is preserved" "node001 node002 node003" \
    "$(expand_hostlist 'node[001-003]' | tr '\n' ' ' | sed 's/ *$//')"

# A partial expansion is worse than none: a truncated list is a short block,
# which reads as a topology defect rather than as a parse failure.
expand_hostlist 'slinky-[3-2]' >/dev/null 2>&1
check "a reversed range is rejected" "1" "$?"
expand_hostlist 'slinky-[0-1' >/dev/null 2>&1
check "an unbalanced bracket is rejected" "1" "$?"
expand_hostlist 'slinky-[a-b]' >/dev/null 2>&1
check "a non-numeric range is rejected" "1" "$?"

# --- parsing what Topograph wrote ------------------------------------------

check "a good topology.conf parses to clique -> nodes" "${EXPECTED}" \
    "$(topology_blocks "${CONF_GOOD}")"

# Topograph packs a non-adjacent pair as a comma list. Same cluster, same
# answer, different bytes -- which is exactly why the committed expectation is
# derived rather than a byte-literal golden.
check "a non-adjacent pair parses to the same membership" "cq0 slinky-0 slinky-2
cq1 slinky-1 slinky-3" \
    "$(topology_blocks "# block001=cq0
BlockName=block001 Nodes=slinky-[0,2]
# block002=cq1
BlockName=block002 Nodes=slinky-[1,3]
BlockSizes=2")"

# THE PLUGIN COUPLING. `plugin` (topograph engine) and `TopologyPlugin`
# (slurmctld) must both say block. Flip ONE and slurmctld fatals on an
# unrecognized key, so that combination cannot ship silently. Flip BOTH back
# to tree and the file is internally consistent, slurmctld starts, and the
# lane is no longer testing the block topology it exists to test -- with
# nothing anywhere reporting an error. A check that merely looked for node
# names would pass on this.
topology_blocks "SwitchName=S1 Switches=S[2-3]
SwitchName=S2 Nodes=slinky-[0-1]
SwitchName=S3 Nodes=slinky-[2-3]" >/dev/null 2>&1
check "topology/tree output is rejected (no BlockName lines)" "1" "$?"

# The seed the leaf ships in configFiles. Topograph overwrites the key on a
# successful sync, so the marker surviving means no sync landed: the file
# describes nothing about this cluster and must not pass.
topology_blocks "# Managed by NVIDIA Topograph (engine: slinky). Pre-sync placeholder.
BlockName=aicr-preseed Nodes=aicr-preseed-node
BlockSizes=1" >/dev/null 2>&1
check "the pre-sync seed has no clique comment and is rejected" "1" "$?"

# Without the `# <block>=<clique>` comment the block cannot be attributed to
# an accelerator domain, and the only comparison left is set-of-sets -- the
# one that cannot see a swap. Fail rather than silently weaken.
topology_blocks "BlockName=block001 Nodes=slinky-[2-3]
BlockName=block002 Nodes=slinky-[0-1]
BlockSizes=2" >/dev/null 2>&1
check "a block with no clique comment is rejected" "1" "$?"

# --- deriving the expectation from the cluster -----------------------------

check "the expectation follows pod placement, not ordinal order" "${EXPECTED}" \
    "$(expected_blocks "${PODS}" "${NODES}")"

# A slurmd with no accelerator domain is the failure the GPU-free control
# plane exists to prevent. Dropping it would shrink the topology to something
# that still compares equal.
expected_blocks "slinky-0 aicr-uat-slurm-control-plane" "${NODES}" >/dev/null 2>&1
check "a pod on an unlabelled node is rejected" "1" "$?"
expected_blocks "slinky-0 " "${NODES}" >/dev/null 2>&1
check "a pod with no node assignment is rejected" "1" "$?"

# --- THE MUTATIONS ---------------------------------------------------------
#
# Each replays a defect that leaves a file slurmctld starts on, and asserts
# the comparison the live check makes goes red.

# 1. THE SWAP. Topograph attributes the right block SIZES to the wrong
#    domains. Every structural property is preserved -- two blocks, two nodes
#    each, the four nodes partitioned, BlockSizes=2 -- so a shape-only
#    assertion passes. Only keying the comparison by clique catches it.
check "a swapped clique attribution is caught" "MISMATCH" \
    "$([[ "$(topology_blocks "# block001=cq1
BlockName=block001 Nodes=slinky-[2-3]
# block002=cq0
BlockName=block002 Nodes=slinky-[0-1]
BlockSizes=2")" == "$(expected_blocks "${PODS}" "${NODES}")" ]] && echo MATCH || echo MISMATCH)"

# 2. THE RELABEL, which is the live mutation this task ran: move worker3 from
#    cq1 to cq0 and the cluster's true layout becomes three nodes in one
#    domain and one in the other. The file Topograph wrote for the OLD layout
#    is unchanged, so the comparison must reject it. (On a live cluster
#    Topograph re-syncs and writes the 3/1 split; either way the expectation
#    and the file disagree until both describe the same cluster.)
check "a node moved between cliques is caught" "MISMATCH" \
    "$([[ "$(topology_blocks "${CONF_GOOD}")" == "$(expected_blocks "${PODS}" "aicr-uat-slurm-worker cq0
aicr-uat-slurm-worker2 cq0
aicr-uat-slurm-worker3 cq0
aicr-uat-slurm-worker4 cq1")" ]] && echo MATCH || echo MISMATCH)"

# 3. THE 3/1 SPLIT a re-synced Topograph writes for that relabelled cluster,
#    in the form it actually writes: a clique larger than blockSizes becomes
#    SEVERAL blocks, so the three-node cq0 is `# block001=cq0` with two nodes
#    plus `# block002=cq0` with the third. Read off a live cluster after the
#    relabel. It describes the relabelled cluster correctly and must therefore
#    MATCH: the check follows the labels rather than insisting on two blocks of
#    two. One row per BLOCK would compare three rows against two and call this
#    a mismatch.
check "the multi-block 3/1 split matching a relabelled cluster is accepted" "MATCH" \
    "$([[ "$(topology_blocks "# block001=cq0
BlockName=block001 Nodes=slinky-[2-3]
# block002=cq0
BlockName=block002 Nodes=slinky-0
# block003=cq1
BlockName=block003 Nodes=slinky-1
BlockSizes=2")" == "$(expected_blocks "${PODS}" "aicr-uat-slurm-worker cq0
aicr-uat-slurm-worker2 cq0
aicr-uat-slurm-worker3 cq0
aicr-uat-slurm-worker4 cq1")" ]] && echo MATCH || echo MISMATCH)"

# 3b. And the aggregation is not a free pass: the same three-block shape with
#     one node attributed to the wrong clique must still fail.
check "a multi-block split with one node in the wrong clique is caught" "MISMATCH" \
    "$([[ "$(topology_blocks "# block001=cq0
BlockName=block001 Nodes=slinky-[2-3]
# block002=cq1
BlockName=block002 Nodes=slinky-0
# block003=cq0
BlockName=block003 Nodes=slinky-1
BlockSizes=2")" == "$(expected_blocks "${PODS}" "aicr-uat-slurm-worker cq0
aicr-uat-slurm-worker2 cq0
aicr-uat-slurm-worker3 cq0
aicr-uat-slurm-worker4 cq1")" ]] && echo MATCH || echo MISMATCH)"

# 4. A SINGLE NODE IN THE WRONG BLOCK. Sizes stay 2/2 and the partition still
#    covers all four, so this too survives a shape-only check.
check "one node in the wrong block is caught" "MISMATCH" \
    "$([[ "$(topology_blocks "# block001=cq0
BlockName=block001 Nodes=slinky-[1-2]
# block002=cq1
BlockName=block002 Nodes=slinky-[0,3]
BlockSizes=2")" == "$(expected_blocks "${PODS}" "${NODES}")" ]] && echo MATCH || echo MISMATCH)"

# 5. THE BASELINE. Without this the four cases above would all pass on a
#    comparison that never matches anything.
check "the correct topology.conf is accepted" "MATCH" \
    "$([[ "$(topology_blocks "${CONF_GOOD}")" == "$(expected_blocks "${PODS}" "${NODES}")" ]] && echo MATCH || echo MISMATCH)"

# --- the declared-layout guard, and that main still runs it -----------------
#
# check_declared_layout re-asserts each worker against WORKER_CLIQUE_MAP before
# any expectation is derived from the labels. It is NOT redundant with the
# comparison, and the cases below are what stop it being deleted as such.
#
# Its own contract first: the declared layout passes, a relabelled worker
# fails, an unlabelled worker fails. Pure -- it takes the node table as a
# parameter and its helpers (worker_indices, kind_worker_node, worker_clique)
# read only WORKER_CLIQUE_MAP.
check_declared_layout "${CLUSTER}" "${NODES}" >/dev/null 2>&1
check "the declared clique layout is accepted" "0" "$?"

check_declared_layout "${CLUSTER}" "${NODES_RELABELLED}" >/dev/null 2>&1
check "a worker relabelled into the wrong clique is rejected" "1" "$?"

check_declared_layout "${CLUSTER}" "aicr-uat-slurm-worker cq0
aicr-uat-slurm-worker2 cq0
aicr-uat-slurm-worker3
aicr-uat-slurm-worker4 cq1" >/dev/null 2>&1
check "a worker carrying no clique label at all is rejected" "1" "$?"

# --- THE VERDICT ITSELF: driving compare_once -------------------------------
#
# Everything above compares topology_blocks against expected_blocks IN THIS
# HARNESS. That enumerates the defect classes cheaply and keeps its value, but
# the string comparison those cases run is the harness's own, not the script's.
# compare_once is the function whose exit code the lane gates on, and until
# these cases it was never called: replacing its decision with `if false; then`
# left every case above green while the live check accepted any topology.conf
# at all. Run, observed, and the reason this section exists.
#
# So these cases drive compare_once ITSELF with only its two cluster reads
# replaced -- slurmd_pod_table and the ConfigMap kubectl, ONE LAYER DEEP at the
# API boundary. The parsing, the derivation, the comparison and the exit code
# that run here are the ones the lane runs.
#
# THE STUB STATE IS NAMED stub_* ON PURPOSE. bash scopes dynamically and
# compare_once declares locals named `pods`, `nodes` and `conf`, so a stub
# closing over a variable of one of those names reads compare_once's own EMPTY
# local instead of the fixture. Under `set -u` that aborts the subshell, which
# still exits non-zero and would have every negative case here passing without
# the function ever seeing a table.
# The optional 4th argument is the ConfigMap read's exit code, defaulting to
# the success every case above wants. Without it the `kubectl ... || {` clause
# is unreachable from here: a stub that always succeeds can only ever exercise
# what compare_once does with the text it got back.
compare_once_with() {
    local stub_pods="$1" stub_nodes="$2" stub_conf="$3" stub_read_rc="${4:-0}"
    (
        slurmd_pod_table() { printf '%s\n' "${stub_pods}"; }
        kubectl() { printf '%s' "${stub_conf}"; return "${stub_read_rc}"; }
        compare_once "kind-${CLUSTER}" "${stub_nodes}"
    )
}

# first_sentence <text>
#
# The first line of <text> up to its first ". ". compare_once's messages carry
# the discrimination in their opening sentence (which clause fired, and with
# what numbers) and continue into paragraphs of rationale; asserting the whole
# paragraph would make every reworded comment a test failure, and asserting
# only the exit code cannot tell one rejection from another.
first_sentence() {
    printf '%s\n' "${1}" | awk 'NR == 1 { k = index($0, ". "); print (k ? substr($0, 1, k) : $0); exit }'
}

# layout_section <text> <label>
#
# Prints the indented rows the mismatch message writes under `  <label> (`,
# joined with " | ". Read separately, the two sections prove the message names
# BOTH layouts and names them the right way round: printing the file's layout
# under "expected" sends an operator to relabel a cluster that is correct.
layout_section() {
    printf '%s\n' "${1}" | awk -v head="  ${2} (" '
        index($0, head) == 1 { grab = 1; next }
        grab && index($0, "    ") == 1 { rows = rows (rows == "" ? "" : " | ") substr($0, 5); next }
        { grab = 0 }
        END { print rows }'
}

# THE BASELINE, and it is load-bearing here for the same reason case 5 is
# above: a compare_once that accepted nothing would satisfy every rejection
# case below. Its stdout is asserted whole, because the layout it echoes back
# is what a passing run leaves in the CI log as evidence.
check "compare_once accepts a topology.conf matching the labels" \
    "ok: topology.conf matches the derived clique layout
  cq0 slinky-2 slinky-3
  cq1 slinky-0 slinky-1" \
    "$(compare_once_with "${PODS}" "${NODES}" "${CONF_GOOD}" 2>/dev/null)"
compare_once_with "${PODS}" "${NODES}" "${CONF_GOOD}" >/dev/null 2>&1
check "compare_once returns 0 on a matching topology.conf" "0" "$?"

# THE SWAP, now through the function rather than around it. Same fixture as
# case 1 above: right block sizes, wrong domains.
CONF_SWAPPED="# block001=cq1
BlockName=block001 Nodes=slinky-[2-3]
# block002=cq0
BlockName=block002 Nodes=slinky-[0-1]
BlockSizes=2"
compare_once_with "${PODS}" "${NODES}" "${CONF_SWAPPED}" >/dev/null 2>&1
check "compare_once returns 1 on a swapped clique attribution" "1" "$?"

SWAP_ERR="$(compare_once_with "${PODS}" "${NODES}" "${CONF_SWAPPED}" 2>&1 >/dev/null)"
check "the swap is reported as a topology mismatch" \
    "topology.conf does not match the clique layout the nodes carry." \
    "$(first_sentence "${SWAP_ERR}")"
check "the mismatch names the layout derived from the labels" \
    "cq0 slinky-2 slinky-3 | cq1 slinky-0 slinky-1" \
    "$(layout_section "${SWAP_ERR}" expected)"
check "the mismatch names the layout parsed from the file" \
    "cq0 slinky-0 slinky-1 | cq1 slinky-2 slinky-3" \
    "$(layout_section "${SWAP_ERR}" actual)"

# THE READINESS CLAUSE, and the pairing is what makes this case discriminate.
# The file below agrees PERFECTLY with the three pods that are Running: one
# node in cq0, two in cq1, which is what Topograph writes while the fourth pod
# is Pending. Drop the count check and the comparison returns 0, so the lane
# accepts a topology describing three quarters of the cluster and publishes
# evidence for it. Only the count distinguishes that from a converged cluster.
PODS_ONE_PENDING="slinky-0 aicr-uat-slurm-worker3
slinky-1 aicr-uat-slurm-worker4
slinky-2 aicr-uat-slurm-worker"
CONF_THREE_RUNNING="# block001=cq0
BlockName=block001 Nodes=slinky-2
# block002=cq1
BlockName=block002 Nodes=slinky-[0-1]
BlockSizes=2"
compare_once_with "${PODS_ONE_PENDING}" "${NODES}" "${CONF_THREE_RUNNING}" >/dev/null 2>&1
check "compare_once returns 1 when a slurmd pod is not Running" "1" "$?"
check "the short count is reported as readiness, with both numbers" \
    "3 slurmd pods Running, want 4." \
    "$(first_sentence "$(compare_once_with "${PODS_ONE_PENDING}" "${NODES}" \
        "${CONF_THREE_RUNNING}" 2>&1 >/dev/null)")"

# THE PRE-SYNC SEED. topology_blocks rejects this text on its own (the seed
# block carries no clique comment), so the exit code alone cannot tell the two
# apart -- which is exactly why the message is asserted. Without the marker
# clause the operator is told the file could not be PARSED, sending them to
# Topograph's output for a bug, when the file is the placeholder the leaf ships
# and the real answer is that no sync has landed yet.
CONF_PRESEED="# Managed by NVIDIA Topograph (engine: slinky). Pre-sync placeholder.
BlockName=aicr-preseed Nodes=aicr-preseed-node
BlockSizes=1"
compare_once_with "${PODS}" "${NODES}" "${CONF_PRESEED}" >/dev/null 2>&1
check "compare_once returns 1 on the pre-sync seed" "1" "$?"
check "the pre-sync seed is reported as a missing sync, not a parse failure" \
    "topology.conf still carries the 'aicr-preseed' seed; Topograph has not completed a sync" \
    "$(first_sentence "$(compare_once_with "${PODS}" "${NODES}" "${CONF_PRESEED}" 2>&1 >/dev/null)")"

# THE FOUR CLAUSES NOTHING REACHED, each of which returns 1 from a different
# point before the comparison. The exit code cannot tell them apart -- all four
# are 1, the same 1 a genuine mismatch returns -- so what is asserted is the
# message, which is the only thing that sends an operator to the right place.
# Every one of these is a state a real run hits: an apiserver that refuses the
# read, a ConfigMap key not yet written, output Topograph could not have
# produced, and a node that lost its label.

# The ConfigMap read itself failing. Distinct from the empty-value clause
# below: this is "the apiserver would not answer", which is a retry, and the
# message must not claim anything about what topology.conf says.
compare_once_with "${PODS}" "${NODES}" "" 1 >/dev/null 2>&1
check "compare_once returns 1 when the ConfigMap cannot be read" "1" "$?"
check "a failed read is reported as a read failure, naming key and namespace" \
    "could not read slinky-slurm-config-extra/topology.conf in slurm" \
    "$(first_sentence "$(compare_once_with "${PODS}" "${NODES}" "" 1 2>&1 >/dev/null)")"

# The read SUCCEEDING and returning nothing, which is what a ConfigMap whose
# key has not been written yet looks like. Same fixture as above but rc 0, so
# the pair proves the two clauses are actually distinguished rather than one
# swallowing the other.
compare_once_with "${PODS}" "${NODES}" "" >/dev/null 2>&1
check "compare_once returns 1 when the ConfigMap key is empty" "1" "$?"
check "an empty key is reported as empty, not as a failed read" \
    "slinky-slurm-config-extra/topology.conf is empty" \
    "$(first_sentence "$(compare_once_with "${PODS}" "${NODES}" "" 2>&1 >/dev/null)")"

# Text that will not parse. A BlockName with no `# block=<clique>` comment
# above it cannot be attributed to an accelerator domain, so there is nothing
# to compare -- and this is NOT the pre-sync seed, which carries its own marker
# and its own message. compare_once has to echo the text it could not read,
# because "could not parse" without the input sends an operator to read a
# ConfigMap by hand.
CONF_UNPARSEABLE="BlockName=block001 Nodes=slinky-[0-1]
BlockSizes=2"
compare_once_with "${PODS}" "${NODES}" "${CONF_UNPARSEABLE}" >/dev/null 2>&1
check "compare_once returns 1 on a topology.conf it cannot parse" "1" "$?"
PARSE_ERR="$(compare_once_with "${PODS}" "${NODES}" "${CONF_UNPARSEABLE}" 2>&1 >/dev/null)"
check "an unparseable file is reported as a parse failure, not a mismatch" "1" \
    "$(printf '%s\n' "${PARSE_ERR}" | grep -cF 'could not read the block topology out of topology.conf:')"
check "the parse failure echoes the text it could not read" \
    "BlockName=block001 Nodes=slinky-[0-1] | BlockSizes=2" \
    "$(printf '%s\n' "${PARSE_ERR}" | awk '/^    / { rows = rows (rows == "" ? "" : " | ") substr($0, 5) } END { print rows }')"

# The derivation failing, and the pairing is what makes it discriminate. Four
# pods are Running so the readiness clause is past, but one sits on a node
# carrying no clique label. expected_blocks drops that pod from its pairs,
# reports the node, and returns non-zero -- and the topology.conf below is
# exactly the THREE-node layout those surviving pairs derive to.
#
# So ignoring that non-zero return is not a differently worded failure, it is a
# GREEN RUN: the file and the shrunken expectation agree, and the lane signs
# evidence for a cluster where a node lost its accelerator domain. Measured:
# with `|| return 1` softened to `|| true` this case returns 0, and asserting
# only the message would not have noticed, because expected_blocks writes its
# complaint to stderr either way.
PODS_UNLABELLED_HOST="slinky-0 aicr-uat-slurm-worker3
slinky-1 aicr-uat-slurm-worker4
slinky-2 aicr-uat-slurm-worker
slinky-3 aicr-uat-slurm-worker9"
compare_once_with "${PODS_UNLABELLED_HOST}" "${NODES}" "${CONF_THREE_RUNNING}" >/dev/null 2>&1
check "compare_once returns 1 when the expectation cannot be derived" "1" "$?"
check "an underivable expectation names the unlabelled node and its pod" \
    "expected_blocks: node aicr-uat-slurm-worker9 (hosting slinky-3) carries no ${GPU_CLIQUE_LABEL} label" \
    "$(first_sentence "$(compare_once_with "${PODS_UNLABELLED_HOST}" "${NODES}" \
        "${CONF_THREE_RUNNING}" 2>&1 >/dev/null)")"
# And it must not be dressed up as a mismatch: the shrunken layout under
# "expected" would read as Topograph having invented a node.
check "an underivable expectation is not reported as a topology mismatch" "0" \
    "$(compare_once_with "${PODS_UNLABELLED_HOST}" "${NODES}" "${CONF_THREE_RUNNING}" 2>&1 >/dev/null \
        | grep -cF 'topology.conf does not match the clique layout')"

# THE CALL SITE, which is the half that a function-level test cannot see.
#
# main_with drives main() itself with the two kubectl-touching functions
# replaced -- one layer deep, at the API boundary -- so what runs is main's own
# control flow over fixture data. The relabelled case pairs a WRONG label
# layout with a comparison that SUCCEEDS, which is not a contrived pairing: it
# is the state a live cluster reaches on its own. Topograph resyncs to whatever
# the labels say, so minutes after a relabel the file and the labels agree
# perfectly and the comparison alone goes green on a cluster whose clique
# layout is wrong. If main stops calling check_declared_layout, that case
# returns 0 and this test goes red.
main_with() {
    local node_table="$1" compare_stdout="$2" compare_stderr="$3" compare_rc="$4"
    (
        node_clique_table() { printf '%s\n' "${node_table}"; }
        # The deadline path dumps the slurmd pods. Stubbed so no case here can
        # reach a real cluster, or hang waiting for one that is not there.
        kubectl() { return 1; }
        compare_once() {
            [[ -n "${compare_stderr}" ]] && printf '%s\n' "${compare_stderr}" >&2
            printf '%s\n' "${compare_stdout}"
            return "${compare_rc}"
        }
        # No convergence budget: every case below is decided on the first
        # attempt, and a retry would only turn a red into a slow red.
        export VERIFY_TOPOLOGY_TIMEOUT=0
        export VERIFY_TOPOLOGY_INTERVAL=0
        main "${CLUSTER}" >/dev/null 2>&1
        printf '%s' "$?"
    )
}

check "main passes when the labels are declared and the topology matches" "0" \
    "$(main_with "${NODES}" "ok: topology.conf matches the derived clique layout" "" 0)"
check "main fails on a relabelled cluster even when the comparison succeeds" "1" \
    "$(main_with "${NODES_RELABELLED}" "ok: topology.conf matches the derived clique layout" "" 0)"
# And the comparison's own verdict still reaches the caller, so the declared
# check is an ADDITIONAL gate rather than the only one left.
check "main fails when the comparison fails" "1" \
    "$(main_with "${NODES}" "" "topology.conf does not match the clique layout the nodes carry." 1)"

# THE VERDICT COMES FROM compare_once's EXIT CODE, not from the shape of its
# output. kubectl writes to stderr on a healthy cluster -- a deprecation
# notice, a throttling line, a "couldn't get current server API group list"
# retry -- and any of it landing in the captured text is not evidence about
# the topology. Keying success on the text turned that chatter into a full
# convergence budget of retries and then a FAILURE on a cluster that matches,
# on the one step every later step is gated on.
check "main passes when a matching comparison also emits stderr chatter" "0" \
    "$(main_with "${NODES}" "ok: topology.conf matches the derived clique layout" \
        "W0908 12:00:00.000000   1234 warnings.go:70] v1 Endpoints is deprecated" 0)"
# The mirror, so the fix is not simply "always pass": chatter alongside a
# FAILING comparison must still fail.
check "main fails when chatter accompanies a failing comparison" "1" \
    "$(main_with "${NODES}" "" "W0908 warnings.go:70] deprecated
topology.conf does not match the clique layout the nodes carry." 1)"

# --- the check is wired into the lane --------------------------------------
#
# A guard nobody runs is not a guard, and on this lane it is THE guard. The
# leaf's inline healthCheckAsserts carry the same assertion, but they are
# dispatched by `expected-resources`, a DEPLOYMENT-phase check, and the lane
# runs VALIDATE_PHASES=conformance (tests/uat/kind/run-sim). So this step is
# what holds the #2358 acceptance bar here; if it stops running, or stops
# gating, the lane goes green on a wrong topology.
WORKFLOW="${SCRIPT_DIR}/../../../.github/workflows/uat-kind-sim.yaml"
check "the CI lane calls verify-topology.sh" "1" \
    "$(grep -c 'tests/uat/kind/verify-topology\.sh' "${WORKFLOW}" | tr -d ' ')"
# Calling it is not enough: a step whose failure nothing depends on still lets
# the run publish evidence for a wrong topology. The conformance job must be
# conditioned on the topology step's outcome.
check "the topology step has an id the conformance job can depend on" "1" \
    "$(yq -r '[.jobs["uat-kind-sim"].steps[] | select(.id == "topology")] | length' "${WORKFLOW}")"
check "conformance is gated on the topology step" "1" \
    "$(yq -r '[.jobs["uat-kind-sim"].steps[] | select(.id == "conformance") | select(.if | test("steps\\.topology\\.outcome"))] | length' "${WORKFLOW}")"

# --- and the job summary says so --------------------------------------------
#
# Gating the run is half of it. The step summary is the artifact a human reads
# when a run goes red, and a table that omits this step showed Bootstrap, Prep
# and Install succeeding with Validate skipped, naming nothing that failed.
SUMMARY_RUN="$(yq -r '.jobs["uat-kind-sim"].steps[] | select(.name == "Test Summary") | .run' "${WORKFLOW}")"
check "the Test Summary step was found in the workflow" "1" \
    "$([[ -n "${SUMMARY_RUN}" && "${SUMMARY_RUN}" != "null" ]] && echo 1 || echo 0)"

check "the topology gate has its own row in the job summary" "1" \
    "$(printf '%s' "${SUMMARY_RUN}" | grep -c 'steps\.topology\.outcome' | tr -d ' ')"

# EVERY gating step, not just this one: a step with no row can turn the job red
# while the table shows successes and skips. `versions` is the one exclusion --
# it resolves tool pins before any lane work and is not a lane outcome.
missing=""
while read -r step_id; do
    [[ -z "${step_id}" || "${step_id}" == "versions" ]] && continue
    case "${SUMMARY_RUN}" in
        *"steps.${step_id}.outcome"*) ;;
        *) missing="${missing} ${step_id}" ;;
    esac
done < <(yq -r '.jobs["uat-kind-sim"].steps[] | select(.id) | .id' "${WORKFLOW}")
check "every gating step has a row in the job summary" "" "${missing# }"

# The Validate row is a literal in YAML and the phase list is set in run-sim, so
# the two can drift; the cloud lanes' "Validate (all phases)" label is exactly
# that drift, copied onto a lane that runs one phase. Read the value rather than
# restating it, and prove the read found something before trusting the compare.
LANE_PHASES="$(sed -n 's/^VALIDATE_PHASES="\(.*\)"$/\1/p' "${SCRIPT_DIR}/run-sim")"
check "the phase list was actually extracted from run-sim" "1" \
    "$([[ -n "${LANE_PHASES}" ]] && echo 1 || echo 0)"
check "the job summary names the phases run-sim runs, not every phase" "1" \
    "$(printf '%s' "${SUMMARY_RUN}" | grep -cF "Validate (phases: ${LANE_PHASES})" | tr -d ' ')"
check "the job summary does not claim every validation phase ran" "0" \
    "$(printf '%s' "${SUMMARY_RUN}" | grep -cF 'all phases' | tr -d ' ')"

# Git permits shell-active characters in a branch name (`git check-ref-format
# --branch 'feat/a$(id)b'` succeeds), so a ref spliced inline into a run block
# is an expression injection. uat-azure.yaml routes it through env; this lane
# copied the older uncorrected shape and must not drift back to it.
check "the summary takes the branch name from env, not inline in the run block" "0" \
    "$(printf '%s' "${SUMMARY_RUN}" | grep -cF 'github.ref_name' | tr -d ' ')"

if [[ "${fail}" -ne 0 ]]; then
    echo "topology-golden_test.sh: FAILED" >&2
    exit 1
fi
echo "topology-golden_test.sh: all checks passed"
