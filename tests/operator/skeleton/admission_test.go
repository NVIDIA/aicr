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

package skeleton

import (
	"context"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// chartPolicies are the admission policies the chart installs.
var chartPolicies = []string{
	"aicr-operator-applications", "aicr-operator-jobs", "aicr-operator-rollout-outcome", "aicr-operator-runtime",
	"aicr-operator-state", "aicr-operator-approval",
}

// admissionHolds checks the chart's policies against the real Argo CD CRDs
// and a cluster that aggregates RBAC: every expression type-checks, the
// operator's identity is denied a non-conforming Application and admitted
// its own, and a namespace admin holds none of the administrative surface.
// The envtest chart suite (tests/operator/chart) covers each rule.
func admissionHolds(ctx context.Context, t *testing.T) {
	for _, name := range chartPolicies {
		policy := &admissionv1.ValidatingAdmissionPolicy{}
		waitFor(t, time.Minute, "type checking of "+name, func() bool {
			return k8s.Get(ctx, client.ObjectKey{Name: name}, policy) == nil &&
				policy.Status.ObservedGeneration == policy.Generation && policy.Status.TypeChecking != nil
		})
		for _, w := range policy.Status.TypeChecking.ExpressionWarnings {
			t.Errorf("%s: %s: %s", name, w.FieldRef, w.Warning)
		}
	}

	cfg := rest.CopyConfig(restConfig)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + operatorNamespace + ":aicr-operator",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + operatorNamespace, "system:authenticated"},
	}
	operator, err := client.New(cfg, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	app, err := argocd.Application(argocd.Target{Namespace: "argocd", Project: "aicr", RuntimeName: v1alpha1.SingletonName},
		&plan.Document{RuntimeUID: "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0", DesiredStateDigest: digest},
		&plan.Release{ID: "admission-probe", Component: "admission-probe", Phase: plan.PhasePrimary, TargetNamespace: "monitoring",
			Chart: plan.Chart{Source: plan.ChartMirrored, Repository: "oci://" + registryHost + "/aicr/charts/admission-probe", Digest: digest}},
		[]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = operator.Create(ctx, app.DeepCopy(), client.DryRunAll); err != nil {
		t.Errorf("the operator's own Application is denied on the real CRD: %v", err)
	}
	tagged := app.DeepCopy()
	tagged.Object["spec"].(map[string]any)["source"].(map[string]any)["targetRevision"] = "v1.0.0"
	if err = operator.Create(ctx, tagged, client.DryRunAll); err == nil || !strings.Contains(err.Error(), "pin a sha256 digest") {
		t.Errorf("a tagged Application: %v, want the digest rule", err)
	}

	tenantNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "skeleton-tenant"}}
	if err = k8s.Create(ctx, tenantNS); client.IgnoreAlreadyExists(err) != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-admin", Namespace: tenantNS.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "Group", Name: "skeleton-tenants"}},
	}
	if err = k8s.Create(ctx, binding); client.IgnoreAlreadyExists(err) != nil {
		t.Fatal(err)
	}
	tenant := func(attrs authorizationv1.ResourceAttributes) bool {
		sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: "tenant", Groups: []string{"skeleton-tenants", "system:authenticated"}, ResourceAttributes: &attrs,
		}}
		if err := k8s.Create(ctx, sar); err != nil {
			t.Fatal(err)
		}
		return sar.Status.Allowed
	}
	// The authorizer sees a new binding through its informer, so the
	// tenant's own rights take a moment to appear; without them the
	// denials below prove nothing.
	waitFor(t, time.Minute, "the tenant to manage its namespace", func() bool {
		return tenant(authorizationv1.ResourceAttributes{Verb: "create", Group: "apps", Resource: "deployments", Namespace: tenantNS.Name})
	})
	group := v1alpha1.GroupVersion.Group
	for _, a := range []authorizationv1.ResourceAttributes{
		{Verb: "get", Group: group, Resource: "clusterruntimes"},
		{Verb: "update", Group: group, Resource: "clusterruntimes"},
		{Verb: "list", Group: group, Resource: "runtimerollouts"},
		{Verb: "update", Group: group, Resource: "runtimerollouts", Subresource: "approve"},
		{Verb: "get", Group: group, Resource: "runtimerollouts", Namespace: tenantNS.Name},
		{Verb: "create", Group: "argoproj.io", Resource: "applications", Namespace: tenantNS.Name},
		{Verb: "update", Group: "argoproj.io", Resource: "applications", Namespace: "argocd"},
		{Verb: "get", Resource: "configmaps", Namespace: operatorNamespace},
		{Verb: "create", Group: "batch", Resource: "jobs", Namespace: jobsNamespace},
		{Verb: "delete", Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicies"},
	} {
		if tenant(a) {
			t.Errorf("a namespace admin may %s %s/%s %s in %q", a.Verb, a.Group, a.Resource, a.Subresource, a.Namespace)
		}
	}
}
