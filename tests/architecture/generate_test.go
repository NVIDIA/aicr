// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package architecture

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestGeneratePolicy writes the current observed reference set to
// facade-policy.yaml. It is an authoring tool, not a gate: it only runs when
// AICR_WRITE_FACADE_POLICY=1, mirroring the AICR_UPDATE_GOLDEN convention used
// by pkg/recipe's golden tests.
//
// The generated file classifies every package as constrained. Sorting packages
// into facade/infrastructure and writing the reason for each is a human step —
// the generator deliberately does not guess, because a wrong reason is worse
// than a missing one.
func TestGeneratePolicy(t *testing.T) {
	if os.Getenv("AICR_WRITE_FACADE_POLICY") != "1" {
		t.Skip("set AICR_WRITE_FACADE_POLICY=1 to regenerate facade-policy.yaml")
	}

	refs := observedReferences(t)

	byPackage := make(map[string]map[string]symbolClass)
	for ref := range refs {
		if byPackage[ref.Package] == nil {
			byPackage[ref.Package] = make(map[string]symbolClass)
		}
		byPackage[ref.Package][ref.Symbol] = ref.Class
	}

	out := policy{Version: 1, Constrained: make(map[string]constrainedPackage, len(byPackage))}
	for name, symbols := range byPackage {
		out.Constrained[name] = constrainedPackage{
			Reason:    "TODO: state why this is not a facade gap",
			Permanent: true,
			Symbols:   symbols,
		}
	}

	data, err := yaml.Marshal(out)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	// Destructive full regen, not a merge: this overwrites the entire committed
	// file with every observed package sorted into constrained, discarding the
	// facade/infrastructure bucketing and every curated reason/tracking value
	// in the file being replaced. Whoever runs this to capture one new symbol
	// must hand-reapply all of that curation before the result can be
	// committed -- see "Regenerating the policy" in
	// docs/contributor/architecture-gate.md.
	if err := os.WriteFile("facade-policy.yaml", data, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	t.Logf("wrote facade-policy.yaml with %d packages", len(out.Constrained))
}

// observedReferences is the single source of what the gate sees, shared by the
// generator and the gate itself so the two can never diverge.
func observedReferences(t *testing.T) map[reference]bool {
	t.Helper()
	const prefix = "github.com/NVIDIA/aicr/"
	paths := analyzedPackagePaths(t, prefix)
	loaded := loadForAnalysis(t, paths...)

	refs := make(map[reference]bool)
	for _, path := range paths {
		lp := loaded[path]
		for ref := range packageQualifiedRefs(lp, prefix) {
			refs[ref] = true
		}
		for ref := range foreignMethodRefs(lp, prefix) {
			refs[ref] = true
		}
	}
	return dropInternalRefs(refs)
}

// analyzedRootPackages are the two facade-boundary import-path roots this
// gate scans, including their entire subtree. Keeping this as the single
// `go list` pattern (rather than the two exact package paths) means a future
// split -- e.g. pkg/cli/foo -- is picked up automatically instead of
// silently escaping the gate until someone notices and allowlists it away.
var analyzedRootPackages = []string{"./pkg/cli/...", "./pkg/server/..."}

// analyzedPackagePaths resolves every package under the analyzed roots to
// its concrete import path via `go list`. Today that returns exactly
// pkg/cli and pkg/server -- there are no subpackages yet -- so the observed
// reference set is unchanged from a hardcoded two-package list.
func analyzedPackagePaths(t *testing.T, modulePrefix string) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), goListTimeout)
	defer cancel()

	root := repoRoot(t)
	args := append([]string{"list"}, analyzedRootPackages...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("go list %s: %s", strings.Join(analyzedRootPackages, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		t.Fatalf("run go list: %v", err)
	}

	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, modulePrefix) {
			t.Fatalf("go list returned package %q outside module prefix %q", line, modulePrefix)
		}
		paths = append(paths, line)
	}
	if len(paths) == 0 {
		t.Fatal("go list returned no packages under pkg/cli or pkg/server")
	}
	return paths
}

// analyzedInternalPrefixes are the module-relative roots covered by
// isInternalToAnalyzed: pkg/cli and pkg/server, including their subtrees.
var analyzedInternalPrefixes = []string{"pkg/cli", "pkg/server"}

// isInternalToAnalyzed reports whether pkgPath (module-relative, e.g.
// "pkg/cli/foo") is itself part of the analyzed set: pkg/cli, pkg/server, or
// a package under either one's subtree. A reference whose owner package
// satisfies this is internal wiring within the analyzed set, not a
// business-logic reference that could bypass the facade -- for example a
// call from pkg/cli/foo into pkg/cli/bar, or into pkg/cli itself.
func isInternalToAnalyzed(pkgPath string) bool {
	for _, prefix := range analyzedInternalPrefixes {
		if pkgPath == prefix || strings.HasPrefix(pkgPath, prefix+"/") {
			return true
		}
	}
	return false
}

// dropInternalRefs removes every reference whose owner package is part of
// the analyzed set itself. See isInternalToAnalyzed.
func dropInternalRefs(refs map[reference]bool) map[reference]bool {
	filtered := make(map[reference]bool, len(refs))
	for ref := range refs {
		if isInternalToAnalyzed(ref.Package) {
			continue
		}
		filtered[ref] = true
	}
	return filtered
}

// TestAnalyzedPackagePathsCoversWholeSubtree pins that the analyzed set is
// resolved via `go list ./pkg/cli/... ./pkg/server/...`, not a hardcoded
// two-package list — so a future `pkg/cli/foo` split is picked up
// automatically instead of silently escaping the gate. It only asserts that
// pkg/cli and pkg/server are included, not that they are the only entries:
// a subpackage split is a supported architecture change (the very thing the
// subtree expansion exists to handle), so this test must keep passing when
// one appears rather than breaking on a legitimate refactor.
func TestAnalyzedPackagePathsCoversWholeSubtree(t *testing.T) {
	t.Parallel()

	const prefix = "github.com/NVIDIA/aicr/"
	paths := analyzedPackagePaths(t, prefix)

	got := make(map[string]bool, len(paths))
	for _, path := range paths {
		got[path] = true
	}
	for _, want := range []string{prefix + "pkg/cli", prefix + "pkg/server"} {
		if !got[want] {
			t.Errorf("analyzedPackagePaths() = %v, missing %q", paths, want)
		}
	}
}

// TestIsInternalToAnalyzed pins the prefix-skip predicate that stops a
// reference from one analyzed package into another (e.g. a future
// pkg/cli/foo calling into pkg/cli/bar, or into pkg/cli itself) from being
// recorded as a business-logic reference.
func TestIsInternalToAnalyzed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pkg  string
		want bool
	}{
		{"pkg/cli", true},
		{"pkg/cli/foo", true},
		{"pkg/cli/foo/bar", true},
		{"pkg/server", true},
		{"pkg/server/bar", true},
		{"pkg/recipe", false},
		{"pkg/client/v1", false},
		// Prefix-collision guard: a bare strings.HasPrefix("pkg/cliX", "pkg/cli")
		// would wrongly match; the predicate must require a "/" boundary or an
		// exact match.
		{"pkg/clix", false},
		{"pkg/serverless", false},
	}
	for _, tt := range tests {
		t.Run(tt.pkg, func(t *testing.T) {
			t.Parallel()
			if got := isInternalToAnalyzed(tt.pkg); got != tt.want {
				t.Errorf("isInternalToAnalyzed(%q) = %v, want %v", tt.pkg, got, tt.want)
			}
		})
	}
}

// TestDropInternalRefs proves the subtree-escape fix hermetically, without a
// real pkg/cli subpackage: a reference whose owner is under pkg/cli or
// pkg/server (simulating a future split) must never survive into the
// gate's observed reference set, while references into unrelated packages
// (including the facade) pass through untouched.
func TestDropInternalRefs(t *testing.T) {
	t.Parallel()

	in := map[reference]bool{
		{Package: "pkg/cli", Symbol: "Run", Class: classBehavioral}:             true,
		{Package: "pkg/cli/foo", Symbol: "Helper", Class: classBehavioral}:      true,
		{Package: "pkg/server/bar", Symbol: "Handler", Class: classType}:        true,
		{Package: "pkg/recipe", Symbol: "Recipe", Class: classType}:             true,
		{Package: "pkg/client/v1", Symbol: "NewClient", Class: classBehavioral}: true,
	}

	got := dropInternalRefs(in)

	want := map[reference]bool{
		{Package: "pkg/recipe", Symbol: "Recipe", Class: classType}:             true,
		{Package: "pkg/client/v1", Symbol: "NewClient", Class: classBehavioral}: true,
	}
	if len(got) != len(want) {
		t.Fatalf("dropInternalRefs() = %+v, want %+v", got, want)
	}
	for ref := range want {
		if !got[ref] {
			t.Errorf("dropInternalRefs() dropped %+v, want kept", ref)
		}
	}
	for ref := range got {
		if isInternalToAnalyzed(ref.Package) {
			t.Errorf("dropInternalRefs() kept internal reference %+v", ref)
		}
	}
}
