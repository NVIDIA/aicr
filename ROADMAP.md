# Eidos Roadmap

This roadmap tracks remaining work for Eidos v2 launch and future enhancements.

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

**Acceptance:** `eidos validate --deployment` and `eidos validate --conformance ai` produce valid output.

---

### E2E Deployment Validation

**Status:** Partial

Validate bundler output deploys successfully on target platforms.

| Platform | Script Deploy | ArgoCD Deploy |
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

Migrate capabilities from Eidos v1 playbooks.

| Bundler | Description |
|---------|-------------|
| NIM Operator | NVIDIA Inference Microservices deployment |
| KServe | Inference serving configurations |
| Nsight Operator | Cluster-wide profiling and observability |
| Storage | GPU workload storage configurations |

#### Recipe Creation Tooling

Simplify recipe development and contribution.

- Interactive recipe builder CLI
- Recipe contribution workflow (PR template, validation gates)

---

### P2 — Medium Value

#### Configuration Drift Detection

Detect when clusters diverge from recipe-defined state.

- `eidos diff` command for snapshot comparison
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
- **ArgoCD deployer** — `pkg/bundler/deployer/argocd/` alongside Helm deployer
- **Validation framework** — Four-phase validation (readiness, deployment, performance, conformance)

## Revision History

| Date | Change |
|------|--------|
| 2026-02-14 | Moved implemented items to Completed: EKS H100 recipes, snapshot-to-recipe, monitoring, Skyhook Ubuntu |
| 2026-01-26 | Reorganized: removed completed items, streamlined structure |
| 2026-01-17 | Restructured to JIRA format with Initiatives and Epics |
| 2026-01-06 | Updated structure |
| 2026-01-05 | Added Opens section based on architectural decisions |
| 2026-01-01 | Initial comprehensive roadmap |
