# Secure Accelerator Access

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-03-10 03:40:33 UTC
**Kubernetes Version:** v1.35
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
cluster-policy   ready    2026-03-10T03:25:45Z
```

### GPU Operator Pods

**GPU operator pods**
```
$ kubectl get pods -n gpu-operator -o wide
NAME                                             READY   STATUS      RESTARTS   AGE   IP             NODE                           NOMINATED NODE   READINESS GATES
gpu-feature-discovery-6rcxf                      1/1     Running     0          14m   10.0.224.30    ip-10-0-206-2.ec2.internal     <none>           <none>
gpu-feature-discovery-8jhh7                      1/1     Running     0          14m   10.0.224.179   ip-10-0-171-111.ec2.internal   <none>           <none>
gpu-operator-6bf99d6478-r55t5                    1/1     Running     0          14m   10.0.6.44      ip-10-0-6-165.ec2.internal     <none>           <none>
node-feature-discovery-gc-5495c9b5c9-5jhtb       1/1     Running     0          14m   10.0.4.105     ip-10-0-6-165.ec2.internal     <none>           <none>
node-feature-discovery-master-6f876b9c85-97zcw   1/1     Running     0          14m   10.0.6.62      ip-10-0-6-165.ec2.internal     <none>           <none>
node-feature-discovery-worker-7z8fm              1/1     Running     0          14m   10.0.230.31    ip-10-0-196-144.ec2.internal   <none>           <none>
node-feature-discovery-worker-9s5tc              1/1     Running     0          14m   10.0.154.69    ip-10-0-171-111.ec2.internal   <none>           <none>
node-feature-discovery-worker-vb62k              1/1     Running     0          14m   10.0.189.91    ip-10-0-206-2.ec2.internal     <none>           <none>
nvidia-container-toolkit-daemonset-c49gs         1/1     Running     0          14m   10.0.201.217   ip-10-0-171-111.ec2.internal   <none>           <none>
nvidia-container-toolkit-daemonset-lr895         1/1     Running     0          14m   10.0.182.110   ip-10-0-206-2.ec2.internal     <none>           <none>
nvidia-cuda-validator-9866n                      0/1     Completed   0          12m   10.0.247.169   ip-10-0-206-2.ec2.internal     <none>           <none>
nvidia-cuda-validator-f42hd                      0/1     Completed   0          12m   10.0.143.223   ip-10-0-171-111.ec2.internal   <none>           <none>
nvidia-dcgm-4bq8l                                1/1     Running     0          14m   10.0.145.214   ip-10-0-171-111.ec2.internal   <none>           <none>
nvidia-dcgm-exporter-g2fjs                       1/1     Running     0          14m   10.0.247.52    ip-10-0-206-2.ec2.internal     <none>           <none>
nvidia-dcgm-exporter-wqqqn                       1/1     Running     0          14m   10.0.172.246   ip-10-0-171-111.ec2.internal   <none>           <none>
nvidia-dcgm-xjsqq                                1/1     Running     0          14m   10.0.159.246   ip-10-0-206-2.ec2.internal     <none>           <none>
nvidia-device-plugin-daemonset-5884b             1/1     Running     0          14m   10.0.255.120   ip-10-0-171-111.ec2.internal   <none>           <none>
nvidia-device-plugin-daemonset-kx2zg             1/1     Running     0          14m   10.0.185.249   ip-10-0-206-2.ec2.internal     <none>           <none>
nvidia-driver-daemonset-qc7cg                    3/3     Running     0          14m   10.0.198.38    ip-10-0-171-111.ec2.internal   <none>           <none>
nvidia-driver-daemonset-vvlsc                    3/3     Running     0          14m   10.0.166.43    ip-10-0-206-2.ec2.internal     <none>           <none>
nvidia-mig-manager-4gn76                         1/1     Running     0          14m   10.0.135.89    ip-10-0-171-111.ec2.internal   <none>           <none>
nvidia-mig-manager-8s9wj                         1/1     Running     0          14m   10.0.253.166   ip-10-0-206-2.ec2.internal     <none>           <none>
nvidia-operator-validator-twprm                  1/1     Running     0          14m   10.0.231.53    ip-10-0-171-111.ec2.internal   <none>           <none>
nvidia-operator-validator-vwnsb                  1/1     Running     0          14m   10.0.194.119   ip-10-0-206-2.ec2.internal     <none>           <none>
```

### GPU Operator DaemonSets

**GPU operator DaemonSets**
```
$ kubectl get ds -n gpu-operator
NAME                                      DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR                                                          AGE
gpu-feature-discovery                     2         2         2       2            2           nvidia.com/gpu.deploy.gpu-feature-discovery=true                       14m
node-feature-discovery-worker             3         3         3       3            3           <none>                                                                 14m
nvidia-container-toolkit-daemonset        2         2         2       2            2           nvidia.com/gpu.deploy.container-toolkit=true                           14m
nvidia-dcgm                               2         2         2       2            2           nvidia.com/gpu.deploy.dcgm=true                                        14m
nvidia-dcgm-exporter                      2         2         2       2            2           nvidia.com/gpu.deploy.dcgm-exporter=true                               14m
nvidia-device-plugin-daemonset            2         2         2       2            2           nvidia.com/gpu.deploy.device-plugin=true                               14m
nvidia-device-plugin-mps-control-daemon   0         0         0       0            0           nvidia.com/gpu.deploy.device-plugin=true,nvidia.com/mps.capable=true   14m
nvidia-driver-daemonset                   2         2         2       2            2           nvidia.com/gpu.deploy.driver=true                                      14m
nvidia-mig-manager                        2         2         2       2            2           nvidia.com/gpu.deploy.mig-manager=true                                 14m
nvidia-operator-validator                 2         2         2       2            2           nvidia.com/gpu.deploy.operator-validator=true                          14m
```

## DRA-Mediated GPU Access

GPU access is provided through DRA ResourceClaims (`resource.k8s.io/v1`), not through
direct `hostPath` volume mounts to `/dev/nvidia*`. The DRA driver advertises individual
GPU devices via ResourceSlices, and pods request access through ResourceClaims.

### ResourceSlices (Device Advertisement)

**ResourceSlices**
```
$ kubectl get resourceslices -o wide
NAME                                                           NODE                           DRIVER                      POOL                           AGE
ip-10-0-171-111.ec2.internal-compute-domain.nvidia.com-q9xqc   ip-10-0-171-111.ec2.internal   compute-domain.nvidia.com   ip-10-0-171-111.ec2.internal   11m
ip-10-0-171-111.ec2.internal-gpu.nvidia.com-7cbz2              ip-10-0-171-111.ec2.internal   gpu.nvidia.com              ip-10-0-171-111.ec2.internal   11m
ip-10-0-206-2.ec2.internal-compute-domain.nvidia.com-2n2cq     ip-10-0-206-2.ec2.internal     compute-domain.nvidia.com   ip-10-0-206-2.ec2.internal     11m
ip-10-0-206-2.ec2.internal-gpu.nvidia.com-79gvw                ip-10-0-206-2.ec2.internal     gpu.nvidia.com              ip-10-0-206-2.ec2.internal     11m
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
    creationTimestamp: "2026-03-10T03:29:20Z"
    generateName: ip-10-0-171-111.ec2.internal-compute-domain.nvidia.com-
    generation: 2
    name: ip-10-0-171-111.ec2.internal-compute-domain.nvidia.com-q9xqc
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-10-0-171-111.ec2.internal
      uid: fef55be3-f566-47c8-8bb8-52c117cb3855
    resourceVersion: "1169500"
    uid: 8087c1b4-71e0-42c3-9f74-12629e2ee5b5
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
    nodeName: ip-10-0-171-111.ec2.internal
    pool:
      generation: 1
      name: ip-10-0-171-111.ec2.internal
      resourceSliceCount: 1
- apiVersion: resource.k8s.io/v1
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2026-03-10T03:29:22Z"
    generateName: ip-10-0-171-111.ec2.internal-gpu.nvidia.com-
    generation: 2
    name: ip-10-0-171-111.ec2.internal-gpu.nvidia.com-7cbz2
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-10-0-171-111.ec2.internal
      uid: fef55be3-f566-47c8-8bb8-52c117cb3855
    resourceVersion: "1169562"
    uid: 3441669c-08c4-43ff-9b83-42c5f3dddcff
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
          string: 0000:64:00.0
        resource.kubernetes.io/pcieRoot:
          string: pci0000:55
        type:
          string: gpu
        uuid:
          string: GPU-bc5610b9-79c8-fedd-8899-07539c7f868a
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
          string: GPU-fbc2c554-4d37-8938-0032-f923bad0f716
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
          string: GPU-a65a773d-52bb-bcc1-a8ee-f78c3faa2e2d
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
          string: GPU-82e45d1b-1618-559f-144c-eab51545030b
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
          string: GPU-39e28159-8c62-ee71-64db-b748edd61e15
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
          string: GPU-e64d69ca-b4b3-59b2-e78c-94f26c4db365
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
          string: GPU-04d228d3-3b5a-3534-f5cf-969706647d56
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
          string: GPU-c4529c8d-69c4-b61d-e0bc-7b2460096005
      capacity:
        memory:
          value: 81559Mi
      name: gpu-0
    driver: gpu.nvidia.com
    nodeName: ip-10-0-171-111.ec2.internal
    pool:
      generation: 1
      name: ip-10-0-171-111.ec2.internal
      resourceSliceCount: 1
- apiVersion: resource.k8s.io/v1
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2026-03-10T03:29:19Z"
    generateName: ip-10-0-206-2.ec2.internal-compute-domain.nvidia.com-
    generation: 1
    name: ip-10-0-206-2.ec2.internal-compute-domain.nvidia.com-2n2cq
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-10-0-206-2.ec2.internal
      uid: b171b90a-eb8f-4662-bd0d-2055b634dc98
    resourceVersion: "1168846"
    uid: 3eca27ae-5231-4845-8407-1e24fd9b5683
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
    nodeName: ip-10-0-206-2.ec2.internal
    pool:
      generation: 1
      name: ip-10-0-206-2.ec2.internal
      resourceSliceCount: 1
- apiVersion: resource.k8s.io/v1
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2026-03-10T03:29:21Z"
    generateName: ip-10-0-206-2.ec2.internal-gpu.nvidia.com-
    generation: 2
    name: ip-10-0-206-2.ec2.internal-gpu.nvidia.com-79gvw
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-10-0-206-2.ec2.internal
      uid: b171b90a-eb8f-4662-bd0d-2055b634dc98
    resourceVersion: "1169576"
    uid: 0b3dc1d8-a1ba-4fae-894b-cb90e62ed783
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
          string: GPU-dbabb552-a092-0ca9-0580-8d4fe378eb02
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
          string: GPU-5342927e-e180-84f1-55ba-257f1cbd3ba4
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
          string: GPU-95085215-739e-e7c6-4011-8dbe004af8c3
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
          string: GPU-a7b658ad-f23e-cea9-2523-569d521700bf
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
          string: GPU-1e9a0e94-769a-b1e6-36f7-9296e286ef90
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
          string: GPU-16b2cd36-9dbe-3ee7-0810-07b330e36e04
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
          string: GPU-92da0328-2f33-b563-d577-9d2b9f21f280
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
          string: GPU-184dab49-47ce-eeec-2239-3e03fbd4c002
      capacity:
        memory:
          value: 81559Mi
      name: gpu-1
    driver: gpu.nvidia.com
    nodeName: ip-10-0-206-2.ec2.internal
    pool:
      generation: 1
      name: ip-10-0-206-2.ec2.internal
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
[{"name":"kube-api-access-dl259","projected":{"defaultMode":420,"sources":[{"serviceAccountToken":{"expirationSeconds":3607,"path":"token"}},{"configMap":{"items":[{"key":"ca.crt","path":"ca.crt"}],"name":"kube-root-ca.crt"}},{"downwardAPI":{"items":[{"fieldRef":{"apiVersion":"v1","fieldPath":"metadata.namespace"},"path":"namespace"}]}}]}}]
```

**ResourceClaim allocation**
```
$ kubectl get resourceclaim isolated-gpu -n secure-access-test -o wide
NAME           STATE     AGE
isolated-gpu   pending   12s
```

> **Note:** ResourceClaim may show `pending` after pod completion because the DRA controller deallocates claims when the consuming pod terminates. The pod logs below confirm GPU isolation was enforced during execution.

### Container GPU Visibility (only allocated GPU visible)

**Isolation test logs**
```
$ kubectl logs isolation-test -n secure-access-test
=== Visible NVIDIA devices ===
crw-rw-rw- 1 root root 195, 254 Mar 10 03:40 /dev/nvidia-modeset
crw-rw-rw- 1 root root 507,   0 Mar 10 03:40 /dev/nvidia-uvm
crw-rw-rw- 1 root root 507,   1 Mar 10 03:40 /dev/nvidia-uvm-tools
crw-rw-rw- 1 root root 195,   1 Mar 10 03:40 /dev/nvidia1
crw-rw-rw- 1 root root 195, 255 Mar 10 03:40 /dev/nvidiactl

=== nvidia-smi output ===
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-bc5610b9-79c8-fedd-8899-07539c7f868a)

=== GPU count ===
0, NVIDIA H100 80GB HBM3, GPU-bc5610b9-79c8-fedd-8899-07539c7f868a

Secure accelerator access test completed
```

**Result: PASS** — GPU access mediated through DRA ResourceClaim. No direct host device mounts. Only allocated GPU visible in container.

## Cleanup

**Delete test namespace**
```
$ cleanup_ns secure-access-test

```
