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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

const (
	testValidatorSA = "admin-validator"
	testValidatorNS = "aicr-validation"
)

// validatorSAPath is the request path the validator ServiceAccount is read at.
var validatorSAPath = "/api/v1/namespaces/" + testValidatorNS + "/serviceaccounts/" + testValidatorSA

// newScriptedAPIServer is recordingAPIServer with the responses chosen per
// request instead of always failing, so a run can be let through the
// ServiceAccount check and stopped at a later, known request.
func newScriptedAPIServer(t *testing.T, respond func(method, path string) (int, string)) *recordingAPIServer {
	t.Helper()

	rec := &recordingAPIServer{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests = append(rec.requests, r.Method+" "+r.URL.Path)
		rec.mu.Unlock()
		code, body := respond(r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

const failureStatus = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"scripted apiserver"}`

// provisionedCluster answers as a cluster where an administrator created the
// validator ServiceAccount and bound it to cluster-admin, and fails every
// other request.
func provisionedCluster(method, path string) (int, string) {
	switch {
	case method == http.MethodGet && path == validatorSAPath:
		return http.StatusOK, `{"kind":"ServiceAccount","apiVersion":"v1","metadata":{"name":"` +
			testValidatorSA + `","namespace":"` + testValidatorNS + `"}}`
	case method == http.MethodPost && path == "/apis/authorization.k8s.io/v1/subjectaccessreviews":
		return http.StatusCreated, `{"kind":"SubjectAccessReview","apiVersion":"authorization.k8s.io/v1","status":{"allowed":true}}`
	default:
		return http.StatusInternalServerError, failureStatus
	}
}

// isNamespaceOrRBACPath reports whether path addresses a Namespace object or
// one of the five RBAC kinds, the objects an existing-ServiceAccount run must
// never write.
func isNamespaceOrRBACPath(path string) bool {
	if path == "/api/v1/namespaces" || path == "/api/v1/namespaces/"+testValidatorNS {
		return true
	}
	for _, resource := range []string{"/serviceaccounts", "/roles", "/rolebindings", "/clusterroles", "/clusterrolebindings"} {
		if strings.HasSuffix(path, resource) || strings.Contains(path, resource+"/") {
			return true
		}
	}
	return false
}

// writeValidateInputs writes the recipe and snapshot files the runs below
// validate and returns their paths.
func writeValidateInputs(t *testing.T, dir string) (recipePath, snapshotPath string) {
	t.Helper()
	recipePath = filepath.Join(dir, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte(skipPreflightRecipeYAML), 0o600); err != nil {
		t.Fatalf("failed to write test recipe file: %v", err)
	}
	snapshotPath = filepath.Join(dir, "snapshot.yaml")
	snapshotYAML := "kind: Snapshot\napiVersion: aicr.run/v1\nmetadata:\n  version: test\nmeasurements:\n  - type: K8s\n"
	if err := os.WriteFile(snapshotPath, []byte(snapshotYAML), 0o600); err != nil {
		t.Fatalf("failed to write test snapshot file: %v", err)
	}
	return recipePath, snapshotPath
}

// TestValidateCmd_MissingValidatorServiceAccountFailsBeforeSnapshotCapture
// pins that a missing validator ServiceAccount is reported before the
// live-capture branch deploys the snapshot agent, whose Job and RBAC would
// otherwise be created first. The only request the run may make is the read
// that finds the ServiceAccount missing. The name arrives by flag or by
// --config, so both resolutions are covered.
//
// The control is the same run with no name: it must reach the agent, or the
// "only one request" assertion would hold for a harness that never reaches
// the cluster at all.
func TestValidateCmd_MissingValidatorServiceAccountFailsBeforeSnapshotCapture(t *testing.T) {
	tests := []struct {
		name       string
		flag       bool
		config     bool
		wantAgents bool
	}{
		{name: "named by flag", flag: true},
		{name: "named by config", config: true},
		{name: "control: no name reaches the agent", wantAgents: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			rec := newScriptedAPIServer(t, func(method, path string) (int, string) {
				if method == http.MethodGet && path == validatorSAPath {
					return http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
				}
				return http.StatusInternalServerError, failureStatus
			})
			kubeconfig := rec.writeKubeconfig(t, tmp)
			t.Setenv("KUBECONFIG", kubeconfig)
			recipePath, _ := writeValidateInputs(t, tmp)

			args := []string{"aicr", "validate", "--recipe", recipePath, "--kubeconfig", kubeconfig}
			if tt.flag {
				args = append(args, "--"+flagValidatorSA, testValidatorSA)
			}
			if tt.config {
				configPath := filepath.Join(tmp, "aicr.yaml")
				configYAML := "kind: AICRConfig\napiVersion: aicr.run/v1beta1\nspec:\n  validate:\n    agent:\n" +
					"      validatorServiceAccountName: " + testValidatorSA + "\n"
				if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
					t.Fatalf("failed to write config: %v", err)
				}
				args = append(args, "--config", configPath)
			}

			err := RootCommand().Run(t.Context(), args)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}

			seen := rec.seen()
			if tt.wantAgents {
				if slices.Equal(seen, []string{"GET " + validatorSAPath}) || len(seen) == 0 {
					t.Fatalf("control run made requests %v; it must reach the snapshot agent", seen)
				}
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeNotFound)
			if want := []string{"GET " + validatorSAPath}; !slices.Equal(seen, want) {
				t.Errorf("requests = %v, want only %v: nothing may be created before the missing ServiceAccount is reported",
					seen, want)
			}
		})
	}
}

// TestValidateCmd_ValidatorServiceAccountWritesNoRBAC drives a full
// `aicr validate --snapshot` run against a cluster where the named
// ServiceAccount is provisioned and bound, and stops it at the first data
// ConfigMap. Reaching that ConfigMap with no namespace or RBAC write on the way
// is what proves the name reached ValidateState and not only the CLI's own
// pre-flight: per-run mode would have applied the namespace first.
//
// The control, the same run with no name, must write the namespace, so the
// detector is known to see such a write.
func TestValidateCmd_ValidatorServiceAccountWritesNoRBAC(t *testing.T) {
	for _, named := range []bool{true, false} {
		name := "named"
		if !named {
			name = "control: no name applies the namespace"
		}
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			rec := newScriptedAPIServer(t, provisionedCluster)
			kubeconfig := rec.writeKubeconfig(t, tmp)
			t.Setenv("KUBECONFIG", kubeconfig)
			recipePath, snapshotPath := writeValidateInputs(t, tmp)

			args := []string{"aicr", "validate", "--recipe", recipePath, "--snapshot", snapshotPath,
				"--kubeconfig", kubeconfig}
			if named {
				args = append(args, "--"+flagValidatorSA, testValidatorSA)
			}
			if err := RootCommand().Run(t.Context(), args); err == nil {
				t.Fatal("expected the scripted apiserver to fail the run, got nil")
			}

			seen := rec.seen()
			var writes []string
			reachedConfigMaps := false
			for _, req := range seen {
				method, path, _ := strings.Cut(req, " ")
				if method == http.MethodPost && strings.HasSuffix(path, "/configmaps") {
					reachedConfigMaps = true
				}
				if method == http.MethodGet {
					continue
				}
				if isNamespaceOrRBACPath(path) {
					writes = append(writes, req)
				}
			}

			if !named {
				if len(writes) == 0 {
					t.Fatalf("control run wrote no namespace or RBAC object (requests %v); the detector is inert", seen)
				}
				return
			}
			if len(writes) > 0 {
				t.Errorf("run wrote %v; with an existing validator ServiceAccount it must write no namespace or RBAC object",
					writes)
			}
			if !reachedConfigMaps {
				t.Errorf("run never reached the data ConfigMaps (requests %v); it stopped before validation preparation", seen)
			}
		})
	}
}
