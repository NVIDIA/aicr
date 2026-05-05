# NVIDIA AI Cluster Runtime — Container Image Inventory

## Summary

- Components: **22**
- Unique images: **70**
- Distinct registries: **11**

Registries: `602401143452.dkr.ecr.us-west-2.amazonaws.com`, `cr.kgateway.dev`, `docker.io`, `gcr.io`, `ghcr.io`, `gke.gcr.io`, `nvcr.io`, `public.ecr.aws`, `quay.io`, `registry.k8s.io`, `us-docker.pkg.dev`

## Components

| Component | Type | Chart | Pinned Version | Images |
|-----------|------|-------|----------------|--------|
| aws-ebs-csi-driver | helm | aws-ebs-csi-driver/aws-ebs-csi-driver | 2.59.0 | 6 |
| aws-efa | helm | aws-efa-k8s-device-plugin | v0.5.3 | 1 |
| cert-manager | helm | jetstack/cert-manager | v1.20.2 | 4 |
| dynamo-platform | helm | dynamo-platform | 1.0.2 | 1 |
| gke-nccl-tcpxo | manifest | — | — | 4 |
| gpu-operator | helm | nvidia/gpu-operator | — | 15 |
| grove | helm | grove-charts | v0.1.0-alpha.6 | 1 |
| k8s-ephemeral-storage-metrics | helm | k8s-ephemeral-storage-metrics/k8s-ephemeral-storage-metrics | 1.19.2 | 1 |
| k8s-nim-operator | helm | k8s-nim-operator | 3.1.0 | 1 |
| kai-scheduler | helm | kai-scheduler | v0.14.1 | 2 |
| kgateway | helm | kgateway | v2.0.0 | 1 |
| kgateway-crds | helm | kgateway-crds | v2.0.0 | 0 |
| kube-prometheus-stack | helm | prometheus-community/kube-prometheus-stack | 84.4.0 | 8 |
| kubeflow-trainer | helm | kubeflow-trainer | 2.2.0 | 3 |
| kueue | helm | kueue | 0.17.1 | 1 |
| network-operator | helm | nvidia/network-operator | — | 5 |
| nfd | helm | node-feature-discovery | 0.18.3 | 1 |
| nodewright-customizations | manifest | — | — | 4 |
| nodewright-operator | helm | skyhook-operator | — | 3 |
| nvidia-dra-driver-gpu | helm | nvidia/nvidia-dra-driver-gpu | — | 1 |
| nvsentinel | helm | nvsentinel | v1.3.0 | 6 |
| prometheus-adapter | helm | prometheus-community/prometheus-adapter | 5.3.0 | 1 |

## Images by component

### aws-ebs-csi-driver

- `public.ecr.aws/csi-components/csi-attacher:v4.11.0-eksbuild.4`
- `public.ecr.aws/csi-components/csi-node-driver-registrar:v2.16.0-eksbuild.4`
- `public.ecr.aws/csi-components/csi-provisioner:v6.2.0-eksbuild.3`
- `public.ecr.aws/csi-components/csi-resizer:v2.1.0-eksbuild.4`
- `public.ecr.aws/csi-components/livenessprobe:v2.18.0-eksbuild.4`
- `public.ecr.aws/ebs-csi-driver/aws-ebs-csi-driver:v1.59.0`

### aws-efa

- `602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/aws-efa-k8s-device-plugin:v0.5.3`

### cert-manager

- `quay.io/jetstack/cert-manager-cainjector:v1.20.2`
- `quay.io/jetstack/cert-manager-controller:v1.20.2`
- `quay.io/jetstack/cert-manager-startupapicheck:v1.20.2`
- `quay.io/jetstack/cert-manager-webhook:v1.20.2`

### dynamo-platform

- `nvcr.io/nvidia/ai-dynamo/kubernetes-operator:1.0.2`

### gke-nccl-tcpxo

- `gcr.io/gke-release/nri-device-injector@sha256:7704e2bd74b8edbb76b6913c7904cc2362f1fa887c4d4aba7b19778ea353537c`
- `gke.gcr.io/pause:3.8@sha256:880e63f94b145e46f1b1082bb71b85e21f16b99b180b9996407d61240ceb9830`
- `ubuntu:24.04@sha256:c4a8d5503dfb2a3eb8ab5f807da5bc69a85730fb49b5cfca2330194ebcc41c7b`
- `us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:v1.0.15`

### gpu-operator

- `nvcr.io/nvidia/cloud-native/dcgm:4.5.2-1-ubuntu22.04`
- `nvcr.io/nvidia/cloud-native/gdrdrv:v2.5.2`
- `nvcr.io/nvidia/cloud-native/k8s-cc-manager:v0.4.0`
- `nvcr.io/nvidia/cloud-native/k8s-driver-manager:v0.10.0`
- `nvcr.io/nvidia/cloud-native/k8s-mig-manager:v0.14.0`
- `nvcr.io/nvidia/cloud-native/nvidia-fs:2.27.3`
- `nvcr.io/nvidia/cloud-native/nvidia-sandbox-device-plugin:v0.0.3`
- `nvcr.io/nvidia/cloud-native/vgpu-device-manager:v0.4.2`
- `nvcr.io/nvidia/driver:580.105.08`
- `nvcr.io/nvidia/gpu-operator:v26.3.1`
- `nvcr.io/nvidia/k8s-device-plugin:v0.19.0`
- `nvcr.io/nvidia/k8s/container-toolkit:v1.19.0`
- `nvcr.io/nvidia/k8s/dcgm-exporter:4.5.1-4.8.0-distroless`
- `nvcr.io/nvidia/kubevirt-gpu-device-plugin:v1.5.0`
- `vgpu-manager`

### grove

- `ghcr.io/ai-dynamo/grove/grove-operator:v0.1.0-alpha.6`

### k8s-ephemeral-storage-metrics

- `ghcr.io/jmcgrath207/k8s-ephemeral-storage-metrics:1.19.2`

### k8s-nim-operator

- `nvcr.io/nvidia/cloud-native/k8s-nim-operator:v3.1.0`

### kai-scheduler

- `ghcr.io/kai-scheduler/kai-scheduler/crd-upgrader:v0.14.1`
- `ghcr.io/kai-scheduler/kai-scheduler/operator:v0.14.1`

### kgateway

- `cr.kgateway.dev/kgateway-dev/kgateway:v2.0.0`

### kgateway-crds

_No images extracted._

### kube-prometheus-stack

- `docker.io/grafana/grafana:13.0.1`
- `ghcr.io/jkroepke/kube-webhook-certgen:1.8.2`
- `quay.io/kiwigrid/k8s-sidecar:2.7.1`
- `quay.io/prometheus-operator/prometheus-operator:v0.90.1`
- `quay.io/prometheus/alertmanager:v0.32.0`
- `quay.io/prometheus/node-exporter:v1.11.1`
- `quay.io/prometheus/prometheus:v3.11.3`
- `registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.18.0`

### kubeflow-trainer

- `ghcr.io/kubeflow/trainer/trainer-controller-manager:v2.2.0`
- `pytorch/pytorch:2.9.1-cuda12.8-cudnn9-runtime`
- `registry.k8s.io/jobset/jobset:v0.11.0`

### kueue

- `registry.k8s.io/kueue/kueue:v0.17.1`

### network-operator

- `busybox:1.36`
- `nvcr.io/nvidia/cloud-native/network-operator:v26.1.1`
- `nvcr.io/nvidia/doca/doca_telemetry:1.22.5-doca3.1.0-host`
- `nvcr.io/nvidia/mellanox/doca-driver:doca3.2.0-25.10-1.2.8.0-2`
- `nvcr.io/nvidia/mellanox/k8s-rdma-shared-dev-plugin:network-operator-v26.1.0`

### nfd

- `registry.k8s.io/nfd/node-feature-discovery:v0.18.3`

### nodewright-customizations

- `ghcr.io/nvidia/nodewright-packages/nvidia-setup:0.2.2`
- `ghcr.io/nvidia/nodewright-packages/nvidia-tuned:0.3.0`
- `ghcr.io/nvidia/skyhook-packages/nvidia-tuning-gke:0.1.1`
- `ghcr.io/nvidia/skyhook-packages/shellscript:1.1.1`

### nodewright-operator

- `bitnami/kubectl:latest@sha256:1bc359beb3ae3982591349df11db50b0917b0596e8bed8ab9cf0c8a84a3502d1`
- `nvcr.io/nvidia/skyhook/operator:v0.15.0@sha256:09e4f71cca8757818515f9e7dd4b8f47d30c642dc3a7efe1329d5c19efea76b9`
- `quay.io/brancz/kube-rbac-proxy:v0.15.0@sha256:2c7b120590cbe9f634f5099f2cbb91d0b668569023a81505ca124a5c437e7663`

### nvidia-dra-driver-gpu

- `nvcr.io/nvidia/k8s-dra-driver-gpu:v25.12.0`

### nvsentinel

- `ghcr.io/nvidia/nvsentinel/gpu-health-monitor:v1.3.0-dcgm-3.x`
- `ghcr.io/nvidia/nvsentinel/gpu-health-monitor:v1.3.0-dcgm-4.x`
- `ghcr.io/nvidia/nvsentinel/labeler:v1.3.0`
- `ghcr.io/nvidia/nvsentinel/metadata-collector:v1.3.0`
- `ghcr.io/nvidia/nvsentinel/platform-connectors:v1.3.0`
- `ghcr.io/nvidia/nvsentinel/syslog-health-monitor:v1.3.0`

### prometheus-adapter

- `registry.k8s.io/prometheus-adapter/prometheus-adapter:v0.12.0`

