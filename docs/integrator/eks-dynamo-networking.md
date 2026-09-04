# EKS Dynamo Networking Prerequisites

For `*-eks-ubuntu-inference-dynamo` recipes, AICR configures
`dynamo-platform` with Kubernetes-native discovery. As of the Dynamo 1.4+
bump, AICR no longer installs bundled NATS by default: the request plane
defaults to TCP and the KV event plane defaults to ZMQ
(`ai-dynamo/dynamo#11951`). This removes the old `4222` NATS requirement,
but it does **not** remove the underlying networking requirement — the
request plane and KV events are now **direct frontend↔worker pod-to-pod
connections** instead of both sides talking to a `dynamo-platform-nats`
StatefulSet, and if your deployment places the Frontend and Worker
components on nodes in different security groups, that traffic can be
silently blocked.

**This only matters if your deployment actually splits Frontend and Worker
across security groups.** AICR's own supported paths often don't:
- The `inference-perf` performance validator (the canonical AICR-supported
  path) deliberately co-locates every Dynamo component — Frontend, EPP,
  Worker — on the same GPU node cohort specifically to avoid this class of
  bug (see the comment on `applyInferenceWorkerScheduling` in
  `validators/performance/inference_perf_constraint.go`).
- The demo workload (`demos/workloads/inference/vllm-agg.yaml`) puts the
  Frontend on a `cpu-worker` node group, separate from the Worker's
  `gpu-worker` group — that one **does** need the rules below.
- The UAT inference lane (`tests/uat/lib/phases.sh`) co-locates Frontend and
  Worker on the same `gpu-worker` group — no cross-nodegroup rules needed.

If your deployment co-locates every Dynamo component on one node group (one
security group), you can skip the Frontend↔Worker rules below — there is no
cross-SG traffic between them to allow. The separate Prometheus/`ai-service-metrics`
requirement further down still applies regardless of Frontend/Worker
co-location; see that section. The Frontend↔Worker rules below apply only
when Frontend and Worker sit in different security groups, described by
role rather than by fixed node-group name:

**Confirmed on a live Dynamo 1.4.1 EKS deployment (`aicr-gb300`, 2026-09-01)
and against upstream runtime source (`lib/runtime/src/pipeline/network/manager.rs`,
`distributed.rs`).** The 1.4.2 chart pins the same `grove`/`kai-scheduler`
dependency versions as 1.4.1, so this is not expected to change in 1.4.2.
Traffic between the Frontend/router role and the Worker role is
**bidirectional**, by connection initiator:

- **Frontend/router → Worker** — request-plane connection to the worker's
  `DYN_TCP_RPC_PORT`. OS-assigned by default.
- **Worker → Frontend/router** — response-stream connection to the
  frontend's `DYN_TCP_RESPONSE_STREAM_PORT`. OS-assigned by default.
- **Frontend/router → Worker** — ZMQ subscriber connects to the worker's
  bound port `5557`. KV-event data then flows back over that established
  connection, but the SG rule follows the connecting side (frontend/router
  → worker), not the data direction.

If the Frontend/router and Worker roles sit in different security groups,
these ports may be blocked in one or both directions. **The two blocked
transports fail very differently — don't conflate them:**

- **Blocked TCP request/response plane (`DYN_TCP_RPC_PORT`,
  `DYN_TCP_RESPONSE_STREAM_PORT`) breaks inference outright.** A dispatched
  request that can't reach the worker (or a response that can't get back to
  the frontend) surfaces as a **request error**, not a pod crash — Dynamo's
  Rust TCP client returns connection failures as `Result` errors at the
  call site, it does not panic the process. Neither the Frontend nor
  Worker's own readiness probe exercises cross-pod networking at all
  (confirmed in upstream's `component_worker.go`: *"ReadinessProbe in
  Dynamo worker context doesn't determine that the worker is ready to
  receive traffic"*), so a pure TCP-plane block does not by itself cause
  `CrashLoopBackOff` or fail the DGD's own readiness gate — those pass
  normally, then the `inference-perf` performance validator's separate
  `/v1/chat/completions` health probe (up to ~5 min after normal DGD
  startup, not a fixed extra 15 min) is what actually fails, since that's
  the first real end-to-end request exercised.
- **Blocked ZMQ (`5557`) does *not* break inference by itself** — Dynamo's
  ZMQ event plane is deliberately best-effort/lossy (confirmed in upstream:
  *"the event plane is already best-effort/lossy ... so a dropped event
  costs routing-estimate freshness, not correctness"*), and the default
  `dynamo-router` mode (`least-loaded`) doesn't consume KV-cache events at
  all — only `DYN_ROUTER_MODE=kv` does. Blocking `5557` alone degrades
  KV-aware routing quality (or is a complete no-op under `least-loaded`);
  it will not fail the chat-completion health probe on its own. Verify it
  with a dedicated reachability check, not by watching inference requests
  fail.

You can confirm reachability for the fixed ZMQ port directly from a node in
the Frontend/router's security group before re-running. The probe pod needs
a toleration matching whatever taint that node group carries (e.g.
`dedicated=system-workload` on AICR's own `--system-node-selector`
convention — check your actual node group's taints and adjust), and must
target the **worker Pod IP directly, not a Service**: the operator-generated
worker Service only forwards the health port (`9090`), not `5557`.

```shell
# <workload-namespace> is wherever your DynamoGraphDeployment actually runs
# (e.g. dynamo-workload for the demo; aicr-inference-perf-<run-id> for the
# inference-perf validator) -- check `kubectl get dynamographdeployments -A`
# if unsure.
kubectl get pod -n <workload-namespace> -l nvidia.com/dynamo-component-type=worker -o wide  # find the worker Pod IP
kubectl run tcp-probe --rm -i --restart=Never --image=busybox:1.36 \
  --overrides='{"spec":{"nodeSelector":{"<frontend-node-label-key>":"<value>"},"tolerations":[{"operator":"Exists"}]}}' \
  -- sh -c 'nc -zv -w 5 <worker-pod-ip> 5557'
```

The bare `"tolerations":[{"operator":"Exists"}]` above tolerates every taint
regardless of key, value, or effect — simplest when you don't know exactly
which taints (and which effects, `NoSchedule` vs `NoExecute`) the target
node group carries. Narrow it to the specific key/value/effect if you want
the probe to land only on a specific node group.

This only validates the ZMQ port and only the frontend/router→worker
direction. It does **not** validate either dynamic TCP plane
(`DYN_TCP_RPC_PORT`, `DYN_TCP_RESPONSE_STREAM_PORT`) in either direction —
those bind to an OS-assigned port only once the pod is running, and the
frontend's response-stream listener specifically binds **lazily**, only
once a request/response has actually flowed. To check them: `kubectl exec`
into a running frontend or worker pod and inspect its actual listening
sockets (e.g. `ss -tlnp` if available in the image, or read
`/proc/net/tcp`) **after** sending at least one real inference request
through the deployment — checking immediately after the Pod reaches
`Running` will show nothing yet and does not by itself indicate a
networking failure. Then probe that specific port from the other side.

The conformance validator's `ai-service-metrics` check adds a third requirement:
it dials Prometheus over the cluster Service (typically
`kube-prometheus-prometheus.monitoring.svc:9090`). The orchestrator Job that
runs the check tolerates every taint and now sets a *preferred*
`dependencyAffinity` toward Prometheus, so the scheduler co-locates it with the
Prometheus pod when possible. The preference is best-effort, not required, so it
can still fall back to any worker node (e.g. if the Prometheus node is
unschedulable) — including one whose ENI is in a security group that cannot
reach the Prometheus pod.

When that happens, the dial times out at 5 s and the check is marked `failed`:

```text
[SERVICE_UNAVAILABLE] Prometheus unreachable at http://kube-prometheus-prometheus.monitoring.svc:9090 — verify network connectivity
```

On a fallback placement the outcome can be **non-deterministic from run to
run**: scheduling tie-breaks and image-locality scoring decide which node wins,
so a re-run on a "freshly working" cluster is not a reliable signal that the SG
topology is correct.

The preferred `dependencyAffinity` ([issue #933](https://github.com/NVIDIA/aicr/issues/933),
resolved) makes this far less likely, but because it is best-effort the `9090`
SG rule below remains the reliable cluster-side guarantee.

## Required Security Group Rules

### Frontend↔Worker rules

Only applies if your deployment splits the Frontend/router and Worker roles
across security groups (see above — AICR's own validator and UAT paths
co-locate them and need none of this).

Allow ingress from the **Frontend/router security group to the Worker
security group** on:
- TCP `5557` - ZMQ KV-cache event plane (fixed port). Standard AICR worker
  replicas each run with the default `--data-parallel-size` of 1, so every
  replica listens on plain `5557` — the offset only applies if you
  explicitly configure a larger `--data-parallel-size` for a single
  deployment (DP ranks within one logical worker group, not one port per
  replica). If you do, widen this to `5557` through
  `5557 + (data-parallel-size - 1)`.
- TCP ephemeral range `1024-65535` - Dynamo request plane `DYN_TCP_RPC_PORT` (OS-assigned)

Allow ingress from the **Worker security group to the Frontend/router
security group** on:
- TCP ephemeral range `1024-65535` - Dynamo response-stream `DYN_TCP_RESPONSE_STREAM_PORT` (OS-assigned)

### Prometheus rule — always required, independent of Frontend/Worker placement

`kube-prometheus-stack` is scheduled via AICR's `--system-node-selector`
regardless of where Frontend and Worker land (`recipes/registry.yaml`'s
`kube-prometheus-stack` entry pins it to the `system` node scheduling
group), so this rule is needed **even when Frontend and Worker are
co-located** — it is not part of the Frontend↔Worker relationship above.

Allow ingress on TCP `9090` (Prometheus, required for the `ai-service-metrics`
conformance check) from **every security group that can host the
conformance orchestrator Job** into the **Prometheus/system security
group**. The orchestrator Job tolerates every taint (can schedule on any
node group) and only *prefers* — via best-effort `dependencyAffinity` — to
co-locate with Prometheus; on a fallback placement it can land on any
worker node. Every node group whose pods can host the orchestrator must
therefore be able to reach the Prometheus pod's IP on `9090`, including
Worker's security group even if Frontend and Worker are co-located and
otherwise need none of the rules above. On clusters with separate
customer/system ENI subnets (e.g. DGXC EKS), this means the Prometheus-side
SG must accept ingress from every other worker SG, not only from itself.

If the cluster has more than two worker security groups (e.g. a separate
inference node group), repeat the `9090` rule for each SG that can host the
orchestrator — on a fallback placement it may land on any of them.

### Example

Using AICR's own `--system-node-selector`/`--accelerated-node-selector`
convention (Frontend on the system node group, Worker on the GPU node
group) as a concrete instance of the frontend/router vs. worker roles
above. `kube-prometheus-stack` is a third, separate group in the general
case — it lands on whatever `--system-node-selector` targets, which is
often but not always the same group as your Frontend (e.g. the demo's
Frontend runs on `cpu-worker`, not AICR's system group, so Prometheus
there is a distinct SG). Don't assume `<frontend-sg-id>` and
`<prometheus-sg-id>` are the same without checking:

```shell
# 1) Find SG IDs for the Frontend/router, Worker, and Prometheus nodegroups
aws ec2 describe-instances \
  --filters "Name=tag:eks:nodegroup-name,Values=<frontend-nodegroup>" \
  --query "Reservations[0].Instances[0].SecurityGroups[*].GroupId" \
  --output text

aws ec2 describe-instances \
  --filters "Name=tag:eks:nodegroup-name,Values=<worker-nodegroup>" \
  --query "Reservations[0].Instances[0].SecurityGroups[*].GroupId" \
  --output text

aws ec2 describe-instances \
  --filters "Name=tag:eks:nodegroup-name,Values=<system-nodegroup>" \
  --query "Reservations[0].Instances[0].SecurityGroups[*].GroupId" \
  --output text

# 2a) ZMQ KV-events: frontend/router → worker on 5557
aws ec2 authorize-security-group-ingress --group-id <worker-sg-id> \
  --protocol tcp --port 5557 --source-group <frontend-sg-id>

# 2b) Request plane: frontend/router → worker (ephemeral range)
aws ec2 authorize-security-group-ingress --group-id <worker-sg-id> \
  --protocol tcp --port 1024-65535 --source-group <frontend-sg-id>

# 2c) Response stream: worker → frontend/router (ephemeral range)
aws ec2 authorize-security-group-ingress --group-id <frontend-sg-id> \
  --protocol tcp --port 1024-65535 --source-group <worker-sg-id>

# 3) Prometheus: always required, from every SG that can host the
#    conformance orchestrator -- worker-sg and frontend-sg here, but
#    repeat for any other worker security group in the cluster (this
#    rule applies even if Frontend and Worker are co-located and skip
#    rules 2a-2c above). <prometheus-sg-id> is NOT assumed to equal
#    <frontend-sg-id> -- use the SG you found for <system-nodegroup> above.
aws ec2 authorize-security-group-ingress --group-id <prometheus-sg-id> \
  --protocol tcp --port 9090 --source-group <worker-sg-id>

aws ec2 authorize-security-group-ingress --group-id <prometheus-sg-id> \
  --protocol tcp --port 9090 --source-group <frontend-sg-id>
```
