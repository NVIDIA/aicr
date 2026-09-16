# Upgrade Transition Records

A transition record answers "is this component version transition safe?" as
machine-readable data, so `aicr upgrade-check` can answer it for an operator
before they run the upgrade. This page is how you author one.

The design is [ADR-021](../design/021-component-upgrade-safety.md). The schema,
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

Two rules in `pkg/upgrade` make the obligation self-renewing rather than
something you can defer:

- A record's `to` ceiling **may not reach past the currently pinned version**.
  An author cannot have read the migration notes for a version nobody has
  released, so a record can only ever describe ground already covered. Every pin
  bump therefore lands outside the existing record and forces you back into it.
- The `from` domains **may not leave a hole** below the pin. An operator sitting
  on a version in the hole would match no transition at all.

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
operator's move. What the check withholds is somebody *else's* steps: when a
jump crosses two boundaries, or crosses yours from a starting point your `from`
does not cover, no steps render at all, because composing two migrations or
running one authored for a different origin is the failure `blocked` exists to
prevent. So write the `blocked` steps as instructions a reader will follow, not
as a formality to satisfy the gate.

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

[ADR-019](../design/019-k8s-aibom-runtime-inventory.md) admitted a component
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
    to: ">=0.18.0 <=0.19.0"    # needs both bounds; ceiling at or below the pin
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
      - deployers: [helm, helmfile]
        steps:
          - id: stable-kebab-case-id
            description: What to do.
            reason: Why, and what happens if you skip it.
      - steps:                 # omit `deployers` for the remainder
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
structural check is worse than a rejected record. Prereleases are supported:
grove is pinned at one.

**Deployer groups must partition.** No two explicit groups may claim the same
deployer, at most one group may omit `deployers` (that one is *the* remainder),
and a `manual` or `blocked` verdict must cover all five of `argocd`,
`argocd-helm`, `flux`, `helm`, `helmfile`. Otherwise an Argo CD operator gets a
verdict promising steps with none for them.

Write the steps a given deployer needs in that deployer's group, even when they
duplicate another group's. A group is one ordered sequence a reader follows top
to bottom, not a fragment to be assembled. Step ids may repeat across groups,
since a consumer addresses a step as (deployer, id).

### Where your lowest `from` floor starts is a real decision

A record is *crossed* when the operator's source version sits below the floor
its `to` names and their target reaches it. That is a property of the jump, not
of `from`: a boundary a jump flies straight over still counts, so writing
`from: ">=2.0.0 <3.0.0"` instead of `from: "<3.0.0"` no longer hides the
boundary from anyone starting below 2.0.0. It used to, and a `blocked` record
written that way was invisible to exactly the jump it existed to stop.

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

## Checking your work

```bash
make lint                                   # includes check-upgrade-records
./tools/check-upgrade-records               # just this gate
aicr upgrade-check --from <old> --to <new> --deployer helm
```

The gate runs every well-formedness rule over every record the registry
references. It **asserts no verdict**, so "make the test green" cannot be
satisfied by writing `safe`; flipping a `manual` record to `safe` makes it fail
harder, because `safe` may not carry steps.

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

- [ADR-021: Component Upgrade Safety](../design/021-component-upgrade-safety.md)
- [Upgrade Notes](../user/component-catalog.md#upgrade-notes): the operator-facing prose for migrations
- [Components](component.md): adding and configuring registry components
