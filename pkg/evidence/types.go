// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package evidence

import (
	"time"

	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

// EvidenceEntry holds all data needed to render a single evidence document.
// Multiple checks can contribute to a single entry when they share an EvidenceFile.
type EvidenceEntry struct {
	// RequirementID is the CNCF requirement identifier (e.g., "dra_support").
	RequirementID string

	// Title is the human-readable title for the evidence document.
	Title string

	// Description is a one-paragraph description of what this requirement demonstrates.
	Description string

	// Filename is the output filename (e.g., "dra-support.md").
	Filename string

	// Checks contains the individual check results contributing to this entry.
	Checks []CheckEntry

	// Status is the aggregate status: pass if all checks pass, fail if any fails.
	Status validator.ValidationStatus

	// GeneratedAt is the timestamp when evidence was generated.
	GeneratedAt time.Time
}

// CheckEntry represents one check result within an evidence entry.
type CheckEntry struct {
	// Name is the check registry name (e.g., "accelerator-metrics").
	Name string

	// Status is the check outcome.
	Status validator.ValidationStatus

	// Reason is the check output/reason text.
	Reason string

	// Duration is how long the check took.
	Duration time.Duration

	// Artifacts contains diagnostic evidence captured during check execution.
	Artifacts []checks.Artifact
}
