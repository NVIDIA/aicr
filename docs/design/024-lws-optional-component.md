# ADR-024: LeaderWorkerSet (LWS) as an Opt-In Registry Component

## Status

**Proposed** — 2026-09-01.

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
  component values, bumped together (see the values-file MAINTENANCE note).
- The health check: CRD Established, `lws-controller-manager` available in
  `lws-system`, pods healthy. Verified live via
  `make component-test COMPONENT=lws`.
- The BOM row and requalification on every version bump.

Upstream owns the chart, image, and CRD schema. Users own every
`LeaderWorkerSet` CR — AICR ships no workload CRs.

Notable properties of v0.8.0: no cert-manager dependency (the chart
self-manages its webhook certificate; `enableCertManager: true` plus a
`dependencyRef` is the switch if that ever changes), scheduling paths are
the chart's top-level `nodeSelector`/`tolerations`, and `registry.k8s.io`
is already on the egress allowlist.

## Follow-Up: Stock Recipe Adoption

When a stock inference recipe needs LWS (for example a vLLM-based
multi-host serving leaf), that leaf adds the `componentRef` — a mixin once
two or more leaves need it. Its existing KWOK/UAT lanes then cover LWS
automatically. Two decisions ride along with that adoption, not before it:

- enabling `leaderworkerset` in kueue's pinned `integrations.frameworks`
  (today Kueue does not queue LWS workloads; the component catalog says so),
- any gang-scheduling wiring (`gangSchedulingManagement` is empty in the
  qualified values).

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
- `docs/user/component-catalog.md` — lws row and opt-in wiring
