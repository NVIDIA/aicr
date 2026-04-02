# NVIDIA NIM on EKS

[NVIDIA NIM](https://developer.nvidia.com/nim) on EKS is a Kubernetes-based AI inference platform that deploys and manages NVIDIA NIM microservices on Amazon EKS with GPU scheduling, autoscaling, and Gateway API integration. NIM microservice lifecycle is managed by the [NIM Operator](https://github.com/NVIDIA/k8s-nim-operator). The platform is configured and validated using [NVIDIA AI Cluster Runtime (AICR)](https://github.com/NVIDIA/aicr).

## Conformance Submission

- [PRODUCT.yaml](PRODUCT.yaml)

## Evidence

Evidence was collected on an EKS v1.35 cluster with NVIDIA H100 80GB HBM3 GPUs running NIM inference workloads, validated by AICR.

| # | Requirement | Feature | Result | Evidence |
|---|-------------|---------|--------|----------|
| 1 | `dra_support` | Dynamic Resource Allocation | PASS | [dra-support.md](evidence/dra-support.md) |
| 2 | `gang_scheduling` | Gang Scheduling (KAI Scheduler) | PASS | [gang-scheduling.md](evidence/gang-scheduling.md) |
| 3 | `secure_accelerator_access` | Secure Accelerator Access | PASS | [secure-accelerator-access.md](evidence/secure-accelerator-access.md) |
| 4 | `accelerator_metrics` | Accelerator Metrics (DCGM Exporter) | PASS | [accelerator-metrics.md](evidence/accelerator-metrics.md) |
| 5 | `ai_service_metrics` | AI Service Metrics (NIM Inference) | PASS | [ai-service-metrics.md](evidence/ai-service-metrics.md) |
| 6 | `ai_inference` | Inference API Gateway (kgateway) | PASS | [inference-gateway.md](evidence/inference-gateway.md) |
| 7 | `robust_controller` | Robust AI Operator (NIM Operator) | PASS | [robust-operator.md](evidence/robust-operator.md) |
| 8 | `pod_autoscaling` | Pod Autoscaling (HPA + GPU Metrics) | PASS | [pod-autoscaling.md](evidence/pod-autoscaling.md) |
| 9 | `cluster_autoscaling` | Cluster Autoscaling | PASS | [cluster-autoscaling.md](evidence/cluster-autoscaling.md) |

All 9 MUST conformance requirement IDs across 9 evidence files are **Implemented**. 3 SHOULD requirements (`driver_runtime_management`, `gpu_sharing`, `virtualized_accelerator`) are also Implemented.
