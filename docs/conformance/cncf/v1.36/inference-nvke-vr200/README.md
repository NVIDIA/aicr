# AICR Inference Stack on NVKE (VR200) — conformance evidence

This evidence set was collected on a Kubernetes v1.36 NVKE tenant cluster with
NVIDIA VR200 accelerators (Vera Rubin NVL72 trays, 4 GPUs per node, 2 nodes),
running the AICR **inference** stack: the Gateway API surface from
`agentgateway`, on top of GPU Operator, the NVIDIA DRA driver, KAI Scheduler,
DCGM exporter and kube-prometheus.

**This is not an upstream CNCF submission** (no `PRODUCT.yaml`). NVKE is
already certified at [`../nvke/`](../nvke/) — platformVersion 0.22.0, on two
ARM64 **GB200** workers. This directory does not modify that submission.

It is the inference sibling of [`../trainer-nvke-vr200/`](../trainer-nvke-vr200/),
which covers the same hardware under the training profile. The split follows
the one already in the repository between
[`v1.35/nim-eks/`](../../v1.35/nim-eks/) and
[`v1.35/trainer-eks-gb200/`](../../v1.35/trainer-eks-gb200/): a training stack
records `ai_inference` as an absent prerequisite, an inference stack
demonstrates it.

Collected with the `vr200-nvke-ubuntu-inference` recipe
([NVIDIA/aicr#3679](https://github.com/NVIDIA/aicr/pull/3679)).

## Results

These are **check results**, not checklist verdicts. AICR's
`--cncf-submission` renderer produces the verdicts, and the run that produced
these files predates it — see [How this was collected](#how-this-was-collected).

The v1.36 checklist carries eight MUST requirements:

| # | Requirement (v1.36 MUST) | Feature | Check result | Evidence |
|---|---|---|---|---|
| 1 | `gang_scheduling` | Gang Scheduling (KAI Scheduler, two-phase barrier) | PASS | [gang-scheduling.md](evidence/gang-scheduling.md) |
| 2 | `secure_accelerator_access` | Secure Accelerator Access | PASS | [secure-accelerator-access.md](evidence/secure-accelerator-access.md) |
| 3 | `accelerator_metrics` | Accelerator Metrics (DCGM Exporter) | PASS | [accelerator-metrics.md](evidence/accelerator-metrics.md) |
| 4 | `ai_service_metrics` | AI Service Metrics | PASS | [ai-service-metrics.md](evidence/ai-service-metrics.md) |
| 5 | `ai_inference` | Inference API Gateway (agentgateway v1.5.0) | PASS | [inference-gateway.md](evidence/inference-gateway.md), [header routing and weighted traffic](evidence/gateway-routing-behavior.txt) |
| 6 | `robust_controller` | Robust AI Operator (Kubeflow Trainer) | PASS — admission only, reconciliation not demonstrated | [robust-operator.md](evidence/robust-operator.md) |
| 7 | `pod_autoscaling` | Pod Autoscaling (HPA + GPU metrics) | PASS — scale-up only | [pod-autoscaling.md](evidence/pod-autoscaling.md) |
| 8 | `cluster_autoscaling` | Cluster Autoscaling | NOT TESTED — bare metal, no cloud node group | [cluster-autoscaling.md](evidence/cluster-autoscaling.md) |

**Supporting evidence**, which the v1.36 checklist does not carry as a
requirement:

| Requirement | Feature | Check result | Evidence |
|---|---|---|---|
| `dra_support` | Dynamic Resource Allocation | PASS — API and ResourceSlices only, no allocation test | [dra-support.md](evidence/dra-support.md) |

`dra_support` was a requirement at v1.35 only. Later checklists drop it and
AICR renders it as supporting evidence rather than a requirement row
(`pkg/evidence/cncf/requirements.go`).

So, in the collector's sections: **five of the eight requirements pass
outright, two pass with the qualifications below, and one was not tested.**
`cluster_autoscaling` was skipped rather than declared not-applicable, so
nothing here establishes it either way.

The v1.36 checklist also carries six SHOULDs — `driver_runtime_management`,
`gpu_sharing`, `virtualized_accelerator`, `advanced_inference_ingress`,
`high_performance_networking` and `disaggregated_inference`. **None of them is
tested here**, and none has a section in this set. AICR has a check for one,
`driver_runtime_management`, added in
[#3683](https://github.com/NVIDIA/aicr/pull/3683) after this collection ran;
the other five have no AICR check.

## How this was collected

**Command**, run 2026-11-06 against the tenant cluster:

```
aicr validate -k <tenant.kubeconfig> \
  --recipe <resolved vr200-nvke-ubuntu-inference> \
  --snapshot <cluster snapshot> \
  --data <recipes/ from the #3679 branch> \
  --phase conformance --cncf-submission \
  --evidence-dir <dir> --timeout 30m -o <ctrf.json>
```

- **Recipe** — `vr200-nvke-ubuntu-inference`, from the branch that became
  [#3679](https://github.com/NVIDIA/aicr/pull/3679), passed as a resolved file
  with `--data` pointing at that branch's `recipes/` tree (497 files).
- **Binary** — built from source at `f12ed731d`, which is on the
  `feat/vr200-nvke-training` branch of
  [#3620](https://github.com/NVIDIA/aicr/pull/3620) and not on `main`: that PR
  was squashed into `fe2618f46`, so `main` has the code but not this commit.
  It reports `aicr version dev (commit: unknown, date: unknown)`, so the files
  carry no version string of their own; this README is the record of which
  build made them.
- **Features** — no `--feature`, so all nine were collected: `dra-support`,
  `gang-scheduling`, `secure-access`, `accelerator-metrics`,
  `ai-service-metrics`, `inference-gateway`, `robust-operator`,
  `pod-autoscaling`, `cluster-autoscaling`.
- **Outcome** — exit status 0. Every section completed and none reported FAIL.

**Three of those flags did nothing.** At `f12ed731d` the `validate` action
short-circuits on `--cncf-submission` and returns `runCNCFSubmission` before it
parses the phase selection or resolves the snapshot path
(`pkg/cli/validate.go`), and `runCNCFSubmission` never touches the output flag.
So `--phase conformance` ran no validator phase and therefore no check,
`--snapshot` was never read — the collector queried the live cluster through
`-k` — and no CTRF file was written at `-o`. All three are recorded above
because they were typed, not because they did anything.

**The recipe was read for two things.** The run log states the first:
*"loading recipe to resolve the GPU allocation policy for CNCF evidence
collection"*, resolving `gpuAllocationPolicy=device-plugin-extended-resource`.
The second is silent — `CollectCNCFEvidence` calls
`ResolveInferenceGatewayProfile` on the recipe's component refs
(`pkg/client/v1/evidence.go`) and threads the declared Gateway into the
collector. That selection is what makes `ai_inference` PASS here and SKIP under
the training recipe; see
[What the gateway result rests on](#what-the-gateway-result-rests-on).

`f12ed731d` predates [#3666](https://github.com/NVIDIA/aicr/pull/3666)
(`bd08bec25`), which replaced the shell collector at
`pkg/evidence/cncf/scripts/collect-evidence.sh` with rendering from the
conformance run and made check selection follow the recipe. **These files are
that shell collector's output.** The same command on current `main` would not
reproduce them:

- Check selection now comes from the recipe, and `vr200-nvke-ubuntu-inference`
  declares neither `gang-scheduling` nor `robust-controller`, so neither
  section would be produced at all.
- Checks of the same name now test more: `robust-controller` requires a valid
  TrainJob reconciled to `Complete`, `pod-autoscaling` adds a one-replica
  negative control and a scale-down, and `ai-service-metrics` looks for a
  request counter rather than scraped training metrics.

Read the sections here as observations of the platform, not as results of the
checks that carry those names today. Re-collecting against a current build is
what would turn them into current validation results.

## What the gateway result rests on

`ai_inference` is the requirement this profile exists to demonstrate, and it is
the one AICR gates on the **recipe** rather than the cluster. A recipe that
declares no gateway records

```
**Result: SKIP (prerequisite absent)** — the recipe declares no inference gateway component.
```

even when a healthy Gateway is Programmed, by design: *"a Gateway left over
from another install is not evidence for this recipe."* The same cluster
recorded that SKIP under the training recipe and records PASS here, because
`vr200-nvke-ubuntu-inference` declares `agentgateway-crds` and `agentgateway`.

Beyond the collector's own check — GatewayClass Accepted, Gateway Programmed,
and the InferencePool CRDs registered (the collector's result line calls them
Established; what it captured is a `kubectl get crds` listing, which shows
registration and creation time, not the `Established` condition) — the Gateway
was exercised directly: header
routing selected the expected backend both ways, and a configured 80/20 split
produced 82/18 across 100 requests with no failures. Those probes are not
collector output; they are recorded in
[gateway-routing-behavior.txt](evidence/gateway-routing-behavior.txt) with the
objects that produced them in
[gateway-routing-configuration.json](evidence/gateway-routing-configuration.json).

## Results that are narrower than they look

**`robust_controller` shows the operator admitting, not reconciling.** The
check found Kubeflow Trainer 2.2.0 running with its three CRDs registered, and
verified that the validating webhook rejected a schema-valid but rule-invalid
TrainJob with a webhook-attributed message. That TrainJob was submitted as a
server-side dry run, so nothing was persisted and nothing was reconciled.
AICR's own requirement text for the Kubeflow path asks for a valid TrainJob
reconciled to `Complete`; this set does not carry that, and the PASS above
should be read as admission control working, not the controller reconciling.

**The operator is also not this recipe's.** Kubeflow Trainer is on this cluster
from the training bring-up; `vr200-nvke-ubuntu-inference` declares no AI
operator, matching `vr200-rke2-ubuntu-inference`. Today's
`robust_controller_check.go` takes its subject from the recipe —
`kubeflow-trainer`, else `dynamo-platform`, else skip — so a cluster built from
this recipe alone records a skip here rather than any result.

**`pod_autoscaling` records a scale-up only.** The HPA read `gpu_utilization`
through the Prometheus Adapter and took the Deployment from one replica to two
under load; that is in [Pods After Scale-Up](evidence/pod-autoscaling.md). The
current check also wants a one-replica negative control and a scale-down back
to one. Neither is in this set.

**`dra_support` rests on API and ResourceSlice checks only.** The section
records the served `resource.k8s.io/v1` API, the DeviceClasses, the DRA driver
pods and the two `compute-domain.nvidia.com` ResourceSlices, one per worker.
Nothing was allocated: under the recipe's `device-plugin-extended-resource`
policy the collector deploys no claim pod and marks the behavioral full-GPU
ResourceClaim test **not applicable**, because whole GPUs are
device-plugin-allocated through the `nvidia.com/gpu` extended resource. The
file says so, and adds that pool-generation and device-taint validation of
those slices belongs to the Go validator rather than the script — which, per
the section above, did not run on this path. The collector carries no
multi-node NVLink test of any kind, so nothing here speaks to MNNVL; that claim
for this hardware lives in
[`../trainer-nvke-vr200/`](../trainer-nvke-vr200/), which measured 996 GB/s
across both trays.

## The cluster was not deployed from this recipe

The evidence was **collected against** `vr200-nvke-ubuntu-inference`, not
produced by deploying it. This tenant cluster was assembled by hand and the
Gateway installed from the component charts; `aicr validate` then read the
cluster and rendered the evidence with that recipe as the reference. The two
diverge in both directions: the cluster carries `kubeflow-trainer` and
`network-operator`, which this leaf does not declare, and it lacks `nvsentinel`,
`nodewright-operator`, `nodewright-customizations`, `dra-node-labeler` and
`k8s-ephemeral-storage-metrics`, which it does.

None of those five feeds any of the requirements above, so the observations
stand on their own. But a `--phase conformance` validator run against this
recipe on this cluster would fail `platform-health`, which expects every
declared component present, and that is expected rather than a conformance
result. That is **not** why no `platform-health` section appears here, though:
the shell collector has no such section, and as noted above no validator check
ran at all on this path.

The versions diverge too. The cluster runs the **NVIDIA DRA driver 25.8.1**,
while the recipe pins the upstream `dra-driver-nvidia-gpu` chart at **0.5.1**
(chart version and appVersion alike) — a different distribution of the driver
on a different version scheme, not an older or newer build of the same one.

What this set shows is that, in the collector's sections, five of the eight
v1.36 requirements pass outright and two more pass with the qualifications
above, with `cluster_autoscaling` untested. Those are that collector's
observations and not checklist verdicts: this is not a conformance claim, and
it does not show that a cluster built from this recipe alone reproduces any of
it.

## Tested platform

| Component | Version |
| --- | --- |
| Kubernetes | v1.36.3 |
| Ubuntu | 26.04.1 LTS |
| Kernel (workers) | `7.0.0-2022-nvidia-bos-64k` |
| Containerd | 2.3.0 |
| OVN-Kubernetes | 2.4.2 |
| GPU Operator | v26.7.2 |
| NVIDIA driver | 615.71.09 |
| NVIDIA DRA driver | 25.8.1 |
| NVIDIA Network Operator | 26.7.1 |
| Node Feature Discovery | 0.20.1 |
| KAI Scheduler | v0.16.9 |
| Kubeflow Trainer | 2.2.0 |
| Agentgateway | v1.5.0 |
| cert-manager | v1.20.2 |
| kube-prometheus-stack chart | 84.4.0 |
| Prometheus Adapter chart | 5.3.0 |

The workers run the `nvidia-bos-64k` kernel line. On the generic
`nvidia-64k` kernel no driver brings the GPUs up at all — PRI reads return
`0xbadf5720`, FSP and GSP never hand off, and every GPU fails probe with
`-EIO`. That is a node-image property, not a recipe one, and is recorded in
full in [`../trainer-nvke-vr200/`](../trainer-nvke-vr200/).

## Caveat

Cross-node pod networking on this cluster is degraded: every node-to-node path
caps at 1–2 MB/s on 400 Gb/s links, and the fault is at the underlay rather
than the overlay — MTU, latency, loss and policing have all been ruled out. It
did not affect these results, which are single-request HTTP through a Gateway
and per-node metrics and scheduling, but anything moving bulk data between pods
on this cluster would be affected.

The evidence retains this tenant cluster's private addressing. Pod IPs and
Service ClusterIPs appear in eight of the reports — in `kubectl` output, in
endpoint listings and in Prometheus labels. They are RFC 1918 addresses inside
one tenant cluster's overlay: they route nowhere outside it and name no site,
rack or host. Tenant node names, GPU UUIDs, DCGM `pci_bus_id` labels and the
cluster's `providerID` are retained on the same reasoning, and
[`../trainer-nvke-vr200/`](../trainer-nvke-vr200/) retains the same classes of
value. Commands, timestamps, versions, outcomes and measured values are
retained verbatim.
