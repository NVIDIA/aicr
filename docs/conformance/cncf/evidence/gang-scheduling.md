# Gang Scheduling (KAI Scheduler)

**Generated:** 2026-02-19 19:11:48 UTC
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
admission               1/1     1            1           22h
binder                  1/1     1            1           22h
kai-operator            1/1     1            1           47h
kai-scheduler-default   1/1     1            1           47h
pod-grouper             1/1     1            1           22h
podgroup-controller     1/1     1            1           22h
queue-controller        1/1     1            1           22h
```

**KAI scheduler pods**
```
$ kubectl get pods -n kai-scheduler
NAME                                     READY   STATUS    RESTARTS   AGE
admission-669878d9d8-thrfv               1/1     Running   0          19h
binder-7f67b6c8f8-qkx4v                  1/1     Running   0          19h
kai-operator-6dd58c647-mhbr2             1/1     Running   0          19h
kai-scheduler-default-75b48f4b9f-vq52j   1/1     Running   0          19h
pod-grouper-5d5c88b6fb-fgbfn             1/1     Running   0          19h
podgroup-controller-56947478b-ldphl      1/1     Running   0          19h
queue-controller-5f5b6895b6-t46mz        1/1     Running   0          19h
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

**Test manifest:** `tests/ai-conformance/manifests/gang-scheduling-test.yaml`

**Apply test manifest**
```
$ kubectl apply -f /Users/yuanc/projects/eidos/tests/ai-conformance/manifests/gang-scheduling-test.yaml
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
gang-test-group                                         11s
pg-gang-worker-0-9fe42ea9-4f55-4674-84fb-c52ba0dba958   10s
pg-gang-worker-1-977cfdce-ffc5-4c19-aa80-1bb7f15fc1fd   10s
```

**Pod status**
```
$ kubectl get pods -n gang-scheduling-test -o wide
NAME            READY   STATUS      RESTARTS   AGE   IP               NODE                             NOMINATED NODE   READINESS GATES
gang-worker-0   0/1     Completed   0          12s   100.65.141.103   ip-100-64-171-120.ec2.internal   <none>           <none>
gang-worker-1   0/1     Completed   0          12s   100.65.244.167   ip-100-64-171-120.ec2.internal   <none>           <none>
```

**gang-worker-0 logs**
```
$ kubectl logs gang-worker-0 -n gang-scheduling-test
Thu Feb 19 19:12:05 2026       
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 580.105.08             Driver Version: 580.105.08     CUDA Version: 13.0     |
+-----------------------------------------+------------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |
|                                         |                        |               MIG M. |
|=========================================+========================+======================|
|   0  NVIDIA H100 80GB HBM3          On  |   00000000:B9:00.0 Off |                    0 |
| N/A   31C    P0            115W /  700W |   74199MiB /  81559MiB |      0%      Default |
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
Thu Feb 19 19:12:05 2026       
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 580.105.08             Driver Version: 580.105.08     CUDA Version: 13.0     |
+-----------------------------------------+------------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |
|                                         |                        |               MIG M. |
|=========================================+========================+======================|
|   0  NVIDIA H100 80GB HBM3          On  |   00000000:CA:00.0 Off |                    0 |
| N/A   27C    P0             69W /  700W |       0MiB /  81559MiB |      0%      Default |
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
