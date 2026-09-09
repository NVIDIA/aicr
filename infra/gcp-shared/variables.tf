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

# List of variables which can be provided ar runtime to override the specified defaults 
variable "project_id" {
  description = "GCP Project ID"
  type        = string
  default     = "eidosx"
}

# NVidia GitHub Repo
variable "git_repo" {
  description = "GitHub Repo"
  type        = string
  default     = "NVIDIA/aicr"
}

# Region for Artifact Registry and Cloud Run
variable "region" {
  description = "GCP region for Artifact Registry and Cloud Run"
  type        = string
  default     = "us-west1"
}

# Demo Artifact Registry Location
variable "location" {
  description = "GCP Artifact Registry location"
  type        = string
  default     = "us"
}
