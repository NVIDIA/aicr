# Accelerator & AI Service Metrics

**Kubernetes Version:** v1.35
**Platform:** linux/amd64
**Validated on:** Kubernetes v1.35 clusters with NVIDIA H100 80GB HBM3
**Generated:** 2026-03-10 03:41:11 UTC

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
prometheus-kube-prometheus-prometheus-0   2/2     Running   0          18m
```

**Prometheus service**
```
$ kubectl get svc kube-prometheus-prometheus -n monitoring
NAME                         TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)             AGE
kube-prometheus-prometheus   ClusterIP   172.20.135.224   <none>        9090/TCP,8080/TCP   18m
```

### Prometheus Adapter (Custom Metrics API)

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                  READY   STATUS    RESTARTS   AGE
prometheus-adapter-78b8b8d75c-fh4cf   1/1     Running   0          17m
```

**Prometheus adapter service**
```
$ kubectl get svc prometheus-adapter -n monitoring
NAME                 TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)   AGE
prometheus-adapter   ClusterIP   172.20.178.141   <none>        443/TCP   17m
```

### Grafana

**Grafana pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=grafana
NAME                       READY   STATUS    RESTARTS   AGE
grafana-56fbffd7d7-r2htr   3/3     Running   0          18m
```

## Accelerator Metrics (DCGM Exporter)

NVIDIA DCGM Exporter exposes per-GPU metrics including utilization, memory usage,
temperature, power draw, and more in Prometheus exposition format.

### DCGM Exporter Health

**DCGM exporter pod**
```
$ kubectl get pods -n gpu-operator -l app=nvidia-dcgm-exporter -o wide
NAME                         READY   STATUS    RESTARTS   AGE   IP             NODE                           NOMINATED NODE   READINESS GATES
nvidia-dcgm-exporter-g2fjs   1/1     Running   0          15m   10.0.247.52    gpu-node-2     <none>           <none>
nvidia-dcgm-exporter-wqqqn   1/1     Running   0          15m   10.0.172.246   gpu-node-1   <none>           <none>
```

**DCGM exporter service**
```
$ kubectl get svc -n gpu-operator -l app=nvidia-dcgm-exporter
NAME                   TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)    AGE
nvidia-dcgm-exporter   ClusterIP   172.20.181.11   <none>        9400/TCP   15m
```

### DCGM Metrics Endpoint

Query DCGM exporter directly to show raw GPU metrics in Prometheus format.

**Key GPU metrics from DCGM exporter (sampled)**
```
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-c4529c8d-69c4-b61d-e0bc-7b2460096005",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08",container="main",namespace="dynamo-workload",pod="vllm-agg-0-vllmdecodeworker-s65j5",pod_uid=""} 30
DCGM_FI_DEV_GPU_TEMP{gpu="1",UUID="GPU-bc5610b9-79c8-fedd-8899-07539c7f868a",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 29
DCGM_FI_DEV_GPU_TEMP{gpu="2",UUID="GPU-fbc2c554-4d37-8938-0032-f923bad0f716",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 26
DCGM_FI_DEV_GPU_TEMP{gpu="3",UUID="GPU-a65a773d-52bb-bcc1-a8ee-f78c3faa2e2d",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 29
DCGM_FI_DEV_GPU_TEMP{gpu="4",UUID="GPU-82e45d1b-1618-559f-144c-eab51545030b",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 28
DCGM_FI_DEV_GPU_TEMP{gpu="5",UUID="GPU-39e28159-8c62-ee71-64db-b748edd61e15",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 26
DCGM_FI_DEV_GPU_TEMP{gpu="6",UUID="GPU-e64d69ca-b4b3-59b2-e78c-94f26c4db365",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 28
DCGM_FI_DEV_GPU_TEMP{gpu="7",UUID="GPU-04d228d3-3b5a-3534-f5cf-969706647d56",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 26
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-c4529c8d-69c4-b61d-e0bc-7b2460096005",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08",container="main",namespace="dynamo-workload",pod="vllm-agg-0-vllmdecodeworker-s65j5",pod_uid=""} 113.611000
DCGM_FI_DEV_POWER_USAGE{gpu="1",UUID="GPU-bc5610b9-79c8-fedd-8899-07539c7f868a",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 68.347000
DCGM_FI_DEV_POWER_USAGE{gpu="2",UUID="GPU-fbc2c554-4d37-8938-0032-f923bad0f716",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 65.709000
DCGM_FI_DEV_POWER_USAGE{gpu="3",UUID="GPU-a65a773d-52bb-bcc1-a8ee-f78c3faa2e2d",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.316000
DCGM_FI_DEV_POWER_USAGE{gpu="4",UUID="GPU-82e45d1b-1618-559f-144c-eab51545030b",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 68.717000
DCGM_FI_DEV_POWER_USAGE{gpu="5",UUID="GPU-39e28159-8c62-ee71-64db-b748edd61e15",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 65.742000
DCGM_FI_DEV_POWER_USAGE{gpu="6",UUID="GPU-e64d69ca-b4b3-59b2-e78c-94f26c4db365",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.328000
DCGM_FI_DEV_POWER_USAGE{gpu="7",UUID="GPU-04d228d3-3b5a-3534-f5cf-969706647d56",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 66.997000
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-c4529c8d-69c4-b61d-e0bc-7b2460096005",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08",container="main",namespace="dynamo-workload",pod="vllm-agg-0-vllmdecodeworker-s65j5",pod_uid=""} 0
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-bc5610b9-79c8-fedd-8899-07539c7f868a",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-fbc2c554-4d37-8938-0032-f923bad0f716",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="3",UUID="GPU-a65a773d-52bb-bcc1-a8ee-f78c3faa2e2d",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="4",UUID="GPU-82e45d1b-1618-559f-144c-eab51545030b",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="5",UUID="GPU-39e28159-8c62-ee71-64db-b748edd61e15",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="6",UUID="GPU-e64d69ca-b4b3-59b2-e78c-94f26c4db365",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="7",UUID="GPU-04d228d3-3b5a-3534-f5cf-969706647d56",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="0",UUID="GPU-c4529c8d-69c4-b61d-e0bc-7b2460096005",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08",container="main",namespace="dynamo-workload",pod="vllm-agg-0-vllmdecodeworker-s65j5",pod_uid=""} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="1",UUID="GPU-bc5610b9-79c8-fedd-8899-07539c7f868a",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="2",UUID="GPU-fbc2c554-4d37-8938-0032-f923bad0f716",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="3",UUID="GPU-a65a773d-52bb-bcc1-a8ee-f78c3faa2e2d",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="4",UUID="GPU-82e45d1b-1618-559f-144c-eab51545030b",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="5",UUID="GPU-39e28159-8c62-ee71-64db-b748edd61e15",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="gpu-node-1",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
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
          "Hostname": "gpu-node-1",
          "UUID": "GPU-bc5610b9-79c8-fedd-8899-07539c7f868a",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-fbc2c554-4d37-8938-0032-f923bad0f716",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-a65a773d-52bb-bcc1-a8ee-f78c3faa2e2d",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-82e45d1b-1618-559f-144c-eab51545030b",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-39e28159-8c62-ee71-64db-b748edd61e15",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-e64d69ca-b4b3-59b2-e78c-94f26c4db365",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-04d228d3-3b5a-3534-f5cf-969706647d56",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-92da0328-2f33-b563-d577-9d2b9f21f280",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-184dab49-47ce-eeec-2239-3e03fbd4c002",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-dbabb552-a092-0ca9-0580-8d4fe378eb02",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-5342927e-e180-84f1-55ba-257f1cbd3ba4",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-95085215-739e-e7c6-4011-8dbe004af8c3",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-a7b658ad-f23e-cea9-2523-569d521700bf",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-1e9a0e94-769a-b1e6-36f7-9296e286ef90",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-16b2cd36-9dbe-3ee7-0810-07b330e36e04",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-c4529c8d-69c4-b61d-e0bc-7b2460096005",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "main",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-s65j5",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.184,
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
          "Hostname": "gpu-node-1",
          "UUID": "GPU-bc5610b9-79c8-fedd-8899-07539c7f868a",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-fbc2c554-4d37-8938-0032-f923bad0f716",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-a65a773d-52bb-bcc1-a8ee-f78c3faa2e2d",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-82e45d1b-1618-559f-144c-eab51545030b",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-39e28159-8c62-ee71-64db-b748edd61e15",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-e64d69ca-b4b3-59b2-e78c-94f26c4db365",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-04d228d3-3b5a-3534-f5cf-969706647d56",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-92da0328-2f33-b563-d577-9d2b9f21f280",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-184dab49-47ce-eeec-2239-3e03fbd4c002",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-dbabb552-a092-0ca9-0580-8d4fe378eb02",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-5342927e-e180-84f1-55ba-257f1cbd3ba4",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-95085215-739e-e7c6-4011-8dbe004af8c3",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-a7b658ad-f23e-cea9-2523-569d521700bf",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-1e9a0e94-769a-b1e6-36f7-9296e286ef90",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-16b2cd36-9dbe-3ee7-0810-07b330e36e04",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-c4529c8d-69c4-b61d-e0bc-7b2460096005",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "main",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-s65j5",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.444,
          "74166"
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
          "Hostname": "gpu-node-1",
          "UUID": "GPU-bc5610b9-79c8-fedd-8899-07539c7f868a",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "29"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-fbc2c554-4d37-8938-0032-f923bad0f716",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "26"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-a65a773d-52bb-bcc1-a8ee-f78c3faa2e2d",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "29"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-82e45d1b-1618-559f-144c-eab51545030b",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "28"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-39e28159-8c62-ee71-64db-b748edd61e15",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "26"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-e64d69ca-b4b3-59b2-e78c-94f26c4db365",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "28"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-04d228d3-3b5a-3534-f5cf-969706647d56",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "26"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-92da0328-2f33-b563-d577-9d2b9f21f280",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-184dab49-47ce-eeec-2239-3e03fbd4c002",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "29"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-dbabb552-a092-0ca9-0580-8d4fe378eb02",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "28"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-5342927e-e180-84f1-55ba-257f1cbd3ba4",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "29"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-95085215-739e-e7c6-4011-8dbe004af8c3",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "29"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-a7b658ad-f23e-cea9-2523-569d521700bf",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-1e9a0e94-769a-b1e6-36f7-9296e286ef90",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "30"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-16b2cd36-9dbe-3ee7-0810-07b330e36e04",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-c4529c8d-69c4-b61d-e0bc-7b2460096005",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "main",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-s65j5",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.702,
          "30"
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
          "Hostname": "gpu-node-1",
          "UUID": "GPU-bc5610b9-79c8-fedd-8899-07539c7f868a",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "68.347"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-fbc2c554-4d37-8938-0032-f923bad0f716",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "65.709"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-a65a773d-52bb-bcc1-a8ee-f78c3faa2e2d",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "67.316"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-82e45d1b-1618-559f-144c-eab51545030b",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "68.717"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-39e28159-8c62-ee71-64db-b748edd61e15",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "65.742"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-e64d69ca-b4b3-59b2-e78c-94f26c4db365",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "67.328"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-04d228d3-3b5a-3534-f5cf-969706647d56",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-wqqqn",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "66.997"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-92da0328-2f33-b563-d577-9d2b9f21f280",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "69.339"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-184dab49-47ce-eeec-2239-3e03fbd4c002",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "68.754"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-dbabb552-a092-0ca9-0580-8d4fe378eb02",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "68.61"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-5342927e-e180-84f1-55ba-257f1cbd3ba4",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "66.499"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-95085215-739e-e7c6-4011-8dbe004af8c3",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "67.645"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-a7b658ad-f23e-cea9-2523-569d521700bf",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "66.68"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-1e9a0e94-769a-b1e6-36f7-9296e286ef90",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "68.395"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-2",
          "UUID": "GPU-16b2cd36-9dbe-3ee7-0810-07b330e36e04",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.247.52:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-g2fjs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "69.523"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "gpu-node-1",
          "UUID": "GPU-c4529c8d-69c4-b61d-e0bc-7b2460096005",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "main",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.172.246:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-s65j5",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1773114089.943,
          "113.611"
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
$ kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1 | python3 -c "..." # extract resource names
namespaces/gpu_utilization
pods/gpu_utilization
namespaces/gpu_memory_used
pods/gpu_memory_used
namespaces/gpu_power_usage
pods/gpu_power_usage
```

**Result: PASS** — DCGM exporter provides per-GPU metrics (utilization, memory, temperature, power). Prometheus actively scrapes and stores metrics. Custom metrics API available via prometheus-adapter.
