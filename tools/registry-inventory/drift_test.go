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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestCommittedRegistryEgressDocFresh asserts the committed docs page is a byte
// projection of the live sources — the same freshness discipline the BOM uses.
// The doc is host-level only, so it drifts (and this test fails) just when the
// egress surface changes; run `make registry-docs` and commit to refresh it.
func TestCommittedRegistryEgressDocFresh(t *testing.T) {
	repoRoot := repoRootForTest()
	want := renderDoc(Collect(repoRoot))
	docPath := filepath.Join(repoRoot, "docs", "contributor", "registry-egress.md")
	got, err := os.ReadFile(docPath) //nolint:gosec // fixed in-repo doc path
	if err != nil {
		t.Fatalf("read %s: %v\n  Run `make registry-docs` to generate it.", docPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is stale — the build/CI egress surface changed.\n"+
			"  Run `make registry-docs` and commit the result.", docPath)
	}
}

// repoRootForTest points at the AICR root; tests run with CWD set to the
// package directory (tools/registry-inventory), two levels down.
func repoRootForTest() string { return filepath.Join("..", "..") }

// TestRegistryHostsWithinAllowlist is the drift gate. It statically extracts
// every registry/package host AICR's structured build & CI sources reach out to
// and fails if any host is missing from the committed allowlist — so a PR that
// introduces a new registry cannot merge without a reviewer consciously adding
// it. This is the registry-egress-surface analog of TestCommittedBOMVersionsMatchRegistry.
//
// It performs no network I/O (all inputs are in-repo files), so it runs cleanly
// under `make test`.
func TestRegistryHostsWithinAllowlist(t *testing.T) {
	inv := Collect(repoRootForTest())

	unknown, unused, err := diffAllowlist(allowlistFile, inv.Hosts)
	if err != nil {
		t.Fatalf("diffAllowlist: %v", err)
	}

	for _, h := range unknown {
		t.Errorf("egress host %q is not in %s.\n"+
			"  A structured build/CI source now reaches this host. If intended, add it to\n"+
			"  the allowlist with justification; otherwise pin the dependency to an approved host.",
			h, allowlistFile)
	}

	// A required source that failed to read/parse silently drops its egress from
	// the inventory while the host diff stays green — fail on it too (F14/F25).
	for _, e := range inv.SourceErrors {
		t.Errorf("required source failed to parse (its egress is missing from the gate): %s", e)
	}

	// Unused entries are informational only — a removed dependency should not
	// hard-fail an unrelated PR — but surfacing them keeps the allowlist honest.
	for _, h := range unused {
		t.Logf("note: allowlisted host %q is no longer detected; consider pruning %s", h, allowlistFile)
	}
}

// TestExternalActionsArePinned asserts that every GitHub Actions `uses:` ref in
// active workflows is pinned to a full 40-char commit SHA. Branch/tag/latest
// pins on a marketplace action are a mutable supply-chain surface.
func TestExternalActionsArePinned(t *testing.T) {
	inv := Collect(repoRootForTest())

	for _, r := range inv.Records {
		if r.PackageType != PkgGitHubAction {
			continue
		}
		// First-party reusable workflows/actions under the AICR repo are the
		// one legitimate non-SHA case (release automation pins them to tags);
		// gate only third-party marketplace actions.
		if isFirstPartyAction(r.Detail) {
			continue
		}
		if r.PinType != PinSHA {
			t.Errorf("external action %q is pinned by %s (%q), want a 40-char commit SHA (%s)",
				r.Detail, r.PinType, r.Pin, r.Source)
		}
	}
}

func isFirstPartyAction(ownerRepo string) bool {
	// Fail closed: an empty/malformed identity is NOT first-party — it must
	// continue through pin validation rather than be waved past.
	return ownerRepo == "NVIDIA/aicr" ||
		filepathHasPrefix(ownerRepo, "NVIDIA/aicr/")
}

func filepathHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
