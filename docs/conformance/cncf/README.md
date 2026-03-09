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
├── submission/
│   ├── PRODUCT.yaml
│   └── README.md
└── evidence/
    ├── index.md
    ├── dra-support.md
    ├── gang-scheduling.md
    ├── secure-accelerator-access.md
    ├── accelerator-metrics.md
    ├── inference-gateway.md
    ├── robust-operator.md
    ├── pod-autoscaling.md
    └── cluster-autoscaling.md

pkg/evidence/scripts/             # Evidence collection script + test manifests
├── collect-evidence.sh
└── manifests/
    ├── dra-gpu-test.yaml
    ├── gang-scheduling-test.yaml
    └── hpa-gpu-test.yaml
```

## Usage

Evidence collection has two steps:

### Structural Validation (CI)

`aicr validate` checks component health, CRDs, and constraints for CI:

```bash
# Structural validation + evidence rendering
aicr validate -r recipe.yaml \
  --phase conformance --evidence-dir ./evidence
```

### CNCF Submission Evidence

Add `--cncf-submission` to collect detailed behavioral evidence for CNCF AI
Conformance submission. This deploys GPU workloads, captures command outputs,
workload logs, nvidia-smi output, and Prometheus queries:

```bash
# Collect all behavioral evidence
aicr validate --phase conformance \
  --evidence-dir ./evidence --cncf-submission

# Collect specific features
aicr validate --phase conformance \
  --evidence-dir ./evidence --cncf-submission -f dra -f hpa
```

Alternatively, run the evidence collection script directly:
```bash
./pkg/evidence/scripts/collect-evidence.sh all
./pkg/evidence/scripts/collect-evidence.sh dra
```

> **Note:** The `--cncf-submission` flag deploys GPU workloads and takes ~5-10
> minutes. The evidence collection script uses polling with early exit on both
> success and failure, minimizing wait times. The HPA test uses CUDA N-Body
> Simulation to stress GPUs and verifies scale-up.

### Two Modes

| | `aicr validate --phase conformance` | `--cncf-submission` |
|---|---|---|
| **Purpose** | CI pass/fail | CNCF submission evidence |
| **Speed** | ~3 minutes | ~5-10 minutes |
| **Deploys workloads** | Yes (DRA, gang, HPA, secure access) | Yes (all + GPU stress test) |
| **Output** | Pass/fail + diagnostic artifacts | Detailed behavioral evidence (command outputs, logs, metrics) |
| **DRA GPU allocation test** | Deploys pod, verifies GPU access + isolation | Same + nvidia-smi output capture |
| **Gang scheduling test** | Deploys PodGroup, verifies co-scheduling | Same + worker logs |
| **HPA autoscaling** | Metrics API + scale-up/down validation | CUDA N-Body stress test + scale-up |
| **Metrics** | Custom metrics API data-path verification | DCGM exporter + Prometheus queries |
| **Gateway** | Condition verification (Accepted, Programmed) | Same |
| **Webhook test** | Rejection test with invalid CR | Same |
| **Cluster autoscaling** | Karpenter NodePools or EKS node group validation | EKS ASG via AWS API |

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
| 7 | Pod Autoscaling | `pod_autoscaling` | [evidence/pod-autoscaling.md](evidence/pod-autoscaling.md) |
| 8 | Cluster Autoscaling | `cluster_autoscaling` | [evidence/cluster-autoscaling.md](evidence/cluster-autoscaling.md) |
