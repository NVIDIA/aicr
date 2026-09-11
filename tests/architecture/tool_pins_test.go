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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"gopkg.in/yaml.v3"
)

// toolPins are the tools #2667 moved off `go install pkg@version` and onto a
// `go build` from this module. That build takes its version from go.mod, while
// the rest of the repo -- tools/check-tools, the composite-action inputs, the
// setup-tools console output -- still reads .settings.yaml. Two files now
// describe one version.
//
// Renovate updates them through different managers (the native gomod manager
// for go.mod, a customManagers regex for .settings.yaml), so they can be bumped
// in separate PRs and drift. Drift is not cosmetic: `make api-diff` and
// `make license-check` would run a different tool version than the one the
// repository documents and than tools/check-tools verifies against, and
// check-tools would start failing locally with no obvious cause.
var toolPins = []struct {
	settingsPath []string // key path within .settings.yaml
	toolPackage  string   // package named by the go.mod tool directive
	goModModule  string   // module whose go.mod version must match
}{
	{[]string{"linting", "apidiff"}, "golang.org/x/exp/cmd/apidiff", "golang.org/x/exp"},
	{[]string{"linting", "go_licenses"}, "github.com/google/go-licenses/v2", "github.com/google/go-licenses/v2"},
}

// TestToolPinsMatchGoMod holds .settings.yaml and go.mod to the same version for
// every tool built from the main module.
func TestToolPinsMatchGoMod(t *testing.T) {
	root := repoRoot(t)

	settings := loadSettings(t, filepath.Join(root, ".settings.yaml"))
	mf := parseGoMod(t, filepath.Join(root, "go.mod"))
	versions := requiredVersions(mf)

	// A `replace` that retargets one of these modules makes the require line a
	// lie about what `go build` produces, and this test would then demand
	// .settings.yaml match the pre-replacement version -- failing the engineer
	// who correctly recorded the real one. Refuse to render a verdict instead.
	for _, rep := range mf.Replace {
		for _, tp := range toolPins {
			if rep.Old.Path == tp.goModModule {
				t.Fatalf("go.mod replaces %s; this test compares .settings.yaml against the "+
					"require line, which no longer describes what `go build` produces. "+
					"Teach it to resolve the replacement before relying on it again.",
					tp.goModModule)
			}
		}
	}

	tools := toolDirectives(mf)

	for _, tp := range toolPins {
		t.Run(strings.Join(tp.settingsPath, "."), func(t *testing.T) {
			// Checked per tool rather than in a separate test so a dropped
			// directive cannot be masked by the module still being required
			// for another reason. `go mod tidy` would eventually drop the
			// require too, but that is a later signal with a vaguer message.
			if !tools[tp.toolPackage] {
				t.Errorf("go.mod has no tool directive for %s.\n"+
					"Without it `go mod tidy` drops the dependency and the `go build` in "+
					"CI resolves the package outside this module, silently restoring the "+
					"sum.golang.org dependency #2667 removed. Restore it with "+
					"`go get -tool %s`.", tp.toolPackage, tp.toolPackage)
				return
			}

			want, ok := settingsString(t, settings, tp.settingsPath)
			if !ok {
				t.Fatalf(".settings.yaml is missing %s", strings.Join(tp.settingsPath, "."))
			}
			got, ok := versions[tp.goModModule]
			if !ok {
				t.Fatalf("go.mod has no require for %s", tp.goModModule)
			}
			if got != want {
				t.Errorf("%s pins %s but go.mod requires %s.\n"+
					"Both describe the version of the same tool, which is built from this "+
					"module. Bump whichever is stale; a version bump to one is not complete "+
					"without the other.",
					strings.Join(tp.settingsPath, "."), want, got)
			}
		})
	}
}

// parseGoMod parses go.mod with the same library the go command uses, so
// `replace`, `exclude` and the `tool` block are understood rather than
// pattern-matched. golang.org/x/mod is already a direct dependency
// (pkg/corroborate/generate.go uses its semver package), so this costs nothing.
func parseGoMod(t *testing.T, path string) *modfile.File {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	mf, err := modfile.Parse(path, data, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return mf
}

// requiredVersions maps module path to required version.
func requiredVersions(mf *modfile.File) map[string]string {
	out := make(map[string]string, len(mf.Require))
	for _, r := range mf.Require {
		out[r.Mod.Path] = r.Mod.Version
	}
	return out
}

// toolDirectives is the set of packages named by `tool` directives.
func toolDirectives(mf *modfile.File) map[string]bool {
	out := make(map[string]bool, len(mf.Tool))
	for _, tool := range mf.Tool {
		out[tool.Path] = true
	}
	return out
}

// loadSettings decodes .settings.yaml into a generic tree. A typed struct would
// have to track every unrelated key in the file.
func loadSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

// settingsString walks a key path and returns the leaf as a string. A key that
// exists but holds a non-string is reported as such rather than as missing:
// dropping the quotes on a version YAML types as a float would otherwise send
// the reader looking for a key that is already there.
func settingsString(t *testing.T, tree map[string]any, path []string) (string, bool) {
	t.Helper()
	var cur any = tree
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[key]
		if !ok {
			return "", false
		}
	}
	s, ok := cur.(string)
	if !ok {
		t.Fatalf("%s is %T, want string (quote the value so YAML keeps it a string)",
			strings.Join(path, "."), cur)
	}
	return s, true
}
