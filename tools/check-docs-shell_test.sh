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

# Unit harness for tools/check-docs-shell — the shell placeholder docs gate.
# Run directly: bash tools/check-docs-shell_test.sh
# Wired into CI via `make test` (test-shell target, runs tools/*_test.sh).
#
# Hermetic with respect to docs/: every assertion runs against fixture .md or
# .mdx files in a temp directory. It is NOT hermetic with respect to the
# network — the first run resolves the pinned docs toolchain via npm. Under CI
# that is required; locally a missing node/npm is a skip, matching the driver.
#
# The fixtures pin issue #2266 in both directions: every shell construct that
# keeps a `<placeholder>` away from the lexer must pass, and every context that
# hands one to the lexer, including command substitution nested inside double
# quotes, must be reported at its source line and column.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="${SCRIPT_DIR}/check-docs-shell"

if [[ -z "${CI:-}" && -z "${GITHUB_ACTIONS:-}" ]]; then
    if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
        echo "SKIP: node/npm not available — check-docs-shell tests are CI-enforced"
        exit 0
    fi
fi

TMPDIR_TEST="$(mktemp -d)"
trap 'rm -rf "${TMPDIR_TEST}"' EXIT

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1 — $2"; fails=$((fails + 1)); }

# run <checker> [<dir> ...]: capture combined output and the checker exit code.
OUT=""
RC=0
run() {
    OUT="$("$@" 2>&1)"
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
check_count() { # <name> <needle> <want>
    local got
    got="$(grep -cF -- "$2" <<<"${OUT}")"
    if [[ "${got}" == "$3" ]]; then pass "$1"; else fail "$1" "want $3 lines containing '$2', got ${got}: ${OUT}"; fi
}

# --- Fixture 1: every construct that keeps a placeholder inert passes. ---
# Quotes, escapes, comments, heredoc text, parameter expansion, and real
# redirections must not be reported. Each line pins one rule: `$'it\'s <x>'`
# ends early without ANSI-C escape handling, the unquoted `${NAME:-<x>}` is
# reported without parameter-expansion skipping, and `<in 2>err`, `<in >out`,
# and a comment containing `->` resemble placeholders. Non-shell fences,
# including `console` transcripts, are out of scope.
DIR_INERT="${TMPDIR_TEST}/inert"
mkdir -p "${DIR_INERT}"
cat >"${DIR_INERT}/inert.md" <<'MD'
# Inert placeholders

```bash
NAMESPACE="my-namespace"  # replace with your namespace
kubectl delete namespace "${NAMESPACE}"
kubectl get pods -n "<namespace>" -l 'app=<name>'
git commit -m "evidence: <recipe> (unsigned)"
echo $'it\'s <literal>'
kubectl get \<escaped\>
# replace <namespace> before running
echo ${NAME:-<default>}
for ((i = 0; i<n; i++)); do :; done
sort <input.txt >output.txt
sort <input.txt 2>errors.txt
sort < input.txt
sort < input.txt>sorted.txt
while read -r n; do echo "$n"; done <nodes.txt  # one node per line -> next
diff <(ls a) <(ls b)
exec 3<&0
grep foo <<< "<herestring>"
cat <<EOF
<heredoc-body>
EOF
cat <<'QUOTED'
$(kubectl get pods -n <quoted-heredoc-data>)
QUOTED
```

```sh
	cat <<-EOF
	<tab-stripped-body>
	EOF
echo done
```

```console
$ kubectl get pods -n <namespace>
NAME   READY
```

```text
aicr verify <bundle-dir> [flags]
```

```yaml
name: <placeholder>
```
MD

run "${CHECK}" "${DIR_INERT}"
check_rc_zero "inert-exits-zero"
check_contains "inert-prints-summary" "OK: 2 shell block(s)"
check_absent "inert-has-no-diagnostic" "DOC-SHELL:"

# --- Fixture 2: every lexer-visible placeholder is reported in place. ---
# Line and column are Markdown source coordinates, including the indentation of
# a fence nested in a list item.
DIR_BAD="${TMPDIR_TEST}/bad"
mkdir -p "${DIR_BAD}"
cat >"${DIR_BAD}/commands.md" <<'MD'
# Commands

```bash
kubectl delete namespace <namespace>
echo "pods: $(kubectl get pods -n <ns>)"
echo `cat <file>`
aicr diff --baseline <path|cm://...>
aicr bundle <your original flags> \
  --output ./bundle
cat <<EOF
<heredoc-body>
EOF
kubectl get pods -n <after-heredoc>
```

1. Step one:

   ```shell
   helm uninstall <release>
   ```
MD
cat >"${DIR_BAD}/labels.mdx" <<'MDX'
# Every shell label is checked

~~~zsh
kubectl -n <zsh-namespace> get pods
~~~

```Bash
kubectl -n <uppercase-label> get pods
```
MDX

run "${CHECK}" "${DIR_BAD}"
check_rc_nonzero "bad-exits-nonzero"
check_contains "bad-bare-argument" "commands.md:4:26: unquoted placeholder <namespace>"
check_contains "bad-substitution-in-double-quotes" "commands.md:5:35: unquoted placeholder <ns>"
check_contains "bad-backtick-substitution" "commands.md:6:11: unquoted placeholder <file>"
check_contains "bad-alternation" "commands.md:7:22: unquoted placeholder <path|cm://...>"
check_contains "bad-multi-word" "commands.md:8:13: unquoted placeholder <your original flags>"
check_contains "bad-after-heredoc" "commands.md:13:21: unquoted placeholder <after-heredoc>"
check_contains "bad-indented-fence-column" "commands.md:19:19: unquoted placeholder <release>"
check_contains "bad-tilde-zsh" "labels.mdx:4:12: unquoted placeholder <zsh-namespace>"
check_contains "bad-uppercase-label" "labels.mdx:8:12: unquoted placeholder <uppercase-label>"
check_absent "bad-heredoc-body-inert" "<heredoc-body>"
check_count "bad-reports-every-placeholder" "DOC-SHELL:" "9"

# Lexer state must survive constructs that resemble heredocs, and an unquoted
# heredoc body runs command substitution, so its placeholders are live while a
# stray `"` in it closes nothing. A `<<` inside arithmetic, or a `<<-`
# delimiter whose tabs are not stripped, would open a heredoc that never ends
# and hide every later line. Placeholders are matched by their closing `>`, not
# their spelling: paths, ellipses, and spaced alternation all redirect in bash.
DIR_STATE="${TMPDIR_TEST}/state"
mkdir -p "${DIR_STATE}"
cat >"${DIR_STATE}/state.md" <<'MD'
# Lexer state

```bash
echo $(( 1 << 20 ))
kubectl get pods -n <after-arithmetic>
cat <<EOF
data: "$(kubectl get nodes -l <in-heredoc-substitution>)"
plain <heredoc-text> and a stray " quote
EOF
kubectl apply -f </path/to/manifest.yaml>
aicr recipe --intent <training | inference>
aicr bundle -r recipe.yaml <...>
```
MD
printf '%s\n' '' '```sh' $'\tcat <<-EOF' $'\t<tab-stripped-body>' $'\tEOF' \
    'kubectl get pods -n <after-tab-heredoc>' '```' >>"${DIR_STATE}/state.md"

run "${CHECK}" "${DIR_STATE}"
check_rc_nonzero "state-exits-nonzero"
check_contains "state-after-arithmetic" "state.md:5:21: unquoted placeholder <after-arithmetic>"
check_contains "state-heredoc-substitution" "state.md:7:31: unquoted placeholder <in-heredoc-substitution>"
check_contains "state-path-placeholder" "state.md:10:18: unquoted placeholder </path/to/manifest.yaml>"
check_contains "state-spaced-alternation" "state.md:11:22: unquoted placeholder <training | inference>"
check_contains "state-ellipsis" "state.md:12:28: unquoted placeholder <...>"
check_contains "state-after-tab-heredoc" "state.md:19:21: unquoted placeholder <after-tab-heredoc>"
check_absent "state-heredoc-text-inert" "<heredoc-text>"
check_absent "state-tab-heredoc-body-inert" "<tab-stripped-body>"
check_count "state-reports-every-placeholder" "DOC-SHELL:" "6"
check_contains "bad-prints-remedy" "Assign a variable before first use and quote the expansion"

# --- Fixture 3: refuse to pass when nothing was checked. ---
DIR_EMPTY="${TMPDIR_TEST}/no-shell"
mkdir -p "${DIR_EMPTY}"
cat >"${DIR_EMPTY}/prose.md" <<'MD'
# Prose only

```text
kubectl delete namespace <namespace>
```
MD

run "${CHECK}" "${DIR_EMPTY}"
check_rc_nonzero "no-shell-blocks-exits-nonzero"
check_contains "no-shell-blocks-explains" "no bash/sh/shell/zsh fenced code blocks were found"

# --- Fixture 4: the default scan covers docs/ but skips docs/design/. ---
# A copy of the driver in a scratch repository resolves REPO_ROOT there, while
# the symlinked tools/mdx keeps the pinned parser and its node_modules.
FAKE_REPO="${TMPDIR_TEST}/repo"
mkdir -p "${FAKE_REPO}/tools" "${FAKE_REPO}/docs/user" "${FAKE_REPO}/docs/design"
cp "${CHECK}" "${FAKE_REPO}/tools/check-docs-shell"
ln -s "${SCRIPT_DIR}/mdx" "${FAKE_REPO}/tools/mdx"
cat >"${FAKE_REPO}/docs/user/guide.md" <<'MD'
# Guide

```bash
NAMESPACE="my-namespace"  # replace with your namespace
kubectl get pods -n "${NAMESPACE}"
```
MD
cat >"${FAKE_REPO}/docs/design/001-adr.md" <<'MD'
# ADR

```bash
kubectl get pods -n <namespace>
```
MD

run "${FAKE_REPO}/tools/check-docs-shell"
check_rc_zero "default-scan-skips-design"
check_contains "default-scan-checks-user-docs" "OK: 1 shell block(s) in 1 Markdown file(s)"

run "${FAKE_REPO}/tools/check-docs-shell" "${FAKE_REPO}/docs/design"
check_rc_nonzero "explicit-root-checks-design"
check_contains "explicit-root-reports-design" "001-adr.md:4:21: unquoted placeholder <namespace>"

# Inside a git work tree the default scan follows git, so local runs match CI:
# a gitignored scratch file is skipped, and a page not yet added is checked.
git -C "${FAKE_REPO}" init -q
printf 'docs/scratch/\n' >"${FAKE_REPO}/.gitignore"
mkdir -p "${FAKE_REPO}/docs/scratch"
cat >"${FAKE_REPO}/docs/scratch/plan.md" <<'MD'
# Local plan

```bash
kubectl get pods -n <ignored-placeholder>
```
MD

run "${FAKE_REPO}/tools/check-docs-shell"
check_rc_zero "git-scan-skips-ignored"
check_contains "git-scan-checks-user-docs" "OK: 1 shell block(s) in 1 Markdown file(s)"

cat >"${FAKE_REPO}/docs/user/new-page.md" <<'MD'
# New page

```bash
kubectl get pods -n <untracked-placeholder>
```
MD

run "${FAKE_REPO}/tools/check-docs-shell"
check_rc_nonzero "git-scan-checks-untracked"
check_contains "git-scan-reports-untracked" "new-page.md:4:21: unquoted placeholder <untracked-placeholder>"
check_absent "git-scan-still-skips-ignored" "<ignored-placeholder>"
check_absent "git-scan-still-skips-design" "001-adr.md"

# git ls-files --cached still lists a tracked page deleted from the work tree;
# the scan must skip it rather than fail to read it.
rm -f "${FAKE_REPO}/docs/user/new-page.md"
cp "${FAKE_REPO}/docs/user/guide.md" "${FAKE_REPO}/docs/user/removed.md"
git -C "${FAKE_REPO}" add docs/user/removed.md
rm -f "${FAKE_REPO}/docs/user/removed.md"

run "${FAKE_REPO}/tools/check-docs-shell"
check_rc_zero "git-scan-skips-deleted-tracked"
check_absent "git-scan-does-not-read-deleted" "cannot read"

# --- Fixture 5: fences nested in MDX components are checked. ---
# Plain CommonMark reads a fence directly inside <Tabs> as raw HTML. The second
# file adds an unrelated MDX error, so discovery must fall back to the recovery
# parser rather than to plain Markdown.
DIR_MDX="${TMPDIR_TEST}/mdx"
mkdir -p "${DIR_MDX}"
cat >"${DIR_MDX}/nested-jsx.mdx" <<'MDX'
# Nested shell

```bash
echo control
```

<Tabs>
```bash
kubectl get pods -n <in-jsx>
```
</Tabs>
MDX
{
    cat "${DIR_MDX}/nested-jsx.mdx"
    printf '\nMDX hazard <= 2\n'
} >"${DIR_MDX}/nested-jsx-with-hazard.md"

run "${CHECK}" "${DIR_MDX}"
check_rc_nonzero "mdx-nested-exits-nonzero"
check_contains "mdx-nested-is-reported" "nested-jsx.mdx:9:21: unquoted placeholder <in-jsx>"
check_contains "mdx-recovery-is-reported" "nested-jsx-with-hazard.md:9:21: unquoted placeholder <in-jsx>"

# --- Fixture 6: discovery and read failures fail closed. ---
run "${CHECK}" "${TMPDIR_TEST}/no-such-dir"
if [[ "${RC}" == "2" ]]; then pass "missing-root-exits-2"; else fail "missing-root-exits-2" "want rc=2, got ${RC}"; fi
check_contains "missing-root-is-explained" "docs scan root is not a directory"

# Each stub emits a valid file before failing, so a partial list must not be
# parsed into a passing result.
STUB_BIN="${TMPDIR_TEST}/stub-bin"
mkdir -p "${STUB_BIN}"
cat >"${STUB_BIN}/find" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "${AICR_TEST_STUB_RESULT:?}"
echo "synthetic find failure" >&2
exit 23
SH
cat >"${STUB_BIN}/git" <<'SH'
#!/usr/bin/env bash
if [[ "$1" == "rev-parse" ]]; then
    exec "${AICR_TEST_REAL_GIT:?}" "$@"
fi
printf '%s\0' "${AICR_TEST_STUB_RESULT:?}"
echo "synthetic git failure" >&2
exit 23
SH
chmod +x "${STUB_BIN}/find" "${STUB_BIN}/git"

OUT="$(PATH="${STUB_BIN}:${PATH}" AICR_TEST_STUB_RESULT="${DIR_INERT}/inert.md" \
    "${CHECK}" "${DIR_INERT}" 2>&1)"
RC=$?
check_rc_nonzero "find-failure-exits-nonzero"
check_contains "find-failure-is-explained" "ERROR: failed to enumerate Markdown files under: ${DIR_INERT}"
check_absent "find-failure-does-not-report-success" "OK:"

OUT="$(PATH="${STUB_BIN}:${PATH}" AICR_TEST_STUB_RESULT="docs/user/guide.md" \
    AICR_TEST_REAL_GIT="$(command -v git)" "${FAKE_REPO}/tools/check-docs-shell" 2>&1)"
RC=$?
check_rc_nonzero "git-failure-exits-nonzero"
check_contains "git-failure-is-explained" "ERROR: failed to list Markdown files under docs/ with git ls-files"
check_absent "git-failure-does-not-report-success" "OK:"

# The driver only passes files it found, so call the checker directly to pin
# the unreadable-file branch.
run node "${SCRIPT_DIR}/mdx/check-docs-shell.mjs" "${DIR_INERT}/inert.md" "${TMPDIR_TEST}/missing.md"
check_rc_nonzero "unreadable-file-exits-nonzero"
check_contains "unreadable-file-is-reported" "missing.md: cannot read"
check_contains "unreadable-file-is-counted" "1 Markdown file(s) could not be checked"

if [[ "${fails}" -gt 0 ]]; then
    echo "check-docs-shell: ${fails} assertion(s) failed" >&2
    exit 1
fi

echo "All check-docs-shell tests passed"
