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

// specKeyRE matches the document's top-level `spec:` mapping key.
var specKeyRE = regexp.MustCompile(`^(\s*)spec\s*:\s*$`)

// numNodesChildRE matches `numNodes:` at a given exact indentation.
func numNodesChildRE(indent string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(indent) + `numNodes\s*:`)
}

// specHasNumNodes reports whether a single YAML document declares numNodes as a
// DIRECT CHILD of spec.
//
// Path-aware on purpose. An earlier version matched `numNodes:` anywhere in the
// document, which accepted `metadata.numNodes` — a key Kubernetes ignores, while
// the required `spec.numNodes` stays absent and admission still fails. Matching
// the key without its parent is not a weaker check, it is the wrong check.
//
// Comment lines are stripped first: these manifests legitimately discuss
// "spec.numNodes: Required value" in prose, and a scan that does not strip them
// matches that instead of the real key, passing even when the key is deleted.
//
// A full YAML parse is unavailable — the manifests are Helm templates containing
// {{ }} expressions that no YAML parser accepts — so this walks indentation.
func specHasNumNodes(doc string) bool {
	var lines []string
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") || strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	for i, line := range lines {
		m := specKeyRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		specIndent := m[1]
		var childRE *regexp.Regexp
		for _, sub := range lines[i+1:] {
			subIndent := sub[:len(sub)-len(strings.TrimLeft(sub, " \t"))]
			// Dedent to spec's level or shallower ends the spec mapping.
			if len(subIndent) <= len(specIndent) {
				break
			}
			if childRE == nil {
				childRE = numNodesChildRE(subIndent)
			}
			if childRE.MatchString(sub) {
				return true
			}
		}
	}
	return false
}

// computeDomainDocsMissingNumNodes returns the 0-based indexes of YAML
// documents that declare kind: ComputeDomain without spec.numNodes.
//
// Scoped per document: a multi-document manifest where one ComputeDomain sets
// the key and a second omits it would satisfy a whole-file scan while still
// failing admission. No such manifest exists in the catalog today; the guard is
// document-scoped so adding one cannot silently bypass it.
func computeDomainDocsMissingNumNodes(content string) []int {
	var missing []int
	for i, doc := range strings.Split(content, "\n---") {
		if !strings.Contains(doc, "kind: ComputeDomain") {
			continue
		}
		if !specHasNumNodes(doc) {
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

// TestComputeDomainScannerCases pins the scanner's behavior directly, so the
// catalog guard above cannot quietly stop discriminating if the catalog changes.
// Each case is a shape that has either fooled a previous version of this
// scanner or must keep working.
func TestComputeDomainScannerCases(t *testing.T) {
	t.Parallel()

	const header = "apiVersion: resource.nvidia.com/v1beta1\nkind: ComputeDomain\n"

	tests := []struct {
		name        string
		doc         string
		wantMissing bool
	}{
		{
			name: "spec.numNodes present",
			doc:  header + "metadata:\n  name: cd\nspec:\n  numNodes: 0\n  channel:\n    allocationMode: All\n",
		},
		{
			name:        "spec.numNodes absent",
			doc:         header + "metadata:\n  name: cd\nspec:\n  channel:\n    allocationMode: All\n",
			wantMissing: true,
		},
		{
			// Regression: an earlier scanner matched numNodes anywhere in the
			// document, so this passed while admission would still fail.
			name:        "numNodes under metadata, not spec",
			doc:         header + "metadata:\n  name: cd\n  numNodes: 0\nspec:\n  channel:\n    allocationMode: All\n",
			wantMissing: true,
		},
		{
			// Regression: an earlier scanner did not strip comments, so the
			// prose in the real manifest satisfied it even with the key gone.
			name:        "numNodes only mentioned in a comment",
			doc:         header + "metadata:\n  name: cd\nspec:\n  # numNodes: Required value\n  channel:\n    allocationMode: All\n",
			wantMissing: true,
		},
		{
			name: "nested numNodes does not satisfy the direct-child rule",
			doc:  header + "metadata:\n  name: cd\nspec:\n  channel:\n    numNodes: 0\n",
			// numNodes exists but under spec.channel, not spec.
			wantMissing: true,
		},
		{
			name: "templated value is acceptable",
			doc:  header + "metadata:\n  name: cd\nspec:\n  numNodes: {{ .Values.numNodes }}\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := !specHasNumNodes(tt.doc)
			if got != tt.wantMissing {
				t.Errorf("specHasNumNodes reported missing=%v, want %v\ndoc:\n%s",
					got, tt.wantMissing, tt.doc)
			}
		})
	}
}

// TestComputeDomainMultiDocument covers the per-document scoping: a file where
// one ComputeDomain is valid and a second is not must report only the second.
func TestComputeDomainMultiDocument(t *testing.T) {
	t.Parallel()

	content := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unrelated\n" +
		"\n---\napiVersion: resource.nvidia.com/v1beta1\nkind: ComputeDomain\nmetadata:\n  name: ok\nspec:\n  numNodes: 0\n" +
		"\n---\napiVersion: resource.nvidia.com/v1beta1\nkind: ComputeDomain\nmetadata:\n  name: bad\nspec:\n  channel:\n    allocationMode: All\n"

	missing := computeDomainDocsMissingNumNodes(content)
	if len(missing) != 1 || missing[0] != 2 {
		t.Errorf("missing documents = %v, want [2] (only the third document lacks spec.numNodes)", missing)
	}
}
