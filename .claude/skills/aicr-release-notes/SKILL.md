---
name: aicr-release-notes
description: |
  Use when drafting the human-readable GitHub release notes summary for an
  upcoming AICR release. Triggers on "release notes", "draft release notes",
  "/aicr-release-notes", or any request to summarize commits since the last
  tag into a polished release announcement.
  Runs tools/changelog, groups commits into thematic highlights, mirrors the
  style of the previous release, and writes a Markdown draft to a temp file
  for hand-editing before publishing.
---

# AICR Release Notes Draft

Generates the user-facing release notes summary that goes into the GitHub
Releases body (e.g. <https://github.com/NVIDIA/aicr/releases/tag/v0.13.0>),
NOT the raw `tools/changelog` commit list that already appears below the
summary. Output is a draft — the author hand-edits before publishing.

## When to Use

- User asks to draft release notes, release summary, or release announcement
- User invokes `/aicr-release-notes`
- A tag is about to be cut and the maintainer needs the highlights paragraph

Do NOT use this skill to publish a release, push a tag, or edit
`CHANGELOG.md`. It only writes a Markdown draft to a temp file.

## Inputs

`tools/changelog` is the single source of truth for:

- Which tag range is being summarized (it picks the latest stable tag
  and prints `[MSG] Changes since vX.Y.Z` to stderr).
- The set of commits to consider.
- The author handle for every commit (already rendered as
  `by [@handle](https://github.com/handle)` at the end of each line).

Do NOT re-derive any of this with separate `git log`, `gh api`, or
`gh pr list` calls. If `tools/changelog` doesn't surface it, it
doesn't belong in the summary.

No optional input is needed. Do not ask for or guess the target tag —
the filename is fixed (see Step 5) and the body never names the new
tag (GitHub renders the tag in the release header).

## Procedure

### Step 1 — Gather raw material

Run in parallel:

```bash
# Commit list since last stable tag — single source of truth
tools/changelog

# Previous release body for style mirroring (use the tag from
# tools/changelog's "[MSG] Changes since vX.Y.Z" stderr line)
gh release view <previous-tag> --json body --jq '.body'
```

If `tools/changelog` errors ("No release tags found", empty output),
stop and ask the user how to proceed — do not invent a range.

### Step 2 — Classify commits into themes

Read every line of `tools/changelog` output. Group by **user-visible
impact**, NOT by conventional-commit scope. The goal is a release-notes
narrative, not a mirror of `git log`. Useful theme buckets, in rough
priority order:

1. **Headline feature** — the single most significant new capability
   (often a new command, a new deployer, a new contract). Open paragraph
   should name 3–4 of these inline as bolded phrases.
2. **New deployer / output target** — bundler additions, new packaging.
3. **Recipes & overlays** — new accelerator/service/intent combinations,
   new mixins. Use a bulleted sub-list when there are 3+.
4. **Validation / evidence / supply chain** — anything that strengthens
   trust: BOM, SBOM, signing, evidence verification, conformance.
5. **Docs / DX** — new doc site features, CLI ergonomics, config
   unification.
6. **Other improvements** — collect leftover user-visible wins.

**Exclude from the narrative** (they still appear in the raw changelog
below the summary on the GitHub release page):

- `deps:` bumps and Renovate/Dependabot lines
- CI plumbing that doesn't change developer experience
- Pure refactors with no user-visible effect UNLESS the cumulative effect
  is a public API surface change worth flagging (e.g. "Per-Builder
  DataProvider isolation" got a mention because it's a contract change
  for embedders of `pkg/client/v1` / `pkg/aicr`)
- Test-only changes
- Doc-style fixups

### Step 3 — Draft the Markdown

Match the exact structure of the previous release. Required sections, in
order:

1. **Opening paragraph** — one sentence. "This release focuses on
   …, …, …, and …." Each major theme is `**bolded**` inline. No
   heading above it.
2. **`### Highlights`** — heading exactly as written.
3. **`**Theme Name**` blocks** — each starts with bolded title, em dash
   (` — `, with spaces), then 1–3 sentences OR a bulleted sub-list.
   Use sub-lists when enumerating 3+ concrete items (e.g. recipes added).
4. **Closing credits line** — `***Thanks to*** @user1, @user2, …, and
   @mchmarny.` Alphabetical (case-insensitive) by handle, with
   `@mchmarny` moved to the final position preceded by `and `.

**Style rules** drawn from prior releases:

- Issue/PR references use `[NVIDIA/aicr#NNN](https://github.com/NVIDIA/aicr/issues/NNN)`
  form, NOT a bare `#NNN`.
- External product links use full URLs in markdown
  (`[docs.nvidia.com/aicr](https://docs.nvidia.com/aicr)`).
- Backtick CLI commands: `` `aicr validate` ``, `` `aicr evidence verify` ``.
- Use em dashes (` — `) not hyphens for the inline definition pattern.
- No emoji. No "What's Changed" heading. No version-comparison link
  (GitHub adds those automatically).
- Keep total length comparable to the previous release (~250–400 words
  in the summary, not counting the auto-appended changelog).

### Step 4 — Build the contributor list

The thanks line comes **entirely** from the `by [@handle](...)`
annotations already present in `tools/changelog` output. Extract every
unique `@handle` with a simple grep/awk over the changelog text:

```bash
tools/changelog 2>/dev/null \
  | grep -oE 'by \[@[^]]+\]' \
  | sed -E 's/by \[@//; s/\]$//' \
  | sort -uf \
  | grep -viE '\[bot$'
```

Note the `[^]]+` capture stops at the FIRST `]`, so handles like
`dependabot[bot]` come out as `dependabot[bot` (no trailing `]`). The
final `grep -viE '\[bot$'` accounts for this — do NOT change it to
`'\[bot\]$'` or bots will leak into the thanks line.

Then:

- Drop any handle ending in `[bot` after extraction (bot accounts:
  `dependabot[bot]`, `github-actions[bot]`, `renovate[bot]`,
  `copy-pr-bot`, etc.).
- Sort alphabetically (case-insensitive).
- Move `mchmarny` to the final slot preceded by `and `.
- Do NOT link the @-mentions in the output — GitHub auto-links them.

### Step 5 — Write the draft

Write to `$TMPDIR/aicr-release-notes.md` — fixed filename, no version
suffix. Do NOT write under the repo tree — this is a hand-edit draft,
not a checked-in artifact. Overwrite any prior draft at that path.

**Append an "Unresolved questions for hand-edit" section at the bottom
of the file**, separated from the credits line by a horizontal rule
(`---`). The author edits the file directly, so questions belong in
the file, not in chat. Typical content:

- Calls the author should make about emphasis (e.g., "should the X
  bump be promoted to a highlight?")
- Things to verify before publishing (issue references, feature
  completeness, prior-release framing)
- Anything the skill chose to omit that the author may want back

Format the section as:

```markdown
---

## Unresolved questions for hand-edit

1. **<topic>** — <one-or-two sentence note explaining the call to make>
2. **<topic>** — <…>
```

This section is for the author's eyes only and gets deleted before
publishing.

After writing, print to chat:

1. A ready-to-run macOS clipboard command on its own line in a fenced
   bash block: `pbcopy < <absolute-path>`. The user copies that line,
   runs it, and pastes into the GitHub release form. Do not print the
   bare path on a separate line — the `pbcopy` form is the path.
2. A one-line summary of which themes the draft surfaced (so the user
   can quickly tell if you missed something).

Do NOT cat the full draft back into chat — the user will open the file
directly. Do NOT print the unresolved questions separately — they are
already in the file.

## Output Format Reference

The structure to mirror, with placeholders:

```markdown
This release focuses on <theme-1-bolded>, <theme-2-bolded>, <theme-3-bolded>, and <theme-4-bolded>.

### Highlights

**<Theme 1 Title>** — <1–3 sentence narrative explaining what shipped and
why it matters to a user. Reference commands in backticks. Link issues as
[NVIDIA/aicr#NNN](https://github.com/NVIDIA/aicr/issues/NNN).>

**<Theme 2 Title>** — <narrative>

**<Theme with enumerated items>**

* <Concrete item 1>
* <Concrete item 2>
* <Concrete item 3>

**Other Improvements**

* <Leftover user-visible win 1>
* <Leftover user-visible win 2>

**<Supply Chain or Trust Theme>** — <narrative>

***Thanks to*** @alice, @bob, @carol, and @mchmarny.
```

## Failure Modes

- **`tools/changelog` is empty** — likely the tag already exists or
  `LAST_TAG..HEAD` is empty. Ask the user which range to summarize.
- **`gh release view` fails** — the previous tag may not have a release
  yet. Fall back to reading the README's recent-releases section or
  ask the user to point at a reference release for style.
- **Repo state has uncommitted changes** — fine, `tools/changelog` only
  reads git history. No need to stash.

## What This Skill Does NOT Do

- Does not run `git tag` or push tags
- Does not create the GitHub release
- Does not edit `CHANGELOG.md` or any in-repo file
- Does not re-derive commit ranges, author handles, or commit lists
  outside of `tools/changelog` output
