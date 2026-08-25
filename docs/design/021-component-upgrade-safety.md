# ADR-021: Component Upgrade Safety

## Status

**Proposed** — 2026-08-21.

Numbering note: 020 is double-claimed at time of writing. Branch `docs/adr-020-resolution-policy` carries `020-recipe-resolution-policy.md`, and [#2334](https://github.com/NVIDIA/aicr/pull/2334) proposes ADR-020 for snapshot agent run isolation. Renumber at merge if 021 is also taken.

Builds on the registry-declared component facts established by `ownsCRDs` ([#2264](https://github.com/NVIDIA/aicr/issues/2264)) and the uniform local-chart bundle layout in `pkg/bundler/deployer/localformat`. It does not change recipe resolution or the deployer contract. It does change bundle layout in two bounded ways: [Decision 4](#decision-4-migration-content-ships-as-an-adjacent-generated-release) adds an optional `-premigrate` folder, and [Decision 7](#decision-7-generated-wrappers-carry-two-versions) adds fields to generated wrapper `Chart.yaml`.

## Problem

A component in the registry ships a new release with a breaking change. The upgrade has prerequisites the operator must satisfy before the new version can safely replace the old one, and some of those prerequisites are not automatable.

Today AICR has no answer. It pins a chart version in `recipes/registry.yaml`, generates a bundle, and the operator applies it. Nothing in the artifact says whether the transition from the version they are running to the version they are about to install is safe, requires work, or is unsupported. The knowledge exists, in release notes, in a maintainer's head, in a GitHub issue, and none of it is machine-readable or attached to the artifact that encodes the version change.

The failure mode is silent. An operator regenerates a bundle after a pin bump, applies it, and discovers the breaking change as an outage.

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
- **Everything in a bundle is a Helm release.** `pkg/bundler/deployer/localformat/doc.go` classifies Kustomize components and raw manifests into generated wrapper charts, so `helm list -A` is a complete, uniform inventory of an AICR-deployed stack. Release name matches the ComponentRef name from `registry.yaml`.
- **Every bundle embeds `recipe.yaml`** (`pkg/bundler/bundler.go:81,2518`), written with deterministic YAML. So a bundle and a recipe are interchangeable inputs to any version comparison.
- **`Masterminds/semver/v3`** is already vendored.

## Decision

### Decision 1: Boundary

**Whoever authors a chart owns its migration hooks.**

- **Upstream charts.** Upstream owns them. Where an upstream chart lacks a hook it should have, the remedy is an upstream contribution. AICR never injects a hook into a chart it did not write.
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

Records are keyed by semver ranges, not explicit version pairs, so they do not go stale on every patch release. The example below is the real nodewright `skyhook.nvidia.com` to `nodewright.nvidia.com` rename, drawn from its [upstream migration guide](https://github.com/NVIDIA/nodewright/blob/main/docs/getting-started/migration.md):

```yaml
# recipes/upgrades/nodewright-operator.yaml
apiVersion: aicr.run/v1alpha1
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
    references:
      - https://github.com/NVIDIA/nodewright/blob/main/docs/getting-started/migration.md

  # 0.20.0 removes the legacy API group outright.
  - from: "<0.20.0"
    to:   ">=0.20.0"
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

Note what the two records together express: a **migration window**. The rename is optional between 0.18.0 and 0.20.0 and mandatory before crossing into 0.20.0. Directional ranges carry that without any new concept.

**Five verdicts.** Three would not be enough to be honest, and the last two fail for different reasons with different remedies:

| Verdict | Meaning |
|---|---|
| `safe` | In-place upgrade works. Nothing to do. |
| `manual` | Upgrade works only after the listed steps. |
| `blocked` | Direct transition unsupported. Either the jump spans more than one block, or it needs an uninstall and reinstall. |
| `unknown` | No record matches this transition. **Not an assertion of safety.** |
| `unversioned` | The two sides cannot be compared at all. **Not an assertion of safety.** |

**A transition may also carry `hooks`.** Steps describe what a human does; `hooks` reference AICR-authored migration manifests that ship as a release beside the component. The field is defined in [Decision 4](#decision-4-migration-content-ships-as-an-adjacent-generated-release), which owns its delivery; it is listed here so the transition schema is complete in one place.

**Fields are validated against the verdict.** A `manual` or `blocked` record MUST carry at least one step, since both are defined by the work they require; a `safe` record MUST carry none. `unknown` and `unversioned` are never authored, only computed. The well-formedness check enforces this, so the verdict table above cannot drift from what a record actually says.

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

**Records are directional by construction.** A record for 0.18 to 0.20 says nothing about 0.20 to 0.18. Range matching MUST NOT match in reverse. A downgrade with no explicit reverse record resolves to `unknown`, never to `safe`. Matching a forward record in reverse would be exactly the "negative check that passes on an ambiguous condition" anti-pattern in `CLAUDE.md`, and it is easy to write by accident.

**A record defines a block, and a jump may span only one.** A record applies when the source version satisfies `from` *and* the target is at or past the lower bound of `to`, so a record describes a boundary the jump crosses rather than one exact hop:

| Jump | A: `<0.18.0` → `>=0.18.0 <0.20.0` | B: `<0.20.0` → `>=0.20.0` |
|---|---|---|
| 0.17.2 → 0.18.1 | applies | no (target below `0.20.0`) |
| 0.17.2 → 0.20.1 | applies | applies |
| 0.19.0 → 0.20.1 | no (`from` excludes 0.19.0) | applies |

If exactly one record applies, its verdict stands. **If more than one applies, the verdict is `blocked`**, and the report names the first block's `to` range as the stopping point.

Composing the steps of both records would be wrong, not merely cautious. Nodewright's mirror controller exists only in 0.18.x and 0.19.x. A jump straight from 0.17 to 0.20 never runs it, so the `skyhook.nvidia.com` CRD is removed with nothing ever mirrored and the objects are unrecoverable. Listing both records' steps would have implied the jump was fine. Row three is the reason the rule needs no "already migrated" special case: an operator on 0.19.0 has done the rename, and A drops out on its own because `from` stops matching.

**The escape hatch is authoring, not a flag.** To permit a wider jump, widen a block's ranges or author a record covering the full span. Because there is no `skippable` field, there is nothing an author can forget to set, and the failure mode of forgetting is a block rather than a silent data loss.

In practice this is quiet. An ordinary component gets one broad block per major line (`from: ">=25.0 <26.0"`, `to: ">=25.0 <26.0"`, `verdict: safe`) and no jump inside that line ever spans. Blocks multiply only where real boundaries exist, which is exactly where spanning should stop you.

**Steps are grouped by deployer, not filtered per step.** `stepsByDeployer` holds one entry per deployer group, each with its own ordered `steps` list; a group that omits `deployers:` applies to every deployer. This is not cosmetic. In the nodewright migration the step *list itself* differs: under GitOps the rename and the legacy deletion collapse into one atomic commit, because a separate `kubectl delete` fights auto-sync and self-heal. Under imperative Helm they are two distinct steps in a load-bearing order.

Grouping rather than per-step filtering matters because **order is part of the instruction**. A reader of a group sees exactly the sequence they must perform. With a single flat list plus per-step deployer tags, the reader has to filter mentally before the ordering means anything, and the numbering they see depends on that filtering. Rendering the imperative "delete legacy CRs" step to an Argo CD user with a footnote saying not to do it would be worse still.

This is also where AICR adds value no upstream document can. The nodewright guide must hedge across every deployer, explaining Argo behavior to Flux users and vice versa. AICR knows which deployer you chose, so it renders the one path that applies to you.

**Step `id` is unique within its group.** The same logical action may reuse an id across groups (`rename-crs` appears in both above) because each group is a self-contained sequence; a consumer addresses a step as (deployer, id). A group may not list a deployer that another group in the same transition already claims.

**Deployer identifiers are the canonical `--deployer` values** from `pkg/bundler/config/config.go`: `helm`, `helmfile`, `argocd`, `argocd-helm`, `flux`. There are five. `localformat` is the internal bundle-layout package every deployer consumes, not a selectable deployer.

**`reason` is separate from `description`.** The description says what to do; the reason says why the order or the shape matters. Deleting Skyhooks before DeploymentPolicies is not stylistic, it is what the admission webhook requires. A step whose reason is missing is a step somebody will eventually "optimize".

**`reversible` is a boolean; `reversibleNotes` carries the conditions.** The only machine-actionable question is whether a supported path back exists. Nodewright's answer is yes, but only until legacy cleanup runs (default 24h), after which rollback re-applies every package from scratch. Conditions like that belong in the note rather than in extra enum states, which would add cases to validate and render without adding anything a consumer could act on: a time window is legible only from the note either way.

The cost to accept: a consumer reading the boolean alone gets an over-optimistic answer for a time-bounded case. Renderers therefore always print `reversibleNotes` next to the boolean and never surface the boolean by itself.

Either way the operator needs this *before* upgrading, which is why it is a property of the forward transition rather than something discovered during a rollback.

Like `ownsCRDs`, these records are human-audited assertions. They carry the same strength and the same failure mode: a wrong record grants false confidence. [Decision 9](#decision-9-uat-covers-upgrade-and-rollback) addresses that directly.

### Decision 3: Ownership classes and what AICR can see

A single component migration can span three ownership classes, and they have different answers. The nodewright rename spans all three.

| Class | Example | Versioned by | Who migrates it |
|---|---|---|---|
| Upstream chart | `nodewright-operator` | Upstream chart version | `helm upgrade` migrates it. AICR still records the transition. |
| AICR-authored content | `nodewright-customizations` | The AICR release | AICR maintainers rewrite the manifests; users get it by regenerating. |
| User-authored resources | A user's own `Skyhook` CRs | Not AICR's at all | Only the user. AICR cannot fix them. |

**AICR-authored components are keyed on the AICR version.** `nodewright-customizations` is manifest-only: `defaultRepository: ""`, no `defaultVersion`, and its content is five `Skyhook` CRs that AICR itself authors under `recipes/components/nodewright-customizations/manifests/`. There is no upstream version to compare, so the matcher would have nothing to match on. Its transitions are therefore keyed on AICR version ranges, which is exactly what [Decision 7](#decision-7-generated-wrappers-carry-two-versions) already stamps into the wrapper's `version:` field. Without this rule, every manifest-only component is invisible to the matcher.

The same rule covers local-path Kustomize components. `writer.go:516-519` rejects a `Tag` without a `Repository`, so a local-path kustomization has no version of its own either, and AICR's release is likewise the only thing that changes it.

This also means AICR-authored content is not a secondary concern. When AICR authors the resources a component consumes, an upstream rename becomes AICR's migration to perform and AICR's contract change to announce.

**Helm does most of class 2 for free, with one hazard it cannot handle.** Because AICR wraps these manifests in a generated chart, a rename drops the old resource from the chart and adds the new one, so `helm upgrade` prunes and creates with no hook at all. The exception is adoption: the nodewright mirror controller *pre-creates* the `NodeWright` object, so Helm attempts to create a resource that already exists and carries no release ownership, and fails with an ownership-metadata error. Helm 3 adopts only an object already carrying `app.kubernetes.io/managed-by: Helm` and matching `meta.helm.sh/release-*` annotations; the mirror stamps `nodewright.nvidia.com/mirrored-from` instead.

Declarative metadata cannot reach this. `helm.sh/resource-policy: keep` and sync-wave ordering do not annotate a live object. Adoption requires writing annotations onto a resource that already exists, which is inherently mutating. This is the concrete case AICR must own under [Decision 1](#decision-1-boundary), and it ships as an adjacent release under [Decision 4](#decision-4-migration-content-ships-as-an-adjacent-generated-release).

The cost is real and must be paid: any hook Job needs a digest-pinned image under the ADR-006 policy, and it must appear in the BOM like every other image, or a migration Job becomes the one unpinned, unscanned image in the bundle.

**Transitions may be cross-component.** A `nodewright-operator` version change forces a `nodewright-customizations` content change. A record therefore may name other components it affects, so the report groups the coupled change as one migration rather than two unrelated rows.

**Class 3 is the highest-severity case, and only online mode can see it.** If a user authored their own `Skyhook` CRs outside AICR, the rename ships, their objects are untouched, and the 0.20.0 CRD removal cascade-deletes them. AICR has no offline visibility into resources it never generated.

So a transition record may declare the API groups and kinds it affects:

```yaml
    affectedResources:
      - group: skyhook.nvidia.com
        kinds: [Skyhook, DeploymentPolicy]
```

In online mode the check lists matching objects in the cluster that carry no AICR or Helm ownership and reports them as at-risk. This is a read-only list operation, not a new assertion format, and it is **advisory**: it warns, it does not fail the gate, because AICR blocking an upgrade over resources it does not own is a claim it has not earned.

Offline mode cannot answer this question at all. It says so explicitly rather than staying silent, since silence here reads as safety.

### Decision 4: Migration content ships as an adjacent generated release

[Decision 1](#decision-1-boundary) makes AICR responsible for migrating content in charts it generates. It does not say how that content reaches a cluster, and the obvious answer does not work: the wrapper chart is synthesized at bundle time from `manifestFiles`, so there is no checked-in `templates/` to add a hook to, and Kustomize components have the same shape (`kustomize build` output wrapped as `templates/manifest.yaml`).

Content lives beside the component's existing manifests:

```text
recipes/components/nodewright-customizations/
  manifests/
    tuning.yaml
  migrations/
    adopt-mirrored-crs.yaml        # new
```

and is referenced from the transition record:

```yaml
    hooks:
      - file: migrations/adopt-mirrored-crs.yaml
        phase: pre-upgrade
```

The bundler emits it as a separate generated chart folder, ordered immediately before the component it serves:

```text
006-nodewright-operator/
007-nodewright-customizations-premigrate/     <- new
008-nodewright-customizations/
```

**This mirrors an injection the bundler already performs.** `localformat` emits `(NNN+1)-<name>-post/` as a `KindLocalHelm` folder when a component carries both an upstream chart and raw manifests. `-premigrate` is the same move in the other direction, so the layout gains no new concept.

Three properties follow:

- **Uniform across component kinds.** Manifest-only, Kustomize, and upstream-chart components are treated identically, because the migration release does not depend on whether the component itself has a generated chart. Decision 1's rule holds with no special case: AICR never touches an upstream chart, it emits a release *beside* it.

  Uniformity is reasoned, not demonstrated, for one of the three. The Kustomize path is fully implemented (`pkg/bundler/deployer/localformat/kustomize.go`, in-process krusty), but **the registry contains zero Kustomize components today** and no overlay references one. So that limb is untested in practice and should be treated as a design claim until a Kustomize component exists.
- **Ordering is folder order**, not Helm hook phase semantics that would have to behave identically across all five deployers.
- **It is an ordinary release.** Visible in `helm list`, uninstallable, and subject to the same checksum, BOM, and signing paths as every other folder.

Helm hook annotations still apply *within* the folder where finer ordering is needed. Flux's helm-controller runs Helm hooks and Argo CD translates `helm.sh/hook` into its own semantics, so one annotation set covers most of the deployer matrix rather than six bespoke renderings.

The image-pinning cost from [Decision 3](#decision-3-ownership-classes-and-what-aicr-can-see) applies here: any Job in a `migrations/` file needs a digest-pinned image and a BOM entry.

### Decision 5: One matcher, three independent axes

One matcher runs over a `component -> version` table per side. Three separate inputs decide how those tables are built and how the result is rendered. They are independent, which is why "offline mode" and "online mode" are not the right framing.

**Where the `from` table comes from.** `--from <recipe|bundle|cluster>`. Artifacts are read with no cluster access, which is the CI and GitOps path and the whole feature for anyone who keeps recipes in git. `cluster` reads installed release inventory through the Helm SDK, keyed on release name matching component name. This is authoritative for *which release version is installed*, including when that has drifted from the artifact in git. It is **not** a live view of cluster resources: Helm records what it last applied, so a hand-edited resource leaves the release metadata unchanged. `--to` is always an artifact, since there is nothing to upgrade *to* in a cluster. Because every bundle embeds a deterministic `recipe.yaml`, the recipe and bundle forms share one code path.

**Whether a cluster scan runs.** The at-risk scan for unmanaged resources ([Decision 3](#decision-3-ownership-classes-and-what-aicr-can-see)) needs a cluster no matter where the `from` table came from, so it is its own axis rather than a property of `--from`. It is implied by `--from cluster` and available alongside artifact comparison via `--scan-cluster`. Comparing two bundles while scanning a live cluster for unmanaged `Skyhook` objects is a legitimate combination, and the two-mode framing had no name for it.

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

Shelling out to `helm list -A -o json` is **not** a viable alternative. [Decision 7](#decision-7-generated-wrappers-carry-two-versions) makes `aicr.run/component-version` the field the matcher reads, and `helm list` returns chart and app version but not chart annotations. Reading those needs `helm get metadata` per release, which is N+1 subprocesses and a Helm-CLI output format to track, or the SDK.

Vendoring the SDK is a substantial change on its own and may land as its own PR ahead of this work; sequencing is an [open question](#open-questions).

### Decision 6: Strict by default, semver-calibrated

The check is strict by default. It fails on `manual`, on `blocked`, on `unversioned`, and on `unknown` across a **breaking boundary**. It passes `safe`, and passes `unknown` within a non-breaking boundary.

A breaking boundary is a major bump, **or a minor bump while the major version is 0**. Semver gives no stability guarantee below 1.0, so `0.18 -> 0.19` may break exactly as `1.x -> 2.x` may. Treating 0.x minors as non-breaking would have passed an unassessed `0.17.2 -> 0.18.1`, which is this ADR's own worked example and an entire API-group rename.

`unversioned` fails unconditionally because the semver calibration below has nothing to calibrate on: there is no boundary to classify. It is a blind spot rather than an unassessed transition, and the remedy is in the operator's hands, since pinning a comparable ref resolves it. The escape-hatch flag covers anyone who accepts the blind spot deliberately.

The calibration uses the signal semver already carries. Without it, a matrix that starts at zero coverage would fail on every component, and strict mode would sit disabled forever. With it, an unassessed 1.x to 2.x still stops you on day one.

An escape-hatch flag disables the gate. Name is an [open question](#open-questions).

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
- **`aicr.run/component-version` is the payload version**, and it is the only field the matcher reads. Free-form, so a Kustomize `defaultTag` like `release-1.4` does not have to masquerade as semver.
- **`appVersion` also carries the payload version.** Conventional Helm usage, and it makes plain `helm list` output readable for a human even though the matcher ignores it.

**Dev-build fallback is mandatory.** `pkg/cli/root.go:38` sets `versionDefault = "dev"`, which is not valid SemVer 2, and Helm rejects it for `Chart.yaml` `version:`. The bundler normalizes non-release versions to `0.0.0-dev` and strips a leading `v`. Without this, `make dev-env` and Tilt break. This is an explicitly tested case, not a discovered one.

Reproducibility is unaffected. The AICR version already travels in every bundle through `recipe.yaml`, so bundle bytes already vary by AICR version. `pkg/bundler/stock_render_parity_golden_test.go:41` already pins the builder and bundler versions "so the digest is a pure function of the catalog and the render", and stamping rides that existing pattern.

### Decision 8: Close the `ownsCRDs` deployer gap

`ownsCRDs` is consumed by exactly one deployer, `pkg/bundler/deployer/flux/flux.go:994`. On helm, helmfile, argocd, and argocd-helm, CRDs still sit at day-one schema after every upgrade. That is a live upgrade defect, not a hypothetical one.

This ADR records `ownsCRDs` as the precedent the transition-record design follows, and names the deployer gap as in-scope work. It may well ship as its own issue and PR rather than riding the same change.

### Decision 9: UAT covers upgrade and rollback

A single release-to-release lane, up then down:

1. Deploy the previous AICR release's bundle.
2. Upgrade to the version under test. Assert component health.
3. `helm rollback`. Assert component health again.

`uat-run.yaml` already takes an `aicr_version` input, and a previous AICR release naturally carries older component pins. So one lane exercises real component version transitions *and* AICR's own contract stability, with no fixture machinery to build. The existing per-component health checks are the pass criteria.

This is the joint that makes the whole design trustworthy. Without it, every `safe` and every `reversible: true` is an unverified human assertion carrying the same false-confidence risk as a wrong `ownsCRDs` flag. With it, a verdict is a tested claim.

Rollback deserves its own assertion because **rollback is not the inverse of upgrade**. `helm rollback` restores the previous release's manifests, but it cannot undo CRD schema changes (Helm skips `crds/` on rollback exactly as on upgrade), data migrations already executed, CR stored-version rewrites, or anything node-level. A `safe` upgrade does not imply a safe rollback, and only a test can tell the difference.

## Example

An operator regenerates after AICR ships the nodewright rename. The bundle was built with `--deployer argocd`, so the check infers that from `--to` and only the GitOps path renders.

```console
$ aicr upgrade-check --from ./bundles-v0.16.0 --to ./bundles-v0.17.0 --scan-cluster

COMPONENT                    FROM      TO        VERDICT   NOTES
nodewright-operator          0.17.2    0.18.1    manual    1 step, reversible 24h
nodewright-customizations    v0.16.0   v0.17.0   manual    coupled: see nodewright-operator

nodewright-operator 0.17.2 -> 0.18.1  (manual, reversible: yes)
  skyhook.nvidia.com is renamed to nodewright.nvidia.com. The mirror
  controller is read-only on legacy objects, so GitOps controllers observe
  no drift from the operator upgrade itself.

  Also changes: nodewright-customizations (AICR-authored CRs, rewritten
  in this release)

  PRECONDITION
    All Skyhook objects are in `complete` status with no nodes in
    progress. Upgrading mid-rollout hands the migrated operator a stage
    in flight.

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
| Registry well-formedness (Go) | Runs against **real** `recipes/upgrades/*.yaml`. Asserts shape only: files parse, ranges are valid semver, no record matches its own reverse, the referenced component exists. Asserts no verdict, so it survives pin bumps by construction. |
| KWOK (new, this ADR) | Synthetic fixture component with two trivial chart versions. Install, upgrade, roll back, and confirm the check reads the right versions back. No network chart pull, per-PR speed, unaffected when a real pin moves. |
| UAT (new, [Decision 9](#decision-9-uat-covers-upgrade-and-rollback)) | Release-to-release against real clusters. The only layer that validates a real component's `safe` or `reversible` claim. |

The KWOK lane extends existing infrastructure rather than building new. `kwok/scripts/validate-scheduling.sh` already deploys AICR bundles through a real `helm install` path against simulated nodes, and already enumerates releases with `helm list -A -o json` (lines 367 and 571).

It deliberately proves nothing about real component upgrades. It proves the mechanism: version detection through the wrapper annotations from [Decision 7](#decision-7-generated-wrappers-carry-two-versions), verdict lookup, and rollback read-back.

## Acceptance Criteria

1. `aicr upgrade-check --from <recipe> --to <recipe> --deployer <name>` reports a verdict for every component whose version changed, and exits non-zero when any verdict is `manual`, `blocked`, or `unversioned`, or `unknown` across a major boundary.
2. A downgrade with no explicit reverse record reports `unknown`, never `safe`.
3. A jump spanning more than one block reports `blocked` and names the stopping point, rather than composing the crossed records' steps.
4. `--from cluster` against a KWOK-installed bundle reports the same versions the artifact path reports for the same artifacts.
5. Recipe-to-recipe with no `--deployer` and no bundle to infer from exits non-zero asking for the flag, rather than defaulting or rendering every deployer's steps.
6. Wrapper charts expose the payload version in `aicr.run/component-version`, and a `dev` build still produces a Helm-valid `Chart.yaml`.
7. A malformed or reverse-matching record in `recipes/upgrades/*.yaml` fails `make lint` rather than being silently skipped.
8. `make qualify` passes.

## Open Questions

- **Is the boundary premise true?** Decision 1 assumes most upgrades are already handled by the component's own chart. That is a structural argument, not a measurement, and nobody has checked it against this registry. An audit of past `registry.yaml` pin bumps, asking for each whether it required operator action beyond `helm upgrade`, would either support the boundary or overturn it. If a large fraction did require action, the rejected mutating-hook alternative deserves reopening.
- **When to vendor `helm.sh/helm/v4`.** Online mode requires it, and it is a substantial change in its own right: it lands in `make scan`, the license allowlist, api-diff, and the vendor tree. It may be worth landing as its own PR before this work rather than inside it, which would also let the offline check (steps 1 through 5) ship independently. Sequencing undecided.
- **Escape-hatch flag name.** `--force-updated` was considered and rejected: it reads as forcing an update rather than suppressing a check. Candidates: `--skip-upgrade-check` (disables the gate entirely), `--allow-unknown-upgrades` (relaxes only the `unknown` case, keeping `manual` and `blocked` fatal). The second is narrower and probably better, but it needs a name for the first case too.
- **No pinning gate covers Kustomize.** `bom-pinning-check` in `make lint` verifies that every *Helm* component has a pinned chart version per ADR-006; it runs `tools/bom`, which renders charts, so a Kustomize `defaultTag` is unchecked. The `unversioned` verdict means a branch ref can no longer produce a silent false negative, but it will fail strict mode until someone repins. Rejecting mutable Kustomize refs at lint time would catch it at authoring instead, and probably belongs as an ADR-006 amendment rather than here. Latent today: the registry has no Kustomize components.
- **Kustomize `Chart.yaml` version.** Decision 7 puts the payload version in an annotation specifically because Kustomize tags may not be semver. Confirm no deployer path reads wrapper `version:` for anything meaningful before changing it.
- **Coverage ratchet.** Matrix coverage starts at zero. A maintainer-side CI gate (fail when a `registry.yaml` pin bump crosses a major boundary with no matching record) is probably needed for records to ever get authored. Not decided here.
- **Values-drift detection is out of scope.** Validating `recipes/components/*/values.yaml` keys against each chart's `values.schema.json` addresses the silent-misconfiguration gap named in Context, but it belongs on the PR that bumps the pin and changes the values, not at `upgrade-check` time (per review on #2343). Tracked separately.

## Alternatives Considered

**AICR-injected hooks in upstream charts.** Registry records carry Job manifests that AICR injects as pre-upgrade or post-upgrade hooks into any component's chart, riding the existing `PreManifestFiles` machinery (`pkg/recipe/metadata.go:144-155`).

Rejected on two grounds. First, only a narrow slice of real migrations is Job-shaped: CRD stored-version migration and data or schema migration. Node-level changes, drain windows, values-schema renames, component replacement, and operational prerequisites are all outside what a Job can do. Second, for the slice that *is* Job-shaped, the upstream chart is the right owner, and Helm already provides the hook. AICR rendering a parallel hook mechanism duplicates a facility one layer down and takes on permanent ownership of migration logic it did not write. Where an upstream chart lacks a hook it should have, the remedy is an upstream contribution.

**This rejection is scoped to upstream charts.** [Decision 1](#decision-1-boundary) permits hooks in charts AICR generates itself, where the "duplicates a facility one layer down" argument has no referent because there is no layer down. What stays rejected is AICR injecting hooks into charts it did not write.

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
- `reversible: false` surfaces one-way upgrades before they happen, which is when the information is actionable.
- Online mode gains a uniform installed-component inventory, useful well beyond this feature.
- Wrapper charts stop reporting a fictional `0.1.0`, which improves plain `helm list` output for humans regardless of the check.

**Negative and risky.**

- **Vendoring `helm.sh/helm/v4`** is a large dependency for one feature and lands in `make scan`, api-diff, and the vendor tree. Licensing is probably fine but is not free: `license-check` allows only MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC, and Zlib, and clears MPL-2.0 only through ten explicit per-import-path ignores, all HashiCorp. Helm is Apache-2.0 and its usual MPL-2.0 touchpoints (`errwrap`, `go-multierror`) are already among them, but the Makefile is explicit that "unrelated MPL-2.0 deps still fail closed for review" and that ignores must not be added to work around the policy. Helm v4's full dependency tree has not been resolved against that list.
- **Coverage starts at zero.** Every transition is `unknown` until someone authors a record. Without a coverage ratchet the matrix may never get populated, and the feature degrades to an elaborate way of printing "unknown".
- **Records are human assertions.** A wrong `safe` is worse than no record, because it converts uncertainty into false confidence. Decision 9 is the mitigation, and it only mitigates what UAT actually exercises.
- **UAT cluster time grows.** A release-to-release lane adds a second full deploy and a rollback to every cell it runs on, against an already-contended reservation pool.
- **`helm diff` noise.** With wrapper `version:` tracking the AICR version, a release that changes nothing material still shows a chart version bump. Rendered manifests stay identical so there is no resource churn, but `helm_diff` is pinned in `.settings.yaml` and used in CI.
- **Strict-by-default will surprise people.** Existing pipelines that regenerate bundles will start failing on unassessed major bumps. That is the intended behavior, and it still needs a migration path and a release note.

## Implementation Plan

Ordered so each step is independently useful and independently revertible.

1. **Wrapper chart versioning** (Decision 7). Template change, dev-build normalization, golden updates. No new feature depends on it landing first, but online mode is wrong without it.
2. **Transition record schema and loader.** `recipes/upgrades/<component>.yaml`, the `upgrades.file` registry field, semver range matching with strict directionality, and a lint gate rejecting a record that matches in reverse.
3. **Offline check.** `--from`/`--to` over recipes and bundles, table and JSON output, strict-by-default gating with the escape hatch. This is the whole feature for CI and GitOps.
4. **Bundle rendering.** Transition records for the resolved component set render into the bundle README, filtered to the bundle's deployer, so operators who never run the command still see the steps that apply to them.
5. **`-premigrate` folder emission** (Decision 4). Mirrors the existing `-post` injection in `localformat`. Only needed once a real migration requires a hook; the nodewright adoption case is the first.
6. **Online mode.** Helm release inventory read path, on the vendored SDK. Blocked on the vendoring landing.
7. **KWOK upgrade and rollback lane** (Testing Strategy). Synthetic fixture component, per-PR speed. Catches read-path regressions long before a real cluster run would.
8. **UAT upgrade and rollback lane** (Decision 9).
9. **`ownsCRDs` deployer gap** (Decision 8). Likely its own issue.

Authoring the first real records, starting with nodewright-operator, should happen alongside step 3 so the check is exercised against real data rather than fixtures.
