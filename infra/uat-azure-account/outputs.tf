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

output "AZURE_CLIENT_ID" {
  description = "Managed-identity client id — azure/login's client-id input."
  value       = azurerm_user_assigned_identity.github_actions.client_id
}

output "AZURE_PRINCIPAL_ID" {
  description = "Managed-identity principal (object) id — the --assignee for out-of-band role assignments when manage_role_assignments=false."
  value       = azurerm_user_assigned_identity.github_actions.principal_id
}

output "AZURE_TENANT_ID" {
  description = "Entra tenant id — azure/login's tenant-id input."
  value       = var.tenant_id
}

output "AZURE_SUBSCRIPTION_ID" {
  description = "Target subscription — azure/login's subscription-id input."
  value       = var.subscription_id
}
