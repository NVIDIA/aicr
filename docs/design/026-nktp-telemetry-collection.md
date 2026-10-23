# ADR-026: Adopt NKTP as the stock cluster telemetry pipeline in AICR

## Status

**Proposed** — 2026-10-23.

## Decision Summary

AICR adopts NKTP, the NVKE Telemetry Pipeline (`nktp`), as the telemetry
pipeline of every stock recipe except `ocp` and
`b300-mk8s-ubuntu-inference-dynamo`. NKTP collects, processes, and exports
metrics, logs, and traces to a backend. Its bundled `otel-lgtm` is the
default backend. It is for evaluation only, and the cluster owner replaces it
for production use.

`kube-prometheus-stack` stays in `base`, reduced to the prometheus-operator and
one Prometheus server. It is no longer a monitoring stack or a backend. It is a
runtime dependency that supplies the few metrics AICR components query, such as
the GPU metrics behind `prometheus-adapter`. Each such component declares what
it reads in the registry, and the bundler scopes that Prometheus server to
exactly that. Consumers keep today's endpoint.

## Context and Decision Drivers

### AICR's telemetry today

Every stock recipe installs `prometheus-operator-crds` and
`kube-prometheus-stack` in the `monitoring` namespace. `kube-prometheus-stack`
runs a Prometheus server, Grafana, Alertmanager, `kube-state-metrics`, and
`node-exporter`, and collects metrics only. Its defaults are for evaluation: the
Prometheus server on `emptyDir`, Grafana with the password `admin`, and
Alertmanager with no receiver. Five platform overlays give the Prometheus server
a 50Gi volume.

Its Prometheus server serves two roles, which this decision separates:

- **Cluster observability:** dashboards, node and Kubernetes metrics, and
  alert rules. No AICR component reads these.
- **Runtime metrics for components:** `prometheus-adapter` turns three DCGM
  series into the custom and external metrics APIs, which HPAs on GPU
  metrics and the `pod-autoscaling` and `cluster-autoscaling` conformance
  checks use. The Dynamo planner, in its SLA mode, queries frontend and
  router traffic series. The `ai-service-metrics` validator queries
  `DCGM_FI_DEV_GPU_UTIL`.

### NKTP

NKTP, the NVKE Telemetry Pipeline, is a scalable OpenTelemetry pipeline for
collecting, processing, and exporting Kubernetes metrics, logs, and traces,
with sensible defaults and flexible, shareable configuration. It ships as one
Helm chart:
[`nktp`](https://github.com/dsx-ai-factory/nvke/tree/main/nktp/deploy/charts/nktp).
The chart installs the OpenTelemetry (OTel) Operator and a fixed topology of
collectors, each an `OpenTelemetryCollector` resource.

| Workload | Role |
|---|---|
| `gateway_metrics`, `gateway_logs`, `gateway_traces` | Per-signal export to the backend. Autoscaled. |
| `daemonset` | One per node: container and host logs, host metrics, node-local scraping, OTLP ingest. |
| `large_target_metrics` | Scrapes endpoints that belong to no node, such as `kube-state-metrics`. |
| `cluster_events` | Kubernetes events, once per cluster. |
| `watchdog` | Receives the collectors' own telemetry on a separate path. |

Three Target Allocators assign scrape targets. Two of them discover every
`ServiceMonitor`, `PodMonitor`, `Probe`, and `ScrapeConfig`; the third
serves the watchdog's own targets. NKTP bundles `kube-state-metrics`,
`node-exporter`, `metrics-server` (off by default), and `otel-lgtm`, and it
requires cert-manager.

### Decision drivers

1. **Logs and traces.** A stock cluster collects metrics only. Logs go
   nowhere unless the owner brings an agent, and instrumented workloads have
   no endpoint to send traces to.
2. **GPU-node host metrics.** `kube-prometheus-stack`'s `node-exporter` follows
   the system node selector, so with one set, GPU nodes report no host metrics.
3. **Scale.** One Prometheus server scrapes every target, and the recipe offers
   no sharding.
4. **Runtime consumers keep working.** The adapter's APIs, the conformance
   checks, and the Dynamo planner must work without changes on their side.
5. **Class boundaries.** Under ADR-018, observability
   is `ops`, and a `core` component depends only on `core` components. The
   `core` components that render monitors therefore depend on the monitoring
   CRDs, never on a collector or a backend.

## Decision

### 1. Adoption Scope

`nktp` is a stock component in `base`, class `ops`. Cluster observability moves
to it: dashboards, node and Kubernetes metrics, logs, events, and the traces of
instrumented workloads. `ocp` keeps OpenShift's own monitoring.
`b300-mk8s-ubuntu-inference-dynamo` disables the monitoring set.

### 2. Observability Backend and Runtime Metrics Dependency

**Observability backend.** NKTP exports to `otel-lgtm` by default. This is
the one place its limits are stated. `otel-lgtm` is upstream's image for
development, demos, and testing. It runs as one pod with all data on
`emptyDir`, lost on every restart, including any NKTP upgrade that changes a
dashboard. Its Grafana grants anonymous Admin, it runs as root, and it has no
rule evaluation or Alertmanager. It is an evaluation backend and inherently
insecure in production: anyone who can reach it inside the cluster reads every
collected log as Grafana Admin. AICR does not harden it. For production the
owner disables it and points NKTP's exporters at their own backend, which owns
persistence, access control, alerting, and retention.

**Runtime metrics dependency.** `kube-prometheus-stack` stays in `base` as a
runtime dependency of the components that query metrics. It keeps its release,
namespace, and Service (`kube-prometheus-prometheus.monitoring:9090`), so the
adapter, the Dynamo planner, and the validators are unchanged. It runs only the
prometheus-operator and one Prometheus server. There is no Grafana,
Alertmanager, `node-exporter`, `kube-state-metrics`, rule, or default
ServiceMonitor. It stores hours of data on `emptyDir`, with no persistent
volume, because its consumers rely on recent data. It stays in production; an
owner may replace it under the contract in Decision 3.

**Why the two roles are separate.** Pointing the consumers at `otel-lgtm` ties
HPA and conformance to an evaluation backend and to NKTP's pipeline, and forces
owners to re-point every consumer when they move to production. Feeding a
separate Prometheus server from NKTP's pipeline keeps that coupling since a
Prometheus server outage can cause backpressure on NKTP's main pipeline.
Scraping the declared targets directly keeps the runtime path independent of
NKTP, at the cost of scraping those few targets twice.

### 3. Runtime Metrics Contract

A component that queries the runtime Prometheus server declares
`telemetry.promql` in its registry entry:

- `monitorsFrom` names the components whose ServiceMonitors and PodMonitors
  expose the data. Each resolves to its namespace in the resolved recipe.
- `series` lists RE2 patterns, matched against the metric name as scraped.

`prometheus-adapter` declares `gpu-operator` and the three DCGM gauges it
maps. `dynamo-platform` declares itself and the planner's SLA-mode traffic
series.

At bundle time, on every path (CLI, API server, and SDK), AICR takes the union
of the enabled consumers' declarations and writes it at value paths that
`kube-prometheus-stack`'s registry entry declares. The Prometheus server then
selects every monitor in exactly those namespaces, whatever their labels, keeps
only the declared series, and loads no rules, probes, or scrape configurations.
Those paths cannot be overridden. A declared source that is absent or disabled
fails the bundle, as does a pattern that does not compile. With
`kube-prometheus-stack` disabled nothing is injected, and consumers use the
platform's Prometheus, as `prometheus-adapter-ocp` does with OpenShift's Thanos
Querier. Adding a consumer is a registry change only; overlays carry no
telemetry configuration. Validators and evidence collection query only declared
series, or query the observability backend.

**Replacing the runtime Prometheus server.** An owner who points consumers at
their own backend must provide:

- the Prometheus query API, at a URL and path prefix;
- authentication and CA trust that each consumer can carry: the adapter's
  authentication configuration, as on OpenShift, and the planner's token and
  CA. The planner sends no custom headers today;
- a selector that scopes queries to this cluster, if the backend is shared.
  Without one, external metrics average every cluster's GPUs, and
  `ai-service-metrics` passes on another cluster's data;
- network reachability from every consumer.

The Prometheus CRD marks scrape classes, which carry the series filter, as
experimental. Their behavior is pinned with `prometheus-operator-crds`.

### 4. Upstream Ownership Boundary

NKTP owns its topology, every collector configuration, target discovery,
the RBAC and versions of its bundled dependencies, the collector image,
scale validation, and its release process.

AICR owns release selection, the values files, node placement paths,
dependency edges, health and conformance assertions, rendering across
deployers, mirror and BOM coverage, transition records, catalog
documentation, the runtime Prometheus server's values, and the runtime metrics
contract.

AICR consumes NKTP's public, versioned releases as it consumes
`kube-prometheus-stack`'s, with no fork or patch, and adapts them through
values.

### 5. Compatibility Contracts

Each contract states the requirement; the implementation sets the values
that meet it.

**Dependencies and readiness.**

- `nktp` depends on cert-manager, for its operator's webhooks, and on
  `prometheus-operator-crds`, for its monitors. Both are `core`.
- `nktp` also depends on `kube-prometheus-stack`, so that on an existing cluster
  `kube-prometheus-stack`'s `node-exporter` is gone before NKTP's binds the same
  host port.
- A component that renders monitors depends only on
  `prometheus-operator-crds`.
- NKTP's autoscalers need the resource metrics API. Each platform overlay
  enables NKTP's bundled metrics-server where the platform ships no such API.
- The node collector needs the `privileged` Pod Security level, which its
  namespace carries.
- Nothing inside the release is sequenced. The collector resources are
  created at the same time as the operator; its webhooks admit them even
  before the operator is running, and the operator builds the collectors once
  it starts. A deployer can report the release ready before any collector
  exists, as `helm install --wait` does, so AICR's health check, not the
  deployer, confirms the pipeline is up.

**CRDs.**

- NKTP is the sole owner of the `opentelemetry.io` CRDs. AICR replaces them
  on upgrade wherever the deployer supports a CRD step, and the step never
  applies the monitoring CRDs bundled inside NKTP's chart, which belong to
  `prometheus-operator-crds`.
- Uninstall keeps the CRDs but deletes every collector, through a
  `pre-delete` hook on the deployers that run Helm hooks. That hook's Job has
  no toleration setting, so uninstall is qualified on a fully tainted cluster.

**Scheduling and resources.**

- NKTP's central workloads, `otel-lgtm`, and the runtime Prometheus server and
  its operator are system-class.
- The node collector, `node-exporter`, and the `kube-state-metrics`
  DaemonSet reach every node.
- Collectors keep NKTP's priority class with `preemptionPolicy: Never`, so
  adopting NKTP never evicts a running workload.
- Each stock platform's system pool must fit NKTP's central workloads, and
  qualification shows that it does.
- AICR does not override collector resources. NKTP tunes each collector's
  resource limits, memory limiter, and queues together, so an override would
  have to retune all three and be requalified on every NKTP upgrade.

**Security and data.**

- **Privilege.** The OTel Operator holds cluster-wide RBAC over workloads and
  its CRDs. Target Allocators and collectors read monitors, pods, nodes, and
  events cluster-wide. The node collector is privileged and mounts host log
  and metric paths. `otel-lgtm` runs as root. NKTP's other workloads run
  non-root, read-only, with capabilities dropped. `kube-state-metrics` Secret
  metrics stay off.
- **Data.** Logs are collected from every container in every namespace, from
  the node journal, from host auth and audit logs, and from the API server
  audit log where present. NKTP's default pipelines mask recognized
  credentials in log bodies and attributes before export. Unknown credential
  names and encoded secrets pass through, and the source files keep the
  originals.
- **Trust model.** Access control belongs to the backend. With `otel-lgtm`,
  in-cluster access means access to every collected log (Decision 2).
  Excluding namespaces or sources means editing NKTP's collector
  configuration.

**Health and validation.**

- NKTP's health check is read-only and fails closed. It requires:
  - the `OpenTelemetryCollector` CRD to be `Established`;
  - the operator, all seven collector workloads, all three Target
    Allocators, and the bundled exporters to be rolled out;
  - no release pod to be failed, pending, or crash-looping.
- `kube-prometheus-stack`'s health check covers only the operator and the
  runtime Prometheus server.
- The adapter's health check requires its metrics APIs to be `Available`.
- When `otel-lgtm` is disabled, the bundle fails if any of NKTP's five
  exporters (three gateways, the watchdog, and the watchdog's sidecar) still
  targets it.

**Migration and rollback.**

On an existing cluster the change is an in-place upgrade of
`kube-prometheus-stack` plus a new `nktp` release.

- **What the upgrade removes.** It removes Grafana, Alertmanager,
  `node-exporter`, `kube-state-metrics`, and every rule and default monitor.
  Dashboards and rules authored on `kube-prometheus-stack` go with it unless
  exported first.
- **Interruption.** The operator recreates the Prometheus server on `emptyDir`,
  so the adapter's metrics are unavailable for about half a minute, and the
  runtime Prometheus server starts empty.
- **Left behind.** Three objects remain: the Prometheus server's old volume, the
  admission webhook Secret, and the operator's kubelet Service in
  `kube-system`.
- **Where the steps ship.** They ship as an ADR-021 `manual` record on
  `kube-prometheus-stack`, with steps for all seven deployers, and the
  upgrade guide (`docs/user/upgrading.md`) carries them.
- **Rollback.** Rollback restores the previous release's values. NKTP must be
  removed first, because the restored stack's `node-exporter` binds the same
  host port as NKTP's.

## Consequences

**Positive.**

- Every stock recipe collects logs, events, and OTLP traces as well as
  metrics, through one pinned, mirrored, health-checked pipeline.
- Host metrics reach GPU nodes.
- Collection scales per node by construction, and through the autoscalers on
  every platform with a resource metrics API.
- The runtime consumers are unchanged.

**Negative.**

- The observability backend has the limits in Decision 2 on every platform:
  - no persistence, including on the five platforms with a volume today;
  - no alert evaluation.
- `--storage-class` no longer reaches telemetry. It still applies to
  `nvsentinel-mongodb` and `slurm-accounting-mariadb`.
- Every stock recipe collects every container's logs, the node journal, and
  host auth and audit logs, with no namespace filter.
- NKTP requests several times the resources of today's stack, including a
  share on every GPU node.
- Upgrading interrupts the adapter for about half a minute and leaves three
  objects to clean up.

**Neutral.**

- AICR consumes NKTP releases as it consumes `kube-prometheus-stack`'s.
- Production backends are the owner's choice.
- `kube-prometheus-stack` keeps its name but is no longer a monitoring stack.

## Alternatives Considered

**Replace `kube-prometheus-stack` entirely, with consumers reading
`otel-lgtm`.** Rejected: it ties HPA and conformance to an evaluation backend
and to NKTP's pipeline, and owners must re-point every consumer to reach
production.

**NKTP feeds a new Prometheus server with only the declared series.** Rejected:

- A Prometheus server outage adds backpressure on NKTP's main pipeline.
- An unauthenticated write endpoint lets any pod feed HPAs false metrics.
- It adds a component and changes every consumer's endpoint.

**Keep the full stack, with NKTP for logs and traces only.** Rejected: NKTP
has no per-signal switch. The two `node-exporter`s collide on a host port,
and each stack scrapes the other's targets unless AICR overrides NKTP's
collection configuration.

**A standalone prometheus-operator and Prometheus server.** Rejected: there is
no maintained standalone operator chart, so AICR would maintain manifests or
adopt a third-party chart, and every consumer would be re-pointed. It reaches
the same result as the reduced stack with more AICR-owned surface.

**Harden `otel-lgtm` into a supported backend.** Rejected: upstream positions
the image for development, demos, and testing, and says a persistent volume
does not change that.

**NKTP only, with no in-cluster Prometheus server.** Rejected for now:

- No OpenTelemetry component serves the custom or external metrics APIs.
- The KEDA route for OTLP is a 0.x add-on from one vendor that serves
  external metrics only.
- An NVIDIA-owned adapter would be a new project.
- Any route through NKTP's node collector couples the HPA path to the
  pipeline.

**Do not integrate NKTP.** Rejected: logs, GPU-node host metrics, and
scale stay unaddressed.

## Acceptance Criteria

**Release and supply chain.**

- Every image the chart deploys, all of them third-party, can be pinned by
  digest through the chart's values, per ADR-006.
- The chart host is on the registry allowlist, and `aicr mirror list` finds
  every image.
- The chart is signed, with provenance, by NKTP's release workflow.

**Rendering and lifecycle.**

- Bundles render deterministically on all seven deployers, with BOM and
  mirror coverage.
- The `opentelemetry.io` CRDs upgrade across a chart bump on every deployer
  with a CRD step.
- Dependency order is exercised on the KWOK deployer matrix and on UAT.
- Uninstall and rollback are exercised on every deployer, including on a
  fully tainted cluster.
- Upgrading from an earlier NKTP release to the candidate keeps telemetry
  flowing.

**Runtime metrics.**

- Every stock recipe that enables `kube-prometheus-stack` injects exactly the
  declared namespaces and series, identically through the CLI, the API server,
  and the SDK. The others inject nothing.
- The runtime Prometheus server stores only declared series.
- The adapter serves both metrics APIs, and `pod-autoscaling` and
  `cluster-autoscaling` pass.
- The Dynamo planner's SLA queries return data on an inference-dynamo
  recipe.

**Operational safety.**

- With the system node selector set, every central workload schedules on
  each UAT platform's system pool.
- The node agents run on every node, GPU nodes included.
- NKTP's autoscalers report utilization wherever a resource metrics API
  exists.
- Resource use is measured, and NKTP's published scale benchmark covers the
  largest stock target.
- Exporters use bounded queues, and their drops are visible on the watchdog
  path.
- The release sends cumulative counters.

**Security.**

- RBAC is documented per principal.
- Data handling and the evaluation posture of `otel-lgtm` are documented.

**Migration.**

- On each UAT platform, upgrading from the previous AICR release by the upgrade
  guide slims `kube-prometheus-stack`, cleans up the three leftover objects, and
  installs NKTP. Both the `helm` path and a GitOps prune path are exercised.
- NKTP discovers every existing monitor, and the adapter serves GPU metrics
  afterward.
- The upgrade check reports the `manual` record.

## References

- [NKTP Helm chart](https://github.com/dsx-ai-factory/nvke/tree/main/nktp/deploy/charts/nktp):
  the NVKE Telemetry Pipeline. The qualified release is recorded on the
  implementation pull request.
- [OpenTelemetry Operator](https://github.com/open-telemetry/opentelemetry-operator)
- [grafana/otel-lgtm](https://github.com/grafana/docker-otel-lgtm)
- [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)
- [prometheus-operator-crds](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus-operator-crds)
- [prometheus-adapter](https://github.com/kubernetes-sigs/prometheus-adapter)
- [Component contributor guide](../contributor/component.md)
- [ADR-006: Container Image Pinning Policy](006-image-pinning-policy.md)
- [ADR-007: Recipe Evidence](007-recipe-evidence.md)
- [ADR-018: Class-Partitioned Bundles](018-class-partitioned-bundles.md),
  as amended by #3341
- [ADR-019: k8s-aibom Runtime AI Inventory Component](019-k8s-aibom-runtime-inventory.md)
- [ADR-021: Component Upgrade Safety](021-component-upgrade-safety.md)
- [ADR-025: NVIDIA Cluster Readiness Engine Component](025-nvcre-cluster-certification.md)
- NVIDIA/aicr#3341 (class definitions) and #3350 (class field and
  dependency boundaries)
