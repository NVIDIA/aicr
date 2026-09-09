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

# Guards the pins and the node-naming/clique map in setup-gpu-sim.sh.
# Hermetic: sources the script and calls its pure functions, never a cluster.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Resolve the subject SCRIPT_DIR-relative so this exercises the file in THIS
# worktree, never a deployed copy.
# shellcheck source=./setup-gpu-sim.sh
source "${SCRIPT_DIR}/setup-gpu-sim.sh"

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

# --- kind's worker naming -------------------------------------------------
#
# kind names the FIRST worker "<cluster>-worker" with no numeric suffix, and
# only the second onwards carry one. The naive "${cluster}-worker${i}" yields
# "c-worker1", a node that does not exist, so every label and annotation for
# worker 1 lands nowhere and the lane loses a quarter of its GPU capacity
# without any command failing.
check "worker 1 has no numeric suffix" "c-worker" "$(kind_worker_node c 1)"
check "worker 2 carries its index" "c-worker2" "$(kind_worker_node c 2)"
check "worker 4 carries its index" "c-worker4" "$(kind_worker_node c 4)"

out="$(kind_worker_node c 0)"; rc=$?
check "index 0 returns empty stdout" "" "${out}"
check "index 0 fails closed" "1" "${rc}"
out="$(kind_worker_node c notanumber)"; rc=$?
check "non-numeric index returns empty stdout" "" "${out}"
check "non-numeric index fails closed" "1" "${rc}"

# --- clique topology ------------------------------------------------------
#
# Tasks 5 and 6 read these labels to assert a non-trivial block topology.
# Collapsing them to a single clique still produces a valid cluster, so only
# an explicit assertion catches it.
check "worker 1 is in clique cq0" "cq0" "$(worker_clique 1)"
check "worker 2 is in clique cq0" "cq0" "$(worker_clique 2)"
check "worker 3 is in clique cq1" "cq1" "$(worker_clique 3)"
check "worker 4 is in clique cq1" "cq1" "$(worker_clique 4)"

out="$(worker_clique 9)"; rc=$?
check "unmapped worker returns empty stdout" "" "${out}"
check "unmapped worker fails closed" "1" "${rc}"

check "the map enumerates four workers in order" "1 2 3 4" \
    "$(worker_indices | tr '\n' ' ' | sed 's/ $//')"

# Two cliques of two is the minimum that makes a block topology non-trivial.
# One clique, or a 3/1 split, would let a topology assertion pass vacuously.
check "there are exactly two distinct cliques" "2" \
    "$(for i in $(worker_indices); do worker_clique "$i"; echo; done | sort -u | wc -l | tr -d ' ')"
check "each clique holds exactly two workers" "2 2" \
    "$(for i in $(worker_indices); do worker_clique "$i"; echo; done | sort | uniq -c | awk '{print $1}' | tr '\n' ' ' | sed 's/ $//')"

# --- the kind config and the map must stay in step ------------------------
#
# Adding a worker to the kind config without extending the clique map leaves
# an unlabelled node that still advertises GPUs, which silently corrupts the
# topology the lane asserts on.
config="${SCRIPT_DIR}/slurm-cluster-config.yaml"
check "the kind config exists" "yes" "$([[ -f "$config" ]] && echo yes || echo no)"
check "the kind config declares one worker per mapped index" \
    "$(worker_indices | wc -l | tr -d ' ')" \
    "$(grep -c '^  - role: worker$' "$config" | tr -d ' ')"
check "the kind config declares exactly one control plane" "1" \
    "$(grep -c '^  - role: control-plane$' "$config" | tr -d ' ')"

# --- the image pin --------------------------------------------------------
#
# This is the reproducibility contract. ghcr.io/nvidia/nvml-mock:latest is the
# only tag the chart works against, so the lane pins the DIGEST that tag
# resolved to. A regression to a floating tag would still run today and break
# with no change on our side, which is exactly what a test has to catch.
ref="$(nvml_mock_image_ref)"
check "the image reference is a digest, not a tag" "canonical" \
    "$(printf '%s' "$ref" | grep -qE '^ghcr\.io/nvidia/nvml-mock@sha256:[0-9a-f]{64}$' && echo canonical || echo "not-canonical:${ref}")"

# The chart renders image as "{{ repository }}:{{ tag }}", so the digest is
# split across the two values. If the split drifts, helm renders a reference
# such as "repo@sha256:" or "repo:latest" and the pin evaporates. Reassemble
# the two --set values and require the canonical reference back.
repo_arg="$(nvml_mock_helm_image_args | sed -n '1p')"
tag_arg="$(nvml_mock_helm_image_args | sed -n '2p')"
check "the first --set value targets image.repository" "image.repository" "${repo_arg%%=*}"
check "the second --set value targets image.tag" "image.tag" "${tag_arg%%=*}"
check "the split --set values reassemble to the pinned digest" "$ref" \
    "${repo_arg#*=}:${tag_arg#*=}"

check "the chart version reads as an exact release" "release" \
    "$(printf '%s' "$NVML_MOCK_CHART_VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' && echo release || echo "not-a-release:${NVML_MOCK_CHART_VERSION}")"

# A SEMVER-SHAPED STRING IS NOT A PIN. `--version 0.3.0` resolves through a
# MUTABLE tag, and this one moved: the digest recorded when the lane was written
# (sha256:191b6942..., manifest created 2026-09-06T16:02:01Z) is not what the
# tag pointed at two days later (sha256:637b4a53..., created 2026-09-08T11:26:36Z,
# a different chart layer). Nothing compared them, so the lane installed a chart
# its own comment did not describe -- on a DaemonSet that runs on every node in a
# job holding id-token: write and packages: write.
#
# So the resolution has to happen by digest. These assert the reference the
# install actually passes to helm, not the constants in isolation.
check "the chart digest is a full sha256 reference" "digest" \
    "$(printf '%s' "$NVML_MOCK_CHART_DIGEST" | grep -qE '^sha256:[0-9a-f]{64}$' && echo digest || echo "not-a-digest:${NVML_MOCK_CHART_DIGEST}")"
check "the chart reference resolves by digest, not by tag" \
    "${NVML_MOCK_CHART}@${NVML_MOCK_CHART_DIGEST}" "$(nvml_mock_chart_ref)"

# A grep that fires on the regression it guards: reintroducing the floating
# tag as an operative value. Comment lines are stripped first, because the
# script documents at length WHY :latest is not used and naming the tag in
# prose is not the regression.
check "the script carries no floating nvml-mock tag in operative code" "0" \
    "$(grep -vE '^[[:space:]]*#' "${SCRIPT_DIR}/setup-gpu-sim.sh" \
        | grep -cE 'nvml-mock:latest|image\.tag=latest' | tr -d ' ')"

# --- the device plugin manifest ------------------------------------------
#
# The plugin is what turns the mocked NVML library into schedulable
# nvidia.com/gpu capacity. Without the nodeSelector it lands on every node
# including the control plane; without the mock driver root it reports no
# devices at all.
manifest="$(device_plugin_manifest)"
check "the plugin image is pinned to a released tag" "1" \
    "$(printf '%s' "$manifest" | grep -cF "image: ${DEVICE_PLUGIN_IMAGE}" | tr -d ' ')"
check "the plugin targets only the mock-labelled nodes" "1" \
    "$(printf '%s' "$manifest" | grep -cF "${MOKKA_NODE_TYPE_LABEL}: ${MOKKA_NODE_TYPE}" | tr -d ' ')"
check "the plugin reads the mock driver root" "1" \
    "$(printf '%s' "$manifest" | grep -cF -- "--nvidia-driver-root=${NVML_MOCK_DRIVER_ROOT}" | tr -d ' ')"
check "the plugin discovers devices through nvml" "1" \
    "$(printf '%s' "$manifest" | grep -cF -- "--device-discovery-strategy=nvml" | tr -d ' ')"

# --- GPUS_PER_WORKER is the chart's number, not ours -----------------------
#
# Tasks 5 and 6 consume "8 GPUs per worker" as this task's produced interface,
# and verify_capacity gates on it. The value is not a free choice: it is
# however many devices the chart's h100 profile declares. Checking it against
# a fixture extracted verbatim from chart 0.3.0 makes that an external
# reference rather than the constant agreeing with itself.
fixture="${SCRIPT_DIR}/testdata/nvml-mock-h100-devices.txt"
check "the chart profile fixture exists" "yes" \
    "$([[ -f "$fixture" ]] && echo yes || echo no)"
# Couples the fixture to the pin: bumping the chart without re-extracting the
# profile fails here rather than silently comparing against a stale count.
check "the fixture was extracted from the pinned chart version" \
    "${NVML_MOCK_CHART_VERSION}" \
    "$(awk '/^chart_version:/ {print $2}' "$fixture")"
# And from the pinned DIGEST. The version line alone would have gone on
# agreeing across the 0.3.0 re-push, which is the move this pin exists to
# survive; only the digest distinguishes the two charts that both call
# themselves 0.3.0.
check "the fixture was extracted from the pinned chart digest" \
    "${NVML_MOCK_CHART_DIGEST}" \
    "$(awk '/^chart_digest:/ {print $2}' "$fixture")"
check "the fixture describes the profile the script installs" \
    "${NVML_MOCK_GPU_PROFILE}" \
    "$(awk '/^profile:/ {print $2}' "$fixture")"
check "GPUS_PER_WORKER equals the chart profile's device count" \
    "$(grep -cE '^  - index:' "$fixture" | tr -d ' ')" \
    "${GPUS_PER_WORKER}"

# --- the pin reaches its consumer -----------------------------------------
#
# Every assertion above exercises nvml_mock_helm_image_args in ISOLATION. That
# is not the contract. The contract is that the INSTALL carries the pin, and
# dropping "${set_args[@]}" from the helm invocation satisfies every isolated
# assertion while helm falls back to the chart default image.tag: latest. That
# silent return to a floating tag is the precise regression the digest exists
# to prevent, so it has to be asserted at the call site.
#
# helm is an external binary, so stubbing it is one layer deep: the real
# install_nvml_mock body runs and we read back the argv it built. Filtering on
# the HELM_ARGV prefix matters, because install_nvml_mock also echoes a
# progress line containing the digest, which would otherwise satisfy these
# greps and mask the very mutation they exist to catch.
# shellcheck disable=SC2329  # invoked indirectly, by install_nvml_mock
helm() { printf 'HELM_ARGV%s\n' "$(printf ' %s' "$@")"; }
install_argv="$(install_nvml_mock test-context | grep '^HELM_ARGV')"

check "the install passes the pinned repository to helm" "1" \
    "$(printf '%s\n' "$install_argv" | grep -cF -- "--set image.repository=${NVML_MOCK_IMAGE_REPOSITORY}@${NVML_MOCK_IMAGE_DIGEST%%:*}")"
check "the install passes the pinned digest hex to helm" "1" \
    "$(printf '%s\n' "$install_argv" | grep -cF -- "--set image.tag=${NVML_MOCK_IMAGE_DIGEST#*:}")"
# The chart reference itself, which is where the pin now lives. A digest
# constant the install does not use is decoration, and `--version` alongside a
# digest is worse than useless: helm re-resolves the tag and compares, so an
# upstream re-tag fails a lane the digest had already made reproducible.
check "the install passes the chart digest reference to helm" "1" \
    "$(printf '%s\n' "$install_argv" | grep -cF -- "$(nvml_mock_chart_ref)")"
check "the install passes no bare, tag-resolved chart reference" "0" \
    "$(printf '%s\n' "$install_argv" | grep -cE -- "${NVML_MOCK_CHART}[[:space:]]")"
check "the install does not resolve the chart through --version" "0" \
    "$(printf '%s\n' "$install_argv" | grep -cF -- '--version')"
check "the install selects the gpu profile" "1" \
    "$(printf '%s\n' "$install_argv" | grep -cF -- "--set gpu.profile=${NVML_MOCK_GPU_PROFILE}")"
# Nothing in the install may name a floating tag.
check "the install names no floating tag" "0" \
    "$(printf '%s\n' "$install_argv" | grep -cE 'image\.tag=latest|nvml-mock:latest')"

# --- the spike corrections reach their consumer ---------------------------
#
# The mokka label and topograph's two annotations are two of the five things
# the spike established by trial and error (topograph answered 502 without the
# annotations and 200 with them). They are cheap to delete and expensive to
# rediscover, and until now nothing failed if they went missing.
# shellcheck disable=SC2329  # invoked indirectly, by label_workers
kubectl() { printf 'KUBECTL_ARGV%s\n' "$(printf ' %s' "$@")"; }
label_argv="$(label_workers test-context testcluster | grep '^KUBECTL_ARGV')"

check "every worker is labelled as a mock GPU node" "4" \
    "$(printf '%s\n' "$label_argv" | grep -F -- 'label node' | grep -cF -- "${MOKKA_NODE_TYPE_LABEL}=${MOKKA_NODE_TYPE}")"
check "the labelling pass applies the cq0/cq0/cq1/cq1 layout" "cq0 cq0 cq1 cq1" \
    "$(printf '%s\n' "$label_argv" | grep -oE "${GPU_CLIQUE_LABEL}=[^ ]+" | sed 's/.*=//' | tr '\n' ' ' | sed 's/ $//')"
check "every worker gets topograph's region annotation" "4" \
    "$(printf '%s\n' "$label_argv" | grep -F -- 'annotate node' | grep -cF -- "${TOPOGRAPH_REGION_ANNOTATION}=${TOPOGRAPH_REGION}")"
# The instance annotation must name the node it is applied to. A single
# shared value would still be four annotations, and topograph would collapse
# the four workers onto one instance.
check "each instance annotation carries that node's own name" \
    "testcluster-worker testcluster-worker2 testcluster-worker3 testcluster-worker4" \
    "$(printf '%s\n' "$label_argv" | grep -oE "${TOPOGRAPH_INSTANCE_ANNOTATION}=[^ ]+" | sed 's/.*=//' | tr '\n' ' ' | sed 's/ $//')"
check "labelling touches only the mapped workers" "4" \
    "$(printf '%s\n' "$label_argv" | grep -cF -- 'label node')"

exit "${fail}"
