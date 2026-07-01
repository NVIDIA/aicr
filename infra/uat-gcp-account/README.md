# UAT GCP Account Setup

IAM configuration for the GKE UAT pipeline (`.github/workflows/uat-gcp.yaml`), invoked through the shared dispatch surface (`uat-run.yaml`) for ad-hoc runs and via the nightly batch (`uat-nightly-batch.yaml`) on a cron.

## Relationship to demo-api-server

This config shares the `eidosx` GCP project with `infra/demo-api-server`, which
owns the foundational resources:

| Resource | Owner | This Config |
|----------|-------|-------------|
| Workload Identity Pool (`github-actions-pool`) | demo-api-server | references (not managed) |
| WIF Provider (`github-actions-provider`) | demo-api-server | references (not managed) |
| Service Account (`github-actions`) | demo-api-server | data source + additive IAM |
| GCP API enablement | demo-api-server | additive (idempotent) |

This config adds **only** the IAM roles the service account needs for GKE
cluster lifecycle management (create, connect, destroy):

- `roles/container.admin` -- GKE cluster CRUD
- `roles/compute.admin` -- VPC, subnets, firewall, instances
- `roles/cloudkms.admin` -- KMS for secrets encryption
- `roles/iam.serviceAccountAdmin` -- Create node pool service accounts
- `roles/resourcemanager.projectIamAdmin` -- Bind roles to node pool SAs

All bindings use `google_project_iam_member` (additive), so they cannot conflict
with `demo-api-server`'s bindings on the same service account.

## Evidence-dashboard read SA (GP5)

`evidence-dashboard.tf` adds a dedicated **read-only** service account,
`evidence-read`, for the GP5 Pages publish workflow
(`.github/workflows/evidence-dashboard-publish.yaml`). Its only grant is
`roles/storage.objectViewer` on the UAT evidence bucket
(`var.evidence_bucket`, default `aicr-testgrid-staging`) -- a bucket-scoped,
additive `google_storage_bucket_iam_member` binding; the bucket itself is
managed elsewhere and is **not** created or owned here. It is impersonated
through the existing `github-actions-pool` federation (repository-scoped) and
is deliberately separate from the shared `github-actions` SA and the GP2
evidence-publish writer, so a Pages build can only read published evidence.

The publish workflow inlines this SA email directly (matching the repo norm for
`GCP_WIF_SERVICE_ACCOUNT`); the `EVIDENCE_READ_SERVICE_ACCOUNT` output exists so
the value can be confirmed against what the workflow hardcodes. GP3
(`infra/evidence-dashboard`) still owns the hardened data bucket and the
dedicated `objectCreator` writer.

## State

Backend: `gs://eidos-tf-state/uat-gcp` (separate prefix from `demo`).

## Usage

```bash
cd infra/uat-gcp-account
terraform init
terraform plan
terraform apply
```
