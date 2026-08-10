# Accelerator Metrics (DCGM Exporter)

**Cluster:** `EKS / p6e-gb200.36xlarge / NVIDIA-GB200`
**Generated:** 2026-08-09 01:25:00 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates that the DCGM exporter exposes per-GPU metrics (utilization, memory,
temperature, power) in Prometheus format via a standardized metrics endpoint.

## Monitoring Stack Health

### Prometheus

**Prometheus pods**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus
NAME                                      READY   STATUS    RESTARTS   AGE
prometheus-kube-prometheus-prometheus-0   2/2     Running   0          26d
```

**Prometheus service**
```
$ kubectl get svc kube-prometheus-prometheus -n monitoring
NAME                         TYPE        CLUSTER-IP    EXTERNAL-IP   PORT(S)             AGE
kube-prometheus-prometheus   ClusterIP   172.20.48.2   <none>        9090/TCP,8080/TCP   26d
```

### Prometheus Adapter (Custom Metrics API)

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                  READY   STATUS    RESTARTS   AGE
prometheus-adapter-849d7698c8-cfr9b   1/1     Running   0          26d
```

**Prometheus adapter service**
```
$ kubectl get svc prometheus-adapter -n monitoring
NAME                 TYPE        CLUSTER-IP     EXTERNAL-IP   PORT(S)   AGE
prometheus-adapter   ClusterIP   172.20.252.6   <none>        443/TCP   26d
```

### Grafana

**Grafana pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=grafana
NAME                       READY   STATUS    RESTARTS   AGE
grafana-5755f666cf-w6rgw   3/3     Running   0          26d
```

## Accelerator Metrics (DCGM Exporter)

NVIDIA DCGM Exporter exposes per-GPU metrics including utilization, memory usage,
temperature, power draw, and more in Prometheus exposition format.

### DCGM Exporter Health

**DCGM exporter pod**
```
$ kubectl get pods -n gpu-operator -l app=nvidia-dcgm-exporter -o wide
NAME                         READY   STATUS    RESTARTS      AGE   IP               NODE                             NOMINATED NODE   READINESS GATES
nvidia-dcgm-exporter-thpnl   1/1     Running   2 (26d ago)   26d   100.65.229.140   ip-100-64-142-208.ec2.internal   <none>           <none>
nvidia-dcgm-exporter-zn7nr   1/1     Running   2 (26d ago)   26d   100.65.162.68    ip-100-64-135-20.ec2.internal    <none>           <none>
```

**DCGM exporter service**
```
$ kubectl get svc -n gpu-operator -l app=nvidia-dcgm-exporter
NAME                   TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)    AGE
nvidia-dcgm-exporter   ClusterIP   172.20.216.228   <none>        9400/TCP   26d
```

### DCGM Metrics Endpoint

Query DCGM exporter directly to show raw GPU metrics in Prometheus format.

**Key GPU metrics from DCGM exporter (sampled)**
```
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-c4873a68-bfd2-8a3e-7776-061ea4b074bc",pci_bus_id="00000000:29:00.0",device="nvidia0",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 27
DCGM_FI_DEV_GPU_TEMP{gpu="1",UUID="GPU-edc1afc5-3d3b-2f54-3197-6d588a896e20",pci_bus_id="00000000:3F:00.0",device="nvidia1",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 28
DCGM_FI_DEV_GPU_TEMP{gpu="2",UUID="GPU-15e915b5-e8eb-5aa0-cd6c-a873d59a4406",pci_bus_id="00000000:9C:00.0",device="nvidia2",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 27
DCGM_FI_DEV_GPU_TEMP{gpu="3",UUID="GPU-63c10f33-73a6-e0b7-06c8-1454c80ab6fd",pci_bus_id="00000000:B2:00.0",device="nvidia3",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 26
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-c4873a68-bfd2-8a3e-7776-061ea4b074bc",pci_bus_id="00000000:29:00.0",device="nvidia0",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 165.926000
DCGM_FI_DEV_POWER_USAGE{gpu="1",UUID="GPU-edc1afc5-3d3b-2f54-3197-6d588a896e20",pci_bus_id="00000000:3F:00.0",device="nvidia1",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 168.570000
DCGM_FI_DEV_POWER_USAGE{gpu="2",UUID="GPU-15e915b5-e8eb-5aa0-cd6c-a873d59a4406",pci_bus_id="00000000:9C:00.0",device="nvidia2",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 156.479000
DCGM_FI_DEV_POWER_USAGE{gpu="3",UUID="GPU-63c10f33-73a6-e0b7-06c8-1454c80ab6fd",pci_bus_id="00000000:B2:00.0",device="nvidia3",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 174.197000
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-c4873a68-bfd2-8a3e-7776-061ea4b074bc",pci_bus_id="00000000:29:00.0",device="nvidia0",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-edc1afc5-3d3b-2f54-3197-6d588a896e20",pci_bus_id="00000000:3F:00.0",device="nvidia1",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-15e915b5-e8eb-5aa0-cd6c-a873d59a4406",pci_bus_id="00000000:9C:00.0",device="nvidia2",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="3",UUID="GPU-63c10f33-73a6-e0b7-06c8-1454c80ab6fd",pci_bus_id="00000000:B2:00.0",device="nvidia3",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="0",UUID="GPU-c4873a68-bfd2-8a3e-7776-061ea4b074bc",pci_bus_id="00000000:29:00.0",device="nvidia0",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="1",UUID="GPU-edc1afc5-3d3b-2f54-3197-6d588a896e20",pci_bus_id="00000000:3F:00.0",device="nvidia1",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="2",UUID="GPU-15e915b5-e8eb-5aa0-cd6c-a873d59a4406",pci_bus_id="00000000:9C:00.0",device="nvidia2",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="3",UUID="GPU-63c10f33-73a6-e0b7-06c8-1454c80ab6fd",pci_bus_id="00000000:B2:00.0",device="nvidia3",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
DCGM_FI_DEV_FB_FREE{gpu="0",UUID="GPU-c4873a68-bfd2-8a3e-7776-061ea4b074bc",pci_bus_id="00000000:29:00.0",device="nvidia0",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 188729
DCGM_FI_DEV_FB_FREE{gpu="1",UUID="GPU-edc1afc5-3d3b-2f54-3197-6d588a896e20",pci_bus_id="00000000:3F:00.0",device="nvidia1",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 188729
DCGM_FI_DEV_FB_FREE{gpu="2",UUID="GPU-15e915b5-e8eb-5aa0-cd6c-a873d59a4406",pci_bus_id="00000000:9C:00.0",device="nvidia2",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 188729
DCGM_FI_DEV_FB_FREE{gpu="3",UUID="GPU-63c10f33-73a6-e0b7-06c8-1454c80ab6fd",pci_bus_id="00000000:B2:00.0",device="nvidia3",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 188729
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-c4873a68-bfd2-8a3e-7776-061ea4b074bc",pci_bus_id="00000000:29:00.0",device="nvidia0",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
DCGM_FI_DEV_FB_USED{gpu="1",UUID="GPU-edc1afc5-3d3b-2f54-3197-6d588a896e20",pci_bus_id="00000000:3F:00.0",device="nvidia1",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
DCGM_FI_DEV_FB_USED{gpu="2",UUID="GPU-15e915b5-e8eb-5aa0-cd6c-a873d59a4406",pci_bus_id="00000000:9C:00.0",device="nvidia2",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
DCGM_FI_DEV_FB_USED{gpu="3",UUID="GPU-63c10f33-73a6-e0b7-06c8-1454c80ab6fd",pci_bus_id="00000000:B2:00.0",device="nvidia3",modelName="NVIDIA GB200",Hostname="ip-100-64-135-20.ec2.internal",DCGM_FI_DRIVER_VERSION="580.126.20"} 0
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
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-c4873a68-bfd2-8a3e-7776-061ea4b074bc",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:29:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238717.755,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-edc1afc5-3d3b-2f54-3197-6d588a896e20",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:3F:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238717.755,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-15e915b5-e8eb-5aa0-cd6c-a873d59a4406",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:9C:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238717.755,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-63c10f33-73a6-e0b7-06c8-1454c80ab6fd",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B2:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238717.755,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-ab4eb29b-2c6f-7eb5-4373-44193966f1a2",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:29:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238717.755,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-b1b35bd6-498c-c7be-5f54-12a5ab945074",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:3F:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238717.755,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-e9cc19fd-0359-1dab-7602-eecadb825238",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:9C:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238717.755,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-8d80422b-8dc8-7cb0-6016-ac56a5c9876f",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B2:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238717.755,
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
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-c4873a68-bfd2-8a3e-7776-061ea4b074bc",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:29:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.108,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-edc1afc5-3d3b-2f54-3197-6d588a896e20",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:3F:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.108,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-15e915b5-e8eb-5aa0-cd6c-a873d59a4406",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:9C:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.108,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-63c10f33-73a6-e0b7-06c8-1454c80ab6fd",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B2:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.108,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-ab4eb29b-2c6f-7eb5-4373-44193966f1a2",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:29:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.108,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-b1b35bd6-498c-c7be-5f54-12a5ab945074",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:3F:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.108,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-e9cc19fd-0359-1dab-7602-eecadb825238",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:9C:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.108,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-8d80422b-8dc8-7cb0-6016-ac56a5c9876f",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B2:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.108,
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
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-c4873a68-bfd2-8a3e-7776-061ea4b074bc",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:29:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.441,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-edc1afc5-3d3b-2f54-3197-6d588a896e20",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:3F:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.441,
          "28"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-15e915b5-e8eb-5aa0-cd6c-a873d59a4406",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:9C:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.441,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-63c10f33-73a6-e0b7-06c8-1454c80ab6fd",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B2:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.441,
          "26"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-ab4eb29b-2c6f-7eb5-4373-44193966f1a2",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:29:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.441,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-b1b35bd6-498c-c7be-5f54-12a5ab945074",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:3F:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.441,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-e9cc19fd-0359-1dab-7602-eecadb825238",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:9C:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.441,
          "27"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-8d80422b-8dc8-7cb0-6016-ac56a5c9876f",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B2:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.441,
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
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-c4873a68-bfd2-8a3e-7776-061ea4b074bc",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:29:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.783,
          "165.926"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-edc1afc5-3d3b-2f54-3197-6d588a896e20",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:3F:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.783,
          "168.57"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-15e915b5-e8eb-5aa0-cd6c-a873d59a4406",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:9C:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.783,
          "156.479"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-135-20.ec2.internal",
          "UUID": "GPU-63c10f33-73a6-e0b7-06c8-1454c80ab6fd",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.162.68:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B2:00.0",
          "pod": "nvidia-dcgm-exporter-zn7nr",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.783,
          "174.197"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-ab4eb29b-2c6f-7eb5-4373-44193966f1a2",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:29:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.783,
          "169.287"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-b1b35bd6-498c-c7be-5f54-12a5ab945074",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:3F:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.783,
          "172.216"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-e9cc19fd-0359-1dab-7602-eecadb825238",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:9C:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.783,
          "176.497"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.126.20",
          "Hostname": "ip-100-64-142-208.ec2.internal",
          "UUID": "GPU-8d80422b-8dc8-7cb0-6016-ac56a5c9876f",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "100.65.229.140:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA GB200",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B2:00.0",
          "pod": "nvidia-dcgm-exporter-thpnl",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1786238718.783,
          "191.501"
        ]
      }
    ]
  }
}
```

**Result: PASS** — DCGM exporter provides per-GPU metrics (utilization, memory, temperature, power). Prometheus actively scrapes and stores metrics.
