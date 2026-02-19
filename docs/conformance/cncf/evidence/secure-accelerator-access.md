# Secure Accelerator Access

**Generated:** 2026-02-19 19:14:47 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

## Summary

1. **GPU Operator** — ClusterPolicy ready, all DaemonSets running (driver, device-plugin, DCGM, toolkit, validator, MIG manager)
2. **Device Advertisement** — 8x NVIDIA H100 80GB HBM3 GPUs advertised individually via ResourceSlices with unique UUIDs and PCI bus IDs
3. **No Direct Host Access** — Pod volumes contain only `kube-api-access` (projected token). GPU access exclusively through DRA `resourceClaims`
4. **Device Isolation** — Only 1 GPU visible (`/dev/nvidia6`, UUID `GPU-ca1b8386-...`) out of 8 on the node
5. **Result: PASS**

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
NAME                                            READY   STATUS      RESTARTS      AGE   IP               NODE                             NOMINATED NODE   READINESS GATES
gpu-feature-discovery-pjlrw                     1/1     Running     0             20m   100.65.4.71      ip-100-64-171-120.ec2.internal   <none>           <none>
gpu-operator-54f86f694c-wn8tz                   1/1     Running     0             19h   100.64.7.63      ip-100-64-4-149.ec2.internal     <none>           <none>
node-feature-discovery-gc-559d7b578d-btpc6      1/1     Running     0             19h   100.65.168.24    ip-100-64-83-166.ec2.internal    <none>           <none>
node-feature-discovery-master-75765d64b-td98v   1/1     Running     0             19h   100.64.4.111     ip-100-64-6-88.ec2.internal      <none>           <none>
node-feature-discovery-worker-5mlc6             1/1     Running     0             22h   100.64.8.11      ip-100-64-9-88.ec2.internal      <none>           <none>
node-feature-discovery-worker-6xx4q             1/1     Running     0             22h   100.64.7.203     ip-100-64-6-88.ec2.internal      <none>           <none>
node-feature-discovery-worker-bkfmp             1/1     Running     0             21m   100.65.9.193     ip-100-64-171-120.ec2.internal   <none>           <none>
node-feature-discovery-worker-xmhs6             1/1     Running     0             22h   100.65.52.121    ip-100-64-83-166.ec2.internal    <none>           <none>
node-feature-discovery-worker-zm4d9             1/1     Running     0             22h   100.64.5.27      ip-100-64-4-149.ec2.internal     <none>           <none>
nvidia-container-toolkit-daemonset-9cbkl        1/1     Running     0             20m   100.65.145.135   ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-cuda-validator-dv2w2                     0/1     Completed   0             18m   100.65.60.26     ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-dcgm-exporter-hblfm                      1/1     Running     2 (18m ago)   20m   100.65.85.64     ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-dcgm-pp7ng                               1/1     Running     0             20m   100.65.49.189    ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-device-plugin-daemonset-z5wwl            1/1     Running     0             20m   100.65.165.86    ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-driver-daemonset-97gpb                   3/3     Running     0             20m   100.65.233.201   ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-mig-manager-798tp                        1/1     Running     0             17m   100.65.230.163   ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-operator-validator-d4z4j                 1/1     Running     0             20m   100.65.71.124    ip-100-64-171-120.ec2.internal   <none>           <none>
```

### GPU Operator DaemonSets

**GPU operator DaemonSets**
```
$ kubectl get ds -n gpu-operator
NAME                                      DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR                                                          AGE
gpu-feature-discovery                     1         1         1       1            1           nvidia.com/gpu.deploy.gpu-feature-discovery=true                       22h
node-feature-discovery-worker             5         5         5       5            5           <none>                                                                 22h
nvidia-container-toolkit-daemonset        1         1         1       1            1           nvidia.com/gpu.deploy.container-toolkit=true                           22h
nvidia-dcgm                               1         1         1       1            1           nvidia.com/gpu.deploy.dcgm=true                                        22h
nvidia-dcgm-exporter                      1         1         1       1            1           nvidia.com/gpu.deploy.dcgm-exporter=true                               22h
nvidia-device-plugin-daemonset            1         1         1       1            1           nvidia.com/gpu.deploy.device-plugin=true                               22h
nvidia-device-plugin-mps-control-daemon   0         0         0       0            0           nvidia.com/gpu.deploy.device-plugin=true,nvidia.com/mps.capable=true   22h
nvidia-driver-daemonset                   1         1         1       1            1           nvidia.com/gpu.deploy.driver=true                                      22h
nvidia-mig-manager                        1         1         1       1            1           nvidia.com/gpu.deploy.mig-manager=true                                 22h
nvidia-operator-validator                 1         1         1       1            1           nvidia.com/gpu.deploy.operator-validator=true                          22h
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
ip-100-64-171-120.ec2.internal-compute-domain.nvidia.com-8k72n   ip-100-64-171-120.ec2.internal   compute-domain.nvidia.com   ip-100-64-171-120.ec2.internal   18m
ip-100-64-171-120.ec2.internal-gpu.nvidia.com-7npv2              ip-100-64-171-120.ec2.internal   gpu.nvidia.com              ip-100-64-171-120.ec2.internal   18m
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
    creationTimestamp: "2026-02-19T18:56:05Z"
    generateName: ip-100-64-171-120.ec2.internal-compute-domain.nvidia.com-
    generation: 1
    name: ip-100-64-171-120.ec2.internal-compute-domain.nvidia.com-8k72n
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-100-64-171-120.ec2.internal
      uid: a94c3e56-9f0e-42fb-abad-32cd237c6c6b
    resourceVersion: "3895850"
    uid: 01b23c93-bc76-45dd-84af-cfe9dfe67f26
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
    nodeName: ip-100-64-171-120.ec2.internal
    pool:
      generation: 1
      name: ip-100-64-171-120.ec2.internal
      resourceSliceCount: 1
- apiVersion: resource.k8s.io/v1
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2026-02-19T18:56:07Z"
    generateName: ip-100-64-171-120.ec2.internal-gpu.nvidia.com-
    generation: 1
    name: ip-100-64-171-120.ec2.internal-gpu.nvidia.com-7npv2
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-100-64-171-120.ec2.internal
      uid: a94c3e56-9f0e-42fb-abad-32cd237c6c6b
    resourceVersion: "3895867"
    uid: 48aa57b4-983e-45dd-8010-649bf113e94c
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

**Pod spec volumes and resourceClaims**
```
$ kubectl get pod isolation-test -n secure-access-test -o jsonpath={.spec} | python3 -c import sys,json; spec=json.loads(sys.stdin.read()); print('resourceClaims:', json.dumps(spec.get('resourceClaims',[]),indent=2)); print('volumes:', json.dumps(spec.get('volumes',[]),indent=2))
error: unknown shorthand flag: 'c' in -c
See 'kubectl get --help' for usage.
(exit code: 0)
```

**Pod resourceClaims**
```
$ kubectl get pod isolation-test -n secure-access-test -o jsonpath={.spec.resourceClaims}
[{"name":"gpu","resourceClaimName":"isolated-gpu"}]
```

**Pod volumes**
```
$ kubectl get pod isolation-test -n secure-access-test -o jsonpath={.spec.volumes}
[{"name":"kube-api-access-rkwvp","projected":{"defaultMode":420,"sources":[{"serviceAccountToken":{"expirationSeconds":3607,"path":"token"}},{"configMap":{"items":[{"key":"ca.crt","path":"ca.crt"}],"name":"kube-root-ca.crt"}},{"downwardAPI":{"items":[{"fieldRef":{"apiVersion":"v1","fieldPath":"metadata.namespace"},"path":"namespace"}]}}]}}]
```

**ResourceClaim allocation**
```
$ kubectl get resourceclaim isolated-gpu -n secure-access-test -o wide
NAME           STATE     AGE
isolated-gpu   pending   11s
```

### Container GPU Visibility (only allocated GPU visible)

**Isolation test logs**
```
$ kubectl logs isolation-test -n secure-access-test
=== Visible NVIDIA devices ===
crw-rw-rw- 1 root root 195, 254 Feb 19 19:15 /dev/nvidia-modeset
crw-rw-rw- 1 root root 508,   0 Feb 19 19:15 /dev/nvidia-uvm
crw-rw-rw- 1 root root 508,   1 Feb 19 19:15 /dev/nvidia-uvm-tools
crw-rw-rw- 1 root root 195,   6 Feb 19 19:15 /dev/nvidia6
crw-rw-rw- 1 root root 195, 255 Feb 19 19:15 /dev/nvidiactl

=== nvidia-smi output ===
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0)

=== GPU count ===
0, NVIDIA H100 80GB HBM3, GPU-ca1b8386-093b-60cc-349d-c4a38b9124c0

Secure accelerator access test completed
```

**Result: PASS** — GPU access mediated through DRA ResourceClaim. No direct host device mounts. Only allocated GPU visible in container.

## Cleanup

**Delete test namespace**
```
$ kubectl delete namespace secure-access-test --ignore-not-found
namespace "secure-access-test" deleted
```
