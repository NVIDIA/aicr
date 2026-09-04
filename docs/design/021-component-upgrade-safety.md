# ADR-021: Component Upgrade Safety

## Status

**Proposed** — 2026-08-21.

Implementation status: [Decision 7](#decision-7-generated-wrappers-carry-two-versions) shipped in [#2526](https://github.com/NVIDIA/aicr/issues/2526); generated wrappers now stamp both versions. Everything else below remains proposed. The problem statements are left as written at proposal time — they describe the state the ADR was reasoning about, not the state of `main`.

Numbering note: 020 is double-claimed at time of writing. Branch `docs/adr-020-resolution-policy` carries `020-recipe-resolution-policy.md`, and [#2334](https://github.com/NVIDIA/aicr/pull/2334) proposes ADR-020 for snapshot agent run isolation. Renumber at merge if 021 is also taken.

Builds on the registry-declared component facts established by `ownsCRDs` ([#2264](https://github.com/NVIDIA/aicr/issues/2264)) and the uniform local-chart bundle layout in `pkg/bundler/deployer/localformat`. It does not change recipe resolution or the deployer contract. It does change bundle layout in two bounded ways: [Decision 4](#decision-4-migration-content-ships-as-an-adjacent-generated-release) adds an optional `-premigrate` folder, and [Decision 7](#decision-7-generated-wrappers-carry-two-versions) adds fields to generated wrapper `Chart.yaml`.

## Problem

On 2026-08-24, [#2333](https://github.com/NVIDIA/aicr/pull/2333) moved nvsentinel from `v1.9.0` to `v1.20.0`, skipping eleven minor versions. The stated motivation was performance improvements. It was typed `Build/CI/tooling`, the breaking-change box was left unchecked, and the description said nothing about existing clusters, upgrade paths, or migration.

Nothing about that is unusual or careless. It is what a pin bump looks like.

**But no one claimed the upgrade was safe, because nothing asks.** A pin says what a *new* deployment gets. Whether a running v1.9.0 cluster can reach v1.20.0 was never asserted, and UAT could not have answered it either: every lane is `provision → CUJ → teardown`, standing up a fresh cluster and destroying it, so no lane has ever exercised an upgrade. The eleven-version jump may well be perfectly safe. Nobody recorded an answer, and nothing in the repository would tell you.

That is the gap. AICR pins a chart version, generates a bundle, and the operator applies it. Nothing in the artifact says whether the transition from the version they are running to the one they are about to install is safe, requires work, or is unsupported. The knowledge exists in upstream release notes, in a maintainer's head, in a GitHub issue. None of it is machine-readable, and none of it is attached to the artifact that encodes the version change.

The failure mode is silent. An operator regenerates a bundle after a pin bump, applies it, and discovers the breaking change as an outage.

## In Brief

Every move of a component's pinned version gets a **transition record**: a small YAML file beside the component saying whether going between two version ranges is safe, needs manual steps, or is unsupported, and what those steps are for each deployer.

```text
   pin bump    ──▶   transition record   ──▶   aicr upgrade-check   ──▶   verdict + steps
                                                                          rendered for the
 whoever moved       authored by the           compares two artifacts,    deployer you chose
 the pin             same person               or a cluster and one
```

**Whoever moves the pin writes the record**, and that is a translation rather than new work: bumping a component already means testing it and reading its migration notes. A CI gate fails when a pinned version has no record covering it, so the matrix cannot quietly fall behind.

**AICR does not perform migrations.** It records them, renders them into the bundle for the deployer you chose, and declines to claim more than it can back: `safe` is exercised by UAT, `manual` rests on the authoring process, and anything it cannot compare says so rather than staying quiet.

## Goals

- Make "is this version transition safe?" a machine-readable question with a machine-readable answer, attached to the component that owns it.
- Answer it both offline, from artifacts alone, and online, against a running cluster.
- Fail closed by default, so an unassessed major transition stops a pipeline rather than passing silently.
- Cover downgrade as well as upgrade, without building a second mechanism.
- Make a `safe` verdict something that is *tested*, not merely asserted.

## Non-Goals

- The `aicr` CLI never mutates a cluster. Migration content it generates is applied by the deployer, like every other resource in a bundle.
- AICR does not inject migration hooks into any chart, upstream or generated. Content for components AICR owns ships as a separate adjacent release; see [Decision 4](#decision-4-migration-content-ships-as-an-adjacent-generated-release).
- AICR does not orchestrate upgrades, drive rollout, or roll back on failure. `helm upgrade` and `helm rollback` remain the mechanism.
- No new in-cluster preflight assertion format. See [Alternatives Considered](#alternatives-considered).
- Validating `recipes/components/*/values.yaml` against each chart's schema is out of scope. It addresses a real silent-misconfiguration gap, but belongs on the PR that bumps the pin and changes the values, not at `upgrade-check` time (per review on #2343).

## Context

### What Helm already covers

Most component upgrades should need nothing from AICR. `helm upgrade` rolls new manifests, and a chart that needs a migration step can ship its own `pre-upgrade` or `post-upgrade` hook. When a component owner automates their migration correctly, AICR gets it for free by bumping the pin.

**How often that holds in this registry is unmeasured.** The boundary in [Decision 1](#decision-1-boundary) rests on it, so it deserves measurement rather than assumption. See [Open Questions](#open-questions).

This matters for scoping: an AICR-level hook mechanism would duplicate a facility that already exists one layer down, authored by people who know the migration better than AICR does.

### What Helm does not cover

- **CRDs.** Helm installs a chart's `crds/` directory on first install and never touches it again. Flux's helm-controller inherits this via its `spec.upgrade.crds: Skip` default. A chart bump whose CRDs changed otherwise runs a new controller against the previous schema.
- **AICR's own values.** Nothing validates the keys in `recipes/components/<name>/values.yaml` against the chart's schema. When upstream renames a value, Helm silently ignores the orphaned key and the component comes up with a default the recipe never intended. No chart hook can catch this, because the chart does not know AICR's values file exists.
- **AICR's own contract.** Recipe fields, `--set` keys, profile names, a component being added, removed, or replaced. Uniquely AICR's.
- **Operational prerequisites.** Drain windows, backups, maintenance approval. Not automatable by anyone.

### Existing AICR precedents this builds on

- **`ownsCRDs`** (`pkg/recipe/components.go:129-154`) is already an upgrade-safety mechanism: a registry-declared, per-component, human-audited fact that a deployer consumes to change upgrade behavior. Its doc comment encodes the audit criteria (sole CRD ownership, no webhook conversion strategy). Four standalone `-crds` components in the registry solve the same problem a second way.
- **`healthCheck.assertFile`** references a per-component file outside `registry.yaml`. 43 registry entries reference one today, across 41 distinct check files.
- **Everything in a bundle is a Helm release.** `pkg/bundler/deployer/localformat/doc.go` wraps Kustomize components and raw manifests into generated charts, so `helm list -A` enumerates every component of an AICR-deployed stack under a release name matching its ComponentRef name in `registry.yaml`.

  One limit, load-bearing later: the inventory is complete in *names* but not yet in *versions*. Generated wrappers report a hardcoded `0.1.0` until [Decision 7](#decision-7-generated-wrappers-carry-two-versions) stamps the payload version.
- **Every bundle embeds `recipe.yaml`** (`pkg/bundler/bundler.go:81,2518`), written with deterministic YAML. So a bundle and a recipe are interchangeable inputs to any version comparison.
- **`Masterminds/semver/v3`** is already vendored.

## Decision

Twelve decisions. The first four define the artifact and where it ships; the next four define how it is read and enforced; the last four keep it honest over time.

| | Decision | In one line |
|---|---|---|
| 1 | [Boundary](#decision-1-boundary) | Whoever authors a chart owns its migration content. The remedy for third-party charts is the record, not an upstream PR. |
| 2 | [Transition records](#decision-2-transition-records) | Per-component YAML keyed by semver ranges. Directional, never forward-reaching, and a `safe` verdict must name what verified it. |
| 3 | [Ownership classes](#decision-3-ownership-classes-and-what-aicr-can-see) | Upstream, AICR-authored, and user-authored content are versioned and migrated differently. |
| 4 | [Adjacent migration release](#decision-4-migration-content-ships-as-an-adjacent-generated-release) | Migration content ships as a `-premigrate` release beside the component, never injected into a chart. |
| 5 | [One matcher, three axes](#decision-5-one-matcher-three-independent-axes) | Where `from` comes from, whether a cluster scan runs, and which deployer to render are independent. |
| 6 | [Non-zero exit by default](#decision-6-non-zero-exit-by-default-semver-calibrated) | An opt-in check, but one that exits non-zero so CI can use it. 0.x minors count as breaking. |
| 7 | [Wrappers carry two versions](#decision-7-generated-wrappers-carry-two-versions) | Generated charts stamp the AICR version and the payload version separately. |
| 8 | [Close the `ownsCRDs` gap](#decision-8-close-the-ownscrds-deployer-gap) | CRDs must actually upgrade before a `safe` verdict can mean anything. |
| 9 | [UAT covers upgrade](#decision-9-uat-covers-upgrade-and-rollback) | `safe` is tested; `manual` rests on process; hardware-specific residual falls to evidence. |
| 10 | [Coverage gate](#decision-10-a-coverage-gate-keeps-records-current) | Every pinned version must be covered by a record someone actually edited and substantiated, enforced in CI with a shrinking allowlist. |
| 11 | [Authoring workflow](#decision-11-authoring-is-a-documented-workflow-not-adr-content) | The checklist lives in contributor docs; whoever moves the pin writes the record. |
| 12 | [Pins, not releases](#decision-12-the-matrix-describes-aicrs-pins-not-upstreams-releases) | AICR describes transitions between versions it pinned, and cannot express hops it skipped. |


### Decision 1: Boundary

**Whoever authors a chart owns its migration hooks.**

- **Upstream charts.** Upstream owns them, and AICR never injects a hook into a chart it did not write. Contributing a missing hook upstream is worth doing, but it is not the remedy this ADR depends on, because AICR has no leverage over most of the registry: of 43 components, roughly 29 are third-party. **The remedy is the record.** A transition record documents a cert-manager migration exactly as well as a nodewright one, and recording never required owning the chart. Automating it is a bonus; knowing about it is the product.
- **AICR-generated charts.** AICR owns them. `localformat` wraps *both* manifest-only components and Kustomize components into generated `KindLocalHelm` charts (`doc.go:48`; `kustomize build` output becomes `templates/manifest.yaml`). For those AICR is the chart author and there is no layer below to delegate to, so migrating that content is AICR's responsibility. How that content ships is [Decision 4](#decision-4-migration-content-ships-as-an-adjacent-generated-release); this decision fixes only who owns it.

  Ownership of the *content* varies independently, and Kustomize is not one case but two. `writer.go:525-531` builds from `repo//path?ref=tag` when `Repository` is set, and from a plain local filesystem path when it is not. So a git-sourced kustomization carries upstream content, while a local-path one is AICR-authored throughout, exactly like a manifest-only component. [Decision 4](#decision-4-migration-content-ships-as-an-adjacent-generated-release) makes the distinction moot for hooks by never injecting into any generated chart, but it matters for versioning; see [Decision 3](#decision-3-ownership-classes-and-what-aicr-can-see).

Ownership is the right axis because the usual argument against AICR rendering hooks (the chart author knows the migration better, and Helm already gives them the facility) has no force when AICR is the chart author. Drawn this way the line is narrow: it forbids by construction the genuinely risky case, injecting into third-party charts, while permitting the case where AICR already controls every byte.

This is not a niche carve-out. 23 registry components ship AICR-authored manifests, and 11 are manifest-only (`defaultRepository: ""`).

AICR owns three things no chart can:

- Transition facts it has audited across the stack it composes.
- Breaking changes in AICR's own contract, including the content of every AICR-authored manifest.
- The ability to compute an upgrade matrix across a whole composed stack, which no single component can see.

### Decision 2: Transition records

A per-component file, referenced from the registry, mirroring the existing `healthCheck.assertFile` pattern:

```yaml
# recipes/registry.yaml
- name: nodewright-operator
  healthCheck:
    assertFile: checks/nodewright-operator/health-check.yaml
  upgrades:
    file: upgrades/nodewright-operator.yaml
```

Records are keyed by semver ranges, not explicit version pairs, so they do not go stale on every patch release. The [Example](#example) carries a full record, the real nodewright `skyhook.nvidia.com` to `nodewright.nvidia.com` rename, alongside the output it produces.

**Five verdicts.** Three would not be enough to be honest, and the last two fail for different reasons with different remedies:

| Verdict | Meaning |
|---|---|
| `safe` | In-place upgrade works. Nothing to do. |
| `manual` | Upgrade works only after the listed steps. |
| `blocked` | Direct transition unsupported. Either the jump spans more than one block, or it needs an uninstall and reinstall. |
| `unknown` | No record matches this transition. **Not an assertion of safety.** |
| `unversioned` | The two sides cannot be compared at all. **Not an assertion of safety.** |

**A transition may also carry `hooks`.** Steps describe what a human does; `hooks` reference AICR-authored migration manifests that ship as a release beside the component. The field is defined in [Decision 4](#decision-4-migration-content-ships-as-an-adjacent-generated-release), which owns its delivery; it is listed here so the transition schema is complete in one place.

**The loader fails closed on an unrecognized `apiVersion`.** Records are stamped `aicr.run/v1beta1`, the target assigned to `ComponentUpgrades` by [ADR-022 §2](022-artifact-maturity-and-deprecation.md). The kind starts at its target rather than on the alpha track: ADR-022 Release N already accepts `aicr.run/v1beta1` for authoring artifacts, so there is no alpha version to emit and later retire. An unreadable or unrecognized record MUST return `ErrCodeInvalidRequest` naming the value found and the value expected. It MUST NOT be skipped, and it MUST NOT degrade to `unknown`.

That last clause matters for the same reason `unknown` and `unversioned` are separate verdicts. "A record exists and I could not read it" is not "no record exists", and collapsing them hides which action closes the gap. A loader that quietly skips a record it cannot parse would reproduce this ADR's own motivating failure, the silent one, inside the tool built to prevent it. The catalog loader used to check `kind` only and never `apiVersion` (see ADR-015 and [#1812](https://github.com/NVIDIA/aicr/issues/1812)); [ADR-022 §8](022-artifact-maturity-and-deprecation.md) closed that, and `recipes/upgrades/*.yaml` sits in the same tree an external `--data` catalog points at, so a record loader must implement the ADR-022 gate rather than needing to opt out of a fail-open one. The obligation is on this still-unimplemented loader; it does not share `provider.go`'s registry gate, so "inherits" would describe the fail-closed policy default, not code reuse.

**Fields are validated against the verdict.** A `manual` or `blocked` record MUST carry at least one step, since both are defined by the work they require; a `safe` record MUST carry none, and MUST carry `verifiedBy`.

**`safe` must name what verified it.** [Decision 9](#decision-9-uat-covers-upgrade-and-rollback) asserts that a verdict is a tested claim, but nothing in the schema made the claim point at the test. `verifiedBy` closes that: a UAT lane, a KWOK run, or an upstream release note that backs the assertion.

This exists to close a hole in [Decision 10](#decision-10-a-coverage-gate-keeps-records-current)'s coverage gate. That gate asserts every pinned version falls inside some record's `to` range, and the cheapest way to satisfy it for all 34 allowlist entries is one blanket record per component: `to: ">=0.0.0"`, `verdict: safe`, no steps. It clears the gate, passes strict mode, and is well-formed by construction, because "carries no steps" is trivially true of a record that says nothing. The block-spanning backstop cannot fire either, since widening is precisely what reduces the block count.

Without `verifiedBy` the gate would measure coverage rather than assessment, and apply pressure toward the one outcome this ADR calls worse than nothing. It is the same dynamic the [Testing Strategy](#testing-strategy) designs around when it refuses to pin verdicts in Go tests, arriving through a different door. Widening a range stays legal and free when it is honest; a blanket `safe` across a component's whole history now has to name what verified it, and there is nothing to name. The same rule applies to a `replaces` block, which in practice is always `manual`: a replacement that claimed to need no operator work would be a rename, and a rename is a chart-identity change on one component rather than a replacement of one by another.

`hooks` are a deliberate exception and remain allowed on `safe`. A verdict describes what the **operator** must do, and a hook is AICR doing the work instead. A transition fully handled by a migration release requires nothing of the operator, so forbidding hooks there would force it to `manual`, failing the gate for something already automated. That is the opposite of what the verdicts are for. `unknown` and `unversioned` are never authored, only computed. The well-formedness check enforces this, so the verdict table above cannot drift from what a record actually says.

`unknown` is a gap in the *data*, fixed by authoring a record. `unversioned` is a gap in the *inputs*, fixed by pinning something comparable. Collapsing them would hide which action closes the gap.

**`unversioned` exists because some refs carry no usable version.** A git-sourced Kustomize component pins `defaultTag`, documented as a git tag, branch, *or commit* (`components.go:209`), and nothing validates which:

| `defaultTag` | Identity | Ordering | Result |
|---|---|---|---|
| Semver tag `v1.2.3` | yes | yes | compared normally |
| Non-semver tag `release-1.4` | yes | no | `unversioned` |
| Commit SHA | yes | no | `unversioned` (direction undecidable) |
| Branch `main` | no | no | `unversioned` |

The branch row is why this needs its own verdict rather than falling through to `unknown`. Two bundles built a month apart from `main` carry identical ref strings while the content underneath may have changed completely. A matcher comparing strings would conclude nothing changed and say nothing, and silence reads as `safe`. That is the fail-open shape directionality is guarded against above, arriving through a different door.

**It also covers this feature's own migration.** Bundles generated before [Decision 7](#decision-7-generated-wrappers-carry-two-versions) ships carry `version: 0.1.0` and no `aicr.run/component-version` annotation, so every generated chart in them reads as the same fictional version. Online mode against such a bundle reports `unversioned` rather than comparing `0.1.0` to `0.1.0` and declaring no change.

**Steps are structured data, never prose.** The same record renders to a CLI table, a bundle README section, or JSON for CI. Documentation is an *output* of the contract, not the source of it. That is what makes the contract automatable.

**Records are directional by construction.** A record's `verdict`, `stepsByDeployer` and `precondition` describe going forward and say nothing about coming back: a record for 0.18 to 0.20 must never supply a verdict or steps for 0.20 to 0.18. Range matching MUST NOT match in reverse. A downgrade with no explicit reverse record resolves to `unknown`, never to `safe`. Matching a forward record in reverse would be exactly the "negative check that passes on an ambiguous condition" anti-pattern in `CLAUDE.md`, and it is easy to write by accident.

**A record defines a block, and a jump may span only one.** A record applies when the source version satisfies `from` *and* the target is at or past the lower bound of `to`, so a record describes a boundary the jump crosses rather than one exact hop:

| Jump | A: `<0.18.0` → `>=0.18.0 <0.20.0` | B: `<0.20.0` → `>=0.20.0` |
|---|---|---|
| 0.17.2 → 0.18.1 | applies | no (target below `0.20.0`) |
| 0.17.2 → 0.20.1 | applies | applies |
| 0.19.0 → 0.20.1 | no (`from` excludes 0.19.0) | applies |

If exactly one record applies, its verdict stands. **If more than one applies, the verdict is `blocked`**, and the report names the first block's `to` range as the stopping point.

Composing the steps of both records would be wrong, not merely cautious. Nodewright's mirror controller exists only in 0.18.x and 0.19.x. A jump straight from 0.17 to 0.20 never runs it, so the `skyhook.nvidia.com` CRD is removed with nothing ever mirrored and the objects are unrecoverable. Listing both records' steps would have implied the jump was fine. Row three is the reason the rule needs no "already migrated" special case: an operator on 0.19.0 has done the rename, and A drops out on its own because `from` stops matching.

**A record may make claims about past versions, never future ones.** A record's `to` upper bound MUST NOT exceed the version currently pinned in `registry.yaml`. Widening *backward* on `from` is free and unbounded; reaching *forward* is not permitted at all.

The asymmetry is the whole idea. An author writing a record has read the migration notes for versions that exist, and can honestly say how far back a claim holds. They cannot have read the notes for a version nobody has released, so a range that reaches forward is asserting something its author could not have checked.

This is what makes [Decision 10](#decision-10-a-coverage-gate-keeps-records-current)'s coverage gate mean something. Without it, one record per component with `to: ">=0.0.0"` satisfies coverage forever, and no later pin bump ever touches a record again. With it, every bump moves the pin past the record's ceiling, so every bump forces an author back into the record at the moment they have the migration notes open. The gate stops measuring whether a record exists and starts measuring whether someone looked.

It costs nothing on the quiet path: extending a block's ceiling from `<=25.3.0` to `<=25.4.0` is a one-character edit. The point is not the edit, it is that the edit happens while the author is deciding whether the bump is safe.

**Together with coverage, this forces a continuous chain.** The gate requires the current pin to be covered, and no record may reach past it, so some record must end exactly at the pin. Extending that record backward, or adding one behind it, is then the only way to keep the chain whole. A gap can only appear if an author deliberately leaves one.

That is mechanically checkable inside the record file alone, with no git history and no external data: sort the transitions by `to`, and require each record's `from` to meet or overlap the previous record's `to`. A hole between them is a version range the component could be running that no record describes, which is precisely what would later surface as `unknown` to whoever is furthest behind.

**Widening a block is an authoring act, not a flag.** To permit a wider jump, widen a block's `from` range backward or author a record covering the full span. Because there is no `skippable` field, there is nothing an author can forget to set, and the failure mode of forgetting is a block rather than a silent data loss.

In practice this is quiet. An ordinary component gets one broad block per major line, `from: ">=25.0 <26.0"` with `to: ">=25.0 <=25.3"` when 25.3 is the pin, and no jump inside that line ever spans. The `to` ceiling tracks the pin and the `from` floor stays put, so a patch bump is a one-character edit to a record that already carries its `verifiedBy`. Blocks multiply only where real boundaries exist, which is exactly where spanning should stop you.

**Steps are grouped by deployer, not filtered per step.** `stepsByDeployer` holds one entry per deployer group, each with its own ordered `steps` list; a group that omits `deployers:` applies to every deployer. This is not cosmetic. In the nodewright migration the step *list itself* differs: under GitOps the rename and the legacy deletion collapse into one atomic commit, because a separate `kubectl delete` fights auto-sync and self-heal. Under imperative Helm they are two distinct steps in a load-bearing order.

Grouping rather than per-step filtering matters because **order is part of the instruction**. A reader of a group sees exactly the sequence they must perform. With a single flat list plus per-step deployer tags, the reader has to filter mentally before the ordering means anything, and the numbering they see depends on that filtering. Rendering the imperative "delete legacy CRs" step to an Argo CD user with a footnote saying not to do it would be worse still.

This is also where AICR adds value no upstream document can. The nodewright guide must hedge across every deployer, explaining Argo behavior to Flux users and vice versa. AICR knows which deployer you chose, so it renders the one path that applies to you.

**Step `id` is unique within its group.** The same logical action may reuse an id across groups because each group is a self-contained sequence; a consumer addresses a step as (deployer, id).

**Groups partition the deployers; they do not overlap.** An explicit group may not claim a deployer another explicit group already claims. A group omitting `deployers` is the remainder, covering exactly those deployers no explicit group claims, so it is "all" only when no explicit group exists. And because a `manual` or `blocked` verdict promises steps, **every deployer must be covered by some group** for those verdicts: a partition with a gap would hand an Argo CD operator a `manual` verdict with no steps, which is the failure deployer grouping exists to prevent. The well-formedness check enforces both.

**Deployer identifiers are the canonical `--deployer` values** from `pkg/bundler/config/config.go`: `helm`, `helmfile`, `argocd`, `argocd-helm`, `flux`. There are five. `localformat` is the internal bundle-layout package every deployer consumes, not a selectable deployer.

**`reason` is separate from `description`.** The description says what to do; the reason says why the order or the shape matters. Deleting Skyhooks before DeploymentPolicies is not stylistic, it is what the admission webhook requires. A step whose reason is missing is a step somebody will eventually "optimize".

**`reversible` is the one exception, and it is deliberately weak.** It is the author's belief about whether coming back will work, recorded opportunistically while writing the forward record, because that is the moment someone has actually read the migration notes. It is **optional, and absent means no claim** rather than an implied yes.

It is **advisory and never affects a verdict.** AICR does not gate a rollback on it and does not test it to the standard it tests upgrades. Claiming otherwise would be claiming knowledge the project does not have: rollback is not the inverse of upgrade, the reverse direction is not systematically exercised, and a boolean written by an author while thinking about the forward path is not evidence about the reverse one.

What it is good for is the thing an operator needs *before* upgrading rather than after: a heads-up that an upgrade may be one-way. Nodewright's is a good example of why the note matters more than the boolean, since it is reversible only until legacy cleanup runs at 24h, after which rollback re-applies every package from scratch. So renderers always print `reversibleNotes` beside the flag and never surface the flag alone.

**Field reference.** The worked example above shows the shape; this is the contract. A component opts in with `upgrades: {file: upgrades/<component>.yaml}` in `registry.yaml`.

| Field | Required | Notes |
|---|---|---|
| `apiVersion` | yes | `aicr.run/v1beta1`, per [ADR-022 §2](022-artifact-maturity-and-deprecation.md). The loader fails closed on anything else. |
| `kind`, `component` | yes | `ComponentUpgrades`; `component` must match the registry entry. |
| `transitions[]` | yes | One or more. |
| `.from`, `.to` | yes | Semver ranges. A record applies when the source satisfies `from` and the target is at or past `to`'s lower bound. `to` MUST NOT reach past the currently pinned version; widen `from` backward instead. |
| `.verdict` | yes | `safe`, `manual`, or `blocked`. `unknown` and `unversioned` are computed, never authored. |
| `.verifiedBy` | for `safe` | What backs the claim: a UAT lane, a KWOK run, or an upstream release note. Required on `safe` so the gate measures assessment rather than coverage. |
| `.summary` | yes | One sentence on what changes. |
| `.precondition` | no | Cluster state that must hold before starting. |
| `.reversible`, `.reversibleNotes` | no | Advisory only; absent means no claim. Notes required when the flag is set. |
| `.stepsByDeployer[]` | for `manual`/`blocked` | One group per deployer set, each with an ordered `steps` list. Forbidden on `safe`. Omitting `deployers` covers the deployers no explicit group claims. |
| `.stepsByDeployer[].steps[]` | yes | `id` unique within the group, `description` required, `reason` optional. |
| `.hooks[]` | no | Allowed on any verdict, including `safe`. `file` under `manifests/migrations/`, `phase` pre- or post-upgrade. See [Decision 4](#decision-4-migration-content-ships-as-an-adjacent-generated-release). |
| `.affectedResources[]` | no | `group` plus `kinds`; drives the at-risk scan in [Decision 3](#decision-3-ownership-classes-and-what-aicr-can-see). |
| `.references[]` | no | Links to upstream migration notes. |
| `replaces` | no | Top-level, not per-transition. Names the component this one supersedes, with `verdict`, `summary` and `stepsByDeployer` carrying the migration. Joins the removed and added rows into one. |

Two things a record deliberately does not express: a component being *added or removed*, which the matcher handles rather than a record ([Decision 5](#decision-5-one-matcher-three-independent-axes)), and cross-component coupling, which is a non-goal ([Decision 3](#decision-3-ownership-classes-and-what-aicr-can-see)).

Like `ownsCRDs`, these records are human-audited assertions. They carry the same strength and the same failure mode: a wrong record grants false confidence. [Decision 9](#decision-9-uat-covers-upgrade-and-rollback) addresses that directly.

### Decision 3: Ownership classes and what AICR can see

A single migration can span three ownership classes with different answers. The nodewright rename spans all three.

| Class | Example | Versioned by | Who migrates it |
|---|---|---|---|
| Upstream chart | `nodewright-operator` | Upstream chart version | `helm upgrade`. AICR still records the transition. |
| AICR-authored content | `nodewright-customizations` | The AICR release | AICR maintainers rewrite the manifests; users get it by regenerating. |
| User-authored resources | A user's own `Skyhook` CRs | Not AICR's at all | Only the user. AICR cannot fix them. |

**AICR-authored content is keyed on the AICR version.** `nodewright-customizations` is manifest-only (`defaultRepository: ""`, no `defaultVersion`) and its content is five `Skyhook` CRs AICR authors itself, so there is no upstream version to compare and the matcher would have nothing to match on. Its transitions key on AICR version ranges instead, which is what [Decision 7](#decision-7-generated-wrappers-carry-two-versions) already stamps into the wrapper. The same applies to local-path Kustomize components, since `writer.go:516-519` rejects a `Tag` without a `Repository`. Without this rule every manifest-only component is invisible to the matcher.

**Helm handles most of this class for free, with one hazard it cannot.** A rename drops the old resource from the generated chart and adds the new one, so `helm upgrade` prunes and creates with no hook at all. The exception is adoption: nodewright's mirror controller *pre-creates* the `NodeWright` object, so Helm tries to create a resource that already exists without release ownership and fails with an ownership-metadata error. Helm 3 adopts only objects already carrying `app.kubernetes.io/managed-by: Helm` and matching `meta.helm.sh/release-*` annotations; the mirror stamps `nodewright.nvidia.com/mirrored-from` instead.

Declarative metadata cannot reach that. `helm.sh/resource-policy: keep` and sync-wave ordering do not annotate a live object, so adoption requires writing annotations onto an existing resource, which is inherently mutating. This is the concrete case AICR must own under [Decision 1](#decision-1-boundary), shipping as an adjacent release under [Decision 4](#decision-4-migration-content-ships-as-an-adjacent-generated-release). Any such Job needs a digest-pinned image and a BOM entry like every other image.

**Cross-component effects are described in steps, not modelled.** A `nodewright-operator` version change forces a `nodewright-customizations` content change, and the obvious move is a schema field naming affected components. This ADR deliberately does not do that.

The coupling is real but the trigger is rare: it takes an API-group rename or equivalent to make one component's version move rewrite another's content. Modelling it would cost a record field, a rule for which side owns the shared verdict, and a way to tell genuine coupling from ordinary dependency, since `dependencyRefs` already carries 278 edges across the tree and almost none of them imply a migration. That is a lot of machinery aimed at something that happens approximately never, and it would misfire on every unrelated edge in between.

A step describes it perfectly well: "the NodeWright CRs shipped by `nodewright-customizations` are rewritten in this release; regenerate your bundle to pick them up." The affected component's own record carries its own transition as usual. What is lost is the report grouping two rows as one migration, which is presentation rather than correctness.

Revisit if a second unrelated case appears. One occurrence is an anecdote.

**Class 3 is the highest-severity case, and only online mode can see it.** If a user authored their own `Skyhook` CRs outside AICR, the rename ships, their objects are untouched, and the 0.20.0 CRD removal cascade-deletes them. A record declares the API groups and kinds it affects (`affectedResources`, see the field table in [Decision 2](#decision-2-transition-records)), and in online mode the check lists matching objects carrying no AICR or Helm ownership and reports them as at-risk.

That scan is a read-only list, not a new assertion format, and it is **advisory**: it warns rather than failing, because AICR blocking an upgrade over resources it does not own is a claim it has not earned. Offline mode cannot answer the question at all, and says so rather than staying silent, since silence here reads as safety.

### Decision 4: Migration content ships as an adjacent generated release

[Decision 1](#decision-1-boundary) makes AICR responsible for migrating content in charts it generates, but not for how that content reaches a cluster. The obvious answer fails: the wrapper chart is synthesized at bundle time from `manifestFiles`, so there is no checked-in `templates/` to add a hook to, and Kustomize components have the same shape.

Content lives beside the component's manifests; the bundler emits it as its own chart folder, ordered immediately before the component it serves, and a record points at it via `hooks`.

```text
recipes/components/nodewright-customizations/   bundle output
  manifests/                                    006-nodewright-operator/
    tuning.yaml                                 007-nodewright-customizations-premigrate/
    migrations/                                 008-nodewright-customizations/
      adopt-mirrored-crs.yaml
```

**This mirrors an injection the bundler already performs.** `localformat` emits `(NNN+1)-<name>-post/` when a component carries both an upstream chart and raw manifests. `-premigrate` is the same move in the other direction, so the layout gains no new concept.

Three properties follow. It is **uniform across component kinds**, because the migration release does not depend on whether the component itself has a generated chart, so Decision 1's rule holds with no special case: AICR never touches an upstream chart, it emits a release *beside* it. **Ordering is folder order**, not Helm hook phase semantics that would have to behave identically across all five deployers. And **it is an ordinary release**, visible in `helm list`, uninstallable, and covered by the same checksum, BOM, and signing paths as every other folder. Helm hook annotations still work *within* the folder for finer ordering, since Flux's helm-controller runs them and Argo CD translates `helm.sh/hook` into its own semantics.

One caveat on uniformity: it is reasoned, not demonstrated, for Kustomize. That path is implemented (`localformat/kustomize.go`, in-process krusty) but **the registry has zero Kustomize components**, so treat that limb as a design claim until one exists.

**Rollback ordering is undefined, deliberately.** The obvious question is whether the `-premigrate` release rolls back with the component and in what order. This ADR does not answer it, and that is consistent rather than an omission: AICR does not orchestrate rollback at all (`helm rollback` is the mechanism), records are directional so a forward verdict and its steps never apply in reverse, and `reversible` is advisory precisely because the project does not test the reverse direction to the standard it tests upgrades.

Defining an ordering guarantee here would be the one place the ADR promised rollback behaviour it has declined to promise everywhere else.

It is also frequently unanswerable. The migration this mechanism exists for writes ownership annotations onto live objects so Helm can adopt them, and that has no meaningful inverse: removing the annotations would break the release that now owns them. A migration with no inverse cannot have a correct rollback ordering, only a chosen one.

What holds instead is what the layout already gives: `-premigrate` is an ordinary release, so `helm rollback` works on it exactly as on any other, and the operator decides whether that is wanted. Where the answer matters for a specific migration, the record's `reversibleNotes` is the place to say so.

**The location is load-bearing.** [Decision 3](#decision-3-ownership-classes-and-what-aicr-can-see) requires a migration Job image to be digest-pinned under ADR-006 and to appear in the BOM. A sibling `migrations/` directory would have been invisible to both: `tools/bom/main.go:355` walks only `.../manifests`, and the pin test at `recipes/manifest_images_test.go:45` skips paths without `/manifests/`. The requirement would have been stated and then quietly unenforceable.

Nesting under `manifests/` closes that with no tooling change, because the BOM walk is recursive and the pin test matches `/manifests/` as a substring. It also cannot leak into a deployment: the bundler ships only files named in `manifestFiles` and `preManifestFiles`, so a migration manifest is inert until a record's `hooks` references it.

### Decision 5: One matcher, three independent axes

One matcher runs over a `component -> version` table per side. Three separate inputs decide how those tables are built and how the result is rendered. They are independent, which is why "offline mode" and "online mode" are not the right framing.

**Where the `from` table comes from.** `--from <recipe|bundle|cluster>`. Artifacts are read with no cluster access, which is the CI and GitOps path and the whole feature for anyone who keeps recipes in git. `cluster` reads installed release inventory through the Helm SDK, keyed on release name matching component name. This is authoritative for *which release version is installed*, including when that has drifted from the artifact in git. It is **not** a live view of cluster resources: Helm records what it last applied, so a hand-edited resource leaves the release metadata unchanged. `--to` is always an artifact, since there is nothing to upgrade *to* in a cluster.

**`--to` is optional when `--from` is a bundle or recipe.** Omitting it re-resolves that artifact's own embedded criteria against the running binary's registry, synthesizing the target from AICR's current pins. This exists because it answers a different question from the one the two-argument form answers. `--from X --to Y` asks "is this specific move safe?" and presumes the operator already worked out where they are going. What an operator actually holds is an old bundle and the question **"am I behind, and does catching up hurt?"** The single-argument form asks that directly, and it is cheap because a bundle already embeds `recipe.yaml`, which embeds the criteria it was resolved from. Because every bundle embeds a deterministic `recipe.yaml`, the recipe and bundle forms share one code path.

**Whether a cluster scan runs.** The at-risk scan for unmanaged resources ([Decision 3](#decision-3-ownership-classes-and-what-aicr-can-see)) needs a cluster no matter where the `from` table came from, so it is its own axis rather than a property of `--from`. It is implied by `--from cluster` and available alongside artifact comparison via `--scan-cluster`. Comparing two bundles while scanning a live cluster for unmanaged `Skyhook` objects is a legitimate combination, and the two-mode framing had no name for it.

**The report states the span a verdict covers.** A record's `to` is a range, so `safe` may cover one patch or eleven minors, and the range expression buries that. The check renders it explicitly ("safe across 11 minors"), which makes [Decision 12](#decision-12-the-matrix-describes-aicrs-pins-not-upstreams-releases)'s point that jump size scales the claim operational rather than advisory: a reviewer sees the width without decoding a semver constraint.

**The component set can change, and the three cases differ.** Comparing `component -> version` tables means a component may appear on one side only, which is not a version transition at all.

| Case | In `from` | In `to` | What the check does |
|---|---|---|---|
| Added | no | yes | Reports it. Nothing to do: a new component is simply installed. |
| Removed | yes | no | Reports it, and says the component **stays installed**. AICR does not uninstall it. |
| Replaced | as removed | as added | Joined into a single row when the new component declares `replaces`. |

Removal is reported rather than acted on because AICR dropping a component from a recipe is a statement about what AICR now ships, not an instruction to tear down a running workload. Uninstalling on the operator's behalf, from a signal that weak, is not a decision the tool should make. Reporting it leaves the operator to decide.

**Replacement is declared by the new component.** The matcher cannot derive it: nothing in the tables distinguishes "A was replaced by B" from "A went away and B arrived". So the incoming component's record says so, and the report joins what would otherwise be two unrelated rows.

```yaml
component: agentgateway
replaces:
  component: kgateway
  verdict: manual
  summary: >-
    kgateway is superseded by agentgateway for v2.2 inference routing.
  stepsByDeployer:
    - steps:
        - id: port-route-resources
          description: >-
            Re-author kgateway route resources as agentgateway equivalents.
            Field names and defaults differ; this is not a rename.
          reason: >-
            Nothing migrates them automatically. The old resources keep
            working against the old controller until you remove it.
        - id: retire-kgateway
          description: >-
            Once agentgateway is serving traffic, uninstall the kgateway
            release. AICR leaves it installed and will not remove it.
```

**The report says the work is yours.** A replacement is not a version bump the tool absorbs, so it renders as one row naming both components and a verdict that stops a strict run:

```text
COMPONENT                    FROM        TO          VERDICT   NOTES
agentgateway                 kgateway    v1.3.1      manual    replaces kgateway, 2 steps

agentgateway  replaces kgateway  (manual)
  kgateway is superseded by agentgateway for v2.2 inference routing.

  STEPS
    1. port-route-resources
       Re-author kgateway route resources as agentgateway equivalents.
       Field names and defaults differ; this is not a rename.
       Why: nothing migrates them automatically. The old resources keep
       working against the old controller until you remove it.
    2. retire-kgateway
       Once agentgateway is serving traffic, uninstall the kgateway
       release. AICR leaves it installed and will not remove it.
```

The `FROM` column carries the outgoing component's name rather than a version, because there is no shared version line to compare. That is the honest rendering: the two are different software, and the operator is being told to do a migration, not to accept an upgrade.

This is deliberately one-directional and names a single component, which is why it is admitted where cross-component coupling ([Decision 3](#decision-3-ownership-classes-and-what-aicr-can-see)) is not. There is no graph to walk, nothing to infer from the 278 `dependencyRefs` edges, and no question of which side owns the verdict: the arriving component does, because it is the one whose record the operator is reading when the change lands.

It reuses the transition vocabulary rather than inventing one, so `verdict`, `summary` and `stepsByDeployer` mean what they mean everywhere else. Replacement usually carries real migration work, which is why it typically resolves to `manual`. `kgateway` to `agentgateway` ([#871](https://github.com/NVIDIA/aicr/pull/871)) is the one occurrence so far, and without the declaration it would have reported as an unexplained removal beside an unexplained addition.

**Deployer context.** Steps are deployer-scoped ([Decision 2](#decision-2-transition-records)), a bundle records the deployer it was built with, and a recipe does not. The check infers the deployer from `--to` when it is a bundle and otherwise **requires `--deployer`**. It does not guess, and it does not render every path: showing an Argo CD operator the imperative "delete legacy CRs" step is the failure deployer-scoping exists to prevent, so a silent default would reintroduce it. Recipe-to-recipe in CI therefore passes `--deployer` explicitly, which the pipeline already knows because it passes the same value to `aicr bundle`.

Output:

```text
COMPONENT              FROM      TO        VERDICT   NOTES
gpu-operator           v25.3.0   v25.3.2   safe      patch, recorded
nodewright-operator    0.17.2    0.18.1    manual    1 step, reversible (24h)
nfd                    0.18.0    0.19.0    unknown   no record (minor)
kueue                  0.13.0    0.11.0    unknown   downgrade, no reverse record
```

Rollback needs no separate command. `--from cluster --to <older-recipe>` computes the reverse transition and does a lookup with no new machinery.

Online mode reads through the Helm SDK. `.settings.yaml:84` already pins `helm: 'v4.2.4'` as a testing tool and `go.mod` has no Helm SDK at all, so `helm.sh/helm/v4` aligns AICR's read path with the CLI the project already ships and tests against.

Shelling out to `helm list -A -o json` is **not** sufficient. It returns chart and app version, which covers upstream charts, but not chart annotations, which is what [Decision 7](#decision-7-generated-wrappers-carry-two-versions) requires for generated wrappers, where the chart version describes the wrapper rather than its payload. Reading annotations needs `helm get metadata` per release, which is N+1 subprocesses and a Helm-CLI output format to track, or the SDK.

Vendoring the SDK is a substantial change on its own and may land as its own PR ahead of this work; sequencing is an [open question](#open-questions).

### Decision 6: Non-zero exit by default, semver-calibrated

**This is a check you choose to run, not a gate you must pass through.** Nothing in `aicr recipe` or `aicr bundle` invokes it, and it cannot be: `bundle` has a recipe and produces a bundle, with no source version, so it cannot compute a transition at all. Running `upgrade-check` is opt-in, which is what review on #2343 correctly observed about its place next to `recipe`, `bundle`, and `query`.

Given that, the exit code exists to make running it *worth* something. A pipeline should be able to consume the result without parsing output, so the command exits non-zero on a transition that needs attention. Informing and erroring are not alternatives here: the matrix and any steps print in full either way, and the exit code is orthogonal to the report.

The check exits non-zero by default. It fails on `manual`, on `blocked`, on `unversioned`, and on `unknown` across a **breaking boundary**. It passes `safe`, and passes `unknown` within a non-breaking boundary.

A breaking boundary is a major bump, **or a minor bump while the major version is 0**. Semver gives no stability guarantee below 1.0, so `0.18 -> 0.19` may break exactly as `1.x -> 2.x` may. Treating 0.x minors as non-breaking would have passed an unassessed `0.17.2 -> 0.18.1`, which is this ADR's own worked example and an entire API-group rename.

`unversioned` fails unconditionally because the semver calibration below has nothing to calibrate on: there is no boundary to classify. It is a blind spot rather than an unassessed transition, and the remedy is in the operator's hands, since pinning a comparable ref resolves it. `--fail-on-error=false` covers anyone who accepts the blind spot deliberately.

The calibration uses the signal semver already carries. Without it, a matrix that starts at zero coverage would fail on every component, and strict mode would sit disabled forever. With it, an unassessed 1.x to 2.x still stops you on day one.

`--fail-on-error` controls this, defaulting to true, mirroring `aicr validate` (`pkg/cli/validate.go:416`, `Value: true`). A plain boolean rather than an override with a special name: there is nothing to escape from, since the caller already chose to run the check. `aicr diff --fail-on-drift` is the same flag shape with the opposite default, so both conventions in the repo are flag-driven and this adds no new one.

### Decision 7: Generated wrappers carry two versions

`localformat/templates/chart.yaml.tmpl:19` and `wrapper-chart.yaml.tmpl:19` both hardcode `version: 0.1.0`. Online mode therefore reports `0.1.0` for every Kustomize-derived and manifest-derived component, hiding the version the transition records are keyed on.

The field is being asked two different questions. Split them:

```yaml
apiVersion: v2
name: {{ .Name }}
description: Generated wrapper chart vendoring {{ .ChartName }}@{{ .ChartVersion }} for {{ .Parent }}.
type: application
version: {{ .AICRVersion }}
appVersion: "{{ .ComponentVersion }}"
annotations:
  aicr.run/component-version: "{{ .ComponentVersion }}"
  aicr.run/generated-by: "{{ .AICRVersion }}"
```

- **`version:` is the AICR version that generated the wrapper.** The wrapper's content is produced entirely by AICR's templates, so AICR's version is the honest answer to "what version is this artifact". It also matches what `recipe.yaml` already does: `pkg/recipe/builder.go:231` sets `result.Metadata.Version` from the AICR binary version, and `pkg/cli/validate.go:845` compares it against the running binary for skew detection.
- **`aicr.run/component-version` is the payload version**, read by the matcher for generated wrappers. Free-form, so a Kustomize `defaultTag` like `release-1.4` does not have to masquerade as semver.
- **`appVersion` also carries the payload version.** Conventional Helm usage, and it makes plain `helm list` output readable for a human even though the matcher ignores it.

**Upstream charts need no annotation: the matcher reads their own version.** A `KindUpstreamHelm` folder installs the upstream chart directly, so the release's chart version *is* the payload version, and Helm already records it. gpu-operator, cert-manager, aws-efa and the rest are read straight from the release.

That is also what the annotation exists for. A generated wrapper's chart version describes the wrapper, not what it carries: today a hardcoded `0.1.0`, and the AICR version once this decision lands. Reading the release version there would be wrong, so the annotation supplies the payload version that the chart version cannot.

So the rule is one sentence with two branches: **read `aicr.run/component-version` when it is present, otherwise read the release's chart version.** Presence of the annotation is exactly the signal that the chart version means something else, which is why the fallback needs no separate flag or lookup table.

**Dev-build fallback is mandatory.** `pkg/cli/root.go:38` sets `versionDefault = "dev"`, which is not valid SemVer 2, and Helm rejects it for `Chart.yaml` `version:`. The bundler normalizes non-release versions to `0.0.0-dev` and strips a leading `v`. Without this, `make dev-env` and Tilt break. This is an explicitly tested case, not a discovered one.

Reproducibility is unaffected. The AICR version already travels in every bundle through `recipe.yaml`, so bundle bytes already vary by AICR version. `pkg/bundler/stock_render_parity_golden_test.go:41` already pins the builder and bundler versions "so the digest is a pure function of the catalog and the render", and stamping rides that existing pattern.

### Decision 8: Close the `ownsCRDs` deployer gap

`ownsCRDs` is consumed by exactly one deployer, `pkg/bundler/deployer/flux/flux.go:994`. On helm, helmfile, argocd, and argocd-helm, CRDs still sit at day-one schema after every upgrade. That is a live upgrade defect, not a hypothetical one.

This ADR records `ownsCRDs` as the precedent the transition-record design follows, and names the deployer gap as in-scope work that must land **before** verdicts ship. A `safe` transition whose CRDs changed is not actually safe on a deployer that never upgrades CRDs, so leaving this open would have the check assert a safety it cannot deliver on four of five deployers. It is independent of the rest of this ADR and can ship as its own issue and PR, which is why the Implementation Plan puts it first rather than last.

### Decision 9: UAT covers upgrade and rollback

A single release-to-release lane, up then down:

1. Deploy the previous AICR release's bundle.
2. Upgrade to the version under test. Assert component health.
3. `helm rollback`. Assert component health again.

`uat-run.yaml` already takes an `aicr_version` input, and a previous AICR release naturally carries older component pins. So one lane exercises real component version transitions *and* AICR's own contract stability, with no fixture machinery to build. The existing per-component health checks are the pass criteria.

This is the joint that makes the whole design trustworthy. Without it, every `safe` is an unverified human assertion carrying the same false-confidence risk as a wrong `ownsCRDs` flag. With it, a verdict is a tested claim.

**The lane asserts the record, not just health.** Component health passing after an upgrade says nothing about whether the verdict was right, and validating the classification is the whole point of the lane. So each transition it exercises asserts three things: that the steps the record declared for this deployer were the steps actually needed, and that health passes afterwards. A lane that only checks health would leave `safe` exactly as unverified as it is today.

The lane also rolls back and re-asserts health, but that is a smoke check that rollback does not explode, not validation of `reversible`. Reverse transitions are not held to the standard forward ones are, for the reasons in Decision 2.

`verifiedBy` is where a record names which of these backed its claim ([Decision 2](#decision-2-transition-records)), so the assertion and the thing that justifies it stay attached.

**`safe` transitions are the priority.** They are simultaneously the cheapest to test and the most dangerous to get wrong. Cheapest, because `safe` means no manual steps, so the transition is deploy, upgrade, assert, roll back with nothing for a human to perform. Most dangerous, because a wrong `safe` asserts nothing needs doing, so there are no steps whose absence would tip anyone off; the operator finds out at the outage. A wrong `manual` merely costs effort.

Non-`safe` transitions are not covered by testing at all, and deliberately so: their correctness rests on the authoring workflow in [Decision 11](#decision-11-authoring-is-a-documented-workflow-not-adr-content). A component owner who writes manual steps is **not** expected to also contribute UAT automation for performing them. AICR automates what is free.

| Verdict | How its correctness is established |
|---|---|
| `safe` | The UAT lane, systematically. |
| `manual`, `blocked` | The authoring workflow ([Decision 11](#decision-11-authoring-is-a-documented-workflow-not-adr-content)): checklist, pin-bump rule, review. |
| Residual: hardware-specific risk outside the UAT matrix | Evidence from whoever performed the upgrade. |

**A tested pair does not validate a whole range.** A record's `to` is a range, but UAT exercises specific version pairs, so a `safe` verdict covering `>=1.2.0 <2.0.0` is tested at whichever pair the lane happened to run and asserted for the rest. This is the same authored-claim problem the record has everywhere else, narrowed rather than removed by testing, and it is worth being explicit that "tested" means "tested at a point in this range".

**Two things testing cannot reach.** Manual steps, by definition. And accelerator-specific risk: UAT is H100-only across all four reservations, while the registry supports gb200, b200, h200, l40s and others. That residual is narrower than it sounds, because most transitions are chart-level and accelerator-independent; the exposure is transitions whose risk *is* hardware-specific, such as driver or topology behavior.

For that residual, and only that residual, evidence submitted by whoever performed the upgrade is the fallback. AICR already signs and publishes evidence (`aicr validate --emit-attestation`, `aicr evidence sign|publish|verify`), so this reuses a path rather than inventing one. Its predicate and whether it warrants a distinct kind alongside ADR-007's `cncf` and `attestation` are implementation questions, not decided here.

One caveat to record rather than discover: operator-submitted evidence is weaker provenance than a CI-produced attestation. A recipe-test attestation comes from a known runner on a known commit; upgrade evidence comes from whoever ran the upgrade, potentially self-attested on their own cluster. Both are signed. They are not equally strong, and "signed" must not be read as "trusted" when the two appear side by side.

Rollback deserves its own assertion because **rollback is not the inverse of upgrade**. `helm rollback` restores the previous release's manifests, but it cannot undo CRD schema changes (Helm skips `crds/` on rollback exactly as on upgrade), data migrations already executed, CR stored-version rewrites, or anything node-level. A `safe` upgrade does not imply a safe rollback, and only a test can tell the difference.

### Decision 10: A coverage gate keeps records current

Matrix coverage starts at zero, and nothing so far forces it upward. A pin bump that forgets a record produces `unknown` for everyone upgrading into that version, which is indistinguishable from "nobody has assessed this yet". Review on #2343 named this as the ADR's main long-term risk: *"Keeping it up to date and correct over time will be a challenge."*

**The gate.** A Go test reads `registry.yaml` and asserts that, for every pinned component version, some transition record has a `to` range covering it. `tools/bom/freshness_test.go:64` (`TestCommittedBOMVersionsMatchRegistry`) is the same shape and the precedent to follow.

A static test cannot see transitions, because it has no `from` side. But `to`-range coverage is the assertion that matters: if the pinned version falls outside every record's `to` range, then by construction anyone upgrading into it gets `unknown`. So the gate catches exactly the case it needs to, and a bump that lands outside coverage must extend an existing block or add one.

**Scope.** 34 registry components carry `defaultVersion`, plus one overlay override at `recipes/overlays/aks.yaml:190` which must be included or the gate has a hole. Manifest-only components carry no chart version and key on the AICR release per [Decision 3](#decision-3-ownership-classes-and-what-aicr-can-see), so every AICR release is potentially a transition for them; gating that is a different check and is out of scope here.

**An explicit allowlist, not a diff.** Components without records start on an allowlist that begins at 34 entries and shrinks. Gating only on pins that changed in the current PR would be quieter, but it cannot see a record being *deleted*, since no pin moves when that happens, and it leaves pre-existing gaps invisible indefinitely. An allowlist makes the debt countable in one file and makes removing an entry a deliberate act. The repo already uses this shape for API-diff acknowledgement baselines and `.openvex.json`.

**Three things make the gate measure assessment rather than coverage.** [Decision 2](#decision-2-transition-records) forbids a record's `to` from reaching past the current pin, so every bump forces an author back into the record; it requires `safe` to name what verified it; and the report states the span a verdict covers, so a wide `from` is visible in review. Any one alone is evadable. Together, the cheapest record that clears the gate is one somebody edited, substantiated, and can be seen to have overreached.

**The gate measures coverage; `verifiedBy` measures assessment.** On its own, a coverage assertion is satisfiable by a blanket `safe` record spanning a component's whole history, which is why [Decision 2](#decision-2-transition-records) requires `safe` to name what verified it. The two are a pair: the gate creates the obligation to have a record, and `verifiedBy` stops the cheapest record from being a lie.

Two alternatives were considered and rejected, and neither is the forward-reach ban above, which constrains *direction* rather than *size*. **Capping range width** (at most one major crossed, backward included) reads like the obvious fix but fights [Decision 12](#decision-12-the-matrix-describes-aicrs-pins-not-upstreams-releases)'s own measurement: AICR's pins really are jumpy, `v1.9.0` to `v1.20.0` in one commit, so wide records are legitimate and common. A cap would generate friction on honest cases while a dishonest author simply writes two records instead of one. **Gating the `from` side from git history**, which `tools/upgrade-matrix` already reconstructs, would turn coverage into a real transition assertion, but it makes a tool that is explicitly best-effort and wired to no gate load-bearing in CI, and it re-derives at gate time what UAT establishes directly.

**Who writes the record.** The person bumping the pin. This is a translation, not new work: bumping cert-manager already means testing it and reading its migration notes, and the record is where that reading gets written down instead of discarded. This is also what makes the boundary in [Decision 1](#decision-1-boundary) hold for components AICR has no upstream leverage over.

The gate creates the obligation. `verifiedBy` forces an author to name what backs a `safe` verdict. Making that cheap to produce, by giving them a checklist of what to verify in the first place, is a separate and still-open question.

### Decision 11: Authoring is a documented workflow, not ADR content

[Decision 10](#decision-10-a-coverage-gate-keeps-records-current) creates the obligation to write a record. It does not tell an author what to put in one, which is the expensive part and the likeliest reason records go unwritten.

**The checklist lives in `docs/contributor/`, not here.** An ADR records a decision; a checklist evolves as components and failure modes accumulate. Embedding it would mean amending a decision record to add a bullet, so this ADR decides only that the checklist exists, where it lives, and what it is based on.

**Its basis is [ADR-019](019-k8s-aibom-runtime-inventory.md), treating a version bump as a re-qualification event.** ADR-019 admitted a component only after five categories of gate passed: release and supply chain, Helm and Kubernetes lifecycle, security and privacy, operational safety, and AICR qualification. What it does not cover is what happens next, because those gates ran once, for one release, and nothing re-runs them when a pin moves. That is the same seam this ADR stands on, approached from the other side, and it maps directly: "CRD conversion, migration, and retention behavior is documented" is the nodewright rename; "deterministic install, update, rollback, and uninstall ownership" is [Decision 9](#decision-9-uat-covers-upgrade-and-rollback). Reusing those categories turns a verdict into the output of a checklist rather than a from-scratch judgement.

**The author is whoever moves the pin.** This is a translation, not new work. Bumping cert-manager already means testing it and reading its migration notes; the record is where that reading gets written down instead of discarded. A skill under the existing `aicr-*` convention (see `docs/contributor/skills.md`) is the natural vehicle for walking an author through it.

**Upgrade-supportability becomes an admission criterion.** A component whose upstream does not support upgrade should not enter the registry. Where upgrading is found to be broken, the remedy is an issue against that project and a hold on admission until it is resolved. This is what keeps [Decision 1](#decision-1-boundary)'s boundary honest for components AICR does not own: AICR is not promising to fix third-party upgrade paths, it is declining to ship components that lack one.

**Sequencing.** None of this lands with this ADR. `recipes/upgrades/` does not exist yet, so a rule instructing contributors to write records into it would be stale from the day it merged. The rule, the checklist, and the skill land with the implementation.

### Decision 12: The matrix describes AICR's pins, not upstream's releases

Records accumulate, so the natural question is how far back to keep them. The answer is that volume was never the problem, and the real constraint is a different one.

**Pins are sparse and jumpy.** AICR does not track a component release-for-release. nvsentinel went from `v1.9.0` to `v1.20.0` in a single commit ([#2333](https://github.com/NVIDIA/aicr/pull/2333), 2026-08-24), skipping eleven minor versions. A component may sit on one pin across many AICR releases and then move several versions at once.

`tools/upgrade-matrix` reconstructs this from git history and generates [the component version matrix](../user/component-version-matrix.md). Across the last eight releases it finds 19 transitions, of which 7 need extra scrutiny: chart-identity changes, backwards moves, or jumps of three or more major/minor versions. Roughly a third of real transitions are not ordinary one-version bumps.

So the unit is neither calendar time nor AICR release count. It is **the component's own pin history**, and that history is short by construction: a handful of entries over a component's life in the registry, not one per AICR release. Retention therefore needs no horizon. Keep every pinned transition; the set stays small on its own, and deleting one only degrades the answer given to whoever is furthest behind.

**The real limit is expressiveness, not volume.** AICR can only describe a transition between two versions it has pinned. Where upstream requires landing on an intermediate version AICR skipped, that hop cannot be expressed, and **no AICR release can generate a bundle for it**, because none ever pinned it. Had nvsentinel required a stop at `v1.15.0` to migrate a CRD, the `v1.9.0` to `v1.20.0` record could not have offered it.

A record covering such a jump MUST say so rather than implying AICR can carry the operator through: the verdict is `blocked`, and the steps point at the upstream migration guide for the hops AICR cannot produce. This is the one case where the remedy really is outside AICR, and it is worth stating plainly because it is invisible from the matrix alone.

**A chart's identity can change, and then versions are not comparable.** `tools/upgrade-matrix` finds this twice in recent history: `nvidia-dra-driver-gpu` moved to `oci://registry.k8s.io/dra-driver-nvidia/charts` ([#1285](https://github.com/NVIDIA/aicr/pull/1285)) so its pin went from `25.12.0` to `0.4.0`, and `nodewright-operator` changed chart between v0.15.1 and v0.17.0. Neither is a downgrade; the two version strings simply belong to different version lines.

A semver-keyed record cannot express such a hop, and a matcher comparing versions would read it as a downgrade and resolve to `unknown` forever. A record covering a chart-identity change MUST therefore state it explicitly rather than relying on range matching, and the well-formedness check should reject a record whose `from` and `to` straddle a chart change without saying so. Tracked in [Open Questions](#open-questions) since the field shape is undecided.

**Jump size scales the claim.** `safe` on `v1.9.0` to `v1.20.0` asserts that eleven releases of upstream change are collectively safe, which is a far larger assertion than `safe` across one minor. The authoring checklist in [Decision 11](#decision-11-authoring-is-a-documented-workflow-not-adr-content) should scale scrutiny to the size of the jump, and a reviewer should treat a wide range with proportionally more suspicion.

**Stepping forward still works where AICR pinned the intermediates.** A cluster several pins behind upgrades in hops, which is what [Decision 2](#decision-2-transition-records)'s block rule already forces. Each intermediate bundle comes from the AICR release that pinned those versions: releases are immutable and remain available, and **records travel with the release that pinned them**, so v0.16's tree still describes transitions into v0.16's pins.

**Scope.** None of this creates a project-wide support policy. AICR has none today, and a general statement about supported releases belongs in `RELEASING.md`.

## Example

The real nodewright `skyhook.nvidia.com` to `nodewright.nvidia.com` rename, drawn from its [upstream migration guide](https://github.com/NVIDIA/nodewright/blob/main/docs/getting-started/migration.md). First the record:

```yaml
# recipes/upgrades/nodewright-operator.yaml
apiVersion: aicr.run/v1beta1
kind: ComponentUpgrades
component: nodewright-operator
transitions:
  # The rename ships in 0.18.0. The operator mirrors existing Skyhook
  # objects into NodeWright equivalents and retains legacy copies.
  - from: "<0.18.0"
    to:   ">=0.18.0 <0.20.0"
    verdict: manual
    reversible: true
    reversibleNotes: >-
      Only until legacy cleanup prunes legacy node state
      (LEGACY_CLEANUP_DELAY, default 24h). After that, rolling back
      re-applies every package from scratch.
    summary: >-
      skyhook.nvidia.com is renamed to nodewright.nvidia.com. The mirror
      controller is read-only on legacy objects, so GitOps controllers
      observe no drift from the operator upgrade itself.
    precondition: >-
      No Skyhook is in an in-flight rollout state (in_progress, erroring,
      blocked, waiting, unknown) and no nodes are mid-package. Paused and
      disabled Skyhooks migrate as-is; the mirror copies the annotation, so
      do not resume or enable them.
    stepsByDeployer:
      - deployers: [argocd, argocd-helm, flux]
        steps:
          - id: rename-crs
            description: >-
              In a single commit, remove the Skyhook manifests and add the
              NodeWright equivalents. Rewrite apiVersion and kind only;
              keep metadata.name identical.
            reason: >-
              One commit lets the controller prune the old object and adopt
              the mirrored new one in a single sync. Splitting it leaves a
              window where auto-sync recreates what you just deleted.
      - deployers: [helm, helmfile]
        steps:
          - id: rename-crs
            description: >-
              Rewrite apiVersion and kind in your manifests, then apply. The
              mirror pre-created the NodeWright, so this adopts the existing
              object rather than creating one.
          - id: delete-legacy-crs
            description: >-
              After confirming each NodeWright carries the
              nodewright.nvidia.com/mirrored-from stamp and is reconciling,
              delete the legacy Skyhook objects, then the DeploymentPolicy
              objects.
            reason: >-
              The admission webhook rejects DeploymentPolicy deletion while
              referencing Skyhooks still exist, so the order is
              load-bearing.
    affectedResources:
      - group: skyhook.nvidia.com
        kinds: [Skyhook, DeploymentPolicy]
    # The adoption manifest in Decision 4 lives under
    # nodewright-customizations, whose content it fixes, so it hangs off
    # that component's own record rather than this one. Cross-component
    # effects are described in steps; see Decision 3.
    references:
      - https://github.com/NVIDIA/nodewright/blob/main/docs/getting-started/migration.md

  # 0.20.0 removes the legacy API group outright. Written when 0.20.0 is the
  # pinned version; `to` is bounded there because a record may not claim
  # anything about versions that do not exist yet.
  - from: "<0.20.0"
    to:   ">=0.20.0 <=0.20.0"
    verdict: manual
    reversible: false
    summary: >-
      0.20.0 removes the skyhook.nvidia.com CRD, which cascade-deletes any
      Skyhook objects still present.
    precondition: >-
      No Skyhook objects remain in the cluster.
    stepsByDeployer:
      - steps:            # no `deployers:` means every deployer
          - id: confirm-rename-complete
            description: >-
              Confirm no Skyhook objects remain. If any do, complete the
              rename migration on 0.18.x or 0.19.x before crossing into
              0.20.0.
            reason: >-
              The CRD removal cascade-deletes whatever is left, and the
              objects are unrecoverable afterwards.
```

The two transitions together express a **migration window**: the rename is optional between 0.18.0 and 0.20.0 and mandatory before crossing into 0.20.0. Directional ranges carry that with no new concept.

Now the output. An operator regenerates after AICR ships the rename. The bundle was built with `--deployer argocd`, so the check infers that from `--to` and only the GitOps path renders.

```console
$ aicr upgrade-check --from ./bundles-v0.16.0 --to ./bundles-v0.17.0 --scan-cluster

COMPONENT                    FROM      TO        VERDICT   NOTES
nodewright-operator          0.17.2    0.18.1    manual    1 step, reversible 24h
nodewright-customizations    v0.16.0   v0.17.0   manual    CRs rewritten, regenerate

nodewright-operator 0.17.2 -> 0.18.1  (manual, reversible: yes)
  skyhook.nvidia.com is renamed to nodewright.nvidia.com. The mirror
  controller is read-only on legacy objects, so GitOps controllers observe
  no drift from the operator upgrade itself.

  Also changes: nodewright-customizations (AICR-authored CRs, rewritten
  in this release)

  PRECONDITION
    No Skyhook is in an in-flight rollout state (in_progress, erroring,
    blocked, waiting, unknown) and no nodes are mid-package. Paused and
    disabled Skyhooks migrate as-is; do not resume or enable them.

  STEPS (deployer: argocd)
    1. rename-crs
       In a single commit, remove the Skyhook manifests and add the
       NodeWright equivalents. Rewrite apiVersion and kind only; keep
       metadata.name identical.
       Why: one commit lets the controller prune the old object and adopt
       the mirrored new one in a single sync. Splitting it leaves a window
       where auto-sync recreates what you just deleted.

  ROLLBACK
    Safe until legacy cleanup prunes legacy node state
    (LEGACY_CLEANUP_DELAY, default 24h). After that, rolling back
    re-applies every package from scratch.

  AT RISK (not managed by AICR)
    2 Skyhook objects in namespace `platform` carry no AICR or Helm
    ownership. AICR will not migrate these. The skyhook.nvidia.com CRD is
    removed in operator 0.20.0, which cascade-deletes anything remaining.

  References:
    https://github.com/NVIDIA/nodewright/blob/main/docs/getting-started/migration.md

upgrade check failed: 1 transition requires manual steps
```

Under `--deployer helm` the same record renders two steps instead of one, because the imperative path cannot merge the rename and the legacy deletion into a single atomic commit.

The `AT RISK` block appears only in online mode. Offline, the check states that it cannot see unmanaged resources rather than omitting the section, since an absent warning reads as an all-clear.

## Testing Strategy

The governing discipline is a design constraint, not a test-plan detail: **no test asserts a specific verdict for a real component.** Verdicts for real components are validated empirically, by KWOK and UAT. Pinning them in a Go test means every `registry.yaml` pin bump churns the suite, and the pressure to keep tests green becomes pressure to weaken the records.

| Layer | Coverage |
|---|---|
| Unit (Go) | Table-driven matcher tests over synthetic records: verdict selection, semver range edges, and that a forward record never matches in reverse. Never reads the real registry. |
| Golden (Go) | Rendered table and JSON report compared byte-for-byte against checked-in goldens with an `-update` flag, not by substring match. Synthetic input. |
| Registry well-formedness (Go) | Runs against **real** `recipes/upgrades/*.yaml`. Asserts files parse, ranges are valid semver, no record matches its own reverse, the referenced component exists, `safe` carries `verifiedBy`, no `to` reaches past the pin, and no gap sits between records. **Deliberately pin-sensitive**: a bump that does not touch the record fails it. |
| KWOK (new, this ADR) | Synthetic fixture component with two trivial chart versions. Install, upgrade, roll back, and confirm the check reads the right versions back. No network chart pull, per-PR speed, unaffected when a real pin moves. |
| UAT (new, [Decision 9](#decision-9-uat-covers-upgrade-and-rollback)) | Release-to-release against real clusters. The only layer that tests a real component's `safe` verdict, at the version pair it happens to run. The rollback leg is a smoke check, not validation of `reversible`. |

The registry check is the one layer that fails on a pin bump, and that is [Decision 10](#decision-10-a-coverage-gate-keeps-records-current)'s pressure rather than churn. The discipline above still holds, because the check never asserts *what* a verdict should be, only that someone recorded one and named what backs it. "Make the test green" therefore cannot be satisfied by writing `safe`, which is precisely the failure mode the discipline guards against.

The KWOK lane extends existing infrastructure rather than building new. `kwok/scripts/validate-scheduling.sh` already deploys AICR bundles through a real `helm install` path against simulated nodes, and already enumerates releases with `helm list -A -o json` (lines 367 and 571).

It deliberately proves nothing about real component upgrades. It proves the mechanism: version detection through the wrapper annotations from [Decision 7](#decision-7-generated-wrappers-carry-two-versions), verdict lookup, and rollback read-back.

## Acceptance Criteria

1. `aicr upgrade-check --from <recipe> --to <recipe> --deployer <name>` reports a verdict for every component whose version changed, and exits non-zero when any verdict is `manual`, `blocked`, or `unversioned`, or `unknown` across a breaking boundary (a major bump, or a minor bump while major is 0).
2. A downgrade with no explicit reverse record reports `unknown`, never `safe`.
3. A jump spanning more than one block reports `blocked` and names the stopping point, rather than composing the crossed records' steps.
4. `--from cluster` against a KWOK-installed bundle reports the same versions the artifact path reports for the same artifacts.
5. Recipe-to-recipe with no `--deployer` and no bundle to infer from exits non-zero asking for the flag, rather than defaulting or rendering every deployer's steps.
6. Wrapper charts expose the payload version in `aicr.run/component-version`, and a `dev` build still produces a Helm-valid `Chart.yaml`.
7. A malformed or reverse-matching record in `recipes/upgrades/*.yaml` fails `make lint` rather than being silently skipped.
8. A `safe` record with no `verifiedBy` fails the well-formedness check, so a blanket `safe` cannot satisfy the coverage gate.
9. A record whose `to` reaches past the currently pinned version fails the well-formedness check, so no record can make a claim about a version that does not exist yet.
10. A record file whose transitions leave a gap, where one record's `from` does not meet or overlap the previous record's `to`, fails the well-formedness check.
11. A record carrying an unrecognized `apiVersion` fails with `ErrCodeInvalidRequest` naming both values, and is neither skipped nor reported as `unknown`.
12. A pinned component version with no record whose `to` range covers it fails `make test`, unless the component is on the coverage allowlist.
13. `make qualify` passes.

## Open Questions

Genuinely undecided design, not work items. Known gaps in the design are stated in the decision each one limits; the implementation work they imply is tracked outside this document.

- **Is the boundary premise true?** [Decision 1](#decision-1-boundary) assumes most upgrades are already handled by the component's own chart. That is a structural argument, not a measurement, and nobody has checked it against this registry. An audit of past pin bumps, asking for each whether it required operator action beyond `helm upgrade`, would either support the boundary or overturn it. If a large fraction did require action, the rejected mutating-hook alternative deserves reopening.
- **Where the authoring checklist draws the line between a gate and a preference.** [Decision 11](#decision-11-authoring-is-a-documented-workflow-not-adr-content) bases it on ADR-019's five categories, but ADR-019 distinguishes gates from qualification preferences deliberately, and which of its categories are gates for a *version bump* rather than for *admission* is not obvious. Getting this wrong makes the checklist either toothless or unaffordable.

## Alternatives Considered

**AICR-injected hooks in upstream charts.** Registry records carry Job manifests that AICR injects as pre-upgrade or post-upgrade hooks into any component's chart, riding the existing `PreManifestFiles` machinery (`pkg/recipe/metadata.go:144-155`).

Rejected on two grounds. First, only a narrow slice of real migrations is Job-shaped: CRD stored-version migration and data or schema migration. Node-level changes, drain windows, values-schema renames, component replacement, and operational prerequisites are all outside what a Job can do. Second, for the slice that *is* Job-shaped, the upstream chart is the right owner, and Helm already provides the hook. AICR rendering a parallel hook mechanism duplicates a facility one layer down and takes on permanent ownership of migration logic it did not write.

**Scoped to injection, not to migration content.** [Decision 1](#decision-1-boundary) makes AICR responsible for migrating content in charts it generates, and [Decision 4](#decision-4-migration-content-ships-as-an-adjacent-generated-release) ships that as a release beside the component. What stays rejected is injecting into a chart AICR did not write.

**Per-step chainsaw preflight assertions.** Attach a read-only chainsaw assert to each manual step, reusing the health-check executor and the `pkg/chainsaw/allowlist.go` read-only gate, so "did you actually re-author your CRs?" gets a machine check.

Rejected **as the core mechanism**, because it answers a different question than the one this ADR is about. A chainsaw assert answers "is the cluster in state X?" It cannot express "what is installed versus what would be installed", because the assert has no knowledge of the target version. That comparison is the core need and it requires no cluster at all.

**The rejection is narrow.** The nodewright migration has a genuine read-only precondition ("all Skyhook objects are in `complete` status with no nodes in progress"), and [Decision 3](#decision-3-ownership-classes-and-what-aicr-can-see) admits a read-only cluster scan for at-risk unmanaged resources. So cluster-side reads are in scope; what is out of scope is expressing them as a per-step chainsaw assert format.

Preconditions are structured prose today, rendered and not evaluated. Making them machine-checkable is an [open question](#open-questions), and `pkg/bundler/gatemanifest` already renders per-component chainsaw gates with their own ServiceAccount and ClusterRole per deployer if that path is taken.

**Snapshot-to-snapshot version inference.** Snapshots capture running container image tags (`pkg/collector/k8s/image.go`). Mapping an image tag back to a chart version is heuristic and lossy, and it is strictly worse than reading Helm release inventory, which is exact.

**Documentation and links only.** A `migrationURL` field per component, rendered into the bundle README. Cheapest option with zero new failure modes, but nothing is machine-readable, nothing gates, and an operator who does not read the README learns about the breaking change as an outage.

**Full orchestration.** `aicr upgrade` drives the upgrade end to end, verifies health, and rolls back on failure. Makes AICR a deployment tool rather than a configuration generator, and duplicates what Helm, Argo CD, and Flux already do well.

## Consequences

**Positive.**

- A version transition carries a verdict that a pipeline can act on.
- Rollback coverage comes free from a direction-aware matcher, with no second mechanism.
- `reversible` surfaces a likely one-way upgrade before it happens, which is when the information is actionable, without claiming more than an author's read of the migration notes supports.
- Online mode gains a uniform installed-component inventory, useful well beyond this feature.
- Wrapper charts stop reporting a fictional `0.1.0`, which improves plain `helm list` output for humans regardless of the check.

**Negative and risky.**

- **Vendoring `helm.sh/helm/v4`** is a large dependency for one feature and lands in `make scan`, api-diff, and the vendor tree. Licensing is probably fine but is not free: `license-check` allows only MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC, and Zlib, and clears MPL-2.0 only through ten explicit per-import-path ignores, all HashiCorp. Helm is Apache-2.0 and its usual MPL-2.0 touchpoints (`errwrap`, `go-multierror`) are already among them, but the Makefile is explicit that "unrelated MPL-2.0 deps still fail closed for review" and that ignores must not be added to work around the policy. Helm v4's full dependency tree has not been resolved against that list.
- **Coverage starts at zero.** Every transition is `unknown` until someone authors a record, and [Decision 10](#decision-10-a-coverage-gate-keeps-records-current)'s gate starts with an allowlist of 34 components. The gate creates the obligation, and the forward-reach ban makes every later pin bump renew it, but nothing makes the *first* record appear for a component still on the allowlist. The matrix is only as useful as the backlog people work through.
- **Records are human assertions.** A wrong `safe` is worse than no record, because it converts uncertainty into false confidence. Decision 9 is the mitigation, and it only mitigates what UAT actually exercises.
- **UAT cluster time grows.** A release-to-release lane adds a second full deploy and a rollback to every cell it runs on, against an already-contended reservation pool.
- **`helm diff` noise.** With wrapper `version:` tracking the AICR version, a release that changes nothing material still shows a chart version bump. Rendered manifests stay identical so there is no resource churn, but `helm_diff` is pinned in `.settings.yaml` and used in CI.
- **The non-zero exit will surprise people.** A pipeline that adopts `upgrade-check` starts failing on unassessed transitions across a breaking boundary, which includes 0.x minor bumps and so fires more often than "major bumps" would suggest. That is the intended behavior, and it still needs a release note. `--fail-on-error=false` is the deliberate opt-out.

## Implementation Plan

Ordered so each step is independently useful and independently revertible.

1. **`ownsCRDs` deployer gap** (Decision 8). First, because it is a prerequisite for a verdict meaning what it says: a `safe` transition whose CRDs changed is not safe on a deployer that leaves CRDs at their day-one schema, which today is four of five. Independent of everything below, so it can ship as its own PR in parallel.
2. **Wrapper chart versioning** (Decision 7) — **shipped** ([#2526](https://github.com/NVIDIA/aicr/issues/2526)). Template change, dev-build normalization, golden updates. No new feature depends on it landing first, but online mode is wrong without it.
3. **Transition record schema and loader.** `recipes/upgrades/<component>.yaml`, the `upgrades.file` registry field, semver range matching with strict directionality, and a lint gate rejecting a record that matches in reverse. The loader calls the `apiVersion` gate and fails closed on an unrecognized value; it does not skip and does not degrade to `unknown`.
4. **Offline check.** `--from`/`--to` over recipes and bundles, table and JSON output, non-zero exit by default via `--fail-on-error`. This is the whole feature for CI and GitOps.
5. **Bundle rendering.** Transition records for the resolved component set render into the bundle README, filtered to the bundle's deployer, so operators who never run the command still see the steps that apply to them.
6. **`-premigrate` folder emission** (Decision 4). Mirrors the existing `-post` injection in `localformat`. Only needed once a real migration requires a hook; the nodewright adoption case is the first.
7. **Online mode.** Helm release inventory read path, on the vendored SDK. Blocked on the vendoring landing.
8. **KWOK upgrade and rollback lane** (Testing Strategy). Synthetic fixture component, per-PR speed. Catches read-path regressions long before a real cluster run would.
9. **UAT upgrade and rollback lane** (Decision 9).
10. **Coverage gate and allowlist** (Decision 10). Lands with the first real records so the allowlist starts shrinking rather than sitting static.
11. **Authoring workflow** (Decision 11). The `CLAUDE.md` pin-bump rule gains a transition-record clause (mirrored to `AGENTS.md` by `check-agents-sync`), the checklist lands in `docs/contributor/`, and the admission criterion lands wherever component admission is governed. Ships with step 3 or later, never before `recipes/upgrades/` exists.

Authoring the first real records, starting with nodewright-operator, should happen alongside step 4 so the check is exercised against real data rather than fixtures.
