<!--
Vendored, verbatim, from the source skill this port derives from: Mark Chmarny's
`triage-issues` SKILL.md, as shared in the aicr Slack channel on 2026-07-24
(sha256 2c25368b0f737ce4598a7c897f787702…, 189 lines, 6486 bytes at vendoring).
This copy exists so reviewers and CI can check the port's policy against its
source without access to the author's machine. Do not edit it; it is a record,
not an active skill (no SKILL.md name, so nothing loads it).
-->
---
name: triage-issues
description: |
  Use when the user runs `/triage-issues` or asks to triage, review, or
  clean up a GitHub org-level Projects v2 board (default NVIDIA AICR
  project 248). Identifies P2 issues to promote to P1, Ready items to
  demote to Backlog, and stale issues to close — then applies the
  user-confirmed changes via `gh` CLI. Triggers on backlog hygiene
  before sprint planning, release prep, or board housekeeping.
user_invocable: true
---

# Triage Issues

## Overview

Review a GitHub Projects v2 board, produce a structured recommendation
across three buckets (promote priority, demote status, close), get
explicit user confirmation, then apply the approved changes via `gh`.

**Default board:** `https://github.com/orgs/NVIDIA/projects/248/views/2`
(AICR). Pass `owner/number` or a project URL as the skill arg to triage
a different board.

## Process

### 1. Resolve the project

```bash
# Defaults if no arg: owner=NVIDIA, num=248
gh project view "$num" --owner "$owner" --format json
gh project field-list "$num" --owner "$owner" --format json
```

Capture from `field-list`:
- Project ID (`PVT_*`)
- `Status` field ID (`PVTSSF_*`) and option IDs (Backlog / Ready / In progress / In review / Done)
- `Priority` field ID (`PVTSSF_*`) and option IDs (P0 / P1 / P2)

IDs differ per project — re-fetch every run, never hardcode.

### 2. Pull every item

```bash
gh project item-list "$num" --owner "$owner" --format json --limit 200 \
  > /tmp/claude/project-items.json
```

Verify `jq '.items | length'` matches `items.totalCount` from step 1.

### 3. Slice to active issues

```bash
jq '[.items[]
     | select(.content.type == "Issue")
     | select(.status == "Done" | not)]' \
  /tmp/claude/project-items.json > /tmp/claude/active-issues.json
```

Note: bash escapes `!=` inside `jq`; use `(.status == "Done" | not)`.

### 4. Identify candidates

Read each candidate's title, labels, body excerpt, updatedAt via
`gh issue view <n> -R <owner>/<repo> --json title,labels,state,updatedAt,body`.
Decide using these rules:

**Promote P2 → P1** when an issue is:
- A bug actively being worked on (`In progress`, matches current branch)
- A direct unblocker for other tracked work
- The cross-cutting parent goal of a set of epics that is the next priority
- In review and important to ship soon

**Demote Ready → Backlog** when an issue is:
- Blocked on upstream code, external testbed, or future work ("once X lands")
- An umbrella epic whose child issues are the actionable units
- Self-labeled "Roadmap" / "RFC" / "Proposal"

**Close** when an issue is:
- Superseded by an architectural decision now documented elsewhere
- A tracking epic whose only deliverable is captured by a single child
- A duplicate of a more specific issue

### 5. Present recommendations

Output three tables (number | title | reasoning). **Do not mutate yet.**

### 6. Confirm via `AskUserQuestion`

One multi-select question per bucket. Closures are irreversible — list
each candidate as its own option so the user can accept the subset.

### 7. Apply approved changes

Every triaged issue gets a comment recording the outcome — **including
no-change verdicts**. Board edits and closures are visible, but no-change
triage leaves no trace otherwise; the comment is what tells the assignee
and watchers that the issue was reviewed and the current placement is
intentional. Skip the comment only when a recent triage comment from this
session already exists on the same issue.

**Field updates (priority and status):**

```bash
gh project item-edit \
  --project-id "$PROJECT_ID" \
  --id "$ITEM_ID" \
  --field-id "$FIELD_ID" \
  --single-select-option-id "$OPTION_ID"

gh issue comment "$n" -R "$owner/$repo" --body "Triaged: <P2→P1 / Ready→Backlog>. <1-2 sentences of reasoning>"
```

**Closures (comment first, then close):**

```bash
gh issue comment "$n" -R "$owner/$repo" --body "Closing: <reason>.

<1-2 sentences of context — what changed, where the work lives now,
how to reopen if needed>"

gh issue close "$n" -R "$owner/$repo" --reason "not planned"
```

**No-change triage (still leave a brief ACK):**

```bash
gh issue comment "$n" -R "$owner/$repo" --body "Triaged: confirmed at <Priority> / <Status>. <1 sentence on why current placement is correct>."
```

### 8. Verify

Re-pull items and confirm the new status/priority for every changed
item before reporting completion.

## GraphQL IDs Cheat-Sheet

| Thing | Source |
|---|---|
| Project ID (`PVT_*`) | `gh project view --format json → .id` |
| Field ID (`PVTSSF_*`) | `field-list → .fields[] where .name == "Status" \|\| "Priority"` |
| Option IDs | Same field — `.options[].id` for each (P0/P1/P2, Backlog/Ready/...) |
| **Project item ID (`PVTI_*`)** | `item-list → .items[].id` |

The project item ID is **not** the issue's `node_id`. `item-edit`
needs the `PVTI_*` from `item-list`.

## Confirmation Is Mandatory

**Never apply field changes or close issues without explicit user
confirmation via `AskUserQuestion`.** Board changes are visible
org-wide; closures are hard to reverse. The skill's value is the
analysis; the mutation must be opt-in.

## Common Pitfalls

| Pitfall | Fix |
|---|---|
| Using issue node ID where project item ID is required | `item-edit` needs `PVTI_*` from `item-list` |
| `gh project item-list` defaults to 30 results | Pass `--limit 200`; verify against `items.totalCount` |
| Bash escapes `!=` inside `jq` | Use `select(.status == "X" \| not)` |
| Closing without explanation | Always `gh issue comment` first; the reason flag is too terse |
| Silent no-change triage | Even when nothing changes, post an ACK comment so the assignee knows triage happened |
| Treating epics as Ready work | Epics belong in Backlog; their children are Ready |
| Hardcoding option IDs across boards | Re-fetch via `field-list` every run |
| Promoting every in-progress item to P1 | Stalled in-progress (>30 days no update) may need a different action — surface it but don't auto-promote |

## Closure Comment Template

```
Closing: <superseded by #X / scope captured by #Y / architectural
decision Z documented in <link>>.

<1-2 sentences: what changed under the issue, where the live work is
tracked, how to reopen or file a new issue if scope returns>.
```

## Invocation

```
/triage-issues
# defaults to NVIDIA/248

/triage-issues NVIDIA/123
# owner/number shorthand

/triage-issues https://github.com/orgs/NVIDIA/projects/248/views/2
# explicit URL
```
