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
#
# A creator counts only when a `kind create cluster` command itself builds THE
# slurm cluster: its --name or --config argument names the slurm cluster or
# the slurm kind config, either literally or through a variable the same file
# assigns. A workflow expression there counts the same way when it is
# ${{ env.VAR }} and the file assigns VAR, and counts as the slurm cluster
# when it cannot be resolved from the file, as does a variable the file
# assigns an expression, so an expression fails closed in either spelling.
# A copied bootstrap passes the config through a variable, an inlined
# create passes the name, and a lane with its own topology and name (the Mokka
# gpu-operator lane) is not a second copy of this one, even when other lines
# in its file mention the slurm cluster. Backslash continuations are joined
# first, because the bootstrap spreads its create over several lines. An empty
# DEFAULT_CLUSTER_NAME counts every create, so a lost name flags every creator
# rather than none.
#
# Reads operative lines on stdin; exits 0 when one of them is such a create.
creates_slurm_cluster() {
    awk -v cfg='slurm-cluster-config' -v name="${DEFAULT_CLUSTER_NAME}" '
        function names_slurm(s) { return index(s, cfg) > 0 || index(s, name) > 0 }
        function assigns(line, ref) { return line ~ ("(^|[^A-Za-z0-9_])" ref "(=|:[[:space:]])") }
        # True when line assigns ref a value holding a GitHub expression
        # anywhere, `$(echo "${{ ... }}")` included. The value ends with its
        # shell command (the first ; or &&), so an unrelated expression later
        # on the same line (a one-line `VAR=x; kind create cluster --image
        # ${{ ... }}`) does not count. Calls match(), so callers inside a
        # match() loop must save RSTART and RLENGTH first.
        function assigns_expression(line, ref,    v) {
            if (!match(line, "(^|[^A-Za-z0-9_])" ref "(=|:[[:space:]])")) return 0
            v = substr(line, RSTART + RLENGTH)
            sub(/(;|&&).*$/, "", v)
            return index(v, "${{") > 0
        }
        # Rewrites each GitHub expression in s: ${{ env.VAR }} becomes ${VAR}
        # when a line of this file assigns VAR, so resolves_slurm judges it
        # like the $VAR spelling, and anything else becomes UNRESOLVED, which
        # resolves_slurm counts as the slurm cluster. Workflow expressions are
        # not shell variables, so without this a quoted one splits into tokens
        # that name nothing and the create escapes the check.
        function expand_expressions(s,    out, at, rest, end, expr, ref, i, val) {
            out = ""
            while ((at = index(s, "${{")) > 0) {
                out = out substr(s, 1, at - 1)
                rest = substr(s, at + 3)
                end = index(rest, "}}")
                expr = end ? substr(rest, 1, end - 1) : rest
                s = end ? substr(rest, end + 2) : ""
                gsub(/^[[:space:]]+|[[:space:]]+$/, "", expr)
                val = "UNRESOLVED"
                if (expr ~ /^env\.[A-Za-z_][A-Za-z0-9_]*$/) {
                    ref = substr(expr, 5)
                    for (i = 1; i <= n; i++)
                        if (assigns(logical[i], ref))
                            val = "${" ref "}"
                }
                out = out val
            }
            return out s
        }
        # True when s names the slurm cluster literally, or through a $VAR or
        # ${VAR} that a line of this file assigns (shell VAR=, YAML VAR:) to a
        # value that does or to an expression, or carries an expression
        # expand_expressions could not resolve. An expression-valued variable
        # is unresolvable from the file, so it fails closed like the
        # ${{ env.VAR }} spelling of the same chain.
        function resolves_slurm(s,    rest, ref, nxt, i) {
            if (names_slurm(s) || index(s, "UNRESOLVED") > 0) return 1
            rest = s
            while (match(rest, /\$\{?[A-Za-z_][A-Za-z0-9_]*/)) {
                ref = substr(rest, RSTART + 1, RLENGTH - 1)
                nxt = RSTART + RLENGTH
                sub(/^\{/, "", ref)
                for (i = 1; i <= n; i++)
                    if (assigns(logical[i], ref) && (names_slurm(logical[i]) || assigns_expression(logical[i], ref)))
                        return 1
                rest = substr(rest, nxt)
            }
            return 0
        }
        {
            line = pending $0
            if (line ~ /\\[[:space:]]*$/) {
                sub(/\\[[:space:]]*$/, " ", line)
                pending = line
                next
            }
            pending = ""
            logical[++n] = line
        }
        END {
            for (i = 1; i <= n; i++) {
                at = index(logical[i], "kind create cluster")
                if (at == 0) continue
                if (name == "") exit 0
                k = split(expand_expressions(substr(logical[i], at + 19)), arg, /[[:space:]=]+/)
                for (j = 1; j < k; j++)
                    if ((arg[j] == "--name" || arg[j] == "--config") && resolves_slurm(arg[j + 1]))
                        exit 0
            }
            exit 1
        }'
}
slurm_cluster_creators() {
    grep -rl 'kind create cluster' "$@" 2>/dev/null \
    | awk -F/ '{ base = $NF }
        base == "bootstrap-cluster.sh" || base == "bootstrap-cluster_test.sh" { next }
        base ~ /\.(ya?ml|sh)$/ || base !~ /\./ { print }' \
    | while IFS= read -r f; do
        # Capture before matching, for the reason given above. Here the stakes
        # invert: a SIGPIPEd producer makes a file that DOES restate the create
        # sequence report as no-match, so this guard would drop a real
        # duplicate and pass. That direction is silent.
        file_ops="$(operative "$f")"
        creates_slurm_cluster <<<"${file_ops}" && printf '%s\n' "$f"
      done
}
duplicates="$(slurm_cluster_creators "${REPO_ROOT}/.github/workflows" "${REPO_ROOT}/tests/uat")"
check "nothing else creates the slurm cluster" "" "${duplicates}"

# Both directions of that scoping, on fixtures: a copied bootstrap, a verbatim
# one, an inlined slurm create by literal or by workflow env (as a shell
# variable or as ${{ env.VAR }}), and a create whose name is an expression
# the file cannot resolve, directly, through an unassigned env.VAR, or through
# a variable assigned an expression, are caught; an independent topology
# (also when it is named and imaged through expressions, on one line or
# several), a create of kind's default cluster, a slurm create that is only
# a comment, and an independent create in a file that mentions the slurm
# cluster elsewhere (including in a variable whose name ends in the create's
# own) are not.
fixture="$(mktemp -d)"
mkdir -p "${fixture}/workflows"
# shellcheck disable=SC2016 # the fixture holds the copied script's text, unexpanded
printf '%s\n' 'BOOTSTRAP_KIND_CONFIG="${DIR}/slurm-cluster-config.yaml"' \
    'kind create cluster --config "${BOOTSTRAP_KIND_CONFIG}"' > "${fixture}/bootstrap-cluster-v2.sh"
printf '%s\n' "run: kind create cluster --name ${DEFAULT_CLUSTER_NAME} --config /tmp/k.yaml" \
    > "${fixture}/workflows/inline-slurm.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'env:' "  CLUSTER: ${DEFAULT_CLUSTER_NAME}" \
    'run: kind create cluster --name="$CLUSTER" --config /tmp/k.yaml' \
    > "${fixture}/workflows/inline-slurm-env.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'env:' "  CLUSTER: ${DEFAULT_CLUSTER_NAME}" \
    'run: kind create cluster --name "${{ env.CLUSTER }}" --config /tmp/k.yaml' \
    > "${fixture}/workflows/inline-slurm-expr.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'run: kind create cluster --name "${{ inputs.cluster }}" --config /tmp/k.yaml' \
    > "${fixture}/workflows/unresolvable-expr.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'env:' '  CLUSTER: ${{ inputs.cluster }}' \
    'run: kind create cluster --name "${{ env.CLUSTER }}" --config /tmp/k.yaml' \
    > "${fixture}/workflows/unresolvable-env.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'run: kind create cluster --name "${{ env.NOPE }}" --config /tmp/k.yaml' \
    > "${fixture}/workflows/unassigned-env.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'env:' '  CLUSTER: ${{ inputs.cluster }}' \
    'run: kind create cluster --name "$CLUSTER" --config /tmp/k.yaml' \
    > "${fixture}/workflows/unresolvable-env-var.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'run: CLUSTER=${{ inputs.cluster }}; kind create cluster --name "$CLUSTER" --config /tmp/k.yaml' \
    > "${fixture}/workflows/unresolvable-shell-var.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'env:' '  CLUSTER:     ${{ inputs.cluster }}' \
    'run: kind create cluster --name "$CLUSTER" --config /tmp/k.yaml' \
    > "${fixture}/workflows/unresolvable-env-aligned.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'run: CLUSTER=$(echo "${{ inputs.cluster }}"); kind create cluster --name "$CLUSTER" --config /tmp/k.yaml' \
    > "${fixture}/workflows/unresolvable-subst-var.yaml"
# Two variables in one argument: the scan must reach the second after
# resolving the first.
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'env:' '      KIND_DIR: /tmp/kind' '      CFG: slurm-cluster-config.yaml' \
    'run: kind create cluster --name x --config "$KIND_DIR/$CFG"' \
    > "${fixture}/workflows/inline-slurm-two-vars.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'run: CLUSTER=aicr-mokka; kind create cluster --name "$CLUSTER" --image "${{ steps.mokka.outputs.node_image }}" --config /tmp/kind-mokka.yaml' \
    > "${fixture}/workflows/independent-one-line.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'run: CLUSTER=aicr-mokka && kind create cluster --name "$CLUSTER" --image "${{ steps.mokka.outputs.node_image }}" --config /tmp/kind-mokka.yaml' \
    > "${fixture}/workflows/independent-one-line-and.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'env:' '  CLUSTER: aicr-mokka' \
    'run: kind create cluster --name "${{ env.CLUSTER }}" --image "${{ steps.mokka.outputs.node_image }}" --config /tmp/kind-mokka.yaml' \
    > "${fixture}/workflows/independent-expr.yaml"
printf '%s\n' 'run: kind create cluster --name aicr-mokka --config /tmp/kind-mokka.yaml' \
    > "${fixture}/workflows/independent.yaml"
printf '%s\n' 'run: kind create cluster' > "${fixture}/workflows/default-name.yaml"
printf '%s\n' "# sized like ${DEFAULT_CLUSTER_NAME}" "# was: kind create cluster --name ${DEFAULT_CLUSTER_NAME}" \
    'run: kind create cluster --name other --config /tmp/o.yaml' \
    > "${fixture}/workflows/comment-only.yaml"
# shellcheck disable=SC2016 # the fixture holds the workflow's text, unexpanded
printf '%s\n' 'env:' "  SLURM_CLUSTER_NAME: ${DEFAULT_CLUSTER_NAME}" '  CLUSTER_NAME: aicr-mokka' \
    "run: kubectl --context kind-${DEFAULT_CLUSTER_NAME} get nodes" \
    'run: yq .nodes tests/uat/kind/slurm-cluster-config.yaml' \
    'run: kind create cluster --name "$CLUSTER_NAME" --config /tmp/kind-mokka.yaml' \
    > "${fixture}/workflows/mixed.yaml"
cp "${SCRIPT_DIR}/bootstrap-cluster.sh" "${fixture}/bootstrap-cluster-copy.sh"
check "a copied bootstrap and an inlined slurm create are duplicates; other topologies are not" \
    "$(printf '%s\n' "${fixture}/bootstrap-cluster-v2.sh" "${fixture}/bootstrap-cluster-copy.sh" \
        "${fixture}/workflows/inline-slurm.yaml" "${fixture}/workflows/inline-slurm-env.yaml" \
        "${fixture}/workflows/inline-slurm-expr.yaml" "${fixture}/workflows/unresolvable-expr.yaml" \
        "${fixture}/workflows/unresolvable-env.yaml" "${fixture}/workflows/unassigned-env.yaml" \
        "${fixture}/workflows/unresolvable-env-var.yaml" "${fixture}/workflows/unresolvable-shell-var.yaml" \
        "${fixture}/workflows/unresolvable-env-aligned.yaml" "${fixture}/workflows/unresolvable-subst-var.yaml" \
        "${fixture}/workflows/inline-slurm-two-vars.yaml" | sort)" \
    "$(slurm_cluster_creators "${fixture}" | sort)"
check "a lost slurm cluster name flags every creator rather than none" \
    "$(grep -rl 'kind create cluster' "${fixture}" | sort)" \
    "$(DEFAULT_CLUSTER_NAME='' slurm_cluster_creators "${fixture}" | sort)"
rm -rf "${fixture}"

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

# --- the CI step outlives the bootstrap's own waits ------------------------
#
# A step timeout that fires first kills the bootstrap mid-wait, and the run
# shows a cancelled step instead of the wait that ran out. The budget was
# summed by hand twice and both sums missed waits, so it is derived here from
# the real sequence: the stubs log every wait budget and every poll sleep
# while main runs, including in the setup-gpu-sim.sh child, which is why they
# are exported and log to a file. No capacity ever appears, so
# wait_for_capacity spends its whole poll budget.
WAIT_LOG="$(mktemp "${TMPDIR:-/tmp}/bootstrap-waits.XXXXXX")"
export WAIT_LOG
# to_seconds <value> [bare] prints a positive duration in whole seconds.
# kind --wait, helm --timeout and kubectl --timeout are Go durations, which
# need a unit, so a bare number is accepted only for sleep. Anything else
# prints nothing and fails: a compound or fractional value (1m30s, 1.5h), an
# unknown unit, and zero, which `kubectl rollout status` reads as no limit.
# shellcheck disable=SC2329  # invoked indirectly, by the stubs below
to_seconds() {
    local value="$1" bare="${2:-}" n unit
    if [[ "${value}" =~ ^([0-9]+)([smh])$ ]]; then
        n="${BASH_REMATCH[1]}" unit="${BASH_REMATCH[2]}"
    elif [[ -n "${bare}" && "${value}" =~ ^[0-9]+$ ]]; then
        n="${value}" unit=s
    else
        return 1
    fi
    case "${unit}" in
        s) n=$((10#${n})) ;;
        m) n=$((10#${n} * 60)) ;;
        h) n=$((10#${n} * 3600)) ;;
    esac
    ((n > 0)) || return 1
    printf '%s' "${n}"
}
# log_budget <tool> <value> [bare] logs the value in seconds, or marks it
# UNPARSED so the budget check below fails closed instead of miscounting it.
# shellcheck disable=SC2329  # invoked indirectly, by the stubs below
log_budget() {
    local seconds
    if seconds="$(to_seconds "$2" "${3:-}")"; then
        printf '%s %s\n' "$1" "${seconds}" >>"${WAIT_LOG}"
    else
        printf '%s UNPARSED:%s\n' "$1" "$2" >>"${WAIT_LOG}"
    fi
}
# record_budget <tool> <flag> <argv...> logs the budget given to <flag>,
# whether passed as `<flag> <v>` or `<flag>=<v>`. The flag is per tool because
# `--wait` takes the budget for kind but is a bare switch for helm.
# shellcheck disable=SC2329  # invoked indirectly, by the stubs below
record_budget() {
    local tool="$1" flag="$2" arg value="" take=0
    shift 2
    for arg in "$@"; do
        if ((take)); then
            value="${arg}"
            take=0
        elif [[ "${arg}" == "${flag}" ]]; then
            take=1
        elif [[ "${arg}" == "${flag}="* ]]; then
            value="${arg#"${flag}"=}"
        fi
    done
    [[ -n "${value}" ]] && log_budget "${tool}" "${value}"
    return 0
}
# shellcheck disable=SC2329  # invoked indirectly, by main and setup-gpu-sim.sh
kind() { record_budget kind --wait "$@"; }
# Drains a piped manifest: an unread pipe SIGPIPEs the heredoc producer, and
# setup-gpu-sim.sh's pipefail turns that into a failed install.
# shellcheck disable=SC2329
kubectl() {
    case " $* " in *" -f - "*) cat >/dev/null ;; esac
    record_budget kubectl --timeout "$@"
}
# shellcheck disable=SC2329
helm() { record_budget helm --timeout "$@"; }
# shellcheck disable=SC2329
sleep() { log_budget sleep "$1" bare; }
export -f to_seconds log_budget record_budget kind kubectl helm sleep
main testcluster >/dev/null 2>&1
unset -f to_seconds log_budget record_budget kind kubectl helm sleep

# Proves the harness reached every wait, so the comparison below cannot pass
# on a run that stopped early: kind's create wait, the node Ready wait, the
# nvml-mock install, the device plugin and host engine rollouts, the GPU
# health monitor pre-pull, then the capacity poll (its sleeps folded into one
# entry).
check "the harness observed every bootstrap wait in order" \
    "kind kubectl helm kubectl kubectl kubectl sleep" \
    "$(awk '$1 == "sleep" && prev == "sleep" { next } { print $1; prev = $1 }' "${WAIT_LOG}" |
        tr '\n' ' ' | sed 's/ $//')"
unparsed="$(awk '$2 ~ /^UNPARSED:/ { printf "%s%s %s", sep, $1, substr($2, 10); sep = ", " }' "${WAIT_LOG}")"
check "every wait budget is a duration the harness can convert" "" "${unparsed}"
wait_budget="$(awk '{sum += $2} END {print sum + 0}' "${WAIT_LOG}")"
budget_label="${wait_budget}s"
[[ -z "${unparsed}" ]] || wait_budget="" budget_label="total unknown"
rm -f "${WAIT_LOG}"
bootstrap_minutes="$(yq -r '.jobs["uat-kind-sim"].steps[] | select(.id == "bootstrap") | ."timeout-minutes"' "${WORKFLOW}")"
check "the bootstrap step outlives its waits (${budget_label})" "outlives" \
    "$([[ "${wait_budget}" =~ ^[0-9]+$ && "${bootstrap_minutes}" =~ ^[0-9]+$ ]] &&
        ((bootstrap_minutes * 60 > wait_budget)) &&
        echo outlives || echo "timeout-minutes=${bootstrap_minutes:-unset}")"

exit "${fail}"
