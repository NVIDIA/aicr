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

import (
	"os"
	"path/filepath"
	"sort"
)

// cujSpec describes a critical user journey and the in-repo signals that prove
// it is exercised. UAT execution is keyed off the journey's intent (matched
// against the intents the nightly-enrolled reservations run), not mere tree
// presence.
type cujSpec struct {
	id     string // canonical matrix id
	intent string // intent a nightly-enrolled reservation must run
	// uatPhase is the UAT runner phase that executes this journey's workload.
	// Enrolling the intent only proves the stack is deployed and validated; the
	// journey itself is live only when this phase is an enabled pipeline step.
	uatPhase string
	// chainsawGlobs are paths (relative to tests/chainsaw) whose existence signals
	// per-PR chainsaw coverage.
	chainsawGlobs []string
	// demoGlobs are paths (relative to demos) signaling documentation-only presence.
	demoGlobs []string
	// uatTreeGlobs are paths (relative to a tests/uat/<cloud> dir) whose existence
	// signals UAT *assets* exist — used only to flag present-but-unwired stubs.
	uatTreeGlobs []string
}

func canonicalCUJs() []cujSpec {
	return []cujSpec{
		{
			id: "cuj1-training-kubeflow", intent: "training", uatPhase: "train",
			chainsawGlobs: []string{"cli/cuj1-training"},
			demoGlobs:     []string{"cuj1-*.md"},
			uatTreeGlobs:  []string{"tests/cuj1-training"},
		},
		{
			id: "cuj2-inference-dynamo", intent: "inference", uatPhase: "serve",
			chainsawGlobs: []string{"cli/cuj2-inference"},
			demoGlobs:     []string{"cuj2*.md"},
			uatTreeGlobs:  []string{"tests/cuj2-inference"},
		},
	}
}

// BuildMatrix assembles the full coverage matrix from the live CLI registry and
// the in-repo signal trees rooted at repoRoot. It fails closed on an unresolvable
// UAT wiring or version axis rather than reporting the affected rows as
// uncovered — a generated doc must not turn a broken input into a quiet
// downgrade of real nightly coverage.
func BuildMatrix(repoRoot string) (Matrix, error) {
	wired, err := scanWiredUAT(repoRoot)
	if err != nil {
		return Matrix{}, err
	}
	axis, err := scanVersionAxis(repoRoot)
	if err != nil {
		return Matrix{}, err
	}

	verbs, err := scanVerbs(repoRoot, cliVerbs(), wired)
	if err != nil {
		return Matrix{}, err
	}

	m := Matrix{VersionAxis: axis}
	for _, cuj := range canonicalCUJs() {
		harnesses, stubNote := scanCUJ(repoRoot, cuj, wired)
		m.Rows = append(m.Rows, newRow(KindCUJ, cuj.id, harnesses, stubNote))
	}

	for verb, harnesses := range verbs {
		m.Rows = append(m.Rows, newRow(KindCLI, verb, harnesses, ""))
	}

	sort.Slice(m.Rows, func(i, j int) bool {
		if m.Rows[i].Kind != m.Rows[j].Kind {
			return m.Rows[i].Kind < m.Rows[j].Kind
		}
		return m.Rows[i].Item < m.Rows[j].Item
	})
	return m, nil
}

// Stub notes explain *why* present UAT assets are not live coverage. The two
// causes are operationally different — one is fixed in the registry, the other
// in the per-cloud pipelines — so the row names the one that applies.
const (
	noteIntentNotEnrolled = "UAT assets present but no nightly-enrolled reservation runs this intent " +
		"(see infra/uat/reservations.yaml)"
	noteJourneyStepDisabled = "the intent runs nightly and its stack is deployed and validated, but the " +
		"journey's own workload step is disabled in the per-cloud UAT pipelines"
)

// scanCUJ resolves the harness set for a CUJ and, when present-but-unexecuted
// UAT assets exist for it, the note explaining why (which the caller renders as
// stubbed, not covered).
func scanCUJ(repoRoot string, cuj cujSpec, wired wiredUAT) (harnesses map[Harness]bool, stubNote string) {
	harnesses = map[Harness]bool{}
	if anyGlobExists(filepath.Join(repoRoot, "tests", "chainsaw"), cuj.chainsawGlobs) {
		harnesses[HarnessChainsaw] = true
	}
	// Live nightly coverage needs both halves: a reservation enrolled for the
	// intent, AND its pipeline actually executing the journey's workload phase.
	live := wired.runsJourney(cuj.intent, cuj.uatPhase)
	if live {
		harnesses[HarnessUAT] = true
	}
	if anyGlobExists(filepath.Join(repoRoot, "demos"), cuj.demoGlobs) {
		harnesses[HarnessDemo] = true
	}
	if !live && uatAssetsExist(repoRoot, cuj.uatTreeGlobs) {
		stubNote = noteIntentNotEnrolled
		if wired.runsIntent(cuj.intent) {
			stubNote = noteJourneyStepDisabled
		}
	}
	return harnesses, stubNote
}

// uatAssetsExist reports whether any tests/uat/<cloud> dir contains one of globs.
func uatAssetsExist(repoRoot string, globs []string) bool {
	uatRoot := filepath.Join(repoRoot, "tests", "uat")
	clouds, err := os.ReadDir(uatRoot)
	if err != nil {
		return false
	}
	for _, c := range clouds {
		if !c.IsDir() {
			continue
		}
		if anyGlobExists(filepath.Join(uatRoot, c.Name()), globs) {
			return true
		}
	}
	return false
}

// anyGlobExists reports whether any of the rel globs resolves to an existing
// path under base.
func anyGlobExists(base string, globs []string) bool {
	for _, g := range globs {
		matches, err := filepath.Glob(filepath.Join(base, g))
		if err == nil && len(matches) > 0 {
			return true
		}
		if _, statErr := os.Stat(filepath.Join(base, g)); statErr == nil {
			return true
		}
	}
	return false
}

// newRow derives the rendered row from the harness set. A row is covered when an
// executable harness (chainsaw/KWOK, or a wired UAT/GPU-nightly) exercises it;
// stubbed when only present-but-unexecuted UAT assets exist (stubNote says why);
// otherwise not-yet-covered.
func newRow(kind Kind, item string, harnesses map[Harness]bool, stubNote string) Row {
	r := Row{Kind: kind, Item: item, Harnesses: harnesses}

	executable := harnesses[HarnessChainsaw] || harnesses[HarnessKWOK] ||
		harnesses[HarnessUAT] || harnesses[HarnessGPUNightly]

	switch {
	case executable:
		r.Status = StatusCovered
	case stubNote != "":
		r.Status = StatusStubbed
		r.Note = stubNote
	default:
		r.Status = StatusNotYetCovered
		if harnesses[HarnessDemo] {
			r.Note = "documented in demos only; no executable test yet"
		}
	}

	r.Hardware, r.Cadence = hardwareCadence(harnesses, r.Status)
	return r
}

// hardwareCadence picks the coarsest hardware class and cadence implied by the
// harness set, in order of strongest signal.
func hardwareCadence(h map[Harness]bool, status Status) (hardware, cadence string) {
	switch {
	case status == StatusStubbed:
		return "GPU (unwired)", emDash
	case h[HarnessUAT] || h[HarnessGPUNightly]:
		return "GPU (H100, real)", "nightly"
	case h[HarnessChainsaw] || h[HarnessKWOK]:
		return "simulated / none", "per-PR"
	case h[HarnessDemo]:
		return "docs", emDash
	default:
		return emDash, emDash
	}
}
