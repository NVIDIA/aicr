# DRA Support (Dynamic Resource Allocation)

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-02-24 20:20:22 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

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
NAME                                                READY   STATUS    RESTARTS        AGE   IP              NODE                             NOMINATED NODE   READINESS GATES
nvidia-dra-driver-gpu-controller-75f987ff5f-chrbn   1/1     Running   1 (3m54s ago)   47h   100.65.71.124   ip-100-64-171-120.ec2.internal   <none>           <none>
nvidia-dra-driver-gpu-kubelet-plugin-rmxdj          2/2     Running   2 (3m54s ago)   17m   100.65.2.168    ip-100-64-171-120.ec2.internal   <none>           <none>
```

## Device Advertisement (ResourceSlices)

**ResourceSlices**
```
$ kubectl get resourceslices
NAME                                                             NODE                             DRIVER                      POOL                             AGE
ip-100-64-171-120.ec2.internal-compute-domain.nvidia.com-76zr9   ip-100-64-171-120.ec2.internal   compute-domain.nvidia.com   ip-100-64-171-120.ec2.internal   2m12s
ip-100-64-171-120.ec2.internal-gpu.nvidia.com-75xvv              ip-100-64-171-120.ec2.internal   gpu.nvidia.com              ip-100-64-171-120.ec2.internal   2m10s
```

## GPU Allocation Test

Deploy a test pod that requests 1 GPU via ResourceClaim and verifies device access.

**Test manifest:** `docs/conformance/cncf/manifests/dra-gpu-test.yaml`

```yaml
---
apiVersion: v1
kind: Namespace
metadata:
  name: dra-test
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: gpu-claim
  namespace: dra-test
spec:
  devices:
    requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          allocationMode: ExactCount
          count: 1
---
apiVersion: v1
kind: Pod
metadata:
  name: dra-gpu-test
  namespace: dra-test
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: false
    seccompProfile:
      type: RuntimeDefault
  tolerations:
    - operator: Exists
  resourceClaims:
    - name: gpu
      resourceClaimName: gpu-claim
  containers:
    - name: gpu-test
      image: nvidia/cuda:12.9.0-base-ubuntu24.04
      command: ["bash", "-c", "ls /dev/nvidia* && echo 'DRA GPU allocation successful'"]
      securityContext:
        allowPrivilegeEscalation: false
      resources:
        claims:
          - name: gpu
```

**Apply test manifest**
```
$ kubectl apply -f docs/conformance/cncf/manifests/dra-gpu-test.yaml
namespace/dra-test created
resourceclaim.resource.k8s.io/gpu-claim created
pod/dra-gpu-test created
```

**ResourceClaim status**
```
$ kubectl get resourceclaim -n dra-test -o wide
NAME        STATE     AGE
gpu-claim   pending   10s
```

**Pod status**
```
$ kubectl get pod dra-gpu-test -n dra-test -o wide
NAME           READY   STATUS      RESTARTS   AGE   IP              NODE                             NOMINATED NODE   READINESS GATES
dra-gpu-test   0/1     Completed   0          10s   100.65.63.246   ip-100-64-171-120.ec2.internal   <none>           <none>
```

**Pod logs**
```
$ kubectl logs dra-gpu-test -n dra-test
/dev/nvidia-modeset
/dev/nvidia-uvm
/dev/nvidia-uvm-tools
/dev/nvidia2
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
