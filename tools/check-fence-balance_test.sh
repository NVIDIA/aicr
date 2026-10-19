#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

# Cases for tools/check-fence-balance.
#
# This guard protects THIRD_PARTY_NOTICES.md, which is generated at release time
# and never committed, so there is no artifact in the tree to assert against.
# Driving the guard through tools/generate-notices would cost a full go-licenses
# walk across four platforms, so the checker is exercised directly here.
#
# The cases that matter are the REJECTIONS. A fence guard that silently never
# matches passes every input, which is indistinguishable from "the file is
# fine" — so each corruption mode is pinned with the message it must produce.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TOOL="${ROOT}/tools/check-fence-balance"
TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

failures=0

# expect_ok <name> <content>
expect_ok() {
    local name="$1" content="$2" output
    printf '%s\n' "${content}" > "${TMP}/case.md"
    if output=$("${TOOL}" "${TMP}/case.md" 2>&1); then
        echo "ok   ${name}"
    else
        echo "FAIL ${name}: expected balanced, got: ${output}" >&2
        failures=$((failures + 1))
    fi
}

# expect_fail <name> <expected-substring> <content>
expect_fail() {
    local name="$1" want="$2" content="$3" output
    printf '%s\n' "${content}" > "${TMP}/case.md"
    if output=$("${TOOL}" "${TMP}/case.md" 2>&1); then
        echo "FAIL ${name}: expected rejection, but it passed" >&2
        failures=$((failures + 1))
        return
    fi
    if [[ "${output}" != *"${want}"* ]]; then
        echo "FAIL ${name}: want message containing '${want}', got: ${output}" >&2
        failures=$((failures + 1))
        return
    fi
    echo "ok   ${name}"
}

# --- accepted -------------------------------------------------------------

expect_ok "balanced block" 'prose
```text
LICENSE BODY
```
more prose'

expect_ok "consecutive blocks" '```text
first
```
```text
second
```'

# The wrapper generate-notices emits uses more backticks than the content can
# contain, which is precisely how embedded fences are survived.
expect_ok "longer opener survives a shorter inner fence" '````text
inner ``` does not close the wrapper
````'

# A line starting with backticks but carrying punctuation is prose, not a
# delimiter; treating it as one would produce false rejections on license text.
expect_ok "backticked prose is not a delimiter" '```text
```not-a-fence!
```'

expect_ok "trailing whitespace on the closer" '```text
body
```   '

# --- rejected -------------------------------------------------------------

# The real corruption mode: upstream license text contains a bare ``` which
# closes the wrapper early, so everything after it renders inverted.
expect_fail "license text escapes its block" "closing fence with no opener" '```text
license body containing
```
stray prose that now renders as markdown
```'

expect_fail "unterminated block" "unterminated code fence" '```text
opened and never closed'

# A generator that produced no blocks at all has failed silently upstream; the
# output would look like plain prose rather than a notices file.
expect_fail "no fenced blocks at all" "no fenced blocks found" 'just prose
with no fences anywhere'

# --- argument handling ----------------------------------------------------

if "${TOOL}" >/dev/null 2>&1; then
    echo "FAIL missing argument: expected non-zero exit" >&2
    failures=$((failures + 1))
else
    echo "ok   missing argument is rejected"
fi

if "${TOOL}" "${TMP}/does-not-exist.md" >/dev/null 2>&1; then
    echo "FAIL missing file: expected non-zero exit" >&2
    failures=$((failures + 1))
else
    echo "ok   missing file is rejected"
fi

if [[ "${failures}" -ne 0 ]]; then
    echo "check-fence-balance: ${failures} case(s) failed" >&2
    exit 1
fi

echo "check-fence-balance rejects every corruption mode and accepts valid input"
