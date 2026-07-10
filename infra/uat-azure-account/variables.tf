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

variable "tenant_id" {
  description = "Entra ID tenant that hosts the managed identity. Discover with `az account show --query tenantId -o tsv`."
  type        = string
  default     = "43083d15-7273-40c1-b7db-39efd9ccc17a"
}

variable "subscription_id" {
  description = "Subscription the UAT clusters are provisioned into — the `deployment.tenancy` value in tests/uat/azure/cluster-config.yaml."
  type        = string
  default     = "e88faa01-b4fd-49d3-b934-0ad9f9fca307"
}

variable "git_repo" {
  description = "GitHub repository (org/name) allowed to federate."
  type        = string
  default     = "NVIDIA/aicr"
}

variable "identity_name" {
  description = "Name of the user-assigned managed identity GitHub Actions federates as."
  type        = string
  default     = "github-actions-aicr"
}

variable "location" {
  description = "Region for the identity resource group (matches the UAT clusters' region)."
  type        = string
  default     = "westus"
}

variable "manage_role_assignments" {
  description = "Create the subscription role assignments for the pipeline identity. Requires Owner/User Access Administrator; deployers holding only the ABAC-scoped RBAC Administrator grant must set this to false and have a subscription admin create the assignments (see README)."
  type        = bool
  default     = true
}
