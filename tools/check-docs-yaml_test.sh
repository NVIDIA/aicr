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

# Unit harness for tools/check-docs-yaml — the YAML-in-Markdown docs gate.
# Run directly: bash tools/check-docs-yaml_test.sh
# Wired into CI via `make test` (test-shell target, runs tools/*_test.sh).
#
# Hermetic with respect to docs/: every assertion runs against fixture .md or
# .mdx files in a temp directory. It is NOT hermetic with respect to the
# network — the first run resolves the pinned docs toolchain via npm. Under CI
# that is required; locally a missing node/npm is a skip, matching the driver.
#
# The regression fixture pins issue #2308: `...` is YAML's explicit document
# end marker, so more content without a new `---` document must be rejected.
# Other fixtures ensure the Markdown parser finds real fences, valid partial
# examples stay useful, diagnostics point to source lines, and one bad block
# never prevents later failures from being reported.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="${SCRIPT_DIR}/check-docs-yaml"

if [[ -z "${CI:-}" && -z "${GITHUB_ACTIONS:-}" ]]; then
    if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
        echo "SKIP: node/npm not available — check-docs-yaml tests are CI-enforced"
        exit 0
    fi
fi

TMPDIR_TEST="$(mktemp -d)"
trap 'rm -rf "${TMPDIR_TEST}"' EXIT

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1 — $2"; fails=$((fails + 1)); }

# run <dir> [<dir> ...]: capture combined output and the checker exit code.
OUT=""
RC=0
run() {
    OUT="$("${CHECK}" "$@" 2>&1)"
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

# --- Fixture 1: valid complete and partial YAML in every supported fence. ---
# This covers .md and .mdx discovery, yaml and yml labels, backtick and tilde
# fences, longer fences, and CommonMark's three-space-indented fence form.
DIR_VALID="${TMPDIR_TEST}/valid"
mkdir -p "${DIR_VALID}"
cat >"${DIR_VALID}/complete.md" <<'MD'
# Complete Kubernetes object

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: example
data:
  enabled: "true"
```

````yaml
features:
  - training
  - monitoring
````

   ```yaml
   nested:
     enabled: true
   ```
MD
cat >"${DIR_VALID}/fragment.mdx" <<'MDX'
# Useful partial configuration

~~~yml
resourcesPerNode:
  limits:
    nvidia.com/gpu: 8
~~~
MDX

run "${DIR_VALID}"
check_rc_zero "valid-fences-exit-zero"
check_contains "valid-fences-print-summary" "OK:"
check_absent "valid-fences-have-no-diagnostic" "DOC-YAML:"

# --- Fixture 2: every document in a valid multi-document stream parses. ---
# A terminal `...` is legal, and a following document is legal when it begins
# with `---`. The checker must reject the missing start marker, not `...` itself.
DIR_MULTI="${TMPDIR_TEST}/multi-document"
mkdir -p "${DIR_MULTI}"
cat >"${DIR_MULTI}/multi.md" <<'MD'
# Multiple YAML documents

```yaml
---
name: first
...
---
name: second
...
```
MD

run "${DIR_MULTI}"
check_rc_zero "multi-document-exits-zero"
check_contains "multi-document-prints-summary" "OK:"

# --- Fixture 3: malformed content in non-YAML fences is ignored. ---
# A valid YAML block keeps this from passing merely because the checker found
# no blocks at all.
DIR_NON_YAML="${TMPDIR_TEST}/non-yaml"
mkdir -p "${DIR_NON_YAML}"
cat >"${DIR_NON_YAML}/languages.md" <<'MD'
# Other languages

```text
not: [valid YAML
```

```json
{not valid JSON either
```

```yaml-template
{{ not valid YAML either
```

```yaml
checked: true
```
MD

run "${DIR_NON_YAML}"
check_rc_zero "non-yaml-fences-exit-zero"
check_contains "non-yaml-fences-print-summary" "OK:"
check_absent "non-yaml-fences-have-no-diagnostic" "DOC-YAML:"

# --- Fixture 4: finding zero YAML blocks fails closed. ---
# A scanner that silently stops finding fences must not make the gate green.
DIR_ZERO="${TMPDIR_TEST}/zero-yaml"
mkdir -p "${DIR_ZERO}"
cat >"${DIR_ZERO}/plain.md" <<'MD'
# No YAML examples

```text
not: [valid YAML
```
MD

run "${DIR_ZERO}"
check_rc_nonzero "zero-yaml-blocks-exit-nonzero"

# --- Fixture 5: the issue #2308 explicit-document-end regression. ---
# Parsing only the first document, or stopping at `...`, incorrectly passes
# this block. A full-stream parse sees the second mapping lacks a `---` start.
DIR_END_MARKER="${TMPDIR_TEST}/end-marker"
mkdir -p "${DIR_END_MARKER}"
cat >"${DIR_END_MARKER}/end-marker.md" <<'MD'
# Content after an explicit document end

```yaml
apiVersion: v1
kind: ConfigMap
...
metadata:
  name: unexpectedly-after-end
```
MD

run "${DIR_END_MARKER}"
check_rc_nonzero "end-marker-exits-nonzero"
check_contains "end-marker-has-diagnostic-prefix" "DOC-YAML:"
check_contains "end-marker-cites-file" "end-marker.md:"

# --- Fixture 6: yaml metadata is not an escape hatch. ---
# Markdown stores text after the language as fence metadata. The checker must
# still parse a block whose exact language is yaml, regardless of that metadata.
DIR_META="${TMPDIR_TEST}/metadata"
mkdir -p "${DIR_META}"
cat >"${DIR_META}/no-parse.md" <<'MD'
# Fence metadata does not bypass validation

```yaml no-parse
name: "unterminated
```
MD

run "${DIR_META}"
check_rc_nonzero "yaml-no-parse-exits-nonzero"
check_contains "yaml-no-parse-is-reported" "DOC-YAML:"
check_contains "yaml-no-parse-cites-file" "no-parse.md:"

# A comma-suffixed label is neither a supported language nor an opt-out. Fail
# it explicitly so this ad-hoc opt-out cannot move a block outside the gate.
DIR_COMMA_META="${TMPDIR_TEST}/comma-metadata"
mkdir -p "${DIR_COMMA_META}"
cat >"${DIR_COMMA_META}/comma-no-parse.md" <<'MD'
# Unsupported comma suffix

```yaml,no-parse
name: otherwise-valid
```
MD

run "${DIR_COMMA_META}"
check_rc_nonzero "yaml-comma-no-parse-exits-nonzero"
check_contains "yaml-comma-no-parse-is-explained" "unsupported YAML fence label"

# --- Fixture 7: errors aggregate across directories and use source lines. ---
# The quote starts on Markdown line 8, not YAML-block line 3. Passing two roots
# at once also pins the public command's one-or-more-directory interface.
DIR_QUOTE="${TMPDIR_TEST}/bad-quote"
DIR_INDENT="${TMPDIR_TEST}/bad-indent"
mkdir -p "${DIR_QUOTE}" "${DIR_INDENT}"
cat >"${DIR_QUOTE}/bad-quote.mdx" <<'MDX'
# Broken quote

The diagnostic below must cite its line in this Markdown file.

```yml
metadata:
  labels:
    team: "unterminated
```
MDX
cat >"${DIR_INDENT}/bad-indent.md" <<'MD'
# Broken indentation

```yaml
root:
  child: true
 sibling: false
```
MD

run "${DIR_QUOTE}" "${DIR_INDENT}"
check_rc_nonzero "multiple-errors-exit-nonzero"
check_contains "quote-reports-absolute-source-line" "bad-quote.mdx:8:"
check_contains "indent-error-is-also-reported" "bad-indent.md:"
check_contains "quote-uses-diagnostic-prefix" "DOC-YAML: ${DIR_QUOTE}/bad-quote.mdx:"
check_contains "indent-uses-diagnostic-prefix" "DOC-YAML: ${DIR_INDENT}/bad-indent.md:"

# Parser recovery can produce many follow-on errors from one bad token. Keep
# enough detail to identify the cause without flooding CI output for one block.
DIR_NOISY_ERRORS="${TMPDIR_TEST}/noisy-errors"
mkdir -p "${DIR_NOISY_ERRORS}"
cat >"${DIR_NOISY_ERRORS}/multiple-doc-errors.md" <<'MD'
# Several malformed YAML documents

```yaml
first: [
---
second: [
---
third: [
---
fourth: [
```
MD

run "${DIR_NOISY_ERRORS}"
check_rc_nonzero "noisy-block-exits-nonzero"
check_contains "noisy-block-reports-suppressed-errors" "additional error(s) suppressed for this block"
DIAGNOSTIC_COUNT="$(printf '%s\n' "${OUT}" | awk '/^DOC-YAML:/ { count++ } END { print count + 0 }')"
if [[ "${DIAGNOSTIC_COUNT}" == "4" ]]; then
    pass "noisy-block-caps-diagnostics"
else
    fail "noisy-block-caps-diagnostics" "want 4 diagnostic lines, got ${DIAGNOSTIC_COUNT}: ${OUT}"
fi

# --- Fixture 8: a stale shared-toolchain lock fails closed with recovery help. ---
# A SIGKILL can leave the directory lock behind. Override sleep so this exercises
# all retry attempts without adding a minute to the shell-test suite.
DIR_STALE_LOCK="${TMPDIR_TEST}/stale-install-lock"
mkdir -p "${DIR_STALE_LOCK}/.aicr-install-lock"

OUT="$(
  (
    unset CI GITHUB_ACTIONS
    # shellcheck source=tools/mdx/bootstrap.sh
    source "${SCRIPT_DIR}/mdx/bootstrap.sh"
    DOCS_NODE_DIR="${DIR_STALE_LOCK}"
    DOCS_NODE_STAMP="${DIR_STALE_LOCK}/node_modules/.aicr-lock-digest"
    DOCS_NODE_INSTALL_LOCK="${DIR_STALE_LOCK}/.aicr-install-lock"
    sleep() { :; }
    ensure_docs_node_toolchain "docs YAML fence gate"
  ) 2>&1
)"
RC=$?

check_rc_nonzero "stale-install-lock-exits-nonzero-locally"
check_contains "stale-install-lock-explains-timeout" "timed out waiting for another docs parser install"
check_contains "stale-install-lock-explains-process-check" "verify no docs parser or npm ci is running"
check_contains "stale-install-lock-gives-removal-command" "rmdir \"${DIR_STALE_LOCK}/.aicr-install-lock\""
check_absent "stale-install-lock-does-not-blame-node" "install Node 20+"

# --- Fixture 9: MDX component wrappers do not hide YAML fences. ---
# Plain CommonMark treats this tightly nested fence as raw HTML, while MDX
# exposes it as a code node. The valid top-level control also ensures this
# cannot fail only because the scanner found zero blocks.
DIR_MDX_JSX="${TMPDIR_TEST}/mdx-jsx"
mkdir -p "${DIR_MDX_JSX}"
cat >"${DIR_MDX_JSX}/nested-jsx.mdx" <<'MDX'
# Nested YAML

```yaml
control: valid
```

<Tabs>
```yaml
broken: [
```
</Tabs>
MDX

run "${DIR_MDX_JSX}"
check_rc_nonzero "mdx-jsx-malformed-yaml-exits-nonzero"
check_contains "mdx-jsx-malformed-yaml-is-reported" "DOC-YAML: ${DIR_MDX_JSX}/nested-jsx.mdx:9:"
check_contains "mdx-jsx-blocks-counted-once" "ERROR: 1 of 2 YAML block(s) fail syntax parsing."
check_absent "mdx-jsx-does-not-report-success" "OK:"

# An unrelated MDX error elsewhere in the file must not disable JSX-aware
# fence discovery. The plain-Markdown tree still hides the nested fence, so
# silently abandoning MDX parsing would make this malformed YAML pass.
DIR_MDX_RECOVERY="${TMPDIR_TEST}/mdx-recovery"
mkdir -p "${DIR_MDX_RECOVERY}"
cat >"${DIR_MDX_RECOVERY}/nested-jsx-with-hazard.md" <<'MD'
# Nested YAML with unrelated MDX syntax

```yaml
control: valid
```

<Tabs>
```yaml
broken: [
```
</Tabs>

MDX hazard <= 2
MD

run "${DIR_MDX_RECOVERY}"
check_rc_nonzero "mdx-recovery-malformed-yaml-exits-nonzero"
check_contains "mdx-recovery-malformed-yaml-is-reported" "DOC-YAML: ${DIR_MDX_RECOVERY}/nested-jsx-with-hazard.md:9:"
check_contains "mdx-recovery-blocks-counted-once" "ERROR: 1 of 2 YAML block(s) fail syntax parsing."
check_absent "mdx-recovery-does-not-report-success" "OK:"

# --- Fixture 10: partial file discovery failures fail closed. ---
# The fake find emits a valid file and then fails. A process-substitution
# implementation can accidentally parse that partial result and return OK.
DIR_FIND_FAILURE="${TMPDIR_TEST}/find-failure"
FIND_STUB_BIN="${TMPDIR_TEST}/find-stub-bin"
mkdir -p "${DIR_FIND_FAILURE}" "${FIND_STUB_BIN}"
cat >"${DIR_FIND_FAILURE}/valid.md" <<'MD'
```yaml
checked: true
```
MD
cat >"${FIND_STUB_BIN}/find" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "${AICR_TEST_FIND_RESULT:?}"
echo "synthetic find failure" >&2
exit 23
SH
chmod +x "${FIND_STUB_BIN}/find"

OUT="$(
    PATH="${FIND_STUB_BIN}:${PATH}" \
        AICR_TEST_FIND_RESULT="${DIR_FIND_FAILURE}/valid.md" \
        "${CHECK}" "${DIR_FIND_FAILURE}" 2>&1
)"
RC=$?
check_rc_nonzero "find-partial-failure-exits-nonzero"
check_contains \
    "find-partial-failure-is-explained" \
    "ERROR: failed to enumerate Markdown files under: ${DIR_FIND_FAILURE}"
check_contains "find-partial-failure-preserves-cause" "synthetic find failure"
check_absent "find-partial-failure-does-not-report-success" "OK:"

# --- Fixture 11: file-list sorting failures also fail closed. ---
# This pins the second command in the former discovery pipeline. Emitting the
# input before failing ensures a partial sorted list cannot reach the parser.
DIR_SORT_FAILURE="${TMPDIR_TEST}/sort-failure"
SORT_STUB_BIN="${TMPDIR_TEST}/sort-stub-bin"
mkdir -p "${DIR_SORT_FAILURE}" "${SORT_STUB_BIN}"
cat >"${DIR_SORT_FAILURE}/valid.md" <<'MD'
```yaml
checked: true
```
MD
cat >"${SORT_STUB_BIN}/sort" <<'SH'
#!/usr/bin/env bash
for argument in "$@"; do
    if [[ -f "${argument}" ]]; then
        while IFS= read -r line; do
            printf '%s\n' "${line}"
        done <"${argument}"
    fi
done
echo "synthetic sort failure" >&2
exit 24
SH
chmod +x "${SORT_STUB_BIN}/sort"

OUT="$(PATH="${SORT_STUB_BIN}:${PATH}" "${CHECK}" "${DIR_SORT_FAILURE}" 2>&1)"
RC=$?
check_rc_nonzero "sort-partial-failure-exits-nonzero"
check_contains "sort-partial-failure-is-explained" "ERROR: failed to sort Markdown file list"
check_contains "sort-partial-failure-preserves-cause" "synthetic sort failure"
check_absent "sort-partial-failure-does-not-report-success" "OK:"

# --- Fixture 12: directive-only streams retain their parser errors. ---
for directive in '%YAML invalid' '%TAG not-valid' '%YAML 1.2'; do
    DIR_DIRECTIVE="${TMPDIR_TEST}/directive-${directive// /-}"
    mkdir -p "${DIR_DIRECTIVE}"
    printf '# Directive without a document\n\n```yaml\n%s\n```\n' "${directive}" \
        >"${DIR_DIRECTIVE}/directive.md"
    run "${DIR_DIRECTIVE}"
    check_rc_nonzero "directive-${directive}-exits-nonzero"
    check_contains "directive-${directive}-has-source-location" "DOC-YAML: ${DIR_DIRECTIVE}/directive.md:"
    check_absent "directive-${directive}-does-not-report-success" "OK:"
done

DIR_EMPTY_VALID="${TMPDIR_TEST}/empty-valid"
mkdir -p "${DIR_EMPTY_VALID}"
cat >"${DIR_EMPTY_VALID}/empty.md" <<'MD'
```yaml
```

```yaml
# Empty fragments are valid YAML.
```

```yaml
%YAML 1.2
---
name: valid
```
MD
run "${DIR_EMPTY_VALID}"
check_rc_zero "empty-and-explicit-documents-exit-zero"
check_contains "empty-and-explicit-documents-are-counted" "OK: 3 YAML block(s)"

# --- Fixture 13: aliases must refer to an earlier anchor in this document. ---
DIR_ALIASES="${TMPDIR_TEST}/aliases"
mkdir -p "${DIR_ALIASES}"
cat >"${DIR_ALIASES}/invalid.md" <<'MD'
```yaml
labels: *missing
```

```yaml
first: *later
second: &later value
```

```yaml
first: &earlier value
---
second: *earlier
```
MD
run "${DIR_ALIASES}"
check_rc_nonzero "unresolved-aliases-exit-nonzero"
check_contains "missing-alias-has-source-location" "invalid.md:2:9:"
check_contains "forward-alias-has-source-location" "invalid.md:6:8:"
check_contains "cross-document-alias-has-source-location" "invalid.md:13:9:"
check_contains "all-invalid-alias-blocks-are-counted" "ERROR: 3 of 3 YAML block(s)"

DIR_VALID_ALIASES="${TMPDIR_TEST}/valid-aliases"
mkdir -p "${DIR_VALID_ALIASES}"
cat >"${DIR_VALID_ALIASES}/valid.md" <<'MD'
```yaml
first: &label original
second: *label
third: &label replacement
fourth: *label
recursive: &self [*self]
---
first: &label separate-document
second: *label
```
MD
run "${DIR_VALID_ALIASES}"
check_rc_zero "valid-and-recursive-aliases-exit-zero"
check_contains "valid-aliases-print-summary" "OK: 1 YAML block(s)"

# --- Fixture 14: comments are not examples, including during MDX recovery. ---
DIR_COMMENTS="${TMPDIR_TEST}/comments"
mkdir -p "${DIR_COMMENTS}"
cat >"${DIR_COMMENTS}/comments.mdx" <<'MDX'
{/*
```yaml
broken: [
```
*/}

```yaml
control: valid
```
MDX
run "${DIR_COMMENTS}"
check_rc_zero "mdx-comments-exit-zero"
check_contains "mdx-comments-are-not-counted" "OK: 1 YAML block(s)"

# Force fallback parsing while retaining both comment styles. The unclosed
# fence in the HTML comment must not consume the visible example after it.
cat >>"${DIR_COMMENTS}/comments.mdx" <<'MDX'

MDX hazard <= 2

<!--
```yaml
broken: [
-->

```yaml
second: valid
```
MDX
run "${DIR_COMMENTS}"
check_rc_zero "recovery-comments-exit-zero"
check_contains "recovery-comments-are-not-counted" "OK: 2 YAML block(s)"

cat >>"${DIR_COMMENTS}/comments.mdx" <<'MDX'

<Tabs>
```yaml
value: "{/* literal comment marker */}
```
</Tabs>
MDX
run "${DIR_COMMENTS}"
check_rc_nonzero "comment-markers-in-yaml-remain-checked"
check_contains "comment-markers-in-yaml-preserve-source-line" "comments.mdx:24:"
check_contains "recovery-keeps-visible-jsx-fences" "ERROR: 1 of 3 YAML block(s)"

DIR_LITERAL_MARKERS="${TMPDIR_TEST}/literal-markers"
mkdir -p "${DIR_LITERAL_MARKERS}"
cat >"${DIR_LITERAL_MARKERS}/literal.md" <<'MD'
MDX hazard <= 2

An opening comment marker in inline code: `{/*`.

```yaml
mdxMarker: "{/*"
htmlMarker: "<!--"
```

{/*
```yaml
broken: [
```
*/}

<!--
```yaml
broken: [
```
-->

    ```yaml
    literalCode: [
    ```
MD
run "${DIR_LITERAL_MARKERS}"
check_rc_zero "literal-markers-do-not-hide-later-comments"
check_contains "literal-code-and-comments-are-not-fences" "OK: 1 YAML block(s)"

# --- Fixture 15: escaped comment markers remain visible during recovery. ---
# An escaped marker is literal. Paired backslashes also prevent a flow comment
# from starting here: an inline comment cannot swallow a separate fenced block.
# Include a valid control so missed examples cannot pass unnoticed.
for marker in html mdx; do
    for slashes in 0 1 2 3 4; do
        DIR_ESCAPED="${TMPDIR_TEST}/escaped-${marker}-${slashes}"
        mkdir -p "${DIR_ESCAPED}"
        {
            printf '```yaml\ncontrol: valid\n```\n\nMDX hazard <= 2\n\n'
            for (( i=0; i<slashes; i++ )); do printf '\\'; done
            if [[ "${marker}" == "html" ]]; then printf '<!--\n\n'; else printf '{/*\n\n'; fi
            printf '```yaml\nbroken: [\n```\n\n'
            if [[ "${marker}" == "html" ]]; then printf '%s\n' '-->'; else printf '*/}\n'; fi
        } >"${DIR_ESCAPED}/escaped.md"
        run "${DIR_ESCAPED}"
        if (( slashes > 0 )); then
            check_rc_nonzero "escaped-${marker}-${slashes}-exits-nonzero"
            check_contains "escaped-${marker}-${slashes}-counts-visible-fence" "ERROR: 1 of 2 YAML block(s)"
        else
            check_rc_zero "unescaped-${marker}-${slashes}-exits-zero"
            check_contains "unescaped-${marker}-${slashes}-ignores-comment" "OK: 1 YAML block(s)"
        fi
    done
done

DIR_ESCAPED_UNCLOSED="${TMPDIR_TEST}/escaped-unclosed"
mkdir -p "${DIR_ESCAPED_UNCLOSED}"
cat >"${DIR_ESCAPED_UNCLOSED}/escaped.md" <<'MD'
```yaml
control: valid
```

MDX hazard <= 2

\<!--

```yaml
broken: [
```
MD
run "${DIR_ESCAPED_UNCLOSED}"
check_rc_nonzero "escaped-unclosed-comment-exits-nonzero"
check_contains "escaped-unclosed-comment-preserves-source-line" "escaped.md:10:"
check_contains "escaped-unclosed-comment-counts-visible-fence" "ERROR: 1 of 2 YAML block(s)"

# --- Fixture 16: recovery preserves comment boundaries in Markdown containers. ---
DIR_CONTAINER_COMMENTS="${TMPDIR_TEST}/container-comments"
mkdir -p "${DIR_CONTAINER_COMMENTS}"
cat >"${DIR_CONTAINER_COMMENTS}/containers.md" <<'MD'
MDX hazard <= 2

> {/*
> ```yaml
> hidden: [
> ```
> */}
>
> ```yaml
> visible: true
> ```

- <!--
  ```yaml
  hidden: [
  -->

  ```yaml
  visible: true
  ```

{/* braces { and } inside a comment do not close it */ /* another comment */}

{ // line comment
/*
```yaml
hidden: [
```
*/ }

Inline <!--
comment
--> and {/* inline comment */} text.

<Tabs>
```yaml
visible: true
```
</Tabs>
MD
run "${DIR_CONTAINER_COMMENTS}"
check_rc_zero "container-comments-exit-zero"
check_contains "container-comments-keep-visible-fences" "OK: 3 YAML block(s)"

# A missing quote prefix ends the container. An unterminated comment inside it
# must not hide a subsequent top-level example during recovery.
cat >"${DIR_CONTAINER_COMMENTS}/escaped-container.md" <<'MD'
```yaml
control: valid
```

> {/*
> hidden text

```yaml
broken: [
```

*/}
MD
run "${DIR_CONTAINER_COMMENTS}"
check_rc_nonzero "comment-cannot-escape-blockquote"
check_contains "escaped-container-reports-visible-error" "escaped-container.md:9:"
check_contains "escaped-container-keeps-other-examples" "ERROR: 1 of 5 YAML block(s)"

# --- Fixture 17: directives need a document even after a valid document. ---
for directive in '%YAML 1.2' '%TAG !e! tag:example.com,2026:'; do
    DIR_TRAILING_DIRECTIVE="${TMPDIR_TEST}/trailing-directive"
    mkdir -p "${DIR_TRAILING_DIRECTIVE}"
    printf '```yaml\nvalid: true\n...\n%s\n```\n' "${directive}" \
        >"${DIR_TRAILING_DIRECTIVE}/trailing.md"
    run "${DIR_TRAILING_DIRECTIVE}"
    check_rc_nonzero "trailing-${directive}-exits-nonzero"
    check_contains "trailing-${directive}-has-source-location" "trailing.md:4:1:"
    check_contains "trailing-${directive}-explains-missing-document" "directives must be followed by '---'"

    printf '```yaml\nvalid: true\n...\n%s\n---\n```\n' "${directive}" \
        >"${DIR_TRAILING_DIRECTIVE}/trailing.md"
    run "${DIR_TRAILING_DIRECTIVE}"
    check_rc_zero "terminated-${directive}-exits-zero"
done

# --- Fixture 18: YAML tokens, not text matching, identify trailing directives. ---
DIR_DIRECTIVE_LITERALS="${TMPDIR_TEST}/directive-literals"
mkdir -p "${DIR_DIRECTIVE_LITERALS}"
cat >"${DIR_DIRECTIVE_LITERALS}/literals.md" <<'MD'
```yaml
literal: |
  %YAML 1.2
  %TAG !e! tag:example.com,2026:
quoted: "%YAML 1.2"
...
# %YAML 1.2
```

```yaml
? &key original
: *key
recursive: &self {child: *self}
```
MD
run "${DIR_DIRECTIVE_LITERALS}"
check_rc_zero "directive-literals-and-anchored-keys-exit-zero"
check_contains "directive-literals-and-anchored-keys-are-counted" "OK: 2 YAML block(s)"

# Compare recovery with the full MDX parser across comments, containers, and
# newline styles. Keep this in the shell harness so `make test` always runs it.
if node --test "${SCRIPT_DIR}/mdx/remark-docs-recovery.test.mjs"; then
    pass "recovery-parser-matches-mdx"
else
    fail "recovery-parser-matches-mdx" "parser comparison failed"
fi

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All check-docs-yaml tests passed"
