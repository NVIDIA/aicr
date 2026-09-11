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

# Verify no Go tool is installed in a way that depends on the checksum database.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETUP_TOOLS="${SCRIPT_DIR}/setup-tools"

bash -n "${SETUP_TOOLS}"

# This guard used to assert that every `go install` cleared GOFLAGS. #2667
# removed the last one, so the contract is now stricter and the guard asserts
# the stricter thing: no `go install pkg@version` at all.
#
# `go install pkg@version` resolves outside the main module. Nothing it builds
# is covered by this repo's go.sum, so every transitive dependency is
# authenticated against sum.golang.org on each run, and an outage there fails
# the install -- which is what took down tests / E2E on the v0.21.1 release
# (#2664) and still reached the qualification gate afterwards (#2667). Building
# from the main module verifies against the committed go.sum instead and never
# consults the checksum database.
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

if [[ "${#install_lines[@]}" -ne 0 ]]; then
    echo "FAIL: found ${#install_lines[@]} 'go install' line(s) in setup-tools." >&2
    echo "      Each one reintroduces the sum.golang.org dependency #2667 removed." >&2
    echo "      Build from the main module with build_module_tool, or install a" >&2
    echo "      checksum-pinned binary release the way the oasdiff block does." >&2
    printf '        %s\n' "${install_lines[@]}" >&2
    exit 1
fi

# A floor, so deleting the installs outright cannot make the check above pass
# vacuously. Two tools are built from the module: apidiff and go-licenses,
# neither of which publishes a binary release. Tools that do publish one
# (addlicense, oasdiff, ctlptl, ...) are downloaded and checksum-verified
# instead, and are deliberately not counted here.
mapfile -t module_builds < <(
    grep -nE '(^|[[:space:]])build_module_tool ' "${SETUP_TOOLS}" \
        | grep -vE '^[0-9]+:[[:space:]]*#' | grep -v 'build_module_tool()' || true
)

readonly MIN_MODULE_BUILDS=2
if [[ "${#module_builds[@]}" -lt "${MIN_MODULE_BUILDS}" ]]; then
    echo "FAIL: found ${#module_builds[@]} 'build_module_tool' call(s), expected at least ${MIN_MODULE_BUILDS};" >&2
    echo "      if a tool was intentionally moved to a binary release, lower MIN_MODULE_BUILDS with it" >&2
    exit 1
fi

echo "No checksum-database-dependent installs; ${#module_builds[@]} tools built from the main module"
