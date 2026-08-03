# ADR-016: Slurm Accounting Enablement

## Status

**Accepted** — 2026-07-27.

Depends on the recipe artifact and strict-boundary work in
[ADR-015](015-recipe-configuration-profiles.md), tracked by
[#1761](https://github.com/NVIDIA/aicr/issues/1761). This ADR does not use the
profile selection mechanism for accounting; it defines one Slurm-specific
typed configuration alongside the at-most-one selected profile.

## Problem

Slinky Slurm accounting needs SlurmDBD plus a MariaDB-compatible database.
AICR must support three customer intents:

1. accounting disabled;
2. accounting enabled against a customer-managed database, either in-cluster
   or external; and
3. accounting enabled against a database installation provided by AICR.

Today accounting is a bundle-time chart override:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --set slinkyslurm:accounting.enabled=true
```

That is sufficient to render SlurmDBD, but it cannot safely express the three
intents:

- `accounting.enabled` distinguishes only enabled from disabled;
- independently setting Slurm and MariaDB component booleans permits
  contradictory configurations;
- bundle-time `--set` values are absent from `recipe.yaml`, so normal
  `aicr validate` and recipe evidence do not know which ownership model was
  selected; and
- enabling an undeclared MariaDB component at bundle or installation time
  would change the recipe's component inventory after resolution.

The resolved recipe must remain the source of truth for whether accounting is
disabled, customer-managed, or AICR-provided. Bundle generation may accept
environment-specific connection values, but it must not be the point where
the ownership mode is chosen.

## Goals

- Provide one enumerated, customer-facing accounting mode.
- Record the effective mode in every newly generated Slurm recipe.
- Derive all Slurm and MariaDB installation gates from that mode.
- Keep the component inventory stable across the three modes.
- Prevent `--set`, `--set-json`, `--set-file`, `--dynamic`, install-time Helm
  values, and component filters from changing mode-owned gates.
- Support customer-managed databases both inside and outside the cluster.
- Install MariaDB Operator and an accounting database only for the
  AICR-provided mode.
- Reuse the existing snapshot evidence for detecting a pre-existing official
  MariaDB Operator API or MariaDB custom resource.
- Make validation and evidence mode-aware without recording database
  credentials.

## Non-Goals

- Operating MariaDB as a service for the customer. AICR does not own backup
  policy, restore execution, capacity planning, monitoring response, incident
  response, or day-two database administration.
- Auto-detecting a database and silently selecting a mode.
- Distinguishing an in-cluster customer database from an external customer
  database. Both have the same ownership contract.
- Adding a generic recipe parameter language.
- Allowing accounting mode to be changed after the recipe has been resolved.
- Recording credential values in a recipe, bundle, snapshot, log, or evidence
  artifact.
- Proving the identity of the customer-managed database in recipe evidence.
  The recipe attests the ownership mode; when bundle attestation is enabled,
  the static bundle artifact binds the selected connection metadata.

## Decision

### 1. One typed accounting mode

Introduce the enum:

```text
disabled
customer-managed
aicr-provided
```

`disabled` is the default. `aicr-provided` deliberately describes what AICR
supplies without implying that AICR operates the database after installation.

There is no public `accounting.enabled` field and no independent MariaDB
installation switch. The mode is the only ownership selector.

### 2. Recipe input and resolved artifact

The AICR config-file input is:

```yaml
spec:
  recipe:
    configuration:
      slurm:
        accounting:
          mode: aicr-provided
```

The CLI equivalent is:

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent training \
  --platform slurm \
  --slurm-accounting-mode aicr-provided
```

The SDK request and the resolving REST API carry the same typed field adjacent
to criteria and profile selection. Accounting mode is not a criteria axis:
it does not participate in catalog matching or multiply leaf overlays.

The flat resolved `RecipeResult` records:

```yaml
configuration:
  slurm:
    accounting:
      mode: aicr-provided
```

Newly generated Slurm recipes always materialize the mode, including
`disabled`. Omitting the input therefore selects and records `disabled`; it
does not produce an unspecified state. Supplying the field for a non-Slurm
recipe is `ErrCodeInvalidRequest`.

The CLI flag, config field, SDK option, GET parameter, and POST field normalize
to one typed value before resolution. Invalid enum values fail closed. Normal
CLI-over-config precedence applies between those two input surfaces; two
values in a request surface that is not defined to have precedence are
rejected when they disagree.

### 3. Artifact version and evidence

Accounting mode is desired state that changes bundle rendering and validation.
An older binary must not silently ignore it.

If accounting lands before the first release containing ADR-015's
`aicr.run/v1alpha3` recipe support, accounting is incorporated into that
strict schema. Otherwise, accounting-bearing recipes use the next
`RecipeResult` apiVersion understood by the producing binary. In either case:

- full-artifact decoding is strict at file, REST adoption, bundler, mirror,
  and evidence projection boundaries;
- the version/configuration cross-check is bidirectional;
- legacy recipes without the field retain legacy semantics rather than being
  upgraded into evidence of `disabled`; and
- new Slurm recipes record the field explicitly.

The canonical recipe digest covers `configuration.slurm.accounting.mode`.
Consequently, `aicr validate --emit-attestation` binds the accounting ownership
mode through `predicate.recipe.digest`.

Changing the mode requires regenerating the recipe and invalidates prior
recipe evidence. Enabling accounting through a bundle-only override does not
produce equivalent evidence and is not accepted for new mode-bearing recipes.
Evidence produced from a legacy recipe remains valid for that legacy recipe,
but it makes no accounting-mode claim.

### 4. Mode effects

AICR derives the effective component values:

| Mode | Slinky accounting | MariaDB CRDs and operator | Accounting MariaDB instance |
|---|---:|---:|---:|
| `disabled` | `accounting.enabled: false` | `install: false` | `install: false` |
| `customer-managed` | `accounting.enabled: true` | `install: false` | `install: false` |
| `aicr-provided` | `accounting.enabled: true` | `install: true` | `install: true` |

The derived gates are owned by the accounting configuration. Customers cannot
override them directly. A same-value override is rejected as unsupported
rather than accepted as a second representation of the same intent.

The protected paths initially include:

- `slinky-slurm:accounting.enabled`;
- the install gate for MariaDB Operator CRDs;
- the install gate for MariaDB Operator; and
- `slurm-accounting-mariadb:install`.

Any additional path that controls whether these resources exist joins the
protected set. Protected component-presence state is also guarded so
`--bundlers` or an equivalent SDK filter cannot silently remove a required
component from an enabled mode.

### 5. Stable component inventory and values gating

Every supported Slurm recipe includes references for:

- the MariaDB Operator CRDs;
- the MariaDB Operator; and
- the AICR accounting MariaDB instance.

Those references exist in all three modes. When their derived `install` value
is false, they render no Kubernetes resources, publish no readiness
expectations, and introduce no runtime controller.

The official MariaDB Operator publishes separate
[`mariadb-operator-crds` and `mariadb-operator` Helm charts](https://github.com/mariadb-operator/mariadb-operator#helm-installation).
Implementation must pin compatible versions and preserve CRD-before-operator-
before-instance ordering.

Before implementing the registry entries, complete a render spike that proves
the no-op contract for every supported deployer. If the upstream charts cannot
render nothing from a values gate, use an AICR-owned thin conditional wrapper
or an explicit registry-declared install-gate mechanism. Do not approximate the
contract by adding or removing component references during bundle generation.

The gate implementation must also define no-op behavior for:

- health-check hydration;
- expected-resource checks;
- image and mirror discovery;
- BOM generation;
- readiness hooks;
- deployment ordering; and
- vendored and non-vendored charts.

`install: false` means the component is part of the declared recipe inventory
but absent from the deployable runtime inventory.

### 6. Database connection contract

For `customer-managed`, customers continue to provide non-secret connection
metadata at bundle generation:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --set-file \
    slinkyslurm:accounting.storageConfig=./accounting-storage.yaml
```

The file contains fields such as host, port, database, username, and a Secret
name/key reference. It does not contain a password. `accounting.enabled` is
not accepted in that file because the recipe owns it.

An in-cluster Service DNS name and an external DNS name are handled
identically. AICR does not infer ownership from the hostname or from the
presence of MariaDB Operator APIs.

For `aicr-provided`, AICR supplies and locks a compatible connection contract:

- Service host `mariadb`;
- port `3306`;
- database `slurm_acct_db`;
- username `slurm`; and
- credential Secret `mariadb-password`, key `password`.

The `slurm-accounting-mariadb` component configures the initial database, user,
all-database privileges, and generated runtime Secret directly on the MariaDB
resource. The bundle carries `generate: true` and the Secret reference, never
the generated credential.

The initial implementation keeps the AICR-provided connection contract fixed.
Making its names configurable is a follow-up that must update both producers
and consumers atomically and add the affected paths to the accounting lock.

### 7. Validation

Validation is split across three boundaries.

#### Recipe generation

- Validate the enum and its applicability to a Slurm recipe.
- Materialize the default.
- Apply and record the derived mode-owned values.
- With a supplied snapshot, record official MariaDB Operator conflict evidence
  in recipe metadata. Warn for `api-detected`, `crs-detected`, and `unknown`
  without blocking recipe generation.
- Do not require MariaDB Operator absence for `disabled` or
  `customer-managed`; a customer-managed in-cluster database may legitimately
  use it.

The existing `K8s.mariadb-operator` collector is conflict evidence only. It
reports official API/resource presence and intentionally does not claim
operator or database health.

#### Bundle generation

- Reconstruct the final effective values after recipe values and all override
  forms.
- Require every protected gate and protected component-presence state to equal
  the recipe-selected mode.
- Reject `--dynamic` or install-time exposure of protected paths.
- Require customer-managed connection metadata to be structurally complete.
- Require AICR-provided connection values to match the pinned cross-component
  contract.
- Fail before writing output when effective values cannot be reconstructed.

This follows the existing bundle-time coherence-check pattern: the final gate
runs where all static override inputs are known.

#### Deployment and runtime

Mode-specific readiness checks are:

- `disabled`: no Accounting resource or SlurmDBD workload expected from AICR;
- `customer-managed`: referenced Secret exists, the database endpoint is
  reachable from the Slurm namespace, and the Slinky Accounting/SlurmDBD
  workload becomes ready; and
- `aicr-provided`: CRDs established, operator ready, MariaDB resource ready,
  operator-generated initial User, Database, and Grant resources ready,
  generated Secret present, Service reachable, and Slinky Accounting/SlurmDBD
  ready.

Connection probes must be bounded, non-destructive, and avoid logging Secret
contents. A network-only probe is insufficient when authentication failure
would still leave SlurmDBD unusable; the implementation should use the
least-privileged authenticated readiness operation available.

After deployment, normal `aicr validate` uses the recipe mode to choose the
expected checks. Because normal validation requires a recipe, the selected
mode is available to both validation and recipe-evidence emission.

### 8. Interaction with configuration profiles

Accounting is not a second ADR-015 profile:

- exactly one configuration profile may still be selected per resolved
  recipe;
- accounting may coexist with that profile;
- accounting cannot reuse `--profile` because Slurm recipes may also need a
  GPU ownership profile; and
- no generic facility for declaring arbitrary independent typed parameters is
  introduced.

This is an explicit, Slurm-specific exception to ADR-015's rejection of a
generic parameter language. It is admitted because its schema, values,
effects, validation, and ownership lock are closed in code.

When a Slurm recipe has both a selected profile and accounting configuration,
their owned paths must be disjoint. Any overlap is a generation-time error;
there is no precedence rule. Qualification covers the combinations that a
Slurm family exposes, even though the two selections use separate fields.

Profiles and accounting use one shared component-path ownership model.
Exact, ancestor, and descendant intersections use the same rule. Accounting
remains typed configuration, not a profile. It declares one mode-specific
ownership set in code. Recipe generation and bundle-time enforcement consume
that same set.

Each owner keeps its own override policy. Accounting rejects every explicit
override attempt on an owned path, even when the value is unchanged. Profile
override behavior remains defined by ADR-015.

### 9. Installation-managed ownership boundary

For `aicr-provided`, AICR owns:

- chart and image version pins;
- CRD, operator, and database-resource packaging;
- dependency ordering;
- initial Secret generation wiring;
- SlurmDBD connection wiring;
- render, coherence, and readiness checks; and
- compatibility testing for upgrades shipped in later AICR releases.

The customer owns:

- selecting and sizing the StorageClass and persistent capacity;
- backup destination, schedule, retention, and restore testing;
- availability topology beyond AICR's documented default;
- monitoring and alert response;
- capacity management;
- incident response;
- credential rotation after installation; and
- deciding when to apply a later AICR bundle containing an upgrade.

Documentation must call this **AICR-provided** or
**installation-managed**, never a fully managed database service.

## Compatibility and Migration

Legacy recipes have no accounting mode. Bundle generation preserves today's
default-disabled behavior when no accounting override is present, but
validation treats the ownership mode as unspecified: absence in an old
artifact is not evidence that accounting was intentionally disabled.

During a documented compatibility window, a legacy recipe may continue using:

```shell
--set slinkyslurm:accounting.enabled=true
```

That legacy form means only `customer-managed`; it can never select
`aicr-provided`. It emits a deprecation warning directing the customer to
regenerate the recipe. Because the override is absent from the recipe, normal
recipe validation and recipe evidence remain unable to attest the legacy
selection.

For a new mode-bearing recipe:

- direct overrides of `accounting.enabled` or a MariaDB install gate fail;
- `--dynamic` on those paths fails;
- install-time mutations are guarded where the deployer has a typed schema or
  hook surface; and
- manual post-generation edits remain outside AICR's interception boundary
  and surface as recipe/deployment drift during validation.

The Slurm family conversion is a qualification and evidence cut-over:

- resolved recipe bytes and digests change;
- evidence for each supported mode is generated separately;
- existing disabled clusters remain operationally unchanged;
- published examples switch from `accounting.enabled` to
  `--slurm-accounting-mode`; and
- users of older AICR binaries must regenerate only after upgrading to a
  version that understands the accounting-bearing recipe schema.

## Implementation Plan

### Phase 0: Confirm component packaging

1. Select and pin compatible MariaDB Operator CRD and operator chart versions.
2. Render the upstream charts and identify whether they have a true whole-chart
   no-op value.
3. Choose the thin-wrapper or registry install-gate implementation when they
   do not.
4. Define the initial AICR-provided storage, replica, resource, security, and
   backup defaults explicitly; do not inherit mutable upstream defaults.
5. Verify the `mariadb-password/password` generation contract against the
   selected operator version.

Exit criterion: each proposed component renders zero resources with
`install: false`, and the enabled graph renders in CRD, operator, database,
Slinky order.

### Phase 1: Typed recipe contract

1. Add `AccountingMode` and closed recipe configuration structs in
   `pkg/recipe`.
2. Add `configuration` to `RecipeResult` and the recipe request/config types,
   plus finalization, deterministic serialization, query projection, and
   strict decoding. Catalog `RecipeMetadata` does not author this field.
3. Add CLI, AICRConfig, SDK, and resolving-API inputs with one normalization
   path.
4. Default and materialize `disabled` for Slurm recipes.
5. Add the required artifact-version and compatibility gates.
6. Include the mode in canonical recipe digests and evidence projection
   tests.

Exit criterion: the same selection through every supported input surface
produces byte-identical resolved recipes, and older/unknown consumers fail
closed.

### Phase 2: Components and derivation

1. Add registry entries, values, manifests, dependency edges, scheduling
   paths, health checks, and pinned versions for the MariaDB components.
2. Add all component references to every supported Slurm family.
3. Derive Slinky and MariaDB install gates from the recipe mode.
4. Implement no-op handling for install-false components across all
   deployers, mirror discovery, BOM, health, and readiness.
5. Regenerate the container-image BOM documentation.

Exit criterion: all three modes retain the same recipe component names, while
rendered runtime inventories match the mode table.

### Phase 3: Coherence and pre-install safety

1. Add one shared accounting ownership descriptor and effective-value
   evaluator.
2. Enforce it in direct bundler, CLI, server, mirror, and deployer-specific
   install-time surfaces.
3. Reject contradictory static overrides, dynamic values, and component
   filters before output.
4. Add the snapshot MariaDB conflict constraint for `aicr-provided`.
5. Add bounded customer connection and AICR-provided dependency readiness
   gates.

Exit criterion: no supported AICR surface can produce or expose a bundle whose
effective accounting ownership differs from the resolved recipe.

### Phase 4: Runtime validation and evidence

1. Make Slinky accounting health assertions mode-aware.
2. Add MariaDB CRD, operator, instance, Service, Secret-reference, and
   SlurmDBD readiness assertions.
3. Extend safe collector readings only where runtime validation lacks a
   required signal; do not collect credential values.
4. Add recipe-evidence tests proving mode changes alter the recipe digest and
   select different validation expectations.
5. Add separate evidence pathing or labels when required so modes cannot
   overwrite each other's published result.

Exit criterion: `aicr validate` distinguishes all three deployed outcomes and
the signed evidence unambiguously binds the expected mode.

### Phase 5: Qualification, migration, and documentation

1. Add unit tests for enum parsing, defaulting, applicability, version gates,
   merge behavior, deterministic output, legacy behavior, and override locks.
2. Add render tests for every supported deployer and all three modes.
3. Add KWOK coverage for disabled and renderable enabled modes; use a real
   Kubernetes environment for database and SlurmDBD readiness.
4. Qualify every Slurm recipe/profile combination affected by the new
   components.
5. Update the component catalog, CLI/API references, recipe development
   documentation, accounting guide, and guided demos.
6. Document the legacy override deprecation window and upgrade path.
7. Run `make bom-docs`, the affected-package coverage gate, required
   `golangci-lint` package scans, and `make qualify`.

## Acceptance Criteria

| Scenario | Required outcome |
|---|---|
| No accounting input | Resolved Slurm recipe records `disabled`; no accounting or MariaDB resources render |
| Explicit `disabled` | Byte-equivalent to the default selection |
| Customer-managed, in-cluster DB | SlurmDBD renders with customer Service and Secret references; AICR MariaDB resources do not |
| Customer-managed, external DB | Same ownership behavior; external endpoint is accepted and checked |
| AICR-provided | CRDs, operator, MariaDB instance, generated Secret contract, and SlurmDBD render in dependency order |
| Mode plus conflicting `accounting.enabled` | Bundle generation fails before output |
| Mode plus conflicting install gate | Bundle generation fails before output |
| Protected `--dynamic` path | Bundle generation fails |
| AICR-provided plus MariaDB CRs or inconclusive detection | Recipe generation warns and records metadata; bundle generation blocks with `customer-managed` remediation |
| AICR-provided plus MariaDB API only | Recipe and bundle generation warn; bundle generation proceeds |
| AICR-provided without snapshot evidence | Bundle generation warns that conflicts were not evaluated and proceeds for query-mode and older-snapshot compatibility |
| Legacy recipe plus accounting enable override | Customer-managed behavior during compatibility window, with deprecation warning |
| Mode changed and recipe regenerated | Canonical recipe digest changes |
| Credential inspection | No password appears in recipe, bundle values, logs, snapshot, or evidence |

## Initial Product Defaults

The first implementation uses deliberately conservative installation defaults:

1. a 20 GiB persistent volume with no StorageClass selected by AICR; the
   customer selects the StorageClass and may override capacity;
2. one standalone MariaDB replica with Galera disabled;
3. no backup resource; backup destination, schedule, retention, and restore
   testing remain customer-owned;
4. explicit operator and database resource requests and limits in the
   component values; and
5. no automatic database upgrade. AICR pins MariaDB Operator, CRD, and cluster
   chart versions together, tests later pins as a unit, and the customer
   chooses when to install a later bundle.

Changing these defaults does not change the three-mode API, but it requires
the same chart render, compatibility, BOM, and readiness qualification as the
initial pin.
