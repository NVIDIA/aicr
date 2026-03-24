# AI Service Metrics (Prometheus ServiceMonitor Discovery)

**Cluster:** `EKS / p5.48xlarge / NVIDIA-H100-80GB-HBM3`
**Generated:** 2026-03-24 14:06:00 UTC
**Kubernetes Version:** v1.35
**Platform:** linux/amd64

---

Demonstrates that Prometheus discovers and collects metrics from AI workloads
that expose them in Prometheus exposition format, using the ServiceMonitor CRD
for automatic target discovery.

## vLLM Inference Workload

A vLLM inference server (serving Qwen/Qwen3-0.6B on GPU via DRA ResourceClaim)
exposes application-level metrics in Prometheus format at `:8000/metrics`.
A ServiceMonitor enables Prometheus to automatically discover and scrape the endpoint.

**vLLM workload pod**
```
$ kubectl get pods -n vllm-metrics-test -o wide
NAME          READY   STATUS    RESTARTS   AGE
vllm-server   1/1     Running   0          5m
```

**vLLM metrics endpoint (sampled after 10 inference requests)**
```
$ kubectl exec -n vllm-metrics-test vllm-server -- python3 -c "..." | grep vllm:
vllm:request_success_total{engine="0",finished_reason="length",model_name="Qwen/Qwen3-0.6B"} 10.0
vllm:prompt_tokens_total{engine="0",model_name="Qwen/Qwen3-0.6B"} 80.0
vllm:generation_tokens_total{engine="0",model_name="Qwen/Qwen3-0.6B"} 500.0
vllm:time_to_first_token_seconds_count{engine="0",model_name="Qwen/Qwen3-0.6B"} 10.0
vllm:time_to_first_token_seconds_sum{engine="0",model_name="Qwen/Qwen3-0.6B"} 0.205
vllm:inter_token_latency_seconds_count{engine="0",model_name="Qwen/Qwen3-0.6B"} 490.0
vllm:inter_token_latency_seconds_sum{engine="0",model_name="Qwen/Qwen3-0.6B"} 0.864
vllm:e2e_request_latency_seconds_count{engine="0",model_name="Qwen/Qwen3-0.6B"} 10.0
vllm:kv_cache_usage_perc{engine="0",model_name="Qwen/Qwen3-0.6B"} 0.0
vllm:prefix_cache_queries_total{engine="0",model_name="Qwen/Qwen3-0.6B"} 80.0
vllm:num_requests_running{engine="0",model_name="Qwen/Qwen3-0.6B"} 0.0
vllm:num_requests_waiting{engine="0",model_name="Qwen/Qwen3-0.6B"} 0.0
```

## ServiceMonitor

**ServiceMonitor for vLLM**
```
$ kubectl get servicemonitor vllm-inference -n vllm-metrics-test -o yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  labels:
    release: prometheus
  name: vllm-inference
  namespace: vllm-metrics-test
spec:
  endpoints:
  - interval: 15s
    path: /metrics
    port: http
  selector:
    matchLabels:
      app: vllm-inference
```

**Service endpoint**
```
$ kubectl get endpoints vllm-inference -n vllm-metrics-test
NAME             ENDPOINTS          AGE
vllm-inference   10.0.170.78:8000   5m
```

## Prometheus Target Discovery

Prometheus automatically discovers the vLLM workload as a scrape target via
the ServiceMonitor and actively collects metrics.

**Prometheus scrape target (active)**
```
$ kubectl exec -n monitoring prometheus-kube-prometheus-prometheus-0 -- \
    wget -qO- 'http://localhost:9090/api/v1/targets?state=active' | \
    jq '.data.activeTargets[] | select(.labels.job=="vllm-inference")'
{
  "job": "vllm-inference",
  "endpoint": "http://10.0.170.78:8000/metrics",
  "health": "up",
  "lastScrape": "2026-03-24T14:06:50.899967845Z"
}
```

## vLLM Metrics in Prometheus

Prometheus collects vLLM application-level inference metrics including request
throughput, token counts, latency distributions, and KV cache utilization.

**vLLM metrics queried from Prometheus (after 10 inference requests)**
```
$ kubectl exec -n monitoring prometheus-kube-prometheus-prometheus-0 -- \
    wget -qO- 'http://localhost:9090/api/v1/query?query={job="vllm-inference",__name__=~"vllm:.*"}'
vllm:request_success_total{model_name="Qwen/Qwen3-0.6B"} 10
vllm:prompt_tokens_total{model_name="Qwen/Qwen3-0.6B"} 80
vllm:generation_tokens_total{model_name="Qwen/Qwen3-0.6B"} 500
vllm:time_to_first_token_seconds_count{model_name="Qwen/Qwen3-0.6B"} 10
vllm:time_to_first_token_seconds_sum{model_name="Qwen/Qwen3-0.6B"} 0.205
vllm:inter_token_latency_seconds_count{model_name="Qwen/Qwen3-0.6B"} 490
vllm:inter_token_latency_seconds_sum{model_name="Qwen/Qwen3-0.6B"} 0.864
vllm:prefix_cache_queries_total{model_name="Qwen/Qwen3-0.6B"} 80
vllm:iteration_tokens_total_sum{model_name="Qwen/Qwen3-0.6B"} 580
```

**Result: PASS** — Prometheus discovers the vLLM inference workload via ServiceMonitor and actively scrapes its Prometheus-format metrics endpoint. Application-level AI inference metrics (request success count, prompt/generation token throughput, time-to-first-token latency, inter-token latency, KV cache usage, prefix cache queries) are collected and queryable in Prometheus.

## Cleanup

**Delete test namespace**
```
$ kubectl delete ns vllm-metrics-test
```
