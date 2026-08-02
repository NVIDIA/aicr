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

> **TODO before merging (tracked in NVIDIA/aicr#1836):** the port(s) below
> are not yet confirmed against a real Dynamo 1.4+ EKS deployment. What's
> known from the AICR recipes: the ZMQ KV-event endpoint is set explicitly
> per worker via `--kv-events-config`, e.g.
> `{"enable_kv_cache_events":true,"publisher":"zmq","endpoint":"tcp://*:5557"}`
> (see `tests/manifests/dynamo-vllm-smoke-test.yaml`), offset by `+dp_rank`
> for dp_rank > 0. The TCP request plane does not have one fixed,
> documented port the way NATS had `4222` — confirm the actual listening
> port(s) on a live cluster before finalizing the SG rule below:
> ```shell
> kubectl exec -n dynamo-system <frontend-pod> -- ss -tlnp
> kubectl exec -n dynamo-system <worker-pod> -- ss -tlnp
> ```

If the GPU and system node groups sit in different security groups, these
ports may be blocked from GPU nodes to the frontend's node (and vice versa).
Typical symptoms:
- Dynamo frontend and vLLM worker pods stuck in `CrashLoopBackOff`, or a
  frontend that starts cleanly but never successfully routes a request
  through to a worker
- Worker startup probes failing with `connection refused` because the
  process exits before serving
- The `inference-perf` performance validator failing after its
  workload-readiness (10 min) and health (5 min) gates lapse — roughly
  15 min — while `deployment` and `conformance` pass; the workload never
  reaches a ready state

You can confirm reachability directly from a GPU node before re-running. The
toleration is required because the GPU node groups on these clusters are
tainted (`NoSchedule`/`NoExecute`); without it the probe pod stays `Pending`
and never runs:

```shell
kubectl run tcp-probe --rm -i --restart=Never --image=busybox:1.36 \
  --overrides='{"spec":{"nodeSelector":{"<gpu-node-label-key>":"<value>"},"tolerations":[{"operator":"Exists"}]}}' \
  -- sh -c 'nc -zv -w 5 <worker-pod-ip-or-svc> <PORT>'
```

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

Allow ingress from the GPU node security group to the system node security
group on:
- TCP `<PORT>` - Dynamo request plane + KV events (dynamo-platform) — confirm exact port(s) on-cluster, see TODO above
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

# 2) Allow Dynamo request/event-plane + Prometheus from GPU SG -> system SG
aws ec2 authorize-security-group-ingress --group-id <system-sg-id> \
  --protocol tcp --port <PORT> --source-group <gpu-sg-id>

aws ec2 authorize-security-group-ingress --group-id <system-sg-id> \
  --protocol tcp --port 9090 --source-group <gpu-sg-id>
```
