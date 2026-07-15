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

set -euo pipefail

readonly MAX_CHECKSUM_BYTES=1048576
AICR_NETWORK_TIMEOUT_SECONDS="${AICR_NETWORK_TIMEOUT_SECONDS:-120}"
if [[ ! "${AICR_NETWORK_TIMEOUT_SECONDS}" =~ ^[0-9]+$ ]] ||
  ((AICR_NETWORK_TIMEOUT_SECONDS < 1 || AICR_NETWORK_TIMEOUT_SECONDS > 600)); then
  echo "::error::AICR_NETWORK_TIMEOUT_SECONDS must be an integer from 1 through 600" >&2
  exit 1
fi
readonly AICR_NETWORK_TIMEOUT_SECONDS

temporary_files=()
cleanup() {
  local path
  for path in "${temporary_files[@]}"; do
    rm -f -- "${path}"
  done
}
trap cleanup EXIT

die() {
  echo "::error::$*" >&2
  exit 1
}

[[ $# -eq 2 ]] || die "usage: publish-homebrew.sh <formula> <tap-directory>"
readonly FORMULA="$1"
readonly TAP_DIRECTORY="$2"
RELEASE_TAG="${RELEASE_TAG:-}"
[[ "${RELEASE_TAG}" != *$'\n'* && "${RELEASE_TAG}" != *$'\r'* ]] || die "RELEASE_TAG must be single-line"
[[ "${RELEASE_TAG}" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
  die "RELEASE_TAG must be stable vMAJOR.MINOR.PATCH"
readonly RELEASE_TAG
readonly RELEASE_VERSION="${RELEASE_TAG#v}"
readonly URL_PREFIX="https://github.com/NVIDIA/aicr/releases/download/${RELEASE_TAG}/"

[[ -f "${FORMULA}" && ! -L "${FORMULA}" ]] || die "candidate formula must be a regular file"
[[ -d "${TAP_DIRECTORY}" && ! -L "${TAP_DIRECTORY}" ]] || die "tap directory must be a real directory"
[[ -d "${TAP_DIRECTORY}/Formula" && ! -L "${TAP_DIRECTORY}/Formula" ]] ||
  die "tap Formula directory is missing or unsafe"
readonly DESTINATION="${TAP_DIRECTORY}/Formula/aicr.rb"
[[ -f "${DESTINATION}" && ! -L "${DESTINATION}" ]] || die "existing Formula/aicr.rb is missing or unsafe"

extract_version() {
  local formula="$1" matches
  matches="$(grep -E '^[[:space:]]*version "[^"]+"[[:space:]]*$' "${formula}" || true)"
  [[ "$(wc -l <<<"${matches}" | tr -d ' ')" == "1" && -n "${matches}" ]] ||
    die "formula must contain exactly one version declaration"
  sed -E 's/^[[:space:]]*version "([^"]+)"[[:space:]]*$/\1/' <<<"${matches}"
}

formula_version="$(extract_version "${FORMULA}")"
[[ "${formula_version}" == "${RELEASE_VERSION}" ]] || die "candidate formula version does not match RELEASE_TAG"

pairs_file="$(mktemp "${TMPDIR:-/tmp}/aicr-homebrew-pairs.XXXXXX")"
checksums_file="$(mktemp "${TMPDIR:-/tmp}/aicr-homebrew-checksums.XXXXXX")"
temporary_files+=("${pairs_file}" "${checksums_file}")

awk '
  /^[[:space:]]*url "[^"]+"[[:space:]]*$/ {
    if (pending) exit 2
    url=$0
    sub(/^[[:space:]]*url "/, "", url)
    sub(/"[[:space:]]*$/, "", url)
    pending=1
    next
  }
  /^[[:space:]]*sha256 "[0-9A-Fa-f]+"[[:space:]]*$/ {
    if (!pending) exit 3
    checksum=$0
    sub(/^[[:space:]]*sha256 "/, "", checksum)
    sub(/"[[:space:]]*$/, "", checksum)
    print url "\t" tolower(checksum)
    pending=0
    next
  }
  END { if (pending) exit 4 }
' "${FORMULA}" >"${pairs_file}" || die "formula URL and checksum pairs are malformed"

[[ "$(wc -l <"${pairs_file}" | tr -d ' ')" == "4" ]] ||
  die "formula must contain exactly four archive URL and checksum pairs"
[[ -z "$(cut -f1 "${pairs_file}" | sort | uniq -d | head -n 1)" ]] || die "formula contains a duplicate archive URL"

expected_archives=(
  "aicr_${RELEASE_VERSION}_darwin_amd64.tar.gz"
  "aicr_${RELEASE_VERSION}_darwin_arm64.tar.gz"
  "aicr_${RELEASE_VERSION}_linux_amd64.tar.gz"
  "aicr_${RELEASE_VERSION}_linux_arm64.tar.gz"
)
for archive in "${expected_archives[@]}"; do
  expected_url="${URL_PREFIX}${archive}"
  [[ "$(awk -F '\t' -v url="${expected_url}" '$1 == url { count++ } END { print count+0 }' "${pairs_file}")" == "1" ]] ||
    die "formula must reference ${archive} exactly once"
  formula_checksum="$(awk -F '\t' -v url="${expected_url}" '$1 == url { print $2 }' "${pairs_file}")"
  [[ "${formula_checksum}" =~ ^[0-9a-f]{64}$ ]] || die "formula checksum is malformed for ${archive}"
done

timeout --foreground "${AICR_NETWORK_TIMEOUT_SECONDS}s" \
  curl --connect-timeout 10 --max-time 60 --retry 2 --retry-all-errors \
  --max-filesize "${MAX_CHECKSUM_BYTES}" \
  --fail --silent --show-error --location \
  "${URL_PREFIX}aicr_checksums.txt" >"${checksums_file}" || die "failed to fetch public release checksums"
checksum_bytes="$(wc -c <"${checksums_file}" | tr -d ' ')"
[[ "${checksum_bytes}" =~ ^[0-9]+$ ]] || die "failed to measure public checksum manifest"
((checksum_bytes <= MAX_CHECKSUM_BYTES)) || die "public checksum manifest exceeds size limit"

for archive in "${expected_archives[@]}"; do
  expected_url="${URL_PREFIX}${archive}"
  formula_checksum="$(awk -F '\t' -v url="${expected_url}" '$1 == url { print $2 }' "${pairs_file}")"
  manifest_count="$(awk -v archive="${archive}" '$2 == archive { count++ } END { print count+0 }' "${checksums_file}")"
  [[ "${manifest_count}" == "1" ]] || die "public checksum manifest must contain ${archive} exactly once"
  public_checksum="$(awk -v archive="${archive}" '$2 == archive { print tolower($1) }' "${checksums_file}")"
  [[ "${public_checksum}" =~ ^[0-9a-f]{64}$ ]] || die "public checksum is malformed for ${archive}"
  [[ "${formula_checksum}" == "${public_checksum}" ]] || die "formula checksum mismatch for ${archive}"
done

existing_version="$(extract_version "${DESTINATION}")"
[[ "${existing_version}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
  die "existing formula has an invalid stable version"

compare_versions() {
  local left="$1" right="$2" index
  local -a left_parts right_parts
  IFS=. read -r -a left_parts <<<"${left}"
  IFS=. read -r -a right_parts <<<"${right}"
  for index in 0 1 2; do
    if ((10#${left_parts[${index}]} < 10#${right_parts[${index}]})); then echo -1; return; fi
    if ((10#${left_parts[${index}]} > 10#${right_parts[${index}]})); then echo 1; return; fi
  done
  echo 0
}

comparison="$(compare_versions "${existing_version}" "${RELEASE_VERSION}")"
if [[ "${comparison}" == "1" ]]; then
  die "existing Homebrew formula is newer than this release"
fi
if [[ "${comparison}" == "0" ]]; then
  cmp -s "${FORMULA}" "${DESTINATION}" || die "existing formula conflicts with this release tag"
  echo "changed=false"
  exit 0
fi

replacement="$(mktemp "${DESTINATION}.tmp.XXXXXX")"
temporary_files+=("${replacement}")
install -m 0644 "${FORMULA}" "${replacement}"
mv -f -- "${replacement}" "${DESTINATION}"
echo "changed=true"
