# AICR v1.1.2 golden fixtures

Produced on 2026-10-22 by the pinned AICR image
`ghcr.io/nvidia/aicr@sha256:611cbcb8a44b30ccfbbf77a797ed0a8e7fae228c97088a849f70b042709a62fb`
(v1.1.2, commit `2bf662b`), unmodified, for the pilot lane criteria. v1.1.2 is
v1.1.0 rebuilt with Go 1.27.3; it renders what v1.1.0 renders.

| Fixture | Produced by |
| --- | --- |
| `recipe-h100-eks-ubuntu-training-kubeflow.json` | `aicr recipe --service eks --accelerator h100 --os ubuntu --intent training --platform kubeflow --format json` |
| `recipe-h100-eks-ubuntu-training.json` | The same without `--platform` |
| `recipe-h100-gke-cos-training-kubeflow.json` | `aicr recipe --service gke --accelerator h100 --os cos --intent training --platform kubeflow --gke-tcpxo-interfaces eth1=pilot-gpu-nic-0,...,eth8=pilot-gpu-nic-7 --format json` |
| `upgrade-check-v1.0.0-to-v1.1.2-eks.json` | `aicr upgrade-check --from <v1.0.0 EKS lane recipe> --to <v1.1.2 EKS lane recipe> --deployer argocd --format json --fail-on-error=false`: gpu-operator and nodewright-operator `safe`, nvsentinel `manual` |
| `upgrade-check-eks-training-to-kubeflow.json` | `aicr upgrade-check` from the training recipe to the training + kubeflow recipe (an added component) |
| `bundle-h100-eks-ubuntu-training-kubeflow/` | `aicr bundle --recipe <EKS lane recipe> --deployer helm --readiness-hooks`: the same files as the v1.0.0 fixture, and the full top-level listing (`LISTING.txt`) |

Notes:

- The bundle layout and its top-level listing are unchanged from v1.0.0.
- `schemas/` holds AICR's published `RecipeResult` and `Snapshot` schemas from
  the same commit. Against v1.0.0, `RecipeResult` only adds enum values and
  optional fields; `Snapshot` is unchanged.
- No snapshot fixture: the operator's snapshot parsing is unchanged, and the
  v1.0.0 captures still cover it. `make operator-e2e` takes a real v1.1.2
  snapshot on kind.
