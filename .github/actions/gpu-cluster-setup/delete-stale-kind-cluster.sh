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

set -euo pipefail
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME must be set}"
kind_cluster_label="io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}"
docker_timeout() {
  timeout 30s docker "$@"
}

read_kind_container_ids() {
  local output

  if ! output="$(docker_timeout ps -aq --filter "label=${kind_cluster_label}" 2>&1)"; then
    echo "::error::failed to query stale kind containers for ${KIND_CLUSTER_NAME}"
    echo "${output}"
    exit 1
  fi

  remaining_containers=()
  if [[ -n "${output}" ]]; then
    mapfile -t remaining_containers <<< "${output}"
  fi
}

if kind get clusters | grep -Fxq "${KIND_CLUSTER_NAME}"; then
  echo "Deleting stale kind cluster: ${KIND_CLUSTER_NAME}"
  if ! timeout 180s kind delete cluster --name "${KIND_CLUSTER_NAME}"; then
    echo "::warning::kind delete cluster timed out or failed; falling back to direct container cleanup"
  fi
else
  echo "No stale kind cluster named ${KIND_CLUSTER_NAME}"
fi

read_kind_container_ids
if (( ${#remaining_containers[@]} > 0 )); then
  echo "Removing stale containers for ${KIND_CLUSTER_NAME}:"
  docker_timeout ps -a --filter "label=${kind_cluster_label}"
  docker_timeout rm -f "${remaining_containers[@]}"
fi

read_kind_container_ids
if (( ${#remaining_containers[@]} > 0 )); then
  echo "::error::stale containers still remain for ${KIND_CLUSTER_NAME}:"
  docker_timeout ps -a --filter "label=${kind_cluster_label}"
  exit 1
fi
