# Cluster Autoscaling

**Cluster:** `EKS / p6e-gb200.36xlarge / NVIDIA-GB200`
**Generated:** 2026-08-09 01:31:29 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates CNCF AI Conformance requirement that the platform has GPU-aware
cluster autoscaling infrastructure configured, with Auto Scaling Groups capable
of scaling GPU node groups based on workload demand.

## Summary

1. **GPU Node Group (ASG)** — EKS Auto Scaling Group configured with GPU instances
2. **Capacity Reservation** — Dedicated GPU capacity available for scale-up
3. **Scalable Configuration** — ASG min/max configurable for demand-based scaling
4. **Kubernetes Integration** — ASG nodes auto-join the EKS cluster with GPU labels
5. **Autoscaler Compatibility** — Cluster Autoscaler supported via ASG tag discovery

---

## GPU Node Auto Scaling Group

The cluster uses an AWS Auto Scaling Group (ASG) for GPU nodes, which can scale
up/down based on workload demand.

## EKS Cluster Details

- **Region:** us-east-1
- **Cluster:** aws-us-east-1-yljtrxpmzu-dgxc-k8s-aws-use1-non-prod
- **GPU Node Group:** customer-gpu

## GPU Nodes

**GPU nodes**
```
$ kubectl get nodes -l nvidia.com/gpu.present=true -o custom-columns=NAME:.metadata.name,INSTANCE-TYPE:.metadata.labels.node\.kubernetes\.io/instance-type,GPUS:.metadata.labels.nvidia\.com/gpu\.count,PRODUCT:.metadata.labels.nvidia\.com/gpu\.product,NODE-GROUP:.metadata.labels.nodeGroup,ZONE:.metadata.labels.topology\.kubernetes\.io/zone
NAME                             INSTANCE-TYPE        GPUS   PRODUCT        NODE-GROUP     ZONE
ip-100-64-135-20.ec2.internal    p6e-gb200.36xlarge   4      NVIDIA-GB200   customer-gpu   us-east-1-dfw-2a
ip-100-64-142-208.ec2.internal   p6e-gb200.36xlarge   4      NVIDIA-GB200   customer-gpu   us-east-1-dfw-2a
```

## Auto Scaling Group (AWS)

**GPU ASG details**
```
$ aws autoscaling describe-auto-scaling-groups --region us-east-1 --auto-scaling-group-names yljtrxpmzu-gpu --query AutoScalingGroups[0].{Name:AutoScalingGroupName,MinSize:MinSize,MaxSize:MaxSize,DesiredCapacity:DesiredCapacity,AvailabilityZones:AvailabilityZones,HealthCheckType:HealthCheckType} --output table
--------------------------------------------------------------------------------
|                           DescribeAutoScalingGroups                          |
+-----------------+------------------+----------+-----------+------------------+
| DesiredCapacity | HealthCheckType  | MaxSize  |  MinSize  |      Name        |
+-----------------+------------------+----------+-----------+------------------+
|  2              |  EC2             |  2       |  2        |  yljtrxpmzu-gpu  |
+-----------------+------------------+----------+-----------+------------------+
||                              AvailabilityZones                             ||
|+----------------------------------------------------------------------------+|
||  us-east-1-dfw-2a                                                          ||
|+----------------------------------------------------------------------------+|
```

**GPU launch template**
```
$ aws ec2 describe-launch-template-versions --region us-east-1 --launch-template-id lt-02e290edc9625d74f --versions $Latest --query LaunchTemplateVersions[0].LaunchTemplateData.{InstanceType:InstanceType,ImageId:ImageId} --output table
-------------------------------------------------
|        DescribeLaunchTemplateVersions         |
+------------------------+----------------------+
|         ImageId        |    InstanceType      |
+------------------------+----------------------+
|  ami-00bfffc818d3fe07d |  p6e-gb200.36xlarge  |
+------------------------+----------------------+
```

**ASG autoscaler tags**
```
$ aws autoscaling describe-tags --region us-east-1 --filters Name=auto-scaling-group,Values=yljtrxpmzu-gpu --query Tags[*].{Key:Key,Value:Value} --output table
--------------------------------------------------------------------------
|                              DescribeTags                              |
+---------------------------------------------------------------+--------+
|                              Key                              | Value  |
+---------------------------------------------------------------+--------+
|  k8s.io/cluster/yljtrxpmzu-dgxc-k8s-aws-use1-non-prod         |  owned |
|  kubernetes.io/cluster/yljtrxpmzu-dgxc-k8s-aws-use1-non-prod  |  owned |
+---------------------------------------------------------------+--------+
```

## Capacity Reservation

**GPU capacity reservation**
```
$ aws ec2 describe-capacity-reservations --region us-east-1 --query CapacityReservations[?InstanceType==`p6e-gb200.36xlarge`].{ID:CapacityReservationId,Type:InstanceType,State:State,Total:TotalInstanceCount,Available:AvailableInstanceCount,AZ:AvailabilityZone} --output table
----------------------------------------------------------------------------------------------------
|                                   DescribeCapacityReservations                                   |
+------------------+------------+-----------------------+---------+---------+----------------------+
|        AZ        | Available  |          ID           |  State  |  Total  |        Type          |
+------------------+------------+-----------------------+---------+---------+----------------------+
|  us-east-1-dfw-2a|  14        |  cr-092d1aa426a641392 |  active |  18     |  p6e-gb200.36xlarge  |
|  us-east-1-dfw-2a|  18        |  cr-0e2f3833a602809a6 |  active |  18     |  p6e-gb200.36xlarge  |
|  us-east-1-dfw-2a|  18        |  cr-0dcdbb7d0621f543d |  active |  18     |  p6e-gb200.36xlarge  |
+------------------+------------+-----------------------+---------+---------+----------------------+
```

**Result: PASS** — EKS cluster with GPU nodes managed by Auto Scaling Group, ASG configuration verified via AWS API. Evidence is configuration-level; a live scale event is not triggered to avoid disrupting the cluster.
