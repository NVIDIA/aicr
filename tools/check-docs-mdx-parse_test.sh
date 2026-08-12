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

# Unit harness for tools/check-docs-mdx-parse — the parser-level docs gate.
# Run directly: bash tools/check-docs-mdx-parse_test.sh
# Wired into CI via `make test` (test-shell target, runs tools/*_test.sh).
#
# Hermetic with respect to docs/: every assertion runs against fixture .md
# files in a temp dir. It is NOT hermetic with respect to the network — the
# first run resolves the pinned MDX toolchain via npm. Under CI that is
# required; locally a missing node/npm is a skip, matching the driver.
#
# What this pins:
#   1. The #2050 regression — '(gate <= 2,000)' must fail, with the parser's
#      own "Unexpected character `=`" message that Fern emits verbatim.
#   2. The two hazard classes tools/check-docs-mdx cannot see: a stray closing
#      tag and an unclosed fragment. These are why the parse gate exists.
#   3. The false-positive direction — '< 500' style prose must PASS, so the
#      gate never rejects content `fern generate --docs` accepts.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="${SCRIPT_DIR}/check-docs-mdx-parse"

if [[ -z "${CI:-}" && -z "${GITHUB_ACTIONS:-}" ]]; then
    if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
        echo "SKIP: node/npm not available — check-docs-mdx-parse tests are CI-enforced"
        exit 0
    fi
fi

TMPDIR_TEST="$(mktemp -d)"
trap 'rm -rf "${TMPDIR_TEST}"' EXIT

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1 — $2"; fails=$((fails + 1)); }

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
    if [[ "${RC}" == "0" ]]; then pass "$1"; else fail "$1" "want rc=0, got ${RC}: ${OUT}"; fi
}
check_contains() { # <name> <needle>
    if [[ "${OUT}" == *"$2"* ]]; then pass "$1"; else fail "$1" "expected to contain: $2"; fi
}
check_absent() { # <name> <needle>
    if [[ "${OUT}" != *"$2"* ]]; then pass "$1"; else fail "$1" "expected NOT to contain: $2"; fi
}

# --- Fixture 1: the #2050 regression that broke the docs publish. ---
DIR_BREAKER="${TMPDIR_TEST}/breaker"
mkdir -p "${DIR_BREAKER}"
cat >"${DIR_BREAKER}/bare-lt.md" <<'MD'
# Bare less-than-or-equal breaks MDX

The Dynamo inference counterpart passes with 579.70 ms TTFT p99 (gate <= 2,000).
MD

run "${DIR_BREAKER}"
check_rc_nonzero "breaker-exits-nonzero"
check_contains   "breaker-cites-file-line-col" "bare-lt.md:3:"
# Fern emits this exact string; asserting on it is what proves the local gate
# and the publish step are running the same parser.
check_contains   "breaker-parser-message" 'Unexpected character `=` (U+003D) before name'

# --- Fixture 2: hazards tools/check-docs-mdx cannot detect. ---
# A stray closing tag and an unclosed fragment both need parser state to
# recognize. The bash checker allowlists '/' and '>' after '<' and passes both;
# this gate must not.
DIR_TAGS="${TMPDIR_TEST}/tags"
mkdir -p "${DIR_TAGS}"
cat >"${DIR_TAGS}/stray-close.md" <<'MD'
# Stray closing tag

The renderer emits a </div> with no opening tag.
MD
cat >"${DIR_TAGS}/open-fragment.md" <<'MD'
# Unclosed fragment

An empty fragment <> left open in prose.
MD

run "${DIR_TAGS}"
check_rc_nonzero "tags-exits-nonzero"
check_contains   "stray-close-reported" "stray-close.md:"
check_contains   "open-fragment-reported" "open-fragment.md:"

# --- Fixture 3: MDX-safe prose must PASS (no false positives). ---
# '<' followed by whitespace stays literal text in MDX. If this fixture ever
# fails, the gate has become stricter than the publish step and will reject
# content Fern would have rendered.
DIR_SAFE="${TMPDIR_TEST}/safe"
mkdir -p "${DIR_SAFE}"
cat >"${DIR_SAFE}/safe-prose.md" <<'MD'
# MDX-safe prose

Embed the cause only when status < 500, since 4xx carries client feedback.

Recipes targeting Kubernetes < 1.15 must enable the feature gate explicitly.

Guards fire before any cluster mutation, so skips are cheap (typically < 10 s).

The wrapped form `<= 2,000` and a self-closed <br /> are both fine.

So is a <span>styled</span> word and a <Component /> with <Foo bar="baz" />.

| Gate | Bound |
| ---- | ----- |
| TTFT | `<= 2,000` ms |
MD

run "${DIR_SAFE}"
check_rc_zero "safe-exits-zero"
check_absent  "safe-no-violation" "MDX-PARSE:"

# --- Fixture 4: YAML frontmatter is stripped before parsing. ---
# Fern removes frontmatter before MDX sees it, so a title containing '<=' is
# valid. Without remark-frontmatter the delimiters parse as a thematic break and
# the body as prose, and this file was rejected while Fern published it — a
# false positive on content that ships.
DIR_FM="${TMPDIR_TEST}/frontmatter"
mkdir -p "${DIR_FM}"
cat >"${DIR_FM}/with-frontmatter.md" <<'MD'
---
title: Latency gate <= 2,000 ms
description: TTFT p99 under <= 2,000 ms at 256 concurrency
---

# Page with frontmatter

Body text with a wrapped `<= 2,000` gate.
MD

run "${DIR_FM}"
check_rc_zero "frontmatter-exits-zero"
check_absent  "frontmatter-no-violation" "MDX-PARSE:"

# --- Fixture 5: a hazard AFTER frontmatter is still caught, at the true line. ---
# Stripping must not shift reported line numbers, or every diagnostic in a
# frontmatter file points at the wrong place.
DIR_FMH="${TMPDIR_TEST}/frontmatter-hazard"
mkdir -p "${DIR_FMH}"
cat >"${DIR_FMH}/fm-hazard.md" <<'MD'
---
title: Safe here <= 1
---

# Page

Body hazard gate <= 5 sits on line 7.
MD

run "${DIR_FMH}"
check_rc_nonzero "frontmatter-hazard-exits-nonzero"
check_contains   "frontmatter-hazard-true-line" "fm-hazard.md:7:"

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All check-docs-mdx-parse tests passed"
