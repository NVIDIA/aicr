# PR #697 Review-Feedback Revision Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Address 10 of 11 actionable findings from CodeRabbit + self-review on PR #697 (AICR-bot Slack cadence). 9 commits on the existing PR branch, no functional change to the cadence design — only correctness, robustness, and doc-truthfulness fixes.

**Architecture:** All changes are CI-only (workflow YAML, composite-action shell, design markdown). No application code, no schema. Each commit addresses one finding (with the two doc-only findings batched per the brainstorm decision).

**Tech Stack:** GitHub Actions, bash, jq, `actions/github-script` (Node.js inside YAML), markdown. Lint with `yamllint` and `markdownlint`. Bash hardening verified by extracting the `run:` block and shellchecking it.

**Working location:** `.worktrees/aicr-bot-slack-cadence` on branch `feat/aicr-bot-slack-cadence` (PR #697 head). All `git`/edit commands below assume `cd .worktrees/aicr-bot-slack-cadence` from the repo root, OR direct invocation with absolute paths.

**Out of scope:**
- Em-dash → ASCII in marker (deferred; not currently broken).
- "Three-layer composite" rationale comments (CodeRabbit-claimed convention not present in repo CLAUDE.md or `.coderabbit.yaml`; investigate separately).

**Verification posture:** No staging Slack channel. Production verification runs at the next 05:00 UTC heartbeat (Mon-Fri) and next vuln-scan / issue-report / tag-push events. Pre-push verification limited to lint + shellcheck + visual diff inspection.

**Execution method:** solo (single-author CI revision; no parallelizable work).

---

### Task 1: Bound Slack history lookup to today's window (finding #1, part 1/2)

**Why:** `limit=20` on `conversations.history` can miss today's parent on busy channels. Bounding by `oldest=$(today 00:00 UTC)` makes the result deterministic regardless of unrelated chatter and lets us safely raise the limit to the cheap default (100) without scanning multi-day history.

**Files:**
- Modify: `.github/actions/slack-thread-post/action.yml` — the MARKER computation block (~lines 88-89) and the `conversations.history` call (~lines 118-122).

- [ ] **Step 1: Add `TODAY_START_TS` computation alongside the marker**

Replace the marker block (the comment line plus the single `MARKER=...` line):

```bash
        # ---- Daily marker (UTC) — internal, not caller-controlled ----------
        MARKER="Daily AICR-bot status — $(date -u +%Y-%m-%d)"
```

with:

```bash
        # ---- Daily marker (UTC) — internal, not caller-controlled ----------
        MARKER="Daily AICR-bot status — $(date -u +%Y-%m-%d)"

        # Unix timestamp at 00:00:00 UTC today. Used as 'oldest' on
        # conversations.history so the lookup window is exactly today's UTC
        # day — independent of channel chatter volume.
        TODAY_START_TS=$(date -u -d 'today 00:00:00' +%s 2>/dev/null \
          || date -u -j -f '%Y-%m-%d %H:%M:%S' \
              "$(date -u +%Y-%m-%d) 00:00:00" +%s)
```

- [ ] **Step 2: Update the history call to use limit=100 + oldest**

Replace:

```bash
        if history=$(slack_api conversations.history --get \
              --data-urlencode "channel=${CHANNEL_ID}" \
              --data-urlencode "limit=20"); then
```

with:

```bash
        if history=$(slack_api conversations.history --get \
              --data-urlencode "channel=${CHANNEL_ID}" \
              --data-urlencode "limit=100" \
              --data-urlencode "oldest=${TODAY_START_TS}"); then
```

- [ ] **Step 3: Verify yamllint passes**

```bash
yamllint .github/actions/slack-thread-post/action.yml
```

Expected: no output (yamllint is silent on success).

- [ ] **Step 4: Verify shell block parses cleanly**

```bash
yq '.runs.steps[0].run' .github/actions/slack-thread-post/action.yml > /tmp/action.sh
shellcheck -s bash /tmp/action.sh
```

Expected: no errors. SC2154-style "referenced but not assigned" warnings on `${BOT_TOKEN}`, `${CHANNEL_ID}`, `${BODY}`, `${MODE}` are acceptable — those are GHA-injected at runtime via the step `env:`.

- [ ] **Step 5: Commit**

```bash
git add .github/actions/slack-thread-post/action.yml
git commit -S -m "$(cat <<'EOF'
fix(ci): bound Slack history lookup to today's UTC window

Replaces limit=20 with limit=100 + oldest=$(today 00:00 UTC). With the
lookup window pinned to today's UTC day, channel chatter volume cannot
push the daily parent out of the result set. The previous limit=20 risked
missing the parent on busy channels and silently lazy-creating a duplicate.

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md
EOF
)"
```

---

### Task 2: Classify Slack API errors as permanent vs transient (finding #1, part 2/2)

**Why:** The current `slack_api` helper returns 1 for every error and the caller silently lazy-creates a parent on any history failure. That masks misconfiguration (missing scope, revoked token, bot not in channel) — the workflow runs daily without anyone noticing. Permanent errors should fail loudly so the operator fixes the setup.

**Files:**
- Modify: `.github/actions/slack-thread-post/action.yml` — the `slack_api` function's error branch (~lines 99-108).

- [ ] **Step 1: Replace the slack_api error branch with classification**

Replace:

```bash
          if [[ "${ok}" != "true" ]]; then
            local err
            err=$(printf '%s' "${response}" | jq -r '.error // "unknown_error"')
            echo "::error::Slack ${method} failed: ${err}" >&2
            printf '%s' "${response}" | jq . >&2 || true
            return 1
          fi
```

with:

```bash
          if [[ "${ok}" != "true" ]]; then
            local err
            err=$(printf '%s' "${response}" | jq -r '.error // "unknown_error"')
            printf '%s' "${response}" | jq . >&2 || true
            # Permanent misconfiguration — operator must fix; do not lazy-create.
            case "${err}" in
              invalid_auth|not_authed|token_revoked|token_expired|\
              missing_scope|invalid_scope|not_allowed_token_type|\
              channel_not_found|not_in_channel)
                echo "::error::Slack ${method} failed: ${err} — fix Slack app config or repo secrets/vars" >&2
                exit 1
                ;;
            esac
            # Transient (rate_limited, request_timeout, internal_error,
            # fatal_error, service_unavailable, 5xx) — caller decides whether
            # to lazy-create.
            echo "::warning::Slack ${method} transient failure: ${err}" >&2
            return 1
          fi
```

Note: `exit 1` (not `return 1`) inside the function is intentional — it terminates the whole step, since recovery is impossible. The trailing `return 1` is reached only for transient errors and lets the caller fall through to lazy-create.

- [ ] **Step 2: Verify yamllint passes**

```bash
yamllint .github/actions/slack-thread-post/action.yml
```

Expected: no output.

- [ ] **Step 3: Verify shellcheck passes**

```bash
yq '.runs.steps[0].run' .github/actions/slack-thread-post/action.yml > /tmp/action.sh
shellcheck -s bash /tmp/action.sh
```

Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add .github/actions/slack-thread-post/action.yml
git commit -S -m "$(cat <<'EOF'
fix(ci): classify Slack API errors as permanent vs transient

Permanent errors (invalid_auth, missing_scope, token_revoked,
channel_not_found, not_in_channel, etc.) now exit the step with an
explicit operator-action message. Transient errors (rate_limited,
timeout, server errors) still return 1, letting the caller fall through
to lazy-create as before.

Without this, a misconfigured bot token or missing channel scope would
silently lazy-create a duplicate parent every day with no signal to the
operator.

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md
EOF
)"
```

---

### Task 3: Use full release window in weekly summary (finding #2)

**Why:** `gh release list --limit 20` truncates *before* the `publishedAt >= SINCE` filter, so a high-release week can undercount. AICR's release cadence is at most a few per week; `--limit 200` covers ~6 months and a single page suffices. Full pagination is YAGNI for this volume.

**Files:**
- Modify: `.github/workflows/weekly-summary.yaml` — the `gh release list` call in the `Collect releases (last 7 days)` step.

- [ ] **Step 1: Replace the limit**

Replace:

```yaml
          RELEASES_JSON=$(gh release list --repo "${REPO}" --limit 20 \
            --json tagName,publishedAt)
```

with:

```yaml
          # --limit 200 covers ~6 months at AICR's release cadence; the
          # publishedAt >= SINCE filter below trims to the 7-day window.
          # Pagination is YAGNI for this volume.
          RELEASES_JSON=$(gh release list --repo "${REPO}" --limit 200 \
            --json tagName,publishedAt)
```

- [ ] **Step 2: Verify yamllint passes**

```bash
yamllint .github/workflows/weekly-summary.yaml
```

Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/weekly-summary.yaml
git commit -S -m "$(cat <<'EOF'
fix(ci): use full release window in weekly summary

Bumps gh release list --limit from 20 to 200. The previous limit
truncated the result set before the publishedAt >= SINCE filter ran, so
a busy release week could omit valid releases from the digest.

200 covers ~6 months at AICR's release cadence; pagination is
unnecessary for this volume.

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md
EOF
)"
```

---

### Task 4: Bound weekly vuln-scan link to 7-day window (finding #3)

**Why:** The current query picks the most recent successful `vuln-scan-images.yaml` run *across all time*. If every scan in the current week failed, Saturday's digest still links to a green run from a prior week — silently hiding the regression.

**Files:**
- Modify: `.github/workflows/weekly-summary.yaml` — the entire `Collect latest vuln scan` step.

- [ ] **Step 1: Replace the vuln-scan collection block**

Replace:

```yaml
      - name: Collect latest vuln scan
        env:
          GH_TOKEN: ${{ github.token }}
          REPO: ${{ github.repository }}
        run: |
          set -euo pipefail
          RUN_JSON=$(gh run list --repo "${REPO}" \
            --workflow=vuln-scan-images.yaml --status=success --limit=1 \
            --json databaseId,createdAt,url)

          COUNT=$(printf '%s' "${RUN_JSON}" | jq 'length')
          if [[ "${COUNT}" -eq 0 ]]; then
            BLOCK="*Latest vuln scan:* _no successful run found — see Actions tab_"
          else
            URL=$(printf '%s' "${RUN_JSON}" | jq -r '.[0].url')
            CREATED=$(printf '%s' "${RUN_JSON}" | jq -r '.[0].createdAt')
            DAY=$(date -u -d "${CREATED}" '+%a %H:%M UTC' 2>/dev/null \
              || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "${CREATED}" '+%a %H:%M UTC')
            BLOCK="*Latest vuln scan:* ${DAY} — <${URL}|view run>"
          fi
```

with:

```yaml
      - name: Collect latest vuln scan
        env:
          GH_TOKEN: ${{ github.token }}
          REPO: ${{ github.repository }}
        run: |
          set -euo pipefail
          # Pull the last 50 successful runs so the 7-day filter has plenty
          # to choose from even if the workflow runs >1x/day.
          ALL_RUNS=$(gh run list --repo "${REPO}" \
            --workflow=vuln-scan-images.yaml --status=success --limit=50 \
            --json databaseId,createdAt,url)

          # 7-day-ago threshold (UTC), with macOS BSD-date fallback.
          THRESHOLD=$(date -u -d '7 days ago' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
            || date -u -v-7d +%Y-%m-%dT%H:%M:%SZ)

          RUN_JSON=$(printf '%s' "${ALL_RUNS}" \
            | jq --arg t "${THRESHOLD}" \
                '[.[] | select(.createdAt >= $t)] | sort_by(.createdAt) | reverse | .[0:1]')

          COUNT=$(printf '%s' "${RUN_JSON}" | jq 'length')
          if [[ "${COUNT}" -eq 0 ]]; then
            BLOCK="*Latest vuln scan:* _no successful run this week — see Actions tab_"
          else
            URL=$(printf '%s' "${RUN_JSON}" | jq -r '.[0].url')
            CREATED=$(printf '%s' "${RUN_JSON}" | jq -r '.[0].createdAt')
            DAY=$(date -u -d "${CREATED}" '+%a %H:%M UTC' 2>/dev/null \
              || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "${CREATED}" '+%a %H:%M UTC')
            BLOCK="*Latest vuln scan:* ${DAY} — <${URL}|view run>"
          fi
```

- [ ] **Step 2: Verify yamllint passes**

```bash
yamllint .github/workflows/weekly-summary.yaml
```

Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/weekly-summary.yaml
git commit -S -m "$(cat <<'EOF'
fix(ci): bound weekly vuln-scan link to 7-day window

The previous query picked the most recent successful run across all
time. If every scan in the current week failed, Saturday's digest would
link to a green run from a prior week and hide the regression.

Now we pull the last 50 successful runs and filter to createdAt >=
now - 7 days in jq, falling back to "no successful run this week" when
empty.

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md
EOF
)"
```

---

### Task 5: Compute on-tag Slack mode from commit timestamp (finding #4)

**Why:** Currently `notify` derives day-of-week from `date -u +%u` at job-runtime. A tag pushed at 23:55 UTC Friday whose `notify` job runs at 00:05 UTC Saturday becomes a top-level weekend post instead of replying to Friday's parent. The `notify` job already checks out the repo, so we can use the commit's committer timestamp via `git log` — independent of any GHA event-payload quirks.

**Files:**
- Modify: `.github/workflows/on-tag.yaml` — the `Build message and choose mode` step (the DOW computation block, ~lines 533-539).

- [ ] **Step 1: Replace the DOW computation**

Replace:

```bash
          # date -u +%u: 1=Mon … 7=Sun
          DOW=$(date -u +%u)
          if [[ "${DOW}" -le 5 ]]; then
            MODE="reply"
          else
            MODE="top-level"
          fi
```

with:

```bash
          # Derive DOW from the commit's committer timestamp, not the
          # notify-job runner clock. A tag pushed late on Friday UTC whose
          # notify job runs after midnight would otherwise post top-level
          # on Saturday instead of replying under Friday's daily parent.
          EVENT_TS=$(git log -1 --format=%cI HEAD)
          DOW=$(date -u -d "${EVENT_TS}" +%u 2>/dev/null \
            || date -u -j -f '%Y-%m-%dT%H:%M:%S%z' "${EVENT_TS}" +%u)
          if [[ "${DOW}" -le 5 ]]; then
            MODE="reply"
          else
            MODE="top-level"
          fi
```

- [ ] **Step 2: Verify yamllint passes**

```bash
yamllint .github/workflows/on-tag.yaml
```

Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/on-tag.yaml
git commit -S -m "$(cat <<'EOF'
fix(ci): compute on-tag Slack mode from commit timestamp

Derives day-of-week from the commit's committer timestamp (git log -1)
instead of the notify-job runner clock. A tag pushed at 23:55 UTC Friday
whose notify job runs at 00:05 UTC Saturday no longer posts top-level on
Saturday by accident — it correctly replies under Friday's parent.

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md
EOF
)"
```

---

### Task 6: Include new-and-closed issues in weekly +new count (finding #5)

**Why:** The current `+new` count filters `open` for `created_at >= since`. Issues created and already closed within the same 7-day window aren't counted, even though "new this week" plain-language includes them. The `closed` count is already paginated; reuse that to derive a true "+new" total.

**Files:**
- Modify: `.github/workflows/weekly-summary.yaml` — inside the `actions/github-script` `script:` block, the `closed`/`total`/`newCount` chunk.

- [ ] **Step 1: Replace the closed/newCount block**

Replace:

```javascript
            const closed = (await github.paginate(
              github.rest.issues.listForRepo,
              { ...context.repo, state: 'closed', since, per_page: 100 },
            )).filter(i => !i.pull_request &&
                          new Date(i.closed_at) >= new Date(since)).length;

            const total = open.length;
            const newCount = open.filter(
              i => new Date(i.created_at) >= new Date(since)).length;
```

with:

```javascript
            // Pull recently-closed issues (not just count) so we can union
            // their "newly-created in this window" subset with the open
            // newly-created subset. Without this, an issue opened and
            // closed in the same 7 days never counts toward "+new".
            const recentClosed = (await github.paginate(
              github.rest.issues.listForRepo,
              { ...context.repo, state: 'closed', since, per_page: 100 },
            )).filter(i => !i.pull_request &&
                          new Date(i.closed_at) >= new Date(since));
            const closed = recentClosed.length;

            const total = open.length;
            const sinceMs = new Date(since).getTime();
            const newOpen = open.filter(
              i => new Date(i.created_at).getTime() >= sinceMs).length;
            const newClosed = recentClosed.filter(
              i => new Date(i.created_at).getTime() >= sinceMs).length;
            const newCount = newOpen + newClosed;
```

- [ ] **Step 2: Verify yamllint passes**

```bash
yamllint .github/workflows/weekly-summary.yaml
```

Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/weekly-summary.yaml
git commit -S -m "$(cat <<'EOF'
fix(ci): include new-and-closed issues in weekly +new count

The previous +new derivation filtered the open-issue list only, so any
issue opened and closed within the same 7-day window was excluded. Plain
reading of "X open, +Y new, -Z closed" implies +Y covers all new issues,
not just those still open.

We now union the newly-created subset of open and recently-closed
issues. recentClosed is already paginated for the -closed metric; the
incremental cost is one extra filter pass.

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md
EOF
)"
```

---

### Task 7: Deduplicate weekly issue priority counts (finding #6)

**Why:** The current loop accumulates `prioritized += c` across P0/P1/P2 buckets. An issue carrying multiple priority labels (rare but legal in GitHub) is counted in each bucket *and* in `prioritized`, making `noPri = total - prioritized` wrong — possibly negative.

**Files:**
- Modify: `.github/workflows/weekly-summary.yaml` — inside the `actions/github-script` `script:` block, the `pri`/`prioritized`/`noPri` block.

- [ ] **Step 1: Replace the priority accumulation**

Replace:

```javascript
            let prioritized = 0;
            const pri = PRIORITY_LABELS.map(l => {
              const c = open.filter(i => hasLabel(i, l)).length;
              prioritized += c;
              return [l, c];
            });
            const noPri = total - prioritized;
```

with:

```javascript
            // Per-priority counts (an issue may appear in multiple buckets
            // if it carries multiple priority labels — that's fine here).
            const pri = PRIORITY_LABELS.map(l => [
              l, open.filter(i => hasLabel(i, l)).length,
            ]);
            // For the noPri remainder, dedupe by issue number — an issue
            // with both P0 and P1 counts once toward "prioritized".
            const prioritizedNums = new Set(
              open.filter(i => PRIORITY_LABELS.some(l => hasLabel(i, l)))
                  .map(i => i.number),
            );
            const noPri = total - prioritizedNums.size;
```

- [ ] **Step 2: Verify yamllint passes**

```bash
yamllint .github/workflows/weekly-summary.yaml
```

Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/weekly-summary.yaml
git commit -S -m "$(cat <<'EOF'
fix(ci): deduplicate weekly issue priority counts

Per-priority bucket counts unchanged. For the noPri remainder, we now
dedupe by issue number using a Set, so an issue carrying multiple
priority labels (e.g., both P0 and P1) is counted once — not summed
across buckets, which previously made noPri = total - prioritized
under-report or go negative.

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md
EOF
)"
```

---

### Task 8: Link SLA-breach issues in weekly summary (finding #10)

**Why:** The current breach line is just a count (`:rotating_light: 1 SLA breach`). The weekly digest is the highest-attention surface for these issues; readers should be able to click directly. Cap at 5 to avoid wall-of-text.

**Files:**
- Modify: `.github/workflows/weekly-summary.yaml` — inside the `actions/github-script` `script:` block, the `if (breaches.length > 0)` chunk.

- [ ] **Step 1: Replace the breach line construction**

Replace:

```javascript
            if (breaches.length > 0) {
              lines.push(`:rotating_light: ${breaches.length} SLA breach${breaches.length === 1 ? '' : 'es'}`);
            }
```

with:

```javascript
            if (breaches.length > 0) {
              const repoUrl = `https://github.com/${context.repo.owner}/${context.repo.repo}`;
              const SHOW = 5;
              const shown = breaches
                .slice(0, SHOW)
                .map(b => `<${repoUrl}/issues/${b.num}|#${b.num}> (${b.pri}, +${b.over}d over)`);
              const overflow = breaches.length > SHOW
                ? ` _+${breaches.length - SHOW} more_`
                : '';
              const word = breaches.length === 1 ? 'breach' : 'breaches';
              lines.push(`:rotating_light: ${breaches.length} SLA ${word}: ${shown.join(', ')}${overflow}`);
            }
```

- [ ] **Step 2: Verify yamllint passes**

```bash
yamllint .github/workflows/weekly-summary.yaml
```

Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/weekly-summary.yaml
git commit -S -m "$(cat <<'EOF'
fix(ci): link SLA-breach issues in weekly summary

The breach line previously showed only a count. Each breach now renders
as a Slack-formatted issue link with priority and over-SLA days. Capped
at 5 to avoid wall-of-text — anything beyond appears as "+N more".

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md
EOF
)"
```

---

### Task 9: Align Slack-cadence design doc with implementation (findings #7 + #8)

**Why:** Two doc-only fixes, batched per the brainstorm decision. (a) Three fenced code blocks have no language identifier — markdownlint MD040. (b) The failure-mode table claims `chat.postMessage` failures log the message body to `$GITHUB_STEP_SUMMARY`; the action does no such write. Removing the claim is cheaper than adding the feature; YAGNI.

**Files:**
- Modify: `docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md` — opening fences at lines 47, 56, 77, and the failure-mode-table row near line 199.

- [ ] **Step 1: Add `text` language identifier to fenced block 1 (Mon-Fri timeline)**

Replace:

```text
### Mon-Fri (daily threaded)

```
```

with:

```text
### Mon-Fri (daily threaded)

```text
```

(I.e., the bare opening fence becomes `` ```text ``. The closing fence on line 52 is unchanged.) Use Edit anchored on the heading line so the match is unique to this fence.

- [ ] **Step 2: Add `text` language identifier to fenced block 2 (Sat-Sun timeline)**

Replace:

```text
### Sat-Sun (weekly only, no daily noise)

```
```

with:

```text
### Sat-Sun (weekly only, no daily noise)

```text
```

- [ ] **Step 3: Add `text` language identifier to fenced block 3 (marker pattern)**

Replace:

```text
The first line of every weekday parent message is exactly:

```
```

with:

```text
The first line of every weekday parent message is exactly:

```text
```

- [ ] **Step 4: Remove the GITHUB_STEP_SUMMARY claim**

In the failure-mode table, replace the `chat.postMessage` row:

```text
| `chat.postMessage` fails on the post/reply | Log message body to `$GITHUB_STEP_SUMMARY`, exit non-zero. Matches today's behaviour on webhook failure. |
```

with:

```text
| `chat.postMessage` fails on the post/reply | `slack_api` emits the failed response as a workflow `::error::` annotation and exits non-zero. The job fails so the operator sees the run in red. |
```

- [ ] **Step 5: Verify markdownlint passes**

```bash
markdownlint docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md
```

Expected: no MD040 warnings on the three updated fences. Other warnings (if any) are pre-existing and out of scope.

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md
git commit -S -m "$(cat <<'EOF'
docs(superpowers): align Slack-cadence design doc with implementation

- Adds 'text' language identifier to three unlabeled fenced blocks
  (markdownlint MD040).
- Removes the failure-mode-table claim that chat.postMessage failures
  write the message body to GITHUB_STEP_SUMMARY. The action emits an
  ::error:: annotation and exits non-zero; it does not write to the
  step summary file. Doc now matches implementation.

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md
EOF
)"
```

---

### Task 10: Push and observe production verification

**Why:** No staging Slack channel. Push the 9 commits and watch the next workflow firings.

- [ ] **Step 1: Push to PR branch**

```bash
git push origin feat/aicr-bot-slack-cadence
```

- [ ] **Step 2: Confirm PR head matches**

```bash
git log --oneline -12
```

Expected: 9 new fix/docs commits on top of the original 8.

- [ ] **Step 3: Note the verification windows**

Production smoke depends on these triggers (no action required, just expectations):

| Workflow | Next firing | What to verify |
|---|---|---|
| `daily-status.yaml` | Next 05:00 UTC Mon-Fri | Heartbeat parent posted; no duplicate from a slow `conversations.history` |
| `vuln-scan-images.yaml` | Next 06:00 UTC Mon-Fri | Reply lands under today's parent (proves Task 1 oldest filter works) |
| `issue-report.yaml` | Next 12:30 UTC Mon-Fri | Reply lands under today's parent |
| `on-tag.yaml` | Next tag push | Mode chosen from commit timestamp (proves Task 5) |
| `weekly-summary.yaml` | Next Saturday 14:00 UTC | Releases counted from full window, vuln-scan from 7-day window, +new includes new-and-closed, priority deduped, breaches linked |

- [ ] **Step 4: Re-request CodeRabbit review**

Post a comment on PR #697:

```text
@coderabbitai review
```

Use the GitHub web UI or, if `gh` TLS is working again, `gh pr comment 697 --body "@coderabbitai review"`.

---

## Self-Review

**Spec coverage:** Each of the 10 in-scope findings (#1, #2, #3, #4, #5, #6, #7, #8, #10) maps to a task. Tasks 1+2 cover #1; Task 9 covers #7+#8. Em-dash (#9) and three-layer-composite (#11) are explicitly out of scope. ✓

**Placeholder scan:** No TBDs, no "implement later," every task has exact replacement code. ✓

**Type consistency:** `RUN_JSON`, `ALL_RUNS`, `THRESHOLD`, `recentClosed`, `prioritizedNums`, `EVENT_TS`, `TODAY_START_TS` are introduced in their first use and referenced consistently. ✓

**Cross-file invariants:** Tasks 6, 7, 8 all touch the same `actions/github-script` block in `weekly-summary.yaml`. The Edit anchors are non-overlapping — Task 6 modifies the `closed`/`newCount` chunk, Task 7 modifies the `pri`/`prioritized` chunk, Task 8 modifies the `breaches` chunk. ✓

**Commit-order independence:** Each commit lints clean on its own; if any single commit is reverted, the remaining commits still leave the file in a working state. ✓
