# AICR × Cluster Readiness Engine: performance integration

**Status:** Proposed for review — 2026-08-26, revised 2026-08-27 after Jason Du’s comments  
**Audience:** AICR, CRE, DGXC release engineering  
**Review draft.** Not published to the public docs site until accepted.

## Epic and task list

**Epic — Integrate Cluster Readiness Engine into AICR performance testing** (16 tasks)

### Group A — Foundation (7 tasks)

- Task 1 — Add CRE as a recipe component and publish a bundle
- Task 2 — Test CRE on a manually created cluster
- Task 3 — Implement the AICR NCCL check that drives `WorkloadRun`
- Task 4 — Implement the AICR training and goodput check (opt in GPU types gradually)
- Task 5 — Proof-of-concept correlation on `eks × h100`
- Task 6 — Inject OKE and NKE fabric configuration
- Task 7 — Upstream CRE fixes and fabric entries

### Group B — Opt in overlays that run NCCL today (7 tasks)

Same mechanism for every overlay: flip the recipe to the CRE NCCL check after correlation. New service × GPU combinations use this group when a recipe exists; they are not a second backlog.

- Task 8 — Opt in `eks × h100`
- Task 9 — Opt in `eks × gb200`
- Task 10 — Opt in `gke × h100`
- Task 11 — Opt in `aks × h100`
- Task 12 — Opt in `eks × h200`
- Task 13 — Opt in `oke × gb200`
- Task 14 — Opt in `nke × l40`

### Group C — Retirement (2 tasks)

- Task 15 — Retire the TrainJob NCCL path
- Task 16 — Update documentation and tests

## Summary

This document is the working backlog for integrating the NVIDIA Cluster Readiness Engine (CRE, formerly Excalibur) into AICR performance testing.

**The work is one epic containing 16 tasks.**

**Epic title:** Integrate Cluster Readiness Engine into AICR performance testing

| Group | Tasks | Count | What it covers |
|---|---|---|---|
| A — Foundation | 1–7 | 7 | Deploy CRE, prove it on a real cluster, write both checks, fabric |
| B — Overlay opt-in | 8–14 | 7 | Flip the seven overlays that run NCCL today |
| C — Retirement | 15–16 | 2 | Delete the old path and update docs |

**Where these get filed:** Linear, in the CRE team, with GitHub sync switched off. See "Where to file" below.

**The single decision point is task 5.** It compares bus bandwidth between the existing check and the CRE check on `eks × h100`. If the two do not correlate, the plan stops there, and nothing has been deleted.

**Retirement rule:** the old path is deleted only after every overlay that runs an NCCL check today has been validated on CRE. Migration is incremental; deletion is atomic.

### What changed in this revision

Jason Du’s review of the 37-task draft:

- **Group A** is the five core tasks (1, 3, 5, 6, 7), plus a simplified task 2 and a task 4 that opts GPU types in gradually.
- **Task 2** is a test on a **manually created cluster**, not the internal qualification pipeline (that pipeline adds complexity this work does not need).
- **Task 4** is implemented so GPU types can be opted in one at a time. That removes the old Group C (seven per-overlay training tickets).
- **Old Group D** (new CSP × GPU tickets) duplicated Group B. Overlay opt-in is one list. Extra combinations are not separate groups; they join Group B when a recipe exists.

Dashboard, per-node attribution, and adaptive fault isolation are **out of this epic**. They can be filed later if still wanted after the NCCL path is on CRE.

## Where to file

Agreed in Slack on 2026-08-26 in the CRE and AICR thread: **Linear is the system of record.** Jira is not used as the working backlog, and no GitHub epic is opened on `NVIDIA/aicr` or `dsx-ai-factory/cluster-readiness-engine` while CRE is private. Linear's GitHub sync stays off so nothing is mirrored into a repository.

**Tool:** [Linear](https://linear.app)

**Workspace:** the CRE / DSX AI Factory workspace the CRE team already uses.

**Team:** CRE (Cluster Readiness Engine). Confirm the team key with Lalit Adithya before creating anything.

**Container:** a Linear Project named `AICR × CRE performance integration`, holding one parent issue (the epic) with the 16 tasks as its children. A parent issue with sub-issues is an acceptable alternative if a project is too heavy.

**GitHub sync:** off. Create the issues unattached to any GitHub repository, and confirm under Settings, then Integrations, then GitHub that this project does not push issues into `cluster-readiness-engine` or `NVIDIA/aicr`.

**Optional Jira pointer:** one NKX epic whose description links the Linear project, for DGXC visibility only. Do not clone all 16 tasks into Jira.

Once created, record the identifiers here.

**Linear project URL:** _to be filled after creation_

**Epic issue key:** _to be filled after creation, in the form CRE-nnn_

## Epic

**Title:** Integrate Cluster Readiness Engine into AICR performance testing

CRE is a Kubernetes controller that certifies GPU clusters before production workloads run on them. It runs real training and communication workloads across topology-aware node groups, measures performance, detects hardware failures, and reports every bad node with a reason. It never cordons or taints a node; quarantine is left to the platform.

This epic makes CRE the executor for AICR performance validation, starting with the NCCL check, while AICR remains the judge of pass and fail. Training and goodput are one check that overlays opt into by GPU type, not a second migration program.

**Repositories and services**

- AICR: <https://github.com/NVIDIA/aicr>
- CRE: <https://github.com/NVIDIA/cluster-readiness-engine>
- Internal recipes repository: may still host a CRE component pin until the OSS chart is the default install path

## Decisions already made

1. **NCCL on public CRE uses `Certification`; goodput still uses `WorkloadRun`.** For EKS H100 NCCL, the OSS catalog already owns the EFA nccl-tests image and MPI command, so AICR creates `nvcre.nvidia.com/v1alpha1` `Certification` with `communication/nccl-all-reduce` and remains the judge. `WorkloadRun` stays the API for checks that must supply image, MPI, and fabric overrides (training/goodput, and any platform CRE does not already cover).
2. **AICR remains the judge.** Thresholds stay in the recipe's `performance.constraints`. CRE executes and reports; AICR evaluates.
3. **The transport assertion stays in AICR.** CRE's log-parsing profiles extract values but cannot assert presence or reject a value. AICR's existing assertion, roughly 200 lines, is retained.
4. **Each check gets its own constraint name.** The two checks will not report the same number, and sharing a name would permanently miscalibrate one of them. This is the least reversible decision here.
5. **Supply fabric configuration directly, and contribute it upstream in parallel.** This removes a three-to-six-week external dependency from the critical path at the cost of doing one day of work twice.
6. **Retirement is coverage-gated.** All seven overlays that run an NCCL check are validated first, then they migrate, then the old path is deleted.
7. **GPU types opt in gradually** for training and goodput (task 4). There is no per-overlay training ticket list.
8. **Do not run the internal qualification pipeline for the first CRE deploy.** Prove the component on a manually created cluster (task 2).

## Why do this

AICR's NCCL check is roughly 3,974 lines of non-test Go, or 8,685 including tests and fabric fixtures. It builds a Kubeflow TrainJob, applies per-cloud fabric configuration, waits for the launcher pod, and scrapes bus bandwidth out of the launcher log. CRE performs the same orchestration declaratively and reports results as structured resource status.

CRE also measures training performance (TFLOPS per GPU, step time, interruptions, lost work, goodput ratio), which AICR does not have today. That capability is implemented once and opted in by GPU type.

The NCCL migration is the on-ramp against a baseline we already trust.

## Tasks

### Group A — Foundation

Seven tasks, done once. Everything else depends on this group.

#### Task 1 — Add CRE as a recipe component and publish a bundle

Register the CRE Helm chart as a component in the internal recipes repository and publish a bundle containing it. The chart is on NVIDIA's NGC registry with 21 releases, latest 1.112.2, and the internal recipes CI already authenticates to that organisation. Rendering with default values produces exactly one image pinned to the chart version, seven CRDs, and five log-parsing profiles, so a Helm-only install is sufficient and the CRE CLI is not required to bootstrap.

Two pieces of known friction are already resolved. The chart hardcodes an image pull secret reference, which AICR supports natively for validator Jobs and the snapshot agent. The chart also renders a ServiceMonitor with no enable gate, so prometheus-operator CRDs must be present, and AICR already ships them.

This lands in the internal repository rather than the OSS registry, and moves when CRE open-sources.

**Done when** the component is registered, a published bundle contains CRE, and a Helm install succeeds on a cluster. **Depends on** nothing. **Effort** about one day.

#### Task 2 — Test CRE on a manually created cluster

Install the published bundle on a cluster created by hand and confirm the controller, CRDs, pull secret, and a first `Certification` (`communication/nccl-all-reduce`) come up. **Do not** run this through the internal qualification pipeline. That pipeline would add complexity this integration does not need for the first prove-out.

**Done when** CRE is running on that cluster and an NCCL `Certification` can be created against it. **Depends on** task 1.

#### Task 3 — Implement the AICR NCCL check that drives `Certification`

Write the validator that creates a `nvcre.nvidia.com/v1alpha1` `Certification` with `communication/nccl-all-reduce`, watches it to a Succeeded or Failed condition, reads max `busBW` from the category Workflow's `BandwidthMeasurement` results, evaluates the result against a new recipe constraint name, and asserts transport from the launcher log inside AICR.

The Certification owns the OSS catalog's EFA nccl-tests image and MPI command. AICR still supplies node targeting (selector, names, shared taints) and remains the judge. Training/goodput stays on `WorkloadRun` (task 4), which carries image, script, and goodput log-profile overrides.

Most of this is patterned on existing files. The check reuses AICR's threshold parsing, log fetch, evidence emission, and validator Job scaffolding, so the genuinely new code is creating a resource, watching it, and reading its status. No RBAC change is needed, because AICR deliberately binds each run's validator service account to cluster-admin precisely so validators can reach CRDs that do not exist at compile time.

The check is written so overlays can opt in one combination at a time (Group B). It is not referenced by every shipped overlay on day one.

**Done when** the check is written, unit tests are green, and `make qualify` passes, with the overlay catalog still pointing at the old check except where Group B has flipped. **Depends on** nothing to write; tasks 1 and 2 to run against a cluster.

#### Task 4 — Implement the AICR training and goodput check (opt in GPU types gradually)

Write a second validator that drives a `WorkloadRun` for a multi-node training workload and reads `GoodputMeasurement` (TFLOPS per GPU, average step time, interruption count, lost work time, goodput ratio).

**The implementation must let GPU types opt in gradually.** One check, recipe- or constraint-gated per accelerator (and overlay), so H100 can ship before GB200, and no seven-ticket training migration is required. Overlays that are not opted in must not run the check.

**Done when** the check is implemented with tests, at least one GPU type can be opted in without enabling the rest, and a opted-in combination reports goodput as a constraint result. **Depends on** task 3 for the shared `WorkloadRun` plumbing.

#### Task 5 — Proof-of-concept correlation on `eks × h100`

Run the existing TrainJob NCCL check and the new CRE check on the same hardware and compare bus bandwidth. **This is the go or no-go decision for the whole epic.**

The two are expected to disagree in absolute terms for two known reasons. AICR takes a single value from the last row of one run, while CRE reports an average across sampled measurements. And on AWS, AICR deliberately loads the EFA plugin while CRE deliberately removes it, on the stated basis that the built-in RDMA transport works without it.

Absolute agreement is not the bar. What matters is that the offset is predictable, and that once the CRE constraint is calibrated to it, no run produces a split verdict where one check passes and the other fails. The first launcher log also settles whether AICR's transport assertion holds unchanged or needs rewording.

**Done when** both paths have a successful run on the same cluster, the offset is documented, and the team records one of three outcomes: proceed, stop, or retain CRE solely for training capability. **Depends on** tasks 1, 2, and 3.

#### Task 6 — Inject OKE and NKE fabric configuration

CRE detects platform from each node's provider ID, with label fallbacks for bare metal, and detection already covers every platform AICR publishes bundles for. This includes OKE, which CRE calls `oci`, and NKE, which CRE calls `forge` and identifies through an explicit site-name label check. What is missing for both is only the fabric configuration entry, not the ability to recognise the platform.

CRE's platform override matrix is compiled into the manager binary, so changing it upstream ships only with a new image and chart release. Supply the `oci` and `forge` fabric configuration through the `WorkloadRun` configuration and overrides fields instead, which are user-supplied API fields AICR already controls.

**Done when** injected `oci` and `forge` fabric configuration lives in the AICR check path and can be applied without a CRE image bump. **Depends on** task 3.

#### Task 7 — Upstream CRE fixes and fabric entries

Track upstream work here even though the patches land in CRE. Priority order:

1. H200 on AWS receives no EFA resource request. It matches only the generic AWS block, so it falls back to TCP while still producing a plausible-looking number. A wrong measurement is worse than an unsupported platform. Roughly fifteen lines.
2. A documented override-matching variable is not actually bound, so expressions referencing it fail to compile.
3. Fabric configuration entries for `oci` and `forge` from task 6, contributed upstream so the configuration eventually lives in one place.
4. The Metal3 provider-ID mapping has no discriminator, unlike the two comparable customer mappings. Correct today, since Mistral is the only Metal3 customer.
5. Platform detection trusts the first node. A consistency-checking variant exists but is not always used.

**Done when** the H200 EFA fix is in a CRE release AICR can pin, or AICR carries a documented override that makes H200 measure RDMA, and the `oci` and `forge` fabric merge requests are filed. **Depends on** nothing. Off the critical path except for task 12 (`eks × h200`).

### Group B — Opt in overlays that run NCCL today

Seven overlays. These define the retirement criterion: the old path is deleted only once all of them are validated on CRE.

Every task uses the same method. Run both checks side by side, calibrate the new CRE constraint, then flip the overlay. The old check stays in the catalog until Group C. No overlay flips until its numbers are understood.

New service × GPU recipes (for example `gke × gb200`, `aks × h200`, A100, `metal3 × gb300`) are **not** a second group of tickets. When those overlays exist and should run NCCL, they opt in the same way as tasks 8–14.

#### Task 8 — Opt in `eks × h100`

Overlay `h100-eks-training`, default variant, current threshold at or above 300. CRE is ready. Same combination as the proof of concept in task 5, so it should flip first.

#### Task 9 — Opt in `eks × gb200`

Overlay `gb200-eks-training`, two variants: `net` at or above 40 and `nvls` at or above 500. CRE is ready. This is the combination that exercises both variants together with ComputeDomain, DRA, and IMEX. Budget hardware iteration; the first run is unlikely to be one-shot.

#### Task 10 — Opt in `gke × h100`

Overlay `h100-gke-cos-training`, default variant, current threshold at or above 300. CRE is ready.

#### Task 11 — Opt in `aks × h100`

Overlay `h100-aks-training`, default variant, current threshold at or above 150. CRE is ready.

#### Task 12 — Opt in `eks × h200`

Overlay `h200-eks-training`, default variant, current threshold at or above 300. **Blocked on task 7.** Until the EFA request is fixed, H200 measures TCP and would validate against a fallback number that looks plausible but is wrong.

#### Task 13 — Opt in `oke × gb200`

Overlay `gb200-oke-training`, `nvls` variant, current threshold at or above 500. **Blocked on task 6**, the injected `oci` fabric configuration.

#### Task 14 — Opt in `nke × l40`

Internal overlay `l40-nke-training`, `net` variant, current threshold at or above 20. **Blocked on task 6**, the injected `forge` fabric configuration.

### Group C — Retirement

Two tasks, both after Group B is complete.

#### Task 15 — Retire the TrainJob NCCL path

Delete the old check once every overlay in Group B runs on CRE. This removes roughly 6,300 lines.

Retirement is all-or-nothing by the shape of the code. Three files are per-cloud fabric plumbing and two are preflight gates, together about 800 lines. The remaining 3,200 or so is generic orchestration. That generic surface survives while any overlay still uses the old path, so migrating `eks`, `gke`, and `aks` alone removes only about 300 lines.

**Depends on** tasks 8 through 14.

#### Task 16 — Update documentation and tests

Update constraint names, the validate performance phase documentation, user-facing pages, and contributor pages to describe the CRE check only. Nine further overlays reference the old NCCL check only in comments explaining why it is omitted; those comments are documentation-only edits.

**Done when** `make qualify` is green and no documentation describes the deleted path.

## Sequencing

The proof of concept is tasks 1, 2, 3, and 5 and gates overlay opt-in. Stop after task 3 if the plumbing is awkward (nothing shipped). Stop after task 5 if bus bandwidth does not correlate. Nothing is deleted before task 15.

Once task 5 passes, tasks 8–11 can flip the four overlays CRE is already ready for. Tasks 6 and 7 unblock tasks 12–14. Task 4 can land in parallel with Group B; GPU types opt in when ready, without a separate training program.

Task 15 is last and requires all of tasks 8 through 14.

## Out of scope (this epic)

- Internal qualification pipeline for the first CRE deploy (task 2 is a manual cluster)
- Per-overlay training tickets (replaced by gradual GPU opt-in in task 4)
- A second “new CSP × GPU” ticket group (duplicates Group B)
- Publishing CRE results to <https://validation.aicr.run/#/>, per-node attribution, and adaptive fault isolation (file later if still needed)
- `bcm` (no performance validation today)
- Slurm overlays (a Pod-launched check bypasses `slurmd`)
- Cloning this backlog into GitHub, or duplicating all tasks in NKX Jira

## Open items

Two claims are inferred from CRE source, not live hardware, and neither is blocking: whether the Mistral cluster’s provider ID has the expected prefix, and whether BCM detects as `onprem`.

Deferred to the proof of concept by design: whether bus bandwidth correlates and at what offset, and whether the transport assertion survives the AWS plugin removal.

## Review asks

1. Confirm this 16-task cut matches Jason’s comments (Group A core + manual cluster + gradual GPU opt-in; no Group C; no duplicate Group D).
2. Confirm Linear team name and key, then paste the project URL and epic key into "Where to file".
3. Confirm task 5 remains the gate before Group B opt-in.
