# EKS Dynamo Networking Prerequisites

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
