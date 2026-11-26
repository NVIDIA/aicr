# Mistral GB300 Setup

The `gb300-mistral-ubuntu-training` recipe targets GB300 NVL72 on Mistral
Compute with Ubuntu. It carries the generic GB300 recipe's GPU Operator,
node tuning, DRA driver and NVLS benchmark settings, without the Network
Operator and NFD. This is a Preview recipe; signed validation evidence is
pending.

## Provider-owned components

Mistral installs NVIDIA add-ons as cluster-creation options. The recipe
assumes the Network Operator option is always set: Mistral's Network Operator
and its NFD install drive the fabric and the storage clients, which select
nodes by the Network Operator's MOFED readiness label. The recipe therefore
never installs the Network Operator or NFD.

The GPU Operator option is the `gpuStack` profile. Its one value,
`mistral-managed` (the default), is a cluster created with that option: the
recipe installs no GPU Operator.

Under `mistral-managed` the recipe records an external `nvidia.com/gpu`
advertiser and also leaves out Prometheus Adapter, pending a qualified scrape
of Mistral's DCGM exporter. Snapshot-based generation requires
`GPU.hardware.driver-loaded` and `K8s.policy.devicePlugin.enabled` to be
`true`, so a cluster created without Mistral's GPU Operator fails closed
instead of resolving to a bundle with no GPU stack.

A cluster created without Mistral's GPU Operator option is not covered yet.
A value that installs the GPU Operator needs a snapshot check that tells
Mistral's installation apart from AICR's. Mistral's GPU Operator installs as
Helm release `gpu-operator` in namespace `gpu-operator` with plain Helm
labels, the same coordinates the recipe would use, so no such check exists
today.

## Generate and inspect

Mistral nodes are provisioned by Metal3. A snapshot resolves
`service=mistral` when its nodes also carry a non-empty
`mistralcompute.ai/node-pool` label; without one it reports `metal3`.

```bash
aicr snapshot --node-selector nvidia.com/gpu.present=true --output snapshot.yaml
aicr recipe --snapshot snapshot.yaml --intent training --output recipe.yaml
aicr bundle --recipe recipe.yaml --deployer argocd --output ./bundle
```

Criteria-only generation records the provider checks without observing a
cluster:

```bash
aicr recipe --service mistral --accelerator gb300 --os ubuntu \
  --intent training --output recipe.yaml
```

## Qualification scope

This page covers `mistral / gb300 / ubuntu / training`. Deployment and
conformance validation are inherited from the GB300 recipes and run against
whichever GPU Operator is installed; Mistral's installation uses the
`gpu-operator` namespace those checks read. The NVLS benchmark uses the
`gb300/mistral` runtime, which starts from `gb300/generic` and has the same
823 GB/s threshold. The validator applies a 10% tolerance, so a run passes at
740.7 GB/s or more. No driver version is pinned under
`mistral-managed`: Mistral owns that lifecycle.
