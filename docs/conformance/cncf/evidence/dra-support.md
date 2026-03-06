# DRA Support (Dynamic Resource Allocation)

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-03-06 19:36:55 UTC
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

## DeviceClasses

**DeviceClasses**
```
$ kubectl get deviceclass
NAME                                        AGE
compute-domain-daemon.nvidia.com            44h
compute-domain-default-channel.nvidia.com   44h
gpu.nvidia.com                              44h
mig.nvidia.com                              44h
vfio.gpu.nvidia.com                         44h
```

## DRA Driver Health

**DRA driver pods**
```
$ kubectl get pods -n nvidia-dra-driver -o wide
NAME                                               READY   STATUS    RESTARTS   AGE   IP               NODE                             NOMINATED NODE   READINESS GATES
nvidia-dra-driver-gpu-controller-9d69fbdcb-z27mn   1/1     Running   0          44h   100.64.5.27      ip-100-64-4-149.ec2.internal     <none>           <none>
nvidia-dra-driver-gpu-kubelet-plugin-bdmdm         2/2     Running   0          44h   100.65.180.246   ip-100-64-147-149.ec2.internal   <none>           <none>
```

## Device Advertisement (ResourceSlices)

**ResourceSlices**
```
$ kubectl get resourceslices
NAME                                                             NODE                             DRIVER                      POOL                             AGE
ip-100-64-147-149.ec2.internal-compute-domain.nvidia.com-bbg8t   ip-100-64-147-149.ec2.internal   compute-domain.nvidia.com   ip-100-64-147-149.ec2.internal   44h
ip-100-64-147-149.ec2.internal-gpu.nvidia.com-sgw47              ip-100-64-147-149.ec2.internal   gpu.nvidia.com              ip-100-64-147-149.ec2.internal   44h
```

## GPU Allocation Test

Deploy a test pod that requests 1 GPU via ResourceClaim and verifies device access.

**Test manifest:** `pkg/evidence/scripts/manifests/dra-gpu-test.yaml`
```yaml
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# DRA GPU allocation test
# Usage: kubectl apply -f pkg/evidence/scripts/manifests/dra-gpu-test.yaml
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
$ kubectl apply -f manifests/dra-gpu-test.yaml
namespace/dra-test created
resourceclaim.resource.k8s.io/gpu-claim created
pod/dra-gpu-test created
```

**ResourceClaim status**
```
$ kubectl get resourceclaim -n dra-test -o wide
NAME        STATE     AGE
gpu-claim   pending   11s
```

**Pod status**
```
$ kubectl get pod dra-gpu-test -n dra-test -o wide
NAME           READY   STATUS      RESTARTS   AGE   IP              NODE                             NOMINATED NODE   READINESS GATES
dra-gpu-test   0/1     Completed   0          11s   100.65.57.124   ip-100-64-147-149.ec2.internal   <none>           <none>
```

**Pod logs**
```
$ kubectl logs dra-gpu-test -n dra-test
/dev/nvidia-modeset
/dev/nvidia-uvm
/dev/nvidia-uvm-tools
/dev/nvidia3
/dev/nvidiactl
DRA GPU allocation successful
```

**Result: PASS** — Pod completed successfully with GPU access via DRA.

## Cleanup

**Delete test namespace**
```
$ cleanup_ns dra-test

```
