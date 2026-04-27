# AICR-Bot Slack Cadence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Migrate AICR-Bot's Slack posting from per-event Incoming Webhooks to a two-cadence Web-API model: a Mon-Fri short heartbeat parent with threaded replies for vuln-scan / issue-report / release events, and a Saturday top-level weekly digest.

**Architecture:** New shared composite action `.github/actions/slack-thread-post/` encapsulates all Slack Web API calls (find or lazy-create today's parent via `conversations.history` + marker pattern, then `chat.postMessage` with `thread_ts` for replies, or no `thread_ts` for top-level). One new heartbeat workflow plus one new weekly-summary workflow are added; three existing workflows (`vuln-scan-images.yaml`, `issue-report.yaml`, `on-tag.yaml`) replace their inline `curl` Slack calls with the composite action.

**Tech Stack:**
- GitHub Actions (composite actions, scheduled workflows, `workflow_dispatch`)
- Bash + `curl` + `jq` for Slack Web API calls
- Slack Web API: `chat.postMessage`, `conversations.history`
- Linting: `actionlint`, `yamllint`, embedded `shellcheck` (via actionlint)

**Spec:** `docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md`

---

## File Structure

| File | Status | Purpose |
|---|---|---|
| `.github/actions/slack-thread-post/action.yml` | NEW | Composite action: shell + curl + jq for all three Slack post modes |
| `.github/workflows/daily-status.yaml` | NEW | Mon-Fri 05:00 UTC heartbeat parent post |
| `.github/workflows/weekly-summary.yaml` | NEW | Saturday 14:00 UTC weekly top-level digest |
| `.github/workflows/vuln-scan-images.yaml` | MODIFY | Cron → weekday-only; notify job → composite action (mode=reply) |
| `.github/workflows/issue-report.yaml` | MODIFY | Cron → weekday-only; Slack post step → composite action (mode=reply) |
| `.github/workflows/on-tag.yaml` | MODIFY | Notify job → composite action; mode determined by day-of-week |

Each file has one responsibility. The composite action absorbs all Slack-protocol complexity so caller workflows stay short and uniform. No mixing of concerns.

---

## Operational Prerequisite (BEFORE PR merge — repo admin task)

This is **not a code task** but must complete before the PR's workflows will succeed in the production channel. Document this in the PR description.

1. Create or reuse a Slack App in the NVIDIA workspace.
2. Grant OAuth scopes:
   - `chat:write`
   - `channels:history` (public channel) **or** `groups:history` (private channel)
3. Install the app to the workspace; invite the bot to the AICR target channel.
4. Add repo settings:
   - **Secret** `SLACK_BOT_TOKEN` (the `xoxb-…` bot token)
   - **Variable** `SLACK_CHANNEL_ID` (channel ID, e.g., `C012ABC3DEF`; not secret)
5. Keep existing `SLACK_SERVICE` secret in place — removed in a follow-up cleanup PR after a full week of clean runs.

---

## Task 1: Create the `slack-thread-post` composite action

**Files:**
- Create: `.github/actions/slack-thread-post/action.yml`

This is the foundation — every other task depends on it. The action encapsulates: input validation, marker computation, Slack API calls with error handling, three modes (`heartbeat-only`, `reply`, `top-level`).

- [ ] **Step 1: Create the directory and action file**

```bash
mkdir -p .github/actions/slack-thread-post
```

Then create `.github/actions/slack-thread-post/action.yml` with the following exact content:

```yaml
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

name: 'Slack Thread Post'
description: 'Post messages to Slack with daily-thread coordination via marker pattern'

inputs:
  bot-token:
    description: 'Slack bot token (xoxb-...)'
    required: true
  channel-id:
    description: 'Slack channel ID (e.g. C012ABC3DEF)'
    required: true
  body:
    description: 'Message text. Required when mode=reply or mode=top-level; ignored when mode=heartbeat-only.'
    required: false
    default: ''
  mode:
    description: 'heartbeat-only | reply | top-level'
    required: true

outputs:
  parent-ts:
    description: "Timestamp of today's daily parent (set in heartbeat-only and reply modes)"
    value: ${{ steps.post.outputs.parent_ts }}
  posted-ts:
    description: 'Timestamp of the posted message (set in reply and top-level modes)'
    value: ${{ steps.post.outputs.posted_ts }}

runs:
  using: 'composite'
  steps:
    - name: Post to Slack
      id: post
      shell: bash
      env:
        BOT_TOKEN: ${{ inputs.bot-token }}
        CHANNEL_ID: ${{ inputs.channel-id }}
        BODY: ${{ inputs.body }}
        MODE: ${{ inputs.mode }}
      run: |
        set -euo pipefail

        # ---- Input validation -----------------------------------------------
        if [[ -z "${BOT_TOKEN}" ]]; then
          echo "::error::SLACK_BOT_TOKEN is empty — set the secret in repo settings"
          exit 1
        fi
        if [[ -z "${CHANNEL_ID}" ]]; then
          echo "::error::SLACK_CHANNEL_ID is empty — set the variable in repo settings"
          exit 1
        fi
        case "${MODE}" in
          heartbeat-only|reply|top-level) ;;
          *)
            echo "::error::invalid mode: '${MODE}' (must be heartbeat-only, reply, or top-level)"
            exit 1
            ;;
        esac
        if [[ "${MODE}" != "heartbeat-only" && -z "${BODY}" ]]; then
          echo "::error::input 'body' is required when mode=${MODE}"
          exit 1
        fi

        # ---- Daily marker (UTC) — internal, not caller-controlled ----------
        MARKER="Daily AICR-bot status — $(date -u +%Y-%m-%d)"

        # ---- Slack API helper -----------------------------------------------
        # Calls https://slack.com/api/<method>, fails on response.ok != true.
        # Echoes the full response on success so the caller can jq it.
        slack_api() {
          local method="$1"; shift
          local response
          response=$(curl -sS \
            -H "Authorization: Bearer ${BOT_TOKEN}" \
            -H 'Content-Type: application/json; charset=utf-8' \
            "$@" \
            "https://slack.com/api/${method}")
          local ok
          ok=$(printf '%s' "${response}" | jq -r '.ok')
          if [[ "${ok}" != "true" ]]; then
            local err
            err=$(printf '%s' "${response}" | jq -r '.error // "unknown_error"')
            echo "::error::Slack ${method} failed: ${err}"
            printf '%s' "${response}" | jq . >&2 || true
            return 1
          fi
          printf '%s' "${response}"
        }

        # ---- Top-level mode: post and exit ----------------------------------
        if [[ "${MODE}" == "top-level" ]]; then
          payload=$(jq -n --arg ch "${CHANNEL_ID}" --arg t "${BODY}" \
            '{channel: $ch, text: $t}')
          response=$(slack_api chat.postMessage --data "${payload}")
          posted_ts=$(printf '%s' "${response}" | jq -r '.ts')
          echo "posted_ts=${posted_ts}" >> "${GITHUB_OUTPUT}"
          echo "Posted top-level message: ts=${posted_ts}"
          exit 0
        fi

        # ---- heartbeat-only / reply: find or lazy-create today's parent -----
        # conversations.history may rate-limit; on failure we treat as "no parent
        # found" and fall through to lazy-create. Worst case is a duplicate
        # parent, which self-heals next day.
        parent_ts=""
        if history=$(slack_api conversations.history --get \
              --data-urlencode "channel=${CHANNEL_ID}" \
              --data-urlencode "limit=20"); then
          parent_ts=$(printf '%s' "${history}" \
            | jq -r --arg m "${MARKER}" \
                '[.messages[] | select(.text != null and (.text | startswith($m)))] | .[0].ts // empty')
        else
          echo "::warning::conversations.history failed — proceeding with lazy-create"
        fi

        if [[ -z "${parent_ts}" ]]; then
          payload=$(jq -n --arg ch "${CHANNEL_ID}" --arg t "${MARKER}" \
            '{channel: $ch, text: $t}')
          response=$(slack_api chat.postMessage --data "${payload}")
          parent_ts=$(printf '%s' "${response}" | jq -r '.ts')
          echo "Created parent: ts=${parent_ts}"
        else
          echo "Found existing parent: ts=${parent_ts}"
        fi

        echo "parent_ts=${parent_ts}" >> "${GITHUB_OUTPUT}"

        # ---- heartbeat-only: stop after parent is ensured -------------------
        if [[ "${MODE}" == "heartbeat-only" ]]; then
          exit 0
        fi

        # ---- reply: post body as thread reply -------------------------------
        payload=$(jq -n \
          --arg ch "${CHANNEL_ID}" \
          --arg t "${BODY}" \
          --arg ts "${parent_ts}" \
          '{channel: $ch, text: $t, thread_ts: $ts}')
        response=$(slack_api chat.postMessage --data "${payload}")
        posted_ts=$(printf '%s' "${response}" | jq -r '.ts')
        echo "posted_ts=${posted_ts}" >> "${GITHUB_OUTPUT}"
        echo "Posted thread reply: ts=${posted_ts} (parent=${parent_ts})"
```

- [ ] **Step 2: Lint with actionlint**

Run: `actionlint .github/actions/slack-thread-post/action.yml`
Expected: no output (success).

If errors: read each, fix in the file, re-run. Common issues will be: missing `name:` on a step, wrong indentation, unsupported `runs.using` value (must be `composite`), or shellcheck warnings on the embedded shell.

- [ ] **Step 3: Lint with yamllint**

Run: `yamllint .github/actions/slack-thread-post/action.yml`
Expected: no output (success).

The project's `.yamllint` (or default config) sets line-length and indentation rules. If a line is flagged for length, prefer breaking strings with shell concatenation over disabling the rule.

- [ ] **Step 4: Lint shell with shellcheck (extracted)**

`actionlint` includes shellcheck on `run:` blocks, so step 2 already covers this. If you want a standalone confirmation:

```bash
# Extract the shell block and shellcheck it directly
yq '.runs.steps[] | select(.id == "post") | .run' \
  .github/actions/slack-thread-post/action.yml \
  | shellcheck -s bash -
```

Expected: no output (success). If `yq` is unavailable, skip — actionlint already shellchecks.

- [ ] **Step 5: Commit**

```bash
git add .github/actions/slack-thread-post/action.yml
git commit -s -S -m "feat(ci): add slack-thread-post composite action

Encapsulates Slack Web API calls for daily-thread coordination:
- mode=heartbeat-only: ensure today's parent exists (find or create)
- mode=reply: post body as thread reply under today's parent
  (lazy-creates parent if missing)
- mode=top-level: post body as top-level message

Marker pattern 'Daily AICR-bot status — YYYY-MM-DD' (UTC) is computed
internally so every caller agrees on the lookup key. State lives in
Slack via conversations.history; no external store needed.

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md"
```

---

## Task 2: Add the `daily-status` heartbeat workflow

**Files:**
- Create: `.github/workflows/daily-status.yaml`

Mon-Fri 05:00 UTC. Calls the composite action with `mode=heartbeat-only` to ensure today's parent message exists in the channel.

- [ ] **Step 1: Create the workflow file**

Create `.github/workflows/daily-status.yaml` with the following exact content:

```yaml
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

name: Daily AICR-Bot Status

on:
  schedule:
    - cron: '0 5 * * 1-5'   # Mon-Fri 05:00 UTC
  workflow_dispatch: {}

permissions:
  contents: read

concurrency:
  group: ${{ github.workflow }}
  cancel-in-progress: true

jobs:
  heartbeat:
    name: Ensure Daily Parent Message
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - name: Checkout Code
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6.0.2
        with:
          persist-credentials: false

      - name: Post heartbeat
        uses: ./.github/actions/slack-thread-post
        with:
          bot-token: ${{ secrets.SLACK_BOT_TOKEN }}
          channel-id: ${{ vars.SLACK_CHANNEL_ID }}
          mode: heartbeat-only
```

- [ ] **Step 2: Lint with actionlint**

Run: `actionlint .github/workflows/daily-status.yaml`
Expected: no output (success).

- [ ] **Step 3: Lint with yamllint**

Run: `yamllint .github/workflows/daily-status.yaml`
Expected: no output (success).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/daily-status.yaml
git commit -s -S -m "feat(ci): add daily AICR-bot status heartbeat workflow

Posts a short parent message ('Daily AICR-bot status — YYYY-MM-DD')
Mon-Fri at 05:00 UTC. Other Slack-posting workflows reply in-thread
under this parent on weekdays.

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md"
```

---

## Task 3: Migrate `vuln-scan-images.yaml` to threaded reply

**Files:**
- Modify: `.github/workflows/vuln-scan-images.yaml`

Two changes: (a) cron from daily to weekday-only, (b) replace the inline `curl` in the `notify` job with a call to the composite action.

- [ ] **Step 1: Change the cron to weekday-only**

Open `.github/workflows/vuln-scan-images.yaml`. Find the `on.schedule.cron` line (currently `'0 6 * * *'`) and change it to:

```yaml
on:
  schedule:
    - cron: '0 6 * * 1-5'  # Mon-Fri 06:00 UTC
  workflow_dispatch: {}
```

- [ ] **Step 2: Replace the `notify` job's Slack step**

Find the `notify` job (its `name: Slack Notification`). Replace its entire `steps:` block with:

```yaml
    steps:
      - name: Checkout Code
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6.0.2
        with:
          persist-credentials: false

      - name: Download all scan results
        uses: actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c  # v8.0.1
        with:
          pattern: scan-*
          merge-multiple: true
          path: scan-results

      - name: Build Slack message
        env:
          SHA: ${{ github.sha }}
          SERVER: ${{ github.server_url }}
          REPO: ${{ github.repository }}
          RUN_ID: ${{ github.run_id }}
        run: |
          set -euo pipefail
          SHORT_SHA="${SHA:0:7}"
          TIMESTAMP=$(date -u '+%Y-%m-%d %H:%M')
          RUN_URL="${SERVER}/${REPO}/actions/runs/${RUN_ID}"

          MESSAGE="Vulnerability Scan: ${TIMESTAMP} (${SHORT_SHA})"
          for f in scan-results/*.txt; do
            [[ -f "$f" ]] || continue
            MESSAGE="${MESSAGE}"$'\n'"• $(cat "$f")"
          done
          MESSAGE="${MESSAGE}"$'\n'"<${RUN_URL}|View run>"

          {
            echo "SLACK_MESSAGE<<SLACK_EOF"
            echo "${MESSAGE}"
            echo "SLACK_EOF"
          } >> "${GITHUB_ENV}"

      - name: Post to Slack
        uses: ./.github/actions/slack-thread-post
        with:
          bot-token: ${{ secrets.SLACK_BOT_TOKEN }}
          channel-id: ${{ vars.SLACK_CHANNEL_ID }}
          body: ${{ env.SLACK_MESSAGE }}
          mode: reply
```

The previous `Post to Slack` step (which had the inline `curl` to `hooks.slack.com`) is gone. The previous `Download all scan results` step is preserved verbatim.

- [ ] **Step 3: Lint with actionlint**

Run: `actionlint .github/workflows/vuln-scan-images.yaml`
Expected: no output (success).

- [ ] **Step 4: Lint with yamllint**

Run: `yamllint .github/workflows/vuln-scan-images.yaml`
Expected: no output (success).

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/vuln-scan-images.yaml
git commit -s -S -m "feat(ci): vuln-scan-images posts as Slack thread reply

- Cron shifts to Mon-Fri only ('0 6 * * 1-5'); weekend signal moves
  to the new weekly-summary workflow.
- Replaces the inline webhook curl in the notify job with the new
  slack-thread-post composite action (mode=reply). Body assembly
  is moved to a dedicated step that writes to GITHUB_ENV so the
  multi-line message survives YAML escaping.
- Drops the SLACK_SERVICE env from the notify job. The webhook
  secret stays in repo settings until all migrations land.

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md"
```

---

## Task 4: Migrate `issue-report.yaml` to threaded reply

**Files:**
- Modify: `.github/workflows/issue-report.yaml`

Two changes: (a) cron weekday-only, (b) replace the `Post to Slack` step.

- [ ] **Step 1: Change the cron to weekday-only**

Find the `on.schedule.cron` line (currently `'30 12 * * *'`) and change it to:

```yaml
on:
  schedule:
    # 5:30 AM Pacific (12:30 UTC during PDT Mar–Nov, Mon-Fri only).
    - cron: '30 12 * * 1-5'
  workflow_dispatch: {}
```

(Update the comment to reflect Mon-Fri.)

- [ ] **Step 2: Add a checkout step at the top of the `report` job**

The job currently has no checkout step. Add this as the **first** step in the `report` job's `steps:` block (before the existing `Collect issue metrics` step):

```yaml
      - name: Checkout Code
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6.0.2
        with:
          persist-credentials: false
```

- [ ] **Step 3: Replace the `Post to Slack` step**

Find the existing `Post to Slack` step (the one that runs `curl … hooks.slack.com`). Replace it with these two steps:

```yaml
      - name: Stage Slack message
        run: |
          set -euo pipefail
          {
            echo "SLACK_MESSAGE<<SLACK_EOF"
            jq -r '.text' slack-payload.json
            echo "SLACK_EOF"
          } >> "${GITHUB_ENV}"

      - name: Post to Slack
        uses: ./.github/actions/slack-thread-post
        with:
          bot-token: ${{ secrets.SLACK_BOT_TOKEN }}
          channel-id: ${{ vars.SLACK_CHANNEL_ID }}
          body: ${{ env.SLACK_MESSAGE }}
          mode: reply
```

- [ ] **Step 4: Lint with actionlint**

Run: `actionlint .github/workflows/issue-report.yaml`
Expected: no output (success).

- [ ] **Step 5: Lint with yamllint**

Run: `yamllint .github/workflows/issue-report.yaml`
Expected: no output (success).

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/issue-report.yaml
git commit -s -S -m "feat(ci): issue-report posts as Slack thread reply

- Cron shifts to Mon-Fri only ('30 12 * * 1-5'); weekend signal
  moves to the new weekly-summary workflow.
- Replaces the inline webhook curl with the slack-thread-post
  composite action (mode=reply). The existing actions/github-script
  step still writes slack-payload.json; a small staging step lifts
  .text into GITHUB_ENV for the action.
- Adds an actions/checkout step (required for local composite
  action access).

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md"
```

---

## Task 5: Migrate `on-tag.yaml` notify job (day-of-week aware)

**Files:**
- Modify: `.github/workflows/on-tag.yaml`

Weekday releases thread under the daily parent; weekend releases post top-level.

- [ ] **Step 1: Replace the `notify` job's `steps:` block**

Find the `notify` job (`name: Slack Notification`). Replace its entire `steps:` block with:

```yaml
    steps:
      - name: Checkout Code
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6.0.2
        with:
          persist-credentials: false

      - name: Build message and choose mode
        env:
          TAG: ${{ github.ref_name }}
          REPO: ${{ github.repository }}
          SERVER: ${{ github.server_url }}
        run: |
          set -euo pipefail
          RELEASE_URL="${SERVER}/${REPO}/releases/tag/${TAG}"
          MESSAGE="AICR ${TAG} has been released: ${RELEASE_URL}"

          # date -u +%u: 1=Mon … 7=Sun
          DOW=$(date -u +%u)
          if [[ "${DOW}" -le 5 ]]; then
            MODE="reply"
          else
            MODE="top-level"
          fi

          {
            echo "SLACK_MESSAGE<<SLACK_EOF"
            echo "${MESSAGE}"
            echo "SLACK_EOF"
            echo "SLACK_MODE=${MODE}"
          } >> "${GITHUB_ENV}"

      - name: Post to Slack
        uses: ./.github/actions/slack-thread-post
        with:
          bot-token: ${{ secrets.SLACK_BOT_TOKEN }}
          channel-id: ${{ vars.SLACK_CHANNEL_ID }}
          body: ${{ env.SLACK_MESSAGE }}
          mode: ${{ env.SLACK_MODE }}
```

The previous step's `SLACK_SERVICE` env and `curl` to `hooks.slack.com` are gone.

- [ ] **Step 2: Lint with actionlint**

Run: `actionlint .github/workflows/on-tag.yaml`
Expected: no output (success).

- [ ] **Step 3: Lint with yamllint**

Run: `yamllint .github/workflows/on-tag.yaml`
Expected: no output (success).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/on-tag.yaml
git commit -s -S -m "feat(ci): on-tag release notify uses daily-thread coordination

Mon-Fri release tags reply in-thread under the daily heartbeat
parent (lazy-creates if a release fires before the 05:00 UTC
heartbeat). Sat-Sun release tags post top-level (no daily parent
on weekends — releases are rare and inherently noteworthy).

Day-of-week is computed from 'date -u +%u' (1=Mon … 7=Sun).

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md"
```

---

## Task 6: Add the `weekly-summary` workflow

**Files:**
- Create: `.github/workflows/weekly-summary.yaml`

Saturday 14:00 UTC. Aggregates 7-day releases, 7-day issue movement, and a link to the latest vuln scan run; posts a single top-level message.

- [ ] **Step 1: Create the workflow file**

Create `.github/workflows/weekly-summary.yaml` with the following exact content:

```yaml
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

name: Weekly AICR Summary

on:
  schedule:
    - cron: '0 14 * * 6'   # Saturday 14:00 UTC
  workflow_dispatch: {}

permissions:
  contents: read

concurrency:
  group: ${{ github.workflow }}
  cancel-in-progress: true

jobs:

  summary:
    name: Post Weekly Summary
    runs-on: ubuntu-latest
    timeout-minutes: 5
    permissions:
      contents: read
      issues: read
      actions: read
    steps:
      - name: Checkout Code
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6.0.2
        with:
          persist-credentials: false

      - name: Collect releases (last 7 days)
        id: releases
        env:
          GH_TOKEN: ${{ github.token }}
          REPO: ${{ github.repository }}
        run: |
          set -euo pipefail
          # Window: now - 7 days, UTC
          SINCE=$(date -u -d '7 days ago' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
            || date -u -v-7d +%Y-%m-%dT%H:%M:%SZ)

          RELEASES_JSON=$(gh release list --repo "${REPO}" --limit 20 \
            --json tagName,publishedAt,url)

          # Filter to publishedAt >= SINCE
          FILTERED=$(printf '%s' "${RELEASES_JSON}" \
            | jq --arg since "${SINCE}" \
                '[.[] | select(.publishedAt >= $since)]')

          COUNT=$(printf '%s' "${FILTERED}" | jq 'length')

          if [[ "${COUNT}" -eq 0 ]]; then
            BLOCK="*Releases (0):* _none this week_"
          else
            BULLETS=$(printf '%s' "${FILTERED}" | jq -r \
              '.[] | "• <\(.url)|\(.tagName)> (\(.publishedAt | sub("T.*"; "")))"')
            BLOCK="*Releases (${COUNT}):*"$'\n'"${BULLETS}"
          fi

          {
            echo "RELEASES_BLOCK<<REL_EOF"
            echo "${BLOCK}"
            echo "REL_EOF"
          } >> "${GITHUB_ENV}"

      - name: Collect issue movement (last 7 days)
        uses: actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3  # v9.0.0
        env:
          GITHUB_TOKEN: ${{ github.token }}
        with:
          script: |
            const fs = require('fs');
            const TYPE_LABELS = ['bug', 'feature', 'documentation'];
            const PRIORITY_LABELS = ['P0', 'P1', 'P2'];
            const SLA = {
              P0: { days: 30 },
              P1: { days: 180 },
            };
            const SEVEN_DAYS_MS = 7 * 86400000;
            const since = new Date(Date.now() - SEVEN_DAYS_MS).toISOString();

            const hasLabel = (i, n) => i.labels.some(l => l.name === n);
            const ageDays = (i) =>
              Math.floor((Date.now() - new Date(i.created_at)) / 86400000);

            const open = (await github.paginate(
              github.rest.issues.listForRepo,
              { ...context.repo, state: 'open', per_page: 100 },
            )).filter(i => !i.pull_request);

            const closed = (await github.paginate(
              github.rest.issues.listForRepo,
              { ...context.repo, state: 'closed', since, per_page: 100 },
            )).filter(i => !i.pull_request &&
                          new Date(i.closed_at) >= new Date(since)).length;

            const total = open.length;
            const newCount = open.filter(
              i => new Date(i.created_at) >= new Date(since)).length;

            const types = TYPE_LABELS
              .map(l => [l, open.filter(i => hasLabel(i, l)).length])
              .filter(([, c]) => c > 0);

            let prioritized = 0;
            const pri = PRIORITY_LABELS.map(l => {
              const c = open.filter(i => hasLabel(i, l)).length;
              prioritized += c;
              return [l, c];
            });
            const noPri = total - prioritized;

            const unowned = open.filter(i =>
              (hasLabel(i, 'P0') || hasLabel(i, 'P1')) &&
              (!i.assignees || i.assignees.length === 0)).length;

            const breaches = [];
            for (const i of open) {
              const age = ageDays(i);
              for (const [k, sla] of Object.entries(SLA)) {
                if (hasLabel(i, k) && age > sla.days) {
                  breaches.push({ num: i.number, pri: k, over: age - sla.days });
                }
              }
            }

            const lines = [
              '*Issue movement (7d):*',
              `${total} open, +${newCount} new, -${closed} closed`,
            ];
            const priParts = pri
              .filter(([, c]) => c > 0)
              .map(([l, c]) => `${l}: ${c}`);
            if (noPri > 0) priParts.push(`none: ${noPri}`);
            lines.push(`Priority: ${priParts.join(', ')}`);
            if (types.length > 0) {
              lines.push(`Type: ${types.map(([l, c]) => `${l}: ${c}`).join(', ')}`);
            }
            if (unowned > 0) {
              lines.push(`Unowned P0/P1: *${unowned}*`);
            }
            if (breaches.length > 0) {
              lines.push(`:rotating_light: ${breaches.length} SLA breach${breaches.length === 1 ? '' : 'es'}`);
            }

            const block = lines.join('\n');
            fs.appendFileSync(process.env.GITHUB_ENV,
              `ISSUES_BLOCK<<ISS_EOF\n${block}\nISS_EOF\n`);

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
            BLOCK="*Latest vuln scan:* _no successful run this week — see Actions tab_"
          else
            URL=$(printf '%s' "${RUN_JSON}" | jq -r '.[0].url')
            CREATED=$(printf '%s' "${RUN_JSON}" | jq -r '.[0].createdAt')
            DAY=$(date -u -d "${CREATED}" '+%a %H:%M UTC' 2>/dev/null \
              || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "${CREATED}" '+%a %H:%M UTC')
            BLOCK="*Latest vuln scan:* ${DAY} — <${URL}|view run>"
          fi

          {
            echo "VULN_BLOCK<<VULN_EOF"
            echo "${BLOCK}"
            echo "VULN_EOF"
          } >> "${GITHUB_ENV}"

      - name: Assemble summary message
        env:
          REPO: ${{ github.repository }}
        run: |
          set -euo pipefail
          # Week-of label: Monday of the current calendar week (UTC)
          # date -u +%u gives 1=Mon..7=Sun; subtract that minus 1 to get Monday
          DOW=$(date -u +%u)
          WEEK_START=$(date -u -d "$((DOW - 1)) days ago" +%Y-%m-%d 2>/dev/null \
            || date -u -v-$((DOW - 1))d +%Y-%m-%d)

          MESSAGE="*AICR weekly* — week of ${WEEK_START}"$'\n\n'
          MESSAGE+="${RELEASES_BLOCK}"$'\n\n'
          MESSAGE+="${ISSUES_BLOCK}"$'\n\n'
          MESSAGE+="${VULN_BLOCK}"

          {
            echo "SLACK_MESSAGE<<SLACK_EOF"
            echo "${MESSAGE}"
            echo "SLACK_EOF"
          } >> "${GITHUB_ENV}"

      - name: Post to Slack (top-level)
        uses: ./.github/actions/slack-thread-post
        with:
          bot-token: ${{ secrets.SLACK_BOT_TOKEN }}
          channel-id: ${{ vars.SLACK_CHANNEL_ID }}
          body: ${{ env.SLACK_MESSAGE }}
          mode: top-level
```

- [ ] **Step 2: Lint with actionlint**

Run: `actionlint .github/workflows/weekly-summary.yaml`
Expected: no output (success).

- [ ] **Step 3: Lint with yamllint**

Run: `yamllint .github/workflows/weekly-summary.yaml`
Expected: no output (success).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/weekly-summary.yaml
git commit -s -S -m "feat(ci): add weekly AICR summary workflow (Saturday 14:00 UTC)

Top-level Slack post with three sections:
- Releases this week (gh release list filtered to last 7 days)
- Issue movement (7-day window of issue-report logic)
- Latest vuln scan (link to most recent successful run)

Read time: under 30 seconds. Posts via slack-thread-post composite
action with mode=top-level (no thread coordination needed).

Refs: docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md"
```

---

## Task 7: Run the full project lint suite

**Files:** none modified.

Verify the whole working tree still passes the project's lint gate before opening the PR.

- [ ] **Step 1: Run `make lint`**

Run: `make lint`
Expected: success (exit 0). The output may include yamllint and golangci-lint runs. Yaml-related findings on the new/modified files should be zero.

If `make lint` fails on unrelated existing issues (pre-existing on `upstream/main`), confirm by running `git stash && make lint && git stash pop` — if the unstashed run fails identically, the failure is pre-existing and not blocking. Document any pre-existing failure in the PR description.

- [ ] **Step 2: Run actionlint across all workflows**

Run: `actionlint`
Expected: no output (success). Catches any cross-file issues missed by per-file linting.

---

## Task 8: Pre-PR smoke test (manual gate)

**Files:** none modified. Operator-driven verification.

This task does not produce a commit. It produces evidence that the migration works end-to-end against a real Slack channel — required before opening the PR for review.

- [ ] **Step 1: Confirm Slack app is set up and bot is in the channel**

Confirm with the repo admin (or perform yourself if you have permissions) that the Operational Prerequisite is complete:

```bash
# Quick check: does the secret exist?
gh secret list --repo <fork-or-target-repo> | grep -E '^SLACK_BOT_TOKEN'
gh variable list --repo <fork-or-target-repo> | grep -E '^SLACK_CHANNEL_ID'
```

Both should be present. If you want to use a separate test channel for this smoke (recommended), set `SLACK_CHANNEL_ID` on a fork or in a separate test channel for the duration of testing.

- [ ] **Step 2: Push the branch and trigger the heartbeat manually**

```bash
git push origin feat/aicr-bot-slack-cadence
gh workflow run daily-status.yaml --ref feat/aicr-bot-slack-cadence
gh run watch
```

Expected: workflow succeeds. Open the configured Slack channel; verify a parent message exists with text starting with `Daily AICR-bot status — <today UTC>`.

- [ ] **Step 3: Trigger `vuln-scan-images.yaml` manually**

```bash
gh workflow run vuln-scan-images.yaml --ref feat/aicr-bot-slack-cadence
gh run watch
```

Expected: workflow succeeds. In Slack, the per-image counts message appears as a thread reply under the heartbeat parent (not as a top-level post).

- [ ] **Step 4: Trigger `issue-report.yaml` manually**

```bash
gh workflow run issue-report.yaml --ref feat/aicr-bot-slack-cadence
gh run watch
```

Expected: workflow succeeds. The issue digest appears as a thread reply under the heartbeat parent.

- [ ] **Step 5: Trigger `weekly-summary.yaml` manually**

```bash
gh workflow run weekly-summary.yaml --ref feat/aicr-bot-slack-cadence
gh run watch
```

Expected: workflow succeeds. A top-level post appears in the channel with the three sections (Releases / Issue movement / Latest vuln scan). Not threaded.

- [ ] **Step 6 (optional): Test the on-tag path on a fork**

If you want to verify `on-tag.yaml` end-to-end before merge:

```bash
# On a fork only — never on the production repo:
git tag v0.0.0-thread-test
git push origin v0.0.0-thread-test
gh run watch
```

Expected: `notify` job succeeds. On a weekday, the release post appears as a thread reply; on a weekend, top-level. Clean up after:

```bash
gh release delete v0.0.0-thread-test --yes
git push --delete origin v0.0.0-thread-test
git tag -d v0.0.0-thread-test
```

- [ ] **Step 7: Open the PR**

```bash
gh pr create --draft --base main --head feat/aicr-bot-slack-cadence \
  --title "feat(ci): consolidate AICR-Bot Slack posts into daily thread + weekly digest" \
  --body-file - <<'PR_BODY'
## Summary

Replaces three independent top-level Slack messages per day with a two-cadence model: weekday daily heartbeat-and-thread, weekend weekly top-level digest. Migrates from Slack Incoming Webhooks (cannot thread) to the Slack Web API via a new shared composite action.

## Operational prerequisite (must be complete before merge)

- [ ] Slack App created with scopes `chat:write` and `channels:history` (or `groups:history`)
- [ ] Bot installed in the AICR Slack channel
- [ ] Repo secret `SLACK_BOT_TOKEN` set
- [ ] Repo variable `SLACK_CHANNEL_ID` set

The existing `SLACK_SERVICE` webhook secret stays in place; removed in a follow-up PR after one full week of clean runs.

## Smoke test results (in fork)

- [x] Heartbeat parent posts with marker
- [x] vuln-scan threads under parent
- [x] issue-report threads under parent
- [x] weekly-summary posts top-level with three sections
- [x] on-tag (weekday) threads; on-tag (weekend) top-level

## Spec

`docs/superpowers/specs/2026-04-27-aicr-bot-slack-cadence-design.md`

## Plan

`docs/superpowers/plans/2026-04-27-aicr-bot-slack-cadence.md`
PR_BODY
```

After CI passes, mark the PR ready for review (`gh pr ready <number>`).

---

## Self-Review (post-plan, pre-execution)

This section is the author's check — completed once before executing tasks.

- **Spec coverage:**
  - "Two cadences" → Tasks 2 (daily heartbeat) + 6 (weekly summary) ✓
  - "Mon-Fri only crons" → Tasks 3, 4 ✓
  - "Daily heartbeat 05:00 UTC, marker pattern" → Task 2 + Task 1 (marker is internal to the action) ✓
  - "Weekly summary Sat 14:00 UTC, three sections" → Task 6 ✓
  - "State via conversations.history" → Task 1 ✓
  - "Lazy-create on weekday" → Task 1 (mode=reply path) ✓
  - "Weekend on-tag = top-level" → Task 5 (mode chosen by `date -u +%u`) ✓
  - "Race tolerance (duplicate parent OK)" → Task 1 (no locking attempted; documented in spec) ✓
  - "Webhook → bot API migration; SLACK_SERVICE retained during rollout" → Tasks 3-5 drop the env from migrated jobs only; the secret stays in repo settings ✓
  - "Operational prerequisite (Slack app, scopes, secrets)" → Documented in plan header and PR body template ✓
  - All edge cases in the spec map to the composite action's error handling (Task 1) ✓

- **Placeholder scan:** No "TBD", "TODO", "implement later". Every code block is complete and self-contained. ✓

- **Type / signature consistency:**
  - The composite action exposes inputs `bot-token`, `channel-id`, `body`, `mode` and outputs `parent-ts`, `posted-ts`. All caller workflows (Tasks 2-6) pass exactly these inputs.
  - Mode values are literally one of `heartbeat-only`, `reply`, `top-level` everywhere — no typos.
  - The marker string `"Daily AICR-bot status — $(date -u +%Y-%m-%d)"` is computed in Task 1 and never duplicated in callers (the spec explicitly mandates this).

---

**Plan complete and saved to `docs/superpowers/plans/2026-04-27-aicr-bot-slack-cadence.md`.**

## Execution choice

Two options:

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — Execute tasks in this session using `superpowers:executing-plans`, batch execution with checkpoints.

Tell me which approach you'd like.
