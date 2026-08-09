# ADR-017: UAT Daytime Cluster-Name Convention

## Status

**Accepted** — 2026-08-05.

Originated from the UAT daytime-cluster leak/teardown work (the ephemeral
daytime-name change in #1275, DC2, and the orphan-teardown reliability effort)
and the observation that the cross-run discovery key baked into the daytime
cluster name is cloud-scoped — it cannot distinguish two reservations held by
the same account, and it has no slot axis for a future multi-cluster-per-day
end state.

## Problem

The UAT day/night broker (#1274, and `docs/contributor/uat.md`) provisions two
kinds of cluster, whose names double as the actuator's `deployment.id` — the
`github.com/mchmarny/cluster` actuator derives the cloud cluster name, the
resource group (AKS `<id>-rg`), and the terraform-state key **all** from that
single id:

```text
nightly:  aicr-uat-<run_id>                        # run-scoped, per-run isolation
daytime:  aicr-uat-day-<reservation>-<run_id>      # ephemeral, reservation-scoped
```

The nightly name is run-scoped and needs no discovery — the run that provisions
it also tears it down. The **daytime** name is different: a `daytime-up` run
provisions and *holds* the cluster, and a *later* run (the evening
`daytime-down`, or a `nightly`/`daytime-up` pre-batch guard) must find that held
cluster without being able to reconstruct its exact name (the `<run_id>` belongs
to a different run). Discovery is therefore a **cross-run prefix scan**:

- the pre-batch **guard** matches the broad prefix `^aicr-uat-day-<reservation>-[0-9]`
  and *blocks* the batch if anything matches;
- **teardown** (`Resolve daytime cluster to tear down`) matches the end-anchored
  `^aicr-uat-day-<reservation>-[0-9]+$` and *destroys* the single match.

The daytime name — and therefore the discovery key — is thus load-bearing. The
current `<reservation>` key has three problems:

1. **Cloud-scoped collisions.** The reservation name (e.g. `aws-h100`,
   `azure-h100`) is unique *per cloud*, not per account. The moment one account
   holds more than one reservation whose daytime clusters share a cloud, the
   discovery prefixes are no longer guaranteed disjoint, and a guard or teardown
   scan can match the wrong cluster.
2. **No slot axis.** The name encodes exactly one daytime cluster per
   reservation. The documented end state (both flavors per cloud during the day,
   blocked today only on capacity — see `docs/contributor/uat.md` Roadmap) needs
   a second dimension the name cannot express.
3. **Length pressure.** The full reservation name is verbose and consumes the
   scarce GKE 40-character cluster-name budget (see the length budget below).
   `azure-h100` alone is 10 characters of an already-tight name.

## Non-Goals

- Changing the **nightly** name. `aicr-uat-<run_id>` is run-scoped, needs no
  cross-run discovery, and is unchanged.
- Standing up a second daytime slot today. The `slot` axis is introduced now (as
  a runtime input, default `0`) so the name grammar and discovery scans are
  slot-aware, but the multi-slot end state remains blocked on capacity. Only
  `slot=0` is provisioned.
- Changing the committed `id: aicr-uat` placeholder in each
  `tests/uat/**/cluster-config.yaml`. That value is overwritten at runtime by the
  `DEPLOYMENT_ID` the workflow derives; it is not part of this convention.
- The OCI evidence/testgrid naming scheme (`testgrid-publish.yml`), which is a
  separate, digest-bound scheme unrelated to cluster names.

## Context

- The daytime name is derived once, in each per-cloud workflow's job-level
  `DEPLOYMENT_ID` env expression, from the `daytime-up` branch of a ternary
  (`aicr-uat-day-…` vs the nightly `aicr-uat-<run_id>`).
- The reservation registry (`infra/uat/reservations.yaml`, parsed by
  `pkg/uatbroker`) is the single source of truth for per-reservation data. New
  per-reservation fields are added there and surfaced to the workflows through
  the `uat-broker reservations --name <name>` `key=value` output, which
  `uat-run.yaml`'s `resolve` job exposes as `needs.resolve.outputs.<key>`.
- GKE caps a cluster name at **40** characters; AKS at **63** (plus a
  `<name>-rg` resource group, name + 3); EKS at **100**. GKE is the only binding
  cap.

## Decision

Replace the daytime cluster's cross-run discovery key with a **short registry
slug** plus a **runtime slot**, and key both discovery scans on the
`(slug, slot)` pair:

```text
nightly:  aicr-uat-<run_id>                        # UNCHANGED
daytime:  aicr-uat-day-<slug>-<slot>-<run_id>      # slug = new registry field; slot=0 today

guard matcher:    ^aicr-uat-day-<slug>-<slot>-[0-9]
teardown resolve: ^aicr-uat-day-<slug>-<slot>-[0-9]+$
```

### The slug

`slug` is a **new 2–4 character registry field** on each reservation row,
unique across all rows. It is the compact, account-stable discovery key the
reservation name is too long and too cloud-scoped to be. Launch values:

| Reservation | Slug |
|-------------|------|
| `aws-h100`  | `ah1` |
| `gcp-h100`  | `gh1` |
| `azure-h100`| `zh1` |
| `aws-gb200` | `ag2` |
| `kind-h100` | `kh1` |

The nvkind lane (`kind-h100`) gets a slug for uniformity, though it provisions
no cloud cluster and never uses the daytime name (see the consumer inventory).

**Slug length/charset policy: `^[a-z][a-z0-9]{1,3}$`** — 2 to 4 characters,
starting with a letter, lowercase alphanumeric. Uniqueness and the charset are
validated at registry load time (`pkg/uatbroker`), alongside the existing
per-row field validation, so a bad or duplicate slug fails the parse — and every
broker workflow — rather than silently colliding two reservations' discovery
keys.

### The slot

`slot` is a **runtime input**, not a registry field. It defaults to `'0'` and is
threaded from `uat-run.yaml` (a workflow-level `slot` string input) into each
per-cloud pipeline's `with:` block and thence into the daytime name and both
discovery scans. Modeling it as a runtime axis — rather than baking it into the
registry — keeps a single reservation row able to host multiple daytime slots
once capacity allows, without a registry change per slot.

### Length budget

Name length = `len("aicr-uat-day-")` (13) + `slug` + `-` (1) + `slot` + `-` (1)
+ `run_id` = **15 + slug + slot + run_id**. GitHub run ids are 11 digits today
and are modeled up to 13 for headroom. Against the binding GKE 40-char cap:

| slug | slot | run_id | name len | GKE headroom (40) |
|-----:|-----:|-------:|---------:|------------------:|
| 2 | 1 | 11 | 29 | 11 |
| 3 | 1 | 11 | 30 | 10 |
| 3 | 1 | 13 | 32 | 8 |
| 4 | 1 | 13 | 33 | 7 |
| 4 | 2 | 11 | 32 | 8 |
| 4 | 2 | 13 | **34** | **6** |

The **worst case** — a 4-char slug, a 2-digit slot, and a 13-digit run id — is
34 characters, leaving **≥ 6 characters of GKE headroom permanently**. Today's
actual names (`gh1`, 1-digit slot, 11-digit run id → 30 chars) sit at 10
characters of headroom. AKS (63, plus a 37-char worst-case `<name>-rg`) and EKS
(100) have ample room; GKE is the only cap this budget is sized against. The
prior scheme's `azure-h100` daytime name was already 35 characters at an 11-digit
run id — the new convention is *shorter* while adding the slot axis.

### Per-(reservation, slot) discovery key

The guard and teardown scans key on `(slug, slot)` rather than the reservation
name for two reasons that compound:

- **Multi-reservation-per-account.** A slug is globally unique across the
  registry (load-time enforced), so two reservations an account holds in the
  same cloud have disjoint discovery prefixes by construction — the collision
  the cloud-scoped reservation name could not rule out.
- **Multi-slot end state.** With `slot` in the key, `slot=0` and `slot=1` daytime
  clusters on the *same* reservation have disjoint prefixes, so the guard blocks
  only a leak in the *same* slot and teardown resolves exactly one cluster per
  slot. The name grammar and both scans are slot-ready even though only `slot=0`
  is provisioned today.

Both scans preserve their existing fail-closed posture: a `list`-error exits 1
(never read as "no cluster, clear to proceed"), teardown retries the list call a
bounded number of times before failing loud, and a `> 1` match fails loudly
rather than destroying an arbitrary cluster. The guard stays the broad matcher
(`^prefix[0-9]`, anything prefix-ish blocks a batch); teardown stays end-anchored
(`^prefix[0-9]+$`, only an exact `<prefix><run-id>` is a destroy target).

### Consumer inventory

The daytime name and its discovery key are consumed in exactly these places, all
updated by this convention:

| Consumer | Uses |
|----------|------|
| `uat-run.yaml` | new workflow-level `slot` input; passes `slug` (from `needs.resolve.outputs.slug`) and `slot` into the aws/gcp/azure `with:` blocks (not kind) |
| `uat-aws.yaml`, `uat-gcp.yaml`, `uat-azure.yaml` | new `slug`/`slot` `workflow_call` inputs; the `DEPLOYMENT_ID` daytime branch; the pre-batch guard prefix; the teardown resolve prefix; the scheme comments |
| `infra/uat/reservations.yaml` | new `slug:` field per row |
| `pkg/uatbroker` (`model.go`, `registry.go`) | `Reservation.Slug` field + load-time validation; **reservation-name charset validation** (the name is the legacy `grep -E` prefix — see the migration section) |
| `tools/uat-broker/main.go` | `slug=<value>` in the `--name` `key=value` output |
| `tools/uat-broker/README.md` | documents the `slug=` line in the `reservations --name` output |
| `docs/contributor/uat.md` | lifecycle table, example commands, pre-batch-guard prose |

`uat-kind.yaml` is **not** a consumer: the nvkind lane provisions no cloud
cluster, so it takes no `slug`/`slot` and derives no daytime name. Its registry
row still carries a slug for field uniformity and future use.

### Dual-prefix migration

The daytime name is a *live, cross-run* discovery key: at cut-over an
OLD-named daytime cluster (`aicr-uat-day-<reservation>-<run_id>`) may still be
held on a reservation, and the NEW guard/teardown scans — keyed on the slug —
would not find it, silently leaking the cluster (guard) or skipping its teardown
(resolve).

The migration is therefore **dual-prefix**: during the transition, the guard and
teardown steps match **both** the new prefix
`aicr-uat-day-${SLUG}-${SLOT}-` **and** the legacy prefix
`aicr-uat-day-${RESERVATION}-`. The reservation name stays available to the
steps (as an env var) purely for this legacy leg. New clusters are always
provisioned with the new name; the legacy leg exists only so any pre-existing
OLD-named daytime cluster is still discovered, blocked, and torn down. Each
workflow carries a comment marking the legacy leg as a **transitional shim to
remove once no old-named daytime clusters remain**.

The dual-prefix legs preserve the guard/teardown asymmetry independently: each
leg's guard matcher is broad (`^prefix[0-9]`), each leg's teardown matcher is
end-anchored (`^prefix[0-9]+$`), and the combined result upholds the same
fail-closed and at-most-one-match invariants as a single prefix.

Because the legacy leg interpolates the reservation `name` into a `grep -E`
pattern, the name must be metacharacter-free or the pattern would be invalid and
the scans' `|| true` would read the grep error as "no match" (fail-open). The
registry validator therefore constrains the reservation `name` charset
(`^[a-z]([a-z0-9-]*[a-z0-9])?$`, in `pkg/uatbroker`) at load time — the same
data-source fail-closed treatment as `slug` and `slot` — so no reservation row
can inject an ERE metacharacter into the scan. `slug` and `slot` are likewise
validated (charset / numeric) in each workflow's `Validate inputs` step before
they reach the pattern.

**Follow-up on shim removal.** Two decisions are deferred to the change that
removes the legacy leg, and are recorded here as the tracking record (a
dedicated issue should reference this section):

- **Guard slot scope.** Once the legacy leg is gone, the guard's discovery key
  is purely `(slug, slot)`-scoped, so a `nightly` run at the default `slot=0`
  would no longer be blocked by a leaked daytime cluster in a *different* slot on
  the same reservation — yet a nightly batch consumes the *whole* reservation,
  not one slot. This is unreachable today (`uat-run.yaml` rejects `slot != 0`),
  but the removal change should decide the guard's slot scope deliberately
  (e.g. a nightly guard that matches all slots of the reservation) rather than
  inheriting the migration-era single-slot behavior.
- **Removal trigger.** The shim is removed once no old-named daytime cluster can
  still be held — i.e. after at least one full daytime cycle has run under the
  new names on every daytime reservation.

## Consequences

- **Discovery keys are account-stable and slot-ready.** A slug is globally unique
  in the registry, so two reservations in one account never share a daytime
  prefix, and the `slot` axis lets one reservation host multiple daytime clusters
  the moment capacity lands — neither achievable with the reservation-name key.
- **Names stay comfortably inside the GKE cap.** Worst case 34/40; today 30/40.
  The convention is shorter than the scheme it replaces despite the added slot.
- **The registry gains one validated field.** `slug` is validated for
  non-emptiness, uniqueness, and charset at load time in the same loop as the
  other fields, so a data typo fails the broker rather than colliding two
  reservations' discovery keys at runtime.
- **A transitional dual-prefix window.** During migration the guard and teardown
  scans carry a second, legacy leg keyed on the reservation name so an in-flight
  OLD-named cluster is not orphaned. The legacy leg is a marked shim and is
  removed in a follow-up once no old-named daytime clusters remain — the single
  standing cost of the rename.
- **No change to nightly runs, the evidence/testgrid scheme, or the committed
  cluster-config placeholders.** The blast radius is the daytime name and its two
  discovery scans.

## Alternatives Considered

### A. Keep the reservation-name key (status quo)

Rejected: cloud-scoped, so it cannot rule out a same-account, same-cloud
collision; it has no slot axis; and it is the longest of the options against the
GKE cap.

### B. Slug in the registry, slot also in the registry

Rejected: a per-slot registry row (or a slot list on the row) couples the number
of daytime slots to a data edit and duplicates the reservation's other fields per
slot. Modeling `slot` as a runtime input keeps one reservation row able to host
any number of slots with no registry churn.

### C. Hash/truncate the reservation name instead of a hand-assigned slug

Rejected: a truncation or hash is neither human-readable in the Actions UI nor
guaranteed collision-free at 2–4 characters. A hand-assigned, load-time-unique
slug is both readable and provably unique.
