# Accelerator & AI Service Metrics

**Generated:** 2026-02-19 19:30:44 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

## Summary

1. **Monitoring Stack** — Prometheus, Grafana, prometheus-adapter all running healthy
2. **DCGM Exporter** — Running on GPU node, exposing per-GPU metrics at `:9400/metrics` in Prometheus format
3. **GPU Metrics Available** — Temperature (26-31C), power draw (66-115W), utilization, memory copy util for all 8x H100 GPUs
4. **Per-Workload Attribution** — GPU 6 metrics include pod/namespace/container labels for `vllm-agg-0-vllmdecodeworker` workload
5. **Prometheus Scraping** — Prometheus actively scraping DCGM exporter via ServiceMonitor, all queries return per-GPU data
6. **Custom Metrics API** — prometheus-adapter exposes `gpu_utilization`, `gpu_memory_used`, `gpu_power_usage` via Kubernetes custom metrics API for HPA
7. **Result: PASS**

---

Demonstrates two CNCF AI Conformance observability requirements:

1. **accelerator_metrics** — Fine-grained GPU performance metrics (utilization, memory,
   temperature, power) exposed via standardized Prometheus endpoint
2. **ai_service_metrics** — Monitoring system that discovers and collects metrics from
   workloads exposing Prometheus exposition format

## Monitoring Stack Health

### Prometheus

**Prometheus pods**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus
NAME                                      READY   STATUS    RESTARTS   AGE
prometheus-kube-prometheus-prometheus-0   2/2     Running   0          20h
```

**Prometheus service**
```
$ kubectl get svc kube-prometheus-prometheus -n monitoring
NAME                         TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)             AGE
kube-prometheus-prometheus   ClusterIP   172.20.174.169   <none>        9090/TCP,8080/TCP   6d22h
```

### Prometheus Adapter (Custom Metrics API)

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                 READY   STATUS    RESTARTS   AGE
prometheus-adapter-658b9f4fc-7sdfx   1/1     Running   0          19h
```

**Prometheus adapter service**
```
$ kubectl get svc prometheus-adapter -n monitoring
NAME                 TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)   AGE
prometheus-adapter   ClusterIP   172.20.192.109   <none>        443/TCP   6d22h
```

### Grafana

**Grafana pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=grafana
NAME                      READY   STATUS    RESTARTS   AGE
grafana-c4bf56ffd-285sl   3/3     Running   0          20h
```

## Accelerator Metrics (DCGM Exporter)

NVIDIA DCGM Exporter exposes per-GPU metrics including utilization, memory usage,
temperature, power draw, and more in Prometheus exposition format.

### DCGM Exporter Health

**DCGM exporter pod**
```
$ kubectl get pods -n gpu-operator -l app=nvidia-dcgm-exporter -o wide
NAME                         READY   STATUS    RESTARTS      AGE   IP             NODE                             NOMINATED NODE   READINESS GATES
nvidia-dcgm-exporter-hblfm   1/1     Running   2 (34m ago)   36m   100.65.85.64   ip-100-64-171-120.ec2.internal   <none>           <none>
```

**DCGM exporter service**
```
$ kubectl get svc -n gpu-operator -l app=nvidia-dcgm-exporter
NAME                   TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)    AGE
nvidia-dcgm-exporter   ClusterIP   172.20.144.227   <none>        9400/TCP   23h
```

### DCGM Metrics Endpoint

Query DCGM exporter directly to show raw GPU metrics in Prometheus format.

**Key GPU metrics from DCGM exporter (sampled)**
```
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 26
DCGM_FI_DEV_GPU_TEMP{gpu="1",UUID="GPU-289275cb-a907-ab73-9a95-058ae119f62d",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 27
DCGM_FI_DEV_GPU_TEMP{gpu="2",UUID="GPU-f814846a-9bbe-469e-97c3-d037d67c3c32",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 27
DCGM_FI_DEV_GPU_TEMP{gpu="3",UUID="GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 28
DCGM_FI_DEV_GPU_TEMP{gpu="4",UUID="GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 28
DCGM_FI_DEV_GPU_TEMP{gpu="5",UUID="GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 26
DCGM_FI_DEV_GPU_TEMP{gpu="6",UUID="GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08",container="main",namespace="dynamo-workload",pod="vllm-agg-0-vllmdecodeworker-5fljt"} 31
DCGM_FI_DEV_GPU_TEMP{gpu="7",UUID="GPU-b60b817a-a091-c492-4211-92b276d697e6",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 27
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.268000
DCGM_FI_DEV_POWER_USAGE{gpu="1",UUID="GPU-289275cb-a907-ab73-9a95-058ae119f62d",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.616000
DCGM_FI_DEV_POWER_USAGE{gpu="2",UUID="GPU-f814846a-9bbe-469e-97c3-d037d67c3c32",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 66.477000
DCGM_FI_DEV_POWER_USAGE{gpu="3",UUID="GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 69.523000
DCGM_FI_DEV_POWER_USAGE{gpu="4",UUID="GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 66.297000
DCGM_FI_DEV_POWER_USAGE{gpu="5",UUID="GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 66.324000
DCGM_FI_DEV_POWER_USAGE{gpu="6",UUID="GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08",container="main",namespace="dynamo-workload",pod="vllm-agg-0-vllmdecodeworker-5fljt"} 115.220000
DCGM_FI_DEV_POWER_USAGE{gpu="7",UUID="GPU-b60b817a-a091-c492-4211-92b276d697e6",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 69.424000
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-289275cb-a907-ab73-9a95-058ae119f62d",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-f814846a-9bbe-469e-97c3-d037d67c3c32",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="3",UUID="GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="4",UUID="GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="5",UUID="GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="6",UUID="GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08",container="main",namespace="dynamo-workload",pod="vllm-agg-0-vllmdecodeworker-5fljt"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="7",UUID="GPU-b60b817a-a091-c492-4211-92b276d697e6",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="0",UUID="GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="1",UUID="GPU-289275cb-a907-ab73-9a95-058ae119f62d",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="2",UUID="GPU-f814846a-9bbe-469e-97c3-d037d67c3c32",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="3",UUID="GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="4",UUID="GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="5",UUID="GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
```

### Prometheus Querying GPU Metrics

Query Prometheus to verify it is actively scraping and storing DCGM metrics.

**GPU Utilization (DCGM_FI_DEV_GPU_UTIL)**
```
{
  "status": "success",
  "data": {
    "resultType": "vector",
    "result": [
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.57,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-289275cb-a907-ab73-9a95-058ae119f62d",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.57,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-f814846a-9bbe-469e-97c3-d037d67c3c32",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.57,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.57,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.57,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.57,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "exported_container": "main",
          "exported_namespace": "dynamo-workload",
          "exported_pod": "vllm-agg-0-vllmdecodeworker-5fljt",
          "gpu": "6",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.57,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-b60b817a-a091-c492-4211-92b276d697e6",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.57,
          "0"
        ]
      }
    ]
  }
}
```

**GPU Memory Used (DCGM_FI_DEV_FB_USED)**
```
{
  "status": "success",
  "data": {
    "resultType": "vector",
    "result": [
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.895,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-289275cb-a907-ab73-9a95-058ae119f62d",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.895,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-f814846a-9bbe-469e-97c3-d037d67c3c32",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.895,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.895,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.895,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.895,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "exported_container": "main",
          "exported_namespace": "dynamo-workload",
          "exported_pod": "vllm-agg-0-vllmdecodeworker-5fljt",
          "gpu": "6",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.895,
          "74198"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-b60b817a-a091-c492-4211-92b276d697e6",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529464.895,
          "0"
        ]
      }
    ]
  }
}
```

**GPU Temperature (DCGM_FI_DEV_GPU_TEMP)**
```
{
  "status": "success",
  "data": {
    "resultType": "vector",
    "result": [
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.206,
          "26"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-289275cb-a907-ab73-9a95-058ae119f62d",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.206,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-f814846a-9bbe-469e-97c3-d037d67c3c32",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.206,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.206,
          "28"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.206,
          "28"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.206,
          "26"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "exported_container": "main",
          "exported_namespace": "dynamo-workload",
          "exported_pod": "vllm-agg-0-vllmdecodeworker-5fljt",
          "gpu": "6",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.206,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-b60b817a-a091-c492-4211-92b276d697e6",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.206,
          "27"
        ]
      }
    ]
  }
}
```

**GPU Power Draw (DCGM_FI_DEV_POWER_USAGE)**
```
{
  "status": "success",
  "data": {
    "resultType": "vector",
    "result": [
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.485,
          "67.241"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-289275cb-a907-ab73-9a95-058ae119f62d",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.485,
          "67.576"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-f814846a-9bbe-469e-97c3-d037d67c3c32",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.485,
          "66.504"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.485,
          "69.557"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.485,
          "66.273"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.485,
          "66.488"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "exported_container": "main",
          "exported_namespace": "dynamo-workload",
          "exported_pod": "vllm-agg-0-vllmdecodeworker-5fljt",
          "gpu": "6",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.485,
          "115.215"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-171-120.ec2.internal",
          "UUID": "GPU-b60b817a-a091-c492-4211-92b276d697e6",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "100.65.85.64:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-hblfm",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771529465.485,
          "69.376"
        ]
      }
    ]
  }
}
```

## AI Service Metrics (Custom Metrics API)

Prometheus adapter exposes custom metrics via the Kubernetes custom metrics API,
enabling HPA and other consumers to act on workload-specific metrics.

**Custom metrics API available resources**
```
$ kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1 | jq .resources[].name
namespaces/gpu_utilization
namespaces/gpu_memory_used
pods/gpu_memory_used
pods/gpu_power_usage
namespaces/gpu_power_usage
pods/gpu_utilization
```

**Result: PASS** — DCGM exporter provides per-GPU metrics (utilization, memory, temperature, power). Prometheus actively scrapes and stores metrics. Custom metrics API available via prometheus-adapter.
