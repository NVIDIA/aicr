# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

# The github-actions service account and Workload Identity Federation pool/provider
# are created by infra/demo-api-server. This config references them as data sources
# and adds only the IAM roles required for GKE cluster lifecycle management.
#
# Existing roles from demo-api-server (not managed here):
#   roles/artifactregistry.writer, roles/iam.serviceAccountUser,
#   roles/logging.logWriter, roles/monitoring.metricWriter,
#   roles/run.invoker, roles/run.admin, roles/secretmanager.secretAccessor,
#   roles/storage.objectAdmin, roles/storage.objectViewer

# Reference the existing service account (created by infra/demo-api-server)
data "google_service_account" "github_actions" {
  account_id = "github-actions"
  project    = var.project_id
}

# Additional project-level roles for GKE cluster management.
# Uses google_project_iam_member (additive) to avoid conflicts with
# demo-api-server's google_project_iam_member bindings.
locals {
  gke_roles = toset([
    "roles/container.admin",                 # GKE cluster CRUD
    "roles/compute.admin",                   # VPC, subnets, firewall, instances
    "roles/cloudkms.admin",                  # KMS for secrets encryption
    "roles/iam.serviceAccountAdmin",         # Create node pool service accounts
    "roles/resourcemanager.projectIamAdmin", # Bind roles to node pool SAs
  ])
}

resource "google_project_iam_member" "github_actions_gke_roles" {
  for_each = local.gke_roles
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${data.google_service_account.github_actions.email}"
}
