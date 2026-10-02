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
	"os/exec"
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
//
// Scope comes from git, not from walking the worktree. A walk also reads
// whatever the developer happens to have on disk -- agent scratch
// directories, extra git worktrees, editor caches, local virtualenvs -- and
// reports their contents as defects in this repository. Those paths are
// ignored precisely because they are not the repository, and a denylist of
// directory basenames never keeps up with the next tool to invent one.
// `--cached --others --exclude-standard` is the honest universe: everything
// tracked plus everything untracked that is not ignored, so a brand-new
// reader is still caught before it is staged.
func TestNoFileReadsTheRemovedToolPins(t *testing.T) {
	root := repoRoot(t)

	// Assembled at run time so this file does not match its own scan.
	needles := []string{
		"linting" + "." + "apidiff",
		"linting" + "." + "go_licenses",
	}

	const self = "tests/architecture/tool_pins_test.go"

	// -z because a path may contain anything but NUL; the default listing
	// quotes such paths instead, which would not resolve as a filename.
	cmd := exec.Command("git", "-C", root, "ls-files", "--cached", "--others",
		"--exclude-standard", "-z")
	cmd.Env = gitScanEnv(t)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// Without stderr this reports only "exit status 128", which is the
		// same for a missing git, a non-repository, and a permission error.
		t.Fatalf("git ls-files in %s: %v: %s", root, err, strings.TrimSpace(stderr.String()))
	}
	paths := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")

	scanned := 0
	for _, rel := range paths {
		if rel == "" || rel == self {
			continue
		}
		path := filepath.Join(root, rel)
		info, statErr := os.Stat(path)
		if statErr != nil || info.IsDir() || info.Size() > 1<<20 {
			// Submodule gitlinks are directories, a tracked path may be
			// deleted in the worktree, and an oversized file is not a
			// hand-written pin reader.
			continue
		}
		data, readErr := os.ReadFile(path) //nolint:gosec // repo-relative, from git ls-files
		if readErr != nil {
			continue
		}
		scanned++
		for _, needle := range needles {
			if !strings.Contains(string(data), needle) {
				continue
			}
			t.Errorf("%s still references .settings.yaml %s, which no longer exists.\n"+
				"yq prints \"null\" for a missing key and exits 0, so this reader gets the "+
				"string \"null\" as a version rather than an error. Read the go.mod require "+
				"line instead -- go_mod_required_version in tools/common does it.", rel, needle)
		}
	}

	// Without this the scan passes vacuously when git is unavailable or the
	// listing comes back empty, which is the one failure mode a grep-based
	// guard cannot survive: it would report success having read nothing.
	if scanned == 0 {
		t.Fatal("git ls-files yielded no readable files; discovery is broken and this scan " +
			"would pass without having examined anything")
	}
}

// TestToolPinScanIgnoresInheritedGitDir pins the hazard gitScanEnv exists for:
// an inherited GIT_DIR retargets `git -C root ls-files` at another repository,
// and the scan above would then examine that repository's files and report a
// clean result for this one.
//
// Both directions are asserted. Checking only that the sanitized listing is
// correct would still pass if -C already beat the environment, leaving the
// sanitizing dead code that no one could safely remove.
func TestToolPinScanIgnoresInheritedGitDir(t *testing.T) {
	root := repoRoot(t)

	// A decoy repository holding exactly one file, so a hijacked listing is
	// unmistakable rather than merely different.
	//
	// Build it with the sanitized environment too. These commands run before
	// the t.Setenv calls below, but the ambient environment may already carry
	// GIT_DIR -- which is the situation this test exists for. `git init` would
	// then initialize that repository instead of the decoy, and worse, `git
	// config` would write into it: a test leaving its fixture's identity in
	// the developer's real repository.
	cleanEnv := gitScanEnv(t)
	decoy := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		setup := exec.Command("git", append([]string{"-C", decoy}, args...)...)
		setup.Env = cleanEnv
		if out, err := setup.CombinedOutput(); err != nil {
			t.Fatalf("git %v in decoy: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(decoy, "decoy.txt"), []byte("decoy\n"), 0o600); err != nil {
		t.Fatalf("write decoy file: %v", err)
	}

	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)

	list := func(env []string) string {
		t.Helper()
		cmd := exec.Command("git", "-C", root, "ls-files", "--cached", "--others",
			"--exclude-standard", "-z")
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git ls-files: %v", err)
		}
		return string(out)
	}

	// The environment wins over -C: without sanitizing, the scan reads the
	// decoy. If this stops holding, gitScanEnv is no longer load-bearing.
	if raw := list(os.Environ()); strings.Contains(raw, "go.mod") {
		t.Error("an inherited GIT_DIR no longer retargets the listing, so gitScanEnv " +
			"guards nothing; confirm before deleting it")
	}

	if sanitized := list(gitScanEnv(t)); !strings.Contains(sanitized, "go.mod") {
		t.Error("sanitized listing does not contain go.mod, so the scan is still reading " +
			"the repository the environment points at rather than the one under test")
	}
}

// gitScanEnv returns the process environment with git's repository-local
// variables stripped, so `git -C root` resolves the repository by ordinary
// discovery from root.
//
// -C only changes the directory git starts in. GIT_DIR, GIT_WORK_TREE,
// GIT_INDEX_FILE and their siblings override discovery outright and win over
// it. Git sets several of them for hooks, and `git bisect run`, `git rebase
// --exec`, and CI wrappers all propagate them into child processes -- so a
// scan inheriting them silently reads a different repository. Measured here:
// an inherited GIT_DIR took the listing from 2523 files to 1, which this test
// would then have reported as a clean scan.
//
// The names come from git rather than a literal list, for the same reason the
// scan above no longer keeps a literal list of directories to skip: the set
// grows, and `git rev-parse --local-env-vars` is the maintained answer. It
// reports names only, needing no repository of its own, so it is safe to call
// before the environment has been cleaned.
func gitScanEnv(t *testing.T) []string {
	t.Helper()

	out, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		t.Fatalf("git rev-parse --local-env-vars: %v", err)
	}
	local := make(map[string]bool)
	for name := range strings.FieldsSeq(string(out)) {
		local[name] = true
	}
	if len(local) == 0 {
		t.Fatal("git rev-parse --local-env-vars named nothing; the sanitizing step below " +
			"would be a no-op and an inherited GIT_DIR would retarget the scan")
	}

	env := os.Environ()
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok && local[name] {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}
