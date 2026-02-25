# CNCF AI Conformance Evidence

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Kubernetes Version:** v1.34
**Platform:** EKS (p5.48xlarge, NVIDIA H100 80GB HBM3)

## Results

| # | Requirement | Feature | Result | Evidence |
|---|-------------|---------|--------|----------|
| 1 | `dra_support` | Dynamic Resource Allocation | PASS | [dra-support.md](dra-support.md) |
| 2 | `gang_scheduling` | Gang Scheduling (KAI Scheduler) | PASS | [gang-scheduling.md](gang-scheduling.md) |
| 3 | `secure_accelerator_access` | Secure Accelerator Access | PASS | [secure-accelerator-access.md](secure-accelerator-access.md) |
| 4 | `accelerator_metrics` / `ai_service_metrics` | Accelerator & AI Service Metrics | PASS | [accelerator-metrics.md](accelerator-metrics.md) |
| 5 | `ai_inference` | Inference API Gateway (kgateway) | PASS | [inference-gateway.md](inference-gateway.md) |
| 6 | `robust_controller` | Robust AI Operator (Dynamo) | PASS | [robust-operator.md](robust-operator.md) |
| 7 | `pod_autoscaling` | Pod Autoscaling (HPA + GPU metrics) | PASS | [pod-autoscaling.md](pod-autoscaling.md) |
| 8 | `cluster_autoscaling` | Cluster Autoscaling (EKS ASG) | PASS | [cluster-autoscaling.md](cluster-autoscaling.md) |
