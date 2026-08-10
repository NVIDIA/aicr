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

# --- Fixture 0: the script must parse. ---
# The awk programs live inside single-quoted bash strings, so one apostrophe in
# a comment ("Fern's") silently terminates the quote and turns the rest of the
# program into shell tokens. `bash -n` catches that before any behavioral
# assertion has to.
if bash -n "${CHECK}" 2>/dev/null; then
    pass "script-parses"
else
    fail "script-parses" "bash -n reported a syntax error (an apostrophe inside the awk block?)"
fi

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

A valid element like <br /> stays clean.
MD

run "${DIR_SAFE}"
check_rc_zero  "wrapped-lt-exits-zero"
check_absent   "wrapped-lt-no-violation" "bare < not starting a valid tag"

# --- Fixture 2b: '<' followed by WHITESPACE. Must NOT be reported. ---
# Verified against @mdx-js/mdx: micromark only enters JSX-tag mode when a
# name-ish character follows '<' immediately, so '< 500' and friends stay
# literal text and parse cleanly. This checker is a strict subset of the real
# parser, so reporting them here would be a false positive — it would force
# contributors to backtick prose that `fern generate --docs` accepts.
DIR_WS="${TMPDIR_TEST}/whitespace"
mkdir -p "${DIR_WS}"
cat >"${DIR_WS}/lt-space.md" <<'MD'
# Less-than followed by whitespace is literal text

Embed the cause only when status < 500, since 4xx carries client feedback.

Recipes targeting Kubernetes < 1.15 must enable the feature gate explicitly.

Guards fire before any cluster mutation, so skips are cheap (typically < 10 s).
MD

run "${DIR_WS}"
check_rc_zero "lt-space-exits-zero"
check_absent  "lt-space-no-violation" "bare < not starting a valid tag"
check_absent  "lt-space-no-word-tag"  "bare <word> tag"

# --- Fixture 2d: well-formed JSX must NOT be reported. ---
# MDX accepts self-closing components and balanced elements, and Fern's own
# component set (Cards, Tabs, Accordions) is authored that way. Check 5 used to
# flag every '<' followed by a letter, so it rejected all of these. A line
# showing evidence of well-formed JSX ('/>' or '</') is now left to the parse
# gate, which can actually tell whether the tags balance.
DIR_JSX="${TMPDIR_TEST}/jsx"
mkdir -p "${DIR_JSX}"
cat >"${DIR_JSX}/valid-jsx.md" <<'MD'
# Well-formed JSX is valid MDX

A <span>styled</span> word renders fine.

A self-closing <Component /> renders fine.

So does <Foo bar="baz" /> with attributes.

And a void element like <br /> stays clean.
MD

run "${DIR_JSX}"
check_rc_zero "valid-jsx-exits-zero"
check_absent  "valid-jsx-no-word-tag"  "bare <word> tag"
check_absent  "valid-jsx-no-void"      "non-self-closing void element"
check_absent  "valid-jsx-no-bare-lt"   "bare < not starting a valid tag"

# --- Fixture 2e: an unbalanced placeholder is still caught. ---
# Narrowing check 5 must not blind it to the case it exists for: a bare
# '<word>' placeholder on a line with no JSX evidence.
DIR_PLACEHOLDER="${TMPDIR_TEST}/placeholder"
mkdir -p "${DIR_PLACEHOLDER}"
cat >"${DIR_PLACEHOLDER}/placeholder.md" <<'MD'
# Bare placeholder

Pass <name> to select the component you want to bundle.
MD

run "${DIR_PLACEHOLDER}"
check_rc_nonzero "placeholder-exits-nonzero"
check_contains   "placeholder-reported" "MDX: bare <word> tag outside code fence"

# --- Fixture 2f: YAML frontmatter is not scanned; content after it still is. ---
# Fern strips frontmatter before MDX, so '<=' in a title is valid. Skipping it
# by line number (rather than rewriting the file) keeps later diagnostics
# pointing at the true line.
DIR_FM="${TMPDIR_TEST}/frontmatter"
mkdir -p "${DIR_FM}"
cat >"${DIR_FM}/fm-safe.md" <<'MD'
---
title: Latency gate <= 2,000 ms
description: TTFT p99 under <= 2,000 ms
---

# Page

Body text with a wrapped `<= 2,000` gate.
MD
cat >"${DIR_FM}/fm-hazard.md" <<'MD'
---
title: Safe here <= 1
---

# Page

Body hazard gate <= 5 sits on line 7.
MD

run "${DIR_FM}"
check_rc_nonzero "frontmatter-hazard-exits-nonzero"
check_absent     "frontmatter-title-not-flagged" "fm-safe.md"
check_contains   "frontmatter-hazard-true-line"  "fm-hazard.md:7:"

# --- Fixture 2c: '<30' must produce exactly ONE diagnostic, not two. ---
# Check 5 owns letter-prefixed names and check 6 owns everything that cannot
# start a name, so a digit-prefixed sequence belongs to check 6 alone. When
# check 5 also matched digits, one source token emitted two lines and
# double-incremented the error count.
DIR_DIGIT="${TMPDIR_TEST}/digit"
mkdir -p "${DIR_DIGIT}"
cat >"${DIR_DIGIT}/digit-tag.md" <<'MD'
# Digit-prefixed angle bracket

Cold start completes in <30 s on a warm cache.
MD

run "${DIR_DIGIT}"
check_rc_nonzero "digit-tag-exits-nonzero"
check_contains   "digit-tag-reported" "MDX: bare < not starting a valid tag"
check_absent     "digit-tag-not-double-reported" "MDX: bare <word> tag"
if [[ "$(grep -c 'digit-tag.md:3:' <<<"${OUT}")" == "1" ]]; then
    pass "digit-tag-single-diagnostic"
else
    fail "digit-tag-single-diagnostic" "want exactly 1 diagnostic for line 3, got $(grep -c 'digit-tag.md:3:' <<<"${OUT}")"
fi

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
