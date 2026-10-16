# Nebius B300 inference recipe

The `b300-mk8s-ubuntu-inference-dynamo` recipe targets Nebius Managed Service for
Kubernetes (MK8s), Ubuntu 24.04 and B300 SXM workers. B300 is distinct from the
Grace Blackwell GB300 platform. This local contribution is undergoing qualification;
it does not yet carry signed upstream evidence.

## Reference configuration

Use Kubernetes 1.34 or newer and driverless GPU workers: GPU Operator owns the
NVIDIA driver and the Nebius Marketplace Network Operator owns the DOCA driver.
The reference testbed has two workers with eight B300 GPUs each. Do not install
this recipe over another operator installation without resolving ownership first.

GPU Operator advertises whole GPUs as `nvidia.com/gpu`. Standalone DRA enables
ComputeDomains only. This recipe does not validate full-GPU DRA allocation or
cross-node NVLink. Network Operator supplies NFD; standalone NFD is disabled.

The eight active interfaces, `ib4` through `ib11`, are exposed as one shared
`rdma/ib` resource with 63 allocations per node. One allocation exposes all eight
HCAs; it does not reserve an exclusive NIC or guarantee bandwidth. No Multus,
IPAM, IPPool or IPoIBNetwork is required. Confirm interface names before using a
different Nebius node image or instance type.

## Generate and test

```bash
aicr snapshot --output snapshot.yaml
aicr recipe --snapshot snapshot.yaml --service mk8s --accelerator b300 \
  --os ubuntu --intent inference --platform dynamo --nodes 2 --output recipe.yaml
aicr bundle --recipe recipe.yaml --deployer argocd-helm \
  --accelerated-node-selector nvidia.com/gpu.present=true \
  --accelerated-node-toleration nvidia.com/gpu:NoSchedule --output bundle
```

Detection of MK8s requires the Nebius provider and a managed node-group label;
a generic Kubernetes cluster running on Nebius VMs is not sufficient evidence.
Explicit criteria remain available for older snapshots lacking that label.

Run deployment, conformance and performance phases against the resolved recipe.
The inference benchmark uses Qwen3-8B, concurrency 256 per GPU, Dynamo routing,
and the Nebius `compute-csi-default-sc` StorageClass for its temporary model cache.
The initial acceptance gates are 50,000 output tokens/second and 2,000 ms p99 TTFT,
using the validator's standard tolerance. These are acceptance criteria, not
claimed B300 measurements. Record actual throughput, latency, GPU occupancy and
runtime image versions in qualification evidence. The stock inference validator
currently uses Dynamo vLLM runtime 1.4.2 with this recipe's operator 1.5.0;
qualification must exercise that pairing, not imply identical versions.

The Network Operator 25.7.0 marketplace distribution, KAI Scheduler v0.17.1 and
Dynamo platform 1.5.0 pins are scoped exceptions matching the reference stack.
Other recipes continue to use registry defaults. Monitoring and lab-specific
Prometheus endpoints are not prerequisites of this minimal inference stack.

The generated catalog image inventory uses registry chart sources and shared
values. Its Network Operator 25.7.0 variant is not an inventory of the Nebius
Marketplace distribution. Use the resolved Nebius bundle and its mirrored image
inventory when preparing deployment-specific artifacts.

KWOK profiles model scheduling only; they cannot validate GPU execution, driver
health, RDMA traffic or performance. Use the live validators and record signed
recipe evidence before upstream submission, following
[recipe development](recipe-development.md) and
[evidence publishing](../contributor/evidence-publishing.md).
