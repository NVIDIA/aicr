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

Controllers whose memory grows with cluster size get their request and limit
from a measured tier table, selected by a node count the user states: `aicr
bundle --nodes` at generation time, or `sizing.nodeCount` when an
`argocd-helm` bundle is installed. Each component declares its table in
`recipes/components/<name>/sizing.yaml`. With no count, a cluster gets the
v1.0.0 defaults, and a large cluster deployed that way is expected to run
out of memory. Applying a tier only raises values, and an explicit user
override wins.

## Problem

Five controllers in AICR's stack hold cluster-wide informer caches, so their
working set grows with node count: gpu-operator, network-operator,
maintenance-operator, the nodewright manager, and the GB300 NIC
configuration daemon. The NIC daemon is the worst case: it runs on every
node, and each copy caches every NicDevice in the cluster
([#3149](https://github.com/NVIDIA/aicr/issues/3149)).

v1.0.0 shipped one static request and limit per controller, sized for small
clusters. Two GB300 bring-ups, at about 1700 and 2500 GPU nodes, OOM-killed
all five ([#3148](https://github.com/NVIDIA/aicr/issues/3148)). On one,
gpu-operator crash-looped 44 times, and about 230 rebooted nodes came back
with no GPU operands until it recovered.

v1.1.0 raised the static limits to 2500-node values: #3212 for four
controllers (8Gi to 12Gi), #3236 for the NIC daemon (2Gi). Requests stayed
at small-cluster values, which leaves two problems:

- **Requests misstate usage on large clusters.** At 2500 nodes the four
  controllers request about 4.5Gi in total and use about 25Gi at peak. When
  a system node runs short, the kubelet evicts first the pods furthest above
  their requests, which are these. Large clusters still hand-set requests
  through `valueOverrideKeys`.
- **One value cannot fit every size.** A 2500-node limit on a 20-node
  cluster promises memory the node may not have, and the NIC daemon's limit
  is multiplied by every node. Above about 3000 nodes even the raised values
  need hand-tuning.

AICR has no way to turn a node count into a resource value. `aicr bundle
--nodes N` copies N verbatim into the paths in
`nodeScheduling.nodeCountPaths`, and only nodewright's `estimatedNodeCount`
is listed there. The count is also often unknown when the bundle is
generated: the clusters in #3148 install a published OCI bundle through
`argocd-helm` and size it at install time.

## Goals

- Size controller memory requests and limits from a node count the user
  states, at generation time and at deploy time.
- Back every sized value with a measurement recorded next to it.
- Keep outputs reproducible: the same inputs give byte-identical bundles.
- Keep explicit user overrides authoritative.

## Non-Goals

- **Deriving a count AICR was not given.** The epic requires that node count
  alone never silently specializes a configuration, so a snapshot-derived
  `criteria.nodes` sizes nothing.
- **CPU, replica counts and client QPS.** These belong to
  [#2430](https://github.com/NVIDIA/aicr/issues/2430).
- **Inputs other than node count**, such as pod count, NicDevice count or
  node size (see Consequences).
- **Other components.** NFD master and NVSentinel are measured but not in
  scope. Adding them is a data change under this ADR.
- **Fixing the growth itself.** The NIC daemon's unscoped cache is #3149.
  gpu-operator's per-node cost is a question raised with its team in #3214.

## Context

Node counts in this ADR are all nodes, CPU and GPU, unless they say GPU
nodes.

### What the measurements show

Fleet readings cover clusters of 24 to about 2650 nodes, recorded in #3148
and #3214; the full tables and method are in the 2026-11-04 comment on
#3214. The tiers rest on cadvisor working-set series
(`container_memory_working_set_bytes`) from each cluster's Prometheus:

| Cluster | Nodes (GPU) | Pods | Window | Readings |
|---|---|---|---|---|
| OCI L40S | 417 (407) | 13.8k | 24h | p50 / p90 / max |
| OCI GB300 | 453 (447) | 18k | 24h | max only |
| OCI GB300 | 670 (656) | 21.7k | 24h | p50 / p90 / max |
| Bare-metal GB300 A | 2143 (2039) | 82k | 7d | p50 / p90 / max |
| Bare-metal GB300 B | 2191 at read time, up to 2653 (2012 to 2500) | 13.2k | 7d | p50 / p90 / max |

Instantaneous readings (one `kubectl top` sample) at 89 to 1400 GPU nodes
fill the gaps; the one the tiers use is at 1112 GPU nodes.

Three properties of the series shape the rules:

- **p90 sits within 5 to 10% of p50** for every controller. These are flat
  caches with occasional spikes, so p90 is a good "usually needed" value
  that reserves little more than the average.
- **Peaks can sit well above p90.** The 7-day maximum is 1.1x p90 for
  network-operator and nodewright, 1.3x for gpu-operator and 1.7x for
  maintenance-operator. The NIC daemon peaks at startup, when it lists the
  cluster-wide NicDevice cache.
- **Some controllers drift at a constant node count.** On bare-metal
  GB300 B, maintenance-operator's peak went from 5.2Gi to 11.8Gi in three
  weeks and network-operator's from 8.1Gi to 10.6Gi, with the node count
  roughly flat. A limit from the first reading would have killed
  maintenance-operator. gpu-operator moved from 3.4Gi to 3.0Gi.

The v1.0.0 defaults break at roughly these sizes. The column is in GPU
nodes because the older readings recorded only GPU nodes:

| Controller | v1.0.0 request / limit | Breaks near (GPU nodes) | Evidence |
|---|---|---|---|
| gpu-operator | 300Mi / 700Mi | 500 | 635Mi 24h peak at 447 (91%); 715Mi 7-day peak at 576 |
| network-operator (GB300) | 2Gi / 2Gi | 550 to 600 | 1594Mi 24h peak at 447 (78%); 75 restarts at 576 |
| maintenance-operator (GB300) | 256Mi / 1Gi | 500 to 700 | about 2.5Gi per 1000 nodes |
| nodewright manager | 2000Mi / 4000Mi | 600 to 900 | 1868Mi 24h peak at 447 |
| NIC configuration daemon (GB300) | 128Mi / 512Mi | 700 | per-pod max 400Mi at 89, 903Mi at 1112 |

### What drives the growth

gpu-operator and network-operator grow nearly linearly, at about 1.2 to
1.4Mi and 3.2 to 3.6Mi per node. maintenance-operator also grows at a
constant node count, for reasons not yet known
([#3267](https://github.com/NVIDIA/aicr/issues/3267) adds the scrape to find
out).

Nodewright was first read as pod-driven; the readings point to total node
count instead. Its cache holds every Node, per-node Skyhook CRs, pods, and
the package-execution Jobs it retains for `jobTtlSucceeded` (1h default) and
`jobTtlFailed` (24h).

- **Total nodes explain the small clusters.** On a cluster with 118 GPU
  nodes, 436 in total and 18k pods, the manager was OOM-killed at a 2Gi
  limit with a 1.86Gi working set, the same as on the 453-node OCI GB300
  cluster, at 1.87Gi. By GPU nodes the first looked like an outlier.
- **Pods do not explain the large ones.** Bare-metal GB300 A (2143 nodes,
  82k pods) runs both replicas at 6.7Gi; bare-metal GB300 B (up to 2653
  nodes, 13k pods) runs its leader at 10.5Gi. More nodes, plus Jobs retained
  from B's node churn, explain the gap.

The two small clusters also share a pod count (18k), so no measured pair
separates pods from nodes cleanly. Churn is not known in advance, so the
tiers size nodewright to the largest reading at each node count.

### What AICR already has

- `aicr bundle --nodes`, with config-file, REST (`nodes`) and SDK
  (`BundleOptions.Nodes`) equivalents.
- [ADR-021](021-component-upgrade-safety.md)'s pattern of per-component data
  in a registry-referenced file, validated fail-closed at load.
- [ADR-015](015-recipe-configuration-profiles.md)'s profile locks, which
  cover every mutation source including node-count injection, with a
  template-time guard on `argocd-helm` root-chart values.
- The `argocd-helm` child Application template, which already merges a
  component's static values with its root-chart values at render time.
- The readiness-gate chart, the precedent for install-time template logic in
  a generated chart.

## Options

### A. Static values sized for the largest cluster

The status quo after v1.1.0. Every cluster pays a 2500-node limit, requests
still misstate usage, and anything above 2500 nodes needs hand-tuning.

### B. A scale criteria axis or overlay per size band

Multiplies the recipe matrix by the number of bands, selects a
configuration by size, which #2427 rules out unless the user chooses it,
and cannot apply at install time.

### C. Documented overrides only

Users compute their own values with `--set` or install-time overrides, as
the #3148 clusters do today. Each new large cluster rediscovers the numbers,
usually through a crash loop.

### D. Measured tier tables selected by a stated count

A per-component table of measured tiers, keyed on a node count the user
states at generation or deploy time. The rest of this ADR describes it.

### E. A per-node formula

`base + per_node × N` fits gpu-operator and network-operator, but not
nodewright (churn) or maintenance-operator (drift at a constant count). It
resizes and restarts the controller on every node-count change, and it
cannot be qualified, because there is no finite set of points to measure.

A formula validated at one scale can miss badly at another. NodeWright
publishes
[scaling equations](https://docs.nvidia.com/nodewright/operations/resources-at-scale)
for its manager, validated to 1k nodes: a request of `N × 0.45Mi` and a
limit of `N × 0.8Mi`. At 2000 nodes they give 0.9Gi and 1.6Gi against a
measured 10.5Gi and 11.6Gi, about 12x and 7x off.

Tiers are the formula's bucketed form, as in the Kubernetes project's
[addon-resizer](https://github.com/kubernetes/autoscaler/tree/master/addon-resizer),
which rounds N up to a geometric bucket so an addon is not resized on every
node join.

### F. The Vertical Pod Autoscaler on the controllers

VPA sizes each cluster from its own history, the opposite of AICR's
contract that identical inputs give identical, qualified bundles. It is
another component to run, needs days of metrics before it recommends
anything, applies a change by evicting the pod (a controller restart,
which #3148 showed is costly for gpu-operator), and needs the same scrapes
the controllers lack today (#3265 to #3269). It moves the measurement
problem rather than removing it.

### Comparison

| | A. Static | B. Criteria axis | C. Overrides only | D. Tiers | E. Formula | F. VPA |
|---|---|---|---|---|---|---|
| Explicit intent, no silent specialization | yes | yes | yes | yes | yes | no |
| Sizes a published bundle at install time | no | no | yes | yes | yes | yes |
| Every shipped value measured and reproducible | no | yes | no | yes | no | no |
| Small clusters pay only for their size | no | yes | yes | yes | yes | yes |
| Resizes only at a few known boundaries | n/a | yes | n/a | yes | no | no |
| No new recipe-matrix dimension | yes | no | yes | yes | yes | yes |
| No new in-cluster component | yes | yes | yes | yes | yes | no |

## Decision

Adopt option D. Each controller whose memory grows with cluster size
declares measured tiers in `recipes/components/<name>/sizing.yaml`,
referenced from the registry. Only a count the user states selects a tier.

A tier's request is the p90 of the working set at the largest cluster
measured in the tier; its limit is the highest peak measured in the tier,
times 1.3. No tier is interpolated. Tier values never decrease as the count
rises, and applying a tier only raises values. Outside an ADR-015 lock set,
an explicit user override replaces the tier's result.

## Proposal

### Defaults

With no count, controllers carry the values AICR shipped in v1.0.0, before
#3212 and #3236:

| Controller | Values path | Request / limit |
|---|---|---|
| gpu-operator | `operator.resources` | 300Mi / 700Mi |
| network-operator (GB300) | `operator.resources` | 2Gi / 2Gi |
| maintenance-operator (GB300) | `maintenance-operator-chart.operator.resources` | 256Mi / 1Gi |
| nodewright manager | `controllerManager.manager.resources` | 2000Mi / 4000Mi |
| NIC configuration daemon (GB300) | `nicConfigurationOperator.configurationDaemon.resources` | 128Mi / 512Mi |

That applies to a bundle generated without `--nodes` and to an
`argocd-helm` bundle installed with `sizing.nodeCount: 0`. A cluster larger
than the first tier that states no count is under-provisioned by choice;
the documentation says so. #3236 moved the NIC daemon's resources into
network-operator's values; that move stays, and only its numbers revert.

### The node count

The count comes from `aicr bundle --nodes N` (or its config-file, REST and
SDK equivalents) or from `sizing.nodeCount` in an `argocd-helm` bundle's
root-chart values. `criteria.nodes` and snapshot-derived counts never select
a tier.

The count is every node in the cluster, CPU and GPU, as `kubectl get nodes`
reports. Each sized controller caches every Node object, so a node without
a GPU costs memory too. On the GB300 clusters measured, nodes without GPUs
are 2 to 9% of the total, and every measured cluster lands in the same tier
whether counted by GPU or by total nodes. A cluster with a large CPU pool
would not: the 118-GPU-node cluster in Context has 436 nodes.

`--nodes` today is an estimated GPU-node count, copied into
`nodeScheduling.nodeCountPaths`. Its only path is nodewright's
`estimatedNodeCount`, which has no effect because AICR sets the manager's
resources explicitly. This ADR redefines the flag as all nodes and keeps the
copy. A caller still passing a GPU count can land one tier low, so the flag
help, the docs and the release notes state the new meaning.

There is no separate count for CPU nodes:

- The tiers are 2 to 3x wide, so a second count would rarely change the
  tier selected.
- No measurement separates what a CPU node costs these controllers from
  what a GPU node costs, so a two-dimensional table would rest on invented
  values.
- Each parameter adds a flag, REST parameter, SDK field, config key,
  root-chart value and schema entry.

The sizing file's `basis` field leaves room for one if measurements later
warrant it.

Every input takes a non-negative integer; 0 means no count. A negative
count is rejected with `ErrCodeInvalidRequest` on the CLI, config-file,
REST and SDK paths. On `argocd-helm` the schema declares the same minimum,
and because schema validation can be skipped, the template also fails on a
negative `sizing.nodeCount`.

### The sizing file

`recipes/components/<name>/sizing.yaml`, referenced from the registry
entry's `sizing.file`:

```yaml
apiVersion: aicr.run/v1beta1
kind: ComponentSizing
basis: nodes
resources:
  - path: operator.resources
    tiers:
      - maxNodes: 300
      - maxNodes: 600
        requests: {memory: 512Mi}
        limits: {memory: 768Mi}
        source: "#3214 (2026-11-04): p90 410Mi, max 496Mi, 24h at 417 nodes (407 GPU), 13.8k pods"
      - maxNodes: 1500
        requests: {memory: 768Mi}
        limits: {memory: 1Gi}
        source: "#3214 (2026-11-04): p90 602Mi, max 744Mi, 24h at 670 nodes (656 GPU), 21.7k pods"
      - maxNodes: 3000
        requests: {memory: 2.5Gi}
        limits: {memory: 4Gi}
        source: "#3214 (2026-11-04): p90 2353Mi, max 3006Mi, 7d at 2191 to 2653 nodes (2012 to 2500 GPU), 13.2k pods"
  - path: maintenance-operator-chart.operator.resources
    requiresPath: maintenance-operator-chart
    tiers: [...]
```

**Fields:**

- `apiVersion` and `kind`: the artifact header, as for ADR-021's
  `ComponentUpgrades`.
- `basis`: the count the table is keyed on. Only `nodes` is defined.
- `path`: the values path of a resources block, as `--set` writes it.
- `requiresPath`: optional. The entry applies only when the resolved values
  contain this key, so the maintenance-operator and NIC daemon entries apply
  only where the GB300 values file defines them.
- `maxNodes`: the tier's upper bound, which also names the tier. A tier
  covers counts above the previous tier's `maxNodes`, up to its own.
- `requests` and `limits`: memory quantities. A tier without them leaves
  the values unchanged; the first tier normally has none, so a cluster in it
  keeps the defaults.
- `source`: required on every tier that sets a value. It names the issue
  and date, p90 and max, the window, and the measured cluster's total
  nodes, GPU nodes and pod count.

Above the top tier, the top tier applies and `aicr bundle` logs a warning;
see the note on the 3000 ceiling.

**Deriving values:**

- **Request:** the p90 of the working set over the window, at the largest
  cluster measured in the tier. The pod then stays under its request, and
  out of the kubelet's first eviction group, 90% of the time. Not the
  limit, as GKE
  [recommends](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/vertical-pod-autoscaling):
  that would reserve the peak on every system node, and these controllers
  spend most of their time well under it.
- **Limit:** the highest peak measured in the tier, over the longest window
  available, times 1.3. The 30% covers spikes above p90 and some drift
  between measurements; see Open questions for tightening it.
- **Rounding:** round up to a multiple of 256Mi below 2Gi, and of 512Mi
  from 2Gi.
- **Instantaneous readings:** the reading is used as the p90, and reading ×
  1.7 as the peak, so the limit is reading × 1.7 × 1.3. 1.7 is the largest
  max-to-p90 ratio measured on any controller. A windowed reading replaces
  an instantaneous one as soon as it exists.
- **DaemonSets** (the NIC daemon): the request uses the median across pods
  of each pod's p90, and the limit uses the largest single-pod peak.
- **Missing data:** a component with no reading inside a tier is sized from
  its nearest reading above the tier, never below.
- **Monotonic tiers:** no tier value is below the same field in the tier
  under it, with the defaults as the first tier.
- **Re-measurement:** peaks are re-read, from the longest window kept (7
  days where available), before each release that changes the component's
  pin or values. For a controller that drifts at a constant node count, the
  limit uses the drifted peak.

**Validation.** The loader fails closed with `ErrCodeInvalidRequest` on:

- a malformed path;
- a `basis` other than `nodes`;
- `maxNodes` values that do not strictly ascend;
- a quantity that does not parse;
- a limit below its request;
- a tier that sets a value without a `source`;
- a tier that breaks monotonic tiers.

The contributor guide (`docs/contributor/component-sizing.md`, added by the
implementation PR) walks through writing a table, and no unit test asserts
a real component's tier values, the same rule as for upgrade records.

### Initial tiers

Values are memory request / limit, derived with the rules above. The
`2000Mi` and `4000Mi` figures are the v1.0.0 literals.

| Tier (nodes) | gpu-operator | network-operator | maintenance-operator | nodewright (per replica) | NIC daemon (per pod) |
|---|---|---|---|---|---|
| ≤ 300 | 300Mi / 700Mi | 2Gi / 2Gi | 256Mi / 1Gi | 2000Mi / 4000Mi | 128Mi / 512Mi |
| 301 to 600 | 512Mi / 768Mi | 2Gi / 2.5Gi | 2.5Gi / 5.5Gi | 2000Mi / 4000Mi | 768Mi / 2Gi |
| 601 to 1500 | 768Mi / 1Gi | 2Gi / 2.5Gi | 5Gi / 7Gi | 2.5Gi / 4000Mi | 768Mi / 2Gi |
| 1501 to 3000 | 2.5Gi / 4Gi | 10.5Gi / 13.5Gi | 7Gi / 15Gi | 10.5Gi / 15Gi | 1Gi / 2.5Gi |

The ≤ 300 tier is the defaults; every reading in that range sits under
them. The reading behind each other cell, in Mi as p90 / peak (24h unless
marked 7d); the full data is in #3214:

| Tier (nodes) | gpu-operator | network-operator | maintenance-operator | nodewright leader | NIC daemon (per pod) |
|---|---|---|---|---|---|
| 301 to 600 | 410 / 496, OCI 417 | 955 (OCI 417) / 1594 (OCI 453) | 2345, instantaneous, 1112 GPU nodes † | 1377 (OCI 417) / 1868 (OCI 453) | 707 / 903, instantaneous, 1112 GPU nodes † |
| 601 to 1500 | 602 / 744, OCI 670 | 1384 / 1737, OCI 670 | 4996 / 5462, B at 2191 † | 2163 / 2287, OCI 670 | 707 / 903, instantaneous, 1112 GPU nodes |
| 1501 to 3000 | 2353 / 3006, 7d, B | 10341 / 10619, 7d, B | 7047 / 11768, 7d, B | 10555 / 11625, 7d, B | 901 / 1599, 7d, B |

† No reading in the tier; sized from the nearest reading above.

Notes on the table:

- **Over-sized cells.** Cells marked † and the instantaneous readings are
  the first to requalify once a cluster of that size runs the component
  with a scrape.
- **Cells set by monotonic tiers, not by their own reading:**
  - network-operator's request at 301 to 600 and 601 to 1500 (p90 0.9Gi and
    1.4Gi, below the 2Gi default);
  - nodewright's request at 301 to 600 (p90 1.3Gi, below 2000Mi);
  - nodewright's limit at 301 to 600 and 601 to 1500 (1.3x peak is 2.4Gi
    and 2.9Gi, below 4000Mi).
- **Nodewright replicas.** The request is per replica. On bare-metal
  GB300 B the standby's p90 is 3.6Gi against the leader's 10.5Gi, so the
  standby over-reserves by about 7Gi; on A both sit at 6.7Gi.
- **Per-node totals.** AICR does not keep the nodewright replicas on
  separate nodes. On one system node, the top tier totals 31.5Gi of
  requests and 50Gi of limits with one replica, and 42Gi and 65Gi with
  both, against 866Gi to 993Gi allocatable on the GB300 system nodes
  measured.
- **The 3000 ceiling.** Nothing above about 2650 nodes has been measured,
  and the NIC daemon's working set grows with the cluster-wide NicDevice
  count (about 0.16Mi per device at the median; the largest cluster
  measured had about 5000). Above 3000 the top tier still applies; raising
  the ceiling needs a new measured tier.

### Precedence

Applying a tier is raise-only: it sets each memory request and limit to the
larger of the current value and the tier's value, compared as Kubernetes
quantities. An unparseable current value fails the bundle. A tier therefore
never undercuts a larger value from a recipe overlay or mixin.

On a path outside any ADR-015 profile lock set, explicit user overrides are
applied after sizing and replace its result: `--set`, `--set-json` and
`--set-file` at generation time, and the component's root-chart values at
install time.

Sizing never writes a path in a lock set. On such a path ADR-015's rules
apply unchanged: a divergent bundle-time override is rejected, one that
restates the locked value passes, and an `argocd-helm` root-chart value
that intersects the path fails rendering.

### Generation time (`--nodes`)

The sizing pass runs once per component, after base, overlay and inline
values resolve and before user overrides. For every deployer except
`argocd-helm` the result is baked into the component's values, including
bundle-rendered manifests such as the NIC daemon's NicClusterPolicy.

`bundle-info.yaml` does not record the tier. Its reader rejects unknown
fields, so a new field needs a release that reads it before one that writes
it (ADR-022 §3), and nothing shows its `warnings` to users. The sized
values in each component's `values.yaml` are the record.

### Deploy time (`argocd-helm`)

The root-chart values are the `values.yaml` of the generated `argocd-helm`
chart, which the operator sets at install. Remote-chart components with a
sizing table are not sized at generation time. The bundle carries the
table, and Helm selects a row at render time:

- The root-chart values gain `sizing: {nodeCount: N}`, where N is the
  bundle's `--nodes` or 0. `values.schema.json` declares it as an integer
  of at least 0.
- For each sized component, the bundler writes `static/<comp>-sizing.yaml`
  with every tier's effective values, already raised against the
  component's unsized static values. The template only selects a row; it
  never compares quantities.
- The child Application template merges the component's static values file
  (`$static`), then the selected row, then the component's root-chart
  values, so user values still win. A count of 0 selects nothing.
- The template stamps the count and tier on each sized child Application as
  annotations (`aicr.nvidia.com/sizing-node-count`,
  `aicr.nvidia.com/sizing-tier`), the render-time record of what an
  installation applied. Helm has no place to show a warning, so the bundle
  README and the docs carry the above-3000 guidance.

One code path decides each component's sizing: on an `argocd-helm` bundle,
`--nodes` only sets the default `sizing.nodeCount`.

Two cases are handled specially:

- **Bundle-rendered manifests** cannot see root-chart values, so they keep
  generation-time sizing. Today that is only the NIC daemon; the bundle
  README says `sizing.nodeCount` does not reach it and `--nodes` does.
- **A `--dynamic` export of a sized path** is rejected at bundle time. Its
  pre-filled root value would otherwise always mask the selected tier.

Other deployers have no install-time sizing yet; they can add it by
following this section.

### For operators

- **Which count.** Pass the count the cluster will reach before its next
  re-bundle or `sizing.nodeCount` change, not only today's. A count that is
  too high over-reserves; one that is too low risks OOM kills.
- **Changing it.** On `argocd-helm`, moving `sizing.nodeCount` across a
  tier boundary re-renders the sized child Applications and restarts their
  controllers. Within a tier only the annotations change.
- **Checking it.** The applied values are in each component's `values.yaml`;
  on `argocd-helm` the child Applications' sizing annotations show the count
  and tier.
- **Above 3000 nodes.** The top tier applies; set overrides from your own
  measurements.
- **The NIC daemon on a published `argocd-helm` bundle.** `sizing.nodeCount`
  does not reach it. Generate the bundle with `--nodes`, or set
  `nicConfigurationOperator.configurationDaemon.resources` with `--set` at
  generation time.

## Consequences

**Positive:**

- Small clusters go back to small reservations.
- Large clusters get requests that match usage, so the scheduler places
  controllers where they fit.
- A published `argocd-helm` bundle is sized with one value instead of a
  hand-copied block per controller.
- Every sized value points to its measurement, and bundles are byte-stable
  for the same inputs.

**Negative:**

- **Larger default exposure.** A cluster above 300 nodes that states no
  count regresses to the pre-v1.1.0 failure mode, by design. The release
  notes must call it out.
- **The NIC daemon is not sized at install time.** A published
  `argocd-helm` bundle built without `--nodes`, the #3148 case, leaves it at
  512Mi whatever `sizing.nodeCount` says.
- **Node count is an imperfect proxy.** Nodewright may also track pods, and
  Jobs retained from churn add to it; maintenance-operator drifts at a
  constant count; the NIC daemon tracks NicDevices. Each `source` records
  the measured cluster's total nodes, GPU nodes and pods, so an operator
  with a far denser or churnier cluster knows to override.
- **Thin mid-range data.** The clusters measured between 300 and 1500
  nodes run neither maintenance-operator nor the NIC daemon, so those cells
  are sized from larger clusters and over-reserve. Nothing between 670 and
  2191 nodes has a scrape.
- **The 30% margin is a stopgap** (see Open questions).
- **The count is the user's responsibility.** A stale count after a
  scale-up under-sizes the cluster until the bundle is regenerated or
  `sizing.nodeCount` is changed.

## Adoption plan

1. This ADR.
2. One implementation PR, closing
   [#3643](https://github.com/NVIDIA/aicr/issues/3643) and
   [#3644](https://github.com/NVIDIA/aicr/issues/3644):
   - the `sizing.yaml` schema, loader and data files, and the restored
     defaults;
   - the generation-time sizing pass;
   - the `argocd-helm` root value, per-tier static files, template
     selection, annotations and `--dynamic` guard;
   - the user docs and `docs/contributor/component-sizing.md`.

Both halves ship together: restoring the defaults without the `argocd-helm`
path would leave the #3148 clusters on hand overrides again.

## Open questions

- **Requalifying the 301 to 600 and 601 to 1500 tiers, and tightening the
  30% margin,** once the five controllers are scraped continuously
  ([#3265](https://github.com/NVIDIA/aicr/issues/3265) to
  [#3269](https://github.com/NVIDIA/aicr/issues/3269)). VPA's default is
  15% over the p90 of daily peaks, but it re-measures continuously; AICR
  measures once per release. Until then, drift is found at the next
  measurement or the next OOM kill.
- **A separate CPU-node count** (see The node count), if a cluster with a
  large CPU pool lands in the wrong tier.
- **Nodewright inputs known at bundle time.** AICR knows how many packages a
  recipe ships, which NodeWright's own formula multiplies by, and can set
  `jobTtlSucceeded` and `jobTtlFailed` lower in the large tiers to shrink
  the retained-Job cache. Either refines the nodewright cells; neither adds
  an input to tier selection.
- **Reporting the measured NodeWright numbers upstream** (see Option E).
- **Install-time sizing for the other deployers.** For `helm` and
  `helmfile`, `install.sh` could read an `AICR_NODE_COUNT` environment
  variable and layer per-tier values files. `flux`, `fleet` and `terraform`
  are not yet designed.
- **Passing a count from the AICR Operator**
  ([ADR-027](027-aicr-operator.md)) to the bundle step it runs.
