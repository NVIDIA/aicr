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
	"context"
	stderrors "errors"
	"reflect"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const provisionSA = "irsa-snapshotter"

// seedServiceAccount creates the target ServiceAccount provisioning requires.
func seedServiceAccount(ctx context.Context, t *testing.T, clientset *fake.Clientset, namespace, name string) {
	t.Helper()
	if _, err := clientset.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding ServiceAccount: %v", err)
	}
}

// TestProvisionServiceAccountRoles_Names pins the naming scheme, because its
// only real requirement is structural: a provisioned name must never be
// mistakable for a run-scoped one. Every run-scoped name ends in a run ID
// whose final segment is 16 lowercase-hex characters, and these end in
// "-rbac" — "r" is not a hex digit, so the two name spaces are disjoint by
// construction rather than by luck.
func TestProvisionServiceAccountRoles_Names(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewClientset()
	seedServiceAccount(ctx, t, clientset, testNamespace, provisionSA)

	res, err := ProvisionServiceAccountRoles(ctx, clientset, ProvisionOptions{
		Namespace:          testNamespace,
		ServiceAccountName: provisionSA,
	})
	if err != nil {
		t.Fatalf("ProvisionServiceAccountRoles() error = %v", err)
	}

	wantRole := "aicr-agent-" + provisionSA + "-rbac"
	wantClusterRole := "aicr-agent-" + testNamespace + "-" + provisionSA + "-rbac"
	if res.Role != wantRole || res.RoleBinding != wantRole {
		t.Errorf("Role/RoleBinding = %q/%q, want %q", res.Role, res.RoleBinding, wantRole)
	}
	if res.ClusterRole != wantClusterRole || res.ClusterRoleBinding != wantClusterRole {
		t.Errorf("ClusterRole/ClusterRoleBinding = %q/%q, want %q", res.ClusterRole, res.ClusterRoleBinding, wantClusterRole)
	}

	// A run-scoped name is "<prefix>-<runID>". Nothing provisioned may be
	// one, whatever prefix a caller supplies.
	for _, name := range []string{res.Role, res.RoleBinding, res.ClusterRole, res.ClusterRoleBinding} {
		if strings.HasSuffix(name, "-"+testRunID) {
			t.Errorf("provisioned name %q collides with the run-scoped name space", name)
		}
		if !strings.HasSuffix(name, provisionedNameSuffix) {
			t.Errorf("provisioned name %q does not carry the %q suffix that keeps it out of the run-scoped name space", name, provisionedNameSuffix)
		}
	}
}

// TestProvisionServiceAccountRoles_ObjectsArePermanentAndUnscoped asserts the
// property the whole feature rests on: nothing provisioned carries a run ID,
// so no run's cleanup can ever reclaim it. Deployer.createdByThisRun requires
// the run-ID label, and these objects deliberately have none.
func TestProvisionServiceAccountRoles_ObjectsArePermanentAndUnscoped(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewClientset()
	seedServiceAccount(ctx, t, clientset, testNamespace, provisionSA)

	res, err := ProvisionServiceAccountRoles(ctx, clientset, ProvisionOptions{
		Namespace:          testNamespace,
		ServiceAccountName: provisionSA,
	})
	if err != nil {
		t.Fatalf("ProvisionServiceAccountRoles() error = %v", err)
	}

	role, err := clientset.RbacV1().Roles(testNamespace).Get(ctx, res.Role, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Role not created: %v", err)
	}
	cr, err := clientset.RbacV1().ClusterRoles().Get(ctx, res.ClusterRole, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ClusterRole not created: %v", err)
	}

	for name, got := range map[string]map[string]string{res.Role: role.Labels, res.ClusterRole: cr.Labels} {
		if _, ok := got[labels.RunID]; ok {
			t.Errorf("%s carries the %s label; a run's cleanup could reclaim it", name, labels.RunID)
		}
		if got[labels.Component] != labels.ValueAgentRBAC {
			t.Errorf("%s component label = %q, want %q", name, got[labels.Component], labels.ValueAgentRBAC)
		}
		if got[labels.Component] == labels.ValueSnapshotAgent {
			t.Errorf("%s is labeled as a run-scoped snapshot-agent object", name)
		}
	}

	// A Deployer must not be able to claim these as its own, whatever run
	// ID it holds.
	d := NewDeployer(clientset, Config{Namespace: testNamespace, RunID: testRunID})
	if d.createdByThisRun(role.Labels) || d.createdByThisRun(cr.Labels) {
		t.Error("createdByThisRun matched a provisioned object; run cleanup would delete a permanent grant")
	}

	// The bindings must point at the operator's ServiceAccount.
	rb, err := clientset.RbacV1().RoleBindings(testNamespace).Get(ctx, res.RoleBinding, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("RoleBinding not created: %v", err)
	}
	crb, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, res.ClusterRoleBinding, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ClusterRoleBinding not created: %v", err)
	}
	want := []rbacv1.Subject{{Kind: kindServiceAccount, Name: provisionSA, Namespace: testNamespace}}
	if !reflect.DeepEqual(rb.Subjects, want) {
		t.Errorf("RoleBinding subjects = %v, want %v", rb.Subjects, want)
	}
	if !reflect.DeepEqual(crb.Subjects, want) {
		t.Errorf("ClusterRoleBinding subjects = %v, want %v", crb.Subjects, want)
	}
}

// TestProvisionServiceAccountRoles_DiscoverNetworkRules covers both rule sets:
// the read-only baseline, and the baseline plus the cluster-scoped mutating
// rules live discovery needs. The distinction matters because a provisioned
// grant is permanent — a --discover-network provisioning leaves nodes:patch
// and pods/exec:create on the ServiceAccount indefinitely.
func TestProvisionServiceAccountRoles_DiscoverNetworkRules(t *testing.T) {
	tests := []struct {
		name            string
		discoverNetwork bool
		wantMutating    bool
	}{
		{name: "read-only baseline", discoverNetwork: false, wantMutating: false},
		{name: "discovery adds the mutating rules", discoverNetwork: true, wantMutating: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			clientset := fake.NewClientset()
			seedServiceAccount(ctx, t, clientset, testNamespace, provisionSA)

			res, err := ProvisionServiceAccountRoles(ctx, clientset, ProvisionOptions{
				Namespace:          testNamespace,
				ServiceAccountName: provisionSA,
				DiscoverNetwork:    tt.discoverNetwork,
			})
			if err != nil {
				t.Fatalf("ProvisionServiceAccountRoles() error = %v", err)
			}
			if res.DiscoverNetwork != tt.discoverNetwork {
				t.Errorf("result DiscoverNetwork = %v, want %v", res.DiscoverNetwork, tt.discoverNetwork)
			}

			cr, err := clientset.RbacV1().ClusterRoles().Get(ctx, res.ClusterRole, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("ClusterRole not created: %v", err)
			}
			if got := hasRule(cr.Rules, "", "nodes", "patch"); got != tt.wantMutating {
				t.Errorf("nodes:patch granted = %v, want %v", got, tt.wantMutating)
			}
			if got := hasRule(cr.Rules, "", "pods/exec", verbCreate); got != tt.wantMutating {
				t.Errorf("pods/exec:create granted = %v, want %v", got, tt.wantMutating)
			}
			// The baseline read-only rules are present either way.
			if !hasRule(cr.Rules, "", "nodes", verbList) {
				t.Error("baseline nodes:list rule missing")
			}

			// The provisioned ClusterRole must carry exactly what a
			// run-scoped one would, so an adopted ServiceAccount is not
			// quietly less capable than a run-owned one.
			if !reflect.DeepEqual(cr.Rules, clusterRules(tt.discoverNetwork)) {
				t.Error("provisioned ClusterRole rules differ from the run-scoped agent's")
			}
			role, err := clientset.RbacV1().Roles(testNamespace).Get(ctx, res.Role, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Role not created: %v", err)
			}
			if !reflect.DeepEqual(role.Rules, namespacedRules()) {
				t.Error("provisioned Role rules differ from the run-scoped agent's")
			}
		})
	}
}

// TestProvisionServiceAccountRoles_Idempotent re-runs provisioning over a
// stale rule set and asserts the rules are refreshed in place rather than the
// call failing or the stale grant surviving. This is the aicr-upgrade path:
// an operator re-runs the command and expects the current rules.
func TestProvisionServiceAccountRoles_Idempotent(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewClientset()
	seedServiceAccount(ctx, t, clientset, testNamespace, provisionSA)

	opts := ProvisionOptions{Namespace: testNamespace, ServiceAccountName: provisionSA, DiscoverNetwork: true}
	res, err := ProvisionServiceAccountRoles(ctx, clientset, opts)
	if err != nil {
		t.Fatalf("first ProvisionServiceAccountRoles() error = %v", err)
	}

	// Simulate a stale grant left by an older aicr: strip the rules from
	// both roles. A merely-idempotent implementation that skipped existing
	// objects would leave them stripped.
	stale, err := clientset.RbacV1().ClusterRoles().Get(ctx, res.ClusterRole, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading ClusterRole: %v", err)
	}
	stale.Rules = nil
	if _, err = clientset.RbacV1().ClusterRoles().Update(ctx, stale, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("staling ClusterRole: %v", err)
	}

	res2, err := ProvisionServiceAccountRoles(ctx, clientset, opts)
	if err != nil {
		t.Fatalf("second ProvisionServiceAccountRoles() error = %v", err)
	}
	if !reflect.DeepEqual(res, res2) {
		t.Errorf("second result = %+v, want %+v (names must be deterministic)", res2, res)
	}

	refreshed, err := clientset.RbacV1().ClusterRoles().Get(ctx, res.ClusterRole, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading ClusterRole: %v", err)
	}
	if !reflect.DeepEqual(refreshed.Rules, clusterRules(true)) {
		t.Error("re-provisioning did not refresh the stale ClusterRole rules")
	}

	// Exactly one of each object, not a duplicate per run.
	crs, err := clientset.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing ClusterRoles: %v", err)
	}
	if len(crs.Items) != 1 {
		t.Errorf("ClusterRoles = %d, want 1", len(crs.Items))
	}
	roles, err := clientset.RbacV1().Roles(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing Roles: %v", err)
	}
	if len(roles.Items) != 1 {
		t.Errorf("Roles = %d, want 1", len(roles.Items))
	}
}

// TestProvisionServiceAccountRoles_Rejections covers every input the call
// refuses before writing anything — most importantly a ServiceAccount that
// does not exist, which must be ErrCodeNotFound rather than four dangling
// objects bound to nothing.
func TestProvisionServiceAccountRoles_Rejections(t *testing.T) {
	tests := []struct {
		name      string
		seed      string
		opts      ProvisionOptions
		wantCode  aicrerrors.ErrorCode
		wantInMsg string
	}{
		{
			name:      "missing ServiceAccount",
			opts:      ProvisionOptions{Namespace: testNamespace, ServiceAccountName: provisionSA},
			wantCode:  aicrerrors.ErrCodeNotFound,
			wantInMsg: "not found in namespace",
		},
		{
			name:     "empty namespace",
			opts:     ProvisionOptions{ServiceAccountName: provisionSA},
			wantCode: aicrerrors.ErrCodeInvalidRequest,
		},
		{
			name:     "empty ServiceAccount name",
			opts:     ProvisionOptions{Namespace: testNamespace},
			wantCode: aicrerrors.ErrCodeInvalidRequest,
		},
		{
			name:     "whitespace ServiceAccount name",
			opts:     ProvisionOptions{Namespace: testNamespace, ServiceAccountName: "   "},
			wantCode: aicrerrors.ErrCodeInvalidRequest,
		},
		{
			name:      "name too long to compose",
			seed:      strings.Repeat("a", 250),
			opts:      ProvisionOptions{Namespace: testNamespace, ServiceAccountName: strings.Repeat("a", 250)},
			wantCode:  aicrerrors.ErrCodeInvalidRequest,
			wantInMsg: "not a valid Kubernetes object name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			clientset := fake.NewClientset()
			if tt.seed != "" {
				seedServiceAccount(ctx, t, clientset, testNamespace, tt.seed)
			}

			_, err := ProvisionServiceAccountRoles(ctx, clientset, tt.opts)
			if err == nil {
				t.Fatal("ProvisionServiceAccountRoles() error = nil, want an error")
			}
			if !stderrors.Is(err, aicrerrors.New(tt.wantCode, "")) {
				t.Errorf("error = %v, want code %s", err, tt.wantCode)
			}
			if tt.wantInMsg != "" && !strings.Contains(err.Error(), tt.wantInMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantInMsg)
			}

			// Nothing may be written on a rejected call.
			crs, listErr := clientset.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{})
			if listErr != nil {
				t.Fatalf("listing ClusterRoles: %v", listErr)
			}
			if len(crs.Items) != 0 {
				t.Errorf("ClusterRoles = %d, want 0 (a rejected call must write nothing)", len(crs.Items))
			}
		})
	}
}

// TestProvisionServiceAccountRoles_RefusesToRetargetAnotherSubject covers the
// one way the cluster-scoped name can be ambiguous: it joins namespace and
// ServiceAccount with "-", so ("a-b", "c") and ("a", "b-c") compose the same
// name. Silently updating the binding would revoke the first ServiceAccount's
// cluster grants, so the second provisioning must fail closed.
func TestProvisionServiceAccountRoles_RefusesToRetargetAnotherSubject(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewClientset()
	seedServiceAccount(ctx, t, clientset, "a-b", "c")
	seedServiceAccount(ctx, t, clientset, "a", "b-c")

	first, err := ProvisionServiceAccountRoles(ctx, clientset, ProvisionOptions{Namespace: "a-b", ServiceAccountName: "c"})
	if err != nil {
		t.Fatalf("first ProvisionServiceAccountRoles() error = %v", err)
	}

	_, err = ProvisionServiceAccountRoles(ctx, clientset, ProvisionOptions{Namespace: "a", ServiceAccountName: "b-c"})
	if err == nil {
		t.Fatal("second ProvisionServiceAccountRoles() error = nil, want a conflict")
	}
	if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeConflict, "")) {
		t.Errorf("error = %v, want ErrCodeConflict", err)
	}

	// The first ServiceAccount keeps its grant.
	crb, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, first.ClusterRoleBinding, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading ClusterRoleBinding: %v", err)
	}
	want := []rbacv1.Subject{{Kind: kindServiceAccount, Name: "c", Namespace: "a-b"}}
	if !reflect.DeepEqual(crb.Subjects, want) {
		t.Errorf("ClusterRoleBinding subjects = %v, want %v (the first grant must survive)", crb.Subjects, want)
	}
}

// hasRule reports whether rules grant verb on resource in apiGroup.
func hasRule(rules []rbacv1.PolicyRule, apiGroup, resource, verb string) bool {
	for _, r := range rules {
		if !contains(r.APIGroups, apiGroup) || !contains(r.Resources, resource) || !contains(r.Verbs, verb) {
			continue
		}
		return true
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
