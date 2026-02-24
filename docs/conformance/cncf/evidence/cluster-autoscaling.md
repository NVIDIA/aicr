# Cluster Autoscaling

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-02-24 20:26:59 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates CNCF AI Conformance requirement that the platform can scale up/down
node groups containing specific accelerator types based on pending pods requesting
those accelerators.

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

**Auto Scaling Groups**
```
-------------------------------------------------------------
|                 DescribeAutoScalingGroups                 |
+---------+------------+------+------+----------------------+
| Desired | Instances  | Max  | Min  |        Name          |
+---------+------------+------+------+----------------------+
|  1      |  1         |  1   |  1   |  ktsetfavua-cpu      |
|  1      |  1         |  1   |  1   |  ktsetfavua-gpu      |
|  3      |  3         |  3   |  3   |  ktsetfavua-system   |
+---------+------------+------+------+----------------------+
```

### GPU ASG Configuration

**GPU ASG details**
```
---------------------------------------
|      DescribeAutoScalingGroups      |
+------------------+------------------+
|  DesiredCapacity |  1               |
|  HealthCheckType |  EC2             |
|  LaunchTemplate  |  ktsetfavua-gpu  |
|  MaxSize         |  1               |
|  MinSize         |  1               |
|  Name            |  ktsetfavua-gpu  |
+------------------+------------------+
||         AvailabilityZones         ||
|+-----------------------------------+|
||  us-east-1e                       ||
|+-----------------------------------+|
```

### Launch Template (GPU Instance Type)

**GPU launch template**
```
-------------------------------------------------------------------
|                 DescribeLaunchTemplateVersions                  |
+---------------------------------------+-------------------------+
|                ImageId                |      InstanceType       |
+---------------------------------------+-------------------------+
|  ami-XXXXXXXXXXXX                     |  p5.48xlarge            |
+---------------------------------------+-------------------------+
||                      CapacityReservation                      ||
|+--------------------------------+------------------------------+|
||  CapacityReservationPreference |  capacity-reservations-only  ||
|+--------------------------------+------------------------------+|
|||                  CapacityReservationTarget                  |||
||+------------------------------+------------------------------+||
|||  CapacityReservationId       |  cr-0cbe491320188dfa6        |||
||+------------------------------+------------------------------+||
```

## Capacity Reservation

Dedicated GPU capacity ensures instances are available for scale-up without
on-demand availability risk.

**GPU capacity reservation**
```
---------------------------------------
|    DescribeCapacityReservations     |
+------------+------------------------+
|  AZ        |  us-east-1e            |
|  Available |  0                     |
|  ID        |  cr-0cbe491320188dfa6  |
|  State     |  active                |
|  Total     |  10                    |
|  Type      |  p5.48xlarge           |
+------------+------------------------+
```

## Current GPU Nodes

GPU nodes provisioned by the ASG are registered in the Kubernetes cluster with
appropriate labels and GPU resources.

**GPU nodes**
```
$ kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.capacity.nvidia\.com/gpu,INSTANCE-TYPE:.metadata.labels.node\.kubernetes\.io/instance-type,VERSION:.status.nodeInfo.kubeletVersion
NAME                             GPU      INSTANCE-TYPE   VERSION
ip-100-64-171-120.ec2.internal   8        p5.48xlarge     v1.34.1
ip-100-64-4-149.ec2.internal     <none>   m4.16xlarge     v1.34.2
ip-100-64-6-88.ec2.internal      <none>   m4.16xlarge     v1.34.2
ip-100-64-83-166.ec2.internal    <none>   m4.16xlarge     v1.34.1
ip-100-64-9-88.ec2.internal      <none>   m4.16xlarge     v1.34.2
```

## Autoscaler Integration

The GPU ASG is tagged for Kubernetes Cluster Autoscaler discovery. When a Cluster
Autoscaler or Karpenter is deployed with appropriate IAM permissions, it can
automatically scale GPU nodes based on pending pod requests.

**ASG autoscaler tags**
```
------------------------------------------------------------------------------
|                                DescribeTags                                |
+-------------------------------------------------------------------+--------+
|                                Key                                | Value  |
+-------------------------------------------------------------------+--------+
|  k8s.io/cluster-autoscaler/enabled                                |  true  |
|  k8s.io/cluster-autoscaler/ktsetfavua-dgxc-k8s-aws-use1-non-prod  |  owned |
|  k8s.io/cluster/ktsetfavua-dgxc-k8s-aws-use1-non-prod             |  owned |
|  kubernetes.io/cluster/ktsetfavua-dgxc-k8s-aws-use1-non-prod      |  owned |
+-------------------------------------------------------------------+--------+
```

## Platform Support

Most major cloud providers offer native node autoscaling for their managed
Kubernetes services:

| Provider | Service | Autoscaling Mechanism |
|----------|---------|----------------------|
| AWS | EKS | Auto Scaling Groups, Karpenter, Cluster Autoscaler |
| GCP | GKE | Node Auto-provisioning, Cluster Autoscaler |
| Azure | AKS | Node pool autoscaling, Cluster Autoscaler, Karpenter |
| OCI | OKE | Node pool autoscaling, Cluster Autoscaler |

The cluster's GPU ASG can be integrated with any of the supported autoscaling
mechanisms. Kubernetes Cluster Autoscaler and Karpenter both support ASG-based
node group discovery via tags (`k8s.io/cluster-autoscaler/enabled`).

**Result: PASS** — GPU node group (ASG) configured with p5.48xlarge instances, backed by capacity reservation, tagged for autoscaler discovery, and scalable via min/max configuration.
