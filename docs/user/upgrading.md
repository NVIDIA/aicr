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

**`blocked`:** do not make this jump in one step. The report always names the boundary it stops at, and the detail block under the row says which of three things happened. The first of them comes with instructions; the other two deliberately do not.

**Blocked, and here is how to do it safely.** One record describes exactly your move, and its author marked it `blocked`, meaning it must not be done in a single step. That record's steps are under the row, scoped to the deployer you named, saying what to do instead: typically land on an intermediate version, do some work there, and continue. Read them the way you read a `manual` row, and re-run the check when you get there.

**Blocked because you would skip a boundary.** Either the jump crosses two or more recorded boundaries, so no single record describes it, or a `blocked` record sits between you and your target but was written for a different starting point. Upgrade to the stopping point the report names, apply it, then re-run.

**Blocked because nothing describes your starting version.** A boundary is in the way, but no record covers an upgrade *from* where you are, usually because your version is below the lowest one anybody has written a record for. Upgrade to a recorded version first, then re-run. The report names the earliest recorded starting point.

The last two show **no** steps, and that is the point. The record carrying them describes a different move than the one you asked about, and running one migration's steps without the work that precedes them is how data gets destroyed.

That third case is stricter than a tool that simply had no record for you, and the strictness is deliberate. `upgrade-check` is opt-in: nothing in `aicr recipe` or `aicr bundle` invokes it, so a check somebody chose to run should not also be quietly permissive. When a boundary is in the way and nothing describes your starting point, saying so is more useful than a verdict nobody authored for your situation.

**`unknown`:** nobody has assessed this transition. That is a gap in AICR's data, not a verdict of safe. Treat it as unassessed: read the component's own upstream release notes, and consider [authoring the record](../contributor/upgrade-records.md) so the next operator does not repeat the work. A run fails on `unknown` only across a breaking boundary (a major bump, or a minor bump while the major version is `0`).

**`unversioned`:** one side's version is not comparable, so no boundary can be classified at all. Pin something comparable and re-run.

A component that appears on only one side is reported too. An added component is simply installed. A **removed** component stays installed: AICR dropping a component from a recipe says what AICR now ships, not that your running workload should be torn down. Removing it is your call.

## Gating a pipeline

`upgrade-check` exits non-zero by default when any component needs attention, so a pipeline can consume the result without parsing output:

```shell
aicr upgrade-check --from old-recipe.yaml --to new-recipe.yaml --deployer argocd
```

Add `--fail-on-error=false` to report without gating. The report prints in full either way; the exit code is orthogonal to it.

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

Records are **directional**. A record describing a forward upgrade never applies in reverse, so a downgrade with no explicit reverse record reports `unknown`, never `safe`. That is deliberate: undoing a migration is rarely the same work as doing it, and a CRD deleted on the way up does not come back on the way down.

Treat an `unknown` downgrade as genuinely unassessed rather than as a quiet pass.

## What this does not cover

- **It does not read your cluster's state.** The comparison is between two artifacts; nothing is inspected, deployed or modified. (A `cm://` path is an artifact location like a file path, so reading or writing one does contact that cluster's API for the ConfigMap itself.) If your cluster has drifted from the recipe you think you deployed, the check compares the artifacts you gave it, not reality. Reading installed Helm release inventory is tracked in [#2531](https://github.com/NVIDIA/aicr/issues/2531).
- **Bundle input works only for `helm` bundles today.** Other deployers' bundles carry no embedded `recipe.yaml` ([#2753](https://github.com/NVIDIA/aicr/issues/2753)). Recipe files work everywhere.
- **Coverage starts near zero.** Records are being written component by component, so most transitions still report `unknown`. Absence of a record is absence of assessment.
- **Records are human assertions.** A `safe` verdict names what verified it, but it is somebody's reading of the migration notes plus a test lane, not a proof.

## See Also

- [`aicr upgrade-check` reference](cli-reference.md#aicr-upgrade-check)
- [Upgrade Notes](component-catalog.md#upgrade-notes): per-component migration prose, including the ones with no record yet
- [Authoring transition records](../contributor/upgrade-records.md): for whoever bumps the pin
- [Generating Bundles](bundling.md): including the DRA driver eviction caveat, which is upgrade-relevant and predates this check
