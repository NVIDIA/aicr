#
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# GitHub Actions -> Entra workload identity federation for the Azure UAT
# pipeline (.github/workflows/uat-azure.yaml, invoked via uat-run.yaml).
#
# The federation subject is a USER-ASSIGNED MANAGED IDENTITY, not an app
# registration: this tenant blocks member app-registration creation
# (authorizationPolicy defaultUserRolePermissions.allowedToCreateApps=false
# and no directory roles are granted), so an azuread_application apply fails
# with Authorization_RequestDenied. A managed identity is an ARM resource —
# subscription Contributor suffices to create it — and supports the same
# federated credentials; azure/login consumes its client-id unchanged.
#
# The workflow authenticates keylessly: GitHub mints an OIDC token,
# azure/login exchanges it against the federated identity credentials below,
# and the resulting az CLI context (~/.azure) is what the AKS actuator
# container consumes. No client secret exists anywhere (a managed identity
# cannot even have one).

data "azurerm_subscription" "current" {}

# Holds only the pipeline identity — deliberately separate from the
# run-scoped cluster RGs the actuator creates and destroys.
resource "azurerm_resource_group" "identity" {
  name     = "aicr-uat-identity-rg"
  location = var.location
}

resource "azurerm_user_assigned_identity" "github_actions" {
  name                = var.identity_name
  resource_group_name = azurerm_resource_group.identity.name
  location            = azurerm_resource_group.identity.location
}

# One federated credential per allowed subject. Mirrors the AWS trust policy
# (infra/uat-aws-account/federation.tf): main + the test/uat staging branch.
# Entra federated credentials match subjects EXACTLY (no wildcards), so each
# allowed ref is its own resource.
locals {
  allowed_subjects = {
    main     = "repo:${var.git_repo}:ref:refs/heads/main"
    test-uat = "repo:${var.git_repo}:ref:refs/heads/test/uat"
  }
}

resource "azurerm_federated_identity_credential" "github" {
  for_each = local.allowed_subjects

  # resource_group_name is intentionally omitted (deprecated in azurerm 4.x —
  # the identity id already carries the RG; removed in v5) and the identity
  # ref uses user_assigned_identity_id (parent_id is deprecated for the same
  # removal).
  name                      = "github-${each.key}"
  user_assigned_identity_id = azurerm_user_assigned_identity.github_actions.id
  audience                  = ["api://AzureADTokenExchange"]
  issuer                    = "https://token.actions.githubusercontent.com"
  subject                   = each.value
}

# --- Role assignments -------------------------------------------------------
#
# Creating the assignments below requires subscription Owner /
# User Access Administrator (or an ABAC grant covering these role
# definitions). The UAT deployer's own grant is typically the ABAC-scoped
# RBAC Administrator limited to Network Contributor + AcrPull, which CANNOT
# create these — set manage_role_assignments=false in that case and have a
# subscription admin create them out-of-band (exact commands in the README's
# Permissions Granted section).

# Subscription-scoped Contributor: the AKS actuator creates resource groups,
# AKS clusters, VNets, and node pools — there is no pre-created resource
# group to scope down to. (The clst<subscription-hex> state storage account
# is bootstrapped once, out-of-band, by the actuator's setup tool — see the
# README's State Bootstrap section.) Revisit narrowing to a dedicated RG
# scope if the actuator gains a fixed-RG mode. Rationale and the scope
# trade-off are recorded in the README's Permissions Granted section.
resource "azurerm_role_assignment" "contributor" {
  count = var.manage_role_assignments ? 1 : 0

  scope                = data.azurerm_subscription.current.id
  role_definition_name = "Contributor"
  principal_id         = azurerm_user_assigned_identity.github_actions.principal_id
}

# The pipeline needs Microsoft.Authorization/roleAssignments/write — and
# /delete on destroy — beyond Contributor, for exactly three role
# definitions:
#   - Network Contributor (4d97b98b…) and AcrPull (7f951dda…): the two
#     assignments the actuator's iam.tf creates for the cluster identity
#     and kubelet.
#   - Azure Kubernetes Service RBAC Cluster Admin (b1ff04bb…): the pipeline
#     SELF-ASSIGNS this per run, scoped to the run's resource group
#     (uat-azure.yaml "Grant run-scoped cluster admin"), because the
#     actuator provisions AKS with Entra RBAC and local accounts disabled —
#     without a data-plane role the first kubectl call is Forbidden. A
#     subscription-scoped grant would be cluster-admin on EVERY Azure-RBAC
#     cluster in this SHARED subscription; the per-RG self-assignment keeps
#     the blast radius to the run's own cluster and dies with the RG at
#     teardown.
# Grant it least-privilege, as the actuator README prescribes: Role Based
# Access Control Administrator constrained by an ABAC condition to
# assigning/deleting ONLY those three role definitions, so the pipeline
# identity still cannot grant itself (or anyone else) broader access.
resource "azurerm_role_assignment" "role_assignments_scoped" {
  count = var.manage_role_assignments ? 1 : 0

  scope                = data.azurerm_subscription.current.id
  role_definition_name = "Role Based Access Control Administrator"
  principal_id         = azurerm_user_assigned_identity.github_actions.principal_id
  condition_version    = "2.0"
  condition            = "((!(ActionMatches{'Microsoft.Authorization/roleAssignments/write'})) OR (@Request[Microsoft.Authorization/roleAssignments:RoleDefinitionId] ForAnyOfAnyValues:GuidEquals {4d97b98b-1d4f-4787-a291-c67834d212e7, 7f951dda-4ed3-4680-a7ca-43fe172d538d, b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b})) AND ((!(ActionMatches{'Microsoft.Authorization/roleAssignments/delete'})) OR (@Resource[Microsoft.Authorization/roleAssignments:RoleDefinitionId] ForAnyOfAnyValues:GuidEquals {4d97b98b-1d4f-4787-a291-c67834d212e7, 7f951dda-4ed3-4680-a7ca-43fe172d538d, b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b}))"
}
