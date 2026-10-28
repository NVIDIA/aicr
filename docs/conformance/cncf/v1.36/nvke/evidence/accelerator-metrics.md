# Accelerator Metrics (DCGM Exporter)

**Cluster:** `NVKE conformance cluster / NVIDIA-GB200`
**Generated:** 2026-10-27 23:55:06 UTC
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
prometheus-kube-prometheus-prometheus-0   2/2     Running   0          27m
```

**Prometheus service**
```
$ kubectl get svc kube-prometheus-prometheus -n monitoring
NAME                         TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)             AGE
kube-prometheus-prometheus   ClusterIP   REDACTED_IP_01   <none>        9090/TCP,8080/TCP   29h
```

### Prometheus Adapter (Custom Metrics API)

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                  READY   STATUS    RESTARTS   AGE
prometheus-adapter-54f5596948-brj98   1/1     Running   0          26m
```

**Prometheus adapter service**
```
$ kubectl get svc prometheus-adapter -n monitoring
NAME                 TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)   AGE
prometheus-adapter   ClusterIP   REDACTED_IP_02   <none>        443/TCP   29h
```

### Grafana

**Grafana pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=grafana
NAME                      READY   STATUS    RESTARTS   AGE
grafana-fd96bc865-dms4n   3/3     Running   0          27m
```

## Accelerator Metrics (DCGM Exporter)

NVIDIA DCGM Exporter exposes per-GPU metrics including utilization, memory usage,
temperature, power draw, and more in Prometheus exposition format.

### DCGM Exporter Health

**DCGM exporter pod**
```
$ kubectl get pods -n gpu-operator -l app=nvidia-dcgm-exporter -o wide
NAME                         READY   STATUS    RESTARTS      AGE   IP             NODE                                  NOMINATED NODE   READINESS GATES
nvidia-dcgm-exporter-r62ft   1/1     Running   3 (29h ago)   29h   REDACTED_IP_13   gpu-worker-2   <none>           <none>
nvidia-dcgm-exporter-sc8gf   1/1     Running   3 (29h ago)   29h   REDACTED_IP_29   gpu-worker-1   <none>           <none>
```

**DCGM exporter service**
```
$ kubectl get svc -n gpu-operator -l app=nvidia-dcgm-exporter
NAME                   TYPE        CLUSTER-IP    EXTERNAL-IP   PORT(S)    AGE
nvidia-dcgm-exporter   ClusterIP   REDACTED_IP_04   <none>        9400/TCP   29h
```

### DCGM Metrics Endpoint

Query DCGM exporter directly to show raw GPU metrics in Prometheus format.

**Key GPU metrics from DCGM exporter (sampled)**
```
DCGM_FI_DEV_FB_FREE{gpu="1",UUID="GPU-REDACTED_UUID_01",pci_bus_id="00000009:01:00.0",device="nvidia1",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 188729
DCGM_FI_DEV_FB_FREE{gpu="3",UUID="GPU-REDACTED_UUID_02",pci_bus_id="00000019:01:00.0",device="nvidia3",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 188729
DCGM_FI_DEV_FB_FREE{gpu="0",UUID="GPU-REDACTED_UUID_05",pci_bus_id="00000008:01:00.0",device="nvidia0",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 188729
DCGM_FI_DEV_FB_FREE{gpu="2",UUID="GPU-REDACTED_UUID_12",pci_bus_id="00000018:01:00.0",device="nvidia2",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 188729
DCGM_FI_DEV_FB_USED{gpu="1",UUID="GPU-REDACTED_UUID_01",pci_bus_id="00000009:01:00.0",device="nvidia1",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_FB_USED{gpu="3",UUID="GPU-REDACTED_UUID_02",pci_bus_id="00000019:01:00.0",device="nvidia3",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-REDACTED_UUID_05",pci_bus_id="00000008:01:00.0",device="nvidia0",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_FB_USED{gpu="2",UUID="GPU-REDACTED_UUID_12",pci_bus_id="00000018:01:00.0",device="nvidia2",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_GPU_TEMP{gpu="1",UUID="GPU-REDACTED_UUID_01",pci_bus_id="00000009:01:00.0",device="nvidia1",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 37
DCGM_FI_DEV_GPU_TEMP{gpu="3",UUID="GPU-REDACTED_UUID_02",pci_bus_id="00000019:01:00.0",device="nvidia3",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 38
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-REDACTED_UUID_05",pci_bus_id="00000008:01:00.0",device="nvidia0",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 37
DCGM_FI_DEV_GPU_TEMP{gpu="2",UUID="GPU-REDACTED_UUID_12",pci_bus_id="00000018:01:00.0",device="nvidia2",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 37
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-REDACTED_UUID_01",pci_bus_id="00000009:01:00.0",device="nvidia1",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="3",UUID="GPU-REDACTED_UUID_02",pci_bus_id="00000019:01:00.0",device="nvidia3",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-REDACTED_UUID_05",pci_bus_id="00000008:01:00.0",device="nvidia0",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-REDACTED_UUID_12",pci_bus_id="00000018:01:00.0",device="nvidia2",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="1",UUID="GPU-REDACTED_UUID_01",pci_bus_id="00000009:01:00.0",device="nvidia1",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="3",UUID="GPU-REDACTED_UUID_02",pci_bus_id="00000019:01:00.0",device="nvidia3",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="0",UUID="GPU-REDACTED_UUID_05",pci_bus_id="00000008:01:00.0",device="nvidia0",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="2",UUID="GPU-REDACTED_UUID_12",pci_bus_id="00000018:01:00.0",device="nvidia2",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 0
DCGM_FI_DEV_POWER_USAGE{gpu="1",UUID="GPU-REDACTED_UUID_01",pci_bus_id="00000009:01:00.0",device="nvidia1",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 149.84
DCGM_FI_DEV_POWER_USAGE{gpu="3",UUID="GPU-REDACTED_UUID_02",pci_bus_id="00000019:01:00.0",device="nvidia3",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 174.734
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-REDACTED_UUID_05",pci_bus_id="00000008:01:00.0",device="nvidia0",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 166.345
DCGM_FI_DEV_POWER_USAGE{gpu="2",UUID="GPU-REDACTED_UUID_12",pci_bus_id="00000018:01:00.0",device="nvidia2",modelName="NVIDIA GB200",hostname="gpu-worker-1",DCGM_FI_DRIVER_VERSION="580.173.02"} 149.992
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
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_04",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000008:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.965,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_09",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000018:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.965,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_10",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000019:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.965,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_11",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000009:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.965,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_05",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000008:01:00.0",
          "pod": "nvidia-dcgm-exporter-sc8gf",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.965,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_12",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000018:01:00.0",
          "pod": "nvidia-dcgm-exporter-sc8gf",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.965,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_01",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "worker",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gang-scheduling-test",
          "pci_bus_id": "00000009:01:00.0",
          "pod": "gang-worker-1",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.965,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_02",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "worker",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gang-scheduling-test",
          "pci_bus_id": "00000019:01:00.0",
          "pod": "gang-worker-0",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.965,
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
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_04",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000008:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.982,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_09",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000018:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.982,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_10",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000019:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.982,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_11",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000009:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.982,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_05",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000008:01:00.0",
          "pod": "nvidia-dcgm-exporter-sc8gf",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.982,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_12",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000018:01:00.0",
          "pod": "nvidia-dcgm-exporter-sc8gf",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.982,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_01",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "worker",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gang-scheduling-test",
          "pci_bus_id": "00000009:01:00.0",
          "pod": "gang-worker-1",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.982,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_02",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "worker",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gang-scheduling-test",
          "pci_bus_id": "00000019:01:00.0",
          "pod": "gang-worker-0",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.982,
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
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_04",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000008:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.997,
          "37"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_09",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000018:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.997,
          "37"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_10",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000019:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.997,
          "38"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_11",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000009:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.997,
          "37"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_05",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000008:01:00.0",
          "pod": "nvidia-dcgm-exporter-sc8gf",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.997,
          "37"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_12",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000018:01:00.0",
          "pod": "nvidia-dcgm-exporter-sc8gf",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.997,
          "37"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_01",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "worker",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gang-scheduling-test",
          "pci_bus_id": "00000009:01:00.0",
          "pod": "gang-worker-1",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.997,
          "37"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_02",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "worker",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gang-scheduling-test",
          "pci_bus_id": "00000019:01:00.0",
          "pod": "gang-worker-0",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145308.997,
          "38"
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
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_04",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000008:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145309.012,
          "162.899"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_09",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000018:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145309.012,
          "154.16"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_10",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000019:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145309.012,
          "171.136"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_11",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "gpu-worker-2",
          "instance": "REDACTED_IP_13:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000009:01:00.0",
          "pod": "nvidia-dcgm-exporter-r62ft",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145309.012,
          "158.044"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_05",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000008:01:00.0",
          "pod": "nvidia-dcgm-exporter-sc8gf",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145309.012,
          "166.345"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_12",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000018:01:00.0",
          "pod": "nvidia-dcgm-exporter-sc8gf",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145309.012,
          "149.994"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_01",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "worker",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gang-scheduling-test",
          "pci_bus_id": "00000009:01:00.0",
          "pod": "gang-worker-1",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145309.012,
          "149.838"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.173.02",
          "UUID": "GPU-REDACTED_UUID_02",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "worker",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "hostname": "gpu-worker-1",
          "instance": "REDACTED_IP_29:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gang-scheduling-test",
          "pci_bus_id": "00000019:01:00.0",
          "pod": "gang-worker-0",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1793145309.012,
          "174.713"
        ]
      }
    ]
  }
}
```

**Result: PASS** — DCGM exporter provides per-GPU metrics (utilization, memory, temperature, power). Prometheus actively scrapes and stores metrics.
