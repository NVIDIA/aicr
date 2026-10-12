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

# Cases for tools/check-upgrade-records, with git and go stubbed on PATH.
#
# The cases that matter are the rejections: a gate whose merge base is missing,
# or whose tests skip or vanish, exits 0 just like a gate that checked
# everything, so each of those is pinned with the message it must produce.

set -euo pipefail

# The default-ref cases assume the caller has not chosen a ref.
unset AICR_UPGRADE_BASE_REF

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TOOL="${ROOT}/tools/check-upgrade-records"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/check-upgrade-records-test.XXXXXX")
trap 'rm -rf "${TMP}"' EXIT

readonly TESTS=(
	TestRealUpgradeRecordsWellFormed
	TestRecordedComponentsCoverTheirPins
	TestBaseCoverageViewMatchesEmbedded
	TestChangedPinsHaveUpgradeRecords
)

mkdir -p "${TMP}/bin" "${TMP}/fixture/recipes"
printf 'components: []\n' > "${TMP}/fixture/recipes/registry.yaml"

cat > "${TMP}/bin/git" <<'STUB'
#!/usr/bin/env bash
case "$1" in
rev-parse)
	[[ "${STUB_UPSTREAM:-0}" == 1 ]]
	;;
merge-base)
	printf '%s\n' "$3" > "${STUB_DIR}/merge-base-ref"
	if [[ "${STUB_MERGE_BASE_FAIL:-0}" == 1 ]]; then
		echo "fatal: Not a valid object name $3" >&2
		exit 128
	fi
	echo 0123456789abcdef
	;;
archive)
	printf '%s %s\n' "$2" "$3" > "${STUB_DIR}/archive-args"
	tar -c -C "${STUB_DIR}/fixture" recipes
	;;
*)
	echo "unexpected git $*" >&2
	exit 99
	;;
esac
STUB

cat > "${TMP}/bin/go" <<'STUB'
#!/usr/bin/env bash
{
	printf 'dir=%s\n' "${AICR_UPGRADE_BASE_RECIPES:-}"
	if [[ -f "${AICR_UPGRADE_BASE_RECIPES:-/nonexistent}/registry.yaml" ]]; then
		echo extracted=yes
	else
		echo extracted=no
	fi
	printf 'args=%s\n' "$*"
} > "${STUB_DIR}/go-call"
cat "${STUB_DIR}/go-output"
exit "${STUB_GO_RC:-0}"
STUB
chmod +x "${TMP}/bin/git" "${TMP}/bin/go"

failures=0

# go_output <test-name>=<PASS|SKIP|FAIL|absent>... writes the stubbed go test
# output; every test not named passes.
go_output() {
	local name state line
	: > "${TMP}/go-output"
	for name in "${TESTS[@]}"; do
		state=PASS
		for line in "$@"; do
			[[ "${line%%=*}" == "${name}" ]] && state="${line#*=}"
		done
		[[ "${state}" == absent ]] && continue
		printf -- '--- %s: %s (0.01s)\n' "${state}" "${name}" >> "${TMP}/go-output"
	done
}

# run_tool [VAR=value...]: runs the gate with the stubs, capturing rc and output.
run_tool() {
	rm -f "${TMP}/merge-base-ref" "${TMP}/archive-args" "${TMP}/go-call"
	rc=0
	output=$(env PATH="${TMP}/bin:${PATH}" STUB_DIR="${TMP}" TMPDIR="${TMP}" "$@" "${TOOL}" 2>&1) || rc=$?
}

pass() { echo "ok   $1"; }
fail() {
	echo "FAIL $1: $2" >&2
	echo "--- output ---" >&2
	printf '%s\n' "${output}" >&2
	failures=$((failures + 1))
}

expect_rc() {
	local name="$1" want="$2"
	if [[ "${want}" == 0 && "${rc}" -ne 0 ]] || [[ "${want}" != 0 && "${rc}" -eq 0 ]]; then
		fail "${name}" "want rc ${want}, got ${rc}"
		return 1
	fi
}

expect_contains() {
	local name="$1" haystack="$2" want="$3"
	if [[ "${haystack}" != *"${want}"* ]]; then
		fail "${name}" "want '${want}'"
		return 1
	fi
}

# --- accepted -------------------------------------------------------------

go_output
run_tool
name="every test passing exits 0"
if expect_rc "${name}" 0; then pass "${name}"; fi

name="defaults to origin/main without an upstream remote"
if expect_contains "${name}" "$(cat "${TMP}/merge-base-ref")" "origin/main"; then pass "${name}"; fi

name="archives the recipes tree at the merge base"
archived=$(cat "${TMP}/archive-args")
if [[ "${archived}" == "0123456789abcdef recipes" ]]; then
	pass "${name}"
else
	fail "${name}" "git archive got '${archived}'"
fi

name="go sees the extracted merge-base recipes"
call=$(cat "${TMP}/go-call")
if expect_contains "${name}" "${call}" "extracted=yes" &&
	expect_contains "${name}" "${call}" "dir=${TMP}/check-upgrade-records."; then
	pass "${name}"
fi

name="go runs every gate test"
ok=1
for t in "${TESTS[@]}"; do
	expect_contains "${name}" "${call}" "${t}" || ok=0
done
[[ "${ok}" == 1 ]] && pass "${name}"

name="the extracted tree is removed on exit"
leftover=$(find "${TMP}" -maxdepth 1 -name 'check-upgrade-records.*')
if [[ -z "${leftover}" ]]; then pass "${name}"; else fail "${name}" "left ${leftover}"; fi

run_tool STUB_UPSTREAM=1
name="prefers upstream/main when it exists"
if expect_rc "${name}" 0 && expect_contains "${name}" "$(cat "${TMP}/merge-base-ref")" "upstream/main"; then
	pass "${name}"
fi

run_tool STUB_UPSTREAM=1 AICR_UPGRADE_BASE_REF=mirror/release-1.0
name="AICR_UPGRADE_BASE_REF wins over upstream/main"
if expect_rc "${name}" 0 && expect_contains "${name}" "$(cat "${TMP}/merge-base-ref")" "mirror/release-1.0"; then
	pass "${name}"
fi

# --- rejected -------------------------------------------------------------

run_tool STUB_MERGE_BASE_FAIL=1
name="a missing merge base fails with the fetch hint"
if expect_rc "${name}" 1 &&
	expect_contains "${name}" "${output}" "no merge base between HEAD and origin/main" &&
	expect_contains "${name}" "${output}" "git fetch origin main"; then
	pass "${name}"
fi
name="a missing merge base never reaches go"
if [[ ! -e "${TMP}/go-call" ]]; then pass "${name}"; else fail "${name}" "go ran"; fi

run_tool STUB_MERGE_BASE_FAIL=1 AICR_UPGRADE_BASE_REF=0123abc
name="a ref with no remote gets no fetch command in its hint"
if expect_rc "${name}" 1 && expect_contains "${name}" "${output}" "make 0123abc reachable"; then
	if [[ "${output}" == *"git fetch"* ]]; then fail "${name}" "hint names a fetch"; else pass "${name}"; fi
fi

for t in "${TESTS[@]}"; do
	go_output "${t}=SKIP"
	run_tool
	name="a skipped ${t} fails"
	if expect_rc "${name}" 1 && expect_contains "${name}" "${output}" "${t} did not run to a PASS"; then
		pass "${name}"
	fi

	go_output "${t}=absent"
	run_tool
	name="a missing ${t} fails"
	if expect_rc "${name}" 1 && expect_contains "${name}" "${output}" "${t} did not run to a PASS"; then
		pass "${name}"
	fi
done

go_output TestChangedPinsHaveUpgradeRecords=FAIL
run_tool STUB_GO_RC=1
name="a failing go test fails and shows its output"
if expect_rc "${name}" 1 &&
	expect_contains "${name}" "${output}" "--- FAIL: TestChangedPinsHaveUpgradeRecords"; then
	pass "${name}"
fi

if [[ "${failures}" -gt 0 ]]; then
	echo "${failures} test(s) failed" >&2
	exit 1
fi
echo "check-upgrade-records tests passed"
