# Secure Accelerator Access

**Cluster:** `NVKE conformance cluster / NVIDIA-GB200`
**Generated:** 2026-10-27 23:54:54 UTC
**Kubernetes Version:** v1.36
**Platform:** linux/arm64

---

Demonstrates that GPU access is mediated through Kubernetes allocation APIs
(DRA ResourceClaims or device plugin resource limits, per the cluster's GPU
allocation mode), not via direct host device mounts. This ensures proper
isolation, access control, and auditability of accelerator usage.

## GPU Allocation Mode

**Configured policy (AICR_GPU_ALLOCATION_POLICY):** `device-plugin-extended-resource`
**Mode source:** recipe-configured GPU allocation policy "device-plugin-extended-resource"
**Resolved mode:** `device-plugin`

## GPU Operator Health

### ClusterPolicy

**ClusterPolicy status**
```
$ kubectl get clusterpolicy -o wide
NAME             STATUS   AGE
cluster-policy   ready    2026-10-26T18:28:23Z
```

### GPU Operator Pods

**GPU operator pods**
```
$ kubectl get pods -n gpu-operator -o wide
NAME                                       READY   STATUS      RESTARTS      AGE   IP              NODE                                     NOMINATED NODE   READINESS GATES
gpu-feature-discovery-g482h                1/1     Running     0             29h   REDACTED_IP_30    gpu-worker-1      <none>           <none>
gpu-feature-discovery-l4vhc                1/1     Running     0             29h   REDACTED_IP_14    gpu-worker-2      <none>           <none>
gpu-operator-5849486646-z2mqs              1/1     Running     0             25m   REDACTED_IP_40   control-plane   <none>           <none>
gpu-operator-readiness-gate-twm4v          0/1     Completed   0             25m   REDACTED_IP_47   gpu-worker-1      <none>           <none>
nvidia-container-toolkit-daemonset-hkr7q   1/1     Running     0             24h   REDACTED_IP_54    gpu-worker-1      <none>           <none>
nvidia-container-toolkit-daemonset-vl7tt   1/1     Running     0             24h   REDACTED_IP_46    gpu-worker-2      <none>           <none>
nvidia-dcgm-b42gv                          1/1     Running     0             29h   REDACTED_IP_28    gpu-worker-1      <none>           <none>
nvidia-dcgm-exporter-r62ft                 1/1     Running     3 (29h ago)   29h   REDACTED_IP_13    gpu-worker-2      <none>           <none>
nvidia-dcgm-exporter-sc8gf                 1/1     Running     3 (29h ago)   29h   REDACTED_IP_29    gpu-worker-1      <none>           <none>
nvidia-dcgm-trqm9                          1/1     Running     0             29h   REDACTED_IP_12    gpu-worker-2      <none>           <none>
nvidia-device-plugin-daemonset-h4xm9       1/1     Running     0             29h   REDACTED_IP_27    gpu-worker-1      <none>           <none>
nvidia-device-plugin-daemonset-pbc9w       1/1     Running     0             29h   REDACTED_IP_11    gpu-worker-2      <none>           <none>
nvidia-driver-daemonset-mrdc5              2/2     Running     0             29h   REDACTED_IP_25    gpu-worker-1      <none>           <none>
nvidia-driver-daemonset-rmjwz              2/2     Running     0             29h   REDACTED_IP_23     gpu-worker-2      <none>           <none>
nvidia-mig-manager-qmqq2                   1/1     Running     0             29h   REDACTED_IP_32    gpu-worker-1      <none>           <none>
nvidia-mig-manager-z9jr9                   1/1     Running     0             29h   REDACTED_IP_16    gpu-worker-2      <none>           <none>
nvidia-operator-validator-cttw4            1/1     Running     0             29h   REDACTED_IP_24     gpu-worker-2      <none>           <none>
nvidia-operator-validator-q8ztp            1/1     Running     0             29h   REDACTED_IP_26    gpu-worker-1      <none>           <none>
```

### GPU Operator DaemonSets

**GPU operator DaemonSets**
```
$ kubectl get ds -n gpu-operator
NAME                                      DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR                                                          AGE
gpu-feature-discovery                     2         2         2       2            2           nvidia.com/gpu.deploy.gpu-feature-discovery=true                       29h
nvidia-container-toolkit-daemonset        2         2         2       2            2           nvidia.com/gpu.deploy.container-toolkit=true                           29h
nvidia-dcgm                               2         2         2       2            2           nvidia.com/gpu.deploy.dcgm=true                                        29h
nvidia-dcgm-exporter                      2         2         2       2            2           nvidia.com/gpu.deploy.dcgm-exporter=true                               29h
nvidia-device-plugin-daemonset            2         2         2       2            2           nvidia.com/gpu.deploy.device-plugin=true                               29h
nvidia-device-plugin-mps-control-daemon   0         0         0       0            0           nvidia.com/gpu.deploy.device-plugin=true,nvidia.com/mps.capable=true   29h
nvidia-driver-daemonset                   2         2         2       2            2           nvidia.com/gpu.deploy.driver=true                                      29h
nvidia-mig-manager                        2         2         2       2            2           nvidia.com/gpu.deploy.mig-manager=true                                 29h
nvidia-operator-validator                 2         2         2       2            2           nvidia.com/gpu.deploy.operator-validator=true                          29h
```

## Device-Plugin-Mediated GPU Access

GPU access is provided through the device plugin (`nvidia.com/gpu` extended
resource in `resources.limits`), not through direct `hostPath` volume mounts to
`/dev/nvidia*`. The kubelet grants each container exactly the devices the
device plugin allocated to it.

## Device Isolation Verification

Deploy a test pod with two containers and verify container-level isolation
(kubernetes-sigs/ai-conformance#75 pattern):
1. Authorized container (`nvidia.com/gpu: 1` limit): `nvidia-smi` sees EXACTLY one GPU
2. Unauthorized sibling container (no GPU request): no `/dev/nvidia*` device nodes,
   and `nvidia-smi` is absent or fails
3. No `hostPath` volumes to `/dev/nvidia*`

**Apply isolation test manifest**
```
$ kubectl apply -f /tmp/aicr-evidence-ibinNv
namespace/secure-access-test created
pod/isolation-test created
```

### Pod Spec (device plugin limits, no hostPath volumes)

**Container GPU limits**
```
$ kubectl get pod isolation-test -n secure-access-test -o jsonpath={range .spec.containers[*]}{.name}{": nvidia.com/gpu="}{.resources.limits.nvidia\.com/gpu}{"\n"}{end}
authorized: nvidia.com/gpu=1
unauthorized: nvidia.com/gpu=
```

**Pod volumes (no hostPath)**
```
$ kubectl get pod isolation-test -n secure-access-test -o jsonpath={.spec.volumes}
[{"name":"kube-api-access-svfv7","projected":{"defaultMode":420,"sources":[{"serviceAccountToken":{"expirationSeconds":3607,"path":"token"}},{"configMap":{"items":[{"key":"ca.crt","path":"ca.crt"}],"name":"kube-root-ca.crt"}},{"downwardAPI":{"items":[{"fieldRef":{"apiVersion":"v1","fieldPath":"metadata.namespace"},"path":"namespace"}]}}]}}]
```

**Container exit codes**
```
$ kubectl get pod isolation-test -n secure-access-test -o jsonpath={range .status.containerStatuses[*]}{.name}{": exitCode="}{.state.terminated.exitCode}{"\n"}{end}
authorized: exitCode=0
unauthorized: exitCode=0
```

### Authorized Container (positive probe: exactly one GPU)

**Authorized container logs**
```
$ kubectl logs isolation-test -c authorized -n secure-access-test
=== nvidia-smi -L (authorized container, nvidia.com/gpu: 1) ===
GPU 0: NVIDIA GB200 (UUID: GPU-REDACTED_UUID_01)
visible GPU count: 1
PASS: exactly one GPU visible
```

### Unauthorized Sibling Container (negative probes: no GPU visible)

**Unauthorized container logs**
```
$ kubectl logs isolation-test -c unauthorized -n secure-access-test
=== /dev/nvidia* (unauthorized sibling, no GPU request) ===
PASS: no /dev/nvidia* device nodes visible

=== nvidia-smi (must be absent or fail) ===
PASS: nvidia-smi absent or fails without GPU allocation
```

**Result: PASS** — GPU access mediated through device plugin `nvidia.com/gpu` limits: the authorized container saw exactly one GPU and the unauthorized sibling container saw none. No direct host device mounts.

## Cleanup

**Delete test namespace**
```
$ cleanup_ns secure-access-test

```
