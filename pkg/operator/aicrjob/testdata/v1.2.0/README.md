# AICR v1.2.0 golden fixtures

Produced on 2026-11-03 by the pinned AICR image
`ghcr.io/nvidia/aicr@sha256:5ef145efe98ede9f4da7be3a66304f5fdc95dd31f825295d6a608a39feb0cac9`
(v1.2.0, commit `935e074`), unmodified, for the lane criteria.

| Fixture | Produced by |
| --- | --- |
| `recipe-h100-eks-ubuntu-training-kubeflow.json` | `aicr recipe --service eks --accelerator h100 --os ubuntu --intent training --platform kubeflow --format json` |
| `recipe-h100-eks-ubuntu-training.json` | The same without `--platform` |
| `recipe-h100-gke-cos-training-kubeflow.json` | `aicr recipe --service gke --accelerator h100 --os cos --intent training --platform kubeflow --gke-tcpxo-interfaces eth1=pilot-gpu-nic-0,...,eth8=pilot-gpu-nic-7 --format json` |
| `upgrade-check-v1.1.2-to-v1.2.0-eks.json` | `aicr upgrade-check --from <v1.1.2 EKS lane recipe> --to <v1.2.0 EKS lane recipe> --deployer argocd --format json --fail-on-error=false`: nvidia-dra-driver-gpu `safe`, nfd `manual` |
| `upgrade-check-v1.1.2-to-v1.2.0-gke.json` | The same for the GKE lane recipes, with the same verdicts |
| `upgrade-check-eks-training-to-kubeflow.json` | `aicr upgrade-check` from the training recipe to the training + kubeflow recipe (an added component) |
| `bundle-h100-eks-ubuntu-training-kubeflow/` | `aicr bundle --recipe <EKS lane recipe> --deployer helm --readiness-hooks`: the same files as the v1.1.2 fixture, and the full top-level listing (`LISTING.txt`) |

Notes:

- The `<v1.1.2 ... lane recipe>` inputs are the v1.1.2 fixtures in `../v1.1.2/`.
- Against v1.1.2 the lane recipes keep the same components; nfd moves from
  0.19.0 to 0.20.1 and nvidia-dra-driver-gpu from 0.5.0 to 0.5.1.
- The bundle holds the same files, but its deployment order changed, so the
  numbered folder names differ from the v1.1.2 fixture (`LISTING.txt`).
- Upgrade reports add an optional per-component `route`, which the operator
  does not read.
- `schemas/` holds AICR's published `RecipeResult` and `Snapshot` schemas from
  the same commit. Against v1.1.2, `RecipeResult` only adds enum values;
  `Snapshot` is unchanged.
- No snapshot fixture: the operator's snapshot parsing is unchanged, and the
  v1.0.0 captures still cover it.
