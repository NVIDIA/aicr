# Proposal: NCCL Fabric Validation for Eidos Performance Phase

**Author:** Robert Wipfel
**Date:** 2026-02-09
**Status:** Draft
**Roadmap Item:** P0 — NCCL fabric validation (Validator Enhancements)

## Summary

This proposal contributes NCCL fabric and interconnect validation checks to the Eidos validator framework, addressing the P0 "NCCL fabric validation" item in the roadmap. The work ports validation logic from existing EKS cluster tooling (`cnai-eks`) into Eidos's Go-based, Job-executed, constraint-driven validation model.

## Motivation

The Eidos roadmap identifies three validator enhancements needed for MVP:

| Feature | Priority | Status |
|---------|----------|--------|
| **NCCL fabric validation** | **P0** | **Not started** |
| CNCF AI conformance | P1 | Not started |
| Remediation guidance | P1 | Not started |

Today, Eidos validates **readiness** (GPU hardware detection from snapshot data) and **deployment** (GPU operator version from live cluster). The **performance** phase — which validates that GPU interconnects function at expected bandwidth — has no checks implemented.

Without performance validation, Eidos can confirm a cluster has GPUs and operators but cannot confirm whether those GPUs can communicate effectively. Clusters pass readiness and deployment but may silently underperform in production.

## Prior Art

The `cnai-eks` repository contains validation scripts used for EKS + H100 (p5.48xlarge) cluster bring-up:

| Script | What It Validates | Key Thresholds |
|--------|-------------------|----------------|
| `testNvlink.sh` | Intra-node GPU-to-GPU via NVLink | >= 140 GB/s avg bus bandwidth (8x H100) |
| `testEfa.sh` | Single-node EFA communication (P2P/SHM disabled) | 12-16 GB/s avg bus bandwidth, 32 EFA adapters |
| `testEfaMultiNode.yaml` | Multi-node EFA via MPIJob (2+ nodes) | 300-500 GB/s avg bus bandwidth |
| `testUcx.sh` | UCX GPU Direct RDMA detection | > 10 GB/s (RDMA working) vs ~1.1 GB/s (host staging fallback) |
| `validateEks.sh` | 4-phase orchestration (hardware, K8s resources, NCCL perf, summary) | Composite pass/fail |

These scripts deploy Kubernetes Jobs, run `nccl-tests` (`all_reduce_perf`), parse bandwidth from logs, and report pass/fail against thresholds. This follows the same general pattern as Eidos's validation agent — deploying Jobs, reading results, and aggregating into structured output.

### Architecture Alignment

The `cnai-eks` validation phases map to Eidos's model:

```
cnai-eks/validateEks.sh          Eidos Validation Phases
========================         =======================
Phase 1: Per-node hardware  -->  Readiness   (snapshot-based, partially done)
Phase 2: K8s resource check -->  Deployment  (live cluster, GPU operator done)
Phase 3: NCCL performance  -->  Performance (NOT STARTED - this proposal)
Phase 4: Summary report    -->  Conformance (not started)
```

## Proposed Changes

### 1. Performance Phase Checks (New Package)

Create `pkg/validator/checks/performance/` with the following checks:

#### a. NVLink Bandwidth Check

**Constraint:** `Performance.nccl.nvlink-bandwidth`

Validates intra-node GPU-to-GPU communication via NVLink by deploying a single-node NCCL `all_reduce_perf` Job with `NCCL_NET_DISABLE=1` (forces NVLink-only communication).

```yaml
# Recipe constraint example
constraints:
  - name: Performance.nccl.nvlink-bandwidth
    value: ">= 140"  # GB/s avg bus bandwidth
```

**Logic (derived from `testNvlink.sh`):**
- Deploy K8s Job requesting 8 GPUs
- Run `all_reduce_perf -b 8 -e 8G -f 2 -g 1 -c 1 -n 100` with `NCCL_NET_DISABLE=1`
- Parse "Avg bus bandwidth" from output
- Evaluate against constraint expression

**Return:** actual bandwidth (e.g., `"146.2"`), pass/fail, error

#### b. EFA Single-Node Bandwidth Check

**Constraint:** `Performance.nccl.efa-bandwidth`

Validates EFA networking by deploying a single-node NCCL Job with P2P and SHM disabled, forcing traffic through EFA.

```yaml
constraints:
  - name: Performance.nccl.efa-bandwidth
    value: ">= 12"  # GB/s avg bus bandwidth (single-node baseline)
```

**Logic (derived from `testEfa.sh`):**
- Deploy K8s Job requesting 8 GPUs + 32 EFA adapters + hugepages
- Run `all_reduce_perf` with `NCCL_P2P_DISABLE=1 NCCL_SHM_DISABLE=1`
- Set EFA-specific env: `FI_PROVIDER=efa`, `FI_EFA_USE_DEVICE_RDMA=1`, `NCCL_CROSS_NIC=1`, etc.
- Parse "Avg bus bandwidth" from output
- Evaluate against constraint expression

**Return:** actual bandwidth (e.g., `"14.3"`), pass/fail, error

#### c. EFA Adapter Count Check

**Constraint:** `Performance.efa.adapter-count`

Validates expected number of EFA adapters are available to the NCCL runtime.

```yaml
constraints:
  - name: Performance.efa.adapter-count
    value: "== 32"  # Expected for p5.48xlarge
```

**Logic (derived from `testEfa.sh` EFA detection):**
- Parse EFA NIC count from NCCL debug output (`found N nics`)
- Evaluate against constraint expression

**Return:** actual count (e.g., `"32"`), pass/fail, error

### 2. Recipe Constraint Definitions

Add performance constraints to EKS training overlays:

```yaml
# pkg/recipe/data/overlays/h100-eks-training.yaml (additions)
constraints:
  - name: Performance.nccl.nvlink-bandwidth
    value: ">= 140"
  - name: Performance.nccl.efa-bandwidth
    value: ">= 12"
  - name: Performance.efa.adapter-count
    value: "== 32"
```

Thresholds are based on observed results from p5.48xlarge testing and aligned with publicly documented NCCL performance expectations for H100 NVLink and EFA configurations.

### 3. Performance Test Job Specification

The NCCL test Jobs require specific resources and environment configuration:

```yaml
# NVLink test Job resources
resources:
  limits:
    nvidia.com/gpu: 8
    memory: 100Gi
    cpu: "48"

# EFA test Job resources (additional)
resources:
  limits:
    nvidia.com/gpu: 8
    vpc.amazonaws.com/efa: 32
    hugepages-2Mi: 5Gi
    memory: 100Gi
    cpu: "48"
```

Key environment variables for EFA tests:
- `FI_PROVIDER=efa` — Force EFA provider
- `FI_EFA_USE_DEVICE_RDMA=1` — Enable GPU Direct RDMA
- `NCCL_CROSS_NIC=1` — Enable multi-rail
- `NCCL_MIN_NCHANNELS=32` — Minimum NCCL channels
- `NCCL_NET_GDR_LEVEL=5` — GPU Direct RDMA level

### 4. Test Container Image

The `cnai-eks` scripts use `763104351884.dkr.ecr.us-east-1.amazonaws.com/pytorch-training:2.7.1-gpu-py312-cu128-ubuntu22.04-ec2` (AWS Deep Learning Container with NCCL, OpenMPI, and `aws-ofi-nccl` pre-installed).

For Eidos, we need a strategy for the test container image:

**Option A (Recommended):** Reference platform-specific NCCL test images in recipe metadata. EKS recipes reference the AWS DLC, GKE recipes reference a GCE-appropriate image, etc. The recipe overlay controls which image the performance Job uses.

**Option B:** Build a standalone NCCL test image published alongside the Eidos validator image. More portable but requires maintaining the image.

## Scope

### In Scope

- `pkg/validator/checks/performance/` package with NVLink, EFA bandwidth, and EFA adapter count checks
- Registration via `init()` in the check registry (phase: `"performance"`)
- Unit tests using `fake.NewSimpleClientset()` with mocked Job outputs
- Recipe constraint additions to `h100-eks-training` and `h100-eks-ubuntu-training` overlays
- Documentation in `pkg/validator/checks/performance/README.md`

### Out of Scope (Follow-Up Work)

- **Multi-node NCCL validation** — Requires MPIJob/MPI Operator; different execution model than single-Job. Would be a follow-up using `testEfaMultiNode.yaml` as starting point.
- **UCX GPU Direct RDMA check** — Currently blocked on kernel 6.14 + OpenRM driver combination. Can be added when the driver issue is resolved.
- **Non-EKS platforms** — GKE, AKS, OKE will need different network fabric checks (no EFA). The pattern established here can be extended per platform.
- **Conformance report generation** — Separate P1 roadmap item.

## Implementation Plan

| Step | Description | Files |
|------|-------------|-------|
| 1 | Create performance check package scaffold | `pkg/validator/checks/performance/` |
| 2 | Implement NVLink bandwidth check + constraint validator | `nvlink_bandwidth.go`, `nvlink_bandwidth_test.go` |
| 3 | Implement EFA bandwidth check + constraint validator | `efa_bandwidth.go`, `efa_bandwidth_test.go` |
| 4 | Implement EFA adapter count check | `efa_adapters.go`, `efa_adapters_test.go` |
| 5 | Add performance constraints to EKS training overlays | `pkg/recipe/data/overlays/h100-eks-training.yaml` |
| 6 | Integration tests | `performance_integration_test.go` |
| 7 | Documentation | `pkg/validator/checks/performance/README.md` |

## Expected Bandwidth Reference

Based on testing on p5.48xlarge (8x H100 + 32x EFA):

| Test | Metric | Expected Range |
|------|--------|----------------|
| NVLink (single-node, 8 GPU) | Avg bus bandwidth | 140-150 GB/s |
| EFA (single-node, P2P disabled) | Avg bus bandwidth | 12-16 GB/s |
| EFA (2-node, multi-rail) | Avg bus bandwidth | 450-460 GB/s |
| EFA (4-node, multi-rail) | Avg bus bandwidth | 300-310 GB/s |

NCCL bus bandwidth is a normalized metric, not raw NIC throughput. Values > 400 GB/s on a 400 GB/s node are normal and expected due to NCCL's algorithm-aware reporting.

## Acceptance Criteria

1. `eidos validate --recipe h100-eks-training.yaml` runs performance phase checks when `--phases performance` is specified
2. NVLink bandwidth check deploys a Job, parses NCCL output, and evaluates `>= 140 GB/s` constraint
3. EFA bandwidth check deploys a Job with EFA resources, parses NCCL output, and evaluates `>= 12 GB/s` constraint
4. EFA adapter count check validates expected adapter count
5. All checks return structured results via the existing `ValidationResult` model
6. `make test` passes with race detector enabled
7. `make lint` passes

## Open Questions

1. **Test container image strategy** — Should we reference platform-specific images in recipe metadata (Option A) or build/publish a standalone NCCL test image (Option B)?
2. **Timeout configuration** — NCCL tests take 5-10 minutes. Should the performance phase have a configurable per-check timeout, or use the existing Job-level timeout?
3. **Threshold parameterization** — Should bandwidth thresholds be fully driven by recipe constraints (proposed), or should there be default fallbacks per accelerator type?
4. **Multi-node scope** — Should multi-node NCCL (MPIJob) be included in this initial PR, or deferred to a follow-up? (Proposed: deferred — different execution model.)
