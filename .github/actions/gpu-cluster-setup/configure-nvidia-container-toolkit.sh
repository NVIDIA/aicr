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

sudo nvidia-ctk runtime configure --runtime=docker --set-as-default --cdi.enabled
sudo nvidia-ctk config --set accept-nvidia-visible-devices-as-volume-mounts=true --in-place
sudo nvidia-ctk config --set accept-nvidia-visible-devices-envvar-when-unprivileged=false --in-place
set +e
timeout 120s sudo systemctl restart docker
restart_status=$?
set -e
if (( restart_status != 0 )); then
  echo "::error::Docker restart failed after NVIDIA runtime configuration"
  sudo systemctl status docker --no-pager || true
  sudo journalctl -u docker --since "10 minutes ago" --no-pager || true
  exit "${restart_status}"
fi

for attempt in $(seq 1 30); do
  if systemctl is-active --quiet docker && timeout 5s docker info >/dev/null 2>&1; then
    echo "Docker is healthy after NVIDIA runtime configuration."
    exit 0
  fi
  echo "Waiting for Docker to become healthy... (${attempt}/30)"
  sleep 2
done

echo "::error::Docker did not become healthy after NVIDIA runtime configuration"
sudo systemctl status docker --no-pager || true
exit 1
