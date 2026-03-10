# Cluster Autoscaling

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-03-10 03:44:07 UTC
**Kubernetes Version:** v1.35
**Platform:** linux/amd64

---

Demonstrates CNCF AI Conformance requirement that the platform has GPU-aware
cluster autoscaling infrastructure configured, with Auto Scaling Groups capable
of scaling GPU node groups based on workload demand.

## Summary

1. **GPU Node Group (ASG)** — EKS Auto Scaling Group configured with GPU instances (p5.48xlarge)
2. **Capacity Reservation** — Dedicated GPU capacity available for scale-up
3. **Scalable Configuration** — ASG min/max configurable for demand-based scaling
4. **Kubernetes Integration** — ASG nodes auto-join the EKS cluster with GPU labels
5. **Autoscaler Compatibility** — Cluster Autoscaler and Karpenter supported via ASG tag discovery
6. **Result: PASS**

---

## GPU Node Auto Scaling Group

The cluster uses an AWS Auto Scaling Group (ASG) for GPU nodes, which can scale
up/down based on workload demand. The ASG is configured with p5.48xlarge instances
(8x NVIDIA H100 80GB HBM3 each) backed by a capacity reservation.

## EKS Cluster Details

- **Region:** us-east-1
- **Cluster:** aws-us-east-1-aicr-cuj2
- **GPU Node Group:** gpu-worker

## GPU Nodes

**GPU nodes**
```
$ kubectl get nodes -l nvidia.com/gpu.present=true -o custom-columns=NAME:.metadata.name,INSTANCE-TYPE:.metadata.labels.node\.kubernetes\.io/instance-type,GPUS:.metadata.labels.nvidia\.com/gpu\.count,PRODUCT:.metadata.labels.nvidia\.com/gpu\.product,NODE-GROUP:.metadata.labels.nodeGroup,ZONE:.metadata.labels.topology\.kubernetes\.io/zone
NAME                           INSTANCE-TYPE   GPUS   PRODUCT                 NODE-GROUP   ZONE
ip-10-0-171-111.ec2.internal   p5.48xlarge     8      NVIDIA-H100-80GB-HBM3   gpu-worker   us-east-1e
ip-10-0-206-2.ec2.internal     p5.48xlarge     8      NVIDIA-H100-80GB-HBM3   gpu-worker   us-east-1e
```

## Auto Scaling Group (AWS)

**GPU ASG details**
```
$ aws autoscaling describe-auto-scaling-groups --region us-east-1 --auto-scaling-group-names aicr-cuj2-gpu-worker --query AutoScalingGroups[0].{Name:AutoScalingGroupName,MinSize:MinSize,MaxSize:MaxSize,DesiredCapacity:DesiredCapacity,AvailabilityZones:AvailabilityZones,HealthCheckType:HealthCheckType} --output table
---------------------------------------------
|         DescribeAutoScalingGroups         |
+------------------+------------------------+
|  DesiredCapacity |  2                     |
|  HealthCheckType |  EC2                   |
|  MaxSize         |  2                     |
|  MinSize         |  2                     |
|  Name            |  aicr-cuj2-gpu-worker  |
+------------------+------------------------+
||            AvailabilityZones            ||
|+-----------------------------------------+|
||  us-east-1e                             ||
|+-----------------------------------------+|
```

**GPU launch template**
```
$ aws ec2 describe-launch-template-versions --region us-east-1 --launch-template-id lt-038186420dd139467 --versions $Latest --query LaunchTemplateVersions[0].LaunchTemplateData.{InstanceType:InstanceType,ImageId:ImageId} --output table
-------------------------------------------
|     DescribeLaunchTemplateVersions      |
+------------------------+----------------+
|         ImageId        | InstanceType   |
+------------------------+----------------+
|  ami-0d60865d127c3d404 |  p5.48xlarge   |
+------------------------+----------------+
```

**ASG autoscaler tags**
```
$ aws autoscaling describe-tags --region us-east-1 --filters Name=auto-scaling-group,Values=aicr-cuj2-gpu-worker --query Tags[*].{Key:Key,Value:Value} --output table
-----------------------------------------------------------------
|                         DescribeTags                          |
+--------------------------------------+------------------------+
|                  Key                 |         Value          |
+--------------------------------------+------------------------+
|  Name                                |  aicr-cuj2-gpu-worker  |
|  k8s.io/cluster-autoscaler/aicr-cuj2 |  owned                 |
|  k8s.io/cluster-autoscaler/enabled   |  true                  |
|  k8s.io/cluster/aicr-cuj2            |  owned                 |
|  kubernetes.io/cluster/aicr-cuj2     |  owned                 |
+--------------------------------------+------------------------+
```

## Capacity Reservation

**GPU capacity reservation**
```
$ aws ec2 describe-capacity-reservations --region us-east-1 --query CapacityReservations[?InstanceType==`p5.48xlarge`].{ID:CapacityReservationId,Type:InstanceType,State:State,Total:TotalInstanceCount,Available:AvailableInstanceCount,AZ:AvailabilityZone} --output table
---------------------------------------
|    DescribeCapacityReservations     |
+------------+------------------------+
|  AZ        |  us-east-1e            |
|  Available |  2                     |
|  ID        |  cr-0cbe491320188dfa6  |
|  State     |  active                |
|  Total     |  10                    |
|  Type      |  p5.48xlarge           |
+------------+------------------------+
```

**Result: PASS** — EKS cluster with GPU nodes managed by Auto Scaling Group, ASG configuration verified via AWS API. Evidence is configuration-level; a live scale event is not triggered to avoid disrupting the cluster.
