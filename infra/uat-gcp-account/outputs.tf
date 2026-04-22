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

output "PROJECT_ID" {
  value       = data.google_project.project.project_id
  description = "GCP Project ID to use in GitHub Actions."
}

output "REGION" {
  value       = var.region
  description = "GCP Region to use in GitHub Actions."
}

output "SERVICE_ACCOUNT" {
  value       = data.google_service_account.github_actions.email
  description = "Service account email for GitHub Actions federated auth."
}
