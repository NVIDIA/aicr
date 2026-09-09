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

# Scan every `go install` rather than checking a named list. A positive list
# goes stale in the direction that matters: it cannot catch a *new* install
# added without the GOFLAGS reset, which is the regression this guards against.
# It also fails whenever an install is legitimately removed -- as #2664 did,
# replacing three of them with binary-release downloads.
#
# Anchor on start-of-line or whitespace before `go`, not on "some character then
# whitespace": the latter cannot see an unindented `go install` at column 1,
# which is exactly the shape this guard exists to catch. Requiring whitespace
# (rather than any non-`#` character) also keeps `cargo install` from matching.
#
# Comment lines are excluded so prose mentioning `go install` does not trip it.
mapfile -t install_lines < <(
    grep -nE '(^|[[:space:]])go install ' "${SETUP_TOOLS}" \
        | grep -vE '^[0-9]+:[[:space:]]*#' || true
)

# A floor, so removing every `go install` cannot make this pass vacuously.
# Three remain: apidiff, addlicense, and go-licenses, none of which publishes a
# binary release (see .github/actions/install-go-licenses for that contract).
readonly MIN_INSTALLS=3
if [[ "${#install_lines[@]}" -lt "${MIN_INSTALLS}" ]]; then
    echo "FAIL: found ${#install_lines[@]} 'go install' lines, expected at least ${MIN_INSTALLS};" >&2
    echo "      if an install was intentionally removed, lower MIN_INSTALLS with it" >&2
    exit 1
fi

failed=0
for line in "${install_lines[@]}"; do
    if [[ "${line}" != *"GOFLAGS= go install "* ]]; then
        echo "FAIL: 'go install' does not clear GOFLAGS: ${line}" >&2
        failed=1
    fi
done
[[ "${failed}" -eq 0 ]] || exit 1

echo "All ${#install_lines[@]} versioned Go tool installs clear GOFLAGS"
