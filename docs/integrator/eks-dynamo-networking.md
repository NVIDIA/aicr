# EKS Dynamo Networking Prerequisites

For `*-eks-ubuntu-inference-dynamo` recipes, `dynamo-platform` commonly runs:
- `etcd` on TCP `2379`
- `nats` (JetStream) on TCP `4222`

If system components and GPU workloads are on different node groups/security groups, these ports may be blocked from GPU nodes to system nodes. Typical symptoms:
- `Unable to create lease` (etcd unreachable)
- `JetStream not available` (NATS unreachable)

The conformance validator's `ai-service-metrics` check adds a third requirement:
it dials Prometheus over the cluster Service (typically
`kube-prometheus-prometheus.monitoring.svc:9090`). The orchestrator Job that
runs the check tolerates every taint and has no node-affinity toward
Prometheus, so the kube-scheduler may place it on any worker node — including
one whose ENI is in a security group that cannot reach the Prometheus pod.

When that happens, the dial times out at 5 s and the check is marked `failed`:

```
[SERVICE_UNAVAILABLE] Prometheus unreachable at http://kube-prometheus-prometheus.monitoring.svc:9090 — verify network connectivity
```

The outcome is **non-deterministic from run to run** on the same cluster: the
first scheduling decision is a tie-break, and subsequent runs are dominated by
image-locality scoring (whichever node pulled the validator image first keeps
winning). A re-run on a "freshly working" cluster is therefore not a reliable
signal that the SG topology is correct.

This is tracked in [issue #933](https://github.com/NVIDIA/aicr/issues/933);
this page documents the cluster-side prerequisite until the validator gains
`podAffinity` toward Prometheus.

## Required Security Group Rules

Allow ingress from the GPU node security group to the system node security group on:
- TCP `2379` — etcd (dynamo-platform)
- TCP `4222` — NATS / JetStream (dynamo-platform)
- TCP `9090` — Prometheus (required for the `ai-service-metrics` conformance check)

The `9090` rule is symmetrically required: the validator orchestrator pod may
land on **any** worker node, so every node group whose pods can host the
orchestrator must be able to reach the Prometheus pod's IP on `9090`. On
clusters with separate customer/system ENI subnets (e.g. DGXC EKS), this means
the system SG must accept ingress from the customer SG (and any other worker
SG), not only from itself.

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

# 2) Allow etcd + NATS + Prometheus from GPU SG -> system SG
aws ec2 authorize-security-group-ingress --group-id <system-sg-id> \
  --protocol tcp --port 2379 --source-group <gpu-sg-id>

aws ec2 authorize-security-group-ingress --group-id <system-sg-id> \
  --protocol tcp --port 4222 --source-group <gpu-sg-id>

aws ec2 authorize-security-group-ingress --group-id <system-sg-id> \
  --protocol tcp --port 9090 --source-group <gpu-sg-id>
```

If the cluster has more than two worker security groups, repeat the `9090`
rule for each non-system SG that can host pods — the validator orchestrator
has no scheduling preference and may land on any of them.
