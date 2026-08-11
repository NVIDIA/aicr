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

# Verify the notices generator fails closed when go-licenses collects nothing.
#
# go-licenses decides what is standard library by matching a package's source
# path against go/build's GOROOT. A binary linked with -trimpath carries no
# GOROOT, the prefix collapses to "/", every absolute path matches, and the tool
# reports the entire dependency graph as stdlib: it writes no files and exits 0.
# That used to surface as an unrelated "cp: .../linux_amd64/.: No such file or
# directory" from the merge step, and the same silence makes 'go-licenses check'
# pass without inspecting anything.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
GENERATE_NOTICES="${SCRIPT_DIR}/generate-notices"

bash -n "${GENERATE_NOTICES}"

# CI installs go-licenses through a shared composite action. An install that
# inherits -trimpath produces a binary with no GOROOT at all, which is the same
# defect one indirection further out, so pin the contract here too.
INSTALL_ACTION="${REPO_ROOT}/.github/actions/install-go-licenses/action.yml"
if [[ ! -f "${INSTALL_ACTION}" ]]; then
    echo "FAIL: ${INSTALL_ACTION} is missing" >&2
    exit 1
fi
if ! grep -q 'GOFLAGS= go install' "${INSTALL_ACTION}"; then
    echo "FAIL: the go-licenses install action does not clear GOFLAGS" >&2
    exit 1
fi
# Reject every inline install, not just one that forgets GOFLAGS: an inline step
# also escapes the pinned version the composite action takes from
# .settings.yaml, so a workflow could silently run a different go-licenses. The
# leading pattern skips commented-out lines. Only workflows are scanned - the
# action itself is where the real `GOFLAGS= go install` lives.
if grep -rEn '^[[:space:]]*[^#]*go install[[:space:]]+github\.com/google/go-licenses' \
    "${REPO_ROOT}/.github/workflows/"; then
    echo "FAIL: a workflow installs go-licenses inline;" >&2
    echo "use the ./.github/actions/install-go-licenses composite action instead." >&2
    exit 1
fi

# The behavioral case below runs the real generator, which needs yq to verify
# its release-target matrix. Without it the script exits on that check instead of
# the guard under test, which would read as a defect rather than a missing tool.
# Skip with one clear message locally; fail in CI, where the guard must actually
# be exercised. The variant check mirrors setup-tools: Python yq wraps jq.
yq_unavailable=""
if ! command -v yq >/dev/null 2>&1; then
    yq_unavailable="yq is not installed"
elif ! yq --version 2>/dev/null | grep -q "mikefarah/yq"; then
    yq_unavailable="yq at $(command -v yq) is not mikefarah/yq (Go-based)"
fi
if [[ -n "${yq_unavailable}" ]]; then
    if [[ -n "${CI:-}" ]]; then
        echo "FAIL: ${yq_unavailable}; the notices guard cannot be verified in CI" >&2
        exit 1
    fi
    echo "SKIP: ${yq_unavailable}; run 'make tools-setup' to install the pinned version"
    exit 0
fi

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/aicr-notices-test.XXXXXX")"
trap 'rm -rf "${TEST_TMP}"' EXIT

# Stand in for a go-licenses that classifies every package as stdlib: succeeds,
# emits nothing, creates no save_path. It also records the GOROOT each
# invocation actually received, which is what the callers must supply.
GO_LICENSES_PROBE="${TEST_TMP}/goroot-seen"
export GO_LICENSES_PROBE
mkdir -p "${TEST_TMP}/bin"
cat > "${TEST_TMP}/bin/go-licenses" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "${GOROOT-}" >> "${GO_LICENSES_PROBE}"
exit 0
EOF
chmod +x "${TEST_TMP}/bin/go-licenses"

# Assert go-licenses ran with a usable GOROOT in its ENVIRONMENT. Checking the
# call site's text is not enough: a bare `GOROOT=...` assignment satisfies a
# grep but never reaches the child process, leaving the exact silent-stdlib
# failure this suite exists to catch. Every caller is run with GOROOT unset so
# the only way the stub can observe one is if the caller exported it.
assert_goroot_exported() {
    local context="$1" seen
    if [[ ! -s "${GO_LICENSES_PROBE}" ]]; then
        echo "FAIL: ${context}: go-licenses was never invoked" >&2
        exit 1
    fi
    while IFS= read -r seen; do
        if [[ -z "${seen}" ]]; then
            echo "FAIL: ${context}: go-licenses ran with no GOROOT in its environment." >&2
            echo "Assigning GOROOT is not enough - it must be exported, or go-licenses" >&2
            echo "falls back to its (possibly empty) baked-in GOROOT and reports every" >&2
            echo "package as standard library while still exiting 0." >&2
            exit 1
        fi
        if [[ ! -d "${seen}" ]]; then
            echo "FAIL: ${context}: go-licenses received GOROOT='${seen}', not a directory" >&2
            exit 1
        fi
    done < "${GO_LICENSES_PROBE}"
}

set +e
stderr_output="$(
    cd "${REPO_ROOT}" && env -u GOROOT PATH="${TEST_TMP}/bin:${PATH}" \
        OUTPUT="${TEST_TMP}/NOTICES.md" \
        LICENSES_DIR="${TEST_TMP}/licenses-cache" \
        bash "${GENERATE_NOTICES}" 2>&1 >/dev/null
)"
rc=$?
set -e

if [[ ${rc} -eq 0 ]]; then
    echo "FAIL: generate-notices exited 0 after go-licenses collected nothing" >&2
    exit 1
fi

if ! grep -q "collected no licenses" <<<"${stderr_output}"; then
    echo "FAIL: generate-notices did not report the empty collection as the cause." >&2
    echo "stderr was:" >&2
    echo "${stderr_output}" >&2
    exit 1
fi

# An empty collection must never reach the notices file.
if [[ -e "${TEST_TMP}/NOTICES.md" ]]; then
    echo "FAIL: generate-notices wrote a notices file from an empty collection" >&2
    exit 1
fi

assert_goroot_exported "generate-notices"
echo "generate-notices fails closed on an empty go-licenses collection"

# The license-check gate shares the defect: with no usable GOROOT, go-licenses
# inspects nothing and exits 0, so the gate passes vacuously. Exercise the real
# target against the stub rather than reading the Makefile.
: > "${GO_LICENSES_PROBE}"
set +e
license_output="$(
    cd "${REPO_ROOT}" && env -u GOROOT PATH="${TEST_TMP}/bin:${PATH}" \
        make license-check 2>&1
)"
license_rc=$?
set -e

if [[ ${license_rc} -ne 0 ]]; then
    echo "FAIL: 'make license-check' failed against a stub go-licenses:" >&2
    echo "${license_output}" >&2
    exit 1
fi

assert_goroot_exported "make license-check"
echo "make license-check exports a usable GOROOT to go-licenses"
