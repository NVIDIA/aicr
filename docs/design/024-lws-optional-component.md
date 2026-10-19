# ADR-024: LeaderWorkerSet (LWS) as an Opt-In Registry Component

## Status

**Proposed** — 2026-09-01. Revised 2026-10-19 to separate the decision from
its implementation and to state the ownership boundary and adoption gates.

Implementation follows acceptance as a separate change. The registry entry,
component values, health check, component catalog row and BOM row were
drafted at
[`405d96dd2`](https://github.com/NVIDIA/aicr/pull/2500/commits/405d96dd278aa15ad031406d00f839f8374a90b0)
on this ADR's pull request and are not part of it.

Qualified artifact set: chart `lws` v0.8.0 from
`oci://registry.k8s.io/lws/charts` (pull digest
`sha256:e7996d0b9ca8a1ab2d86458b0435a8d842389b81325dc650792389a2c1ad7f57`),
controller image `registry.k8s.io/lws/lws:v0.8.0`, CRD
`leaderworkersets.leaderworkerset.x-k8s.io`, namespace `lws-system`.

## Decision

Register [LeaderWorkerSet](https://github.com/kubernetes-sigs/lws) as an
opt-in component: pinned, health-checked, and in the BOM, but referenced by
no stock recipe. Users enable it with one `componentRef` in a custom or
external overlay:

```yaml
spec:
  componentRefs:
    - name: lws
      type: Helm
      valuesFile: components/lws/values.yaml
```

## Why

LWS is the Kubernetes SIGs API for running leader/worker pod groups as one
unit — the standard workload primitive for multi-node inference serving.
Serving platforms built on AICR clusters (vLLM, Dynamo multi-host,
ModelPlane, and similar stacks) need a qualified, pinned LWS instead of
installing it from unpinned upstream sources. None of the stock recipes need
it yet, so it ships the same way as `slinky-topograph` and `k8s-aibom`:
available and qualified, installed only where an overlay asks for it.

## What AICR Owns

- The pins: registry `defaultVersion` and the `image.manager.tag` pin in
  component values, bumped together, and the
  [ADR-021](021-component-upgrade-safety.md) transition record each pin bump
  needs.
- The health check: CRD Established, `lws-controller-manager` available in
  `lws-system`, pods healthy. Verified live on Kind with
  `make component-test COMPONENT=lws` against the drafted implementation.
- The BOM row and requalification on every version bump.
- Review of the chart's RBAC. The RBAC objects and webhook configuration the
  chart renders are upstream content that AICR installs unmodified. AICR reads
  them at each pin and calls out any widening in the bump pull request; it
  does not patch the chart.
- Mirror coverage. `aicr mirror list` renders each referenced component's
  chart and extracts its images, so a recipe that references `lws` lists the
  chart and `registry.k8s.io/lws/lws` with no LWS-specific code. AICR owns
  keeping the pinned chart rendering cleanly there.

Upstream owns the chart content, including its RBAC, the image, and the CRD
schema. Users own every `LeaderWorkerSet` CR and the RBAC that decides who may
create one; AICR ships no workload CRs.

Notable properties of v0.8.0: no cert-manager dependency (the chart
self-manages its webhook certificate; `enableCertManager: true` plus a
`dependencyRef` is the switch if that ever changes), scheduling paths are
the chart's top-level `nodeSelector`/`tolerations`, and `registry.k8s.io`
is already on the egress allowlist.

## Adoption Gates

A stock recipe or mixin references `lws` only after all of the following
hold. Each is checkable on `main` or with one command.

1. The implementation is on `main`: registry entry, component values, health
   check, catalog row and BOM row, qualifying the artifact set in the Status
   section as one set (chart digest, controller image, CRD). A newer pin
   requalifies all three together and amends this ADR.
2. `make component-test COMPONENT=lws` passes on Kind at the pinned version.
3. The chart's rendered RBAC and webhook configuration at the pin have been
   reviewed, as described under What AICR Owns.
4. `aicr mirror list` for the adopting recipe lists the `lws` chart and
   `registry.k8s.io/lws/lws` at the pinned tag.
5. The adopting leaf's KWOK lane passes with `lws` included, and the adoption
   pull request decides the two items below.
6. The adoption pull request amends this ADR to name the recipe.

## Follow-Up: Stock Recipe Adoption

When a stock inference recipe needs LWS (for example a vLLM-based
multi-host serving leaf), that leaf adds the `componentRef` — a mixin once
two or more leaves need it. Its existing KWOK/UAT lanes then cover LWS
automatically. Two decisions ride along with that adoption, not before it:

- enabling `leaderworkerset` in kueue's pinned `integrations.frameworks`
  (today Kueue does not queue LWS workloads),
- any gang-scheduling wiring (`gangSchedulingManagement` is empty in the
  drafted values).

That adoption amends this ADR to name the recipe.

## Non-Goals

- Adding LWS to any stock overlay, mixin, or base recipe now.
- Changing kueue's framework list now.
- Shipping default `LeaderWorkerSet` CRs.
- A standing CI e2e lane while no stock recipe references the component
  (`make component-test COMPONENT=lws` is the on-demand check).

## References

- [kubernetes-sigs/lws v0.8.0](https://github.com/kubernetes-sigs/lws/releases/tag/v0.8.0)
- [ADR-019](019-k8s-aibom-runtime-inventory.md) — the registry-only
  component boundary this follows
- [ADR-021](021-component-upgrade-safety.md) — transition records for pin
  bumps
- `docs/user/component-catalog.md` — lws row and opt-in wiring, added by the
  implementation change
