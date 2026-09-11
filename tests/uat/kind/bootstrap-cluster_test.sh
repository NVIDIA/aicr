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

# Guards bootstrap-cluster.sh and the ONE-SCRIPT-TWO-CALLERS contract it
# exists to hold: the local acceptance run and the CI lane must stand the
# cluster up through the same committed file. Two hand-written copies of
# `kind create cluster` drift, and the drift surfaces as an assertion that
# passes locally and fails in CI (or the reverse).
#
# Hermetic: sources the script and stubs kind/kubectl, never a cluster.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Resolve the subject SCRIPT_DIR-relative so this exercises the file in THIS
# worktree, never a deployed copy.
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
# Sourcing the bootstrap also defines setup-gpu-sim.sh's constants and pure
# helpers (WORKER_CLIQUE_MAP, worker_indices, DEFAULT_CLUSTER_NAME, ...): the
# bootstrap sources it for the clique map rather than restating the layout.
# shellcheck source=./bootstrap-cluster.sh
source "${SCRIPT_DIR}/bootstrap-cluster.sh"

KIND_CONFIG="${SCRIPT_DIR}/slurm-cluster-config.yaml"
SETTINGS="${REPO_ROOT}/.settings.yaml"
WORKFLOW="${REPO_ROOT}/.github/workflows/uat-kind-sim.yaml"
RUNNER="${SCRIPT_DIR}/run-sim"

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

# Operative lines only: every file below documents at length WHY it is shaped
# the way it is, and naming a command in prose is not the regression any of
# these greps guards.
operative() { grep -vE '^[[:space:]]*#' "$1"; }

# --- the cluster name lives in ONE place ----------------------------------
#
# Three artifacts have to agree on it: the kind config that declares it, the
# bootstrap that creates it, and setup-gpu-sim.sh's default. A bootstrap that
# carried its own literal would create `aicr-uat-slurm` and then label the
# workers of whatever cluster the GPU-sim script defaulted to -- which on a
# machine with two clusters is a silent cross-cluster write, not an error.
check "the bootstrap defaults to the cluster the kind config names" \
    "$(sed -n 's/^name:[[:space:]]*//p' "${KIND_CONFIG}")" \
    "$(bootstrap_cluster_name)"
check "the GPU simulation defaults to the same cluster" \
    "$(bootstrap_cluster_name)" "${DEFAULT_CLUSTER_NAME}"
check "an explicit name overrides the default" "scratch-cluster" \
    "$(bootstrap_cluster_name scratch-cluster)"

# --- the node image is resolved, never pinned here -------------------------
#
# Every other kind consumer in the repo resolves testing.kind_node_image from
# .settings.yaml at run time so Renovate bumps one line. Parsed here with sed
# rather than yq so the expectation is derived independently of the script's
# own yq call.
check "the node image comes from .settings.yaml" \
    "$(sed -n "s/^  kind_node_image:[[:space:]]*['\"]\\{0,1\\}\\([^'\"]*\\)['\"]\\{0,1\\}[[:space:]]*$/\\1/p" "${SETTINGS}")" \
    "$(bootstrap_node_image)"
check "the bootstrap hardcodes no node image" "0" \
    "$(operative "${SCRIPT_DIR}/bootstrap-cluster.sh" | grep -c 'kindest/node' | tr -d ' ')"

# --- the create call ------------------------------------------------------
#
# kind is an external binary, so stubbing it is one layer deep: the real
# create_cluster body runs and we read back the argv it built. Dropping
# --config from that call yields a healthy ONE-node cluster, which then fails
# three phases later as "three slurmd pods Pending" -- a failure that points
# at the operator rather than at the missing flag.
# shellcheck disable=SC2329  # invoked indirectly, by create_cluster
kind() { printf 'KIND_ARGV%s\n' "$(printf ' %s' "$@")"; }
create_argv="$(create_cluster testcluster | grep '^KIND_ARGV')"
check "the create names the cluster explicitly" "1" \
    "$(printf '%s\n' "${create_argv}" | grep -cF -- '--name testcluster')"
check "the create uses the four-worker config" "1" \
    "$(printf '%s\n' "${create_argv}" | grep -cF -- "--config ${KIND_CONFIG}")"
check "the create pins the resolved node image" "1" \
    "$(printf '%s\n' "${create_argv}" | grep -cF -- "--image $(bootstrap_node_image)")"
unset -f kind

# --- the post-condition discriminates -------------------------------------
#
# verify_gpu_topology is the bootstrap's contract with Tasks 5 and 6: four
# workers advertising GPUs in two cliques of two, and a GPU-free control
# plane. Each fixture below is a cluster that comes up HEALTHY and would pass
# a "cluster is ready" check, so only an explicit assertion separates them.
# The table is the `<node>|<gpu-capacity>|<clique>` view of the cluster that
# verify_gpu_topology reads from one kubectl call.
NODE_TABLE=""
# shellcheck disable=SC2329  # invoked indirectly, by verify_gpu_topology
kubectl() { printf '%s\n' "${NODE_TABLE}"; }

good_table() {
    printf 'testcluster-control-plane||\n'
    printf 'testcluster-worker|8|cq0\n'
    printf 'testcluster-worker2|8|cq0\n'
    printf 'testcluster-worker3|8|cq1\n'
    printf 'testcluster-worker4|8|cq1\n'
}

NODE_TABLE="$(good_table)"
verify_gpu_topology test-context testcluster >/dev/null 2>&1; rc=$?
check "the expected four-worker two-clique cluster passes" "0" "${rc}"

# The Task 6 mutation: move one worker into the other clique. Every node is
# still Ready and still advertises eight GPUs, so nothing else notices.
NODE_TABLE="$(good_table | sed 's/^testcluster-worker3|8|cq1$/testcluster-worker3|8|cq0/')"
verify_gpu_topology test-context testcluster >/dev/null 2>&1; rc=$?
check "a 3/1 clique split fails" "1" "${rc}"

# The negative control the whole GPU-free control plane rests on. If the mock
# label ever reaches the control plane the device plugin schedules there, a
# fifth node advertises GPUs, and a slurmd pod can land on it.
NODE_TABLE="$(good_table | sed 's/^testcluster-control-plane||$/testcluster-control-plane|8|/')"
verify_gpu_topology test-context testcluster >/dev/null 2>&1; rc=$?
check "a GPU-advertising control plane fails" "1" "${rc}"

# Partial capacity: the device plugin registered but found fewer devices than
# the pinned profile declares.
NODE_TABLE="$(good_table | sed 's/^testcluster-worker2|8|cq0$/testcluster-worker2|4|cq0/')"
verify_gpu_topology test-context testcluster >/dev/null 2>&1; rc=$?
check "short GPU capacity on a worker fails" "1" "${rc}"

# An unlabelled worker: label_workers ran against the wrong cluster, or one
# kubectl call failed and the script carried on.
NODE_TABLE="$(good_table | sed 's/^testcluster-worker4|8|cq1$/testcluster-worker4|8|/')"
verify_gpu_topology test-context testcluster >/dev/null 2>&1; rc=$?
check "a worker with no clique label fails" "1" "${rc}"

# A missing worker: kind created fewer nodes than the config declares.
NODE_TABLE="$(good_table | grep -v '^testcluster-worker4')"
verify_gpu_topology test-context testcluster >/dev/null 2>&1; rc=$?
check "a missing worker fails" "1" "${rc}"
unset -f kubectl

# --- one bootstrap, two callers -------------------------------------------
#
# The requirement this file exists for. Task 6 verifies the acceptance
# assertion locally and the CI lane runs it; both must stand the cluster up
# through THIS script. A workflow that inlines the two commands instead is
# the drift that makes a green local run and a red CI run (or the reverse)
# possible, and nothing else in the tree forces the two to agree.
check "the CI lane exists" "yes" \
    "$([[ -f "${WORKFLOW}" ]] && echo yes || echo no)"
# Captured ONCE, then matched against, rather than piped into each grep.
# `grep -q` exits the instant it matches, closing the pipe under a producer
# that is still writing; `operative` then dies of EPIPE, and pipefail promotes
# that to the pipeline's status -- so a SUCCESSFUL match reports "no". It is a
# race on the producer's second write (this workflow renders ~9KB through a
# 4KB stdio buffer), which is why it fires on CI and not on a developer box.
workflow_ops="$(operative "${WORKFLOW}")"
check "the CI lane calls the shared bootstrap" "yes" \
    "$(grep -qF 'tests/uat/kind/bootstrap-cluster.sh' <<<"${workflow_ops}" && echo yes || echo no)"
check "the CI lane drives the sim runner" "yes" \
    "$(grep -qF 'tests/uat/kind/run-sim' <<<"${workflow_ops}" && echo yes || echo no)"
# The nvkind runner would apply that lane's cluster assumptions to this one:
# EXPECTED_GPU_NODES=skip drops the four-worker census, and
# TRAINJOB_NUM_NODES=1 describes a single-GPU node this cluster does not have.
check "the CI lane does not drive the nvkind runner" "0" \
    "$(grep -cE 'tests/uat/kind/run[^-]' <<<"${workflow_ops}" | tr -d ' ')"

# No second copy of the create sequence anywhere a lane could reach. Scoped to
# the workflows and the UAT tree (a doc may legitimately quote the command).
# Two files are exempt, by EXACT basename: the bootstrap, because it is the
# copy, and this file, because the pattern below is itself the string being
# searched for. Exact rather than a `^bootstrap-cluster` prefix, which is the
# guard's own most likely failure: nobody drifts by writing `other-lane.sh`,
# they drift by copying the bootstrap to `bootstrap-cluster-v2.sh` or
# `bootstrap-cluster-gpu.sh` and editing it, and a prefix would exempt
# exactly that.
#
# Candidates are otherwise filtered by KIND of file rather than by excluding
# known noise: a workflow, a shell script, or an extensionless runner shim
# can create a cluster, and nothing else here can. That drops an editor
# backup or a `.sh.bak` left by a mutation run, which would otherwise fail
# this check on a developer machine for a file git never sees.
duplicates="$(grep -rl 'kind create cluster' \
    "${REPO_ROOT}/.github/workflows" "${REPO_ROOT}/tests/uat" 2>/dev/null \
    | awk -F/ '{ base = $NF }
        base == "bootstrap-cluster.sh" || base == "bootstrap-cluster_test.sh" { next }
        base ~ /\.(ya?ml|sh)$/ || base !~ /\./ { print }' \
    | while IFS= read -r f; do
        # Capture before matching, for the reason given above. Here the stakes
        # invert: a SIGPIPEd producer makes a file that DOES restate the create
        # sequence report as no-match, so this guard would drop a real
        # duplicate and pass. That direction is silent.
        file_ops="$(operative "$f")"
        grep -q 'kind create cluster' <<<"${file_ops}" && printf '%s\n' "$f"
      done)"
check "nothing else creates the slurm cluster" "" "${duplicates}"

# The runner shim must not restate the cluster shape either: both the census
# count and the census selector are derived from setup-gpu-sim.sh's map and
# label constants, so a fifth worker changes one file.
check "the sim runner derives the GPU-node count from the clique map" "1" \
    "$(operative "${RUNNER}" | grep -c 'worker_indices' | tr -d ' ')"
check "the sim runner hardcodes no GPU-node count" "0" \
    "$(operative "${RUNNER}" | grep -cE 'EXPECTED_GPU_NODES=[0-9]' | tr -d ' ')"
check "the sim runner derives the census selector from the mock label" "1" \
    "$(operative "${RUNNER}" | grep -c 'GPU_CENSUS_SELECTOR=.*MOKKA_NODE_TYPE_LABEL' | tr -d ' ')"

# --- the runner decision (D4) ---------------------------------------------
#
# The lane exists because simulated GPU nodes need no hardware, so it runs on
# a GitHub-hosted runner and takes no reservation. Moving it to the
# self-hosted GPU runner would consume the kind-h100 reservation's capacity
# WITHOUT holding its lease, racing the nvkind lane on the same machine.
check "the lane runs on a GitHub-hosted runner" "1" \
    "$(operative "${WORKFLOW}" | grep -cE '^ *runs-on: ubuntu-latest$' | tr -d ' ')"
check "the lane names no self-hosted runner label" "0" \
    "$(operative "${WORKFLOW}" | grep -c 'linux-amd64-gpu' | tr -d ' ')"
check "the lane takes no reservation" "0" \
    "$(operative "${WORKFLOW}" | grep -c 'reservation' | tr -d ' ')"

# A slurm recipe has no K8s-native CUJ: a TrainJob would go to the Kubernetes
# scheduler and bypass slurmd, measuring the wrong path, and the TrainJob CRD
# is not even installed by this recipe. cuj_phase_for returns `none` for it;
# this lane invokes discrete phases, so nothing but this check stops a train
# step being added back.
check "the lane runs no TrainJob CUJ" "0" \
    "$(operative "${WORKFLOW}" | grep -cE 'run-sim +train' | tr -d ' ')"

exit "${fail}"
