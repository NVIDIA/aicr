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

package snapshotter

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const (
	provisionNamespace = "gpu-operator"
	provisionSAName    = "irsa-snapshotter"
	provisionRunID     = "20260821-142233-9f3a1c0b7e2d4a55"
)

// writeRoles renders into a scratch working directory and returns the result.
// It pins RunID so the directory name is deterministic, and t.Chdir keeps the
// output out of the repository — the call writes to the working directory by
// design.
func writeRoles(t *testing.T, config *AgentRolesConfig) *AgentRolesResult {
	t.Helper()
	t.Chdir(t.TempDir())
	res, err := WriteAgentRoleManifests(config)
	if err != nil {
		t.Fatalf("WriteAgentRoleManifests() error = %v", err)
	}
	return res
}

// defaultRolesConfig is the standard input: a read-only grant with a pinned
// run ID.
func defaultRolesConfig() *AgentRolesConfig {
	return &AgentRolesConfig{
		Namespace:          provisionNamespace,
		ServiceAccountName: provisionSAName,
		RunID:              provisionRunID,
	}
}

// TestWriteAgentRoleManifests_WritesReviewableDirectory covers the layout the
// documented workflow depends on: a `snapshot-rbac-<runID>` directory holding
// one parseable object per file, which is what makes both
// `kubectl apply -f <dir>/` and the `kubectl delete -f <dir>/` teardown work.
func TestWriteAgentRoleManifests_WritesReviewableDirectory(t *testing.T) {
	res := writeRoles(t, defaultRolesConfig())

	wantDir := "snapshot-rbac-" + provisionRunID
	if res.Dir != wantDir {
		t.Errorf("Dir = %q, want %q", res.Dir, wantDir)
	}
	if res.RunID != provisionRunID {
		t.Errorf("RunID = %q, want %q", res.RunID, provisionRunID)
	}

	entries, err := os.ReadDir(res.Dir)
	if err != nil {
		t.Fatalf("reading %s: %v", res.Dir, err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	want := []string{"01-role.yaml", "02-rolebinding.yaml", "03-clusterrole.yaml", "04-clusterrolebinding.yaml"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("directory contents = %v, want %v", got, want)
	}

	if len(res.Objects) != len(want) {
		t.Fatalf("Objects = %d, want %d", len(res.Objects), len(want))
	}
	wantKinds := []string{"Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding"}
	for i, obj := range res.Objects {
		if obj.Kind != wantKinds[i] {
			t.Errorf("Objects[%d].Kind = %q, want %q", i, obj.Kind, wantKinds[i])
		}
		if obj.Path != filepath.Join(wantDir, want[i]) {
			t.Errorf("Objects[%d].Path = %q, want %q", i, obj.Path, filepath.Join(wantDir, want[i]))
		}
		body, readErr := os.ReadFile(obj.Path)
		if readErr != nil {
			t.Fatalf("reading %s: %v", obj.Path, readErr)
		}
		if !strings.HasPrefix(string(body), "# ") {
			t.Errorf("%s does not open with a YAML comment header", obj.Path)
		}
		var parsed map[string]any
		if unmarshalErr := yaml.Unmarshal(body, &parsed); unmarshalErr != nil {
			t.Errorf("%s is not parseable YAML: %v", obj.Path, unmarshalErr)
		}
		if parsed["kind"] != obj.Kind {
			t.Errorf("%s kind = %v, want %q", obj.Path, parsed["kind"], obj.Kind)
		}
		if parsed["metadata"] == nil {
			t.Errorf("%s has no metadata; the comment header may have swallowed the object", obj.Path)
		}
	}
}

// TestWriteAgentRoleManifests_DiscoverNetwork asserts the flag is what decides
// whether the written ClusterRole carries the mutating discovery rules, and
// that the plain form carries none of them.
func TestWriteAgentRoleManifests_DiscoverNetwork(t *testing.T) {
	tests := []struct {
		name            string
		discoverNetwork bool
		wantMutating    bool
	}{
		{name: "plain grant stays read-only", discoverNetwork: false},
		{name: "discovery grant adds the mutating rules", discoverNetwork: true, wantMutating: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := defaultRolesConfig()
			config.DiscoverNetwork = tt.discoverNetwork
			res := writeRoles(t, config)

			if res.DiscoverNetwork != tt.discoverNetwork {
				t.Errorf("DiscoverNetwork = %v, want %v", res.DiscoverNetwork, tt.discoverNetwork)
			}

			body, err := os.ReadFile(filepath.Join(res.Dir, "03-clusterrole.yaml"))
			if err != nil {
				t.Fatalf("reading the ClusterRole: %v", err)
			}
			cr := &rbacv1.ClusterRole{}
			if unmarshalErr := yaml.Unmarshal(body, cr); unmarshalErr != nil {
				t.Fatalf("unmarshalling the ClusterRole: %v", unmarshalErr)
			}

			mutating := map[string]string{"nodes": "patch", "pods/exec": "create", "customresourcedefinitions": "create"}
			for resource, verb := range mutating {
				if got := clusterRoleGrants(cr, resource, verb); got != tt.wantMutating {
					t.Errorf("%s: %s = %v, want %v", resource, verb, got, tt.wantMutating)
				}
			}

			// The header must explain the mutating rules, not merely carry
			// them: reading the file is how an operator consents to them.
			if strings.Contains(string(body), "nodes: patch") != tt.wantMutating {
				t.Errorf("header explains nodes: patch = %v, want %v", !tt.wantMutating, tt.wantMutating)
			}
		})
	}
}

// clusterRoleGrants reports whether cr grants verb on resource.
func clusterRoleGrants(cr *rbacv1.ClusterRole, resource, verb string) bool {
	for _, rule := range cr.Rules {
		for _, res := range rule.Resources {
			if res != resource {
				continue
			}
			for _, v := range rule.Verbs {
				if v == verb {
					return true
				}
			}
		}
	}
	return false
}

// TestWriteAgentRoleManifests_ExistingDirectory asserts a collision fails with
// a structured conflict instead of overwriting. The manifests an operator is
// midway through reviewing are exactly what must not change under them.
func TestWriteAgentRoleManifests_ExistingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := "snapshot-rbac-" + provisionRunID
	if mkErr := os.Mkdir(dir, 0o700); mkErr != nil {
		t.Fatalf("seeding the directory: %v", mkErr)
	}
	sentinel := filepath.Join(dir, "01-role.yaml")
	if seedErr := os.WriteFile(sentinel, []byte("# operator is reading this\n"), 0o600); seedErr != nil {
		t.Fatalf("seeding a file: %v", seedErr)
	}

	res, err := WriteAgentRoleManifests(defaultRolesConfig())
	if err == nil {
		t.Fatal("WriteAgentRoleManifests() error = nil, want ErrCodeConflict")
	}
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeConflict, "")) {
		t.Errorf("error = %v, want code %s", err, errors.ErrCodeConflict)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error = %q, want it to name the directory %q", err.Error(), dir)
	}

	body, readErr := os.ReadFile(sentinel)
	if readErr != nil {
		t.Fatalf("reading the seeded file: %v", readErr)
	}
	if string(body) != "# operator is reading this\n" {
		t.Errorf("the existing file was overwritten; got %q", body)
	}
}

// TestWriteAgentRoleManifests_GeneratesRunIDWhenUnset covers the normal path,
// where the run ID comes from the shared generator rather than the caller.
func TestWriteAgentRoleManifests_GeneratesRunIDWhenUnset(t *testing.T) {
	config := defaultRolesConfig()
	config.RunID = ""
	res := writeRoles(t, config)

	if res.RunID == "" {
		t.Fatal("RunID = \"\", want a generated run ID")
	}
	if !strings.HasPrefix(res.Dir, "snapshot-rbac-") {
		t.Errorf("Dir = %q, want the snapshot-rbac- prefix", res.Dir)
	}
	if res.Dir != "snapshot-rbac-"+res.RunID {
		t.Errorf("Dir = %q, want it built from RunID %q", res.Dir, res.RunID)
	}
	if _, err := os.Stat(res.Dir); err != nil {
		t.Errorf("generated directory %q not created: %v", res.Dir, err)
	}
}

// TestWriteAgentRoleManifests_NoClusterAccess is the load-bearing test for
// this path: it must work with no kubeconfig and no cluster at all. KUBECONFIG
// is pointed at a file that cannot yield a client and the in-cluster
// environment is cleared, so any attempt to build a clientset or read the
// ServiceAccount would fail the call rather than silently pass.
//
// It also pins the deliberate simplification: a ServiceAccount that does not
// exist is no longer an ErrCodeNotFound. Nothing is consulted that could
// know, and the operator reviews the manifests before applying them.
func TestWriteAgentRoleManifests_NoClusterAccess(t *testing.T) {
	unreachable := filepath.Join(t.TempDir(), "not-a-kubeconfig")
	if seedErr := os.WriteFile(unreachable, []byte("this is not a kubeconfig\n"), 0o600); seedErr != nil {
		t.Fatalf("seeding the kubeconfig: %v", seedErr)
	}
	t.Setenv("KUBECONFIG", unreachable)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	config := defaultRolesConfig()
	config.ServiceAccountName = "no-such-serviceaccount"
	res := writeRoles(t, config)

	if len(res.Objects) != 4 {
		t.Fatalf("Objects = %d, want 4 written without any cluster access", len(res.Objects))
	}
	body, err := os.ReadFile(res.Objects[1].Path)
	if err != nil {
		t.Fatalf("reading the RoleBinding: %v", err)
	}
	if !strings.Contains(string(body), "no-such-serviceaccount") {
		t.Errorf("RoleBinding does not name the unverified ServiceAccount:\n%s", body)
	}
}

// TestWriteAgentRoleManifests_Rejections covers every input refused before
// anything reaches the filesystem, so a bad value never leaves a directory
// behind that would block the corrected retry.
func TestWriteAgentRoleManifests_Rejections(t *testing.T) {
	tests := []struct {
		name   string
		config *AgentRolesConfig
	}{
		{name: "nil config", config: nil},
		{name: "empty namespace", config: &AgentRolesConfig{ServiceAccountName: provisionSAName}},
		{name: "whitespace namespace", config: &AgentRolesConfig{Namespace: "  ", ServiceAccountName: provisionSAName}},
		{name: "empty ServiceAccount name", config: &AgentRolesConfig{Namespace: provisionNamespace}},
		{name: "whitespace ServiceAccount name", config: &AgentRolesConfig{Namespace: provisionNamespace, ServiceAccountName: " "}},
		{
			name: "name too long to compose",
			config: &AgentRolesConfig{
				Namespace:          provisionNamespace,
				ServiceAccountName: strings.Repeat("a", 250),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			res, err := WriteAgentRoleManifests(tt.config)
			if err == nil {
				t.Fatal("WriteAgentRoleManifests() error = nil, want ErrCodeInvalidRequest")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code %s", err, errors.ErrCodeInvalidRequest)
			}
			if res != nil {
				t.Errorf("result = %+v, want nil", res)
			}

			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatalf("reading the working directory: %v", readErr)
			}
			if len(entries) != 0 {
				t.Errorf("working directory has %d entries, want 0 (a rejected call must write nothing)", len(entries))
			}
		})
	}
}
