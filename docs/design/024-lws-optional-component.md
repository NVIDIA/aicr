# ADR-024: LeaderWorkerSet (LWS) as an Opt-In Registry Component

## Status

**Proposed** — 2026-09-01.

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

The chart version pin, chart digest, and component values are selected in
the implementation PR against the then-current upstream releases, with the
selection rationale recorded there — upstream moves faster than this ADR
(v0.9 through v1.0.0 shipped while it was in review), so binding a version
here would only manufacture drift. If the registry entry records a
`defaultDigest`, it is the SHA-256 of the pinned chart archive, per the
`recipes/registry.yaml` header — not the OCI pull digest.

## Why

LWS is the Kubernetes SIGs API for running leader/worker pod groups as one
unit — the standard workload primitive for multi-node inference serving.
Serving platforms built on AICR clusters (vLLM, ModelPlane, and similar
stacks) need a qualified, pinned LWS instead of installing it from unpinned
upstream sources. Dynamo multi-host can also consume LWS, but only where an
overlay opts out of grove: every stock Dynamo overlay ships grove as its
pod-group orchestrator (`TestGroveLeavesAreExactlyTheDynamoLeaves` pins the
correspondence), so stock Dynamo is not an LWS consumer.

No stock recipe needs LWS yet, so it ships on the
registered-without-a-stock-consumer boundary that `nvcre` established:
`TestNVCRERegisteredWithoutOverlay` keeps that component installable from
the registry while no shipped overlay or mixin references it, and Gate 5
adds the same guard for `lws`.

`registry.k8s.io`, the chart and image host, is already on the egress
allowlist (see [registry-egress](../contributor/registry-egress.md)).

## What AICR Owns

- The chart pin: registry `defaultVersion`, bumped like any other
  component. The controller image is a chart-default sub-image and stays
  governed by the chart pin — per ADR-006 rule 3, AICR records no in-tree
  image-tag override.
- The health check: read-only assertions that the CRD is Established, the
  controller Deployment is available, and its pods are healthy.
- The BOM row, component-catalog row, and ADR-021 upgrade-record coverage
  on every version bump.
- Review of the chart's rendered RBAC and webhook configuration — upstream
  content AICR installs unmodified. AICR re-reads both at each pin bump
  and calls out any widening in the bump PR; it does not patch the chart.
  Gate 3 covers only the initial registration; this line carries the
  obligation across bumps, which happen before adoption too and reach
  custom-overlay consumers.
- Mirror coverage: `aicr mirror list` renders each referenced component's
  chart and extracts its images, so a recipe that references `lws` lists
  the chart and the controller image with no LWS-specific code. AICR owns
  keeping the pinned chart rendering cleanly there.

Upstream owns the chart, image, and CRD schema. Users own every
`LeaderWorkerSet` CR — AICR ships no workload CRs.

Pin bumps before adoption carry the standard obligations (BOM refresh,
upgrade record, CRD inventory) and do not amend this ADR; the ADR is
amended once, at stock-recipe adoption (Gate 8).

## Gates

Each gate names the evidence that shows it was met, so each is checkable
on `main`.

### Registration gates — evidence lands in the implementation PR

1. **Pin selection.** `defaultVersion` chosen from the then-current
   upstream releases, with the selection rationale in the PR description.
   Any recorded digest is the chart-archive SHA-256 (see Decision).
2. **Health check.** The read-only check is registered in `registry.yaml`
   and verified live via `make component-test COMPONENT=lws`, with the run
   recorded in the PR's Testing section.
3. **RBAC and webhook review.** The PR records its rendered-RBAC and
   webhook-configuration review — the rendered rules, what they permit,
   and the conclusion — as ADR-019 did for `k8s-aibom`.
4. **CRD lifecycle.** The chart ships its CRDs under `crds/`, so per
   ADR-021 plain Helm leaves them at the first-install schema — and the
   CRD set is already growing: lws v1.0.0 adds two `DisaggregatedSet`
   CRDs beside `LeaderWorkerSet`. `ownsCRDs` and the manual CRD step are
   not alternatives. Per ADR-021 Decision 8, `ownsCRDs` automates the
   CRD upgrade only on the helm and flux deployers: helmfile emits no
   CRD hook, terraform upgrades through `helm_release`, which skips
   `crds/`, and fleet cannot run `apply-crds.sh` either. The PR records
   the `ownsCRDs` decision (with the audit its doc comment requires) for
   helm and flux, and every transition record carries the manual CRD
   step as an explicit group for helmfile, terraform, and fleet
   regardless of that decision.
5. **Registered without a stock consumer.** A registry test in the style
   of `TestNVCRERegisteredWithoutOverlay` keeps `lws` installable while no
   stock overlay or mixin references it.

### Adoption gates — evidence lands in the adopting recipe's PR and UAT lane

6. **Functional verification.** The health check is read-only and KWOK
   validates scheduling, so neither shows a `LeaderWorkerSet` being
   admitted and reconciled. The adopting recipe's UAT lane creates a
   `LeaderWorkerSet` and asserts the group reaches Ready — the in-process
   health-check path cannot create objects, so this runs in UAT, not in
   `validate`.
7. **Kueue and gang scheduling.** Two decisions ride along with adoption,
   not before it: enabling `leaderworkerset` in kueue's pinned
   `integrations.frameworks` (the trimmed framework list in
   `recipes/components/kueue/values.yaml` does not queue LWS workloads
   today), and any gang-scheduling wiring. The amendment records both
   outcomes, deferrals included.
8. **Amendment.** Adoption amends this ADR to name the recipe and record
   the Gate 7 outcomes.

## Non-Goals

- Adding LWS to any stock overlay, mixin, or base recipe now.
- Changing kueue's framework list now.
- Shipping default `LeaderWorkerSet` CRs.
- A standing CI e2e lane while no stock recipe references the component
  (`make component-test COMPONENT=lws` is the on-demand check).

## References

- [kubernetes-sigs/lws](https://github.com/kubernetes-sigs/lws)
- [ADR-006](006-image-pinning-policy.md) — rule 3: no in-tree pins for
  chart-default sub-images
- [ADR-019](019-k8s-aibom-runtime-inventory.md) — the recorded
  rendered-RBAC review this follows
- [ADR-021](021-component-upgrade-safety.md) — CRD upgrade behavior,
  `ownsCRDs`, and the Decision 8 deployer gap Gate 4 builds on
- [registry-egress](../contributor/registry-egress.md) — the
  `registry.k8s.io` allowlist entry
