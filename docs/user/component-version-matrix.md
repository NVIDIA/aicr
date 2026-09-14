<!--
Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
-->

# Component Version Matrix

Which component version shipped in each AICR release, and what changed between them.

Use this to work out what an upgrade actually moves. AICR does not track components release-for-release: a component may sit on one pin across several AICR releases and then move many versions at once, so the jump between two AICR releases is often larger than it looks.

**This page is generated.** Run `make version-matrix-docs` to refresh it. Prose outside the markers is preserved.

## How to read the change column

Two categories deserve more attention than a version bump normally would:

- **`chart-change`** means the chart's repository or name moved, so the two version strings belong to different version lines and cannot be compared. The pin looks like a downgrade but is not.
- **`backwards`** means the version genuinely decreased.

A jump of three or more major or minor versions is also flagged, because the further a pin moves in one step, the more upstream change a single upgrade has to absorb.

<!-- BEGIN AICR-VERSION-MATRIX -->
| Component | v0.13.0 | v0.14.0 | v0.15.0 | v0.16.0 | v0.17.0 | v0.18.0 | v0.19.0 | v0.20.0 |
|---|---|---|---|---|---|---|---|---|
| `agentgateway` | v2.2.1 | v2.2.1 | v2.2.1 | v2.2.1 | **v1.3.1** | v1.3.1 | v1.3.1 | v1.3.1 |
| `agentgateway-crds` | v2.2.1 | v2.2.1 | v2.2.1 | v2.2.1 | **v1.3.1** | v1.3.1 | v1.3.1 | v1.3.1 |
| `aws-ebs-csi-driver` | 2.59.0 | 2.59.0 | 2.59.0 | 2.59.0 | 2.59.0 | 2.59.0 | 2.59.0 | 2.59.0 |
| `aws-efa` | v0.5.26 | v0.5.26 | v0.5.26 | **v0.5.29** | v0.5.29 | v0.5.29 | v0.5.29 | v0.5.29 |
| `cert-manager` | v1.20.2 | v1.20.2 | v1.20.2 | v1.20.2 | v1.20.2 | v1.20.2 | v1.20.2 | v1.20.2 |
| `dynamo-platform` | 1.0.2 | 1.0.2 | **1.2.0** | 1.2.0 | **1.2.1** | 1.2.1 | 1.2.1 | 1.2.1 |
| `gatekeeper` | – | – | 3.22.2 | 3.22.2 | 3.22.2 | 3.22.2 | 3.22.2 | 3.22.2 |
| `gpu-operator` | v26.3.1 | v26.3.1 | **v26.3.2** | v26.3.2 | v26.3.2 | **v26.3.3** | v26.3.3 | v26.3.3 |
| `grove` | v0.1.0-alpha.6 | v0.1.0-alpha.6 | **v0.1.0-alpha.8** | v0.1.0-alpha.8 | v0.1.0-alpha.8 | v0.1.0-alpha.8 | v0.1.0-alpha.8 | v0.1.0-alpha.8 |
| `k8s-aibom` | – | – | – | – | – | – | – | 1.3.0 |
| `k8s-ephemeral-storage-metrics` | 1.19.2 | 1.19.2 | 1.19.2 | 1.19.2 | 1.19.2 | 1.19.2 | 1.19.2 | 1.19.2 |
| `k8s-nim-operator` | 3.1.0 | 3.1.0 | 3.1.0 | 3.1.0 | 3.1.0 | 3.1.0 | 3.1.0 | 3.1.0 |
| `k8s-nim-operator-ocp` | – | – | – | – | – | – | 3.1.0 | 3.1.0 |
| `kai-scheduler` | v0.14.1 | v0.14.1 | v0.14.1 | v0.14.1 | v0.14.1 | v0.14.1 | v0.14.1 | v0.14.1 |
| `kube-prometheus-stack` | 84.4.0 | 84.4.0 | 84.4.0 | 84.4.0 | 84.4.0 | 84.4.0 | 84.4.0 | 84.4.0 |
| `kubeflow-trainer` | 2.2.0 | 2.2.0 | 2.2.0 | 2.2.0 | 2.2.0 | 2.2.0 | 2.2.0 | 2.2.0 |
| `kueue` | 0.17.1 | 0.17.1 | 0.17.1 | 0.17.1 | 0.17.1 | **0.18.2** | 0.18.2 | 0.18.2 |
| `mariadb-operator` | – | – | – | – | – | – | 26.6.0 | 26.6.0 |
| `mariadb-operator-crds` | – | – | – | – | – | – | 26.6.0 | 26.6.0 |
| `network-operator` | 26.1.1 | 26.1.1 | 26.1.1 | 26.1.1 | 26.1.1 | 26.1.1 | **26.4.1** | 26.4.1 |
| `nfd` | 0.18.3 | 0.18.3 | 0.18.3 | 0.18.3 | **0.19.0** | 0.19.0 | 0.19.0 | 0.19.0 |
| `nodewright-operator` | v0.15.1 | v0.15.1 | **v0.17.0** | **v0.17.1** | v0.17.1 | v0.17.1 | v0.17.1 | v0.17.1 |
| `nvidia-dra-driver-gpu` | 25.12.0 | 25.12.0 | **0.4.1-rc.1** | 0.4.1-rc.1 | **0.4.1** | 0.4.1 | 0.4.1 | 0.4.1 |
| `nvidia-dra-driver-gpu-ocp` | – | – | – | – | – | – | 0.4.1 | 0.4.1 |
| `nvsentinel` | v1.3.0 | v1.3.0 | **v1.9.0** | v1.9.0 | v1.9.0 | v1.9.0 | v1.9.0 | **v1.20.0** |
| `prometheus-adapter` | 5.3.0 | 5.3.0 | 5.3.0 | 5.3.0 | 5.3.0 | 5.3.0 | 5.3.0 | 5.3.0 |
| `prometheus-adapter-ocp` | – | – | – | – | – | – | 5.3.0 | 5.3.0 |
| `prometheus-operator-crds` | 28.0.1 | 28.0.1 | 28.0.1 | 28.0.1 | 28.0.1 | 28.0.1 | 28.0.1 | 28.0.1 |
| `slinky-slurm` | – | 1.1.0 | 1.1.0 | 1.1.0 | **1.2.0** | 1.2.0 | 1.2.0 | 1.2.0 |
| `slinky-slurm-operator` | 1.1.0 | 1.1.0 | 1.1.0 | 1.1.0 | **1.2.0** | 1.2.0 | 1.2.0 | 1.2.0 |
| `slinky-slurm-operator-crds` | 1.1.0 | 1.1.0 | 1.1.0 | 1.1.0 | **1.2.0** | 1.2.0 | 1.2.0 | 1.2.0 |
| `slinky-topograph` | – | – | – | – | – | 0.5.0 | 0.5.0 | **1.0.0** |
| `slurm-accounting-mariadb` | – | – | – | – | – | – | 26.6.0 | 26.6.0 |

**bold** = changed from the previous release. 33 components across 8 releases.

## Transitions

| Component | Release | From | To | Change |
|---|---|---|---|---|
| `agentgateway` | v0.17.0 | `v2.2.1` | `v1.3.1` | **major -1 backwards** |
| `agentgateway-crds` | v0.17.0 | `v2.2.1` | `v1.3.1` | **major -1 backwards** |
| `aws-efa` | v0.16.0 | `v0.5.26` | `v0.5.29` | patch +3 |
| `dynamo-platform` | v0.15.0 | `1.0.2` | `1.2.0` | minor +2 |
| `dynamo-platform` | v0.17.0 | `1.2.0` | `1.2.1` | patch +1 |
| `gpu-operator` | v0.15.0 | `v26.3.1` | `v26.3.2` | patch +1 |
| `gpu-operator` | v0.18.0 | `v26.3.2` | `v26.3.3` | patch +1 |
| `kueue` | v0.18.0 | `0.17.1` | `0.18.2` | minor +1 |
| `network-operator` | v0.19.0 | `26.1.1` | `26.4.1` | **minor +3** |
| `nfd` | v0.17.0 | `0.18.3` | `0.19.0` | minor +1 |
| `nodewright-operator` | v0.15.0 | `v0.15.1` | `v0.17.0` | **chart-change** |
| `nodewright-operator` | v0.16.0 | `v0.17.0` | `v0.17.1` | patch +1 |
| `nvidia-dra-driver-gpu` | v0.15.0 | `25.12.0` | `0.4.1-rc.1` | **chart-change** |
| `nvsentinel` | v0.15.0 | `v1.3.0` | `v1.9.0` | **minor +6** |
| `nvsentinel` | v0.20.0 | `v1.9.0` | `v1.20.0` | **minor +11** |
| `slinky-slurm` | v0.17.0 | `1.1.0` | `1.2.0` | minor +1 |
| `slinky-slurm-operator` | v0.17.0 | `1.1.0` | `1.2.0` | minor +1 |
| `slinky-slurm-operator-crds` | v0.17.0 | `1.1.0` | `1.2.0` | minor +1 |
| `slinky-topograph` | v0.20.0 | `0.5.0` | `1.0.0` | major +1 |

19 transitions across 8 releases; **7 need scrutiny** (chart change, backwards move, or >= 3 major/minor apart).
<!-- END AICR-VERSION-MATRIX -->
