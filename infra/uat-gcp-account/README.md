# UAT GCP Account Setup

IAM configuration for the nightly GKE UAT workflow (`.github/workflows/uat-gcp.yaml`).

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

## State

Backend: `gs://eidos-tf-state/uat-gcp` (separate prefix from `demo`).

## Usage

```bash
cd infra/uat-gcp-account
terraform init
terraform plan
terraform apply
```
