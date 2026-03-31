# Secure Accelerator Access

**Cluster:** `EKS / p5.48xlarge / NVIDIA-H100-80GB-HBM3`
**Generated:** 2026-04-01 23:14:45 UTC
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
cluster-policy   ready    2026-04-01T22:12:51Z
```

### GPU Operator Pods

**GPU operator pods**
```
$ kubectl get pods -n gpu-operator -o wide
NAME                                             READY   STATUS      RESTARTS   AGE   IP             NODE                           NOMINATED NODE   READINESS GATES
gpu-feature-discovery-bvjjh                      1/1     Running     0          61m   10.0.218.175   ip-10-0-251-220.ec2.internal   <none>           <none>
gpu-feature-discovery-q4k8g                      1/1     Running     0          61m   10.0.133.127   ip-10-0-180-136.ec2.internal   <none>           <none>
gpu-operator-6bf99d6478-lpll4                    1/1     Running     0          61m   10.0.4.84      ip-10-0-7-209.ec2.internal     <none>           <none>
node-feature-discovery-gc-5495c9b5c9-5lv2g       1/1     Running     0          61m   10.0.6.61      ip-10-0-7-209.ec2.internal     <none>           <none>
node-feature-discovery-master-6f876b9c85-b7wlm   1/1     Running     0          61m   10.0.6.161     ip-10-0-7-209.ec2.internal     <none>           <none>
node-feature-discovery-worker-lrn2p              1/1     Running     0          61m   10.0.212.66    ip-10-0-251-220.ec2.internal   <none>           <none>
node-feature-discovery-worker-srp76              1/1     Running     0          61m   10.0.231.205   ip-10-0-180-136.ec2.internal   <none>           <none>
node-feature-discovery-worker-svrbw              1/1     Running     0          61m   10.0.201.87    ip-10-0-184-187.ec2.internal   <none>           <none>
nvidia-container-toolkit-daemonset-2kj4m         1/1     Running     0          61m   10.0.236.177   ip-10-0-180-136.ec2.internal   <none>           <none>
nvidia-container-toolkit-daemonset-98f25         1/1     Running     0          61m   10.0.157.16    ip-10-0-251-220.ec2.internal   <none>           <none>
nvidia-cuda-validator-cpnk4                      0/1     Completed   0          59m   10.0.146.2     ip-10-0-180-136.ec2.internal   <none>           <none>
nvidia-cuda-validator-l665p                      0/1     Completed   0          59m   10.0.247.132   ip-10-0-251-220.ec2.internal   <none>           <none>
nvidia-dcgm-bwb6w                                1/1     Running     0          61m   10.0.129.30    ip-10-0-251-220.ec2.internal   <none>           <none>
nvidia-dcgm-exporter-2xrln                       1/1     Running     0          61m   10.0.187.45    ip-10-0-180-136.ec2.internal   <none>           <none>
nvidia-dcgm-exporter-sscnw                       1/1     Running     0          61m   10.0.147.205   ip-10-0-251-220.ec2.internal   <none>           <none>
nvidia-dcgm-gdm9j                                1/1     Running     0          61m   10.0.130.151   ip-10-0-180-136.ec2.internal   <none>           <none>
nvidia-device-plugin-daemonset-5dmkr             1/1     Running     0          61m   10.0.170.117   ip-10-0-180-136.ec2.internal   <none>           <none>
nvidia-device-plugin-daemonset-tg9x2             1/1     Running     0          61m   10.0.169.151   ip-10-0-251-220.ec2.internal   <none>           <none>
nvidia-driver-daemonset-9xv78                    3/3     Running     0          61m   10.0.163.144   ip-10-0-251-220.ec2.internal   <none>           <none>
nvidia-driver-daemonset-fbvmz                    3/3     Running     0          61m   10.0.147.204   ip-10-0-180-136.ec2.internal   <none>           <none>
nvidia-mig-manager-6565z                         1/1     Running     0          58m   10.0.243.110   ip-10-0-180-136.ec2.internal   <none>           <none>
nvidia-mig-manager-jm8tl                         1/1     Running     0          58m   10.0.191.228   ip-10-0-251-220.ec2.internal   <none>           <none>
nvidia-operator-validator-bpg4w                  1/1     Running     0          61m   10.0.160.53    ip-10-0-251-220.ec2.internal   <none>           <none>
nvidia-operator-validator-mws7n                  1/1     Running     0          61m   10.0.247.220   ip-10-0-180-136.ec2.internal   <none>           <none>
```

### GPU Operator DaemonSets

**GPU operator DaemonSets**
```
$ kubectl get ds -n gpu-operator
NAME                                      DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR                                                          AGE
gpu-feature-discovery                     2         2         2       2            2           nvidia.com/gpu.deploy.gpu-feature-discovery=true                       61m
node-feature-discovery-worker             3         3         3       3            3           <none>                                                                 61m
nvidia-container-toolkit-daemonset        2         2         2       2            2           nvidia.com/gpu.deploy.container-toolkit=true                           61m
nvidia-dcgm                               2         2         2       2            2           nvidia.com/gpu.deploy.dcgm=true                                        61m
nvidia-dcgm-exporter                      2         2         2       2            2           nvidia.com/gpu.deploy.dcgm-exporter=true                               61m
nvidia-device-plugin-daemonset            2         2         2       2            2           nvidia.com/gpu.deploy.device-plugin=true                               61m
nvidia-device-plugin-mps-control-daemon   0         0         0       0            0           nvidia.com/gpu.deploy.device-plugin=true,nvidia.com/mps.capable=true   61m
nvidia-driver-daemonset                   2         2         2       2            2           nvidia.com/gpu.deploy.driver=true                                      61m
nvidia-mig-manager                        2         2         2       2            2           nvidia.com/gpu.deploy.mig-manager=true                                 61m
nvidia-operator-validator                 2         2         2       2            2           nvidia.com/gpu.deploy.operator-validator=true                          61m
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
ip-10-0-180-136.ec2.internal-compute-domain.nvidia.com-kfxd7   ip-10-0-180-136.ec2.internal   compute-domain.nvidia.com   ip-10-0-180-136.ec2.internal   60m
ip-10-0-180-136.ec2.internal-gpu.nvidia.com-8w29z              ip-10-0-180-136.ec2.internal   gpu.nvidia.com              ip-10-0-180-136.ec2.internal   59m
ip-10-0-251-220.ec2.internal-compute-domain.nvidia.com-btqsj   ip-10-0-251-220.ec2.internal   compute-domain.nvidia.com   ip-10-0-251-220.ec2.internal   60m
ip-10-0-251-220.ec2.internal-gpu.nvidia.com-qwdqr              ip-10-0-251-220.ec2.internal   gpu.nvidia.com              ip-10-0-251-220.ec2.internal   59m
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
    creationTimestamp: "2026-04-01T22:14:50Z"
    generateName: ip-10-0-180-136.ec2.internal-compute-domain.nvidia.com-
    generation: 1
    name: ip-10-0-180-136.ec2.internal-compute-domain.nvidia.com-kfxd7
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-10-0-180-136.ec2.internal
      uid: c01459a2-a385-4843-bc1f-582d283ea94e
    resourceVersion: "101864746"
    uid: 84642059-2fb9-484f-bb98-7e5ae1802eba
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
    nodeName: ip-10-0-180-136.ec2.internal
    pool:
      generation: 1
      name: ip-10-0-180-136.ec2.internal
      resourceSliceCount: 1
- apiVersion: resource.k8s.io/v1
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2026-04-01T22:14:52Z"
    generateName: ip-10-0-180-136.ec2.internal-gpu.nvidia.com-
    generation: 2
    name: ip-10-0-180-136.ec2.internal-gpu.nvidia.com-8w29z
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-10-0-180-136.ec2.internal
      uid: c01459a2-a385-4843-bc1f-582d283ea94e
    resourceVersion: "101865710"
    uid: 89a1966f-5c3f-4664-a5b7-b348a122db07
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
          string: "0000:53:00.0"
        resource.kubernetes.io/pcieRoot:
          string: pci0000:44
        type:
          string: gpu
        uuid:
          string: GPU-15704b32-f531-14ce-0530-1ac21e4b68e6
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
          string: GPU-edc718f8-e593-6468-b9f9-563d508366ed
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
          string: GPU-e2d9b65e-98cb-5b7a-90f0-e0336573f9e2
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
          string: GPU-3a325419-de5f-778f-cf4e-fe7290362ac5
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
          string: GPU-275ad37d-ebd6-4cf6-3867-0499ba033a12
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
          string: GPU-3cab564d-1f63-674b-a831-024600bf985c
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
          string: GPU-d0f25a6f-9a3f-61b9-c128-3d14759651d7
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
          string: GPU-9bc10e9a-e27e-652b-9a1e-e84f7e446206
      capacity:
        memory:
          value: 81559Mi
      name: gpu-7
    driver: gpu.nvidia.com
    nodeName: ip-10-0-180-136.ec2.internal
    pool:
      generation: 1
      name: ip-10-0-180-136.ec2.internal
      resourceSliceCount: 1
- apiVersion: resource.k8s.io/v1
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2026-04-01T22:14:51Z"
    generateName: ip-10-0-251-220.ec2.internal-compute-domain.nvidia.com-
    generation: 1
    name: ip-10-0-251-220.ec2.internal-compute-domain.nvidia.com-btqsj
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-10-0-251-220.ec2.internal
      uid: d55d06fd-ee55-4525-b7da-393b71669e8f
    resourceVersion: "101864753"
    uid: af18d2bf-b15f-43cb-8d2b-a49098f4f5bd
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
    nodeName: ip-10-0-251-220.ec2.internal
    pool:
      generation: 1
      name: ip-10-0-251-220.ec2.internal
      resourceSliceCount: 1
- apiVersion: resource.k8s.io/v1
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2026-04-01T22:14:52Z"
    generateName: ip-10-0-251-220.ec2.internal-gpu.nvidia.com-
    generation: 2
    name: ip-10-0-251-220.ec2.internal-gpu.nvidia.com-qwdqr
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: ip-10-0-251-220.ec2.internal
      uid: d55d06fd-ee55-4525-b7da-393b71669e8f
    resourceVersion: "101865689"
    uid: 48e7fc88-8ff6-4c50-9e74-8755d19ede37
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
          string: 0000:ca:00.0
        resource.kubernetes.io/pcieRoot:
          string: pci0000:bb
        type:
          string: gpu
        uuid:
          string: GPU-530bd4b0-238b-f0c2-b496-63595812bca8
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
          string: GPU-3f048793-8751-030e-5870-ebbd2b10cef2
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
          string: GPU-cc644abe-17e4-7cb7-500d-ed8c09aea2fb
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
          string: GPU-8d0b1081-9549-2b14-7e01-b4a725873c21
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
          string: GPU-38bbfee9-dc95-ffb5-4034-f9a6c82a45bb
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
          string: GPU-24087b69-8889-6b23-feeb-2905664fbcbf
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
          string: GPU-d2f75162-e86d-0da0-0af4-3fa0b80038cd
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
          string: GPU-b00fe5f9-5832-19d6-0276-28d8630f0f4b
      capacity:
        memory:
          value: 81559Mi
      name: gpu-6
    driver: gpu.nvidia.com
    nodeName: ip-10-0-251-220.ec2.internal
    pool:
      generation: 1
      name: ip-10-0-251-220.ec2.internal
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
[{"name":"kube-api-access-vk49g","projected":{"defaultMode":420,"sources":[{"serviceAccountToken":{"expirationSeconds":3607,"path":"token"}},{"configMap":{"items":[{"key":"ca.crt","path":"ca.crt"}],"name":"kube-root-ca.crt"}},{"downwardAPI":{"items":[{"fieldRef":{"apiVersion":"v1","fieldPath":"metadata.namespace"},"path":"namespace"}]}}]}}]
```

**ResourceClaim allocation**
```
$ kubectl get resourceclaim isolated-gpu -n secure-access-test -o wide
NAME           STATE     AGE
isolated-gpu   pending   13s
```

> **Note:** ResourceClaim may show `pending` after pod completion because the DRA controller deallocates claims when the consuming pod terminates. The pod logs below confirm GPU isolation was enforced during execution.

### Container GPU Visibility (only allocated GPU visible)

**Isolation test logs**
```
$ kubectl logs isolation-test -n secure-access-test
=== Visible NVIDIA devices ===
crw-rw-rw- 1 root root 195, 254 Apr  1 23:14 /dev/nvidia-modeset
crw-rw-rw- 1 root root 507,   0 Apr  1 23:14 /dev/nvidia-uvm
crw-rw-rw- 1 root root 507,   1 Apr  1 23:14 /dev/nvidia-uvm-tools
crw-rw-rw- 1 root root 195,   7 Apr  1 23:14 /dev/nvidia7
crw-rw-rw- 1 root root 195, 255 Apr  1 23:14 /dev/nvidiactl

=== nvidia-smi output ===
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-530bd4b0-238b-f0c2-b496-63595812bca8)

=== GPU count ===
0, NVIDIA H100 80GB HBM3, GPU-530bd4b0-238b-f0c2-b496-63595812bca8

Secure accelerator access test completed
```

**Result: PASS** — GPU access mediated through DRA ResourceClaim. No direct host device mounts. Only allocated GPU visible in container.

## Cleanup

**Delete test namespace**
```
$ cleanup_ns secure-access-test

```
