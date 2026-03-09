// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
)

// EvidenceEntry holds all data needed to render a single evidence document.
// Multiple checks can contribute to one entry when they share a requirement.
type EvidenceEntry struct {
	// RequirementID is the CNCF requirement identifier.
	RequirementID string

	// Title is the human-readable evidence document title.
	Title string

	// Description is a one-paragraph description.
	Description string

	// Filename is the output filename (e.g., "dra-support.md").
	Filename string

	// Checks contains the individual check results.
	Checks []CheckEntry

	// Status is the aggregate: "passed" if all pass, "failed" if any fails.
	Status string

	// GeneratedAt is the evidence generation timestamp.
	GeneratedAt time.Time
}

// CheckEntry represents one check result within an evidence entry.
type CheckEntry struct {
	// Name is the validator name.
	Name string

	// Status is the check outcome ("passed", "failed", "skipped", "other").
	Status string

	// Message is the error message (from termination log on failure).
	Message string

	// Stdout contains the evidence output lines.
	Stdout []string

	// Duration is the execution time in milliseconds.
	Duration int
}
