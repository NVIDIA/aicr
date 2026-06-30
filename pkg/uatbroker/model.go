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
	CloudAWS = "aws"
	CloudGCP = "gcp"
)

// validClouds is the set of accepted Reservation.Cloud values.
var validClouds = map[string]bool{CloudAWS: true, CloudGCP: true}

// Reservation is one row of the UAT reservation registry
// (infra/uat/reservations.yaml). Each row maps a reservation Name — the key
// the day/night broker leases via the GitHub Actions concurrency group
// "uat-<Name>" — to the cloud-specific identifiers and the on-disk
// cluster/test configuration a UAT run consumes.
type Reservation struct {
	Name              string `yaml:"name"`
	Cloud             string `yaml:"cloud"`
	ReservationID     string `yaml:"reservation-id"`
	Accelerator       string `yaml:"accelerator"`
	GPUCount          int    `yaml:"gpu-count"`
	ClusterConfigPath string `yaml:"cluster-config-path"`
	TestConfigDir     string `yaml:"test-config-dir"`
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
}
