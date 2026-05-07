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

# Artifact Registry repository for demo API server images
resource "google_artifact_registry_repository" "demo" {
  repository_id = "demo"
  project       = var.project_id
  location      = var.location
  format        = "DOCKER"
  description   = "Docker repository for demo API server images"

  # Cleanup policy to remove old images
  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"
    most_recent_versions {
      keep_count = 10
    }
  }

  depends_on = [google_project_service.default]
}

# Grant the GitHub Actions service account permission to push images
resource "google_artifact_registry_repository_iam_member" "github_actions_writer" {
  repository = google_artifact_registry_repository.demo.name
  location   = google_artifact_registry_repository.demo.location
  role       = "roles/artifactregistry.writer"
  member     = "serviceAccount:${google_service_account.github_actions_user.email}"
}

# Grant the GitHub Actions service account permission to read images
resource "google_artifact_registry_repository_iam_member" "github_actions_reader" {
  repository = google_artifact_registry_repository.demo.name
  location   = google_artifact_registry_repository.demo.location
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.github_actions_user.email}"
}
