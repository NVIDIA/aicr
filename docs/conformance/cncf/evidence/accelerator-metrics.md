# Accelerator & AI Service Metrics

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-03-02 18:29:45 UTC
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
prometheus-kube-prometheus-prometheus-0   2/2     Running   0          47h
```

**Prometheus service**
```
$ kubectl get svc kube-prometheus-prometheus -n monitoring
NAME                         TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)             AGE
kube-prometheus-prometheus   ClusterIP   172.20.206.75   <none>        9090/TCP,8080/TCP   2d
```

### Prometheus Adapter (Custom Metrics API)

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                  READY   STATUS    RESTARTS   AGE
prometheus-adapter-585f5dfc99-nm42j   1/1     Running   0          47h
```

**Prometheus adapter service**
```
$ kubectl get svc prometheus-adapter -n monitoring
NAME                 TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)   AGE
prometheus-adapter   ClusterIP   172.20.42.196   <none>        443/TCP   2d
```

### Grafana

**Grafana pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=grafana
NAME                       READY   STATUS    RESTARTS   AGE
grafana-6494c6659c-l9jsk   3/3     Running   0          47h
```

## Accelerator Metrics (DCGM Exporter)

NVIDIA DCGM Exporter exposes per-GPU metrics including utilization, memory usage,
temperature, power draw, and more in Prometheus exposition format.

### DCGM Exporter Health

**DCGM exporter pod**
```
$ kubectl get pods -n gpu-operator -l app=nvidia-dcgm-exporter -o wide
NAME                         READY   STATUS    RESTARTS   AGE   IP               NODE                             NOMINATED NODE   READINESS GATES
nvidia-dcgm-exporter-9v4gs   1/1     Running   0          2d    100.65.218.207   ip-100-64-147-149.ec2.internal   <none>           <none>
```

**DCGM exporter service**
```
$ kubectl get svc -n gpu-operator -l app=nvidia-dcgm-exporter
NAME                   TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)    AGE
nvidia-dcgm-exporter   ClusterIP   172.20.145.184   <none>        9400/TCP   2d
```

### DCGM Metrics Endpoint

Query DCGM exporter directly to show raw GPU metrics in Prometheus format.

**Key GPU metrics from DCGM exporter (sampled)**
```
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-81d79b08-40a0-40ae-3fc5-82b8ff8b8138",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 27
DCGM_FI_DEV_GPU_TEMP{gpu="1",UUID="GPU-4fc48812-c1c8-3bb7-1313-724533aa7df7",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 28
DCGM_FI_DEV_GPU_TEMP{gpu="2",UUID="GPU-8d76cfcf-a144-5e43-876b-a4b71f2aecd1",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08",container="main",namespace="dynamo-workload",pod="vllm-agg-0-vllmdecodeworker-dkb9q",pod_uid=""} 29
DCGM_FI_DEV_GPU_TEMP{gpu="3",UUID="GPU-e69a4117-e5f9-04a7-d170-aafac6a7e692",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 29
DCGM_FI_DEV_GPU_TEMP{gpu="4",UUID="GPU-eaef2c36-d7aa-5f60-37bc-3e0a53de34ff",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 28
DCGM_FI_DEV_GPU_TEMP{gpu="5",UUID="GPU-1af5cfae-1878-a06c-5dc0-c16e5cf11a20",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 26
DCGM_FI_DEV_GPU_TEMP{gpu="6",UUID="GPU-a0e6d978-c416-5df8-1ab9-eb27b31eab35",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 28
DCGM_FI_DEV_GPU_TEMP{gpu="7",UUID="GPU-bd2999a7-7982-6643-fa9e-2d1a2cd7be27",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 27
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-81d79b08-40a0-40ae-3fc5-82b8ff8b8138",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.433000
DCGM_FI_DEV_POWER_USAGE{gpu="1",UUID="GPU-4fc48812-c1c8-3bb7-1313-724533aa7df7",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 70.849000
DCGM_FI_DEV_POWER_USAGE{gpu="2",UUID="GPU-8d76cfcf-a144-5e43-876b-a4b71f2aecd1",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08",container="main",namespace="dynamo-workload",pod="vllm-agg-0-vllmdecodeworker-dkb9q",pod_uid=""} 110.786000
DCGM_FI_DEV_POWER_USAGE{gpu="3",UUID="GPU-e69a4117-e5f9-04a7-d170-aafac6a7e692",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.933000
DCGM_FI_DEV_POWER_USAGE{gpu="4",UUID="GPU-eaef2c36-d7aa-5f60-37bc-3e0a53de34ff",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 69.140000
DCGM_FI_DEV_POWER_USAGE{gpu="5",UUID="GPU-1af5cfae-1878-a06c-5dc0-c16e5cf11a20",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.033000
DCGM_FI_DEV_POWER_USAGE{gpu="6",UUID="GPU-a0e6d978-c416-5df8-1ab9-eb27b31eab35",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.156000
DCGM_FI_DEV_POWER_USAGE{gpu="7",UUID="GPU-bd2999a7-7982-6643-fa9e-2d1a2cd7be27",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 68.495000
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-81d79b08-40a0-40ae-3fc5-82b8ff8b8138",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-4fc48812-c1c8-3bb7-1313-724533aa7df7",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-8d76cfcf-a144-5e43-876b-a4b71f2aecd1",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08",container="main",namespace="dynamo-workload",pod="vllm-agg-0-vllmdecodeworker-dkb9q",pod_uid=""} 0
DCGM_FI_DEV_GPU_UTIL{gpu="3",UUID="GPU-e69a4117-e5f9-04a7-d170-aafac6a7e692",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="4",UUID="GPU-eaef2c36-d7aa-5f60-37bc-3e0a53de34ff",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="5",UUID="GPU-1af5cfae-1878-a06c-5dc0-c16e5cf11a20",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="6",UUID="GPU-a0e6d978-c416-5df8-1ab9-eb27b31eab35",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="7",UUID="GPU-bd2999a7-7982-6643-fa9e-2d1a2cd7be27",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="0",UUID="GPU-81d79b08-40a0-40ae-3fc5-82b8ff8b8138",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="1",UUID="GPU-4fc48812-c1c8-3bb7-1313-724533aa7df7",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="2",UUID="GPU-8d76cfcf-a144-5e43-876b-a4b71f2aecd1",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08",container="main",namespace="dynamo-workload",pod="vllm-agg-0-vllmdecodeworker-dkb9q",pod_uid=""} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="3",UUID="GPU-e69a4117-e5f9-04a7-d170-aafac6a7e692",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="4",UUID="GPU-eaef2c36-d7aa-5f60-37bc-3e0a53de34ff",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="5",UUID="GPU-1af5cfae-1878-a06c-5dc0-c16e5cf11a20",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-100-64-147-149.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
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
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-81d79b08-40a0-40ae-3fc5-82b8ff8b8138",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476205.739,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-e69a4117-e5f9-04a7-d170-aafac6a7e692",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476205.739,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-eaef2c36-d7aa-5f60-37bc-3e0a53de34ff",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476205.739,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-1af5cfae-1878-a06c-5dc0-c16e5cf11a20",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476205.739,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-a0e6d978-c416-5df8-1ab9-eb27b31eab35",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476205.739,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-bd2999a7-7982-6643-fa9e-2d1a2cd7be27",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476205.739,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-4fc48812-c1c8-3bb7-1313-724533aa7df7",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476205.739,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-8d76cfcf-a144-5e43-876b-a4b71f2aecd1",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "main",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-dkb9q",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476205.739,
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
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-81d79b08-40a0-40ae-3fc5-82b8ff8b8138",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.062,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-e69a4117-e5f9-04a7-d170-aafac6a7e692",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.062,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-eaef2c36-d7aa-5f60-37bc-3e0a53de34ff",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.062,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-1af5cfae-1878-a06c-5dc0-c16e5cf11a20",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.062,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-a0e6d978-c416-5df8-1ab9-eb27b31eab35",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.062,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-bd2999a7-7982-6643-fa9e-2d1a2cd7be27",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.062,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-4fc48812-c1c8-3bb7-1313-724533aa7df7",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.062,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-8d76cfcf-a144-5e43-876b-a4b71f2aecd1",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "main",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-dkb9q",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.062,
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
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-81d79b08-40a0-40ae-3fc5-82b8ff8b8138",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.403,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-e69a4117-e5f9-04a7-d170-aafac6a7e692",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.403,
          "29"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-eaef2c36-d7aa-5f60-37bc-3e0a53de34ff",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.403,
          "28"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-1af5cfae-1878-a06c-5dc0-c16e5cf11a20",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.403,
          "26"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-a0e6d978-c416-5df8-1ab9-eb27b31eab35",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.403,
          "28"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-bd2999a7-7982-6643-fa9e-2d1a2cd7be27",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.403,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-4fc48812-c1c8-3bb7-1313-724533aa7df7",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.403,
          "28"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-8d76cfcf-a144-5e43-876b-a4b71f2aecd1",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "main",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-dkb9q",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.403,
          "29"
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
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-81d79b08-40a0-40ae-3fc5-82b8ff8b8138",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.746,
          "67.433"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-e69a4117-e5f9-04a7-d170-aafac6a7e692",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.746,
          "67.933"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-eaef2c36-d7aa-5f60-37bc-3e0a53de34ff",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.746,
          "69.14"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-1af5cfae-1878-a06c-5dc0-c16e5cf11a20",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.746,
          "67.033"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-a0e6d978-c416-5df8-1ab9-eb27b31eab35",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.746,
          "67.156"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-bd2999a7-7982-6643-fa9e-2d1a2cd7be27",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.746,
          "68.495"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-4fc48812-c1c8-3bb7-1313-724533aa7df7",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-9v4gs",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.746,
          "70.849"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-8d76cfcf-a144-5e43-876b-a4b71f2aecd1",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "main",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.218.207:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-dkb9q",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772476206.746,
          "110.786"
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
pods/gpu_utilization
pods/gpu_memory_used
namespaces/gpu_memory_used
namespaces/gpu_power_usage
pods/gpu_power_usage
```

**Result: PASS** — DCGM exporter provides per-GPU metrics (utilization, memory, temperature, power). Prometheus actively scrapes and stores metrics. Custom metrics API available via prometheus-adapter.
