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
out of memory. A tier can raise a value but never lower it, and an explicit
user override wins.

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
  a system node runs short, the kubelet evicts these pods first, because
  they run furthest above their requests. Large clusters still hand-set
  requests through `valueOverrideKeys`.
- **One value cannot fit every size.** A 2500-node limit on a 20-node
  cluster promises memory the node may not have, and the NIC daemon's limit
  is multiplied by every node. Above about 3000 nodes even the raised values
  need hand-tuning.

AICR cannot derive a value from cluster size. `aicr bundle --nodes N` copies
N verbatim into the paths in `nodeScheduling.nodeCountPaths`, and only
nodewright's `estimatedNodeCount` is listed there. The count is also often
unknown when the bundle is generated: the clusters in #3148 install a
published OCI bundle through `argocd-helm` and size it from the root
Application's values.

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

### What the measurements show

Fleet readings cover clusters of 24 to about 2650 nodes, recorded in #3148
and #3214; the full tables and method are in the 2026-11-04 comment on
#3214. The tiers rest on cadvisor working-set series from each cluster's
Prometheus:

| Cluster | Nodes (GPU) | Pods | Window |
|---|---|---|---|
| OCI L40S | 417 (407) | 13.8k | 24h |
| OCI GB300 | 670 (656) | 21.7k | 24h |
| Mistral GB300 | 2143 (2039) | 82k | 7d |
| Lambda GB300 | 2191 at read time, up to 2653 (2012) | 13k | 7d |

Older instantaneous readings at 89 to 1400 GPU nodes fill the gaps. Node
counts in this ADR are all nodes unless they say GPU nodes.

Three properties of the series shape the rules:

- **p90 sits within 5 to 10% of p50** for every controller. These are flat
  caches with occasional spikes, so p90 is a good "usually needed" value
  that reserves little more than the average.
- **Peaks can sit well above p90.** The 7-day maximum is 1.1x p90 for
  network-operator and nodewright, 1.3x for gpu-operator and 1.7x for
  maintenance-operator. The NIC daemon peaks at startup, when it lists the
  cluster-wide NicDevice cache.
- **Some controllers drift at a constant node count.** On the Lambda
  cluster, maintenance-operator's peak went from 5.2Gi to 11.8Gi in three
  weeks and network-operator's from 8.1Gi to 10.6Gi, with the node count
  roughly flat. A limit from the first reading would have killed
  maintenance-operator. gpu-operator moved from 3.4Gi to 3.0Gi.

The v1.0.0 defaults break at roughly these sizes:

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
  limit with a 1.86Gi working set, the same as on a cluster with 453 nodes
  (447 GPU), at 1.87Gi. By GPU nodes the first looked like an outlier.
- **Pods do not explain the large ones.** The Mistral cluster (2143 nodes,
  82k pods) runs both replicas at 6.7Gi; the Lambda cluster (up to 2653
  nodes, 13k pods) runs its leader at 10.5Gi. More nodes, plus Jobs retained
  from Lambda's node churn, explain the gap.

The two small clusters also share a pod count, so no measured pair
separates pods from nodes cleanly. Churn is not known in advance, so the
tiers size nodewright to the largest reading at each node count.

### Guidance elsewhere

Outside sources agree on the shape and differ on the headroom:

- The Kubernetes project sizes kube-system addons from node count with
  [addon-resizer](https://github.com/kubernetes/autoscaler/tree/master/addon-resizer):
  `base + per_node × N`, with N rounded up to the next bucket of a geometric
  sequence (ratio 1.5) so the addon is not resized on every node join. That
  is a tier table.
  [metrics-server](https://github.com/kubernetes-sigs/metrics-server/blob/master/FAQ.md)
  and
  [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics#scaling-kube-state-metrics)
  document linear growth in node and object count and point at it.
- Under
  [node-pressure eviction](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/#pod-selection-for-kubelet-eviction),
  the kubelet evicts first the pods whose usage exceeds their request,
  ranked by how far over they are. That is why requests, not only limits,
  must track usage.
- [GKE](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/vertical-pod-autoscaling)
  recommends a memory request equal to the limit. This ADR does not follow
  it: it would reserve the peak on every system node, and these controllers
  spend most of their time well under it.
- The Vertical Pod Autoscaler's default margin is 15% over the p90 of daily
  peaks across 8 days (`--recommendation-margin-fraction`). That assumes
  continuous re-measurement. AICR measures once per release, so it carries
  a wider margin until the controllers are scraped continuously.
- NodeWright publishes
  [scaling equations](https://docs.nvidia.com/nodewright/operations/resources-at-scale)
  for its manager, validated to 1k nodes: a request of `N × 0.45Mi` and a
  limit of `N × 0.8Mi`. At 2000 nodes they give 0.9Gi and 1.6Gi against a
  measured 10.5Gi and 11.6Gi, about 12x and 7x off. A formula validated at
  one scale can miss badly at another, which is the case for measuring
  every tier.

### What AICR already has

- `aicr bundle --nodes`, with config-file, REST (`nodes`) and SDK
  (`BundleOptions.Nodes`) equivalents.
- [ADR-021](021-component-upgrade-safety.md)'s pattern of per-component data
  in a registry-referenced file, validated fail-closed at load.
- [ADR-015](015-recipe-configuration-profiles.md)'s profile locks, which
  cover every mutation source including node-count injection, with a
  template-time guard on `argocd-helm` install-time values.
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
NodeWright's own formula shows how far one can miss. Tiers are the
formula's bucketed form, as in addon-resizer.

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
referenced from the registry. Only a count the user states selects a tier;
a count AICR infers, such as `criteria.nodes`, never does.

A tier's request is the p90 of the measured working set, and its limit is
the highest measured peak times 1.3, both from the largest cluster measured
in the tier. No tier is interpolated. A tier never lowers a value. Outside
an ADR-015 lock set, an explicit user override replaces the tier's result.
The sized values are the record of the tier applied.

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
the documentation says so. #3236's change that sources the NIC daemon's
resources from the network-operator values stays; only its numbers revert.

### The node count

The count comes from `aicr bundle --nodes N` (or its config-file, REST and
SDK equivalents) or from `sizing.nodeCount` in an `argocd-helm` bundle's
root values. `criteria.nodes` and snapshot-derived counts never select a
tier.

The count is every node in the cluster, CPU and GPU, as `kubectl get nodes`
reports. Each sized controller caches every Node object, so a node without
a GPU costs memory too. On the GB300 clusters measured, nodes without GPUs
are 2 to 9% of the total, and every measured cluster lands in the same tier
whether counted by GPU or by total nodes. A cluster with a large CPU pool
would not: the 118-GPU-node cluster in Context has 436 nodes.

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
kind: ComponentSizing
basis: nodes
resources:
  - path: operator.resources
    tiers:
      - maxNodes: 300
      - maxNodes: 600
        requests: {memory: 512Mi}
        limits: {memory: 768Mi}
        source: "#3214 (2026-11-04): p90 410Mi, max 496Mi, 24h at 417 nodes (407 GPU)"
      - maxNodes: 1500
        requests: {memory: 768Mi}
        limits: {memory: 1Gi}
        source: "#3214 (2026-11-04): p90 602Mi, max 744Mi, 24h at 670 nodes (656 GPU)"
      - maxNodes: 3000
        requests: {memory: 2.5Gi}
        limits: {memory: 4Gi}
        source: "#3214 (2026-11-04): p90 2353Mi, max 3006Mi, 7d at 2191-2653 nodes (2012-2500 GPU)"
  - path: maintenance-operator-chart.operator.resources
    requiresPath: maintenance-operator-chart
    tiers: [...]
```

**Structure:**

- A tier covers counts above the previous tier's `maxNodes`, up to its own.
  A tier with no `requests` or `limits` leaves the values unchanged.
- `requiresPath` applies an entry only when the resolved values contain
  that key, so the maintenance-operator and NIC daemon entries apply only
  where the GB300 values file defines them.
- Above the top tier, the top tier applies, and `aicr bundle` logs a
  warning that no cluster that size was measured. Raising the ceiling needs
  a new measured tier.

**Deriving values:**

- **Request:** the p90 of the working set over the measurement window, at
  the largest cluster measured in the tier. The pod then stays under its
  request, and out of the kubelet's first eviction group, 90% of the time.
- **Limit:** the highest peak in the tier, over the longest window
  available, times 1.3. The 30% covers spikes above p90 and some drift
  between measurements. It is wider than VPA's 15% because AICR re-measures
  once per release, not continuously.
- **Rounding:** up, to the next 256Mi below 2Gi and the next 512Mi above.
- **Instantaneous readings** stand in for p90 as-is, and for the peak after
  multiplying by 1.7, the largest max-to-p90 ratio measured on any
  controller. A windowed reading replaces one as soon as it exists.
- **Missing data:** a component with no measurement inside a tier is sized
  from its nearest measurement above the tier, never below.
- **Never lower:** no tier value is below the same field in the tier under
  it.
- **Re-measurement:** peaks are re-read, from the longest window kept (7
  days where available), before each release that changes the component's
  pin or values. For a controller that drifts at a constant node count, the
  limit uses the drifted peak.

Every tier that changes a value carries a `source` naming its measurement.

**Validation.** The loader fails closed with `ErrCodeInvalidRequest` on:

- a malformed path;
- `maxNodes` values that do not strictly ascend;
- a quantity that does not parse;
- a limit below its request;
- a tier that changes a value without a `source`;
- a tier value below the same field in the tier under it.

### Initial tiers

Counts are all nodes; values are memory request / limit, derived with the
rules above.

| Tier | gpu-operator | network-operator | maintenance-operator | nodewright (per replica) | NIC daemon (per pod) |
|---|---|---|---|---|---|
| ≤ 300 | 300Mi / 700Mi | 2Gi / 2Gi | 256Mi / 1Gi | 2000Mi / 4000Mi | 128Mi / 512Mi |
| 301 to 600 | 512Mi / 768Mi | 2Gi / 2.5Gi | 2.5Gi / 5.5Gi | 2000Mi / 4000Mi | 768Mi / 2Gi |
| 601 to 1500 | 768Mi / 1Gi | 2Gi / 2.5Gi | 5Gi / 7Gi | 2.5Gi / 4000Mi | 768Mi / 2Gi |
| 1501 to 3000 | 2.5Gi / 4Gi | 10.5Gi / 13.5Gi | 7Gi / 15Gi | 10.5Gi / 15Gi | 1Gi / 2.5Gi |

The readings behind each cell (Mi; p90 / max; window; cluster):

| Tier | gpu-operator | network-operator | maintenance-operator | nodewright leader | NIC daemon (per pod) |
|---|---|---|---|---|---|
| ≤ 300 | max 384, 24h, 244 GPU nodes | max 728, 24h, 244 GPU nodes | 290 at 89 GPU nodes; 130 to 156 at 141 | max 1024, 24h, 244 GPU nodes (11k pods) | max 400 at 89 GPU nodes, flat from 24 |
| 301 to 600 | 410 / 496, 24h, OCI L40S 417 nodes | 955 / 1392, 24h, OCI L40S 417; max 1594, 24h, OCI GB300 453 | **none in tier**; sized from 2345 instantaneous at about 1120 nodes | 1377 / 1536, 24h, OCI L40S 417; max 1868, 24h, OCI GB300 453; 1859 at OOM, RTX PRO 436 | **none in tier**; sized from p90 707 / max 903 across pods, instantaneous, at about 1120 nodes |
| 601 to 1500 | 602 / 744, 24h, OCI GB300 670 | 1384 / 1737, 24h, OCI GB300 670 | **none in tier**; sized from 4996 / 5462, 24h, Lambda at 2191 | 2163 / 2287, 24h, OCI GB300 670 | p90 707 / max 903 across pods, instantaneous, at about 1120 nodes |
| 1501 to 3000 | 2353 / 3006, 7d, Lambda 2191 to 2653; 1757 / 2579, 7d, Mistral 2143 | 10341 / 10619, 7d, Lambda | 7047 / 11768, 7d, Lambda | 10555 / 11625, 7d, Lambda; 6674 / 8295, 7d, Mistral | per-pod p90 median 901, q99 1069; per-pod max 1599, 7d, Lambda |

Notes on the table:

- **Over-sized cells.** Cells marked **none in tier** are sized from a
  larger cluster. They and the instantaneous readings are the first to
  requalify once a cluster of that size runs the component with a scrape.
- **Cells set by never-lower, not by their own reading:**
  - network-operator's request at 301 to 600 and 601 to 1500 (p90 0.9Gi and
    1.4Gi, below the 2Gi default);
  - nodewright's request at 301 to 600 (p90 1.3Gi, below 2000Mi);
  - nodewright's limit at 301 to 600 and 601 to 1500 (1.3x peak is 2.4Gi
    and 2.9Gi, below 4000Mi).
- **Nodewright replicas.** The request is per replica. On the Lambda
  cluster the standby's p90 is 3.6Gi against the leader's 10.5Gi, so the
  standby over-reserves by about 7Gi; on Mistral both sit at 6.7Gi.
- **Per-node totals.** AICR does not keep the nodewright replicas on
  separate nodes. On one system node, the top tier totals 31.5Gi of
  requests and 50Gi of limits with one replica, and 42Gi and 65Gi with
  both, against 866Gi to 993Gi allocatable on the GB300 system nodes
  measured.
- **The 3000 ceiling.** Nothing above about 2650 nodes has been measured,
  and the NIC daemon's working set grows with the cluster-wide NicDevice
  count (about 0.16Mi per device at the median; the largest cluster
  measured had about 5000).

### Precedence

Applying a tier sets each memory request and limit to the larger of the
current value and the tier's value, compared as Kubernetes quantities; an
unparseable current value fails the bundle. A tier therefore never
undercuts a larger value from a recipe overlay or mixin.

On a path outside any ADR-015 profile lock set, explicit user overrides are
applied after sizing and replace its result: `--set`, `--set-json` and
`--set-file` at generation time, and the component's root-chart values at
install time.

Sizing never writes a path in a lock set. On such a path ADR-015's rules
apply unchanged: a divergent bundle-time override is rejected, one that
restates the locked value passes, and an `argocd-helm` install-time value
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

Remote-chart components with a sizing table are not sized at generation
time. The bundle carries the table, and Helm selects a row at render time:

- The root `values.yaml` gains `sizing: {nodeCount: N}`, where N is the
  bundle's `--nodes` or 0. `values.schema.json` declares it as an integer
  of at least 0.
- For each sized component, the bundler writes `static/<comp>-sizing.yaml`
  with every tier's effective values, already computed with the never-lower
  rule against the unsized static values. The template only selects a row;
  it never compares quantities.
- The child Application template merges `$static`, then the selected row,
  then the root chart's values for the component, so user values still win.
  A count of 0 selects nothing.
- The template stamps the count and tier on each sized child Application as
  annotations (`aicr.nvidia.com/sizing-node-count`,
  `aicr.nvidia.com/sizing-tier`), the render-time record of what an
  installation applied. Above the top tier the tier annotation is the top
  tier's `maxNodes`; Helm has no place to show a warning, so the bundle
  README and the docs carry that guidance.

One code path decides each component's sizing: on an `argocd-helm` bundle,
`--nodes` only sets the default `sizing.nodeCount`.

Two cases are handled specially:

- **Bundle-rendered manifests** cannot see install-time values, so they keep
  generation-time sizing. Today that is only the NIC daemon; the bundle
  README says `sizing.nodeCount` does not reach it and `--nodes` does.
- **A `--dynamic` export of a sized path** is rejected at bundle time. Its
  pre-filled root value would otherwise always mask the selected tier.

Other deployers have no install-time sizing yet; they can add it by
following this section.

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
- **Node count is an imperfect proxy.** Nodewright may also track pods, and
  Jobs retained from churn add to it; maintenance-operator drifts at a
  constant count; the NIC daemon tracks NicDevices. Each `source` records
  the measured cluster's total nodes, GPU nodes and pods, so an operator
  with a far denser or churnier cluster knows to override.
- **Thin mid-range data.** The two clusters measured between 300 and 1500
  nodes run neither maintenance-operator nor the NIC daemon, so those cells
  are sized from larger clusters and over-reserve. Nothing between 670 and
  2191 nodes has a scrape.
- **The 30% margin is a stopgap** for measuring once per release. It comes
  down once the controllers are scraped continuously (#3265 to #3269).
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
   - the docs.

Both halves ship together: restoring the defaults without the `argocd-helm`
path would leave the #3148 clusters on hand overrides again.

## Open questions

- **Requalifying the 301 to 600 and 601 to 1500 tiers, and tightening the
  30% margin toward VPA's 15%,** once the five controllers are scraped
  continuously ([#3265](https://github.com/NVIDIA/aicr/issues/3265) to
  [#3269](https://github.com/NVIDIA/aicr/issues/3269)). Until then, drift is
  found at the next measurement or the next OOM kill.
- **A separate count for CPU nodes**, if a cluster with a large CPU pool
  lands in the wrong tier, or if measurements show a CPU node costs these
  controllers far less than a GPU node. The `basis` field is where it would
  go.
- **Nodewright inputs known at bundle time.** AICR knows how many packages a
  recipe ships, which NodeWright's own formula multiplies by, and can set
  `jobTtlSucceeded` and `jobTtlFailed` lower in the large tiers to shrink
  the retained-Job cache. Either refines the nodewright cells; neither adds
  an input to tier selection.
- **Reporting the measured NodeWright numbers upstream.** Its published
  equations undersize the manager by about 12x (request) and 7x (limit) at
  2000 nodes.
- **Install-time sizing for the other deployers.** For `helm` and
  `helmfile`, `install.sh` could read an `AICR_NODE_COUNT` environment
  variable and layer per-tier values files. `flux`, `fleet` and `terraform`
  are not yet designed.
- **Passing a count from the AICR Operator**
  ([ADR-027](027-aicr-operator.md)) to the bundle step it runs.
