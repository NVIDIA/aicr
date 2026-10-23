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

package releasepolicy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// notesBaseStepName is the on-tag.yaml step that pins GoReleaser's current tag
// and previous tag for a stable release (#3289).
const notesBaseStepName = "Pin the release tag and the previous stable tag"

// notesBaseStepTimeout bounds one run of the extracted step. It only runs a few
// local git commands, so a hang means a broken fixture, not a slow machine.
const notesBaseStepTimeout = 30 * time.Second

// notesBaseGoReleaserDefault is the lookup GoReleaser v2.18.3 performs when
// GORELEASER_PREVIOUS_TAG is unset (previousTagSha in internal/pipe/git). The
// fixtures assert it picks a pre-release, so a fixture that stops reproducing
// the bug fails loudly instead of letting the fix pass vacuously.
func notesBaseGoReleaserDefault(t *testing.T, dir, tag string) string {
	t.Helper()
	return notesBaseGit(t, dir, "describe", "--tags", "--abbrev=0", "tags/"+tag+"^")
}

// notesBaseGoReleaserCurrentDefault is the lookup GoReleaser v2.18.3 performs
// for the current tag when GORELEASER_CURRENT_TAG is unset and no
// git.prerelease_suffix is configured (getTag in internal/pipe/git): the first
// line of `git tag --points-at HEAD --sort -version:refname`. git version-sorts
// an RC ahead of its own GA, so with both tags on one commit this names the RC.
func notesBaseGoReleaserCurrentDefault(t *testing.T, dir string) string {
	t.Helper()
	out := notesBaseGit(t, dir, "tag", "--points-at", "HEAD", "--sort", "-version:refname")
	first, _, _ := strings.Cut(out, "\n")
	return first
}

// TestReleaseNotesBaseStepShape pins how the step is wired. The behavior is
// covered by TestReleaseNotesStartAtPreviousStableTag; this guards the wiring
// that behavior test cannot see: which tags the step applies to, and that it
// runs before GoReleaser does.
func TestReleaseNotesBaseStepShape(t *testing.T) {
	t.Parallel()
	doc := loadYAML(t, ".github/workflows/on-tag.yaml")
	job := mapValue(t, mapValue(t, doc, "jobs"), "build-ko")
	steps := sliceValue(t, job, "steps")

	index := stepIndex(steps, notesBaseStepName)
	if index < 0 {
		t.Fatalf("build-ko must have a %q step", notesBaseStepName)
	}
	step := steps[index].(map[string]any)

	// Pre-releases keep GoReleaser's default: an RC's notes cover the changes
	// since the previous tag of any kind.
	if got := stringValue(t, step, "if"); got != "needs.detect.outputs.is_prerelease == 'false'" {
		t.Errorf("step if = %q, want it limited to stable tags", got)
	}

	// The step has to land before GoReleaser reads the environment.
	release := stepIndex(steps, "Build and Release")
	if release < 0 {
		t.Fatal("build-ko must have a Build and Release step")
	}
	if index >= release {
		t.Errorf("%q (step %d) must run before Build and Release (step %d)", notesBaseStepName, index, release)
	}

	// GITHUB_ENV reaches the composite action's steps; a bare `export` in this
	// step would not. Guard against that regression.
	script := stringValue(t, step, "run")
	if !strings.Contains(script, `>> "${GITHUB_ENV}"`) {
		t.Error("step must export GORELEASER_PREVIOUS_TAG through GITHUB_ENV, not the step shell")
	}
	if !strings.Contains(script, "GORELEASER_PREVIOUS_TAG=") {
		t.Error("step must set GORELEASER_PREVIOUS_TAG")
	}

	// The current tag must be pinned before the previous-tag lookup, whose
	// "no earlier stable tag" path exits the step early. Otherwise the first
	// stable release would skip the pin.
	current := strings.Index(script, "GORELEASER_CURRENT_TAG=${TAG}")
	if current < 0 {
		t.Error("step must set GORELEASER_CURRENT_TAG to the tag that triggered the run")
	}
	if lookup := strings.Index(script, "git describe"); current >= 0 && lookup >= 0 && current > lookup {
		t.Error("GORELEASER_CURRENT_TAG must be written before the previous-tag lookup, which can exit early")
	}
	if exit := strings.Index(script, "exit 0"); current >= 0 && exit >= 0 && current > exit {
		t.Error("GORELEASER_CURRENT_TAG must be written before the step's early exit")
	}

	// The tag reaches the shell through env, never interpolated into run.
	if strings.Contains(script, "${{") {
		t.Error("step run block must not interpolate GitHub expressions; pass the tag through env")
	}
	if stringValue(t, mapValue(t, step, "env"), "TAG") != "${{ github.ref_name }}" {
		t.Error("step must take the tag from github.ref_name through env")
	}

	// GoReleaser has to see the variable, so nothing may clear it afterwards.
	for _, raw := range steps[index+1 : release+1] {
		later := raw.(map[string]any)
		if strings.Contains(fmt.Sprint(later["run"]), "GORELEASER_PREVIOUS_TAG") {
			t.Errorf("step %q must not touch GORELEASER_PREVIOUS_TAG", later["name"])
		}
	}
}

// TestReleaseNotesStartAtPreviousStableTag runs the shipped step against real
// repositories. The acceptance criterion is that a stable tag pushed while RC
// tags of the same version are still present gets release notes that start at
// the previous stable tag.
func TestReleaseNotesStartAtPreviousStableTag(t *testing.T) {
	t.Parallel()
	script := notesBaseScript(t)

	cases := []struct {
		name string
		// history is a sequence of commits; each entry lists the tags created
		// at that commit, in order. The last commit is the one being released.
		history [][]string
		tag     string
		// goreleaserDefault is what GoReleaser would choose as the previous tag
		// with no override, or "" when the fixture is not about that failure.
		goreleaserDefault string
		// currentDefault is what GoReleaser would choose as the CURRENT tag
		// with no override, or "" when that already equals tag. Set it where GA
		// and an RC share a commit and the default is wrong.
		currentDefault string
		// want is the value exported as GORELEASER_PREVIOUS_TAG, or "" when the
		// step must not export one (no earlier stable tag). GORELEASER_CURRENT_TAG
		// is exported in every case.
		want string
	}{
		{
			name: "GA on a later commit than the RC (the v1.0.0 case)",
			history: [][]string{
				{"v1.2.3"},
				{"v1.3.0-rc1"},
				{"v1.3.0-rc2"},
				{"v1.3.0"},
			},
			tag:               "v1.3.0",
			goreleaserDefault: "v1.3.0-rc2",
			want:              "v1.2.3",
		},
		{
			name: "GA promoted on the commit of rc2, rc1 still on the remote",
			history: [][]string{
				{"v1.2.3"},
				{"v1.3.0-rc1"},
				{"v1.3.0-rc2", "v1.3.0"},
			},
			tag:               "v1.3.0",
			goreleaserDefault: "v1.3.0-rc1",
			currentDefault:    "v1.3.0-rc2",
			want:              "v1.2.3",
		},
		{
			name: "GA with several RCs and commits between them",
			history: [][]string{
				{"v1.2.3"},
				{},
				{"v1.3.0-rc1"},
				{},
				{"v1.3.0-rc2"},
				{},
				{"v1.3.0"},
			},
			tag:               "v1.3.0",
			goreleaserDefault: "v1.3.0-rc2",
			want:              "v1.2.3",
		},
		{
			name: "nearest stable tag wins over an older one",
			history: [][]string{
				{"v1.2.2"},
				{"v1.2.3"},
				{"v1.3.0-rc1"},
				{"v1.3.0"},
			},
			tag:               "v1.3.0",
			goreleaserDefault: "v1.3.0-rc1",
			want:              "v1.2.3",
		},
		{
			name: "patch release while an RC of the next minor exists",
			history: [][]string{
				{"v1.2.3"},
				{"v1.3.0-rc1"},
				{"v1.2.4"},
			},
			tag:               "v1.2.4",
			goreleaserDefault: "v1.3.0-rc1",
			want:              "v1.2.3",
		},
		{
			name: "no RC present is unchanged",
			history: [][]string{
				{"v1.2.3"},
				{},
				{"v1.3.0"},
			},
			tag:  "v1.3.0",
			want: "v1.2.3",
		},
		{
			name: "non-release tags are not a base",
			history: [][]string{
				{"v1.2.3"},
				{"nightly", "latest", "v1.3.0-rc1"},
				{"vNext"},
				{"v1.3.0"},
			},
			tag:               "v1.3.0",
			goreleaserDefault: "vNext",
			want:              "v1.2.3",
		},
		{
			name: "first stable release has no base but still pins the current tag",
			history: [][]string{
				{"v1.0.0-rc1"},
				{"v1.0.0-rc2"},
				{"v1.0.0"},
			},
			tag:  "v1.0.0",
			want: "",
		},
		{
			name: "first stable release promoted on its RC's commit still pins the current tag",
			history: [][]string{
				{"v1.0.0-rc1"},
				{"v1.0.0-rc2", "v1.0.0"},
			},
			tag:            "v1.0.0",
			currentDefault: "v1.0.0-rc2",
			want:           "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := notesBaseRepo(t, tc.history)

			if tc.goreleaserDefault != "" {
				if got := notesBaseGoReleaserDefault(t, dir, tc.tag); got != tc.goreleaserDefault {
					t.Fatalf("fixture does not reproduce the bug: GoReleaser's own lookup = %q, want %q",
						got, tc.goreleaserDefault)
				}
			}
			if tc.currentDefault != "" {
				if got := notesBaseGoReleaserCurrentDefault(t, dir); got != tc.currentDefault {
					t.Fatalf("fixture does not reproduce the bug: GoReleaser's own current tag = %q, want %q",
						got, tc.currentDefault)
				}
			}

			githubEnv, output, err := runNotesBaseStep(t, script, dir, tc.tag)
			if err != nil {
				t.Fatalf("step failed: %v\n%s", err, output)
			}
			want := "GORELEASER_CURRENT_TAG=" + tc.tag + "\n"
			if tc.want != "" {
				want += "GORELEASER_PREVIOUS_TAG=" + tc.want + "\n"
			}
			if githubEnv != want {
				t.Errorf("GITHUB_ENV = %q, want %q\n%s", githubEnv, want, output)
			}
			if tc.goreleaserDefault != "" && strings.Contains(githubEnv, tc.goreleaserDefault) {
				t.Errorf("GITHUB_ENV still points at the leftover pre-release %s", tc.goreleaserDefault)
			}
			if tc.currentDefault != "" && strings.Contains(githubEnv, "CURRENT_TAG="+tc.currentDefault) {
				t.Errorf("GORELEASER_CURRENT_TAG is the RC %s, not the GA %s", tc.currentDefault, tc.tag)
			}
		})
	}
}

// TestReleaseCurrentTagIsGAWhenGAAndRCShareACommit is the scenario
// `make bump-promote` creates: the GA tag lands on the RC's own commit. Left to
// itself GoReleaser would name the RC the current tag, build the GA run as the
// RC, and check it against the RC release that already exists. The step must
// pin the current tag to the GA tag whichever pre-release it shares the commit
// with.
func TestReleaseCurrentTagIsGAWhenGAAndRCShareACommit(t *testing.T) {
	t.Parallel()
	script := notesBaseScript(t)

	cases := []struct {
		name    string
		history [][]string
		tag     string
	}{
		{
			name:    "one RC shares the commit",
			history: [][]string{{"v1.2.3"}, {"v1.3.0-rc1"}, {"v1.3.0-rc2", "v1.3.0"}},
			tag:     "v1.3.0",
		},
		{
			name:    "several pre-release kinds share the commit",
			history: [][]string{{"v1.2.3"}, {"v1.3.0-beta.1", "v1.3.0-rc1", "v1.3.0-rc2", "v1.3.0"}},
			tag:     "v1.3.0",
		},
		{
			name:    "patch GA shares its commit with an RC",
			history: [][]string{{"v1.2.3"}, {"v1.2.4-rc1", "v1.2.4"}},
			tag:     "v1.2.4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := notesBaseRepo(t, tc.history)

			// Precondition: without the pin GoReleaser picks a pre-release.
			unpinned := notesBaseGoReleaserCurrentDefault(t, dir)
			if !strings.Contains(unpinned, "-") {
				t.Fatalf("fixture does not reproduce the bug: GoReleaser's own current tag = %q, want a pre-release", unpinned)
			}

			githubEnv, output, err := runNotesBaseStep(t, script, dir, tc.tag)
			if err != nil {
				t.Fatalf("step failed: %v\n%s", err, output)
			}
			if !strings.Contains(githubEnv, "GORELEASER_CURRENT_TAG="+tc.tag+"\n") {
				t.Errorf("GITHUB_ENV = %q, want GORELEASER_CURRENT_TAG=%s\n%s", githubEnv, tc.tag, output)
			}
			if strings.Contains(githubEnv, "CURRENT_TAG="+unpinned) {
				t.Errorf("GORELEASER_CURRENT_TAG is the pre-release %s that GoReleaser would pick on its own", unpinned)
			}
		})
	}
}

// TestReleaseNotesBaseExportsOnlyValidTags keeps the step from exporting a
// malformed previous tag into GITHUB_ENV. Tag names are attacker-influenced input to
// a workflow that holds write and signing permissions.
func TestReleaseNotesBaseExportsOnlyValidTags(t *testing.T) {
	t.Parallel()
	script := notesBaseScript(t)

	// `v1.2.3+meta` is stable by the "no hyphen" rule and matches the git
	// `v[0-9]*` glob, but is not a plain SemVer release tag.
	dir := notesBaseRepo(t, [][]string{
		{"v1.2.3+meta"},
		{"v1.3.0"},
	})
	githubEnv, output, err := runNotesBaseStep(t, script, dir, "v1.3.0")
	if err == nil {
		t.Fatalf("step accepted a non-SemVer base tag; GITHUB_ENV = %q\n%s", githubEnv, output)
	}
	if strings.Contains(githubEnv, "GORELEASER_PREVIOUS_TAG=") {
		t.Errorf("GITHUB_ENV = %q, want no previous tag exported for a rejected tag", githubEnv)
	}
}

func notesBaseScript(t *testing.T) string {
	t.Helper()
	doc := loadYAML(t, ".github/workflows/on-tag.yaml")
	job := mapValue(t, mapValue(t, doc, "jobs"), "build-ko")
	steps := sliceValue(t, job, "steps")
	index := stepIndex(steps, notesBaseStepName)
	if index < 0 {
		t.Fatalf("build-ko must have a %q step", notesBaseStepName)
	}
	return stringValue(t, steps[index].(map[string]any), "run")
}

// notesBaseRepo builds a repository whose history is `history`, oldest first,
// creating the listed annotated tags (what tools/bump creates) at each commit.
func notesBaseRepo(t *testing.T, history [][]string) string {
	t.Helper()
	dir := t.TempDir()
	notesBaseGit(t, dir, "init", "-q")
	for index, tags := range history {
		path := filepath.Join(dir, "file.txt")
		if err := os.WriteFile(path, fmt.Appendf(nil, "commit %d\n", index), 0o600); err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
		notesBaseGit(t, dir, "add", "file.txt")
		notesBaseGit(t, dir, "commit", "-q", "-m", fmt.Sprintf("commit %d", index))
		for _, tag := range tags {
			notesBaseGit(t, dir, "tag", "-a", tag, "-m", "Release "+tag)
		}
	}
	return dir
}

func notesBaseGitEnv() []string {
	return append(os.Environ(),
		// Keep host git configuration (signing, hooks, templates) out of the
		// fixtures so they behave the same on a laptop and in CI.
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=release-notes-test",
		"GIT_AUTHOR_EMAIL=release-notes-test@example.invalid",
		"GIT_COMMITTER_NAME=release-notes-test",
		"GIT_COMMITTER_EMAIL=release-notes-test@example.invalid",
	)
}

func notesBaseGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), notesBaseStepTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = notesBaseGitEnv()
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// runNotesBaseStep executes the extracted step the way the Actions runner does
// (`bash -e -o pipefail`) with TAG and GITHUB_ENV set. It returns the contents
// the step appended to GITHUB_ENV, its combined output, and its run error.
func runNotesBaseStep(t *testing.T, script, repo, tag string) (string, string, error) {
	t.Helper()
	work := t.TempDir()
	scriptPath := filepath.Join(work, "step.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatalf("stage extracted step: %v", err)
	}
	envPath := filepath.Join(work, "github_env")
	if err := os.WriteFile(envPath, nil, 0o600); err != nil {
		t.Fatalf("create GITHUB_ENV: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), notesBaseStepTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-e", "-o", "pipefail", scriptPath)
	command.Dir = repo
	command.Env = append(notesBaseGitEnv(), "TAG="+tag, "GITHUB_ENV="+envPath)
	combined, runErr := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("step exceeded the test deadline: %v\n%s", ctx.Err(), combined)
	}
	exported, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read GITHUB_ENV: %v", err)
	}
	return string(exported), string(combined), runErr
}
