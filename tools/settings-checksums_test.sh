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

# Guard the isolation between the <tool>_checksums blocks in .settings.yaml.
#
# Every block uses the same sub-keys (linux_amd64, darwin_arm64, ...) at the
# same indent, so a refresh script whose edit is not scoped to its own block
# rewrites all of them. update-chainsaw-checksums did exactly that until #2658:
# a single sed anchored on the arch key alone clobbered four blocks, and its
# post-check passed because it only asked whether the new value existed
# somewhere in the file.
#
# Two independent digests colliding is not a thing that happens, so any two
# blocks sharing a value means something wrote across a boundary. This runs in
# `make test-shell`, so a bad Renovate refresh is caught on the PR that carries
# it rather than in nightly UAT.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETTINGS_FILE="${SCRIPT_DIR}/../.settings.yaml"

if [[ ! -f "${SETTINGS_FILE}" ]]; then
  echo "FAIL: ${SETTINGS_FILE} not found" >&2
  exit 1
fi

# Discovery is deliberately awk, not python+yaml: this runs inside `make test`
# via test-shell, and PyYAML is not part of any documented setup step here. A
# missing module would abort the whole gate with ModuleNotFoundError, naming
# neither the package nor the remedy.
#
# Two counts are compared so a discovery bug cannot pass as a clean result. The
# grep counts every `*_checksums:` key at any indent; the awk parser only
# understands the 2-space/4-space shape the refresh scripts write. If a block
# ever moves, the parser stops seeing it -- and comparing against the grep is
# what turns that into a failure instead of a silently smaller comparison set.
declared=$(grep -cE '^[[:space:]]*[a-zA-Z_]+_checksums:[[:space:]]*$' "${SETTINGS_FILE}" || true)

parsed=$(awk '
  /^  [a-zA-Z_]+_checksums:[[:space:]]*$/ {
    block = $1; sub(/:$/, "", block); next
  }
  # Any column-0 line, or any other 2-space key, ends the current block.
  /^[^ ]/ { block = ""; next }
  /^  [a-zA-Z_]/ && !/^    / { block = ""; next }
  block != "" && /^    [a-zA-Z_0-9]+:[[:space:]]*.+$/ {
    key = $1; sub(/:$/, "", key)
    value = $2; gsub(/^\x27|\x27$/, "", value)
    print block, key, value
  }
' "${SETTINGS_FILE}")

parsed_blocks=$(printf '%s\n' "${parsed}" | awk 'NF {print $1}' | sort -u | wc -l | tr -d ' ')

if [[ "${declared}" -eq 0 ]]; then
  echo "FAIL: no '*_checksums' blocks found in ${SETTINGS_FILE}; the discovery" \
       "above is broken, not the data" >&2
  exit 1
fi

if [[ "${parsed_blocks}" -ne "${declared}" ]]; then
  echo "FAIL: ${declared} '*_checksums' key(s) declared but ${parsed_blocks}" \
       "parsed -- a block moved out of the 2-space/4-space shape this parser" \
       "and the refresh scripts both assume" >&2
  exit 1
fi

# Two independent digests do not collide, so the same value under the same
# sub-key in two blocks means something wrote across a boundary.
duplicates=$(printf '%s\n' "${parsed}" \
  | awk 'NF { print $2, $3, $1 }' \
  | sort \
  | awk '
      { key = $1 " " $2 }
      key == prev_key { printf "%s and %s share %s = %s\n", prev_block, $3, $1, $2 }
      { prev_key = key; prev_block = $3 }
    ')

if [[ -n "${duplicates}" ]]; then
  echo "FAIL: a refresh script wrote outside its own block:" >&2
  printf '%s\n' "${duplicates}" | sed 's/^/  /' >&2
  exit 1
fi

echo "PASS: ${parsed_blocks} checksum blocks hold distinct values"
