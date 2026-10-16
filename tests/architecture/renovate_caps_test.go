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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

// kustomizeCapLiftsAt is the floor of the k8s.io/apimachinery v0.38 line, which
// moves to sigs.k8s.io/structured-merge-diff/v7 (v0.38.0-alpha.2 already
// requires it), the version kustomize v0.21.3 pulls in through
// k8s.io/kube-openapi. The "-0" pre-release sorts below every other
// pre-release, so an alpha bump counts as reaching it.
const kustomizeCapLiftsAt = "v0.38.0-0"

// TestKustomizeRenovateCapLiftsAtK8s038 ties the .github/renovate.json5 cap on
// sigs.k8s.io/kustomize to the k8s.io/apimachinery version in go.mod, so the
// k8s 0.38 bump cannot merge while the cap still holds kustomize back (#3273).
func TestKustomizeRenovateCapLiftsAtK8s038(t *testing.T) {
	root := repoRoot(t)

	versions := requiredVersions(parseGoMod(t, filepath.Join(root, "go.mod")))
	apimachinery, ok := versions["k8s.io/apimachinery"]
	if !ok {
		t.Fatal("go.mod has no require for k8s.io/apimachinery")
	}

	path := filepath.Join(root, ".github", "renovate.json5")
	cfg, err := os.ReadFile(path) //nolint:gosec // repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if problem := kustomizeCapProblem(apimachinery, string(cfg)); problem != "" {
		t.Error(problem)
	}
}

func TestKustomizeCapProblem(t *testing.T) {
	const (
		capped = `{
  packageRules: [
    {
      matchManagers: ["gomod"],
      matchDepNames: ["sigs.k8s.io/kustomize/api", "sigs.k8s.io/kustomize/kyaml"],
      allowedVersions: "<0.21.3",
    },
  ],
}`
		uncapped = `{
  packageRules: [
    {
      groupName: "kubernetes",
      matchPackageNames: ["k8s.io/**", "sigs.k8s.io/**"],
    },
  ],
}`
		commentOnly = `{
  packageRules: [
    // matchDepNames: ["sigs.k8s.io/kustomize/api"], removed with k8s 0.38.
    {
      groupName: "kubernetes",
      matchPackageNames: ["k8s.io/**", "sigs.k8s.io/**"],
    },
  ],
}`
	)

	tests := []struct {
		name         string
		apimachinery string
		cfg          string
		wantProblem  string // substring; empty means no problem
	}{
		{"v0.37 with cap", "v0.37.1", capped, ""},
		{"v0.37 without cap", "v0.37.1", uncapped, "Restore the cap"},
		{"comment alone is not a cap", "v0.37.1", commentOnly, "Restore the cap"},
		{"v0.37 last patch with cap", "v0.37.99", capped, ""},
		{"exact floor with cap", "v0.38.0-0", capped, "Delete the"},
		{"v0.38 alpha with cap", "v0.38.0-alpha.2", capped, "Delete the"},
		{"v0.38 with cap", "v0.38.0", capped, "Delete the"},
		{"v0.39 with cap", "v0.39.2", capped, "Delete the"},
		{"v0.38 without cap", "v0.38.0", uncapped, ""},
		{"invalid version", "latest", capped, "not a semantic version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kustomizeCapProblem(tt.apimachinery, tt.cfg)
			if tt.wantProblem == "" {
				if got != "" {
					t.Errorf("unexpected problem: %s", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantProblem) {
				t.Errorf("problem = %q, want it to contain %q", got, tt.wantProblem)
			}
		})
	}
}

// kustomizeCapProblem reports what is wrong with the kustomize cap for the
// given apimachinery version, or "" when the cap matches it.
//
// Below v0.38 the cap must be present, not merely tolerated: that is what
// proves hasKustomizeCap still recognizes the rule. Without it a reformatted
// rule would go undetected and the check meant to remove it would pass.
func kustomizeCapProblem(apimachinery, renovateCfg string) string {
	if !semver.IsValid(apimachinery) {
		return fmt.Sprintf("go.mod requires k8s.io/apimachinery %q, which is not a semantic version", apimachinery)
	}
	capped := hasKustomizeCap(renovateCfg)
	unblocked := semver.Compare(apimachinery, kustomizeCapLiftsAt) >= 0

	switch {
	case unblocked && capped:
		return fmt.Sprintf("go.mod requires k8s.io/apimachinery %s, from the v0.38 line that moves to "+
			"sigs.k8s.io/structured-merge-diff/v7, the version conflict that blocked kustomize v0.21.3.\n"+
			"Delete the sigs.k8s.io/kustomize allowedVersions rule from .github/renovate.json5 "+
			"and this test in this PR (#3273). Left in place, the cap holds kustomize at v0.21.2 for good.",
			apimachinery)
	case !unblocked && !capped:
		return fmt.Sprintf("go.mod requires k8s.io/apimachinery %s, which still builds against "+
			"sigs.k8s.io/structured-merge-diff/v6, but .github/renovate.json5 has no rule naming "+
			"sigs.k8s.io/kustomize. Without it Renovate proposes kustomize v0.21.3, which fails "+
			"`go build` (#3246, #3271).\n"+
			"Restore the cap, or update hasKustomizeCap if the rule was only reformatted.",
			apimachinery)
	}
	return ""
}

// hasKustomizeCap reports whether a non-comment line of the Renovate config
// names a sigs.k8s.io/kustomize module. The kubernetes group matches
// sigs.k8s.io/** by glob, so the cap is the only rule that names kustomize.
// A line-based scan rather than a parse: this module has no JSON5 decoder, and
// tools/drift-report/config_test.go scans renovate.json5 the same way.
func hasKustomizeCap(cfg string) bool {
	for line := range strings.SplitSeq(cfg, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, `"sigs.k8s.io/kustomize/`) {
			return true
		}
	}
	return false
}
