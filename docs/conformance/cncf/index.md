# CNCF AI Conformance

## Overview

This directory contains evidence for [CNCF Kubernetes AI Conformance](https://github.com/cncf/k8s-ai-conformance)
certification. Each submission certifies a specific product on a specific Kubernetes
distribution, with evidence collected using AICR as the validation tooling.

> **Note:** It is the **product deployed on a Kubernetes platform** that is conformant.
> AICR serves as the deployment and validation tooling (similar to sonobuoy for K8s
> conformance), while the certified product is the AI inference/training platform.

## Checklist Versions

The requirement set is keyed by the upstream checklist version, which is the
Kubernetes minor being certified. AICR embeds verbatim copies of the upstream
checklists (`cncf/k8s-ai-conformance` `docs/AIConformance-<version>.yaml`) under
`pkg/evidence/cncf/schemas/`, pinned by digest, and carries v1.35, v1.36 and
v1.37. Evidence rendering selects the version from the validated cluster's
Kubernetes minor. A minor this build carries no checklist for renders against
the nearest carried one with a warning: below v1.35 against v1.35, above v1.37
against v1.37 (upstream also publishes v1.33 and v1.34, which AICR does not
carry). The Go SDK accepts an explicit version
(`RenderCNCFEvidenceOptions.ConformanceVersion`). A CLI flag for the explicit
version is a follow-up to #3606.

What changed between versions, as it affects AICR evidence:

| Version | Change | Effect on evidence |
|---------|--------|--------------------|
| v1.35 | Baseline: 9 MUST requirements including `dra_support` | `dra-support.md` is a requirement row |
| v1.36 | `dra_support` removed; `advanced_inference_ingress`, `high_performance_networking`, `disaggregated_inference` added (SHOULD) | `dra-support.md` renders under **Supporting Evidence**, not as a requirement; the SHOULD rows have no AICR check and appear in the index as `NOT TESTED (no AICR check)` |
| v1.37 | `workload_sandboxing` added (SHOULD); hybrid verification recommends `junit.xml` and `e2e.log` | No AICR check yet, so the row reads `NOT TESTED (no AICR check)`; see #3615 and #3605 |

Once at least one submission-required check has produced a result, the
rendered `index.md` lists every requirement of the selected checklist in
upstream order with its aggregate status (below). A requirement no non-skipped
check evidenced has no page and no evidence link. A report whose conformance
checks were all skipped or are all diagnostic writes nothing at all, not even
those rows.

## Requirement status

A requirement's status is computed from every check expected to evidence it
at the selected checklist version (`pkg/evidence/cncf/aggregate.go`,
`AggregateReport`), fail-closed: a requirement never reads as passed because
nothing contradicted it. The same aggregation feeds the evidence index and the
submission statuses (#3605); the CTRF report keeps every check's real verdict.

| Status | Meaning | How it arises |
|--------|---------|---------------|
| `PASSED` | Automated proof: every check on one proof path is present and `passed` | Where a requirement has alternative proofs (`secure_accelerator_access`: `secure-accelerator-access` on ordinary recipes, `slinky-slurm-gpu-access` on Slinky Slurm recipes) one complete path suffices; the page's `Proof:` line names the checks that established it, and the other path may be absent or skipped. A failed alternative fails the requirement whatever the other path did: on this cluster the capability was disproven |
| `FAILED (<checks>)` | A disproven capability | A check on any proof path `failed`, whatever the other paths did |
| `NOT TESTED (<reason>)` | An unmet test prerequisite, not a disproven capability | A check was `skipped` (reason code appended, e.g. `slurm-managed-gpus` when `secure-accelerator-access` defers to the Slurm path and `slinky-slurm-gpu-access` did not run), `other` (crashed), `pending`, or `not run` (absent from the report); `no AICR check` when AICR ships no check for the requirement |
| `NOT APPLICABLE (<justification>)` | Declared by configuration | Only from `spec.validate.evidence.cncf.notApplicable`, only for a conditional MUST (today `cluster_autoscaling`); any other id is rejected. It takes precedence over a check that ran, whose result stays on the page as an observation |

Statuses are scoped by level. The index header reports **Applicable MUSTs**:
`PASSED` when every MUST not declared not applicable is `PASSED`, else
`FAILED`. A SHOULD renders its own status, `FAILED (<checks>)` or
`NOT TESTED (<reason>)`, and does not fail the aggregate; submission
packaging (#3605) maps either to Not Implemented. The Go SDK exposes the aggregation as
`cncf.Aggregate` from `RenderCNCFEvidenceWithResult` (`Passed`,
`IncompleteMusts`, per-requirement `Status`, `Reason`, `Proof`, `Checks`);
`aicr validate` logs it after writing the evidence, and deriving the exit
status of a submission run from it is #3602.

Requirement page titles name the capability (`Inference API Gateway`,
`Robust AI Operator`); the implementation a check observed (which gateway,
scheduler or operator) is recorded in the check output on the page.

## Submissions

| Version | Product | Platform | Status | Evidence |
|---------|---------|----------|--------|----------|
| v1.35 | [NVIDIA NIM](https://developer.nvidia.com/nim) | EKS | 9/9 PASS | [v1.35/nim-eks/](v1.35/nim-eks/) |
| v1.36 | NVIDIA Kubernetes Engine | NVKE | 7 applicable MUSTs PASS; 1 conditional N/A | [v1.36/nvke/](v1.36/nvke/) |
| v1.36 | AICR training stack (VR200) | NVKE | 7 PASS, 2 SKIP — evidence only, see `../nvke/` for the submission | [v1.36/trainer-nvke-vr200/](v1.36/trainer-nvke-vr200/) |

## Directory Structure

```
docs/conformance/cncf/
├── index.md                          # This file
└── v1.35/                            # Kubernetes version
    └── nim-eks/                      # Product + platform (mirrors CNCF repo)
        ├── PRODUCT.yaml              # CNCF submission metadata
        ├── README.md                 # Submission overview + results table
        └── evidence/                 # Behavioral evidence files
            ├── index.md
            ├── dra-support.md
            ├── gang-scheduling.md
            ├── secure-accelerator-access.md
            ├── accelerator-metrics.md
            ├── ai-service-metrics.md
            ├── inference-gateway.md
            ├── robust-operator.md
            ├── pod-autoscaling.md
            └── cluster-autoscaling.md

pkg/evidence/cncf/                    # CNCF evidence collector package
├── collector.go                      # Feature registry, alias mapping
├── consts.go                         # Feature name constants
├── renderer.go                       # Evidence file rendering
├── aggregate.go                      # Requirement status aggregation, fail-closed by level
├── checklist.go                      # Versioned upstream checklists (ids, levels, descriptions), digest-pinned
├── requirements.go                   # Which conformance checks evidence which requirement; evidence page per requirement
├── schemas/                          # Verbatim upstream AIConformance-<version>.yaml copies
├── templates.go                      # Evidence templates
├── types.go                          # Shared types
└── scripts/                          # Evidence collection script + test manifests
    ├── collect-evidence.sh
    └── manifests/
        ├── dra-gpu-test.yaml
        ├── dynamo-vllm-agg.yaml
        ├── gang-scheduling-test.yaml
        ├── hpa-gpu-test.yaml
        └── trainer-pytorch-test.yaml
```

## Usage

Evidence collection has two steps:

### Structural Validation (CI)

`aicr validate` checks component health, CRDs, and constraints for CI:

```bash
# Structural validation + evidence rendering
aicr validate -r recipe.yaml \
  --phase conformance --evidence-dir ./evidence
```

### CNCF Submission Evidence

Add `--cncf-submission` to collect detailed behavioral evidence for CNCF AI
Conformance submission. This deploys GPU workloads, captures command outputs,
workload logs, nvidia-smi output, and Prometheus queries:

```bash
# Collect all behavioral evidence
aicr validate --phase conformance \
  --evidence-dir ./evidence --cncf-submission

# Collect specific features
aicr validate --phase conformance \
  --evidence-dir ./evidence --cncf-submission -f dra -f hpa
```

Alternatively, run the evidence collection script directly. Valid section
names are `dra`, `gang`, `secure`, `accelerator-metrics`, `service-metrics`,
`gateway`, `operator`, `hpa`, `cluster-autoscaling`, or `all`:

```bash
./pkg/evidence/cncf/scripts/collect-evidence.sh all
./pkg/evidence/cncf/scripts/collect-evidence.sh dra
```

> **Note:** The `--cncf-submission` flag deploys GPU workloads and takes ~5-10
> minutes. The evidence collection script automatically detects the AI workload
> type (NIM inference, Dynamo inference, or Kubeflow training) and collects
> appropriate metrics and operator evidence.

Each section writes exactly one **PASS**, **SKIP**, or **FAIL** verdict.
**SKIP** marks an absent optional prerequisite (a recipe that declares no
inference gateway, or a standalone run with no Gateway named
`inference-gateway`; no supported operator, an unsupported cluster-autoscaling provider, or an
operator present with no workload to reconcile) and is not a failure. A check
whose access path the recipe does not use also skips — see the Slinky Slurm
note under the GPU allocation test below.
A **FAIL** — an unhealthy present capability (including an inference Gateway
the recipe declares but the cluster does not have, or has programmed by another
class), a failed query, or a missing/malformed verdict — fails closed and makes the collection exit non-zero, so a
recorded section failure is no longer masked as overall success.

### Two Modes

> **Being retired.** The collector-script path behind `--cncf-submission` is
> being folded into `aicr validate --phase conformance` under
> [#3601](https://github.com/NVIDIA/aicr/issues/3601); the decision is recorded
> in the ADR-007 amendment
> ([docs/design/007-recipe-evidence.md](../../design/007-recipe-evidence.md)).
> `--evidence-dir` will then require `--cncf-submission`, so the
> `--evidence-dir`-alone rendering shown under Structural Validation goes
> away with it. Until that lands, the table below describes the shipped
> behavior.

| | `aicr validate --phase conformance` | `--cncf-submission` |
|---|---|---|
| **Purpose** | CI pass/fail | CNCF submission evidence |
| **Speed** | ~10-15 minutes (`pod-autoscaling` alone takes 6-9; its catalog timeout is 15m) | ~5-10 minutes |
| **Deploys workloads** | Yes (GPU allocation via DRA or device plugin, gang, HPA, secure access) | Yes (all + GPU stress test) |
| **Output** | Pass/fail + diagnostic artifacts | Detailed behavioral evidence (command outputs, logs, metrics) |
| **GPU allocation test** | secure-accelerator-access deploys a test pod via DRA or the device plugin (capability-driven) and verifies GPU access + isolation; dra-support's full-GPU DRA behavioral subtest is recorded N/A on ComputeDomain-only clusters; on MNNVL clusters (a Ready, schedulable node carrying the `nvidia.com/gpu.clique` label with a usable `compute-domain.nvidia.com` ResourceSlice) dra-support instead runs a behavioral IMEX channel subtest — it creates a ComputeDomain, waits for the driver-generated ResourceClaimTemplate, and a probe pod must observe exactly one allocated IMEX channel device (#1649); clusters without the clique label record that subtest N/A; clusters whose MNNVL nodes' channels are all held by existing claims (e.g. the Slinky Slurm leaves' standing ComputeDomain) are verified from the holding claim instead (allocated, reserved, consumer pod Running on the node) and fail as inconclusive when no holder verifies. On Slinky Slurm recipes secure-accelerator-access **skips**: the NodeSet reserves every GPU on a node for its `slurmd` pod and Slurm allocates per job through GRES and cgroups, so a Kubernetes-scheduled probe would either never schedule or attest an access path the workloads never use. On the GPU-backed Slinky recipes (not the kind leaf) `slinky-slurm-gpu-access` verifies Slurm's own access path instead: an `srun --gpus=1` job must list exactly one GPU and open exactly one GPU device node, and a job with no GPU request on the same Slurm node must open none (Slurm's cgroup device controller refuses the open with EPERM). Its result fills the `secure_accelerator_access` evidence page. `slinky-slurm-health` additionally confirms that a GPU-requesting job is allocated and its Pyxis container starts, and `slinky-slurm-imex-channel`, selected by the three GB Slurm leaves, confirms concurrent jobs receive distinct IMEX channels | Mode-aware evidence script (recipe policy selects the mode, standalone runs detect capability): behavioral full-GPU ResourceClaim test under DRA, device-plugin two-container isolation test + ResourceSlice evidence otherwise; nvidia-smi output capture. The evidence script has no Slinky Slurm path. |
| **Gang scheduling test** | Two CPU-only phases in one per-run namespace, matching upstream `TestGangScheduling`. Positive: a PodGroup with `minMember: 2` and two members requesting 100m CPU and 128Mi must have both members `Succeeded`, each with `spec.nodeName`, within 2 min and co-scheduled within 30 s. Negative: a PodGroup sized so its members cannot all fit (`min(nodes+1, 16)` members whose summed CPU requests exceed the cluster's allocatable CPU) must keep zero members bound (`spec.nodeName`) or `Running` for a 30 s window; one bound member fails the check by name. Credit requires KAI's affirmative gang refusal in the PodGroup `status.schedulingConditions` ("Resources were found for k pods while N are required for gang scheduling"), so merely-unbound pods under a stalled or absent scheduler are not credited. No GPU is requested; members associate to the PodGroup via the `pod-group-name` annotation | Two-phase all-or-nothing test on one pinned GPU node: a blocker leaves exactly one GPU free — neither worker may be **bound** (`spec.nodeName`, not pod phase) during the barrier window — then the blocker is removed and both workers must complete together; worker logs captured. Requires a **fully idle, Ready, schedulable GPU node with ≥ 2 allocatable GPUs**. Barrier credit requires the scheduler's AFFIRMATIVE gang decision, both parts verified live: each worker shows `PodScheduled=False/Unschedulable` AND the named PodGroup's scheduling status carries KAI's one-of-two gang refusal ("Resources were found for 1 pods while 2 are required for gang scheduling") — so merely-unbound pods (down/restarting/backlogged scheduler) and generically-refused pods (queue quota, transient fit errors) are never credited. Pods associate to the PodGroup via the `pod-group-name` annotation (the `pod-group.scheduling.run.ai/*` labels alone do NOT associate — KAI's pod-grouper ignores them and creates per-pod groups) |
| **HPA autoscaling** | `pod-autoscaling` mirrors the upstream `TestHPAAutoscaling`: a Deployment whose pods each request one accelerator (DRA claim or `nvidia.com/gpu: 1`, following the resolved allocation mode as `secure-accelerator-access` does) serves a per-pod gauge from a mounted ConfigMap; a PodMonitor the check creates makes the recipe's Prometheus scrape it and the `prometheus-adapter` rule shipped in `recipes/components/prometheus-adapter/values.yaml` serves it as the per-pod custom metric `ai_conformance_hpa_queue_depth`. An HPA (`minReplicas` 1, `maxReplicas` 2, `Pods` metric, target 10, 60s scale-down stabilization) is created only once the custom metrics API reports the baseline value; the HPA must reconcile at one replica and hold it for a one-minute negative control, scale to two Running and Ready pods when the metric is raised to 100, and return to one when it is lowered to 2. The check therefore needs two allocatable accelerators at once; the single-H100 kind leaves do not declare it. The custom metrics API must report each new value for every expected pod before a phase is judged; an adapter that does not serve the metric **fails** the check when `prometheus-adapter` (or `prometheus-adapter-ocp`) is in the recipe (upstream skips; the recipe owns the adapter). The CTRF detail names the phase that produced the verdict | CUDA GPU stress test + scale-up |
| **Metrics** | `accelerator-metrics` scrapes the DCGM exporter and requires per-device utilization and memory samples (a sample must carry a device label such as `gpu`, `UUID` or `pci_bus_id`); temperature, power and interconnect are recorded `PASS (SHOULD)` or `SKIP (SHOULD)` and never fail. `ai-service-metrics` deploys a CPU stub that exposes a request counter, a latency histogram and a queue-depth gauge labelled with a per-run id, plus a Service and a ServiceMonitor carrying the `release=kube-prometheus-stack` label; it waits for Prometheus to load the ServiceMonitor into its scrape pools (`/api/v1/scrape_pools`, so prometheus-operator's reconcile schedule does not consume the collection budget), then for Prometheus to collect the baseline, sends exactly 10 requests through the Service, and requires the collected counter to equal the baseline plus 10 — a higher value fails (something else sent traffic), a ServiceMonitor Prometheus does not load within the monitor-load timeout fails, and one loaded but not collected within the collection timeout fails, each with its own message. `pod-autoscaling` waits for its PodMonitor the same way before judging the custom metrics pipeline, when the recipe declares `kube-prometheus-stack`. Neither check depends on a model being served | DCGM exporter + Prometheus queries |
| **Gateway** | Condition verification (Accepted, Programmed) | Same |
| **Webhook test** | Rejection test with invalid CR | Schema-valid but webhook-invalid CR per operator, submitted with server-side dry-run (nothing persists); PASS requires a denial naming the operator's own webhook entry — CRD-schema rejections (`Required value`) and denials from other admission webhooks (e.g. a cluster policy webhook) are explicitly classified as NOT demonstrating the operator's webhook and FAIL |
| **Cluster autoscaling** | Cloud node group validation | Cloud-provider autoscaler API |
