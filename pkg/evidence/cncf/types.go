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

package cncf

import (
	"time"
)

// evidenceEntry holds all data needed to render a single evidence document.
// Multiple checks can contribute to one entry when they share a requirement.
type evidenceEntry struct {
	// RequirementID is the CNCF requirement identifier.
	RequirementID string

	// Title is the human-readable evidence document title.
	Title string

	// Description is a one-paragraph description.
	Description string

	// Filename is the output filename (e.g., "dra-support.md").
	Filename string

	// Checks contains the individual check results.
	Checks []checkEntry

	// Status is the requirement's aggregate verdict (AggregateReport):
	// passed only when a complete proof path passed as a demonstration,
	// observed when a path passed but a check marked its result
	// observation-only (which does not establish a MUST), failed when a
	// check failed, not tested when a check was skipped, crashed or never
	// ran, not applicable when configuration declares it so. See the
	// RequirementStatus constants.
	Status RequirementStatus

	// Reason explains a Status other than passed.
	Reason string

	// Proof names the checks whose passed results established Status passed.
	Proof []string

	// Level is the requirement's upstream level at Version; empty when
	// Supporting.
	Level Level

	// Supporting is true when Version does not list the requirement, so the
	// page is supporting evidence rather than a requirement row.
	Supporting bool

	// Version is the checklist version the entry was rendered against.
	Version Version

	// GeneratedAt is the evidence generation timestamp.
	GeneratedAt time.Time
}

// indexRow is one requirement of the checklist in the index table, in
// upstream order: the aggregate verdict plus, when a page was rendered, its
// title and filename. A requirement no non-skipped check evidenced has no
// page and no evidence link.
type indexRow struct {
	// RequirementID is the upstream requirement identifier.
	RequirementID string

	// Level is the requirement's upstream level at the rendered version.
	Level Level

	// Title and Filename are the evidence page, empty when none was rendered.
	Title    string
	Filename string

	// Status and Reason are the aggregate verdict; see evidenceEntry.
	Status RequirementStatus
	Reason string
}

// checkEntry represents one check result within an evidence entry.
type checkEntry struct {
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
