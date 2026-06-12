# Copilot Instructions for NVIDIA AI Cluster Runtime (AICR)

GitHub Copilot should follow the project's canonical coding-agent rules. These
rules — package architecture, required error/context/logging patterns, the
Snapshot → Recipe → Validate → Bundle workflow, "add a component/collector/API
endpoint" guides, commit signing, and PR requirements — live in one place and
are mirrored for tools that read this directory.

Do not duplicate that content here. Read the canonical sources directly:

- **[AGENTS.md](../AGENTS.md)** — canonical agent rules and patterns (CI-synced
  mirror of `.claude/CLAUDE.md`); this is the file Copilot should treat as
  authoritative.
- **[DEVELOPMENT.md](../DEVELOPMENT.md)** — local setup, project architecture,
  Make targets, Tilt/Kind workflows, debugging.
- **[CONTRIBUTING.md](../CONTRIBUTING.md)** — design principles, DCO/sign-off,
  commit signing, and the pull request process.
- **[docs/contributor/index.md](../docs/contributor/index.md)** — detailed system
  architecture and component documentation.

Tool versions and quality thresholds are defined once in
[`.settings.yaml`](../.settings.yaml) (the single source of truth); never
hardcode them in prose.
