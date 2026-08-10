# Secure Accelerator Access

**Cluster:** `EKS / p6e-gb200.36xlarge / NVIDIA-GB200`
**Generated:** 2026-08-09 01:24:23 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates that GPU access is mediated through Kubernetes allocation APIs
(DRA ResourceClaims or device plugin resource limits, per the cluster's GPU
allocation mode), not via direct host device mounts. This ensures proper
isolation, access control, and auditability of accelerator usage.

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

## GPU Operator Health

### ClusterPolicy

**ClusterPolicy status**
```
$ kubectl get clusterpolicy -o wide
NAME             STATUS   AGE
cluster-policy   ready    2026-07-13T20:05:39Z
```

### GPU Operator Pods

**GPU operator pods**
```
$ kubectl get pods -n gpu-operator -o wide
NAME                                       READY   STATUS      RESTARTS      AGE   IP               NODE                             NOMINATED NODE   READINESS GATES
gpu-feature-discovery-c52dw                1/1     Running     0             26d   100.65.132.85    ip-100-64-135-20.ec2.internal    <none>           <none>
gpu-feature-discovery-q6hg4                1/1     Running     0             26d   100.65.142.86    ip-100-64-142-208.ec2.internal   <none>           <none>
gpu-operator-56465b8db9-2qxm2              1/1     Running     0             26d   100.64.11.197    ip-100-64-9-72.ec2.internal      <none>           <none>
nvidia-container-toolkit-daemonset-57kfl   1/1     Running     0             26d   100.65.182.148   ip-100-64-142-208.ec2.internal   <none>           <none>
nvidia-container-toolkit-daemonset-jwd2j   1/1     Running     0             26d   100.65.237.130   ip-100-64-135-20.ec2.internal    <none>           <none>
nvidia-cuda-validator-p2mvl                0/1     Completed   0             26d   100.65.185.89    ip-100-64-135-20.ec2.internal    <none>           <none>
nvidia-cuda-validator-vkhtv                0/1     Completed   0             26d   100.65.243.10    ip-100-64-142-208.ec2.internal   <none>           <none>
nvidia-dcgm-exporter-thpnl                 1/1     Running     2 (26d ago)   26d   100.65.229.140   ip-100-64-142-208.ec2.internal   <none>           <none>
nvidia-dcgm-exporter-zn7nr                 1/1     Running     2 (26d ago)   26d   100.65.162.68    ip-100-64-135-20.ec2.internal    <none>           <none>
nvidia-dcgm-s28t2                          1/1     Running     0             26d   100.65.184.85    ip-100-64-135-20.ec2.internal    <none>           <none>
nvidia-dcgm-v666n                          1/1     Running     0             26d   100.65.130.57    ip-100-64-142-208.ec2.internal   <none>           <none>
nvidia-device-plugin-daemonset-lnp9c       1/1     Running     0             26d   100.65.177.131   ip-100-64-142-208.ec2.internal   <none>           <none>
nvidia-device-plugin-daemonset-sznnp       1/1     Running     0             26d   100.65.132.202   ip-100-64-135-20.ec2.internal    <none>           <none>
nvidia-driver-daemonset-2vv27              2/2     Running     4 (26d ago)   26d   100.65.166.141   ip-100-64-135-20.ec2.internal    <none>           <none>
nvidia-driver-daemonset-w9b5z              2/2     Running     4 (26d ago)   26d   100.65.161.204   ip-100-64-142-208.ec2.internal   <none>           <none>
nvidia-mig-manager-48hvr                   1/1     Running     0             26d   100.65.168.175   ip-100-64-142-208.ec2.internal   <none>           <none>
nvidia-mig-manager-z8cxg                   1/1     Running     0             26d   100.65.172.170   ip-100-64-135-20.ec2.internal    <none>           <none>
nvidia-operator-validator-2m9b2            1/1     Running     0             26d   100.65.242.29    ip-100-64-135-20.ec2.internal    <none>           <none>
nvidia-operator-validator-4qxm8            1/1     Running     0             26d   100.65.195.174   ip-100-64-142-208.ec2.internal   <none>           <none>
```

### GPU Operator DaemonSets

**GPU operator DaemonSets**
```
$ kubectl get ds -n gpu-operator
NAME                                      DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR                                                          AGE
gpu-feature-discovery                     2         2         2       2            2           nvidia.com/gpu.deploy.gpu-feature-discovery=true                       26d
nvidia-container-toolkit-daemonset        2         2         2       2            2           nvidia.com/gpu.deploy.container-toolkit=true                           26d
nvidia-dcgm                               2         2         2       2            2           nvidia.com/gpu.deploy.dcgm=true                                        26d
nvidia-dcgm-exporter                      2         2         2       2            2           nvidia.com/gpu.deploy.dcgm-exporter=true                               26d
nvidia-device-plugin-daemonset            2         2         2       2            2           nvidia.com/gpu.deploy.device-plugin=true                               26d
nvidia-device-plugin-mps-control-daemon   0         0         0       0            0           nvidia.com/gpu.deploy.device-plugin=true,nvidia.com/mps.capable=true   26d
nvidia-driver-daemonset                   2         2         2       2            2           nvidia.com/gpu.deploy.driver=true                                      26d
nvidia-mig-manager                        2         2         2       2            2           nvidia.com/gpu.deploy.mig-manager=true                                 26d
nvidia-operator-validator                 2         2         2       2            2           nvidia.com/gpu.deploy.operator-validator=true                          26d
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
$ kubectl apply -f /var/folders/7j/mskv1qz54czgq01n7dbh3ttc0000gp/T//aicr-evidence-X0Y8Hd
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
[{"name":"kube-api-access-lg4qg","projected":{"defaultMode":420,"sources":[{"serviceAccountToken":{"expirationSeconds":3607,"path":"token"}},{"configMap":{"items":[{"key":"ca.crt","path":"ca.crt"}],"name":"kube-root-ca.crt"}},{"downwardAPI":{"items":[{"fieldRef":{"apiVersion":"v1","fieldPath":"metadata.namespace"},"path":"namespace"}]}}]}}]
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
GPU 0: NVIDIA GB200 (UUID: GPU-b1b35bd6-498c-c7be-5f54-12a5ab945074)
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
