# ADR-028: Scale-Out Fabric Selection in Recipes

## Status

**Proposed** — 2026-10-29. Answers
[#3503](https://github.com/NVIDIA/aicr/issues/3503). Amends
[ADR-015](015-recipe-configuration-profiles.md) by widening what a `gpuStack`
profile value may encode and by letting a leaf declaration shadow an
inherited one; ADR-015 receives its reciprocal "Amended by
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
| GKE A4X | GB200 | **RoCE** | `gke-gb200-rdma` gIB installer; Managed DRANET `mrdma.google.com` |
| GKE A4 | B200 | **RoCE** | No AICR networking component on the B200 leaves; DRANET is a cluster prerequisite |
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

The immediate consumer is NCCL validation. Beside the TrainJob checks, AICR
offers NVCRE-driven checks as an opt-in path (ADR-025 keeps them off stock
overlays and names replacing the TrainJob checks a non-goal). NVCRE renders
the certification workload per fabric. Today it can render AWS GB300 only
as RoCE, because that render came from the internal rack, while the public
product is EFA. NVCRE needs AICR to say which fabric the recipe targets.
The recipe is a design-time artifact (ADR-015), so the answer cannot come
from runtime detection inside the validator: two runs of the same recipe
must validate the same thing.

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
original name) and is decoded strictly: an unknown field in a `v1beta2`
artifact is rejected (`pkg/recipe/decode.go`). The server routes only
`/v1/recipe`, `/v1/query`, and `/v1/bundle`, and those already serve
`selectedProfile`; the `/v2` endpoints some code comments still mention do
not exist.

### What NVCRE proposed

In the thread that started this ADR, the NVCRE
owner proposed adding `spec.fabric` (`efa | ib | roce | tcpxo`) to the
NVCRE APIs with this resolution order:

1. `spec.fabric` set: use it and validate it against the cluster.
2. Unset: detect from the cluster (`vpc.amazonaws.com/efa` allocatable,
   RoCE ResourceSlices published by the `dra.networking.k8s.aws` driver
   with `dra.vpc.amazonaws.com/deviceType: roce` devices, and similar;
   `roce.networking.k8s.aws` is the DeviceClass that selects them, not the
   slice driver, as cluster-readiness-engine#576 records).
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
| ADR-015 impact | None | Reopens one-declaration rule | Widens value semantics; adds leaf shadowing of one inherited declaration | Violates design-time principle |
| Value explosion | None | None (orthogonal) | M×N on overlays with both axes | None |
| Snapshot inference | Same new readings | Same new readings | Same new readings | Runtime detection instead |
| Evidence coordinates | New dimension in every path | Two profile segments | Unchanged shape | Fabric not in evidence |
| Fit for EKS (single fabric) | Redundant axis | No declaration | No declaration; derived | Detected |
| Readability | Best | Good | Worst: tuple in a name | n/a |
| Reversibility | Hard: criteria are public contract | Medium | Easy: values can be split later | n/a |

## Decision

**Option C.** Extend `gpuStack` with a reserved `fabric` marker. It is the
cheapest path that satisfies the goals and keeps every ADR-015 invariant
intact but one, the declaration-uniqueness rule, which gains the narrow
shadowing case described under "OKE declaration scope". The known costs are accepted and made explicit rather than implied
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
GB300 leaf after a RoCE target has been qualified; the gate key on
`network-operator` is introduced in "Switching the NicClusterPolicy" below.

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
        - name: network-operator
          overrides: {aicr: {fabric: ib}}    # selects the gated IB manifest
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
        - name: network-operator
          overrides: {aicr: {fabric: roce}}  # selects the gated RoCE manifest
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
  totality and the distinguishing-constraint rule. The gate key on
  `network-operator` is an ordinary owned value path, so totality holds
  the same way it does for `gcp-driver-installer` today.
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

1. **Values-gated per-fabric manifests.** This is the pattern ADR-015
   blesses for conditional installation: a manifest renders nothing unless
   a nested gate value is selected, exactly as `gcp-driver-installer` gates
   on `installer.enabled`. The profile value flips the gate through an
   ordinary owned override, and the component set is identical across
   values, which is what ADR-015 requires. Two placements are possible.

   **1a. Gated manifests on the existing `network-operator` ref**
   (preferred). The ref lists one policy manifest per fabric, for example
   `nic-cluster-policy-<target>-ib.yaml` and
   `nic-cluster-policy-<target>-roce.yaml` (the RoCE file also carrying the
   IPAM pool and NADs). Each file wraps itself in a gate on an AICR-owned
   key under the component's values, for example
   `network-operator.aicr.fabric`, which the profile value sets. The
   manifests already read `.Values "network-operator"` today
   (`nic-cluster-policy-generic-gb300.yaml`), so the key is in scope of the
   template. The chart tolerates the key: the pinned network-operator
   chart ships no `values.schema.json` of its own (checked at the pinned
   26.7.0, where only the bundled node-feature-discovery subchart carries a
   schema; the conversion PR re-checks against the pin it ships). Nothing changes
   hands: the live `nic-cluster-policy` CR stays in the
   `network-operator-post` release on every deployer, so an existing
   cluster upgrades in place with no ownership migration. Cost: the gate
   key is an owned path locked for every value, and the two readiness
   couplings below must learn to see through the gate.

   **1b. Separate manifest-only components**, `network-operator-policy-ib`
   and `network-operator-policy-roce`, both unconditionally in the
   composition. Same gate mechanics; the policy moves out of the
   `network-operator-post` release into a new one. That is a live-object
   ownership migration on every existing cluster of the family: Helm and
   Helmfile refuse to install a release over a resource another release
   owns, so the conversion must first re-label the live CR
   (`meta.helm.sh/release-name`, `meta.helm.sh/release-namespace`) to the
   new release before the install, and the Argo CD app-of-apps must drop
   the finalizer (`resources-finalizer.argocd.argoproj.io`) from the
   outgoing `network-operator-post` Application before it is pruned, or
   the cascading delete removes the policy and tears down OFED and the
   RDMA plugin until a later wave recreates it. A `roce` to `ib` switch
   on a live cluster hits the same sequence. The manifests also lose
   `.Values "network-operator"`, so every value they read must be copied
   into the new component. 1b is kept only as the fallback if the gate
   key proves unacceptable to a future chart version.

   **Couplings costed into both placements.** Two readers derive fabric
   resources from the policy manifest. `recipeDeclaresRDMAFabric`
   (`validators/deployment/expected_resources.go`) is a filename check on
   the `network-operator` ref alone, and `recipeAttachesNicClusterPolicy`
   (`pkg/bundler/readiness.go`) regex-scans raw manifest content across
   every ref. With two gated files both scans would see both fabrics and
   gate on resources that never appear. Both must evaluate the gate, by
   rendering the manifest with the component's values before extracting
   resources, and 1b must additionally point them at the new components.
   This is costed into step 5 together with an upgrade test from a
   pre-conversion bundle.
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

**Preferred: option 1, placement 1a.** It is the pattern ADR-015 already
defines, it changes neither the profile grammar nor the release that owns
the live policy, and the lock machinery works as it does for
`gcp-driver-installer`. Option 2 is the fallback if the gate key cannot be
carried on the chart's values. Option 3 is kept in reserve for a future
case where a single component must vary a manifest without a presence
gate.

### Derivation where no declaration models the fabric

Recipes from overlay families with no `gpuStack` declaration (EKS, generic
H100) and families whose declaration predates this ADR must still answer
the fabric question without changing. For those, a single function in
`pkg/recipe`, `FabricOf(recipe)`, derives it from the composition already
in the recipe. Nothing is persisted. The function is pure and
deterministic, so two readers of the same recipe bytes get the same answer.

Derivation keys on three things the resolved recipe already holds:
component identity, manifest identity, and, for a gated manifest, the
gate's value under the component's resolved values (`aicr.fabric` on
`network-operator`). It never reads manifest content. Five of the six
NicClusterPolicy manifests are Helm templated, so a plain YAML parse is
not available, and shape is not a reliable signal anyway: AKS and Nebius
use the shared RDMA plugin with no `linkTypes` selector and are
InfiniBand, while OKE L40S uses SR-IOV and is RoCE. A gated manifest
whose gate value is absent from the recipe is an error, not an open gate.

| Composition contains | Derived fabric |
|---|---|
| `aws-efa` | `efa` |
| A policy manifest in the table below, ungated or with its gate value selecting it | the fabric registered for that manifest |
| `gke-gb200-rdma` | `roce` |
| `gke-nccl-tcpxo` | `tcpxo` |
| `network-operator` with no policy manifest (kind), `network-operator-ocp-olm` alone, `dranet` (VR200), a namespace-only `network-operator` ref (Talos) | not a fabric signal |
| no fabric signal | unset; NCCL NET checks are not applicable |

`dranet` exposes ConnectX-9 RDMA ports through DRA and says nothing about
the fabric behind them, and the VR200 leaves run only the NVLS variant, so
they are explicit no-fabric leaves until a VR200 NET check exists.

| Manifest | Component | Fabric |
|---|---|---|
| `nic-cluster-policy-aks.yaml` | `network-operator` | `ib` |
| `nic-cluster-policy-generic-gb300.yaml` | `network-operator` | `ib` |
| `nic-cluster-policy-nebius-b300.yaml` | `network-operator` | `ib` |
| `nic-cluster-policy-nvke-gb200.yaml` | `network-operator` | `ib` |
| `nic-cluster-policy-oke-gb200.yaml` | `network-operator` | `ib` |
| `nic-cluster-policy-oke-l40s.yaml` | `network-operator` | `roce` |
| `nicclusterpolicy.yaml` | `network-operator-ocp` | `ib` |

The manifest table lives in `pkg/recipe` beside `FabricOf`. Each row also
carries the manifest's content digest. Rows are permanent:

- A row's fabric never changes once shipped. A fabric change is a new
  manifest file, not an edit to an old one.
- A row outlives its manifest. When a policy manifest is retired from the
  catalog, as a conversion to gated per-fabric manifests may do, its row
  stays and is marked retired. Recipes generated before the retirement
  still name the file, and so does the evidence signed over them.

That is what keeps `FabricOf` stable across AICR versions for a recipe
whose bytes did not change. Two tests back it. One fails when a policy
manifest is added to the catalog without a row. The other fails when a
shipped manifest's content changes without its row's digest being updated
in the same change, so every edit of a policy manifest is reviewed beside
the fabric it is registered under.

"Unset" is reserved for compositions with no fabric signal at all. Many
catalog leaves have none because they deploy no scale-out networking
component. Examples:

- GKE A100 (`gke-nccl-tcpxo` is omitted on purpose), A4 (B200), and A4X
  Max (GB300), and the GKE H100 inference leaves.
- The OKE leaves that deploy no network component (`a100-oke-training`,
  `gb200-oke-inference`, `l40s-oke-inference`).
- BCM, k0s, LKE, kind, OCP leaves that carry only the OLM subscription,
  Talos, and VR200.

None of them declares an NCCL NET check. Unset is not a catch-all: a
policy manifest missing from the table, or more than one signal deriving
different fabrics, is an error rather than unset. Two tests pin the
split. Every leaf that declares an NCCL NET check must derive exactly one
fabric. The set of leaves that derive unset is a committed golden list, so
a leaf that gains or loses a fabric signal changes that list and is
reviewed, rather than passing silently.

`FabricOf` returns the declared marker when `metadata.selectedProfile.fabric`
is present and derives otherwise. Derivation is a fallback, not a second
source of truth. `FabricOf` returns an error, never a guess, when a declared
marker disagrees with any derivation signal in the composition it is given
(components, gates, and manifest rows alike), or when signals disagree with
each other: `fabric: roce` beside `aws-efa`, or `fabric: ib` beside
`gke-nccl-tcpxo`. The check therefore runs wherever `FabricOf` runs. At
catalog load it fails the catalog. At resolution it runs again on the
composition that survives snapshot filtering and profile application,
which catalog load cannot see (ADR-015), and a conflict fails generation
before the recipe reaches any consumer. In the evidence verifier it
rejects the attestation (see "Consumers").

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

`/v1/recipe` and `/v1/query` already serve `selectedProfile`, so
`selectedProfile.fabric` appears there where present, and `/v1/bundle`
accepts it. The OpenAPI contract adds it as optional. `aicr query` shows it
only when it is in the artifact; no virtual selector is invented, because
selectors read hydrated fields. Go callers and validators use `FabricOf`.

**Compatibility rule.** `v1beta2` artifacts are decoded strictly, so a
binary that predates the marker rejects a fabric-marked recipe at decode
rather than silently dropping the field. That is the right direction, and
it sets the order of adoption: the schema change (step 2) ships at least
one release before the first family carries a marker (step 5), so by the
time a marked recipe exists, the previous release already reads it. The
rule holds for a family already on `v1beta2` (OKE) as much as for one
converting from `aicr.run/v1`. Unmarked recipes are unaffected either way.

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
  `mixed` fails closed. Any DRA-based reading, this one and any later EKS
  RoCE reading alike, filters on the slice's `spec.driver` and the device
  attributes that name the fabric, never on the presence of ResourceSlices
  as such: GB300 EKS nodes publish `compute-domain.nvidia.com` slices that
  say nothing about the fabric.

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

- **How validators receive it.** Validators are handed a
  `ValidationInput`, not the recipe, and it carries no `selectedProfile`
  (`pkg/validator/v1/validation_input.go`). A validator can derive from
  `componentRefs` but cannot see a declared marker. `ValidationInput`
  therefore gains a `Fabric` field, populated from `FabricOf(recipe)` where
  the input is built, exactly as `GPUAllocationPolicy` is resolved from the
  recipe today. Empty means no fabric: an unset derivation, or a standalone
  input built without recipe context.
- **NVCRE check.** `nccl-cre-all-reduce-bw` is the opt-in NVCRE-driven
  validator (`validators/performance/cre_nccl.go`), qualified today only on
  EKS H100 (`cre_qualification.go`). It copies `ValidationInput.Fabric`
  into `spec.fabric` on the `Certification`, the field
  cluster-readiness-engine#576 introduces. When `Fabric` is empty the check
  reports not applicable and does not run; it never omits `spec.fabric` and
  leaves NVCRE to detect, which would reintroduce Option D through the
  back door. AICR requires that an explicit `spec.fabric` wins over
  detection and that a mismatch with the cluster fails the run rather than
  switching fabric; NVCRE's proposal already validates an explicit value
  against the cluster. "Explicit wins" is enforced on the AICR side too,
  because today the Certification is created through the dynamic client
  with default create options (`createUnstructured`), and an API server
  prunes a field the installed CRD does not know with at most a warning.
  Against an NVCRE whose CRD predates `spec.fabric`, the field would vanish
  silently and NVCRE would render from platform and architecture. Three
  guards close that: the check runs only against an NVCRE at or above the
  pinned version that carries the field; the Certification is created with
  `fieldValidation: Strict`, so an unknown field is an error rather than a
  prune; and the check reads `spec.fabric` back from the created object
  and fails if it is absent or differs before trusting any result. The
  qualification key gains a fabric dimension, `(service, accelerator,
  fabric)`, since the current `(service, accelerator)` key cannot say
  "qualified on EFA, not on RoCE" for one accelerator; only
  `(eks, h100, efa)` exists today.
- **TrainJob checks.** `nccl-all-reduce-bw-net` and `-nvls` keep their
  `(service, accelerator)` template matrix. ADR-025 keeps them in place,
  and whether a recipe selects the TrainJob or the NVCRE backend is decided
  by a separate ADR-025 amendment on per-recipe backend selection, not
  here. Moving that matrix onto `FabricOf` is possible but not required by
  this ADR.
- **Transport assertion.** Two mechanisms exist today, and the fabric
  picks between them. On runtimes that log at `NCCL_DEBUG=INFO`,
  `verifyTransportFromLogs` matches the banner `NCCL INFO Using network
  <plugin>` with a `(\S+)` capture and rejects only `Socket`. The capture
  is too narrow for a per-fabric gate: the EKS fixture reads `Using
  network AWS Libfabric`, so the capture yields `AWS`. The assertion is
  reworked to parse the rest of the banner line and match it against an
  accept list per fabric: `efa` accepts `AWS Libfabric` and `Libfabric`;
  `ib` and `roce` accept `IB`. Each provider name is backed by a log
  fixture before it is added. Google's `gIB` is added only when a GKE A4
  NET runtime exists to confirm it against; none does today. `tcpxo` is
  not banner-checked at all: the shipped GKE H100 runtime sets
  `NCCL_DEBUG=WARN` on purpose (#1712), so no banner is logged, and TCPXO
  transport is proven by the worker-pod watch in
  `nccl_tcpxo_transport.go`. That watch is the `tcpxo` guard. A declared
  fabric whose guard names another transport fails. Limitation: NCCL
  prints `IB` for InfiniBand and RoCE alike, so an `ib` recipe running
  over RoCE, or the reverse, is not detectable from logs. The snapshot
  `linkType` constraint is the guard for that case.
- **Bandwidth floors.** Per-fabric floors become expressible, which is the
  shape #1256 asks for.
- **Evidence and attestation.** The fabric claim ships in a new predicate
  type, `recipe-evidence/v4`, following the v2 and v3 precedent
  (`pkg/evidence/attestation/types.go`). The attestation builder records
  `FabricOf` as a top-level predicate field, so unprofiled recipes such as
  EKS carry it too. v4 requires the field and records an empty value for a
  recipe that derives unset. For a v4 statement the evidence verifier
  binds it the way it binds the profile claims
  (`pkg/evidence/verifier/identity.go`): it recomputes `FabricOf` from the
  manifest-verified recipe and rejects a statement whose recorded fabric
  differs or is missing. Signed v4 evidence can therefore not carry a
  fabric that disagrees with the attested composition. v1 through v3
  statements carry no fabric claim and verify under their original rules,
  as v1 and v2 statements do today, so every committed pointer stays
  verifiable. The evidence path and the existing pointers do not change.
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
from `aicr.run/v1` to `aicr.run/v1beta2`, `ownedPaths` appears, clients
start receiving `v1beta2` artifacts for the family, and every shipped
evidence pointer for the family must be re-signed. ADR-015
also forbids declaring a value that is not yet qualified, so a declaration
cannot land ahead of its second value.

| Overlay family | Change | Existing recipes |
|---|---|---|
| `eks.yaml` and leaves | None. Derived `efa`. | Unchanged |
| `gke-cos.yaml` | None. Derived `roce` on A4X (GB200), `tcpxo` on A3 Mega, unset on A4 (B200) and A4X Max (GB300), which carry no networking component. | Unchanged |
| `aks.yaml` | None. Derived `ib`. | Unchanged |
| `oke-ol.yaml` and OKE leaves | None until an OKE GB300 leaf with a qualified RoCE target exists; then that leaf declares its own marked `gpuStack`, which shadows the inherited one (see "OKE declaration scope"). | Unchanged, including the accelerator-less OKE coordinates |
| `gb300-generic-ubuntu-training.yaml` | None until a Spectrum-X RoCE GB300 cluster is qualified; then a new `gpuStack` declaration, default `operator-managed` (`fabric: ib`) reproducing today's rendered composition, plus `operator-managed-roce`, and the gated per-fabric policy manifests on the `network-operator` ref (placement 1a). | Unchanged now. At conversion: overlay and result version bumps, the three signed pointers under `recipes/evidence/gb300-generic-ubuntu-training` re-signed in the same release, and an upgrade test from a pre-conversion bundle proving the live `nic-cluster-policy` is updated in place on every deployer. |
| `gb200-nvke-ubuntu-training.yaml`, `b300-mk8s-*` | None. Derived `ib`. | Unchanged |

**OKE declaration scope.** `oke-ol.yaml` carries the one `gpuStack`
declaration for every OKE composition: A100, GB200 (InfiniBand), and L40S
(RoCE) leaves all inherit the same `oci-managed` default, and so do the
accelerator-less OKE coordinates (`--service oke --os ol --intent
training`, and the same for inference), which the resolver builds without
any accelerator overlay. A fabric marker cannot be added to that
declaration, because one value would label two fabrics. Moving the
declaration down to the per-accelerator overlays was considered and
rejected: the accelerator-less coordinates would lose their declaration,
drop from `aicr.run/v1beta2` to `aicr.run/v1`, and lose `selectedProfile`,
which breaks the headline invariant for two shipped coordinates.

Instead this ADR amends the ADR-015 uniqueness rule with one narrow case,
**declaration shadowing**: when a composition carries declarations from
more than one overlay, the declaration from the most specific overlay in
the existing merge order replaces the inherited one whole. Values are not
merged across declarations, `default` is not inherited, and the result is
still exactly one declaration, so the resolution algorithm, the lock
template, and the evidence segment are unchanged. Two declarations at the
same depth remain an error, and external catalogs still extend rather
than redeclare. The shadowing declaration must satisfy the same totality,
distinguishing-constraint, and qualification rules as any other.

When the first OKE GB300 RoCE target is qualified, the new GB300 leaf
declares its own `gpuStack` with the same driver-mode values as
`oke-ol.yaml` plus the markers and `*-roce`, shadowing the inherited one.
`oke-ol.yaml` is not edited. Every other OKE composition, the A100, GB200,
L40S leaves and the accelerator-less coordinates alike, keeps resolving
from the `oke-ol.yaml` declaration byte-identically, digest included. No
OKE evidence pointer is committed today, so nothing is re-signed. The
change ships with a golden test over every OKE coordinate the resolver can
produce, including the accelerator-less ones, asserting that only the new
GB300 leaf differs.

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
- The ADR-015 resolution algorithm, lock template, component-set rule,
  and evidence coordinate shape are unchanged. The one-declaration rule is
  amended in one narrow way: a leaf declaration may shadow an inherited
  one whole, so a composition still resolves exactly one declaration.
- `gpuStack` values on overlays with both axes become tuples. The value
  list on OKE can reach four entries. The marker keeps the tuple explicit.
  Splitting `fabric` into a criteria dimension later is mechanical because
  the value set and the recorded field already exist.
- On the overlays that gain a second fabric, the `network-operator` ref
  carries one gated policy manifest per fabric, and the gate key is locked
  for every value, the standing ADR-015 cost. The live policy stays in the
  `network-operator-post` release, so no deployer migrates ownership. The
  two readiness scans that derive fabric resources from the manifest learn
  to evaluate the gate.
- A family's first conversion to two fabrics is a full ADR-015 family
  conversion, with the overlay and result version bumps and evidence
  re-signing that implies, and an upgrade test from a pre-conversion
  bundle. It is scheduled per family, not by this ADR.
- Fabric-marked recipes are readable only by binaries from the release
  that introduced the marker onward; older binaries reject them at decode.
  The schema change ships at least one release ahead of the first marker.
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
2. Catalog schema: the `fabric` marker and its load-time validation, plus
   the resolution-time recheck on the filtered composition. Wire model:
   extend `ProfileValue`, `SelectedProfile`, `applyEffectiveProfile`
   (`pkg/recipe`), and `RecipeResult.schema.json` so `selectedProfile.fabric`
   survives resolution, the digest, and `/v1` serialization. A
   fabric-marked profile round-trip and digest test covers that path, since
   the step 1 golden test exercises only unmarked leaves. OpenAPI update,
   api-diff and openapi-diff gates. Ships at least one release before
   step 5 (compatibility rule).
3. Measurement: promote `linkType` to a scalar with the pseudo-version
   bump; add the EFA and mrdma readings; fail-closed tests. A prerequisite
   for step 5, since an absent reading fails generation.
4. Validators, evidence, and NVCRE: `ValidationInput.Fabric`;
   `nccl-cre-all-reduce-bw` passes `spec.fabric` and reports not applicable
   when it is empty, gated on the pinned NVCRE version that carries the
   field, created with strict field validation, and read back before the
   result is trusted; `creQualifiedEntries` keyed on
   `(service, accelerator, fabric)` with an entry per fabric the check is
   measured on; the banner accept list on INFO runtimes and the worker-pod
   watch as the `tcpxo` guard; per-fabric floors; the `recipe-evidence/v4`
   predicate and the verifier's `FabricOf` binding. Lands with
   cluster-readiness-engine#576.
5. Per family, in the release where its second fabric is qualified: the
   `gpuStack` declaration or markers, the second value, the gated
   per-fabric policy manifests, the gate-aware `recipeDeclaresRDMAFabric`
   and `recipeAttachesNicClusterPolicy`, re-signed evidence, and an
   upgrade test from a pre-conversion bundle on each deployer, together.
   GB300 generic first; OKE, with a shadowing declaration on the new GB300
   leaf, when a RoCE OKE GB300 target exists. The shadowing rule itself,
   with its same-depth conflict test and the accelerator-less OKE golden
   coordinates, lands in step 2.
6. Docs: `docs/contributor/recipe.md` profile vocabulary, component catalog
   notes, CLI and API references.

## Open questions

- Whether `K8s.node.allocatable.*` should become a general reading rather
  than an EFA-specific one.
- Whether the TrainJob variants should move their template selection onto
  `FabricOf`, or be left alone while ADR-025 keeps them in place.
