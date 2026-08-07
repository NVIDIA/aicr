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

# Unit harness for tools/check-docs-mdx check 6 — the fail-closed allowlist
# rule for a bare '<' not followed by a valid JSX name-start.
# Run directly: bash tools/check-docs-mdx_test.sh
# Wired into CI via `make test` (test-shell target, runs tools/*_test.sh).
#
# Hermetic: builds fixture .md files in a temp dir and runs the checker against
# them, so no docs/ content is read and nothing on disk is mutated. The
# fixtures pin the regression from issue #2050 (Fern's MDX parser rejects
# '(gate <= 2,000)' with "Unexpected character = (U+003D) before name", which
# the denylist-era checker reported as OK) and guard against false positives
# when the same token is safely wrapped in inline or fenced code.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="${SCRIPT_DIR}/check-docs-mdx"

TMPDIR_TEST="$(mktemp -d)"
trap 'rm -rf "${TMPDIR_TEST}"' EXIT

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1 — $2"; fails=$((fails + 1)); }

# run <dir>: capture combined stdout+stderr into $OUT and exit code into $RC.
OUT=""
RC=0
run() {
    OUT="$("${CHECK}" "$1" 2>&1)"
    RC=$?
}

check_rc_nonzero() { # <name>
    if [[ "${RC}" != "0" ]]; then pass "$1"; else fail "$1" "want nonzero rc, got 0"; fi
}
check_rc_zero() { # <name>
    if [[ "${RC}" == "0" ]]; then pass "$1"; else fail "$1" "want rc=0, got ${RC}"; fi
}
check_contains() { # <name> <needle>
    if [[ "${OUT}" == *"$2"* ]]; then pass "$1"; else fail "$1" "expected to contain: $2"; fi
}
check_absent() { # <name> <needle>
    if [[ "${OUT}" != *"$2"* ]]; then pass "$1"; else fail "$1" "expected NOT to contain: $2"; fi
}

# --- Fixture 1: the #2050 regression — a bare '<= ' outside any code span. ---
# The checker MUST fail closed here. If check 6 is removed, this token is not
# a void element (check 1), autolink (check 4), or <name-start> tag (check 5),
# so nothing else flags it and this assertion fails — proving the rule is what
# catches it.
DIR_HAZARD="${TMPDIR_TEST}/hazard"
mkdir -p "${DIR_HAZARD}"
cat >"${DIR_HAZARD}/bare-lt.md" <<'MD'
# Bare less-than-or-equal hazard

The TTFT p99 stays low (gate <= 2,000) under the calibrated inference gate.
MD

run "${DIR_HAZARD}"
check_rc_nonzero "bare-lt-exits-nonzero"
check_contains   "bare-lt-reported" "MDX: bare < not starting a valid tag"
check_contains   "bare-lt-line-cited" "bare-lt.md:3:"

# --- Fixture 2: the SAME token, but safely wrapped. No false positive. ---
# Inline backtick span and fenced code block both hide the '<=' from every
# check, so a clean fixture built only from wrapped hazards must pass.
DIR_SAFE="${TMPDIR_TEST}/safe"
mkdir -p "${DIR_SAFE}"
cat >"${DIR_SAFE}/wrapped-lt.md" <<'MD'
# Wrapped less-than-or-equal is safe

The TTFT p99 stays low (gate `<= 2,000`) under the calibrated inference gate.

```text
inference-perf TTFT p99 gate <= 2,000 ms
```

A valid element like <br /> and a closing </div> must also stay clean.
MD

run "${DIR_SAFE}"
check_rc_zero  "wrapped-lt-exits-zero"
check_absent   "wrapped-lt-no-violation" "bare < not starting a valid tag"

# --- Fixture 3: '<= ' inside a tilde (~~~) fenced code block. No false pos. ---
# CommonMark honors ~~~ fences as code; the checker must skip their contents
# just like ``` fences, so the hazard token stays hidden.
DIR_TILDE="${TMPDIR_TEST}/tilde"
mkdir -p "${DIR_TILDE}"
cat >"${DIR_TILDE}/tilde-fence.md" <<'MD'
# Tilde fence hides the hazard

~~~
inference-perf TTFT p99 gate <= 2,000 ms
~~~
MD

run "${DIR_TILDE}"
check_rc_zero  "tilde-fence-exits-zero"
check_absent   "tilde-fence-no-violation" "bare < not starting a valid tag"

# --- Fixture 4: '<= ' inside a double-backtick (``…``) span. No false pos. ---
# CommonMark closes an N-backtick span at the next run of exactly N backticks;
# the checker strips spans of any run length, so the '<=' inside is code.
DIR_DBT="${TMPDIR_TEST}/dbt"
mkdir -p "${DIR_DBT}"
cat >"${DIR_DBT}/double-backtick.md" <<'MD'
# Double-backtick span hides the hazard

Use ``(gate <= 2,000)`` to express the inference gate inline.
MD

run "${DIR_DBT}"
check_rc_zero  "double-backtick-exits-zero"
check_absent   "double-backtick-no-violation" "bare < not starting a valid tag"

# --- Fixture 5: a triple-backtick fence that CONTAINS a lone double-backtick. ---
# The fence-length rule requires the closing run to be the SAME char and at
# least as long as the opener, so an inner ``` shorter run (or the lone ``)
# must NOT close the fence early and expose the hazard on a later line.
DIR_LEN="${TMPDIR_TEST}/fencelen"
mkdir -p "${DIR_LEN}"
cat >"${DIR_LEN}/fence-length.md" <<'MD'
# Fence-length rule keeps the block open

```
here is a lone `` double backtick inside the block
inference-perf TTFT p99 gate <= 2,000 ms
```
MD

run "${DIR_LEN}"
check_rc_zero  "fence-length-exits-zero"
check_absent   "fence-length-no-violation" "bare < not starting a valid tag"

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All check-docs-mdx tests passed"
