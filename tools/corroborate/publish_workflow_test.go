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

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// shaPinned matches a third-party action ref pinned to a full 40-hex commit SHA
// (repo[/path]@<sha>), the repo norm — a tag or branch ref must NOT match.
var shaPinned = regexp.MustCompile(`@[0-9a-fA-F]{40}$`)

// workflowPath is the GP5 Pages publish workflow relative to this package.
const workflowPath = "../../.github/workflows/evidence-dashboard-publish.yaml"

// siteDir is the generator output dir the workflow publishes as the Pages
// artifact; the build step's -out and upload-pages-artifact path must agree.
const siteDir = "_site"

// publishWorkflow is the subset of the workflow YAML the security-critical
// invariants are asserted against. The full file is linted for syntax by the
// repo actionlint gate (merge-gate.yaml); this test pins the contract the
// linter cannot express: the publish identity is read-only and the Pages
// permissions are scoped to the job that needs them.
type publishWorkflow struct {
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	If          string            `yaml:"if"`
	Permissions map[string]string `yaml:"permissions"`
	Environment struct {
		Name string `yaml:"name"`
	} `yaml:"environment"`
	Env   map[string]string `yaml:"env"`
	Steps []workflowStep    `yaml:"steps"`
}

type workflowStep struct {
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

func loadPublishWorkflow(t *testing.T) publishWorkflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(workflowPath))
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	var wf publishWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	return wf
}

// job returns the named job or fails the test if it is absent.
func (wf publishWorkflow) job(t *testing.T, name string) workflowJob {
	t.Helper()
	j, ok := wf.Jobs[name]
	if !ok {
		t.Fatalf("missing %s job", name)
	}
	return j
}

// TestPublishWorkflowPagesPermissions asserts the deploy job declares the
// Pages write + OIDC permissions the deploy-pages action requires, and that
// the top-level default stays least-privilege (contents: read).
func TestPublishWorkflowPagesPermissions(t *testing.T) {
	wf := loadPublishWorkflow(t)

	// Exact maps, not selected keys: an accidentally-added scope (e.g. a stray
	// contents: write or packages: write) must fail the least-privilege split.
	if want := (map[string]string{"contents": "read"}); !reflect.DeepEqual(wf.Permissions, want) {
		t.Errorf("top-level permissions = %v, want %v", wf.Permissions, want)
	}

	build := wf.job(t, "build")
	if want := (map[string]string{"contents": "read", "id-token": "write"}); !reflect.DeepEqual(build.Permissions, want) {
		t.Errorf("build job permissions = %v, want %v", build.Permissions, want)
	}

	deploy := wf.job(t, "deploy")
	if want := (map[string]string{"pages": "write", "id-token": "write"}); !reflect.DeepEqual(deploy.Permissions, want) {
		t.Errorf("deploy job permissions = %v, want %v", deploy.Permissions, want)
	}
	if deploy.Environment.Name != "github-pages" {
		t.Errorf("deploy environment = %q, want github-pages", deploy.Environment.Name)
	}
}

// TestPublishWorkflowUsesPagesActions asserts the canonical Pages publish
// chain is present and the build job does not silently hold pages: write.
func TestPublishWorkflowUsesPagesActions(t *testing.T) {
	wf := loadPublishWorkflow(t)

	// Map each third-party action's repo path to its full uses ref so we can
	// assert both presence and that it is pinned to a commit SHA (not a tag or
	// branch, which are mutable). Local ./ actions are exempt.
	uses := map[string]string{}
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if step.Uses == "" || strings.HasPrefix(step.Uses, "./") {
				continue
			}
			uses[strings.SplitN(step.Uses, "@", 2)[0]] = step.Uses
		}
	}
	for _, want := range []string{
		"actions/configure-pages",
		"actions/upload-pages-artifact",
		"actions/deploy-pages",
	} {
		ref, ok := uses[want]
		if !ok {
			t.Errorf("workflow does not use %s", want)
			continue
		}
		if !shaPinned.MatchString(ref) {
			t.Errorf("%s must be pinned to a full commit SHA, got %q", want, ref)
		}
	}

	build := wf.job(t, "build")

	// The build job carries GCS read creds and must NOT also hold pages: write.
	if _, has := build.Permissions["pages"]; has {
		t.Error("build job must not declare pages permission (separation of duties)")
	}

	// The generator must write to exactly the dir upload-pages-artifact
	// publishes, so the deployed site is the freshly generated one.
	var genWritesSite bool
	var artifactPath string
	for _, step := range build.Steps {
		if strings.Contains(step.Run, "./bin/corroborate -in") && stepWritesTo(step.Run, siteDir) {
			genWritesSite = true
		}
		if strings.HasPrefix(step.Uses, "actions/upload-pages-artifact@") {
			artifactPath = step.With["path"]
		}
	}
	if !genWritesSite {
		t.Errorf("generator must write to %s", siteDir)
	}
	if artifactPath != siteDir {
		t.Errorf("upload-pages-artifact path = %q, want generator output %q", artifactPath, siteDir)
	}
}

// stepWritesTo reports whether a run script passes `-out <dir>` as an exact
// token (so `_site` does not spuriously match `_site_check`).
func stepWritesTo(run, dir string) bool {
	fields := strings.Fields(run)
	for i, f := range fields {
		if f == "-out" && i+1 < len(fields) && fields[i+1] == dir {
			return true
		}
	}
	return false
}

// TestPublishWorkflowReadOnlyIdentity asserts the build job authenticates to
// GCS with a read-only identity — not the GP2 write SA, not the shared
// project-wide SA.
func TestPublishWorkflowReadOnlyIdentity(t *testing.T) {
	wf := loadPublishWorkflow(t)

	build := wf.job(t, "build")
	sa := build.Env["GCS_READ_SERVICE_ACCOUNT"]
	if sa == "" {
		t.Fatal("build job missing GCS_READ_SERVICE_ACCOUNT env")
	}
	// Must be the dedicated read SA.
	if !strings.Contains(sa, "evidence-read@") {
		t.Errorf("read SA = %q, want an evidence-read@ identity", sa)
	}
	// The shared project-wide SA and the GP2 publish SA are forbidden here:
	// a Pages publish must never run with write/admin GCS scope.
	for _, forbidden := range []string{
		"github-actions@eidosx",
		"evidence-publish@eidosx",
	} {
		if strings.Contains(sa, forbidden) {
			t.Errorf("read SA %q must not reference the privileged identity %q", sa, forbidden)
		}
	}

	// The auth step must impersonate exactly this env var, not a literal.
	var authed bool
	for _, step := range build.Steps {
		if strings.HasPrefix(step.Uses, "google-github-actions/auth@") {
			authed = true
			if got := step.With["service_account"]; got != "${{ env.GCS_READ_SERVICE_ACCOUNT }}" {
				t.Errorf("auth service_account = %q, want ${{ env.GCS_READ_SERVICE_ACCOUNT }}", got)
			}
		}
	}
	if !authed {
		t.Error("build job has no google-github-actions/auth step")
	}
}

// TestPublishWorkflowDeterminismGate asserts the in-job determinism gate is
// wired: the generator runs twice and the outputs are diffed so a
// non-reproducible build fails loudly instead of publishing.
func TestPublishWorkflowDeterminismGate(t *testing.T) {
	wf := loadPublishWorkflow(t)

	build := wf.job(t, "build")
	var gen string
	for _, step := range build.Steps {
		if strings.Contains(step.Run, "corroborate") {
			gen += step.Run
		}
	}
	if strings.Count(gen, "./bin/corroborate -in") < 2 {
		t.Error("determinism gate requires two generator builds")
	}
	if !strings.Contains(gen, "diff -r") {
		t.Error("determinism gate must diff the two builds")
	}
}

// TestPublishWorkflowForkSafety asserts every job is gated to the canonical
// repo so a fork never obtains GCS creds or Pages write.
func TestPublishWorkflowForkSafety(t *testing.T) {
	wf := loadPublishWorkflow(t)

	for name, job := range wf.Jobs {
		if !strings.Contains(job.If, "github.repository == 'nvidia/aicr'") {
			t.Errorf("job %q missing canonical-repo guard (fork safety)", name)
		}
	}
}
