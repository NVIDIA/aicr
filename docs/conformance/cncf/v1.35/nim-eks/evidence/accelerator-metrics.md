# Accelerator Metrics (DCGM Exporter)

**Cluster:** `EKS / p5.48xlarge / NVIDIA-H100-80GB-HBM3`
**Generated:** 2026-04-01 23:15:23 UTC
**Kubernetes Version:** v1.35
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
prometheus-kube-prometheus-prometheus-0   2/2     Running   0          64m
```

**Prometheus service**
```
$ kubectl get svc kube-prometheus-prometheus -n monitoring
NAME                         TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)             AGE
kube-prometheus-prometheus   ClusterIP   172.20.72.172   <none>        9090/TCP,8080/TCP   64m
```

### Prometheus Adapter (Custom Metrics API)

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                  READY   STATUS    RESTARTS   AGE
prometheus-adapter-78b8b8d75c-wv9h2   1/1     Running   0          64m
```

**Prometheus adapter service**
```
$ kubectl get svc prometheus-adapter -n monitoring
NAME                 TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)   AGE
prometheus-adapter   ClusterIP   172.20.38.130   <none>        443/TCP   64m
```

### Grafana

**Grafana pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=grafana
NAME                       READY   STATUS    RESTARTS   AGE
grafana-56fbffd7d7-8rnr6   3/3     Running   0          64m
```

## Accelerator Metrics (DCGM Exporter)

NVIDIA DCGM Exporter exposes per-GPU metrics including utilization, memory usage,
temperature, power draw, and more in Prometheus exposition format.

### DCGM Exporter Health

**DCGM exporter pod**
```
$ kubectl get pods -n gpu-operator -l app=nvidia-dcgm-exporter -o wide
NAME                         READY   STATUS    RESTARTS   AGE   IP             NODE                           NOMINATED NODE   READINESS GATES
nvidia-dcgm-exporter-2xrln   1/1     Running   0          62m   10.0.187.45    ip-10-0-180-136.ec2.internal   <none>           <none>
nvidia-dcgm-exporter-sscnw   1/1     Running   0          62m   10.0.147.205   ip-10-0-251-220.ec2.internal   <none>           <none>
```

**DCGM exporter service**
```
$ kubectl get svc -n gpu-operator -l app=nvidia-dcgm-exporter
NAME                   TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)    AGE
nvidia-dcgm-exporter   ClusterIP   172.20.93.244   <none>        9400/TCP   62m
```

### DCGM Metrics Endpoint

Query DCGM exporter directly to show raw GPU metrics in Prometheus format.

**Key GPU metrics from DCGM exporter (sampled)**
```
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-15704b32-f531-14ce-0530-1ac21e4b68e6",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 31
DCGM_FI_DEV_GPU_TEMP{gpu="1",UUID="GPU-edc718f8-e593-6468-b9f9-563d508366ed",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 33
DCGM_FI_DEV_GPU_TEMP{gpu="2",UUID="GPU-e2d9b65e-98cb-5b7a-90f0-e0336573f9e2",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 31
DCGM_FI_DEV_GPU_TEMP{gpu="3",UUID="GPU-3a325419-de5f-778f-cf4e-fe7290362ac5",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 34
DCGM_FI_DEV_GPU_TEMP{gpu="4",UUID="GPU-275ad37d-ebd6-4cf6-3867-0499ba033a12",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 34
DCGM_FI_DEV_GPU_TEMP{gpu="5",UUID="GPU-3cab564d-1f63-674b-a831-024600bf985c",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 32
DCGM_FI_DEV_GPU_TEMP{gpu="6",UUID="GPU-d0f25a6f-9a3f-61b9-c128-3d14759651d7",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08",container="llama-3-2-1b-ctr",namespace="nim-workload",pod="llama-3-2-1b-7577f87fc7-dhb97",pod_uid=""} 37
DCGM_FI_DEV_GPU_TEMP{gpu="7",UUID="GPU-9bc10e9a-e27e-652b-9a1e-e84f7e446206",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 31
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-15704b32-f531-14ce-0530-1ac21e4b68e6",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.692000
DCGM_FI_DEV_POWER_USAGE{gpu="1",UUID="GPU-edc718f8-e593-6468-b9f9-563d508366ed",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.219000
DCGM_FI_DEV_POWER_USAGE{gpu="2",UUID="GPU-e2d9b65e-98cb-5b7a-90f0-e0336573f9e2",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.899000
DCGM_FI_DEV_POWER_USAGE{gpu="3",UUID="GPU-3a325419-de5f-778f-cf4e-fe7290362ac5",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 66.711000
DCGM_FI_DEV_POWER_USAGE{gpu="4",UUID="GPU-275ad37d-ebd6-4cf6-3867-0499ba033a12",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.875000
DCGM_FI_DEV_POWER_USAGE{gpu="5",UUID="GPU-3cab564d-1f63-674b-a831-024600bf985c",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 67.664000
DCGM_FI_DEV_POWER_USAGE{gpu="6",UUID="GPU-d0f25a6f-9a3f-61b9-c128-3d14759651d7",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08",container="llama-3-2-1b-ctr",namespace="nim-workload",pod="llama-3-2-1b-7577f87fc7-dhb97",pod_uid=""} 112.670000
DCGM_FI_DEV_POWER_USAGE{gpu="7",UUID="GPU-9bc10e9a-e27e-652b-9a1e-e84f7e446206",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 65.061000
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-15704b32-f531-14ce-0530-1ac21e4b68e6",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-edc718f8-e593-6468-b9f9-563d508366ed",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-e2d9b65e-98cb-5b7a-90f0-e0336573f9e2",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="3",UUID="GPU-3a325419-de5f-778f-cf4e-fe7290362ac5",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="4",UUID="GPU-275ad37d-ebd6-4cf6-3867-0499ba033a12",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="5",UUID="GPU-3cab564d-1f63-674b-a831-024600bf985c",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_GPU_UTIL{gpu="6",UUID="GPU-d0f25a6f-9a3f-61b9-c128-3d14759651d7",pci_bus_id="00000000:B9:00.0",device="nvidia6",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08",container="llama-3-2-1b-ctr",namespace="nim-workload",pod="llama-3-2-1b-7577f87fc7-dhb97",pod_uid=""} 0
DCGM_FI_DEV_GPU_UTIL{gpu="7",UUID="GPU-9bc10e9a-e27e-652b-9a1e-e84f7e446206",pci_bus_id="00000000:CA:00.0",device="nvidia7",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="0",UUID="GPU-15704b32-f531-14ce-0530-1ac21e4b68e6",pci_bus_id="00000000:53:00.0",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="1",UUID="GPU-edc718f8-e593-6468-b9f9-563d508366ed",pci_bus_id="00000000:64:00.0",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="2",UUID="GPU-e2d9b65e-98cb-5b7a-90f0-e0336573f9e2",pci_bus_id="00000000:75:00.0",device="nvidia2",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="3",UUID="GPU-3a325419-de5f-778f-cf4e-fe7290362ac5",pci_bus_id="00000000:86:00.0",device="nvidia3",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="4",UUID="GPU-275ad37d-ebd6-4cf6-3867-0499ba033a12",pci_bus_id="00000000:97:00.0",device="nvidia4",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
DCGM_FI_DEV_MEM_COPY_UTIL{gpu="5",UUID="GPU-3cab564d-1f63-674b-a831-024600bf985c",pci_bus_id="00000000:A8:00.0",device="nvidia5",modelName="NVIDIA H100 80GB HBM3",Hostname="ip-10-0-180-136.ec2.internal",DCGM_FI_DRIVER_VERSION="580.105.08"} 0
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
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-15704b32-f531-14ce-0530-1ac21e4b68e6",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-edc718f8-e593-6468-b9f9-563d508366ed",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-e2d9b65e-98cb-5b7a-90f0-e0336573f9e2",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-3a325419-de5f-778f-cf4e-fe7290362ac5",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-275ad37d-ebd6-4cf6-3867-0499ba033a12",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-3cab564d-1f63-674b-a831-024600bf985c",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-9bc10e9a-e27e-652b-9a1e-e84f7e446206",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-3f048793-8751-030e-5870-ebbd2b10cef2",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-cc644abe-17e4-7cb7-500d-ed8c09aea2fb",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-8d0b1081-9549-2b14-7e01-b4a725873c21",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-38bbfee9-dc95-ffb5-4034-f9a6c82a45bb",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-24087b69-8889-6b23-feeb-2905664fbcbf",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-d2f75162-e86d-0da0-0af4-3fa0b80038cd",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-b00fe5f9-5832-19d6-0276-28d8630f0f4b",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-530bd4b0-238b-f0c2-b496-63595812bca8",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-d0f25a6f-9a3f-61b9-c128-3d14759651d7",
          "__name__": "DCGM_FI_DEV_GPU_UTIL",
          "container": "llama-3-2-1b-ctr",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "nim-workload",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "llama-3-2-1b-7577f87fc7-dhb97",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085339.885,
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
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-15704b32-f531-14ce-0530-1ac21e4b68e6",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-edc718f8-e593-6468-b9f9-563d508366ed",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-e2d9b65e-98cb-5b7a-90f0-e0336573f9e2",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-3a325419-de5f-778f-cf4e-fe7290362ac5",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-275ad37d-ebd6-4cf6-3867-0499ba033a12",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-3cab564d-1f63-674b-a831-024600bf985c",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-9bc10e9a-e27e-652b-9a1e-e84f7e446206",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-3f048793-8751-030e-5870-ebbd2b10cef2",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-cc644abe-17e4-7cb7-500d-ed8c09aea2fb",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-8d0b1081-9549-2b14-7e01-b4a725873c21",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-38bbfee9-dc95-ffb5-4034-f9a6c82a45bb",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-24087b69-8889-6b23-feeb-2905664fbcbf",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-d2f75162-e86d-0da0-0af4-3fa0b80038cd",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-b00fe5f9-5832-19d6-0276-28d8630f0f4b",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-530bd4b0-238b-f0c2-b496-63595812bca8",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "0"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-d0f25a6f-9a3f-61b9-c128-3d14759651d7",
          "__name__": "DCGM_FI_DEV_FB_USED",
          "container": "llama-3-2-1b-ctr",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "nim-workload",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "llama-3-2-1b-7577f87fc7-dhb97",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.205,
          "75050"
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
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-15704b32-f531-14ce-0530-1ac21e4b68e6",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-edc718f8-e593-6468-b9f9-563d508366ed",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "33"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-e2d9b65e-98cb-5b7a-90f0-e0336573f9e2",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-3a325419-de5f-778f-cf4e-fe7290362ac5",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "34"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-275ad37d-ebd6-4cf6-3867-0499ba033a12",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "34"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-3cab564d-1f63-674b-a831-024600bf985c",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "32"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-9bc10e9a-e27e-652b-9a1e-e84f7e446206",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-3f048793-8751-030e-5870-ebbd2b10cef2",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-cc644abe-17e4-7cb7-500d-ed8c09aea2fb",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "33"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-8d0b1081-9549-2b14-7e01-b4a725873c21",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-38bbfee9-dc95-ffb5-4034-f9a6c82a45bb",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "32"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-24087b69-8889-6b23-feeb-2905664fbcbf",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "33"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-d2f75162-e86d-0da0-0af4-3fa0b80038cd",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-b00fe5f9-5832-19d6-0276-28d8630f0f4b",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "32"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-530bd4b0-238b-f0c2-b496-63595812bca8",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "31"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-d0f25a6f-9a3f-61b9-c128-3d14759651d7",
          "__name__": "DCGM_FI_DEV_GPU_TEMP",
          "container": "llama-3-2-1b-ctr",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "nim-workload",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "llama-3-2-1b-7577f87fc7-dhb97",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.554,
          "37"
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
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-15704b32-f531-14ce-0530-1ac21e4b68e6",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "67.692"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-edc718f8-e593-6468-b9f9-563d508366ed",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "67.219"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-e2d9b65e-98cb-5b7a-90f0-e0336573f9e2",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "67.899"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-3a325419-de5f-778f-cf4e-fe7290362ac5",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "66.711"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-275ad37d-ebd6-4cf6-3867-0499ba033a12",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "67.875"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-3cab564d-1f63-674b-a831-024600bf985c",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "67.664"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-9bc10e9a-e27e-652b-9a1e-e84f7e446206",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-2xrln",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "65.061"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-3f048793-8751-030e-5870-ebbd2b10cef2",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia0",
          "endpoint": "gpu-metrics",
          "gpu": "0",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:53:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "68.284"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-cc644abe-17e4-7cb7-500d-ed8c09aea2fb",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia1",
          "endpoint": "gpu-metrics",
          "gpu": "1",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:64:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "70.963"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-8d0b1081-9549-2b14-7e01-b4a725873c21",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia2",
          "endpoint": "gpu-metrics",
          "gpu": "2",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:75:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "67.535"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-38bbfee9-dc95-ffb5-4034-f9a6c82a45bb",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia3",
          "endpoint": "gpu-metrics",
          "gpu": "3",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:86:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "68.419"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-24087b69-8889-6b23-feeb-2905664fbcbf",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia4",
          "endpoint": "gpu-metrics",
          "gpu": "4",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:97:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "69.498"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-d2f75162-e86d-0da0-0af4-3fa0b80038cd",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia5",
          "endpoint": "gpu-metrics",
          "gpu": "5",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:A8:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "69.66"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-b00fe5f9-5832-19d6-0276-28d8630f0f4b",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "66.98"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-251-220.ec2.internal",
          "UUID": "GPU-530bd4b0-238b-f0c2-b496-63595812bca8",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "nvidia-dcgm-exporter",
          "device": "nvidia7",
          "endpoint": "gpu-metrics",
          "gpu": "7",
          "instance": "10.0.147.205:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "gpu-operator",
          "pci_bus_id": "00000000:CA:00.0",
          "pod": "nvidia-dcgm-exporter-sscnw",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "68.367"
        ]
      },
      {
        "metric": {
          "DCGM_FI_DRIVER_VERSION": "580.105.08",
          "Hostname": "ip-10-0-180-136.ec2.internal",
          "UUID": "GPU-d0f25a6f-9a3f-61b9-c128-3d14759651d7",
          "__name__": "DCGM_FI_DEV_POWER_USAGE",
          "container": "llama-3-2-1b-ctr",
          "device": "nvidia6",
          "endpoint": "gpu-metrics",
          "gpu": "6",
          "instance": "10.0.187.45:9400",
          "job": "nvidia-dcgm-exporter",
          "modelName": "NVIDIA H100 80GB HBM3",
          "namespace": "nim-workload",
          "pci_bus_id": "00000000:B9:00.0",
          "pod": "llama-3-2-1b-7577f87fc7-dhb97",
          "service": "nvidia-dcgm-exporter"
        },
        "value": [
          1775085340.891,
          "112.67"
        ]
      }
    ]
  }
}
```

**Result: PASS** — DCGM exporter provides per-GPU metrics (utilization, memory, temperature, power). Prometheus actively scrapes and stores metrics.
