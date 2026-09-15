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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"gopkg.in/yaml.v3"
)

// toolPins are the tools #2667 moved off `go install pkg@version` and onto a
// `go build` from this module. That build takes its version from go.mod, so
// go.mod is their only pin: .settings.yaml deliberately does not repeat it and
// tools/check-tools, tools/api-diff and the load-versions action all read the
// require line instead.
//
// #2741 is why. While the version lived in both files, Renovate updated them
// through different managers -- the native gomod manager for go.mod, a
// customManagers regex for .settings.yaml -- and shipped a PR that moved only
// one. tools/api-diff compares the built binary against the pin and exits 17 on
// drift, so the half-update failed nine unrelated-looking shell tests. The two
// assertions below keep the second copy from coming back.
var toolPins = []struct {
	settingsPath []string // key path that must NOT reappear in .settings.yaml
	toolPackage  string   // package named by the go.mod tool directive
	goModModule  string   // module whose require line is the pin
}{
	{[]string{"linting", "apidiff"}, "golang.org/x/exp/cmd/apidiff", "golang.org/x/exp"},
	{[]string{"linting", "go_licenses"}, "github.com/google/go-licenses/v2", "github.com/google/go-licenses/v2"},
}

// TestToolPinsLiveOnlyInGoMod holds go.mod as the single source of truth for
// every tool built from the main module.
func TestToolPinsLiveOnlyInGoMod(t *testing.T) {
	root := repoRoot(t)

	settings := loadSettings(t, filepath.Join(root, ".settings.yaml"))
	mf := parseGoMod(t, filepath.Join(root, "go.mod"))
	versions := requiredVersions(mf)
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

			if _, ok := versions[tp.goModModule]; !ok {
				t.Errorf("go.mod has no require for %s, which is the only pin this "+
					"tool has.", tp.goModModule)
			}

			// Every reader of this pin -- tools/api-diff, tools/check-tools and
			// the load-versions action, all via go_mod_required_version -- is a
			// text scan of the require line. None of them can see a `replace`,
			// but `go build` honors one, so a replaced module makes the require
			// line describe a version that is never built. That reads as a
			// passing pin over a tool built from somewhere else, which is the
			// dangerous direction. Rejected here rather than taught to every
			// reader: a wildcard replace has no version for them to report at
			// all. Both forms are rejected -- a version-specific replace still
			// diverts the build whenever the left side matches.
			for _, rep := range mf.Replace {
				if rep.Old.Path != tp.goModModule {
					continue
				}
				t.Errorf("go.mod replaces %s with %s, but the require line is this "+
					"tool's only pin and every reader of it parses that line as text.\n"+
					"`go build` would use the replacement while tools/api-diff and "+
					"tools/check-tools reported the require version, so a fork or a "+
					"local path would pass as the pinned release. Drop the replace, or "+
					"teach go_mod_required_version to resolve it before relying on it.",
					tp.goModModule, rep.New.Path)
			}

			if got, ok := settingsString(t, settings, tp.settingsPath); ok {
				t.Errorf(".settings.yaml pins %s = %s, but go.mod is the single source "+
					"of truth for tools built from this module.\n"+
					"A second copy drifts: Renovate moves the two files through "+
					"different managers and #2741 shipped a PR that updated only one, "+
					"failing the api-diff gate with exit 17. Delete the key and read "+
					"the version from the go.mod require line instead.",
					strings.Join(tp.settingsPath, "."), got)
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

// TestNoFileReadsTheRemovedToolPins walks the worktree for readers of the
// .settings.yaml keys this repo no longer defines.
//
// The keys are gone and TestToolPinsLiveOnlyInGoMod keeps them gone, but a
// reader left behind does not fail loudly: `yq` exits 0 and prints "null" for
// a missing key, so the caller gets the four-character string "null" as a
// version. #2741 shipped exactly that -- tools/setup-tools still read both
// keys, so `make tools-setup` compared every installed tool against "null",
// never matched, and rebuilt apidiff on every run while reporting success.
//
// A repo-wide scan rather than a list of known callers: the readers missed
// were tools/setup-tools and tools/generate-notices, both extensionless
// scripts that a *.sh glob does not match.
func TestNoFileReadsTheRemovedToolPins(t *testing.T) {
	root := repoRoot(t)

	// Assembled at run time so this file does not match its own scan.
	needles := []string{
		"linting" + "." + "apidiff",
		"linting" + "." + "go_licenses",
	}
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true, "bin": true,
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if path == filepath.Join(root, "tests", "architecture", "tool_pins_test.go") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 1<<20 {
			return nil //nolint:nilerr // unreadable or oversized files are not pin readers
		}
		data, err := os.ReadFile(path) //nolint:gosec // repo-relative walk
		if err != nil {
			return nil //nolint:nilerr // binaries and transient files are not pin readers
		}
		for _, needle := range needles {
			if !strings.Contains(string(data), needle) {
				continue
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			t.Errorf("%s still references .settings.yaml %s, which no longer exists.\n"+
				"yq prints \"null\" for a missing key and exits 0, so this reader gets the "+
				"string \"null\" as a version rather than an error. Read the go.mod require "+
				"line instead -- go_mod_required_version in tools/common does it.", rel, needle)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}
