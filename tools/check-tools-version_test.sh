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

# Hermetic tests for strict tool-version pins. Go binary metadata, command
# version output, and .settings.yaml reads are stubbed; no network is needed.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK_TOOLS="${SCRIPT_DIR}/check-tools"
# shellcheck source=tools/common
. "${SCRIPT_DIR}/common"
# Unreadable metadata and mismatches are part of the test matrix, so capture
# their exit codes instead of inheriting common's errexit setting.
set +e

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT

APIDIFF_PINNED_VERSION="v0.0.0-20260727155853-b88d891fe743"
APIDIFF_MISMATCH_VERSION="v0.0.0-20260701000000-deadbeefdead"
ADDLICENSE_PINNED_VERSION="v1.2.0"
ADDLICENSE_MISMATCH_VERSION="v1.1.1"
GO_LICENSES_PINNED_VERSION="v2.0.1"
GO_LICENSES_MISMATCH_VERSION="v1.6.0"
ORAS_PINNED_VERSION="1.3.3"
ORAS_MISMATCH_VERSION="1.3.4"
DOCKER_VERSION="27.3.1"
export APIDIFF_PINNED_VERSION APIDIFF_MISMATCH_VERSION
export ADDLICENSE_PINNED_VERSION ADDLICENSE_MISMATCH_VERSION
export GO_LICENSES_PINNED_VERSION GO_LICENSES_MISMATCH_VERSION
export ORAS_PINNED_VERSION ORAS_MISMATCH_VERSION DOCKER_VERSION

cat >"${STUB_DIR}/go" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" != "version" || "${2:-}" != "-m" ]]; then
    exit 2
fi

binary_name="${3##*/}"
case "${binary_name}" in
    apidiff)
        command_path="golang.org/x/exp/cmd/apidiff"
        module_path="golang.org/x/exp"
        pinned_version="${APIDIFF_PINNED_VERSION}"
        mismatch_version="${APIDIFF_MISMATCH_VERSION}"
        ;;
    addlicense)
        command_path="github.com/google/addlicense"
        module_path="github.com/google/addlicense"
        pinned_version="${ADDLICENSE_PINNED_VERSION}"
        mismatch_version="${ADDLICENSE_MISMATCH_VERSION}"
        ;;
    go-licenses)
        command_path="github.com/google/go-licenses/v2"
        module_path="github.com/google/go-licenses/v2"
        pinned_version="${GO_LICENSES_PINNED_VERSION}"
        mismatch_version="${GO_LICENSES_MISMATCH_VERSION}"
        ;;
    *)
        exit 2
        ;;
esac

version="${pinned_version}"
if [[ "${TOOL_TARGET:-}" == "${binary_name}" ]]; then
    case "${TOOL_MODE:-correct}" in
        correct)
            ;;
        mismatch)
            version="${mismatch_version}"
            ;;
        unreadable)
            exit 1
            ;;
        *)
            exit 2
            ;;
    esac
fi

printf '%s: go1.26.0\n' "$3"
printf '\tpath\t%s\n' "${command_path}"
printf '\tmod\t%s\t%s\th1:stub\n' "${module_path}" "${version}"
STUB

cat >"${STUB_DIR}/yq" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
    'keys | .[]')
        printf 'linting\nsecurity_tools\n'
        ;;
    '.linting | keys | .[]')
        printf 'apidiff\naddlicense\ngo_licenses\n'
        ;;
    '.security_tools | keys | .[]')
        echo oras
        ;;
    '.linting.apidiff')
        echo "${APIDIFF_PINNED_VERSION}"
        ;;
    '.linting.addlicense')
        echo "${ADDLICENSE_PINNED_VERSION}"
        ;;
    '.linting.go_licenses')
        echo "${GO_LICENSES_PINNED_VERSION}"
        ;;
    '.security_tools.oras')
        echo "${ORAS_PINNED_VERSION}"
        ;;
    *)
        exit 2
        ;;
esac
STUB

for tool_name in apidiff addlicense go-licenses; do
    cat >"${STUB_DIR}/${tool_name}" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
done

cat >"${STUB_DIR}/oras" <<'STUB'
#!/usr/bin/env bash
version="${ORAS_PINNED_VERSION}"
if [[ "${TOOL_TARGET:-}" == "oras" ]]; then
    case "${TOOL_MODE:-correct}" in
        correct)
            ;;
        mismatch)
            version="${ORAS_MISMATCH_VERSION}"
            ;;
        unreadable)
            exit 1
            ;;
        *)
            exit 2
            ;;
    esac
fi
printf 'Version: %s+Homebrew\n' "${version}"
STUB

cat >"${STUB_DIR}/docker" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" != "version" ]]; then
    exit 2
fi
if [[ "${TOOL_MODE:-}" == "not-running" ]]; then
    exit 1
fi
printf '%s\n' "${DOCKER_VERSION}"
STUB

chmod +x "${STUB_DIR}/go" "${STUB_DIR}/yq" \
    "${STUB_DIR}/apidiff" "${STUB_DIR}/addlicense" \
    "${STUB_DIR}/go-licenses" "${STUB_DIR}/oras" "${STUB_DIR}/docker"

# Keep missing-tool cases hermetic: after a stub is moved aside, PATH must not
# fall through to a copy of that tool preinstalled on the host or CI runner.
UTILITY_DIR="${STUB_DIR}/utilities"
mkdir -p "${UTILITY_DIR}"
for utility in awk bash cut dirname grep head mv rm sed tr; do
    ln -s "$(command -v "${utility}")" "${UTILITY_DIR}/${utility}"
done
export PATH="${STUB_DIR}:${UTILITY_DIR}"

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1 — $2"; fails=$((fails + 1)); }

check_helper() {
    local name="$1"
    local mode="$2"
    local want_rc="$3"
    local want_output="$4"
    local output
    local rc

    output=$(TOOL_TARGET=apidiff TOOL_MODE="${mode}" \
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
    local tool_name="$2"
    local mode="$3"
    local want_rc="$4"
    local want_row="$5"
    local executable="${STUB_DIR}/${tool_name}"
    local unavailable="${executable}.unavailable"
    local output
    local rc
    local row

    if [[ "${mode}" == "missing" ]]; then
        mv "${executable}" "${unavailable}"
    fi
    output=$(TOOL_TARGET="${tool_name}" TOOL_MODE="${mode}" \
        bash "${CHECK_TOOLS}" 2>&1)
    rc=$?
    if [[ "${mode}" == "missing" ]]; then
        mv "${unavailable}" "${executable}"
    fi

    row=$(printf '%s\n' "${output}" | awk -v tool="${tool_name}" \
        '$1 == tool {
            if ($3 == "not" && $4 == "running") {
                print $2 "|" $3 " " $4 "|" $5
            } else {
                print $2 "|" $3 "|" $4
            }
        }')
    if [[ "${rc}" == "${want_rc}" && "${row}" == "${want_row}" ]]; then
        pass "${name}"
    else
        fail "${name}" "want rc=${want_rc} row='${want_row}', got rc=${rc} row='${row}'"
    fi
}

check_helper "extracts-exact-module-version" correct 0 \
    "${APIDIFF_PINNED_VERSION}"
check_helper "extracts-mismatched-module-version" mismatch 0 \
    "${APIDIFF_MISMATCH_VERSION}"
check_helper "rejects-unreadable-build-metadata" unreadable 1 ""

check_tools_row "accepts-exact-apidiff" apidiff correct 0 \
    "${APIDIFF_PINNED_VERSION}|${APIDIFF_PINNED_VERSION}|✓"
check_tools_row "rejects-mismatched-apidiff" apidiff mismatch 1 \
    "${APIDIFF_PINNED_VERSION}|${APIDIFF_MISMATCH_VERSION}|⚠"
check_tools_row "rejects-unreadable-apidiff" apidiff unreadable 1 \
    "${APIDIFF_PINNED_VERSION}|unknown|⚠"
check_tools_row "rejects-missing-apidiff" apidiff missing 1 \
    "${APIDIFF_PINNED_VERSION}|-|✗"

check_tools_row "accepts-exact-addlicense" addlicense correct 0 \
    "${ADDLICENSE_PINNED_VERSION}|${ADDLICENSE_PINNED_VERSION}|✓"
check_tools_row "rejects-mismatched-addlicense" addlicense mismatch 1 \
    "${ADDLICENSE_PINNED_VERSION}|${ADDLICENSE_MISMATCH_VERSION}|⚠"
check_tools_row "rejects-unreadable-addlicense" addlicense unreadable 1 \
    "${ADDLICENSE_PINNED_VERSION}|unknown|⚠"
check_tools_row "rejects-missing-addlicense" addlicense missing 1 \
    "${ADDLICENSE_PINNED_VERSION}|-|✗"

check_tools_row "accepts-exact-go-licenses" go-licenses correct 0 \
    "${GO_LICENSES_PINNED_VERSION}|${GO_LICENSES_PINNED_VERSION}|✓"
check_tools_row "rejects-mismatched-go-licenses" go-licenses mismatch 1 \
    "${GO_LICENSES_PINNED_VERSION}|${GO_LICENSES_MISMATCH_VERSION}|⚠"
check_tools_row "rejects-unreadable-go-licenses" go-licenses unreadable 1 \
    "${GO_LICENSES_PINNED_VERSION}|unknown|⚠"
check_tools_row "rejects-missing-go-licenses" go-licenses missing 1 \
    "${GO_LICENSES_PINNED_VERSION}|-|✗"

check_tools_row "accepts-exact-oras-and-normalizes-build-metadata" oras correct 0 \
    "${ORAS_PINNED_VERSION}|${ORAS_PINNED_VERSION}|✓"
check_tools_row "rejects-mismatched-oras" oras mismatch 1 \
    "${ORAS_PINNED_VERSION}|${ORAS_MISMATCH_VERSION}|⚠"
check_tools_row "rejects-unreadable-oras" oras unreadable 1 \
    "${ORAS_PINNED_VERSION}|unknown|⚠"
check_tools_row "rejects-missing-oras" oras missing 1 \
    "${ORAS_PINNED_VERSION}|-|✗"

check_tools_row "reports-any-running-docker-version" docker correct 0 \
    "any|${DOCKER_VERSION}|✓"
check_tools_row "warns-when-docker-is-not-running" docker not-running 0 \
    "any|not running|⚠"
check_tools_row "reports-missing-docker" docker missing 0 \
    "any|-|✗"

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All strict tool-version tests passed"
