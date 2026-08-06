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

# Decision-flow tests for tools/api-diff. Git, Go, and apidiff are stubbed so no
# worktree is created and no network access is needed. yq is intentionally not
# stubbed: these tests exercise the real structural parser supplied by the
# repository's pinned toolchain, so mikefarah/yq is the one required tool.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
API_DIFF="${SCRIPT_DIR}/api-diff"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Without yq every assertion below fails inside has_tools, which reads as dozens
# of unrelated defects rather than one missing tool. Skip with a single clear
# message locally; fail in CI, where the gate must actually be exercised.
# The variant check mirrors setup-tools: Python yq wraps jq and emits
# JSON-quoted strings, which would corrupt the comparisons rather than error.
yq_unavailable=""
if ! command -v yq >/dev/null 2>&1; then
    yq_unavailable="yq is not installed"
elif ! yq --version 2>/dev/null | grep -q "mikefarah/yq"; then
    yq_unavailable="yq at $(command -v yq) is not mikefarah/yq (Go-based)"
fi
if [[ -n "${yq_unavailable}" ]]; then
    if [[ -n "${CI:-}" ]]; then
        echo "FAIL: ${yq_unavailable}; the API-diff gate cannot be verified in CI" >&2
        exit 1
    fi
    echo "SKIP: ${yq_unavailable}; run 'make tools-setup' to install the pinned version"
    exit 0
fi

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT

cat >"${STUB_DIR}/go" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "env" && "$2" == "GOPATH" ]]; then
    if [[ "${GOPATH_SCENARIO:-correct}" == "failure" ]]; then
        echo "mock go env GOPATH failure" >&2
        exit 42
    fi
    dirname "${STUB_DIR}"
    exit 0
fi
if [[ "$1" == "version" && "$2" == "-m" ]]; then
    case "${APIDIFF_VERSION_SCENARIO:-correct}" in
        correct)
            printf '%s: go1.26.0\n\tmod\tgolang.org/x/exp\t%s\th1:stub\n' "$3" "${PINNED_APIDIFF_VERSION}"
            exit 0
            ;;
        mismatch)
            printf '%s: go1.26.0\n\tmod\tgolang.org/x/exp\tv0.0.0-mismatch\th1:stub\n' "$3"
            exit 0
            ;;
        unreadable)
            exit 1
            ;;
    esac
fi
exit 1
STUB

cat >"${STUB_DIR}/git" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "tag" ]]; then
    printf 'tag\n' >>"${GIT_LOG}"
    case "${GIT_SCENARIO}" in
        no-tags)
            ;;
        no-stable)
            printf '%s\n' v2.0.0-rc.1 v1.9.0-beta.1
            ;;
        failure)
            echo "mock git tag failure" >&2
            exit 42
            ;;
        *)
            echo "unexpected Git scenario: ${GIT_SCENARIO}" >&2
            exit 2
            ;;
    esac
    exit 0
fi
if [[ "$1" == "worktree" && "$2" == "add" ]]; then
    printf 'baseline=%s\n' "$5" >>"${GIT_LOG}"
    mkdir -p "$4"
    exit 0
fi
if [[ "$1" == "worktree" && "$2" == "prune" ]]; then
    printf 'prune\n' >>"${GIT_LOG}"
    exit 0
fi
if [[ "$1" == "worktree" && "$2" == "remove" ]]; then
    exit 0
fi
echo "unexpected git invocation: $*" >&2
exit 2
STUB

cat >"${STUB_DIR}/apidiff" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == "-w" ]]; then
    : >"$2"
    exit 0
fi
if [[ "${1:-}" == "-incompatible" ]]; then
    echo "unexpected redundant apidiff -incompatible invocation" >&2
    exit 99
fi

printf 'mode=report cwd=%s\n' "$(pwd)" >>"${APIDIFF_LOG}"
if [[ -n "${APIDIFF_REPORT:-}" ]]; then
    printf '%s' "${APIDIFF_REPORT}"
elif [[ -n "${APIDIFF_INCOMPATIBLE:-}" ]]; then
    printf 'Incompatible changes:\n%s' "${APIDIFF_INCOMPATIBLE}"
else
    exit 0
fi
STUB

chmod +x "${STUB_DIR}/go" "${STUB_DIR}/git" "${STUB_DIR}/apidiff"
export STUB_DIR
export PATH="${STUB_DIR}:${PATH}"
PINNED_APIDIFF_VERSION="$(yq eval -r '.linting.apidiff' "${SCRIPT_DIR}/../.settings.yaml")"
export PINNED_APIDIFF_VERSION

EMPTY_EXCEPTIONS="${STUB_DIR}/empty.yaml"
EXACT_EXCEPTIONS="${STUB_DIR}/exact.yaml"
MISMATCHED_EXCEPTIONS="${STUB_DIR}/mismatched.yaml"
MALFORMED_EXCEPTIONS="${STUB_DIR}/malformed.yaml"
WRONG_BASELINE_EXCEPTIONS="${STUB_DIR}/wrong-baseline.yaml"
MIXED_BASELINE_EXCEPTIONS="${STUB_DIR}/mixed-baseline.yaml"
NULL_FIELDS_EXCEPTIONS="${STUB_DIR}/null-fields.yaml"
DUPLICATE_BASELINE_EXCEPTIONS="${STUB_DIR}/duplicate-baseline.yaml"
REINDENTED_EXCEPTIONS="${STUB_DIR}/reindented.yaml"
MISSING_EXCEPTIONS="${STUB_DIR}/missing.yaml"

cat >"${EMPTY_EXCEPTIONS}" <<'YAML'
acknowledgements: []
YAML

cat >"${EXACT_EXCEPTIONS}" <<'YAML'
acknowledgements:
  - baseline: v9.8.7
    issue: '#1234'
    summary: Remove the legacy method.
    rationale: The replacement has been available for two releases.
    incompatible_changes:
      - 'Client.Legacy: removed'
YAML

cat >"${MISMATCHED_EXCEPTIONS}" <<'YAML'
acknowledgements:
  - baseline: v9.8.7
    issue: '#1234'
    summary: Remove the legacy method.
    rationale: The replacement has been available for two releases.
    incompatible_changes:
      - 'Client.Other: removed'
YAML

cat >"${MALFORMED_EXCEPTIONS}" <<'YAML'
acknowledgements:
  - baseline: v9.8.7
    issue: '#1234'
    summary: Remove the legacy method.
    incompatible_changes:
      - 'Client.Legacy: removed'
YAML

cat >"${WRONG_BASELINE_EXCEPTIONS}" <<'YAML'
acknowledgements:
  - baseline: v9.8.6
    issue: '#1234'
    summary: Remove the legacy method.
    rationale: The replacement has been available for two releases.
    incompatible_changes:
      - 'Client.Legacy: removed'
YAML

cat >"${MIXED_BASELINE_EXCEPTIONS}" <<'YAML'
acknowledgements:
  - baseline: v9.8.7
    issue: '#1234'
    summary: Remove the legacy method.
    rationale: The replacement has been available for two releases.
    incompatible_changes:
      - 'Client.Legacy: removed'
  - baseline: v8.0.0
    issue: '#1000'
    summary: Unrelated historical change.
    rationale: Records a change against a different release baseline.
    incompatible_changes:
      - 'Client.Historical: removed'
YAML

cat >"${NULL_FIELDS_EXCEPTIONS}" <<'YAML'
acknowledgements:
  - baseline: v9.8.7
    issue: null
    summary: Remove the legacy method.
    rationale: The replacement has been available for two releases.
    incompatible_changes:
      - 'Client.Legacy: removed'
YAML

cat >"${DUPLICATE_BASELINE_EXCEPTIONS}" <<'YAML'
acknowledgements:
  - baseline: v9.8.7
    issue: '#1234'
    summary: Remove the legacy method.
    rationale: The replacement has been available for two releases.
    incompatible_changes:
      - 'Client.Legacy: removed'
  - baseline: v9.8.7
    issue: '#1235'
    summary: Duplicate acknowledgement.
    rationale: This duplicate must be rejected as ambiguous.
    incompatible_changes:
      - 'Client.Legacy: removed'
YAML

cat >"${REINDENTED_EXCEPTIONS}" <<'YAML'
acknowledgements:
    - baseline: v9.8.7
      issue: '#1234'
      summary: Remove the legacy method.
      rationale: The replacement has been available for two releases.
      incompatible_changes:
        - 'Client.Legacy: removed'
YAML

OUT=""
RC=0
GIT_LOG=""
APIDIFF_LOG=""
run() {
    local scenario=$1
    local baseline=${2:-}
    local incompatible=${3:-}
    local exceptions_file=${4:-${EMPTY_EXCEPTIONS}}
    local version_scenario=${5:-correct}
    local caller_dir=${6:-${SCRIPT_DIR}}
    local gopath_scenario=${7:-correct}

    GIT_LOG="${STUB_DIR}/${scenario}.git.log"
    APIDIFF_LOG="${STUB_DIR}/${scenario}.apidiff.log"
    : >"${GIT_LOG}"
    : >"${APIDIFF_LOG}"
    export GIT_LOG APIDIFF_LOG
    if [[ -n "${baseline}" ]]; then
        OUT="$(cd "${caller_dir}" && GIT_SCENARIO="${scenario}" API_DIFF_BASELINE="${baseline}" \
            APIDIFF_INCOMPATIBLE="${incompatible}" \
            APIDIFF_REPORT="${APIDIFF_REPORT:-}" \
            API_DIFF_EXCEPTIONS_FILE="${exceptions_file}" \
            APIDIFF_VERSION_SCENARIO="${version_scenario}" \
            GOPATH_SCENARIO="${gopath_scenario}" "${API_DIFF}" 2>&1)"
    else
        OUT="$(cd "${caller_dir}" && GIT_SCENARIO="${scenario}" APIDIFF_INCOMPATIBLE="${incompatible}" \
            APIDIFF_REPORT="${APIDIFF_REPORT:-}" \
            API_DIFF_EXCEPTIONS_FILE="${exceptions_file}" \
            APIDIFF_VERSION_SCENARIO="${version_scenario}" \
            GOPATH_SCENARIO="${gopath_scenario}" "${API_DIFF}" 2>&1)"
    fi
    RC=$?
}

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1 — $2"; fails=$((fails + 1)); }
check_rc() {
    if [[ "${RC}" == "$2" ]]; then pass "$1"; else fail "$1" "want rc=$2 got rc=${RC}"; fi
}
check_contains() {
    if [[ "${OUT}" == *"$2"* ]]; then pass "$1"; else fail "$1" "expected to contain: $2"; fi
}
check_absent() {
    if [[ "${OUT}" != *"$2"* ]]; then pass "$1"; else fail "$1" "expected not to contain: $2"; fi
}
check_log_contains() {
    local contents
    contents="$(<"${GIT_LOG}")"
    if [[ "${contents}" == *"$2"* ]]; then pass "$1"; else fail "$1" "expected Git log to contain: $2"; fi
}
check_log_absent() {
    local contents
    contents="$(<"${GIT_LOG}")"
    if [[ "${contents}" != *"$2"* ]]; then pass "$1"; else fail "$1" "expected Git log not to contain: $2"; fi
}
check_apidiff_log_equals() {
    local contents
    contents="$(<"${APIDIFF_LOG}")"
    if [[ "${contents}" == "$2" ]]; then pass "$1"; else fail "$1" "expected apidiff log: $2 got: ${contents}"; fi
}
check_occurrences() {
    local occurrences
    occurrences="$(grep -F -o -- "$2" <<<"${OUT}" | wc -l | tr -d ' ')"
    if [[ "${occurrences}" == "$3" ]]; then pass "$1"; else fail "$1" "want $3 occurrence(s) of: $2 got ${occurrences}"; fi
}

run failure v9.8.7 "" "${EMPTY_EXCEPTIONS}" correct "${SCRIPT_DIR}" failure
check_rc "gopath-lookup-failure-fails" 1
check_contains "gopath-lookup-failure-is-diagnostic" "Could not determine GOPATH"
check_log_absent "gopath-lookup-failure-stops-before-git" "prune"

run failure v9.8.7 "" "${EMPTY_EXCEPTIONS}" mismatch
check_rc "mismatched-apidiff-version-fails" 17
check_contains "mismatched-apidiff-version-is-diagnostic" "apidiff version mismatch"
check_log_absent "mismatched-apidiff-version-stops-before-git" "prune"

run failure v9.8.7 "" "${EMPTY_EXCEPTIONS}" unreadable
check_rc "unverifiable-apidiff-version-fails" 17
check_contains "unverifiable-apidiff-version-is-diagnostic" "Could not verify the module version of apidiff"
check_log_absent "unverifiable-apidiff-version-stops-before-git" "prune"

run no-tags
check_rc "empty-tag-list-fails" 10
check_contains "empty-tag-list-is-diagnostic" "No stable release tag reachable from HEAD"

run no-stable
check_rc "prerelease-only-tag-list-fails" 10
check_contains "prerelease-only-tag-list-is-diagnostic" "No stable release tag reachable from HEAD"

run failure
check_rc "git-tag-failure-preserves-status" 42
check_contains "git-tag-failure-is-preserved" "mock git tag failure"
check_absent "git-tag-failure-is-not-no-match" "No stable release tag reachable from HEAD"

run failure v9.8.7
check_rc "explicit-baseline-succeeds" 0
check_log_absent "explicit-baseline-skips-tag-discovery" "tag"
check_log_contains "explicit-baseline-is-honored" "baseline=v9.8.7"
check_log_contains "stale-worktrees-are-pruned-before-add" $'prune\nbaseline=v9.8.7'

outside_module_dir="${STUB_DIR}/outside-module"
mkdir -p "${outside_module_dir}"
run failure v9.8.7 "" "${EMPTY_EXCEPTIONS}" correct "${outside_module_dir}"
check_rc "new-side-from-outside-module-succeeds" 0
check_apidiff_log_equals "new-side-loads-once-from-repository-root" \
    "mode=report cwd=${REPO_ROOT}"

run decision v9.8.7 "" "${MISSING_EXCEPTIONS}"
check_rc "missing-exceptions-file-fails" 11
check_contains "missing-exceptions-file-is-diagnostic" "API-diff exceptions file not found"

removed_method=$'- Client.Legacy: removed\n'
report_with_incompatible_and_compatible=$'Incompatible changes:\n- Client.Legacy: removed\nCompatible changes:\n- Client.New: added\n'

APIDIFF_REPORT="${report_with_incompatible_and_compatible}"
run decision v9.8.7 "${removed_method}" "${EMPTY_EXCEPTIONS}"
unset APIDIFF_REPORT
check_rc "unacknowledged-method-removal-fails" 16
check_contains "unacknowledged-method-removal-is-reported" "Client.Legacy: removed"
check_contains "plain-report-keeps-incompatible-header" "Incompatible changes:"
check_contains "plain-report-keeps-compatible-header" "Compatible changes:"
check_contains "plain-report-keeps-compatible-change" "Client.New: added"
check_occurrences "incompatible-change-is-not-printed-twice" "Client.Legacy: removed" 1
check_apidiff_log_equals "plain-report-loads-new-side-once" \
    "mode=report cwd=${REPO_ROOT}"
check_contains "unacknowledged-method-removal-is-diagnostic" "Add a baseline-scoped acknowledgement"

compatible_only_report=$'Compatible changes:\n- Client.New: added\n'
APIDIFF_REPORT="${compatible_only_report}"
run decision v9.8.7 "" "${EMPTY_EXCEPTIONS}"
unset APIDIFF_REPORT
check_rc "compatible-only-report-succeeds" 0
check_contains "compatible-only-change-is-reported" "Client.New: added"
check_contains "compatible-only-report-is-clean" "No incompatible"
check_apidiff_log_equals "compatible-only-report-loads-new-side-once" \
    "mode=report cwd=${REPO_ROOT}"

run decision v9.8.7 "${removed_method}" "${EXACT_EXCEPTIONS}"
check_rc "exact-baseline-acknowledgement-succeeds" 0
check_contains "exact-baseline-acknowledgement-is-diagnostic" "Allowing acknowledged incompatible API change(s) for v9.8.7: #1234"

run decision v9.8.7 "${removed_method}" "${MISMATCHED_EXCEPTIONS}"
check_rc "mismatched-change-list-fails" 16
check_contains "mismatched-change-list-is-diagnostic" "do not exactly match the acknowledgement"
check_contains "mismatched-change-list-reports-acknowledged-value" "Client.Other: removed"

run decision v9.8.7 "${removed_method}" "${MALFORMED_EXCEPTIONS}"
check_rc "malformed-acknowledgement-fails" 13
check_contains "malformed-acknowledgement-is-diagnostic" "Malformed API-diff acknowledgements"

run decision v9.8.7 "${removed_method}" "${WRONG_BASELINE_EXCEPTIONS}"
check_rc "wrong-baseline-fails" 14
check_contains "wrong-baseline-is-stale-diagnostic" "Stale API-diff acknowledgement baseline(s)"
check_contains "wrong-baseline-reports-active-baseline" "active baseline is v9.8.7"
check_contains "wrong-baseline-reports-stale-baseline" "v9.8.6"

run decision v9.8.7 "${removed_method}" "${MIXED_BASELINE_EXCEPTIONS}"
check_rc "mixed-current-and-stale-baselines-fail" 14
check_contains "mixed-baselines-are-stale-diagnostic" "Stale API-diff acknowledgement baseline(s)"
check_contains "mixed-baselines-report-stale-baseline" "v8.0.0"

run decision v9.8.7 "" "${WRONG_BASELINE_EXCEPTIONS}"
check_rc "stale-baseline-with-clean-diff-succeeds" 0
check_contains "clean-diff-reports-stale-baseline-as-prunable" "Prunable API-diff acknowledgement baseline(s)"
check_contains "clean-diff-names-prunable-baseline" "v9.8.6"
check_absent "clean-diff-does-not-fail-on-stale-baseline" "Stale API-diff acknowledgement baseline(s)"

run decision v9.8.7 "${removed_method}" "${NULL_FIELDS_EXCEPTIONS}"
check_rc "null-required-field-fails" 13
check_contains "null-required-field-is-diagnostic" "Malformed API-diff acknowledgements"

run decision v9.8.7 "${removed_method}" "${DUPLICATE_BASELINE_EXCEPTIONS}"
check_rc "duplicate-baseline-fails" 15
check_contains "duplicate-baseline-is-diagnostic" "Duplicate API-diff acknowledgements for baseline v9.8.7"

run decision v9.8.7 "" "${DUPLICATE_BASELINE_EXCEPTIONS}"
check_rc "duplicate-baseline-with-clean-diff-fails" 15
check_contains "duplicate-baseline-with-clean-diff-is-diagnostic" "Duplicate API-diff acknowledgements for baseline v9.8.7"

run decision v9.8.7 "${removed_method}" "${REINDENTED_EXCEPTIONS}"
check_rc "reindented-valid-yaml-succeeds" 0
check_contains "reindented-valid-yaml-is-diagnostic" "Allowing acknowledged incompatible API change(s) for v9.8.7: #1234"

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All API-diff decision-flow tests passed"
