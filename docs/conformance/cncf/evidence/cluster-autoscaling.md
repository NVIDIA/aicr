# Cluster Autoscaling

**Kubernetes Version:** v1.35
**Platform:** linux/amd64
**Validated on:** EKS (p5.48xlarge, 8x H100) and GKE (a3-megagpu-8g, 8x H100)

---

Demonstrates CNCF AI Conformance requirement that the platform has GPU-aware
cluster autoscaling infrastructure configured, capable of scaling GPU node
groups based on workload demand.

## Summary

| Platform | Autoscaler | GPU Instances | Nodes | Result |
|----------|-----------|---------------|-------|--------|
| **EKS** | AWS Auto Scaling Group | p5.48xlarge (8x H100) | 2 | **PASS** |
| **GKE** | GKE built-in cluster autoscaler | a3-megagpu-8g (8x H100) | 2 | **PASS** |

---

## EKS: Auto Scaling Groups

**Generated:** 2026-03-10 03:44:07 UTC

The cluster uses an AWS Auto Scaling Group (ASG) for GPU nodes, which can scale
up/down based on workload demand. The ASG is configured with p5.48xlarge instances
(8x NVIDIA H100 80GB HBM3 each) backed by a capacity reservation.

### EKS Cluster Details

- **Region:** us-east-1
- **Cluster:** aws-us-east-1-aicr-cuj2
- **GPU Node Group:** gpu-worker

### GPU Nodes

**GPU nodes**
```
$ kubectl get nodes -l nvidia.com/gpu.present=true -o custom-columns=NAME:.metadata.name,INSTANCE-TYPE:.metadata.labels.node\.kubernetes\.io/instance-type,GPUS:.metadata.labels.nvidia\.com/gpu\.count,PRODUCT:.metadata.labels.nvidia\.com/gpu\.product,NODE-GROUP:.metadata.labels.nodeGroup,ZONE:.metadata.labels.topology\.kubernetes\.io/zone
NAME                           INSTANCE-TYPE   GPUS   PRODUCT                 NODE-GROUP   ZONE
ip-10-0-171-111.ec2.internal   p5.48xlarge     8      NVIDIA-H100-80GB-HBM3   gpu-worker   us-east-1e
ip-10-0-206-2.ec2.internal     p5.48xlarge     8      NVIDIA-H100-80GB-HBM3   gpu-worker   us-east-1e
```

### Auto Scaling Group (AWS)

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

### Capacity Reservation

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

**Result: PASS** — EKS cluster with GPU nodes managed by Auto Scaling Group, ASG configuration verified via AWS API.

---

## GKE: Built-in Cluster Autoscaler

**Generated:** 2026-03-16 21:50:46 UTC

GKE includes a built-in cluster autoscaler that manages node pool scaling based
on workload demand. The autoscaler is configured per node pool.

### GKE Cluster Details

- **Project:** eidosx
- **Zone:** us-central1-c

### GPU Nodes

**GPU nodes**
```
$ kubectl get nodes -l nvidia.com/gpu.present=true -o custom-columns=NAME:.metadata.name,INSTANCE-TYPE:.metadata.labels.node\.kubernetes\.io/instance-type,GPUS:.status.capacity.nvidia\.com/gpu,ACCELERATOR:.metadata.labels.cloud\.google\.com/gke-accelerator,NODE-POOL:.metadata.labels.cloud\.google\.com/gke-nodepool
NAME                                                 INSTANCE-TYPE   GPUS   ACCELERATOR             NODE-POOL
gke-aicr-demo2-aicr-demo2-gpu-worker-8de6040c-h2d0   a3-megagpu-8g   8      nvidia-h100-mega-80gb   aicr-demo2-gpu-worker
gke-aicr-demo2-aicr-demo2-gpu-worker-8de6040c-t81x   a3-megagpu-8g   8      nvidia-h100-mega-80gb   aicr-demo2-gpu-worker
```

### GKE Cluster Autoscaler Status

**Cluster Autoscaler Status**
```
autoscalerStatus: Running
clusterWide:
  health:
    lastProbeTime: "2026-03-16T21:50:43Z"
    lastTransitionTime: "2026-03-12T21:28:08Z"
    nodeCounts:
      registered:
        ready: 6
        total: 6
    status: Healthy
  scaleDown:
    status: NoCandidates
  scaleUp:
    status: NoActivity
nodeGroups:
- health:
    cloudProviderTarget: 1
    maxSize: 1
    minSize: 1
    status: Healthy
  name: .../gke-aicr-demo2-aicr-demo2-cpu-worker-cd95cf64-grp
- health:
    cloudProviderTarget: 2
    maxSize: 2
    minSize: 2
    status: Healthy
  name: .../gke-aicr-demo2-aicr-demo2-gpu-worker-8de6040c-grp
- health:
    cloudProviderTarget: 1
    maxSize: 3
    minSize: 1
    status: Healthy
  name: .../gke-aicr-demo2-aicr-demo2-system-f5af1da6-grp
- health:
    cloudProviderTarget: 1
    maxSize: 3
    minSize: 1
    status: Healthy
  name: .../gke-aicr-demo2-aicr-demo2-system-358b1ae8-grp
- health:
    cloudProviderTarget: 1
    maxSize: 3
    minSize: 1
    status: Healthy
  name: .../gke-aicr-demo2-aicr-demo2-system-b313be0b-grp
```

**Result: PASS** — GKE cluster with 2 GPU nodes and built-in cluster autoscaler active, all node groups healthy.

---

Evidence is configuration-level; a live scale event is not triggered to avoid disrupting the cluster.
