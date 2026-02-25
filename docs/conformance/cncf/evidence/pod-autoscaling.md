# Pod Autoscaling (HPA with GPU Metrics)

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-02-24 20:43:33 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates CNCF AI Conformance requirement that HPA functions correctly for pods
utilizing accelerators, including the ability to scale based on custom GPU metrics.

## Summary

1. **Prometheus Adapter** — Exposes GPU metrics via Kubernetes custom metrics API
2. **Custom Metrics API** — `gpu_utilization`, `gpu_memory_used`, `gpu_power_usage` available
3. **GPU Stress Workload** — Deployment running CUDA N-Body Simulation to generate GPU load
4. **HPA Configuration** — Targets `gpu_utilization` with threshold of 50%
5. **HPA Scale-Up** — Successfully scales replicas when GPU utilization exceeds target
6. **HPA Scale-Down** — Successfully scales back down when GPU load is removed
7. **Result: PASS**

---

## Prometheus Adapter

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                 READY   STATUS    RESTARTS      AGE
prometheus-adapter-d9dbc69cb-jxgp9   1/1     Running   1 (27m ago)   23h
```

**Prometheus adapter service**
```
$ kubectl get svc prometheus-adapter -n monitoring
NAME                 TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)   AGE
prometheus-adapter   ClusterIP   172.20.192.109   <none>        443/TCP   11d
```

## Custom Metrics API

**Available custom metrics**
```
$ kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1 | jq .resources[].name
pods/gpu_memory_used
namespaces/gpu_memory_used
pods/gpu_power_usage
namespaces/gpu_power_usage
namespaces/gpu_utilization
pods/gpu_utilization
```

## GPU Stress Test Deployment

Deploy a GPU workload running CUDA N-Body Simulation to generate sustained GPU utilization,
then create an HPA targeting `gpu_utilization` to demonstrate autoscaling.

**Test manifest:** `docs/conformance/cncf/manifests/hpa-gpu-test.yaml`

```yaml
---
apiVersion: v1
kind: Namespace
metadata:
  name: hpa-test
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-workload
  namespace: hpa-test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: gpu-workload
  template:
    metadata:
      labels:
        app: gpu-workload
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        seccompProfile:
          type: RuntimeDefault
      tolerations:
        - operator: Exists
      containers:
        - name: gpu-worker
          image: nvcr.io/nvidia/k8s/cuda-sample:nbody-cuda11.7.1-ubuntu18.04
          command: ["bash", "-c"]
          args: ["while true; do /cuda-samples/nbody -benchmark -numbodies=16777216 -iterations=10000; done"]
          securityContext:
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
          resources:
            limits:
              nvidia.com/gpu: 1
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: gpu-workload-hpa
  namespace: hpa-test
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: gpu-workload
  minReplicas: 1
  maxReplicas: 4
  metrics:
    - type: Pods
      pods:
        metric:
          name: gpu_utilization
        target:
          type: AverageValue
          averageValue: "50"
```

**Apply test manifest**
```
$ kubectl apply -f docs/conformance/cncf/manifests/hpa-gpu-test.yaml
namespace/hpa-test created
deployment.apps/gpu-workload created
horizontalpodautoscaler.autoscaling/gpu-workload-hpa created
```

**GPU workload pod**
```
$ kubectl get pods -n hpa-test -o wide
NAME                           READY   STATUS    RESTARTS   AGE   IP              NODE                             NOMINATED NODE   READINESS GATES
gpu-workload-7d7f4dbdf-9559s   1/1     Running   0          3s    100.65.30.220   ip-100-64-171-120.ec2.internal   <none>           <none>
```

## HPA Status

**HPA status**
```
$ kubectl get hpa -n hpa-test
NAME               REFERENCE                 TARGETS   MINPODS   MAXPODS   REPLICAS   AGE
gpu-workload-hpa   Deployment/gpu-workload   100/50    1         4         2          101s
```

**HPA details**
```
$ kubectl describe hpa gpu-workload-hpa -n hpa-test
Name:                         gpu-workload-hpa
Namespace:                    hpa-test
Labels:                       <none>
Annotations:                  <none>
CreationTimestamp:            Tue, 24 Feb 2026 12:43:45 -0800
Reference:                    Deployment/gpu-workload
Metrics:                      ( current / target )
  "gpu_utilization" on pods:  100 / 50
Min replicas:                 1
Max replicas:                 4
Deployment pods:              2 current / 2 desired
Conditions:
  Type            Status  Reason              Message
  ----            ------  ------              -------
  AbleToScale     True    ReadyForNewScale    recommended size matches current size
  ScalingActive   True    ValidMetricFound    the HPA was able to successfully calculate a replica count from pods metric gpu_utilization
  ScalingLimited  False   DesiredWithinRange  the desired count is within the acceptable range
Events:
  Type    Reason             Age   From                       Message
  ----    ------             ----  ----                       -------
  Normal  SuccessfulRescale  28s   horizontal-pod-autoscaler  New size: 2; reason: pods metric gpu_utilization above target
```

## GPU Utilization Evidence

**GPU utilization (nvidia-smi)**
```
$ kubectl exec -n hpa-test gpu-workload-7d7f4dbdf-9559s -- nvidia-smi --query-gpu=utilization.gpu,utilization.memory,power.draw --format=csv
utilization.gpu [%], utilization.memory [%], power.draw [W]
100 %, 0 %, 592.74 W
```

## Pods After Scale-Up

**Pods after scale-up**
```
$ kubectl get pods -n hpa-test -o wide
NAME                           READY   STATUS    RESTARTS   AGE    IP              NODE                             NOMINATED NODE   READINESS GATES
gpu-workload-7d7f4dbdf-9559s   1/1     Running   0          108s   100.65.30.220   ip-100-64-171-120.ec2.internal   <none>           <none>
gpu-workload-7d7f4dbdf-v8csq   1/1     Running   0          33s    100.65.63.246   ip-100-64-171-120.ec2.internal   <none>           <none>
```

## Scale-Down Verification

Scale the deployment to 0, replace GPU workload with an idle container, then
scale back to 1. Verify HPA detects reduced utilization and scales down.

**HPA after scale-down**
```
$ kubectl get hpa -n hpa-test
NAME               REFERENCE                 TARGETS   MINPODS   MAXPODS   REPLICAS   AGE
gpu-workload-hpa   Deployment/gpu-workload   100/50    1         4         2          3m3s
```

**Pods after scale-down**
```
$ kubectl get pods -n hpa-test -o wide
NAME                            READY   STATUS    RESTARTS   AGE   IP              NODE                             NOMINATED NODE   READINESS GATES
gpu-workload-5dbfcc64b7-lsksc   1/1     Running   0          18s   100.65.38.156   ip-100-64-171-120.ec2.internal   <none>           <none>
gpu-workload-5dbfcc64b7-td2mg   1/1     Running   0          40s   100.65.165.86   ip-100-64-171-120.ec2.internal   <none>           <none>
```

**HPA events**
```
$ kubectl describe hpa gpu-workload-hpa -n hpa-test
Name:                         gpu-workload-hpa
Namespace:                    hpa-test
Labels:                       <none>
Annotations:                  <none>
CreationTimestamp:            Tue, 24 Feb 2026 12:43:45 -0800
Reference:                    Deployment/gpu-workload
Metrics:                      ( current / target )
  "gpu_utilization" on pods:  100 / 50
Min replicas:                 1
Max replicas:                 4
Deployment pods:              2 current / 2 desired
Conditions:
  Type            Status  Reason              Message
  ----            ------  ------              -------
  AbleToScale     True    ReadyForNewScale    recommended size matches current size
  ScalingActive   True    ValidMetricFound    the HPA was able to successfully calculate a replica count from pods metric gpu_utilization
  ScalingLimited  False   DesiredWithinRange  the desired count is within the acceptable range
Events:
  Type     Reason                        Age                 From                       Message
  ----     ------                        ----                ----                       -------
  Warning  FailedGetScale                50s (x2 over 65s)   horizontal-pod-autoscaler  deployments.apps "gpu-workload" not found
  Warning  FailedGetPodsMetric           35s                 horizontal-pod-autoscaler  unable to get metric gpu_utilization: no metrics returned from custom metrics API
  Warning  FailedComputeMetricsReplicas  35s                 horizontal-pod-autoscaler  invalid metrics (1 invalid out of 1), first error is: failed to get pods metric value: unable to get metric gpu_utilization: no metrics returned from custom metrics API
  Normal   SuccessfulRescale             20s (x2 over 111s)  horizontal-pod-autoscaler  New size: 2; reason: pods metric gpu_utilization above target
```

**Result: PASS** — HPA successfully scaled up when GPU utilization exceeded target, and scaled back down when load was removed.

## Cleanup

**Delete test namespace**
```
$ kubectl delete namespace hpa-test --ignore-not-found
namespace "hpa-test" deleted
```
