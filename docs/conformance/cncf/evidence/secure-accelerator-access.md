# Secure Accelerator Access

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-02-24 20:21:40 UTC
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
cluster-policy   ready    2026-02-18T20:16:26Z
```

### GPU Operator Pods

**GPU operator pods**
```
$ kubectl get pods -n gpu-operator -o wide
NAME                                            READY   STATUS      RESTARTS        AGE     IP               NODE                             NOMINATED NODE   READINESS GATES
gpu-feature-discovery-dh2p9                     1/1     Running     0               5m2s    100.65.4.71      ip-100-64-171-120.ec2.internal   <none>           <none>
gpu-operator-54f86f694c-wn8tz                   1/1     Running     0               5d20h   100.64.7.63      ip-100-64-4-149.ec2.internal     <none>           <none>
node-feature-discovery-gc-559d7b578d-btpc6      1/1     Running     0               5d20h   100.65.168.24    ip-100-64-83-166.ec2.internal    <none>           <none>
node-feature-discovery-master-75765d64b-td98v   1/1     Running     0               5d20h   100.64.4.111     ip-100-64-6-88.ec2.internal      <none>           <none>
node-feature-discovery-worker-5mlc6             1/1     Running     0               6d      100.64.8.11      ip-100-64-9-88.ec2.internal      <none>           <none>
node-feature-discovery-worker-6xx4q             1/1     Running     0               6d      100.64.7.203     ip-100-64-6-88.ec2.internal      <none>           <none>
node-feature-discovery-worker-bkfmp             1/1     Running     1 (5m12s ago)   5d1h    100.65.49.189    ip-100-64-171-120.ec2.internal   <none>           <none>
node-feature-discovery-worker-xmhs6             1/1     Running     0               6d      100.65.52.121    ip-100-64-83-166.ec2.internal    <none>           <none>
node-feature-discovery-worker-zm4d9             1/1     Running     0               6d      100.64.5.27      ip-100-64-4-149.ec2.internal     <none>           <none>
nvidia-container-toolkit-daemonset-fmmhr        1/1     Running     0               5m2s    100.65.145.135   ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-cuda-validator-w9nvg                     0/1     Completed   0               3m3s    100.65.63.246    ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-dcgm-exporter-br2tz                      1/1     Running     1 (2m54s ago)   5m2s    100.65.138.241   ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-dcgm-l29t6                               1/1     Running     0               5m2s    100.65.111.44    ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-device-plugin-daemonset-jzbk9            1/1     Running     0               5m2s    100.65.244.167   ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-driver-daemonset-97gpb                   3/3     Running     3 (5m12s ago)   5d1h    100.65.9.193     ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-mig-manager-hq49r                        1/1     Running     0               5m2s    100.65.254.73    ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-operator-validator-69vkl                 1/1     Running     0               5m2s    100.65.51.102    ip-100-64-171-120.ec2.internal   <none>           <none>
```

### GPU Operator DaemonSets

**GPU operator DaemonSets**
```
$ kubectl get ds -n gpu-operator
NAME                                      DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR                                                          AGE
gpu-feature-discovery                     1         1         1       1            1           nvidia.com/gpu.deploy.gpu-feature-discovery=true                       6d
node-feature-discovery-worker             5         5         5       5            5           <none>                                                                 6d
nvidia-container-toolkit-daemonset        1         1         1       1            1           nvidia.com/gpu.deploy.container-toolkit=true                           6d
nvidia-dcgm                               1         1         1       1            1           nvidia.com/gpu.deploy.dcgm=true                                        6d
nvidia-dcgm-exporter                      1         1         1       1            1           nvidia.com/gpu.deploy.dcgm-exporter=true                               6d
nvidia-device-plugin-daemonset            1         1         1       1            1           nvidia.com/gpu.deploy.device-plugin=true                               6d
nvidia-device-plugin-mps-control-daemon   0         0         0       0            0           nvidia.com/gpu.deploy.device-plugin=true,nvidia.com/mps.capable=true   6d
nvidia-driver-daemonset                   1         1         1       1            1           nvidia.com/gpu.deploy.driver=true                                      6d
nvidia-mig-manager                        1         1         1       1            1           nvidia.com/gpu.deploy.mig-manager=true                                 6d
nvidia-operator-validator                 1         1         1       1            1           nvidia.com/gpu.deploy.operator-validator=true                          6d
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
ip-100-64-171-120.ec2.internal-compute-domain.nvidia.com-76zr9   ip-100-64-171-120.ec2.internal   compute-domain.nvidia.com   ip-100-64-171-120.ec2.internal   3m32s
ip-100-64-171-120.ec2.internal-gpu.nvidia.com-75xvv              ip-100-64-171-120.ec2.internal   gpu.nvidia.com              ip-100-64-171-120.ec2.internal   3m30s
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
    creationTimestamp: "2026-02-24T20:18:17Z"
    generateName: ip-100-64-171-120.ec2.internal-compute-domain.nvidia.com-
    generation: 1
    name: ip-100-64-171-120.ec2.internal-compute-domain.nvidia.com-76zr9
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-100-64-171-120.ec2.internal
      uid: a94c3e56-9f0e-42fb-abad-32cd237c6c6b
    resourceVersion: "6642417"
    uid: 2e9ffac0-5bdd-49b0-8f6a-5a23764608b3
  spec:
    devices:
    - attributes:
        id:
          int: 0
        type:
          string: channel
      name: channel-0
    - attributes:
        id:
          int: 0
        type:
          string: daemon
      name: daemon-0
    driver: compute-domain.nvidia.com
    nodeName: ip-100-64-171-120.ec2.internal
    pool:
      generation: 1
      name: ip-100-64-171-120.ec2.internal
      resourceSliceCount: 1
- apiVersion: resource.k8s.io/v1
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2026-02-24T20:18:19Z"
    generateName: ip-100-64-171-120.ec2.internal-gpu.nvidia.com-
    generation: 1
    name: ip-100-64-171-120.ec2.internal-gpu.nvidia.com-75xvv
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-100-64-171-120.ec2.internal
      uid: a94c3e56-9f0e-42fb-abad-32cd237c6c6b
    resourceVersion: "6642429"
    uid: 83776fa9-badc-49b5-8204-6135190dfd88
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
          string: 0000:75:00.0
        resource.kubernetes.io/pcieRoot:
          string: pci0000:66
        type:
          string: gpu
        uuid:
          string: GPU-f814846a-9bbe-469e-97c3-d037d67c3c32
      capacity:
        memory:
          value: 81559Mi
      name: gpu-2
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
          string: GPU-3cc59718-d7df-49ac-07a3-a6cedfe263c6
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
          string: GPU-71fc8f21-7800-5bb9-53ad-7e6fc93ef15f
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
          string: GPU-dee5c16e-1d0a-cec8-a9ea-f878a4be1b3d
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
          string: GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0
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
          string: GPU-b60b817a-a091-c492-4211-92b276d697e6
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
          string: GPU-22dbdd79-f55a-92a8-aa39-322198e72ed6
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
          string: GPU-289275cb-a907-ab73-9a95-058ae119f62d
      capacity:
        memory:
          value: 81559Mi
      name: gpu-1
    driver: gpu.nvidia.com
    nodeName: ip-100-64-171-120.ec2.internal
    pool:
      generation: 1
      name: ip-100-64-171-120.ec2.internal
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
[{"name":"kube-api-access-xsvnl","projected":{"defaultMode":420,"sources":[{"serviceAccountToken":{"expirationSeconds":3607,"path":"token"}},{"configMap":{"items":[{"key":"ca.crt","path":"ca.crt"}],"name":"kube-root-ca.crt"}},{"downwardAPI":{"items":[{"fieldRef":{"apiVersion":"v1","fieldPath":"metadata.namespace"},"path":"namespace"}]}}]}}]
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
crw-rw-rw- 1 root root 195, 254 Feb 24 20:22 /dev/nvidia-modeset
crw-rw-rw- 1 root root 508,   0 Feb 24 20:22 /dev/nvidia-uvm
crw-rw-rw- 1 root root 508,   1 Feb 24 20:22 /dev/nvidia-uvm-tools
crw-rw-rw- 1 root root 195,   2 Feb 24 20:22 /dev/nvidia2
crw-rw-rw- 1 root root 195, 255 Feb 24 20:22 /dev/nvidiactl

=== nvidia-smi output ===
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-f814846a-9bbe-469e-97c3-d037d67c3c32)

=== GPU count ===
0, NVIDIA H100 80GB HBM3, GPU-f814846a-9bbe-469e-97c3-d037d67c3c32

Secure accelerator access test completed
```

**Result: PASS** — GPU access mediated through DRA ResourceClaim. No direct host device mounts. Only allocated GPU visible in container.

## Cleanup

**Delete test namespace**
```
$ kubectl delete namespace secure-access-test --ignore-not-found
namespace "secure-access-test" deleted
```
