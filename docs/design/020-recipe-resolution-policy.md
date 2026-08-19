# ADR-020: Recipe-Declared Resolution Policy (Strict Dimensions)

## Status

**Proposed**, 2026-08-19.

Part of [#1782](https://github.com/NVIDIA/aicr/issues/1782) (joint-coverage
formulation to subsume `requireOSIfNeeded`), a follow-up to
[#1542](https://github.com/NVIDIA/aicr/issues/1542) design 4.1. Milestone
**v1 (API Stability)**: recipe resolution semantics are part of the frozen
schema surface, so the divergence described below has to be resolved before
the schema is declared stable.

## Problem

Recipe resolution enforces two independent correctness checks with
overlapping but non-identical semantics:

1. **`requireOSIfNeeded`** (`pkg/recipe/metadata_store.go`) is a pre-merge
   guard. When the caller states a `service` but no `os`, it scans the
   matched overlays for one carrying that exact `service`+`accelerator`
   pair. If none does, and OS-gated overlays exist that *would* match if an
   OS were named, it fails with `specify an OS (valid: ...)`.
2. **`verifyCriteriaCoverage`** (`pkg/recipe/coverage.go`) is a post-merge
   post-condition. Every dimension the caller stated must be carried by at
   least one applied overlay. Each dimension is checked **independently**.

That independence is the defect. Per-dimension coverage is satisfied when
`service` and `accelerator` are honored by *separate* overlays, while the
guard demands that *one* overlay carry both before the OS-agnostic tier
counts as served. Union-of-dimensions is not coverage of the combination.

The two agree on the embedded catalog only by accident of its shape. OKE and
GKE gate their entire overlay subtrees on `os` (every GKE/OKE overlay carries
one), so a `service=gke` query matches nothing and `service` itself goes
uncovered, so both checks reject it. Other services ship OS-agnostic *joint*
mid-tier overlays (`h100-eks-training` carries service+accelerator+intent
together), so wherever coverage passes, the guard's joint match exists too.

Nothing in the schema requires that shape. An external `--data` catalog can
present the split layout, and then the checks diverge:

| overlay | criteria |
| --- | --- |
| `svc-foo` | `service: eks` |
| `accel-gpu` | `accelerator: h100` |
| `svc-accel-ubuntu` | `service: eks, accelerator: h100, os: ubuntu` |

Query `service=eks accelerator=h100`: coverage passes (each dimension is
carried by *some* overlay), while the eks+h100-specific content silently
never applies. `TestBuildRecipeResult_GuardAndCoverageComposition` pins this
catalog.

Three consequences make this a v1 blocker rather than a cleanup:

- **Ordering decides the error.** The guard runs first and masks the coverage
  error for the same query.
- **The two error shapes are not interchangeable.** Coverage failures are
  `NewWithContext` carrying structured `details.uncovered[]`, which
  `pkg/client/v1` relaxation switches on (`relax.go`; `relax_test.go` pins
  its dimension vocabulary against `recipe.CoverageDimensionNames`). Guard
  failures are a plain `aicrerrors.New` with no context, and therefore
  programmatically opaque. The same underlying condition is actionable or
  not, depending on which check happened to catch it.
- **The mechanism has the policy baked into its name and body.**
  `requireOSIfNeeded` hardcodes both the dimension (`os`) and the subset it
  tests (`service`+`accelerator`). It cannot generalize.

## Evidence

Measured over all 427 distinct criteria projections of the embedded catalog
(every non-empty subset of every overlay's stated dimensions, the same
enumeration `TestCoverageGoldenMatrix` uses): 117 resolve successfully, 310
fail.

| Candidate rule | Successes preserved | Failures still caught |
| --- | --- | --- |
| Guard as-is vs. coverage alone | n/a | **0** projections where the guard is the only thing failing |
| One overlay must carry *all* stated dimensions | 101 / 117 | 310 / 310 |
| Joint maximality over *every* dimension | 101 / 117 | 310 / 310 |
| **Joint sufficiency (this ADR)** | **117 / 117** | **310 / 310** |

The Subsumption section below records the two further formulations that were
measured and rejected for failing the ticket's actual bar.

Two findings drive the decision:

- **The guard is already redundant on embedded data.** Zero projections fail
  the guard that per-dimension coverage would not also reject. All 50
  `requiresOS: true` golden entries are GKE (29) or OKE (21).
- **A purely structural rule cannot work.** Both "one overlay carries
  everything" and unrestricted joint maximality break the same 16
  currently-valid queries such as `service=eks accelerator=h100`, which
  legitimately resolve as `eks` + `h100-any` merged. They break because
  stating `intent` would unlock a richer joint match. Structurally that is
  *identical* to the `os` case the guard exists for. `os` and `intent` are
  indistinguishable in the data; only domain meaning separates them, so the
  distinction must be declared somewhere.

## Non-Goals

- **Defaulting an unstated dimension.** This ADR makes omission an *error*
  where it would silently lose content. Choosing a value on the caller's
  behalf is a separate concern.
- **Changing which overlays match.** `Criteria.Matches` and
  `FindMatchingOverlays` are untouched.
- **Re-litigating `intent`.** The 16 generic-tier queries keep passing. If
  `intent` should later become strict, this ADR makes that a one-line catalog
  change plus a golden regeneration, not a code change.
- **`nodes`.** It is not a coverage dimension (#1781) and cannot be declared
  strict.

## Decision

### Declaration

`RecipeMetadataSpec` gains an optional, inheritable `resolution` field:

```yaml
# recipes/overlays/base.yaml
spec:
  resolution:
    strictDimensions: [os]
```

```go
// ResolutionPolicy governs how unstated criteria dimensions are treated
// during overlay resolution. It inherits through spec.base like any other
// spec field and is stripped from the hydrated RecipeResult.
type ResolutionPolicy struct {
    // StrictDimensions may not be silently generalized: if leaving one
    // unstated would cause resolution to skip an overlay that jointly
    // carries more of the stated criteria, resolution fails instead of
    // returning the weaker match. Dimensions absent from this list are
    // elective: unstated means "give me the generic tier".
    StrictDimensions []string `json:"strictDimensions,omitempty" yaml:"strictDimensions,omitempty"`
}
```

No dimension name appears in any Go type, function, or field. The only place
`os` appears is catalog data.

This follows the `spec.profile` precedent from
[ADR-015](015-recipe-configuration-profiles.md): an overlay-scoped spec field
resolved after composition and never copied into a hydrated `RecipeResult`.

### Scope and inheritance

The policy inherits through `spec.base` using the existing
`resolveInheritanceChain` + `RecipeMetadataSpec.Merge` machinery (last wins),
so a subtree can override it without touching embedded data:

```yaml
# a deliberately OS-agnostic subtree
metadata: { name: kind }
spec:
  resolution:
    strictDimensions: []
```

Mixins carry no `criteria` and contribute no coverage; they carry no
resolution policy either.

**Which policy applies.** The rule evaluates a *candidate* overlay that is
deliberately not applied, so the candidate's own resolved policy governs,
walked through its base chain. The overlay being skipped is the one asserting
whether skipping it is acceptable. The applied set's merged policy is not
consulted.

**Inheritance is what keeps this fail-closed.** A new OS-gated overlay added
to any catalog inherits `[os]` from base automatically. Authors annotate only
the exception, never the rule; omission is safe, and opting out is explicit
and visible in the file that does it.

### The rule

`verifyCriteriaCoverage` enforces two conditions:

- **Completeness** (existing, unchanged): every stated dimension is carried
  by some applied overlay.
- **Joint sufficiency** (new): resolution fails when **no applied overlay
  jointly carries every stated dimension** *and* supplying a value for a
  strict dimension would **enlarge the applied overlay set**.

Both halves are required. The first is the escape hatch that keeps the
generic tier valid: when one overlay already honors the whole stated
combination, nothing has been silently dropped and no further criteria are
demanded. The second is what detects the drop: if naming a strict dimension
would pull additional overlays into the result, that content is being lost
by omission rather than by choice.

This is `requireOSIfNeeded` generalized on both halves rather than replaced:

| `requireOSIfNeeded` | joint sufficiency |
| --- | --- |
| success shortcut: some matched overlay carries `service`+`accelerator` jointly | some applied overlay carries **all stated** dimensions jointly |
| `availableOSForCriteria`: an OS-gated overlay would match if an OS were supplied | supplying a **strict** dimension enlarges the applied set |

The guard hardcoded the subset (`service`+`accelerator`) and the dimension
(`os`). Joint sufficiency reads the subset from the query and the dimension
from `strictDimensions`. Because only strict dimensions drive the second
half, elective dimensions such as `intent` never demand completion, which is
what keeps the generic tier queries valid.

On the divergence catalog above, no applied overlay carries both `service`
and `accelerator`, and supplying `os: ubuntu` would pull in
`svc-accel-ubuntu`. `os` is strict, so resolution fails.

`requireOSIfNeeded` and `availableOSForCriteria` are deleted, along with both
call sites in `metadata_store.go`.

### Subsumption

The ticket's bar is that the new rule *genuinely* subsumes the guard, so the
claim is verified rather than asserted. Two earlier formulations were
measured and rejected for failing it:

| Formulation | Successes preserved | Failures caught | Subsumes guard |
| --- | --- | --- | --- |
| Joint maximality over stated dimensions | 117 / 117 | 310 / 310 | **No** |
| Strictly richer match set (#1542's suggestion) | 104 / 117 | 310 / 310 | Yes |
| **Joint sufficiency (this ADR)** | **117 / 117** | **310 / 310** | **Yes** |

Joint maximality measured coverage of *stated* dimensions only, which makes
an OS-gated overlay invisible whenever it adds content without covering more
of what the caller asked for. Two catalog shapes escape it while the guard
rejects them: an OS-only overlay carrying neither stated dimension, and an
OS-gated overlay carrying `service` alone, which ties rather than exceeds the
applied count.

The richer-match-set formulation subsumes the guard but drops the success
shortcut, so it demands `--os` for 13 currently-valid queries including
`aicr query --service eks --accelerator h100 --intent training`, a documented
CLI example. That is a breaking change at v1 freeze.

Subsumption is pinned by a **differential property test**: over generated
small catalogs, every query the retired guard would have rejected must also
be rejected by joint sufficiency. The test retains the guard's logic as a
test-only oracle so the property is checked against the real predicate rather
than a restatement of it.

### Error surface

Joint-sufficiency failures carry their own context key, **not** `uncovered`:

```
details.strictDimensions: [
  { dimension: "os",
    validValues: ["ubuntu"],
    wouldCover:  ["service", "accelerator"],
    via:         "svc-accel-ubuntu" }
]
```

Two reasons. `uncovered` means "you stated it and nothing honored it", but
here the dimension is *unstated*, so the shape does not fit. And
`pkg/client/v1` relaxation clears `details.uncovered` dimensions and retries;
routing joint failures there would let relaxation clear the check and return
the partial recipe, reinstating #1542. A distinct key is fail-closed by
default and needs no change to `relax.go`.

The message keeps the guard's actionable shape:

```
service 'eks' with accelerator 'h100' requires os (valid: ubuntu)
```

### Validation

Unknown dimension names in `strictDimensions` are rejected at catalog load
with `ErrCodeInvalidRequest`, validated against `CoverageDimensionNames()`.
`nodes` is rejected for the same reason it is absent from
`coverageDimensions`.

## Alternatives Considered

**A. Hardcode `os` as the one strict dimension in Go.** An `elective bool`
field on `coverageDimension`. Identical behavior on the embedded catalog and
roughly 70 fewer lines with no schema commitment. Rejected because external
`--data` catalogs would inherit an assumption tuned to NVIDIA's catalog shape
with no way to change it, and the whole point of #1782 is that external
catalogs can present shapes the embedded data does not. Remains the fallback
if the v1 schema review objects to the new field: it satisfies the same
requirement.

**B. Catalog-level `recipes/resolution.yaml`.** A dedicated top-level file
with its own `kind`. Rejected once inheritance was available: layering is
per-file, so opting out would mean *shadowing* an embedded file, whereas an
inheritable spec field lets an external catalog declare the override on its
own overlay and never touch embedded data. It also needs a new kind, a new
loader, and cannot express per-subtree policy.

**C. Joint maximality over every dimension (no strict list).** Genuinely
dimension-agnostic and needs no declaration, but flips 16 currently-valid
queries into errors demanding `--intent`. That is a breaking behavior change
at v1 freeze.

**D. `flexDimensions` (inverse polarity).** List what *may* be omitted. Its
merit is fail-closed schema growth: a sixth criteria dimension would default
to strict. Rejected because adding a criteria dimension already carries an
explicit cross-repo audit checklist (CLAUDE.md), so the benefit rarely fires,
while the cost lands on the common case: every catalog listing 4 of 5
dimensions to express one decision, and a subtree opt-out becoming "list all
five" instead of `[]`.

**E. Per-overlay boolean flag with no inheritance.** Attaches the judgment to
the content that knows it, but a new OS-gated overlay whose author forgets
the flag silently loses the check. Inheritance from base gets the same
expressiveness with the opposite failure mode.

## Consequences

- `docs/contributor/recipe.md` currently documents that the guard is **not
  subsumed** by the coverage post-condition and that "both checks apply". That
  section is rewritten.
- `testdata/coverage_golden.yaml` is regenerated: the 50 `requiresOS: true`
  entries become `strictDimensions` failures. `classify()` in
  `coverage_matrix_test.go` updates accordingly. The regeneration lands as its
  own commit, with a before/after comparison confirming only the
  classification label moved and no outcome flipped.
- `TestBuildRecipeResult_GuardAndCoverageComposition` is retained and
  retargeted at the new rule. It remains the regression pin for the
  divergence.
- Existing external `--data` catalogs need no change: they inherit
  `strictDimensions: [os]` from the embedded base and keep exactly today's
  behavior.
- The v1 schema surface grows by one optional field.
