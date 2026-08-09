# AICR Training Stack on EKS (GB200) — regenerated conformance evidence

This evidence set was collected with the strengthened behavioral tests from
[#1529](https://github.com/NVIDIA/aicr/issues/1529) (PR #2097) on an EKS
v1.34 cluster with NVIDIA GB200 accelerators (`p6e-gb200.36xlarge`, 4 GPUs
per node), running the AICR **training** stack: GPU Operator, KAI Scheduler,
Kubeflow Trainer, DCGM exporter, and kube-prometheus.

**This is not an upstream CNCF submission** (no `PRODUCT.yaml`): the
training profile deploys no inference gateway, so the `ai_inference`
requirement is not demonstrated here — the corresponding evidence records a
fail-closed SKIP on confirmed absence. The upstream-submitted set remains
[`../nim-eks/`](../nim-eks/), which this directory deliberately does not
modify. The purpose of this set is to demonstrate the strengthened tests on
real hardware:

- **Gang scheduling** is now a two-phase all-or-nothing test: under
  constrained capacity neither pod may be bound, with the scheduler's
  affirmative gang-attributed refusal required ("Resources were found for 1
  pods while 2 are required for gang scheduling"), then both pods must
  complete together once capacity is freed.
- **Robust operator** requires a rejection attributed to the operator's own
  validating webhook by its exact name (schema rejections and foreign
  webhook denials fail).

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
| 9 | `cluster_autoscaling` | Cluster Autoscaling | PASS | [cluster-autoscaling.md](evidence/cluster-autoscaling.md) |

Collected 2026-08-08 with `pkg/evidence/cncf/scripts/collect-evidence.sh all`.
