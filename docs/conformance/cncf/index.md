# CNCF AI Conformance

## Overview

This directory contains evidence for [CNCF Kubernetes AI Conformance](https://github.com/cncf/k8s-ai-conformance)
certification. Each submission certifies a specific product on a specific Kubernetes
distribution, with evidence collected using AICR as the validation tooling.

> **Note:** It is the **product deployed on a Kubernetes platform** that is conformant.
> AICR serves as the deployment and validation tooling (similar to sonobuoy for K8s
> conformance), while the certified product is the AI inference/training platform.

## Submissions

| Version | Product | Platform | Status | Evidence |
|---------|---------|----------|--------|----------|
| v1.35 | [NVIDIA NIM](https://developer.nvidia.com/nim) | EKS | 9/9 PASS | [v1.35/nim-eks/](v1.35/nim-eks/) |

## Directory Structure

```
docs/conformance/cncf/
├── index.md                          # This file
└── v1.35/                            # Kubernetes version
    └── nim-eks/                      # Product + platform (mirrors CNCF repo)
        ├── PRODUCT.yaml              # CNCF submission metadata
        ├── README.md                 # Submission overview + results table
        └── evidence/                 # Behavioral evidence files
            ├── index.md
            ├── dra-support.md
            ├── gang-scheduling.md
            ├── secure-accelerator-access.md
            ├── accelerator-metrics.md
            ├── ai-service-metrics.md
            ├── inference-gateway.md
            ├── robust-operator.md
            ├── pod-autoscaling.md
            └── cluster-autoscaling.md

pkg/evidence/cncf/                    # CNCF evidence collector package
├── collector.go                      # Feature registry, alias mapping
├── renderer.go                       # Evidence file rendering
├── requirements.go                   # CNCF requirement ID mapping
├── templates.go                      # Evidence templates
├── types.go                          # Shared types
└── scripts/                          # Evidence collection script + test manifests
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

Alternatively, run the evidence collection script directly. Valid section
names are `dra`, `gang`, `secure`, `accelerator-metrics`, `service-metrics`,
`gateway`, `operator`, `hpa`, `cluster-autoscaling`, or `all`:

```bash
./pkg/evidence/cncf/scripts/collect-evidence.sh all
./pkg/evidence/cncf/scripts/collect-evidence.sh dra
```

> **Note:** The `--cncf-submission` flag deploys GPU workloads and takes ~5-10
> minutes. The evidence collection script automatically detects the AI workload
> type (NIM inference, Dynamo inference, or Kubeflow training) and collects
> appropriate metrics and operator evidence.

### Two Modes

| | `aicr validate --phase conformance` | `--cncf-submission` |
|---|---|---|
| **Purpose** | CI pass/fail | CNCF submission evidence |
| **Speed** | ~3 minutes | ~5-10 minutes |
| **Deploys workloads** | Yes (DRA, gang, HPA, secure access) | Yes (all + GPU stress test) |
| **Output** | Pass/fail + diagnostic artifacts | Detailed behavioral evidence (command outputs, logs, metrics) |
| **DRA GPU allocation test** | Deploys pod, verifies GPU access + isolation | Same + nvidia-smi output capture |
| **Gang scheduling test** | Deploys PodGroup, verifies co-scheduling | Same + worker logs |
| **HPA autoscaling** | Metrics API + scale-up validation | CUDA GPU stress test + scale-up |
| **Metrics** | Custom metrics API data-path verification | DCGM exporter + Prometheus queries |
| **Gateway** | Condition verification (Accepted, Programmed) | Same |
| **Webhook test** | Rejection test with invalid CR | Same |
| **Cluster autoscaling** | Cloud node group validation | Cloud-provider autoscaler API |
