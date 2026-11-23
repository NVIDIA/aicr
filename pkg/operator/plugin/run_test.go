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

package plugin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "aicr.run/api/operator/v1alpha1"
	"aicr.run/pkg/operator/plan"
)

// The commands end to end against a real API server, as a certificate user
// whose name the server reports: no username is ever typed.
func TestRunAgainstAnAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("KUBEBUILDER_ASSETS is not set")
		}
		t.Skip("KUBEBUILDER_ASSETS is not set (run `make test`)")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "charts", "aicr-operator", "files", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err == nil {
		err = v1alpha1.AddToScheme(scheme)
	}
	if err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	if err = admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "aicr-system"}}); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	ro := &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "rollout-plugin-run", Labels: map[string]string{v1alpha1.LabelRuntime: v1alpha1.SingletonName}},
		Spec: v1alpha1.RuntimeRolloutSpec{
			SchemaVersion: v1alpha1.RolloutSchemaVersion, RuntimeUID: "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0",
			ToPlanDigest: digest, StartingInventoryDigest: digest, ApprovalRequired: true,
			Deadline:      metav1.NewTime(time.Now().Add(time.Hour).Truncate(time.Second)),
			ActionTimeout: metav1.Duration{Duration: time.Minute},
			Steps:         []v1alpha1.RolloutStep{{Release: "nfd", Operation: v1alpha1.OperationInstall, Assessment: v1alpha1.AssessmentNotApplicable}},
		},
	}
	if err = admin.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	user, err := env.AddUser(envtest.User{Name: "carol", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := user.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err = os.WriteFile(kubeconfig, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"describe", ro.Name, "--kubeconfig", kubeconfig}, "required, pending"},
		// Without --expires the approval lasts until the rollout's deadline.
		{[]string{"approve", ro.Name, "--kubeconfig", kubeconfig, "--reason", "window"},
			"approved by carol until " + ro.Spec.Deadline.UTC().Format(time.RFC3339)},
		{[]string{"revoke", ro.Name, "--reason", "incident", "--kubeconfig", kubeconfig}, "revoked by carol"},
		{[]string{"describe", "--kubeconfig", kubeconfig, ro.Name}, "revoked by carol"},
	} {
		var out bytes.Buffer
		if err = Run(ctx, c.args, &out); err != nil || !strings.Contains(out.String(), c.want) {
			t.Fatalf("Run(%q) = %v\n%s", c.args, err, out.String())
		}
	}
	if err = Run(ctx, []string{"approve", "rollout-missing", "--kubeconfig", kubeconfig, "--reason", "x"}, &bytes.Buffer{}); err == nil {
		t.Fatal("approving a missing rollout succeeded")
	}

	// Acknowledging a record that names no apply step covers all of its
	// steps, and the output says so; without the plan to map the release to
	// its component, the output claims neither.
	doc, objs := testPlan(t, `{}`, nil)
	reportOf := func(steps string) *plan.Content {
		return plan.AssessmentContent([]byte(`{"deployer":"argocd","objectNamesCompared":true,"components":[` +
			`{"component":"cert-manager","change":"version","verdict":"manual","steps":[` + steps + `]}],"summary":{}}`))
	}
	noApply := reportOf(`{"id":"widen","description":"w"},{"id":"sync","description":"s"}`)
	withApply := reportOf(`{"id":"check","description":"c"},{"id":"apply","description":"a"},{"id":"widen","description":"w"},{"id":"sync","description":"s"}`)
	for _, report := range []*plan.Content{noApply, withApply} {
		cm, cmErr := report.ConfigMap(testNS, v1alpha1.SingletonName, doc.RuntimeUID, nil)
		if cmErr != nil {
			t.Fatal(cmErr)
		}
		objs = append(objs, cm)
	}
	for _, o := range objs {
		if err = admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, toPlan string
		report       *plan.Content
		want         string
	}{
		{"rollout-plugin-acknowledge", doc.DesiredStateDigest, noApply,
			"carol acknowledged all steps of cert-manager's record (widen, sync), which names no apply step; the rollout continues"},
		{"rollout-plugin-acknowledge-apply", doc.DesiredStateDigest, withApply,
			"carol acknowledged the post-apply steps of cert-manager (widen, sync); the rollout continues"},
		{"rollout-plugin-acknowledge-no-plan", "sha256:" + strings.Repeat("0", 64), noApply,
			"carol acknowledged the steps cert-manager's record holds (widen, sync); the rollout continues"},
	} {
		held := ro.DeepCopy()
		held.ObjectMeta = metav1.ObjectMeta{Name: tc.name, Labels: ro.Labels}
		held.Spec.ToPlanDigest, held.Spec.StartingInventoryDigest = tc.toPlan, tc.toPlan
		held.Spec.AssessmentRef = &v1alpha1.DigestedConfigMapRef{ConfigMap: tc.report.Name, Digest: tc.report.Digest}
		held.Spec.Steps = []v1alpha1.RolloutStep{{Release: "cert-manager", Operation: v1alpha1.OperationUpgrade,
			Assessment: v1alpha1.AssessmentManual, PostApplySteps: []string{"widen", "sync"}}}
		if err = admin.Create(ctx, held); err != nil {
			t.Fatal(err)
		}
		if err = Run(ctx, []string{"approve", held.Name, "--kubeconfig", kubeconfig, "--reason", "window"}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if err = admin.Get(ctx, client.ObjectKeyFromObject(held), held); err != nil {
			t.Fatal(err)
		}
		held.Status.Phase = v1alpha1.PhaseAwaitingAcknowledgement
		held.Status.Releases = []v1alpha1.ReleaseStatus{{ID: "cert-manager", State: v1alpha1.ReleaseReady}}
		if err = admin.Status().Update(ctx, held); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err = Run(ctx, []string{"acknowledge", held.Name, "--kubeconfig", kubeconfig}, &out); err != nil || !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%s: acknowledge = %v\n%s\nwant %q", tc.name, err, out.String(), tc.want)
		}
	}
}
