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

package cli

import (
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// skipPreflightRecipeYAML is the same criteria-less RecipeResult that
// TestValidateCmd_KubeconfigSelectsValidationCluster uses: readiness stays a
// no-op against it while the inline gpu-operator override supplies the
// whole-GPU advertiser that validation-input construction requires. The recipe
// declares no checks, so the only skip-list defect either case below can hit is
// the unknown-name one, which is the defect under test.
const skipPreflightRecipeYAML = "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\n" +
	"metadata:\n  version: test\ncomponentRefs:\n  - name: gpu-operator\n    type: Helm\n" +
	"    source: https://helm.ngc.nvidia.com/nvidia\n    version: v25.10.0\n" +
	"    overrides:\n      devicePlugin:\n        enabled: true\n"

// recordingAPIServer is a stand-in apiserver that records every request it
// receives and fails each one. It is a real HTTP server rather than a fake
// clientset so the assertion lands at the process boundary: "the cluster was
// not touched" means no request left this process, which is exactly the promise
// the --skip-check help text makes. Every response is a 500 Status so the run
// that does reach the cluster fails immediately instead of waiting on a watch.
type recordingAPIServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string
}

func newRecordingAPIServer(t *testing.T) *recordingAPIServer {
	t.Helper()

	rec := &recordingAPIServer{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests = append(rec.requests, r.Method+" "+r.URL.Path)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"recording apiserver"}`))
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

// seen returns the requests recorded so far, as "METHOD /path" strings.
func (r *recordingAPIServer) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

// writeKubeconfig writes a kubeconfig pointing at the recording server into dir
// and returns its path.
func (r *recordingAPIServer) writeKubeconfig(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, "recording.kubeconfig")
	body := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: recording
  cluster:
    server: %s
contexts:
- name: recording
  context:
    cluster: recording
    user: recording
current-context: recording
users:
- name: recording
  user: {}
`, r.srv.URL)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("failed to write kubeconfig: %v", err)
	}
	return path
}

// TestValidateCmd_UnknownSkipCheckRejectedBeforeClusterIsTouched pins the
// promise the --skip-check help text makes: "Rejected before the cluster is
// touched when a name matches no check". The catalog-backed guard lives in
// pkg/validator and runs inside ValidatePhases, which is AFTER the Action's
// agent-deploy branch contacts the cluster to capture a snapshot. So a typo'd
// skip name used to pay for cluster contact (and, against a real cluster, a
// ServiceAccount, Role and Job) before the error surfaced.
//
// The bug this catches: moving the CLI-side preflight back below the
// snapshot/agent branch, or dropping it. Either way the unknown-name run
// reaches the apiserver and the recorded request list stops being empty.
//
// The known-name case is the control. Without it, "zero requests" would be
// satisfied by any harness that simply never talks to a cluster; with it, the
// same recipe, flags and server produce a non-empty request log, so the empty
// one in the first case is a property of the guard rather than of the setup.
//
// What this does NOT cover: the recording server fails the run at the
// permission pre-check, so the control never reaches the ServiceAccount, Role
// and Job creation calls themselves. The assertion made here is the stronger
// and simpler one, that no request reached the apiserver at all.
func TestValidateCmd_UnknownSkipCheckRejectedBeforeClusterIsTouched(t *testing.T) {
	tests := []struct {
		name          string
		skipCheck     string
		wantErrSubstr string
		wantRequests  bool
	}{
		{
			// The defect: an unknown name must be rejected against the
			// catalog before the Action reaches the agent-deploy branch.
			name:          "unknown skip name is rejected before any cluster request",
			skipCheck:     "no-such-check-typo",
			wantErrSubstr: `skipChecks entry "no-such-check-typo" matches no validator in the catalog`,
			wantRequests:  false,
		},
		{
			// Control: dra-support is in recipes/validators/catalog.yaml, so
			// the guard passes and the run proceeds into the agent-deploy
			// branch, where it fails on the recording server.
			name:          "known skip name proceeds to the cluster",
			skipCheck:     "dra-support",
			wantErrSubstr: "recording apiserver",
			wantRequests:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			rec := newRecordingAPIServer(t)
			kubeconfig := rec.writeKubeconfig(t, tmp)
			// Pinned so neither the explicit flag path nor default discovery
			// can reach a real cluster.
			t.Setenv("KUBECONFIG", kubeconfig)

			recipePath := filepath.Join(tmp, "recipe.yaml")
			if err := os.WriteFile(recipePath, []byte(skipPreflightRecipeYAML), 0o600); err != nil {
				t.Fatalf("failed to write test recipe file: %v", err)
			}

			// Driven through RootCommand rather than validateCmd so the
			// run goes through the same command tree `aicr validate` does.
			// No --snapshot: that is the branch that deploys the
			// snapshot-capture agent, the one that touches the cluster.
			err := RootCommand().Run(t.Context(), []string{"aicr", "validate",
				"--recipe", recipePath,
				"--skip-check", tt.skipCheck,
				"--kubeconfig", kubeconfig,
			})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErrSubstr)
			}

			seen := rec.seen()
			if tt.wantRequests && len(seen) == 0 {
				t.Errorf("control case recorded no apiserver requests; the harness cannot observe cluster contact, "+
					"so the zero-request assertion in the sibling case would prove nothing (skip-check %q)", tt.skipCheck)
			}
			if !tt.wantRequests && len(seen) != 0 {
				t.Errorf("the cluster was touched before the unknown skip name was rejected: %v", seen)
			}
		})
	}
}

// TestValidateCmd_UnknownSkipCheckIsInvalidRequest pins the error CODE of the
// rejection separately from its ordering. It is kept apart from the ordering
// test because a kubeconfig that cannot be built also yields
// ErrCodeInvalidRequest: the code alone does not discriminate this guard from
// the failure it replaced, so it is asserted here alongside the message rather
// than standing in for it.
func TestValidateCmd_UnknownSkipCheckIsInvalidRequest(t *testing.T) {
	tmp := t.TempDir()
	rec := newRecordingAPIServer(t)
	kubeconfig := rec.writeKubeconfig(t, tmp)
	t.Setenv("KUBECONFIG", kubeconfig)

	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte(skipPreflightRecipeYAML), 0o600); err != nil {
		t.Fatalf("failed to write test recipe file: %v", err)
	}

	err := RootCommand().Run(t.Context(), []string{"aicr", "validate",
		"--recipe", recipePath,
		"--skip-check", "no-such-check-typo",
		"--kubeconfig", kubeconfig,
	})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "matches no validator in the catalog") {
		t.Errorf("error = %v, want the catalog-backed rejection", err)
	}
}
