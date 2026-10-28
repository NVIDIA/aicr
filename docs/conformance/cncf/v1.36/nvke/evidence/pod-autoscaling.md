# Pod Autoscaling (HPA with GPU Metrics)

**Cluster:** `NVKE conformance cluster / NVIDIA-GB200`
**Generated:** 2026-10-27 23:59:11 UTC
**Kubernetes Version:** v1.36
**Platform:** linux/arm64

---

Demonstrates CNCF AI Conformance requirement that HPA functions correctly for pods
utilizing accelerators, including the ability to scale based on custom GPU metrics.

## Summary

1. **Prometheus Adapter** — Exposes GPU metrics via Kubernetes custom metrics API
2. **Custom Metrics API** — `gpu_utilization`, `gpu_memory_used`, `gpu_power_usage` available
3. **GPU Stress Workload** — Deployment running CUDA N-Body Simulation to generate GPU load
4. **HPA Configuration** — Targets `gpu_utilization` with threshold of 50%
5. **HPA Scale-Up** — Successfully scales replicas when GPU utilization exceeds target

The result is stated by the verdict line at the end of this section, after the checks run.

---

## Prometheus Adapter

**Prometheus adapter pod**
```
$ kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
NAME                                  READY   STATUS    RESTARTS   AGE
prometheus-adapter-54f5596948-brj98   1/1     Running   0          30m
```

**Prometheus adapter service**
```
$ kubectl get svc prometheus-adapter -n monitoring
NAME                 TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)   AGE
prometheus-adapter   ClusterIP   REDACTED_IP_02   <none>        443/TCP   29h
```

## Custom Metrics API

**Available custom metrics**
```
$ kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1 | python3 -c "..." # extract resource names
pods/gpu_power_usage
namespaces/gpu_utilization
pods/gpu_utilization
namespaces/gpu_memory_used
pods/gpu_memory_used
namespaces/gpu_power_usage
```

## GPU Stress Test Deployment

Deploy a GPU workload running CUDA N-Body Simulation to generate sustained GPU utilization,
then create an HPA targeting `gpu_utilization` to demonstrate autoscaling.

**Test manifest:** `pkg/evidence/cncf/scripts/manifests/hpa-gpu-test.yaml`
```yaml
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
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

# HPA Pod Autoscaling test with custom GPU metrics
# Demonstrates HPA scaling based on gpu_utilization from prometheus-adapter
# Usage: kubectl apply -f pkg/evidence/cncf/scripts/manifests/hpa-gpu-test.yaml
---
apiVersion: v1
kind: Namespace
metadata:
  name: hpa-test
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-workload
  namespace: hpa-test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: gpu-workload
  template:
    metadata:
      labels:
        app: gpu-workload
    spec:
      restartPolicy: Always
      terminationGracePeriodSeconds: 1
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        seccompProfile:
          type: RuntimeDefault
      tolerations:
        - operator: Exists
      containers:
        - name: gpu-worker
          image: nvcr.io/nvidia/cuda:12.9.0-devel-ubuntu24.04
          command: ["bash", "-c"]
          args:
            - |
              cat > /tmp/s.cu << 'EOF'
              __global__ void k(float *d, int n) {
                int i = blockIdx.x * blockDim.x + threadIdx.x;
                float v = (float)i;
                for (int j = 0; j < n; j++) v = v * v + 0.1f;
                if (i < 1) d[0] = v;
              }
              int main() {
                float *d; cudaMalloc(&d, sizeof(float));
                for (;;) { k<<<4096,256>>>(d, 1000000); cudaDeviceSynchronize(); }
              }
              EOF
              nvcc -o /tmp/s /tmp/s.cu && exec /tmp/s
          securityContext:
            readOnlyRootFilesystem: false  # nvcc writes to /tmp during compile
            allowPrivilegeEscalation: false
          resources:
            limits:
              nvidia.com/gpu: 1
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: gpu-workload-hpa
  namespace: hpa-test
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: gpu-workload
  minReplicas: 1
  maxReplicas: 2
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 30
  metrics:
    - type: Pods
      pods:
        metric:
          name: gpu_utilization
        target:
          type: AverageValue
          averageValue: "50"
```

**Apply test manifest**
```
$ kubectl apply -f manifests/hpa-gpu-test.yaml
namespace/hpa-test created
deployment.apps/gpu-workload created
horizontalpodautoscaler.autoscaling/gpu-workload-hpa created
```

**GPU workload pod**
```
$ kubectl get pods -n hpa-test -o wide
NAME                            READY   STATUS    RESTARTS   AGE   IP              NODE                                  NOMINATED NODE   READINESS GATES
gpu-workload-5d8b6948fd-kcg5r   1/1     Running   0          1s    REDACTED_IP_53   gpu-worker-1   <none>           <none>
```

## HPA Status

**HPA status**
```
$ kubectl get hpa -n hpa-test
NAME               REFERENCE                 TARGETS   MINPODS   MAXPODS   REPLICAS   AGE
gpu-workload-hpa   Deployment/gpu-workload   100/50    1         2         2          62s
```

**HPA details**
```
$ kubectl describe hpa gpu-workload-hpa -n hpa-test
Name:                         gpu-workload-hpa
Namespace:                    hpa-test
Labels:                       <none>
Annotations:                  <none>
CreationTimestamp:            Tue, 27 Oct 2026 23:59:11 +0000
Reference:                    Deployment/gpu-workload
Metrics:                      ( current / target )
  "gpu_utilization" on pods:  100 / 50
Min replicas:                 1
Max replicas:                 2
Behavior:
  Scale Up:
    Stabilization Window: 0 seconds
    Select Policy: Max
    Policies:
      - Type: Pods     Value: 4    Period: 15 seconds
      - Type: Percent  Value: 100  Period: 15 seconds
  Scale Down:
    Stabilization Window: 30 seconds
    Select Policy: Max
    Policies:
      - Type: Percent  Value: 100  Period: 15 seconds
Deployment pods:       2 current / 2 desired
Conditions:
  Type            Status  Reason              Message
  ----            ------  ------              -------
  AbleToScale     True    ReadyForNewScale    recommended size matches current size
  ScalingActive   True    ValidMetricFound    the HPA was able to successfully calculate a replica count from pods metric gpu_utilization
  ScalingLimited  False   DesiredWithinRange  the desired count is within the acceptable range
Events:
  Type    Reason             Age   From                       Message
  ----    ------             ----  ----                       -------
  Normal  SuccessfulRescale  17s   horizontal-pod-autoscaler  New size: 2; reason: pods metric gpu_utilization above target
```

## GPU Utilization Evidence

**GPU utilization (nvidia-smi)**
```
$ kubectl exec -n hpa-test gpu-workload-5d8b6948fd-h24z4 -- nvidia-smi --query-gpu=utilization.gpu,utilization.memory,power.draw --format=csv
utilization.gpu [%], utilization.memory [%], power.draw [W]
100 %, 0 %, 353.56 W
```

## Pods After Scale-Up

**Pods after scale-up**
```
$ kubectl get pods -n hpa-test -o wide
NAME                            READY   STATUS    RESTARTS   AGE   IP              NODE                                  NOMINATED NODE   READINESS GATES
gpu-workload-5d8b6948fd-h24z4   1/1     Running   0          17s   REDACTED_IP_45   gpu-worker-2   <none>           <none>
gpu-workload-5d8b6948fd-kcg5r   1/1     Running   0          62s   REDACTED_IP_53   gpu-worker-1   <none>           <none>
```

## Cleanup

**Delete test namespace**
```
$ cleanup_ns hpa-test

```

**Result: PASS** — HPA successfully read gpu_utilization metric and scaled replicas when utilization exceeded target threshold.
