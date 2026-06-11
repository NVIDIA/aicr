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
// against the scheduled workflows' wired configs), not mere tree presence.
type cujSpec struct {
	id     string // canonical matrix id
	intent string // intent a wired UAT config must select for live coverage
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
			id: "cuj1-training-kubeflow", intent: "training",
			chainsawGlobs: []string{"cli/cuj1-training"},
			demoGlobs:     []string{"cuj1-*.md"},
			uatTreeGlobs:  []string{"tests/cuj1-training"},
		},
		{
			id: "cuj2-inference-dynamo", intent: "inference",
			chainsawGlobs: []string{"cli/cuj2-inference"},
			demoGlobs:     []string{"cuj2*.md"},
			uatTreeGlobs:  []string{"tests/cuj2-inference"},
		},
	}
}

// versionAxis lists the AICR versions actually exercised today. The scheduled
// UAT workflows build only the current checkout (`go build -o ./aicr
// ./cmd/aicr`), so the live axis is just `main`. The multi-version matrix
// (main + previous N stable releases) is owned by the dynamic-clusters epic
// (DC5) and is rendered here only once those workflows exist.
func versionAxis() []string {
	return []string{"main"}
}

// BuildMatrix assembles the full coverage matrix from the live CLI registry and
// the in-repo signal trees rooted at repoRoot.
func BuildMatrix(repoRoot string) Matrix {
	m := Matrix{VersionAxis: versionAxis()}
	wired := scanWiredUAT(repoRoot)

	for _, cuj := range canonicalCUJs() {
		harnesses, unwiredUAT := scanCUJ(repoRoot, cuj, wired)
		m.Rows = append(m.Rows, newRow(KindCUJ, cuj.id, harnesses, unwiredUAT))
	}

	for verb, harnesses := range scanVerbs(repoRoot, cliVerbs()) {
		m.Rows = append(m.Rows, newRow(KindCLI, verb, harnesses, false))
	}

	sort.Slice(m.Rows, func(i, j int) bool {
		if m.Rows[i].Kind != m.Rows[j].Kind {
			return m.Rows[i].Kind < m.Rows[j].Kind
		}
		return m.Rows[i].Item < m.Rows[j].Item
	})
	return m
}

// scanCUJ resolves the harness set for a CUJ and whether present-but-unwired UAT
// assets exist for it (which the caller renders as stubbed, not covered).
func scanCUJ(repoRoot string, cuj cujSpec, wired wiredUAT) (harnesses map[Harness]bool, unwiredUAT bool) {
	harnesses = map[Harness]bool{}
	if anyGlobExists(filepath.Join(repoRoot, "tests", "chainsaw"), cuj.chainsawGlobs) {
		harnesses[HarnessChainsaw] = true
	}
	if wired.intents[cuj.intent] {
		harnesses[HarnessUAT] = true
	}
	if anyGlobExists(filepath.Join(repoRoot, "demos"), cuj.demoGlobs) {
		harnesses[HarnessDemo] = true
	}
	// UAT assets present on disk but the journey's intent is not wired into any
	// scheduled workflow → stub, not live coverage.
	if !wired.intents[cuj.intent] && uatAssetsExist(repoRoot, cuj.uatTreeGlobs) {
		unwiredUAT = true
	}
	return harnesses, unwiredUAT
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
// stubbed when only present-but-unwired UAT assets exist; otherwise not-yet-covered.
func newRow(kind Kind, item string, harnesses map[Harness]bool, unwiredUAT bool) Row {
	r := Row{Kind: kind, Item: item, Harnesses: harnesses}

	executable := harnesses[HarnessChainsaw] || harnesses[HarnessKWOK] ||
		harnesses[HarnessUAT] || harnesses[HarnessGPUNightly]

	switch {
	case executable:
		r.Status = StatusCovered
	case unwiredUAT:
		r.Status = StatusStubbed
		r.Note = "UAT assets present but no scheduled workflow runs them — inference UAT tracked by DC3 (#1276), Azure by DC6 (#1280)"
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
		return "GPU (unwired)", "—"
	case h[HarnessUAT] || h[HarnessGPUNightly]:
		return "GPU (H100, real)", "nightly"
	case h[HarnessChainsaw] || h[HarnessKWOK]:
		return "simulated / none", "per-PR"
	case h[HarnessDemo]:
		return "docs", "—"
	default:
		return "—", "—"
	}
}
