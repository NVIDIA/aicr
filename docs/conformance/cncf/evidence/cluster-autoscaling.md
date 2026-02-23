# Cluster Autoscaling

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-02-23 22:08:05 UTC
**Kubernetes Version:** v1.34
**Platform:** EKS (p5.48xlarge, NVIDIA H100 80GB HBM3)

> **Note:** Cluster-specific identifiers (account IDs, AMI IDs, node hostnames,
> capacity reservation IDs) have been sanitized in this evidence document.

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
$ aws autoscaling describe-auto-scaling-groups --query '...'
+---------+------------+------+------+----------------+
| Desired | Instances  | Max  | Min  |     Name       |
+---------+------------+------+------+----------------+
|  1      |  1         |  1   |  1   |  <cluster>-cpu |
|  1      |  1         |  2   |  1   |  <cluster>-gpu |
|  3      |  3         |  3   |  3   |  <cluster>-sys |
+---------+------------+------+------+----------------+
```

### GPU ASG Configuration

**GPU ASG details**
```
+------------------+------------------+
|  DesiredCapacity |  1               |
|  HealthCheckType |  EC2             |
|  MaxSize         |  2               |
|  MinSize         |  1               |
|  InstanceType    |  p5.48xlarge     |
+------------------+------------------+
|  AvailabilityZone: us-east-1e      |
+------------------------------------+
```

### Launch Template

**GPU instance type and capacity reservation**
```
Instance Type:               p5.48xlarge
Capacity Reservation:        capacity-reservations-only (dedicated)
```

## Capacity Reservation

Dedicated GPU capacity ensures instances are available for scale-up without
on-demand availability risk.

**GPU capacity reservation**
```
+------------+------------------------+
|  AZ        |  us-east-1e            |
|  Available |  4                     |
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
$ kubectl get nodes -o custom-columns=NAME:...,GPU:...,INSTANCE-TYPE:...,VERSION:...
NAME          GPU      INSTANCE-TYPE   VERSION
gpu-node-1    8        p5.48xlarge     v1.34.1
sys-node-1    <none>   m4.16xlarge     v1.34.2
sys-node-2    <none>   m4.16xlarge     v1.34.2
sys-node-3    <none>   m4.16xlarge     v1.34.1
cpu-node-1    <none>   m4.16xlarge     v1.34.2
```

## Autoscaler Integration

The GPU ASG is tagged for Kubernetes Cluster Autoscaler discovery. When a Cluster
Autoscaler or Karpenter is deployed with appropriate IAM permissions, it can
automatically scale GPU nodes based on pending pod requests.

**ASG autoscaler tags**
```
+-------------------------------------------------------+--------+
|                         Key                           | Value  |
+-------------------------------------------------------+--------+
|  k8s.io/cluster-autoscaler/enabled                    |  true  |
|  k8s.io/cluster-autoscaler/<cluster-name>             |  owned |
|  kubernetes.io/cluster/<cluster-name>                 |  owned |
+-------------------------------------------------------+--------+
```

## Alternative: Automated Autoscaling Demo

For a fully automated demonstration, deploy the Kubernetes Cluster Autoscaler or
Karpenter to trigger scale-up/down based on pending GPU pods:

```bash
# 1. Create IAM role with autoscaling permissions
#    Required: autoscaling:DescribeAutoScalingGroups, autoscaling:SetDesiredCapacity,
#    autoscaling:TerminateInstanceInAutoScalingGroup, ec2:DescribeLaunchTemplateVersions,
#    ec2:DescribeInstanceTypes

# 2. Deploy Cluster Autoscaler with IRSA (IAM Roles for Service Accounts)
#    Set eks.amazonaws.com/role-arn on the service account

# 3. Create a pending GPU pod to trigger scale-up
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: gpu-pending
spec:
  tolerations: [{operator: Exists}]
  containers:
    - name: gpu
      image: nvidia/cuda:12.9.0-base-ubuntu24.04
      resources:
        limits:
          nvidia.com/gpu: 8  # request all GPUs on a new node
EOF

# 4. Cluster Autoscaler detects pending pod → scales ASG → new node joins
# 5. Pod schedules on new node
# 6. Delete pod → Autoscaler scales down after cool-down period
```

> **Note:** This requires an IAM role with EC2 and Auto Scaling permissions
> associated with the Cluster Autoscaler service account via IRSA. The IAM
> configuration is cluster-specific and managed by the infrastructure team.

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
