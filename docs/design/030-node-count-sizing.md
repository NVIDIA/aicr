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
(deploy time).

## Decision Summary

A cluster that states no node count gets today's defaults. A user who states a GPU node count, with `aicr
bundle --nodes` at generation time or `sizing.nodeCount` when an
`argocd-helm` bundle is installed, can get lower memory values for the
controllers whose memory grows with cluster size. Each component declares
the lower values in `recipes/components/<name>/sizing.yaml` as tiers, each
backed by a recorded measurement. Sizing only ever lowers a value, never
touches a value the recipe's inline overrides or the user set, and changes
nothing for a count above 2000 GPU nodes.

## Problem

Five controllers in AICR's stack hold cluster-wide informer caches, so their
working set grows with cluster size: gpu-operator, network-operator,
maintenance-operator, the nodewright manager, and the GB300 NIC
configuration daemon. The NIC daemon runs on every node, and each copy
caches every NicDevice in the cluster
([#3149](https://github.com/NVIDIA/aicr/issues/3149)).

Today's defaults are sized for the largest clusters: 8Gi to 12Gi limits
for the four controllers and 2Gi for the NIC daemon, which hold at 2500 GPU
nodes. Smaller values OOM-killed all five on two GB300 clusters at about
1600 and 2500 GPU nodes
([#3148](https://github.com/NVIDIA/aicr/issues/3148)). For every other
cluster these values cost something:

- **Limits far above what the cluster can use.** With both nodewright
  replicas on one system node, the controllers' limits add up to 56Gi on a
  20-node cluster whose controllers use under 1Gi. A controller that leaks
  or misbehaves can take memory far past what its cluster size explains
  before the kernel stops it, and the node's other pods pay through
  node-pressure eviction. The NIC daemon's 2Gi is multiplied by every node.
- **Requests above usage.** network-operator requests 2Gi. At 250 GPU
  nodes and below its p90 working set is under 900Mi, so the scheduler
  reserves memory the controller does not use.

Lowering these values for everyone would OOM-kill the largest clusters'
controllers (#3148), and changing them
for a large cluster re-rolls its controllers, which on a 2000-node cluster
takes hours. A cluster's size is the input that tells the two cases apart,
and AICR has no way to turn a node count into a resource value: `aicr
bundle --nodes N` copies N verbatim into the paths in
`nodeScheduling.nodeCountPaths`, and only nodewright's `estimatedNodeCount`
is listed there. The count is also often unknown when the bundle is
generated: large clusters install a published OCI bundle through
`argocd-helm`.

## Goals

- Lower controller memory requests and limits from a GPU node count the
  user states, at generation time and at deploy time.
- Change nothing for a cluster that states no count or more than 2000 GPU
  nodes: its rendered values and pod specs stay as today's. For every
  deployer except `argocd-helm` the bundle stays byte-identical; an
  `argocd-helm` bundle always carries the sizing machinery (see Deploy
  time), which selects nothing at such a count.
- Back every lowered value with a measurement recorded next to it, taken at
  or near the top of its tier.
- Keep outputs reproducible: the same inputs give byte-identical bundles.
- Keep recipe inline overrides and user overrides authoritative.
- Make it obvious to the next contributor, human or agent, how a tier is
  refined when better data arrives.

## Non-Goals

- **Raising any value above today's defaults,** including the requests on
  large clusters, which sit below usage. Raising them re-rolls the
  largest clusters; see Open questions.
- **Deriving a count AICR was not given.** The epic requires that node count
  alone never silently specializes a configuration, so a snapshot-derived
  `criteria.nodes` sizes nothing.
- **CPU, replica counts and client QPS.** These belong to
  [#2430](https://github.com/NVIDIA/aicr/issues/2430).
- **Inputs other than GPU node count**, such as CPU nodes, pod count,
  NicDevice count or node size (see Consequences).
- **Other components.** NFD master and NVSentinel are measured but not in
  scope. Adding them is a data change under this ADR.
- **Fixing the growth itself.** The NIC daemon's unscoped cache is #3149.
  gpu-operator's per-node cost is a question raised with its team in #3214.

## Context

Node counts in this ADR are GPU nodes, the meaning `--nodes` already has,
unless they say total nodes.

### What the measurements show

Readings cover clusters of 24 to about 2500 GPU nodes, recorded in #3148
and #3214; the 2026-11-04 comment on #3214 has the method and the full
tables. Two kinds of reading back the tiers:

- **Windowed:** cadvisor working-set series
  (`container_memory_working_set_bytes`) from a Prometheus, read as p50,
  p90 and max over 24 hours or 7 days, or as max only where p90 was not
  kept.
- **Instantaneous:** one `kubectl top` or metrics-server sample. It is a
  floor, not a peak, and the rules below scale it accordingly.

| Cluster | GPU nodes (total) | Pods | Kind | Used for |
|---|---|---|---|---|
| Fleet sweep, 74 clusters | 1 to 576 | up to 19k | instantaneous | checks below 700 |
| exmplr, bare-metal GB300 | 159 (165) | 5.7k | instantaneous; windowed from 2026-11-05 | ≤ 250 checks |
| jbp, OCI GB300 | 244 (249) | 11k | 24h max | ≤ 250 |
| OCI L40S | 407 (417) | 13.8k | 24h | ≤ 450 |
| OCI GB300 | 447 (453) | 18k | 24h max | ≤ 450 |
| OCI GB300 | 656 (670) | 21.7k | 24h | ≤ 700 |
| bare-metal GB300 | 1112 | | instantaneous | maintenance-operator, NIC daemon |
| Bare-metal GB300 A | 2039 (2143) | 82k | 7d | top tier evidence |
| Bare-metal GB300 B | 2012 to 2500 (2191 to 2653) | 13.2k | 7d | top tier evidence |

Four properties of the series shape the rules:

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
  roughly flat.
- **Some readings were taken while the container was being OOM-killed.**
  nodewright at 118 GPU nodes (1859Mi), 408 (1288Mi) and 447 (1868Mi), and
  network-operator at 576 (75 restarts, no series), all at a 2Gi limit.
  Those numbers are what the container reached before the kill, not what
  it needed, so they cannot set a limit.

### What drives the growth

gpu-operator and network-operator grow nearly linearly, at about 1.2 to
1.4Mi and 3.2 to 3.6Mi per GPU node. maintenance-operator also grows at a
constant node count, for reasons not yet known
([#3267](https://github.com/NVIDIA/aicr/issues/3267) adds the scrape to find
out).

Each controller caches every Node, so CPU nodes cost memory too, but on the
GB300 clusters measured they are 2 to 9% of the total. Nodewright is the
exception that shows the limits of a GPU count. Its cache holds every Node,
per-node Skyhook CRs, pods, and the package-execution Jobs it retains for
`jobTtlSucceeded` (1h default) and `jobTtlFailed` (24h). A cluster with 118
GPU nodes, 436 nodes in total and 18k pods killed its manager at a 2Gi
limit, as did a 447-GPU-node cluster with the same pod count. The tiers
below size nodewright so that both are covered, which keeps its request at
today's value in every tier.

### What AICR already has

- `aicr bundle --nodes`, documented as an estimated GPU node count, with
  config-file, REST (`nodes`) and SDK (`BundleOptions.Nodes`) equivalents.
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

Today's defaults. Correct for the largest clusters; every
smaller cluster carries their limits and, for two controllers, their
requests.

### B. A scale criteria axis or overlay per size band

Multiplies the recipe matrix by the number of bands, selects a
configuration by size, which #2427 rules out unless the user chooses it,
and cannot apply at install time.

### C. Documented overrides only

Users compute their own values with `--set` or install-time overrides. Each
cluster rediscovers the numbers.

### D. Measured tiers below today's defaults, selected by a stated count

Today's defaults are the top tier. A per-component table lists lower tiers,
each measured, keyed on a GPU node count the user states at generation or
deploy time. The rest of this ADR describes it.

### E. A per-node formula

`base + per_node × N` fits gpu-operator and network-operator, but not
nodewright (pods, Jobs, CPU nodes) or maintenance-operator (drift at a
constant count). It resizes and restarts the controller on every node-count
change, and it cannot be qualified, because there is no finite set of points
to measure.

A formula validated at one scale can miss badly at another. NodeWright
publishes
[scaling equations](https://docs.nvidia.com/nodewright/operations/resources-at-scale)
for its manager, validated to 1k nodes: a request of `N × 0.45Mi` and a
limit of `N × 0.8Mi`. At 2000 nodes they give 0.9Gi and 1.6Gi against a
measured 10.5Gi and 11.6Gi, about 12x and 7x off.

Tiers are the formula's bucketed form, as in the Kubernetes project's
[addon-resizer](https://github.com/kubernetes/autoscaler/tree/master/addon-resizer),
which rounds N up to a bucket so an addon is not resized on every node join.

### F. The Vertical Pod Autoscaler on the controllers

VPA sizes each cluster from its own history, the opposite of AICR's
contract that identical inputs give identical, qualified bundles. It is
another component to run, needs days of metrics before it recommends
anything, applies a change by evicting the pod (a controller restart,
which #3148 showed is costly for gpu-operator), and needs the same scrapes
the controllers lack today (#3265 to #3269).

### Comparison

| | A. Static | B. Criteria axis | C. Overrides only | D. Tiers | E. Formula | F. VPA |
|---|---|---|---|---|---|---|
| Explicit intent, no silent specialization | yes | yes | yes | yes | yes | no |
| No change for a cluster that states no count | yes | yes | yes | yes | no | no |
| Sizes a published bundle at install time | no | no | yes | yes | yes | yes |
| Every shipped value measured and reproducible | yes | yes | no | yes | no | no |
| Small clusters pay only for their size | no | yes | yes | yes | yes | yes |
| Resizes only at a few known boundaries | n/a | yes | n/a | yes | no | no |
| No new recipe-matrix dimension | yes | no | yes | yes | yes | yes |
| No new in-cluster component | yes | yes | yes | yes | yes | no |

## Decision

Adopt option D. Today's defaults are the top tier and apply when no count is
stated or the count is above 2000 GPU nodes. Each controller whose memory
grows with cluster size may declare lower tiers in
`recipes/components/<name>/sizing.yaml`, referenced from the registry. Only
a count the user states selects one. A count walks down from the top: it
selects the lowest tier whose bound is at or above it.

A tier only lowers values. It never sets a value above the tier above it,
never raises a request, never touches a value the recipe's inline overrides
or the user set, and is only as low as a reading at or near its bound
supports.

## Proposal

### The top tier: today's defaults

| Controller | Values path | Values files that set it | Request / limit |
|---|---|---|---|
| gpu-operator | `operator.resources` | `values.yaml` | 300Mi / 8Gi |
| network-operator | `operator.resources` | `values-generic-gb300.yaml` | 2Gi / 12Gi |
| | | `values-nvke-gb200.yaml`, `values-oke-gb200.yaml`, `values-oke-l40s.yaml` | 2Gi / 2Gi |
| maintenance-operator | `maintenance-operator-chart.operator.resources` | `values-generic-gb300.yaml` | 256Mi / 12Gi |
| nodewright manager | `controllerManager.manager.resources` | `values.yaml` | 2000Mi / 12Gi |
| NIC configuration daemon | `nicConfigurationOperator.configurationDaemon.resources` | `values-generic-gb300.yaml` | 256Mi / 2Gi |

These are the current defaults. A bundle generated
without `--nodes`, or with a count above 2000, and an `argocd-helm` bundle
installed with `sizing.nodeCount` 0 or above 2000, get exactly these.

### The node count

The count comes from `aicr bundle --nodes N` (or its config-file, REST and
SDK equivalents) or from `sizing.nodeCount` in an `argocd-helm` bundle's
root-chart values. `criteria.nodes` and snapshot-derived counts never select
a tier.

The count is GPU nodes, the meaning `--nodes` and the REST `nodes`
parameter already document, so this ADR changes no frozen v1 surface. GPU
nodes is also what an operator plans a cluster in, and what the recipe
endpoint's `nodes` means. CPU nodes cost memory too (see Context). On the
GB300 clusters with windowed readings they are 2 to 9% of the total, but
that share does not hold in general: the 118-GPU-node cluster in Context
has 436 nodes in total. The 1.3 margin is therefore not relied on to cover
CPU nodes. Instead, a tier's limit covers clusters up to the total node
count recorded (`nodes`) by the readings it was derived from. Nodewright's
≤ 250 and ≤ 450 limits, for example, come from a reading at 670 total
nodes, so a ≤ 250 cluster with 925 total nodes is outside what they cover.
A cluster whose total node count exceeds that of its tier's limit readings
should state a higher count or override. A separate CPU count is an open
question, and the sizing file's `basis` field leaves room for it.

`--nodes` keeps feeding `nodeScheduling.nodeCountPaths`. Every input takes a
non-negative integer; 0 means no count. A negative count is rejected with
`ErrCodeInvalidRequest` on the CLI, config-file, REST and SDK paths. On
`argocd-helm` the schema declares it an integer of at least 0, and because
schema validation can be skipped, the template also fails on a negative or
fractional `sizing.nodeCount` rather than truncating it into a lower tier.

### The sizing file

`recipes/components/<name>/sizing.yaml`, referenced from the registry
entry's `sizing.file`. The nodewright file, which shows every kind of
measurement:

```yaml
apiVersion: aicr.run/v1beta1
kind: ComponentSizing
basis: gpuNodes
resources:
  - path: controllerManager.manager.resources
    tiers:
      - maxNodes: 250
        requests: {memory: 2000Mi}
        limits: {memory: 3Gi}
        measurements:
          - sets: []
            kind: windowed
            window: 24h
            gpuNodes: 244
            nodes: 249
            pods: 11029
            max: 1024Mi
            date: 2026-10-14
            ref: https://github.com/NVIDIA/aicr/issues/3214
          - sets: [requests]
            kind: instantaneous
            gpuNodes: 118
            nodes: 436
            pods: 18156
            max: 1859Mi
            truncatedAt: 2Gi
            date: 2026-10-13
            ref: https://github.com/NVIDIA/aicr/issues/3214
          - sets: [limits]
            kind: windowed
            window: 24h
            gpuNodes: 656
            nodes: 670
            pods: 21700
            p90: 2163Mi
            max: 2287Mi
            date: 2026-11-04
            ref: https://github.com/NVIDIA/aicr/issues/3214
      - maxNodes: 450
        requests: {memory: 2000Mi}
        limits: {memory: 3Gi}
        measurements: [...]
      - maxNodes: 700
        requests: {memory: 2000Mi}
        limits: {memory: 3Gi}
        measurements: [...]
      - maxNodes: 2000
```

**Fields:**

- `apiVersion` and `kind`: the artifact header, as for ADR-021's
  `ComponentUpgrades`.
- `basis`: the count the table is keyed on. Only `gpuNodes` is defined.
- `path`: the values path of a resources block, as `--set` writes it.
  Sizing reads and writes the block's `requests.memory` and
  `limits.memory` as separate fields, and every exclusion below (inline
  overrides, lock sets, user overrides) applies per field. The
  entry applies to every values file that sets the path and is skipped
  where none does. The maintenance-operator entry therefore reaches only
  the generic GB300 values file today: NVKE GB200 declares
  `maintenance-operator-chart` but sets no resources there.
- `maxNodes`: the tier's upper bound, which also names the tier. A tier
  covers counts above the next lower tier's `maxNodes`, up to its own.
  Counts above the highest `maxNodes` get the top tier.
- `requests` and `limits`: memory quantities. A tier without them changes
  nothing; it marks a bound whose values are not yet measured.
- `measurements`: required on every tier that sets a value. Each entry
  records one reading:
  - `sets`: which of `requests` and `limits` it set, or empty for a reading
    that only constrained the result (one that a higher reading outweighed,
    or a truncated reading that sent the limit to a reading above);
  - `kind`: `windowed` or `instantaneous`, and `window` for a windowed one;
  - `gpuNodes`, `nodes` and `pods`: the measured cluster at read time;
  - `p90` where the reading kept it, and `max`;
  - `truncatedAt`: the limit in force, when the container was being
    OOM-killed;
  - `date` and `ref`: when it was read, and the issue comment holding the
    raw data.

The structured block is what makes a tier refinable: anyone reading the
file can see that a cell rests on an instantaneous reading, a reading from
a cluster well below the bound, or a reading from above the tier, and
replace it.

### Deriving values

- **Covering reading.** A tier is sized from a reading within 10% of its
  bound, above or below. The 1.3 limit margin absorbs that gap: a 10%
  shortfall at the measured slopes adds under 10% to the working set.
  Windowed readings with a p90 are preferred, then windowed readings with a
  max only, then instantaneous ones. If no reading is that close, the
  nearest reading above the bound covers the tier, which over-sizes it.
- **Request:** the highest p90 among the covering reading and every
  reading inside the tier, using a reading's max when p90 was not kept and
  a truncated reading's value as it stands. The pod then stays under its
  request, and out of the kubelet's first eviction group, about 90% of the
  time. Not the limit, as GKE [recommends](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/vertical-pod-autoscaling):
  that would reserve the peak on every system node.
- **Limit:** the highest peak among the covering reading and every reading
  inside the tier, times 1.3. The 30% covers spikes above p90 and some drift
  between measurements; see Open questions for tightening it.
- **Truncated readings.** If any reading inside the tier was taken while the
  container was being OOM-killed, the limit comes instead from the nearest
  reading above the bound whose peak exceeds the limit that killed it.
- **Instantaneous readings:** the reading stands in for p90, and reading ×
  1.7 for the peak, so it contributes reading × 1.7 × 1.3 to a limit. 1.7 is
  the largest max-to-p90 ratio measured on any controller.
- **DaemonSets** (the NIC daemon): the request uses the median across pods
  of each pod's p90, and the limit uses the largest single-pod peak.
- **Rounding:** round up to a multiple of 256Mi below 2Gi, and of 512Mi
  from 2Gi.
- **Caps:** no value is above the same field in the tier above, and no
  request is above the top tier's. A tier may equal the tier above; every
  component's file keeps the same bounds, so one count selects the same
  row in each.
- **Re-measurement:** the readings are re-read, from the longest window
  kept, before each release that changes the component's pin or values. For
  a controller that drifts at a constant node count, a limit derived from
  that cluster's readings uses the drifted peak, whichever tier it sets.

**Validation.** The loader fails closed with `ErrCodeInvalidRequest` on:

- a malformed path;
- a `basis` other than `gpuNodes`;
- `maxNodes` values that do not strictly ascend;
- a quantity that does not parse;
- a limit below its request;
- a tier value above the same field in the next higher tier;
- a tier that sets `requests` or `limits` with no measurement whose `sets`
  names it;
- a measurement missing `kind`, `gpuNodes`, `max`, `date` or `ref`, or a
  windowed one missing `window`.

The loader cannot reject a well-formed path that no values file sets,
because an entry is skipped by design where a file does not set it. A
unit test fails instead when a sizing path is set by none of the
component's values files (registry, overlay or mixin), so a typo or a key
renamed by a chart bump cannot turn sizing into a silent no-op.

The contributor guide (`docs/contributor/component-sizing.md`, added by the
implementation PR) holds these rules, the Prometheus queries behind each
kind of reading, and a "Refining a tier" walkthrough. A line in `AGENTS.md`
points to it from the component-values task. No unit test asserts a real
component's tier values, the same rule as for upgrade records.

### Initial tiers

Request / limit; "today's" means the tier changes nothing for that
controller.

| Tier (GPU nodes) | gpu-operator | network-operator | maintenance-operator | nodewright (per replica) | NIC daemon (per pod) |
|---|---|---|---|---|---|
| > 2000, or no count | 300Mi / 8Gi | 2Gi / 12Gi | 256Mi / 12Gi | 2000Mi / 12Gi | 256Mi / 2Gi |
| ≤ 2000 | today's | today's | today's | today's | today's |
| ≤ 700 | 300Mi / 1Gi | 1.5Gi / 12Gi | 256Mi / 5.5Gi | 2000Mi / 3Gi | today's |
| ≤ 450 | 300Mi / 1Gi | 1.5Gi / 2.5Gi | 256Mi / 5.5Gi | 2000Mi / 3Gi | today's |
| ≤ 250 | 300Mi / 768Mi | 1Gi / 2Gi | 256Mi / 5.5Gi | 2000Mi / 3Gi | today's |

network-operator's values apply to the generic GB300 values file as shown.
The three files that set 2Gi / 2Gi keep the lower of their value and the
tier's (see Precedence).

The reading behind each cell, in Mi as p90 / peak, 24h unless marked.
"Capped" means the derived value was above the tier above's, or a request
above today's, and the cap applied:

| Tier | gpu-operator | network-operator | maintenance-operator | nodewright |
|---|---|---|---|---|
| ≤ 700 | request capped; limit 744 at 656 | request 1384 at 656; limit from 10.6Gi drifted peak, 7d at 2012, capped † | 2345 instantaneous at 1112 ‡ | request 2163 at 656, capped; limit 2287 at 656 |
| ≤ 450 | request capped; limit 635 at 447 | request 1594 at 447, capped; limit 1594 at 447 | as ≤ 700 ‡ | request 1868 truncated at 447, capped; limit 2287 at 656 † |
| ≤ 250 | request capped; limit 319 instantaneous at 141 | request and limit 846 instantaneous at 159 | as ≤ 700 ‡ | request 1859 truncated at 118, capped; limit 2287 at 656 † |

† A reading inside the tier was truncated: network-operator crash-looped at
2Gi at 576; nodewright was OOM-killed at 2Gi at 118, 408 and 447. The limit
comes from the nearest reading above that exceeds 2Gi. For
network-operator that is bare-metal GB300 B, whose peak drifted to 10.6Gi
at a roughly constant count, so the drift rule applies and ≤ 700 keeps
today's 12Gi limit.

‡ No reading within 10% of the bound; sized from the nearest reading above.

Notes on the table:

- **≤ 2000 keeps today's values.** Nothing between 657 and 2011 GPU nodes
  has a scrape. Both bare-metal GB300 clusters are within 10% of 2000, but
  their 7-day windows straddle a period at about 2500 nodes; see Open
  questions.
- **The NIC daemon is not lowered.** Its only reading at or above 250 GPU
  nodes, short of the 2000-node windowed one, is an instantaneous one at
  1112 that already reaches today's 2Gi limit with the 1.7 × 1.3 factor.
  The registry entry has no NIC daemon resource until a lower tier is
  measured.
- **maintenance-operator rests on one instantaneous reading** and is the
  first cell to refine. exmplr (159 GPU nodes) has shipped its
  maintenance-operator and NIC daemon series to the fleet Prometheus since
  2026-11-05; its first windowed readings are 597 / 602 and 366 / 680 per
  pod. It covers the ≤ 250 tier if it grows to its planned 256-node test.
- **Requests follow the highest reading in the tier.** gpu-operator's and
  nodewright's requests therefore stay at today's values in every tier:
  nodewright's in-tier readings were taken while it was being OOM-killed
  at 1.8Gi. network-operator's request is the only one lowered.
- **Nodewright replicas.** Requests and limits are per replica. AICR does
  not keep the two replicas on separate nodes.
- **Per-node totals.** In the ≤ 250 tier, the controllers' limits on one
  system node with both nodewright replicas total about 14Gi, against 56Gi
  today.

### Precedence

Sizing runs on a component's values after its values files resolve: the
registry's values file and the overlay's `valuesFile`. For each memory
request and limit that a sizing entry names and those files set, it writes
the smaller of the resolved value and the tier's value, compared as
Kubernetes quantities. It skips a path the files do not set, and an
unparseable resolved value fails the bundle. Because both the resolved
values and the tier have request ≤ limit, the result does too.

Sizing never writes:

- a path set by a recipe's inline `overrides` in an overlay or mixin;
- a path in an ADR-015 profile lock set, where ADR-015's rules apply
  unchanged;
- a user override. `--set`, `--set-json` and `--set-file` at generation
  time, and the component's root-chart values at install time, are applied
  after sizing and replace its result.

After every override is applied, the bundler fails closed with
`ErrCodeInvalidRequest` if a sized path's request is above its limit. On
`argocd-helm` it checks every emitted row merged over the component's
static values, which already carry the generation-time overrides, so a
`--set` request that a row's limit would undercut fails the bundle rather
than the sync. Only an install-time root-chart value can still produce
one, because the template never compares quantities; the API server then
rejects the pod spec and Argo CD reports the sync error. The documentation
states this limitation.

### Generation time (`--nodes`)

The sizing pass runs once per component, after values files resolve and
before inline and user overrides. For every deployer except `argocd-helm`
the result is baked into the component's values, including any
bundle-rendered manifest that reads a sized path.

`bundle-info.yaml` does not record the tier. Its reader rejects unknown
fields, so a new field needs a release that reads it before one that writes
it (ADR-022 §3), and nothing shows its `warnings` to users. The sized
values in each component's `values.yaml` are the record.

### Deploy time (`argocd-helm`)

The root-chart values are the `values.yaml` of the generated `argocd-helm`
chart, which the operator sets at install. Sizing is decided per path:

- **Chart-values paths of remote-chart components** are sized at install
  time. The bundler writes `static/<comp>-sizing.yaml` with one row per
  tier: for each sized path, the smaller of the unsized static value and the
  tier's value. Paths set by generation-time `--set`, `--set-json` or
  `--set-file`, by inline overrides, or in a lock set are left out of every
  row, so those values survive. The template only selects a row; it never
  compares quantities.
- **Paths read by bundle-rendered manifests** cannot see root-chart values,
  so they are sized at generation time from `--nodes`. A component with
  any such path is sized entirely at generation time and gets no rows, so
  one count sizes all of it. No path in the initial tiers is one; the NIC
  daemon's would be.
- **Local-chart components** are sized at generation time.

Rendering:

- The root-chart values gain `sizing: {nodeCount: N}`, where N is the
  bundle's `--nodes` or 0. `values.schema.json` declares it as an integer
  of at least 0. An install that does not set `sizing.nodeCount` therefore
  inherits the bundle's count, so a bundle published for clusters of
  different sizes should be generated without `--nodes`.
- The child Application template merges the component's static values
  (`$static`), then the selected row, then the component's root-chart
  values, so user values still win. A count of 0 or above the highest
  `maxNodes` selects nothing.
- The selector converts both `sizing.nodeCount` and each row's `maxNodes`
  to `int64` before comparing. A count arrives as `int64` from `--set` and
  as `float64` from a values file, and Helm's comparison functions fail on
  mixed types; tests cover both sources.
- The template stamps `aicr.nvidia.com/sizing-node-count` on each sized
  child Application, and `aicr.nvidia.com/sizing-tier` (the row's
  `maxNodes`) only on one whose values a row changed.
- A `--dynamic` export of a sized path is rejected at bundle time. Its
  pre-filled root value would otherwise always mask the selected row.

Each component is sized from exactly one count: on an `argocd-helm`
bundle, `--nodes` sets the default `sizing.nodeCount` and sizes the
components sized at generation time, and `sizing.nodeCount` sizes the
rest. Other deployers have no install-time sizing yet; they can add it
by following this section.

### For operators

- **Which count.** GPU nodes, as `kubectl get nodes -l
  nvidia.com/gpu.present=true` reports, at the size the cluster will reach
  before its next re-bundle or `sizing.nodeCount` change. A count too high
  only gives back today's values; one too low risks OOM kills.
- **No count is safe.** A bundle generated without `--nodes` and installed
  without `sizing.nodeCount` keeps today's values. An `argocd-helm` bundle
  generated with `--nodes` carries that count as the default for
  `sizing.nodeCount`: set it at install, or generate without `--nodes`,
  when the target cluster's size differs.
- **Changing it.** Moving the count across a tier boundary re-renders the
  sized controllers and restarts them. Within a tier the sized values do
  not change; on `argocd-helm` the `sizing-node-count` annotation does.
- **Checking it.** The applied values are in each component's
  `values.yaml`; on `argocd-helm` the child Applications' sizing
  annotations show the count and the row.
- **Large CPU pools.** Each measurement records the cluster's total nodes.
  A cluster with more total nodes than the readings behind its tier (670
  for nodewright's ≤ 250 and ≤ 450 limits) should state a higher count or
  override.

## Consequences

**Positive:**

- Nothing changes for a cluster that states no count or more than 2000 GPU
  nodes, so no large cluster re-rolls.
- Small and mid-sized clusters that state a count get limits sized to their
  measurements, and a lower request for network-operator.
- A published `argocd-helm` bundle is sized with one value instead of a
  hand-copied block per controller.
- Every lowered value points to its measurements in a structured form, and
  bundles are byte-stable for the same inputs.

**Negative:**

- **The count is the user's responsibility.** A stale count after a
  scale-up under-sizes the cluster until the bundle is regenerated or
  `sizing.nodeCount` changes. Each tier is sized at its bound, so a cluster
  that grows within its tier stays covered.
- **GPU nodes is an imperfect proxy.** CPU nodes, pods and Jobs retained
  from churn (nodewright), drift at a constant count (maintenance-operator)
  and NicDevices (the NIC daemon) are not counted. Each measurement records
  total nodes and pods, so an operator with a far denser cluster knows to
  override.
- **Thin mid-range data.** Nothing between 657 and 2011 GPU nodes has a
  scrape, so ≤ 2000 keeps today's values. maintenance-operator's cells rest
  on one instantaneous reading, and the NIC daemon is not lowered at all.
- **Requests on large clusters stay below usage,** as today.
- **The 30% margin is a stopgap** (see Open questions).

## Adoption plan

1. This ADR.
2. One implementation PR, closing
   [#3643](https://github.com/NVIDIA/aicr/issues/3643) and
   [#3644](https://github.com/NVIDIA/aicr/issues/3644):
   - the `sizing.yaml` schema, loader and data files; no default changes;
   - the generation-time sizing pass, the post-override request ≤ limit
     check, and the SDK's negative-count check;
   - the `argocd-helm` root value, per-tier static files, template
     selection, annotations and `--dynamic` guard;
   - the user docs, `docs/contributor/component-sizing.md` and its
     `AGENTS.md` pointer.

## Open questions

- **Sizing ≤ 2000** from the bare-metal GB300 clusters, once a 7-day window
  sits entirely at about 2000 GPU nodes, or from any cluster scraped between
  700 and 2000.
- **Refining maintenance-operator and the NIC daemon** from exmplr's
  windowed series, at 24 hours and 7 days, and at 256 nodes if it grows.
- **Tightening the 30% margin** once the five controllers are scraped
  continuously ([#3265](https://github.com/NVIDIA/aicr/issues/3265) to
  [#3269](https://github.com/NVIDIA/aicr/issues/3269)). VPA's default is
  15% over the p90 of daily peaks, but it re-measures continuously; AICR
  measures once per release.
- **Raising requests on large clusters** to match usage, weighed against the
  re-roll it forces on every one of them.
- **A separate CPU-node count,** if a cluster with a large CPU pool lands in
  the wrong tier.
- **Nodewright inputs known at bundle time.** AICR knows how many packages a
  recipe ships, which NodeWright's own formula multiplies by, and can set
  `jobTtlSucceeded` and `jobTtlFailed` lower to shrink the retained-Job
  cache. Either refines the nodewright cells; neither adds an input to tier
  selection.
- **Reporting the measured NodeWright numbers upstream** (see Option E).
- **Install-time sizing for the other deployers.** For `helm` and
  `helmfile`, `install.sh` could read an `AICR_NODE_COUNT` environment
  variable and layer per-tier values files. `flux`, `fleet` and `terraform`
  are not yet designed.
- **Passing a count from the AICR Operator**
  ([ADR-027](027-aicr-operator.md)) to the bundle step it runs.
