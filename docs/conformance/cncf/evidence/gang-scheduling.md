# Gang Scheduling (KAI Scheduler)

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-02-24 20:20:59 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates that the cluster supports gang (all-or-nothing) scheduling using KAI
scheduler with PodGroups. Both pods in the group must be scheduled together or not at all.

## KAI Scheduler Components

**KAI scheduler deployments**
```
$ kubectl get deploy -n kai-scheduler
NAME                    READY   UP-TO-DATE   AVAILABLE   AGE
admission               1/1     1            1           5d23h
binder                  1/1     1            1           5d23h
kai-operator            1/1     1            1           7d
kai-scheduler-default   1/1     1            1           7d
pod-grouper             1/1     1            1           5d23h
podgroup-controller     1/1     1            1           5d23h
queue-controller        1/1     1            1           5d23h
```

**KAI scheduler pods**
```
$ kubectl get pods -n kai-scheduler
NAME                                     READY   STATUS    RESTARTS   AGE
admission-669878d9d8-thrfv               1/1     Running   0          5d20h
binder-7f67b6c8f8-qkx4v                  1/1     Running   0          5d20h
kai-operator-6dd58c647-mhbr2             1/1     Running   0          5d20h
kai-scheduler-default-75b48f4b9f-vq52j   1/1     Running   0          5d20h
pod-grouper-5d5c88b6fb-fgbfn             1/1     Running   0          5d20h
podgroup-controller-56947478b-ldphl      1/1     Running   0          5d20h
queue-controller-5f5b6895b6-t46mz        1/1     Running   0          5d20h
```

## PodGroup CRD

**PodGroup CRD**
```
$ kubectl get crd podgroups.scheduling.run.ai
NAME                          CREATED AT
podgroups.scheduling.run.ai   2026-02-12T20:42:05Z
```

## Gang Scheduling Test

Deploy a PodGroup with minMember=2 and two GPU pods. KAI scheduler ensures both
pods are scheduled atomically.

**Test manifest:** `docs/conformance/cncf/manifests/gang-scheduling-test.yaml`

```yaml
---
apiVersion: v1
kind: Namespace
metadata:
  name: gang-scheduling-test
---
apiVersion: scheduling.run.ai/v2alpha2
kind: PodGroup
metadata:
  name: gang-test-group
  namespace: gang-scheduling-test
spec:
  minMember: 2
  queue: default-queue
---
apiVersion: v1
kind: Pod
metadata:
  name: gang-worker-0
  namespace: gang-scheduling-test
  labels:
    pod-group.scheduling.run.ai/name: gang-test-group
    pod-group.scheduling.run.ai/group-id: gang-test-group
spec:
  schedulerName: kai-scheduler
  restartPolicy: Never
  tolerations:
    - operator: Exists
  containers:
    - name: worker
      image: nvidia/cuda:12.9.0-base-ubuntu24.04
      command: ["bash", "-c", "nvidia-smi && echo 'Gang worker 0 completed successfully'"]
      resources:
        claims:
          - name: gpu
  resourceClaims:
    - name: gpu
      resourceClaimName: gang-gpu-claim-0
---
apiVersion: v1
kind: Pod
metadata:
  name: gang-worker-1
  namespace: gang-scheduling-test
  labels:
    pod-group.scheduling.run.ai/name: gang-test-group
    pod-group.scheduling.run.ai/group-id: gang-test-group
spec:
  schedulerName: kai-scheduler
  restartPolicy: Never
  tolerations:
    - operator: Exists
  containers:
    - name: worker
      image: nvidia/cuda:12.9.0-base-ubuntu24.04
      command: ["bash", "-c", "nvidia-smi && echo 'Gang worker 1 completed successfully'"]
      resources:
        claims:
          - name: gpu
  resourceClaims:
    - name: gpu
      resourceClaimName: gang-gpu-claim-1
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: gang-gpu-claim-0
  namespace: gang-scheduling-test
spec:
  devices:
    requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          allocationMode: ExactCount
          count: 1
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: gang-gpu-claim-1
  namespace: gang-scheduling-test
spec:
  devices:
    requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          allocationMode: ExactCount
          count: 1
```

**Apply test manifest**
```
$ kubectl apply -f docs/conformance/cncf/manifests/gang-scheduling-test.yaml
namespace/gang-scheduling-test created
podgroup.scheduling.run.ai/gang-test-group created
pod/gang-worker-0 created
pod/gang-worker-1 created
resourceclaim.resource.k8s.io/gang-gpu-claim-0 created
resourceclaim.resource.k8s.io/gang-gpu-claim-1 created
```

**PodGroup status**
```
$ kubectl get podgroups -n gang-scheduling-test -o wide
NAME                                                    AGE
gang-test-group                                         12s
pg-gang-worker-0-9d788a4d-ca91-4057-8fcd-569ca994417e   12s
pg-gang-worker-1-ac139cd5-5d46-471f-bdfb-6c52470eb405   11s
```

**Pod status**
```
$ kubectl get pods -n gang-scheduling-test -o wide
NAME            READY   STATUS      RESTARTS   AGE   IP              NODE                             NOMINATED NODE   READINESS GATES
gang-worker-0   0/1     Completed   0          13s   100.65.1.153    ip-100-64-171-120.ec2.internal   <none>           <none>
gang-worker-1   0/1     Completed   0          12s   100.65.247.22   ip-100-64-171-120.ec2.internal   <none>           <none>
```

**gang-worker-0 logs**
```
$ kubectl logs gang-worker-0 -n gang-scheduling-test
Tue Feb 24 20:21:18 2026       
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 580.105.08             Driver Version: 580.105.08     CUDA Version: 13.0     |
+-----------------------------------------+------------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |
|                                         |                        |               MIG M. |
|=========================================+========================+======================|
|   0  NVIDIA H100 80GB HBM3          On  |   00000000:75:00.0 Off |                    0 |
| N/A   28C    P0             67W /  700W |       0MiB /  81559MiB |      0%      Default |
|                                         |                        |             Disabled |
+-----------------------------------------+------------------------+----------------------+

+-----------------------------------------------------------------------------------------+
| Processes:                                                                              |
|  GPU   GI   CI              PID   Type   Process name                        GPU Memory |
|        ID   ID                                                               Usage      |
|=========================================================================================|
|  No running processes found                                                             |
+-----------------------------------------------------------------------------------------+
Gang worker 0 completed successfully
```

**gang-worker-1 logs**
```
$ kubectl logs gang-worker-1 -n gang-scheduling-test
Tue Feb 24 20:21:18 2026       
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 580.105.08             Driver Version: 580.105.08     CUDA Version: 13.0     |
+-----------------------------------------+------------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |
|                                         |                        |               MIG M. |
|=========================================+========================+======================|
|   0  NVIDIA H100 80GB HBM3          On  |   00000000:86:00.0 Off |                    0 |
| N/A   29C    P0             69W /  700W |       0MiB /  81559MiB |      0%      Default |
|                                         |                        |             Disabled |
+-----------------------------------------+------------------------+----------------------+

+-----------------------------------------------------------------------------------------+
| Processes:                                                                              |
|  GPU   GI   CI              PID   Type   Process name                        GPU Memory |
|        ID   ID                                                               Usage      |
|=========================================================================================|
|  No running processes found                                                             |
+-----------------------------------------------------------------------------------------+
Gang worker 1 completed successfully
```

**Result: PASS** — Both pods scheduled and completed together via gang scheduling.

## Cleanup

**Delete test namespace**
```
$ kubectl delete namespace gang-scheduling-test --ignore-not-found
namespace "gang-scheduling-test" deleted
```
