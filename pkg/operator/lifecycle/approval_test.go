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

package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

const (
	namespace = "aicr-system"
	digestA   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var (
	restConfig *rest.Config
	scheme     = runtime.NewScheme()
	admin      client.Client
)

// TestMain runs against a real API server: approvals are merge patches the
// CRD's set-once rules judge, and WhoAmI is a SelfSubjectReview.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set; lifecycle tests cannot run")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "skipping lifecycle tests: KUBEBUILDER_ASSETS is not set (run `make test`)")
		os.Exit(0)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "charts", "aicr-operator", "files", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		os.Exit(1)
	}
	restConfig = cfg
	if err = clientgoscheme.AddToScheme(scheme); err == nil {
		err = v1alpha1.AddToScheme(scheme)
	}
	if err == nil {
		admin, err = client.New(cfg, client.Options{Scheme: scheme})
	}
	if err == nil {
		err = admin.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	}
	code := 1
	if err != nil {
		fmt.Fprintf(os.Stderr, "set up: %v\n", err)
	} else {
		code = m.Run()
	}
	_ = env.Stop()
	os.Exit(code)
}

func as(t *testing.T, user string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(restConfig)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:masters", "system:authenticated"}}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func newRollout(t *testing.T, ctx context.Context, name string, mutate func(*v1alpha1.RuntimeRollout)) *v1alpha1.RuntimeRollout {
	t.Helper()
	ro := &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{v1alpha1.LabelRuntime: v1alpha1.SingletonName}},
		Spec: v1alpha1.RuntimeRolloutSpec{
			SchemaVersion: v1alpha1.RolloutSchemaVersion, RuntimeUID: "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0",
			FromPlanDigest: digestA, ToPlanDigest: digestB, StartingInventoryDigest: digestA,
			Deadline:         metav1.NewTime(time.Now().Add(6 * time.Hour).Truncate(time.Second)),
			ActionTimeout:    metav1.Duration{Duration: 30 * time.Minute},
			ApprovalRequired: true,
			Steps: []v1alpha1.RolloutStep{
				{Release: "gpu-operator", Operation: v1alpha1.OperationUpgrade, Assessment: v1alpha1.AssessmentManual, CRDChange: true},
				{Release: "gpu-operator-readiness", Operation: v1alpha1.OperationVerify, Assessment: v1alpha1.AssessmentNotApplicable},
			},
		},
	}
	if mutate != nil {
		mutate(ro)
	}
	if err := admin.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Delete(context.WithoutCancel(ctx), ro) })
	return ro
}

// The approver's username comes from the API server, never from the user.
func TestApproveAndRevokeAsTheRequester(t *testing.T) {
	ctx := testContext(t)
	ro := newRollout(t, ctx, "rollout-approve-test", nil)
	alice, bob := as(t, "alice"), as(t, "bob")
	user, err := WhoAmI(ctx, alice)
	if err != nil || user != "alice" {
		t.Fatalf("WhoAmI() = %q, %v", user, err)
	}
	expires := time.Now().Add(2 * time.Hour)
	if _, err = Approve(ctx, alice, ro.Name, user, "", expires); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("an approval without a reason: %v", err)
	}
	got, err := Approve(ctx, alice, ro.Name, user, "maintenance window", expires)
	if err != nil || got.Spec.Approval.ApprovedBy != "alice" || got.Spec.Approval.ExpiresAt.Unix() != expires.Truncate(time.Second).Unix() {
		t.Fatalf("Approve() = %+v, %v", got, err)
	}
	_, err = Approve(ctx, bob, ro.Name, "bob", "again", expires)
	if operrors.CodeOf(err) != operrors.ErrCodeConflict || !strings.Contains(err.Error(), "already approved by alice") {
		t.Fatalf("a second approval: %v", err)
	}
	got, err = Revoke(ctx, bob, ro.Name, "bob", "training job not checkpointed", time.Now())
	if err != nil || got.Spec.Approval.RevokedBy != "bob" || got.Spec.Approval.ApprovedBy != "alice" {
		t.Fatalf("Revoke() = %+v, %v", got, err)
	}
	if _, err = Revoke(ctx, alice, ro.Name, "alice", "again", time.Now()); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("a second revocation: %v", err)
	}
}

// Acknowledging names the release the rollout is paused on, as the
// requester, and only once it is paused there.
func TestAcknowledgeThePausedRelease(t *testing.T) {
	ctx := testContext(t)
	ro := newRollout(t, ctx, "rollout-acknowledge-test", func(ro *v1alpha1.RuntimeRollout) {
		ro.Spec.Steps[0].PostApplySteps = []string{"reset-breaker", "verify"}
	})
	alice, bob := as(t, "alice"), as(t, "bob")
	if _, err := Approve(ctx, alice, ro.Name, "alice", "window", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Acknowledge(ctx, bob, ro.Name, "bob", time.Now()); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("acknowledging before the pause: %v", err)
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(ro), ro); err != nil {
		t.Fatal(err)
	}
	ro.Status.Phase = v1alpha1.PhaseAwaitingAcknowledgement
	ro.Status.Releases = []v1alpha1.ReleaseStatus{{ID: "gpu-operator", State: v1alpha1.ReleaseReady}}
	if err := admin.Status().Update(ctx, ro); err != nil {
		t.Fatal(err)
	}
	got, step, err := Acknowledge(ctx, bob, ro.Name, "bob", time.Now())
	if err != nil || step.Release != "gpu-operator" || len(got.Spec.Approval.Acknowledgements) != 1 ||
		got.Spec.Approval.Acknowledgements[0].AcknowledgedBy != "bob" {

		t.Fatalf("Acknowledge() = %+v, %+v, %v", got.Spec.Approval, step, err)
	}
	if _, _, err = Acknowledge(ctx, bob, ro.Name, "bob", time.Now()); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("a second acknowledgement: %v", err)
	}
}

// Without an expiry an approval lasts until the rollout's deadline, so it
// never ends before the rollout can; a rollout past its deadline is refused.
func TestApproveDefaultsToTheDeadline(t *testing.T) {
	ctx := testContext(t)
	tests := []struct {
		name     string
		deadline time.Duration
		code     operrors.ErrorCode
	}{
		{"deadline ahead", 6 * time.Hour, ""},
		{"deadline passed", -time.Minute, operrors.ErrCodeConflict},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ro := newRollout(t, ctx, fmt.Sprintf("rollout-deadline-test-%d", i), func(ro *v1alpha1.RuntimeRollout) {
				ro.Spec.Deadline = metav1.NewTime(time.Now().Add(tt.deadline).Truncate(time.Second))
			})
			got, err := Approve(ctx, as(t, "alice"), ro.Name, "alice", "window", time.Time{})
			if tt.code != "" {
				if operrors.CodeOf(err) != tt.code || !strings.Contains(err.Error(), "passed its deadline") {
					t.Fatalf("Approve() = %v, want %s", err, tt.code)
				}
				return
			}
			if err != nil {
				t.Fatalf("Approve() = %v", err)
			}
			if !got.Spec.Approval.ExpiresAt.Equal(&ro.Spec.Deadline) {
				t.Fatalf("Approve() expires at %s; want the deadline %s", got.Spec.Approval.ExpiresAt, ro.Spec.Deadline)
			}
		})
	}
}

// conflictOnce is user's client with its first patch failing as a
// conflict, as a write by another, such as the operator's status update,
// does under the optimistic lock.
func conflictOnce(t *testing.T, user string) (client.Client, *int) {
	t.Helper()
	cfg := rest.CopyConfig(restConfig)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:masters", "system:authenticated"}}
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	patches := 0
	return interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if patches++; patches == 1 {
				return apierrors.NewConflict(v1alpha1.GroupVersion.WithResource("runtimerollouts").GroupResource(), obj.GetName(),
					errors.New("the object has been modified"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}), &patches
}

// Approving, revoking and acknowledging read the rollout again after a
// conflicting write and succeed, instead of failing the command.
func TestApprovalWritesRetryAConflict(t *testing.T) {
	ctx := testContext(t)
	approved := func(ro *v1alpha1.RuntimeRollout) {
		ro.Spec.Approval = &v1alpha1.Approval{ApprovedBy: "alice", ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour).Truncate(time.Second)), Reason: "window"}
	}
	tests := []struct {
		name    string
		prepare func(*v1alpha1.RuntimeRollout)
		paused  bool
		write   func(c client.Client, name string) (*v1alpha1.RuntimeRollout, error)
		landed  func(*v1alpha1.RuntimeRollout) bool
	}{
		{name: "approve", write: func(c client.Client, name string) (*v1alpha1.RuntimeRollout, error) {
			return Approve(ctx, c, name, "alice", "window", time.Now().Add(time.Hour))
		}, landed: func(ro *v1alpha1.RuntimeRollout) bool {
			return ro.Spec.Approval != nil && ro.Spec.Approval.ApprovedBy == "alice"
		}},
		{name: "revoke", prepare: approved, write: func(c client.Client, name string) (*v1alpha1.RuntimeRollout, error) {
			return Revoke(ctx, c, name, "alice", "incident", time.Now())
		}, landed: func(ro *v1alpha1.RuntimeRollout) bool { return ro.Spec.Approval.RevokedBy == "alice" }},
		{name: "acknowledge", paused: true, prepare: func(ro *v1alpha1.RuntimeRollout) {
			approved(ro)
			ro.Spec.Steps[0].PostApplySteps = []string{"verify"}
		}, write: func(c client.Client, name string) (*v1alpha1.RuntimeRollout, error) {
			ro, _, err := Acknowledge(ctx, c, name, "alice", time.Now())
			return ro, err
		}, landed: func(ro *v1alpha1.RuntimeRollout) bool { return len(ro.Spec.Approval.Acknowledgements) == 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ro := newRollout(t, ctx, "rollout-conflict-"+tt.name, tt.prepare)
			if tt.paused {
				ro.Status.Phase = v1alpha1.PhaseAwaitingAcknowledgement
				ro.Status.Releases = []v1alpha1.ReleaseStatus{{ID: "gpu-operator", State: v1alpha1.ReleaseReady}}
				if err := admin.Status().Update(ctx, ro); err != nil {
					t.Fatal(err)
				}
			}
			c, patches := conflictOnce(t, "alice")
			if _, err := tt.write(c, ro.Name); err != nil || *patches != 2 {
				t.Fatalf("after a conflict: %v, %d patches", err, *patches)
			}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(ro), ro); err != nil || !tt.landed(ro) {
				t.Fatalf("the write did not land: %+v, %v", ro.Spec.Approval, err)
			}
		})
	}
}

func TestApproveRefusesWhatCannotBeApproved(t *testing.T) {
	ctx := testContext(t)
	ended := newRollout(t, ctx, "rollout-ended-test", func(ro *v1alpha1.RuntimeRollout) {
		ro.Labels[v1alpha1.LabelOutcome] = string(v1alpha1.PhaseSucceeded)
	})
	automatic := newRollout(t, ctx, "rollout-automatic-test", func(ro *v1alpha1.RuntimeRollout) { ro.Spec.ApprovalRequired = false })
	unapproved := newRollout(t, ctx, "rollout-unapproved-test", nil)
	c := as(t, "alice")
	expires := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name string
		err  error
		code operrors.ErrorCode
	}{
		{"an ended rollout", func() error { _, err := Approve(ctx, c, ended.Name, "alice", "r", expires); return err }(), operrors.ErrCodeConflict},
		{"a rollout that needs none", func() error { _, err := Approve(ctx, c, automatic.Name, "alice", "r", expires); return err }(), operrors.ErrCodeInvalidRequest},
		{"a missing rollout", func() error { _, err := Approve(ctx, c, "rollout-missing", "alice", "r", expires); return err }(), operrors.ErrCodeNotFound},
		{"revoking an unapproved rollout", func() error { _, err := Revoke(ctx, c, unapproved.Name, "alice", "r", time.Now()); return err }(), operrors.ErrCodeConflict},
	} {
		if operrors.CodeOf(tc.err) != tc.code {
			t.Errorf("%s: %v, want %s", tc.name, tc.err, tc.code)
		}
	}
}

// Describe shows the steps with their verdicts and CRD changes, the
// assessment report, the approval and the reason the rollout is waiting.
func TestDescribe(t *testing.T) {
	ctx := testContext(t)
	report := []byte(`{"deployer":"argocd","objectNamesCompared":true,"components":[` +
		`{"component":"gpu-operator","change":"version","from":"v25.10.0","to":"v26.3.0","verdict":"manual","notes":"driver reload",` +
		`"precondition":"no GPU reset is in progress","steps":[{"id":"apply","description":"sync the bundle"},` +
		`{"id":"verify","description":"check the CRD's stored versions"}]},` +
		`{"component":"nfd","change":"version","from":"v0.17.0","to":"v0.18.0","verdict":"safe"}],` +
		`"summary":{}}`)
	content := plan.AssessmentContent(report)
	cm, err := content.ConfigMap(namespace, v1alpha1.SingletonName, "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = admin.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	ro := newRollout(t, ctx, "rollout-describe-test", func(ro *v1alpha1.RuntimeRollout) {
		ro.Spec.AssessmentRef = &v1alpha1.DigestedConfigMapRef{ConfigMap: content.Name, Digest: content.Digest}
	})
	ro.Status.Phase, ro.Status.Message = v1alpha1.PhaseStalled, "the engine objects changed since the rollout was created: nfd appeared"
	ro.Status.Approval = v1alpha1.ApprovalPending
	if err = admin.Status().Update(ctx, ro); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = Describe(ctx, as(t, "alice"), namespace, ro.Name, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		ro.Name, digestA + " -> " + digestB, "Stalled", "nfd appeared", "required, pending",
		"gpu-operator", "Upgrade", "Manual", "true", "gpu-operator-readiness", "Verify",
		content.Name, "v25.10.0", "v26.3.0", "manual", "driver reload",
		"Recorded steps", "before: no GPU reset is in progress", "apply: sync the bundle", "verify: check the CRD's stored versions",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("description lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "nfd v0.17.0 -> v0.18.0") {
		t.Errorf("a component without recorded steps is listed under them:\n%s", out.String())
	}

	// The approval, then its revocation, and the step in flight.
	if _, err = Approve(ctx, admin, ro.Name, "alice", "window", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = admin.Get(ctx, client.ObjectKeyFromObject(ro), ro); err != nil {
		t.Fatal(err)
	}
	ro.Status.Current = &v1alpha1.CurrentStep{Release: "gpu-operator", Operation: v1alpha1.OperationUpgrade}
	ro.Status.Releases = []v1alpha1.ReleaseStatus{{ID: "gpu-operator-readiness", State: v1alpha1.ReleaseReady}}
	if err = admin.Status().Update(ctx, ro); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err = Describe(ctx, admin, namespace, ro.Name, &out)
	if err != nil || !strings.Contains(out.String(), "approved by alice until") || !strings.Contains(out.String(), "in flight") {
		t.Fatalf("approved description: %v\n%s", err, out.String())
	}
	if _, err = Revoke(ctx, admin, ro.Name, "bob", "incident", time.Now()); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = Describe(ctx, admin, namespace, ro.Name, &out); err != nil || !strings.Contains(out.String(), "revoked by bob") {
		t.Fatalf("revoked description: %v\n%s", err, out.String())
	}

	other := newRollout(t, ctx, "rollout-describe-mismatch", func(ro *v1alpha1.RuntimeRollout) {
		ro.Spec.AssessmentRef = &v1alpha1.DigestedConfigMapRef{ConfigMap: content.Name, Digest: digestA}
	})
	if err = Describe(ctx, admin, namespace, other.Name, &bytes.Buffer{}); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("a report that does not match the rollout's digest: %v", err)
	}
}
