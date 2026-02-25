---
title: "Evidence"
linkTitle: "Evidence"
weight: 10
description: "Detailed conformance evidence for each feature area"
---

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Kubernetes Version:** v1.34
**Platform:** EKS (p5.48xlarge, NVIDIA H100 80GB HBM3)

## Results

| # | Requirement | Feature | Result | Evidence |
|---|-------------|---------|--------|----------|
| 1 | `dra_support` | Dynamic Resource Allocation | PASS | [DRA Support](/docs/conformance/evidence/dra-support/) |
| 2 | `gang_scheduling` | Gang Scheduling (KAI Scheduler) | PASS | [Gang Scheduling](/docs/conformance/evidence/gang-scheduling/) |
| 3 | `secure_accelerator_access` | Secure Accelerator Access | PASS | [Secure Accelerator Access](/docs/conformance/evidence/secure-accelerator-access/) |
| 4 | `accelerator_metrics` / `ai_service_metrics` | Accelerator & AI Service Metrics | PASS | [Accelerator Metrics](/docs/conformance/evidence/accelerator-metrics/) |
| 5 | `ai_inference` | Inference API Gateway (kgateway) | PASS | [Inference Gateway](/docs/conformance/evidence/inference-gateway/) |
| 6 | `robust_controller` | Robust AI Operator (Dynamo) | PASS | [Robust Operator](/docs/conformance/evidence/robust-operator/) |
| 7 | `pod_autoscaling` | Pod Autoscaling (HPA + GPU metrics) | PASS | [Pod Autoscaling](/docs/conformance/evidence/pod-autoscaling/) |
| 8 | `cluster_autoscaling` | Cluster Autoscaling (EKS ASG) | PASS | [Cluster Autoscaling](/docs/conformance/evidence/cluster-autoscaling/) |
