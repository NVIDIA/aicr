# AICR Training Stack on NVKE (VR200) — conformance evidence

This evidence set was collected on a Kubernetes v1.36 NVKE tenant cluster with
NVIDIA VR200 accelerators (Vera Rubin NVL72 trays, 4 GPUs per node, 2 nodes),
running the AICR **training** stack: GPU Operator, NVIDIA DRA driver, KAI
Scheduler, Kubeflow Trainer, DCGM exporter and kube-prometheus.

**This is not an upstream CNCF submission** (no `PRODUCT.yaml`). NVKE is
already certified at [`../nvke/`](../nvke/) — platformVersion 0.22.0, on two
ARM64 **GB200** workers using the `gb200-nvke-ubuntu-training-kubeflow` recipe.
This directory does not modify that submission.

What this set adds is **a different accelerator on the same platform**: Vera
Rubin (VR200) rather than Grace-Blackwell, using the
`vr200-nvke-ubuntu-training` recipe on NVKE 0.23.0. The versions otherwise
line up closely with the certified set — Kubernetes v1.36.3, GPU Operator
v26.7.2, KAI Scheduler v0.16.9, two workers, eight GPUs — so the delta under
test is the accelerator and the platform bits VR200 needs.

Two VR200-specific differences are worth recording:

- **Multi-node NVLink comes from a DRA ComputeDomain**, not a cloud fabric, and
  the GPU driver is installed by GPU Operator because NVKE's node image ships
  none. Both are covered under `dra_support` below.
- `cluster_autoscaling` records a fail-closed SKIP on confirmed absence rather
  than passing, because this is bare metal with no cloud node group to scale.

## Results

| # | Requirement | Feature | Result | Evidence |
|---|-------------|---------|--------|----------|
| 1 | `dra_support` | Dynamic Resource Allocation | PASS | [dra-support.md](evidence/dra-support.md) |
| 2 | `gang_scheduling` | Gang Scheduling (KAI Scheduler, two-phase barrier) | PASS | [gang-scheduling.md](evidence/gang-scheduling.md) |
| 3 | `secure_accelerator_access` | Secure Accelerator Access | PASS | [secure-accelerator-access.md](evidence/secure-accelerator-access.md) |
| 4 | `accelerator_metrics` | Accelerator Metrics (DCGM Exporter) | PASS | [accelerator-metrics.md](evidence/accelerator-metrics.md) |
| 5 | `ai_service_metrics` | AI Service Metrics (PyTorch training) | PASS | [ai-service-metrics.md](evidence/ai-service-metrics.md) |
| 6 | `ai_inference` | Inference API Gateway | SKIP — not part of the training profile (agentgateway confirmed absent) | [inference-gateway.md](evidence/inference-gateway.md) |
| 7 | `robust_controller` | Robust AI Operator (Kubeflow Trainer, webhook-attributed) | PASS | [robust-operator.md](evidence/robust-operator.md) |
| 8 | `pod_autoscaling` | Pod Autoscaling (HPA + GPU metrics) | PASS | [pod-autoscaling.md](evidence/pod-autoscaling.md) |
| 9 | `cluster_autoscaling` | Cluster Autoscaling | SKIP — bare metal, no cloud node group (confirmed absent) | [cluster-autoscaling.md](evidence/cluster-autoscaling.md) |

**7 PASS, 2 SKIP, 0 FAIL.** Collected 2026-11-03 with
`aicr validate --phase conformance --cncf-submission`.

## Platform notes

Two platform facts shaped this run and are worth recording for anyone
reproducing it.

**The node image ships the wrong kernel line.** VR200 needs the
`nvidia-bos-64k` kernel. On the generic `nvidia-64k` kernel no driver brings the
GPUs up at all — PRI reads return `0xbadf5720`, FSP and GSP never hand off, and
every GPU fails probe with `-EIO`. Both workers in this set were moved to
`7.0.0-2022-nvidia-bos-64k` by hand before any evidence was collected.

**The DRA driver needs a non-default driver root.** NVKE's image installs no
driver, so GPU Operator owns it and it lives at `/run/nvidia/driver`. With
`nvidia-dra-driver-gpu` left at the chart default of `/`, the kubelet-plugin
never finds the driver, no ComputeDomain becomes usable, and `dra_support`
degrades silently. The `vr200-nvke-ubuntu-training` overlay carries that
override.

## Multi-node NVLink

Not a CNCF requirement, but it is what the `dra_support` ComputeDomain exists
for on this platform, and it was measured alongside this set:
`all_reduce_perf` 1 GiB..16 GiB over MNNVL across both trays averaged
**996.5 GB/s** (second run 996.1 GB/s), peaking at 1040 GB/s at 16 GiB with zero
mismatches at every size. NCCL logged `NVLS comm ... nvlsRanks 8`, so one NVLS
communicator spanned both nodes.

## Caveat

Cross-node pod networking on this cluster is degraded — roughly 19 KB/s against
3.9 GB/s same-node, measured with the same HTTP server and file on both paths.
It did not affect these results: the NCCL data path is NVLink, and only the MPI
launch and rank bootstrap cross the pod network, which are kilobyte-scale. It is
recorded here because anything that moves bulk data between pods on this cluster
would be affected.
