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
carry). That fallback is for the evidence pages only: a submission run
(`--cncf-submission`, `WriteCNCFSubmission`) certifies the cluster's exact
minor and is refused with `INVALID_REQUEST` when no carried checklist matches
it, unless a checklist is named explicitly
(`spec.validate.evidence.cncf.conformanceVersion` in the validate config, or
`CNCFSubmissionOptions.ConformanceVersion` in the Go SDK), which the README
records. The Go SDK accepts an explicit version for the pages too
(`RenderCNCFEvidenceOptions.ConformanceVersion`).

What changed between versions, as it affects AICR evidence:

| Version | Change | Effect on evidence |
|---------|--------|--------------------|
| v1.35 | Baseline: 9 MUST requirements including `dra_support` | `dra-support.md` is a requirement row |
| v1.36 | `dra_support` removed; `advanced_inference_ingress`, `high_performance_networking`, `disaggregated_inference` added (SHOULD) | `dra-support.md` renders under **Supporting Evidence**, not as a requirement; the SHOULD rows have no AICR check and appear in the index as `NOT TESTED (no AICR check)` |
| v1.37 | `workload_sandboxing` added (SHOULD); hybrid verification recommends `junit.xml` and `e2e.log` | The `workload-sandboxing` check evidences it on a recipe that declares the check; no embedded recipe does yet, so the row reads `NOT TESTED (not run)` (see below). Hybrid verification is the submission directory's `junit.xml` and `e2e.log` (#3605, see [Submission directory](#submission-directory)). PRODUCT.yaml renders a SHOULD that is not established Not Implemented |

`driver_runtime_management` is SHOULD at every carried checklist and graduates
to MUST at v1.38 (upstream KAR-0001). The `driver-runtime-management` check
evidences it from v1.35 on: at SHOULD a failed or skipped result renders on its
own row (`FAILED` or `NOT TESTED`) without failing the requirement aggregate
(the `aicr validate` run itself still fails a failed check under the default
`--fail-on-error`, which is a per-check rule), and the submission's
[export rule](#export-rule) exports such a testcase as skipped; once the
v1.38 checklist is carried the same result blocks the submission. The check
never records N/A.

Once at least one submission-required check has produced a result, the
requirement table lists every requirement of the selected checklist in
upstream order with its aggregate status (below): in `index.md` for the
evidence-pages render the Go SDK's `RenderCNCFEvidence*` methods perform, and
in `README.md` of the submission directory `aicr validate --cncf-submission`
writes. A requirement no non-skipped check evidenced has no page and no
evidence link. A report whose conformance checks were all skipped or are all
diagnostic renders no pages and no index; a submission run still writes
PRODUCT.yaml, README.md and the artifacts, with every requirement not
established.

`workload_sandboxing` is SHOULD at v1.37, the only carried checklist that
lists it, and graduates to MUST at v1.38 (upstream KAR-0020). The
`workload-sandboxing` check evidences it from v1.37 on. A cluster with no
sandboxed `RuntimeClass` fails the check with `unmet prerequisite: no
sandboxed RuntimeClass`, so at SHOULD the row renders `FAILED` on its own
without failing the requirement aggregate (the `aicr validate` run itself
still fails a failed check under the default `--fail-on-error`, which is a
per-check rule); once the v1.38 checklist is carried the same result blocks
the submission. The check never skips and never records N/A: upstream allows a
MUST-level N/A only for the conditional `cluster_autoscaling`. No AICR recipe
ships a sandboxed `RuntimeClass` today, and none declares the check yet: which
runtime the stack provides on which pool, and whether it is a registry
component AICR deploys or a platform prerequisite AICR detects, is the pending
product decision of #3615. Until it lands, the row renders `NOT TESTED (not
run)` at SHOULD; the recipes declare the check once that decision provides a
`RuntimeClass`, and an operator whose cluster already has gVisor or Kata can
declare it on a recipe copy today (naming the class with
`AICR_SANDBOX_RUNTIME_CLASS` when detection does not match).

## Requirement status

A requirement's status is computed from every check expected to evidence it
at the selected checklist version (`pkg/evidence/cncf/aggregate.go`,
`AggregateReport`), fail-closed: a requirement never reads as passed because
nothing contradicted it. The same aggregation feeds the evidence index and the
submission statuses of PRODUCT.yaml (see [Statuses are never typed](#statuses-are-never-typed)); the CTRF report keeps every check's real verdict.

| Status | Meaning | How it arises |
|--------|---------|---------------|
| `PASSED` | Automated proof: every check on one proof path is present and `passed` as a demonstration | Where a requirement has alternative proofs (`secure_accelerator_access`: `secure-accelerator-access` on ordinary recipes, `slinky-slurm-gpu-access` on Slinky Slurm recipes) one complete path suffices; the page's `Proof:` line names the checks that established it, and the other path may be absent or skipped. A failed alternative fails the requirement whatever the other path did: on this cluster the capability was disproven |
| `OBSERVED (<checks>)` | A recorded arrangement, not a demonstration | Every check on a proof path `passed`, but one of them marked its result observation-only (`extra.observationOnly: "true"`, `validators.ObservationOnlyExtra`): it recorded what it saw and drove no behavioral test. The check's CTRF status stays `passed` and its page shows the observations; the requirement is not established, so an observed MUST fails the submission run exactly as a `NOT TESTED` one does (the exit message names it `observed`), unless `notApplicable` covers it or `spec.validate.evidence.cncf.documented` attests it by public documentation, which establishes it while the status stays `OBSERVED`. An observed SHOULD never fails the run. Today only `cluster-autoscaling` sets the marker, on every path that passes. A `passed` result of a check AICR knows to be observation-only that lacks the marker is read as observed too, whatever its `extra.resultSchema` stamp says, so neither an older image nor a dropped marker line renders the requirement `PASSED`; the stamp only words the appended reason (`result contract marker absent` when the image predates the contract, otherwise that the marker went missing or the contract version is unknown) |
| `FAILED (<checks>)` | A disproven capability | A check on any proof path `failed`, whatever the other paths did |
| `NOT TESTED (<reason>)` | An unmet test prerequisite, not a disproven capability | A check was `skipped` (reason code appended, e.g. `slurm-managed-gpus` when `secure-accelerator-access` defers to the Slurm path and `slinky-slurm-gpu-access` did not run), `other` (crashed), `pending`, or `not run` (absent from the report); `no AICR check` when AICR ships no check for the requirement |
| `NOT APPLICABLE (<justification>)` | Declared by configuration | Only from `spec.validate.evidence.cncf.notApplicable`, only for a conditional MUST (today `cluster_autoscaling`); any other id is rejected. It takes precedence over a check that ran, whose result stays on the page as an observation; PRODUCT.yaml renders `N/A` with the justification in `notes`, which the upstream validator accepts for this id with a reviewer warning |

Statuses are scoped by level. The index header reports **Applicable MUSTs**:
`PASSED` when every MUST not declared not applicable is `PASSED`, else
`FAILED`. A SHOULD renders its own status, `OBSERVED (<checks>)`,
`FAILED (<checks>)` or `NOT TESTED (<reason>)`, and does not fail the
aggregate; the [submission directory](#submission-directory) maps each to
Not Implemented. An `OBSERVED` requirement is **established** when
`spec.validate.evidence.cncf.documented` attests it by public documentation
(the [manual-attestation rule](#manual-attestation-rule)); its status stays
`OBSERVED` and it no longer counts as incomplete. The Go SDK exposes the aggregation as
`cncf.Aggregate` from `RenderCNCFEvidenceWithResult` (`Passed`,
`IncompleteMusts`, per-requirement `Status`, `Reason`, `Proof`, `Checks`);
`aicr validate --cncf-submission` logs it after writing the evidence and adds
it to the run's exit status through `Outcome` (#3602); see
[Exit status of a submission run](../../user/validation.md#exit-status-of-a-submission-run).

Requirement page titles name the capability (`Inference API Gateway`,
`Robust AI Operator`); the implementation a check observed (which gateway,
scheduler or operator) is recorded in the check output on the page.

## Submissions

| Version | Product | Platform | Status | Evidence |
|---------|---------|----------|--------|----------|
| v1.35 | [NVIDIA NIM](https://developer.nvidia.com/nim) | EKS | 9/9 PASS | [v1.35/nim-eks/](v1.35/nim-eks/) |
| v1.36 | NVIDIA Kubernetes Engine | NVKE | 7 applicable MUSTs PASS; 1 conditional N/A | [v1.36/nvke/](v1.36/nvke/) |
| v1.36 | AICR training stack (VR200) | NVKE | 7 PASS, 2 SKIP — evidence only, see `../nvke/` for the submission | [v1.36/trainer-nvke-vr200/](v1.36/trainer-nvke-vr200/) |
| v1.36 | AICR inference stack (VR200) | NVKE | legacy-collector observations, not checklist verdicts — evidence only, see `../nvke/` for the submission | [v1.36/inference-nvke-vr200/](v1.36/inference-nvke-vr200/) |

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
├── renderer.go                       # Evidence pages from the CTRF report
├── aggregate.go                      # Requirement status aggregation, fail-closed by level
├── checklist.go                      # Versioned upstream checklists (ids, levels, descriptions), digest-pinned
├── requirements.go                   # Which conformance checks evidence which requirement; evidence page per requirement
├── submission.go                     # The submission directory: staging, redaction, upstream validator gate
├── product.go                        # PRODUCT.yaml from the aggregate and the vendor config
├── export.go                         # junit.xml and e2e.log from the CTRF report (export rule)
├── readme.go                         # README.md: platform facts, requirement index, provenance
├── vendor.go                         # Vendor and documented-attestation config validation
├── schemas/                          # Verbatim upstream AIConformance-<version>.yaml copies
├── upstreamvalidator/                # cncf/k8s-ai-conformance scripts/validate.go, vendored at a pinned commit
├── templates.go                      # Evidence page templates
└── types.go                          # Shared types
```

The directory `aicr validate --cncf-submission` writes is what a vendor
submits as `vX.Y/<product>/` in `cncf/k8s-ai-conformance`:

```
<evidence-dir>/
├── PRODUCT.yaml                      # Statuses from the run, metadata from spec.validate.evidence.cncf.vendor
├── README.md                         # The index: platform facts, requirement table, export rule, provenance, redaction
├── junit.xml                         # One testcase per AICR conformance check, named by check
├── e2e.log                           # The same checks in `go test -v` shape
└── evidence/                         # One page per requirement a non-skipped check evidenced (no index.md)
    ├── gang-scheduling.md
    └── ...
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
# report rendered into the submission directory (below) and checked by the
# pinned upstream validator. The vendor block comes from --config.
aicr validate -r recipe.yaml -s snapshot.yaml --phase conformance \
  --config aicr-config.yaml --cncf-submission --evidence-dir ./submission
```

`--cncf-submission` and `--evidence-dir` require each other. A submission run
requires `--recipe` (the checks are selected from it), the conformance
phase and `spec.validate.evidence.cncf.vendor` in `--config`, and refuses
`--no-cluster` and `--skip-check`: the evidence comes from checks that ran on
a live cluster against the whole recipe. Its exit status adds the requirement
aggregation above to the per-check rule: an applicable MUST that is not
established fails the run, the aggregation never fails it on a SHOULD, and a
failed or crashed check fails it under `--fail-on-error` (the default) as in
any run. The directory it writes is described next.

## Submission directory

One `aicr validate --cncf-submission` run writes everything a vendor submits
as `vX.Y/<product>/` in
[cncf/k8s-ai-conformance](https://github.com/cncf/k8s-ai-conformance): no
hand-written PRODUCT.yaml, no hand-run checks, no hand redaction. Upstream
requires `PRODUCT.yaml` and recommends `junit.xml` and `e2e.log` from v1.37;
`README.md` and the `evidence/` pages are AICR's packaging choice, and no
`index.md` or `platform-configuration.md` is written (README.md is the index
and carries the platform facts).

### Vendor metadata

`spec.validate.evidence.cncf.vendor` carries the
PRODUCT.yaml metadata (`name`, `platformName`, `platformVersion`,
`description`, `contactEmail`, `websiteUrl`, `documentationUrl`,
`productLogoUrl`, `k8sConformanceUrl`; `repoUrl` optional), so the command
stays at two flags. A missing field, a bracketed placeholder, a relative URL
or a `k8sConformanceUrl` outside `https://github.com/cncf/k8s-conformance/tree/(master or main)/vX.Y/<product>`
is refused with `INVALID_REQUEST` before any check runs. Whether the URLs
answer and whether `name` matches a CNCF member in the landscape is the
upstream validator's check at the end of the run. See
[`evidence.cncf.vendor`](../../user/cli-config.md).

### Statuses are never typed

Each PRODUCT.yaml row comes from the
[requirement aggregation](#requirement-status): a requirement `PASSED` by a
complete proof path is `Implemented`, citing `evidence/<page>.md` plus
`junit.xml#<check>` and `e2e.log#<check>` for each check on the path; a
conditional MUST in `notApplicable` is `N/A` with the justification in
`notes`, citing only its page; everything else is `Not Implemented` with the
aggregate's reason in `notes`, which for a SHOULD means "not claimed" and for
an applicable MUST means the run is incomplete. The upstream id, description
and level are copied verbatim from the checklist, as upstream requires.

### Manual-attestation rule

An `OBSERVED` result is never cited as
automated proof. A requirement the run only observed (today
`cluster_autoscaling` on a platform whose autoscaler the check saw but did
not drive) is established only by `spec.validate.evidence.cncf.documented`,
which names public documentation URLs and a note: PRODUCT.yaml then renders
`Implemented` citing the documentation and the page as the observation
record, with no `junit.xml` or `e2e.log` reference, and the README's result
column keeps `OBSERVED`. The attestation applies to that status alone: a
`PASSED` requirement needs none, and a `FAILED` or `NOT TESTED` MUST is not
papered over by documentation and still fails the run.

### Export rule

The raw CTRF report keeps every real verdict. `junit.xml`
and `e2e.log` carry one testcase per conformance-phase check of the run
(deployment and performance results of a `--phase all` run are not
exported: they evidence no CNCF requirement), named by
check (AICR names, not upstream test names, since the upstream suite cannot
exercise KAI gang scheduling and AICR's checks are the evidence), and every
PRODUCT.yaml reference carries a `#<check>` fragment, which the validator
resolves before its upstream-name map. The validator fails a referenced
artifact on any failing testcase anywhere in it, so a non-passing check whose
requirement does not hold against the run is exported as `skipped` with the
original verdict and reason in the message and is referenced by no row: a
check behind a Not Implemented SHOULD, one behind a requirement overridden to
`N/A` (message `not applicable: <justification>; original verdict ...`), and
a supporting-evidence check (`dra-support` from v1.36). A skipped check is
exported skipped with its reason code whatever it evidences. A failed or
crashed check behind an applicable MUST stays a failure, and so does one
that evidences no requirement (`platform-health`): the first leaves the MUST
incomplete through the aggregation unless another proof path or a documented
observation establishes it, and in every case the per-check rule and the
validator reject the directory. The README lists every rewritten testcase.

### Write, gate, move

The directory is written under
`<evidence-dir>.incomplete/` laid out as the validator reads it
(`docs/AIConformance-X.Y.yaml` from the embedded checklist beside
`vX.Y/<name>/PRODUCT.yaml`), redacted in one pass, then checked with the
pinned upstream validator, `cncf/k8s-ai-conformance` `scripts/validate.go`
compiled into the binary at the commit the README records
(`pkg/evidence/cncf/upstreamvalidator`, vendored by
`tools/vendor-cncf-validator` with a digest pin). Only when every applicable
MUST is established and the validator accepts the directory is it moved to
`--evidence-dir`, so a `PRODUCT.yaml` at that path is complete by
construction (an empty directory that existed before an incomplete run
stays empty).
Otherwise nothing reaches it: when a MUST is not established the validator is
not run (the artifacts hold a failure it would reject) and the run exits
non-zero naming the MUST; when the validator rejects the directory the run
exits non-zero with its report (`INTERNAL`), and when it cannot start because
the CNCF landscape is unreachable it exits with `SERVICE_UNAVAILABLE`. The
staged tree stays in either case for inspection and must be removed before
the next run, which exits 2 while it exists (its README.md Validation section
says why it was kept); `--evidence-dir` itself must be absent or empty, and a
path with a `vX.Y/` parent segment is refused because the validator would
read that segment as the checklist version. The whole write, including the
validator's run, is bounded by `defaults.CNCFSubmissionTimeout` (10 minutes);
exceeding it exits `TIMEOUT` (5) with the staged tree kept, as for a
rejection.

**Recovering from a transient validator failure.** A staged tree whose
README.md Validation section reads "not run; the validator could not start"
or "interrupted" is a complete, redacted directory lacking only the
validator's acceptance; every check has run. Rather than re-running the
checks, validate it by hand from the staging root, where the layout is the
one the validator reads (`docs/AIConformance-X.Y.yaml` beside
`vX.Y/<name>/PRODUCT.yaml`):

```bash
EVIDENCE_DIR="submission"                # the --evidence-dir you passed
VALIDATOR_SRC="$HOME/src/k8s-ai-conformance"  # cncf/k8s-ai-conformance at the pinned commit
PRODUCT="v1.35/nvidia-aicr"             # the vX.Y/<name> directory under the staging root
cd "${EVIDENCE_DIR}.incomplete"
go run -tags validate "${VALIDATOR_SRC}/scripts/validate.go" "${PRODUCT}/PRODUCT.yaml"
mv "${PRODUCT}" "../${EVIDENCE_DIR}" && cd .. && rm -r "${EVIDENCE_DIR}.incomplete"
```

The README's Validation section still records the interrupted outcome; say
so beside it, or re-run `aicr validate` once the validator is reachable. A
tree whose README reads "rejected" needs a config or cluster fix and a new
run, never an edit. The validator
needs network access: it fetches the landscape to check the vendor name and
requests every URL the submission cites. A refresh of the pinned revision
re-checks the assumptions this design relies on (fragments on every artifact
reference, the `e2e.log` result-line shape, whole-artifact failure, the
required metadata fields, `ai_inference` outside the auto-tested map), listed
in [#3605](https://github.com/NVIDIA/aicr/issues/3605).

### Redaction

Check output is kept and the identifiers inside it are aliased
consistently across every file by `pkg/evidence/redact` (`TextScrubber`,
policy `minimal` v9): the cluster's node names from the snapshot, container
runtime IDs, UUIDs (Kubernetes UIDs, GPU UUIDs, cloud subscription IDs),
IPv4, IPv6 and MAC addresses, AWS account IDs in ARNs; a JWT is removed. The
vendor's own URLs are preserved. The README states what the pass changed.
The scrubber is not a guarantee that nothing sensitive remains (a free-form
hostname that is not a node name passes through, and so does a node absent
from a partial or redacted snapshot, since the node list comes from the
snapshot), so the operator reviews the directory before submitting it; the
README says so. Review, do not edit: a change made after the run is not
covered by the README's validator outcome; fix the config or the cluster and
re-run. A staged tree is kept only once the
redaction pass has completed (because a MUST was not established or the
validator rejected it); an error or cancellation before that point removes
the whole tree, so no raw check output is left behind. The README's
Validation section states whether the validator accepted the directory,
rejected it, or was not run.

### Optional upstream suite

Running the kubernetes-sigs/ai-conformance
suite (pinned at `fd9ba2d`) remains optional independent verification with
its own provenance; it is not run by `aicr validate`, and upstream testcase
names appear in a submission only for tests actually executed from that
suite, in their own artifacts. A hook to run it from the submission command
is a follow-up to #3605.

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
| `ai_service_metrics` | `ai-service-metrics` | Deploys a CPU stub that exposes a request counter, a latency histogram and a queue-depth gauge labelled with a per-run id, plus a Service and a ServiceMonitor carrying the `release=kube-prometheus-stack` label; waits for Prometheus to load the ServiceMonitor into its scrape pools (`/api/v1/scrape_pools`, so prometheus-operator's reconcile schedule does not consume the collection budget), then for Prometheus to collect the baseline, sends exactly 10 requests through the Service, and requires the collected counter to equal the baseline plus 10. A higher value fails (something else sent traffic), a ServiceMonitor Prometheus does not load within the monitor-load timeout fails, and one loaded but not collected within the collection timeout fails, each with its own message. `pod-autoscaling` waits for its PodMonitor the same way before judging the custom metrics pipeline, when the recipe declares `kube-prometheus-stack`. The check does not depend on a model being served |
| `ai_inference` | `inference-gateway` | GatewayClass Accepted, Gateway Programmed and the InferencePool CRD Established for the inference gateway the recipe declares (`agentgateway` by default, or the component whose `componentRef` sets `overrides.inferenceGateway`, see [Declaring the inference gateway](../../integrator/data-extension.md#declaring-the-inference-gateway)), plus the gateway's network exposure and data-plane readiness; a recipe that declares no gateway skips. Then a traffic phase mirroring the upstream `TestGatewayAPIInference` on that same Gateway: two CPU backends (Deployment + Service each, answering with their own name) and one HTTPRoute in a per-run namespace with an explicit per-run hostname (`ai-conformance-<token>.conformance.invalid`, sent as the probe's `Host` header) and a per-run path prefix on both rules (`/ai-conformance-<token>`), so a tenant route can outrank the probe only by matching that exact hostname; routes already attached are recorded and named on failure. Its first rule sends requests carrying `OpenAI-Project: ai-conformance-canary` to the alternate backend and its default rule splits the rest 70/30. The route must be `Accepted` with `ResolvedRefs` by the recipe's Gateway (its HTTP listener must allow routes from other namespaces, as the shipped agentgateway manifest does), and a probe pod posts to the Gateway's HTTP listener (the proxy Service for a `service-endpoints` data plane, the status address for `gateway-address`): all 20 header requests must reach the canary backend, and within 10 rounds of 500 unmatched requests one round must land the primary backend within 5 percentage points of 70% with no request failing or reaching a non-test backend. The per-round counts are recorded in the `Gateway traffic verdict` artifact; a route whose header rule is gone or whose weights are swapped fails. The check needs a 15m catalog timeout for the phase's bounds (backends available 3m, route accepted 2m, probe 6m); a passing run takes 1-3 minutes |
| `robust_controller` | `robust-controller` | A schema-valid but webhook-invalid CR per operator is submitted with server-side dry-run (nothing persists); PASS requires a denial naming the operator's own webhook entry. CRD-schema rejections (`Required value`) and denials from other admission webhooks (e.g. a cluster policy webhook) are classified as not demonstrating the operator's webhook and FAIL. Then reconciliation: On the Kubeflow Trainer path `robust-controller` asserts the TrainJob CRD is `Established=True` and creates a valid TrainJob on the default `torch-distributed` `ClusterTrainingRuntime` in a per-run namespace — one node, no accelerator, the probe image with a command that exits 0 — and requires `Complete=True` within 3 min, failing on `Failed=True` or a missing runtime (the upstream `TestRobustCRDControllerOperation` shape). The Dynamo path records the existing `DynamoGraphDeployment` conditions as an observation and creates no CR: a minimal one has no terminal condition to reach without a served model. A recipe shipping both operators is evidenced by the Kubeflow path |
| `driver_runtime_management` | `driver-runtime-management` | `driver-runtime-management` mirrors the upstream `TestAcceleratorDriverRuntimeManagement` (KAR-0001). A pod built from a vanilla OS image (`docker.io/library/ubuntu:22.04` by default; `AICR_DRIVER_RUNTIME_IMAGE` replaces the reference for mirrors, and `AICR_VALIDATOR_IMAGE_REGISTRY` does not rewrite it) requests one GPU the way `secure-accelerator-access` does: the recipe's allocation policy selects DRA ResourceClaim or `nvidia.com/gpu: 1`, the device-plugin pod is constrained to the detected device-plugin nodes, the DRA pod is left to the scheduler, and the context tolerations apply. Inside, the probe must see exactly one `/dev/nvidia[0-9]*` device, find an `nvidia-smi` the container runtime configuration injected (PATH, `/usr/bin`, or GKE's `/usr/local/nvidia` tree), and get a numeric driver version from NVML. The platform's advertised version for the node the pod landed on is read from the `gpu.nvidia.com` ResourceSlice `driverVersion` device attribute first and the GPU Feature Discovery label `nvidia.com/cuda.driver-version.full` (or the legacy `nvidia.com/cuda.driver.major`/`minor`/`rev` triple) otherwise; the two must agree at shared numeric precision (`580.65` matches `580.65.06`, `580.65.6` matches `580.65.06`). A node that advertises no version is recorded as a warning and the probe alone decides; a mismatch, a missing tool, a non-functional driver or incomplete probe output fails. Skips on Slinky Slurm recipes, where the NodeSet holds every GPU |
| `cluster_autoscaling` | `cluster-autoscaling` | Karpenter: drives a scale-up and records it (scale-down is not observed). EKS, AKS and GKE: observations — autoscaler presence, GPU pool membership, the `cluster-autoscaler-status` ConfigMap and recent autoscaler events — with no scale event driven (a definitive NotFound is recorded as absence, a failed read as unread, never as absence). No passing path is behavioral proof, so every pass carries the observation-only marker (`extra.observationOnly: "true"`): the completed check reports `passed`, the requirement renders `OBSERVED (cluster-autoscaling observed, not demonstrated)` rather than `PASSED`, and the MUST is not established by it — a submission run exits non-zero naming `cluster_autoscaling` as observed unless `spec.validate.evidence.cncf.notApplicable` declares it with a justification (a cluster with no autoscaler), which renders `NOT APPLICABLE`, or `spec.validate.evidence.cncf.documented` attests it by the provider's autoscaler documentation (a cluster whose autoscaler the check saw but did not drive), which keeps `OBSERVED` and establishes it. One rule fails: on EKS, GPU nodes with no `eks.amazonaws.com/nodegroup` label, no available in-cluster Cluster Autoscaler Deployment and no status ConfigMap (and no Karpenter) fail — `NotFound` when every read answered NotFound, the read's classified code when a Deployment was unavailable or a read could not land — because nothing confirmed an autoscaler, so there is nothing to observe; the remedy for a platform without workload-driven autoscaling is a `spec.validate.evidence.cncf.notApplicable` justification. Otherwise the `cluster_autoscaling` claim rests on provider documentation or that justification. A providerID with no probe (bare metal, Kind, KWOK) skips as "no autoscaler probe for this provider": a missing probe, not product nonapplicability. A fixed-size cluster declares the conditional MUST not applicable through `spec.validate.evidence.cncf.notApplicable`; the constrained claim is [#3428](https://github.com/NVIDIA/aicr/issues/3428) |
| `workload_sandboxing` | `workload-sandboxing` | Mirrors the upstream `TestWorkloadSandboxing` (KAR-0020). Finds a sandboxed `RuntimeClass` (name or handler containing `gvisor`, `runsc`, `kata`, `sandboxed`, `quark` or `krun`, the first by name; or the one `AICR_SANDBOX_RUNTIME_CLASS` names, accepted as given) and runs a CPU-only probe pod on it from a vanilla image (`docker.io/library/ubuntu:22.04` by default; `AICR_SANDBOX_PROBE_IMAGE` replaces the reference for mirrors, and `AICR_VALIDATOR_IMAGE_REGISTRY` does not rewrite it), then the same probe unsandboxed, pinned by `spec.nodeName` to the node the sandboxed pod landed on. The sandboxed pod must pass the four upstream probes (no host daemon such as `kubelet`, `containerd`, `dockerd`, `crio` or `systemd-journal` visible in `/proc`; `/dev/mem` and `/dev/kmem` unreadable; `/etc/kubernetes`, `/var/lib/kubelet` and `/var/log/pods` absent; no host bridge, CNI or bond interface listed) and report a kernel identity (`uname -r`, `/proc/version`, `boot_id`; a field either pod cannot read is ignored) different from the control pod's, because a plain runc pod passes the four probes too. Every failing probe is named in the verdict. No accelerator is requested: a sandbox runtime on a CPU pool satisfies the requirement. A cluster with no sandboxed `RuntimeClass` **fails** with `unmet prerequisite: no sandboxed RuntimeClass` where upstream skips (see the level note above). The check never skips, Slinky Slurm recipes included: the NodeSet holding every GPU does not affect a CPU-only probe. No embedded recipe declares it yet (none ships a sandboxed `RuntimeClass`), so the row renders `NOT TESTED (not run)` at SHOULD until the #3615 runtime decision provides one and the recipes declare it; a cluster that already has gVisor or Kata can declare it on a recipe copy today |
