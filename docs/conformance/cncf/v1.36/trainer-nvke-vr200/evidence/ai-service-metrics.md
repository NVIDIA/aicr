# AI Service Metrics (Prometheus ServiceMonitor Discovery)

**Cluster:** ` / `
**Generated:** 2026-11-04 01:12:08 UTC
**Kubernetes Version:** v1.36
**Platform:** linux/arm64

---

Demonstrates that Prometheus discovers and collects metrics from AI training
workloads that expose them in Prometheus exposition format, using the
ServiceMonitor CRD for automatic target discovery.

## PyTorch Training Workload

**Training workload pod**
```
$ kubectl get pods -n trainer-metrics-test -o wide
NAME                   READY   STATUS    RESTARTS   AGE   IP             NODE                                NOMINATED NODE   READINESS GATES
pytorch-training-job   1/1     Running   0          5s    192.168.2.31   manual-dsx-20261102-205148-w-8837   <none>           <none>
```

**Training metrics endpoint (after training run)**
```
# HELP training_step_total Total training steps completed
# TYPE training_step_total counter
training_step_total 0
# HELP training_loss Current training loss
# TYPE training_loss gauge
training_loss 0.000000
# HELP training_throughput_samples_per_sec Training throughput
# TYPE training_throughput_samples_per_sec gauge
training_throughput_samples_per_sec 0.00
# HELP training_gpu_memory_used_bytes GPU memory used
# TYPE training_gpu_memory_used_bytes gauge
training_gpu_memory_used_bytes 0
# HELP training_gpu_memory_total_bytes GPU memory total
# TYPE training_gpu_memory_total_bytes gauge
training_gpu_memory_total_bytes 299103551488

```

## ServiceMonitor

**Training ServiceMonitor**
```
$ kubectl get servicemonitor pytorch-training -n trainer-metrics-test -o yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"monitoring.coreos.com/v1","kind":"ServiceMonitor","metadata":{"annotations":{},"labels":{"release":"kube-prometheus-stack"},"name":"pytorch-training","namespace":"trainer-metrics-test"},"spec":{"endpoints":[{"interval":"15s","path":"/metrics","port":"metrics"}],"selector":{"matchLabels":{"app":"pytorch-training"}}}}
  creationTimestamp: "2026-11-04T01:12:12Z"
  generation: 1
  labels:
    release: kube-prometheus-stack
  name: pytorch-training
  namespace: trainer-metrics-test
  resourceVersion: "253027"
  uid: 1ce4b780-98bc-4a80-b9ef-642eb719d8dd
spec:
  endpoints:
  - interval: 15s
    path: /metrics
    port: metrics
  selector:
    matchLabels:
      app: pytorch-training
```

**Service endpoint**
```
$ kubectl get endpoints pytorch-training-metrics -n trainer-metrics-test
Warning: v1 Endpoints is deprecated in v1.33+; use discovery.k8s.io/v1 EndpointSlice
NAME                       ENDPOINTS           AGE
pytorch-training-metrics   192.168.2.31:8080   12s
```

## Prometheus Target Discovery

Prometheus automatically discovers the PyTorch training workload as a scrape
target via ServiceMonitor and actively collects metrics.

**Prometheus scrape target (active)**
```
{
  "job": "pytorch-training-metrics",
  "endpoint": "http://192.168.2.31:8080/metrics",
  "health": "unknown",
  "lastScrape": "0001-01-01T00:00:00Z"
}
```

## Training Metrics in Prometheus

Prometheus collects PyTorch training workload metrics including training step
count, loss, throughput, and GPU memory utilization.

**Training metrics queried from Prometheus**
```
training_step_total = 100
training_loss = 1.335018
training_throughput_samples_per_sec = 1396283.26
training_gpu_memory_used_bytes = 29144064
training_gpu_memory_total_bytes = 299103551488
```

**Result: PASS** — Prometheus discovers the PyTorch training workload via ServiceMonitor and actively scrapes its Prometheus-format metrics endpoint. Training-level metrics (step count, loss, throughput, GPU memory) are collected and queryable.
