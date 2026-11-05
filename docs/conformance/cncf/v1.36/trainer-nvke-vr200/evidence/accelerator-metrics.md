# Accelerator Metrics (DCGM Exporter)

**Cluster:** ` / `
**Generated:** 2026-11-04 01:11:28 UTC
**Kubernetes Version:** v1.36
**Platform:** linux/arm64

---

Demonstrates that the DCGM exporter exposes per-GPU metrics (utilization, memory,
temperature, power) in Prometheus format via a standardized metrics endpoint.

## Monitoring Stack Health

### Prometheus

**Prometheus pods**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus
NAME                                      READY   STATUS    RESTARTS   AGE
prometheus-kube-prometheus-prometheus-0   2/2     Running   0          10m
```

**Prometheus service**
```
$ kubectl get svc kube-prometheus-prometheus -n monitoring
NAME                         TYPE        CLUSTER-IP    EXTERNAL-IP   PORT(S)             AGE
kube-prometheus-prometheus   ClusterIP   10.109.29.9   <none>        9090/TCP,8080/TCP   11m
```

### Prometheus Adapter (Custom Metrics API)

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                  READY   STATUS    RESTARTS   AGE
prometheus-adapter-54f5596948-hvhdb   1/1     Running   0          10m
```

**Prometheus adapter service**
```
$ kubectl get svc prometheus-adapter -n monitoring
NAME                 TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)   AGE
prometheus-adapter   ClusterIP   10.111.245.172   <none>        443/TCP   10m
```

### Grafana

**Grafana pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=grafana
NAME                      READY   STATUS    RESTARTS   AGE
grafana-fd96bc865-2mshq   3/3     Running   0          11m
```

## Accelerator Metrics (DCGM Exporter)

NVIDIA DCGM Exporter exposes per-GPU metrics including utilization, memory usage,
temperature, power draw, and more in Prometheus exposition format.

### DCGM Exporter Health

**DCGM exporter pod**
```
$ kubectl get pods -n gpu-operator -l app=nvidia-dcgm-exporter -o wide
NAME                         READY   STATUS    RESTARTS      AGE   IP             NODE                                NOMINATED NODE   READINESS GATES
nvidia-dcgm-exporter-xqqjr   1/1     Running   3 (9h ago)    9h    192.168.2.9    manual-dsx-20261102-205148-w-8837   <none>           <none>
nvidia-dcgm-exporter-xzgsb   1/1     Running   2 (21h ago)   21h   192.168.1.17   manual-dsx-20261102-205148-w-8825   <none>           <none>
```

**DCGM exporter service**
```
$ kubectl get svc -n gpu-operator -l app=nvidia-dcgm-exporter
NAME                   TYPE        CLUSTER-IP     EXTERNAL-IP   PORT(S)    AGE
nvidia-dcgm-exporter   ClusterIP   10.104.75.10   <none>        9400/TCP   24h
```

### DCGM Metrics Endpoint

Query DCGM exporter directly to show raw GPU metrics in Prometheus format.

**Key GPU metrics from DCGM exporter (sampled)**
```
DCGM_FI_DEV_FB_FREE{gpu="2",UUID="GPU-401dc5ce-9ad3-58f4-a530-58634995b5ea",pci_bus_id="0000000A:81:00.0",device="nvidia2",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 285247
DCGM_FI_DEV_FB_FREE{gpu="3",UUID="GPU-6e0e318a-5820-502d-baee-4a88e6f79932",pci_bus_id="0000000A:E1:00.0",device="nvidia3",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 285247
DCGM_FI_DEV_FB_FREE{gpu="1",UUID="GPU-e88c50aa-7e38-5b26-8005-fabdba9d7fec",pci_bus_id="00000002:C1:00.0",device="nvidia1",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 285247
DCGM_FI_DEV_FB_FREE{gpu="0",UUID="GPU-f662ab51-7dd5-545c-be52-5aa9606316be",pci_bus_id="00000002:81:00.0",device="nvidia0",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 285247
DCGM_FI_DEV_FB_USED{gpu="2",UUID="GPU-401dc5ce-9ad3-58f4-a530-58634995b5ea",pci_bus_id="0000000A:81:00.0",device="nvidia2",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_FB_USED{gpu="3",UUID="GPU-6e0e318a-5820-502d-baee-4a88e6f79932",pci_bus_id="0000000A:E1:00.0",device="nvidia3",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_FB_USED{gpu="1",UUID="GPU-e88c50aa-7e38-5b26-8005-fabdba9d7fec",pci_bus_id="00000002:C1:00.0",device="nvidia1",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-f662ab51-7dd5-545c-be52-5aa9606316be",pci_bus_id="00000002:81:00.0",device="nvidia0",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_GPU_TEMP{gpu="2",UUID="GPU-401dc5ce-9ad3-58f4-a530-58634995b5ea",pci_bus_id="0000000A:81:00.0",device="nvidia2",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 31
DCGM_FI_DEV_GPU_TEMP{gpu="3",UUID="GPU-6e0e318a-5820-502d-baee-4a88e6f79932",pci_bus_id="0000000A:E1:00.0",device="nvidia3",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 32
DCGM_FI_DEV_GPU_TEMP{gpu="1",UUID="GPU-e88c50aa-7e38-5b26-8005-fabdba9d7fec",pci_bus_id="00000002:C1:00.0",device="nvidia1",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 31
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-f662ab51-7dd5-545c-be52-5aa9606316be",pci_bus_id="00000002:81:00.0",device="nvidia0",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 32
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-401dc5ce-9ad3-58f4-a530-58634995b5ea",pci_bus_id="0000000A:81:00.0",device="nvidia2",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="3",UUID="GPU-6e0e318a-5820-502d-baee-4a88e6f79932",pci_bus_id="0000000A:E1:00.0",device="nvidia3",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-e88c50aa-7e38-5b26-8005-fabdba9d7fec",pci_bus_id="00000002:C1:00.0",device="nvidia1",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-f662ab51-7dd5-545c-be52-5aa9606316be",pci_bus_id="00000002:81:00.0",device="nvidia0",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="2",UUID="GPU-401dc5ce-9ad3-58f4-a530-58634995b5ea",pci_bus_id="0000000A:81:00.0",device="nvidia2",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="3",UUID="GPU-6e0e318a-5820-502d-baee-4a88e6f79932",pci_bus_id="0000000A:E1:00.0",device="nvidia3",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="1",UUID="GPU-e88c50aa-7e38-5b26-8005-fabdba9d7fec",pci_bus_id="00000002:C1:00.0",device="nvidia1",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="0",UUID="GPU-f662ab51-7dd5-545c-be52-5aa9606316be",pci_bus_id="00000002:81:00.0",device="nvidia0",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 0
DCGM_FI_DEV_POWER_USAGE{gpu="2",UUID="GPU-401dc5ce-9ad3-58f4-a530-58634995b5ea",pci_bus_id="0000000A:81:00.0",device="nvidia2",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 409.068
DCGM_FI_DEV_POWER_USAGE{gpu="3",UUID="GPU-6e0e318a-5820-502d-baee-4a88e6f79932",pci_bus_id="0000000A:E1:00.0",device="nvidia3",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 406.355
DCGM_FI_DEV_POWER_USAGE{gpu="1",UUID="GPU-e88c50aa-7e38-5b26-8005-fabdba9d7fec",pci_bus_id="00000002:C1:00.0",device="nvidia1",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 408.727
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-f662ab51-7dd5-545c-be52-5aa9606316be",pci_bus_id="00000002:81:00.0",device="nvidia0",modelName="NVIDIA Graphics Device",hostname="manual-dsx-20261102-205148-w-8825",DCGM_FI_DRIVER_VERSION="615.71.09"} 418.361
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
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-401dc5ce-9ad3-58f4-a530-58634995b5ea",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:81:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754711.839,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-6e0e318a-5820-502d-baee-4a88e6f79932",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:E1:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754711.839,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-e88c50aa-7e38-5b26-8005-fabdba9d7fec",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:C1:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754711.839,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-f662ab51-7dd5-545c-be52-5aa9606316be",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:81:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754711.839,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-3fb726c1-1a86-5f8d-a1ce-3c8d32905179",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:81:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754711.839,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-5b75d25c-4a30-520f-a57e-d5f62234f0b8",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:C1:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754711.839,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-84bdc882-4277-50e5-85dd-8370a9f0f7b6",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:E1:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754711.839,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-c951412d-ce87-55a5-b580-a483a141b899",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:81:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754711.839,
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
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-401dc5ce-9ad3-58f4-a530-58634995b5ea",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:81:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754712.619,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-6e0e318a-5820-502d-baee-4a88e6f79932",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:E1:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754712.619,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-e88c50aa-7e38-5b26-8005-fabdba9d7fec",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:C1:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754712.619,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-f662ab51-7dd5-545c-be52-5aa9606316be",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:81:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754712.619,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-3fb726c1-1a86-5f8d-a1ce-3c8d32905179",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:81:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754712.619,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-5b75d25c-4a30-520f-a57e-d5f62234f0b8",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:C1:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754712.619,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-84bdc882-4277-50e5-85dd-8370a9f0f7b6",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:E1:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754712.619,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-c951412d-ce87-55a5-b580-a483a141b899",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:81:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754712.619,
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
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-401dc5ce-9ad3-58f4-a530-58634995b5ea",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:81:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754713.627,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-6e0e318a-5820-502d-baee-4a88e6f79932",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:E1:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754713.627,
          "32"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-e88c50aa-7e38-5b26-8005-fabdba9d7fec",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:C1:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754713.627,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-f662ab51-7dd5-545c-be52-5aa9606316be",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:81:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754713.627,
          "32"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-3fb726c1-1a86-5f8d-a1ce-3c8d32905179",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:81:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754713.627,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-5b75d25c-4a30-520f-a57e-d5f62234f0b8",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:C1:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754713.627,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-84bdc882-4277-50e5-85dd-8370a9f0f7b6",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:E1:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754713.627,
          "32"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-c951412d-ce87-55a5-b580-a483a141b899",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:81:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754713.627,
          "32"
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
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-401dc5ce-9ad3-58f4-a530-58634995b5ea",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:81:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754714.611,
          "409.143"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-6e0e318a-5820-502d-baee-4a88e6f79932",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:E1:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754714.611,
          "406.734"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-e88c50aa-7e38-5b26-8005-fabdba9d7fec",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:C1:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754714.611,
          "408.742"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-f662ab51-7dd5-545c-be52-5aa9606316be",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "manual-dsx-20261102-205148-w-8825",
          "instance": "192.168.1.17:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:81:00.0",
          "pod": "nvidia-dcgm-exporter-xzgsb",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754714.611,
          "418.362"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-3fb726c1-1a86-5f8d-a1ce-3c8d32905179",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:81:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754714.611,
          "406.491"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-5b75d25c-4a30-520f-a57e-d5f62234f0b8",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000002:C1:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754714.611,
          "406.709"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-84bdc882-4277-50e5-85dd-8370a9f0f7b6",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:E1:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754714.611,
          "405.111"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "615.71.09",
          "UUID": "GPU-c951412d-ce87-55a5-b580-a483a141b899",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "manual-dsx-20261102-205148-w-8837",
          "instance": "192.168.2.9:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA Graphics Device",
          "namespace": "gpu-operator",
          "pci_bus_id": "0000000A:81:00.0",
          "pod": "nvidia-dcgm-exporter-xqqjr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793754714.611,
          "410.263"
        ]
      }
    ]
  }
}
```

**Result: PASS** — DCGM exporter provides per-GPU metrics (utilization, memory, temperature, power). Prometheus actively scrapes and stores metrics.
