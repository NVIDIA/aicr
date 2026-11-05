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

pkg/evidence/cncf/                    # CNCF evidence rendering package
├── renderer.go                       # Evidence file rendering from the CTRF report
├── aggregate.go                      # Requirement status aggregation, fail-closed by level
├── checklist.go                      # Versioned upstream checklists (ids, levels, descriptions), digest-pinned
├── requirements.go                   # Which conformance checks evidence which requirement; evidence page per requirement
├── schemas/                          # Verbatim upstream AIConformance-<version>.yaml copies
├── templates.go                      # Evidence templates
└── types.go                          # Shared types
```

## Usage

`aicr validate --phase conformance` is the only runner for CNCF AI
Conformance. The recipe declares which conformance checks run, every run
that does not withhold checks (`--skip-check`, `--no-cluster`; a submission
run refuses both) executes all of them at the depth the upstream tests
require, and a flag decides only whether evidence is written. The decision is recorded in the
ADR-007 amendment
([docs/design/007-recipe-evidence.md](../../design/007-recipe-evidence.md))
and tracked in [#3601](https://github.com/NVIDIA/aicr/issues/3601).

```bash
# Nightly run: every conformance check the recipe declares, CTRF report,
# optional recipe-evidence attestation, no evidence files.
aicr validate -r recipe.yaml -s snapshot.yaml --phase conformance

# Submission run: the same checks with the same verdicts, then the CTRF
# report rendered into ./evidence as one page per requirement a non-skipped
# check evidenced, plus index.md (nothing when no check ran to completion).
aicr validate -r recipe.yaml -s snapshot.yaml --phase conformance \
  --cncf-submission --evidence-dir ./evidence
```

`--cncf-submission` and `--evidence-dir` require each other. A submission run
requires `--recipe` (the checks are selected from it) and the conformance
phase, and refuses `--no-cluster` and `--skip-check`: the evidence comes from
checks that ran on a live cluster against the whole recipe. Its exit status
adds the requirement aggregation above to the per-check rule: an applicable
MUST that is not established fails the run after the evidence is written,
the aggregation never fails it on a SHOULD, and a failed or crashed check
fails it under `--fail-on-error` (the default) as in any run.
The submission directory itself (PRODUCT.yaml, README, `junit.xml`, `e2e.log`,
one redaction pass, the pinned upstream validator) is
[#3605](https://github.com/NVIDIA/aicr/issues/3605).

The bash collector that `--cncf-submission` used to run, its `--feature`
section interface and `spec.validate.evidence.cncf.features` were removed in
[#3602](https://github.com/NVIDIA/aicr/issues/3602). Its captures that
exceeded the CNCF bar (the GPU gang barrier and blocker, the CUDA HPA stress
workload, nvidia-smi and DCGM dumps, Prometheus query captures, the
Dynamo/NIM/trainer service-metrics sections, the AWS CLI and GKE ConfigMap
cluster-autoscaling captures, and the DRA section) were deleted without a
port, per the audit in [#3619](https://github.com/NVIDIA/aicr/issues/3619);
the sections at the bar were ported into the Go checks in place
([#3603](https://github.com/NVIDIA/aicr/issues/3603),
[#3604](https://github.com/NVIDIA/aicr/issues/3604),
[#3428](https://github.com/NVIDIA/aicr/issues/3428)).

### What the checks demonstrate

The conformance phase takes 10 to 15 minutes on a two-node GPU cell
(`pod-autoscaling` alone takes 6 to 9; its catalog timeout is 15m). Every
check deploys what it needs into a per-run namespace and removes it.

| Requirement | Check | What the check demonstrates |
|---|---|---|
| `secure_accelerator_access` | `secure-accelerator-access`, `slinky-slurm-gpu-access` | `secure-accelerator-access` deploys a test pod via DRA or the device plugin (following the recipe's allocation policy) and verifies GPU access and isolation. On Slinky Slurm recipes it **skips**: the NodeSet reserves every GPU on a node for its `slurmd` pod and Slurm allocates per job through GRES and cgroups, so a Kubernetes-scheduled probe would either never schedule or attest an access path the workloads never use. On the GPU-backed Slinky recipes `slinky-slurm-gpu-access` verifies Slurm's own access path instead: an `srun --gpus=1` job must list exactly one GPU and open exactly one GPU device node, and a job with no GPU request on the same Slurm node must open none (Slurm's cgroup device controller refuses the open with EPERM). Either path fills the page; see [Requirement status](#requirement-status) for how the alternatives aggregate. After the CNCF subtests, secure-accelerator-access also runs an AICR hardening extra: an unprivileged pod on the NVIDIA RuntimeClass the GPU Operator's ClusterPolicy establishes (`spec.operator.runtimeClass`, or its default `nvidia` when unset; a class present by name alone is not probed) with `NVIDIA_VISIBLE_DEVICES=all` and no allocation, pinned to the test pod's node, which sees no GPU device when the toolkit hardening (`ACCEPT_NVIDIA_VISIBLE_DEVICES_ENVVAR_WHEN_UNPRIVILEGED=false`) is in place. Its artifacts are labelled "AICR hardening extra (not a CNCF item)", its outcome (`HARDENED`, `EXPOSED`, `INCONCLUSIVE`, or `NOT RUN` when the ClusterPolicy does not establish the class) is an observation that never decides the check, and the requirement row does not cite it. `slinky-slurm-health` additionally confirms that a GPU-requesting job is allocated and its Pyxis container starts, and `slinky-slurm-imex-channel`, selected by the three GB Slurm leaves, confirms concurrent jobs receive distinct IMEX channels |
| `dra_support` (v1.35; supporting evidence from v1.36) | `dra-support` | The full-GPU DRA behavioral subtest is recorded N/A on ComputeDomain-only clusters; on MNNVL clusters (a Ready, schedulable node carrying the `nvidia.com/gpu.clique` label with a usable `compute-domain.nvidia.com` ResourceSlice) it runs a behavioral IMEX channel subtest instead: it creates a ComputeDomain, waits for the driver-generated ResourceClaimTemplate, and a probe pod must observe exactly one allocated IMEX channel device (#1649); clusters without the clique label record that subtest N/A; clusters whose MNNVL nodes' channels are all held by existing claims (e.g. the Slinky Slurm leaves' standing ComputeDomain) are verified from the holding claim instead (allocated, reserved, consumer pod Running on the node) and fail as inconclusive when no holder verifies |
| `gang_scheduling` | `gang-scheduling` | Two CPU-only phases in one per-run namespace, matching upstream `TestGangScheduling`. Positive: a PodGroup with `minMember: 2` and two members requesting 100m CPU and 128Mi must have both members `Succeeded`, each with `spec.nodeName`, within 2 min and co-scheduled within 30 s. Negative: a PodGroup sized so its members cannot all fit (`min(nodes+1, 16)` members whose summed CPU requests exceed the cluster's allocatable CPU) must keep zero members bound (`spec.nodeName`) or `Running` for a 30 s window; one bound member fails the check by name. Credit requires KAI's affirmative gang refusal in the PodGroup `status.schedulingConditions` ("Resources were found for k pods while N are required for gang scheduling"), so merely-unbound pods under a stalled or absent scheduler are not credited. No GPU is requested; members associate to the PodGroup via the `pod-group-name` annotation |
| `pod_autoscaling` | `pod-autoscaling` | Mirrors the upstream `TestHPAAutoscaling`: a Deployment whose pods each request one accelerator (DRA claim or `nvidia.com/gpu: 1`, following the resolved allocation mode as `secure-accelerator-access` does) serves a per-pod gauge from a mounted ConfigMap; a PodMonitor the check creates makes the recipe's Prometheus scrape it and the `prometheus-adapter` rule shipped in `recipes/components/prometheus-adapter/values.yaml` serves it as the per-pod custom metric `ai_conformance_hpa_queue_depth`. An HPA (`minReplicas` 1, `maxReplicas` 2, `Pods` metric, target 10, 60s scale-down stabilization) is created only once the custom metrics API reports the baseline value; the HPA must reconcile at one replica and hold it for a one-minute negative control, scale to two Running and Ready pods when the metric is raised to 100, and return to one when it is lowered to 2. The check therefore needs two allocatable accelerators at once; the single-H100 kind leaves do not declare it. The custom metrics API must report each new value for every expected pod before a phase is judged; an adapter that does not serve the metric **fails** the check when `prometheus-adapter` (or `prometheus-adapter-ocp`) is in the recipe (upstream skips; the recipe owns the adapter). The CTRF detail names the phase that produced the verdict |
| `accelerator_metrics` | `accelerator-metrics` | Scrapes the DCGM exporter and requires per-device utilization and memory samples (a sample must carry a device label such as `gpu`, `UUID` or `pci_bus_id`); temperature, power and interconnect are recorded `PASS (SHOULD)` or `SKIP (SHOULD)` and never fail |
| `ai_service_metrics` | `ai-service-metrics` | Deploys a CPU stub that exposes a request counter, a latency histogram and a queue-depth gauge labelled with a per-run id, plus a Service and a ServiceMonitor carrying the `release=kube-prometheus-stack` label; waits for Prometheus to load the ServiceMonitor into its scrape pools (`/api/v1/scrape_pools`, so prometheus-operator's reconcile schedule does not consume the collection budget), then for Prometheus to collect the baseline, sends exactly 10 requests through the Service, and requires the collected counter to equal the baseline plus 10. A higher value fails (something else sent traffic), and a ServiceMonitor not selected within the collection timeout fails. The check does not depend on a model being served |
| `ai_inference` | `inference-gateway` | GatewayClass Accepted, Gateway Programmed and the InferencePool CRD Established for the inference gateway the recipe declares (`agentgateway` by default, or the component whose `componentRef` sets `overrides.inferenceGateway`, see [Declaring the inference gateway](../../integrator/data-extension.md#declaring-the-inference-gateway)), plus the gateway's network exposure and data-plane readiness; a recipe that declares no gateway skips. The weighted-traffic test is [#3428](https://github.com/NVIDIA/aicr/issues/3428) |
| `robust_controller` | `robust-controller` | A schema-valid but webhook-invalid CR per operator is submitted with server-side dry-run (nothing persists); PASS requires a denial naming the operator's own webhook entry. CRD-schema rejections (`Required value`) and denials from other admission webhooks (e.g. a cluster policy webhook) are classified as not demonstrating the operator's webhook and FAIL. Then reconciliation: On the Kubeflow Trainer path `robust-controller` asserts the TrainJob CRD is `Established=True` and creates a valid TrainJob on the default `torch-distributed` `ClusterTrainingRuntime` in a per-run namespace — one node, no accelerator, the probe image with a command that exits 0 — and requires `Complete=True` within 3 min, failing on `Failed=True` or a missing runtime (the upstream `TestRobustCRDControllerOperation` shape). The Dynamo path records the existing `DynamoGraphDeployment` conditions as an observation and creates no CR: a minimal one has no terminal condition to reach without a served model. A recipe shipping both operators is evidenced by the Kubeflow path |
| `cluster_autoscaling` | `cluster-autoscaling` | Karpenter: drives a scale-up and records it (scale-down is not observed). EKS, AKS and GKE: observations — autoscaler presence, GPU pool membership, the `cluster-autoscaler-status` ConfigMap and recent autoscaler events — with no scale event driven, recorded as `OBSERVED`, which is not behavioral proof even though the completed check reports `passed` (a definitive NotFound is recorded as absence, a failed read as unread, never as absence; the requirement-level reading of a passed observation is a follow-up). One rule fails: on EKS, GPU nodes with no `eks.amazonaws.com/nodegroup` label, no available in-cluster Cluster Autoscaler Deployment and no status ConfigMap (and no Karpenter) fail — `NotFound` when every read answered NotFound, the read's classified code when a Deployment was unavailable or a read could not land — because nothing confirmed an autoscaler and a passed result would render `cluster_autoscaling` PASSED; the remedy for a platform without workload-driven autoscaling is a `spec.validate.evidence.cncf.notApplicable` justification. Otherwise the `cluster_autoscaling` claim rests on provider documentation or that justification. A providerID with no probe (bare metal, Kind, KWOK) skips as "no autoscaler probe for this provider": a missing probe, not product nonapplicability. A fixed-size cluster declares the conditional MUST not applicable through `spec.validate.evidence.cncf.notApplicable`; the constrained claim is [#3428](https://github.com/NVIDIA/aicr/issues/3428) |
