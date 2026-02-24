# Kubernetes Platforms Powered by NVIDIA AI Cluster Runtime (AICR)

Kubernetes platforms powered by [NVIDIA AI Cluster Runtime (AICR)](https://github.com/NVIDIA/aicr) are CNCF AI Conformant. AICR generates validated, GPU-accelerated Kubernetes configurations that satisfy all CNCF AI Conformance requirements for accelerator management, scheduling, observability, security, and inference networking.

## Conformance Submission

- [PRODUCT.yaml](PRODUCT.yaml)

## Evidence

Evidence was collected on a Kubernetes v1.34 cluster with NVIDIA H100 80GB HBM3 GPUs using the AICR recipe `h100-eks-ubuntu-inference-dynamo`.

| # | Requirement | Feature | Result | Evidence |
|---|-------------|---------|--------|----------|
| 1 | `dra_support` | Dynamic Resource Allocation | PASS | [dra-support.md](../evidence/dra-support.md) |
| 2 | `gang_scheduling` | Gang Scheduling (KAI Scheduler) | PASS | [gang-scheduling.md](../evidence/gang-scheduling.md) |
| 3 | `secure_accelerator_access` | Secure Accelerator Access | PASS | [secure-accelerator-access.md](../evidence/secure-accelerator-access.md) |
| 4 | `accelerator_metrics` / `ai_service_metrics` | Accelerator & AI Service Metrics | PASS | [accelerator-metrics.md](../evidence/accelerator-metrics.md) |
| 5 | `ai_inference` | Inference API Gateway (kgateway) | PASS | [inference-gateway.md](../evidence/inference-gateway.md) |
| 6 | `robust_controller` | Robust AI Operator (Dynamo) | PASS | [robust-operator.md](../evidence/robust-operator.md) |
| 7 | `pod_autoscaling` | Pod Autoscaling (HPA + GPU Metrics) | PASS | [pod-autoscaling.md](../evidence/pod-autoscaling.md) |
| 8 | `cluster_autoscaling` | Cluster Autoscaling (EKS ASG) | PASS | [cluster-autoscaling.md](../evidence/cluster-autoscaling.md) |

All 9 conformance requirement IDs across 8 evidence files are **Implemented** (`accelerator_metrics` and `ai_service_metrics` share a single evidence file).
