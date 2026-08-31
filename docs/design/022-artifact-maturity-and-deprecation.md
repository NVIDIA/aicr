# ADR-022: Per-Kind Artifact Maturity and the v1 Deprecation Policy

## Status

Accepted on 2026-08-26 by ratification in
[#2373](https://github.com/NVIDIA/aicr/pull/2373).

Revised 2026-08-27 in [#2418](https://github.com/NVIDIA/aicr/pull/2418): §2's
`ComponentUpgrades` row drops its pre-cut alpha branch and starts at
`aicr.run/v1beta1` unconditionally, and §3 binds N, N+1, and N+2 to concrete
AICR releases. Release N's reader support merged in
[#2404](https://github.com/NVIDIA/aicr/pull/2404) and ships in v0.21, so `main`
already accepts `aicr.run/v1beta1`; a kind introduced now can start at its
target without violating §7's rule that a new kind is never stamped with a
version the tree does not accept, and without a shipped alpha version to retire.

Revised 2026-08-28 for [#2421](https://github.com/NVIDIA/aicr/issues/2421): §3
states which clause governs a catalog kind that arrives through the direct
recipe-input path, and scopes the surviving empty-`apiVersion` tolerance to
`RecipeResult`.

Amends [ADR-011](011-artifact-apiversion-policy.md): §1 keeps `pkg/header` as the
single source of version strings but replaces its single-version alias rule;
§3 becomes kind/schema-scoped, covers AICR catalog inputs, and retires its
empty-`apiVersion` tolerance through the initial migration; §4 is replaced by
maturity-specific windows; §2 stands unchanged. Coordinates with the proposed
`ComponentUpgrades` artifact in
[#2343](https://github.com/NVIDIA/aicr/pull/2343). Builds on
[ADR-015](015-recipe-configuration-profiles.md), which already introduced
kind-scoped version evolution as an amendment to ADR-011.

## Problem

Every artifact AICR generates today carries an alpha `apiVersion`. ROADMAP
[§1](../../ROADMAP.md#1-defensible-api-stability) promises a frozen, diff-gated surface at v1,
and the Kubernetes convention that `v1alpha2` invokes — may be dropped or changed
without notice — is the opposite of that promise. Two alpha schema tracks coexist:
`aicr.run/v1alpha2` for general kinds and default/catalog forms, and
`aicr.run/v1alpha3` for profile-bearing `RecipeMetadata` and `RecipeResult`.

A third case exists alongside those two: artifacts predating the `apiVersion`
field may carry no value at all. The snapshot, recipe, and criteria loaders
tolerate that explicitly (`snap.APIVersion != "" &&
!header.IsSupportedAPIVersion(...)` and its equivalents in `pkg/recipe`).
`AICRConfig` has always required an exact, non-empty value. ADR-011 §3 made the
legacy tolerance deliberate where it exists.

Four questions have no recorded answer:

1. **What `apiVersion` do artifacts carry at v1 GA?**
2. **What does a bump owe the previous version?** ADR-011 §4 says dual-accept with
   a transition window. The `pkg/header` godoc says the opposite — a hard break
   with no window — and `IsSupportedAPIVersion` implements the godoc while
   `IsSupportedRecipeResultAPIVersion` implements the ADR. Both statements are
   unconditional, which is the actual gap: neither says *when* each applies.
3. **What `apiVersion` does a newly introduced kind start at?** Nothing answers
   this, so new kinds guess.
4. **Does the empty-`apiVersion` tolerance survive v1?** It is a deliberate
   backward-compatibility affordance today and an unguarded hole tomorrow.

## Non-Goals

- **REST path versioning.** ADR-011 scopes non-artifact `apiVersion`s as a
  non-goal and that stands. Whether the stable REST family is `/v1` or `/v2` is
  recorded separately under #2112.
- **Tagging v1.0.0.** This ADR is prerequisite work, not the release.
- **The CLI, REST, Go SDK, and bundle-layout freezes.** Those are #2111, #2112,
  #2113 under epic #2370. This ADR settles only what the artifacts are called and
  what a change to them owes consumers.

## Decision

### 1. Project v1 and artifact `v1` are separate axes

AICR reaching v1.0.0 does not require every artifact kind to reach
`aicr.run/v1`. ROADMAP §1 asks for a committed baseline, a CI diff-gate, and a
deprecation channel. A gate over a `v1beta1` schema is a real gate: it catches
*unintended* breakage, which is what the freeze promises. The maturity string
governs *intended* breakage. It is selected by wire kind and current schema
track, because one kind may intentionally serve distinct schemas during a
transition.

This mirrors Kubernetes, which shipped 1.0 with beta APIs and serves beta
alongside GA in every release since.

### 2. Per-kind maturity map

Rows are keyed by wire kind and current `apiVersion` where a kind serves more
than one schema. This makes the target deterministic for a real file without
pretending that the legacy `Recipe` input is distinct from the canonical
`RecipeResult` kind it normalizes to.

| Wire kind or artifact | Today | Target | Rationale |
|---|---|---|---|
| `Snapshot` | `v1alpha2` | `aicr.run/v1` | Settled shape; the first artifact an integrator reads |
| `RecipeResult` (default resolved recipe) | `v1alpha2` | `aicr.run/v1` | Public resolved artifact emitted to bundles; legacy `Recipe` input normalizes to this kind |
| `RecipeCriteria` | `v1alpha2` | `aicr.run/v1` | Public recipe-resolution input shared by the CLI, REST API, and Go client |
| `BundleProvenance` (`provenance.yaml`, `localformat.ProvenanceAPIVersion`) | `v1alpha2` | `aicr.run/v1` | Bundle-root audit document consumed by downstream tooling |
| `AICRConfig` | `v1alpha2` | `aicr.run/v1beta1` | Actively growing: #2026 bound 2 of 5 spec sections, #2245 binds the rest. Do not freeze a schema mid-expansion |
| `RecipeMetadata`, `RecipeMixin` (catalog) | `v1alpha2` | `aicr.run/v1beta1` | Authoring schema exercised by 105 shipped catalog files (101 overlays, 4 mixins) |
| `ComponentRegistry` | `v1alpha2` | `aicr.run/v1beta1` | Required root of an external `--data` catalog; authoring schema consumed by bundling and validation |
| `RecipeMetadata`, `RecipeResult` (profile-bearing) | `v1alpha3` | `aicr.run/v1beta2` | Newest (ADR-015), 2 overlays, opt-in via profiles; remains distinct from ordinary `RecipeMetadata` |
| `ComponentUpgrades` (proposed by [ADR-021](021-component-upgrade-safety.md)) | Not shipped | `aicr.run/v1beta1` | New, still-evolving authoring schema; its loader and records are not implemented. Starts at its target, with no alpha version to retire |

`BundleProvenance` here means the bundle-root `provenance.yaml` document emitted
by deployers through `localformat.WriteProvenance`, not the in-toto provenance
predicate excluded from this ADR's scope. The producer emits the version in its
§2 row; a consumer gates on the `BundleProvenance` kind and `apiVersion` before
parsing the document.

The stable artifact path is at `v1`: `Snapshot`, the default resolved
`RecipeResult`, `RecipeCriteria`, and bundle provenance. `pkg/client/v1` is the
Go facade that consumes those artifacts, but this statement does not select a
REST path family; `/v1` versus `/v2` remains #2112. Local
authoring/configuration and opt-in or emerging contracts can sit at beta
honestly.

Maturity also constrains shared nested wire types. A field or nested type
reachable from any `aicr.run/v1` document has GA stability obligations even
when a beta document reuses the same Go type. Breaking beta evolution is
therefore confined to beta-only fields that are not reachable from a GA
document. If a beta change needs a different shape for a shared type, the
implementation must first introduce version-specific wire DTOs and an explicit
conversion boundary; changing the shared type in place would break the GA
contract.

The `v1alpha3` schema does **not** converge into the promoted `v1alpha2` schema.
It becomes `v1beta2`, while ordinary catalog `RecipeMetadata` becomes
`v1beta1`; the distinct values preserve strict decoding and the bidirectional
profile/`apiVersion` validation in `pkg/recipe/profile.go`. The same
`RecipeResult` wire kind has a default `v1` contract and a profile-bearing
`v1beta2` contract. `aicr.run/v1` is not the successor to profile
`aicr.run/v1beta2`. The profile schema's eventual GA identifier is explicitly
deferred to its promotion decision and cannot reuse `aicr.run/v1` while the
default schema remains served.

### 3. The initial migration is staged before v1

Alpha owes no deprecation window under §4, but independently persisted
artifacts still require an operational upgrade and rollback path. The initial
alpha-to-target migration therefore uses a bounded three-release sequence:

1. **Release N — readers first.** Add every target in §2 to the appropriate
   kind/schema-scoped read gate while emitters keep writing the alpha versions.
   Readers accept both tracks. Existing empty-value tolerances remain where
   they already exist.
2. **Release N+1 — emitters switch.** Flip every producer to its §2 target;
   update or regenerate committed artifacts, fixtures, examples, and docs; and
   keep both alpha and target values readable. A rollback to Release N remains
   usable because that release already understands the target values.
3. **Release N+2 — alpha retires.** Remove the alpha values and the legacy
   empty-value exceptions from all in-scope read gates. This is the earliest
   release that may satisfy the artifact-version part of the v1 GA gate.

The sequence is bound to concrete AICR releases:

| Release | Reads | Emits | Tracking |
|---|---|---|---|
| N — v0.21 | alpha + target | alpha | [#2404](https://github.com/NVIDIA/aicr/pull/2404) |
| N+1 — v0.22 | alpha + target | target | [#2416](https://github.com/NVIDIA/aicr/issues/2416) |
| N+2 — v0.23 | target only | target | [#2417](https://github.com/NVIDIA/aicr/issues/2417) |

Releases before N accept only the alpha values, so a target-stamped artifact
does not load on v0.20 or earlier. `RELEASING.md` and
[`docs/integrator/data-extension.md`](../integrator/data-extension.md#catalog-and-binary-compatibility)
carry the consumer-facing form of this table.

Before N+1 starts emitting target artifacts, every producer and consumer in a
pipeline must run Release N or later. Mixed pipelines containing an older
binary are unsupported. Artifact-bearing HTTP boundaries follow this sequence
too: the OpenAPI `BundleRecipeRequest`, bundle handlers, and emitted artifact
headers change their accepted values with the artifact gates. This does not
select the REST path family; that remains #2112.

Generated `Snapshot`, `RecipeResult`, and `BundleProvenance` artifacts must be
recaptured or regenerated during the migration. User-authored `AICRConfig`,
external `RecipeMetadata`/`RecipeMixin` catalogs, and external
`ComponentRegistry` files must be edited by their owners; "regenerate" does not
describe those inputs.

The N+2 retirement is intentional for read-only consumers too. An alpha
snapshot used as an `aicr diff --baseline` input and an alpha `recipe.yaml`
embedded in a bundle no longer load in N+2. AICR has no conversion layer, so an
immutable archive whose source cannot be recaptured remains readable only with
a retained N or N+1 binary. The N-through-N+1 interval is the migration window.

`ComponentUpgrades` does not participate in this sequence regardless of when it
lands. Its records, loader gate, examples, and tests start at `aicr.run/v1beta1`
directly, because N already accepts that value and the kind has no shipped alpha
version to retire.

**The empty-`apiVersion` tolerance retires at N+2.** ADR-011 §3 accepts an
empty value in the snapshot, recipe, and criteria loaders for artifacts
predating the field; `AICRConfig` already rejects it. After N+1 emits only
target versions, an unversioned artifact would otherwise pass those gates
unchallenged — the fail-open shape §8 exists to close.

**A catalog kind is governed by §8 on every path it can arrive by.** The
tolerance above is scoped by wire kind, not by entry point. A `RecipeMetadata`
reaching AICR as a direct recipe input (`aicr bundle -r overlay.yaml`,
`aicr validate -r overlay.yaml`) is the same catalog document it would be inside a
`--data` tree, so it is held to the same fail-closed authoring gate the catalog
scanner applies, including the rejection of an empty value. §3 step 1's
"existing empty-value tolerances remain where they already exist" does not
extend a tolerance to a document the catalog path already rejects; where the two
paths disagreed, the stricter one governs.

This resolves [#2421](https://github.com/NVIDIA/aicr/issues/2421), where
`pkg/recipe/loader.go` short-circuited on an empty value before it inspected the
kind, so a headerless overlay was rejected from a `--data` tree and silently
hydrated when passed with `-r`. Closing it in Release N rather than deferring to
N+1 keeps the two paths from disagreeing across the release where the emitter
switch rewrites every committed header. The empty-value tolerance survives for
`RecipeResult` inputs only, and retires with the rest at N+2.

### 4. The deprecation window is conditional on the level being retired

This **replaces ADR-011 §4**, whose dual-accept rule was stated unconditionally.
The window owed is a function of the maturity of the version being removed:

| Level being retired | Obligation |
|---|---|
| Alpha | None. May be removed in any release, no prior notice |
| Beta | Readable for **2 releases** after deprecation |
| GA | Not removed within a major version of the **AICR release**, i.e. no earlier than the next `vMAJOR` |

"Release" and "major version" here mean the **AICR release axis**
(`vMAJOR.MINOR.PATCH` per `RELEASING.md`), not the artifact version. §1 separates
the two axes; this window is measured on the project's release axis, because
that is the clock a consumer upgrades against. Concretely: an `aicr.run/v1`
kind deprecated during `v1.x` may first be removed in `v2.0.0`.

The `pkg/header` godoc gains this condition. Its hard-break language is correct
*for alpha* and must not be read as the general rule.

**Two releases is deliberate, not inherited.** The Kubernetes equivalent is 9
months or 3 minor releases; at AICR's ~2-week cadence that would be ~18 releases.
A short window means the read gate carries at most one retired version, which
keeps the accept-known logic trivial. The cost is that a consumer who upgrades
less than monthly can miss a window.

Transitions do not overlap for the same wire kind and schema track: a second
bump cannot start until the prior version's read window has closed. Independent
kinds and schema tracks may transition concurrently. This invariant is what
limits each gate to one retiring version.

### 5. Never deprecate toward a less stable version

GA may replace beta and alpha. Beta may replace beta and alpha, never GA. Alpha
may replace only alpha. This is what makes a mixed-maturity map safe: promoting
`Snapshot` to `v1` is a one-way door for `Snapshot` and binds no other kind.

### 6. Split each future bump across two releases

**Applies to bumps that owe a window under §4** — beta and GA. The initial
alpha-to-target migration uses the separate, shorter sequence in §3; it stages
readers for operational safety without creating an ongoing alpha deprecation
entitlement.

Release N adds the new version to the read gate. Release N+1 flips the emitter.
A consumer who rolls back one release can still read what they generated.

AICR has no conversion layer — a file is one version, take it or leave it — so
this matters more here than in Kubernetes, where the apiserver converts between
served versions.

**This composes with §4 rather than duplicating it.** For a beta kind: N adds
read support, N+1 emits the new version and deprecates the old, and N+2 and N+3
are the two subsequent releases in which the old remains readable. Both versions
are readable across four releases, N through N+3; the new version is emitted
while the old remains readable across three, N+1 through N+3. The read-support
lead and the post-deprecation window are different clocks.

For a GA kind: N adds read support, N+1 emits the new version and deprecates the
old, and the old version remains readable for the rest of the current AICR
major. It may be removed no earlier than the first subsequent `vMAJOR` release.
The beta N+3 calculation never shortens that GA boundary.

### 7. A new kind starts on the current track

A kind introduced before §3's Release N+1 emitter switch is stamped
`aicr.run/v1alpha2` by default, unless its §2 row selects otherwise. The row
wins. `ComponentUpgrades` uses that override: it starts at `aicr.run/v1beta1`
whenever it lands, because Release N already accepts that value and starting at
the target spares the kind an emitter flip at N+1 and a retirement at N+2.
Prefer the override for a new kind whose **beta** target track the tree already
accepts; the alpha default exists for kinds whose target does not yet parse. A
`v1` start is not covered by this preference and remains subject to the GA bar
in the next paragraph.

At or after N+1, a new kind starts at
`aicr.run/v1beta1` by default; it may start at
`aicr.run/v1` only when its introducing decision establishes that its public
contract is already ready for GA obligations. There is no implicit post-v1 alpha
lane. Adding one requires an explicit policy amendment and read-gate entry. A
new kind is never stamped with a version the tree does not already accept.

The pre-bump rule is universal for new wire kinds, including kinds with
profile-bearing fields. `v1alpha3` is not a general profile track: it is reserved
for the existing profile-bearing `RecipeMetadata` and `RecipeResult` schemas in
§2. A new kind may use it only through an explicit amendment that adds its own
§2 row and read-gate entry.

Likewise, `aicr.run/v1beta2` is the profile-bearing track selected in §2, not a
universal beta default. A new kind may select it only through an explicit row
that defines its schema discriminator and gate.

`aicr.run/v1alpha1` in particular has never been valid: ADR-013 moved the version
to `v1alpha2` *at* the domain rename, so the legacy pairing was
`aicr.nvidia.com/v1alpha1`.

Every new kind gets a row in the §2 map and an entry in the read gate in the same
change that introduces it.

### 8. External data must be compatible with the binary reading it, and fails closed

This **extends ADR-011 §3** to in-scope AICR catalog loaders. The gate is ordered:

1. Classify the raw document by domain and wire kind, preserving the existing
   skip behavior for unrelated YAML.
2. Before hydration, normalization, or any merge with embedded data, validate
   the raw `kind` and `apiVersion` against that kind/schema track's accepted set.
3. Reject an empty, retired, unknown, or wrong-kind value with
   `ErrCodeInvalidRequest` naming the observed value, the expected value or
   values for that kind, and the remediation. Never replace an unaccepted
   external header with an embedded header and continue.

The accepted set follows §3's release sequence: alpha plus target in N and N+1,
then target only in N+2. This ordering is load-bearing for
`ComponentRegistry`. `pkg/recipe/provider.go` currently warns when an external
registry version differs, merges its components, and stamps the result with the
embedded version. The new gate runs on the raw external `registry.yaml` before
`mergeRegistries`; an unsupported header therefore cannot be erased before it
is checked.

`pkg/recipe/metadata_store.go` already reads `apiVersion` to enforce the ADR-015
profile-kind pairing, but it applies no general accept-known/reject-unknown
gate. `RecipeMetadata` and `RecipeMixin` gain that gate after they are
classified as those AICR kinds and before strict decoding, storage, or
hydration. Unrelated non-recipe YAML keeps the current skip behavior.

ADR-011's domain carve-out remains unchanged. `ValidatorCatalog`
(`validator.nvidia.com/...`), the in-toto provenance predicate, and Zarf/Hauler
formats are separate schemas and are not gated by ADR-022. The bundle-root
`BundleProvenance` document in §2 is an AICR artifact and remains in scope.

The `ComponentUpgrades` loader proposed by ADR-021 follows the same pre-use rule
and reads the version selected by its §2 row, not a separately frozen literal.
Catalog authors need a published statement of which binary
versions accept which catalog versions. Loud, actionable breakage is the
intent; silent downgrade is not.

## Consequences

- Three version tracks exist at v1 — `aicr.run/v1`, `aicr.run/v1beta1`, and the
  profile-bearing `aicr.run/v1beta2` — instead of today's two alpha tracks.
- `pkg/header` remains the single source of version strings while package-local
  emitters and readers select versions by wire kind and schema family, following
  the discriminator pattern ADR-015 established.
- The initial migration spans N through N+2, bound to v0.21, v0.22, and v0.23.
  N stages readers, N+1 switches emitters and committed inputs, and N+2 rejects
  alpha and empty values. Stored artifacts must be migrated during that interval
  or read with a retained older binary; authored configs and external catalogs
  require manual edits.
- `AICRConfig` and the profile-bearing kinds ship at beta and are *expected* to
  evolve after v1.0.0, which makes the deprecation channel (#2115) load-bearing.
  Shared fields reachable from a GA artifact cannot break with them.
- External AICR catalogs, including `ComponentRegistry`, are gated before merge
  or hydration. A catalog that declares a version this binary does not know now
  fails loudly where it previously resolved something plausible and wrong.

## References

- [ADR-011](011-artifact-apiversion-policy.md) — artifact `apiVersion` policy and compatibility gate
- [ADR-013](013-aicr-run-domain-migration.md) — `aicr.run` domain migration, the precedent for a pre-v1 hard break
- [ADR-015](015-recipe-configuration-profiles.md) — recipe configuration profiles, which introduced kind-scoped evolution
- [ROADMAP §1 Defensible API stability](../../ROADMAP.md#1-defensible-api-stability)
- [Kubernetes deprecation policy](https://kubernetes.io/docs/reference/using-api/deprecation-policy/)
