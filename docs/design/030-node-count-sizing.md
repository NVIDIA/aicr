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
nothing for a count above a component's highest tier. The tiers and their
measurements live in the sizing files, not in this ADR: refining a value
from new readings, or after a chart bump, is a data change under the rules
below.

## Problem

Several controllers in AICR's stack hold cluster-wide informer caches, so
their working set grows with cluster size: gpu-operator, network-operator,
maintenance-operator and the nodewright manager among them.

Today's defaults are sized for the largest clusters: 8Gi to 12Gi limits,
which hold at 2500 GPU nodes. Smaller values OOM-killed these controllers
on two GB300 clusters at about
1600 and 2500 GPU nodes
([#3148](https://github.com/NVIDIA/aicr/issues/3148)). For every other
cluster these values cost something:

- **Limits far above what the cluster can use.** With both nodewright
  replicas on one system node, the controllers' limits add up to 56Gi on a
  20-node cluster whose controllers use under 1Gi. A controller that leaks
  or misbehaves can take memory far past what its cluster size explains
  before the kernel stops it, and the node's other pods pay through
  node-pressure eviction.
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
- Change nothing for a cluster that states no count or a count above the
  highest tier: its rendered values and pod specs stay as today's. For every
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
- **Fixing the growth itself.** gpu-operator's per-node cost is a question
  raised with its team in #3214.

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

Four properties of the series shape the rules:

- **p90 sits within 5 to 10% of p50** for every controller. These are flat
  caches with occasional spikes, so p90 is a good "usually needed" value
  that reserves little more than the average.
- **Peaks can sit well above p90.** The 7-day maximum is 1.1x p90 for
  network-operator and nodewright, 1.3x for gpu-operator and 1.7x for
  maintenance-operator.
- **Some controllers drift at a constant node count.** On a bare-metal
  GB300 cluster, maintenance-operator's peak went from 5.2Gi to 11.8Gi in three
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
limit, as did a 447-GPU-node cluster with the same pod count: for such a
controller a GPU count alone does not bound memory.

### What AICR already has

- `aicr bundle --nodes`, documented as an estimated GPU node count, with
  config-file, REST (`nodes`) and SDK (`BundleOptions.Nodes`) equivalents.
- [ADR-021](021-component-upgrade-safety.md)'s pattern of per-component data
  in a registry-referenced file, validated fail-closed at load.
- [ADR-015](015-recipe-configuration-profiles.md)'s profile locks, which
  cover every mutation source including node-count injection, with a
  template-time guard on `argocd-helm` root-chart values
  (`templates/aicr-profile-lock.yaml`), the precedent for install-time
  template logic in a generated chart.
- The `argocd-helm` child Application template, which already merges a
  component's static values with its root-chart values at render time.

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
stated or the count is above a component's highest tier. Each controller whose memory
grows with cluster size may declare lower tiers in
`recipes/components/<name>/sizing.yaml`, referenced from the registry. Only
a count the user states selects one. A count walks down from the top: it
selects the lowest tier whose bound is at or above it.

A tier only lowers values. It is never below the tier under it, never
raises a request, never touches a value the recipe's inline overrides
or the user set, and is only as low as a reading at or near its bound
supports.

## Proposal

### The top tier: today's defaults

The values files AICR ships are the top tier, and sizing never edits
them. A bundle generated without `--nodes`, or with a count above a
component's highest tier, and an `argocd-helm` bundle installed with
`sizing.nodeCount` 0 or above that tier, get exactly the values files'
values.

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
count recorded (`nodes`) by the readings it was derived from: a limit
derived from a reading at 670 total nodes does not cover a cluster with 925
total nodes in the same tier.
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
entry's `sizing.file`. An illustrative file showing every kind of
measurement. Its component and values are examples only, for a path
whose values file sets 2000Mi / 12Gi; the shipped tiers are in the files
themselves:

```yaml
apiVersion: aicr.run/v1beta1
kind: ComponentSizing
basis: gpuNodes
resources:
  - path: controller.resources
    tiers:
      - maxNodes: 250
        # No requests: the requirement (2163Mi) is above today's 2000Mi.
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
          - sets: []
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
        limits: {memory: 3Gi}
        measurements: [...]
      - maxNodes: 700
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
  where none does.
- `maxNodes`: the tier's upper bound, which also names the tier. A tier
  covers counts above the next lower tier's `maxNodes`, up to its own.
  Counts above the highest `maxNodes` get the top tier.
- `requests` and `limits`: memory quantities. A tier without them changes
  nothing; it marks a bound whose values are not yet measured.
- `measurements`: required on every tier that sets a value. Each entry
  records one reading:
  - `sets`: which of `requests` and `limits` it set, or empty for a reading
    that only constrained the result (one that a higher reading outweighed,
    or a truncated reading that brought in a reading from above);
  - `kind`: `windowed` or `instantaneous`, and `window` for a windowed one;
  - `gpuNodes`, `nodes` and `pods`: the measured cluster at read time.
    `nodes` is required on a windowed reading that sets a limit, because
    the limit covers clusters only up to that total (see The node count).
    `pods` is optional: the controllers that track pods do so through
    caches being scoped upstream
    ([Mellanox/network-operator#3173](https://github.com/Mellanox/network-operator/issues/3173),
    [Mellanox/maintenance-operator#275](https://github.com/Mellanox/maintenance-operator/issues/275));
  - `version`, optional: the component's chart version on the measured
    cluster, where known, so a reading that predates a pin shows;
  - `p90` where the reading kept it, and `max`;
  - `truncatedAt`: the limit in force, when the container was being
    OOM-killed;
  - `date`: when it was read;
  - `ref`, optional: a link to the raw data.

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
  a truncated reading's value as it stands. Where a windowed p90 sets the
  request, the pod stays under its request, and out of the kubelet's
  first eviction group, about 90% of the time. A request taken from an
  instantaneous or truncated reading, or left at today's value because
  its requirement is higher, carries no such figure. Not the limit, as GKE [recommends](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/vertical-pod-autoscaling):
  that would reserve the peak on every system node.
- **Limit:** the highest peak among the covering reading and every reading
  inside the tier, times 1.3. The 30% covers spikes above p90 and some drift
  between measurements; see Open questions for tightening it.
- **Truncated readings.** A reading taken while the container was being
  OOM-killed shows what it reached, not what it needed, so its peak is
  not a limit candidate. If a tier holds one, the nearest reading above
  the bound whose peak exceeds the limit that killed it joins the
  candidates. The limit is still the highest peak among all of them, so
  a higher untruncated peak inside the tier is never discarded. If no
  reading above the bound exceeds the killing limit, the tier leaves the
  limit out and today's value applies, until such a reading exists. A
  tier's limit is therefore always above every `truncatedAt` recorded in
  it.
- **Instantaneous readings:** the reading stands in for p90, and reading ×
  1.7 for the peak, so it contributes reading × 1.7 × 1.3 to a limit. 1.7 is
  the largest max-to-p90 ratio measured on any controller.
- **DaemonSets:** the request uses the median across pods
  of each pod's p90, and the limit uses the largest single-pod peak.
- **Rounding:** round up to a multiple of 256Mi below 2Gi, and of 512Mi
  from 2Gi.
- **Ordering:** derive tiers from the lowest up. A tier's request and
  limit are each at least the tier below's, so a requirement measured in
  a lower tier carries upward and is never clamped down to fit. A tier
  may equal the tier above.
- **Today's values are the ceiling:** a request or limit whose
  requirement is at or above today's value is left out of the tier, and
  of every tier above it, so today's value applies. Where values files
  set different values for a path, today's value is the highest of them;
  a file that sets less is covered when sizing runs, which takes the
  smaller of the resolved value and the tier's. For a limit, two cases
  differ. If the values file of the platform a reading was taken on sets
  a limit below that reading's peak, that file's default is undersized: a
  defect fixed in that file, not by sizing, and no finding about another
  platform's file. If the limit is at or above the peak but below peak ×
  1.3, the tier keeps today's value with a thinner margin, and the
  measurement stays in the file to show it.
- **Same bounds:** every component's file declares the same `maxNodes`
  set, so one count selects the same row in each. A unit test over the
  registry enforces it.
- **Re-measurement:** the readings are re-read, from the longest window
  kept, before each release that changes the component's pin or values. For
  a controller that drifts at a constant node count, a limit derived from
  that cluster's readings uses the drifted peak, whichever tier it sets.

**Validation.** The loader fails closed with `ErrCodeInvalidRequest` on:

- an `apiVersion` or `kind` other than `aicr.run/v1beta1` and
  `ComponentSizing`;
- a malformed path;
- a `basis` other than `gpuNodes`;
- `maxNodes` values that do not strictly ascend;
- a quantity that does not parse;
- a limit below its request;
- a tier value above the same field in the next higher tier;
- a limit at or below a `truncatedAt` recorded in the same tier;
- a tier that sets `requests` or `limits` with no measurement whose `sets`
  names it;
- a measurement missing `kind`, `gpuNodes`, `max` or `date`, a windowed
  one missing `window`, or a windowed one that sets a limit missing
  `nodes`.

The loader cannot reject a well-formed path that no values file sets,
because an entry is skipped by design where a file does not set it. A
unit test fails instead when a sizing path is set by none of the
component's values files (registry, overlay or mixin), so a typo or a key
renamed by a chart bump cannot turn sizing into a silent no-op.

The contributor guide (`docs/contributor/component-sizing.md`, added by the
implementation PR) holds these rules, the Prometheus queries behind each
kind of reading, and a "Refining a tier" walkthrough. A line in
`.claude/CLAUDE.md` (mirrored to `AGENTS.md`) points to it from the
component-values task. No unit test asserts a real
component's tier values, the same rule as for upgrade records.

### Where the values live

Each sizing file holds its component's tiers and the measurements behind
them. A pull request that changes a tier changes the file, applying the
rules above to its new readings; it does not change this ADR. The
implementation PR ships the first files. A path that sizes nothing at any
count, such as a controller whose memory tracks something other than GPU
nodes, is left out of the file until it can be measured against them.

### Precedence

Sizing runs on a component's values after its values files resolve: the
component's base `values.yaml` and the componentRef's `valuesFile`, from
an overlay or mixin. For each memory
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

### Generation time

`aicr bundle --nodes` runs the sizing pass once per component, after
values files resolve and before inline and user overrides. For every
deployer except `argocd-helm` the result is baked into the component's
values.

A sizing file never names a path that a bundle-rendered manifest reads,
on any deployer. Such a manifest is rendered at generation time, so on
`argocd-helm` it could not follow the install-time count, and one rule
for every deployer keeps a component sized from one count. This is an
authoring rule, checked in review.

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
- **Paths read by bundle-rendered manifests** are never sized, as at
  generation time.
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
  A cluster with more total nodes than the readings behind its tier
  should state a higher count or override.

## Consequences

**Positive:**

- Nothing changes for a cluster that states no count or a count above the
  highest tier, so no large cluster re-rolls.
- Small and mid-sized clusters that state a count get values sized to their
  measurements.
- Tuning a tier is a data change to one sizing file, reviewed against the
  rules here.
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
  from churn (nodewright) and drift at a constant count
  (maintenance-operator) are not counted. Each measurement records
  total nodes and pods, so an operator with a far denser cluster knows to
  override.
- **A tier is only as good as its readings.** Where no reading lies near
  a bound, the nearest one above sizes the tier, which over-sizes it; where
  none lies above, the tier keeps today's values.
- **Requests on large clusters stay below usage,** as today.
- **The 30% margin is a stopgap** (see Open questions).

## Adoption plan

1. This ADR.
2. One implementation PR, closing
   [#3643](https://github.com/NVIDIA/aicr/issues/3643) and
   [#3644](https://github.com/NVIDIA/aicr/issues/3644):
   - the `sizing.yaml` schema, loader and data files; no default changes;
   - the `ComponentSizing` row in
     [ADR-022](022-artifact-maturity-and-deprecation.md) §2 and its read-gate
     entry, at `aicr.run/v1beta1`;
   - the generation-time sizing pass, the post-override request ≤ limit
     check, and the SDK's negative-count check;
   - the `argocd-helm` root value, per-tier static files, template
     selection, annotations and `--dynamic` guard;
   - the user docs, `docs/contributor/component-sizing.md` and its
     pointer in `.claude/CLAUDE.md` (mirrored to `AGENTS.md`).

## Open questions

- **Tightening the 30% margin** once the sized controllers are scraped
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
  cache. Either refines a nodewright tier; neither adds an input to tier
  selection.
- **Reporting the measured NodeWright numbers upstream** (see Option E).
- **Install-time sizing for the other deployers.** For `helm` and
  `helmfile`, `install.sh` could read an `AICR_NODE_COUNT` environment
  variable and layer per-tier values files. `flux`, `fleet` and `terraform`
  are not yet designed.
- **Passing a count from the AICR Operator**
  ([ADR-027](027-aicr-operator.md)) to the bundle step it runs.
