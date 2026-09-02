# EKS Dynamo Networking Prerequisites

For `*-eks-ubuntu-inference-dynamo` recipes, AICR configures
`dynamo-platform` with Kubernetes-native discovery. As of the Dynamo 1.4+
bump, AICR no longer installs bundled NATS by default: the request plane
defaults to TCP and the KV event plane defaults to ZMQ
(`ai-dynamo/dynamo#11951`). This removes the old `4222` NATS requirement,
but it does **not** remove the underlying cross-nodegroup networking
requirement — the request plane and KV events are now **direct
frontend↔worker pod-to-pod connections** instead of both sides talking to a
`dynamo-platform-nats` StatefulSet on the system nodegroup, and Frontend
pods still run on the system nodegroup while workers run on the GPU
nodegroup, so traffic still crosses the same GPU↔system nodegroup SG
boundary as before.

**Confirmed on a live Dynamo 1.4.1 EKS deployment (`aicr-gb300`, 2026-09-01)
and against upstream runtime source (`lib/runtime/src/pipeline/network/manager.rs`,
`distributed.rs`).** The 1.4.2 chart pins the same `grove`/`kai-scheduler`
dependency versions as 1.4.1, so this is not expected to change in 1.4.2.
Traffic crossing the GPU↔system nodegroup boundary is **bidirectional**, by
connection initiator:

- **System/frontend → GPU/worker** — request-plane connection to the
  worker's `DYN_TCP_RPC_PORT`. OS-assigned by default.
- **GPU/worker → system/frontend** — response-stream connection to the
  frontend's `DYN_TCP_RESPONSE_STREAM_PORT`. OS-assigned by default.
- **System/router → GPU/worker** — ZMQ subscriber connects to the worker's
  bound port `5557` (offset by `+dp_rank` for dp_rank > 0). KV-event data
  then flows back over that established connection, but the SG rule
  follows the connecting side (system → GPU), not the data direction.

If the GPU and system node groups sit in different security groups, these
ports may be blocked from GPU nodes to the frontend's node (and vice versa).
Typical symptoms:
- Dynamo frontend and vLLM worker pods stuck in `CrashLoopBackOff`, or a
  frontend that starts cleanly but never successfully routes a request
  through to a worker
- Worker startup probes failing with `connection refused` because the
  process exits before serving
- The `inference-perf` performance validator failing while `deployment` and
  `conformance` pass; the workload never reaches a ready state. This is a
  blocked-SG-traffic scenario, so it most often surfaces at the
  workload-readiness gate (~10 min timeout) — the deployment never becomes
  ready without connectivity, so the separate ~5 min health-check gate is
  never reached. If readiness does pass (e.g. a subset of pods can reach
  each other) but health checks then fail, the wait is up to ~15 min total.

You can confirm reachability for the fixed ZMQ port directly from a
system-nodegroup node before re-running:

```shell
kubectl run tcp-probe --rm -i --restart=Never --image=busybox:1.36 \
  --overrides='{"spec":{"nodeSelector":{"<system-node-label-key>":"<value>"}}}' \
  -- sh -c 'nc -zv -w 5 <worker-pod-ip-or-svc> 5557'
```

This only validates the ZMQ port for dp_rank 0 (`5557`; add the rank's
offset for dp_rank > 0) and only the system→GPU direction. It does **not**
validate either dynamic TCP plane (`DYN_TCP_RPC_PORT`,
`DYN_TCP_RESPONSE_STREAM_PORT`) in either direction — those bind to an
OS-assigned port only once the pod is running, so there is no fixed port to
probe ahead of time. To check them, `kubectl exec` into a running frontend
or worker pod and inspect its actual listening sockets (e.g. `ss -tlnp` if
available in the image, or read `/proc/net/tcp`), then probe that specific
port from the other side.

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

Allow ingress from the **system node security group to the GPU node
security group** on:
- TCP `5557` through `5557 + (max dp_rank across your workers)` - ZMQ
  KV-cache event plane (fixed base port `5557`, offset per worker by
  `dp_rank`; a single-worker deployment only needs `5557`)
- TCP ephemeral range `1024-65535` - Dynamo request plane `DYN_TCP_RPC_PORT` (OS-assigned)

Allow ingress from the **GPU node security group to the system node
security group** on:
- TCP ephemeral range `1024-65535` - Dynamo response-stream `DYN_TCP_RESPONSE_STREAM_PORT` (OS-assigned)
- TCP `9090` - Prometheus (required for the `ai-service-metrics` conformance check)

The `9090` rule is required as a fallback guarantee: the orchestrator *prefers*
to co-locate with Prometheus, but that preference is best-effort, so it can
still land on any worker node. Every node group whose pods can host the
orchestrator must therefore be able to reach the Prometheus pod's IP on `9090`.
On clusters with separate customer/system ENI subnets (e.g. DGXC EKS), this
means the system SG must accept ingress from the customer SG (and any other
worker SG), not only from itself.

If the cluster has more than two worker security groups (e.g. a separate
inference node group), repeat the `9090` rule for each non-system SG that can
host pods — on a fallback placement the orchestrator may land on any of them.

Example:

```shell
# 1) Find SG IDs for system and GPU nodegroups
aws ec2 describe-instances \
  --filters "Name=tag:eks:nodegroup-name,Values=<system-nodegroup>" \
  --query "Reservations[0].Instances[0].SecurityGroups[*].GroupId" \
  --output text

aws ec2 describe-instances \
  --filters "Name=tag:eks:nodegroup-name,Values=<gpu-nodegroup>" \
  --query "Reservations[0].Instances[0].SecurityGroups[*].GroupId" \
  --output text

# 2a) ZMQ KV-events: system → GPU on 5557 (single worker) or a range
#     covering 5557 + max dp_rank for a multi-worker deployment
aws ec2 authorize-security-group-ingress --group-id <gpu-sg-id> \
  --protocol tcp --port 5557 --source-group <system-sg-id>
# For dp_rank > 0, e.g. up to 8 workers per node:
#   aws ec2 authorize-security-group-ingress --group-id <gpu-sg-id> \
#     --protocol tcp --port 5557-5564 --source-group <system-sg-id>

# 2b) Request plane: system → GPU (ephemeral range)
aws ec2 authorize-security-group-ingress --group-id <gpu-sg-id> \
  --protocol tcp --port 1024-65535 --source-group <system-sg-id>

# 2c) Response stream: GPU → system (ephemeral range)
aws ec2 authorize-security-group-ingress --group-id <system-sg-id> \
  --protocol tcp --port 1024-65535 --source-group <gpu-sg-id>

aws ec2 authorize-security-group-ingress --group-id <system-sg-id> \
  --protocol tcp --port 9090 --source-group <gpu-sg-id>
```
