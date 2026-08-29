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

# Verdict tests for tools/openapi-diff, driven against fixture specs through the
# OPENAPI_DIFF_*_FILE overrides so the real REST contract is never mutated.
#
# The whole point of the gate is the verdict, and every branch of it can pass for
# the wrong reason: a clean run proves nothing about detection, an acknowledged
# run can be silently ignoring everything, and a staleness check that never fires
# lets dead entries pre-approve a break returning. Each case below therefore
# pins an exact exit code rather than "non-zero".
#
# oasdiff and yq are not stubbed: these exercise the real pinned toolchain,
# because a stub of oasdiff would be a stub of the thing under test.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OPENAPI_DIFF="${SCRIPT_DIR}/openapi-diff"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

if command -v go >/dev/null 2>&1; then
    test_go_bin="$(go env GOBIN 2>/dev/null || true)"
    if [[ -z "${test_go_bin}" ]]; then
        test_go_path="$(go env GOPATH 2>/dev/null || true)"
        test_go_path="${test_go_path%%:*}"
        [[ -n "${test_go_path}" ]] && test_go_bin="${test_go_path}/bin"
    fi
    [[ -n "${test_go_bin}" ]] && export PATH="${test_go_bin}:${PATH}"
fi

# Mirrors api-diff_test.sh: a missing tool otherwise reads as many unrelated
# failures instead of one cause. Skip locally, fail in CI where the gate must
# actually run.
missing=""
if ! command -v oasdiff >/dev/null 2>&1; then
    missing="oasdiff is not installed"
elif ! command -v yq >/dev/null 2>&1; then
    missing="yq is not installed"
elif ! yq --version 2>/dev/null | grep -q "mikefarah/yq"; then
    missing="yq at $(command -v yq) is not mikefarah/yq (Go-based)"
fi
if [[ -n "${missing}" ]]; then
    if [[ -n "${CI:-}" ]]; then
        echo "FAIL: ${missing}; the OpenAPI-diff gate cannot be verified in CI" >&2
        exit 1
    fi
    echo "SKIP: ${missing}; run 'make tools-setup' to install the pinned version"
    exit 0
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/openapi-diff-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

fails=0
pass() { echo "PASS: $1"; }
fail() {
    echo "FAIL: $1 — $2"
    fails=$((fails + 1))
}

# require_fixture_edit fails loudly when a fixture edit did not change the spec.
# Without it a moved anchor turns a case into a no-op that reports PASS.
require_fixture_edit() {
    # Must be the first statement: any command here, including `local name=`,
    # overwrites $? and would make the script-failure branch below unreachable.
    local status=$?
    local name="$1"
    if [[ ${status} -ne 0 ]]; then
        fail "${name}" "fixture edit script failed"
        return
    fi
    if cmp -s "${WORK}/spec.yaml" "${WORK}/baseline.yaml"; then
        fail "${name}" "fixture edit produced no change; the case would pass vacuously"
    fi
}

check_rc() {
    local name="$1" want="$2" got="$3"
    if [[ "${got}" == "${want}" ]]; then
        pass "${name}"
    else
        fail "${name}" "exit ${got}, want ${want}"
    fi
}

# The fixture is the real committed contract, so these tests stay meaningful as
# the spec evolves rather than drifting against a hand-written toy spec.
BASE_SPEC="${REPO_ROOT}/api/aicr/v1/server.baseline.yaml"

write_exceptions() {
    printf '%s\n' "acknowledgements:$1" > "${WORK}/exceptions.yaml"
}

run_gate() {
    (
        cd "${REPO_ROOT}" || exit 99
        OPENAPI_DIFF_SPEC_FILE="${WORK}/spec.yaml" \
            OPENAPI_DIFF_BASELINE_FILE="${WORK}/baseline.yaml" \
            OPENAPI_DIFF_EXCEPTIONS_FILE="${WORK}/exceptions.yaml" \
            bash "${OPENAPI_DIFF}" >"${WORK}/out.txt" 2>&1
    )
}

cp "${BASE_SPEC}" "${WORK}/baseline.yaml"

# 1. Identical spec and baseline: the gate must pass. A failure here would mean
#    the gate reports drift on an unchanged contract, which trains people to
#    ignore it.
cp "${BASE_SPEC}" "${WORK}/spec.yaml"
write_exceptions " []"
run_gate
check_rc "identical-spec-passes" 0 "$?"

# 2. Removing an endpoint is the canonical break. Without this the gate could be
#    passing case 1 by never running oasdiff at all.
# A failed fixture edit leaves spec.yaml identical to the baseline, so the gate
# would pass and the case would report success without testing anything. set -e
# is intentionally off in this file, so the status is checked explicitly.
python3 - "${WORK}/spec.yaml" <<'PY'
import sys
path = sys.argv[1]
text = open(path).read()
start = text.index("  /v1/query:")
end = text.index("  /v1/bundle:")
open(path, "w").write(text[:start] + text[end:])
PY
require_fixture_edit "remove-endpoint"
run_gate
check_rc "unacknowledged-break-fails" 1 "$?"
if grep -q "api-path-removed-without-deprecation" "${WORK}/out.txt"; then
    pass "unacknowledged-break-names-the-rule"
else
    fail "unacknowledged-break-names-the-rule" "rule id absent from output"
fi

# 3. An acknowledgement matching that break must clear it.
write_exceptions "
  - id: api-path-removed-without-deprecation
    path: /v1/query
    reason: fixture"
run_gate
check_rc "acknowledged-break-passes" 0 "$?"

# 4. An acknowledgement narrowed to a different path must NOT clear it.
#    Matching on rule id alone would let one endpoint's exception cover the same
#    class of break anywhere.
write_exceptions "
  - id: api-path-removed-without-deprecation
    path: /v1/bundle
    reason: fixture for a different endpoint"
run_gate
check_rc "acknowledgement-does-not-leak-across-paths" 1 "$?"

# 5. Restoring the spec leaves the acknowledgement matching nothing. A stale
#    entry silently pre-approves the break returning, so it must fail.
cp "${BASE_SPEC}" "${WORK}/spec.yaml"
write_exceptions "
  - id: api-path-removed-without-deprecation
    path: /v1/query
    reason: fixture"
run_gate
check_rc "stale-acknowledgement-fails" 12 "$?"

# 6. A malformed exceptions file must fail loudly rather than being read as
#    "no exceptions", which would silently change the verdict.
printf 'not-a-mapping\n' > "${WORK}/exceptions.yaml"
run_gate
check_rc "malformed-exceptions-fails" 11 "$?"

# 7. A missing input must not be mistaken for an empty contract.
write_exceptions " []"
rm -f "${WORK}/spec.yaml"
run_gate
check_rc "missing-spec-fails" 10 "$?"

# 8. Additive change: a new optional parameter must not fail. A gate that
#    rejects additions would push authors to bypass it.
cp "${BASE_SPEC}" "${WORK}/spec.yaml"
python3 - "${WORK}/spec.yaml" <<'PY'
import sys
path = sys.argv[1]
text = open(path).read()
anchor = "    Accelerator:\n      name: accelerator"
addition = (
    "    FixtureOptional:\n"
    "      name: fixtureOptional\n"
    "      in: query\n"
    "      required: false\n"
    "      description: Additive optional parameter.\n"
    "      schema:\n"
    "        type: string\n"
)
assert text.count(anchor) == 1
open(path, "w").write(text.replace(anchor, addition + anchor))
PY
require_fixture_edit "add-optional-parameter"
run_gate
check_rc "additive-change-passes" 0 "$?"

# 9. A removed request parameter is reported at WARN (level 2), not ERR. An
#    ERR-only filter reported zero breaking changes here and passed, dropping a
#    whole rule class the gate exists to cover.
cp "${BASE_SPEC}" "${WORK}/spec.yaml"
write_exceptions " []"
python3 - "${WORK}/spec.yaml" <<'PY'
import sys
path = sys.argv[1]
lines = open(path).read().split("\n")
for i, line in enumerate(lines):
    if "parameters/Nodes" in line:
        del lines[i]
        break
else:
    raise SystemExit("no Nodes parameter reference to remove")
open(path, "w").write("\n".join(lines))
PY
require_fixture_edit "remove-request-parameter"
run_gate
check_rc "warn-level-break-fails" 1 "$?"
if grep -q "request-parameter-removed" "${WORK}/out.txt"; then
    pass "warn-level-break-names-the-rule"
else
    fail "warn-level-break-names-the-rule" "rule id absent from output"
fi

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All OpenAPI-diff tests passed"
