# Gang Scheduling (KAI Scheduler)

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-03-06 19:37:40 UTC
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
admission               1/1     1            1           44h
binder                  1/1     1            1           44h
kai-operator            1/1     1            1           44h
kai-scheduler-default   1/1     1            1           44h
pod-grouper             1/1     1            1           44h
podgroup-controller     1/1     1            1           44h
queue-controller        1/1     1            1           44h
```

**KAI scheduler pods**
```
$ kubectl get pods -n kai-scheduler
NAME                                     READY   STATUS    RESTARTS   AGE
admission-54f4bcf874-lczkp               1/1     Running   0          44h
binder-5f9dc97959-thrf8                  1/1     Running   0          44h
kai-operator-86675bdc5d-ww9sf            1/1     Running   0          44h
kai-scheduler-default-68bc7484b8-hczc2   1/1     Running   0          44h
pod-grouper-56947ccdf-rslf9              1/1     Running   0          44h
podgroup-controller-65c74cbc9c-nbwvr     1/1     Running   0          44h
queue-controller-7847c76b97-zhzdv        1/1     Running   0          44h
```

## PodGroup CRD

**PodGroup CRD**
```
$ kubectl get crd podgroups.scheduling.run.ai
NAME                          CREATED AT
podgroups.scheduling.run.ai   2026-02-28T18:05:52Z
```

## Gang Scheduling Test

Deploy a PodGroup with minMember=2 and two GPU pods. KAI scheduler ensures both
pods are scheduled atomically.

**Test manifest:** `pkg/evidence/scripts/manifests/gang-scheduling-test.yaml`
```yaml
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Gang scheduling test with PodGroup and KAI scheduler
# Demonstrates all-or-nothing scheduling: both pods must be scheduled together
# Requires: KAI scheduler with PodGroup CRD
# Usage: kubectl apply -f pkg/evidence/scripts/manifests/gang-scheduling-test.yaml
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
  securityContext:
    runAsNonRoot: false
    seccompProfile:
      type: RuntimeDefault
  tolerations:
    - operator: Exists
  containers:
    - name: worker
      image: nvidia/cuda:12.9.0-base-ubuntu24.04
      command: ["bash", "-c", "nvidia-smi && echo 'Gang worker 0 completed successfully'"]
      securityContext:
        allowPrivilegeEscalation: false
      resources:
        limits:
          nvidia.com/gpu: 1
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
  securityContext:
    runAsNonRoot: false
    seccompProfile:
      type: RuntimeDefault
  tolerations:
    - operator: Exists
  containers:
    - name: worker
      image: nvidia/cuda:12.9.0-base-ubuntu24.04
      command: ["bash", "-c", "nvidia-smi && echo 'Gang worker 1 completed successfully'"]
      securityContext:
        allowPrivilegeEscalation: false
      resources:
        limits:
          nvidia.com/gpu: 1
```

**Apply test manifest**
```
$ kubectl apply -f manifests/gang-scheduling-test.yaml
namespace/gang-scheduling-test created
podgroup.scheduling.run.ai/gang-test-group created
pod/gang-worker-0 created
pod/gang-worker-1 created
```

**PodGroup status**
```
$ kubectl get podgroups -n gang-scheduling-test -o wide
NAME                                                    AGE
gang-test-group                                         11s
pg-gang-worker-0-99f4d8ea-2574-4ed1-ba4f-8e83daededad   10s
pg-gang-worker-1-0605b26c-1b90-4954-9e6f-8ec74c2713c2   10s
```

**Pod status**
```
$ kubectl get pods -n gang-scheduling-test -o wide
NAME            READY   STATUS      RESTARTS   AGE   IP               NODE                             NOMINATED NODE   READINESS GATES
gang-worker-0   0/1     Completed   0          13s   100.65.208.144   ip-100-64-147-149.ec2.internal   <none>           <none>
gang-worker-1   0/1     Completed   0          12s   100.65.218.207   ip-100-64-147-149.ec2.internal   <none>           <none>
```

**gang-worker-0 logs**
```
$ kubectl logs gang-worker-0 -n gang-scheduling-test
Fri Mar  6 19:37:52 2026       
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 580.105.08             Driver Version: 580.105.08     CUDA Version: 13.0     |
+-----------------------------------------+------------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |
|                                         |                        |               MIG M. |
|=========================================+========================+======================|
|   0  NVIDIA H100 80GB HBM3          On  |   00000000:64:00.0 Off |                    0 |
| N/A   29C    P0             71W /  700W |       0MiB /  81559MiB |      0%      Default |
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
Fri Mar  6 19:37:52 2026       
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 580.105.08             Driver Version: 580.105.08     CUDA Version: 13.0     |
+-----------------------------------------+------------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |
|                                         |                        |               MIG M. |
|=========================================+========================+======================|
|   0  NVIDIA H100 80GB HBM3          On  |   00000000:86:00.0 Off |                    0 |
| N/A   30C    P0             67W /  700W |       0MiB /  81559MiB |      0%      Default |
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
$ cleanup_ns gang-scheduling-test

```
