# Shared GCP Foundation

Root identity configuration for the `eidosx` GCP project. Despite what its
former name (`demo-api-server`) suggested, this module is **not** about any one
workload — it owns the GitHub Actions to GCP trust relationship that most of
AICR's cloud CI depends on.

## What this owns

| Resource | Identifier |
|----------|------------|
| Workload Identity Pool | `github-actions-pool` |
| WIF Provider | `github-actions-provider` |
| Service Account | `github-actions@eidosx.iam.gserviceaccount.com` |
| Project-level IAM | 9 roles bound to that service account |
| GCP API enablement | 16 services |
| Artifact Registry | `demo` (retired, see below) |

The provider's attribute condition pins `NVIDIA/aicr` on `main` or a tag, and
the impersonation binding is scoped to the same repository.

## Who depends on it

Destroying or renaming the pool, provider, or service account breaks all of
these. They are hardcoded references, not module inputs:

- `.github/workflows/uat-gcp.yaml`
- `.github/workflows/uat-janitor.yaml`
- `.github/workflows/evidence-ingest.yaml`
- `.github/workflows/evidence-dashboard-publish.yaml` (impersonates
  `evidence-read@`, reached through this same pool)
- `infra/uat-gcp-account/` — consumes the service account as a **data source**,
  so `terraform plan` there fails outright if it disappears

`testgrid-publish.yml` also targets this project but does **not** belong on that
list: it authenticates through its own `aicr-testgrid[-<env>]-github` pool and
`aicr-testgrid[-<env>]-publish@` account, managed by the aicr-testgrid
Terraform rather than here.

`tools/corroborate/publish_workflow_test.go` also asserts on these identities.

## Retired: the demo API service

AICR ships `aicrd` as a self-hosted service and no longer publishes a hosted
demo. The `demo` Artifact Registry repository and the `roles/run.invoker` /
`roles/run.admin` grants in `federation.tf` are leftovers from that pipeline.
They are deliberately still declared: removing them from config destroys or
revokes them on the next apply, and the decision was to leave the GCP project
untouched. The Cloud Run service itself was never Terraform-managed.

## Usage

```bash
terraform -chdir=infra/gcp-shared init
terraform -chdir=infra/gcp-shared plan
```

Backend: `gs://eidos-tf-state/demo` — the prefix keeps its original name because
changing it is a state migration. `infra/uat-gcp-account` shares the bucket
under a separate prefix.
