# Gang Scheduling (KAI Scheduler)

**Cluster:** `EKS / p6e-gb200.36xlarge / NVIDIA-GB200`
**Generated:** 2026-08-09 01:22:10 UTC
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
admission               1/1     1            1           26d
binder                  1/1     1            1           26d
kai-operator            1/1     1            1           26d
kai-scheduler-default   1/1     1            1           40d
pod-grouper             1/1     1            1           26d
podgroup-controller     1/1     1            1           26d
queue-controller        1/1     1            1           26d
```

**KAI scheduler pods**
```
$ kubectl get pods -n kai-scheduler
NAME                                     READY   STATUS    RESTARTS   AGE
admission-6985c5d87-69nhh                1/1     Running   0          26d
binder-76db6fffcc-fbrm7                  1/1     Running   0          26d
kai-operator-7dcfd654d9-jfswp            1/1     Running   0          26d
kai-scheduler-default-575cff69cc-6rvsk   1/1     Running   0          26d
pod-grouper-5bd8fc9448-xspkc             1/1     Running   0          26d
podgroup-controller-6fc57dcfd8-k4mbj     1/1     Running   0          26d
queue-controller-78c5fd448b-qndwz        1/1     Running   0          26d
```

## PodGroup CRD

**PodGroup CRD**
```
$ kubectl get crd podgroups.scheduling.run.ai
NAME                          CREATED AT
podgroups.scheduling.run.ai   2026-06-29T23:49:46Z
```

## Gang Scheduling Test (two-phase)

Deploy a PodGroup with minMember=2 and two GPU pods, both pinned to one GPU
node. The test runs in two phases so the all-or-nothing barrier is actually
exercised (a group that schedules into ample free capacity proves nothing —
any scheduler would place both pods):

1. **Barrier phase** — a blocker pod occupies all but ONE of the node's GPUs,
   so the two-pod group cannot fully fit. Gang semantics require that
   **neither** pod schedules; one Running pod here means the barrier is
   violated (an ordinary scheduler would run one and leave one Pending).
2. **Release phase** — the blocker is deleted; **both** pods must then be
   scheduled and run to completion together.

**Test manifest:** `pkg/evidence/cncf/scripts/manifests/gang-scheduling-test.yaml`
(the `GANG_TEST_NODE` placeholder is substituted with the chosen node)
```yaml
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
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
#
# Gang scheduling test with PodGroup, device-plugin GPU requests, and KAI
# scheduler. GPUs are requested as scalar nvidia.com/gpu limits — the
# device-plugin production default (#1327).
# Demonstrates all-or-nothing scheduling in TWO phases (see collect_gang in
# collect-evidence.sh): under constrained capacity NEITHER pod may schedule
# (the barrier), then capacity is freed and BOTH must run together. Both
# workers are pinned to a single node via the GANG_TEST_NODE placeholder,
# which the script substitutes with a chosen GPU node before applying.
# Requires: KAI scheduler with PodGroup CRD, NVIDIA device plugin (nvidia.com/gpu)
# Standalone usage (substitute a node name with >= 2 allocatable GPUs):
#   sed "s/GANG_TEST_NODE/<gpu-node>/" gang-scheduling-test.yaml | kubectl apply -f -
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
  # pod-group-name is the LOAD-BEARING association: KAI's pod-grouper skips a
  # bare pod carrying this annotation (no auto-created per-pod PodGroup) and
  # the scheduler joins it to the named PodGroup (verified against KAI
  # v0.14.1: PodGroupAnnotationForPod in pod_controller.go/pod_info.go). The
  # pod-group labels alone do NOT associate — the pod-grouper ignores them
  # and creates per-pod groups, silently degrading the test to individual
  # scheduling (observed live on the gb200 conformance cluster). The labels
  # are retained deliberately: inert at KAI v0.14.1, kept for reporting and
  # forward compatibility.
  annotations:
    pod-group-name: gang-test-group
  labels:
    pod-group.scheduling.run.ai/name: gang-test-group
    pod-group.scheduling.run.ai/group-id: gang-test-group
spec:
  schedulerName: kai-scheduler
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: kubernetes.io/hostname
                operator: In
                values:
                  - GANG_TEST_NODE
  restartPolicy: Never
  # Kubelet-enforced GPU-tenancy bound, independent of the collector's
  # liveness (its section timeout is an uncatchable SIGKILL of the script).
  activeDeadlineSeconds: 600
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
  # See gang-worker-0: the annotation is the load-bearing group association.
  annotations:
    pod-group-name: gang-test-group
  labels:
    pod-group.scheduling.run.ai/name: gang-test-group
    pod-group.scheduling.run.ai/group-id: gang-test-group
spec:
  schedulerName: kai-scheduler
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: kubernetes.io/hostname
                operator: In
                values:
                  - GANG_TEST_NODE
  restartPolicy: Never
  # Kubelet-enforced GPU-tenancy bound, independent of the collector's
  # liveness (its section timeout is an uncatchable SIGKILL of the script).
  activeDeadlineSeconds: 600
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

**Test node:** `ip-100-64-135-20.ec2.internal` (4 allocatable GPUs; blocker occupies 3)

**Apply capacity blocker**
```
$ kubectl apply -f -
pod/gang-capacity-blocker created
```

**Apply test manifest (node-pinned)**
```
$ sed "s/GANG_TEST_NODE/ip-100-64-135-20.ec2.internal/" manifests/gang-scheduling-test.yaml | kubectl apply -f -
namespace/gang-scheduling-test configured
podgroup.scheduling.run.ai/gang-test-group created
pod/gang-worker-0 created
pod/gang-worker-1 created
```

**Pod status under constrained capacity**
```
$ kubectl get pods -n gang-scheduling-test -o wide
NAME                    READY   STATUS    RESTARTS   AGE   IP              NODE                            NOMINATED NODE   READINESS GATES
gang-capacity-blocker   1/1     Running   0          83s   100.65.243.37   ip-100-64-135-20.ec2.internal   <none>           <none>
gang-worker-0           0/1     Pending   0          71s   <none>          <none>                          <none>           <none>
gang-worker-1           0/1     Pending   0          70s   <none>          <none>                          <none>           <none>
```

**Worker PodScheduled conditions under constrained capacity**
```
$ kubectl get pods -n gang-scheduling-test -o jsonpath={range .items[*]}{.metadata.name}{": "}{range .status.conditions[?(@.type=="PodScheduled")]}{.status}{" reason="}{.reason}{" msg="}{.message}{end}{"\n"}{end}
gang-capacity-blocker: True reason= msg=
gang-worker-0: False reason=Unschedulable msg=PodSchedulingErrors: Resources were found for 1 pods while 2 are required for gang scheduling. Additional pods cannot be scheduled due to: no nodes with enough resources were found: 1 node(s) didn't match Pod's node affinity/selector.
5 node(s) didn't have enough resources: GPUs..
gang-worker-1: False reason=Unschedulable msg=no nodes with enough resources were found: 1 node(s) didn't match Pod's node affinity/selector.
5 node(s) didn't have enough resources: GPUs.
```

**PodGroup status under constrained capacity**
```
$ kubectl get podgroups -n gang-scheduling-test -o yaml
apiVersion: v1
items:
- apiVersion: scheduling.run.ai/v2alpha2
  kind: PodGroup
  metadata:
    annotations:
      kubectl.kubernetes.io/last-applied-configuration: |
        {"apiVersion":"scheduling.run.ai/v2alpha2","kind":"PodGroup","metadata":{"annotations":{},"name":"gang-test-group","namespace":"gang-scheduling-test"},"spec":{"minMember":2,"queue":"default-queue"}}
    creationTimestamp: "2026-08-09T01:22:37Z"
    generation: 1
    name: gang-test-group
    namespace: gang-scheduling-test
    resourceVersion: "63481604"
    uid: 39e84197-13ab-45b1-a57c-bbca33eee989
  spec:
    minMember: 2
    queue: default-queue
  status:
    resourcesStatus:
      requested:
        nvidia.com/gpu: "2"
    schedulingConditions:
    - lastTransitionTime: "2026-08-09T01:22:39Z"
      message: "PodSchedulingErrors: Resources were found for 1 pods while 2 are required
        for gang scheduling. Additional pods cannot be scheduled due to: no nodes
        with enough resources were found: 1 node(s) didn't match Pod's node affinity/selector.
        \n5 node(s) didn't have enough resources: GPUs.."
      nodePool: default
      reason: Unschedulable
      reasons:
      - message: "Resources were found for 1 pods while 2 are required for gang scheduling.
          Additional pods cannot be scheduled due to: no nodes with enough resources
          were found: 1 node(s) didn't match Pod's node affinity/selector. \n5 node(s)
          didn't have enough resources: GPUs."
        reason: PodSchedulingErrors
      status: "True"
      transitionID: "1"
      type: UnschedulableOnNodePool
kind: List
metadata:
  resourceVersion: ""
```

**Scheduling events under constrained capacity**
```
$ kubectl get events -n gang-scheduling-test --sort-by=.lastTimestamp
LAST SEEN   TYPE      REASON          OBJECT                      MESSAGE
86s         Normal    Pulled          pod/gang-capacity-blocker   Container image "nvidia/cuda:12.9.0-base-ubuntu24.04" already present on machine
86s         Normal    Created         pod/gang-capacity-blocker   Created container: blocker
86s         Normal    Started         pod/gang-capacity-blocker   Started container blocker
74s         Normal    NotReady        podgroup/gang-test-group    Job is not ready for scheduling. Waiting for 2 pods, currently 1 exist, 0 are gated
50s         Normal    Unschedulable   podgroup/gang-test-group    PodSchedulingErrors: Resources were found for 1 pods while 2 are required for gang scheduling. Additional pods cannot be scheduled due to: no nodes with enough resources were found: 1 node(s) didn't match Pod's node affinity/selector. ...
49s         Warning   Unschedulable   pod/gang-worker-0           PodSchedulingErrors: Resources were found for 1 pods while 2 are required for gang scheduling. Additional pods cannot be scheduled due to: no nodes with enough resources were found: 1 node(s) didn't match Pod's node affinity/selector. ...
49s         Warning   Unschedulable   pod/gang-worker-1           no nodes with enough resources were found: 1 node(s) didn't match Pod's node affinity/selector. ...
```

**Barrier phase:** worker-0 bound to node: <none>, worker-1 bound to node: <none>; the scheduler AFFIRMATIVELY made the gang decision (both workers PodScheduled=False/Unschedulable AND the named PodGroup reports the one-of-two gang refusal: 'Resources were found for 1 pods while 2 are required for gang scheduling').

**PodGroup status**
```
$ kubectl get podgroups -n gang-scheduling-test -o wide
NAME              AGE
gang-test-group   88s
```

**Pod status**
```
$ kubectl get pods -n gang-scheduling-test -o wide
NAME            READY   STATUS      RESTARTS   AGE   IP               NODE                            NOMINATED NODE   READINESS GATES
gang-worker-0   0/1     Completed   0          90s   100.65.213.44    ip-100-64-135-20.ec2.internal   <none>           <none>
gang-worker-1   0/1     Completed   0          89s   100.65.145.232   ip-100-64-135-20.ec2.internal   <none>           <none>
```

**gang-worker-0 logs**
```
$ kubectl logs gang-worker-0 -n gang-scheduling-test
Sun Aug  9 01:23:55 2026
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 580.126.20             Driver Version: 580.126.20     CUDA Version: 13.0     |
+-----------------------------------------+------------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |
|                                         |                        |               MIG M. |
|=========================================+========================+======================|
|   0  NVIDIA GB200                   On  |   00000000:9C:00.0 Off |                    0 |
| N/A   27C    P0            156W / 1200W |       0MiB / 189471MiB |      0%      Default |
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
Sun Aug  9 01:23:55 2026
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 580.126.20             Driver Version: 580.126.20     CUDA Version: 13.0     |
+-----------------------------------------+------------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |
|                                         |                        |               MIG M. |
|=========================================+========================+======================|
|   0  NVIDIA GB200                   On  |   00000000:29:00.0 Off |                    0 |
| N/A   27C    P0            165W / 1200W |       0MiB / 189471MiB |      0%      Default |
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

**Result: PASS** — under constrained capacity neither pod was bound to a node (all-or-nothing barrier held); after freeing capacity both pods were scheduled and completed together.

## Cleanup

**Delete test namespace**
```
$ cleanup_ns gang-scheduling-test

```
