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

package main

// Status is the coverage verdict for a single matrix row. It is deliberately a
// small, closed vocabulary so the rendered table is unambiguous and so a reader
// can sort/filter on it.
type Status string

const (
	// StatusCovered means at least one in-repo, executable signal exercises the
	// item (a chainsaw/KWOK test, or a UAT CUJ tree wired into a workflow).
	StatusCovered Status = "covered"
	// StatusNotYetCovered means the item is a real, shipping capability that no
	// executable signal touches today. Honest gap, not a silent omission.
	StatusNotYetCovered Status = "not-yet-covered"
	// StatusStubbed means UAT signal trees exist but nothing scheduled executes
	// the journey — either no reservation is enrolled for its intent, or the
	// journey's workload step is disabled in the per-cloud pipelines. Distinct
	// from not-yet-covered: the assets are present but inert, and the row's note
	// names which of the two causes applies.
	StatusStubbed Status = "stubbed"
)

// Harness identifies an execution mechanism that can exercise an item. The set
// is ordered (see allHarnesses) so rendered cells are deterministic.
type Harness string

const (
	HarnessChainsaw   Harness = "chainsaw"    // tests/chainsaw/** — runs per-PR in CI
	HarnessKWOK       Harness = "kwok"        // simulated cluster, hardware-independent
	HarnessUAT        Harness = "uat"         // tests/uat/** — real-hardware nightly
	HarnessGPUNightly Harness = "gpu-nightly" // real-GPU nightly matrix (DC epic)
	HarnessDemo       Harness = "demo"        // demos/** — documented, not executed in CI
)

// allHarnesses is the canonical render order for the "Exercised by" cell.
var allHarnesses = []Harness{
	HarnessChainsaw, HarnessKWOK, HarnessUAT, HarnessGPUNightly, HarnessDemo,
}

// Kind separates the two row families the matrix reports.
type Kind string

const (
	KindCUJ Kind = "cuj" // a critical user journey (training, inference)
	KindCLI Kind = "cli" // a CLI verb / verb path (e.g. "evidence verify")
)

// Row is one line of the coverage matrix. Rows are sorted by (Kind, Item) before
// rendering so the output is byte-stable across runs.
type Row struct {
	Kind Kind
	// Item is the CUJ id (e.g. "cuj1-training-kubeflow") or the CLI verb path
	// (e.g. "bundle", "evidence verify").
	Item string
	// Harnesses is the set of mechanisms that exercise Item, rendered in
	// allHarnesses order. Empty when nothing does.
	Harnesses map[Harness]bool
	// Hardware is the coarsest hardware class the item runs on
	// ("none", "kwok-sim", "h100", ...). Free-form but stable.
	Hardware string
	// Cadence is when it runs ("per-PR", "nightly", "weekly", "—").
	Cadence string
	// Status is the coverage verdict.
	Status Status
	// Note carries a short qualifier (e.g. an Azure-stubbed pointer to DC6).
	Note string
}

// VersionAxis is the AICR-version dimension the nightly UAT batch exercises per
// enrolled reservation: tip-of-main plus PreviousReleases stable releases below
// it. The structural matrix records the axis only; the per-version live posture
// is a link into TestGrid, never a cell here.
type VersionAxis struct {
	// PreviousReleases is how many stable releases below main each enrolled
	// reservation runs nightly — uat-nightly-batch.yaml's
	// `inputs.previous_n || 'N'` schedule fallback, which the cron path uses
	// because a schedule event has an empty inputs context (workflow_dispatch
	// defaults are not applied). Zero means the batch runs main only.
	PreviousReleases int
}

// Matrix is the full, sorted set of rows plus metadata for the rendered page.
type Matrix struct {
	Rows        []Row
	VersionAxis VersionAxis
}
