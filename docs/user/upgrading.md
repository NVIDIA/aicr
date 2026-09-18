# Upgrading a Deployed Stack

Moving a cluster from one AICR release to a newer one. The short version: regenerate, **check**, then apply.

```shell
aicr recipe --service eks --accelerator h100 --intent training -o new-recipe.yaml
aicr upgrade-check --from old-recipe.yaml --to new-recipe.yaml --deployer helm
aicr bundle -r new-recipe.yaml --deployer helm -o ./bundles
```

The middle step is the one this page is about.

## Why a check step exists

A new AICR release moves chart version pins. Most of those moves are ordinary and a deployer absorbs them. Some are not: a CRD is renamed, a default flips, an API group changes, and the upgrade damages a running cluster in a way no deployer reports as a failure.

Nothing in `aicr recipe` or `aicr bundle` can tell you which kind you are looking at. `bundle` has a recipe and no source version, so it cannot compute a transition at all. A pin says what a **new** deployment gets; it says nothing about moving an **existing** one.

`aicr upgrade-check` compares two artifacts and answers that question per component, from [transition records](../contributor/upgrade-records.md) written by whoever bumped the pin.

## Running it

Keep the recipe you deployed from. `--from` always needs it: it is the only record of where your cluster came from, and without it there is nothing to compare.

If you have already generated the recipe you intend to move to, name both:

```shell
aicr upgrade-check --from old-recipe.yaml --to new-recipe.yaml --deployer helm
```

If you have not, omit `--to` and ask the other useful question:

```shell
aicr upgrade-check --from old-recipe.yaml --deployer helm
```

That re-resolves your artifact's own criteria against the running binary's pins, answering "am I behind, and does catching up hurt?" rather than "is this specific move safe?". It is usually the question you actually have.

`--deployer` is required whenever a component carries steps, because steps differ per deployer and the tool will not guess. Pass the same value you pass to `aicr bundle`.

## Acting on the report

Full flag and verdict reference lives in the [CLI reference](cli-reference.md#aicr-upgrade-check). What to *do*:

**`safe`:** nothing. Apply the new bundle.

**`manual`:** read the steps under the table. They are scoped to the deployer you named, and they are ordered. Check the `PRECONDITION` line first: it states a cluster state that must hold before you start, and it is prose for you to verify, not something the tool evaluates. Run the steps, then apply the bundle.

**`blocked`:** do not make this jump in one step. The report always names the boundary it stops at, and the detail block under the row says which of four things happened. The first of them comes with instructions; the other three deliberately do not.

**Blocked, and here is how to do it safely.** One record describes exactly your move, and its author marked it `blocked`, meaning it must not be done in a single step. That record's steps are under the row, scoped to the deployer you named, saying what to do instead: typically land on an intermediate version, do some work there, and continue. Read them the way you read a `manual` row, and re-run the check when you get there.

**Blocked because you would skip a boundary.** Either the jump crosses two or more recorded boundaries, so no single record describes it, or a `blocked` record sits between you and your target but was written for a different starting point. Either way the report names a version to land on: the first boundary you would have flown over. Upgrade to it, apply, then re-run.

**Blocked because nothing describes your starting version.** A boundary is in the way, but no record covers an upgrade *from* where you are. Usually that is because your version is below the lowest one anybody has written a record for, and the report names that earliest recorded starting point: upgrade to it first, then re-run. Where your version instead falls in a gap between recorded `from` domains there is no such landing point to name, so the report says to author a record for your starting point instead.

**Blocked because your target is past what the record assessed.** One record does describe a move from where you are, but it stops assessing before your target: its `to` names a ceiling, and you are asking to land above it. Whoever wrote it cannot have read the migration notes for releases that did not exist yet, so it says nothing about the ground past that ceiling. Upgrade no further than the ceiling the report names, then re-run; the other remedy is for someone to widen that record.

The last three show **no** steps, and that is the point. The record carrying them describes a different move than the one you asked about, and running one migration's steps without the work that precedes them is how data gets destroyed.

Those last two cases are stricter than a tool that simply had no record for you, and the strictness is deliberate. `upgrade-check` is opt-in: nothing in `aicr recipe` or `aicr bundle` invokes it, so a check somebody chose to run should not also be quietly permissive. When a boundary is in the way and the records reach neither back to your starting point nor forward to your target, saying so is more useful than a verdict nobody authored for your situation.

**`unknown`:** AICR has nothing to tell you about this transition. That is a gap in its data, not a verdict of safe, and it always fails the run. Read the component's own upstream release notes and decide for yourself. Three different things produce it, and they differ in what would close the gap:

- **No record at all** (`no-record`). Nobody has assessed this component. Consider [authoring the first record](../contributor/upgrade-records.md) so the next operator does not repeat the work.
- **A record exists but is silent here** (`no-boundary-crossed`). Somebody has assessed this component, but wrote no boundary in the range you are moving through. Decide whether one belongs there, and widen the record if it does.
- **You are rolling back** (`downgrade`). See below: this one can never become known.

The difference from `blocked` is worth holding onto. `blocked` means AICR has something to tell you and a version to stop at, so read it and act on it. `unknown` means AICR has nothing, so the investigation is yours. Neither is permission to proceed.

**`unversioned`:** one side's version is not comparable, so no boundary can be classified at all. Pin something comparable and re-run.

A component that appears on only one side is reported too. An added component is simply installed. A **removed** component stays installed: AICR dropping a component from a recipe says what AICR now ships, not that your running workload should be torn down. Removing it is your call.

## Gating a pipeline

`upgrade-check` exits non-zero by default when any component needs attention, so a pipeline can consume the result without parsing output:

```shell
aicr upgrade-check --from old-recipe.yaml --to new-recipe.yaml --deployer argocd
```

Add `--fail-on-error=false` to report without gating. The report prints in full either way; the exit code is orthogonal to it.

Anything other than `safe` exits non-zero, `unknown` included. Adopting this as a hard gate today will fail on most comparisons, because records are still being authored (see **Coverage starts near zero** below); `--fail-on-error=false` plus the JSON payload is the usable shape until coverage grows.

Both a bad invocation and a failing check exit `2`. To tell them apart, write JSON and branch on the payload:

```shell
aicr upgrade-check --from old.yaml --to new.yaml --deployer argocd \
  --format json --output report.json --fail-on-error=false
jq -e '.summary.failing == 0' report.json
```

## Rolling back

Point the check the other way:

```shell
aicr upgrade-check --from new-recipe.yaml --to old-recipe.yaml --deployer helm
```

Records are **directional**. A record describing a forward upgrade never applies in reverse, so a downgrade reports `unknown`, never `safe`. That is deliberate: undoing a migration is rarely the same work as doing it, and a CRD deleted on the way up does not come back on the way down.

**A downgrade always fails the run**, whatever the versions are and whatever records exist. It is the one `unknown` that can never become known: the well-formedness rules reject every reverse record, so no amount of authoring closes it. It is reported as `unknown` rather than `blocked` because `blocked` names an intermediate version to stop at, and you cannot partially roll back.

So a rollback needs human review before you run it. Read the component's own downgrade guidance, and if you accept the risk deliberately, `--fail-on-error=false` gives you the report without the gate.

## What this does not cover

- **It does not read your cluster's state.** The comparison is between two artifacts; nothing is inspected, deployed or modified. (A `cm://` path is an artifact location like a file path, so reading or writing one does contact that cluster's API for the ConfigMap itself.) If your cluster has drifted from the recipe you think you deployed, the check compares the artifacts you gave it, not reality. Reading installed Helm release inventory is tracked in [#2531](https://github.com/NVIDIA/aicr/issues/2531).
- **You still name the deployer.** A bundle records the deployer that built it in [`bundle-info.yaml`](bundling.md), but `upgrade-check` does not read that record yet, so `--deployer` is required whenever a component carries steps, even when reading a bundle.
- **Coverage starts near zero, so expect red.** Exactly two components ship a record today, so most transitions report `unknown` and the check exits non-zero on most comparisons. Absence of a record is absence of assessment, and the tool says so rather than rounding it up to approval. This is a coverage problem with an owner ([#2535](https://github.com/NVIDIA/aicr/issues/2535) makes a record mandatory for every pin bump), and it shrinks as records land. Use `--fail-on-error=false` for the report without the gate in the meantime.
- **Records are human assertions.** A `safe` verdict names what verified it, but it is somebody's reading of the migration notes plus a test lane, not a proof.

## See Also

- [`aicr upgrade-check` reference](cli-reference.md#aicr-upgrade-check)
- [Upgrade Notes](component-catalog.md#upgrade-notes): per-component migration prose, including the ones with no record yet
- [Authoring transition records](../contributor/upgrade-records.md): for whoever bumps the pin
- [Generating Bundles](bundling.md): including the DRA driver eviction caveat, which is upgrade-relevant and predates this check
