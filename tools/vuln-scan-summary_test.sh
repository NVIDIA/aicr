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

# Hermetic tests for tools/vuln-scan-summary, the weekly image scan's Slack
# message body. Run directly: bash tools/vuln-scan-summary_test.sh
# Wired into CI via `make test` (test-shell target).
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUMMARY="${SCRIPT_DIR}/vuln-scan-summary"

WORK_DIR="$(mktemp -d)" || exit 1
trap 'rm -rf "${WORK_DIR}"' EXIT

fails=0
fail() {
  echo "FAIL: $*" >&2
  fails=$((fails + 1))
}

# result writes one per-image file in the scan job's format.
result() {
  local dir="$1" name="$2" critical="$3" high="$4" vex="$5" ids="${6:-}"
  mkdir -p "${dir}"
  {
    echo "${name}: ${critical} critical, ${high} high, 2 medium, 0 low, 0 negligible (${vex} VEX-suppressed)"
    [[ -n "${ids}" ]] && echo "  HIGH+: ${ids}"
  } > "${dir}/${name}.txt"
}

expect() {
  local name="$1" want="$2" got="$3"
  if [[ "${got}" != "${want}" ]]; then
    fail "${name}"$'\n'"--- want"$'\n'"${want}"$'\n'"--- got"$'\n'"${got}"
  fi
}

# Clean images collapse into one line, singular and plural.
dir="${WORK_DIR}/clean"
result "${dir}" aicr 0 0 0
result "${dir}" aicrd 0 0 0
expect "two clean images" "• 2 images with no critical or high findings" "$("${SUMMARY}" "${dir}" </dev/null)"
rm "${dir}/aicrd.txt"
expect "one clean image" "• 1 image with no critical or high findings" "$("${SUMMARY}" "${dir}" </dev/null)"

# IDs are deduplicated across packages, alias lists never split, the list
# stops at MAX_IDS with a remainder count, and VEX suppressions are shown.
dir="${WORK_DIR}/ids"
result "${dir}" nginx 2 5 3 "libssl3 CVE-1 (CVE-1), openssl CVE-1 (CVE-1), nginx BIT-2 (CVE-2,CVE-22), nginx BIT-2 (CVE-2,CVE-22), libxml2 CVE-3, perl CVE-4, dpkg CVE-5, libc6 CVE-6, libc-bin CVE-6"
expect "dedupe and limit" \
  "• nginx: 2 critical, 5 high (3 VEX-suppressed) | CVE-1, BIT-2, CVE-3, CVE-4, CVE-5 +1 more" \
  "$("${SUMMARY}" "${dir}" </dev/null)"
expect "MAX_IDS" \
  "• nginx: 2 critical, 5 high (3 VEX-suppressed) | CVE-1, BIT-2 +4 more" \
  "$(MAX_IDS=2 "${SUMMARY}" "${dir}" </dev/null)"
expect "under the limit has no remainder" \
  "• nginx: 2 critical, 5 high (3 VEX-suppressed) | CVE-1, BIT-2, CVE-3, CVE-4, CVE-5, CVE-6" \
  "$(MAX_IDS=6 "${SUMMARY}" "${dir}" </dev/null)"

# Most critical first, then most high, then by name; clean images last; then
# the scans that did not complete, as given.
dir="${WORK_DIR}/order"
result "${dir}" b-image 1 9 0 "pkg GHSA-b"
result "${dir}" a-image 1 9 0 "pkg GHSA-a"
result "${dir}" few-high 0 1 0 "pkg GHSA-c"
result "${dir}" most-critical 4 2 0 "pkg GHSA-d"
result "${dir}" clean 0 0 0
want="• most-critical: 4 critical, 2 high | GHSA-d
• a-image: 1 critical, 9 high | GHSA-a
• b-image: 1 critical, 9 high | GHSA-b
• few-high: 0 critical, 1 high | GHSA-c
• 1 image with no critical or high findings
• busybox: scan did not complete (failure)"
expect "ordering and incomplete scans" "${want}" \
  "$(printf 'busybox: scan did not complete (failure)\n\n' | "${SUMMARY}" "${dir}")"

# A result line it cannot parse is reported, never counted clean.
dir="${WORK_DIR}/unreadable"
mkdir -p "${dir}"
echo "garbled" > "${dir}/broken.txt"
expect "unreadable result" "• garbled: unreadable result line" "$("${SUMMARY}" "${dir}" </dev/null)"

# No results at all prints nothing.
mkdir -p "${WORK_DIR}/empty"
expect "no results" "" "$("${SUMMARY}" "${WORK_DIR}/empty" </dev/null)"

# A bad limit is a usage error.
if MAX_IDS=0 "${SUMMARY}" "${WORK_DIR}/empty" </dev/null 2>/dev/null; then
  fail "MAX_IDS=0 was accepted"
fi

if [[ ${fails} -gt 0 ]]; then
  echo "${fails} vuln-scan-summary test(s) failed" >&2
  exit 1
fi
echo "vuln-scan-summary: all tests passed"
