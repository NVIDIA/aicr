# ADR-028: Scale-Out Fabric Selection in Recipes

## Status

**Proposed** — 2026-10-29. Answers
[#3503](https://github.com/NVIDIA/aicr/issues/3503). Amends
[ADR-015](015-recipe-configuration-profiles.md) by widening what a `gpuStack`
profile value may encode; ADR-015 receives its reciprocal "Amended by
ADR-028" banner when this ADR is accepted. Related:
[#3439](https://github.com/NVIDIA/aicr/issues/3439) (rail gate follow-up),
[#1256](https://github.com/NVIDIA/aicr/issues/1256) (fabric-specific NCCL
floors, which this ADR makes expressible),
[cluster-readiness-engine#576](https://github.com/NVIDIA/cluster-readiness-engine/issues/576)
(the NVCRE side of the contract).

## Decision Summary

The same accelerator ships behind different scale-out fabrics, and the fabric
changes which components a recipe deploys and how NCCL validation must run.
AICR records the fabric as a recipe-time fact. It is expressed through the
existing `gpuStack` profile, whose values gain a reserved `fabric` marker
(`efa | ib | roce | tcpxo`), recorded in the resolved recipe and handed to
the NCCL validators and NVCRE. No second profile and no new criteria
dimension are added. Where no profile declares it, consumers derive the
fabric from the recipe's composition through one shared function, so
nothing is written into existing recipes.

**Bottom line: this ADR changes no existing recipe.** Fabric enters a recipe
only when an overlay family gains its second fabric value. That is a
versioned overlay change shipped with refreshed evidence, like any other.

## Problem

Scale-out fabric used to follow from service and accelerator. Today AICR
derives it that way: the NCCL validator picks its runtime template from a
`(service, accelerator)` matrix. That no longer holds:

| Target | Accelerator | Fabric | How AICR configures it today |
|---|---|---|---|
| EKS P6e-GB200 / P6e-GB300 | GB200, GB300 | **EFA** (SRD) | `aws-efa` device plugin (`eks.yaml`); NCCL via `aws-ofi-nccl`; resource `vpc.amazonaws.com/efa` |
| EKS GB300, internal rack | GB300 | **RoCE** | internal `aws-net-dra` component, not in the public catalog |
| GKE A3 Mega (H100) | H100 | **TCPXO** | `gke-nccl-tcpxo` installer + NRI device injector |
| GKE A4 / A4X | B200, GB200 | **RoCE** | `gke-gb200-rdma` gIB installer; Managed DRANET `mrdma.google.com` |
| GKE A4X Max | GB300 | **RoCE** | No AICR networking component and no performance check yet; DRANET is a cluster prerequisite |
| AKS ND H100 v5 | H100 | **InfiniBand** | `network-operator` shared RDMA plugin, resource `hca_shared_devices_a`, selected by vendor and driver |
| OKE GB200 | GB200 | **InfiniBand** | `network-operator` shared RDMA, `linkTypes: [infiniband]`, `rdma0..3` |
| OKE L40S | L40S | **RoCE** | `network-operator` SR-IOV plugin + `nv-ipam` + multus |
| NVKE GB200 | GB200 | **InfiniBand** (SR-IOV IPoIB) | `network-operator` with `sriovNetworkOperator`; `sriov-ib-networks` component with one `SriovNetworkNodePolicy` (`linkType: IB`), `SriovIBNetwork`, and `IPPool` per rail; resources `nvidia.com/sriov_resource_rail_<n>` |
| Nebius mK8s B300 | B300 | **InfiniBand** | `network-operator` shared RDMA selected by `ib*` interface names, resource `rdma/ib` |
| Generic / bare metal GB300 | GB300 | **InfiniBand** today; Spectrum-X RoCE shipping | `network-operator` shared RDMA, `linkTypes: [infiniband]`, device IDs 1021 and 1023, resource `rdma/ib` |

Outside AWS the GB300 NVL72 reference design pairs one ConnectX-8 per GPU
with either Quantum-X800 InfiniBand or Spectrum-X Ethernet (RoCE). Azure ND
GB300 v6 is InfiniBand, CoreWeave offers both, GKE is RoCE. So "GB300" alone
does not determine the fabric. The fabric is not a tuning knob: it selects
different components (`aws-efa` vs `network-operator`), different
NicClusterPolicy shapes (shared RDMA vs SR-IOV with multus and IPAM),
different NCCL plugins, and different bandwidth floors.

The immediate consumer is NCCL validation. AICR is moving the NCCL checks
from TrainJob to NVCRE (ADR-025). NVCRE renders the certification workload
per fabric. Today it can render AWS GB300 only as RoCE, because that render
came from the internal rack, while the public product is EFA. NVCRE needs
AICR to say which fabric the recipe targets. The recipe is a design-time
artifact (ADR-015), so the answer cannot come from runtime detection inside
the validator: two runs of the same recipe must validate the same thing.

## Goals

- **Existing recipes are untouched.** A recipe generated from an unchanged
  overlay family resolves to byte-identical output: same digest, same
  evidence coordinates, same lock template, same artifact version. Every
  other goal is subordinate to this one.
- Every resolved recipe has at most one fabric value, determinable from the
  recipe alone before validation runs.
- The fabric selects the right components and manifests on overlays where
  more than one fabric ships for the same accelerator.
- Snapshot-driven generation infers the fabric where the snapshot can see
  it, and fails closed when the snapshot contradicts an explicit selection.
- The NCCL validators and the NVCRE integration read the fabric from the
  recipe, never from the cluster.

## Non-Goals

- Modeling rail topology, NIC counts, or per-rail bandwidth. Those stay in
  the snapshot `NetworkTopology` measurement and in validator thresholds.
- Choosing between EFA and RoCE on AWS for the public catalog. The public
  product is EFA; the RoCE variant stays an internal overlay.
- Multi-fabric clusters (mixed IB and RoCE node pools). Out of scope until a
  user appears; a mixed snapshot fails closed.
- Changing how `--set` or `--dynamic` work.

## Context

### What the snapshot already knows

The network collector ingests the
Network Operator's `NicDevice` objects through k8s-launch-kit and emits a
`NetworkTopology` measurement. Its `capabilities` subtype exposes `sriov`,
`rdma`, and `ib` as addressable scalars. The link type (`Ethernet` |
`InfiniBand`, or empty when discovery could not prove it) is emitted as
`identity.linkType`, but as a Context key, and the constraint catalog
deliberately rejects Context keys: `NetworkTopology.identity.linkType` fails
at load today (`pkg/measurement/catalog.go`, `docs/integrator/measurement-api.md`).
The snapshot says nothing about EFA on EKS, where the signal is
`vpc.amazonaws.com/efa` in node allocatable, or about RoCE on GKE, where it
is the `mrdma.google.com` DeviceClass.

### What ADR-015 allows

A composition carries at most one profile
declaration, with exactly one selected value. Independent parameters and
their Cartesian product were rejected (Alternative C). `gpuStack` is
declared on `aks.yaml`, `gke-cos.yaml`, and `oke-ol.yaml`. Each value must
carry a validation signal that distinguishes it from every sibling. A
fragment may change how components are configured, never the component
set: a root `enabled` override is rejected at catalog load. Conditional
installation is expressed through a values-gated component, whose manifest
renders nothing unless a nested gate value is selected; `gcp-driver-installer`
is the live example. The declaration already carries one piece of reserved
vocabulary, `advertiser: external`, which feeds the #1327 lock closure. The
selected `name=value` pair is the evidence path segment
(`recipes/evidence/<recipe>-gpustack-<value>/`). A profile-bearing result is
stamped `aicr.run/v1beta2` (ADR-015's text still says `v1alpha3`, its
original name) and is served only on `/v2` endpoints.

### What NVCRE proposed

In the thread that started this ADR, the NVCRE
owner proposed adding `spec.fabric` (`efa | ib | roce | tcpxo`) to the
NVCRE APIs with this resolution order:

1. `spec.fabric` set: use it and validate it against the cluster.
2. Unset: detect from the cluster (`vpc.amazonaws.com/efa` allocatable,
   `roce.networking.k8s.aws` ResourceSlices, and similar).
3. Unset and inconclusive: fall back to today's platform plus GPU
   architecture mapping.

AICR always takes path 1. Paths 2 and 3 exist for NVCRE's standalone users.

## Options

### A. New criteria dimension: `fabric: efa | ib | roce | tcpxo`

Add a sixth criteria axis next to `service`, `accelerator`, `os`, `intent`,
`platform`. Overlays declare it in `criteria`; `aicr recipe --fabric roce`
selects it; the snapshot infers it like `accelerator`.

- Pro: semantically the cleanest. Fabric is a hardware fact, which is what
  criteria describe. Every surface already handles criteria uniformly. No
  interaction with profiles.
- Con: a new enum appears everywhere criteria are enumerated: OpenAPI
  `enum:` blocks, `pkg/recipe/criteria.go`, CLI usage strings, docs pages,
  issue templates, and every leaf overlay. Every recipe coordinate grows a
  dimension, which touches evidence pointers and the artifact compatibility
  gate. For most overlays the value is forced by `service`, so the axis is
  redundant on all but a few leaves. The thread's maintainer position was
  that this blast radius is to be avoided if possible, and that if it is
  ever taken, all downstream changes are reviewed for impact first.

### B. New profile: `fabric` beside `gpuStack`

Declare a second profile on the overlays where two fabrics ship, with a
declared default per overlay.

- Pro: small and local. Only the overlays with two fabrics declare it. EKS
  stays EFA with no declaration.
- Con: breaks the ADR-015 one-declaration rule. Today OKE GB200 ships IB and
  OKE L40S ships RoCE on different leaves, so nothing collides yet. The
  first OKE GB300 leaf that offers both fabrics would, because `oke-ol.yaml`
  already declares `gpuStack`. Allowing two declarations makes the resolved
  recipe a merge of one value from each. The valid set becomes the
  Cartesian product. Profile-owned paths must be disjoint, or merge order
  becomes a third parameter. `selectedProfile` turns into a list, which
  changes the evidence segment, the lock template, and the wire model.

### C. Extend the existing `gpuStack` profile

Keep one declaration. Each `gpuStack` value states its fabric through a
reserved `fabric` marker. Overlays with two fabrics declare one value per
(driver mode, fabric) pair.

- Pro: cheapest. No ADR-015 exception, no new criteria enum, one optional
  field in the wire model, and the evidence segment keeps its
  `gpustack-<value>` shape. Overlays with one fabric change nothing; their
  fabric is derived.
- Con: the value becomes a tuple. On OKE the set can grow from two values to
  four, and every new driver mode or fabric multiplies it. The product
  ADR-015 rejected is still there, written out as explicit values instead
  of computed by a merge. `gpuStack` stops meaning only driver ownership.
  Overlays with no driver choice, such as GB300 generic, must declare
  `gpuStack` with a single driver mode to carry the fabric.

### D. Detect at validation time, or `--dynamic`

Let the validator or NVCRE detect the fabric from the nodes, or make it a
bundle-time `--dynamic` value.

- Pro: nothing to declare. NVCRE already detects for its standalone users.
- Con: rejected on the thread and by ADR-015. The recipe is a design-time
  construct; at recipe time AICR does not know which cluster it will be
  deployed to unless a snapshot is supplied, and once generated the recipe
  must validate the same thing on every run. Detection makes the validated
  configuration depend on the cluster rather than on the artifact, which
  defeats the "validated against deployed config" claim that evidence
  rests on. `--dynamic` is a bundle-time override surface for
  validation-neutral tweaks; fabric is not validation-neutral.

### Comparison

| | A. Criteria | B. New profile | C. Extend gpuStack | D. Detect / dynamic |
|---|---|---|---|---|
| Change surface | Large: CLI, API, docs, all overlays | Medium: resolver, wire model, evidence | Small: marker, few overlays | Small |
| ADR-015 impact | None | Reopens one-declaration rule | Widens value semantics only | Violates design-time principle |
| Value explosion | None | None (orthogonal) | M×N on overlays with both axes | None |
| Snapshot inference | Same new readings | Same new readings | Same new readings | Runtime detection instead |
| Evidence coordinates | New dimension in every path | Two profile segments | Unchanged shape | Fabric not in evidence |
| Fit for EKS (single fabric) | Redundant axis | No declaration | No declaration; derived | Detected |
| Readability | Best | Good | Worst: tuple in a name | n/a |
| Reversibility | Hard: criteria are public contract | Medium | Easy: values can be split later | n/a |

## Decision

**Option C.** Extend `gpuStack` with a reserved `fabric` marker. It is the
cheapest path that satisfies the goals and keeps every ADR-015 invariant
intact. The known costs are accepted and made explicit rather than implied
by value names: the tuple is written as `fabric:` on each value, not
inferred from a `-roce` suffix.

Option A remains the right answer if a fourth axis arrives, or if more than
a handful of overlays end up with both a driver choice and a fabric choice.
Taking it requires a prior review of every downstream change, per the
maintainer condition recorded above. The `fabric` marker is designed so
that promotion to a criteria dimension later is mechanical: the value set
and the recorded field are the same.

## Proposal

### Declaration

The `gpuStack` value schema gains one reserved key. The example is an OKE
GB300 leaf after a RoCE target has been qualified; the per-fabric policy
components are introduced in "Switching the NicClusterPolicy" below.

```yaml
profile:
  name: gpuStack
  default: oci-managed
  values:
    oci-managed:                 # Oracle image + add-on, InfiniBand
      fabric: ib
      advertiser: external       # unchanged; fabric and advertiser coexist
      componentRefs:
        - name: gpu-operator
          overrides: {...}       # unchanged driver-ownership overrides
        - name: network-operator-policy-ib
          overrides: {policy: {enabled: true}}
        - name: network-operator-policy-roce
          overrides: {policy: {enabled: false}}
      constraints:
        - name: K8s.oke-addons.nvidia-gpu-plugin
          value: installed
        - name: NetworkTopology.identity.linkType
          value: InfiniBand
    oci-managed-roce:            # Oracle image + add-on, Spectrum-X RoCE
      fabric: roce
      advertiser: external
      componentRefs:
        - name: gpu-operator
          overrides: {...}       # identical to oci-managed; union totality
        - name: network-operator-policy-ib
          overrides: {policy: {enabled: false}}
        - name: network-operator-policy-roce
          overrides: {policy: {enabled: true}}
      constraints:
        - name: K8s.oke-addons.nvidia-gpu-plugin
          value: installed
        - name: NetworkTopology.identity.linkType
          value: Ethernet
    operator-managed:
      fabric: ib
      # ... same shape as oci-managed, with operator-owned driver overrides
```

Rules, enforced at catalog load:

- `fabric` is one of `efa`, `ib`, `roce`, `tcpxo`. It is a closed
  vocabulary validated by a new check, persisted verbatim like
  `advertiser`, and included in the digest. Unlike `advertiser` it has
  several values and triggers no lock closure. It may appear on a value
  that also carries `advertiser: external`.
- If any value of a declaration carries `fabric`, every value must. A
  declaration either models the fabric or it does not.
- Values that differ only in `fabric` must still satisfy ADR-015 union
  totality and the distinguishing-constraint rule. The gate paths on the
  policy components are ordinary owned value paths, so totality holds the
  same way it does for `gcp-driver-installer` today.
- Value naming keeps the ADR-015 vocabulary. The driver-mode part stays
  first. A fabric suffix is used only where two fabrics exist for the same
  driver mode, and it names the non-default fabric. Unmarked single-fabric
  values keep their names. The selected fabric is recorded in
  `selectedProfile.fabric` regardless of whether the name carries a suffix.

### Switching the NicClusterPolicy

The IB and RoCE shapes of a NicClusterPolicy differ structurally: shared
RDMA device plugin with an InfiniBand selector on one side, SR-IOV device
plugin plus `nv-ipam` plus multus and NetworkAttachmentDefinitions on the
other. Today every overlay ships the policy as a `manifestFiles` entry on
the `network-operator` component with the chart's `deployCR: false`. A
profile value has to pick one shape. Four ways to do that:

1. **Values-gated per-fabric manifest components.** This is the pattern
   ADR-015 blesses for conditional installation. Split the policy out of
   `network-operator` into manifest-only components, for example
   `network-operator-policy-ib` and `network-operator-policy-roce`, each
   carrying its NicClusterPolicy and, for RoCE, the IPAM pool and NADs.
   Both components sit unconditionally in the composition. Each manifest is
   wrapped in a gate on a nested value, `policy.enabled`, defaulting to
   off, exactly as `gcp-driver-installer` gates on `installer.enabled`. The
   profile value flips the gate through an ordinary owned override. The
   component set is identical across values, which is what ADR-015
   requires. Manifest-only Helm components already exist
   (`gke-nccl-tcpxo`, `gke-gb200-rdma`). Cost: two registry entries per
   fabric pair, the policy manifests move one directory, and the ADR-015
   standing cost applies: both gated components are locked for every
   value, including the one where they render nothing.
2. **Values-driven policy through the chart.** Set `deployCR: true` and
   express the policy under the chart's `nicClusterPolicy` values, as
   `values-mk8s-b300.yaml` already starts to do. The profile value then
   switches fabric with `overrides` alone. Cost: the OKE and generic
   overlays turned `deployCR` off on purpose, so the manifest CR is
   authoritative and the chart cannot fight it on upgrade. SR-IOV plus IPAM
   plus multus is a large, deeply nested values tree. Union totality would
   force every value to assign every nested path the other fabric touches.
3. **`manifestFiles` in the profile fragment.** Extend the fragment grammar
   so a value names its own manifest files on a component ref, recorded as
   a synthetic `<component>.manifestFiles` owned path. Smallest change to
   the overlays. Cost: a new fragment field with its own totality and lock
   rules, a new kind of owned path that is a file list rather than a values
   path, and a digest input that depends on file names.
4. **One manifest whose content varies per value.** Keep a single
   NicClusterPolicy file and render the fabric-specific parts from the
   selected value. Rejected: ADR-015 names free-form recipe templating as a
   non-goal. The line it draws is between a manifest that renders or does
   not render on a gate (option 1, blessed) and a manifest whose content
   changes with the value (this option).

**Preferred: option 1.** It is the pattern ADR-015 already defines, it
changes neither the profile grammar nor the chart ownership model, and the
lock machinery works as it does for `gcp-driver-installer`. Option 2 is the
fallback if the registry cost proves higher than expected. Option 3 is kept
in reserve for a future case where a single component must vary a manifest
without a presence gate.

### Derivation where no declaration models the fabric

Recipes from overlay families with no `gpuStack` declaration (EKS, generic
H100) and families whose declaration predates this ADR must still answer
the fabric question without changing. For those, a single function in
`pkg/recipe`, `FabricOf(recipe)`, derives it from the composition already
in the recipe. Nothing is persisted. The function is pure and
deterministic, so two readers of the same recipe bytes get the same answer.

Derivation keys on component identity and on manifest identity, never on
manifest content. Three of the six NicClusterPolicy manifests are Helm
templated, so a plain YAML parse is not available, and shape is not a
reliable signal anyway: AKS and Nebius use the shared RDMA plugin with no
`linkTypes` selector and are InfiniBand, while OKE L40S uses SR-IOV and is
RoCE.

| Composition contains | Derived fabric |
|---|---|
| `aws-efa` | `efa` |
| `network-operator-policy-ib` with its gate on | `ib` |
| `network-operator-policy-roce` with its gate on | `roce` |
| `network-operator` referencing a `nic-cluster-policy-*.yaml` manifest | the fabric registered for that manifest (table below) |
| `gke-gb200-rdma` | `roce` |
| `gke-nccl-tcpxo` | `tcpxo` |
| none of the above | unset; NCCL NET checks are not applicable |

| Manifest | Fabric |
|---|---|
| `nic-cluster-policy-aks.yaml` | `ib` |
| `nic-cluster-policy-generic-gb300.yaml` | `ib` |
| `nic-cluster-policy-nebius-b300.yaml` | `ib` |
| `nic-cluster-policy-nvke-gb200.yaml` | `ib` |
| `nic-cluster-policy-oke-gb200.yaml` | `ib` |
| `nic-cluster-policy-oke-l40s.yaml` | `roce` |

The manifest table lives in `pkg/recipe` beside `FabricOf`. Rows are
append-only: a row's fabric never changes once shipped, because a fabric
change is a new manifest file, not an edit to an old one, and a row is
removed only with the manifest it names. That is what keeps `FabricOf`
stable across AICR versions for a recipe whose bytes did not change. A
test fails when a `nic-cluster-policy-*.yaml` is added to the catalog
without a row.

"Unset" is reserved for compositions with no fabric-bearing component at
all, such as GKE A4X Max today, which matches its empty performance check
list. It is not a catch-all: a composition that carries `network-operator`
with a policy manifest missing from the table, or more than one signal
deriving different fabrics, fails catalog load rather than deriving unset.
A test enumerates the explicit no-fabric leaves and asserts every other
catalog leaf derives exactly one fabric.

`FabricOf` returns the declared marker when `metadata.selectedProfile.fabric`
is present and derives otherwise. Derivation is a fallback, not a second
source of truth. A declared marker must agree with every derivation signal
in the composition, components, gates, and manifest rows alike, or catalog
load fails: `fabric: roce` beside `aws-efa`, or `fabric: ib` beside
`gke-nccl-tcpxo`, is rejected.

### Recorded field

```yaml
metadata:
  selectedProfile:
    name: gpuStack
    value: oci-managed-roce
    fabric: roce              # present only when the declaration models fabric
    ownedPaths: {...}
```

`selectedProfile.fabric` is the only new persisted field. It appears only
in recipes resolved from a declaration that carries the marker. It enters
the digest, as every `selectedProfile` field does. No `metadata.fabric`
field is added: a derived fabric is computed by consumers through
`FabricOf`, never written, so a recipe from an unchanged family is
byte-identical to today. The evidence path segment stays
`gpustack-<value>`.

Only the `/v2` endpoints carry `selectedProfile`, so `selectedProfile.fabric`
appears on `/v2/recipe` and `/v2/query` where present. The OpenAPI contract
adds it as optional. `aicr query` shows it only when it is in the artifact;
no virtual selector is invented, because selectors read hydrated fields.
Go callers and validators use `FabricOf`.

### Snapshot inference and fail-closed behavior

Snapshot-driven generation applies the per-value constraints as ADR-015
already specifies. The signals this ADR needs do not exist as addressable
readings today, so each is new collector and catalog work:

- `NetworkTopology.identity.linkType`. Promote `linkType` from a Context
  key to a scalar Data key under `identity`. The measurement API documents
  this as a contract change, so it comes with a pseudo-version bump. The
  value summarizes one hardware group, the GPU node cohort the recipe
  targets, and is set only when discovery reached every node in that group
  and every east-west port agreed. Partial coverage, or nodes outside the
  group, cannot produce a value. An empty value is treated as no reading.
- `K8s.node.allocatable.efa` on EKS: a new reading from the k8s collector,
  aggregated over the GPU nodes the recipe targets with three states:
  `all` (every targeted node allocates `vpc.amazonaws.com/efa`), `none`,
  and `mixed`. `mixed` fails closed before any profile value is selected.
  `K8s.node` is a closed key space in the catalog, so this is a catalog
  entry as well as a collector change.
- `K8s.gke-dranet.mrdma` on GKE: a new reading reporting whether the
  `mrdma.google.com` DeviceClass exists and is usable by the GPU nodes the
  recipe targets. The DeviceClass is cluster-scoped, so the reading is
  bound to those nodes through their ResourceSlices rather than to the
  object's existence. It takes the same `all | none | mixed` states, and
  `mixed` fails closed.

These are live-cluster readings, unlike the AKS and GKE pool projections
(`--aks-gpu-pools`, `--gke-gpu-pools`), which come from a provider dump.
All three share one rule: a reading describes the whole targeted GPU cohort
or it is absent. One matching node never stands for the pool.

The ADR-015 resolution algorithm is unchanged, including its treatment of
an unavailable reading: on the snapshot path, a fabric-modeling family
whose distinguishing reading is absent fails generation with the existing
"reading unavailable, regenerate the snapshot" diagnostic. There is no
fabric-specific exception that defaults silently. Criteria-only generation
(no `--snapshot`) selects the declared default and records the constraints
unevaluated, as it does for `gpuStack` today. A snapshot whose reading
contradicts an explicit `--profile` selection fails with
`ErrCodeInvalidRequest` naming the reading. A snapshot whose cohort reading
is `mixed`, or that reports both link types across GPU nodes, fails closed;
mixed fabrics are out of scope.

This has a sequencing consequence: a family cannot convert to fabric
markers until every snapshot it is generated from can carry the reading.
The measurement work above is therefore a prerequisite for any family
conversion, and a family on the Network Operator path needs `linkType`
discovery to report a confirmed value on its supported clusters before it
converts. Unprofiled families (EKS among them) are unaffected.

### Consumers

- **NVCRE check.** `nccl-cre-all-reduce-bw` is the NVCRE-driven validator
  (`validators/performance/cre_nccl.go`). It calls `FabricOf` on the recipe
  it was handed and sets `spec.fabric` on the `Certification`, the field
  cluster-readiness-engine#576 introduces. AICR requires that an explicit
  `spec.fabric` wins over detection and that a mismatch with the cluster
  fails the run rather than switching fabric; NVCRE's proposal already
  validates an explicit value against the cluster.
- **TrainJob checks.** `nccl-all-reduce-bw-net` and `-nvls` keep their
  `(service, accelerator)` template matrix until the NVCRE cut-over. Moving
  that matrix onto `FabricOf` is possible but not required by this ADR.
- **Transport assertion.** `verifyTransportFromLogs` matches the NCCL
  banner `NCCL INFO Using network <plugin>` with a `(\S+)` capture and
  today rejects only `Socket`. The capture is too narrow for a per-fabric
  gate: the EKS fixture reads `Using network AWS Libfabric`, so the
  capture yields `AWS`. The assertion is reworked to parse the rest of the
  banner line and match it against an accept list per fabric: `efa`
  accepts `AWS Libfabric` and `Libfabric`; `ib` and `roce` accept `IB` and
  Google's `gIB`; `tcpxo` accepts `FasTrak`. Each provider name is backed
  by a log fixture before it is added, and `gIB` is confirmed against a
  GKE A4 run first. A declared fabric whose banner names another plugin
  fails. Limitation: NCCL prints `IB` for InfiniBand and RoCE alike, so an
  `ib` recipe running over RoCE, or the reverse, is not detectable from
  logs. The snapshot `linkType` constraint is the guard for that case.
- **Bandwidth floors.** Per-fabric floors become expressible, which is the
  shape #1256 asks for.
- **Evidence and attestation.** The attestation builder records `FabricOf`
  beside the profile segment it already records. The evidence path and the
  existing pointers do not change.
- **`aicr upgrade-check`.** A fabric change between two recipes is reported
  the same way a `selectedProfile` change is. If upgrade-check does not yet
  surface profile changes, that is added here.

### Overlay plan

Markers and declarations are added lazily: a family gains `fabric` only
when it gains a second fabric value. Adding a marker to an existing
declaration changes `selectedProfile` and therefore every digest in the
family. Adding a declaration to a family that has none is a full ADR-015
family conversion: the overlay (`RecipeMetadata`) moves from
`aicr.run/v1beta1` to `aicr.run/v1beta2`, the resolved `RecipeResult` moves
from `aicr.run/v1` to `aicr.run/v1beta2`, `ownedPaths` appears,
`/v1/recipe` and `/v1/bundle` reject the family, and every shipped evidence
pointer for the family must be re-signed. ADR-015
also forbids declaring a value that is not yet qualified, so a declaration
cannot land ahead of its second value.

| Overlay family | Change | Existing recipes |
|---|---|---|
| `eks.yaml` and leaves | None. Derived `efa`. | Unchanged |
| `gke-cos.yaml` | None. Derived `roce` on A4 and A4X, `tcpxo` on A3 Mega, unset on A4X Max. | Unchanged |
| `aks.yaml` | None. Derived `ib`. | Unchanged |
| `oke-ol.yaml` and OKE leaves | None until an OKE GB300 leaf with a qualified RoCE target exists; then the declaration moves per accelerator (see "OKE declaration scope"). | Unchanged now |
| `gb300-generic-ubuntu-training.yaml` | None until a Spectrum-X RoCE GB300 cluster is qualified; then a new `gpuStack` declaration, default `operator-managed` (`fabric: ib`) reproducing today's composition exactly, plus `operator-managed-roce`, and the per-fabric policy components. | Unchanged now. At conversion: overlay and result version bumps, `/v1` rejection, and the three signed pointers under `recipes/evidence/gb300-generic-ubuntu-training` re-signed in the same release. |
| `gb200-nvke-ubuntu-training.yaml`, `b300-mk8s-*` | None. Derived `ib`. | Unchanged |

**OKE declaration scope.** `oke-ol.yaml` carries the one `gpuStack`
declaration for every OKE leaf: A100, GB200 (InfiniBand), and L40S (RoCE)
all inherit the same `oci-managed` default. A fabric marker cannot be
added there, because one value would label two fabrics, and a leaf cannot
add a second declaration, because ADR-015 deduplicates by declaring source
and fails composition-wide uniqueness otherwise. When the first OKE GB300
RoCE target is qualified, the declaration moves down one level: it is
removed from `oke-ol.yaml` and declared, with identical values plus the
correct marker, on each per-accelerator overlay (`gb200-oke-training`,
`gb200-oke-inference`, `l40s-oke-training`, `l40s-oke-inference`,
`a100-oke-training`, and the new GB300 leaf, which adds `*-roce`). Every
OKE recipe's digest moves once, because `selectedProfile.fabric` appears.
No OKE evidence pointer is committed today, so nothing is re-signed. The
move ships as one change with a golden test showing that each leaf's
resolved composition is unchanged apart from the marker.

This ADR by itself changes no recipe. GB300 generic is expected to convert
first, because Spectrum-X GB300 racks are shipping now. That conversion is
its own change, reviewed against the costs listed above.

## Consequences

- **No existing recipe changes.** Every shipped overlay family resolves
  byte-identically, with the same digest, artifact version, evidence
  coordinates, and lock template. The artifact compatibility gate and the
  committed evidence pointers pass unchanged. A golden test resolves every
  catalog leaf before and after and asserts equality.
- One function, `FabricOf`, answers "which fabric" for the CLI, the API,
  evidence, NVCRE, and the validators. The answer is fixed by the recipe
  bytes.
- The ADR-015 one-declaration rule, resolution algorithm, lock template,
  component-set rule, and evidence coordinate shape are unchanged.
- `gpuStack` values on overlays with both axes become tuples. The value
  list on OKE can reach four entries. The marker keeps the tuple explicit.
  Splitting `fabric` into a criteria dimension later is mechanical because
  the value set and the recorded field already exist.
- The NicClusterPolicy moves out of `network-operator` into per-fabric
  values-gated components on the overlays that gain a second fabric. Both
  components are locked for every value, the standing ADR-015 cost.
- A family's first conversion to two fabrics is a full ADR-015 family
  conversion, with the overlay and result version bumps, `/v1` rejection,
  and evidence re-signing that implies. It is scheduled per family, not by this ADR.
- Overlays with no driver choice carry a one-mode `gpuStack` declaration to
  model fabric. The name is wider than it reads. This is the accepted cost
  of Option C and is called out in the component catalog.
- Three measurement changes: `linkType` promoted to an addressable scalar,
  and two new readings for EFA allocatable and the GKE mrdma DeviceClass.

## Adoption plan

1. `FabricOf`, the derivation and manifest tables, and a golden test
   asserting every current catalog leaf resolves byte-identically. This
   lands first and alone, so the compatibility invariant is proven before
   any schema change.
2. Catalog schema: the `fabric` marker and its load-time validation. Wire
   model: `selectedProfile.fabric` on `/v2`, OpenAPI update, api-diff and
   openapi-diff gates.
3. Measurement: promote `linkType` to a scalar with the pseudo-version
   bump; add the EFA and mrdma readings; fail-closed tests. A prerequisite
   for step 5, since an absent reading fails generation.
4. Validators and NVCRE: `nccl-cre-all-reduce-bw` passes `spec.fabric`;
   per-fabric transport assertion; per-fabric floors. Lands with
   cluster-readiness-engine#576.
5. Per family, in the release where its second fabric is qualified: the
   `gpuStack` declaration or markers, the second value, the per-fabric
   policy components, and re-signed evidence, together. GB300 generic
   first; OKE, with the declaration moved per accelerator, when a RoCE OKE
   GB300 target exists.
6. Docs: `docs/contributor/recipe.md` profile vocabulary, component catalog
   notes, CLI and API references.

## Open questions

- Whether `K8s.node.allocatable.*` should become a general reading rather
  than an EFA-specific one.
- Whether the TrainJob variants should move their template selection onto
  `FabricOf` before the NVCRE cut-over, or be left alone until retired.
