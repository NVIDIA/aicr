# Claude Skills

AICR ships a set of **Claude skills** under
[`.claude/skills/`](https://github.com/NVIDIA/aicr/tree/main/.claude/skills).
A skill is a self-contained, model-invocable procedure: a `SKILL.md` with
YAML frontmatter (`name`, `description`) plus any supporting files
(templates, skeletons). When a request matches a skill's description,
Claude Code loads it and follows it directly — so the skills encode the
project's preferred way to do recurring, judgment-heavy tasks instead of
re-deriving the steps each time.

These are repo-scoped: they live in the codebase, are versioned with it,
and are available to any contributor running Claude Code in this working
tree. They complement — they do not replace — the coding rules in
[CLAUDE.md](https://github.com/NVIDIA/aicr/blob/main/.claude/CLAUDE.md).

## Available Skills

| Skill | Use it when you want to... |
|-------|----------------------------|
| [`aicr-analyzing-snapshots`](https://github.com/NVIDIA/aicr/blob/main/.claude/skills/aicr-analyzing-snapshots/SKILL.md) | Analyze a snapshot YAML — cluster identity, provider characteristics, GPU/network topology, node health, software stack — and produce a structured assessment report. |
| [`aicr-auditing-docs`](https://github.com/NVIDIA/aicr/blob/main/.claude/skills/aicr-auditing-docs/SKILL.md) | Audit the Markdown docs for duplication, drift, bloat, and gaps, producing a prioritized findings report (research, not edits) across README, `docs/`, demos, and governance files. |
| [`aicr-creating-guided-demos`](https://github.com/NVIDIA/aicr/blob/main/.claude/skills/aicr-creating-guided-demos/SKILL.md) | Scaffold an interactive guided demo script (`demos/*.sh`) — live or self-paced — using the Frame → Tell → Show → Close narrative pattern. |
| [`aicr-creating-slide-decks`](https://github.com/NVIDIA/aicr/blob/main/.claude/skills/aicr-creating-slide-decks/SKILL.md) | Build a self-contained HTML slide deck (`demos/*.html`) — inline CSS/SVG, no build step — to present or teach a concept full-screen or projected. |
| [`aicr-managing-openvex`](https://github.com/NVIDIA/aicr/blob/main/.claude/skills/aicr-managing-openvex/SKILL.md) | Add, update, or remove CVE/GHSA suppressions in `.openvex.json`, the OpenVEX document consumed by the daily image vulnerability scan. |
| [`aicr-release-notes`](https://github.com/NVIDIA/aicr/blob/main/.claude/skills/aicr-release-notes/SKILL.md) | Draft the human-readable GitHub release-notes summary for an upcoming release by grouping commits since the last tag into thematic highlights. |

## How Skills Are Invoked

Skills are matched against their `description` frontmatter. There are two
paths:

- **Automatic.** When a request matches a skill's triggers, Claude Code
  loads the skill before responding. The `description` field is the
  matcher — it lists the phrases and intents that should activate the
  skill, so write it for recall.
- **Explicit.** A contributor can name a skill directly (for example,
  `/aicr-auditing-docs`) to force its use.

Never read a `SKILL.md` with a plain file read to "follow it" — invoke it
so its supporting files and conventions load as intended.

## Adding a Skill

1. Create `.claude/skills/<skill-name>/SKILL.md` with `name` and
   `description` frontmatter. The `name` must match the directory.
2. Write the `description` for matching: enumerate the triggers (phrases,
   file paths, intents) that should activate it. This is the single most
   important field — a skill that never matches is dead weight.
3. Keep the body a procedure, not prose: when to use, when *not* to use,
   and the concrete steps. Add supporting files (skeletons, templates)
   alongside `SKILL.md` and reference them by relative path.
4. Add a row to the [Available Skills](#available-skills) table above so
   contributors can discover it.

Skills are design-time tooling for working *on* AICR — they are not part
of the shipped product and do not affect generated artifacts.
