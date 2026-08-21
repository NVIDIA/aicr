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

# Decision-flow tests for tools/api-diff stub Git, Go, and apidiff so no real
# worktree is created. A final end-to-end fixture uses the pinned apidiff against
# a temporary local module. yq is intentionally not stubbed: these tests
# exercise the real structural parser supplied by the repository's pinned
# toolchain, so mikefarah/yq is required throughout.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
API_DIFF="${SCRIPT_DIR}/api-diff"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
export API_DIFF_TEST_REPO_ROOT="${REPO_ROOT}"

if command -v go >/dev/null 2>&1; then
    test_go_bin="$(go env GOBIN 2>/dev/null || true)"
    if [[ -z "${test_go_bin}" ]]; then
        test_go_path="$(go env GOPATH 2>/dev/null || true)"
        test_go_path="${test_go_path%%:*}"
        if [[ -n "${test_go_path}" ]]; then
            test_go_bin="${test_go_path}/bin"
        fi
    fi
    if [[ -n "${test_go_bin}" ]]; then
        export PATH="${test_go_bin}:${PATH}"
    fi
fi
REAL_PATH="${PATH}"
REAL_APIDIFF="$(command -v apidiff 2>/dev/null || true)"

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
if [[ "$1" == "run" && "$2" == "./tools/api-diff-closure" ]]; then
    alias_mode=false
    closure_dir=""
    previous_arg=""
    for arg in "$@"; do
        if [[ "${arg}" == "-aliases" ]]; then
            alias_mode=true
        elif [[ "${previous_arg}" == "-dir" ]]; then
            closure_dir="${arg}"
        fi
        previous_arg="${arg}"
    done
    if [[ "${alias_mode}" == "true" ]]; then
        if [[ "${ALIAS_MAPPING_SCENARIO:-correct}" == "failure" ]]; then
            echo "mock alias mapping failure" >&2
            exit 42
        fi
        if [[ "${ALIAS_MAPPING_SCENARIO:-correct}" == "concrete-generic" ]]; then
            echo "api-diff-closure: inspect exported alias github.com/NVIDIA/aicr/pkg/client/v1.BundleConfig: generic target alias instantiated as github.com/NVIDIA/aicr/pkg/bundler/config.Config[int] cannot be scoped safely; generic target aliases must forward every type parameter unchanged with identical constraints (concrete, transformed, and narrowed instantiations are unsupported)" >&2
            exit 42
        fi
        # api-diff compares this output against an LC_ALL=C-sorted contract
        # without re-sorting, mirroring the real closure tool's sorted output.
        # Keep every line below in sorted order or the contract check fails for
        # the wrong reason.
        cat <<'ALIASES'
BundleArtifact|github.com/NVIDIA/aicr/pkg/bundler/result|Output
BundleAttester|github.com/NVIDIA/aicr/pkg/bundler/attestation|Attester
ALIASES
        if [[ "${ALIAS_MAPPING_SCENARIO:-correct}" == "retarget" ]]; then
            echo 'BundleConfig|github.com/NVIDIA/aicr/pkg/bundler/result|Output'
        else
            echo 'BundleConfig|github.com/NVIDIA/aicr/pkg/bundler/config|Config'
        fi
        echo 'BundleVerifyReport|github.com/NVIDIA/aicr/pkg/bundler/verifier|VerifyResult'
        echo 'CriteriaRegistry|github.com/NVIDIA/aicr/pkg/recipe|CriteriaRegistry'
        echo 'EvidenceVerification|github.com/NVIDIA/aicr/pkg/evidence/verifier|VerifyResult'
        if [[ "${ALIAS_MAPPING_SCENARIO:-correct}" == "extra-generic" ]]; then
            echo 'GenericAlias|github.com/NVIDIA/aicr/pkg/bundler/result|Result'
        fi
        echo 'OIDCResolveOptions|github.com/NVIDIA/aicr/pkg/bundler/attestation|ResolveOptions'
        exit 0
    fi
    if [[ "${ALIAS_CLOSURE_SCENARIO:-correct}" == "failure" ]]; then
        echo "mock alias closure failure" >&2
        exit 42
    fi
    cat <<'CLOSURE'
github.com/NVIDIA/aicr/pkg/bundler/attestation|AttestSubject
github.com/NVIDIA/aicr/pkg/bundler/attestation|Attester
github.com/NVIDIA/aicr/pkg/bundler/attestation|Dependency
github.com/NVIDIA/aicr/pkg/bundler/attestation|ResolveOptions
github.com/NVIDIA/aicr/pkg/bundler/attestation|StatementMetadata
github.com/NVIDIA/aicr/pkg/bundler/config|Config
github.com/NVIDIA/aicr/pkg/bundler/config|DeployerType
github.com/NVIDIA/aicr/pkg/bundler/result|Output
github.com/NVIDIA/aicr/pkg/bundler/result|Result
github.com/NVIDIA/aicr/pkg/bundler/types|BundleType
github.com/NVIDIA/aicr/pkg/recipe|CriteriaAcceleratorType
github.com/NVIDIA/aicr/pkg/recipe|CriteriaField
github.com/NVIDIA/aicr/pkg/recipe|CriteriaIntentType
github.com/NVIDIA/aicr/pkg/recipe|CriteriaOSType
github.com/NVIDIA/aicr/pkg/recipe|CriteriaOrigin
github.com/NVIDIA/aicr/pkg/recipe|CriteriaPlatformType
github.com/NVIDIA/aicr/pkg/recipe|CriteriaRegistry
github.com/NVIDIA/aicr/pkg/recipe|CriteriaServiceType
CLOSURE
    if [[ "${ALIAS_CLOSURE_SCENARIO:-correct}" == "different-membership" ]]; then
        if [[ "${closure_dir}" == "${API_DIFF_TEST_REPO_ROOT}" ]]; then
            echo 'github.com/NVIDIA/aicr/pkg/bundler/result|DeploymentInfo'
        else
            echo 'github.com/NVIDIA/aicr/pkg/bundler/result|BundleError'
        fi
    else
        cat <<'CLOSURE'
github.com/NVIDIA/aicr/pkg/bundler/result|BundleError
github.com/NVIDIA/aicr/pkg/bundler/result|DeploymentInfo
CLOSURE
    fi
    if [[ "${ALIAS_CLOSURE_SCENARIO:-correct}" == "generic-argument" ]]; then
        echo 'github.com/NVIDIA/aicr/pkg/payload|Contract'
    fi
    exit 0
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
allow_internal=false
export_path=""
positional=()
while (( $# > 0 )); do
    case "$1" in
        -allow-internal)
            allow_internal=true
            shift
            ;;
        -w)
            export_path=$2
            shift 2
            ;;
        -incompatible)
            echo "unexpected redundant apidiff -incompatible invocation" >&2
            exit 99
            ;;
        *)
            positional+=("$1")
            shift
            ;;
    esac
done

package="${positional[${#positional[@]}-1]:-}"
if [[ -n "${export_path}" ]]; then
    printf 'mode=export package=%s allow_internal=%s\n' "${package}" "${allow_internal}" >>"${APIDIFF_LOG}"
    : >"${export_path}"
    exit 0
fi

printf 'mode=report package=%s allow_internal=%s cwd=%s\n' "${package}" "${allow_internal}" "$(pwd)" >>"${APIDIFF_LOG}"
if [[ -n "${APIDIFF_FAILURE_PACKAGE:-}" && "${package}" == "${APIDIFF_FAILURE_PACKAGE}" ]]; then
    echo "mock current package load failure for ${package}" >&2
    exit 42
fi
if [[ -n "${APIDIFF_TARGET_REPORT_PACKAGE:-}" && "${package}" == "${APIDIFF_TARGET_REPORT_PACKAGE}" ]]; then
    printf '%s' "${APIDIFF_TARGET_REPORT:-}"
elif [[ -n "${APIDIFF_REPORT:-}" ]]; then
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
            APIDIFF_TARGET_REPORT_PACKAGE="${APIDIFF_TARGET_REPORT_PACKAGE:-}" \
            APIDIFF_TARGET_REPORT="${APIDIFF_TARGET_REPORT:-}" \
            APIDIFF_FAILURE_PACKAGE="${APIDIFF_FAILURE_PACKAGE:-}" \
            ALIAS_MAPPING_SCENARIO="${ALIAS_MAPPING_SCENARIO:-correct}" \
            ALIAS_CLOSURE_SCENARIO="${ALIAS_CLOSURE_SCENARIO:-correct}" \
            API_DIFF_EXCEPTIONS_FILE="${exceptions_file}" \
            APIDIFF_VERSION_SCENARIO="${version_scenario}" \
            GOPATH_SCENARIO="${gopath_scenario}" "${API_DIFF}" 2>&1)"
    else
        OUT="$(cd "${caller_dir}" && GIT_SCENARIO="${scenario}" APIDIFF_INCOMPATIBLE="${incompatible}" \
            APIDIFF_REPORT="${APIDIFF_REPORT:-}" \
            APIDIFF_TARGET_REPORT_PACKAGE="${APIDIFF_TARGET_REPORT_PACKAGE:-}" \
            APIDIFF_TARGET_REPORT="${APIDIFF_TARGET_REPORT:-}" \
            APIDIFF_FAILURE_PACKAGE="${APIDIFF_FAILURE_PACKAGE:-}" \
            ALIAS_MAPPING_SCENARIO="${ALIAS_MAPPING_SCENARIO:-correct}" \
            ALIAS_CLOSURE_SCENARIO="${ALIAS_CLOSURE_SCENARIO:-correct}" \
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

expected_apidiff_log() {
    local cwd=$1

    printf 'mode=export package=github.com/NVIDIA/aicr/pkg/client/v1 allow_internal=false\n'
    printf 'mode=export package=github.com/NVIDIA/aicr/pkg/bundler/attestation allow_internal=true\n'
    printf 'mode=export package=github.com/NVIDIA/aicr/pkg/bundler/config allow_internal=true\n'
    printf 'mode=export package=github.com/NVIDIA/aicr/pkg/bundler/result allow_internal=true\n'
    printf 'mode=export package=github.com/NVIDIA/aicr/pkg/bundler/types allow_internal=true\n'
    printf 'mode=export package=github.com/NVIDIA/aicr/pkg/recipe allow_internal=true\n'
    printf 'mode=report package=github.com/NVIDIA/aicr/pkg/client/v1 allow_internal=false cwd=%s\n' "${cwd}"
    printf 'mode=report package=github.com/NVIDIA/aicr/pkg/bundler/attestation allow_internal=true cwd=%s\n' "${cwd}"
    printf 'mode=report package=github.com/NVIDIA/aicr/pkg/bundler/config allow_internal=true cwd=%s\n' "${cwd}"
    printf 'mode=report package=github.com/NVIDIA/aicr/pkg/bundler/result allow_internal=true cwd=%s\n' "${cwd}"
    printf 'mode=report package=github.com/NVIDIA/aicr/pkg/bundler/types allow_internal=true cwd=%s\n' "${cwd}"
    printf 'mode=report package=github.com/NVIDIA/aicr/pkg/recipe allow_internal=true cwd=%s' "${cwd}"
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

run alias-contract-current-map v9.8.7
check_rc "current-transparent-alias-map-succeeds" 0

ALIAS_MAPPING_SCENARIO=extra-generic
run alias-contract-generic-mismatch v9.8.7
unset ALIAS_MAPPING_SCENARIO
check_rc "unscoped-generic-transparent-alias-fails" 18
check_contains "unscoped-generic-transparent-alias-is-diagnostic" "Transparent alias contract is out of sync"
check_contains "unscoped-generic-transparent-alias-is-named" "GenericAlias"
check_log_absent "unscoped-generic-transparent-alias-stops-before-git" "prune"

ALIAS_MAPPING_SCENARIO=concrete-generic
run alias-contract-concrete-generic v9.8.7
unset ALIAS_MAPPING_SCENARIO
check_rc "concrete-generic-transparent-alias-fails-closed" 18
check_contains "concrete-generic-transparent-alias-is-diagnostic" \
    "generic target alias instantiated as github.com/NVIDIA/aicr/pkg/bundler/config.Config[int] cannot be scoped safely"
check_contains "concrete-generic-transparent-alias-explains-supported-form" \
    "generic target aliases must forward every type parameter unchanged with identical constraints"
check_log_absent "concrete-generic-transparent-alias-stops-before-git" "prune"
check_apidiff_log_equals "concrete-generic-transparent-alias-stops-before-apidiff" ""

ALIAS_MAPPING_SCENARIO=retarget
run alias-contract-retargeted v9.8.7
unset ALIAS_MAPPING_SCENARIO
check_rc "retargeted-transparent-alias-fails" 18
check_contains "retargeted-transparent-alias-is-diagnostic" "Transparent alias contract is out of sync"
check_contains "retargeted-transparent-alias-reports-actual-target" "BundleConfig|github.com/NVIDIA/aicr/pkg/bundler/result|Output"
check_contains "retargeted-transparent-alias-reports-scoped-target" "BundleConfig|github.com/NVIDIA/aicr/pkg/bundler/config|Config"
check_log_absent "retargeted-transparent-alias-stops-before-git" "prune"

ALIAS_MAPPING_SCENARIO=failure
run alias-mapping-failure v9.8.7
unset ALIAS_MAPPING_SCENARIO
check_rc "alias-mapping-failure-fails-closed" 18
check_contains "alias-mapping-failure-is-diagnostic" "Could not inspect exported transparent aliases"
check_log_absent "alias-mapping-failure-stops-before-git" "prune"

ALIAS_CLOSURE_SCENARIO=failure
run alias-closure-failure v9.8.7
unset ALIAS_CLOSURE_SCENARIO
check_rc "alias-closure-derivation-failure-fails-closed" 18
check_contains "alias-closure-derivation-failure-is-diagnostic" "Could not derive the current transparent-alias reachable type closure"

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
check_apidiff_log_equals "facade-and-alias-targets-load-once-from-repository-root" \
    "$(expected_apidiff_log "${REPO_ROOT}")"

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
check_apidiff_log_equals "plain-report-loads-facade-and-alias-targets-once" \
    "$(expected_apidiff_log "${REPO_ROOT}")"
check_contains "unacknowledged-method-removal-is-diagnostic" "Add a baseline-scoped acknowledgement"

compatible_only_report=$'Compatible changes:\n- Client.New: added\n'
APIDIFF_REPORT="${compatible_only_report}"
run decision v9.8.7 "" "${EMPTY_EXCEPTIONS}"
unset APIDIFF_REPORT
check_rc "compatible-only-report-succeeds" 0
check_contains "compatible-only-change-is-reported" "Client.New: added"
check_contains "compatible-only-report-is-clean" "No incompatible"
check_apidiff_log_equals "compatible-only-report-loads-facade-and-alias-targets-once" \
    "$(expected_apidiff_log "${REPO_ROOT}")"

alias_target_cases=(
    'bundle-config|github.com/NVIDIA/aicr/pkg/bundler/config|(*Config).Deployer: removed'
    'bundle-attester|github.com/NVIDIA/aicr/pkg/bundler/attestation|Attester.Identity: removed'
    'oidc-resolve-options|github.com/NVIDIA/aicr/pkg/bundler/attestation|ResolveOptions.Attest: removed'
    'bundle-artifact|github.com/NVIDIA/aicr/pkg/bundler/result|Output.OutputDir: removed'
    'bundle-artifact-nested-result|github.com/NVIDIA/aicr/pkg/bundler/result|Result.Checksum: removed'
    'bundle-artifact-cross-package-type|github.com/NVIDIA/aicr/pkg/bundler/types|BundleType.String: removed'
    'criteria-registry|github.com/NVIDIA/aicr/pkg/recipe|(*CriteriaRegistry).Values: removed'
    'criteria-registry-signature-type|github.com/NVIDIA/aicr/pkg/recipe|CriteriaField: changed from string to int'
)
for case_spec in "${alias_target_cases[@]}"; do
    IFS='|' read -r case_name target_package target_change <<< "${case_spec}"
    APIDIFF_TARGET_REPORT_PACKAGE="${target_package}"
    APIDIFF_TARGET_REPORT=$'Incompatible changes:\n- '"${target_change}"$'\n- Unrelated.Legacy: removed\n'
    run "alias-${case_name}" v9.8.7 "" "${EMPTY_EXCEPTIONS}"
    unset APIDIFF_TARGET_REPORT_PACKAGE APIDIFF_TARGET_REPORT
    check_rc "${case_name}-break-fails" 16
    check_contains "${case_name}-break-is-reported" "${target_change}"
    check_absent "${case_name}-does-not-freeze-unrelated-export" "Unrelated.Legacy: removed"
done

# These receiver spellings are copied from the pinned apidiff report for a
# generic Target[P] with changed pointer and removed value methods. ConfigExtra
# deliberately shares Config's prefix so the scope check cannot pass by merely
# accepting any subject that starts with the target type name.
APIDIFF_TARGET_REPORT_PACKAGE='github.com/NVIDIA/aicr/pkg/bundler/config'
APIDIFF_TARGET_REPORT=$'Incompatible changes:\n- (*Config[P]).Deployer: changed from func(P) P to func([]P) P\n- Config[P].Validate: removed\n- ConfigExtra[P].Validate: removed\n'
run alias-generic-receivers v9.8.7 "" "${EMPTY_EXCEPTIONS}"
unset APIDIFF_TARGET_REPORT_PACKAGE APIDIFF_TARGET_REPORT
check_rc "generic-target-receiver-breaks-fail" 16
check_contains "generic-pointer-receiver-break-is-reported" "(*Config[P]).Deployer: changed from func(P) P to func([]P) P"
check_contains "generic-value-receiver-break-is-reported" "Config[P].Validate: removed"
check_absent "similarly-named-generic-type-remains-excluded" "ConfigExtra[P].Validate: removed"

APIDIFF_TARGET_REPORT_PACKAGE='github.com/NVIDIA/aicr/pkg/bundler/attestation'
APIDIFF_TARGET_REPORT=$'Incompatible changes:\n- VerificationIdentity.Issuer: removed\n'
run alias-unrelated-only v9.8.7 "" "${EMPTY_EXCEPTIONS}"
unset APIDIFF_TARGET_REPORT_PACKAGE APIDIFF_TARGET_REPORT
check_rc "unrelated-target-package-break-succeeds" 0
check_absent "unrelated-target-package-break-is-not-reported" "VerificationIdentity.Issuer: removed"
check_contains "unrelated-target-package-break-keeps-gate-clean" "No incompatible SDK facade or transparent-alias target changes"

ALIAS_CLOSURE_SCENARIO=generic-argument
APIDIFF_TARGET_REPORT_PACKAGE='github.com/NVIDIA/aicr/pkg/payload'
APIDIFF_TARGET_REPORT=$'Incompatible changes:\n- Contract.Legacy: removed\n- Unrelated.Legacy: removed\n'
run alias-generic-argument v9.8.7 "" "${EMPTY_EXCEPTIONS}"
unset ALIAS_CLOSURE_SCENARIO APIDIFF_TARGET_REPORT_PACKAGE APIDIFF_TARGET_REPORT
check_rc "generic-alias-argument-break-fails" 16
check_contains "generic-alias-argument-break-is-reported" "Contract.Legacy: removed"
check_absent "generic-alias-argument-does-not-freeze-unrelated-export" "Unrelated.Legacy: removed"

ALIAS_CLOSURE_SCENARIO=different-membership
APIDIFF_TARGET_REPORT_PACKAGE='github.com/NVIDIA/aicr/pkg/bundler/result'
APIDIFF_TARGET_REPORT=$'Incompatible changes:\n- DeploymentInfo.Legacy: removed\n'
run alias-current-only-reachable-type v9.8.7 "" "${EMPTY_EXCEPTIONS}"
unset APIDIFF_TARGET_REPORT_PACKAGE APIDIFF_TARGET_REPORT
check_rc "current-only-reachable-type-break-succeeds" 0
check_absent "current-only-reachable-type-break-is-not-reported" "DeploymentInfo.Legacy: removed"
check_contains "current-only-reachable-type-break-keeps-gate-clean" "No incompatible SDK facade or transparent-alias target changes"

APIDIFF_TARGET_REPORT_PACKAGE='github.com/NVIDIA/aicr/pkg/bundler/result'
APIDIFF_TARGET_REPORT=$'Incompatible changes:\n- BundleError.Legacy: removed\n'
run alias-baseline-only-reachable-type v9.8.7 "" "${EMPTY_EXCEPTIONS}"
unset APIDIFF_TARGET_REPORT_PACKAGE APIDIFF_TARGET_REPORT
check_rc "baseline-only-reachable-type-break-fails" 16
check_contains "baseline-only-reachable-type-break-is-reported" "BundleError.Legacy: removed"

APIDIFF_FAILURE_PACKAGE='github.com/NVIDIA/aicr/pkg/bundler/config'
run alias-existing-target-load-failure v9.8.7 "" "${EMPTY_EXCEPTIONS}"
unset APIDIFF_FAILURE_PACKAGE
check_rc "existing-target-load-failure-is-not-treated-as-removal" 42
check_contains "existing-target-load-failure-remains-diagnostic" \
    "mock current package load failure for github.com/NVIDIA/aicr/pkg/bundler/config"
check_absent "existing-target-load-failure-does-not-invent-package-removal" \
    "package github.com/NVIDIA/aicr/pkg/bundler/config: removed"

APIDIFF_TARGET_REPORT_PACKAGE='github.com/NVIDIA/aicr/pkg/bundler/result'
APIDIFF_TARGET_REPORT=$'Incompatible changes:\n- Output.Result: changed from Result to DeploymentInfo\n- DeploymentInfo.Legacy: removed\n'
run alias-current-only-type-parent-break v9.8.7 "" "${EMPTY_EXCEPTIONS}"
unset ALIAS_CLOSURE_SCENARIO APIDIFF_TARGET_REPORT_PACKAGE APIDIFF_TARGET_REPORT
check_rc "current-only-type-parent-break-fails" 16
check_contains "current-only-type-parent-break-is-reported" "Output.Result: changed from Result to DeploymentInfo"
check_absent "current-only-nested-type-remains-unreported" "DeploymentInfo.Legacy: removed"

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

# Exercise the complete gate with the pinned apidiff against a real temporary
# module. Config exposes an internal named type through its public field, so the
# internal type is part of the transparent-alias closure even though apidiff
# ignores that package by default.
if [[ -z "${REAL_APIDIFF}" ]]; then
    if [[ -n "${CI:-}" ]]; then
        fail "reachable-internal-target-regression-runs" "apidiff is not installed"
    else
        echo "SKIP: reachable internal target regression; run 'make tools-setup' to install apidiff"
    fi
else
    internal_fixture="${STUB_DIR}/internal-target-fixture"
    if ! (
        set -e
        export PATH="${REAL_PATH}"
        mkdir -p \
            "${internal_fixture}/tools/api-diff-closure" \
            "${internal_fixture}/pkg/client/v1" \
            "${internal_fixture}/pkg/bundler/attestation" \
            "${internal_fixture}/pkg/bundler/config" \
            "${internal_fixture}/pkg/bundler/result" \
            "${internal_fixture}/pkg/bundler/verifier" \
            "${internal_fixture}/pkg/evidence/verifier" \
            "${internal_fixture}/pkg/recipe" \
            "${internal_fixture}/internal/sdkcontract"
        cp "${API_DIFF}" "${internal_fixture}/tools/api-diff"
        cp "${SCRIPT_DIR}/common" "${internal_fixture}/tools/common"
        cp "${SCRIPT_DIR}/api-diff-closure/main.go" "${internal_fixture}/tools/api-diff-closure/main.go"

        cat >"${internal_fixture}/go.mod" <<'EOF'
module github.com/NVIDIA/aicr

go 1.26
EOF
        cat >"${internal_fixture}/.settings.yaml" <<EOF
linting:
  apidiff: '${PINNED_APIDIFF_VERSION}'
EOF
        cat >"${internal_fixture}/pkg/client/v1/api-diff-exceptions.yaml" <<'EOF'
acknowledgements: []
EOF
        cat >"${internal_fixture}/pkg/client/v1/client.go" <<'EOF'
package v1

import (
	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	bundlerverifier "github.com/NVIDIA/aicr/pkg/bundler/verifier"
	evidenceverifier "github.com/NVIDIA/aicr/pkg/evidence/verifier"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

type BundleConfig = config.Config
type BundleAttester = attestation.Attester
type OIDCResolveOptions = attestation.ResolveOptions
type BundleArtifact = result.Output
type CriteriaRegistry = recipe.CriteriaRegistry
type BundleVerifyReport = bundlerverifier.VerifyResult
type EvidenceVerification = evidenceverifier.VerifyResult
EOF
        cat >"${internal_fixture}/pkg/bundler/verifier/verifier.go" <<'EOF'
package verifier

type VerifyResult struct{}
EOF
        cat >"${internal_fixture}/pkg/evidence/verifier/verifier.go" <<'EOF'
package verifier

type VerifyResult struct{}
EOF
        cat >"${internal_fixture}/pkg/bundler/attestation/attestation.go" <<'EOF'
package attestation

type Attester struct{}
type ResolveOptions struct{}
EOF
        cat >"${internal_fixture}/pkg/bundler/config/config.go" <<'EOF'
package config

import "github.com/NVIDIA/aicr/internal/sdkcontract"

type Config struct {
	Contract sdkcontract.Contract
}
EOF
        cat >"${internal_fixture}/pkg/bundler/result/result.go" <<'EOF'
package result

type Output struct{}
EOF
        cat >"${internal_fixture}/pkg/recipe/recipe.go" <<'EOF'
package recipe

type CriteriaRegistry struct{}
EOF
        cat >"${internal_fixture}/internal/sdkcontract/contract.go" <<'EOF'
package sdkcontract

type Contract struct {
	Legacy string
}
EOF

        git -C "${internal_fixture}" init -q -b main
        git -C "${internal_fixture}" add .
        git -C "${internal_fixture}" \
            -c user.name='API Diff Test' \
            -c user.email='api-diff-test@example.com' \
            commit --no-gpg-sign -q -m 'test: establish API baseline'

        cat >"${internal_fixture}/internal/sdkcontract/contract.go" <<'EOF'
package sdkcontract

type Contract struct {
	Legacy int
}
EOF
        git -C "${internal_fixture}" rev-parse --verify HEAD >/dev/null
    ); then
        fail "reachable-internal-target-fixture-setup" "could not create the temporary Git fixture"
    else
        OUT="$(cd "${internal_fixture}" && PATH="${REAL_PATH}" API_DIFF_BASELINE=HEAD ./tools/api-diff 2>&1)"
        RC=$?
        check_rc "reachable-internal-target-incompatibility-fails-closed" 16
        check_contains "reachable-internal-target-incompatibility-is-scoped" \
            "Transparent alias reachable-type changes: github.com/NVIDIA/aicr/internal/sdkcontract (Contract)"
        check_contains "reachable-internal-target-incompatibility-is-reported" \
            "Contract.Legacy: changed from string to int"
        check_absent "reachable-internal-target-is-not-silently-ignored" \
            "Ignoring internal package github.com/NVIDIA/aicr/internal/sdkcontract"
    fi
fi

# Exercise a baseline alias retarget whose old reachable target package is
# deleted from the current tree while its documentation directory remains.
# apidiff cannot load that current package, so the gate must report the package
# removal and still adjudicate it through the exact-baseline acknowledgement
# workflow.
if [[ -z "${REAL_APIDIFF}" ]]; then
    if [[ -n "${CI:-}" ]]; then
        fail "deleted-alias-target-regression-runs" "apidiff is not installed"
    else
        echo "SKIP: deleted alias target regression; run 'make tools-setup' to install apidiff"
    fi
else
    deleted_target_fixture="${STUB_DIR}/deleted-alias-target-fixture"
    if ! (
        set -e
        export PATH="${REAL_PATH}"
        mkdir -p \
            "${deleted_target_fixture}/tools/api-diff-closure" \
            "${deleted_target_fixture}/pkg/client/v1" \
            "${deleted_target_fixture}/pkg/bundler/attestation" \
            "${deleted_target_fixture}/pkg/bundler/config" \
            "${deleted_target_fixture}/pkg/bundler/result" \
            "${deleted_target_fixture}/pkg/bundler/verifier" \
            "${deleted_target_fixture}/pkg/evidence/verifier" \
            "${deleted_target_fixture}/pkg/legacyconfig" \
            "${deleted_target_fixture}/pkg/recipe"
        cp "${API_DIFF}" "${deleted_target_fixture}/tools/api-diff"
        cp "${SCRIPT_DIR}/common" "${deleted_target_fixture}/tools/common"
        cp "${SCRIPT_DIR}/api-diff-closure/main.go" \
            "${deleted_target_fixture}/tools/api-diff-closure/main.go"

        cat >"${deleted_target_fixture}/go.mod" <<'EOF'
module github.com/NVIDIA/aicr

go 1.26
EOF
        cat >"${deleted_target_fixture}/.settings.yaml" <<EOF
linting:
  apidiff: '${PINNED_APIDIFF_VERSION}'
EOF
        cat >"${deleted_target_fixture}/pkg/client/v1/api-diff-exceptions.yaml" <<'EOF'
acknowledgements: []
EOF
        cat >"${deleted_target_fixture}/pkg/client/v1/client.go" <<'EOF'
package v1

import (
	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	bundlerverifier "github.com/NVIDIA/aicr/pkg/bundler/verifier"
	evidenceverifier "github.com/NVIDIA/aicr/pkg/evidence/verifier"
	"github.com/NVIDIA/aicr/pkg/legacyconfig"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

type BundleConfig = legacyconfig.Config
type BundleAttester = attestation.Attester
type OIDCResolveOptions = attestation.ResolveOptions
type BundleArtifact = result.Output
type CriteriaRegistry = recipe.CriteriaRegistry
type BundleVerifyReport = bundlerverifier.VerifyResult
type EvidenceVerification = evidenceverifier.VerifyResult
EOF
        cat >"${deleted_target_fixture}/pkg/bundler/verifier/verifier.go" <<'EOF'
package verifier

type VerifyResult struct{}
EOF
        cat >"${deleted_target_fixture}/pkg/evidence/verifier/verifier.go" <<'EOF'
package verifier

type VerifyResult struct{}
EOF
        cat >"${deleted_target_fixture}/pkg/bundler/attestation/attestation.go" <<'EOF'
package attestation

type Attester struct{}
type ResolveOptions struct{}
EOF
        cat >"${deleted_target_fixture}/pkg/bundler/config/config.go" <<'EOF'
package config

type Config struct{}
EOF
        cat >"${deleted_target_fixture}/pkg/bundler/result/result.go" <<'EOF'
package result

type Output struct{}
EOF
        cat >"${deleted_target_fixture}/pkg/legacyconfig/config.go" <<'EOF'
package legacyconfig

type Config struct{}
EOF
        cat >"${deleted_target_fixture}/pkg/legacyconfig/README.md" <<'EOF'
# Legacy configuration
EOF
        cat >"${deleted_target_fixture}/pkg/recipe/recipe.go" <<'EOF'
package recipe

type CriteriaRegistry struct{}
EOF

        git -C "${deleted_target_fixture}" init -q -b main
        git -C "${deleted_target_fixture}" add .
        git -C "${deleted_target_fixture}" \
            -c user.name='API Diff Test' \
            -c user.email='api-diff-test@example.com' \
            commit --no-gpg-sign -q -m 'test: establish retarget baseline'

        cat >"${deleted_target_fixture}/pkg/client/v1/client.go" <<'EOF'
package v1

import (
	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	bundlerverifier "github.com/NVIDIA/aicr/pkg/bundler/verifier"
	evidenceverifier "github.com/NVIDIA/aicr/pkg/evidence/verifier"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

type BundleConfig = config.Config
type BundleAttester = attestation.Attester
type OIDCResolveOptions = attestation.ResolveOptions
type BundleArtifact = result.Output
type CriteriaRegistry = recipe.CriteriaRegistry
type BundleVerifyReport = bundlerverifier.VerifyResult
type EvidenceVerification = evidenceverifier.VerifyResult
EOF
        rm "${deleted_target_fixture}/pkg/legacyconfig/config.go"
    ); then
        fail "deleted-alias-target-fixture-setup" "could not create the temporary Git fixture"
    else
        OUT="$(cd "${deleted_target_fixture}" && PATH="${REAL_PATH}" API_DIFF_BASELINE=HEAD ./tools/api-diff 2>&1)"
        RC=$?
        check_rc "deleted-alias-target-unacknowledged-break-fails" 16
        check_contains "deleted-alias-target-retarget-is-reported" \
            "BundleConfig: changed from github.com/NVIDIA/aicr/pkg/legacyconfig.Config to github.com/NVIDIA/aicr/pkg/bundler/config.Config"
        check_contains "deleted-alias-target-package-removal-is-scoped" \
            "Transparent alias reachable-type changes: github.com/NVIDIA/aicr/pkg/legacyconfig (Config)"
        check_contains "deleted-alias-target-package-removal-is-reported" \
            "package github.com/NVIDIA/aicr/pkg/legacyconfig: removed"
        check_absent "deleted-alias-target-does-not-abort-during-package-load" \
            "loading github.com/NVIDIA/aicr/pkg/legacyconfig"

        cat >"${deleted_target_fixture}/pkg/client/v1/api-diff-exceptions.yaml" <<'EOF'
acknowledgements:
  - baseline: HEAD
    issue: '#1234'
    summary: Retarget the bundle configuration alias.
    rationale: The old package was intentionally retired with its facade alias target.
    incompatible_changes:
      - 'BundleConfig: changed from github.com/NVIDIA/aicr/pkg/legacyconfig.Config to github.com/NVIDIA/aicr/pkg/bundler/config.Config'
      - 'package github.com/NVIDIA/aicr/pkg/legacyconfig: removed'
EOF
        OUT="$(cd "${deleted_target_fixture}" && PATH="${REAL_PATH}" API_DIFF_BASELINE=HEAD ./tools/api-diff 2>&1)"
        RC=$?
        check_rc "deleted-alias-target-exact-acknowledgement-succeeds" 0
        check_contains "deleted-alias-target-acknowledgement-is-diagnostic" \
            "Allowing acknowledged incompatible API change(s) for HEAD: #1234"
    fi
fi

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All API-diff tests passed"
