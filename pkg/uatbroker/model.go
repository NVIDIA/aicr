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

package uatbroker

// Recognized cloud values for a reservation row.
const (
	CloudAWS   = "aws"
	CloudGCP   = "gcp"
	CloudAzure = "azure"
)

// validClouds is the set of accepted Reservation.Cloud values.
var validClouds = map[string]bool{CloudAWS: true, CloudGCP: true, CloudAzure: true}

// Recognized recipe-intent values. The daytime human-access rotation (#1281,
// DC8) picks one flavor per reservation via Reservation.DaytimeIntent; these
// mirror the intents the per-cloud UAT pipelines accept.
const (
	IntentTraining  = "training"
	IntentInference = "inference"
)

// validIntents is the set of accepted intent values (Reservation.DaytimeIntent
// and, downstream, the pipeline's intent input).
var validIntents = map[string]bool{IntentTraining: true, IntentInference: true}

// Reservation is one row of the UAT reservation registry
// (infra/uat/reservations.yaml). Each row maps a reservation Name — the key
// the day/night broker leases via the GitHub Actions concurrency group
// "uat-<Name>" — to the cloud-specific identifiers and the on-disk
// cluster/test configuration a UAT run consumes.
type Reservation struct {
	Name  string `yaml:"name"`
	Cloud string `yaml:"cloud"`
	// ReservationID is the cloud capacity-reservation identifier (GCP uses the
	// fully-qualified resource path). OPTIONAL: quota-backed reservations
	// (e.g. Azure subscription quota) have no reservation identifier and omit
	// it — the Name is still the lease key either way.
	ReservationID     string `yaml:"reservation-id"`
	Accelerator       string `yaml:"accelerator"`
	GPUCount          int    `yaml:"gpu-count"`
	ClusterConfigPath string `yaml:"cluster-config-path"`
	TestConfigDir     string `yaml:"test-config-dir"`
	// NightlyIntents lists the recipe intents the nightly version-matrix batch
	// (#1274, DC1) runs on this reservation, each a full CUJ per version cell:
	// "training", "inference", or both. Absent defaults to ["training"]; an
	// explicit empty list opts out of the nightly batch entirely — see
	// NightlyIntentsOrDefault. DC3 (#1276) sets it to
	// [training, inference] on every reservation so both CUJs run nightly on
	// both clouds; the batch dispatches them SEQUENTIALLY through the shared
	// per-reservation lease (intent inner-loop, version outer-loop), so there is
	// never contention and `main` lands both intents before any release cell.
	// Entries must be recognized intents and unique (a duplicate would
	// double-run the same cell).
	//
	// AUTHORING CAVEAT: to opt out, the value must be an explicit empty
	// list (`nightly-intents: []`). A bare `nightly-intents:` (YAML null)
	// decodes to nil — indistinguishable from an absent key — and therefore
	// opts the reservation INTO the [training] default, provisioning real
	// GPU capacity. KnownFields cannot catch this (the key is valid);
	// TestParseRegistryBareNullNightlyIntents locks the behavior.
	NightlyIntents []string `yaml:"nightly-intents"`
	// NightlyIntentMinVersions gates RELEASE cells of the nightly version matrix
	// by a per-intent minimum AICR release: intent -> minimum version (a semver
	// tag like "v0.18.0"). It expresses "the first released version known to
	// support this intent on this reservation", so a released version that
	// predates a fix or platform is not run for that intent and does not
	// contribute a predictably-red cell.
	//
	// Only RELEASE cells are gated. The tip-of-main cell (Cell.IsMain) always
	// runs every listed intent — it is built from source and carries the newest
	// fixes — so a min-version pointing at a not-yet-tagged release (the fix is
	// on main but unreleased) correctly runs the intent on main-only until that
	// release ships, then enrolls it automatically. A release tag >= the min
	// runs; a tag below it is dropped for that intent only.
	//
	// OPTIONAL: absent or empty means no gate (every listed intent runs on every
	// cell — the pre-#1789 behavior). Validate requires each key to be an intent
	// this reservation actually lists in NightlyIntents (a min-version for an
	// unrun intent is dead config / a typo) and each value to parse as semver.
	NightlyIntentMinVersions map[string]string `yaml:"nightly-intent-min-versions"`
	// DaytimeIntent opts this reservation into the daytime human-access
	// rotation (#1281, DC8) and picks the flavor stood up on it during the
	// working day: "training" or "inference". Empty means the reservation is
	// NOT part of the daytime rotation (nightly batch only). This is the
	// configurable cloud→flavor default — data, not code — so the split
	// (AWS=training, GCP=inference at launch) can change without a workflow edit.
	DaytimeIntent string `yaml:"daytime-intent"`
}

// NightlyIntentsOrDefault returns the reservation's nightly-batch intents.
// An ABSENT nightly-intents field (nil) defaults to [IntentTraining] — the
// pre-DC3 behavior, so an un-annotated reservation keeps running the training
// CUJ nightly. An EXPLICIT empty list ([]) is a nightly opt-out and returns
// empty: the reservation stays manually dispatchable through uat-run.yaml but
// the nightly batch skips it (used for bring-up of a new cloud before its
// pipeline has earned nightly enrollment). Validate guarantees any listed
// value is a recognized, non-duplicate intent. The returned slice is a fresh
// copy the caller may mutate freely.
func (r *Reservation) NightlyIntentsOrDefault() []string {
	if r.NightlyIntents == nil {
		return []string{IntentTraining}
	}
	out := make([]string, len(r.NightlyIntents))
	copy(out, r.NightlyIntents)
	return out
}

// DaytimeAssignment is one reservation's slot in the daytime human-access
// rotation: the reservation to lease and the intent (flavor) to stand up on
// it. The daytime scheduler (uat-daytime.yaml) consumes a JSON array of these
// as its dispatch matrix.
type DaytimeAssignment struct {
	Reservation string `json:"reservation"`
	Intent      string `json:"intent"`
}

// Registry is the parsed reservations.yaml document.
type Registry struct {
	Reservations []Reservation `yaml:"reservations"`
}

// Cell is one unit of work in the nightly version matrix: a single UAT run
// of one AICRVersion against one Reservation. IsMain marks the tip-of-main
// cell, whose AICRVersion is empty (DC5 installs from source until it wires
// version-parameterized install; a release cell carries its tag).
type Cell struct {
	Reservation string `json:"reservation"`
	AICRVersion string `json:"aicr_version"`
	IsMain      bool   `json:"is_main"`
	// Intents are the nightly intents eligible at this cell's version, in
	// registry order (see EligibleNightlyIntents). The main cell carries every
	// listed intent; a release cell drops any intent gated off by
	// nightly-intent-min-versions. The controller dispatches one run per entry,
	// so an empty list means the cell dispatches nothing.
	Intents []string `json:"intents"`
}
