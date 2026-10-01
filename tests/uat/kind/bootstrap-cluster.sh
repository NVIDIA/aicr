#!/usr/bin/env bash
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
# Stands up the UAT slurm lane's cluster: four real kind workers with
# simulated GPU capacity, in two cliques of two, on a GPU-free control plane.
#
# THIS FILE IS THE ONLY PLACE THE SEQUENCE LIVES. Two callers run it -- a
# local acceptance run and .github/workflows/uat-kind-sim.yaml -- and they
# must stand the cluster up identically. Written out twice, the two copies
# drift, and the drift surfaces as an assertion that passes locally and fails
# in CI, or the reverse: the failure looks like a product defect and is not
# one. bootstrap-cluster_test.sh fails if a second copy of `kind create
# cluster` appears under .github/workflows/ or tests/uat/.
#
# Usage:
#   tests/uat/kind/bootstrap-cluster.sh [cluster-name]
#
# The name defaults to the one slurm-cluster-config.yaml declares. An existing
# cluster of that name is REUSED (the GPU-simulation pass is idempotent:
# labels are applied with --overwrite, the chart with `helm upgrade
# --install`, the plugin with `kubectl apply`), so a repeated local run
# re-converges rather than failing on a name collision.
#
# Sourcing this file defines constants and functions only and touches no
# cluster, so the shell suite can exercise it hermetically.

# Cluster shape (WORKER_CLIQUE_MAP, GPUS_PER_WORKER), kind's worker naming
# (kind_worker_node) and the label constants come from the GPU-simulation
# script rather than being restated here: the clique layout has exactly one
# definition, and setup-gpu-sim_test.sh already gates it against the kind
# config's worker count.
# shellcheck source=./setup-gpu-sim.sh
source "$(dirname "${BASH_SOURCE[0]}")/setup-gpu-sim.sh"

BOOTSTRAP_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BOOTSTRAP_REPO_ROOT="$(cd "${BOOTSTRAP_SCRIPT_DIR}/../../.." && pwd)"
BOOTSTRAP_KIND_CONFIG="${BOOTSTRAP_SCRIPT_DIR}/slurm-cluster-config.yaml"
BOOTSTRAP_SETTINGS="${BOOTSTRAP_REPO_ROOT}/.settings.yaml"

# kind's own readiness wait. Five nodes on a cold runner pull the node image
# once and start five kubelets; 300s is generous and fails closed.
BOOTSTRAP_CREATE_WAIT="${BOOTSTRAP_CREATE_WAIT:-300s}"

# The node-table field separator. Deliberately NOT a tab: bash treats tab as
# IFS whitespace and collapses runs of it, so a node with no GPU capacity
# ("name<tab><tab>cq0") would read its clique into the capacity field and the
# check would compare the wrong values.
BOOTSTRAP_FIELD_SEP='|'

# bootstrap_cluster_name [name]
#
# Prints the cluster name to operate on: the argument when given, otherwise
# the name slurm-cluster-config.yaml declares. The config is the single
# source of truth, so `kind create` and the GPU-simulation pass cannot end up
# addressing two different clusters.
bootstrap_cluster_name() {
    local name="${1:-}"
    if [[ -n "${name}" ]]; then
        printf '%s' "${name}"
        return 0
    fi
    name="$(yq -r '.name' "${BOOTSTRAP_KIND_CONFIG}" 2>/dev/null)"
    if [[ -z "${name}" || "${name}" == "null" ]]; then
        echo "error: ${BOOTSTRAP_KIND_CONFIG} declares no cluster name" >&2
        return 1
    fi
    printf '%s' "${name}"
}

# bootstrap_node_image
#
# Prints the kind node image from .settings.yaml. Resolved at run time rather
# than pinned in the kind config, matching every other kind consumer in the
# repo (kwok/scripts/run-all-recipes.sh, tools/component-test/ensure-cluster.sh,
# the GitHub composite actions) so Renovate bumps one line.
bootstrap_node_image() {
    local image
    image="$(yq -r '.testing.kind_node_image' "${BOOTSTRAP_SETTINGS}" 2>/dev/null)"
    if [[ -z "${image}" || "${image}" == "null" ]]; then
        echo "error: testing.kind_node_image is not set in ${BOOTSTRAP_SETTINGS}" >&2
        return 1
    fi
    printf '%s' "${image}"
}

# bootstrap_require_tools
#
# Fails closed on a missing tool rather than part-way through the bootstrap:
# helm is not needed until the nvml-mock install, which is three minutes and
# one cluster after the point where a missing binary is cheap to fix.
bootstrap_require_tools() {
    local tool rc=0
    for tool in kind kubectl helm yq; do
        command -v "${tool}" >/dev/null 2>&1 || {
            echo "error: ${tool} is not on PATH" >&2
            rc=1
        }
    done
    return "${rc}"
}

# cluster_exists <cluster>
cluster_exists() {
    kind get clusters 2>/dev/null | grep -qx -- "$1"
}

# create_cluster <cluster>
#
# --config is what makes this a FOUR-WORKER cluster. Without it kind creates
# a healthy single-node cluster, and the failure surfaces three phases later
# as three slurmd pods Pending -- which reads as an operator problem rather
# than a missing flag.
create_cluster() {
    local cluster="$1" image
    image="$(bootstrap_node_image)" || return 1
    echo "creating kind cluster ${cluster} from $(basename "${BOOTSTRAP_KIND_CONFIG}") at ${image}"
    kind create cluster \
        --name "${cluster}" \
        --config "${BOOTSTRAP_KIND_CONFIG}" \
        --image "${image}" \
        --wait "${BOOTSTRAP_CREATE_WAIT}"
}

# kube_node_table <context>
#
# Prints one `<node>|<nvidia.com/gpu capacity>|<clique label>` row per node.
# One API call rather than one per node, so the whole-cluster negative
# control below (nothing outside the mapped workers advertises GPUs) reads a
# single consistent view.
kube_node_table() {
    local context="$1"
    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" get nodes \
        -o "jsonpath={range .items[*]}{.metadata.name}${BOOTSTRAP_FIELD_SEP}{.status.capacity.nvidia\.com/gpu}${BOOTSTRAP_FIELD_SEP}{.metadata.labels.nvidia\.com/gpu\.clique}{\"\n\"}{end}"
}

# verify_gpu_topology <context> <cluster>
#
# The bootstrap's post-condition, and the contract the topology assertion
# rests on. A cluster that fails any clause below still comes up HEALTHY --
# every node Ready, every pod Running -- so nothing else in the stack
# separates these cases:
#
#   - a worker in the wrong clique: two blocks of two collapse to 3/1, and
#     the generated topology is wrong rather than absent.
#   - a GPU-advertising control plane: a slurmd pod can land on a node with
#     no clique label, so one Slurm node has no accelerator domain.
#   - short capacity: the device plugin registered but read a different
#     profile than the pinned one.
#
# Reads the labels back from the API rather than trusting that label_workers
# ran: `kubectl label` against a cluster that is not the one under test
# succeeds just as loudly as against the right one.
verify_gpu_topology() {
    local context="$1" cluster="$2" table rc=0

    table="$(kube_node_table "${context}")" || {
        echo "FAIL: could not read nodes from ${context}" >&2
        return 1
    }

    local index node want_clique row cap clique
    for index in $(worker_indices); do
        node="$(kind_worker_node "${cluster}" "${index}")" || return 1
        want_clique="$(worker_clique "${index}")" || return 1

        row="$(printf '%s\n' "${table}" | grep -F -- "${node}${BOOTSTRAP_FIELD_SEP}" | head -1)"
        if [[ -z "${row}" ]]; then
            echo "FAIL: ${node} is not in the cluster (kind created fewer workers than the config declares?)" >&2
            rc=1
            continue
        fi

        IFS="${BOOTSTRAP_FIELD_SEP}" read -r _ cap clique <<<"${row}"
        if [[ "${cap}" != "${GPUS_PER_WORKER}" ]]; then
            echo "FAIL: ${node} advertises '${cap}' GPUs, want ${GPUS_PER_WORKER}" >&2
            rc=1
        fi
        if [[ "${clique}" != "${want_clique}" ]]; then
            echo "FAIL: ${node} is in clique '${clique}', want ${want_clique}" >&2
            rc=1
        fi
        if [[ "${cap}" == "${GPUS_PER_WORKER}" && "${clique}" == "${want_clique}" ]]; then
            echo "ok: ${node} advertises ${cap} GPUs in clique ${clique}"
        fi
    done

    # Negative control. The mapped workers are the ONLY nodes that may
    # advertise GPUs: the control plane never gets the mock label, which is
    # what keeps the device plugin off it, and that is what makes a
    # four-node accelerator domain set meaningful rather than accidental.
    local advertising expected
    advertising="$(printf '%s\n' "${table}" \
        | awk -F"${BOOTSTRAP_FIELD_SEP}" '$2 != "" && $2 != "0" {print $1}' | sort)"
    expected="$(for index in $(worker_indices); do
        kind_worker_node "${cluster}" "${index}"
        echo
    done | sort)"
    if [[ "${advertising}" != "${expected}" ]]; then
        echo "FAIL: nodes advertising GPUs [${advertising//$'\n'/ }] are not exactly the mapped workers [${expected//$'\n'/ }]" >&2
        rc=1
    fi

    return "${rc}"
}

main() {
    local cluster context
    cluster="$(bootstrap_cluster_name "${1:-}")" || return 1
    context="kind-${cluster}"

    bootstrap_require_tools || return 1

    if cluster_exists "${cluster}"; then
        echo "reusing existing kind cluster ${cluster}"
    else
        create_cluster "${cluster}" || return 1
    fi

    kubectl --context "${context}" wait --for=condition=Ready node --all \
        --timeout="${BOOTSTRAP_CREATE_WAIT}" || return 1

    "${BOOTSTRAP_SCRIPT_DIR}/setup-gpu-sim.sh" "${cluster}" || return 1

    echo "verifying the simulated GPU topology"
    verify_gpu_topology "${context}" "${cluster}" || return 1

    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" get nodes \
        -L "${GPU_CLIQUE_LABEL}" -L "${MOKKA_NODE_TYPE_LABEL}"
}

# Source guard: sourcing defines constants and functions only.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    set -uo pipefail
    main "$@"
fi
