# DRA Support (Dynamic Resource Allocation)

**Generated:** 2026-02-19 19:00:10 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

## Summary

1. **DRA API** — All 4 resource types present (DeviceClass, ResourceClaim, ResourceClaimTemplate, ResourceSlice)
2. **DRA Driver** — Controller + kubelet plugin running healthy
3. **ResourceSlices** — GPU and compute-domain drivers advertising devices
4. **GPU Allocation Test** — Pod completed, logs show `/dev/nvidia6` device access and "DRA GPU allocation successful"
5. **Result: PASS**

---

Demonstrates that the cluster supports DRA (resource.k8s.io API group), has a working
DRA driver, advertises GPU devices via ResourceSlices, and can allocate GPUs to pods
through ResourceClaims.

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

## DRA Driver Health

**DRA driver pods**
```
$ kubectl get pods -n nvidia-dra-driver -o wide
NAME                                                READY   STATUS    RESTARTS   AGE    IP              NODE                             NOMINATED NODE   READINESS GATES
nvidia-dra-driver-gpu-controller-74bd58f9c7-zmcd6   1/1     Running   0          19h    100.64.7.215    ip-100-64-4-149.ec2.internal     <none>           <none>
nvidia-dra-driver-gpu-kubelet-plugin-vvc5w          2/2     Running   0          6m8s   100.65.234.92   ip-100-64-171-120.ec2.internal   <none>           <none>
```

## Device Advertisement (ResourceSlices)

**ResourceSlices**
```
$ kubectl get resourceslices
NAME                                                             NODE                             DRIVER                      POOL                             AGE
ip-100-64-171-120.ec2.internal-compute-domain.nvidia.com-8k72n   ip-100-64-171-120.ec2.internal   compute-domain.nvidia.com   ip-100-64-171-120.ec2.internal   4m11s
ip-100-64-171-120.ec2.internal-gpu.nvidia.com-7npv2              ip-100-64-171-120.ec2.internal   gpu.nvidia.com              ip-100-64-171-120.ec2.internal   4m9s
```

## GPU Allocation Test

Deploy a test pod that requests 1 GPU via ResourceClaim and verifies device access.

**Test manifest:** `tests/ai-conformance/manifests/dra-gpu-test.yaml`

**Apply test manifest**
```
$ kubectl apply -f /Users/yuanc/projects/eidos/tests/ai-conformance/manifests/dra-gpu-test.yaml
namespace/dra-test created
resourceclaim.resource.k8s.io/gpu-claim created
pod/dra-gpu-test created
```

**ResourceClaim status**
```
$ kubectl get resourceclaim -n dra-test -o wide
NAME        STATE     AGE
gpu-claim   pending   9s
```

**Pod status**
```
$ kubectl get pod dra-gpu-test -n dra-test -o wide
NAME           READY   STATUS      RESTARTS   AGE   IP               NODE                             NOMINATED NODE   READINESS GATES
dra-gpu-test   0/1     Completed   0          9s    100.65.141.103   ip-100-64-171-120.ec2.internal   <none>           <none>
```

**Pod logs**
```
$ kubectl logs dra-gpu-test -n dra-test
/dev/nvidia-modeset
/dev/nvidia-uvm
/dev/nvidia-uvm-tools
/dev/nvidia6
/dev/nvidiactl
DRA GPU allocation successful
```

**Result: PASS** — Pod completed successfully with GPU access via DRA.

## Cleanup

**Delete test namespace**
```
$ kubectl delete namespace dra-test --ignore-not-found
namespace "dra-test" deleted
```
