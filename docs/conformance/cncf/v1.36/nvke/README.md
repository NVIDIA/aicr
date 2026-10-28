# NVIDIA Kubernetes Engine

[NVIDIA Kubernetes Engine (NVKE)](https://github.com/dsx-ai-factory/nvke)
provides GPU Kubernetes clusters on bare metal. The tested AI platform combines
AICR's `gb200-nvke-ubuntu-training-kubeflow` recipe with Agentgateway for
Gateway API traffic management.

## Conformance Submission

- [PRODUCT.yaml](PRODUCT.yaml)
- [Underlying Kubernetes conformance](https://github.com/cncf/k8s-conformance/tree/master/v1.36/nvke)

## Evidence

Evidence was collected on October 27–28, 2026 (UTC), using the NVKE v0.22.0
qualification builds on Kubernetes v1.36.3, with two ARM64 NVIDIA GB200 workers
and eight GPUs. [Tested software versions](evidence/platform-configuration.md)
include GPU Operator v26.7.2, NVSentinel v1.28.0, KAI Scheduler v0.16.9,
Kubeflow Trainer v2.2.0, and Agentgateway v1.5.0.

| Requirement | Feature | Result | Evidence |
| --- | --- | --- | --- |
| `gang_scheduling` | Gang scheduling | PASS | [All-or-nothing scheduling](evidence/gang-scheduling.md) |
| `secure_accelerator_access` | GPU allocation and isolation | PASS | [Device-plugin isolation](evidence/secure-accelerator-access.md), [RuntimeClass probes on both workers](evidence/runtimeclass-isolation.md) |
| `accelerator_metrics` | Per-GPU metrics | PASS | [DCGM and Prometheus](evidence/accelerator-metrics.md) |
| `ai_service_metrics` | AI workload metrics | PASS | [PyTorch metrics discovery and collection](evidence/ai-service-metrics.md) |
| `ai_inference` | Gateway API traffic management | PASS | [Gateway readiness](evidence/inference-gateway.md), [header routing and weighted traffic](evidence/gateway-routing-behavior.txt) |
| `robust_controller` | AI operator reconciliation | PASS | [Controller and webhook](evidence/robust-operator.md), [completed TrainJob](evidence/trainjob-reconciliation.json), [GPU execution](evidence/trainjob-gpu-execution.txt) |
| `pod_autoscaling` | GPU-metric HPA | PASS | [GPU-based scaling](evidence/pod-autoscaling.md) |
| `cluster_autoscaling` | Workload-driven node scaling | N/A | [Collector result](evidence/cluster-autoscaling.md), [release-specific justification](evidence/platform-configuration.md) |

All seven applicable v1.36 MUST requirements are Implemented. Cluster
autoscaling is conditional and N/A because NVKE v0.22.0 provides
operator-controlled node-pool scaling. The collector's SKIP is preserved.
[DRA discovery](evidence/dra-support.md) is supporting evidence; this run uses
device-plugin whole-GPU allocation.

AICR deployment and conformance validation passed all 10 checks. Behavioral
evidence collection recorded eight passes and one conditional skip. Additional
checks demonstrate header-based and weighted routing, completed GPU TrainJob
execution, and authorized and unauthorized RuntimeClass GPU access on both
workers.

Internal IP addresses, site and host names, and infrastructure and hardware
identifiers have been removed or consistently aliased. Test commands,
timestamps, software versions, outcomes, and measured values are retained.
