#
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

# List of variables which can be provided at runtime to override the specified defaults 

variable "aws_region" {
  description = "AWS Region"
  type        = string
  default     = "us-east-1"
}

variable "git_repo" {
  description = "GitHub Repository in format owner/repo"
  type        = string
  default     = "NVIDIA/aicr"
}

variable "github_actions_role_name" {
  description = "Name for the GitHub Actions IAM role"
  type        = string
  default     = "github-actions-role-aicr"
}

variable "oidc_provider_url" {
  description = "GitHub OIDC provider URL"
  type        = string
  default     = "https://token.actions.githubusercontent.com"
}

variable "oidc_audience" {
  description = "GitHub OIDC audience"
  type        = string
  default     = "sts.amazonaws.com"
}