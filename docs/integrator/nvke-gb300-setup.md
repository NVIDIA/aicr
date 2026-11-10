# NVKE GB300 Setup

The `gb300-nvke-ubuntu-*` recipes target NVKE (NVIDIA Kubernetes Engine):
bare-metal GB300 NVL72 trays running Ubuntu 26.04 on the arm64 `-nvidia-64k`
kernel. Four Preview coordinates:

- `gb300-nvke-ubuntu-training`
- `gb300-nvke-ubuntu-training-kubeflow` — adds Kubeflow Trainer to the training
  leaf
- `gb300-nvke-ubuntu-inference` — platform-neutral inference base, which
  resolves when `--platform` is omitted
- `gb300-nvke-ubuntu-inference-dynamo` — adds `dynamo-platform` and `grove` to
  the inference leaf

They pair the NVKE platform handling of the `vr200-nvke-ubuntu-*` leaves with
GB300 node facts. Unlike VR200, GB300 needs no particular kernel flavour: the
generic `nvidia-64k` kernel brings all four GPUs up.

## Cluster prerequisites

- Kubernetes 1.34 or newer (DRA `resource.k8s.io/v1` for ComputeDomains).
  Observed in practice: 1.36.3.
- **Ubuntu 26.04 on a 7.0 kernel.** Observed: `7.0.0-1018-nvidia-64k`.
- Nodes carry `providerID: nico://<uuid>`, so `aicr snapshot` resolves
  `service: nvke` without `--service`, and the GPU model resolves
  `accelerator: gb300`.
- No pre-installed driver. GPU Operator installs 595.91.07: the registry pin,
  580.173.02, publishes no `-ubuntu26.04` driver container.

## A control plane on a GPU tray

When the tenant control plane runs on a GB300 tray, keep GPU Operator's
operands and the DRA kubelet plugin off it. Label the node before deploying:

```shell
CP_NODE="my-control-plane-node"  # replace with the control plane's node name
kubectl label node "${CP_NODE}" nvidia.com/gpu.deploy.operands=false
```

and generate the bundle with
`--accelerated-node-selector nvidia.com/gpu.deploy.driver=true`. GPU Operator
sets that label only where it deploys the driver, so the selector keeps the DRA
kubelet plugin on the workers.

## Generating the bundle

```shell
# List the label values with: kubectl get nodes -L kubernetes.io/hostname
GPU_NODE="my-gpu-worker"  # replace with a GPU worker's kubernetes.io/hostname label value
aicr snapshot --node-selector "kubernetes.io/hostname=${GPU_NODE}" --privileged -o snapshot.yaml
aicr recipe --snapshot snapshot.yaml --intent inference --platform dynamo -o recipe.yaml
aicr bundle -r recipe.yaml -o bundle \
  --accelerated-node-selector nvidia.com/gpu.deploy.driver=true \
  --dra-eviction-node-label nvidia.com/dra-kubelet-plugin=true \
  --set agentgateway:serviceType=ClusterIP
```

`--set agentgateway:serviceType=ClusterIP` applies to the inference leaves on a
tenant with no LoadBalancer. NVKE provisions none, and
[External address for the gateway](nvke-vr200-setup.md#external-address-for-the-gateway)
applies unchanged.

## NVLink only

None of the four leaves ships an east-west stack: no `network-operator`,
`sriov-ib-networks` or `rdma-modules`, and the training leaf declares no
`nccl-all-reduce-bw-net`. The four InfiniBand rails per tray that
`gb200-nvke-ubuntu-training` wires do not describe these trays, and a NET floor
nothing has measured would be a placeholder. Multi-node training traffic is
MNNVL through a ComputeDomain IMEX channel, inside one NVLink domain.

## What has been measured

**Training, MNNVL.** NCCL `all_reduce_perf` across two trays, 8 GPUs, 1 KiB to
16 GiB: 840.7 GB/s busbw at 16 GiB, zero mismatches, one NVLS communicator with
`nvlsRanks 8` and `MNNVL 1` on every rank. The run used the `gb200/nvke` NVLS
runtime that `testdata/gb300/nvke/runtime-nvls.yaml` copies unchanged, with a
TrainJob and the validator's ComputeDomain spec rendered by hand; the
compiled-validator path for `gb300/nvke` is exercised by CI on merge. One tray
alone reaches about 670 GB/s, so the `>= 700` floor, which the GB300 EKS and OKE
leaves use, cannot be met without NVLS spanning trays.

**Inference, end to end.** The cluster was deployed from the
`-inference-dynamo` leaf's own bundle, with
`--set agentgateway:serviceType=ClusterIP`. `aicr validate --phase all
--cncf-submission` then returned deployment 4/4 and conformance 11/11:

- `driver-runtime-management` found driver 595.91.07 working in a vanilla
  image.
- `inference-gateway` passed with the Gateway `Programmed=True` on a ClusterIP
  Service.

The upstream validator accepted the submission against the v1.36 checklist,
with every applicable MUST established. `cluster_autoscaling` is N/A on NVKE.

## Known gaps

- No east-west NIC path is qualified.
- No `inference-perf` floor on the Dynamo leaf: nothing has measured Dynamo
  serving on NVKE.
