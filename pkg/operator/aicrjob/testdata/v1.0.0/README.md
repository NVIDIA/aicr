# AICR v1.0.0 golden fixtures

Produced on 2026-10-14 by the pinned AICR image
`ghcr.io/nvidia/aicr@sha256:e91ba6f4916a5bfcd19d05933b5b4d07a4dd05a738a1ffebfab243a32cbff433`
(v1.0.0, commit `82bccef`), unmodified, for the pilot lane criteria.

| Fixture | Produced by |
| --- | --- |
| `recipe-h100-eks-ubuntu-training-kubeflow.json` | `aicr recipe --service eks --accelerator h100 --os ubuntu --intent training --platform kubeflow --format json` |
| `recipe-h100-eks-ubuntu-training.json` | The same without `--platform` |
| `recipe-h100-gke-cos-training-kubeflow.json` | `aicr recipe --service gke --accelerator h100 --os cos --intent training --platform kubeflow --gke-tcpxo-interfaces eth1=pilot-gpu-nic-0,...,eth8=pilot-gpu-nic-7 --format json` |
| `recipe-v0.22.0-h100-eks-ubuntu-training-kubeflow.json` | The EKS lane recipe from AICR v0.22.0, the input for an upgrade assessment |
| `upgrade-check-v0.22.0-to-v1.0.0-eks.json` | `aicr upgrade-check --from <v0.22.0 recipe> --to <v1.0.0 recipe> --deployer argocd --format json --fail-on-error=false` |
| `upgrade-check-eks-training-to-kubeflow.json` | `aicr upgrade-check` from the training recipe to the training + kubeflow recipe (an added component) |
| `snapshot-kind-no-gpu.json` | `aicr snapshot` in exact-ServiceAccount mode on a kind v1.37 node without GPUs (`make operator-e2e`), keys sorted |
| `snapshot-eks-amazonlinux.json` | `snapshot-kind-no-gpu.json` edited to an EKS node on Amazon Linux 2023: the K8s node provider, OS image, kernel and OS release, with AICR's fingerprint to match. Not a capture; AICR v1.0.0 relaxes `os=amazonlinux` to `any` unless the OS is stated |
| `snapshot-eks-pre-driver.json` | `snapshot-kind-no-gpu.json` edited to an EKS Ubuntu node before the GPU stack: no GPU feature-discovery labels, the GPU hardware subtype reporting a PCI-detected H100 (`model: h100`), with AICR's fingerprint to match. Not a capture |
| `bundle-h100-eks-ubuntu-training-kubeflow/` | `aicr bundle --recipe <EKS lane recipe> --deployer helm --readiness-hooks`: `bundle-info.yaml`, the bundle's `recipe.yaml`, one upstream-chart release (`009-gpu-operator/upstream.env`), one wrapper-chart release (`010-gpu-operator-readiness/Chart.yaml`) and the full top-level listing (`LISTING.txt`) |

Notes:

- The GKE recipe requires the TCPXO interface mapping and is emitted as
  `aicr.run/v1beta2`.
- The bundle drops `dra-node-labeler`, which the resolved recipe lists, because
  no DRA eviction label was set; the bundle's `recipe.yaml` reflects that.
- `schemas/` holds AICR's published `RecipeResult` and `Snapshot` schemas from
  the same commit. AICR publishes no schema for `bundle-info.yaml` or the
  upgrade-check report; these fixtures pin their shape.
