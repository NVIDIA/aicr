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
recorded in #3148 and #3214, the full tables with method in the 2026-11-04
comment on #3214. The sizing in this ADR rests on cadvisor working-set
series read from each cluster's Prometheus: 24h windows on an OCI L40S
cluster (407 GPU nodes) and an OCI GB300 cluster (656), and 7-day windows on
a Mistral GB300 cluster (2039 nodes, 82k pods) and a Lambda GB300 cluster
(2012 nodes at read time, about 2500 earlier in the window). Older
instantaneous readings at 89 to 1400 nodes fill the gaps.

Three properties of the series shape the rules below:

- **p90 sits close to p50.** For every controller the 90th percentile of the
  working set is within 5 to 10% of the median. These are flat informer
  caches with occasional spikes, so p90 is a good "usually needed" number
  and reserves little more than the average would.
- **Peaks are far above p90 for some controllers.** The maximum over 7 days
  is 1.1x p90 for network-operator and nodewright, 1.3x for gpu-operator and
  1.7x for maintenance-operator. NFD master's hourly resync pass is about
  1.5x its steady state. The NIC daemon's peak is its startup list of the
  cluster-wide NicDevice cache, 15 to 30% above its running working set.
- **Some controllers drift at a constant node count.** On the Lambda cluster,
  maintenance-operator's 24h peak went from 5.2Gi on 2026-10-09 to 11.8Gi
  three weeks later, and network-operator's from 8.1Gi to 10.6Gi, with the
  node count roughly flat. A limit derived from one 24h reading would have
  killed maintenance-operator. gpu-operator, by contrast, moved from 3.4Gi
  to 3.0Gi.

Instantaneous readings understate peaks by about a third for
network-operator and NFD master, and by 10 to 30% for the others.

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

Nodewright is the exception, and its driver is not one thing. Its cache
holds nodes, per-node Skyhook CRs, pods, and the package-execution Jobs it
retains for `jobTtlSucceeded` (1h default) and `jobTtlFailed` (24h). Below
600 nodes, pods dominate: at 244 and 357 nodes, with 11k and 14k pods, it
peaks at the same 1Gi, and a 118-node cluster with 18k pods OOM-killed it at
2Gi. At 2000 nodes the order reverses: the Mistral cluster with 82k pods
runs both replicas at 6.7Gi, while the Lambda cluster with 13k pods runs its
leader at 10.5Gi, so retained Jobs from node churn are the better
explanation there. Neither pod count nor churn is known when a bundle is
generated or installed, which is why section 4 sizes nodewright to the
largest reading at each node count.

maintenance-operator grew about 1Gi in a week at a constant 2500 nodes, so
something besides node count drives it too
([#3267](https://github.com/NVIDIA/aicr/issues/3267) adds the scrape needed
to find out).

**Guidance elsewhere.** What exists outside AICR agrees on the shape and
differs on the headroom:

- The Kubernetes project sizes its own kube-system addons from node count
  with [addon-resizer](https://github.com/kubernetes/autoscaler/tree/master/addon-resizer):
  `base + per_node × N`, with an estimator that rounds N up to the next
  bucket of a geometric sequence (ratio 1.5) so the addon is not resized on
  every node join. That is a tier table.
  [metrics-server](https://github.com/kubernetes-sigs/metrics-server/blob/master/FAQ.md)
  and
  [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics#scaling-kube-state-metrics)
  both document linear growth in node and object count and point at it.
- Under [node-pressure eviction](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/#pod-selection-for-kubelet-eviction),
  the kubelet evicts pods whose usage exceeds their request first, ranked by
  how far over they are. A controller requesting 2Gi and using 8Gi is the
  first pod removed from a stressed system node. That is why requests, not
  only limits, must track usage.
- [GKE's guidance](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/vertical-pod-autoscaling)
  is to set the memory request equal to the limit. This ADR does not: it
  would reserve the peak on every system node, and these controllers spend
  most of their time well under it.
- The Vertical Pod Autoscaler's default margin is 15% over the 90th
  percentile of daily peaks across 8 days
  (`--recommendation-margin-fraction`). That factor assumes continuous
  re-measurement. AICR measures once per release, so it carries a wider one
  until the controllers can be scraped continuously (see Open questions).
- NodeWright publishes
  [scaling equations](https://docs.nvidia.com/nodewright/operations/resources-at-scale)
  for its own manager, validated to 1k nodes: a request of `N × 0.45Mi` and a
  limit of `N × 0.8Mi`. At 2000 nodes they give 0.9Gi and 1.6Gi; the
  measured values are 10.5Gi and 11.6Gi. A formula validated at one scale was
  5 to 7x off at another, which is the case for measuring every tier.

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

**E. A per-node formula instead of tiers.** `base + per_node × N` fits
gpu-operator and network-operator, which grow nearly linearly. It does not
fit nodewright (pod-driven) or maintenance-operator (drifts at a constant
node count), it resizes and restarts the controller on every node-count
change, and it cannot be qualified: there is no finite set of points to
measure. NodeWright's published formula (see Context) shows how far one can
be off outside its validated range. Tiers are the formula's bucketed form,
and that is also what addon-resizer's estimator does.

**F. Run the Vertical Pod Autoscaler on the controllers.** VPA would size
each cluster from its own history, which is the opposite of AICR's
contract: identical inputs give identical bundles, and every shipped value
is qualified. It is another component to deploy and keep healthy, it needs
days of metrics before it recommends anything, it applies a change by
evicting the pod (so every re-size is a controller restart, which #3148
showed is costly for gpu-operator), and it needs the same scrapes the
controllers lack today (#3265 to #3268), so it moves the measurement
problem rather than removing it. VPA's default margin is still useful as the
reference point for this ADR's 30%.

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
        limits: {memory: 768Mi}
        source: "#3214 (2026-11-04): p90 410Mi, max 496Mi, 24h at 407 GPU nodes"
      - maxNodes: 1500
        requests: {memory: 768Mi}
        limits: {memory: 1Gi}
        source: "#3214 (2026-11-04): p90 602Mi, max 744Mi, 24h at 656 GPU nodes"
      - maxNodes: 3000
        requests: {memory: 2.5Gi}
        limits: {memory: 4Gi}
        source: "#3214 (2026-11-04): p90 2353Mi, max 3006Mi, 7d at 2012-2500 GPU nodes"
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
- **Request** is the 90th percentile of the working set over the measurement
  window, at the largest cluster measured in the tier. It is what the
  controller usually needs: the pod stays under its request, and so out of
  the kubelet's first eviction group, 90% of the time.
- **Limit** is the highest peak observed in the tier, over the longest window
  available, times 1.3. The 30% covers the spikes p90 excludes plus some
  drift between measurements. It is deliberately wider than VPA's 15%
  because AICR re-measures once per release, not continuously.
- Both are rounded up: to the next 256Mi below 2Gi, to the next 512Mi
  above. Where only instantaneous readings exist, they are first scaled by
  the ratios in Context. A tier value is never set below the one in the tier
  under it.
- Peaks are read from the longest window the cluster's Prometheus keeps, 7
  days where available, and re-read before each release that changes the
  component's pin or values. For a controller known to drift at a constant
  node count (maintenance-operator today), the limit is taken from the
  drifted peak, not the first one measured.
- The loader fails closed with `ErrCodeInvalidRequest` on any of these:
  - a malformed path;
  - `maxNodes` values that do not strictly ascend;
  - a quantity that does not parse as a Kubernetes quantity;
  - a limit below its request;
  - a changing tier without a `source`;
  - a tier value below the same field in the tier under it.
- Above the top tier, the top tier applies and AICR warns that the size is
  unqualified. Raising the ceiling needs a new measured tier.

### 4. Initial tiers

Values are GPU nodes and memory request / limit, derived with the rules in
section 3 from the measurements below. Each `sizing.yaml` pins these numbers
with their sources.

| Tier | gpu-operator | network-operator | maintenance-operator | nodewright (per replica) | NIC daemon (per pod) |
|---|---|---|---|---|---|
| ≤ 300 | 300Mi / 700Mi | 2Gi / 2Gi | 256Mi / 1Gi | 2000Mi / 4000Mi | 128Mi / 512Mi |
| 301 to 600 | 512Mi / 768Mi | 2Gi / 2.5Gi | 2.5Gi / 3Gi | 2000Mi / 4000Mi | 768Mi / 1.25Gi |
| 601 to 1500 | 768Mi / 1Gi | 2Gi / 2.5Gi | 5Gi / 8Gi | 2.5Gi / 3Gi | 768Mi / 1.25Gi |
| 1501 to 3000 | 2.5Gi / 4Gi | 10.5Gi / 14Gi | 7Gi / 16Gi | 10.5Gi / 16Gi | 1Gi / 2.25Gi |

Working-set readings each tier rests on (Mi; p90 / max; window; cluster):

| Tier | gpu-operator | network-operator | maintenance-operator | nodewright leader | NIC daemon (per pod) |
|---|---|---|---|---|---|
| ≤ 300 | max 384, 24h, 244 nodes | max 728, 24h, 244 nodes | 290 at 89 nodes; 130 to 156 at 141 | max 1024, 24h, 244 nodes (11k pods) | max 400 at 89 nodes, flat from 24 |
| 301 to 600 | 410 / 496, 24h, OCI L40S 407 nodes | 955 / 1392, 24h, OCI L40S 407; max 1594, 24h, OCI GB300 447 | **none in tier**; sized from 2345 instantaneous at 1112 nodes | 1377 / 1536, 24h, OCI L40S 407 (13.8k pods); max 1868, 24h, OCI GB300 447 (18k pods) | **none in tier**; sized from p99 821 / max 903 instantaneous at 1112 |
| 601 to 1500 | 602 / 744, 24h, OCI GB300 656 nodes | 1384 / 1737, 24h, OCI GB300 656 | **none in tier**; sized from 4996 / 5462, 24h, Lambda GB300 at 2012 | 2163 / 2287, 24h, OCI GB300 656 (21.7k pods) | **none in tier**; sized from 1112-node readings as above |
| 1501 to 3000 | 2353 / 3006, 7d, Lambda GB300 2012 to 2500 nodes; 1757 / 2579, 7d, Mistral GB300 2039 | 10341 / 10619, 7d, Lambda | 7047 / 11768, 7d, Lambda (5.2Gi peak three weeks earlier) | 10555 / 11625, 7d, Lambda (13k pods); 6674 / 8295, 7d, Mistral (82k pods) | per-pod p90: median 901, q99 1069; per-pod max 1599, 7d, Lambda |

Readings marked **none in tier** are sized from the nearest reading above
the tier, so those cells are over-sized by construction. They are the first
to requalify once a cluster of that size runs the component with a scrape.

Three cells are held at the default by the never-lower rule rather than by
their own reading: network-operator's and nodewright's requests at 301 to
600, and network-operator's request at 601 to 1500, where p90 (1.4Gi) is
below the v1.0.0 request of 2Gi.

The nodewright request is per replica. On the Lambda cluster the standby
replica's p90 is 3.6Gi against the leader's 10.5Gi, so the standby
over-reserves by about 7Gi; on the Mistral cluster, with 82k pods, both
replicas sit at 6.7Gi. The top tier's requests total about 31Gi and its
limits about 52Gi on one system node; the GB300 system nodes measured have
866Gi to 993Gi allocatable.

The top tier stops at 3000 because nothing above 2500 nodes has been
measured, and because the NIC daemon's 2.25Gi runs out near 6500
NicDevices, about 3000 to 3500 GB300 nodes.

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
  - Nodewright also tracks pod count and retained Jobs, neither known in
    advance, so its tiers take the largest reading at each node count. The
    pods per node of each measured cluster is recorded in the `source`, so
    an operator with a far denser or churnier cluster knows to override.
  - maintenance-operator grows at a constant node count.
  - The NIC daemon tracks NicDevice count.

  Each gap is documented in that component's `sizing.yaml`.
- **maintenance-operator and the NIC daemon have no mid-range data.** The
  two clusters measured between 300 and 1500 nodes run neither, so their
  301 to 600 and 601 to 1500 cells are sized from larger clusters and
  over-reserve. The 601 to 1500 tier also rests on one 24h window at 656
  nodes; nothing between 656 and 2012 nodes has a scrape. Requalifying both
  is a follow-up.
- **The 30% margin is a stopgap for measuring once per release.** It is
  twice VPA's default. It comes down once the controllers can be scraped
  continuously, which needs #3265 to #3268.
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

- **Requalifying the 301 to 600 and 601 to 1500 tiers, and tightening the
  30% margin toward VPA's 15%,** once the five controllers are scraped
  continuously
  ([#3265](https://github.com/NVIDIA/aicr/issues/3265) to
  [#3269](https://github.com/NVIDIA/aicr/issues/3269)). Until then, drift
  is found at the next measurement or the next OOM kill.
- **Reporting the measured NodeWright numbers upstream.** Its published
  equations undersize the manager by 5 to 7x at 2000 nodes.
- **Nodewright inputs that are known at bundle time.** AICR knows how many
  packages a recipe ships, which NodeWright's own formula multiplies by, and
  it can set `jobTtlSucceeded` and `jobTtlFailed` lower in the large tiers,
  which shrinks the retained-Job cache instead of sizing for it. Either is a
  refinement of the nodewright cells under this ADR, not a new input to tier
  selection.
- **Install-time sizing for the other deployers.** For `helm` and
  `helmfile`, `install.sh` could read an `AICR_NODE_COUNT` environment
  variable and layer per-tier values files. `flux`, `fleet` and `terraform`
  are not yet designed.
- **Passing a count from the AICR Operator**
  ([ADR-027](027-aicr-operator.md)) through to the bundle step it runs.
