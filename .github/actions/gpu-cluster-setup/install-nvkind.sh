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

# Installs nvkind at the pinned commit, patched so that its
# ConfigureContainerRuntime step invokes
#   nvidia-ctk runtime configure --runtime=containerd --config-source=file
# instead of the upstream default
#   nvidia-ctk runtime configure --runtime=containerd --config-source=command
#
# Why (issue #1237):
#   The upstream nvkind path runs `containerd config dump` (via
#   --config-source=command) to read the current containerd config
#   before merging in the NVIDIA runtime block. As of containerd
#   2.3.x (shipped in kindest/node:v1.36.1) `config dump` emits
#   schema version=4 / io.containerd.cri.v1.runtime, while the kind
#   node base /etc/containerd/config.toml is still hand-written at
#   version=2 / io.containerd.grpc.v1.cri. The merged drop-in then
#   disagrees with the base on `version`, so the subsequent
#   `systemctl restart containerd` fails ("Job for containerd.service
#   failed") and the worker node never becomes Ready.
#
#   Switching to --config-source=file makes nvidia-ctk read the
#   actual /etc/containerd/config.toml the kind image ships, so the
#   emitted drop-in matches the base's schema version. This is the
#   remediation recommended by the nvidia-container-toolkit
#   maintainers (see #1237 discussion) — the toolkit's
#   --config-source=command behavior is by design, and they will
#   not ship a compat flag.
#
#   Scope: this patch is intentionally narrow — it changes only the
#   nvkind binary's behavior. No other invocation of nvidia-ctk in
#   this repo is affected (the host-level
#   configure-nvidia-container-toolkit.sh targets --runtime=docker
#   on a different code path).

set -euo pipefail

if [[ -z "${NVKIND_VERSION:-}" ]]; then
  echo "::error::NVKIND_VERSION must be set"
  exit 1
fi

nvkind_src="$(mktemp -d)"
trap 'rm -rf "${nvkind_src}"' EXIT

echo "Cloning nvkind at ${NVKIND_VERSION} into ${nvkind_src}"
git clone --quiet https://github.com/NVIDIA/nvkind "${nvkind_src}"
git -C "${nvkind_src}" checkout --quiet "${NVKIND_VERSION}"

node_file="${nvkind_src}/pkg/nvkind/node.go"
if [[ ! -f "${node_file}" ]]; then
  echo "::error::expected nvkind source file not found: ${node_file}"
  exit 1
fi

# Confirm the upstream string we're about to rewrite is still present;
# if nvkind upstream changes it, fail loud rather than silently
# building an unpatched binary.
if ! grep -q -- '--config-source=command' "${node_file}"; then
  echo "::error::nvkind ${NVKIND_VERSION} no longer contains '--config-source=command' in ${node_file#"${nvkind_src}/"}; review whether this patch is still needed"
  exit 1
fi

echo "Patching --config-source=command → --config-source=file (issue #1237)"
sed -i.bak 's/--config-source=command/--config-source=file/g' "${node_file}"
rm -f "${node_file}.bak"

# Verify the patch landed and that the source line we removed is gone.
if grep -q -- '--config-source=command' "${node_file}"; then
  echo "::error::patch did not apply cleanly; --config-source=command still present"
  exit 1
fi
if ! grep -q -- '--config-source=file' "${node_file}"; then
  echo "::error::patch did not apply cleanly; --config-source=file not present after sed"
  exit 1
fi

echo "Building patched nvkind"
(cd "${nvkind_src}" && go install ./cmd/nvkind)

nvkind_bin="${GOBIN:-$(go env GOPATH)/bin}/nvkind"
"${nvkind_bin}" --help
