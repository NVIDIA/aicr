// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package uatbroker resolves the UAT reservation registry
// (infra/uat/reservations.yaml) and expands the nightly version-matrix
// schedule for the day/night UAT broker (#1274).
//
// The registry maps a human-facing reservation NAME — the key the broker
// leases through the GitHub Actions concurrency group "uat-<name>" so
// contending runs queue rather than race — to the cloud, capacity
// reservation id, accelerator, and the on-disk cluster/test configuration a
// UAT run consumes. LoadRegistryFile + Lookup back the `uat-broker
// reservations` CLI; new infrastructure is onboarded by adding a row, with
// no code change.
//
// ExpandSchedule builds the ordered nightly run list: the tip-of-main cell
// first, then the previous N stable releases in descending semver order, per
// reservation. Cells are ordered newest-first so the nightly controller
// drops the OLDEST releases first when its time-box closes — it simply stops
// at the cursor. Release cells carry their tag in AICRVersion for DC5's
// version-parameterized install; until DC5 lands they install from source.
// Each cell also carries the nightly intents ELIGIBLE at its version: the
// main cell runs every listed intent, while a release cell drops any intent
// whose nightly-intent-min-versions floor is newer than the tag, so a release
// that predates an intent's support never contributes a permanently-red cell.
//
// The package performs no network or git I/O and holds no credentials: the
// CLI feeds it the registry bytes and the raw `git tag` list.
package uatbroker
