# AICR Roadmap

This roadmap tracks the remaining work toward AICR v1 (GA) and the
enhancements that continue after v1.

## v1 exit gates

AICR v1 is blocked by two remaining outcomes:

1. **Defensible API stability** — the CLI, REST API, Go SDK facade, bundle
   layout, and artifact schemas have explicit compatibility boundaries backed
   by released baselines and merge-blocking compatibility gates.
2. **Sufficient validated coverage** — the existing recipe portfolio remains
   the supported baseline, GB300 coverage is published and validated, VR200 is
   available as Preview, and validation evidence is publicly verifiable.

Contribution UX and the closed supply-chain work remain important project
priorities, but their v1 foundations are established and they are not remaining
release exit gates. Full component upgrade lifecycle safety is post-v1 work.

## 1. Defensible API stability

The functional scope of the CLI and REST API is established. The remaining
work is to make their stability defensible and to complete the Go SDK facade so
an integrator never needs an unstable internal package.

Two distinct conditions must hold:

- **Functional completeness:** each supported workflow is reachable through
  the appropriate public surface.
- **Compatibility enforcement:** each surface has a committed baseline and a
  CI gate that fails on unintended breakage.

| Surface | Functional scope | Remaining v1 work |
|---|---|---|
| CLI flags and subcommands | Established | Commit the surface baseline and compatibility gate ([#2111](https://github.com/NVIDIA/aicr/issues/2111)) |
| REST API | Established in `api/aicr/v1/server.yaml` | Commit the OpenAPI baseline and breaking-change gate ([#2112](https://github.com/NVIDIA/aicr/issues/2112)) |
| Go SDK facade | Substantially complete | Complete the facade-only `snapshot → criteria → recipe → validate → bundle → verify` workflow ([#2016](https://github.com/NVIDIA/aicr/issues/2016)) |
| Bundle layout and artifact schemas | Established, with maturity rollout governed by [ADR-022](docs/design/022-artifact-maturity-and-deprecation.md) | Commit layout/schema baselines and compatibility gates ([#2113](https://github.com/NVIDIA/aicr/issues/2113)) |

The cross-surface freeze and final closure are tracked by
[#2370](https://github.com/NVIDIA/aicr/issues/2370). Artifact maturity and the
project's v1 release remain separate axes.

### Acceptance

- CLI flags and subcommands, REST endpoints and schemas, the Go SDK exported
  surface, and bundle layout/artifact schemas each have a committed baseline.
- Compatibility checks for all four surfaces run in `make qualify` and the
  merge gate.
- An integrator can implement the complete workflow using
  `github.com/NVIDIA/aicr/pkg/client/v1` plus standard-library and explicitly
  stable third-party types, without importing another AICR `pkg/*` package.
- `RELEASE.md` defines breaking changes and the deprecation policy for every
  surface. Breaking changes after v1 require a major version bump.

## 2. Sufficient validated coverage

The existing validated recipe portfolio is the v1 coverage baseline. Coverage
is expressed as recipe coordinates across service, accelerator, OS, intent,
and platform; a file's presence alone is not evidence that a coordinate works.

### Maturity

- **Supported** — the upstream recipe resolves and bundles, deploys on real
  hardware, passes its declared validation phases, and has published,
  verifiable evidence.
- **Preview** — the recipe path and evidence are useful for early adoption,
  but AICR does not yet make the complete production support and lifecycle
  commitment required for Supported status.
- **Planned** — tracked work that is not part of the v1 release contract.

### Remaining coverage boundary

| Coordinate | v1 maturity | State |
|---|---|---|
| EKS × GB300 × Ubuntu × training × Kubeflow | Supported | Complete; deployment, conformance, and performance evidence published |
| EKS × GB300 × Ubuntu × inference × Dynamo | Supported | Complete; deployment, conformance, and performance evidence published |
| RKE2 × VR200 × Ubuntu × training/inference | Preview | Evidence exists; upstream Preview contract tracked by [#2326](https://github.com/NVIDIA/aicr/issues/2326) |
| Broader VR200 multi-cloud coverage, DPF/BlueField integration, and full observability qualification | Planned | Post-v1 |

[AICR Recipe Validation](https://validation.aicr.run/) is the canonical public
evidence channel. It publishes recipe coordinates, AICR and Kubernetes
versions, per-source and per-test results, evidence identities, historical
runs, and reproducible `aicr evidence verify` commands. A separate TestGrid
implementation is not a v1 dependency.

Coverage integrity matters as much as portfolio breadth. Validation must
measure the artifact a recipe actually ships rather than a test-only fixture;
the remaining fabric-runtime parity work is tracked by
[#2217](https://github.com/NVIDIA/aicr/issues/2217).

### Acceptance

- Every Supported coordinate has published, digest-addressed evidence for its
  declared deployment, conformance, and performance phases.
- GB300 EKS training/Kubeflow and inference/Dynamo remain green against the
  shipped recipes.
- VR200 is clearly labeled Preview; it is not presented as a fully supported
  multi-cloud or lifecycle-qualified platform.
- Validation measures shipped recipe/runtime behavior rather than an
  independently constructed test fixture.
- The evidence for the declared v1 matrix is readable and independently
  verifiable through <https://validation.aicr.run/>.

## Established foundations

### Contribution UX

AICR retains its user, integrator, and contributor documentation, recipe
quality checks, KWOK coverage, and community contribution path. Additional
self-service tooling — including a dedicated `aicr recipe validate` command,
recipe scaffolding, and further review-pipeline automation — continues after
v1 and does not block the v1 tag.

### Closed supply chain

The v1 supply-chain baseline is complete under
[#1149](https://github.com/NVIDIA/aicr/issues/1149): build provenance, bundle
and evidence signing and verification, offline and private trust modes,
KMS-backed verification, signed recipe data, verify-on-deploy guidance, and
end-user verification documentation are shipped.

## Post-v1 priorities

- Promote VR200 from Preview to Supported through broader recipe coverage,
  hardware qualification, UAT, observability, and operational runbooks.
- Continue contribution-path automation and review-pipeline improvements.
- Build the machine-readable component upgrade lifecycle described by
  [#2424](https://github.com/NVIDIA/aicr/issues/2424), including cluster-aware
  upgrade checks and upgrade/rollback validation.
- Continue CNCF AI Conformance work as its requirements mature, treating
  conformance evidence as a first-class validator output.
