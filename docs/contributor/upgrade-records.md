# Upgrade Transition Records

A transition record answers "is this component version transition safe?" as
machine-readable data, so `aicr upgrade-check` can answer it for an operator
before they run the upgrade. This page is how you author one.

The design is [ADR-021](https://github.com/NVIDIA/aicr/blob/main/docs/design/021-component-upgrade-safety.md). The schema,
loader, and well-formedness rules live in `pkg/upgrade`.

## When you owe a record

**When you bump a pinned version.** That is the whole rule. If you change
`defaultVersion` or `defaultTag` for a component in `recipes/registry.yaml`, or
in an overlay or mixin, the component's record has to describe the boundary you
just moved across.

This is not new work. Bumping cert-manager already means testing it and reading
its migration notes; the record is where that reading gets written down instead
of discarded. The rule exists because the alternative has already cost us: AICR
moved nvsentinel from `v1.9.0` to `v1.20.0` in one step, skipping eleven minor
versions, typed as `Build/CI/tooling` with the breaking-change box unchecked.
Nothing claimed the upgrade was safe, because nothing asked.

Three rules in `pkg/upgrade` govern how far a record may reach, and keep the
obligation self-renewing rather than something you can defer:

- A **`safe`** record's `to` ceiling **may not reach past the currently pinned
  version**. `safe` is the vouching verdict, and an author cannot have read the
  migration notes for a version nobody has released.
- **`manual` and `blocked` may reach past the pin.** They are warnings carrying
  instructions, so reaching forward over-warns rather than passing something
  unassessed — and holding them to the pin forced the wrong order of work. The
  order that qualifies an upgrade is: read the migration notes, write the
  record, *then* bump. A component deliberately held below a known-breaking
  release is the case that proves it, since the record most worth having is the
  one describing the release you are not shipping yet.
- The `from` domains **may not leave a hole** below the pin. An operator sitting
  on a version in the hole would match no transition at all. This is the rule
  that makes the obligation self-renewing: a bump that leaves a hole below the
  new pin fails, whatever any record's ceiling says.

A coverage gate enforces the obligation itself. No record assesses an upgrade
into a pinned version, in the registry or in any overlay or mixin override, that
falls inside no record's `to` range: `aicr upgrade-check` reports `unknown`, or
`blocked` when the version lies past the record's highest ceiling. So:

- A component's record must cover every pin it has reached
  (`TestRecordedComponentsCoverTheirPins`, part of `make test`).
- A pin no record covers may not move, and a record that exists on the merge
  base may not disappear while its component remains
  (`TestChangedPinsHaveUpgradeRecords`). A deleted record moves no pin, so only
  a comparison with the base can see it. Moves are compared per source: the
  registry default, `base.yaml`, and each overlay and mixin's own
  `componentRefs`. Bumping one overlay's override counts even when another
  source already pins that version, and a source that adds or drops an
  override is compared against the registry default. What a recipe inherits is not
  resolved, so re-parenting an overlay or attaching a mixin that pins a
  different version goes unseen.

A record reaches a pin unless every one of its `to` ranges starts above it. A
record written ahead of its bump reaches nothing yet, so the gate treats those
pins as recordless until the pin gets there, and the bump that gets it there
must land inside a `to`. A record with no transitions, such as one carrying only
a `replaces` block, reaches no pin at all. Components that predate records are not gated until their pin next moves, and a
component new to the registry needs no record for its first pin, since nobody
upgrades into it from an earlier release.

## Where a record lives

`recipes/components/<component>/upgrades.yaml`, referenced from the component's
registry entry:

```yaml
# recipes/registry.yaml
- name: grove
  upgrades:
    file: components/grove/upgrades.yaml
```

The reference is what makes the file real. An `upgrades.yaml` no registry entry
points at is loaded by nothing and validated by nothing, so
`TestRealUpgradeRecordsWellFormed` fails on an orphan rather than letting it sit
there looking authoritative.

## The three verdicts you may author

| Verdict | Means | Requires |
|---|---|---|
| `safe` | Upgrade in place. Nothing for the operator to do. | `verifiedBy`, and **no** steps |
| `manual` | The operator must do something first, or the upgrade breaks. | At least one step, for every deployer |
| `blocked` | Do not make this jump in one step. Stop at an intermediate version. | At least one step, for every deployer |

**Your `blocked` steps do get shown.** `blocked` requires steps for the same
reason `manual` does: the verdict says "not in one step", and the steps are you
saying what to do instead. They render under the row, deployer-scoped, exactly
as a `manual` record's do, whenever your record is the one that describes the
operator's move. What the check withholds is somebody *else's* steps. When the
selected route takes two or more upgrades, or the jump crosses your boundary
from a starting point your `from` does not cover, no steps render at all,
because composing two migrations or running one authored for a different origin
is the failure `blocked` exists to prevent. Your steps render when your record
alone describes the operator's upgrade. Its `from` holds their version, its
`to` holds their target, and it lists every boundary in between that it must
(see [Listing the boundaries your record
passes](#listing-the-boundaries-your-record-passes)). A run of `safe` records
before yours, taken as one upgrade, still lets yours describe the rest. A
`manual` record on your own boundary, written for another starting range, does
not count. So write
the `blocked` steps as instructions a reader will follow, not as a formality to
satisfy the gate.

**Your `manual` and `blocked` records also ship in bundles.** When a bundle pins
a version inside your record's `to` range, `aicr bundle` writes the record into
`UPGRADING.md` at the bundle root with only the steps for the bundle's
deployer, lists the component in the README, and prints a warning. This is the
path for operators who never run `upgrade-check`, so your `precondition`, each
step's `reason`, and `reversibleNotes` are read at the moment of applying,
without the report around them. The bundle cannot know the operator's starting
version, so it renders your `from` range for them to check. A `safe` record
renders nothing. See [Upgrade guidance in the
bundle](../user/bundling.md#upgrade-guidance-in-the-bundle).

`unknown` and `unversioned` are **computed**, never authored. `unknown` is a gap
in the data, closed by writing a record. `unversioned` is a gap in the inputs,
closed by pinning something comparable. They stay distinct because they call for
different actions.

**`safe` is the claim that needs evidence, which is why it is the one that
requires `verifiedBy`.** Without that requirement the coverage gate would be
satisfiable by one blanket `safe` record per component, which measures coverage
rather than assessment. Name a UAT lane, a KWOK run, or an upstream release
note. "It looked fine" is not a value.

A wrong `safe` is worse than no record at all, because it converts uncertainty
into false confidence. When you do not know, leave it unwritten and let it
report `unknown`.

## A version bump is a re-qualification event

[ADR-019](https://github.com/NVIDIA/aicr/blob/main/docs/design/019-k8s-aibom-runtime-inventory.md) admitted a component
only after five categories of gate passed. Those ran once, against one release,
and nothing re-runs them when a pin moves. So a bump re-opens them, but not all
five equally.

**Gates for a version bump.** Answer these before you write a verdict:

- **Helm and Kubernetes lifecycle.** Did CRDs change? Is conversion, migration,
  and retention behavior documented for the new API? Does install, update,
  rollback, and uninstall ownership still behave deterministically? This is the
  category that produces almost every `manual` verdict.
- **Operational safety.** Does the new version change a default that alters
  behavior on a cluster that never set it? Kueue turning `WaitForPodsReady` on
  by default at 0.19.0 is this category, and it is invisible in a diff of
  AICR's values.
- **AICR qualification.** Does anything AICR itself reads still work? The
  readiness gate, the health check, the validators, and any value path in
  `registry.yaml` all couple to the component's surface. nodewright `v0.18.0`
  renaming `Skyhook` to `NodeWright` broke the readiness gate without changing
  one line of AICR.

**Re-check only on signal.** Release and supply chain, and security and privacy,
are admission gates. Re-run them when the bump crosses something that would
change the answer: a new image, a changed publisher, a new RBAC grant, a new
listening port. A patch bump inside the same publisher does not re-open them.

**Scale the work to the jump.** `safe` across eleven minors asserts far more
than `safe` across one. `aicr upgrade-check` renders the span for exactly this
reason, so a reviewer sees the width without decoding a semver range. If you
find yourself writing one `safe` verdict over a wide range, that is the signal
to either narrow it or go find the evidence.

## Writing it

Start from `recipes/components/grove/upgrades.yaml`, which is a real record for
a real migration. The shape:

```yaml
apiVersion: aicr.run/v1beta1
kind: ComponentUpgrades
component: <name>              # must match the registry entry
transitions:
  - from: "<0.18.0"            # needs an upper bound, or it cannot be shown forward-only
    to: ">=0.18.0 <=0.19.0"    # needs both bounds; if safe, ceiling at or below the pin
    verdict: manual
    summary: >-
      What breaks, in one or two sentences. Not a changelog.
    precondition: >-
      A state the cluster must be in before starting. Rendered, not evaluated.
    reversible: false
    affectedResources:
      - group: example.io
        kinds: [Widget]
    stepsByDeployer:
      - deployers: [helm, helmfile]   # every deployer these steps are written for
        steps:
          - id: stable-kebab-case-id
            description: What to do.
            reason: Why, and what happens if you skip it.
      - steps:                 # omit `deployers` for the remainder: every
                               # deployer no group names, terraform included
          - id: reconcile
            description: Sync as usual.
```

### `summary` is not a release note

The summary answers one question: **what about this transition needs the
operator's attention?** It is read at the moment someone is deciding whether to
run an upgrade, next to a verdict and a step list, so it competes for attention
with the steps themselves.

So it is not a changelog and not a release note. Do not enumerate the release's
features, do not list fixed bugs, and do not summarize what is new. Upstream
already publishes all of that, and duplicating it here means it goes stale the
moment upstream edits it.

Weight it by verdict:

- **`manual` or `blocked`:** name the breakage and its mechanism, tightly. The
  reader needs enough to understand why the steps exist and what happens if they
  skip them. One or two sentences.
- **`safe`:** shorter still. There is nothing to act on, so say what moved and
  stop. `verifiedBy` is carrying the weight here, not the prose.

**Link to the component's own documentation.** That is what `references` is for,
and it is the right home for the detail the summary deliberately leaves out:
upstream migration guides, release notes, the relevant issue, and the
operator-facing [Upgrade Notes](../user/component-catalog.md#upgrade-notes)
entry if one exists. A reader who needs the full story should get a link, not a
wall of transcribed text that drifts out of date.

Good, because it names the breakage and its mechanism:

```yaml
summary: >-
  alpha.12 drops the clustertopologies.grove.io CRD in favor of
  clustertopologybindings.grove.io, which reuses the shortname ct, so applying
  the new CRD before deleting the old one blocks it from reaching Established.
```

Bad, because it is a release note. None of it tells the operator what to do:

```yaml
summary: >-
  alpha.12 adds scheduling improvements, updates dependencies, fixes a
  goroutine leak in the reconciler, and drops an unused CRD.
```

Range grammar is deliberately restricted: a single AND-group of simple
comparators (`<`, `<=`, `>`, `>=`, `=`, or a bare version). No `||`, no hyphen
ranges, no `^`/`~`, no wildcards, no partial versions, no build metadata.
Anything ambiguous is rejected rather than approximated, because a wrong
structural check is worse than a rejected record. Prereleases are supported,
and grove is pinned at one.

`<1.25.0` leaves out 1.25.0's prereleases, such as `1.25.0-rc.1`, because a
prerelease likely carries changes meant for its release. Every other operator
orders a prerelease below its release, so `<=1.25.0` admits `1.25.0-rc.1` and
`>=1.25.0`, `>1.25.0` and `=1.25.0` leave it out. A cluster on a release's
prerelease therefore matches no record unless one names prereleases
explicitly, as grove's do with `<0.1.0-alpha.12`. The coverage check does not
count those versions as a gap. A prerelease bound compares exactly, so
`>=1.25.0-rc.1 <1.25.0` matches nothing. Write `<=1.25.0` or name the last
prerelease instead.

**Deployer groups must partition.** No two explicit groups may claim the same
deployer, at most one group may omit `deployers` (that one is *the* remainder),
and a `manual` or `blocked` verdict must cover all six of `argocd`,
`argocd-helm`, `fleet`, `flux`, `helm`, `helmfile`, `terraform`. Otherwise an Argo CD operator gets a
verdict promising steps with none for them.

Write the steps a given deployer needs in that deployer's group, even when they
duplicate another group's. A group is one ordered sequence a reader follows top
to bottom, not a fragment to be assembled. Step ids may repeat across groups,
since a consumer addresses a step as (deployer, id).

The remainder is a convenience, not a default. A step written for Flux or Argo
CD reads wrong under `terraform`, which also lands there unless a group names
it. For an `ownsCRDs` component, a `manual` or `blocked` record must give
`helmfile` and `terraform` an explicit group with a step that applies the CRDs,
since nothing applies them on those two deployers. A test in `pkg/recipe`
enforces that ([ADR-021](https://github.com/NVIDIA/aicr/blob/main/docs/design/021-component-upgrade-safety.md)).

**`affectedResources` may not name core `Secret`.** The at-risk scan lists every
kind a crossed record names, cluster-wide, and the permission to list Secrets
reads every Secret's data whatever the scan keeps. A `Secret` kind in a CRD
group is unaffected.

### Where your lowest `from` floor starts is a real decision

A record is *crossed* when the operator's source version sits below the floor
its `to` names and their target reaches it. A crossed record can take part in
the operator's route when the route lands in its `from`, even if they start
below it. A record whose `to` reaches past another record's floor vouches for every
release in between, so only write that reach when you have read their notes.

What `from` still decides is whether *your guidance* was authored for that
starting point. So the space below your file's lowest `from` floor is not
neutral ground. An operator down there crosses your boundary, matches no
`from`, and is told `blocked`: "upgrade to a recorded version first". They get
no verdict and, unlike someone your `from` does cover, no steps either, because
you did not write any for where they are.

That is deliberate, and it is why the floor is a decision rather than a
formality. Choose it at the oldest version you are willing to make a statement
about. If you are comfortable saying the transition holds from anywhere below
the boundary, write `from: "<3.0.0"` and say so. If you are not, a bounded floor
is the honest shape, and everyone beneath it correctly gets told to move up
first rather than handed a claim nobody verified.

### Where your `to` ceiling ends is a real decision too

`to`'s floor is the boundary a jump crosses; its ceiling is how far your verdict
reaches. A record does not vouch past that ceiling. An operator whose target
lands above it is told `blocked` with reason `beyond-record-ceiling`: stop at
the ceiling you assessed and re-run, or get the record widened. They get no
verdict and no steps, for the same reason as the floor case, and the report
names your ceiling as the place to stop.

That is not a limitation to route around. A ceiling is your claim about how far
forward you actually looked, so put it where you looked, and widen it
deliberately later rather than reaching for headroom now.

Note what the ceiling is *not* pinned to. Authoring-time rule 2 holds only a
`safe` ceiling at or below the current pin; a `manual` or `blocked` ceiling may
sit above it. So the two ends of a held component work like this: the ceiling
says how far you read, and the pin says how far AICR ships. When AICR
deliberately stays below a breaking release, those diverge, and the record that
describes the release you are *not* shipping is exactly the one an operator
needs. Put the ceiling at the boundary you assessed and let a target above it
land on `beyond-record-ceiling`, which is what turns "this jump has to be taken
on its own" into something the tool says rather than something prose asks for.
When that record is `blocked`, the report renders no steps and does not send
the operator to its ceiling, since the ceiling lies inside the interval it
forbids. Its explanation says to check the move to the record's floor on its
own, or to a version in its `to` when that floor is exclusive, and that check
renders the steps.

### Listing the boundaries your record passes

A record describes upgrades from any version in its `from` to any version in
its `to`, so a wide record can pass other records' boundaries in one step.
When no single record describes a jump, the check plans a route of records,
each starting where the one before lands, and reports the upgrades it takes.

Your record must list in `skips` each boundary its upgrades pass that is one
of these:

- a floor inside your `to`, above your own floor
- the floor of a `manual` or `blocked` record whose `from` admits a version
  your upgrade goes by

A `blocked` record on your own floor counts too, unless yours is `blocked` as
well. A `safe` boundary below your floor needs no entry. Listing a floor is
your claim that a cluster taking your record may skip that boundary's
guidance. psmdb's minor record lists the 1.23.2 patch boundary because
Percona upgrades past patch versions in one step:

```yaml
  - from: ">=1.23.0 <1.24.0"
    to: ">=1.24.0"
    skips: ["1.23.2"]
    verdict: manual
```

A `skips` entry cannot override a stricter verdict for the sources it was
written for. A `safe` record may not skip a `manual` or `blocked` boundary,
and a `manual` record may not skip a `blocked` one, when the other record's
`from` overlaps yours. Narrow your `from` to leave those sources out. If
clusters that ran 1.2 must take a `manual` move to 1.3, a `safe` jump to 2.0
that skips 1.3 must start below 1.2.

The lint gate rejects a record that passes a boundary it must list and does
not, and one whose `skips` names a boundary its upgrades never pass. A record
missing an entry describes no upgrade past that boundary, so the check plans a
route through the other record instead.

**`requiredStop: true`** makes a `safe` record a place every route stops, for
a release that does work an operator must let finish, such as an automatic
migration. A cluster taking it re-runs the check before moving further.
`manual` and `blocked` records already stop a route. No `skips` entry passes a
required stop, so a record whose `from` overlaps one must not reach its floor.

**`stepLimit: minor`**, at the top of the file beside `component`, caps every
upgrade at the next minor version, ignoring patches. Use it when upstream
supports upgrading one minor at a time. Under it a `manual` or `blocked` `to`
may leave out its ceiling, which is then the end of its floor's minor. A
`safe` `to` still names one. A move into a new major needs a record whose `to`
is in that major's first minor and whose `from` names the old major's last
minor, so the check blocks a major bump until someone writes it. A minor is not
the last when any record's `to` starts a later minor of the same major.

**`stepLimit: major`** caps every upgrade at the next major version, ignoring
minors and patches. Use it when upstream supports skipping minors but not
majors. A move from any minor may reach anywhere in the next major, and a
`manual` or `blocked` `to` without a ceiling ends at the end of its floor's
major. No shipped record uses it yet.

### When a migration only runs inside a window of versions

Some migrations happen only while a particular range of versions is running.
Nodewright's rename is one. An operator-side mirror converts Skyhook objects to
NodeWright in v0.18.0 through v0.20.x, and a release that drops the legacy group
would strand any object the mirror never converted. A jump from below the
window to past it never runs the mirror. Express the window with fields the
record already has.

- End the migration record's `to` ceiling at the last version that still runs
  the migration. The ceiling is your claim that the steps hold that far, so a
  jump past the window cannot borrow your verdict. Nothing else catches a
  ceiling that reaches too far. A later `safe` record carries no steps, so the
  check reads the wider ceiling as your word that the jump is fine.
- Give the release that closes the window its own record, with `from` starting
  inside the window and a `precondition` naming what must have finished, such
  as every object mirrored. A jump that crosses both records stops at the start
  of the window.
- To make the requirement hold however the other records change, add a
  `blocked` record whose `from` covers everything below the window and whose
  `to` starts at the release that closes it. Its steps say to land in the
  window, let the migration finish, then continue. It stops any jump from below
  the window to past it, and the report names a landing inside the window.

### Two records on one boundary each speak for their own starting range

Two records may name the same `to` floor when their `from` ranges do not
overlap. Each then describes crossing that boundary from its own range only.
An operator jumping from 1.5.0 past a boundary at 3.0.0 gets the record written
for 1.x, even though the version numbers pass through the range of a `manual`
record written for 2.x. That cluster never ran 2.x, so the 2.x record's steps
describe state it does not have. A `blocked` record written for 2.x does not
stop the jump either. Unless the 1.x record is `blocked` too, it must list that
floor in `skips`, and the listing hands the jump to the 1.x record's verdict.

So if crossing the boundary from 1.x must not happen in one step, such as when
it needs a migration that runs on 2.x software, the 2.x record cannot say it
for you. Say it in the 1.x record. Mark it `blocked` and write steps that land
in 2.x first, as the window's `blocked` record above does, or end its `to`
inside 2.x so the route lands there before the boundary.

## Checking your work

```bash
OLD_RECIPE="old-recipe.yaml"  # replace with a recipe generated before your pin change
NEW_RECIPE="new-recipe.yaml"  # replace with a recipe generated after it

make lint                                   # includes check-upgrade-records
./tools/check-upgrade-records               # just this gate
aicr upgrade-check --from "${OLD_RECIPE}" --to "${NEW_RECIPE}" --deployer helm
```

The gate runs every well-formedness rule over every record the registry
references, then both coverage checks above against the merge base with
`upstream/main` when your clone has that remote, else `origin/main`; set
`AICR_UPGRADE_BASE_REF` to compare against another ref, and fetch the base
first, since a stale one reports moves that are already on `main`. CI runs it
on every pull request. None of these **asserts a verdict**, so "make the test
green" cannot be satisfied by writing `safe`; flipping a `manual` record to
`safe` makes it fail harder, because `safe` may not carry steps.

Do not add a Go test asserting a verdict for a real component. Verdicts for real
components are validated empirically, by KWOK and UAT. Pinning them in the unit
suite means every pin bump churns the suite, and the pressure to keep tests
green becomes pressure to weaken the records.

## Components whose upstream has no upgrade path

If you find that a component simply cannot be upgraded in place, the remedy is
an issue against that project and a hold, not a creative record. AICR is not
promising to fix third-party upgrade paths; it is declining to ship components
that lack one. Roughly two thirds of the registry is software AICR does not own,
and that boundary is what keeps the promise honest.

## See Also

- [ADR-021: Component Upgrade Safety](https://github.com/NVIDIA/aicr/blob/main/docs/design/021-component-upgrade-safety.md)
- [Upgrade Notes](../user/component-catalog.md#upgrade-notes): the operator-facing prose for migrations
- [Components](component.md): adding and configuring registry components
