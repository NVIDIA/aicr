# AICR Project Governance

This document describes how the NVIDIA AI Cluster Runtime (AICR) project is
governed: the roles people hold, how decisions are made, and how maintainers
join and leave. It is intentionally lightweight and will grow with the project.
For the current roster and maintainer responsibilities, see
[MAINTAINERS.md](MAINTAINERS.md).

## Roles

AICR uses three roles. They map directly to the project's GitHub teams and to
[`.github/CODEOWNERS`](.github/CODEOWNERS).

### Contributors

Anyone who opens an issue, pull request, or discussion. Contributors follow the
[Code of Conduct](CODE_OF_CONDUCT.md) and sign off their commits under the DCO
(see [CONTRIBUTING.md](CONTRIBUTING.md)). No special access is required to
contribute.

### Code Owners

Trusted contributors (the `@nvidia/aicr-write` team) who review and approve pull
requests for the paths they own. Code ownership is declared per path in
[`.github/CODEOWNERS`](.github/CODEOWNERS), which GitHub uses to request reviews
automatically. Code owners keep their areas healthy and mentor contributors.

### Maintainers

The `@nvidia/aicr-maintainer` team. Maintainers have merge rights, make and
ratify project decisions, manage releases, and own this governance process —
including adding and removing maintainers. Maintainers are also code owners.

## Areas of Ownership

Path-level ownership is authoritative in
[`.github/CODEOWNERS`](.github/CODEOWNERS). At a high level the project is
organized into:

- **Recipes and evidence** — `recipes/**` (maintainer-reviewed; the evidence
  allowlist is the trust root).
- **Core engine** — recipe resolution, bundling, collection, and validation
  (`pkg/**`).
- **CLI and API server** — `cmd/**`, `pkg/cli`, `pkg/server`.
- **Supply chain and CI** — release, attestation, and workflow tooling.
- **Documentation** — `docs/**` and the root project docs.

Maintainers hold cross-cutting responsibility across all areas.

## Decision-Making

AICR decides by **lazy consensus**: a proposal (pull request, issue, or
discussion) is accepted if no maintainer raises a blocking objection within a
reasonable review window — at least five business days for non-trivial changes.
Most day-to-day changes are merged through normal code-owner review under
[`.github/CODEOWNERS`](.github/CODEOWNERS) and the process in
[CONTRIBUTING.md](CONTRIBUTING.md).

When consensus is not reached:

- **Majority vote.** Any maintainer may call a vote. A proposal passes on a
  simple majority of non-emeritus maintainers; quorum is a simple majority of
  that same group.
- **Blocking objection (veto).** A maintainer may block a change by stating a
  concrete technical rationale and an actionable path to resolution. A blocked
  change proceeds only if a two-thirds supermajority of maintainers votes to
  override.
- **Supermajority decisions.** Adding or removing a maintainer, and changes to
  this document, require a two-thirds supermajority of non-emeritus maintainers.

## Tie-Breaking

If a vote is tied, the **lead maintainer** has the casting vote. The lead
maintainer is a maintainer designated by the maintainer team and recorded with
the `@nvidia/aicr-maintainer` team; the role exists to break deadlocks and
carries no additional day-to-day authority. If the lead maintainer is the
subject of, or is recused from, a decision, the remaining maintainers designate
an acting lead for that decision.

## Adding and Removing Maintainers

### Adding

Maintainers are added on merit. The selection criteria and nomination steps are
listed in [MAINTAINERS.md](MAINTAINERS.md#becoming-a-maintainer). A nominee is
added when a two-thirds supermajority of current maintainers approves.

### Removing and stepping down

A maintainer may step down at any time by opening a pull request that moves them
to emeritus. A maintainer may also be removed by a two-thirds supermajority vote
— for sustained inactivity (see [Emeritus](#emeritus)), or for conduct that
violates the [Code of Conduct](CODE_OF_CONDUCT.md). Removal for cause follows the
Code of Conduct enforcement process.

## Emeritus

A maintainer is considered **inactive** after six months with no substantive
contribution, review, or governance participation. Inactive maintainers are
moved to the Emeritus list in [MAINTAINERS.md](MAINTAINERS.md) — voluntarily, or
by a maintainer vote — which removes merge rights and excludes them from quorum
and vote counts. Emeritus maintainers are welcomed back through the normal
onboarding process when they return to active participation.

## Changing This Document

Amendments follow the supermajority rule above: a pull request plus approval from
a two-thirds supermajority of non-emeritus maintainers.

## Maintainers

The current maintainers (the `@nvidia/aicr-maintainer` team), sorted by GitHub
handle. The GitHub team is the authoritative source; this table is a convenience
snapshot.

| GitHub | Name | Headshot |
|---|---|---|
| [@ArangoGutierrez](https://github.com/ArangoGutierrez) | Carlos Arango Gutierrez | <img src="https://avatars.githubusercontent.com/u/15933089?s=48&v=4" width="48" height="48" alt="ArangoGutierrez"/> |
| [@cullenmcdermott](https://github.com/cullenmcdermott) | Cullen McDermott | <img src="https://avatars.githubusercontent.com/u/9535761?s=48&v=4" width="48" height="48" alt="cullenmcdermott"/> |
| [@dims](https://github.com/dims) | Davanum Srinivas | <img src="https://avatars.githubusercontent.com/u/23304?s=48&v=4" width="48" height="48" alt="dims"/> |
| [@lalitadithya](https://github.com/lalitadithya) | Lalit Adithya V | <img src="https://avatars.githubusercontent.com/u/13063810?s=48&v=4" width="48" height="48" alt="lalitadithya"/> |
| [@lockwobr](https://github.com/lockwobr) | Brian Lockwood | <img src="https://avatars.githubusercontent.com/u/1550334?s=48&v=4" width="48" height="48" alt="lockwobr"/> |
| [@mchmarny](https://github.com/mchmarny) | Mark Chmarny — **lead maintainer** | <img src="https://avatars.githubusercontent.com/u/175854?s=48&v=4" width="48" height="48" alt="mchmarny"/> |
| [@njhensley](https://github.com/njhensley) | Nathan Hensley | <img src="https://avatars.githubusercontent.com/u/229213852?s=48&v=4" width="48" height="48" alt="njhensley"/> |
| [@yuanchen8911](https://github.com/yuanchen8911) | Yuan Chen | <img src="https://avatars.githubusercontent.com/u/27247646?s=48&v=4" width="48" height="48" alt="yuanchen8911"/> |
