#
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

# List of outputs from each terraform apply 

output "AWS_ACCOUNT_ID" {
  value       = data.aws_caller_identity.current.account_id
  description = "AWS Account ID to use in GitHub Actions."
}

output "AWS_REGION" {
  value       = data.aws_region.current.name
  description = "AWS Region to use in GitHub Actions."
}

output "GITHUB_ACTIONS_ROLE_ARN" {
  value       = aws_iam_role.github_actions.arn
  description = "Role ARN to use in GitHub Actions for OIDC authentication."
}

output "OIDC_PROVIDER_ARN" {
  value       = data.aws_iam_openid_connect_provider.github.arn
  description = "OIDC Provider ARN for GitHub Actions."
}

output "GITHUB_ACTIONS_ROLE_NAME" {
  value       = aws_iam_role.github_actions.name
  description = "GitHub Actions IAM role name."
}