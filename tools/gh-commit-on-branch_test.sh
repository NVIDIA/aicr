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

# Unit harness for .github/scripts/gh-commit-on-branch.sh.
# Run directly: bash tools/gh-commit-on-branch_test.sh
# Wired into CI via `make test` (test-shell target).
#
# Hermetic: stubs `gh` on PATH and captures the request body it is handed, so
# no token, network call, or repository write is involved.
#
# The defect this exists for (#2893) is invisible to any test that runs against
# a small file: passing contents through `jq --arg` works until the payload
# crosses an OS argv limit, and then it fails — so the bug shipped twice and sat
# latent in a third place. The size assertions below are therefore load-bearing
# and must stay above BOTH limits this can hit:
#
#   Linux   MAX_ARG_STRLEN  128 KiB for a SINGLE argv string
#   macOS   ARG_MAX          1 MiB for the argv block as a whole
#
# A 2 MB payload clears both, so a regression to `--arg` fails this test on a
# developer's machine and not only on the runner. The real trigger,
# pkg/bundler/testdata/stock_render_golden.yaml, is ~436 KB (~581 KB base64):
# over the Linux per-string cap, under the macOS total. Sizing to the golden
# alone would make this test pass on macOS while the workflow stayed broken.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${REPO_ROOT}/.github/scripts/gh-commit-on-branch.sh"

if [[ ! -x "${SCRIPT}" ]]; then
  echo "FAIL: ${SCRIPT} not found or not executable" >&2
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

STUB_DIR="${WORK}/bin"
mkdir -p "${STUB_DIR}"

# --- Stub `gh` on PATH --------------------------------------------------------
# Copies the request body to GH_STUB_REQUEST for assertions, then answers with a
# canned mutation response. The stub applies the caller's --jq filter with the
# real jq rather than echoing a bare oid, so the script's own response parsing
# is exercised instead of assumed.
#
# GH_STUB_RESPONSE overrides the response body, which is how the rejected-
# mutation case below drives `data.createCommitOnBranch` to null.
cat >"${STUB_DIR}/gh" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
input=""
filter=""
prev=""
for a in "$@"; do
  case "${prev}" in
    --input) input="${a}" ;;
    --jq)    filter="${a}" ;;
  esac
  prev="${a}"
done
[ -n "${input}" ] || { echo "gh stub: no --input given" >&2; exit 1; }
cp "${input}" "${GH_STUB_REQUEST:?GH_STUB_REQUEST unset}"
default='{"data":{"createCommitOnBranch":{"commit":{"oid":"c0ffee1234","url":"https://example.invalid/c"}}}}'
response="${GH_STUB_RESPONSE:-${default}}"
if [ -n "${filter}" ]; then
  printf '%s' "${response}" | jq -r "${filter}"
else
  printf '%s' "${response}"
fi
STUB
chmod +x "${STUB_DIR}/gh"
PATH="${STUB_DIR}:${PATH}"
export PATH

export GH_TOKEN=stub-token
export GH_STUB_REQUEST="${WORK}/request.json"

FAILED=0
check() { # check <label> <expected> <actual>
    if [[ "$2" == "$3" ]]; then
        echo "  ok   $1"
    else
        echo "  FAIL $1: expected '$2', got '$3'"
        FAILED=1
    fi
}

run() { # run <args...> -- captures stdout, leaves status in $?
    "${SCRIPT}" "$@" 2>"${WORK}/stderr.txt"
}

req() { # req <jq filter> -- query the captured request body
    jq -r "$1" <"${GH_STUB_REQUEST}" 2>/dev/null
}

# --- The regression: a payload past both argv limits -------------------------
# Random bytes, not text: base64 must survive NULs, newlines, and high bytes,
# and a byte-for-byte round-trip is the only assertion that proves it did.
BIG="${WORK}/big.bin"
head -c 2000000 /dev/urandom >"${BIG}"

echo "large single file (2 MB, past Linux MAX_ARG_STRLEN and macOS ARG_MAX):"
rm -f "${GH_STUB_REQUEST}"
oid="$(run --repo NVIDIA/aicr --branch renovate/x --headline 'test: refresh' \
    --expected-head-oid abc123 --add "${BIG}")"
check "exits 0" "0" "$?"
check "returns the commit oid" "c0ffee1234" "${oid}"
check "sends exactly one addition" "1" "$(req '.variables.input.fileChanges.additions | length')"

# Round-trip the contents back through base64 and diff against the source. This
# is what catches a truncated, wrapped, or newline-polluted encoding, none of
# which a length check would see.
req '.variables.input.fileChanges.additions[0].contents' | base64 -d >"${WORK}/decoded.bin" 2>/dev/null
check "contents round-trip byte-for-byte" "identical" \
    "$(cmp -s "${BIG}" "${WORK}/decoded.bin" && echo identical || echo differs)"

# --- The latent evidence-path shape: many files, each individually small ------
# This guards a different seam from the case above, not a bigger version of it.
# evidence-commit-signed.sh accumulated every file into one `--argjson
# additions` value, so files that individually cleared the cap still breached it
# together. The script must therefore keep the ACCUMULATED array off argv too —
# it reaches jq as `--slurpfile`, a path. Twelve 100 KB files are 1.6 MB of
# base64 in total, past the macOS argv budget, while no single one is.
echo "many medium files (12 x 100 KB, ~1.6 MB accumulated):"
MANY=()
for i in $(seq 1 12); do
    f="${WORK}/part-${i}.bin"
    head -c 100000 /dev/urandom >"${f}"
    MANY+=(--add "${f}")
done
rm -f "${GH_STUB_REQUEST}"
run --repo NVIDIA/aicr --branch renovate/x --headline 'test: many' \
    --expected-head-oid abc123 "${MANY[@]}" >/dev/null
check "exits 0" "0" "$?"
check "sends every addition" "12" "$(req '.variables.input.fileChanges.additions | length')"
check "no addition is empty" "0" \
    "$(req '[.variables.input.fileChanges.additions[] | select(.contents == "")] | length')"

# --- Paths that word splitting or globbing would mangle -----------------------
# The script iterates its path arrays as ${arr[@]+"${arr[@]}"}, where the inner
# expansion is quoted. Dropping those inner quotes still passes every other case
# here, and fails only on a path carrying a space or a glob character — at which
# point the mutation would add or delete files the caller never selected. The
# decoys exist so a glob that did expand would resolve to something.
echo "paths with spaces and glob characters:"
ODD="${WORK}/odd"
mkdir -p "${ODD}"
printf 'space\n'  >"${ODD}/a b.txt"
printf 'bracket\n' >"${ODD}/g[1].txt"
printf 'star\n'   >"${ODD}/s*.txt"
printf 'decoy\n'  >"${ODD}/g1.txt"
printf 'decoy\n'  >"${ODD}/sXX.txt"
rm -f "${GH_STUB_REQUEST}"
run --repo NVIDIA/aicr --branch b --headline h --expected-head-oid abc \
    --add "${ODD}/a b.txt" --add "${ODD}/g[1].txt" --add "${ODD}/s*.txt" \
    --delete "recipes/evidence/x y.yaml" >/dev/null
check "exits 0" "0" "$?"
check "keeps each path as one addition" "3" \
    "$(req '.variables.input.fileChanges.additions | length')"
check "path with a space survives intact" "${ODD}/a b.txt" \
    "$(req '.variables.input.fileChanges.additions[0].path')"
check "bracket path is not glob-expanded" "${ODD}/g[1].txt" \
    "$(req '.variables.input.fileChanges.additions[1].path')"
check "star path is not glob-expanded" "${ODD}/s*.txt" \
    "$(req '.variables.input.fileChanges.additions[2].path')"
check "no decoy file was pulled in" "0" \
    "$(req '[.variables.input.fileChanges.additions[] | select(.path | test("(g1|sXX)\\.txt$"))] | length')"
check "deletion path with a space survives intact" "recipes/evidence/x y.yaml" \
    "$(req '.variables.input.fileChanges.deletions[0].path')"

# --- Request shape ------------------------------------------------------------
echo "request shape:"
SMALL="${WORK}/small.txt"
printf 'hello\n' >"${SMALL}"
rm -f "${GH_STUB_REQUEST}"
run --repo NVIDIA/aicr --branch renovate/ubuntu-26.04 \
    --headline 'test(bundler): refresh render golden' \
    --body 'Signed-off-by: github-actions[bot] <41898282+github-actions[bot]@users.noreply.github.com>' \
    --expected-head-oid deadbeef \
    --add "${SMALL}" --delete recipes/evidence/gone.yaml >/dev/null
check "repository" "NVIDIA/aicr" "$(req '.variables.input.branch.repositoryNameWithOwner')"
check "branch" "renovate/ubuntu-26.04" "$(req '.variables.input.branch.branchName')"
check "expectedHeadOid" "deadbeef" "$(req '.variables.input.expectedHeadOid')"
check "headline" "test(bundler): refresh render golden" "$(req '.variables.input.message.headline')"
check "body carries the DCO trailer" "1" \
    "$(req '.variables.input.message.body' | grep -c '^Signed-off-by: github-actions\[bot\]')"
check "addition path" "${SMALL}" "$(req '.variables.input.fileChanges.additions[0].path')"
check "addition contents" "hello" \
    "$(req '.variables.input.fileChanges.additions[0].contents' | base64 -d)"
check "deletion path" "recipes/evidence/gone.yaml" \
    "$(req '.variables.input.fileChanges.deletions[0].path')"
check "deletions carry no contents" "null" \
    "$(req '.variables.input.fileChanges.deletions[0].contents')"
check "mutation names createCommitOnBranch" "1" "$(req '.query' | grep -c 'createCommitOnBranch')"

# A commit with no deletions must send [], not null: the GraphQL input type
# rejects a null list, and an absent key would make the mutation fail only when
# some caller happens to have nothing to delete.
echo "empty lists:"
rm -f "${GH_STUB_REQUEST}"
run --repo NVIDIA/aicr --branch b --headline h --expected-head-oid abc --add "${SMALL}" >/dev/null
check "deletions is an empty array" "0" "$(req '.variables.input.fileChanges.deletions | length')"
check "deletions is not null" "array" "$(req '.variables.input.fileChanges.deletions | type')"

# --- Fail-closed cases --------------------------------------------------------
# Each of these must refuse rather than produce an empty, mis-targeted, or
# silently-failed commit.
echo "fail-closed:"
run --repo NVIDIA/aicr --headline h --add "${SMALL}" >/dev/null 2>&1
check "rejects a missing --branch" "1" "$?"
run --repo NVIDIA/aicr --branch b --add "${SMALL}" >/dev/null 2>&1
check "rejects a missing --headline" "1" "$?"
run --repo NVIDIA/aicr --branch b --headline h >/dev/null 2>&1
check "rejects an empty change set" "1" "$?"
run --repo NVIDIA/aicr --branch b --headline h --add "${WORK}/does-not-exist" >/dev/null 2>&1
check "rejects a nonexistent --add path" "1" "$?"
run --repo NVIDIA/aicr --branch b --headline h --add "${SMALL}" --unknown-flag >/dev/null 2>&1
check "rejects an unknown flag" "1" "$?"

# GraphQL reports a rejected mutation as a 200 whose data is null. Returning
# that as a commit oid would let a job report success having committed nothing.
GH_STUB_RESPONSE='{"data":{"createCommitOnBranch":null},"errors":[{"message":"expected head oid does not match"}]}' \
    run --repo NVIDIA/aicr --branch b --headline h --expected-head-oid stale --add "${SMALL}" >/dev/null 2>&1
check "rejects a null commit oid" "1" "$?"

( unset GH_TOKEN; "${SCRIPT}" --repo NVIDIA/aicr --branch b --headline h --add "${SMALL}" ) >/dev/null 2>&1
check "rejects a missing GH_TOKEN" "1" "$?"

if [[ "${FAILED}" -eq 0 ]]; then
    echo "PASS: gh-commit-on-branch request assembly"
else
    echo "FAIL: gh-commit-on-branch request assembly"
fi
exit "${FAILED}"
