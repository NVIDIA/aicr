# AI Service Metrics (NIM Inference)

**Cluster:** `EKS / p5.48xlarge / NVIDIA-H100-80GB-HBM3`
**Generated:** 2026-04-01 23:15:43 UTC
**Kubernetes Version:** v1.35
**Platform:** linux/amd64

---

Demonstrates that NVIDIA NIM inference microservices expose Prometheus-format
metrics that can be discovered and collected by the monitoring stack.

## NIM Inference Workload

**NIMService**
```
$ kubectl get nimservice -n nim-workload
NAME           STATUS   AGE
llama-3-2-1b   Ready    58m
```

**NIM workload pods**
```
$ kubectl get pods -n nim-workload -o wide
NAME                            READY   STATUS    RESTARTS   AGE   IP            NODE                           NOMINATED NODE   READINESS GATES
llama-3-2-1b-7577f87fc7-dhb97   1/1     Running   0          58m   10.0.158.63   ip-10-0-180-136.ec2.internal   <none>           <none>
```

**NIM models endpoint**
```
Model: meta/llama-3.2-1b-instruct
```

**NIM inference metrics endpoint (sampled after generating inference traffic)**
```
num_requests_waiting{model_name="meta/llama-3.2-1b-instruct"} 1.0
num_request_max{model_name="meta/llama-3.2-1b-instruct"} 2048.0
prompt_tokens_total{model_name="meta/llama-3.2-1b-instruct"} 603.0
generation_tokens_total{model_name="meta/llama-3.2-1b-instruct"} 997.0
time_to_first_token_seconds_count{model_name="meta/llama-3.2-1b-instruct"} 34.0
time_to_first_token_seconds_sum{model_name="meta/llama-3.2-1b-instruct"} 3.781902551651001
time_per_output_token_seconds_count{model_name="meta/llama-3.2-1b-instruct"} 963.0
time_per_output_token_seconds_sum{model_name="meta/llama-3.2-1b-instruct"} 1.705470085144043
e2e_request_latency_seconds_count{model_name="meta/llama-3.2-1b-instruct"} 34.0
e2e_request_latency_seconds_sum{model_name="meta/llama-3.2-1b-instruct"} 5.490677356719971
request_prompt_tokens_count{model_name="meta/llama-3.2-1b-instruct"} 34.0
request_prompt_tokens_sum{model_name="meta/llama-3.2-1b-instruct"} 603.0
request_generation_tokens_count{model_name="meta/llama-3.2-1b-instruct"} 34.0
request_generation_tokens_sum{model_name="meta/llama-3.2-1b-instruct"} 997.0
request_success_total{model_name="meta/llama-3.2-1b-instruct"} 34.0
```

## Prometheus Metrics Discovery

A ServiceMonitor is created to enable Prometheus auto-discovery of NIM inference
metrics. NIM exposes metrics at `/v1/metrics` in Prometheus exposition format.

**NIM ServiceMonitor**
```
$ kubectl get servicemonitor nim-inference -n monitoring -o yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"monitoring.coreos.com/v1","kind":"ServiceMonitor","metadata":{"annotations":{},"labels":{"release":"kube-prometheus"},"name":"nim-inference","namespace":"monitoring"},"spec":{"endpoints":[{"interval":"15s","path":"/v1/metrics","port":"api"}],"namespaceSelector":{"matchNames":["nim-workload"]},"selector":{"matchLabels":{"app.kubernetes.io/managed-by":"k8s-nim-operator"}}}}
  creationTimestamp: "2026-04-01T23:16:15Z"
  generation: 1
  labels:
    release: kube-prometheus
  name: nim-inference
  namespace: monitoring
  resourceVersion: "102073064"
  uid: e29b3536-c76d-410c-a236-a3ac5d745822
spec:
  endpoints:
  - interval: 15s
    path: /v1/metrics
    port: api
  namespaceSelector:
    matchNames:
    - nim-workload
  selector:
    matchLabels:
      app.kubernetes.io/managed-by: k8s-nim-operator
```

**Prometheus scrape targets (active)**
```
{
  "job": "llama-3-2-1b",
  "endpoint": "http://10.0.158.63:8000/v1/metrics",
  "health": "up",
  "lastScrape": "2026-04-01T23:18:42.378844773Z"
}
```

**NIM metrics queried from Prometheus**
```
prompt_tokens_total{model_name="meta/llama-3.2-1b-instruct"} = 603
generation_tokens_total{model_name="meta/llama-3.2-1b-instruct"} = 997
time_to_first_token_seconds_sum{model_name="meta/llama-3.2-1b-instruct"} = 3.781902551651001
time_per_output_token_seconds_sum{model_name="meta/llama-3.2-1b-instruct"} = 1.705470085144043
e2e_request_latency_seconds_sum{model_name="meta/llama-3.2-1b-instruct"} = 5.490677356719971
```

**Result: PASS** — Prometheus discovers NIM inference workloads via ServiceMonitor and actively scrapes application-level AI inference metrics (token throughput, request latency, time-to-first-token) from the /v1/metrics endpoint.

## Cleanup

**Delete workload namespace**
```
$ kubectl delete ns nim-workload
```
