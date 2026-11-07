# Component Memory Sizing

A sizing file lowers a controller's memory request and limit for clusters
smaller than the ones its values files are sized for, keyed on the GPU node
count a user states with `aicr bundle --nodes` or, on an `argocd-helm`
bundle, `sizing.nodeCount`. This page is how you write or refine one.

The design is ADR-030 ([#3642](https://github.com/NVIDIA/aicr/issues/3642)).
The schema and loader live in `pkg/recipe/sizing.go`; the generation-time pass
in `pkg/bundler/sizing.go`; the install-time rows and template in
`pkg/bundler/deployer/argocdhelm/sizing.go`. The ADR holds the rules below;
the tier values and their readings live only in the sizing files, so tuning
a value is a data change.

## How sizing applies

- The values files are the top tier. No count, or a count above the highest
  `maxNodes`, changes nothing.
- A count selects the lowest tier whose `maxNodes` is at or above it. Each
  memory request and limit the tier sets becomes the smaller of the resolved
  value and the tier's, compared as Kubernetes quantities. Sizing never
  raises a value.
- A path the values files do not set is skipped. So are paths set by the
  recipe's inline `overrides`, an ADR-015 profile lock set, or a `--set`,
  `--set-json` or `--set-file` override.
- After every override, a sized request above its limit fails the bundle with
  `ErrCodeInvalidRequest`.
- On `argocd-helm`, an upstream chart that is not vendored is sized at install
  time: the bundler writes `static/<name>-sizing.yaml` with one row per tier
  bound, and the child Application template merges the row
  `sizing.nodeCount` selects between the static values and the component's
  root-chart values. Vendored and local charts are sized at generation time.
- A path that a bundle-rendered manifest reads is never sized: that manifest
  is rendered at generation time and cannot see install-time values.

## The file

`recipes/components/<name>/sizing.yaml`, referenced from the registry entry:

```yaml
- name: gpu-operator
  sizing:
    file: components/gpu-operator/sizing.yaml
```

```yaml
apiVersion: aicr.run/v1beta1
kind: ComponentSizing
basis: gpuNodes
resources:
  - path: operator.resources          # a resources block, as --set writes it
    tiers:
      - maxNodes: 250
        requests: {memory: 300Mi}
        limits: {memory: 768Mi}
        measurements:
          - sets: [requests]          # which fields this reading set; [] if it only constrained
            kind: windowed            # or instantaneous
            window: 24h               # windowed only
            gpuNodes: 244
            nodes: 249                # total nodes, optional
            pods: 11029               # optional
            p90: 300Mi                # optional
            max: 365Mi
            truncatedAt: 2Gi          # optional: the limit in force while OOM-killed
            date: 2026-10-14
            ref: https://github.com/NVIDIA/aicr/issues/3214   # optional link to the raw data
      - maxNodes: 2000                # a bound with no values changes nothing
```

The loader fails closed on: a malformed `path`; a `basis` other than
`gpuNodes`; `maxNodes` values that do not strictly ascend; an unparseable
quantity; a limit below its request; a tier value above the same field in the
next higher tier; a tier that sets `requests` or `limits` with no measurement
whose `sets` names it; and a measurement missing `kind`, `gpuNodes`, `max` or
`date`, or a windowed one missing `window`.

`TestRegistrySizingPathsAreSet` fails when a sizing path sets memory in none
of the component's `values*.yaml` files, so a key renamed by a chart bump
cannot turn sizing into a silent no-op. No unit test asserts a real tier's
values; they are validated against measurements, not pinned in the suite.

Keep the same `maxNodes` bounds in every component's file, so one count
selects the same row everywhere.

## Deriving a tier

- **Covering reading.** Size a tier from a reading within 10% of its bound,
  above or below. Prefer windowed readings with a p90, then windowed with a
  max only, then instantaneous. With none that close, the nearest reading
  above the bound covers the tier.
- **Request:** the highest p90 among the covering reading and every reading
  inside the tier. Use a reading's max where p90 was not kept, and a
  truncated reading's value as it stands.
- **Limit:** the highest peak among the covering reading and every reading
  inside the tier, × 1.3.
- **Truncated readings.** If a reading inside the tier was taken while the
  container was being OOM-killed, take the limit from the nearest reading
  above the bound whose peak exceeds the limit that killed it.
- **Instantaneous readings** stand in for p90 as they are, and × 1.7 for the
  peak.
- **DaemonSets:** the request uses the median across pods of each pod's p90;
  the limit uses the largest single-pod peak.
- **Drift.** For a controller whose peak grows at a constant node count, use
  the drifted peak.
- **Rounding:** up to a multiple of 256Mi below 2Gi, of 512Mi from 2Gi.
- **Caps:** no value above the same field in the tier above, and no request
  above the values file's. A tier may equal the tier above.

Record every reading the derivation used in `measurements`, with `sets`
naming the fields it decided. A reading that only constrained the result,
such as a covering reading a higher one outweighed, gets `sets: []`.

## Taking readings

Windowed readings come from cadvisor's `container_memory_working_set_bytes`.
Read p90 and max over 24 hours, or 7 days where the Prometheus keeps it:

```promql
max by (pod) (quantile_over_time(0.9, container_memory_working_set_bytes{namespace="gpu-operator", container="gpu-operator"}[24h]))
max by (pod) (max_over_time(container_memory_working_set_bytes{namespace="gpu-operator", container="gpu-operator"}[24h]))
```

For a DaemonSet, take the median across pods of each pod's p90:

```promql
quantile(0.5, max by (pod) (quantile_over_time(0.9, container_memory_working_set_bytes{container="<container>"}[24h])))
```

Run them against the fleet Prometheus where the cluster ships cadvisor, or
through the in-cluster Prometheus with the Kubernetes API service proxy:

```bash
kubectl get --raw "/api/v1/namespaces/<namespace>/services/<prometheus-service>:9090/proxy/api/v1/query?query=<url-encoded query>"
```

Count GPU nodes with `kubectl get nodes -l nvidia.com/gpu.present=true`, and
record total nodes and running pods at the same time. An instantaneous
reading is one `kubectl top pod` sample; it is a floor, not a peak.

## Refining a tier

1. Take the readings above on a cluster near the tier's bound.
2. Apply the derivation to the new readings together with the ones the file
   already records, and replace the tier's values.
3. Update `measurements`: add the new readings, set `sets` on the ones that
   now decide each field, and drop readings nothing uses any more.
4. Run `go test ./pkg/recipe/ -run Sizing` and bundle the recipe at a count
   inside the tier to see the new values.

When a chart bump changes what a controller caches, re-measure every tier of
that component before the bump lands. A comment at the pin in
`recipes/registry.yaml` or at the top of the sizing file says when that is
already known to be owed.
