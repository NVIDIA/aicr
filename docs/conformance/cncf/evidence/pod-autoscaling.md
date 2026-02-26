# Pod Autoscaling (HPA with GPU Metrics)

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-02-23 21:28:16 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates CNCF AI Conformance requirement that HPA functions correctly for pods
utilizing accelerators, including the ability to scale based on custom GPU metrics.

## Summary

1. **Prometheus Adapter** — Exposes GPU metrics via Kubernetes custom metrics API
2. **Custom Metrics API** — `gpu_utilization`, `gpu_memory_used`, `gpu_power_usage` available
3. **GPU Stress Workload** — Deployment running gpu-burn to generate GPU load
4. **HPA Configuration** — Targets `gpu_utilization` with threshold of 50%
5. **HPA Scaling** — Successfully reads GPU metrics and scales replicas when utilization exceeds target
6. **Result: PASS**

---

## Prometheus Adapter

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                 READY   STATUS    RESTARTS   AGE
prometheus-adapter-d9dbc69cb-jxgp9   1/1     Running   0          36m
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
namespaces/gpu_power_usage
pods/gpu_power_usage
namespaces/gpu_utilization
pods/gpu_utilization
namespaces/gpu_memory_used
pods/gpu_memory_used
```

## GPU Stress Test Deployment

Deploy a GPU workload running gpu-burn to generate sustained GPU utilization,
then create an HPA targeting `gpu_utilization` to demonstrate autoscaling.

**Test manifest:** `pkg/evidence/scripts/manifests/hpa-gpu-test.yaml`

**Apply test manifest**
```
$ kubectl apply -f pkg/evidence/scripts/manifests/hpa-gpu-test.yaml
namespace/hpa-test created
deployment.apps/gpu-workload created
horizontalpodautoscaler.autoscaling/gpu-workload-hpa created
```

**GPU workload pod**
```
$ kubectl get pods -n hpa-test -o wide
NAME                           READY   STATUS    RESTARTS   AGE   IP              NODE                             NOMINATED NODE   READINESS GATES
gpu-workload-7d7f4dbdf-sfkgc   1/1     Running   0          14s   100.65.111.44   ip-100-64-171-120.ec2.internal   <none>           <none>
```

## HPA Status

**HPA status**
```
$ kubectl get hpa -n hpa-test
NAME               REFERENCE                 TARGETS   MINPODS   MAXPODS   REPLICAS   AGE
gpu-workload-hpa   Deployment/gpu-workload   100/50    1         4         2          55s
```

**HPA details**
```
$ kubectl describe hpa gpu-workload-hpa -n hpa-test
Name:                         gpu-workload-hpa
Namespace:                    hpa-test
Labels:                       <none>
Annotations:                  <none>
CreationTimestamp:            Mon, 23 Feb 2026 13:28:28 -0800
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
  Type     Reason                        Age   From                       Message
  ----     ------                        ----  ----                       -------
  Warning  FailedGetPodsMetric           41s   horizontal-pod-autoscaler  unable to get metric gpu_utilization: no metrics returned from custom metrics API
  Warning  FailedComputeMetricsReplicas  41s   horizontal-pod-autoscaler  invalid metrics (1 invalid out of 1), first error is: failed to get pods metric value: unable to get metric gpu_utilization: no metrics returned from custom metrics API
  Normal   SuccessfulRescale             26s   horizontal-pod-autoscaler  New size: 2; reason: pods metric gpu_utilization above target
```

## GPU Utilization Evidence

**GPU utilization (nvidia-smi)**
```
$ kubectl exec -n hpa-test gpu-workload-7d7f4dbdf-6vzgr -- nvidia-smi --query-gpu=utilization.gpu,utilization.memory,power.draw --format=csv
utilization.gpu [%], utilization.memory [%], power.draw [W]
100 %, 0 %, 557.27 W
```

## Pods After Scaling

**Pods**
```
$ kubectl get pods -n hpa-test -o wide
NAME                           READY   STATUS              RESTARTS   AGE   IP               NODE                             NOMINATED NODE   READINESS GATES
gpu-workload-7d7f4dbdf-2x6ll   0/1     ContainerCreating   0          1s    <none>           ip-100-64-171-120.ec2.internal   <none>           <none>
gpu-workload-7d7f4dbdf-6vzgr   1/1     Running             0          31s   100.65.124.107   ip-100-64-171-120.ec2.internal   <none>           <none>
gpu-workload-7d7f4dbdf-hl2rj   0/1     ContainerCreating   0          1s    <none>           ip-100-64-171-120.ec2.internal   <none>           <none>
gpu-workload-7d7f4dbdf-sfkgc   1/1     Running             0          61s   100.65.111.44    ip-100-64-171-120.ec2.internal   <none>           <none>
```

**Result: PASS** — HPA successfully read gpu_utilization metric and scaled replicas when utilization exceeded target threshold.

## Cleanup

**Delete test namespace**
```
$ kubectl delete namespace hpa-test --ignore-not-found
namespace "hpa-test" deleted
```
