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

timeout 300s kind delete cluster --name "${KIND_CLUSTER_NAME}" || true
docker_timeout() {
  local limit="$1"
  shift
  timeout "${limit}" docker "$@"
}
kind_cluster_label="io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}"
mapfile -t remaining_containers < <(docker_timeout 30s ps -aq --filter "label=${kind_cluster_label}" || true)
if (( ${#remaining_containers[@]} > 0 )); then
  echo "Removing leftover kind containers for ${KIND_CLUSTER_NAME}:"
  docker_timeout 30s ps -a --filter "label=${kind_cluster_label}" || true
  docker_timeout 30s rm -f "${remaining_containers[@]}" || true
  mapfile -t remaining_containers < <(docker_timeout 30s ps -aq --filter "label=${kind_cluster_label}" || true)
  if (( ${#remaining_containers[@]} > 0 )); then
    echo "::warning::leftover kind containers still present for ${KIND_CLUSTER_NAME}: ${remaining_containers[*]}"
  fi
fi
