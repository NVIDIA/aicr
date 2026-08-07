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
version: 0.3.19
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
  find "$DIFFPATH" -maxdepth 0 -delete; echo "HEAD moved since setup — restart the review"; exit 1
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

**Verify the workflow script version before Phase 2.** The script about to be passed as
`scriptPath` must contain the sentinel identifier `codexResumeJobId`:
`grep -c codexResumeJobId "<skill-dir>/scripts/workflow.mjs"` — expect a non-zero count.
If it is absent, STOP: the file is a stale or reverted copy, and running it silently
restores the old semantics (observed live: a concurrent session's git operation reverted
uncommitted skill files in a shared checkout, and a full review round ran the old script
unnoticed). The sentinel detects staleness relative to this revision only — if that
identifier is ever renamed, update this check in the same change.

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
  `gh pr comment` the result back to the PR), Codex (background dispatch, a 9-min
  bounded wait plus up to four continuation waits when the job is still running — about 45 min
  for a live job), CodeRabbit (CLI against a detached worktree at `HEAD_SHA`, explicit
  600000 ms timeout — the Bash tool caps any single call at 10 minutes, which is why
  Codex exceeds it by waiting across several calls rather than waiting longer), and integration
  analysis (bounded to `changeList`). Every lane is a
  `general-purpose` agent. All
  parallel, schema-validated, and none may execute the reviewed commit's code.
- **Merge** — dedupe by `path:line:normalized-summary:consumerPath:consumerLine`;
  duplicates merge to the highest severity and union their sources; a finding citing a file
  the reporter never listed in `filesChecked` is flagged for extra scrutiny.

  **Two lanes wording one defect differently stay separate candidates, by design.** Keying
  on location alone was tried and reverted: it did merge those duplicates, but the
  evaluation schema permits exactly one verdict per candidate id, so a merged pair of
  *distinct* same-line defects has no correct verdict — confirming the real one also
  confirms the false one, and refuting the false one dismisses the real one. Retaining both
  summaries prevented data loss but not mis-adjudication, which is the worse failure.

  Instead, candidates sharing a location — `path:line` **and** the same
  `consumerPath`/`consumerLine` — are **flagged** as possible duplicates. The consumer half
  matters: one changed declaration breaking two callers is deliberately two candidates, and
  hinting that they might be duplicates would push reviewers to collapse a distinction the
  key exists to preserve. The flag
  reaches the cross-review candidate list and the refuter prompt, so reviewers decide
  whether the two are one defect and evaluate them consistently. Equivalence stays an
  explicit judgement rather than an assumption from a shared line number.

  Merging also stops once candidates are presented: a late finding that merged into an
  already-evaluated id would inherit votes cast before it existed. Late findings always
  become their own candidate and, being unpresented, stay contested for the human.

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

  **Read `adjudication` on every contested entry.** The bucket holds two different states
  and they need different things from you. `evaluated` means the finding was presented,
  the reviewers voted, and they did not reach 2-of-3 — a genuine split, so break the tie.
  `raised-late` means it was raised *during* the cross-review round, after candidates were
  presented, so nobody cross-evaluated it and its only position is its reporter's — it just
  needs reading. Measured on a real run: 8 of 8 contested findings were `raised-late`, each
  with a single AGREE and NONE elsewhere, so the count read as eight disagreements when
  there were none. `consensusReached` counts both, deliberately: a late finding is
  unadjudicated, and letting it report consensus would be the same overstatement this
  skill exists to avoid.
- **Report incomplete and stop** — Claude, Codex and integration analysis are required
  in round 1, and Claude and Codex must each return exactly one evaluation per
  candidate in the cross-review round. A missing lane, a missing evaluation, a
  duplicate, or an unknown candidate id returns `status: "incomplete"` with the reason
  and raw unverified findings. There is no degraded-consensus mode. CodeRabbit is the
  only best-effort lane: when it does not run, its vote slot records `NONE`, which
  raises the bar (Claude and Codex must then agree) rather than lowering it.

  One deliberate exception, at the level of a **finding** rather than a lane. An
  integration finding claims a specific consumer breaks, so one lacking
  `consumerPath`/`consumerLine` cannot be verified *as an integration claim* — but if it
  still locates a defect (its own path/line) with evidence, it is a perfectly reviewable
  ordinary finding. It is therefore **demoted** — consumer fields stripped, flagged in the
  candidate list, excluded from the integration severity escalation — rather than dropped,
  with a `log()` naming what was demoted. Dropping was tried twice and cost a whole run
  each time: first when the lane returned several findings and one legitimately
  consumer-less observation failed the run; then, after per-finding dropping replaced
  that, when the lane's ONLY finding was such an observation and the zero-survivor rule
  stopped the run with all four lanes `ok` and no report produced (observed on PR 2097).
  The run still stops when every integration finding lacks even a locatable defect or
  evidence — that is the case the fail-closed rule exists for: silently dropping the
  lane's only finding once yielded `consensusReached: true` while a required lane had
  contributed nothing.

  "Contributed nothing" is measured on what survives `intake()`: a demoted finding passes
  through the same coordinate and evidence gates as every other candidate, so a
  consumer-less, whitespace-evidence finding cannot slip through demotion into a
  false-clean. One rule covers the stop: a non-empty integration result that yields no
  accepted finding — neither as an integration claim nor as a demoted ordinary finding —
  stops the run, and the message says how many went for each reason.

  **Coordinates are validated by a single shared rule**, `hasCoords`, applied to a
  finding's own `path`/`line` in `intake()` — every lane, not just integration — and to
  `consumerPath`/`consumerLine` for the integration pair. The response schema **requires**
  `path` and `line` and leaves `consumerPath`/`consumerLine` optional — deliberately, since
  only integration findings carry a consumer — but it constrains none of the four, so
  `""`, `"   "`, `0` and `-1` all satisfy it and all passed a truthiness/null check.
  Tightening the schema instead would fail a whole lane on one bad field, which is the
  all-or-nothing behavior this section exists to remove. It is one helper rather than two
  call-site conditions for a specific reason: every earlier version of this guard fixed the
  pair it was shown and left the other, and a shared rule is what stops the next field pair
  from repeating that.

  **The zero-survivor rule applies to every required round-1 batch**, not just integration.
  Claude's and Codex's round-1 findings go through `intakeBatch`, which reports what
  survived. A required lane whose round-1 findings are *all* malformed contributed
  nothing, and letting them vanish silently is the same false-clean one lane over. Mixed
  batches still proceed. CodeRabbit is exempt: a total loss there records `NONE` and
  raises the bar, exactly as a lane that never ran. Rejection reasons are counted
  separately (`unlocatable` vs `unevidenced`) rather than inferred from a subtraction, so
  the message names the defect the reader should go looking for.

  **It deliberately does NOT apply to cross-review `newFindings`.** Those go through the
  same `intakeBatch` gate — a malformed late finding still never enters the tally — but a
  total loss there is not fatal. The rule tests "this lane contributed nothing", which
  round 1 can assert because findings are the lane's whole output. In the cross-review
  round the lane's output is its *evaluations*, and the completeness gate has already
  returned `incomplete` unless the lane evaluated every presented candidate; `newFindings`
  are supplementary and volunteered. Aborting there would throw away a full set of
  adjudications and the verification round over one imprecise extra finding — the same
  disproportionate total loss seen on PR 1908, one round over. Malformed late findings are
  dropped individually and each drop is logged with its reason.

**Operational notes:**

- The workflow runs in the background — wait for its completion notification.
- If it dies mid-run, **resume, don't restart**:
  `Workflow({scriptPath: ..., resumeFromRunId: "<wf_...>"})` — completed lanes replay
  from cache. Empty or odd result → read `<transcriptDir>/journal.jsonl` first.
- The Codex lane fails in three distinct ways, and the dispatch protocol treats them
  differently:
  - **Lookup miss** — the status call exits 1 with empty stdout and `No job found` on
    stderr. Companion state is keyed by workspace root and each Bash call is a fresh
    shell, so an unpinned lookup resolves to a different workspace and reports a live job
    as unknown; the miss is **not** evidence the job died. **Always recheck exactly once**
    with `--cwd` pinned, whether or not the missing call already carried it — two causes
    produce the identical message and only one is settled by adding the flag. The other is
    transient: in companion v1.0.2 `saveState` writes `state.json` with a plain
    `fs.writeFileSync` (truncate-then-write, no temp-and-rename) while `loadState` wraps
    `JSON.parse` in a bare `catch` returning the **default** state, whose `jobs` list is
    empty. A read landing inside that write window yields a well-formed `No job found`
    rather than an error, and the background worker is rewriting that file precisely while
    the status call runs. So an identical repeat need not return an identical answer. Once
    a pinned recheck has also missed, return `unavailable` saying the job could not be
    located — never re-dispatch (the original may still be running) and never record it as
    exhausted budget.
  - **Fast transient failure** — retried **once**, and only when all three hold:
    `.job.status` is `failed` (never `cancelled`), the job died in **under 60 seconds**
    by its own timestamps, and the error names a known-retryable cause such as an upstream
    capacity rejection (`Selected model is at capacity`) or a transient dispatch fault.
    The threshold is a number rather than a judgment because the observed cases are far
    apart — a capacity rejection at ~10s against a genuine timeout at 10m19s — and an
    undefined "quickly" is how a one-retry budget erodes.
    A retry then costs seconds and these clear on their own.
    **`cancelled` is never retried** — the companion emits it only for explicit
    cancellation, so a retry would restart work someone deliberately stopped. Nor is a
    late failure: `.waitTimedOut` false only means the job became terminal before the
    inner deadline, which a failure at 8:59 also satisfies while having burned the whole
    window. An unrecognised error is not assumed retryable either.
  - **Wait elapsed, job still alive** (`.waitTimedOut` true with `.job.status` still
    `queued`/`running`) — not the end of the lane. The job is dispatched in the background
    and outlives the Bash call waiting on it, so it needs more **time**, not another
    attempt — and the protocol now gives it exactly that: **up to four continuation waits** on the
    *same* job id in fresh calls. Those are not retries; nothing is re-dispatched and no
    work is duplicated. A fifth `.waitTimedOut` is then genuinely exhausted budget, and
    the lane returns `unavailable` with the job id in the structured `jobId` field —
    required, not prose — so the result can be fetched or the run resumed later.
    Measured on a real run: the lane timed out at the full 540000 ms while the job was
    demonstrably mid-work, the job was **still** `running` long after the review was
    abandoned, and its result stayed retrievable — so reporting exhausted budget there
    discarded a required lane, and the whole review with it, over a job that had merely
    not finished.
  - **Exhausted budget** — a fifth `.waitTimedOut`, or no parseable JSON at all because
    the outer timeout killed the call (a dead broker). No retry, no further wait.

  The ceiling is not tunable — the wait runs inside a Bash call, and that tool silently
  kills any foreground command at 600000 ms. Exceeding 10 minutes requires polling across
  several calls, which is exactly what the continuation wait above does: a live job gets
  five waits, roughly forty-five minutes, without a longer single call. The budget was
  three waits until a real ~53-minute job on a +1951/−153 PR outlasted even that — hence
  five waits, and a resumable `jobId` on exhaustion instead of lost work. The inner wait is
  therefore 540000 ms,
  deliberately **below** the outer cap: were the two equal, Bash could kill the command
  before it printed its JSON, leaving no `.waitTimedOut` to classify on. That is why an
  unclassifiable kill counts as exhausted-budget rather than a fast failure — guessing
  wrong there costs another full window for nothing. The lane still reports the job id
  it was waiting on (`jobId`), so even that kill stays resumable.
- Codex is required, so a lane that is still unavailable after its retry makes the run
  report `incomplete`; re-run rather than interpreting a partial result.
- **`incomplete` with a `codexJobId` means the review is NOT lost.** That field is the
  live Codex job that outlasted the wait budget, surfaced top-level (alongside
  `reviewerStatus`) precisely so recovery is mechanical, not improvised. Poll the job
  with the companion status command until it is terminal (a background 60-second loop is
  fine — polling is cheap once the workflow is no longer holding a lane open for it),
  then resume: `Workflow({scriptPath, resumeFromRunId: "<wf_...>", args: {...prevArgs,
  codexResumeJobId: "<job id>"}})` — `prevArgs` is the previous run's args object, unchanged. The three completed lanes replay from cache, the
  Codex lane fetches the existing job's result without dispatching a second one, and the
  run proceeds to cross-review and verification normally. Proven live on PR 2097: a
  ~53-minute job outlasted the then-three-wait budget, and the resumed run recovered it
  with zero re-dispatched work.
- **Execute the CodeRabbit lane's STEP blocks verbatim** — same commands and paths, no
  substitutions; in particular never swap the `find … -delete` cleanup for `rm` (an
  invented `rm -rf` cleanup once blocked a run for hours on a managed-policy
  confirmation prompt).
- CodeRabbit slow runs: check the newest file in `~/.coderabbit/logs/` (429/queue lines
  mean cloud-side queueing) and confirm `which -a coderabbit` resolves to the
  brew-managed binary — a stale `~/.local/bin` copy shadows it.
- **A sandboxed CodeRabbit run hangs instead of failing.** `~/.coderabbit` is outside the
  default sandbox write allowlist, so the CLI cannot create its log or review store; it
  stalls at `connecting_to_review_service` until the timebox kills it. The lane therefore
  **probes** in step 1 whether `~/.coderabbit` is sandbox-writable, and runs the
  `coderabbit` command (and only that command) with sandbox bypass exactly when the probe
  says it is not.

  **Why probe-gated bypass rather than an allowlist assumption.** Adding `~/.coderabbit`
  to the sandbox write allowlist is the narrower grant, but this skill is checked into
  the repo and has to work on a contributor's machine as written: an allowlist entry
  lives in each person's local settings, so a lane that *assumed* it would hand anyone
  who has not made the edit the silent ten-minute hang above rather than a usable lane.
  And the hang means "try sandboxed, fall back on failure" without a probe costs a full
  timebox per wrong guess. The step-1 probe settles it in milliseconds: machines with
  the allowlist entry run step 2 fully sandboxed and pay **no bypass prompt at all**;
  every other machine gets the bypass from the start, portable and self-documenting at
  the call site. Add `~/.coderabbit` (and `~/.claude/plugins/data`, for the Codex
  companion's job log) to your local sandbox `filesystem.allowWrite` if you run this
  often — that is what removes the per-round approval prompts — but the skill must not
  depend on it.

  **Diagnose it by absence:** a stall at `connecting` *with no new file in
  `~/.coderabbit/logs/`* is sandbox denial — a process killed mid-run still flushes a
  partial log, so zero bytes means it never created one. A real cloud problem leaves a log
  with 429/queue lines. Do not read this stall as an outage or as contention with another
  session: an unsandboxed run succeeding while a sandboxed one hangs looks exactly like
  contention and is not.
- **Persisted-store fallback.** If a run still fails, the lane checks
  `~/.coderabbit/reviews/*/*/reviews/*/git.json`, whichever session produced the record.
  Acceptance is `head` plus **the pinned change itself** — never `baseCommitId`. Measured:
  the CLI writes `baseCommitId` from the base it resolves locally in that working
  directory, not from the `--base-commit` the lane passes. In one real store the same head
  carried two different values (a main tip, and a commit not on main at all), while three
  other valid records carried the stale local `main` rather than the base Phase 1 fetched
  from the canonical repo. Gating on equality discarded good reviews whenever the local ref
  lagged — the common case in the fork layout this skill supports. `baseCommitId` is now
  reported in `statusNote` and never branched on.

  What identifies the review instead is the **blob OIDs the record already stores**.
  Acceptance is layered, cheapest first, and only the last step is identity:

  1. Same paths, from `git diff --numstat -z` over the **three-dot** range Phase 1 saved.
     Two-dot would compare the base tip to the head and pull in everything that landed on
     the base after branching — on a real pair, 13 files against the valid record's 59.
     `-z` matters independently: a plain `split()` shreds a path containing a space into
     separate members and would reject every record for such a PR.
  2. Same per-file line counts, against the record's `linesAdded` / `linesRemoved`.
  3. The committed-only `lanes` shape below.
  4. The **file modes and blob OIDs** the record's own patch states, equal to what
     `git diff --raw --no-abbrev` reports for the pinned range.

  Steps 1–2 are a prefilter, never a result: two different patches collide on a numstat
  trivially — a `beta`→`BETA` edit and a `gamma`→`GAMMA` edit in the same file both report
  one added and one removed, and a review of this head against another base can leave
  exactly such a record.

  Step 4 compares OIDs rather than the stored patch text, which was the obvious move and
  is wrong. Bare `git diff` output moves with the reader's gitconfig — measured, the text
  differs under `diff.noprefix` and `diff.context`, and fails outright under
  `diff.external` — so a contributor with ordinary settings gets a permanent mismatch and
  this fallback silently never fires. That is the same works-on-my-machine class the
  sandbox decision rejected an allowlist for. Pinning flags (`--no-ext-diff`, `--no-color`,
  `-U3`, `--src-prefix`) fixes *our* side only; if the CLI produced the record under that
  same config, hardening makes the mismatch worse. Blob OIDs are content hashes: stable on
  both sides, and exact content identity rather than a rendering of it. Both git calls
  still pass `--no-ext-diff --no-color`, since the `--raw` call needs them.

  **Modes are checked before OIDs, because OIDs alone are not identity.** With a `chmod +x`
  in one commit and an edit in the next, a stored review of the *edit alone* carries the
  same path, counts, `lanes` and **both** blob OIDs as the pinned range covering both
  changes — verified. Only the modes differ: pinned `100644 → 100755` against the record's
  `100755 → 100755`. Git states modes in one of four shapes (`old mode`/`new mode`,
  `new file mode`, `deleted file mode`, or the trailing mode on the `index` line), and the
  trailing mode is absent exactly when the `old`/`new` pair is present, so the cases do not
  overlap. A record that states no mode at all cannot prove identity and is skipped.

  An entry with **no** `index` line is not automatically rejected. Git omits that line
  exactly when the blob is unchanged — for a `chmod +x` the mode lines carry the
  information instead, and the mode check above has already matched them. Such an entry is
  accepted when the pinned diff agrees the blob is unchanged (`src == dst`), and rejected
  as `no-index-line` otherwise. Rejecting on sight would have disabled this fallback for
  any PR that merely marks a script executable, which this repo does routinely. A
  rename or copy in the pinned range is rejected outright as `rename-unverifiable`, by an
  explicit status check rather than incidentally: the record is keyed on the post-rename
  path and carries no source path, so an edit-only review of that path is indistinguishable
  from one that covered the rename — measured, a pinned `R077 old.txt -> new.txt` and a
  stored `M new.txt` share modes, blobs and line counts. The scope prefilter usually rejects
  such a record first, since the CLI scopes
  its stored patch and counts to the post-rename path, so the record's numstat differs from
  the pinned range's and the scope prefilter rejects it first.

  Every entry must additionally prove it was **committed-only** — `lanes` is a boolean map
  (`{"committed": true, "uncommitted": false}`), so the check tests the values and requires
  that exact shape. Merely rejecting a truthy `uncommitted` was not enough: a record from
  an older CLI that omits `lanes`, or carries a non-dict, would pass as clean and
  contribute comments on code outside the pinned head. Absent proof of clean, skip.

  The pinned diff itself is computed **fail-closed**: an unreachable base exits non-zero
  with empty output, and treating that as an empty result would reject every record and
  then report a clean "nothing matched" — so the scan aborts with an `ERROR` line instead.
  Several records can also pass every check at once (a re-run after a transient failure
  produces a twin), so candidates are ordered by mtime and exactly one `MATCH` is emitted:
  newest wins, since a re-run supersedes what it replaced.

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

<only findings with a verified consumer pair (non-null `consumerPath`/`consumerLine`)
belong here. A finding flagged "demoted from integration claim" has no consumer pair —
route it to Confirmed Issues like any ordinary finding, even though its `sources`
include the integration lane; omit the section if empty>

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
- Confirmed integration findings identifying broken consumers (a verified
  `consumerPath`/`consumerLine` pair) escalate to at least **medium** severity
  (done in-script); findings demoted from the integration lane do not.
- Severity scale: critical (must fix) > major (should fix) > medium > minor.
- Keep the report concise — actionable findings, not noise.
- Never set `dangerouslyDisableSandbox` for reviewer or companion commands; they run
  fine sandboxed. **Exactly two exceptions**, both kept in sync with the protocols in
  `scripts/workflow.mjs`, both conditional on the sandbox actually denying the write,
  and both scoped to a single command that performs no Git operation, no working-copy
  mutation, and no GitHub write. (They do *read* files — CodeRabbit necessarily reads
  the detached worktree it was pointed at. The rule bars bypassing calls that **act on**
  the working copy, not calls that read a path):
  - **Codex companion** — it writes its job log under `~/.claude/plugins/data`, which is
    sandbox-denied by default. If dispatch fails on that write, bypass for that call
    only; a machine whose sandbox allowlist covers the path never needs the bypass.
  - **CodeRabbit review** — `~/.coderabbit` is outside the default write allowlist, so a
    sandboxed CLI cannot create its log or review store and hangs at
    `connecting_to_review_service` until the timebox kills it. Step 1 of the three-step
    CodeRabbit protocol probes whether `~/.coderabbit` is sandbox-writable; when it is
    not, bypass **step 2 only**, which is a lone `coderabbit review` command. When the
    probe passes, step 2 runs sandboxed and no bypass happens at all. Worktree setup and
    cleanup live in steps 1 and 3 and stay sandboxed always.

  Anything else stays sandboxed. In particular, never bypass a call that also performs
  `git` operations — that is why the CodeRabbit protocol is split into three calls rather
  than the single invocation it used to be.
- **The tool shell is zsh, and every prompt that hands out a shell command says so.**
  `scripts/workflow.mjs` defines a single `SHELL_CONTRACT` constant, interpolated in
  **exactly one place** — `NO_EXECUTION`, which every prompt builder composes exactly
  once. So all six assembled prompts carry it exactly once. Do not add a second
  interpolation: `NO_EXECUTION` already embeds `PINNED_READS`, so putting it in
  `PINNED_READS` or `CODEX_LEAN` as well silently doubles it in every lane. That is how
  the first attempt got it wrong, and no block-level check can see it — the duplication
  only appears once the prompts are assembled. It covers the
  three differences that bite silently: `shopt` and other bash builtins are simply absent,
  an unmatched glob is a hard error that aborts the command list rather than passing
  through literally, and `status` is readonly. Keep it in one place — the two blocks that
  once held hand-written copies each acquired theirs only after a zsh bug had already
  shipped in them, and a new lane must not be able to omit it by accident.
- **Clean up before finishing:** `find "<the DIFFPATH echoed in Phase 1>" -maxdepth 0 -delete
  2>/dev/null || true` — idempotent, because under `set -e` a diff file already removed by an
  earlier abort path would otherwise fail the call and skip the ref cleanup below —
  and delete the
  two scoped refs captured in Phase 1 (`git -C "<repo-path>" update-ref -d "$PRREF"`,
  same for `"$BASEREF"` — use the exact names echoed there, not a guess). Confirm no
  `${TMPDIR:-/tmp}/cr-rabbit.*` worktree path remains in `git worktree list` — write the
  fallback out, since setup creates the worktree under `${TMPDIR:-/tmp}` and with `TMPDIR`
  unset a bare `$TMPDIR/cr-rabbit.*` names `/cr-rabbit.*` while the leak sits in `/tmp` —
  the CodeRabbit
  lane cleans its own in step 3, but verify, since a lane killed between steps 2 and 3
  leaves one behind. Step 1 of the next run reaps `cr-rabbit.*` directories older than
  120 minutes, which bounds the leak but does not clear it now. Do not compare total
  worktree counts; concurrent sessions change the total legitimately.
  Deletion is `find … -delete` rather than `rm` throughout this skill and its workflow —
  managed (admin-deployed) permission policies commonly gate `rm` behind a confirmation
  prompt (`Bash(rm:*)` ask rules match every sub-command and override allow rules), and a
  background lane blocked on a prompt stalls the review. Do not "simplify" back to `rm`.
