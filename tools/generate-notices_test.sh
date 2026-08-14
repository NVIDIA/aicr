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
# Anchor to the executable line, not the file. The action explains the GOFLAGS
# contract in a comment that contains the same literal, so an unanchored
# whole-file grep is satisfied by the prose and stays green even when the real
# `run:` step drops `GOFLAGS=` - the exact regression this guard exists for.
if ! grep -Eq "^[[:space:]]*GOFLAGS= go install[[:space:]]+['\"]?github\.com/google/go-licenses" \
    "${INSTALL_ACTION}"; then
    echo "FAIL: the go-licenses install action does not clear GOFLAGS" >&2
    exit 1
fi
# The assertion above proves at least one correct install exists, not that every
# install is correct. A second install line appended to this action without
# GOFLAGS= would leave it green. The .github/ scan below excludes this
# canonical action directory, so the per-line check here is what catches a
# second unguarded install inside install-go-licenses itself.
#
# Every go-licenses install line must therefore MATCH IN FULL: a GOFLAGS-cleared
# install and nothing else, bar a trailing comment. Excluding lines that merely
# contain "GOFLAGS= go install" somewhere is not enough - `GOFLAGS= go install
# <other>; go install <go-licenses>` contains it while leaving the go-licenses
# install unguarded. `[^;&|]*$` rejects that by banning command separators, so
# each line carries exactly one install and the prefix provably applies to it.
install_violations=0
while IFS= read -r install_line; do
    if ! grep -Eq "^[[:space:]]*GOFLAGS= go install[[:space:]]+['\"]?github\.com/google/go-licenses[^;&|]*$" \
        <<<"${install_line}"; then
        echo "FAIL: ${INSTALL_ACTION}: go-licenses install is not GOFLAGS-cleared," >&2
        echo "or chains other commands onto the same line:" >&2
        echo "  ${install_line}" >&2
        install_violations=1
    fi
done < <(grep -E "^[[:space:]]*[^#]*go install[[:space:]]+['\"]?github\.com/google/go-licenses" \
    "${INSTALL_ACTION}")
if [[ ${install_violations} -ne 0 ]]; then
    exit 1
fi
# Reject every inline install, not just one that forgets GOFLAGS: an inline step
# also escapes the pinned version the composite action takes from
# .settings.yaml, so a workflow could silently run a different go-licenses. The
# leading pattern skips commented-out lines. The quote is optional and accepts
# either form because every install site in this repo quotes the module path: a
# pattern requiring bare whitespace matched only a form nobody writes, and one
# accepting just `"` still let a single-quoted install through.
#
# Scan all of .github/ except the canonical install-go-licenses action itself —
# that is where the real `GOFLAGS= go install` lives, so including it would flag
# the install this guard exists to steer people toward. Every other .github/
# consumer (workflows and composite actions) must use the shared action.
#
# Exclusion is path-aware (exact canonical directory), not --exclude-dir by
# basename: a same-named directory elsewhere under .github/ must still fail.
CANONICAL_INSTALL_DIR="${REPO_ROOT}/.github/actions/install-go-licenses"
found_inline=0
while IFS= read -r match; do
    [[ -z "${match}" ]] && continue
    path="${match%%:*}"
    case "${path}" in
        "${CANONICAL_INSTALL_DIR}/"*) continue ;;
    esac
    echo "${match}" >&2
    found_inline=1
done < <(grep -rEn "^[[:space:]]*[^#]*go install[[:space:]]+['\"]?github\.com/google/go-licenses" \
    "${REPO_ROOT}/.github/" || true)
if [[ ${found_inline} -ne 0 ]]; then
    echo "FAIL: a .github/ workflow or action installs go-licenses inline;" >&2
    echo "use the ./.github/actions/install-go-licenses composite action instead." >&2
    exit 1
fi

# Regression for the basename --exclude-dir trap: a same-named directory under
# .github/ that is NOT the canonical action must still be flagged.
EXCLUDE_REGRESSION_TMP="$(mktemp -d "${TMPDIR:-/tmp}/aicr-notices-exclude.XXXXXX")"
mkdir -p \
    "${EXCLUDE_REGRESSION_TMP}/.github/actions/install-go-licenses" \
    "${EXCLUDE_REGRESSION_TMP}/.github/workflows/install-go-licenses"
printf '%s\n' "GOFLAGS= go install 'github.com/google/go-licenses/v2@v0.0.0'" \
    > "${EXCLUDE_REGRESSION_TMP}/.github/actions/install-go-licenses/action.yml"
expected_sneaky="${EXCLUDE_REGRESSION_TMP}/.github/workflows/install-go-licenses/sneaky.yml"
printf '%s\n' "GOFLAGS= go install 'github.com/google/go-licenses/v2@v0.0.0'" \
    > "${expected_sneaky}"
regression_hits=0
regression_match_path=""
while IFS= read -r match; do
    [[ -z "${match}" ]] && continue
    path="${match%%:*}"
    case "${path}" in
        "${EXCLUDE_REGRESSION_TMP}/.github/actions/install-go-licenses/"*) continue ;;
    esac
    regression_match_path="${path}"
    regression_hits=$((regression_hits + 1))
done < <(grep -rEn "^[[:space:]]*[^#]*go install[[:space:]]+['\"]?github\.com/google/go-licenses" \
    "${EXCLUDE_REGRESSION_TMP}/.github/" || true)
rm -rf "${EXCLUDE_REGRESSION_TMP}"
# hits==1 alone is ambiguous: a filter that drops sneaky.yml and keeps the
# canonical action.yml also yields one hit. Require the surviving path.
if [[ ${regression_hits} -ne 1 ||
      "${regression_match_path}" != "${expected_sneaky}" ]]; then
    echo "FAIL: path-aware install-go-licenses exclusion missed a same-named" >&2
    echo "directory under .github/ (hits=${regression_hits}, want 1;" >&2
    echo "path=${regression_match_path}, want ${expected_sneaky})." >&2
    exit 1
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

# ---------------------------------------------------------------------------
# license-check cases. These need only the stub go-licenses and the real `go`,
# never the generator, so they run BEFORE the yq gate below: a missing yq must
# not silently disable the half of the suite that does not depend on it.
# ---------------------------------------------------------------------------

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

# Negative control for the recipe's GOROOT validation branch. Without it the
# positive case above passes just as happily when that branch is deleted, so the
# fail-closed half of the gate would be unprotected.
#
# The stub must make `go env GOROOT` PRINT an unusable path while EXITING 0.
# Exporting GOROOT=/nonexistent does not work: the real `go env` rejects it and
# exits 2, so `set -e` aborts the recipe before the branch is ever reached and
# the case would pass against a recipe with no validation at all.
REAL_GO="$(command -v go)"
cat > "${TEST_TMP}/bin/go" <<EOF
#!/usr/bin/env bash
# Only the GOROOT lookup is faked; everything else defers to the real toolchain.
if [[ "\${1:-}" == "env" && "\${2:-}" == "GOROOT" && \$# -eq 2 ]]; then
    printf '%s\n' "${TEST_TMP}/not-a-goroot"
    exit 0
fi
exec "${REAL_GO}" "\$@"
EOF
chmod +x "${TEST_TMP}/bin/go"

set +e
unusable_output="$(
    cd "${REPO_ROOT}" && env -u GOROOT PATH="${TEST_TMP}/bin:${PATH}" \
        make license-check 2>&1
)"
unusable_rc=$?
set -e
find "${TEST_TMP}/bin/go" -maxdepth 0 -delete

if [[ ${unusable_rc} -eq 0 ]]; then
    echo "FAIL: 'make license-check' exited 0 with an unusable GOROOT." >&2
    echo "The recipe must fail closed rather than run go-licenses against a" >&2
    echo "GOROOT that makes every package look like standard library." >&2
    echo "${unusable_output}" >&2
    exit 1
fi

if ! grep -q "could not resolve a usable GOROOT" <<<"${unusable_output}"; then
    echo "FAIL: 'make license-check' failed for the wrong reason:" >&2
    echo "${unusable_output}" >&2
    exit 1
fi
echo "make license-check fails closed when GOROOT is unusable"

# ---------------------------------------------------------------------------
# Generator case. This one runs the real generator, which needs yq to verify its
# release-target matrix. Without it the script exits on that check instead of the
# guard under test, which would read as a defect rather than a missing tool.
# Skip with one clear message locally; fail in CI, where the guard must actually
# be exercised. The variant check mirrors setup-tools: Python yq wraps jq.
# ---------------------------------------------------------------------------
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

: > "${GO_LICENSES_PROBE}"
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
