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
# Gives the four real workers of a kind cluster simulated GPU capacity, so the
# UAT slurm lane can run slurmd for real without GPU hardware.
#
# Three things have to line up before `nvidia.com/gpu` appears on a node:
#
#   1. nvml-mock lays a mocked NVML driver tree under /var/lib/nvml-mock.
#   2. The node carries mokka.nvidia.com/type=sgpu, which is what the DEVICE
#      PLUGIN selects on. The chart does NOT write this label, and does not
#      select on it either: chart 0.3.0 defaults to `nodeSelector: {}` and
#      gates the block on `{{- with .Values.nodeSelector }}`, so the mock
#      DaemonSet lands on every node, control plane included. The label is
#      still required, because without it the plugin has nowhere to run.
#   3. The NVIDIA device plugin reads that tree instead of a real driver and
#      advertises the devices it finds.
#
# Miss any one and the cluster comes up healthy with zero GPUs.
#
# Usage:
#   tests/uat/kind/setup-gpu-sim.sh [cluster-name]
#
# Defaults to the cluster named in slurm-cluster-config.yaml. Sourcing this
# file defines constants and functions only and touches no cluster, so the
# shell suite can exercise it hermetically.

# --- pinned artefacts -------------------------------------------------------

# The nvml-mock chart, pinned by DIGEST.
#
# `--version 0.3.0` is NOT a pin: it resolves through a tag, and upstream can
# re-push it. This one already moved. The digest first recorded here,
# sha256:191b6942..., has a manifest created 2026-09-06T16:02:01Z; two days
# later the same 0.3.0 tag pointed at sha256:637b4a53..., created
# 2026-09-08T11:26:36Z with a different chart layer. Both call themselves
# 0.3.0. Nothing compared them, so the lane installed whatever the tag meant
# that day, on a DaemonSet that runs on every node of a job holding
# id-token: write and packages: write.
#
# The two charts render identically here (802 lines, byte for byte, with
# gpu.profile=h100); they differ only in NOTES.txt and the chart's own unit
# test fixtures. That is what makes the move harmless THIS time, and it is not
# a property anyone can rely on for the next one.
#
# helm 4 resolves an oci:// reference carrying @sha256:, so the digest is the
# reference rather than a claim in a comment. `--version` is deliberately NOT
# passed alongside it: helm would then re-resolve the tag and compare, which
# fails the lane whenever upstream re-tags, on a chart the digest had already
# made reproducible.
#
# NVML_MOCK_CHART_VERSION is the human label for the log line and the release
# the digest came from. It resolves nothing; setup-gpu-sim_test.sh asserts the
# install call carries the digest and no --version, so it cannot quietly become
# the resolver again.
#
# To move to a new release:
#   helm show chart oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock --version <v>
# and take the `Digest:` line, then re-verify capacity on a scratch cluster.
NVML_MOCK_CHART="oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock"
NVML_MOCK_CHART_VERSION="0.3.0"
NVML_MOCK_CHART_DIGEST="sha256:637b4a534b9b91d231cd179cf91374aed7e4bbcda648fbd597ad858e269365c6"
NVML_MOCK_NAMESPACE="nvml-mock"

# The node-agent image, pinned by DIGEST.
#
# Read this before changing it. Chart 0.3.0 defaults to the floating tag
# ghcr.io/nvidia/nvml-mock:latest, and that tag is the ONLY one that works:
# the released :0.3.0 image ships no /usr/local/bin/node-agent and CrashLoops
# against this chart, so "pin the image to the chart version" is not available.
#
# A floating tag in a CI lane is not reproducible and can break with no change
# on our side, so the lane pins the digest that :latest resolved to instead.
# The digest below is the multi-arch OCI image INDEX (linux/amd64 and
# linux/arm64), not a single-platform manifest, so it is valid on both the
# amd64 CI runners and arm64 developer machines.
#
# To re-resolve after an upstream release:
#   docker buildx imagetools inspect ghcr.io/nvidia/nvml-mock:latest
# and take the top-level `Digest:` (MediaType image.index.v1+json), then
# re-verify capacity on a scratch cluster before committing the new value.
NVML_MOCK_IMAGE_REPOSITORY="ghcr.io/nvidia/nvml-mock"
NVML_MOCK_IMAGE_DIGEST="sha256:ca34b399fb54af0a8edb01746db271a5dab44d9d4f9bd7a9e77a1553eb5ac350"

# Where the chart writes the mocked driver tree, and where the device plugin
# reads it from. One constant because the two must agree; they are mounted
# into different pods and a mismatch shows up only as zero capacity.
NVML_MOCK_DRIVER_ROOT="/var/lib/nvml-mock/driver"
NVML_MOCK_HOST_ROOT="/var/lib/nvml-mock"

# GPU profile. h100.yaml in the chart declares 8 devices, which is where the
# advertised capacity of 8 per worker comes from. Set explicitly rather than
# inherited so an upstream default change cannot silently reshape the lane.
NVML_MOCK_GPU_PROFILE="h100"
GPUS_PER_WORKER=8

# The device plugin, pinned by DIGEST for the same reason the nvml-mock chart
# and its image above are: a tag is mutable, and a CI gate must not change the
# bytes it runs without a commit. nvcr.io release tags are not immutable.
#
# The digest is the multi-arch manifest LIST (linux/amd64 and linux/arm64), not
# a single-platform manifest, so it is valid on both the amd64 CI runners and
# arm64 developer machines.
#
# To re-resolve after an upstream release, taking the top-level digest:
#   regctl manifest digest nvcr.io/nvidia/k8s-device-plugin:<version>
#   docker buildx imagetools inspect nvcr.io/nvidia/k8s-device-plugin:<version>
# Both printed sha256:b5788e2... for v0.18.2 on 2026-09-11.
#
# DEVICE_PLUGIN_VERSION records which release the digest was taken from. It is
# documentation: nothing resolves through it, and setup-gpu-sim_test.sh fails
# if a tag reference reappears in operative code.
DEVICE_PLUGIN_REPOSITORY="nvcr.io/nvidia/k8s-device-plugin"
DEVICE_PLUGIN_VERSION="v0.18.2"
DEVICE_PLUGIN_DIGEST="sha256:b5788e29e7ae5272de8de863ebe386d6611e608421a6ecc5b4e7d5952aba637f"
DEVICE_PLUGIN_IMAGE="${DEVICE_PLUGIN_REPOSITORY}@${DEVICE_PLUGIN_DIGEST}"
DEVICE_PLUGIN_NAME="nvidia-device-plugin-mock"
DEVICE_PLUGIN_NAMESPACE="kube-system"

# The label the DEVICE PLUGIN selects on. The mock itself does not: chart
# 0.3.0 ships an empty nodeSelector and runs on every node. Keeping the label
# off the control plane is what leaves it GPU-free.
MOKKA_NODE_TYPE_LABEL="mokka.nvidia.com/type"
MOKKA_NODE_TYPE="sgpu"

GPU_CLIQUE_LABEL="nvidia.com/gpu.clique"

# topograph refuses to serve a topology for nodes it cannot place: without
# both of these it answers 502, and with them 200. The instance value is the
# node's own name; the region is arbitrary but must be present and consistent.
TOPOGRAPH_INSTANCE_ANNOTATION="topograph.run/instance"
TOPOGRAPH_REGION_ANNOTATION="topograph.run/region"
TOPOGRAPH_REGION="region-1"

# Worker index to GPU clique. Two cliques of two; see slurm-cluster-config.yaml
# for why. Deliberately not `readonly`, so a shell that sources this file twice
# does not abort on reassignment.
WORKER_CLIQUE_MAP="1:cq0
2:cq0
3:cq1
4:cq1"

DEFAULT_CLUSTER_NAME="aicr-uat-slurm"
KUBECTL_TIMEOUT="30s"
ROLLOUT_TIMEOUT="300s"

# --- pure helpers -----------------------------------------------------------

# kind_worker_node <cluster> <index>
#
# Prints the node name kind gives worker <index> of <cluster>.
#
# kind names the first worker "<cluster>-worker" with NO numeric suffix and
# only numbers the second onwards. Deriving names as "<cluster>-worker<index>"
# therefore targets "<cluster>-worker1", which does not exist; kubectl label
# then fails on one node out of four, or worse, is run with --ignore-not-found
# and leaves a quarter of the cluster silently unlabelled.
#
# Fails closed on anything that is not a positive integer.
kind_worker_node() {
    local cluster="${1:-}" index="${2:-}"
    case "${index}" in
        1) printf '%s-worker' "${cluster}" ;;
        [2-9] | [1-9][0-9]) printf '%s-worker%s' "${cluster}" "${index}" ;;
        *) return 1 ;;
    esac
}

# worker_clique <index>
#
# Prints the GPU clique for worker <index>. Returns 1 with empty stdout for an
# index the map does not cover, so a caller can tell "not mapped" from "mapped
# to an empty value" rather than labelling a node with the empty string.
worker_clique() {
    local index="${1:-}" entry
    while IFS= read -r entry; do
        if [[ "${entry%%:*}" == "${index}" ]]; then
            printf '%s' "${entry#*:}"
            return 0
        fi
    done <<<"${WORKER_CLIQUE_MAP}"
    return 1
}

# worker_indices
#
# Prints every worker index the clique map covers, one per line, in map order.
worker_indices() {
    local entry
    while IFS= read -r entry; do
        printf '%s\n' "${entry%%:*}"
    done <<<"${WORKER_CLIQUE_MAP}"
}

# nvml_mock_chart_ref
#
# Prints the digest reference the chart is installed from.
nvml_mock_chart_ref() {
    printf '%s@%s' "${NVML_MOCK_CHART}" "${NVML_MOCK_CHART_DIGEST}"
}

# nvml_mock_image_ref
#
# Prints the canonical digest reference for the node-agent image.
nvml_mock_image_ref() {
    printf '%s@%s' "${NVML_MOCK_IMAGE_REPOSITORY}" "${NVML_MOCK_IMAGE_DIGEST}"
}

# nvml_mock_helm_image_args
#
# Prints the two `--set` values that pin the chart to the digest, one per line.
#
# The chart's daemonset template renders the image as
# "{{ .Values.image.repository }}:{{ .Values.image.tag }}" with no digest
# support, so setting image.repository to a full digest reference would render
# a trailing colon and setting image.tag to a digest would render two. The
# digest is instead split at its own colon: repository absorbs the "@sha256"
# suffix and tag carries the hex. Helm then joins them back into exactly
# "<repo>@sha256:<hex>". setup-gpu-sim_test.sh reassembles these two values and
# requires nvml_mock_image_ref back, so the trick cannot rot unnoticed.
nvml_mock_helm_image_args() {
    printf '%s\n' \
        "image.repository=${NVML_MOCK_IMAGE_REPOSITORY}@${NVML_MOCK_IMAGE_DIGEST%%:*}" \
        "image.tag=${NVML_MOCK_IMAGE_DIGEST#*:}"
}

# device_plugin_manifest
#
# Prints the DaemonSet that advertises the mocked devices as nvidia.com/gpu.
#
# Not the upstream Helm chart: that chart assumes a real driver installation
# and has no supported way to repoint the plugin at a mock tree. The two flags
# that matter are --nvidia-driver-root (read the mock, not /) and
# --device-discovery-strategy=nvml (ask the mocked library rather than probing
# for real /dev/nvidia* nodes, which do not exist here).
device_plugin_manifest() {
    cat <<MANIFEST
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ${DEVICE_PLUGIN_NAME}
  namespace: ${DEVICE_PLUGIN_NAMESPACE}
spec:
  selector:
    matchLabels:
      name: ${DEVICE_PLUGIN_NAME}
  updateStrategy:
    type: RollingUpdate
  template:
    metadata:
      labels:
        name: ${DEVICE_PLUGIN_NAME}
    spec:
      # Only the workers carry this label, which is what keeps the control
      # plane GPU-free and makes the negative control meaningful.
      nodeSelector:
        ${MOKKA_NODE_TYPE_LABEL}: ${MOKKA_NODE_TYPE}
      tolerations:
        - operator: Exists
      containers:
        - name: nvidia-device-plugin
          image: ${DEVICE_PLUGIN_IMAGE}
          imagePullPolicy: IfNotPresent
          securityContext:
            privileged: true
          args:
            - --nvidia-driver-root=${NVML_MOCK_DRIVER_ROOT}
            - --driver-root-ctr-path=${NVML_MOCK_DRIVER_ROOT}
            - --device-discovery-strategy=nvml
            - --pass-device-specs=true
          volumeMounts:
            - name: mock-root
              mountPath: ${NVML_MOCK_HOST_ROOT}
              readOnly: true
            - name: device-plugins
              mountPath: /var/lib/kubelet/device-plugins
      volumes:
        - name: mock-root
          hostPath:
            path: ${NVML_MOCK_HOST_ROOT}
        - name: device-plugins
          hostPath:
            path: /var/lib/kubelet/device-plugins
MANIFEST
}

# --- cluster actions --------------------------------------------------------

# label_workers <context> <cluster>
#
# Applies the mock-target label, the clique label and topograph's instance and
# region annotations to every mapped worker.
label_workers() {
    local context="$1" cluster="$2" index node clique
    for index in $(worker_indices); do
        node="$(kind_worker_node "${cluster}" "${index}")" || {
            echo "error: cannot derive a node name for worker ${index}" >&2
            return 1
        }
        clique="$(worker_clique "${index}")" || {
            echo "error: worker ${index} has no clique in WORKER_CLIQUE_MAP" >&2
            return 1
        }

        echo "labelling ${node} (clique ${clique})"
        kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
            label node "${node}" --overwrite \
            "${MOKKA_NODE_TYPE_LABEL}=${MOKKA_NODE_TYPE}" \
            "${GPU_CLIQUE_LABEL}=${clique}" || return 1

        kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
            annotate node "${node}" --overwrite \
            "${TOPOGRAPH_INSTANCE_ANNOTATION}=${node}" \
            "${TOPOGRAPH_REGION_ANNOTATION}=${TOPOGRAPH_REGION}" || return 1
    done
}

# install_nvml_mock <context>
install_nvml_mock() {
    local context="$1" set_args=() arg
    while IFS= read -r arg; do
        set_args+=(--set "${arg}")
    done < <(nvml_mock_helm_image_args)

    echo "installing nvml-mock chart ${NVML_MOCK_CHART_VERSION} ($(nvml_mock_chart_ref)) at $(nvml_mock_image_ref)"
    helm --kube-context "${context}" upgrade --install nvml-mock \
        "$(nvml_mock_chart_ref)" \
        --namespace "${NVML_MOCK_NAMESPACE}" \
        --create-namespace \
        --set "gpu.profile=${NVML_MOCK_GPU_PROFILE}" \
        "${set_args[@]}" \
        --wait --timeout "${ROLLOUT_TIMEOUT}"
}

# install_device_plugin <context>
install_device_plugin() {
    local context="$1"
    # The version is logged alongside the digest because a digest alone tells
    # an operator reading CI output nothing about which release is running.
    echo "installing the device plugin ${DEVICE_PLUGIN_VERSION} at ${DEVICE_PLUGIN_IMAGE}"
    device_plugin_manifest |
        kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" apply -f -
}

# verify_capacity <context> <cluster>
#
# Fails unless every mapped worker advertises exactly GPUS_PER_WORKER devices.
# Reading the node object is the only check that proves the whole chain
# (mock, label, plugin, kubelet) actually closed; a green helm rollout does
# not.
verify_capacity() {
    local context="$1" cluster="$2" index node got rc=0
    for index in $(worker_indices); do
        node="$(kind_worker_node "${cluster}" "${index}")" || return 1
        got="$(kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
            get node "${node}" -o jsonpath='{.status.capacity.nvidia\.com/gpu}' 2>/dev/null)"
        if [[ "${got}" != "${GPUS_PER_WORKER}" ]]; then
            echo "FAIL: ${node} advertises '${got}' GPUs, want ${GPUS_PER_WORKER}" >&2
            rc=1
        else
            echo "ok: ${node} advertises ${got} GPUs"
        fi
    done
    return "${rc}"
}

# wait_for_capacity <context> <cluster>
#
# The plugin registers with kubelet a few seconds after its pod is Running, so
# capacity trails the rollout. Poll rather than sleep a fixed interval.
wait_for_capacity() {
    local context="$1" cluster="$2"
    for _ in $(seq 1 30); do
        if verify_capacity "${context}" "${cluster}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 5
    done
    echo "error: GPU capacity did not appear within 150s" >&2
    return 1
}

main() {
    local cluster="${1:-${DEFAULT_CLUSTER_NAME}}"
    local context="kind-${cluster}"

    local tool
    for tool in kubectl helm; do
        command -v "${tool}" >/dev/null 2>&1 || {
            echo "error: ${tool} is not on PATH" >&2
            return 1
        }
    done

    label_workers "${context}" "${cluster}" || return 1
    install_nvml_mock "${context}" || return 1
    install_device_plugin "${context}" || return 1

    kubectl --context "${context}" --request-timeout="${KUBECTL_TIMEOUT}" \
        rollout status "daemonset/${DEVICE_PLUGIN_NAME}" \
        -n "${DEVICE_PLUGIN_NAMESPACE}" --timeout="${ROLLOUT_TIMEOUT}" || return 1

    wait_for_capacity "${context}" "${cluster}" || return 1
    verify_capacity "${context}" "${cluster}"
}

# Source guard: sourcing defines constants and functions only.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    set -uo pipefail
    main "$@"
fi
