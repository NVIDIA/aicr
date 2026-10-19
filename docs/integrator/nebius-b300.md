# Nebius B300 inference recipe

The `b300-mk8s-ubuntu-inference-dynamo` recipe targets Nebius Managed Service for
Kubernetes (MK8s), Ubuntu 24.04 and B300 SXM workers. B300 is distinct from the
Grace Blackwell GB300 platform. The contribution includes signed live
qualification evidence for the reference configuration below.

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

Run deployment, conformance and performance phases against `bundle/recipe.yaml`,
the resolved recipe actually deployed.
The inference benchmark uses Qwen3-8B, concurrency 256 per GPU, Dynamo routing,
and the Nebius `compute-csi-default-sc` StorageClass for its temporary model cache.
The initial acceptance gates are 50,000 output tokens/second and 2,000 ms p99 TTFT,
using the validator's standard tolerance. These are acceptance criteria, not
claimed B300 measurements. Record actual throughput, latency, GPU occupancy and
runtime image versions in qualification evidence. The stock inference validator
selects the runtime version from the recipe's Dynamo component pin; this recipe
uses operator and runtime 1.5.0. The qualified run sets
`AICR_INFERENCE_PERF_ROUTER_MODE=kv` in the `inference-perf` validator catalog
entry. Record that override when reproducing the result; the default router mode
is not what this evidence qualifies.

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

## Qualification evidence boundary

The October 16, 2026 full run at source
`3a80fbf9e10c806e1a554faa22d602978680a2e8` passed all eight checks in one
invocation: four deployment, three conformance and one performance check, with
no failures or skips. The generated bundle was installed onto an existing
cluster using resource adoption with GitOps reconciliation paused. This is not
a pristine clean-install or in-place upgrade qualification.

Dynamo operator and vLLM runtime were both **1.5.0**, with **KV routing**.
The observed runtime image was
`nvcr.io/nvidia/ai-dynamo/vllm-runtime@sha256:d7f73fdc1a66d2e1e0bdec2a74f49093ff6c85bc5e9f5ddf8296634418abe761`.
Qwen3-8B ran on eight one-GPU workers on one B300 node and achieved
**148,254.48 output tokens/second** and **1,211.49 ms p99 TTFT**, passing the
unchanged acceptance gates. The overlay pins NVIDIA driver **595.91.07**;
NVIDIA-SMI validation passed on both GPU nodes.

The [signed evidence pointer](https://github.com/NVIDIA/aicr/blob/main/recipes/evidence/b300-mk8s-ubuntu-inference-dynamo/45d8f16e5893804eca45b900bd037446/sha256-c3c8443e6b6b049fdf204c1e8ab744cb2df4f8948b4fe48a0fa69a2303c7593f.yaml)
references the public OCI bundle. The
[signing workflow](https://github.com/kky-fury/aicr/actions/runs/38969685302)
completed successfully; anonymous signature and integrity verification passed
with the exact fork workflow identity pinned. The evidence records the tested
source commit, rather than a later documentation or evidence-pointer commit.

The single-node inference result **does not qualify inter-node GPUDirect RDMA**.
Exposing all eight HCAs through `rdma/ib`, node readiness and scheduling checks
also do not establish cross-node traffic. An inter-node claim requires separate
multi-node communication evidence; this recipe's inference result makes no
such claim.
