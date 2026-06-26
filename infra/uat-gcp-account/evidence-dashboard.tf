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

# Read-only service account for the GP5 evidence-dashboard publish workflow
# (.github/workflows/evidence-dashboard-publish.yaml).
#
# The publish job syncs the source-keyed evidence tree out of the UAT evidence
# bucket and renders it to GitHub Pages. It only ever READS the bucket, so it
# gets a dedicated identity whose ONLY grant is objectViewer on that one bucket
# -- deliberately not the shared `github-actions` SA (project-wide objectAdmin)
# and not the GP2 evidence-publish writer. A compromise of the Pages build can
# read published evidence and nothing else.
#
# This is the read-only slice GP5 needs. GP3 (infra/evidence-dashboard) still
# owns the hardened data bucket and the dedicated objectCreator writer SA.

resource "google_service_account" "evidence_read" {
  account_id   = "evidence-read"
  display_name = "Read-only evidence-tree reader for the GP5 Pages dashboard build"
}

# Bucket-scoped (not project-scoped) read. Additive member binding so it never
# fights any other IAM on the bucket, and confined to this single bucket.
resource "google_storage_bucket_iam_member" "evidence_read_viewer" {
  bucket = var.evidence_bucket
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.evidence_read.email}"
}

# Let the existing github-actions-pool federation impersonate the read SA from
# NVIDIA/aicr workflow runs. Scoped per-SA via the repository attribute (the
# shared provider, owned by demo-api-server, already pins the repo + main/tag
# refs); the resource grant above keeps this least-privilege regardless.
resource "google_service_account_iam_member" "evidence_read_impersonation" {
  service_account_id = google_service_account.evidence_read.id
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/projects/${data.google_project.project.number}/locations/global/workloadIdentityPools/github-actions-pool/attribute.repository/${var.git_repo}"
}
