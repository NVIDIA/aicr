# ADR-030: Size Controller Memory from Node Count

## Status

**Proposed** — 2026-11-04. Answers
[#3642](https://github.com/NVIDIA/aicr/issues/3642), under the scalability
epic [#2427](https://github.com/NVIDIA/aicr/issues/2427). Resolves the
resource-sizing part of the scale contract asked for in
[#2429](https://github.com/NVIDIA/aicr/issues/2429), and supersedes the
proposed direction in [#3214](https://github.com/NVIDIA/aicr/issues/3214).
Implemented by [#3643](https://github.com/NVIDIA/aicr/issues/3643)
(generation time) and [#3644](https://github.com/NVIDIA/aicr/issues/3644)
(deploy time). Reverses the default limits raised by
[#3212](https://github.com/NVIDIA/aicr/pull/3212) and
[#3236](https://github.com/NVIDIA/aicr/pull/3236).

## Decision Summary

A controller whose memory grows with cluster size gets its request and
limit from a measured tier table, selected by a node count the user states.
The count is stated either when the bundle is generated (`aicr bundle
--nodes`) or when an `argocd-helm` bundle is installed (`sizing.nodeCount`).
Each component declares its table in `recipes/components/<name>/sizing.yaml`,
referenced from the registry. A cluster that states no count gets the
small-cluster defaults AICR shipped in v1.0.0. A large cluster deployed
without a count is expected to run out of memory, and the documentation says
so. A tier can raise a value but never lower it, and an explicit user
override always wins.

## Problem

Five controllers in AICR's stack hold cluster-wide informer caches, so their
working set grows with node count:

- gpu-operator
- network-operator
- maintenance-operator
- the nodewright manager
- the GB300 NIC configuration daemon

The NIC configuration daemon is the worst case: it runs on every node, and
each copy caches every NicDevice in the cluster
([#3149](https://github.com/NVIDIA/aicr/issues/3149)).

v1.0.0 shipped one static request and limit per controller, sized for small
clusters. Two GB300 bring-ups, at about 1700 and 2500 GPU nodes, OOM-killed
all five ([#3148](https://github.com/NVIDIA/aicr/issues/3148)). One of them
crash-looped gpu-operator 44 times. About 230 rebooted nodes then came back
with no GPU operands until it recovered.

v1.1.0 answered with two changes to the static values:

- #3212 raised four controller limits to 2500-node values (8Gi to 12Gi).
- #3236 raised the NIC configuration daemon to 2Gi.

The requests were left unchanged, which creates two new problems:

- **Requests misstate usage on large clusters.** At 2500 nodes the four
  controllers request about 4.5Gi in total and use about 25Gi at peak. The
  scheduler cannot see the difference. When a system node runs short, these
  pods are the first evicted or OOM-killed, because they run furthest above
  their requests. Large clusters still hand-set requests through
  `valueOverrideKeys`.
- **One value cannot fit every size.** A 2500-node limit on a 20-node cluster
  is a promise the node may not be able to keep. The NIC daemon's limit is
  multiplied by every node in the cluster. Above about 3000 nodes, even the
  raised values need hand-tuning.

AICR has no way to derive a value from cluster size. `aicr bundle --nodes N`
copies N verbatim into the paths listed in `nodeScheduling.nodeCountPaths`,
and only nodewright's `estimatedNodeCount` is listed there. The node count is
also often unknown when the bundle is generated: the clusters in #3148 deploy
a published OCI bundle through `argocd-helm` and size it at install time,
from the root Application's values.

## Goals

- Size controller memory requests and limits from a node count the user
  states, at generation time and at deploy time.
- Back every sized value with a measurement, recorded next to the value.
- Keep the outputs reproducible: the same inputs give byte-identical
  bundles, and the applied tier is recorded in the bundle.
- Keep explicit user overrides authoritative.

## Non-Goals

- **Deriving a count AICR was not given.** The epic requires that node count
  alone never silently specializes a configuration. A snapshot-derived
  `criteria.nodes` therefore sizes nothing.
- **CPU, replica counts, and client QPS.** These belong to the
  requalification work in [#2430](https://github.com/NVIDIA/aicr/issues/2430).
- **Inputs other than node count.** Pod count, NicDevice count and node size
  are out of scope (see Consequences).
- **Components outside this ADR's first set.** NFD master and NVSentinel are
  measured but not yet in scope. Adding them is a data change under this ADR
  and needs no new decision.
- **Fixing the growth itself.** The NIC daemon's unscoped cache is
  [#3149](https://github.com/NVIDIA/aicr/issues/3149). gpu-operator's
  per-node cost is a question raised with its team, in
  [#3214](https://github.com/NVIDIA/aicr/issues/3214).

## Context

**Measurements.** Fleet readings cover 1 to 2500 GPU nodes. They are
summarized in #3148 and #3214, with 24h cadvisor peaks at 244, 357, 447 and
2500 nodes, and instantaneous readings elsewhere.

Instantaneous readings understate peaks. Over 24h, peak-to-mean is:

- 1.1 to 1.3x for gpu-operator and nodewright;
- 1.3 to 1.7x for network-operator and NFD master.

The NIC daemon's peak is its startup list of the cluster-wide NicDevice
cache, 15 to 30% above its running working set.

From those readings, the v1.0.0 defaults break at roughly these sizes:

| Controller | v1.0.0 request / limit | Breaks near | Evidence |
|---|---|---|---|
| gpu-operator | 300Mi / 700Mi | 500 nodes | 635Mi 24h peak at 447 (91%), 715Mi 7-day peak at 576 |
| network-operator (GB300) | 2Gi / 2Gi | 550 to 600 | 1594Mi 24h peak at 447 (78%), 75 restarts at 576 |
| maintenance-operator (GB300) | 256Mi / 1Gi | 500 to 700 | about 2.5Gi per 1000 nodes |
| nodewright manager | 2000Mi / 4000Mi | 600 to 900 | 1868Mi 24h peak at 447 with 18k pods |
| NIC configuration daemon (GB300) | 128Mi / 512Mi | 700 | per-pod max 400Mi at 89, 903Mi at 1112 |

**Drivers.** Most of this is linear in node count: gpu-operator at about
1.2 to 1.4Mi per node, and network-operator at about 3.2 to 3.6Mi per node.

Nodewright is the exception: its working set follows pod count and per-node
Skyhook CRs. At 244 and 357 nodes, with 11k and 14k pods, it peaks at the
same 1Gi. A 118-node cluster with 18k pods OOM-killed it at 2Gi.

maintenance-operator grew about 1Gi in a week at a constant 2500 nodes, so
something besides node count drives it too
([#3267](https://github.com/NVIDIA/aicr/issues/3267) adds the scrape needed
to find out).

**Existing mechanisms this builds on:**

- `aicr bundle --nodes`, with its config-file, REST (`nodes`) and SDK
  (`BundleOptions.Nodes`) equivalents, already exists. It is documented as an
  estimate for bundle-time injections.
- [ADR-021](021-component-upgrade-safety.md) set the pattern of per-component
  data in a file the registry references and validated fail-closed at load.
- [ADR-015](015-recipe-configuration-profiles.md) locks profile-owned paths
  against every mutation source, node-count injection included. On
  `argocd-helm`, it adds a template-time guard on install-time values.
- The `argocd-helm` child Application template already merges a component's
  static values with the root chart's values for that component, at render
  time.
- The readiness-gate chart is the precedent for install-time template logic
  in a generated chart.

## Options

**A. Static values sized for the largest cluster (status quo after
v1.1.0).** No mechanism is needed, but every cluster pays a 2500-node limit,
requests still misstate usage, and anything above 2500 nodes needs
hand-tuning.

**B. A scale criteria axis or overlay per size band.** This multiplies the
recipe matrix by the number of bands. It also selects a configuration by
size, which is what #2427 rules out unless the user chooses it, and it cannot
be applied at install time.

**C. Documented `--set` and install-time overrides only.** Users compute
their own values, which is how #3148's clusters run today. Each new large
cluster then rediscovers the numbers, usually through a crash loop.

**D. Declared, measured tier tables, selected by a stated count at
generation or deploy time (chosen).**

## Decision

Adopt option D, with these rules.

### 1. Defaults are the v1.0.0 values

| Controller | Values path | Request / limit |
|---|---|---|
| gpu-operator | `operator.resources` | 300Mi / 700Mi |
| network-operator (GB300) | `operator.resources` | 2Gi / 2Gi |
| maintenance-operator (GB300) | `maintenance-operator-chart.operator.resources` | 256Mi / 1Gi |
| nodewright manager | `controllerManager.manager.resources` | 2000Mi / 4000Mi |
| NIC configuration daemon (GB300) | `nicConfigurationOperator.configurationDaemon.resources` | 128Mi / 512Mi |

A bundle generated without a count, and an `argocd-helm` bundle installed
with `sizing.nodeCount: 0`, carries these values.

A cluster beyond the first tier that states no count is under-provisioned by
choice. Its controllers are expected to be OOM-killed. Tier selection is the
supported fix, and the documentation states this plainly.

#3236's change that sources the NIC daemon's resources from the
network-operator values stays; only its numbers revert.

### 2. The count must be explicit

Sizing applies only to a count stated as one of:

- `aicr bundle --nodes N`, or its config-file, REST and SDK equivalents;
- `sizing.nodeCount` in an `argocd-helm` bundle's root values.

`criteria.nodes` and snapshot-derived counts never select a tier. The count
means GPU nodes, the unit every measurement in this ADR is taken in.

### 3. Tiers are declared per component and measured

`recipes/components/<name>/sizing.yaml`, referenced from the registry
entry's `sizing.file`:

```yaml
kind: ComponentSizing
basis: gpuNodes
resources:
  - path: operator.resources
    tiers:
      - maxNodes: 300
      - maxNodes: 600
        requests: {memory: 512Mi}
        limits: {memory: 2Gi}
        source: "#3214: 635Mi 24h peak at 447; 715Mi 7-day peak at 576"
      - maxNodes: 1500
        requests: {memory: 2Gi}
        limits: {memory: 4Gi}
        source: "#3214: 2052Mi 7-day peak at 1112"
      - maxNodes: 3000
        requests: {memory: 4Gi}
        limits: {memory: 8Gi}
        source: "#3148: 3447Mi 24h peak at 2500"
  - path: maintenance-operator-chart.operator.resources
    requiresPath: maintenance-operator-chart
    tiers: [...]
```

- A tier applies to counts up to its `maxNodes`, and above the previous
  tier's. A tier with no `requests` or `limits` leaves the values unchanged.
- `requiresPath` makes an entry apply only when the resolved values contain
  that key. For example, the maintenance-operator and NIC daemon entries
  apply only where the GB300 values file defines them.
- Every tier that changes a value carries a `source` naming its measurement.
  A tier is never interpolated from neighbouring measurements. A component
  with no measurement inside a tier is sized from its nearest measurement
  above the tier, which over-sizes it, never from one below.
- Requests track steady-state usage near the top of the tier. Limits leave
  at least 1.5x headroom over the highest 24h peak measured in the tier.
  Where only instantaneous readings exist, they are first scaled by the
  peak-to-mean ratios in Context.
- The loader fails closed with `ErrCodeInvalidRequest` on any of these:
  - a malformed path;
  - `maxNodes` values that do not strictly ascend;
  - a quantity that does not parse as a Kubernetes quantity;
  - a limit below its request;
  - a changing tier without a `source`.
- Above the top tier, the top tier applies and AICR warns that the size is
  unqualified. Raising the ceiling needs a new measured tier.

### 4. Initial tiers

Values are GPU nodes and memory request / limit. Each `sizing.yaml` pins
these numbers with their sources.

| Tier | gpu-operator | network-operator | maintenance-operator | nodewright | NIC daemon (per pod) |
|---|---|---|---|---|---|
| ≤ 300 | 300Mi / 700Mi | 2Gi / 2Gi | 256Mi / 1Gi | 2000Mi / 4000Mi | 128Mi / 512Mi |
| 301 to 600 | 512Mi / 2Gi | 2Gi / 4Gi | 1Gi / 4Gi | 2000Mi / 4000Mi | 256Mi / 2Gi |
| 601 to 1500 | 2Gi / 4Gi | 4Gi / 10Gi | 3Gi / 8Gi | 4Gi / 8Gi | 512Mi / 2Gi |
| 1501 to 3000 | 4Gi / 8Gi | 6Gi / 12Gi | 8Gi / 12Gi | 8Gi / 12Gi | 512Mi / 2Gi |

The data each tier rests on:

| Tier | gpu-operator | network-operator | maintenance-operator | nodewright | NIC daemon (per pod) |
|---|---|---|---|---|---|
| ≤ 300 | 24h peak 384Mi at 244 | 24h peak 728Mi at 244 | 290Mi at 89; 130 to 156Mi at 141 | 24h peak 1024Mi at 244 (11k pods) | max 400Mi at 89, flat from 24 nodes |
| 301 to 600 | 24h peaks 471Mi at 357, 635Mi at 447; 7-day peak 715Mi at 576 | 24h peaks 908Mi at 357, 1594Mi at 447; 75 restarts at 2Gi at 576 | none in tier; sized from 2345Mi at 1112 | 24h peaks 1006Mi at 357, 1868Mi at 447 (18k pods) | none in tier; sized from p99 821Mi, max 903Mi at 1112 |
| 601 to 1500 | 7-day peak 2052Mi at 1112; 1506Mi at about 1400 | 2720Mi at 1112 (12h-old pod); 4490Mi at about 1400 | 2345Mi at 1112 (12h-old pod); 2894Mi at about 1400 | 3706Mi at 1112; 3190Mi at about 1400 | max 903Mi at 1112; max 1023Mi at about 1400 |
| 1501 to 3000 | 24h peak 3447Mi at 2500 | 24h peak 8107Mi at 2500 | 24h peak 5186Mi, 6186Mi a week later, at 2500 | 24h peak 9091Mi at 2500 | 24h q99 1316Mi, max 1551Mi at 2500 |

The top tier matches the overrides that have run both large clusters since
#3148. Two of its limits are below the 1.5x headroom rule and are recorded
here instead of hidden:

- **nodewright:** 12Gi is 1.3x its 9091Mi peak.
- **NIC daemon:** 2Gi runs out near 6500 NicDevices, about 3000 to 3500
  GB300 nodes.

Both are reasons the top tier stops at 3000.

### 5. Never lower; explicit overrides win

Applying a tier sets each memory request and limit to the larger of the
current value and the tier's value, compared as Kubernetes quantities. A
current value that does not parse fails the bundle.

A tier therefore never undercuts a larger value set by a recipe overlay or
mixin. Explicit user overrides are applied after sizing and replace its
result:

- `--set`, `--set-json` and `--set-file` at generation time;
- the component's root-chart values at install time.

Sizing never writes a path in an ADR-015 profile lock set.

### 6. Generation time (`--nodes`)

In the bundler, the sizing pass runs once per component after base, overlay
and inline values resolve, and before user overrides.

For every deployer except `argocd-helm`, the result is baked into the
component's values. That includes bundle-rendered manifests such as the
NIC daemon's NicClusterPolicy.

The bundle records the count and the tier applied to each component in
`bundle-info.yaml`, so evidence for a bundle names its sizing.

### 7. Deploy time (`argocd-helm`)

Remote-chart components that have a sizing table are not sized at
generation time. Instead the bundle carries the table, and Helm selects a
row at render time:

- The root `values.yaml` gains `sizing: {nodeCount: N}`. N is the bundle's
  `--nodes`, or 0.
- `values.schema.json` declares `sizing.nodeCount` as an integer of at least
  0.
- For each sized component, the bundler writes `static/<comp>-sizing.yaml`.
  It holds every tier's effective values, already computed with the
  never-lower rule against the component's unsized static values. The
  template only selects a row and never compares quantities.
- The child Application template merges `$static`, then the selected row,
  then the root chart's values for the component, so user values still win.
  A count of 0 selects nothing.

One code path decides each component's sizing. A generation-time `--nodes`
on an `argocd-helm` bundle only sets the default `sizing.nodeCount`.

Two cases are handled specially:

- **Bundle-rendered manifests.** They cannot see install-time values, so
  they keep generation-time sizing. Today that is only the NIC daemon. The
  bundle README says that `sizing.nodeCount` does not reach them and that
  `--nodes` does.
- **`--dynamic`.** A `--dynamic` export of a sized path is rejected at
  bundle time. Its pre-filled root value would otherwise always mask the
  selected tier.

Other deployers have no install-time sizing yet. They can add it later by
following this section's rules.

## Consequences

**Positive:**

- Small clusters go back to small reservations.
- Large clusters get requests that match usage, so the scheduler places
  controllers where they fit.
- A published `argocd-helm` bundle is sized with one value instead of a
  hand-copied block per controller.
- Every sized number points to its measurement.
- Bundles are byte-stable for the same inputs and record their tier.

**Negative:**

- **Larger default exposure.** A cluster above 300 GPU nodes that does not
  state a count regresses to the pre-v1.1.0 failure mode, by design. Release
  notes for the version that lands this must call it out.
- **Node count is an imperfect proxy.**
  - Nodewright tracks pod count, so its tiers are sized for about 40 pods
    per node. A denser cluster can still exhaust a tier.
  - maintenance-operator grows at a constant node count.
  - The NIC daemon tracks NicDevice count.

  Each gap is documented in that component's `sizing.yaml`.
- **The 601 to 1500 tier has the thinnest data.** There are no 24h peaks
  between 447 and 2500 nodes. That tier rests on a 7-day peak at 1112 and
  instantaneous readings at 1112 and about 1400, scaled per Context. In the
  301 to 600 tier, maintenance-operator and the NIC daemon have no reading
  of their own and are sized from 1112. Requalifying both tiers with 24h
  peaks is a follow-up.
- **The count is the user's responsibility.** A stale count after a
  scale-up under-sizes the cluster. Rerunning bundle generation, or changing
  `sizing.nodeCount`, re-sizes it.

## Adoption plan

1. This ADR.
2. [#3643](https://github.com/NVIDIA/aicr/issues/3643), generation time:
   - the `sizing.yaml` schema and loader;
   - the data files and restored defaults;
   - the bundler sizing pass and `bundle-info.yaml` record;
   - the docs.
3. [#3644](https://github.com/NVIDIA/aicr/issues/3644), deploy time: the
   `argocd-helm` root value, the per-tier static files, the template
   selection, and the `--dynamic` guard.

#3643 and #3644 should ship in the same release. Restoring the defaults
without the `argocd-helm` path would leave the clusters in #3148 with only
hand overrides again.

## Open questions

- **Requalifying the 301 to 600 and 601 to 1500 tiers** once cadvisor peaks are scrapeable
  for all five controllers
  ([#3265](https://github.com/NVIDIA/aicr/issues/3265) to
  [#3269](https://github.com/NVIDIA/aicr/issues/3269)).
- **A second input for nodewright** (pod count), if dense clusters keep
  outgrowing its tiers.
- **Install-time sizing for the other deployers.** For `helm` and
  `helmfile`, `install.sh` could read an `AICR_NODE_COUNT` environment
  variable and layer per-tier values files. `flux`, `fleet` and `terraform`
  are not yet designed.
- **Passing a count from the AICR Operator**
  ([ADR-027](027-aicr-operator.md)) through to the bundle step it runs.
