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

cd "$(dirname "${BASH_SOURCE[0]}")/.."

GOTOOLCHAIN="go$(cat .go-version)"
export GOTOOLCHAIN
go version
go env -w GOTOOLCHAIN="${GOTOOLCHAIN}"

# SSH sessions (gh codespace ssh) get their environment from /etc/environment
# through PAM; remoteEnv in devcontainer.json does not reach them. Codespaces
# seeds that file with the container's GOPATH=/go, so override it too. This
# script runs with remoteEnv applied, so PATH and `go env GOPATH` below already
# hold its values, ~/go/bin included; do not append them again.
env_file=/etc/environment
{
    grep -v -E '^(PATH|GOPATH)=' "${env_file}" 2>/dev/null || true
    printf 'PATH="%s"\nGOPATH="%s"\n' "${PATH}" "$(go env GOPATH)"
} | sudo tee "${env_file}.new" >/dev/null
sudo mv "${env_file}.new" "${env_file}"

AUTO_MODE=true make tools-setup
make tools-check

for tool in goreleaser helmfile chainsaw; do
    command -v "$tool" >/dev/null 2>&1 || {
        printf 'required tool %s is missing; rerun `make tools-setup`\n' "$tool" >&2
        exit 1
    }
done
