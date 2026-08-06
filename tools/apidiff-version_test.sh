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

# Hermetic tests for apidiff's module-version pin. The Go binary metadata and
# .settings.yaml reads are stubbed; no tool installation or network is needed.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK_TOOLS="${SCRIPT_DIR}/check-tools"
# shellcheck source=tools/common
. "${SCRIPT_DIR}/common"
# The helper's failure path is part of the test matrix, so capture it instead
# of inheriting common's errexit setting.
set +e

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT

PINNED_VERSION="v0.0.0-20260727155853-b88d891fe743"
MISMATCH_VERSION="v0.0.0-20260701000000-deadbeefdead"
export PINNED_VERSION MISMATCH_VERSION

cat >"${STUB_DIR}/go" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" != "version" || "${2:-}" != "-m" ]]; then
    exit 2
fi

case "${APIDIFF_SCENARIO:-}" in
    correct)
        version="${PINNED_VERSION}"
        ;;
    mismatch)
        version="${MISMATCH_VERSION}"
        ;;
    unreadable)
        exit 1
        ;;
    *)
        exit 2
        ;;
esac

printf '%s: go1.26.0\n' "$3"
printf '\tpath\tgolang.org/x/exp/cmd/apidiff\n'
printf '\tmod\tgolang.org/x/exp\t%s\th1:stub\n' "${version}"
STUB

cat >"${STUB_DIR}/yq" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
    'keys | .[]')
        echo linting
        ;;
    '.linting | keys | .[]')
        echo apidiff
        ;;
    '.linting.apidiff')
        echo "${PINNED_VERSION}"
        ;;
    *)
        exit 2
        ;;
esac
STUB

cat >"${STUB_DIR}/apidiff" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB

chmod +x "${STUB_DIR}/go" "${STUB_DIR}/yq" "${STUB_DIR}/apidiff"
export PATH="${STUB_DIR}:${PATH}"

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1 — $2"; fails=$((fails + 1)); }

check_helper() {
    local name="$1"
    local scenario="$2"
    local want_rc="$3"
    local want_output="$4"
    local output
    local rc

    output=$(APIDIFF_SCENARIO="${scenario}" \
        go_binary_module_version "${STUB_DIR}/apidiff" golang.org/x/exp)
    rc=$?
    if [[ "${rc}" == "${want_rc}" && "${output}" == "${want_output}" ]]; then
        pass "${name}"
    else
        fail "${name}" "want rc=${want_rc} output='${want_output}', got rc=${rc} output='${output}'"
    fi
}

check_tools_row() {
    local name="$1"
    local scenario="$2"
    local want_rc="$3"
    local want_row="$4"
    local check_path="${CHECK_TOOLS_PATH:-${PATH}}"
    local output
    local rc
    local row

    output=$(PATH="${check_path}" APIDIFF_SCENARIO="${scenario}" \
        bash "${CHECK_TOOLS}" 2>&1)
    rc=$?
    row=$(printf '%s\n' "${output}" | awk '$1 == "apidiff" { print $2 "|" $3 "|" $4 }')
    if [[ "${rc}" == "${want_rc}" && "${row}" == "${want_row}" ]]; then
        pass "${name}"
    else
        fail "${name}" "want rc=${want_rc} row='${want_row}', got rc=${rc} row='${row}'"
    fi
}

check_helper "extracts-exact-module-version" correct 0 "${PINNED_VERSION}"
check_helper "extracts-mismatched-module-version" mismatch 0 "${MISMATCH_VERSION}"
check_helper "rejects-unreadable-build-metadata" unreadable 1 ""

check_tools_row "check-tools-accepts-exact-version" correct 0 \
    "${PINNED_VERSION}|${PINNED_VERSION}|✓"
check_tools_row "check-tools-rejects-mismatch" mismatch 1 \
    "${PINNED_VERSION}|${MISMATCH_VERSION}|⚠"
check_tools_row "check-tools-rejects-unreadable-metadata" unreadable 1 \
    "${PINNED_VERSION}|unknown|⚠"

mv "${STUB_DIR}/apidiff" "${STUB_DIR}/apidiff.unavailable"
CHECK_TOOLS_PATH="${STUB_DIR}:/usr/bin:/bin" \
    check_tools_row "check-tools-rejects-missing-binary" missing 1 \
    "${PINNED_VERSION}|-|✗"

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All apidiff version-pin tests passed"
