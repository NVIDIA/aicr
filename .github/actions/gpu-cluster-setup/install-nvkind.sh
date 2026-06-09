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

if [[ -z "${NVKIND_VERSION:-}" ]]; then
  echo "::error::NVKIND_VERSION must be set"
  exit 1
fi

go install "github.com/NVIDIA/nvkind/cmd/nvkind@${NVKIND_VERSION}"
nvkind_bin="${GOBIN:-$(go env GOPATH)/bin}/nvkind"
"${nvkind_bin}" --help
