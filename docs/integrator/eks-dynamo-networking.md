# EKS Dynamo Networking Prerequisites

For `*-eks-ubuntu-inference-dynamo` recipes, AICR configures
`dynamo-platform` with Kubernetes-native discovery and the standard NATS
event plane for KV-cache and runtime events:
- `nats` on TCP `4222`

This NATS dependency is new as of the Dynamo 1.2 bump, which switched discovery
to the NATS event plane. A cluster whose system-node security group only
allowlisted the pre-1.2 control-plane ports will not have `4222` open, so a
bundle that worked on Dynamo 1.0.x can start failing purely from the version
bump — add the `4222` rule below.

Frontend-to-worker inference request/response traffic is separate: Dynamo 1.2
defaults `DYN_REQUEST_PLANE` to TCP, and AICR does not override it to NATS. The
worker runtime relays local vLLM ZMQ KV-cache events onto the NATS-backed event
plane so the KV router or EPP can consume live cache state.

If system components and GPU workloads are on different node groups/security groups, these ports may be blocked from GPU nodes to system nodes. Typical symptoms:
- `JetStream not available` (NATS unreachable)
- Dynamo frontend and vLLM worker pods stuck in `CrashLoopBackOff`, with
  `Exception: Failed to connect to NATS: timed out` in the frontend log
- Worker startup probes failing with `connection refused` because the process
  exits before serving
- The `inference-perf` performance validator failing after its workload-readiness
  (10 min) and health (5 min) gates lapse — roughly 15 min — while `deployment`
  and `conformance` pass; the workload never reaches a ready state

You can confirm reachability directly from a GPU node before re-running. The
toleration is required because the GPU node groups on these clusters are
tainted (`NoSchedule`/`NoExecute`); without it the probe pod stays `Pending`
and never runs:

```shell
kubectl run nats-probe --rm -i --restart=Never --image=busybox:1.36 \
  --overrides='{"spec":{"nodeSelector":{"<gpu-node-label-key>":"<value>"},"tolerations":[{"operator":"Exists"}]}}' \
  -- sh -c 'nc -zv -w 5 dynamo-platform-nats.dynamo-system.svc.cluster.local 4222'
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

Allow ingress from the GPU node security group to the system node security group on:
- TCP `4222` - NATS event plane (dynamo-platform)
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

# 2) Allow NATS + Prometheus from GPU SG -> system SG
aws ec2 authorize-security-group-ingress --group-id <system-sg-id> \
  --protocol tcp --port 4222 --source-group <gpu-sg-id>

aws ec2 authorize-security-group-ingress --group-id <system-sg-id> \
  --protocol tcp --port 9090 --source-group <gpu-sg-id>
```
