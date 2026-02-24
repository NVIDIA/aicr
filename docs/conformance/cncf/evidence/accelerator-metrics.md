# Accelerator & AI Service Metrics

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-02-24 20:22:24 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

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
prometheus-kube-prometheus-prometheus-0   2/2     Running   0          5d20h
```

**Prometheus service**
```
$ kubectl get svc kube-prometheus-prometheus -n monitoring
NAME                         TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)             AGE
kube-prometheus-prometheus   ClusterIP   172.20.174.169   <none>        9090/TCP,8080/TCP   11d
```

### Prometheus Adapter (Custom Metrics API)

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                 READY   STATUS    RESTARTS        AGE
prometheus-adapter-d9dbc69cb-jxgp9   1/1     Running   1 (5m57s ago)   23h
```

**Prometheus adapter service**
```
$ kubectl get svc prometheus-adapter -n monitoring
NAME                 TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)   AGE
prometheus-adapter   ClusterIP   172.20.192.109   <none>        443/TCP   11d
```

### Grafana

**Grafana pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=grafana
NAME                      READY   STATUS    RESTARTS   AGE
grafana-c4bf56ffd-285sl   3/3     Running   0          5d20h
```

## Accelerator Metrics (DCGM Exporter)

NVIDIA DCGM Exporter exposes per-GPU metrics including utilization, memory usage,
temperature, power draw, and more in Prometheus exposition format.

### DCGM Exporter Health

**DCGM exporter pod**
```
$ kubectl get pods -n gpu-operator -l app=nvidia-dcgm-exporter -o wide
NAME                         READY   STATUS    RESTARTS        AGE     IP               NODE                             NOMINATED NODE   READINESS GATES
nvidia-dcgm-exporter-br2tz   1/1     Running   1 (3m43s ago)   5m51s   100.65.138.241   ip-100-64-171-120.ec2.internal   <none>           <none>
```

**DCGM exporter service**
```
$ kubectl get svc -n gpu-operator -l app=nvidia-dcgm-exporter
NAME                   TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)    AGE
nvidia-dcgm-exporter   ClusterIP   172.20.144.227   <none>        9400/TCP   6d
```

### DCGM Metrics Endpoint

Query DCGM exporter directly to show raw GPU metrics in Prometheus format.

**Key GPU metrics from DCGM exporter (sampled)**
```
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 26
DCGM_FI_DEV_GPU_TEMP{gpu="1",UUID="GPU-289275cb-a907-ab73-9a95-058ae119f62d",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 28
DCGM_FI_DEV_GPU_TEMP{gpu="2",UUID="GPU-f814846a-9bbe-469e-97c3-d037d67c3c32",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 27
DCGM_FI_DEV_GPU_TEMP{gpu="3",UUID="GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 29
DCGM_FI_DEV_GPU_TEMP{gpu="4",UUID="GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 29
DCGM_FI_DEV_GPU_TEMP{gpu="5",UUID="GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 27
DCGM_FI_DEV_GPU_TEMP{gpu="6",UUID="GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 29
DCGM_FI_DEV_GPU_TEMP{gpu="7",UUID="GPU-b60b817a-a091-c492-4211-92b276d697e6",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 28
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.351000
DCGM_FI_DEV_POWER_USAGE{gpu="1",UUID="GPU-289275cb-a907-ab73-9a95-058ae119f62d",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.746000
DCGM_FI_DEV_POWER_USAGE{gpu="2",UUID="GPU-f814846a-9bbe-469e-97c3-d037d67c3c32",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 66.834000
DCGM_FI_DEV_POWER_USAGE{gpu="3",UUID="GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 69.771000
DCGM_FI_DEV_POWER_USAGE{gpu="4",UUID="GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 66.401000
DCGM_FI_DEV_POWER_USAGE{gpu="5",UUID="GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 66.619000
DCGM_FI_DEV_POWER_USAGE{gpu="6",UUID="GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 68.529000
DCGM_FI_DEV_POWER_USAGE{gpu="7",UUID="GPU-b60b817a-a091-c492-4211-92b276d697e6",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 69.468000
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-289275cb-a907-ab73-9a95-058ae119f62d",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-f814846a-9bbe-469e-97c3-d037d67c3c32",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="3",UUID="GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="4",UUID="GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="5",UUID="GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="6",UUID="GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-171-120.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.215,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.215,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.215,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.215,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.215,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.215,
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
          "gpu": "6",
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.215,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.215,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.661,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.661,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.661,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.661,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.661,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.661,
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
          "gpu": "6",
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.661,
          "0"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964566.661,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.081,
          "27"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.081,
          "28"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.081,
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.081,
          "29"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.081,
          "29"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.081,
          "27"
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
          "gpu": "6",
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.081,
          "29"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.081,
          "28"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.42,
          "67.351"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.42,
          "67.746"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.42,
          "66.834"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.42,
          "69.771"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.42,
          "66.401"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.42,
          "66.619"
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
          "gpu": "6",
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.42,
          "68.529"
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
          "instance": "100.65.138.241:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-br2tz",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1771964567.42,
          "69.468"
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
namespaces/gpu_power_usage
pods/gpu_utilization
namespaces/gpu_utilization
namespaces/gpu_memory_used
pods/gpu_memory_used
pods/gpu_power_usage
```

**Result: PASS** — DCGM exporter provides per-GPU metrics (utilization, memory, temperature, power). Prometheus actively scrapes and stores metrics. Custom metrics API available via prometheus-adapter.
