// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Claude Code Workflow script — runs in a custom async execution context where
// top-level `return` statements are valid (they return the workflow result).
// Standard JS linters may flag those as parse errors; that is expected.
export const meta = {
  name: 'aicr-cross-review',
  description: 'Multi-reviewer PR review (Claude Code, Codex, CodeRabbit) with integration impact analysis, consensus rounds, and adversarial verification of confirmed findings',
  whenToUse: 'Invoked by the aicr-cross-review skill after Phase 1 setup; args carry the pinned SHAs, saved diff path, PR type, and bounded change list.',
  phases: [
    { title: 'Review', detail: '3 reviewers + integration impact analysis, parallel' },
    { title: 'Cross-review', detail: 'independent re-review, then AGREE/DISAGREE on every candidate' },
    { title: 'Verify', detail: 'one adversarial refuter per confirmed finding' },
  ],
}

// ---------- args ----------
// Tolerate args arriving as a JSON string (observed 2026-07-21: the harness
// delivered `args` stringified even when the tool call passed a real object).
const parsedArgs = typeof args === 'string' ? JSON.parse(args) : (args || {})
const { pr, repo, repoPath, headSha, baseSha, diffPath, prType, changeList, repoNotes } = parsedArgs
for (const [k, v] of Object.entries({ pr, repo, repoPath, headSha, baseSha, diffPath, prType })) {
  if (!v) throw new Error(`missing required arg: ${k}`)
}
const changes = Array.isArray(changeList) ? changeList : []
// ---------- schemas ----------
const FINDING_ITEM = {
  type: 'object',
  additionalProperties: false,
  required: ['severity', 'path', 'line', 'summary', 'evidence', 'impact'],
  properties: {
    severity: { type: 'string', enum: ['critical', 'major', 'medium', 'minor'] },
    path: { type: 'string', description: 'repo-relative file path' },
    line: { type: 'integer' },
    summary: { type: 'string' },
    evidence: { type: 'string', description: 'what in code proves the issue (path:line + fact)' },
    impact: { type: 'string', description: 'who breaks / what regresses' },
    consumerPath: { type: 'string', description: 'integration findings only: the consumer-side file. One consumer per finding — report a second broken consumer as a separate finding.' },
    consumerLine: { type: 'integer' },
  },
}

const FINDINGS_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['status', 'findings', 'openQuestions', 'filesChecked'],
  properties: {
    status: { type: 'string', enum: ['ok', 'unavailable'] },
    statusNote: { type: 'string', description: 'when unavailable: what failed (broker dead, cloud timeout, CLI missing)' },
    findings: { type: 'array', items: FINDING_ITEM },
    openQuestions: { type: 'array', items: { type: 'string' } },
    residualRisk: { type: 'array', items: { type: 'string' } },
    positives: { type: 'array', items: { type: 'string' } },
    filesChecked: { type: 'array', items: { type: 'string' }, description: 'files actually read (full or targeted excerpts), not just the diff' },
  },
}

const EVAL_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['status', 'evaluations', 'newFindings'],
  properties: {
    status: { type: 'string', enum: ['ok', 'unavailable'] },
    statusNote: { type: 'string' },
    evaluations: {
      type: 'array',
      items: {
        type: 'object',
        additionalProperties: false,
        required: ['id', 'verdict', 'reason'],
        properties: {
          id: { type: 'string' },
          verdict: { type: 'string', enum: ['AGREE', 'DISAGREE', 'OPEN_QUESTION'] },
          evidence: { type: 'string', description: 'path:line — REQUIRED for AGREE and DISAGREE' },
          reason: { type: 'string' },
        },
      },
    },
    newFindings: { type: 'array', items: FINDING_ITEM },
    filesChecked: { type: 'array', items: { type: 'string' }, description: 'files actually read during the independent re-review' },
  },
}

const REFUTE_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['verdict', 'reason'],
  properties: {
    verdict: { type: 'string', enum: ['CONFIRMED', 'REFUTED', 'UNVERIFIABLE'] },
    evidence: { type: 'string', description: 'path:line you independently checked — REQUIRED for CONFIRMED and REFUTED' },
    reason: { type: 'string' },
  },
}

// ---------- shared prompt fragments ----------
// codex uses general-purpose (not codex:codex-rescue) so it has full multi-step Bash
// access for the dispatch protocol; codex-rescue prefers background execution and may
// return a job handle instead of structured findings.
// All lanes use general-purpose: the CodeRabbit prompt drives the CLI itself, so the
// `coderabbit:code-reviewer` plugin agent is an undocumented dependency we do not need.
const AGENT_TYPE = { claude: 'general-purpose', codex: 'general-purpose', coderabbit: 'general-purpose' }

const header = `PR #${pr} on ${repo}, head commit ${headSha} (review THIS commit).
Saved diff: ${diffPath} — read the diff from this file; do NOT re-fetch it (the branch may move mid-review; every reviewer must see the same code).
Repo working copy: ${repoPath}.`

// The working copy may be on a different branch (concurrent sessions), so every
// read and search goes through the pinned commit tree.
// Placeholders are substituted with PR-controlled values (filenames, config keys), so
// they go inside SINGLE quotes. Double quotes stop globbing but still expand $(...)
// and backticks, so a filename like docs/$(id).md would execute — the exact thing this
// skill promises not to run. Single quotes are the only literal form in sh/bash/zsh.
const PINNED_READS = `  read:   git -C "${repoPath}" show '${headSha}:<path>'
  search: git -C "${repoPath}" grep -n -e '<pattern>' ${headSha} -- '<glob>'
  SINGLE quotes are required and must not be changed to double quotes: they are what
  makes the substituted value literal. If a path or pattern itself contains a single
  quote it cannot be read this way. Do NOT escape it and do NOT downgrade it to an
  open question — an uninspectable file means this lane could not do its job, so
  return status:"unavailable" naming the path (a verifier returns UNVERIFIABLE).
  Open questions do not block consensus; an unavailable required lane does.`

// This skill reviews code; it never runs it. The intentional execution path (the Go
// coverage lane) was deleted, but these lanes are general-purpose agents with shell
// access, so the prohibition has to be stated rather than assumed.
const NO_EXECUTION = `
Execution rules:
- Do NOT execute anything from the reviewed commit: no tests, builds, package managers,
  repository scripts, Makefile targets, generators, or checked-in binaries. Not even to
  "verify" a finding — an unrunnable claim is an open question, not a reason to run it.
- Do NOT post to GitHub and do NOT mutate the working copy: no "gh pr comment", no
  "gh pr review", no commits, pushes, or branch switches.
- ALLOWED: the trusted commands this prompt explicitly prescribes anywhere (including any
  detached worktree or CLI invocation it spells out), read-only "gh" queries, and the
  saved diff plus pinned reads/searches:
${PINNED_READS}`

const OUTPUT_RULES = `
Reporting rules:
- Report only findings you verified in code. Do not report preferences or speculative concerns as findings.
- If something might be wrong but you cannot verify it, put it in openQuestions, not findings.
- Do not cite upstream charts, external APIs, or third-party docs unless you actually fetched and read them in this session; otherwise it is an open question.
- "No findings" is a valid and valuable outcome. Do not reach for speculative findings to avoid returning empty-handed.
- Every finding needs exact path + line + evidence + impact.
- List every file you actually read in filesChecked.`

const CODEX_LEAN = `
Context rules (IMPORTANT — the Codex broker reproducibly crashes mid-generation on large accumulated context; verified on PR #1196):
- Work primarily from the saved diff (cat "${diffPath}"). Read only targeted excerpts at the pinned commit:
    git -C "${repoPath}" show '${headSha}:<path>' | sed -n '<start>,<end>p'
- For consumer/caller searches use git grep against the pinned tree, not bare rg (which searches the mutable working copy). Use exactly the quoted recipe above; do not unquote it.
- Do NOT read CLAUDE.md.
- Before reporting a missing path/key/field/API, confirm absence at the pinned commit with a targeted check, not a full-file read.`

const CODEX_DISPATCH = `
Codex dispatch protocol (mandatory):
1. comp=$(ls -t ~/.claude/plugins/cache/openai-codex/codex/*/scripts/codex-companion.mjs | head -1)
2. Dispatch the review as a BACKGROUND job (pass --background to codex-companion task) so it returns a job id immediately. NEVER run foreground: a foreground call hangs forever if the broker dies mid-turn.
3. Wait for it with the companion's own bounded wait — do NOT hand-roll a poll loop:
     node "$comp" status <job-id> --wait --timeout-ms 600000 --poll-interval-ms 30000 --json
   Read the returned JSON yourself. The job state is at .job.status (NOT a top-level "status" field), and .waitTimedOut is true if the 10-minute cap expired while the job was still queued/running.
4. .job.status == "completed" → fetch the payload with: node "$comp" result <job-id> --json  (NOT status, which returns only a job summary).
   Anything else (failed, cancelled, or .waitTimedOut) → return status:"unavailable" with the observed .job.status in statusNote. Do NOT retry and do NOT substitute your own review — an unavailable Codex lane is the expected outcome here.
Every Codex task you dispatch must carry the execution rules verbatim in its own prompt — Codex runs in its own shell and does not inherit this one's constraints.
Shell note: never name a shell variable "status" (readonly in zsh; the assignment silently fails).
Sandbox: the review itself runs sandboxed. The one known exception is the companion's own job log under ~/.claude/plugins/data, which is sandbox-denied — if dispatch fails on that write, bypass for that call only. Never bypass for anything else, and never for a call that touches the working copy or GitHub.`

const REVIEW_FOCUS = {
  'code-change': `Review for bugs, regressions, broken consumers, security issues, and instruction/config compliance.

Behavioral correctness (review the changed code's internal logic, not just its callers):
- For each materially changed function or new control-flow branch, trace concrete inputs through happy path, error path, and edge cases; check actual behavior matches comments, tests, and the PR description.
- For fallback/reset/retry/exclusion logic, verify the scope precisely: it must affect only the intended state and not discard unrelated data.
- For metadata/diagnostic output (warnings, errors, logs, status fields), verify names/paths/IDs derive from the real triggering context, not placeholders or stale variables.
- For loops or multi-branch state assembly, check accumulated maps/slices/sets stay consistent across all branches, including early returns and partial-failure paths.

Consumer search:
- Search for consumers of changed exported APIs, config keys, flags, env vars, image names, workflow inputs, file paths, and cross-file behavior changes.
- Check CI/workflows (.github/, .gitlab-ci.yml, Makefile, Helm charts, deployment scripts), tests, fixtures, scripts, and docs that depend on old behavior.
- Skip purely local/private helper callers unless the behavior change escapes the file.`,
  'adr': `This PR is an ADR/design doc. Read the full changed document (prose, usually one file — fine to read in full). Read only the specific prior ADRs/implementation sections a given claim depends on.

Review for concrete design gaps:
- Missing required contracts for correctness
- Unacknowledged behavior changes vs the current system
- Missing operational semantics (failure, rollback, migration, version requirements)
- Claims that do not connect to actual codebase concepts or prior ADRs
Only report a finding if you can point to the exact doc line plus supporting code/doc evidence. No generic style preferences.`,
  'config-change': `Review for correctness, downstream consumers, and environment impact.
- For each changed config value, search the repo for all consumers that read or depend on it.
- Check CI workflows (.github/, .gitlab-ci.yml, Makefile, Helm charts, deployment scripts), application code, and tests referencing the changed keys.
- Skip purely local references unless the change crosses boundaries.`,
  'documentation-only': `Review the changed docs for:
- Factual accuracy: do the docs match what the code actually does?
- Stale references: do linked files, functions, flags, and config keys still exist?
- Missing context: omitted caveats, prerequisites, version requirements.
Read only targeted excerpts of the code/config the docs describe. No style/formatting preferences.`,
}

// ---------- Round 1 prompts ----------
function claudeReviewPrompt() {
  // Deliberately NOT delegating to Skill("code-review:code-review"): that command's
  // step 8 instructs its agent to `gh pr comment` the result back to the PR, and it
  // carries Bash(gh pr comment:*) in allowed-tools. A prompt asking it to skip that
  // is advisory, not a write barrier — this skill must never post by default.
  return `You are the Claude Code reviewer in a multi-reviewer cross-review.
${header}
${repoNotes ? `Repo conventions relevant to this review:\n${repoNotes}\n` : ''}
Do NOT post anything to GitHub. Do not run "gh pr comment", "gh pr review", or any
other write command; your findings are returned to this prompt only.

Review the saved diff thoroughly, then verify every finding at the pinned commit:
${PINNED_READS}
${REVIEW_FOCUS[prType] || REVIEW_FOCUS['code-change']}
Only return findings that survive verification. Before reporting a missing path/config key/field/API, read the full existing file at the pinned commit to confirm it is actually absent.
${NO_EXECUTION}
${OUTPUT_RULES}`
}

function codexReviewPrompt() {
  return `You drive the Codex reviewer in a multi-reviewer cross-review.
${header}
${CODEX_DISPATCH}

${NO_EXECUTION}

Compose a LEAN Codex task prompt containing the saved-diff path, these review instructions, the execution rules above, and the reporting rules:
${REVIEW_FOCUS[prType] || REVIEW_FOCUS['code-change']}
${CODEX_LEAN}
${OUTPUT_RULES}
Translate Codex's raw output into the structured result yourself.`
}

// The CodeRabbit CLI runs exactly once, in round 1; the cross-review round replays
// its saved findings instead of paying for another slow cloud call.
const CODERABBIT_RUN = `Run this WHOLE sequence in a single Bash invocation so the EXIT trap covers agent death or timeout:
   set -euo pipefail
   CRROOT=$(mktemp -d "\${TMPDIR:-/tmp}/cr-rabbit.XXXXXX")
   trap 'git -C "${repoPath}" worktree remove --force "$CRROOT/head" 2>/dev/null || true; rm -rf "$CRROOT"' EXIT
   git -C "${repoPath}" worktree add --detach "$CRROOT/head" ${headSha}
   # Blocks until the cloud review completes; do NOT use "-t uncommitted".
   coderabbit review --agent --base-commit ${baseSha} --dir "$CRROOT/head"
Accept the run ONLY if its reported baseCommit equals ${baseSha}; otherwise discard it and return status:"unavailable" with what it reported. Ignore currentBranch and baseBranch — a detached worktree correctly reports currentBranch:"HEAD" and the CLI's inferred baseBranch stays unrelated even when --base-commit is honored.
Timebox: run this Bash call with an explicit timeout of 600000 ms. The Bash tool defaults to 2 minutes and is capped at 10, so an unset or larger timeout silently kills the run. If it does not finish inside that window, check the newest file in ~/.coderabbit/logs/ for 429/queue lines and return status:"unavailable" with what you saw in statusNote — CodeRabbit is best-effort and the review continues without it.`

function coderabbitReviewPrompt() {
  return `You are the CodeRabbit reviewer in a multi-reviewer cross-review.
${header}
MANDATORY: your findings must come from an actual CodeRabbit CLI review that COMPLETED against the pinned commits. You are a wrapper around CodeRabbit, not a substitute for it — your own direct code reading is the same model as the Claude lane and must never be counted as CodeRabbit's vote. If the CLI review did not run, did not complete, or reviewed the wrong context, return status:"unavailable" with a statusNote.

${CODERABBIT_RUN}
Do NOT post anything to GitHub.

VERIFY CodeRabbit's findings before returning them (verification only — add none of your own) against the pinned tree:
${PINNED_READS}
Before returning a "missing path/key/field/API" finding, read the full existing file at the pinned commit to confirm it is actually absent; drop findings that fail verification and note them in statusNote.
${NO_EXECUTION}
${OUTPUT_RULES}`
}

function integrationPrompt() {
  const list = changes.length ? changes.map((c) => `- ${c}`).join('\n') : '- (none extracted — return an empty findings list)'
  return `Integration impact analysis for a cross-review. This catches issues invisible when reviewing the diff in isolation.
${header}
Verify ONLY these specific changed items. Say nothing for an item when nothing real is found. Do NOT expand the search beyond this list:
${list}

For each item:
1. Search the repo for callers, consumers, and references — beyond the files in the diff.
2. Check CI/CD (.github/workflows/, .github/actions/, .gitlab-ci.yml, Makefile, Helm charts, Tiltfile, deployment scripts), test fixtures and integration tests (testdata/, tests/), docs, and config files.
3. Distinguish "definitely breaks" (consumer depends on exact old behavior) from "might break" (depends on runtime conditions) in the impact field.
4. Report a finding ONLY with both sides as evidence: path/line = changed side, consumerPath/consumerLine = consumer side. ONE consumer per finding — if a second consumer breaks the same way, report it as a separate finding so each is verified independently.

Rules:
- Read and search at the pinned commit, NOT the mutable working copy:
${PINNED_READS}
  The diff shows changes; the pinned tree shows current state.
- Do not cite upstream charts, external APIs, or third-party docs unless you actually fetched them; unverifiable external claims go in openQuestions.
- "No integration impact" is a valid outcome. Do not invent impacts to justify the analysis.
${NO_EXECUTION}
${OUTPUT_RULES}`
}

// ---------- Cross-review prompt ----------
function findingBlock(c) {
  return [
    `${c.id} [${c.severity}] ${c.path}:${c.line} — ${c.summary}`,
    `  Evidence: ${c.evidence}`,
    `  Impact: ${c.impact}`,
    c.consumerPath ? `  Consumer: ${c.consumerPath}:${c.consumerLine || '?'}` : null,
    `  Sources: ${c.sources.map((s) => (s === 'integration' ? '[Integration]' : s)).join(', ')}`,
    c.flags.length ? `  Flags: ${c.flags.join('; ')}` : null,
  ]
    .filter(Boolean)
    .join('\n')
}

function crossPrompt(k, items) {
  const list = items.map(findingBlock).join('\n\n')
  const perReviewer =
    k === 'codex'
      ? `${CODEX_DISPATCH}\n${CODEX_LEAN}\nFor each candidate, have Codex read just the cited lines at the pinned commit (git -C "${repoPath}" show '${headSha}:<path>' | sed -n) before returning a verdict.${prType === 'code-change' ? '\nDuring the independent re-review apply the behavioral correctness checks: trace inputs through happy/error/edge paths; verify the scope of fallback/reset/retry logic; verify diagnostic metadata derives from real context; check multi-branch state consistency.' : ''}`
      : `Re-read the saved diff at ${diffPath} and search/read the repo at the pinned commit (not the mutable working copy):
${PINNED_READS}
For "missing X" candidates, read the full existing file at the pinned commit to confirm absence.`
  return `You are the ${k} reviewer in the cross-review round.
${header}

${`First, INDEPENDENTLY re-review the PR before reading the candidate list — cross-reference the wider repo, not just the diff${prType === 'adr' ? ', including existing architecture, prior ADRs, and codebase patterns' : ''}. Put anything you missed in Round 1 into newFindings and list every file you actually read in filesChecked. THEN evaluate every candidate below.`}

${perReviewer}

Candidate findings:
${list}

${NO_EXECUTION}

Evaluation rules:
- Return exactly one evaluation per candidate id: AGREE / DISAGREE / OPEN_QUESTION. This is the ONLY adjudication round — anything still split afterwards is reported as contested, so take a real position where you can.
- AGREE only if you directly checked the cited file(s); put the checked path:line in evidence.
- DISAGREE must include counter-evidence (path:line) in evidence.
- An AGREE or DISAGREE without evidence ABORTS the whole review — the runtime returns incomplete. Use OPEN_QUESTION when you have not checked.
- [Integration] findings are the LEAST reliable source and get your deepest scrutiny: for "missing path/key" claims verify absence yourself; for upstream/dependency assumptions fetch the source or return OPEN_QUESTION; an unverifiable integration claim defaults to OPEN_QUESTION, never AGREE.
- Multi-source findings are NOT automatically true — multiple reviewers can converge on the same wrong surface-level claim without checking the full file. Verify the evidence yourself.`
}

// ---------- Phase: Review (barrier justified: cross-review needs the merged candidate list) ----------
log(`Round 1: launching Claude Code, Codex, CodeRabbit + integration analysis for PR #${pr} (${prType})`)
const [claudeR, codexR, rabbitR, integR] = await parallel([
  () => agent(claudeReviewPrompt(), { label: 'review:claude', phase: 'Review', schema: FINDINGS_SCHEMA, agentType: AGENT_TYPE.claude }),
  () => agent(codexReviewPrompt(), { label: 'review:codex', phase: 'Review', schema: FINDINGS_SCHEMA, agentType: AGENT_TYPE.codex }),
  () => agent(coderabbitReviewPrompt(), { label: 'review:coderabbit', phase: 'Review', schema: FINDINGS_SCHEMA, agentType: AGENT_TYPE.coderabbit }),
  // general-purpose, not Explore: Explore reads excerpts to locate code and is
  // explicitly not a review/audit agent, which is exactly what this lane does.
  () => agent(integrationPrompt(), { label: 'review:integration', phase: 'Review', schema: FINDINGS_SCHEMA, agentType: 'general-purpose' }),
])

const ok = (r) => (r && r.status === 'ok' ? r : null)
const R = { claude: ok(claudeR), codex: ok(codexR), coderabbit: ok(rabbitR) }
const integ = ok(integR)
const statusOf = (r) => (r ? (r.status === 'ok' ? 'ok' : `unavailable${r.statusNote ? ` — ${r.statusNote}` : ''}`) : 'no result (skipped or died)')
const reviewerStatus = {
  claude: statusOf(claudeR),
  codex: statusOf(codexR),
  coderabbit: statusOf(rabbitR),
  integration: statusOf(integR),
}
// Fixed vote slots — never filtered. CodeRabbit is best-effort: when it does not run,
// its slot simply records NONE (verdictFor returns NONE for a non-source with no
// evaluation), so there is no dynamic participant set, no quorum arithmetic, and no
// arbitration branch to keep alive.
const participants = ['claude', 'codex', 'coderabbit']
// CodeRabbit never takes part in the cross-review round: its CLI is a slow, blocking
// cloud call and re-spawning the lane risks a second invocation for no new
// signal, since the CLI reviews Git changes generically and cannot adjudicate our
// candidate ids. Its round-1 findings still stand as its votes.
const crossParticipants = ['claude', 'codex']

log(`Round 1 done: ${participants.map((k) => `${k}=${R[k] ? 'ok' : 'unavailable'}`).join(', ')}, integration=${integ ? 'ok' : 'unavailable'}`)

const openQuestions = []
const residualRisk = []
const positives = []
for (const [k, r] of Object.entries({ ...R, integration: integ })) {
  if (!r) continue
  for (const q of r.openQuestions || []) openQuestions.push(`[${k}] ${q}`)
  for (const q of r.residualRisk || []) residualRisk.push(`[${k}] ${q}`)
  for (const p of r.positives || []) positives.push(`[${k}] ${p}`)
}

// Report incomplete and stop. Claude, Codex and the integration lane are required:
// each contributes candidates the others miss, so a missing one silently narrows the
// review this skill advertises. CodeRabbit is the only best-effort lane. There is no
// degraded-consensus mode — a short-handed run is reported as incomplete, not as a
// weaker result the reader has to discount.
const incomplete = (reason) => ({
  status: 'incomplete',
  reason,
  pr, headSha, prType, reviewerStatus,
  rawFindings: [claudeR, codexR, rabbitR, integR].filter(Boolean).flatMap((r) => r.findings || []),
  openQuestions, residualRisk, positives,
})
const missing = ['claude', 'codex'].filter((k) => !R[k])
if (!integ) missing.push('integration')
if (missing.length) {
  log(`Stopping: required lane(s) unavailable — ${missing.join(', ')}`)
  return incomplete(`Required lane(s) unavailable: ${missing.join(', ')}. Findings returned RAW and UNVERIFIED — no consensus was computed.`)
}

// ---------- Merge & dedupe into candidates ----------
const candidates = []
const byKey = new Map()
// The dedup key includes a normalized summary so two distinct bugs at the same
// path:line stay separate candidates, and the consumer coordinates so one changed
// declaration breaking two callers stays two candidates — each gets its own
// adversarial verification instead of the second consumer being silently dropped.
const normSummary = (s) => (s || '').toLowerCase().replace(/\s+/g, ' ').trim()
const SEV_RANK = { critical: 3, major: 2, medium: 1, minor: 0 }

function addCandidate(f, source) {
  const key = `${f.path}:${f.line}:${normSummary(f.summary)}:${f.consumerPath || ''}:${f.consumerLine ?? ''}`
  const existing = byKey.get(key)
  if (existing) {
    if (!existing.sources.includes(source)) existing.sources.push(source)
    // Merge the duplicate's DATA, not only its source: first-reporter ordering must
    // not downgrade severity or discard stronger evidence/impact. Consumer coordinates
    // need no merge — they are part of the dedupe key, so a match means they are equal.
    if ((SEV_RANK[f.severity] ?? 0) > (SEV_RANK[existing.severity] ?? 0)) existing.severity = f.severity
    if (f.evidence && !existing.evidence.includes(f.evidence)) existing.evidence += ` | [${source}] ${f.evidence}`
    if (f.impact && !existing.impact.includes(f.impact)) existing.impact += ` | [${source}] ${f.impact}`
    return existing
  }
  const c = { ...f, id: `F${candidates.length + 1}`, sources: [source], flags: [] }
  byKey.set(key, c)
  candidates.push(c)
  return c
}

// "Files checked" as an actual control: a reviewer citing a file it never listed
// gets flagged for extra scrutiny downstream. Blank entries are dropped first —
// f.path.endsWith('') is always true, so filesChecked: [""] would suppress the flag.
function flagUnchecked(c, f, source, filesChecked) {
  const checked = (filesChecked || []).map((p) => (p || '').trim()).filter(Boolean)
  // Suffix matches must land on a path-segment boundary, otherwise
  // internal/bar/handler.go would satisfy a finding at pkg/foo/handler.go.
  const atBoundary = (long, short) => long === short || long.endsWith('/' + short)
  const listed = (path) => checked.some((p) => atBoundary(p, path) || atBoundary(path, p))
  if (!listed(f.path)) c.flags.push(`${source} reported this file without listing it in filesChecked — scrutinize`)
  if (f.consumerPath && !listed(f.consumerPath)) c.flags.push(`${source} cited consumer ${f.consumerPath} without listing it in filesChecked — scrutinize`)
}

// Single intake point for every candidate source. A finding whose evidence is blank
// or whitespace-only is REJECTED here rather than merged: schema `required` only
// guarantees the field exists, and an unevidenced duplicate would otherwise register
// its reviewer as a source, after which evidenceFor() hands it the co-reporter's
// citation and it silently tips the 2-of-3 tally. Rejecting at intake means an
// unevidenced report never becomes a vote anywhere.
function intake(f, source, filesChecked) {
  if (!(f.evidence || '').trim()) {
    log(`Dropped unevidenced finding from ${source} at ${f.path}:${f.line} — "${f.summary}" (evidence is required)`)
    return null
  }
  const c = addCandidate(f, source)
  flagUnchecked(c, f, source, filesChecked)
  return c
}

for (const k of participants) {
  // R[k] is null for a lane that did not run. Only CodeRabbit can be null here — the
  // required lanes already returned incomplete above — but participants is a fixed
  // slot list now, so the guard is what keeps the empty slot from being dereferenced.
  const r = R[k]
  if (!r) continue
  for (const f of r.findings || []) intake(f, k, r.filesChecked)
}
// Integration is a required lane, so malformed output from it stops the run rather
// than being dropped: silently discarding its only finding produced a result that
// looked clean and reported consensusReached: true while the required lane had in
// effect contributed nothing. The schema cannot make these conditionally required.
for (const f of integ.findings || []) {
  if (!f.consumerPath || f.consumerLine == null) {
    return incomplete(`Integration lane returned a finding at ${f.path}:${f.line} ("${f.summary}") without consumerPath/consumerLine. An integration finding is a claim that a specific consumer breaks and cannot be verified without both.`)
  }
}
for (const f of integ.findings || []) intake(f, 'integration', integ.filesChecked)
log(`Merged: ${candidates.length} unique candidate finding(s)`)

// ---------- Consensus (round 1 reports, then one cross-review round) ----------
const evals = { claude: {}, codex: {}, coderabbit: {} } // reviewer -> id -> latest evaluation
const resolution = {} // id -> 'confirmed' | 'dismissed' | 'open'
const dismissReason = {}

// Latest cross-review evaluation wins over original-reporter status, so a reporter
// who later submits DISAGREE is counted correctly rather than locked into AGREE.
const verdictFor = (k, c) => {
  if (evals[k][c.id]) return evals[k][c.id].verdict
  if (c.sources.includes(k)) return 'AGREE'
  return 'NONE'
}
// Trimmed so whitespace-only strings count as absent — a single space is not a citation.
const evidenceFor = (k, c) => {
  const raw = evals[k][c.id] ? (evals[k][c.id].evidence || '') : c.sources.includes(k) ? (c.evidence || '') : ''
  return raw.trim()
}

// Single consensus rule (mirrors SKILL.md), symmetric in both directions: two evidenced
// AGREEs confirm, two evidenced DISAGREEs dismiss, anything else is contested.
// Integration analysis is never a reviewer slot. Evidence is required for AGREE and
// DISAGREE, so a reviewer cannot move a finding without actually checking code.
function tally(c) {
  const agrees = participants.filter((k) => verdictFor(k, c) === 'AGREE' && evidenceFor(k, c))
  const disagrees = participants.filter((k) => verdictFor(k, c) === 'DISAGREE' && evidenceFor(k, c))
  if (agrees.length >= 2) return 'confirmed'
  if (disagrees.length >= 2) return 'dismissed'
  // No single-vote dismissal. A lone evidenced DISAGREE — reachable when CodeRabbit is
  // unavailable and the reporter softens to OPEN_QUESTION — must not bury a finding
  // that only one of three slots ever judged. It stays contested for the human.
  return 'contested'
}

// ONE cross-review round. A third "final positions" round re-asked the same
// reviewers the same question with the positions shown; it rarely moved a verdict
// and cost a full extra fan-out. Anything still split after this round is reported
// as contested for the human to settle.
let pending = candidates.map((c) => c.id)

if (pending.length) {
  const items = candidates.filter((c) => pending.includes(c.id))
  log(`Cross-review: ${items.length} candidate(s)`)
  const results = await parallel(
    crossParticipants.map((k) => () =>
      agent(crossPrompt(k, items), {
        label: `cross:${k}`,
        phase: 'Cross-review',
        schema: EVAL_SCHEMA,
        agentType: AGENT_TYPE[k],
      })),
  )
  // Strict completeness: each required lane must return EXACTLY one evaluation per
  // candidate it was shown — no missing ids, no duplicates, no ids we never presented.
  // Without this, dropping the old crossChecked gate would let a candidate nobody
  // actually evaluated ride its initial source votes straight to "confirmed".
  const presented = new Set(items.map((c) => c.id))
  for (let i = 0; i < crossParticipants.length; i++) {
    const k = crossParticipants[i]
    const res = ok(results[i])
    if (!res) return incomplete(`Required lane ${k} did not complete the cross-review round. No consensus was computed.`)
    const seen = new Set()
    for (const e of res.evaluations || []) {
      if (!presented.has(e.id)) return incomplete(`Cross-review lane ${k} returned an evaluation for unknown candidate ${e.id}.`)
      if (seen.has(e.id)) return incomplete(`Cross-review lane ${k} returned duplicate evaluations for candidate ${e.id}.`)
      seen.add(e.id)
    }
    if (seen.size !== presented.size) {
      const gaps = [...presented].filter((id) => !seen.has(id))
      return incomplete(`Cross-review lane ${k} evaluated ${seen.size} of ${presented.size} candidates (missing: ${gaps.join(', ')}).`)
    }
  }
  let bad = null
  crossParticipants.forEach((k, i) => {
    const res = ok(results[i])
    for (const e of res.evaluations || []) {
      // Dropping an unevidenced AGREE/DISAGREE is not enough: with no stored evaluation
      // the reviewer falls back to its round-1 source vote, so the discarded verdict
      // still decides the tally. Under the strict contract it is fatal.
      if ((e.verdict === 'AGREE' || e.verdict === 'DISAGREE') && !(e.evidence || '').trim()) {
        bad = bad || `Cross-review lane ${k} returned ${e.verdict} on ${e.id} with no evidence.`
        continue
      }
      evals[k][e.id] = e
    }
    // newFindings go through the same intake gate as round 1 — an unevidenced
    // late finding must not enter the tally either.
    for (const f of res.newFindings || []) {
      const c = intake(f, k, res.filesChecked)
      if (c && !pending.includes(c.id)) pending.push(c.id)
    }
  })
  if (bad) return incomplete(bad)
  const still = []
  for (const id of pending) {
    const c = candidates.find((x) => x.id === id)
    // Findings raised DURING cross-review were never presented to anyone, so nobody
    // evaluated them. Their reporters' source votes alone must not confirm them — two
    // lanes independently raising the same late finding would otherwise reach two
    // AGREEs with zero cross-checks, contradicting the one-evaluation-per-candidate
    // contract. They stay contested for the human; we do not add another round.
    if (!presented.has(id)) { still.push(id); continue }
    const t = tally(c)
    if (t === 'confirmed' || t === 'dismissed') {
      resolution[id] = t
      if (t === 'dismissed') {
        dismissReason[id] =
          participants
            .filter((k) => verdictFor(k, c) === 'DISAGREE')
            .map((k) => `${k}: ${(evals[k][id] && (evals[k][id].reason || evals[k][id].evidence)) || 'disagreed'}`)
            .join(' | ') || 'no reviewer support'
      }
    } else {
      still.push(id)
    }
  }
  pending = still
  const counts = Object.values(resolution)
  log(`Tally: confirmed=${counts.filter((v) => v === 'confirmed').length}, dismissed=${counts.filter((v) => v === 'dismissed').length}, contested=${pending.length}`)
}

// Still unresolved after the cross-review round: no valid vote at all -> open question;
// otherwise contested and reported for the human to settle.
const contestedIds = []
for (const id of pending) {
  const c = candidates.find((x) => x.id === id)
  const votes = participants.filter((k) => ['AGREE', 'DISAGREE'].includes(verdictFor(k, c)) && evidenceFor(k, c)).length
  if (votes === 0) {
    resolution[id] = 'open'
    const oq = participants
      .filter((k) => verdictFor(k, c) === 'OPEN_QUESTION')
      .map((k) => `${k}: ${(evals[k][c.id] && evals[k][c.id].reason) || 'open question'}`)
    openQuestions.push(
      oq.length
        ? `[unadjudicated] ${c.path}:${c.line} — ${c.summary} (explicit OPEN_QUESTION — ${oq.join(' | ')})`
        : `[unadjudicated] ${c.path}:${c.line} — ${c.summary} (no reviewer took a position)`,
    )
  } else {
    contestedIds.push(id)
  }
}

// ---------- Phase: Verify (adversarial refuter per confirmed finding, fresh context each) ----------
const confirmedIds = candidates.filter((c) => resolution[c.id] === 'confirmed').map((c) => c.id)
log(`Verification: adversarially re-checking ${confirmedIds.length} confirmed finding(s)`)

function refutePrompt(c) {
  return `Adversarial verification of a code-review finding that reached reviewer consensus. Your job is to try to REFUTE it — default to skepticism.
${header}
Finding ${c.id} [${c.severity}] ${c.path}:${c.line} — ${c.summary}
Evidence claimed: ${c.evidence}
Impact claimed: ${c.impact}
${c.consumerPath ? `Claimed broken consumer: ${c.consumerPath}:${c.consumerLine || '?'} — trace its actual dependency on the changed code` : ''}${c.flags.length ? `\nFlags: ${c.flags.join('; ')}` : ''}

Method:
- Read the FULL cited file(s) at the pinned commit — not just the diff, not the working copy:
${PINNED_READS}
  The diff shows what changed; the full file shows current state.
- For "missing X" claims, confirm X is actually absent from the existing file.
- For consumer-breakage claims, trace the actual dependency from consumer to changed code.
- For claims about upstream/external systems, fetch the cited source; if you cannot, return UNVERIFIABLE.

${NO_EXECUTION}

Return CONFIRMED only if you independently reproduced the evidence. REFUTED requires counter-evidence. Both REQUIRE a path:line citation in the evidence field — a verdict without one is discarded and the finding falls back to an open question.`
}

const verifierResults = await parallel(
  confirmedIds.map((id) => () => {
    const c = candidates.find((x) => x.id === id)
    return agent(refutePrompt(c), { label: `verify:${id}`, phase: 'Verify', schema: REFUTE_SCHEMA })
  }),
)
confirmedIds.forEach((id, i) => {
  const v = verifierResults[i]
  const c = candidates.find((x) => x.id === id)
  // No result, or a CONFIRMED/REFUTED with no citation, means the finding was never
  // actually checked. Fall back to open — "survived adversarial verification" must be
  // literally true, and an unchecked assertion must not dismiss a consensus either.
  const unchecked = !v || ((v.verdict === 'CONFIRMED' || v.verdict === 'REFUTED') && !(v.evidence || '').trim())
  if (unchecked) {
    resolution[id] = 'open'
    openQuestions.push(`[verification] ${c.path}:${c.line} — ${c.summary}: ${v ? `verifier returned ${v.verdict} without evidence` : 'verifier returned no result (agent timeout or failure)'} — treated as UNVERIFIABLE`)
    return
  }
  if (v.verdict === 'REFUTED') {
    resolution[id] = 'dismissed'
    dismissReason[id] = `failed adversarial verification: ${v.reason} (${v.evidence})`
  } else if (v.verdict === 'UNVERIFIABLE') {
    resolution[id] = 'open'
    openQuestions.push(`[verification] ${c.path}:${c.line} — ${c.summary}: ${v.reason}`)
  } else {
    c.verifiedEvidence = v.evidence
  }
})

// Confirmed integration findings identifying broken consumers escalate to at least medium.
for (const c of candidates) {
  if (resolution[c.id] === 'confirmed' && c.sources.includes('integration') && c.severity === 'minor') c.severity = 'medium'
}

// ---------- Result ----------
const emit = (c) => ({
  id: c.id,
  severity: c.severity,
  path: c.path,
  line: c.line,
  summary: c.summary,
  evidence: c.evidence,
  impact: c.impact,
  consumerPath: c.consumerPath || null,
  consumerLine: c.consumerLine || null,
  sources: c.sources,
  flags: c.flags,
  // Every surviving vote is evidenced: intake drops unevidenced findings and an
  // unevidenced cross-review verdict returns incomplete, so no "uncounted" state
  // can reach this point.
  votes: Object.fromEntries(participants.map((k) => [k, verdictFor(k, c)])),
  verifiedEvidence: c.verifiedEvidence || null,
})

const confirmed = candidates.filter((c) => resolution[c.id] === 'confirmed').map(emit)
const contested = candidates
  .filter((c) => contestedIds.includes(c.id))
  .map((c) => ({
    ...emit(c),
    positions: Object.fromEntries(
      participants.map((k) => [
        k,
        {
          verdict: verdictFor(k, c),
          reason: (evals[k][c.id] && evals[k][c.id].reason) || (c.sources.includes(k) ? 'original reporter' : 'no position'),
          evidence: evidenceFor(k, c) || null,
        },
      ]),
    ),
  }))
const dismissed = candidates
  .filter((c) => resolution[c.id] === 'dismissed')
  .map((c) => ({ ...emit(c), why: dismissReason[c.id] || 'no consensus' }))
// Findings resolved to 'open' — reached consensus but did not survive verification
// (UNVERIFIABLE, verifier died, or verifier gave a verdict with no citation), or drew
// no valid vote at all. They MUST be emitted: previously they appeared in none of the
// three arrays, so a candidate could vanish into a text open question while
// consensusReached still reported true.
const unresolved = candidates
  .filter((c) => resolution[c.id] === 'open')
  .map((c) => ({ ...emit(c), why: 'reached consensus but did not survive verification, or drew no valid vote — see openQuestions' }))

log(`Done: ${confirmed.length} confirmed, ${contested.length} contested, ${unresolved.length} unresolved, ${dismissed.length} dismissed, ${openQuestions.length} open questions`)

return {
  status: 'ok',
  pr,
  headSha,
  prType,
  // Consensus means every candidate reached a final disposition. An unresolved
  // finding is an unanswered question, so it blocks the claim just as a contested
  // one does.
  consensusReached: contested.length === 0 && unresolved.length === 0,
  reviewerStatus,
  confirmed,
  contested,
  unresolved,
  dismissed,
  openQuestions,
  residualRisk,
  positives,
}
