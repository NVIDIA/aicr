# ADR-006: Container Image Pinning Policy

## Problem

AICR currently pins container images inconsistently across the registry. The
just-published BOM (`docs/user/container-images.md`) catalogs **70 unique
images across 22 components**; only **4 carry a digest**, and **4 helm
components have no chart-version pin at all** (`gpu-operator`,
`network-operator`, `nvidia-dra-driver-gpu`, `nodewright-operator`). Every
other version is a mutable tag.

Three classes of customer pain follow:

- **Same recipe, different bytes.** A render today and a render six months
  from now can produce a different image set when chart-default sub-images
  are unpinned and upstream charts publish new defaults. The weekly
  `bom-refresh` action surfaces this drift; without a policy it's
  open-ended.
- **No customer-facing reproducibility statement.** Security review and
  air-gap planning need a definitive "this recipe at this AICR version
  deploys exactly these bytes." We cannot assert that today.
- **No clear contract for new components.** A contributor adding a new
  chart has no documented expectation about what to pin. The result is
  ad-hoc per-component decisions that drift in different directions.

The trade-off is real and not free: pinning everything in-tree blocks
upstream security patches behind an AICR release; pinning nothing maximizes
patch flow but breaks reproducibility. We need an intentional position for
each layer of the supply chain.

## Non-Goals

- **Implementing admission-time digest verification.** That's Stage 3 of the
  supply-chain epic (#745); this ADR only declares it as the destination for
  the layer this policy intentionally leaves unpinned.
- **Forking upstream charts** to expose digest fields they don't provide
  natively. Maintenance burden is unbounded.
- **Mandating signature verification on upstream artifacts** at this policy
  layer. Signing requests to upstream maintainers are a separate workstream
  filed alongside #740 once upstream repos have been surveyed.
- **Re-pinning every existing in-tree image reference** in the same change.
  This ADR sets policy; #748/#749 land the in-tree changes incrementally
  under that policy.

## Context

The supply chain divides into three concentric layers, each with a different
ownership boundary:

1. **Helm chart selection.** AICR picks a chart at a specific version. The
   chart name + version is recorded in `recipes/registry.yaml`.
2. **Explicit image overrides.** AICR's per-component `values.yaml` and
   embedded manifests override specific image references the upstream chart
   would otherwise pull. Today these are tag-pinned; some carry digests.
3. **Chart-default sub-images.** The chart's own `values.yaml` references a
   set of sub-images (`gpu-operator` alone ships ~14: driver, toolkit, dcgm,
   dcgm-exporter, device-plugin, validator, mig-manager, vgpu-manager,
   sandbox-device-plugin, gfd, gdrcopy, kata, cc-manager). AICR does not
   override these; the upstream chart's defaults are deployed.

Layer 1 is wholly under AICR's control. Layer 2 is wholly under AICR's
control. Layer 3 is under the upstream chart maintainer's control; AICR can
override individual sub-images with bundle-time `--set` flags but
maintaining a complete override matrix per component is intractable. Many
charts also do not expose digest fields for sub-images natively, only tags.

The reproducibility / patch-flow trade-off looks different at each layer:

| Layer | Pin? | Cost of pinning | Cost of not pinning |
|-------|------|-----------------|---------------------|
| Chart version | **yes** | tiny — one version per component, Renovate-driven bumps | render drift across time, no audit baseline |
| Explicit override (image we already specify) | **yes, digest** | small — tag→digest is mechanical, Renovate handles ongoing rotation | tag-rebuild silently changes deployed bytes |
| Chart-default sub-image | **no** in-tree | very high — per-sub-image override matrix per chart, blocks upstream patches | residual drift; mitigated by chart-version pin + Stage 3 admission verification |

## Decision

**Three-layer pinning policy:**

1. **Pin chart versions for every Helm component, no exceptions.**
   `recipes/registry.yaml` MUST declare `defaultVersion` for every helm
   component. New components without a pin are rejected at PR review and by
   `make bom BOM_STRICT=1` (wired into `make qualify`).

2. **Digest-pin every image AICR overrides explicitly in-tree.** Anywhere
   AICR's `recipes/components/<name>/values.yaml` or embedded manifests set
   an `image:` value, that value MUST carry an `@sha256:` digest in addition
   to its tag. Tag-only references are rejected by the new test added under
   #761 once it's extended to the helm-rendered surface (see #765).

3. **Do not pin chart-default sub-images in-tree.** AICR ships chart-default
   sub-images at whatever the upstream chart resolves at the pinned chart
   version. Reproducibility for these images is delivered by
   admission-time digest verification (#745), not by per-sub-image
   in-tree overrides.

**Renovate is the durable maintenance mechanism.** It auto-opens PRs for
chart-version bumps and digest rotations under the same tag. Patches still
flow; the diff lands as a normal PR with CI.

### Per-component application

| Component | Chart version | Explicit overrides | Chart-default sub-images |
|-----------|---------------|--------------------|--------------------------|
| `gpu-operator` | pinned (Phase B of #748) | digest-pin our overrides (#749) | unpinned in-tree → #745 |
| `network-operator` | pinned (Phase B of #748) | digest-pin our overrides (#749) | unpinned in-tree → #745 |
| `nvidia-dra-driver-gpu` | pinned (Phase B of #748) | digest-pin our overrides (#749) | unpinned in-tree → #745 |
| `nodewright-operator` | pinned (Phase B of #748) | already digest-pinned (skyhook packages) | unpinned in-tree → #745 |
| `cert-manager` | already pinned (`v1.20.2`) | none today | unpinned in-tree → #745 |
| `kube-prometheus-stack` | already pinned (`84.4.0`) | none today | unpinned in-tree → #745 |
| `nfd`, `k8s-ephemeral-storage-metrics` | pinned (Phase A of #748, ✅) | none today | unpinned in-tree → #745 |
| All others | pinned today | digest-pin our overrides (#749) | unpinned in-tree → #745 |

### Contract for new components

A PR adding a new helm component to `recipes/registry.yaml` must:

1. Set `defaultVersion`. CI gates this via `make bom BOM_STRICT=1`.
2. If the PR's `recipes/components/<name>/values.yaml` overrides any
   `image:` reference, every overridden value must include an `@sha256:`
   digest.
3. If the new component is on a registry that uses regional or
   account-scoped URIs (e.g., AWS regional ECR), document the override
   pattern in the values.yaml comment block, following the precedent set
   by `aws-efa` (PR #774, #764).

## Consequences

### Positive

- **Predictable customer-facing claim.** "AICR vX.Y deploys recipe R, which
  produces the chart-version set documented in
  `docs/user/container-images.md`. Within that set, every image AICR
  controls in-tree is digest-pinned. Chart-default sub-images are deployed
  at the upstream chart's defaults at the pinned chart version."
- **Bounded maintenance.** One chart-version per component; digest rotation
  on explicit overrides handled by Renovate. No per-sub-image override
  matrix.
- **Clear contract for new components.** PR review and CI both enforce.
- **Sequencing for the rest of #739 unlocks.** #748 Phase B (NVIDIA-owned
  charts), #749 (digest pinning explicit refs), and #745 (admission-time
  verification) all flow from this policy without further policy debates.

### Negative

- **Chart-default sub-images remain non-deterministic in-tree.** A chart
  rebuild that re-tags `:latest` (or the chart's chosen tag) for a
  sub-image silently changes deployed bytes. The weekly `bom-refresh`
  action surfaces the change; admission-time verification (#745) is the
  long-term answer. Customers who need today, end-to-end byte-level
  reproducibility must wait for Stage 3.
- **`gpu-operator` chart-default sub-images stay unpinned (~14 images).**
  This is the single largest implicit-surface component. The decision
  accepts that tradeoff in exchange for upstream patch flow.
- **More CI friction on Renovate PRs.** Pinning chart versions and
  digest-pinning explicit refs means more bot-driven PRs land per quarter.
  Renovate's grouping config keeps the noise manageable.

### Neutral / Future direction

- **Upstream signing.** AICR and NVSentinel ship cosign-signed artifacts
  with SBOMs. Asking upstream chart and image publishers to do the same
  enables admission-time signature verification (#745) without per-image
  digest allow-lists. A separate workstream — filed against each upstream
  repo once they've been surveyed — pursues that. This ADR doesn't depend
  on it; #745 can land first as a digest-allow-list policy and migrate to
  signature verification when upstream coverage is sufficient.
- **Per-recipe stability signal (#613).** Orthogonal but adjacent — once a
  recipe declares its stability, customers can map "stable" to "byte-level
  reproducible (chart pin + digest pin + admission verification)" and
  "preview" to "chart-pinned only".

## Adoption plan

1. **This ADR lands.** Sets policy, no code changes.
2. **#748 Phase B lands.** Pin `gpu-operator`, `network-operator`,
   `nvidia-dra-driver-gpu`, `nodewright-operator` chart versions. Wire
   `make bom BOM_STRICT=1` into `make qualify` so the contract is
   enforced for new components.
3. **#765 lands.** Tighten `pkg/bom.isLikelyImage` so chart-default
   placeholders (`vgpu-manager`-style bare scalars) don't dilute the
   published BOM.
4. **#749 lands.** Digest-pin explicit overrides; configure Renovate's
   `pinDigests` for ongoing rotation; extend `make qualify` to require
   digests on explicit refs.
5. **#745 lands** (separately tracked, Stage 3 of #739). Admission-time
   digest or signature verification covers chart-default sub-images.

Each step is independently revertible without invalidating the policy.
