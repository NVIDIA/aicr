# AICR v1.3.0 golden fixtures

Produced on 2026-11-22 by the pinned AICR image
`ghcr.io/nvidia/aicr@sha256:4c7ac0f9461c14b7d9a4e86d7742b968a25149ed83bcbc52cd87e11d22871304`
(v1.3.0, commit `d349348`), unmodified, for the lane criteria.

| Fixture | Produced by |
| --- | --- |
| `recipe-h100-eks-ubuntu-training-kubeflow.json` | `aicr recipe --service eks --accelerator h100 --os ubuntu --intent training --platform kubeflow --format json` |
| `recipe-h100-eks-ubuntu-training.json` | The same without `--platform` |
| `recipe-h100-gke-cos-training-kubeflow.json` | `aicr recipe --service gke --accelerator h100 --os cos --intent training --platform kubeflow --gke-tcpxo-interfaces eth1=pilot-gpu-nic-0,...,eth8=pilot-gpu-nic-7 --format json` |
| `upgrade-check-v1.2.0-to-v1.3.0-eks.json` | `aicr upgrade-check --from <v1.2.0 EKS lane recipe> --to <v1.3.0 EKS lane recipe> --deployer argocd --format json --fail-on-error=false`: kube-prometheus-stack and nvsentinel `manual`, nodewright-operator `safe`, nktp added |
| `upgrade-check-v1.2.0-to-v1.3.0-gke.json` | The same for the GKE lane recipes, with the same verdicts |
| `upgrade-check-eks-training-to-kubeflow.json` | `aicr upgrade-check` from the training recipe to the training + kubeflow recipe (an added component) |
| `bundle-h100-eks-ubuntu-training-kubeflow/` | `aicr bundle --recipe <EKS lane recipe> --deployer helm --readiness-hooks`: the same files as the v1.2.0 fixture, and the full top-level listing (`LISTING.txt`) |

Notes:

- The `<v1.2.0 ... lane recipe>` inputs are the v1.2.0 fixtures in `../v1.2.0/`.
- Against v1.2.0 the lane recipes add nktp 0.2.0; kube-prometheus-stack moves
  from 84.4.0 to 84.5.0, nodewright-operator from v0.20.0 to v0.21.0 and
  nvsentinel from v1.28.0 to v1.31.0.
- kube-prometheus-stack's record names no `apply` step (`export`, `upgrade`,
  `clean-up`), so the operator holds all three steps for acknowledgement after
  the apply. nvsentinel's record ends with its `apply` step, so nothing is held
  after it.
- The bundle adds the nktp folder, so the numbered folder names after it
  differ from the v1.2.0 fixture (`LISTING.txt`).
- `schemas/` holds AICR's published `RecipeResult` and `Snapshot` schemas from
  the same commit. Against v1.2.0, `RecipeResult` adds the optional
  `metadata.artifacts` and `metadata.rdmaNamespaceMode`; `Snapshot` is
  unchanged.
- No snapshot fixture: the operator's snapshot parsing is unchanged, and the
  v1.0.0 captures still cover it.
