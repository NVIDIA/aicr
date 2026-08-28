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

// hasYAMLKey reports whether content declares key as a real mapping key,
// ignoring comment lines.
func hasYAMLKey(content, key string) bool {
	var b strings.Builder
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if key != "numNodes" {
		panic("hasYAMLKey: only numNodes is supported")
	}
	return numNodesKeyRE.MatchString(b.String())
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

		// The manifests are Helm templates, so a full YAML parse is not
		// available. Strip comment lines FIRST — the surrounding prose in
		// these files legitimately discusses "spec.numNodes: Required value",
		// and a naive substring scan matches that instead of the real key,
		// producing a guard that passes even when the key is deleted.
		if !hasYAMLKey(content, "numNodes") {
			t.Errorf("%s declares kind: ComputeDomain but does not set spec.numNodes.\n"+
				"  GPU Operator v26.7.0 ships a ComputeDomain CRD copy that marks numNodes\n"+
				"  REQUIRED with no default, and it is installed before the DRA driver's\n"+
				"  permissive copy. On a fresh cluster this CR is rejected at admission with\n"+
				"  \"spec.numNodes: Required value\".\n"+
				"  Set numNodes explicitly (0 is correct under IMEXDaemonsWithDNSNames=true,\n"+
				"  the DRA driver default, where each IMEX daemon starts without waiting for\n"+
				"  a quorum). See PR #2439.", path)
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
