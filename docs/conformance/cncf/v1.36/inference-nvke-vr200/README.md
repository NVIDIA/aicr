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

| # | Requirement | Feature | Result | Evidence |
|---|-------------|---------|--------|----------|
| 1 | `dra_support` | Dynamic Resource Allocation | PASS | [dra-support.md](evidence/dra-support.md) |
| 2 | `gang_scheduling` | Gang Scheduling (KAI Scheduler, two-phase barrier) | PASS | [gang-scheduling.md](evidence/gang-scheduling.md) |
| 3 | `secure_accelerator_access` | Secure Accelerator Access | PASS | [secure-accelerator-access.md](evidence/secure-accelerator-access.md) |
| 4 | `accelerator_metrics` | Accelerator Metrics (DCGM Exporter) | PASS | [accelerator-metrics.md](evidence/accelerator-metrics.md) |
| 5 | `ai_service_metrics` | AI Service Metrics | PASS | [ai-service-metrics.md](evidence/ai-service-metrics.md) |
| 6 | `ai_inference` | Inference API Gateway (agentgateway v1.5.0) | PASS | [inference-gateway.md](evidence/inference-gateway.md), [header routing and weighted traffic](evidence/gateway-routing-behavior.txt) |
| 7 | `robust_controller` | Robust AI Operator (Kubeflow Trainer, webhook-attributed) | PASS | [robust-operator.md](evidence/robust-operator.md) |
| 8 | `pod_autoscaling` | Pod Autoscaling (HPA + GPU metrics) | PASS | [pod-autoscaling.md](evidence/pod-autoscaling.md) |
| 9 | `cluster_autoscaling` | Cluster Autoscaling | SKIP — bare metal, no cloud node group | [cluster-autoscaling.md](evidence/cluster-autoscaling.md) |

**8 PASS, 1 SKIP, 0 FAIL.** Collected 2026-11-06 with
`aicr validate --phase conformance --cncf-submission`.

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

Beyond the collector's own check (GatewayClass Accepted, Gateway Programmed,
InferencePool CRD Established), the Gateway was exercised directly: header
routing selected the expected backend both ways, and a configured 80/20 split
produced 82/18 across 100 requests with no failures. Those probes are not
collector output; they are recorded in
[gateway-routing-behavior.txt](evidence/gateway-routing-behavior.txt) with the
objects that produced them in
[gateway-routing-configuration.json](evidence/gateway-routing-configuration.json).

## Two results that are narrower than they look

**`robust_controller` credits an operator this recipe does not ship.** The
check found Kubeflow Trainer 2.2.0 and drove a TrainJob through it. Kubeflow
Trainer is on this cluster from the training bring-up; the inference recipe
declares no AI operator, matching `vr200-rke2-ubuntu-inference`, which declares
none either. The capability is demonstrated on the platform, but a cluster
built from this recipe alone would have no operator for the check to find.

**`dra_support` passed without its MNNVL subtest.** The DRA API, the
`compute-domain.nvidia.com` ResourceSlices on both workers and the DeviceClass
are all present and recorded. The behavioral IMEX-channel subtest did not run,
because it requires a node carrying the `nvidia.com/gpu.clique` label and no
pod was holding the ComputeDomain's channel at collection time — with IMEX
down, each GPU reports its own cliqueId and GFD publishes no node-level clique.
The fabric itself was healthy throughout (`Fabric State: Completed`, `Global
Fabric Manager State: Connected`). The multi-node NVLink claim for this
hardware lives in [`../trainer-nvke-vr200/`](../trainer-nvke-vr200/), which
measured 996 GB/s across both trays; this profile does not restate it.

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

The evidence carries no internal IP addresses. Tenant node names, GPU UUIDs,
DCGM `pci_bus_id` labels and the cluster's `providerID` are retained, matching
[`../trainer-nvke-vr200/`](../trainer-nvke-vr200/) exactly; none identifies a
site or a host outside this tenant cluster. Commands, timestamps, versions,
outcomes and measured values are retained verbatim.
