# Tested Platform Configuration

The run started on October 27, 2026 at 23:51 UTC and completed on October 28
at 00:00 UTC. The cluster has one ARM64 control-plane node and two ARM64
GB200 workers with four GPUs each. GPU requests use the NVIDIA device plugin's
`nvidia.com/gpu` extended resource.

| Component | Tested version |
| --- | --- |
| NVKE release | v0.22.0 qualification builds |
| NVKE node OS profile | `v0.2.0-rc.5-29-g400c24fb` |
| NVKE node agent | `0.3.0-rc.2-85-g400c24fb` |
| Ubuntu | 24.04.5 LTS |
| Kernel | `6.8.0-1064-nvidia-64k` |
| Kubernetes | v1.36.3 |
| Containerd | 2.3.0 |
| OVN-Kubernetes | 2.4.2 |
| GPU Operator | v26.7.2 |
| NVIDIA driver | 580.173.02 |
| NVIDIA Network Operator | v26.7.0 |
| NVIDIA DRA driver | 0.5.0 |
| NVSentinel | v1.28.0 |
| NodeWright | v0.20.0 |
| Node Feature Discovery | v0.20.1 |
| KAI Scheduler | v0.16.9 |
| Kubeflow Trainer | 2.2.0 |
| Agentgateway | v1.5.0 |
| kube-prometheus-stack chart | 84.4.0 |
| Prometheus Adapter chart | 5.3.0 |

The NVKE node builds are identified in the public
[NVKE v0.22.0 qualification report](https://github.com/dsx-ai-factory/nvke/releases/download/v0.22.0/QUALIFICATION.md).

The tested toolkit configuration uses `RUNTIME_CONFIG_SOURCE=file` and
`ACCEPT_NVIDIA_VISIBLE_DEVICES_ENVVAR_WHEN_UNPRIVILEGED=false`.
Both workers' runtime configuration records:

```toml
accept-nvidia-visible-devices-as-volume-mounts = false
accept-nvidia-visible-devices-envvar-when-unprivileged = false
```

The [RuntimeClass probes](runtimeclass-isolation.md) verify that a non-root
container requesting one GPU sees exactly one GPU, while a non-root container
requesting none cannot access GPU devices, even with
`NVIDIA_VISIBLE_DEVICES=all`.

## Conditional Cluster Autoscaling Requirement

NVKE v0.22.0 provides operator-driven node-pool scaling through
`desiredNodeCount`. The field changes when an operator requests a change;
this is not workload-driven cluster autoscaling. See the release's
[node-pool scaling documentation](https://github.com/dsx-ai-factory/nvke/blob/400c24fb6465b042be883d6b62472528520d6291/docs/workload-clusters/node-pools.md#scale-a-node-pool).

The conditional `cluster_autoscaling` requirement is N/A. GPU-metric HPA is
verified separately in [pod-autoscaling.md](pod-autoscaling.md).

## Additional Capability Scope

[DRA discovery](dra-support.md) verifies ComputeDomain ResourceSlices and the
DRA API. Whole-GPU DRA allocation, GPU sharing, vGPU, cross-node RDMA traffic,
model-aware inference routing, and disaggregated inference were not exercised.
The corresponding SHOULD requirements are not claimed as Implemented.
