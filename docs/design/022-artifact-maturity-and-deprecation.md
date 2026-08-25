# ADR-022: Per-Kind Artifact Maturity and the v1 Deprecation Policy

## Status

Proposed. Amends [ADR-011](011-artifact-apiversion-policy.md): §4 (transition
window) is replaced, §3 (compatibility gate) is extended to the catalog loader,
and §1–§2 stand unchanged. Builds on [ADR-015](015-recipe-configuration-profiles.md),
which already introduced kind-scoped version evolution as an amendment to ADR-011.

## Problem

Every AICR artifact carries an alpha `apiVersion`. ROADMAP
[§2](../../ROADMAP.md#2-stability) promises a frozen, diff-gated surface at v1,
and the Kubernetes convention that `v1alpha2` invokes — may be dropped or changed
without notice — is the opposite of that promise. Two alpha versions coexist
(`aicr.run/v1alpha2` for most kinds, `aicr.run/v1alpha3` for the profile-bearing
`RecipeMetadata` and `RecipeResult`), which compounds it.

Three questions have no recorded answer:

1. **What `apiVersion` do artifacts carry at v1 GA?**
2. **What does a bump owe the previous version?** ADR-011 §4 says dual-accept with
   a transition window. The `pkg/header` godoc says the opposite — a hard break
   with no window — and `IsSupportedAPIVersion` implements the godoc while
   `IsSupportedRecipeResultAPIVersion` implements the ADR. Both statements are
   unconditional, which is the actual gap: neither says *when* each applies.
3. **What `apiVersion` does a newly introduced kind start at?** Nothing answers
   this, so new kinds guess.

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
`aicr.run/v1`. ROADMAP §2 asks for a committed baseline, a CI diff-gate, and a
deprecation channel. A gate over a `v1beta1` schema is a real gate: it catches
*unintended* breakage, which is what the freeze promises. The maturity string
governs *intended* breakage, and it is set per kind.

This mirrors Kubernetes, which shipped 1.0 with beta APIs and serves beta
alongside GA in every release since.

### 2. Per-kind maturity map

| Kind | Today | Target | Rationale |
|---|---|---|---|
| `Snapshot` | `v1alpha2` | `aicr.run/v1` | Settled shape; the first artifact an integrator reads |
| `Recipe`, `RecipeCriteria` | `v1alpha2` | `aicr.run/v1` | 105 shipped overlays of exercise |
| Bundle provenance (`localformat.ProvenanceAPIVersion`) | `v1alpha2` | `aicr.run/v1` | Rides in the bundle, which is what downstream integrates against |
| `AICRConfig` | `v1alpha2` | `aicr.run/v1beta1` | Actively growing: #2026 bound 2 of 5 spec sections, #2245 binds the rest. Do not freeze a schema mid-expansion |
| `RecipeMetadata`, `RecipeResult` | `v1alpha3` | `aicr.run/v1beta1` | Newest (ADR-015), 2 overlays, opt-in via profiles |

The line: **whatever an integrator's code binds to must be at `v1`.** That is
`pkg/client/v1`, the REST surface, `Snapshot`, `Recipe`, and the bundle. Local
configuration and the profile-bearing kinds can sit at beta honestly.

`v1alpha3` does **not** converge into the promoted version. `RecipeResult` stays
distinct at beta, so `RecipeProfileAPIVersion` survives as the discriminator and
the bidirectional profile/apiVersion validation in `pkg/recipe/profile.go` is
unchanged.

### 3. The bump happens now, as a hard break

While every kind is alpha, a bump owes nothing (see §4). The 256 files carrying a
version literal are regenerated in lockstep, exactly as
[ADR-013](013-aicr-run-domain-migration.md) did, and no dual-accept path is
carried forward. ADR-013 made this argument already: doing it before v1 "is the
cheapest possible moment — there is no stable contract to honor yet." That
argument expires at GA, which is why it is spent now.

### 4. The deprecation window is conditional on the level being retired

This **replaces ADR-011 §4**, whose dual-accept rule was stated unconditionally.
The window owed is a function of the maturity of the version being removed:

| Level being retired | Obligation |
|---|---|
| Alpha | None. May be removed in any release, no prior notice |
| Beta | Readable for **2 releases** after deprecation |
| GA | Not removed within a major version |

The `pkg/header` godoc gains this condition. Its hard-break language is correct
*for alpha* and must not be read as the general rule.

**Two releases is deliberate, not inherited.** The Kubernetes equivalent is 9
months or 3 minor releases; at AICR's ~2-week cadence that would be ~18 releases.
A short window means the read gate carries at most one retired version, which
keeps the accept-known logic trivial. The cost is that a consumer who upgrades
less than monthly can miss a window.

### 5. Never deprecate toward a less stable version

GA may replace beta and alpha. Beta may replace beta and alpha, never GA. Alpha
may replace only alpha. This is what makes a mixed-maturity map safe: promoting
`Snapshot` to `v1` is a one-way door for `Snapshot` and binds no other kind.

### 6. Split each bump across two releases

Release N adds the new version to the read gate. Release N+1 flips the emitter.
A consumer who rolls back one release can still read what they generated.

AICR has no conversion layer — a file is one version, take it or leave it — so
this matters more here than in Kubernetes, where the apiserver converts between
served versions.

**This composes with §4 rather than duplicating it.** For a beta kind: N adds
read support, N+1 emits the new version and deprecates the old, and the old stays
readable through N+3. Three releases of overlap total. The two "2 releases" are
different clocks and should not be conflated.

### 7. A new kind starts on the current track

A kind introduced today is stamped `aicr.run/v1alpha2` (or the post-bump
equivalent once §3 lands), never a version the tree does not already accept.
`aicr.run/v1alpha1` in particular has never been valid: ADR-013 moved the version
to `v1alpha2` *at* the domain rename, so the legacy pairing was
`aicr.nvidia.com/v1alpha1`.

Every new kind gets a row in the §2 map and an entry in the read gate in the same
change that introduces it.

### 8. External data must be compatible with the binary reading it, and fails closed

This **extends ADR-011 §3** to the catalog loader, which today checks `kind` only
and never `apiVersion`. ADR-015 documented the consequence: a released binary
pointed at a newer catalog silently resolves an unspecialized recipe.

Silent divergence is the dangerous direction. The accept-known / reject-unknown
gate applies per kind at every external-data boundary, including
`--data` catalogs. An unknown `apiVersion` is an `ErrCodeInvalidRequest` naming
the value, the expected value, and the remediation — never a silent downgrade.

Consequence: catalog authors need a published statement of which binary versions
accept which catalog versions. Loud breakage is the intent.

## Consequences

- Two version tracks exist at v1 — `aicr.run/v1` and `aicr.run/v1beta1` — instead
  of today's two alpha tracks.
- `pkg/header` grows from two gates to roughly four, one per track, following the
  `IsSupportedRecipeResultAPIVersion` pattern ADR-015 established.
- A one-time regeneration touches the 256 files carrying a version literal. No
  transition window is offered, and artifacts stamped with a prior group/version
  must be regenerated. This is the last release in which that is free.
- `AICRConfig` and the profile-bearing kinds ship at beta and are *expected* to
  break after v1.0.0, which makes the deprecation channel (#2115) load-bearing
  rather than a documentation task.
- A catalog that declares a kind an older binary does not know now fails loudly
  where it previously resolved something plausible and wrong.

## References

- [ADR-011](011-artifact-apiversion-policy.md) — artifact `apiVersion` policy and compatibility gate
- [ADR-013](013-aicr-run-domain-migration.md) — `aicr.run` domain migration, the precedent for a pre-v1 hard break
- [ADR-015](015-recipe-configuration-profiles.md) — recipe configuration profiles, which introduced kind-scoped evolution
- [ROADMAP §2 Stability](../../ROADMAP.md#2-stability)
- [Kubernetes deprecation policy](https://kubernetes.io/docs/reference/using-api/deprecation-policy/)
