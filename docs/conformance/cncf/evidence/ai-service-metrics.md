# AI Service Metrics (Prometheus Discovery)

**Kubernetes Version:** v1.35
**Platform:** linux/amd64
**Validated on:** EKS / p5.48xlarge / NVIDIA H100 80GB HBM3

---

Demonstrates that Prometheus discovers and collects metrics from AI workloads
that expose them in Prometheus exposition format, using PodMonitor and
ServiceMonitor CRDs for automatic target discovery across both inference and
training workloads.

## Inference: Dynamo Platform (PodMonitor)

**Cluster:** `aicr-cuj2` (EKS, inference)
**Generated:** 2026-03-25 10:18:30 UTC

The Dynamo operator auto-creates PodMonitors for worker and frontend pods.
The Dynamo vLLM runtime exposes both Dynamo-specific and embedded vLLM metrics
on port 9090 (`system` port) in Prometheus format.

### Dynamo Workload Pods

**Dynamo workload pods**
```
$ kubectl get pods -n dynamo-workload -o wide
NAME                                READY   STATUS    RESTARTS   AGE     IP             NODE                           NOMINATED NODE   READINESS GATES
vllm-agg-0-frontend-qqrff           1/1     Running   0          3m29s   10.0.159.241   ip-10-0-184-187.ec2.internal   <none>           <none>
vllm-agg-0-vllmdecodeworker-95ths   1/1     Running   0          3m29s   10.0.214.229   ip-10-0-180-136.ec2.internal   <none>           <none>
```

### Worker Metrics Endpoint

**Worker metrics (sampled after 10 inference requests)**
```
dynamo_component_request_bytes_total{dynamo_component="backend",dynamo_endpoint="generate",model="Qwen/Qwen3-0.6B"} 11230
dynamo_component_request_duration_seconds_sum{dynamo_component="backend",dynamo_endpoint="generate",model="Qwen/Qwen3-0.6B"} 0.984
dynamo_component_request_duration_seconds_count{dynamo_component="backend",dynamo_endpoint="generate",model="Qwen/Qwen3-0.6B"} 10
dynamo_component_requests_total{dynamo_component="backend",dynamo_endpoint="generate",model="Qwen/Qwen3-0.6B"} 10
dynamo_component_response_bytes_total{dynamo_component="backend",dynamo_endpoint="generate",model="Qwen/Qwen3-0.6B"} 31826
dynamo_component_uptime_seconds 223.250
vllm:engine_sleep_state{engine="0",model_name="Qwen/Qwen3-0.6B",sleep_state="awake"} 1.0
vllm:prefix_cache_queries_total{engine="0",model_name="Qwen/Qwen3-0.6B"} 50.0
```

### PodMonitors (Auto-Created by Dynamo Operator)

**Dynamo PodMonitors**
```
$ kubectl get podmonitors -n dynamo-system
NAME              AGE
dynamo-frontend   11d
dynamo-planner    11d
dynamo-worker     11d
```

**Worker PodMonitor spec**
```
$ kubectl get podmonitor dynamo-worker -n dynamo-system -o yaml
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: dynamo-worker
  namespace: dynamo-system
spec:
  namespaceSelector:
    any: true
  podMetricsEndpoints:
  - interval: 5s
    path: /metrics
    port: system
  selector:
    matchLabels:
      nvidia.com/dynamo-component-type: worker
      nvidia.com/metrics-enabled: "true"
```

### Prometheus Target Discovery

**Prometheus scrape targets (active)**
```
{
  "job": "dynamo-system/dynamo-frontend",
  "endpoint": "http://10.0.159.241:8000/metrics",
  "health": "up",
  "lastScrape": "2026-03-25T10:19:21.101766071Z"
}
{
  "job": "dynamo-system/dynamo-worker",
  "endpoint": "http://10.0.214.229:9090/metrics",
  "health": "up",
  "lastScrape": "2026-03-25T10:19:22.70334816Z"
}
```

### Dynamo Metrics in Prometheus

**Dynamo metrics queried from Prometheus (after 10 inference requests)**
```
dynamo_component_requests_total{endpoint="generate"} = 10
dynamo_component_request_bytes_total{endpoint="generate"} = 11230
dynamo_component_response_bytes_total{endpoint="generate"} = 31826
dynamo_component_request_duration_seconds_count{endpoint="generate"} = 10
dynamo_component_request_duration_seconds_sum{endpoint="generate"} = 0.984
dynamo_component_uptime_seconds = 223.250
dynamo_frontend_input_sequence_tokens_sum = 50
dynamo_frontend_input_sequence_tokens_count = 10
dynamo_frontend_inter_token_latency_seconds_sum = 0.866
dynamo_frontend_inter_token_latency_seconds_count = 490
dynamo_frontend_model_context_length = 40960
dynamo_frontend_model_total_kv_blocks = 37710
```

**Result: PASS** — Prometheus discovers Dynamo inference workloads (frontend + worker) via operator-managed PodMonitors and actively scrapes their Prometheus-format metrics endpoints. Application-level AI inference metrics (request count, request duration, inter-token latency, token throughput, KV cache utilization) are collected and queryable.

---

## Training: PyTorch Workload (ServiceMonitor)

**Cluster:** `aicr-cuj1` (EKS, training)
**Generated:** 2026-03-25 11:03:00 UTC

A PyTorch training workload runs a GPU training loop and exposes training-level
metrics (step count, loss, throughput, GPU memory) on port 8080 in Prometheus
format, discovered via ServiceMonitor.

### Training Workload Pod

**Training pod**
```
$ kubectl get pods -n trainer-metrics-test -o wide
NAME                   READY   STATUS    RESTARTS   AGE
pytorch-training-job   1/1     Running   0          2m
```

### Training Metrics Endpoint

**Training metrics (after 100 training steps)**
```
# HELP training_step_total Total training steps completed
# TYPE training_step_total counter
training_step_total 100
# HELP training_loss Current training loss
# TYPE training_loss gauge
training_loss 1.334257
# HELP training_throughput_samples_per_sec Training throughput
# TYPE training_throughput_samples_per_sec gauge
training_throughput_samples_per_sec 549228.55
# HELP training_gpu_memory_used_bytes GPU memory used
# TYPE training_gpu_memory_used_bytes gauge
training_gpu_memory_used_bytes 79213568
# HELP training_gpu_memory_total_bytes GPU memory total
# TYPE training_gpu_memory_total_bytes gauge
training_gpu_memory_total_bytes 85017624576
```

### ServiceMonitor

**Training ServiceMonitor**
```
$ kubectl get servicemonitor pytorch-training -n trainer-metrics-test -o yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  labels:
    release: kube-prometheus-stack
  name: pytorch-training
  namespace: trainer-metrics-test
spec:
  endpoints:
  - interval: 15s
    path: /metrics
    port: metrics
  selector:
    matchLabels:
      app: pytorch-training
```

### Prometheus Target Discovery

**Prometheus scrape target (active)**
```
{
  "job": "pytorch-training-metrics",
  "endpoint": "http://10.0.212.201:8080/metrics",
  "health": "up",
  "lastScrape": "2026-03-25T11:03:49.310258779Z"
}
```

### Training Metrics in Prometheus

**Training metrics queried from Prometheus**
```
training_step_total = 100
training_loss = 1.334257
training_throughput_samples_per_sec = 549228.55
training_gpu_memory_used_bytes = 79213568
training_gpu_memory_total_bytes = 85017624576
```

**Result: PASS** — Prometheus discovers the PyTorch training workload via ServiceMonitor and actively scrapes its Prometheus-format metrics endpoint. Training-level metrics (step count, loss, throughput, GPU memory) are collected and queryable.

---

## Summary

| Workload | Discovery | Metrics Port | Metrics Type | Result |
|----------|-----------|-------------|--------------|--------|
| **Dynamo vLLM** (inference) | PodMonitor (auto-created) | 9090 (HTTP) | `dynamo_component_*`, `dynamo_frontend_*`, `vllm:*` | **PASS** |
| **PyTorch training** (training) | ServiceMonitor | 8080 (HTTP) | `training_step_total`, `training_loss`, `training_throughput_*`, `training_gpu_memory_*` | **PASS** |

## Cleanup

**Delete inference workload**
```
$ kubectl delete ns dynamo-workload
```

**Delete training workload**
```
$ kubectl delete ns trainer-metrics-test
```
