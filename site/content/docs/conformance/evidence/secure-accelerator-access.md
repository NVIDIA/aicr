---
title: "Secure Accelerator Access"
linkTitle: "Secure Accelerator Access"
weight: 30
description: "Secure accelerator access conformance evidence"
---


**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-03-06 19:38:16 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates that GPU access is mediated through Kubernetes APIs (DRA ResourceClaims
and GPU Operator), not via direct host device mounts. This ensures proper isolation,
access control, and auditability of accelerator usage.

## GPU Operator Health

### ClusterPolicy

**ClusterPolicy status**
```
$ kubectl get clusterpolicy -o wide
NAME             STATUS   AGE
cluster-policy   ready    2026-03-04T22:42:37Z
```

### GPU Operator Pods

**GPU operator pods**
```
$ kubectl get pods -n gpu-operator -o wide
NAME                                            READY   STATUS      RESTARTS   AGE   IP               NODE                             NOMINATED NODE   READINESS GATES
gpu-feature-discovery-8ll95                     1/1     Running     0          44h   10.0.0.10    node-a.example.internal   <none>           <none>
gpu-operator-786cd6c97d-ltrx4                   1/1     Running     0          44h   10.0.0.10     node-a.example.internal      <none>           <none>
node-feature-discovery-gc-bc77948b7-c4s4w       1/1     Running     0          44h   10.0.0.10     node-a.example.internal      <none>           <none>
node-feature-discovery-master-69bb75cbf-w5d79   1/1     Running     0          44h   10.0.0.10      node-a.example.internal      <none>           <none>
node-feature-discovery-worker-n2s2p             1/1     Running     0          44h   10.0.0.10     node-a.example.internal   <none>           <none>
nvidia-container-toolkit-daemonset-lvc26        1/1     Running     0          44h   10.0.0.10    node-a.example.internal   <none>           <none>
nvidia-cuda-validator-dcdzt                     0/1     Completed   0          44h   10.0.0.10    node-a.example.internal   <none>           <none>
nvidia-dcgm-bn89w                               1/1     Running     0          44h   10.0.0.10    node-a.example.internal   <none>           <none>
nvidia-dcgm-exporter-zfgtq                      1/1     Running     0          44h   10.0.0.10    node-a.example.internal   <none>           <none>
nvidia-device-plugin-daemonset-4vrtv            1/1     Running     0          44h   10.0.0.10    node-a.example.internal   <none>           <none>
nvidia-driver-daemonset-ppwxt                   3/3     Running     0          44h   10.0.0.10   node-a.example.internal   <none>           <none>
nvidia-mig-manager-95lbg                        1/1     Running     0          44h   10.0.0.10    node-a.example.internal   <none>           <none>
nvidia-operator-validator-gwj97                 1/1     Running     0          44h   10.0.0.10    node-a.example.internal   <none>           <none>
```

### GPU Operator DaemonSets

**GPU operator DaemonSets**
```
$ kubectl get ds -n gpu-operator
NAME                                      DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR                                                          AGE
gpu-feature-discovery                     1         1         1       1            1           nvidia.com/gpu.deploy.gpu-feature-discovery=true                       44h
node-feature-discovery-worker             1         1         1       1            1           <none>                                                                 44h
nvidia-container-toolkit-daemonset        1         1         1       1            1           nvidia.com/gpu.deploy.container-toolkit=true                           44h
nvidia-dcgm                               1         1         1       1            1           nvidia.com/gpu.deploy.dcgm=true                                        44h
nvidia-dcgm-exporter                      1         1         1       1            1           nvidia.com/gpu.deploy.dcgm-exporter=true                               44h
nvidia-device-plugin-daemonset            1         1         1       1            1           nvidia.com/gpu.deploy.device-plugin=true                               44h
nvidia-device-plugin-mps-control-daemon   0         0         0       0            0           nvidia.com/gpu.deploy.device-plugin=true,nvidia.com/mps.capable=true   44h
nvidia-driver-daemonset                   1         1         1       1            1           nvidia.com/gpu.deploy.driver=true                                      44h
nvidia-mig-manager                        1         1         1       1            1           nvidia.com/gpu.deploy.mig-manager=true                                 44h
nvidia-operator-validator                 1         1         1       1            1           nvidia.com/gpu.deploy.operator-validator=true                          44h
```

## DRA-Mediated GPU Access

GPU access is provided through DRA ResourceClaims (`resource.k8s.io/v1`), not through
direct `hostPath` volume mounts to `/dev/nvidia*`. The DRA driver advertises individual
GPU devices via ResourceSlices, and pods request access through ResourceClaims.

### ResourceSlices (Device Advertisement)

**ResourceSlices**
```
$ kubectl get resourceslices -o wide
NAME                                                             NODE                             DRIVER                      POOL                             AGE
node-a.example.internal-compute-domain.nvidia.com-bbg8t   node-a.example.internal   compute-domain.nvidia.com   node-a.example.internal   44h
node-a.example.internal-gpu.nvidia.com-sgw47              node-a.example.internal   gpu.nvidia.com              node-a.example.internal   44h
```

### GPU Device Details

**GPU devices in ResourceSlice**
```
$ kubectl get resourceslices -o yaml
apiVersion: v1
items:
- apiVersion: resource.k8s.io/v1
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2026-03-04T22:44:34Z"
    generateName: node-a.example.internal-compute-domain.nvidia.com-
    generation: 1
    name: node-a.example.internal-compute-domain.nvidia.com-bbg8t
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: node-a.example.internal
      uid: 2e8f0172-e1d7-4713-9160-a9f215925a19
    resourceVersion: "11088653"
    uid: 0308025e-5f16-44ed-9e0c-e9b9aa51a0ef
  spec:
    devices:
    - attributes:
        id:
          int: 0
        type:
          string: daemon
      name: daemon-0
    - attributes:
        id:
          int: 0
        type:
          string: channel
      name: channel-0
    driver: compute-domain.nvidia.com
    nodeName: node-a.example.internal
    pool:
      generation: 1
      name: node-a.example.internal
      resourceSliceCount: 1
- apiVersion: resource.k8s.io/v1
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2026-03-04T22:44:35Z"
    generateName: node-a.example.internal-gpu.nvidia.com-
    generation: 1
    name: node-a.example.internal-gpu.nvidia.com-sgw47
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: node-a.example.internal
      uid: 2e8f0172-e1d7-4713-9160-a9f215925a19
    resourceVersion: "11088663"
    uid: bde9cab9-6c52-4d1b-aed5-3fcd2241708b
  spec:
    devices:
    - attributes:
        addressingMode:
          string: HMM
        architecture:
          string: Hopper
        brand:
          string: Nvidia
        cudaComputeCapability:
          version: 9.0.0
        cudaDriverVersion:
          version: 13.0.0
        driverVersion:
          version: 580.105.8
        productName:
          string: NVIDIA H100 80GB HBM3
        resource.kubernetes.io/pciBusID:
          string: 0000:86:00.0
        resource.kubernetes.io/pcieRoot:
          string: pci0000:77
        type:
          string: gpu
        uuid:
          string: GPU-e69a4117-e5f9-04a7-d170-aafac6a7e692
      capacity:
        memory:
          value: 81559Mi
      name: gpu-3
    - attributes:
        addressingMode:
          string: HMM
        architecture:
          string: Hopper
        brand:
          string: Nvidia
        cudaComputeCapability:
          version: 9.0.0
        cudaDriverVersion:
          version: 13.0.0
        driverVersion:
          version: 580.105.8
        productName:
          string: NVIDIA H100 80GB HBM3
        resource.kubernetes.io/pciBusID:
          string: 0000:97:00.0
        resource.kubernetes.io/pcieRoot:
          string: pci0000:88
        type:
          string: gpu
        uuid:
          string: GPU-eaef2c36-d7aa-5f60-37bc-3e0a53de34ff
      capacity:
        memory:
          value: 81559Mi
      name: gpu-4
    - attributes:
        addressingMode:
          string: HMM
        architecture:
          string: Hopper
        brand:
          string: Nvidia
        cudaComputeCapability:
          version: 9.0.0
        cudaDriverVersion:
          version: 13.0.0
        driverVersion:
          version: 580.105.8
        productName:
          string: NVIDIA H100 80GB HBM3
        resource.kubernetes.io/pciBusID:
          string: 0000:a8:00.0
        resource.kubernetes.io/pcieRoot:
          string: pci0000:99
        type:
          string: gpu
        uuid:
          string: GPU-1af5cfae-1878-a06c-5dc0-c16e5cf11a20
      capacity:
        memory:
          value: 81559Mi
      name: gpu-5
    - attributes:
        addressingMode:
          string: HMM
        architecture:
          string: Hopper
        brand:
          string: Nvidia
        cudaComputeCapability:
          version: 9.0.0
        cudaDriverVersion:
          version: 13.0.0
        driverVersion:
          version: 580.105.8
        productName:
          string: NVIDIA H100 80GB HBM3
        resource.kubernetes.io/pciBusID:
          string: 0000:b9:00.0
        resource.kubernetes.io/pcieRoot:
          string: pci0000:aa
        type:
          string: gpu
        uuid:
          string: GPU-a0e6d978-c416-5df8-1ab9-eb27b31eab35
      capacity:
        memory:
          value: 81559Mi
      name: gpu-6
    - attributes:
        addressingMode:
          string: HMM
        architecture:
          string: Hopper
        brand:
          string: Nvidia
        cudaComputeCapability:
          version: 9.0.0
        cudaDriverVersion:
          version: 13.0.0
        driverVersion:
          version: 580.105.8
        productName:
          string: NVIDIA H100 80GB HBM3
        resource.kubernetes.io/pciBusID:
          string: 0000:ca:00.0
        resource.kubernetes.io/pcieRoot:
          string: pci0000:bb
        type:
          string: gpu
        uuid:
          string: GPU-bd2999a7-7982-6643-fa9e-2d1a2cd7be27
      capacity:
        memory:
          value: 81559Mi
      name: gpu-7
    - attributes:
        addressingMode:
          string: HMM
        architecture:
          string: Hopper
        brand:
          string: Nvidia
        cudaComputeCapability:
          version: 9.0.0
        cudaDriverVersion:
          version: 13.0.0
        driverVersion:
          version: 580.105.8
        productName:
          string: NVIDIA H100 80GB HBM3
        resource.kubernetes.io/pciBusID:
          string: "0000:53:00.0"
        resource.kubernetes.io/pcieRoot:
          string: pci0000:44
        type:
          string: gpu
        uuid:
          string: GPU-81d79b08-40a0-40ae-3fc5-82b8ff8b8138
      capacity:
        memory:
          value: 81559Mi
      name: gpu-0
    - attributes:
        addressingMode:
          string: HMM
        architecture:
          string: Hopper
        brand:
          string: Nvidia
        cudaComputeCapability:
          version: 9.0.0
        cudaDriverVersion:
          version: 13.0.0
        driverVersion:
          version: 580.105.8
        productName:
          string: NVIDIA H100 80GB HBM3
        resource.kubernetes.io/pciBusID:
          string: 0000:64:00.0
        resource.kubernetes.io/pcieRoot:
          string: pci0000:55
        type:
          string: gpu
        uuid:
          string: GPU-4fc48812-c1c8-3bb7-1313-724533aa7df7
      capacity:
        memory:
          value: 81559Mi
      name: gpu-1
    - attributes:
        addressingMode:
          string: HMM
        architecture:
          string: Hopper
        brand:
          string: Nvidia
        cudaComputeCapability:
          version: 9.0.0
        cudaDriverVersion:
          version: 13.0.0
        driverVersion:
          version: 580.105.8
        productName:
          string: NVIDIA H100 80GB HBM3
        resource.kubernetes.io/pciBusID:
          string: 0000:75:00.0
        resource.kubernetes.io/pcieRoot:
          string: pci0000:66
        type:
          string: gpu
        uuid:
          string: GPU-8d76cfcf-a144-5e43-876b-a4b71f2aecd1
      capacity:
        memory:
          value: 81559Mi
      name: gpu-2
    driver: gpu.nvidia.com
    nodeName: node-a.example.internal
    pool:
      generation: 1
      name: node-a.example.internal
      resourceSliceCount: 1
kind: List
metadata:
  resourceVersion: ""
```

## Device Isolation Verification

Deploy a test pod requesting 1 GPU via ResourceClaim and verify:
1. No `hostPath` volumes to `/dev/nvidia*`
2. Pod spec uses `resourceClaims` (DRA), not `resources.limits` (device plugin)
3. Only the allocated GPU device is visible inside the container

### Pod Spec (no hostPath volumes)

**Pod resourceClaims**
```
$ kubectl get pod isolation-test -n secure-access-test -o jsonpath={.spec.resourceClaims}
[{"name":"gpu","resourceClaimName":"isolated-gpu"}]
```

**Pod volumes (no hostPath)**
```
$ kubectl get pod isolation-test -n secure-access-test -o jsonpath={.spec.volumes}
[{"name":"kube-api-access-w42nb","projected":{"defaultMode":420,"sources":[{"serviceAccountToken":{"expirationSeconds":3607,"path":"token"}},{"configMap":{"items":[{"key":"ca.crt","path":"ca.crt"}],"name":"kube-root-ca.crt"}},{"downwardAPI":{"items":[{"fieldRef":{"apiVersion":"v1","fieldPath":"metadata.namespace"},"path":"namespace"}]}}]}}]
```

**ResourceClaim allocation**
```
$ kubectl get resourceclaim isolated-gpu -n secure-access-test -o wide
NAME           STATE     AGE
isolated-gpu   pending   13s
```

### Container GPU Visibility (only allocated GPU visible)

**Isolation test logs**
```
$ kubectl logs isolation-test -n secure-access-test
=== Visible NVIDIA devices ===
crw-rw-rw- 1 root root 195, 254 Mar  6 19:38 /dev/nvidia-modeset
crw-rw-rw- 1 root root 507,   0 Mar  6 19:38 /dev/nvidia-uvm
crw-rw-rw- 1 root root 507,   1 Mar  6 19:38 /dev/nvidia-uvm-tools
crw-rw-rw- 1 root root 195,   3 Mar  6 19:38 /dev/nvidia3
crw-rw-rw- 1 root root 195, 255 Mar  6 19:38 /dev/nvidiactl

=== nvidia-smi output ===
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-e69a4117-e5f9-04a7-d170-aafac6a7e692)

=== GPU count ===
0, NVIDIA H100 80GB HBM3, GPU-e69a4117-e5f9-04a7-d170-aafac6a7e692

Secure accelerator access test completed
```

**Result: PASS** — GPU access mediated through DRA ResourceClaim. No direct host device mounts. Only allocated GPU visible in container.

## Cleanup

**Delete test namespace**
```
$ cleanup_ns secure-access-test

```
