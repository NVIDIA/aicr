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

// cujSpec describes a critical user journey and the in-repo tree fragments that
// signal its presence under each signal root.
type cujSpec struct {
	id string // canonical matrix id
	// dirGlobs are paths (relative to a signal root) whose existence signals the
	// CUJ is present in that root.
	dirGlobs []string
}

// canonicalCUJs is the closed set of CUJs the matrix reports. Inference (CUJ2)
// live execution lands with the dynamic-clusters epic (DC3, #1276); the tree is
// reported structurally here regardless.
func canonicalCUJs() []cujSpec {
	return []cujSpec{
		{id: "cuj1-training-kubeflow", dirGlobs: []string{
			"cli/cuj1-training", "tests/cuj1-training", "cuj1-*.md",
		}},
		{id: "cuj2-inference-dynamo", dirGlobs: []string{
			"cli/cuj2-inference", "tests/cuj2-inference", "cuj2*.md",
		}},
	}
}

// versionAxis is the AICR-version axis the live UAT matrix exercises (main + the
// previous N stable releases, per the dynamic-clusters epic DC5). The structural
// matrix records the axis; per-version live posture is a TestGrid link.
func versionAxis() []string {
	return []string{"main", "previous N stable releases"}
}

// BuildMatrix assembles the full coverage matrix from the live CLI registry and
// the in-repo signal trees rooted at repoRoot.
func BuildMatrix(repoRoot string) Matrix {
	m := Matrix{VersionAxis: versionAxis()}

	// CUJ rows.
	for _, cuj := range canonicalCUJs() {
		sig := scanCUJ(repoRoot, cuj)
		m.Rows = append(m.Rows, newRow(KindCUJ, cuj.id, sig))
	}

	// CLI verb rows, derived from the live registry.
	verbSig := scanVerbs(repoRoot, cliVerbs())
	for verb, sig := range verbSig {
		m.Rows = append(m.Rows, newRow(KindCLI, verb, sig))
	}

	sort.Slice(m.Rows, func(i, j int) bool {
		if m.Rows[i].Kind != m.Rows[j].Kind {
			return m.Rows[i].Kind < m.Rows[j].Kind
		}
		return m.Rows[i].Item < m.Rows[j].Item
	})
	return m
}

// scanCUJ detects which signal roots contain the CUJ's tree.
func scanCUJ(repoRoot string, cuj cujSpec) *verbSignals {
	sig := &verbSignals{harnesses: map[Harness]bool{}, stubbedOnly: true}
	for _, root := range defaultSignalRoots() {
		base := filepath.Join(repoRoot, root.rel)
		if !anyGlobExists(base, cuj.dirGlobs) {
			continue
		}
		sig.harnesses[root.harness] = true
		if !root.stubbed {
			sig.stubbedOnly = false
		}
	}
	if len(sig.harnesses) == 0 {
		sig.stubbedOnly = false
	}
	return sig
}

// anyGlobExists reports whether any of the rel globs resolves to an existing
// path under base.
func anyGlobExists(base string, globs []string) bool {
	for _, g := range globs {
		matches, err := filepath.Glob(filepath.Join(base, g))
		if err == nil && len(matches) > 0 {
			return true
		}
		// Also try a direct (non-glob) stat for plain paths.
		if _, err := os.Stat(filepath.Join(base, g)); err == nil {
			return true
		}
	}
	return false
}

// newRow derives the rendered row (hardware, cadence, status, note) from the
// harness set. A row is covered when an executable harness (chainsaw or a
// non-stubbed UAT) exercises it; stubbed when seen only under a stubbed root;
// otherwise not-yet-covered.
func newRow(kind Kind, item string, sig *verbSignals) Row {
	r := Row{Kind: kind, Item: item, Harnesses: sig.harnesses}

	executable := sig.harnesses[HarnessChainsaw] || sig.harnesses[HarnessKWOK] ||
		(sig.harnesses[HarnessUAT] && !sig.stubbedOnly) ||
		(sig.harnesses[HarnessGPUNightly] && !sig.stubbedOnly)

	switch {
	case executable:
		r.Status = StatusCovered
	case sig.stubbedOnly && len(sig.harnesses) > 0:
		r.Status = StatusStubbed
		r.Note = "Azure UAT trees present but unwired — revive-or-retire tracked by DC6 (#1280)"
	default:
		r.Status = StatusNotYetCovered
		if sig.harnesses[HarnessDemo] {
			r.Note = "documented in demos only; no executable test yet"
		}
	}

	r.Hardware, r.Cadence = hardwareCadence(sig.harnesses, r.Status)
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
