# GKE TCPXO Networking Prerequisites

For `*-gke-cos-training*` recipes, GPUDirect TCPXO enables high-speed inter-node GPU communication on GKE. Without it, NCCL falls back to TCP (~4 GB/s vs ~340 GB/s with TCPXO).

## Infrastructure Prerequisites

GKE clusters must have multi-NIC networking configured before deploying AICR bundles:

- Multi-NIC networking enabled (8 GPU NICs per a3-megagpu-8g node)
- `Network` + `GKENetworkParamSet` CRs configured for GPU NICs (cluster-specific, not managed by AICR)
- `nccl-tcpxo-installer` DaemonSet on GPU nodes (included in AICR bundle)
- `nri-device-injector` DaemonSet on GPU nodes (included in AICR bundle)

**Important:** The GPU node pool must be provisioned with only the 8 GPU NIC
networks (`gpu-nic-0` through `gpu-nic-7`). Do **not** include a gVNIC additional
network — it takes a GPU NIC PCI slot (`0000:06:00.0`), leaving only 7/8 GPUs
available for TCPXO.

## Workload Pod Configuration (NRI Profile)

The NRI profile mounts the host's `/sys` and `/proc/sys` into the TCPXO daemon
container, giving it PCI sysfs visibility without `hostNetwork`. This preserves
pod networking (DNS, network policies, service mesh compatibility).

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-workload
  annotations:
    # NRI device injection for tcpxo-daemon GPU access
    devices.gke.io/container.tcpxo-daemon: |
      - path: /dev/nvidia0
      - path: /dev/nvidia1
      - path: /dev/nvidia2
      - path: /dev/nvidia3
      - path: /dev/nvidia4
      - path: /dev/nvidia5
      - path: /dev/nvidia6
      - path: /dev/nvidia7
      - path: /dev/nvidiactl
      - path: /dev/nvidia-uvm
      - path: /dev/dmabuf_import_helper
    # Multi-NIC mapping (network names are cluster-specific)
    networking.gke.io/default-interface: eth0
    networking.gke.io/interfaces: |
      [{"interfaceName":"eth0","network":"default"},
       {"interfaceName":"eth1","network":"gpu-nic0"},
       {"interfaceName":"eth2","network":"gpu-nic1"},
       {"interfaceName":"eth3","network":"gpu-nic2"},
       {"interfaceName":"eth4","network":"gpu-nic3"},
       {"interfaceName":"eth5","network":"gpu-nic4"},
       {"interfaceName":"eth6","network":"gpu-nic5"},
       {"interfaceName":"eth7","network":"gpu-nic6"},
       {"interfaceName":"eth8","network":"gpu-nic7"}]
spec:
  hostNetwork: false
  containers:
    - name: tcpxo-daemon
      image: us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.20
      securityContext:
        capabilities:
          add: [NET_ADMIN, NET_BIND_SERVICE]
      volumeMounts:
        - name: nvtcpxo-libraries
          mountPath: /usr/local/nvidia
          readOnly: true
        - name: nvtcpxo-sys
          mountPath: /hostsysfs
        - name: nvtcpxo-proc-sys
          mountPath: /hostprocsysfs
      env:
        - name: LD_LIBRARY_PATH
          value: /usr/local/nvidia/lib64
    - name: workload
      # ... your training container
      volumeMounts:
        - name: nvtcpxo-aperture-devices
          mountPath: /dev/aperture_devices
  volumes:
    - name: nvtcpxo-libraries
      hostPath:
        path: /home/kubernetes/bin/nvidia
    - name: nvtcpxo-sys
      hostPath:
        path: /sys
    - name: nvtcpxo-proc-sys
      hostPath:
        path: /proc/sys
    - name: nvtcpxo-aperture-devices
      hostPath:
        path: /dev/aperture_devices
```

Key properties:
- `hostNetwork: false` — workloads get proper pod networking
- `privileged: false` — tcpxo-daemon uses only `NET_ADMIN` and `NET_BIND_SERVICE`
- `/sys` mounted as `/hostsysfs` — provides PCI sysfs visibility for GPU enumeration
- `/proc/sys` mounted as `/hostprocsysfs` — allows kernel network tuning
- NRI annotations inject GPU devices and multi-NIC interfaces
- Requires NRI device injector DaemonSet deployed on GPU nodes

See [`demos/workloads/training/gke-nccl-test-tcpxo.yaml`](https://github.com/NVIDIA/aicr/blob/main/demos/workloads/training/gke-nccl-test-tcpxo.yaml) for a complete 2-node NCCL benchmark example.

## NCCL Plugin Version Matching

The NCCL test container image must match the cluster's installed TCPXO plugin version. Check with:

```shell
kubectl get ds nccl-tcpxo-installer -n kube-system \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="nccl-tcpxo-installer")].image}'
```

Update the `nccl-plugin-gpudirecttcpx-dev` image tag in your workload to match.

## Troubleshooting

### RxDM detects 7/8 GPUs

If RxDM reports `Number of GPUs detected 7 is not equal to the actual number of GPUs 8`, check the GPU node pool's additional network configuration:

```shell
gcloud container node-pools describe <pool-name> \
  --cluster <cluster> --region <region> --project <project> \
  --format="yaml(networkConfig.additionalNodeNetworkConfigs)"
```

If a **gVNIC network** appears in the list, it is taking a GPU NIC PCI slot. Remove the gVNIC from the node pool and reprovision the GPU nodes.

You can also verify the node NIC mapping:

```shell
kubectl get node <gpu-node> \
  -o jsonpath='{.metadata.annotations.networking\.gke\.io/nic-info}'
```

All 8 GPU NIC PCI addresses should be mapped to `eth1`–`eth8`. If a gVNIC is present, it typically occupies PCI `0000:06:00.0`, displacing the first GPU NIC.

### RxDM detects 0/8 GPUs

If RxDM reports `Number of GPUs detected in the PCI tree 0`, the pod is missing the `/sys` hostPath mount. Ensure `/sys` is mounted as `/hostsysfs` in the tcpxo-daemon container. Without it, the container network namespace hides the host PCI sysfs tree entirely.

## Performance Reference

Validated on GKE 1.35 / a3-megagpu-8g (2 nodes, 16 GPUs):

| Profile | hostNetwork | busBW @ 16 GB | Avg busBW |
|---------|-------------|---------------|-----------|
| NRI (recommended) | false | ~340 GB/s | ~100 GB/s |
| Without TCPXO | N/A | ~4 GB/s | ~4 GB/s |
