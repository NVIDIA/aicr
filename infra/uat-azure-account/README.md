# Azure Entra ID OIDC Setup for GitHub Actions

Terraform configuration for GitHub Actions → Microsoft Entra ID OIDC federation enabling keyless auth for the Azure UAT pipeline ([`.github/workflows/uat-azure.yaml`](../../.github/workflows/uat-azure.yaml)) that creates ephemeral AKS clusters. The pipeline is invoked through the shared dispatch surface (`uat-run.yaml`) for ad-hoc runs and — once enrolled — via the nightly batch (`uat-nightly-batch.yaml`) on a cron.

## Prerequisites

- [Terraform](https://www.terraform.io/downloads.html) >= 1.9.5
- `az login` as a user with:
  - **Contributor** on the target subscription — to create the identity resource group, managed identity, and federated credentials (no Entra directory role is needed; see Why a Managed Identity below)
  - **Owner** or **User Access Administrator** on the subscription — only for the three role assignments; without it, apply with `-var manage_role_assignments=false` and have a subscription admin create them (commands under Permissions Granted)

## Why a Managed Identity

The federation subject is a **user-assigned managed identity**, not an app registration: this tenant blocks member app-registration creation (`authorizationPolicy.defaultUserRolePermissions.allowedToCreateApps=false`) and grants no directory roles, so an `azuread_application` apply fails with `Authorization_RequestDenied`. A managed identity is an ARM resource — subscription Contributor suffices — supports the same federated credentials, and `azure/login` consumes its client ID unchanged.

## What This Creates

- **Resource Group**: `aicr-uat-identity-rg` (holds only the pipeline identity)
- **Managed Identity**: user-assigned, `github-actions-aicr`
- **Federated Identity Credentials**: two, one per allowed OIDC subject (Entra matches subjects exactly, no wildcards):
  - `repo:NVIDIA/aicr:ref:refs/heads/main`
  - `repo:NVIDIA/aicr:ref:refs/heads/test/uat`
- **Role Assignments** (when `manage_role_assignments=true`): two subscription-scoped roles for the identity — **Contributor** and **Role Based Access Control Administrator** (ABAC-constrained to the three role definitions the pipeline assigns; see Permissions Granted). Data-plane kubectl access is NOT granted here — the pipeline self-assigns **Azure Kubernetes Service RBAC Cluster Admin** per run, scoped to the run's resource group

**No client secret is created** — authentication is OIDC-only (a managed identity cannot even have a secret). GitHub mints a short-lived token per workflow run, `azure/login` exchanges it against the federated credentials above, and the resulting `az` CLI context is what the AKS actuator container consumes.

## Usage

```bash
cd infra/uat-azure-account
terraform init
terraform plan
terraform apply       # add -var manage_role_assignments=false without Owner/UAA
terraform output      # IDs needed for the GitHub Actions workflow
```

## Configuration Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `tenant_id` | Entra ID tenant hosting the managed identity | `43083d15-…` (NVIDIA) |
| `subscription_id` | Subscription the UAT clusters are provisioned into (`deployment.tenancy` in `tests/uat/azure/cluster-config.yaml`) | `e88faa01-…` |
| `git_repo` | GitHub repository (format: owner/repo) | `NVIDIA/aicr` |
| `identity_name` | Name of the user-assigned managed identity | `github-actions-aicr` |
| `location` | Region for the identity resource group | `westus` |
| `manage_role_assignments` | Create the subscription role assignments (needs Owner/UAA) | `true` |

## Permissions Granted

- **Contributor** on the whole subscription. The AKS actuator creates resource groups, AKS clusters, VNets, and node pools — there is no pre-created resource group to scope down to. Revisit narrowing to a dedicated resource-group scope if the actuator gains a fixed-RG mode.
- **Role Based Access Control Administrator** on the subscription, constrained by an ABAC condition to assigning/deleting exactly three role definitions: Network Contributor (`4d97b98b…`, VNet access for the cluster identity) and AcrPull (`7f951dda…`, kubelet image pulls) — the two assignments the actuator's `iam.tf` creates (and removes on destroy) — plus Azure Kubernetes Service RBAC Cluster Admin (`b1ff04bb…`, see next bullet). Contributor alone cannot write role assignments, and the ABAC condition means the pipeline identity still cannot grant itself (or anyone else) any other access.
- **Azure Kubernetes Service RBAC Cluster Admin** — deliberately NOT a standing subscription-scoped grant. The actuator provisions AKS with Entra RBAC and local accounts disabled, so the pipeline needs a Kubernetes data-plane role for `kubectl` (via kubelogin's `azurecli` mode) — but a subscription-scoped grant would be cluster-admin on every Azure-RBAC cluster in this **shared** (non-UAT-dedicated) subscription. Instead the workflow self-assigns the role per run, scoped to the run's resource group (`uat-azure.yaml` "Grant run-scoped cluster admin", permitted by the ABAC condition above); the assignment is deleted with the resource group at teardown.
- No Entra directory roles — the pipeline identity cannot manage app registrations, groups, or other directory objects.

**Out-of-band assignment** (when the deployer lacks Owner/UAA and applied with `-var manage_role_assignments=false`) — a subscription admin runs, with `ASSIGNEE=$(terraform output -raw AZURE_PRINCIPAL_ID)` and `SUB=$(terraform output -raw AZURE_SUBSCRIPTION_ID)`:

```bash
az role assignment create --assignee-object-id "$ASSIGNEE" --assignee-principal-type ServicePrincipal \
  --role "Contributor" --scope "/subscriptions/$SUB"
az role assignment create --assignee-object-id "$ASSIGNEE" --assignee-principal-type ServicePrincipal \
  --role "Role Based Access Control Administrator" --scope "/subscriptions/$SUB" \
  --condition-version "2.0" \
  --condition "((!(ActionMatches{'Microsoft.Authorization/roleAssignments/write'})) OR (@Request[Microsoft.Authorization/roleAssignments:RoleDefinitionId] ForAnyOfAnyValues:GuidEquals {4d97b98b-1d4f-4787-a291-c67834d212e7, 7f951dda-4ed3-4680-a7ca-43fe172d538d, b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b})) AND ((!(ActionMatches{'Microsoft.Authorization/roleAssignments/delete'})) OR (@Resource[Microsoft.Authorization/roleAssignments:RoleDefinitionId] ForAnyOfAnyValues:GuidEquals {4d97b98b-1d4f-4787-a291-c67834d212e7, 7f951dda-4ed3-4680-a7ca-43fe172d538d, b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b}))"
```

## State Bootstrap (one-time)

The AKS actuator stores its Terraform state in an `azurerm` backend inside the target subscription (`deployment.state: tenancy` in `tests/uat/azure/cluster-config.yaml`), which is what lets a fresh runner tear down a cluster another run provisioned. That backend is **not** created by the actuator or by this Terraform — bootstrap it once per subscription with the actuator repo's setup tool ([upstream requirement](https://github.com/mchmarny/cluster/blob/main/provider/aks/README.md)):

```bash
# from a checkout of github.com/mchmarny/cluster, az-logged-in to the UAT subscription
provider/aks/tools/setup -c <path-to>/tests/uat/azure/cluster-config.yaml
```

This creates the `cluster-state-rg` resource group, the `clst<subscription-hex>` Storage Account (blob versioning enabled) with a `tfstate` container, and a local `backend.hcl`. The tool is idempotent — re-running against an already-bootstrapped subscription is a no-op.

## GitHub Actions Integration

See [`.github/workflows/uat-azure.yaml`](../../.github/workflows/uat-azure.yaml) for the full workflow. Key auth step:

```yaml
permissions:
  id-token: write  # Required for OIDC
jobs:
  integration-test-azure:
    env:
      AZURE_CLIENT_ID: "<terraform output AZURE_CLIENT_ID>"
      AZURE_TENANT_ID: "<terraform output AZURE_TENANT_ID>"
      AZURE_SUBSCRIPTION_ID: "<terraform output AZURE_SUBSCRIPTION_ID>"
    steps:
      - uses: azure/login@532459ea530d8321f2fb9bb10d1e0bcf23869a43  # v3.0.0
        with:
          client-id: ${{ env.AZURE_CLIENT_ID }}
          tenant-id: ${{ env.AZURE_TENANT_ID }}
          subscription-id: ${{ env.AZURE_SUBSCRIPTION_ID }}
```

The client, tenant, and subscription IDs are identifiers, not secrets — the same posture as the AWS account ID in `uat-aws.yaml`. Access is gated by the federated credential subjects (which repo/ref may exchange a token), not by knowledge of the IDs.

## Security

- **No long-lived credentials** — no client secret exists; OIDC tokens only, time-limited sessions
- **Repository + branch scoped** — only `repo:NVIDIA/aicr:ref:refs/heads/main` and `repo:NVIDIA/aicr:ref:refs/heads/test/uat` can federate; PR branches, tags, and forks are rejected (Entra subject matching is exact)
- **Token lifetime** — federated `az` sessions cannot self-refresh (there is no client secret to renew with), so the pipeline re-runs `azure/login` per phase rather than relying on one session for the whole run
- **Audit trail** — sign-ins and role usage appear in Entra sign-in logs and Azure Activity Log with GitHub OIDC context

## Outputs

| Output | Description |
|--------|-------------|
| `AZURE_CLIENT_ID` | Managed-identity client ID — `azure/login` `client-id` input |
| `AZURE_PRINCIPAL_ID` | Managed-identity principal (object) ID — `--assignee-object-id` for out-of-band role assignments |
| `AZURE_TENANT_ID` | Entra tenant ID — `azure/login` `tenant-id` input |
| `AZURE_SUBSCRIPTION_ID` | Target subscription — `azure/login` `subscription-id` input |

## State Management

This configuration uses **local state**. The `terraform.tfstate` file is gitignored (root `.gitignore` covers `*.tfstate` / `*.tfstate.*`), matching `infra/uat-aws-account/`. For multi-administrator setups, add an `azurerm` backend.

## Cleanup

```bash
terraform destroy -var "tenant_id=$(az account show --query tenantId -o tsv)"
```

Removes the identity resource group, managed identity, federated credentials, and any Terraform-managed role assignments (out-of-band assignments must be removed by an admin). The AKS actuator's own Terraform state storage account (`clst…`) lives in the subscription and is **not** managed here — remove it separately if decommissioning the UAT environment entirely.
