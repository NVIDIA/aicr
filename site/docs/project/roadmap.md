---
title: "Roadmap"

weight: 50
description: "Feature roadmap and project milestones"
---

# Roadmap

This roadmap tracks remaining work for AICR v2 launch and future enhancements.

## Structure

| Section | Description |
|---------|-------------|
| **Remaining MVP Work** | Tasks blocking v2 launch |
| **Backlog** | Post-launch enhancements by priority |
| **Completed** | Delivered capabilities (reference only) |

---

## Remaining MVP Work

### MVP Recipe Matrix Completion

**Status:** In progress (6 leaf recipes complete)

Expand recipe coverage for MVP platforms and accelerators.

**Current:**
- EKS + H100 + Training (+ Ubuntu, + Kubeflow)
- EKS + H100 + Inference (+ Ubuntu, + Dynamo)

**Needed:**

| Platform | Accelerator | Intent | Status |
|----------|-------------|--------|--------|
| EKS | GB200 | Training | Partial (kernel-module-params only) |
| EKS | A100 | Training | Not started |
| GKE | H100 | Training | Not started |
| GKE | H100 | Inference | Not started |
| GKE | GB200 | Training | Not started |
| AKS | H100 | Training | Not started |
| AKS | H100 | Inference | Not started |
| OKE | H100 | Training | Not started |
| OKE | H100 | Inference | Not started |
| OKE | GB200 | Training | Not started |

**Acceptance:** each validates and generates bundles.

---

### Validator Enhancements

**Status:** Core complete, advanced features pending

**Implemented:**
- Constraint evaluation against snapshots
- Component health checks
- Validation result reporting
- Four-phase validation framework (readiness, deployment, performance, conformance)

**Needed:**

| Feature | Description | Priority |
|---------|-------------|----------|
| NCCL fabric validation | Deploy test job, verify GPU-to-GPU communication | P0 |
| CNCF AI conformance | Generate conformance report | P1 |
| Remediation guidance | Actionable fixes for common failures | P1 |

**Acceptance:** `aicr validate --deployment` and `aicr validate --conformance ai` produce valid output.

---

### E2E Deployment Validation

**Status:** Partial

Validate bundler output deploys successfully on target platforms.

| Platform | Script Deploy | Argo CD Deploy |
|----------|---------------|---------------|
| EKS | Not validated | Not validated |
| GKE | Not validated | Not validated |
| AKS | Not validated | Not validated |

**Acceptance:** At least one successful deployment per platform with both deployers.

---

## Backlog

Post-launch enhancements organized by priority.

### P1 — High Value

#### Expand Recipe Coverage

Extend beyond MVP platforms and accelerators.

- Self-managed Kubernetes support
- Additional cloud providers (Oracle OCI, Alibaba Cloud)
- Additional accelerators (L40S, future architectures)
- Prioritized recipe backlog with components

#### New Bundlers

Migrate capabilities from AICR v1 playbooks.

| Bundler | Description |
|---------|-------------|
| NIM Operator | NVIDIA Inference Microservices deployment |
| KServe | Inference serving configurations |
| Nsight Operator | Cluster-wide profiling and observability |
| Storage | GPU workload storage configurations |

#### Recipe Creation Tooling

Simplify recipe development and contribution to accelerate MVP recipe matrix completion.

**Recipe Validation Framework** — Static validation tool to catch errors before PR submission

Three validation levels:
- **Syntax validation** — YAML parsing, required fields present, valid apiVersion/kind
- **Cross-reference validation** — Component refs exist in registry, valueOverrideKeys match, no orphaned components
- **Semantic validation** — Helm/Kustomize sources reachable, constraint expressions parsable, dependency cycles detected

Implementation options:
- `aicr recipe validate` CLI command
- Pre-commit git hook (automatic gating)
- CI/CD integration (GitHub Actions validation step)

**Recipe Scaffolding Generator** — Template-based recipe creation

- Generate overlay YAML from platform/accelerator/intent parameters
- Pre-populate common components for recipe type
- Inline documentation and validation
- Usage: `aicr recipe scaffold --platform gke --accelerator a100 --intent training`

**Component Reference Checker** — Static analysis for recipe integrity

- Validate all overlay components exist in registry
- Check valueOverrideKeys consistency
- Verify required fields (helm.chart OR kustomize.source)
- Validate node scheduling paths for component type

**Development Workflow Integration**

- Recipe template file (`recipes/overlays/_TEMPLATE.yaml`)
- Validation script integrated into `make qualify`
- KWOK test generator (auto-generate `tools/kwok/e2e_<recipe>.sh`)
- Recipe documentation generator (auto-generate `docs/recipes/*.md`)
- Contribution guide (`docs/RECIPE_CONTRIBUTION.md`)

---

### P2 — Medium Value

#### Configuration Drift Detection

Detect when clusters diverge from recipe-defined state.

- `aicr diff` command for snapshot comparison
- Scheduled drift detection via CronJob
- Alerting integration for drift events

#### Enhanced Skyhook Integration

Deeper OS-level node optimization. Ubuntu done; RHEL and Amazon Linux remain.

- OS-specific overlays for RHEL and Amazon Linux
- Automated node configuration validation

---

### P3 — Future

#### Additional API Interfaces

Programmatic integration options.

- gRPC API for high-performance access
- GraphQL API for flexible querying
- Multi-tenancy support

## Completed

Delivered capabilities (reference only).

- **EKS + H100 recipes** — Training (+ Ubuntu, + Kubeflow) and Inference (+ Ubuntu, + Dynamo) overlays
- **Snapshot-to-recipe transformation** — `ExtractCriteriaFromSnapshot` in `pkg/recipe/snapshot.go`
- **Monitoring components** — kube-prometheus-stack, prometheus-adapter, nvsentinel, ephemeral-storage-metrics in registry; monitoring-hpa overlay
- **Skyhook Ubuntu integration** — skyhook-operator + skyhook-customizations with H100 tuning manifest
- **Argo CD deployer** — `pkg/bundler/deployer/argocd/` alongside Helm deployer
- **Validation framework** — Four-phase validation (readiness, deployment, performance, conformance)

## Revision History

| Date | Change |
|------|--------|
| 2026-02-17 | Expanded Recipe Creation Tooling with validation framework details, scaffolding, and workflow integration |
| 2026-02-14 | Moved implemented items to Completed: EKS H100 recipes, snapshot-to-recipe, monitoring, Skyhook Ubuntu |
| 2026-01-26 | Reorganized: removed completed items, streamlined structure |
| 2026-01-05 | Added Opens section based on architectural decisions |
| 2026-01-01 | Initial comprehensive roadmap |
