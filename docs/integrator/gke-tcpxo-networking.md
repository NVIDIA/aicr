# GKE TCPXO Networking Prerequisites

For the **H100 GKE COS training** recipes (`h100-gke-cos-training*`, on `a3-megagpu-8g` nodes), GPUDirect TCPXO enables high-speed inter-node GPU communication on GKE. Without it, the NVIDIA Collective Communications Library (NCCL) falls back to TCP (~4 GB/s vs ~340 GB/s with TCPXO).

> **A100 (a2) exception:** the `a100-gke-cos-training*` recipes intentionally omit the `gke-nccl-tcpxo` component — GPUDirect TCPXO targets H100 `a3-megagpu-8g` nodes, not the A100 `a2-highgpu`/`a2-ultragpu` machine family. The prerequisites below do **not** apply to A100 GKE recipes, and the generated A100 bundle does not install the TCPXO DaemonSets.

## Infrastructure Prerequisites

GKE clusters must have multi-NIC networking configured before deploying AICR bundles:

- Multi-NIC networking enabled (8 GPU NICs per a3-megagpu-8g node)
- `Network` + `GKENetworkParamSet` CRs configured for GPU NICs (cluster-specific, not managed by AICR)
- `nccl-tcpxo-installer` DaemonSet on GPU nodes (included in AICR bundle)
- `nri-device-injector` DaemonSet on GPU nodes (included in AICR bundle)

The last two ship in the AICR bundle. The first two are **cluster provisioning**
— AICR detects them but does not create them.

### Provisioning multi-NIC networking

These four steps are ordered. Step 2 is the one that cannot be undone later;
steps 3 and 4 must both complete before any TCPXO workload — or `aicr validate` —
will work.

1. **Create the VPCs and subnets** — one dedicated VPC + subnet per GPU NIC,
   eight in total, in the cluster's region.
2. **Create the cluster** with `--enable-multi-networking`, plus its two
   prerequisites `--enable-dataplane-v2` and `--enable-ip-alias`.
3. **Create the GPU node pool** on an `a3-megagpu-8g` machine type with
   `--enable-gvnic`, attaching the eight VPC/subnet pairs as repeated
   `--additional-node-network` entries, one per pair, each in the form
   `network=NETWORK,subnetwork=SUBNET`.
4. **Apply the `Network` and `GKENetworkParamSet` CRs** — one pair per GPU NIC,
   binding each additional node network into the cluster so pods can reference
   it by name. **Each `Network` name must contain `gpu-nic`** — for example
   `gpu-nic-0` through `gpu-nic-7`, optionally with a cluster prefix such as
   `aicr-demo2-gpu-nic-0`. The paired `GKENetworkParamSet` is referenced by the
   `Network` through `spec.parametersRef`, so its own name is unconstrained.

> **The `gpu-nic` naming is a requirement, not a convention.** AICR discovers
> these networks by matching `gpu-nic` in the `Network` object's
> `metadata.name` — both the `gke-gpu-nic-networks` deployment check and the
> NCCL benchmark's own interface mapping. Google's sample manifests name the
> Device networks `vpc1`–`vpc8`; applied verbatim those are invisible to AICR,
> and the deployment check reports 0 of 8 on a cluster that is otherwise
> correctly provisioned. Rename them when following that procedure.
>
> Beyond containing `gpu-nic`, the exact names are yours to choose — but the
> workload annotation below must reference the names your cluster actually has.
> The example there uses `gpu-nic0`–`gpu-nic7`; if you provisioned
> `gpu-nic-0`–`gpu-nic-7`, use those instead.

> **Multi-networking cannot be enabled after cluster creation.** `--enable-multi-networking`
> is a create-time flag; there is no `gcloud container clusters update` equivalent,
> so a cluster created without it must be **recreated**. Steps 3 and 4, by
> contrast, can be done on an existing multi-networking cluster — a node pool can
> be added later, and the CRs can be applied at any point.

AICR installs the TCPXO DaemonSets and detects the CRs; it does not provision any
of this networking. These steps are a summary of the prerequisite AICR depends
on, not a complete provisioning runbook — for the full procedure, including the
per-VPC firewall rules and the supported GKE version floors, follow Google's
[GPUDirect and multi-networking guide](https://cloud.google.com/kubernetes-engine/docs/how-to/gpu-bandwidth-gpudirect-tcpx).

Completing steps 1–3 without step 4 is the failure mode worth knowing: the VMs
come up with all nine NICs attached (the node's primary interface plus the eight
GPU NICs) and the AICR TCPXO DaemonSets roll out cleanly, but with no `Network`
objects bound into the cluster no pod can reference a GPU NIC and TCPXO cannot
function.

### Verifying

```shell
kubectl get network.networking.gke.io
```

Expect eight GPU NIC entries (plus the `default` network). Match on the
`gpu-nic` substring rather than an exact name: the rest of each name is chosen
at provisioning time and may carry a local prefix, such as
`aicr-demo2-gpu-nic-0`.

Fewer than eight means the prerequisite is incomplete. AICR's
`gke-gpu-nic-networks` deployment check asserts this same count, so
`aicr validate --phase deployment` reports the shortfall by name rather than
letting it surface later as a performance-phase abort with no bandwidth number.

**Important:** The GPU node pool must be provisioned with only the 8 GPU NIC
networks (`gpu-nic-0` through `gpu-nic-7`). Do **not** include a gVNIC additional
network — it takes a GPU NIC PCI slot (`0000:06:00.0`), leaving only 7/8 GPUs
available for TCPXO. This is distinct from the `--enable-gvnic` node-pool flag,
which selects the gVNIC driver and **is** required: pass the flag, but do not add
a ninth `--additional-node-network` entry for it.

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
      image: us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.21
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

Running a **Kubeflow TrainJob** rather than a bare Pod? A TrainJob cannot add
the `tcpxo-daemon` sidecar, so the wiring must live in a `TrainingRuntime` — see
[Attaching a Training Workload to the Cluster Fabric](../user/fabric-attached-training.md).

See [`demos/workloads/training/gke-nccl-test-tcpxo.yaml`](https://github.com/NVIDIA/aicr/blob/main/demos/workloads/training/gke-nccl-test-tcpxo.yaml) for a complete 2-node NCCL benchmark example. (pinned to the same coupled pair the recipe ships, plugin `v1.0.15` with daemon `v1.0.21`)

## NCCL Plugin Version Matching

Google publishes the plugin installer and the `tcpxo-daemon` sidecar as a
**coupled release pair**. Running a mismatched pair is unsupported. The pair
AICR currently ships is:

| Component | Image | Version |
|---|---|---|
| Plugin installer (DaemonSet, cluster-side) | `nccl-plugin-gpudirecttcpx-dev` | `v1.0.15` |
| Sidecar (workload-side) | `tcpgpudmarxd-dev` | `v1.0.21` |

Check what your cluster actually runs:

```shell
kubectl get ds nccl-tcpxo-installer -n kube-system \
  -o jsonpath='{.spec.template.spec.initContainers[?(@.name=="nccl-tcpxo-installer")].image}'
```

Then set your workload's `tcpxo-daemon` image to the daemon version paired with
it, per [Google's release notes][tcpxo-releases].

**Upgrade in order:** upgrade the plugin installer first, then the workload's
daemon. Google also advises that workloads should not be running while the
installer is upgraded. This is a sequence, not a statement that a mismatched
pair is supported to run.

[tcpxo-releases]: https://github.com/GoogleCloudPlatform/container-engine-accelerators/blob/master/gpudirect-tcpxo/README.md

## Running the NCCL Benchmark

### Automated (recommended): `aicr validate`

The GKE H100 training recipe (`h100-gke-cos-training`) already selects the
automated `nccl-all-reduce-bw` performance check (floor `>= 300` GB/s), so the
benchmark is fully driven for you:

```shell
aicr validate --recipe recipes/overlays/h100-gke-cos-training.yaml \
  --phase performance
```

The validator runs the all-reduce sweep over the validator-fixed `1K`–`16G`
message-size range and asserts the busBW floor. It deploys the
`TrainingRuntime` (`validators/performance/testdata/h100/gke/runtime.yaml`)
**plus** the shared `TrainJob` (`validators/performance/testdata/trainjob.yaml`)
that actually launches the worker Pods. The runtime template carries the GKE
multi-NIC and NRI device annotation keys (`networking.gke.io/interfaces` and
`devices.gke.io/container.tcpxo-daemon`) as `${...}` placeholders, and the
validator **discovers and substitutes their concrete values dynamically** at
apply time — the interface list from the cluster's discovered GPU NIC networks
and the NRI device annotation sized to the per-node GPU count. Because those
values are resolved at runtime, the framework manifest cannot be reproduced by a
plain `kubectl apply` / `envsubst` of `runtime.yaml` alone, so running the
framework path by hand is not supported. Use `aicr validate` for the
framework-equivalent benchmark.

**Prerequisites:** the automated check needs at least **2 schedulable GPU nodes**
with allocatable GPUs — the all-reduce measures East-West fabric between nodes.
The validator counts *discovered* schedulable GPU nodes: with fewer than 2 it
returns a successful *skipped* result without measuring bandwidth. The selected
nodes also need **free** GPU capacity (the TrainJob places a full GPU node per
worker); if the GPUs are already occupied the workers stay Pending and the check
times out — it does not skip. If Kubeflow Trainer is not already installed, the validator
downloads and installs it (Trainer v2.2.0 from GitHub, then removes it
afterward), so the validator environment needs GitHub egress.

### Manual standalone benchmark

To exercise the GPUDirect TCPXO data path directly with raw Pods and a TCPXO
daemon sidecar (independent of the validator framework — useful for debugging),
use the standalone demo manifest. Each pod runs a `tcpxo-daemon` sidecar
(manages the GPUDirect TCPXO data path) plus the `nccl-test` container.

NRI profile (recommended, no `hostNetwork`):

```shell
kubectl create ns nccl-test
# Note: this manifest is pinned to the v1.0.15 / v1.0.21 pair, matching the
# installer the recipe deploys. If your cluster runs a different installer
# version, update both images to that cluster's pair before applying.
kubectl apply -f demos/workloads/training/gke-nccl-test-tcpxo.yaml -n nccl-test

# Wait for pods to be 2/2 Running
kubectl get pods -n nccl-test -o wide -w

# Trigger the AllReduce benchmark from host-1
kubectl exec nccl-test-host-1 -n nccl-test -c nccl-test -- bash -c '
  /scripts/init_ssh.sh nccl-host-1 nccl-host-2 &&
  pushd /scripts && /scripts/gen_hostfiles.sh nccl-host-1 nccl-host-2 && popd &&
  DATA_MIN=1K DATA_MAX=16G BENCHMARK=all_reduce_perf NHOSTS=2 \
    NCCL_LIB_DIR="/usr/local/nvidia/lib64" LD_LIBRARY_PATH="/usr/local/nvidia/lib64" \
    /scripts/demo-run-nccl-test-tcpxo-via-mpi.sh'

# Expected: ~340 GB/s busBW at 16 GB (AllReduce), ~100 GB/s avg
# Clean up
kubectl delete ns nccl-test
```

### Interpreting results

| Metric | Without TCPXO | With TCPXO |
|--------|--------------|------------|
| AllReduce busBW (16 GB) | ~4 GB/s | ~340 GB/s |
| AllReduce avg busBW | ~4 GB/s | ~100 GB/s |

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
