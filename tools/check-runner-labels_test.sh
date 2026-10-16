#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

# Cases for tools/check-runner-labels.
#
# Each case builds a fixture tree and points the guard at it through the
# optional root argument, so the real .github/ never influences the result.
#
# The cases that matter are the REJECTIONS. A guard that never matches prints
# OK for every tree, which is indistinguishable from "no floating labels", so
# every form ubuntu-latest takes in a workflow is pinned with the exit code and
# the exact lines it must produce.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TOOL="${ROOT}/tools/check-runner-labels"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/check-runner-labels-test.XXXXXX")
trap 'rm -rf "${TMP}"' EXIT
FIXTURE="${TMP}/root"

OK_LINE='OK: no ubuntu-latest runner labels under .github/'

failures=0

# fixture <path> <content> [<path> <content> ...]
# Replaces the fixture tree with exactly the given files.
fixture() {
    rm -rf "${FIXTURE}"
    mkdir -p "${FIXTURE}"
    while [[ $# -gt 0 ]]; do
        mkdir -p "$(dirname "${FIXTURE}/$1")"
        printf '%s\n' "$2" > "${FIXTURE}/$1"
        shift 2
    done
}

# has_line <output> <line>: the output contains <line> as a whole line.
has_line() {
    printf '%s\n' "$1" | grep -qxF -- "$2"
}

# expect_ok <name>: the guard exits 0 on the current fixture and prints the
# exact OK line.
expect_ok() {
    local name="$1" output rc=0
    output=$("${TOOL}" "${FIXTURE}" 2>&1) || rc=$?
    if [[ "${rc}" -ne 0 ]]; then
        echo "FAIL ${name}: want exit 0, got ${rc}: ${output}" >&2
        failures=$((failures + 1))
        return
    fi
    if ! has_line "${output}" "${OK_LINE}"; then
        echo "FAIL ${name}: want line '${OK_LINE}', got: ${output}" >&2
        failures=$((failures + 1))
        return
    fi
    echo "ok   ${name}"
}

# expect_fail <name> <rc> <line> [<line> ...]: the guard exits <rc> on the
# current fixture and prints every <line> as a whole line.
expect_fail() {
    local name="$1" want_rc="$2" output rc=0 line
    shift 2
    output=$("${TOOL}" "${FIXTURE}" 2>&1) || rc=$?
    if [[ "${rc}" -ne "${want_rc}" ]]; then
        echo "FAIL ${name}: want exit ${want_rc}, got ${rc}: ${output}" >&2
        failures=$((failures + 1))
        return
    fi
    for line in "$@"; do
        if ! has_line "${output}" "${line}"; then
            echo "FAIL ${name}: want line '${line}', got: ${output}" >&2
            failures=$((failures + 1))
            return
        fi
    done
    echo "ok   ${name}"
}

err_line() {
    echo "ERROR: $1 reference(s) to ubuntu-latest under .github/. Pin GitHub-hosted runners to ubuntu-24.04 (see #3275)."
}

# --- accepted -------------------------------------------------------------

fixture .github/workflows/a.yaml 'jobs:
  build:
    runs-on: ubuntu-24.04
    strategy:
      matrix:
        runner: ubuntu-24.04-arm'
expect_ok "pinned hosted labels"

# A "-latest" wildcard would reject every self-hosted GPU label.
fixture .github/workflows/gpu.yaml 'jobs:
  gpu:
    runs-on: linux-amd64-gpu-h100-latest-1'
expect_ok "self-hosted label containing -latest"

# The guard is scoped to .github/; prose elsewhere may name the label.
fixture .github/workflows/a.yaml 'runs-on: ubuntu-24.04' \
    docs/x.md 'GitHub moved ubuntu-latest to 26.04.'
expect_ok "ubuntu-latest outside .github is ignored"

# --- rejected -------------------------------------------------------------

fixture .github/workflows/a.yaml 'jobs:
  build:
    runs-on: ubuntu-latest'
expect_fail "runs-on ubuntu-latest" 1 \
    '.github/workflows/a.yaml:3:    runs-on: ubuntu-latest' \
    "$(err_line 1)"

fixture .github/workflows/m.yaml 'jobs:
  build:
    strategy:
      matrix:
        runner: ubuntu-latest'
expect_fail "matrix runner value" 1 \
    '.github/workflows/m.yaml:5:        runner: ubuntu-latest' \
    "$(err_line 1)"

fixture .github/workflows/l.yaml 'jobs:
  a:
    runs-on: [ubuntu-latest]
  b:
    runs-on: "ubuntu-latest"'
expect_fail "list and quoted forms" 1 \
    '.github/workflows/l.yaml:3:    runs-on: [ubuntu-latest]' \
    '.github/workflows/l.yaml:5:    runs-on: "ubuntu-latest"' \
    "$(err_line 2)"

# shellcheck disable=SC2016 # the ${{ }} is a literal GitHub expression
fixture .github/actions/x/action.yml 'inputs:
  runner:
    default: ${{ inputs.runner || '"'"'ubuntu-latest'"'"' }}'
expect_fail "expression default in a composite action" 1 \
    ".github/actions/x/action.yml:3:    default: \${{ inputs.runner || 'ubuntu-latest' }}" \
    "$(err_line 1)"

fixture .github/workflows/c.yaml '# runs on ubuntu-latest
jobs: {}'
expect_fail "comment mentioning the label" 1 \
    '.github/workflows/c.yaml:1:# runs on ubuntu-latest' \
    "$(err_line 1)"

fixture .github/workflows/one.yaml 'runs-on: ubuntu-latest' \
    .github/workflows/two.yaml 'x: 1
runs-on: ubuntu-latest'
expect_fail "two hits across two files" 1 \
    '.github/workflows/one.yaml:1:runs-on: ubuntu-latest' \
    '.github/workflows/two.yaml:2:runs-on: ubuntu-latest' \
    "$(err_line 2)"

# --- argument handling ----------------------------------------------------

# A guard that prints OK when there is nothing to scan passes a broken root.
fixture docs/x.md 'no workflows here'
expect_fail "missing .github directory" 2 \
    "ERROR: ${FIXTURE}/.github not found"

if [[ "${failures}" -ne 0 ]]; then
    echo "check-runner-labels: ${failures} case(s) failed" >&2
    exit 1
fi

echo "check-runner-labels rejects every ubuntu-latest form and accepts pinned labels"
