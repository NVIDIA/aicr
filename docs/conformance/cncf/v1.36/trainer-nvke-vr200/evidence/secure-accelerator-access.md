# Secure Accelerator Access

**Cluster:** ` / `
**Generated:** 2026-11-04 01:10:29 UTC
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
cluster-policy   ready    2026-11-03T00:28:46Z
```

### GPU Operator Pods

**GPU operator pods**
```
$ kubectl get pods -n gpu-operator -o wide
NAME                                       READY   STATUS      RESTARTS      AGE   IP             NODE                                     NOMINATED NODE   READINESS GATES
gpu-feature-discovery-qdfrx                1/1     Running     0             21h   192.168.1.18   manual-dsx-20261102-205148-w-8825        <none>           <none>
gpu-feature-discovery-vkwsb                1/1     Running     0             9h    192.168.2.12   manual-dsx-20261102-205148-w-8837        <none>           <none>
gpu-operator-5849486646-6qrbw              1/1     Running     0             24h   192.168.0.16   manual-dsx-20261102-205148-cp-6e9b3991   <none>           <none>
nvidia-container-toolkit-daemonset-22lvg   1/1     Running     0             21h   192.168.1.20   manual-dsx-20261102-205148-w-8825        <none>           <none>
nvidia-container-toolkit-daemonset-v82cb   1/1     Running     0             9h    192.168.2.6    manual-dsx-20261102-205148-w-8837        <none>           <none>
nvidia-cuda-validator-5kg8c                0/1     Completed   0             21h   192.168.1.24   manual-dsx-20261102-205148-w-8825        <none>           <none>
nvidia-cuda-validator-sbg75                0/1     Completed   0             9h    192.168.2.13   manual-dsx-20261102-205148-w-8837        <none>           <none>
nvidia-dcgm-exporter-xqqjr                 1/1     Running     3 (9h ago)    9h    192.168.2.9    manual-dsx-20261102-205148-w-8837        <none>           <none>
nvidia-dcgm-exporter-xzgsb                 1/1     Running     2 (21h ago)   21h   192.168.1.17   manual-dsx-20261102-205148-w-8825        <none>           <none>
nvidia-dcgm-mvgxj                          1/1     Running     0             21h   192.168.1.19   manual-dsx-20261102-205148-w-8825        <none>           <none>
nvidia-dcgm-qw8vp                          1/1     Running     0             9h    192.168.2.10   manual-dsx-20261102-205148-w-8837        <none>           <none>
nvidia-device-plugin-daemonset-6bzq6       1/1     Running     0             9h    192.168.2.8    manual-dsx-20261102-205148-w-8837        <none>           <none>
nvidia-device-plugin-daemonset-n4zss       1/1     Running     0             21h   192.168.1.22   manual-dsx-20261102-205148-w-8825        <none>           <none>
nvidia-driver-daemonset-7vglw              2/2     Running     0             21h   192.168.1.23   manual-dsx-20261102-205148-w-8825        <none>           <none>
nvidia-driver-daemonset-r6x25              2/2     Running     2 (9h ago)    9h    192.168.2.11   manual-dsx-20261102-205148-w-8837        <none>           <none>
nvidia-mig-manager-j2b95                   1/1     Running     0             21h   192.168.1.16   manual-dsx-20261102-205148-w-8825        <none>           <none>
nvidia-mig-manager-zdgk4                   1/1     Running     0             9h    192.168.2.14   manual-dsx-20261102-205148-w-8837        <none>           <none>
nvidia-operator-validator-c8vt5            1/1     Running     0             9h    192.168.2.7    manual-dsx-20261102-205148-w-8837        <none>           <none>
nvidia-operator-validator-zjgxj            1/1     Running     0             21h   192.168.1.21   manual-dsx-20261102-205148-w-8825        <none>           <none>
```

### GPU Operator DaemonSets

**GPU operator DaemonSets**
```
$ kubectl get ds -n gpu-operator
NAME                                      DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR                                                          AGE
gpu-feature-discovery                     2         2         2       2            2           nvidia.com/gpu.deploy.gpu-feature-discovery=true                       24h
nvidia-container-toolkit-daemonset        2         2         2       2            2           nvidia.com/gpu.deploy.container-toolkit=true                           24h
nvidia-dcgm                               2         2         2       2            2           nvidia.com/gpu.deploy.dcgm=true                                        24h
nvidia-dcgm-exporter                      2         2         2       2            2           nvidia.com/gpu.deploy.dcgm-exporter=true                               24h
nvidia-device-plugin-daemonset            2         2         2       2            2           nvidia.com/gpu.deploy.device-plugin=true                               24h
nvidia-device-plugin-mps-control-daemon   0         0         0       0            0           nvidia.com/gpu.deploy.device-plugin=true,nvidia.com/mps.capable=true   24h
nvidia-driver-daemonset                   2         2         2       2            2           nvidia.com/gpu.deploy.driver=true                                      21h
nvidia-mig-manager                        2         2         2       2            2           nvidia.com/gpu.deploy.mig-manager=true                                 24h
nvidia-operator-validator                 2         2         2       2            2           nvidia.com/gpu.deploy.operator-validator=true                          24h
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
$ kubectl apply -f ls4bz4rj5cs1f8v3fzncnt4m0000gn/T//aicr-evidence-VfiAvM
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
[{"name":"kube-api-access-5sd22","projected":{"defaultMode":420,"sources":[{"serviceAccountToken":{"expirationSeconds":3607,"path":"token"}},{"configMap":{"items":[{"key":"ca.crt","path":"ca.crt"}],"name":"kube-root-ca.crt"}},{"downwardAPI":{"items":[{"fieldRef":{"apiVersion":"v1","fieldPath":"metadata.namespace"},"path":"namespace"}]}}]}}]
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
GPU 0: NVIDIA Graphics Device (UUID: GPU-401dc5ce-9ad3-58f4-a530-58634995b5ea)
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
