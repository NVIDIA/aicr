# DRA Support (Dynamic Resource Allocation)

**Cluster:** `EKS / p6e-gb200.36xlarge / NVIDIA-GB200`
**Generated:** 2026-08-09 01:21:56 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates that the cluster supports DRA (resource.k8s.io API group), has a working
DRA driver, and advertises NVIDIA devices via ResourceSlices. Under the DRA GPU
allocation mode a behavioral test additionally allocates a GPU to a pod through a
ResourceClaim; under the device-plugin mode whole GPUs are device-plugin-allocated
and the behavioral ResourceClaim test is not applicable.

## GPU Allocation Mode

**Configured policy (AICR_GPU_ALLOCATION_POLICY):** `<unset>`
**Mode source:** capability detection (no recipe-configured GPU allocation policy)
**Resolved mode:** `device-plugin`

**Detection basis:**
```
DRA not usable: DeviceClass gpu.nvidia.com not found (or query failed — treated as not usable, fail closed)
device plugin usable: Ready, schedulable node(s) with allocatable nvidia.com/gpu [ip-100-64-135-20.ec2.internal=4,ip-100-64-142-208.ec2.internal=4]
note: this detection checks DeviceClass existence, node-local ResourceSlice publication, and scalar allocatable only — full pool-generation/completeness, device-taint, and topology validation is performed by the Go validators (validators/internal/allocmode), not this script
```

## DRA API Enabled

**DRA API resources**
```
$ kubectl api-resources --api-group=resource.k8s.io
NAME                     SHORTNAMES   APIVERSION           NAMESPACED   KIND
deviceclasses                         resource.k8s.io/v1   false        DeviceClass
resourceclaims                        resource.k8s.io/v1   true         ResourceClaim
resourceclaimtemplates                resource.k8s.io/v1   true         ResourceClaimTemplate
resourceslices                        resource.k8s.io/v1   false        ResourceSlice
```

**Served resource.k8s.io version (newest):** `resource.k8s.io/v1`

## DeviceClasses

**DeviceClasses**
```
$ kubectl get deviceclass
NAME                                        AGE
compute-domain-daemon.nvidia.com            26d
compute-domain-default-channel.nvidia.com   26d
```

## DRA Driver Health

**DRA driver pods**
```
$ kubectl get pods -n nvidia-dra-driver -o wide
NAME                                                READY   STATUS    RESTARTS      AGE   IP              NODE                             NOMINATED NODE   READINESS GATES
nvidia-dra-driver-gpu-controller-66f5fbb947-gnxx2   1/1     Running   0             26d   100.64.4.62     ip-100-64-7-176.ec2.internal     <none>           <none>
nvidia-dra-driver-gpu-kubelet-plugin-lj8nq          1/1     Running   1 (26d ago)   26d   100.65.134.98   ip-100-64-142-208.ec2.internal   <none>           <none>
nvidia-dra-driver-gpu-kubelet-plugin-w2w2g          1/1     Running   2 (26d ago)   26d   100.65.168.80   ip-100-64-135-20.ec2.internal    <none>           <none>
```

## Device Advertisement (ResourceSlices)

**ResourceSlices**
```
$ kubectl get resourceslices
NAME                                                              NODE                             DRIVER                      POOL                             AGE
00000-compute-domain.nvidia.com-ip-100-64-135-20.ec2.interhwh98   ip-100-64-135-20.ec2.internal    compute-domain.nvidia.com   ip-100-64-135-20.ec2.internal    26d
00000-compute-domain.nvidia.com-ip-100-64-142-208.ec2.integmr22   ip-100-64-142-208.ec2.internal   compute-domain.nvidia.com   ip-100-64-142-208.ec2.internal   26d
```

**ResourceSlice inventory by NVIDIA driver**
```
gpu.nvidia.com:            0 slice(s)
compute-domain.nvidia.com: 2 slice(s)
```

## GPU Allocation Test

**Behavioral full-GPU allocation: Not applicable** — device-plugin allocation
policy; whole GPUs are device-plugin-allocated (`nvidia.com/gpu` extended
resource), so no full-GPU ResourceClaim test pod is deployed. DRA support is
evidenced by the served resource.k8s.io API and NVIDIA driver ResourceSlice
publication above.

> **Note:** this section validates a subset under the device-plugin mode —
> API served + NVIDIA ResourceSlice publication. Full pool-generation and
> device-taint validation of the slices is performed by the Go validators
> (`aicr validate --phase conformance`, dra-support check), not this script.

**Result: PASS** — resource.k8s.io/v1 is served and NVIDIA DRA driver(s) publish ResourceSlices (gpu.nvidia.com: 0, compute-domain.nvidia.com: 2). Behavioral full-GPU claim test not applicable under the device-plugin allocation policy.
