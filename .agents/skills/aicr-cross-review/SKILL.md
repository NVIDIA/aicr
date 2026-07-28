---
name: aicr-cross-review
description: |
  Multi-agent PR review using Claude Code, Codex, and CodeRabbit. Runs
  parallel reviews with integration impact analysis, then one cross-review
  round to a 2-of-3 consensus, with every confirmed finding adversarially
  verified by a fresh agent. Never runs the reviewed commit's code, and
  never posts unless explicitly asked. Use when asked for a thorough
  cross-review or multi-reviewer
  analysis. Requires the Codex plugin; CodeRabbit is best-effort.
  Claude Code only — uses the Workflow and Agent tools, which are not
  available in other agents.
user-invocable: true
# Reviewer lanes must not reach a skill that posts. code-review permits `gh pr comment`
# and the CodeRabbit skill auto-triggers on review tasks, so denying Skill closes the
# automatic path that removing the explicit nested call did not.
disallowed-tools: Skill
argument-hint: "<PR-number-or-URL>"
version: 0.3.0
---

# AICR Cross-Review: Multi-Agent PR Review with Consensus

Three reviewers (Claude Code, Codex, CodeRabbit) plus a targeted integration impact
analysis, cross-reviewed to 2-of-3 consensus, with every confirmed finding
adversarially verified by a fresh agent. Orchestration runs as a **Workflow**
(`scripts/workflow.mjs`).

**Claude Code only.** If the `Workflow` tool is unavailable, stop and say why — do not
fall back to another review command. `/code-review` in particular posts its result to
the PR (see Phase 2), which this skill never does without an explicit request. Named
`aicr-cross-review` so it does not shadow a contributor's global `cross-review` skill.

**When in doubt, stop.** Every check below either passes or ends the review with an
explanation. The skill never executes the reviewed commit's code, and never posts to
the PR unless you explicitly ask (Phase 5).

## Input

Raw arguments: `$ARGUMENTS`

`$ARGUMENTS` must be a PR number or a URL; both are normalized in Phase 0. There is no
no-argument mode: a fork PR cannot be found from the local branch name alone, since the
branch lives on the contributor's fork while the PR lives on `NVIDIA/aicr`. Stop and ask
for a PR reference rather than guessing. Do not write a parser; `gh` accepts both forms.

## Phase 0: Pre-flight

Only the required lanes are hard requirements. Claude, Codex and integration analysis
must work — if one fails at runtime the review reports `incomplete` and stops.
CodeRabbit is best-effort: a missing or unauthenticated CLI is not an error, its vote
slot just records `NONE`.

```bash
for tool in gh git; do
  which "$tool" >/dev/null || { echo "$tool not found — install it and retry."; exit 1; }
done
ls ~/.claude/plugins/cache/openai-codex/codex/*/scripts/codex-companion.mjs >/dev/null 2>&1 \
  || { echo "Codex companion not found. Install the Codex plugin (Settings → Extensions → Codex)."; exit 1; }
echo "Pre-flight OK."
```

If either check fails, stop and report which tool is missing. Do not fall back to
another review command.

**Resolve the PR number — before anything else needs it.**

Every `gh` call and the ref fetch are scoped to `NVIDIA/aicr` **literally**, written out
in each command. Two reasons: in GitHub's standard fork layout the local repository is
the contributor's fork, which has neither the PR nor `refs/pull/*`; and a shell variable
would not survive anyway, since each Bash call is a fresh shell.

```bash
# $ARGUMENTS must be a PR number or URL — gh accepts either.
test -n "$ARGUMENTS" || { echo "usage: /aicr-cross-review <PR-number-or-URL>"; exit 1; }
gh pr view "$ARGUMENTS" --repo NVIDIA/aicr \
  --json number,title,body,baseRefName,headRefName,headRefOid,files
```

Take `<n>` = `.number` and use that numeric value for every later temp path, scoped ref
name and `gh` call — never the raw argument. Keep the rest of the response; Phase 1 does
not re-fetch it.

**Self-review guard.** From the `files` list just fetched: if any changed path is under
`.agents/skills/aicr-cross-review/`, **stop** — the scripts you would execute are the
ones under review. Ask for a trusted checkout. This catches the accidental case only;
`SKILL.md` lives inside the reviewed repo, so it is not a security boundary.

## Phase 1: Setup

**Batch A — one parallel message:**

1. From the Phase 0 response, pin `HEAD_SHA` = `headRefOid`. Every reviewer reviews
   this exact commit. `<n>` is already resolved in Phase 0; do not re-fetch.
2. Worktree hygiene: `git worktree prune`, then `git worktree list | wc -l`. If the
   count still exceeds ~15, **stop** and ask the user to clean up before retrying.
   Do not remove worktrees yourself — a clean detached-HEAD worktree may be another
   session's active review. (Each worktree adds sandbox deny-list paths; at ~70 the
   profile exceeded the OS spawn-arg limit and every sandboxed Bash call failed with
   `E2BIG`. Recovery needs a fresh session.)

**Batch B — after A** (needs `HEAD_SHA` and `baseRefName`). `gh pr diff` takes no
SHA argument, so pin the diff with `git fetch`. Refs and the diff file are
**session-scoped**: two sessions reviewing the same PR must not share, overwrite, or
delete each other's pinned input.

```bash
set -euo pipefail   # a failed fetch or diff must abort, not leave an empty diff file
BASE="<baseRefName>"                    # from step 1 — never hardcode "main"
DIFFPATH=$(mktemp "${TMPDIR:-/tmp}/cross-review-pr<n>.XXXXXX")   # must end in X on macOS
SID=${DIFFPATH##*.}                     # reuse mktemp's unique suffix to scope the refs
PRREF="refs/cr/pr<n>-$SID"; BASEREF="refs/cr/base<n>-$SID"
# Fetch from the canonical repo by URL, not from `origin`: in GitHub's standard fork
# layout `origin` is the contributor's fork, and refs/pull/* exist only on the canonical
# repository.
git -C "<repo-path>" fetch "https://github.com/NVIDIA/aicr.git" \
  "+refs/pull/<n>/head:$PRREF" "+refs/heads/$BASE:$BASEREF"
# Head moved → stop. Clean the refs we just created before exiting; set -e would
# otherwise abort before the names are ever printed, leaving them unreclaimable.
if [ "$(git -C "<repo-path>" rev-parse "$PRREF")" != "<HEAD_SHA>" ]; then
  git -C "<repo-path>" update-ref -d "$PRREF"; git -C "<repo-path>" update-ref -d "$BASEREF"
  rm -f "$DIFFPATH"; echo "HEAD moved since setup — restart the review"; exit 1
fi
# Echo the names FIRST: under `set -e` an empty or failing diff aborts, and any
# echo below it would never run — leaking the refs and the temp file with a random
# suffix nobody recorded, which Phase 5 then cannot clean up.
echo "DIFFPATH=$DIFFPATH"; echo "PRREF=$PRREF"; echo "BASEREF=$BASEREF"
git -C "<repo-path>" diff "$BASEREF...$PRREF" > "$DIFFPATH"
test -s "$DIFFPATH"                     # a real PR diff is never empty
# repoNotes source, pinned to the BASE ref — a fork PR must not be able to rewrite
# the instructions fed to the reviewer. Absent on some repos; that is fine.
git -C "<repo-path>" show "$BASEREF":.claude/CLAUDE.md 2>/dev/null || echo "(no tracked CLAUDE.md)"
# BASE_SHA is the base branch tip. Its only consumer is CodeRabbit's --base-commit,
# and the CLI resolves the merge-base itself, so this stays consistent with the
# three-dot diff above without a second baseline to keep in sync.
echo "BASE_SHA=$(git -C "<repo-path>" rev-parse "$BASEREF")"
```

Capture `DIFFPATH`, `BASE_SHA`, `PRREF`, `BASEREF` — shell variables do not persist
between Bash calls and Phase 5 needs the ref names.

Then build `repoNotes` for the Claude reviewer only (never fed to Codex — lean-context
rule): distill the base-pinned `CLAUDE.md` plus the local overlay into 3–6 lines of the
rules most likely to catch defects in the changed paths.

The check below reduces accidental exposure, but it is **not a trust boundary**:
reviewer subagents load the checkout's `CLAUDE.md` hierarchy automatically, before any
guard here runs. Treat `repoNotes` as a relevance digest, not a sanitiser.

**For an untrusted or fork PR, run this skill from a session started in a trusted
checkout** — the same operational remedy as the self-review guard in Phase 0. Git
overwrites *ignored* files during checkout without complaint, so checking out a fork
that force-added an ignored overlay silently replaces yours.

```bash
for f in AGENTS.local.md CLAUDE.local.md; do
  [ -e "<repo-path>/$f" ] || continue
  # Skip symlinks first. The tracked-status check applies to the link, not its target,
  # so an untracked symlink pointing at a PR-tracked file would otherwise be reported
  # TRUSTED while resolving to PR-controlled instructions.
  [ -L "<repo-path>/$f" ] && { echo "SKIP $f — symlink"; continue; }
  if git -C "<repo-path>" ls-files --error-unmatch -- "$f" >/dev/null 2>&1; then
    echo "SKIP $f — tracked by this PR, not a trusted local overlay"
  else
    echo "TRUSTED $f"      # regular untracked file: safe to read
  fi
done
```

Read only the paths reported `TRUSTED`. `AGENTS.local.md` is normally a symlink to
`CLAUDE.local.md`, so it is skipped and the overlay is read through the real file —
no content is lost.

## Phase 1.5: Classify and extract the change list

**Classify** the PR: `code-change` | `adr` | `config-change` | `documentation-only`.

**Extract a bounded change list** so integration analysis verifies specific items
instead of fishing across the repo:

- Exported functions/types/constants added, removed, or modified
- Config keys added or changed (`.yaml`, `.toml`, `.json`)
- Workflow inputs/triggers added or changed
- File/manifest paths renamed or restructured
- Behaviorally significant defaults changed (timeouts, versions, namespaces)

> **This skill never runs the PR's code.** No build, test, or coverage step; every
> reviewer prompt forbids it. Only trusted tools run (`git`, `gh`, the CodeRabbit CLI,
> the Codex companion). Coverage is CI's job — see Phase 3.


## Phase 2: Run the review workflow

```
Workflow({
  scriptPath: "<skill-dir>/scripts/workflow.mjs",
  args: {
    pr: <number>,
    repo: "<owner>/<name>",
    repoPath: "<local checkout path>",
    headSha: "<HEAD_SHA>",
    baseSha: "<BASE_SHA>",
    diffPath: "<DIFFPATH>",
    prType: "<classification>",
    changeList: ["<item 1>", "<item 2>"],
    repoNotes: "<3-6 line digest, optional>"
  }
})
```

Pass `changeList` as a real JSON array, not a stringified one. Every lane is
`general-purpose` and inherits the session model, so there is no model argument to
pass.

**What the workflow does** (`scripts/workflow.mjs` is the single source of truth for
the consensus mechanics):

- **Review** — Claude Code (reviews the pinned diff directly; it deliberately does
  *not* delegate to the `code-review` command, whose step 8 instructs its agent to
  `gh pr comment` the result back to the PR), Codex (background dispatch, 10-min
  bounded wait), CodeRabbit (CLI against a detached worktree at `HEAD_SHA`, explicit
  600000 ms timeout — the Bash tool caps at 10 minutes, so a longer timebox cannot be
  enforced), and integration analysis (bounded to `changeList`). Every lane is a
  `general-purpose` agent. All
  parallel, schema-validated, and none may execute the reviewed commit's code.
- **Merge** — dedupe by `path:line:normalized-summary`; duplicates merge to the
  highest severity; a finding citing a file the reporter never listed in
  `filesChecked` is flagged for extra scrutiny.
- **Cross-review (one round, Claude + Codex only)** — each re-reviews independently
  first (anti-anchoring), then returns AGREE/DISAGREE/OPEN_QUESTION per candidate.
  CodeRabbit does *not* take part: its CLI is a slow blocking cloud call and it
  reviews Git changes generically, so it cannot adjudicate our candidate ids and a
  second run over the same commit adds no signal. Its round-1 findings still stand as its AGREE votes, so
  it can still corroborate a split it independently reported. Anything still split
  afterwards is reported as contested for you to settle.
- **Consensus rule** — confirmed = 2 of the 3 reviewer slots AGREE **with evidence**;
  integration analysis is never a reviewer slot. A round-1 finding whose evidence is
  blank or whitespace-only is dropped at intake, so it never registers its reporter as
  a source. In the cross-review round an unevidenced AGREE/DISAGREE instead aborts the
  run (`incomplete`) — dropping it would leave the reviewer's round-1 source vote to
  decide the tally.
- **Verify** — every confirmed finding goes to a fresh adversarial refuter
  (REFUTED → dismissed; UNVERIFIABLE, no result, or a verdict without a citation →
  the `unresolved` array). `consensusReached` is true only when both `contested` and
  `unresolved` are empty — a finding that reached consensus but failed verification is
  an open question, not a settled one.
- **Report incomplete and stop** — Claude, Codex and integration analysis are required
  in round 1, and Claude and Codex must each return exactly one evaluation per
  candidate in the cross-review round. A missing lane, a missing evaluation, a
  duplicate, or an unknown candidate id returns `status: "incomplete"` with the reason
  and raw unverified findings. There is no degraded-consensus mode. CodeRabbit is the
  only best-effort lane: when it does not run, its vote slot records `NONE`, which
  raises the bar (Claude and Codex must then agree) rather than lowering it.

**Operational notes:**

- The workflow runs in the background — wait for its completion notification.
- If it dies mid-run, **resume, don't restart**:
  `Workflow({scriptPath: ..., resumeFromRunId: "<wf_...>"})` — completed lanes replay
  from cache. Empty or odd result → read `<transcriptDir>/journal.jsonl` first.
- A dead Codex broker surfaces as the lane timing out at its bounded wait and
  returning `unavailable`. Codex is required, so the run reports `incomplete` and
  stops; re-run rather than interpreting a partial result.
- CodeRabbit slow runs: check the newest file in `~/.coderabbit/logs/` (429/queue lines
  mean cloud-side queueing) and confirm `which -a coderabbit` resolves to the
  brew-managed binary — a stale `~/.local/bin` copy shadows it.

## Phase 3: CI status for the pinned commit

Do **not** compute coverage locally and do **not** parse CI's coverage comment. The
Merge Gate already enforces the threshold from `.settings.yaml`, forks included. The
coverage comment is posted only for same-repo PRs, carries no head SHA, and is
baselined against the last *successful* main run.

`gh pr checks` reports the PR's **current** head, not `HEAD_SHA`, and the head can move
during a long review. Confirm it first:

```bash
# Separate `if`, not `A && B || C`: gh pr checks exits nonzero for pending (8) and
# failing checks, so chaining would report "head moved" whenever CI is simply red or
# still running.
if [ "$(gh pr view <n> --repo NVIDIA/aicr --json headRefOid -q .headRefOid)" = "<HEAD_SHA>" ]; then
  gh pr checks <n> --repo NVIDIA/aicr   # 0 = green, 8 = still running, other = failing
else
  echo "head moved during review — no CI status for the reviewed commit"
fi
```

If the head moved, omit the CI line rather than reporting another commit's result.
Otherwise report one line: passing, failing (name them), or still running.

Do **not** add `--required`: before the aggregate gate job exists it prints "no required
checks reported" and exits 1, so an ordinary in-progress run looks like an error. Plain
`gh pr checks` exits 8 while running and 0 when green, and handles `skipped`/`neutral`
correctly — unlike a raw check-runs query, which counts them as failures and returns
only the first page (on a green commit: 13 false failures across 30 of 37 checks).

## Phase 4: Consensus report

Build from the workflow's return value plus the CI status line from Phase 3:

```markdown
## Cross-Review Summary for PR #<number>

**Reviewers:** Claude Code, Codex, CodeRabbit + Integration Analysis
**Head commit:** <sha> | **Consensus reached:** Yes/No
**CI for this commit:** <passing | failing: check names | still running>
<note if CodeRabbit was unavailable — it is the only best-effort lane>

### Confirmed Issues (met consensus rule; survived adversarial verification)

| # | File | Line | Severity | Description | Confirmed By |
|---|------|------|----------|-------------|--------------|

### Integration Findings (cross-cutting impact)

| # | Changed File | Consumer File | Severity | Description | Confirmed By |
|---|--------------|---------------|----------|-------------|--------------|

### Unresolved (no settled disposition)

| # | File | Line | Severity | Description | Why unresolved |
|---|------|------|----------|-------------|----------------|

<from the workflow's `unresolved` array: findings that reached consensus but did not
survive adversarial verification, plus findings no reviewer cast a valid vote on; omit
the section if empty>

### Contested Issues (no 2-of-3 disposition)

Split reviewers, a lone dissent, or a finding raised during the cross-review round and
therefore never presented for evaluation.

| # | File | Line | Severity | Description | For | Against | Reasoning |
|---|------|------|----------|-------------|-----|---------|-----------|

### Dismissed Findings

<finding, who flagged it, why dismissed (incl. "failed adversarial verification: ...")>

### Open Questions

<unverifiable findings + reviewers' open questions>

### Residual Risk

<from the workflow's residualRisk array — reviewer-flagged risks that are not
findings; omit the section if empty>

### Positive Observations

<noteworthy good patterns>
```

## Phase 5: Output

**Default: do NOT post.** Present the full report in chat and stop. Do not ask
whether to post.

**Only when explicitly asked to post:** write the filtered summary to a file with the
Write tool, then post it with `--body-file`. Never interpolate the report into a
double-quoted shell argument — findings quote PR content, and backticks or `$(...)` in
a finding would be executed by the shell before `gh` ever runs:

```bash
gh pr comment <n> --repo NVIDIA/aicr --body-file "<report-file>"
```

`<report-file>` is the exact path you passed to Write — a Write-tool call cannot export
a shell variable, so substitute the literal path here.

- Post **issues only**: Confirmed Issues (without the "Confirmed By" column),
  confirmed Integration Findings, Contested Issues, Unresolved, Open Questions.
- **No reviewer-agent attribution and no severity-label prefixes** in posted
  content. State each finding and its evidence plainly.
- Never post Dismissed Findings or Positive Observations.

## Rules

- Never post to the PR without an explicit user request.
- The consensus rule, the required-lane contract, and the single-cross-review-round
  structure live in `scripts/workflow.mjs` — keep it and this doc in sync.
- **This skill never executes the reviewed commit's code.** No builds, tests, coverage,
  package managers, or repository scripts. If a claim can only be settled by running
  something, it is an open question.
- Confirmed integration findings identifying broken consumers escalate to at least
  **medium** severity (done in-script).
- Severity scale: critical (must fix) > major (should fix) > medium > minor.
- Keep the report concise — actionable findings, not noise.
- Never set `dangerouslyDisableSandbox` for reviewer or companion commands; they run
  fine sandboxed. One exception, kept in sync with the Codex dispatch protocol in
  `scripts/workflow.mjs`: the companion writes its job log under
  `~/.claude/plugins/data`, which is sandbox-denied — if dispatch fails on that write,
  bypass for that call only, never for anything touching the working copy or GitHub.
- **Clean up before finishing:** `rm -f <the DIFFPATH echoed in Phase 1>` and delete the two scoped refs
  captured in Phase 1 (`git -C "<repo-path>" update-ref -d "$PRREF"`, same for
  `"$BASEREF"` — use the exact names echoed there, not a guess). Confirm
  no `$TMPDIR/cr-rabbit.*` worktree path remains in
  `git worktree list` — the CodeRabbit lane cleans its own via trap, but verify. Do not
  compare total worktree counts; concurrent sessions change the total legitimately.
