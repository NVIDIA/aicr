# AICR-Bot Slack Cadence: Daily Thread + Weekly Roll-up

**Date:** 2026-04-27
**Author:** Carlos Arango Gutierrez (eduardoa@nvidia.com)
**Origin:** Slack thread with @Mark Chmarny / @Yuan Chen on 2026-04-27, followed by a meeting with Mark refining the cadence.
**Scope:** CI workflow changes only. No application code, no schema changes.

## Objective

Replace the AICR-Bot's three independent top-level Slack messages per day with a two-cadence model:

- **Weekday daily (Mon-Fri):** a short heartbeat parent + threaded replies for vuln scan, issue report, and release announcements. Low-noise, skim-and-skip.
- **Weekend weekly (Saturday):** a single full-content top-level post — releases of the week, 7-day issue movement, latest vuln scan snapshot. The "real" digest people read on Monday morning.

Net effect: 1 short top-level post per workday, 1 substantive top-level post per week, zero bot noise on Sundays.

## Current State

Three workflows post **independent top-level messages** to the AICR Slack channel through the same Incoming Webhook (`secrets.SLACK_SERVICE`):

| Workflow | Trigger | Step | Today's payload |
|---|---|---|---|
| `.github/workflows/issue-report.yaml` | cron `30 12 * * *` (12:30 UTC daily) | `Post to Slack` (lines 266-278) | `AICR Issues — {date}` digest with SLA flags |
| `.github/workflows/vuln-scan-images.yaml` | cron `0 6 * * *` (06:00 UTC daily) | `notify` job (lines 276-306) | `Vulnerability Scan: {ts} ({sha})` per-image counts + run link |
| `.github/workflows/on-tag.yaml` | release tag push | `notify` job (lines 415-435) | `AICR {tag} has been released: {url}` |

All three call `https://hooks.slack.com/services/${SLACK_SERVICE}` with a `text:` payload via `curl`. **Slack Incoming Webhooks cannot post threaded replies** — they always create top-level messages and ignore `thread_ts`. So consolidation is impossible without migrating to the Slack Web API (`chat.postMessage`).

Recent release cadence saw up to ~5 patch releases per workday during release weeks, so on the busiest days the channel can see 7+ top-level bot messages. This is the "chat creep" the proposal targets.

## Decisions (locked in: brainstorm 2026-04-27 + meeting with Mark)

- **Two cadences, not one.** Weekday daily heartbeat-and-thread, weekend weekly full top-level digest.
- **Weekday daily fires Mon-Fri only.** Crons of `vuln-scan-images.yaml` and `issue-report.yaml` shift from `* * *` to `* * 1-5`. They simply don't run Sat/Sun. (Both workflows' only side effect is the Slack post — Step Summaries are nice-to-have, not load-bearing.)
- **Weekday daily heartbeat:** new workflow at **05:00 UTC Mon-Fri**. Posts a short parent message: `Daily AICR-bot status — {YYYY-MM-DD UTC}`. No metrics, no children list — a thread anchor.
- **Weekly summary:** new workflow at **Saturday 14:00 UTC** (~06:00-07:00 Pacific, depending on DST). Posts a single top-level message with three sections (releases of the week, issue movement, latest vuln scan summary). Read time: under 30 seconds.
- **State mechanism for daily threading:** Slack history lookup at runtime. Each workflow calls `conversations.history` and matches the marker pattern against recent posts. No GitHub Actions variables, no sticky issue.
- **Pre-parent fallback (weekday):** lazy-create. If a workflow runs before the 05:00 UTC heartbeat (e.g., an `on-tag` release at 04:00 UTC), it creates the parent itself.
- **Weekend release behaviour:** there is no daily heartbeat on Sat/Sun. Weekend `on-tag` releases post top-level. Weekend releases are rare and inherently noteworthy; one top-level post is not chat creep.
- **Race tolerance:** if two workflows race to create the parent within seconds (rare), both parents survive; later workflows reply to whichever is most recent. Self-heals next day.
- **Backwards compatibility during rollout:** `SLACK_SERVICE` (webhook) secret stays until all four workflows are migrated; deleted from repo settings after.

## Architecture & Data Flow

### Mon-Fri (daily threaded)

```
05:00 UTC   daily-status.yaml      (NEW)        → "Daily AICR-bot status — 2026-04-27"   [parent]
06:00 UTC   vuln-scan-images.yaml  (modified)   → Vulnerability Scan: …                  [thread reply]
12:30 UTC   issue-report.yaml      (modified)   → AICR Issues — Mon, Apr 27 …            [thread reply]
on tag      on-tag.yaml notify     (modified)   → AICR v0.12.0 has been released: …      [thread reply]
```

### Sat-Sun (weekly only, no daily noise)

```
Sat 14:00 UTC   weekly-summary.yaml (NEW)       → "AICR weekly — week of 2026-04-21"     [top-level]
                                                    Releases (5):
                                                      • v0.12.0 (Mon) — link
                                                      • v0.12.1 (Wed) — link
                                                      • …
                                                    Issue movement (7d):
                                                      35 open, +6 new, -4 closed
                                                      P0: 0, P1: 14 (3 unowned), P2: 21
                                                      :rotating_light: 1 SLA breach: #214 (P1, 192d)
                                                    Latest vuln scan: Fri 06:00 UTC
                                                      0 critical, 0 high — all images clean
                                                      <run link>

(any tag during weekend) on-tag.yaml notify     → AICR v0.12.X released: …               [top-level]
```

### Marker pattern (daily lookup key)

The first line of every weekday parent message is exactly:

```
Daily AICR-bot status — {YYYY-MM-DD}
```

where `{YYYY-MM-DD}` is the UTC date (`date -u +%Y-%m-%d`). Used by every replying workflow to find today's parent.

### Find-or-create algorithm (weekday workflows that thread)

1. Compute `marker = "Daily AICR-bot status — $(date -u +%Y-%m-%d)"`.
2. Call `conversations.history` with the channel ID and `limit=20`.
3. Find the most recent message whose `text` starts with `marker`.
4. If found → use its `ts` as `thread_ts`.
5. If not found → call `chat.postMessage` (no `thread_ts`) with `text=marker` to create the parent. Use the returned `ts` as `thread_ts`.
6. Call `chat.postMessage` with `thread_ts` and the workflow's body.

Worst case: 3 API calls (history + create + reply). Best case: 2 (history + reply, when the heartbeat already posted).

## Implementation

### Operational prerequisite (one-time, repo admin, before merge)

1. Create or reuse a Slack App in the NVIDIA workspace.
2. Grant OAuth scopes:
   - `chat:write` — to post and reply.
   - `channels:history` (public channel) **or** `groups:history` (private channel) — to look up today's parent.
3. Install the app to the workspace; invite the bot to the target channel.
4. Add two new repo settings:
   - **Secret** `SLACK_BOT_TOKEN` — the `xoxb-…` bot token.
   - **Variable** `SLACK_CHANNEL_ID` — the channel ID, e.g., `C012ABC3DEF` (not secret).
5. Keep the existing `SLACK_SERVICE` secret until all four workflows are migrated.

### NEW: shared composite action `.github/actions/slack-thread-post/action.yml`

Encapsulates all Slack Web API logic so caller workflows stay short.

**Inputs:**

| input | required | purpose |
|---|---|---|
| `bot-token` | yes | `SLACK_BOT_TOKEN` |
| `channel-id` | yes | `SLACK_CHANNEL_ID` |
| `body` | required when `mode=reply` or `mode=top-level`; ignored when `mode=heartbeat-only` | message text |
| `mode` | yes | `heartbeat-only`, `reply`, or `top-level` |

The marker (`Daily AICR-bot status — $(date -u +%Y-%m-%d)`) is computed internally by the action and is **not** a caller-controlled input. This guarantees every workflow looks for and creates parents using the exact same string.

**Modes:**

- `heartbeat-only` — execute steps 1-5 of the find-or-create algorithm; exit. The parent's text is the marker itself (no caller-supplied body). Used by the daily heartbeat workflow.
- `reply` — execute all six steps. Posts `body` as a thread reply under today's daily parent (lazy-creates the parent if missing). Used by Mon-Fri vuln-scan, issue-report, on-tag.
- `top-level` — skip lookup; post `body` as a top-level message via `chat.postMessage` (no `thread_ts`). Used by weekly-summary and weekend on-tag releases.

**Implementation:** shell + `curl` against `https://slack.com/api/{conversations.history,chat.postMessage}` + `jq` for JSON parsing. Bot token passed as `Authorization: Bearer ${BOT_TOKEN}` header. All Slack API calls check the response `ok` field; on `false`, the action fails with the `error` string for diagnostics.

**Outputs:**

| output | purpose |
|---|---|
| `parent-ts` | `ts` of today's parent (created or found) — only set in `heartbeat-only` and `reply` modes |
| `posted-ts` | `ts` of the posted reply or top-level message |

### NEW: `.github/workflows/daily-status.yaml`

Single job, single step. Triggers:

```yaml
on:
  schedule:
    - cron: '0 5 * * 1-5'   # Mon-Fri 05:00 UTC
  workflow_dispatch: {}
```

Body: checks out repo (for the composite action), invokes `slack-thread-post` with `mode=heartbeat-only`. Permissions: `contents: read`. Concurrency group `daily-status` with `cancel-in-progress: true`.

### NEW: `.github/workflows/weekly-summary.yaml`

Triggers:

```yaml
on:
  schedule:
    - cron: '0 14 * * 6'   # Saturday 14:00 UTC
  workflow_dispatch: {}
```

Single job, three data-collection steps + one Slack post:

1. **Collect releases (7d).** `gh release list --limit 20 --json tagName,publishedAt,htmlUrl`, filter to `publishedAt >= 7 days ago` UTC. Build the `Releases (N)` block with bullet per release: `• {tag} ({weekday}) — <{url}|link>`.
2. **Collect issue movement (7d).** Re-use the `actions/github-script` block from `issue-report.yaml`'s `Collect issue metrics` step but with `since = now - 7*86400000`. Output: total open, +new, -closed, type & priority breakdown, SLA flags.
3. **Collect latest vuln scan.** `gh run list --workflow=vuln-scan-images.yaml --status=success --limit=1 --json databaseId,createdAt,htmlUrl`. The vuln section emits one line: `Latest scan: {weekday} {HH:MM} UTC — <{run_url}|view run>`. **No per-image counts inline:** the existing workflow's `scan-*` artifacts use `retention-days: 1`, which expires before Saturday 14:00 UTC; counts would be unreliable to fetch. The link is sufficient — readers click for the per-image breakdown.
4. **Post to Slack.** Concatenate the three sections into one message; call `slack-thread-post` with `mode=top-level`.

Permissions: `contents: read`, `issues: read`, `actions: read` (for `gh run list`).

### MODIFIED: `.github/workflows/vuln-scan-images.yaml`

- Cron change: `'0 6 * * *'` → `'0 6 * * 1-5'`.
- `notify` job: replace the inline `curl` (lines 276-306) with a call to `./.github/actions/slack-thread-post`:
  - `mode: reply`
  - `bot-token: ${{ secrets.SLACK_BOT_TOKEN }}`
  - `channel-id: ${{ vars.SLACK_CHANNEL_ID }}`
  - `body: ${{ env.MESSAGE }}` (built by the existing message-construction shell)
- Drop the `SLACK_SERVICE` env from the `notify` job.

### MODIFIED: `.github/workflows/issue-report.yaml`

- Cron change: `'30 12 * * *'` → `'30 12 * * 1-5'`.
- Replace the `Post to Slack` step (lines 266-278) with a call to `./.github/actions/slack-thread-post`:
  - `mode: reply`
  - `body: $(jq -r .text < slack-payload.json)` (the existing `actions/github-script` step already writes the message text into `slack-payload.json`)
- Drop the `SLACK_SERVICE` env.

### MODIFIED: `.github/workflows/on-tag.yaml`

- `notify` job posts via the composite action with mode determined by day of week:
  - Mon-Fri: `mode=reply` (threads under today's daily parent; lazy-creates if missing).
  - Sat-Sun: `mode=top-level`.
- Day-of-week is computed in the step: `[[ $(date -u +%u) -le 5 ]]` selects weekday vs. weekend.
- Drop the `SLACK_SERVICE` env.

## Edge Cases & Error Handling

| Scenario | Behaviour |
|---|---|
| Bot not in channel | `chat.postMessage` returns `not_in_channel`. Action fails with explicit error pointing to the operational prerequisite. No retry — operator action required. |
| `conversations.history` rate-limited or fails | Fall back to creating a fresh parent. Worst case: one orphaned top-level message that day. Workflow does not fail. |
| `chat.postMessage` fails on the post/reply | Log message body to `$GITHUB_STEP_SUMMARY`, exit non-zero. Matches today's behaviour on webhook failure. |
| Race producing two daily parents | Both survive; later workflows reply to most recent matching parent. Self-heals next day. |
| `SLACK_BOT_TOKEN` / `SLACK_CHANNEL_ID` unset | Action fails immediately with explicit message naming the missing setting. |
| UTC date rollover during a workflow run | Marker is computed once at step start; subsequent calls in the same run use that snapshot. A workflow that begins at 23:59 UTC and posts at 00:01 UTC threads under yesterday's parent — acceptable. |
| Bot token revoked / rotated | Both APIs return `invalid_auth`. Action fails with that error. Operator rotates the secret. |
| Weekend `on-tag` release | Posts top-level via `mode=top-level`. No daily parent on weekends. |
| US public holiday on a weekday (no Slack readers) | Daily heartbeat still posts; the empty-parent cost is one short line per holiday. Acceptable; not worth the holiday-calendar complexity. |
| Weekly summary fires when no releases happened that week | Section says `Releases (0): _none this week_`. Issue and vuln sections still post. |
| `gh run list --workflow=vuln-scan-images.yaml --status=success --limit=1` returns no run (e.g., all five Mon-Fri runs failed) | Vuln section says "Latest scan: no successful run this week — see Actions tab." Other sections still post. |

## Rollout Plan

Single PR, five logical commits (squash optional) — landed in this order so each step is independently verifiable:

1. **Composite action + heartbeat workflow.** Verify the parent posts Mon-Fri for one cycle. Other workflows still post via webhook.
2. **Migrate `vuln-scan-images.yaml`.** Cron change to weekday-only + Slack step swap. Confirm threading.
3. **Migrate `issue-report.yaml`.** Same pattern.
4. **Migrate `on-tag.yaml`.** Day-of-week-aware mode. Confirm on next release tag.
5. **Add `weekly-summary.yaml`.** Confirm on first Saturday after merge.
6. **Cleanup (separate small PR after one full week of clean runs):** delete the `SLACK_SERVICE` secret in repo settings.

If preferred, all migrations can land as one squash PR — but the staged approach makes rollback trivial (revert the migration commit; webhook path still works).

## Testing

- **Pre-merge smoke test (manual, in author's fork or a test channel):**
  - `workflow_dispatch` `daily-status.yaml` → confirm parent posts with the marker.
  - `workflow_dispatch` `vuln-scan-images.yaml` → confirm reply threads under the parent.
  - `workflow_dispatch` `issue-report.yaml` → confirm reply threads.
  - `workflow_dispatch` `weekly-summary.yaml` → confirm top-level post with three sections.
  - Push a test tag (`v0.0.0-test`) on a fork → confirm `on-tag` notify replies in-thread on a weekday, top-level on a weekend (test by running on different days, or by stubbing the day-of-week check).
- **Test channel isolation (recommended):** add a `SLACK_CHANNEL_ID_TEST` variable; gate via `workflow_dispatch` input so smoke tests don't post to the production channel.
- **Post-merge monitoring:** watch the AICR channel for one full week (Mon-Sun) — verify Mon-Fri threading, Saturday weekly post, no Sunday bot noise.

## Out of Scope (deliberate YAGNI)

- Editing the parent post to embed running counts (e.g., "Releases today: v0.12.0, v0.12.1"). Adds concurrent-edit complexity without clear value over reading the thread.
- Retention or archival of past day threads — Slack's own UI handles this.
- Cross-repo coordination — only the AICR repo's bot posts to this channel.
- Migrating other potential bot consumers (none currently exist in `NVIDIA/aicr`).
- Replacing the issue-report or vuln-scan payload contents — only the transport changes.
- US-holiday awareness for the daily heartbeat. Empty parent on a holiday is acceptable.
- Backfilling weekly summaries for prior weeks — first Saturday after merge is the first weekly post.

## References

- Slack Web API — `chat.postMessage`: https://api.slack.com/methods/chat.postMessage
- Slack Web API — `conversations.history`: https://api.slack.com/methods/conversations.history
- Slack Web API — best practices for threaded posts: https://api.slack.com/messaging/sending#threading
- Existing workflows on `upstream/main`:
  - `1caf2607 feat(ci): add daily Slack issue status report`
  - `7b0dbb1f fix(ci): make issue report counts clickable Slack links`
  - `ad682efe feat(ci): add daily image vulnerability scan workflow`
  - `14178870 ci: add Slack release notification to on-tag workflow`
