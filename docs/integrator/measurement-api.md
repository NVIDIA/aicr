# Measurement Schema

The `Measurement` type in `github.com/NVIDIA/aicr/pkg/measurement` is the
on-wire shape used throughout aicr's Snapshot → Recipe → Validate → Bundle
pipeline. Snapshots serialize a `[]*Measurement` to YAML/JSON; recipes and
validators consume the same shape. This page is the schema contract — any
external producer (cross-repo Go library, CI tool, custom collector) emitting
Measurements should follow it exactly.

The Go types are documented in `pkg/measurement/types.go`. This page documents
the conventions on top of the types (which Type appears how often, which
Subtype names mean what, which fields live in `Context` vs `Data`).

## Top-level structure

```yaml
measurements:
  - type: K8s
    subtypes: [...]
  - type: GPU
    subtypes: [...]
  - type: OS
    subtypes: [...]
  - type: SystemD
    subtypes: [...]
  - type: NodeTopology
    subtypes: [...]
  - type: NetworkTopology     # 0 or 1 today; future: 0..N (one per group)
    subtypes: [...]
```

### Type cardinality

| Type | Cardinality today | Notes |
|------|-------------------|-------|
| `K8s` | 0 or 1 | Cluster-scoped Kubernetes state. |
| `GPU` | 0 or 1 | GPU inventory + driver state. |
| `OS` | 0 or 1 | Host OS metadata. |
| `SystemD` | 0 or 1 | systemd unit states. |
| `NodeTopology` | 0 or 1 | Cluster-wide node taints + labels (aggregate). |
| `NetworkTopology` | 0 or 1 | Per-hardware-group network topology. **Planned multi-instance**: future versions will emit one per discovered group. |

Find-first-by-Type consumers (constraint extractor, recipe validation, diff
indexing) are sound today because every Type appears at most once. When
`NetworkTopology` becomes multi-instance, the relevant consumer rewrites are
tracked alongside the multi-group enablement work.

## Subtype

A `Subtype` has a name plus up to three payload fields:

| Field | Type | Purpose |
|-------|------|---------|
| `data` | `map[string]Reading` (scalar values) | Numeric / boolean / string measurements addressable by key. |
| `context` | `map[string]string` | Descriptive metadata (provenance, identity, free-form labels). |
| `items` | `[]ItemEntry` | Ordered list of structured records. Used when the payload is naturally an array. |

A subtype must carry at least one entry in `data` or `items`. `data` and
`items` are independent and may both be populated.

### ItemEntry

```yaml
- context:
    pciAddress: "0000:03:00.0"
    deviceID: "1023"
  data:
    rail: 0
    numaNode: 0
    traffic: east-west
```

Each `ItemEntry` mirrors a Subtype's scalar contract: `data` holds `Reading`
scalars; `context` holds string-typed descriptive fields. `ItemEntry` does
NOT support nested `items` — the scalar Reading model is preserved.

## K8s slinky-slurm shape

When a `K8s` measurement is emitted, it includes a `slinky-slurm` subtype that
summarizes whether a Slinky `Controller` custom resource was observed. The
summary detects declared installation presence, not operator, webhook, pod, or
Slurm control-plane health. When exactly one Controller is conclusively
observed, the collector also projects a small, secret-safe resource topology.

```yaml
type: K8s
subtypes:
  - subtype: slinky-slurm
    data:
      api-available: true
      detected: true
      collection-state: detected
      api-version: v1beta1
      controller-count: 1
      nodeset-count: 1
      loginset-count: 1
      restapi-count: 1
      accounting-count: 1
    items:
      - context:
          id: controller/slurm/slinky-slurm
          kind: Controller
          namespace: slurm
          name: slinky-slurm
          api-version: v1beta1
        data:
          cluster-name: slinky
          external: false
          accounting-ref-present: true
      - context:
          id: nodeset/slurm/slinky-slurm-worker-slinky
          kind: NodeSet
          namespace: slurm
          name: slinky-slurm-worker-slinky
          api-version: v1beta1
          controller-id: controller/slurm/slinky-slurm
        data:
          partition-enabled: true
      - context:
          id: loginset/slurm/slinky-slurm-login-slinky
          kind: LoginSet
          namespace: slurm
          name: slinky-slurm-login-slinky
          api-version: v1beta1
          controller-id: controller/slurm/slinky-slurm
      - context:
          id: restapi/slurm/slinky-slurm
          kind: RestApi
          namespace: slurm
          name: slinky-slurm
          api-version: v1beta1
          controller-id: controller/slurm/slinky-slurm
      - context:
          id: accounting/slurm/slinky-slurm-accounting
          kind: Accounting
          namespace: slurm
          name: slinky-slurm-accounting
          api-version: v1beta1
          controller-id: controller/slurm/slinky-slurm
        data:
          external: false
```

The summary fields are:

- `api-available` (`bool`) — whether discovery conclusively found the exact
  `slinky.slurm.net` `controllers` API. Omitted when discovery could not
  determine availability.
- `detected` (`bool`) — whether at least one `Controller` was observed.
  Emitted only after presence or absence was conclusively determined.
- `collection-state` (`string`) — always one of `absent`, `detected`,
  `unsupported-multicluster`, or `unknown`.
- `api-version` (`string`) — the actual served API version selected by
  discovery. Emitted after the Controller API is found.
- `controller-count` (`int`) — emitted only after a successful cluster-wide
  List.
- `nodeset-count`, `loginset-count`, `restapi-count`, and `accounting-count`
  (`int`) — counts derived from projected items. They are emitted only when
  every child API and reference needed for the projection was collected
  conclusively; otherwise all child counts are omitted.

State interpretation is fail-closed:

- `absent` means successful discovery found no Controller API, or a successful
  List returned zero Controllers.
- `detected` means exactly one Controller was listed.
- `unsupported-multicluster` means more than one Controller was listed. The
  snapshot retains the count instead of silently selecting one.
- `unknown` means authorization, timeout, network, malformed-discovery, or
  other ambiguous failure prevented a conclusive result. Failed Lists never
  masquerade as a zero count.

Every item uses context keys `id`, `kind`, `namespace`, `name`, and
`api-version`; associated children also carry `controller-id`. The canonical
ID is `<kind-lower>/<namespace>/<name>`. Slinky v1.2.0 `v1beta1` references are
same-namespace `LocalObjectReference`s; compatible served versions are handled
dynamically. If a child API or reference is missing or inconclusive, the
collector retains only the Controller item and omits child items and counts.

The data allowlist is intentionally narrow: Controller projects
`cluster-name`, `external`, and `accounting-ref-present`; NodeSet projects
`partition-enabled`; Accounting projects `external`; LoginSet and RestApi
project identity and association only. The collector never serializes
`extraConf`, replicas, images, pod templates, status, storage connection
fields, Secret/ConfigMap references or contents, JWT/Slurm keys, SSH keys, or
arbitrary container and volume configuration.

Summary fields use ordinary non-item constraint paths, for example
`K8s.slinky-slurm.detected` and
`K8s.slinky-slurm.collection-state`.

Item constraints use the canonical ID, for example:

```text
K8s.slinky-slurm[id=nodeset/slurm/slinky-slurm-worker-slinky].partition-enabled
```

## K8s mariadb-operator shape

`K8s.mariadb-operator` records conflict evidence for the official MariaDB
Operator API only: API group `k8s.mariadb.com`, resource `mariadbs`, Kind
`MariaDB`. It does not inspect Deployments, pods, Services, Secrets,
StatefulSets, or external databases such as RDS.

```yaml
- subtype: mariadb-operator
  data:
    collection-state: crs-detected
    api-available: true
    api-version: v1alpha1
```

The fields are:

- `collection-state` — one of `absent`, `api-detected`, `crs-detected`, or
  `unknown`.
- `api-available` — whether discovery conclusively found the exact
  `k8s.mariadb.com` `mariadbs` API. Omitted when discovery could not
  determine availability.
- `api-version` — the dynamically discovered served version, emitted when the
  exact API is available.

`absent` means the official API group was conclusively not found.
`api-detected` has two disambiguated cases: the official group was discovered
but the exact `k8s.mariadb.com/mariadbs` resource was not served
(`api-available: false`), or that exact resource was served and a successful
List returned zero MariaDB CRs (`api-available: true`).
`crs-detected` means a successful List returned one or more MariaDB CRs;
multiple CRs are valid and use the same state. `unknown` means discovery or
List was inconclusive. No state proves that an operator is running or a
database is healthy, reachable, or customer-provided.

This subtype never selects `accounting.databaseSource`. A future
`aicr-provided` installation may require
`K8s.mariadb-operator.collection-state == absent` as a fresh fail-closed
preflight; `absent` is permission to proceed, not installation intent.

Both `slinky-slurm` and `mariadb-operator` remain in raw snapshots and
`--full` evidence. The default minimal evidence policy drops both subtypes,
including all Slinky items. Constraint evaluation consumes the raw snapshot
before evidence packaging.

## NetworkTopology shape

`TypeNetworkTopology` describes one hardware group's network layout (PFs,
rails, RDMA capabilities, kernel modules, identity). When emitted, the
Measurement MUST follow this layout:

```yaml
type: NetworkTopology
subtypes:
  - subtype: identity
    context:
      identifier:   <stable group identifier, lowercase, RFC-1123>
      machineType:  <e.g. GB300-NVL>
      gpuType:      <e.g. NVIDIA-GB300>
      linkType:     <Ethernet | InfiniBand | "">    # empty if unknown
      nodeSelector: <label=value selector that targets the group's nodes>
    data:
      pf-count:   <int>
      rail-count: <int>
  - subtype: capabilities
    data:
      sriov: <bool>
      rdma:  <bool>
      ib:    <bool>
  - subtype: pfs
    items:
      - context:
          pciAddress:       <e.g. 0000:03:00.0>
          deviceID:         <hex PCI device ID, e.g. 1023>
          psid:             <PSID string>
          partNumber:       <NVIDIA SKU / part number>
          rdmaDevice:       <e.g. mlx5_0>
          networkInterface: <e.g. enp3s0f0np0>
          model:            <human-readable NIC model from VPD, when set>
          connectedGPU:     <GPU identifier from preset topology, e.g. GPU0>
          gpuProximity:     <PCIe-topology class to connectedGPU, e.g. PIX>
        data:
          rail:     <int>
          numaNode: <int>
          traffic:  <east-west | north-south>
      - context: {...}
        data:    {...}
  - subtype: kernel-modules
    data:
      storage.0:    <module name>
      storage.1:    <module name>
      thirdParty.0: <module name>
      thirdParty.1: <module name>
```

### Subtypes

- **`identity`** — group identity and high-level facts. Strings (machineType,
  gpuType, linkType, identifier, nodeSelector) live in `context`. Numeric
  facts (pf-count, rail-count) live in `data`.
- **`capabilities`** — boolean cluster capabilities (sriov, rdma, ib) as
  scalar `Reading` values in `data`.
- **`pfs`** — per-PF records as `items`. Per-PF descriptive identifiers
  (PCI address, device ID, PSID, part number, RDMA device name, netdev
  name, VPD model string, connectedGPU + gpuProximity from preset
  topology) live in `context`; per-PF scalar facts (rail index, NUMA
  node, traffic class) live in `data`. Optional fields (`model`,
  `connectedGPU`, `gpuProximity`) are omitted when unset by l8k.
- **`kernel-modules`** — flat ordered lists of storage and third-party RDMA
  modules. Keys are dotted with a numeric suffix (`storage.0`, `storage.1`,
  `thirdParty.0`, ...) to preserve order and stay within the scalar
  `Reading` model. (This is a deliberate exception to the array-via-items
  pattern: the lists are short, lookup is rare, and the dotted-key form
  is cheap.)

### Field-placement convention

- `context` — values that *describe* or *identify* a record: textual,
  cardinality-low, used for grouping or display. Not constrained to be
  scalar `Reading`s.
- `data` — values that are *measured* or *counted*: int / float / bool /
  short string, addressable by key, comparable in validator constraints.

## Constraint paths

The constraints package addresses a single value within a Measurement using:

```text
{Type}.{Subtype}.{Key}                                # legacy form, looks in Subtype.Data
{Type}.{Subtype}[<selector>].{Key}                    # item form, looks in ItemEntry
```

Selector forms:

| Form | Example | Meaning |
|------|---------|---------|
| Index | `NetworkTopology.pfs[0].rail` | Items entry at index 0. |
| Predicate | `NetworkTopology.pfs[rail=3].pciAddress` | The unique Items entry whose `data["rail"].String() == "3"` (or `context["rail"] == "3"` if not in data). |

Predicate behavior — deterministic single-match resolution:

- LHS is looked up in `ItemEntry.Data` first (stringified via
  `Reading.String()`); falls back to `ItemEntry.Context` if not found in
  Data.
- Exactly one matching entry is required.
- Zero matches returns `ErrCodeNotFound`.
- Two or more matches returns `ErrCodeConflict`. Predicates that can match
  more than one entry are a recipe authoring error; pick a more specific
  field to disambiguate.

Key resolution inside the chosen `ItemEntry`:

- `Data` is consulted first (returns `Reading.String()`).
- `Context` is consulted next (returns the string directly).
- Missing key returns `ErrCodeNotFound`.

## Stability contract

`pkg/measurement` is part of aicr's public API surface (see
[public-api.md](public-api.md)). The Go types AND the schema conventions
documented above are part of the contract. Field-level changes (renames,
type changes, semantic shifts in which fields go in `data` vs `context`)
are breaking and require a pseudo-version bump that downstream consumers
(`k8s-launch-kit`, external CI tools) pin against.

## See also

- `pkg/measurement/types.go` — the Go types
- [`docs/integrator/public-api.md`](public-api.md) — package stability tiers
- [`docs/integrator/recipe-development.md`](recipe-development.md) — how recipes consume Measurement values via constraint paths
