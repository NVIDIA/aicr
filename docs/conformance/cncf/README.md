# CNCF AI Conformance Evidence

## Overview

This directory contains evidence for [CNCF Kubernetes AI Conformance](https://github.com/cncf/k8s-ai-conformance)
certification. The evidence demonstrates that a cluster configured with a specific
recipe meets the Must-have requirements for Kubernetes v1.34.

> **Note:** It is the **cluster configured by a recipe** that is conformant, not the
> tool itself. The recipe determines which components are deployed and how they are
> configured. Different recipes may produce clusters with different conformance profiles.

**Recipe used:** `h100-eks-ubuntu-inference-dynamo`
**Cluster:** EKS with p5.48xlarge (8x NVIDIA H100 80GB HBM3)
**Kubernetes:** v1.34

## Directory Structure

```
docs/conformance/cncf/
├── README.md
├── collect-evidence.sh
├── manifests/
│   ├── dra-gpu-test.yaml
│   └── gang-scheduling-test.yaml
└── evidence/
    ├── index.md
    ├── dra-support.md
    ├── gang-scheduling.md
    ├── secure-accelerator-access.md
    ├── accelerator-metrics.md
    ├── inference-gateway.md
    └── robust-operator.md
```

## Usage

```bash
# Collect all evidence
./docs/conformance/cncf/collect-evidence.sh all

# Collect evidence for a single feature
./docs/conformance/cncf/collect-evidence.sh dra
./docs/conformance/cncf/collect-evidence.sh gang
./docs/conformance/cncf/collect-evidence.sh secure
./docs/conformance/cncf/collect-evidence.sh metrics
./docs/conformance/cncf/collect-evidence.sh gateway
./docs/conformance/cncf/collect-evidence.sh operator
```

## Evidence

See [evidence/index.md](evidence/index.md) for a summary of all collected evidence and results.

## Feature Areas

| # | Feature | Requirement | Evidence File |
|---|---------|-------------|---------------|
| 1 | DRA Support | `dra_support` | [evidence/dra-support.md](evidence/dra-support.md) |
| 2 | Gang Scheduling | `gang_scheduling` | [evidence/gang-scheduling.md](evidence/gang-scheduling.md) |
| 3 | Secure Accelerator Access | `secure_accelerator_access` | [evidence/secure-accelerator-access.md](evidence/secure-accelerator-access.md) |
| 4 | Accelerator & AI Service Metrics | `accelerator_metrics`, `ai_service_metrics` | [evidence/accelerator-metrics.md](evidence/accelerator-metrics.md) |
| 5 | Inference API Gateway | `ai_inference` | [evidence/inference-gateway.md](evidence/inference-gateway.md) |
| 6 | Robust AI Operator | `robust_controller` | [evidence/robust-operator.md](evidence/robust-operator.md) |

## TODO

- [ ] **Cluster Autoscaling** (`cluster_autoscaling`, MUST) — Demonstrate Karpenter or cluster autoscaler scaling GPU node groups based on pending pod requests
- [ ] **Pod Autoscaling** (`pod_autoscaling`, MUST) — Demonstrate HPA scaling pods based on custom GPU metrics (e.g., `gpu_utilization` from prometheus-adapter)
