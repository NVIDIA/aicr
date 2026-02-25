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
│   ├── gang-scheduling-test.yaml
│   └── hpa-gpu-test.yaml
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
```

## Usage

Evidence collection has two steps:

### Step 1: Structural Validation Evidence

`aicr validate` checks component health, CRDs, constraints, and generates
structural evidence:

```bash
# Generate evidence during validation
aicr validate -r recipe.yaml -s snapshot.yaml \
  --phase conformance --evidence-dir ./evidence

# Or use a saved result file
aicr validate -r recipe.yaml -s snapshot.yaml \
  --phase conformance --evidence-dir ./evidence \
  --result validation-result.yaml
```

### Step 2: Behavioral Test Evidence

`collect-evidence.sh` deploys test workloads and collects behavioral evidence
(DRA GPU allocation, gang scheduling, HPA autoscaling, etc.) that requires
running actual GPU workloads on the cluster:

```bash
# Collect all behavioral evidence
./docs/conformance/cncf/collect-evidence.sh all

# Collect evidence for a single feature
./docs/conformance/cncf/collect-evidence.sh dra
./docs/conformance/cncf/collect-evidence.sh gang
./docs/conformance/cncf/collect-evidence.sh secure
./docs/conformance/cncf/collect-evidence.sh metrics
./docs/conformance/cncf/collect-evidence.sh gateway
./docs/conformance/cncf/collect-evidence.sh operator
./docs/conformance/cncf/collect-evidence.sh hpa
./docs/conformance/cncf/collect-evidence.sh cluster-autoscaling
```

> **Note:** The HPA test (`hpa`) deploys a GPU stress workload (nbody) and waits
> for HPA to scale up, then verifies scale-down. This takes ~5 minutes due to
> metric propagation through the DCGM → Prometheus → prometheus-adapter → HPA pipeline.

### Why Two Steps?

| Evidence Type | `aicr validate` | `collect-evidence.sh` |
|---|---|---|
| Component health (pods, CRDs) | Yes | Yes |
| Constraint validation (K8s version, OS) | Yes | No |
| DRA GPU allocation test | No | Yes |
| Gang scheduling test | No | Yes |
| Device isolation verification | No | Yes |
| Gateway condition checks (Accepted, Programmed) | No | Yes |
| Webhook rejection test | No | Yes |
| HPA scale-up and scale-down with GPU load | No | Yes |
| Prometheus query results | No | Yes |
| Cluster autoscaling (ASG config) | No | Yes |

`aicr validate` checks that components are deployed correctly. `collect-evidence.sh`
verifies they work correctly by running actual workloads. Both are needed for
complete conformance evidence.

> **Future:** Behavioral tests are inherently long-running (e.g., HPA test deploys
> CUDA N-Body Simulation and waits ~5 minutes for metric propagation and scaling) and are better
> suited as a separate step than blocking `aicr validate`. A follow-up integration
> is tracked in [#192](https://github.com/NVIDIA/aicr/issues/192).

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
