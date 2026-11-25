# ADR-033: Signed Scale Claims for Recipes

## Status

**Proposed** — 2026-11-25. Answers
[#2429](https://github.com/NVIDIA/aicr/issues/2429) under the scalability
epic [#2427](https://github.com/NVIDIA/aicr/issues/2427). Adds an evidence
kind beside [ADR-007](007-recipe-evidence.md)'s and extends
[ADR-030](030-node-count-sizing.md)'s sizing files. The check is built
under [#2432](https://github.com/NVIDIA/aicr/issues/2432) and the
component requalification under
[#2430](https://github.com/NVIDIA/aicr/issues/2430).

## Decision Summary

A recipe opts in with a `validation.scale` block that names the workloads
whose cost grows with cluster size, the unit each grows with, and the
scenarios to run. The block changes no deployed value.
`aicr validate --phase scale` runs the scenarios with kube-burner and
counts the cluster's nodes at each fidelity level: API-only (KWOK),
GPU-simulated (Mokka) and real. First-party runs are signed as a new
evidence kind, `scale-evidence/v1`, which is kept apart from recipe-test
evidence. A recipe's claim is computed from its signed runs: for each
level, the largest node count that passed the checks that level can show.
Simulated counts are part of the claim and are always labelled, so a
recipe can be qualified by simulation alone. A recipe without the block is
unchanged.

## Problem

AICR cannot say that a recipe works at a given number of nodes.

- **No definition and no test.** `--nodes` is advisory. ADR-030 lowers
  controller memory from a stated count, based on production readings,
  but nothing tests a recipe at any size.
- **Every number so far comes from a production failure.** On GB300
  clusters of 1,700 to 2,700 nodes, these were OOM-killed:
  - the NFD master ([#3038](https://github.com/NVIDIA/aicr/issues/3038));
  - four controllers ([#3148](https://github.com/NVIDIA/aicr/issues/3148));
  - the nic-configuration-daemon
    ([#3149](https://github.com/NVIDIA/aicr/issues/3149),
    [#3698](https://github.com/NVIDIA/aicr/issues/3698)).

  These three have workarounds
  ([#3127](https://github.com/NVIDIA/aicr/pull/3127),
  [#3212](https://github.com/NVIDIA/aicr/pull/3212) and the #3698 fix).
  Separately, `aicr snapshot` overflows its 1 MiB ConfigMap at about 500
  to 700 nodes ([#3253](https://github.com/NVIDIA/aicr/issues/3253)), and
  that issue is open.
- **Cost grows with different units.**
  - gpu-operator grows with GPU nodes.
  - Nodewright, and network-operator before v26.10.0, grow with pods.
    Nodewright was OOM-killed at 436 nodes and 18k pods.
  - The NIC daemon grows with the NicDevices in the whole cluster.
  - The NFD master grows with NodeFeature count times size.
- **Simulation is excluded from evidence.** Real GPU capacity at these
  sizes is rare, so most sizes can only be tested in simulation, yet AICR
  keeps simulated lanes out of the evidence it ingests
  ([UAT](../contributor/uat.md#the-simulated-gpu-slurm-lane)).

## Goals

- Any recipe opts in with data alone. A second recipe needs no code
  change.
- The claim states, for each fidelity level, the largest node count
  passed, and the AICR version each count was measured on.
- No reader can mistake a simulated count for a real one.
- Claims are signed and verifiable offline, and identical inputs give
  identical output.
- Recipes, queries and bundles that do not opt in stay unchanged.

## Non-Goals

- **Provider control planes, etcd, DNS and CNI**, the non-goals of #2427.
  A run above 5,000 nodes names its control plane but does not qualify
  it.
- **Driver and firmware, NCCL and fabric, and image distribution.** Every
  claim lists them as not covered.
- **Partner acceptance** on partner clusters.
- **A claim for every recipe**, or a real-node count in every claim.
- **Changing deployed values.** Sizing stays as ADR-030 defines it.

## Context

### Fidelity levels

The control plane is real at every level. Only the node is simulated.

| Level | Node | Can show | Cannot show |
|---|---|---|---|
| API-only | KWOK: a Node object and its pods, with no kubelet. Its GPU capacity, labels, ResourceSlices and agent objects (NodeFeature, NicDevice) are written for it | Load on central controllers and the control plane as object count grows. The API load is a lower bound, because no kubelet or agent runs | Per-node agents, pod startup, anything a kubelet does |
| GPU-simulated | Mokka: a real kubelet and real agents on a CPU VM, with a mocked GPU | Per-node agent cost, rollouts, API load from N agents | Real timings: the VMs are overcommitted 4:1, so pod startup and rollout times are upper bounds |
| Real | A GPU node | Driver, fabric and real timings | |

One run can mix levels in one cluster.

Mokka's KWOK support
([MEP-0008](https://github.com/NVIDIA/k8s-test-infra/blob/main/enhancements/meps/0008-fgo-integration/README.md))
is merged as a design, and its implementation
([NVIDIA/k8s-test-infra#1244](https://github.com/NVIDIA/k8s-test-infra/issues/1244))
is open. It publishes GPU capacity, GFD and NFD labels and ResourceSlices
for KWOK nodes, but no NodeFeature or NicDevice objects, which are what
the NFD master and the NIC daemon cache.

### The known failures

| Failure | Grows with | Cheapest level expected to reproduce it |
|---|---|---|
| NFD master OOM (#3038) | NodeFeature count × size | API-only, with real-size NodeFeature objects |
| Controller OOM (#3148) | GPU nodes, or pods | API-only, with pods at the recipe's density |
| NIC daemon OOM (#3149, #3698) | NicDevices in the cluster | Mixed: the daemon on GPU-simulated or real nodes, with NicDevices written for KWOK nodes |
| Snapshot overflow (#3253) | Distinct label values | API-only, with realistic per-node labels |

The right-hand column is an expectation. Back-testing each failure at the
version pin where it occurred will confirm or replace it, and the coverage
table below will follow the results.

### What AICR already has

- **Validation.** Three phases: `deployment`, `conformance` and
  `performance`.
  - Checks publish results through `EmitExtra`.
  - Minimal redaction keeps only allowlisted keys and value shapes, and
    counts are limited to five digits.
  - Validators recognize KWOK nodes by the `kwok.x-k8s.io/node`
    annotation, and skip them with the reason `simulated-nodes`.
- **Recipe-test evidence.** ADR-007's predicate is `recipe-evidence/v3`,
  signed keyless and classified against `recipes/evidence/allowlist.yaml`.
  Its verifier rejects any predicate type or phase it does not know.
- **Simulated lanes.**
  - KWOK lanes ([ADR-003](003-scaling-recipe-tests.md),
    [-008](008-kwok-deployer-matrix.md),
    [-010](010-kwok-git-source-lanes.md)) test scheduling on a few
    simulated nodes.
  - A GPU-free Mokka lane deploys gpu-operator
    (`gpu-operator-mokka-test.yaml`).
- **Sizing.** ADR-030 is Proposed. Its implementation,
  [#3712](https://github.com/NVIDIA/aicr/pull/3712), is open and covers
  gpu-operator only.
- **Upstream SLOs.** The
  [Kubernetes SLOs](https://github.com/kubernetes/community/blob/master/sig-scalability/slos/slos.md)
  are:
  - mutating API calls: p99 ≤ 1s;
  - read-only calls: p99 ≤ 1s for one object, and ≤ 30s for a namespace
    or the cluster;
  - stateless pod startup, excluding image pull: p99 ≤ 5s.

  They hold up to 5,000 nodes
  ([thresholds](https://github.com/kubernetes/community/blob/master/sig-scalability/configs-and-limits/thresholds.md)).

## Options

**A. Counts in the recipe**, such as `maxNodes: 2500`. Rejected: results
would change the digest the evidence signs, and the number would have two
sources.

**B. A scale criteria axis, or an overlay per band.** Rejected for the
reasons ADR-030 rejected its option B: it multiplies the recipe matrix and
selects a configuration by size.

**C. Scale results in recipe-test evidence**, with the simulated lanes
added to the allowlist's first-party entries. Rejected: simulated runs
would count toward the hardware corroboration the evidence site shows, and
a reader could not tell which results were simulated.

**D. The contract in the recipe, the counts in their own evidence kind.**
Chosen.

For the check, two alternatives were rejected:

- **A check family in the performance phase** would put hour-long
  control-plane runs among minutes-long data-plane benchmarks.
- **A different runner.** Driving kube-burner through kaps-bench, the NVKE
  scale team's runner, would put a second orchestrator inside
  `aicr validate`. Adding ClusterLoader2 would mean writing every scenario
  twice.

AICR runs kube-burner directly, in a phase of its own, from scenarios that
kaps-bench exports as plain kube-burner configs.

## Decision

Adopt option D, as described below.

## Proposal

### The contract

An overlay opts in with `validation.scale`. It is ordinary recipe data:

- it inherits and merges like the other phases;
- it flows into the resolved recipe and the bundle's `recipe.yaml`;
- the recipe digest covers it;
- `aicr query --selector validation.scale` returns it.

Names and numbers in this example are illustrative:

```yaml
spec:
  validation:
    scale:
      timeout: 8h
      checks: [scale-control-plane, scale-controller-restart, scale-node-join]
      bands: [500, 2500, 8000]
      load:
        podsPerNode: 40           # 18k pods on 436 nodes (ADR-030)
        nicDevicesPerGPUNode: 2   # 4,965 NicDevices on ~2,500 GPU nodes (#3149)
      workloads:
        - {component: nfd, kind: Deployment, name: nfd-master, unit: totalNodes}
        - {component: gpu-operator, kind: Deployment, name: gpu-operator, unit: gpuNodes}
        - {component: network-operator, kind: DaemonSet, name: nic-configuration-daemon, unit: nicDevices}
        - {component: nodewright-operator, kind: Deployment, name: nodewright-manager, unit: pods}
```

- **`checks`** are catalog entries of phase `scale`. Each entry's
  kube-burner config lives under `recipes/validators/scale/<scenario>/`,
  so a new scenario is a data change. `timeout` works as in the other
  phases.
- **`bands`** name points defined in the bands file (see Bands).
- **`load`** sets how many pods and NicDevices a scenario creates per
  node, so units other than nodes are exercised at the recipe's density.
- **`workloads`** lists what the pass criteria watch, each in its
  component's namespace. `unit` is one of `gpuNodes`, `totalNodes`, `pods`
  or `nicDevices`. It sets three things:
  - what the scenarios grow;
  - what the run reports beside node counts;
  - which objects must exist at real count and size before a simulated
    level may count the workload.

The loader fails closed with `ErrCodeInvalidRequest` on:

- an unknown unit;
- a workload whose component is not in the recipe;
- a check that is not a `scale` entry;
- a band the bands file does not define.

The block selects nothing and renders nothing, so a bundle changes only in
its `recipe.yaml`. Profile-bearing recipes decode strictly, so a release
that reads the block must ship before one writes it into such a recipe
([ADR-022](022-artifact-maturity-and-deprecation.md) §3).

### Coverage

A result counts at a level only if the level can show it. This is the
first version, to be replaced by the back-test results:

| Criterion | API-only | GPU-simulated | Real |
|---|---|---|---|
| API latency SLOs | Counts, at KWOK's lower-bound load | Counts | Counts |
| Pod startup SLO | Does not count | A pass counts; a miss is inconclusive | Counts |
| No OOMKill or restart of a listed central controller | Counts | Counts | Counts |
| No OOMKill or restart of a listed per-node agent | Does not count | Counts | Counts |
| Convergence budgets | Central controllers only | A pass counts; a miss is inconclusive | Counts |
| `aicr snapshot` completes | Counts | Counts | Counts |

At the simulated levels, a listed workload counts only if the objects its
unit names exist at real count and size. Until real-size NodeFeature and
NicDevice objects can be written for simulated nodes, the NFD master and
the NIC daemon count only at the real level, and the claim lists them as
not covered below it.

**What each level counts.** The real and GPU-simulated counts are GPU
nodes. The API-only count is every node, because central controllers
cache them all. Counts are nested: a level includes the nodes of every
higher level. A result counts at the level of the nodes it depended on:

- a per-node agent's result counts at its node's level;
- a central controller's or the control plane's result counts at every
  level present in the run.

For example, 2,000 real GPU nodes plus 48,000 KWOK nodes, passing
everything, count 2,000 at the real and GPU-simulated levels and about
50,000 at the API-only level.

**Assigning levels.** The check assigns each node a level:

- a node with the `kwok.x-k8s.io/node` annotation is API-only;
- a node Mokka marks as simulated is GPU-simulated. The marker is to be
  agreed with the Mokka maintainers; the UAT lane uses
  `mokka.nvidia.com/type`;
- every other GPU node is real.

The API cannot prove a node is real, so the signer's identity caps the
level (see Signers).

### Bands and pass criteria

Band points are 500, 2,500 and 8,000 nodes, then 25,000 and 50,000. They
live in `recipes/validators/scale/bands.yaml`, a `ScaleBands` file at
`aicr.run/v1beta1`. A run is judged against the smallest band at or above
its total node count. The claim reports the measured count, not the band.

Every band requires:

- **Upstream SLOs** at the thresholds above, where AICR can measure them.
  - Pod startup is measured from the API.
  - API latency needs the API server's metrics in a Prometheus the check
    can query. Without them it is recorded as not measured, never as
    passed, and the claim says so.
  - Above 5,000 nodes the thresholds are AICR's targets, since upstream
    promises nothing there.
- **No OOMKill and no restart** of a listed workload during the run.
- **Convergence budgets** per scenario: the time for a restarted
  controller to be Ready, and for joined nodes to be labelled and Ready.
  - A band's budget is its first baseline plus a margin, recorded in the
    bands file with the run that set it.
  - Until a band has a baseline, convergence is reported but not gated,
    and the claim says so.

A run above 5,000 total nodes records its control plane: distribution,
Kubernetes version, and API server and etcd layout. The claim prints it.

### The scale check

**Phase.** `scale` joins the three existing phases. It runs only when
named with `--phase scale`, never under `all`. An attested run that
includes `scale` may include no other phase.

**Execution.** Each scenario is one validator Job.

- The Job runs kube-burner, which is embedded in the validator image and
  pinned per AICR release.
- kube-burner's pod latency measurement and its threshold exit code
  decide the pod startup criterion.
- Its metrics profiles read API latency, memory and restarts from
  Prometheus.
- The check reads OOMKills and restarts from the API, and counts nodes per
  level.
- The run takes an `aicr snapshot` at peak size, so #3253 must be fixed
  first.

**Size mismatch.** The run reports the counts it finds. If the bundle was
generated with a stated GPU node count, and the cluster has more GPU nodes
than the bound of the ADR-030 tier that count selects, the check fails:
its controllers are sized for a smaller cluster.

The check learns the stated count in one of two ways:

- from the `aicr.run/sizing-node-count` annotation on an `argocd-helm`
  bundle;
- from a new `aicr validate --nodes` flag, with `aicr bundle --nodes`
  semantics.

With no stated count, the cluster runs today's defaults, and size never
fails the check.

**Output.** One CTRF test per scenario. These new `EmitExtra` keys are
allowlisted in `pkg/evidence/redact`, with a `PolicyVersion` bump:

| Key | Shape |
|---|---|
| `scaleBand` | Count: the band point |
| `nodesReal`, `nodesGpuSimulated`, `nodesApiOnly` | Count, per level as defined above |
| `podsTotal`, `nicDevicesTotal` | Count: totals reached |
| `evidenceKind` | `qualified` or `observed` |
| `controlPlane` | Closed set of distribution codes |
| `apiMutatingP99Ms`, `apiReadP99Ms`, `podStartupP99Ms`, `convergenceSeconds` | Decimal; absent when not measured |
| `oomKills`, `restarts` | Count, summed over the listed workloads |

Two of these keys need something today's rules lack:

- pod totals above 99,999 need a count shape wider than five digits;
- per-workload readings cannot travel in `Extra`, whose values may not
  name a workload, so milestone 2 designs their transport.

### Scale evidence and signing

`aicr validate --phase scale --emit-attestation` writes a bundle of
predicate type `https://aicr.run/scale-evidence/v1`. It reuses ADR-007's
layout, signing and verifier steps, and records:

- the node counts per level, the totals and the band;
- the stated GPU node count and the control plane;
- for each listed workload, its version and the digests of its resolved
  values and sizing file;
- the digests of the bands file and of each scenario config;
- `evidenceKind`.

The recipe-test verifier rejects unknown predicate types. Scale bundles
therefore cannot enter its ingest path or the corroboration site, and
recipe-test evidence keeps its rule that simulated runs never reach it.

**Signers.** `recipes/scale-evidence/allowlist.yaml` lists the identities
whose runs count, each with the highest level it may report. The verifier
rejects a bundle that reports nodes above its signer's level. Workflow
names are proposals:

| Signer | May report |
|---|---|
| `scale-api-only.yaml` in NVIDIA/aicr | API-only |
| `scale-gpu-simulated.yaml` in NVIDIA/aicr | GPU-simulated |
| `scale-real.yaml` in NVIDIA/aicr | Real |
| Listed observed signers | Observed only |

- **Workflow refs.** Each workflow signs from the default branch or a
  release tag.
- **Runners.** Clusters outside GitHub-hosted runners, such as large
  simulated fleets and reserved GPU hardware, are reached through
  self-hosted runners registered to NVIDIA/aicr.
- **Other runs.** Runs from kaps-bench, other CI systems or a personal
  identity are not qualified evidence.
- **Pointers.** Each run, passing or failing, is committed as an ADR-007
  pointer under `recipes/scale-evidence/<recipe>/` through a bot pull
  request. The claim a release ships is therefore reviewed and kept in
  git history.

**Observed readings.** A production cluster running the recipe can add an
`observed` reading from the `observe` scenario, which adds no load. It
requires all of the following:

- the cluster runs the recipe's exact digest;
- no listed workload's values are overridden;
- every listed workload ran for at least 7 days without an OOMKill or
  restart.

The check verifies pod age, restart history and versions; the signer
attests the rest. Observed counts never enter the three level counts. The
claim shows them on their own line, and they are the only signal for
drift over weeks, such as maintenance-operator's growth at a constant node
count (ADR-030).

### The claim

`aicr evidence claim <recipe>` verifies the recipe's scale pointers and
prints its claim for an AICR release, by default the running binary's.
The same pointers and release give the same bytes, as text or JSON. All
values below are illustrative:

```text
Scale validation: gb300-generic-ubuntu-training @ <digest>, AICR vX.Y
  Real GPU nodes          2,000   (measured on vX.Y-1)
  GPU-simulated nodes     2,500   (Mokka; timings are upper bounds)
  API-only nodes         50,000   (KWOK; control plane: <name>)
  Observed                  <n>   (7 days; measured on vX.Y-3)
  Not covered: driver/firmware, NCCL/fabric, image distribution, provider control plane
  Not covered below real: nfd-master, nic-configuration-daemon
  Convergence: not gated at band 8,000 (no baseline)
```

- **The count per level** is the largest count from a valid run that
  passed every criterion the level counts. It stands only if no other
  eligible run failed at a smaller count of that level.
- **A level with no valid run** prints `not measured`. A claim may carry
  simulated levels only. Which recipes keep a real count is a
  maintainers' choice per recipe; this ADR names none.
- **Every count** states the AICR version it was measured on, and every
  level carries its fixed caveat.
- **Eligibility by age:**
  - An API-only count must come from the release's own recipe digest.
  - A GPU-simulated, real or observed count may come from an earlier
    digest, measured on the release or one of the four minor releases
    before it.
  - A count expires sooner when, since its run, any of these has changed:
    a listed workload's version, its resolved values, its sizing file, or
    a scenario the run used. The predicate's digests make this a direct
    comparison.

### Cadence

- **API-only:** every release, on its release candidate, for every recipe
  with a contract, at each band point the available control plane can
  hold.
- **GPU-simulated and real:** whenever capacity allows. Counts carry
  forward under the age rule.
- **Nightly:** the 500-node band also runs nightly on KWOK in CI, as an
  unsigned regression signal.

### Sizing

ADR-030's sizing files extend to the other values #2430 names, under
ADR-030's rules:

- a tier only lowers a value;
- only a stated GPU node count selects a tier;
- a tier never touches an override;
- every value rests on a measurement near its tier's bound.

The extensions:

- **CPU.** `requests` and `limits` gain `cpu` beside `memory`, measured as
  p90 and max in cores.
- **Integer values.** An entry may name an integer path, such as replicas,
  workers, or client QPS and burst. QPS is measured as the client's
  request rate and throttling.

Today's values stay the top tier, sized for the largest qualified band.
Raising a default is an ordinary default change, backed by a scale run at
the top band before it merges. No criteria axis is added.

A scale run bundles with `--nodes` set to its own GPU node count, so each
band exercises the tier a user stating that count gets. Tiers below the
smallest band rest on ADR-030's readings alone.

### The #2429 requirements

| Requirement | How it is met |
|---|---|
| Selecting the contract requires explicit intent | The recipe opts in through data, the run through `--phase scale`, and sized values through a stated count (ADR-030). The contract changes no deployed value |
| Node count alone never silently specializes a recipe | The check reads node counts only to report them. No count from a snapshot or from `criteria.nodes` selects anything |
| Identical inputs give byte-stable output | The contract is digest-covered recipe data, bundles do not change, and the claim depends only on its pointers and the release |
| Each value has one source of truth | Workloads, units and scenarios: the recipe. Thresholds and budgets: the bands file. Load shape: the scenario configs. Sized values: the sizing files. Counts: the signed runs |
| Overrides compose or fail clearly | `--set*` and `--dynamic` compose as ADR-030 and #3712 define. A claim covers the recipe's own values; a bundle that overrides a listed workload's resources is outside it, and `aicr bundle` warns |
| Unsupported deployer and `--dynamic` combinations fail before bundle output | The contract renders nothing into a bundle, so it adds no combination. ADR-030's rules stand |
| Existing queries and bundles are unchanged without opt-in | A recipe without `validation.scale` resolves and bundles byte-identically, and `--phase all` runs what it runs today |
| Actual size differs from the declared size | The claim reports measured counts. The check fails when the cluster has more GPU nodes than the tier its stated count selects |

## Consequences

**Positive:**

- A recipe gains a scale claim through data alone, at the cost it
  warrants: simulated counts for most recipes, real counts where
  maintainers keep them.
- Simulated runs become evidence without weakening recipe-test evidence,
  and every simulated number carries its level.
- The known failures become regression tests instead of incidents.
- Defaults raised for large clusters are tested before they ship.

**Negative:**

- **A simulated count is only as good as its calibration.** Until the
  back-tests confirm the coverage table, a simulated count can overstate
  what it shows. Labels limit the harm; they do not remove it.
- **The real level rests on the signer**, because the API cannot prove a
  node is real.
- **A bring-up window counts only through a registered runner.** A run
  made by hand on a new cluster is at most an observed reading.
- **Opting in changes the recipe's digest once**, so its recipe-test
  evidence must be regenerated.
- **Above 5,000 nodes a result holds only for the control plane it ran
  on.**
- **New upkeep:** a phase, an evidence kind and its allowlist, the bands
  file, scenarios kept in step with kaps-bench, and a kube-burner pin per
  release.

## Adoption plan

1. **This ADR and its first change:**
   - the `validation.scale` schema and loader checks;
   - the `scale` phase name, excluded from `all`;
   - the `ScaleBands` kind and its ADR-022 §2 row;
   - query support.

   No check runs yet.
2. **The check:**
   - kube-burner in the validator image, with the first scenarios exported
     from kaps-bench;
   - the new keys, the scale predicate, its allowlist, and
     `aicr evidence claim`;
   - the API-only workflow and the nightly lane;
   - the #3253 fix.
3. **The reference recipe**, proposed as `gb300-generic-ubuntu-training`:
   its components requalified (#2430) and sized, the GPU-simulated
   workflow, and its claim shipped in a release. A second recipe then opts
   in with data alone.
4. **API-only runs at 25,000 and then 50,000 nodes**, on a control plane
   that can hold them.
5. **The real-node workflow and observed readings.**

Each step updates the pages it affects:

- `docs/user/cli-reference.md`;
- `docs/user/validation.md`;
- the recipe-development pages;
- `docs/contributor/validator.md`;
- `docs/contributor/uat.md`;
- `docs/contributor/component-sizing.md`.

## Open questions

- **Positive evidence of a real node**, so the real level need not rest
  on the signer alone.
- **Per-workload results through redaction** (milestone 2).
- **Budget margins**, once baselines exist.
- **Whether 500 KWOK nodes fit a GitHub-hosted runner.**
- **API latency on managed control planes** without API server metrics:
  accept "not measured", or require the metrics above some band.
- **A scale view on the evidence site.**
- **Memory history for observed readings**, which needs the scrape gaps
  closed ([#3265](https://github.com/NVIDIA/aicr/issues/3265) to
  [#3269](https://github.com/NVIDIA/aicr/issues/3269)).
