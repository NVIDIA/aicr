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

package agent

import (
	stderrors "errors"
	"reflect"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const provisionSA = "irsa-snapshotter"

// buildManifests renders the four manifests for the standard test inputs and
// fails the test on any error.
func buildManifests(t *testing.T, discoverNetwork bool) []Manifest {
	t.Helper()
	manifests, err := BuildServiceAccountRoleManifests(ManifestOptions{
		Namespace:          testNamespace,
		ServiceAccountName: provisionSA,
		DiscoverNetwork:    discoverNetwork,
	})
	if err != nil {
		t.Fatalf("BuildServiceAccountRoleManifests() error = %v", err)
	}
	return manifests
}

// manifestByFile indexes rendered manifests by file name.
func manifestByFile(manifests []Manifest) map[string]Manifest {
	byFile := make(map[string]Manifest, len(manifests))
	for _, m := range manifests {
		byFile[m.FileName] = m
	}
	return byFile
}

// TestProvisionedClusterRoleNameIsInjective pins the property the "." join
// exists for. These two (namespace, ServiceAccount) pairs compose the same
// string under a "-" join, and the collision is not academic: the second
// render applied over the first retargets the live ClusterRoleBinding and
// revokes the first ServiceAccount's cluster permissions. Nothing in the
// generator can detect it, because it reads no cluster.
//
// A namespace is a DNS-1123 label and cannot contain ".", so the first dot
// always separates the two segments. Switching the join back to "-" fails
// this test.
func TestProvisionedClusterRoleNameIsInjective(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		sa        string
	}{
		{"hyphen in the namespace", "a-b", "c"},
		{"hyphen in the ServiceAccount", "a", "b-c"},
		{"dot in the ServiceAccount is still unambiguous", "a", "b.c"},
	}

	seen := make(map[string]string, len(tests))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := provisionedClusterRoleName(tt.namespace, tt.sa)
			if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
				t.Errorf("%q is not a valid ClusterRole name: %v", got, errs)
			}
			if prior, ok := seen[got]; ok {
				t.Errorf("%s/%s collides with %s on name %q",
					tt.namespace, tt.sa, prior, got)
			}
			seen[got] = tt.namespace + "/" + tt.sa
		})
	}
}

// TestBuildServiceAccountRoleManifests_FilesAndNames pins the file layout and
// the naming scheme together, because both are contracts an operator's
// commands depend on: `kubectl apply -f <dir>/` relies on one parseable
// object per file in an order that puts a Role ahead of its RoleBinding, and
// a rendered name must never be mistakable for a run-scoped one. Every
// run-scoped name ends in a run ID whose final segment is 16 lowercase-hex
// characters, and these end in "-rbac" — "r" is not a hex digit, so the two
// name spaces are disjoint by construction rather than by luck.
func TestBuildServiceAccountRoleManifests_FilesAndNames(t *testing.T) {
	manifests := buildManifests(t, false)

	wantRole := "aicr-agent-" + provisionSA + "-rbac"
	wantClusterRole := "aicr-agent-" + testNamespace + "." + provisionSA + "-rbac"

	want := []struct {
		file string
		kind string
		name string
	}{
		{roleFileName, kindRole, wantRole},
		{roleBindingFileName, kindRoleBinding, wantRole},
		{clusterRoleFileName, kindClusterRole, wantClusterRole},
		{clusterRoleBindingFileName, kindClusterRoleBinding, wantClusterRole},
	}
	if len(manifests) != len(want) {
		t.Fatalf("manifests = %d, want %d (one file per object)", len(manifests), len(want))
	}
	for i, w := range want {
		got := manifests[i]
		if got.FileName != w.file {
			t.Errorf("manifest[%d].FileName = %q, want %q (lexical order must apply a Role before its binding)", i, got.FileName, w.file)
		}
		if got.Kind != w.kind {
			t.Errorf("manifest[%d].Kind = %q, want %q", i, got.Kind, w.kind)
		}
		if got.Name != w.name {
			t.Errorf("manifest[%d].Name = %q, want %q", i, got.Name, w.name)
		}
		if !strings.HasSuffix(got.Name, "-rbac") {
			t.Errorf("manifest[%d].Name = %q, want a name ending in -rbac so it cannot collide with a run-scoped name", i, got.Name)
		}
	}
}

// TestBuildServiceAccountRoleManifests_ParseableYAML asserts each file
// round-trips through a YAML decoder into the typed object it claims to be,
// with apiVersion and kind set. A typed object straight out of client-go
// leaves TypeMeta empty, which `kubectl apply` rejects, so this is the check
// that the files are usable at all.
func TestBuildServiceAccountRoleManifests_ParseableYAML(t *testing.T) {
	byFile := manifestByFile(buildManifests(t, false))
	wantAPIVersion := rbacv1.SchemeGroupVersion.String()

	tests := []struct {
		name     string
		file     string
		wantKind string
		into     func() any
		check    func(t *testing.T, obj any)
	}{
		{
			name:     "role carries the namespaced rules",
			file:     roleFileName,
			wantKind: kindRole,
			into:     func() any { return &rbacv1.Role{} },
			check: func(t *testing.T, obj any) {
				t.Helper()
				role, ok := obj.(*rbacv1.Role)
				if !ok {
					t.Fatalf("decoded %T, want *rbacv1.Role", obj)
				}
				if role.Namespace != testNamespace {
					t.Errorf("Role namespace = %q, want %q", role.Namespace, testNamespace)
				}
				if !reflect.DeepEqual(role.Rules, namespacedRules()) {
					t.Errorf("Role rules drifted from namespacedRules(); got %+v", role.Rules)
				}
			},
		},
		{
			name:     "rolebinding names the ServiceAccount subject",
			file:     roleBindingFileName,
			wantKind: kindRoleBinding,
			into:     func() any { return &rbacv1.RoleBinding{} },
			check: func(t *testing.T, obj any) {
				t.Helper()
				rb, ok := obj.(*rbacv1.RoleBinding)
				if !ok {
					t.Fatalf("decoded %T, want *rbacv1.RoleBinding", obj)
				}
				wantSubjects := []rbacv1.Subject{{Kind: kindServiceAccount, Name: provisionSA, Namespace: testNamespace}}
				if !reflect.DeepEqual(rb.Subjects, wantSubjects) {
					t.Errorf("RoleBinding subjects = %+v, want %+v", rb.Subjects, wantSubjects)
				}
				if rb.RoleRef.Kind != kindRole {
					t.Errorf("RoleBinding roleRef kind = %q, want %q", rb.RoleRef.Kind, kindRole)
				}
			},
		},
		{
			name:     "clusterrole carries the cluster rules and no namespace",
			file:     clusterRoleFileName,
			wantKind: kindClusterRole,
			into:     func() any { return &rbacv1.ClusterRole{} },
			check: func(t *testing.T, obj any) {
				t.Helper()
				cr, ok := obj.(*rbacv1.ClusterRole)
				if !ok {
					t.Fatalf("decoded %T, want *rbacv1.ClusterRole", obj)
				}
				if cr.Namespace != "" {
					t.Errorf("ClusterRole namespace = %q, want empty (it is cluster-scoped)", cr.Namespace)
				}
				if !reflect.DeepEqual(cr.Rules, clusterRules(false)) {
					t.Errorf("ClusterRole rules drifted from clusterRules(false); got %+v", cr.Rules)
				}
			},
		},
		{
			name:     "clusterrolebinding binds the clusterrole",
			file:     clusterRoleBindingFileName,
			wantKind: kindClusterRoleBinding,
			into:     func() any { return &rbacv1.ClusterRoleBinding{} },
			check: func(t *testing.T, obj any) {
				t.Helper()
				crb, ok := obj.(*rbacv1.ClusterRoleBinding)
				if !ok {
					t.Fatalf("decoded %T, want *rbacv1.ClusterRoleBinding", obj)
				}
				if crb.RoleRef.Kind != kindClusterRole {
					t.Errorf("ClusterRoleBinding roleRef kind = %q, want %q", crb.RoleRef.Kind, kindClusterRole)
				}
				if got := crb.Labels[labels.Component]; got != labels.ValueAgentRBAC {
					t.Errorf("component label = %q, want %q", got, labels.ValueAgentRBAC)
				}
				if _, ok := crb.Labels[labels.RunID]; ok {
					t.Error("rendered object carries a run-ID label; these objects belong to no run")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := byFile[tt.file]
			if !ok {
				t.Fatalf("no manifest rendered for %s", tt.file)
			}
			obj := tt.into()
			if err := yaml.Unmarshal(m.Content, obj); err != nil {
				t.Fatalf("unmarshalling %s: %v\n%s", tt.file, err, m.Content)
			}
			var apiVersion, kind string
			switch o := obj.(type) {
			case *rbacv1.Role:
				apiVersion, kind = o.APIVersion, o.Kind
			case *rbacv1.RoleBinding:
				apiVersion, kind = o.APIVersion, o.Kind
			case *rbacv1.ClusterRole:
				apiVersion, kind = o.APIVersion, o.Kind
			case *rbacv1.ClusterRoleBinding:
				apiVersion, kind = o.APIVersion, o.Kind
			}
			if apiVersion != wantAPIVersion {
				t.Errorf("%s apiVersion = %q, want %q", tt.file, apiVersion, wantAPIVersion)
			}
			if kind != tt.wantKind {
				t.Errorf("%s kind = %q, want %q", tt.file, kind, tt.wantKind)
			}
			tt.check(t, obj)
		})
	}
}

// TestBuildServiceAccountRoleManifests_DiscoverNetwork covers both rule sets.
// The default grant must stay read-only, and the --discover-network grant
// must carry the mutating rules AND explain them in the header — the header
// is what an operator reads to decide whether to apply the file at all, so a
// silent "nodes: patch" would defeat the point of writing manifests out
// instead of applying them.
func TestBuildServiceAccountRoleManifests_DiscoverNetwork(t *testing.T) {
	tests := []struct {
		name            string
		discoverNetwork bool
		wantRuleCount   int
		wantMutating    bool
	}{
		{name: "default grant is read-only", discoverNetwork: false, wantRuleCount: len(clusterRules(false))},
		{name: "discovery grant adds the mutating rules", discoverNetwork: true, wantRuleCount: len(clusterRules(true)), wantMutating: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := manifestByFile(buildManifests(t, tt.discoverNetwork))[clusterRoleFileName]
			if !ok {
				t.Fatalf("no ClusterRole manifest rendered")
			}
			cr := &rbacv1.ClusterRole{}
			if err := yaml.Unmarshal(m.Content, cr); err != nil {
				t.Fatalf("unmarshalling ClusterRole: %v", err)
			}
			if len(cr.Rules) != tt.wantRuleCount {
				t.Errorf("ClusterRole rules = %d, want %d", len(cr.Rules), tt.wantRuleCount)
			}
			for _, want := range []struct{ resource, verb string }{
				{"nodes", "patch"},
				{"pods/exec", verbCreate},
				{"customresourcedefinitions", verbCreate},
			} {
				if got := hasRule(cr.Rules, want.resource, want.verb); got != tt.wantMutating {
					t.Errorf("%s: %s present = %v, want %v", want.resource, want.verb, got, tt.wantMutating)
				}
			}

			header := string(m.Content)
			for _, want := range []string{"nodes: patch", "pods/exec: create", "MUTATING"} {
				if strings.Contains(header, want) != tt.wantMutating {
					t.Errorf("header mentions %q = %v, want %v; header:\n%s", want, !tt.wantMutating, tt.wantMutating, header)
				}
			}
			if !tt.wantMutating && !strings.Contains(header, "READ-ONLY") {
				t.Errorf("read-only grant does not say so in the header:\n%s", header)
			}
		})
	}
}

// hasRule reports whether any rule grants verb on resource.
func hasRule(rules []rbacv1.PolicyRule, resource, verb string) bool {
	for _, r := range rules {
		for _, res := range r.Resources {
			if res != resource {
				continue
			}
			for _, v := range r.Verbs {
				if v == verb {
					return true
				}
			}
		}
	}
	return false
}

// TestBuildServiceAccountRoleManifests_HeadersExplainTheGrant asserts every
// file states, in its own header, that nothing was applied and how to apply
// and revoke. An operator may open exactly one of these files, and the fact
// they must not miss is that the grant is not live yet.
func TestBuildServiceAccountRoleManifests_HeadersExplainTheGrant(t *testing.T) {
	wantEverywhere := []string{
		"NOTHING HAS BEEN APPLIED",
		"kubectl apply  -f",
		"kubectl delete -f",
		provisionSA,
	}
	for _, m := range buildManifests(t, false) {
		body := string(m.Content)
		if !strings.HasPrefix(body, "# ") {
			t.Errorf("%s does not open with a YAML comment header; got %.40q", m.FileName, body)
		}
		for _, want := range wantEverywhere {
			if !strings.Contains(body, want) {
				t.Errorf("%s header does not contain %q", m.FileName, want)
			}
		}
	}
}

// TestBuildServiceAccountRoleManifests_Deterministic asserts two renders of
// the same inputs are byte-identical. The manifests are reviewed by hand and
// diffed against a previous grant, so incidental churn between runs would
// make a real change hard to spot.
func TestBuildServiceAccountRoleManifests_Deterministic(t *testing.T) {
	first := buildManifests(t, true)
	second := buildManifests(t, true)
	if len(first) != len(second) {
		t.Fatalf("manifest counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if string(first[i].Content) != string(second[i].Content) {
			t.Errorf("%s differs between renders:\n--- first ---\n%s\n--- second ---\n%s",
				first[i].FileName, first[i].Content, second[i].Content)
		}
	}
}

// TestBuildServiceAccountRoleManifests_Rejections covers every input the call
// refuses before rendering anything. A ServiceAccount that does not exist is
// deliberately NOT among them: rendering contacts no cluster, so a mistyped
// name yields manifests the operator inspects before applying.
func TestBuildServiceAccountRoleManifests_Rejections(t *testing.T) {
	tests := []struct {
		name      string
		opts      ManifestOptions
		wantInMsg string
	}{
		{
			name: "empty namespace",
			opts: ManifestOptions{ServiceAccountName: provisionSA},
		},
		{
			name: "whitespace namespace",
			opts: ManifestOptions{Namespace: "  ", ServiceAccountName: provisionSA},
		},
		{
			name: "empty ServiceAccount name",
			opts: ManifestOptions{Namespace: testNamespace},
		},
		{
			name: "whitespace ServiceAccount name",
			opts: ManifestOptions{Namespace: testNamespace, ServiceAccountName: "   "},
		},
		{
			name:      "name too long to compose",
			opts:      ManifestOptions{Namespace: testNamespace, ServiceAccountName: strings.Repeat("a", 250)},
			wantInMsg: "not a valid Kubernetes object name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifests, err := BuildServiceAccountRoleManifests(tt.opts)
			if err == nil {
				t.Fatal("BuildServiceAccountRoleManifests() error = nil, want ErrCodeInvalidRequest")
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code %s", err, aicrerrors.ErrCodeInvalidRequest)
			}
			if tt.wantInMsg != "" && !strings.Contains(err.Error(), tt.wantInMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantInMsg)
			}
			if manifests != nil {
				t.Errorf("manifests = %v, want nil on a rejected call", manifests)
			}
		})
	}
}

// TestBuildServiceAccountRoleManifests_UnknownServiceAccountStillRenders pins
// the deliberate simplification: the old provisioning path failed with
// ErrCodeNotFound when the ServiceAccount was absent. Rendering has no
// cluster to ask, so it renders regardless and the RoleBinding header tells
// the operator to check the name themselves.
func TestBuildServiceAccountRoleManifests_UnknownServiceAccountStillRenders(t *testing.T) {
	manifests, err := BuildServiceAccountRoleManifests(ManifestOptions{
		Namespace:          testNamespace,
		ServiceAccountName: "no-such-serviceaccount",
	})
	if err != nil {
		t.Fatalf("BuildServiceAccountRoleManifests() error = %v, want manifests for an unverified name", err)
	}
	if len(manifests) != 4 {
		t.Fatalf("manifests = %d, want 4", len(manifests))
	}
	rb, ok := manifestByFile(manifests)[roleBindingFileName]
	if !ok {
		t.Fatal("no RoleBinding manifest rendered")
	}
	if !strings.Contains(string(rb.Content), "kubectl get serviceaccount no-such-serviceaccount") {
		t.Errorf("RoleBinding header does not tell the operator how to verify the name:\n%s", rb.Content)
	}
}
