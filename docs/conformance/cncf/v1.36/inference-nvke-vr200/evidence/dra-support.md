# DRA Support (Dynamic Resource Allocation)

**Cluster:** ` / `
**Generated:** 2026-11-06 19:20:47 UTC
**Kubernetes Version:** v1.36
**Platform:** linux/arm64

---

Demonstrates that the cluster supports DRA (resource.k8s.io API group), has a working
DRA driver, and advertises NVIDIA devices via ResourceSlices. Under the DRA GPU
allocation mode a behavioral test additionally allocates a GPU to a pod through a
ResourceClaim; under the device-plugin mode whole GPUs are device-plugin-allocated
and the behavioral ResourceClaim test is not applicable.

## GPU Allocation Mode

**Configured policy (AICR_GPU_ALLOCATION_POLICY):** `device-plugin-extended-resource`
**Mode source:** recipe-configured GPU allocation policy "device-plugin-extended-resource"
**Resolved mode:** `device-plugin`

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
compute-domain-daemon.nvidia.com            3d2h
compute-domain-default-channel.nvidia.com   3d2h
```

## DRA Driver Health

**DRA driver pods**
```
$ kubectl get pods -n nvidia-dra-driver -o wide
NAME                                                READY   STATUS    RESTARTS   AGE    IP             NODE                                     NOMINATED NODE   READINESS GATES
nvidia-dra-driver-gpu-controller-6db899b876-ml5q6   1/1     Running   0          3d2h   192.168.0.17   manual-dsx-20261102-205148-cp-6e9b3991   <none>           <none>
nvidia-dra-driver-gpu-kubelet-plugin-pc2ws          1/1     Running   0          3d2h   192.168.1.26   manual-dsx-20261102-205148-w-8825        <none>           <none>
nvidia-dra-driver-gpu-kubelet-plugin-xmq4t          1/1     Running   0          3d2h   192.168.2.16   manual-dsx-20261102-205148-w-8837        <none>           <none>
```

## Device Advertisement (ResourceSlices)

**ResourceSlices**
```
$ kubectl get resourceslices
NAME                                                              NODE                                DRIVER                      POOL                                AGE
manual-dsx-20261102-205148-w-8825-compute-domain.nvidia.colvqx6   manual-dsx-20261102-205148-w-8825   compute-domain.nvidia.com   manual-dsx-20261102-205148-w-8825   3d2h
manual-dsx-20261102-205148-w-8837-compute-domain.nvidia.cowxhwh   manual-dsx-20261102-205148-w-8837   compute-domain.nvidia.com   manual-dsx-20261102-205148-w-8837   3d1h
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
