---
title: "EKS Dynamo Networking"
linkTitle: "EKS Dynamo Networking"
weight: 34
description: "Security group prerequisites for Dynamo overlays on EKS"
---

For `*-eks-ubuntu-inference-dynamo` recipes, `dynamo-platform` commonly runs:
- `etcd` on TCP `2379`
- `nats` (JetStream) on TCP `4222`

If system components and GPU workloads are on different node groups/security groups, these ports may be blocked from GPU nodes to system nodes. Typical symptoms:
- `Unable to create lease` (etcd unreachable)
- `JetStream not available` (NATS unreachable)

## Required Security Group Rules

Allow ingress from the GPU node security group to the system node security group on:
- TCP `2379`
- TCP `4222`

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

# 2) Allow etcd + NATS from GPU SG -> system SG
aws ec2 authorize-security-group-ingress --group-id <system-sg-id> \
  --protocol tcp --port 2379 --source-group <gpu-sg-id>

aws ec2 authorize-security-group-ingress --group-id <system-sg-id> \
  --protocol tcp --port 4222 --source-group <gpu-sg-id>
```

## GB200 Skyhook Note

GB200 EKS overlays use the Skyhook `no-op` customization manifest instead of the H100 `tuning.yaml`. The H100 tuning packages (`nvidia-setup`, `nvidia-tuned`) are not compatible with GB200 for two reasons:

1. **ARM64 host CPU**: GB200 nodes use Graviton (ARM64) host processors. The H100 tuning packages include x86-specific operations (e.g., EFA driver installation, apt package upgrades) that fail on ARM64.
2. **Blackwell GPU architecture**: GPU-specific tuning parameters (kernel module settings, sysctl values) differ between Hopper (H100) and Blackwell (GB200).

When GB200-specific tuning packages are available, switch `skyhook-customizations` in the GB200 overlays from `no-op.yaml` to `tuning.yaml`.
