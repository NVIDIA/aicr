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

package recipe

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// numNodesKeyRE matches an indented mapping key. Anchored per line so a key
// inside a comment or a quoted error string cannot satisfy it.
var numNodesKeyRE = regexp.MustCompile(`(?m)^[ \t]+numNodes[ \t]*:`)

// computeDomainDocsMissingNumNodes returns the 0-based indexes of YAML
// documents that declare kind: ComputeDomain without a numNodes key.
//
// Scoped per document rather than per file. A multi-document manifest where one
// ComputeDomain sets numNodes and a second omits it would satisfy a whole-file
// scan while still failing admission, and so would an unrelated resource that
// happens to carry a numNodes key. No such manifest exists in the catalog
// today; the guard is document-scoped so that adding one cannot silently
// bypass it.
//
// Comment lines are stripped first: these manifests legitimately discuss
// "spec.numNodes: Required value" in prose, and a naive substring scan matches
// that instead of the real key, passing even when the key is deleted.
//
// A full YAML parse is unavailable — the manifests are Helm templates and
// contain {{ }} expressions that no YAML parser accepts.
func computeDomainDocsMissingNumNodes(content string) []int {
	var missing []int
	for i, doc := range strings.Split(content, "\n---") {
		if !strings.Contains(doc, "kind: ComputeDomain") {
			continue
		}
		var b strings.Builder
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
		if !numNodesKeyRE.MatchString(b.String()) {
			missing = append(missing, i)
		}
	}
	return missing
}

// TestComputeDomainManifestsSetNumNodes guards the fresh-install CRD-overlap
// hazard introduced by GPU Operator v26.7.0.
//
// Two charts in the catalog ship a CRD named computedomains.resource.nvidia.com:
// the standalone nvidia-dra-driver-gpu chart, and — new in gpu-operator
// v26.7.0 — the GPU Operator chart. The two copies are NOT identical. The
// operator's is a stale snapshot that lists numNodes as required and supplies
// no `default: 0`; the DRA driver's makes it optional with a default.
//
// Helm installs crds/ only when the CRD is absent and never upgrades it, and
// gpu-operator is ordered before nvidia-dra-driver-gpu. So on a FRESH cluster
// the operator's stricter copy is the one that lands, and any ComputeDomain CR
// omitting spec.numNodes is rejected by the API server with
// "spec.numNodes: Required value". Structural defaulting cannot rescue it
// because that copy carries no default. Neither chart installs a webhook that
// could supply the field.
//
// An UPGRADED cluster masks this: it already has the permissive copy installed
// by DRA 0.4.1, so the CR still admits. That asymmetry is why this is a unit
// guard rather than something an upgrade-path e2e would catch.
//
// The invariant: every ComputeDomain CR shipped in the catalog must set
// spec.numNodes explicitly, so it is valid under BOTH CRD copies regardless of
// which chart installed the CRD first.
//
// See PR #2439 and issue #1087 for the driver-root analog of this
// cross-component coupling problem.
func TestComputeDomainManifestsSetNumNodes(t *testing.T) {
	t.Parallel()

	efs := GetEmbeddedFS()

	var checked int
	err := fs.WalkDir(efs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		raw, readErr := efs.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		content := string(raw)
		if !strings.Contains(content, "kind: ComputeDomain") {
			return nil
		}
		checked++

		for _, idx := range computeDomainDocsMissingNumNodes(content) {
			t.Errorf("%s: YAML document %d declares kind: ComputeDomain but does not set spec.numNodes.\n"+
				"  GPU Operator v26.7.0 ships a ComputeDomain CRD copy that marks numNodes\n"+
				"  REQUIRED with no default, and it is installed before the DRA driver's\n"+
				"  permissive copy. On a fresh cluster this CR is rejected at admission with\n"+
				"  \"spec.numNodes: Required value\".\n"+
				"  Set numNodes explicitly (0 is correct under IMEXDaemonsWithDNSNames=true,\n"+
				"  the DRA driver default, where each IMEX daemon starts without waiting for\n"+
				"  a quorum). See PR #2439.", path, idx)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded recipes: %v", err)
	}

	// Fail closed on a vacuous pass: if the walk matched nothing, the guard is
	// silently inert and a regression would go unnoticed.
	if checked == 0 {
		t.Fatal("no ComputeDomain manifests found in the embedded recipes — " +
			"this guard is vacuous. Either the manifests moved, or the embed " +
			"pattern no longer covers them.")
	}
	t.Logf("verified %d ComputeDomain manifest(s) set spec.numNodes", checked)
}
