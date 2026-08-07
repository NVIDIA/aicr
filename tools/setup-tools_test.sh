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

# Verify versioned Go tool installs do not inherit an exported vendor mode.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETUP_TOOLS="${SCRIPT_DIR}/setup-tools"

bash -n "${SETUP_TOOLS}"

expected_installs=(
    'github.com/tilt-dev/ctlptl/cmd/ctlptl@v"${CTLPTL_VERSION}"'
    'github.com/goreleaser/goreleaser/v2@"${GORELEASER_VERSION}"'
    'golang.org/x/exp/cmd/apidiff@"${APIDIFF_VERSION}"'
    'github.com/google/addlicense@"${ADDLICENSE_VERSION}"'
    'github.com/google/go-licenses/v2@"${GO_LICENSES_VERSION}"'
)

for install_target in "${expected_installs[@]}"; do
    if ! grep -Fq "GOFLAGS= go install ${install_target}" "${SETUP_TOOLS}"; then
        echo "FAIL: versioned go install does not clear GOFLAGS: ${install_target}" >&2
        exit 1
    fi
done

echo "All versioned Go tool installs clear GOFLAGS"
