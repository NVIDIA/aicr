# Accelerator & AI Service Metrics

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-03-06 19:38:56 UTC
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
kube-prometheus-prometheus   ClusterIP   172.20.243.81   <none>        9090/TCP,8080/TCP   47h
```

### Prometheus Adapter (Custom Metrics API)

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                  READY   STATUS    RESTARTS   AGE
prometheus-adapter-585f5dfc99-cwwdj   1/1     Running   0          47h
```

**Prometheus adapter service**
```
$ kubectl get svc prometheus-adapter -n monitoring
NAME                 TYPE        CLUSTER-IP     EXTERNAL-IP   PORT(S)   AGE
prometheus-adapter   ClusterIP   172.20.140.0   <none>        443/TCP   47h
```

### Grafana

**Grafana pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=grafana
NAME                       READY   STATUS    RESTARTS   AGE
grafana-6494c6659c-rh2ck   3/3     Running   0          47h
```

## Accelerator Metrics (DCGM Exporter)

NVIDIA DCGM Exporter exposes per-GPU metrics including utilization, memory usage,
temperature, power draw, and more in Prometheus exposition format.

### DCGM Exporter Health

**DCGM exporter pod**
```
$ kubectl get pods -n gpu-operator -l app=nvidia-dcgm-exporter -o wide
NAME                         READY   STATUS    RESTARTS   AGE   IP              NODE                             NOMINATED NODE   READINESS GATES
nvidia-dcgm-exporter-zfgtq   1/1     Running   0          44h   100.65.16.111   ip-100-64-147-149.ec2.internal   <none>           <none>
```

**DCGM exporter service**
```
$ kubectl get svc -n gpu-operator -l app=nvidia-dcgm-exporter
NAME                   TYPE        CLUSTER-IP     EXTERNAL-IP   PORT(S)    AGE
nvidia-dcgm-exporter   ClusterIP   172.20.81.36   <none>        9400/TCP   44h
```

### DCGM Metrics Endpoint

Query DCGM exporter directly to show raw GPU metrics in Prometheus format.

**Key GPU metrics from DCGM exporter (sampled)**
```
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.651,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.651,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-wmvrk",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.651,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.651,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.651,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.651,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.651,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.651,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.9,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.9,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-wmvrk",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.9,
          "74166"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.9,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.9,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.9,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.9,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826012.9,
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
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-81d79b08-40a0-40ae-3fc5-82b8ff8b8138",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.126,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.126,
          "29"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-wmvrk",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.126,
          "30"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.126,
          "30"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.126,
          "29"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.126,
          "27"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.126,
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.126,
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
          "Hostname": "ip-100-64-147-149.ec2.internal",
          "UUID": "GPU-81d79b08-40a0-40ae-3fc5-82b8ff8b8138",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.35,
          "67.56"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.35,
          "71.226"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "dynamo-workload",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "vllm-agg-0-vllmdecodeworker-wmvrk",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.35,
          "116.381"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.35,
          "67.94"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.35,
          "69.245"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.35,
          "66.621"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.35,
          "67.478"
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
          "instance": "100.65.16.111:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-zfgtq",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1772826013.35,
          "68.215"
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
pods/gpu_power_usage
namespaces/gpu_utilization
pods/gpu_utilization
namespaces/gpu_memory_used
pods/gpu_memory_used
namespaces/gpu_power_usage
```

**Result: PASS** — DCGM exporter provides per-GPU metrics (utilization, memory, temperature, power). Prometheus actively scrapes and stores metrics. Custom metrics API available via prometheus-adapter.
