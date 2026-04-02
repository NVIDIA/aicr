# CNCF AI Conformance Evidence

**Kubernetes Version:** v1.35
**Platform:** linux/amd64
**Product:** [NVIDIA NIM](https://developer.nvidia.com/nim) on EKS — A Kubernetes-based AI inference platform that deploys and manages NVIDIA NIM microservices on Amazon EKS with GPU scheduling, autoscaling, and Gateway API integration.
**Validation Tooling:** NVIDIA AI Cluster Runtime (AICR)

AICR deploys the runtime components (GPU Operator, NIM Operator, KAI Scheduler,
DCGM Exporter, kgateway, etc.) and validates that the platform meets CNCF AI
Conformance requirements. Evidence was collected on an EKS v1.35 cluster with
NVIDIA H100 80GB HBM3 accelerators running NIM inference workloads.

## Results

| # | Requirement | Feature | Result | Evidence |
|---|-------------|---------|--------|----------|
| 1 | `dra_support` | Dynamic Resource Allocation | PASS | [dra-support.md](dra-support.md) |
| 2 | `gang_scheduling` | Gang Scheduling (KAI Scheduler) | PASS | [gang-scheduling.md](gang-scheduling.md) |
| 3 | `secure_accelerator_access` | Secure Accelerator Access | PASS | [secure-accelerator-access.md](secure-accelerator-access.md) |
| 4 | `accelerator_metrics` | Accelerator Metrics (DCGM Exporter) | PASS | [accelerator-metrics.md](accelerator-metrics.md) |
| 5 | `ai_service_metrics` | AI Service Metrics (NIM Inference) | PASS | [ai-service-metrics.md](ai-service-metrics.md) |
| 6 | `ai_inference` | Inference API Gateway (kgateway) | PASS | [inference-gateway.md](inference-gateway.md) |
| 7 | `robust_controller` | Robust AI Operator (NIM Operator) | PASS | [robust-operator.md](robust-operator.md) |
| 8 | `pod_autoscaling` | Pod Autoscaling (HPA + GPU metrics) | PASS | [pod-autoscaling.md](pod-autoscaling.md) |
| 9 | `cluster_autoscaling` | Cluster Autoscaling | PASS | [cluster-autoscaling.md](cluster-autoscaling.md) |
