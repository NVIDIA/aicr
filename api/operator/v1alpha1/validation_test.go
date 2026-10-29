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

package v1alpha1_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	uidA    = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
)

var k8sClient client.Client

// TestMain runs the CRDs on a real API server. The suite is skipped only when
// envtest assets are absent outside CI; in CI a missing control plane fails.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set; CRD validation tests cannot run")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "skipping CRD validation tests: KUBEBUILDER_ASSETS is not set (run `make test`)")
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
	scheme := runtime.NewScheme()
	if err = v1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "register scheme: %v\n", err)
		os.Exit(1)
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create client: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	os.Exit(code)
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func validRuntime() *v1alpha1.ClusterRuntime {
	return &v1alpha1.ClusterRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName},
		Spec: v1alpha1.ClusterRuntimeSpec{
			Intent:   "training",
			Platform: "kubeflow",
			Placement: v1alpha1.Placement{
				System:      v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{"example.com/pool": "system"}},
				Accelerated: v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{"example.com/pool": "gpu"}},
			},
		},
	}
}

// expectRejected creates or updates obj and requires the API server to reject
// it with a message containing want.
func expectRejected(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("request accepted, want rejection containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("rejection %q does not contain %q", err.Error(), want)
	}
}

func TestClusterRuntimeDefaults(t *testing.T) {
	c := ctx(t)
	rt := validRuntime()
	if err := k8sClient.Create(c, rt); err != nil {
		t.Fatalf("create valid runtime: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rt) })

	got := &v1alpha1.ClusterRuntime{}
	if err := k8sClient.Get(c, client.ObjectKeyFromObject(rt), got); err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	s := got.Spec
	if s.Selection.Policy != v1alpha1.SelectionAutomatic {
		t.Errorf("selection.policy = %q, want Automatic", s.Selection.Policy)
	}
	if s.DeletionPolicy != v1alpha1.DeletionOrphan {
		t.Errorf("deletionPolicy = %q, want Orphan", s.DeletionPolicy)
	}
	if s.Validation.Deployment != v1alpha1.DeploymentValidationRequired {
		t.Errorf("validation.deployment = %q, want Required", s.Validation.Deployment)
	}
	if s.Rollout.StabilizationWindow == nil || s.Rollout.StabilizationWindow.Duration != 10*time.Minute ||
		s.Rollout.ActionTimeout == nil || s.Rollout.ActionTimeout.Duration != 30*time.Minute ||
		s.Rollout.Deadline == nil || s.Rollout.Deadline.Duration != 6*time.Hour {

		t.Errorf("rollout defaults = %+v, want 10m/30m/6h", s.Rollout)
	}
}

func TestClusterRuntimeRejections(t *testing.T) {
	tooMany := make([]v1alpha1.Override, 65)
	for i := range tooMany {
		tooMany[i] = v1alpha1.Override{Component: "gpu-operator", Path: fmt.Sprintf("p%d", i), Value: apiextensionsv1.JSON{Raw: []byte("true")}}
	}
	tests := []struct {
		name   string
		mutate func(*v1alpha1.ClusterRuntime)
		want   string
	}{
		{"second name", func(r *v1alpha1.ClusterRuntime) { r.Name = "other" }, "singleton named 'cluster'"},
		{"pinned without digest", func(r *v1alpha1.ClusterRuntime) { r.Spec.Selection.Policy = v1alpha1.SelectionPinned }, "pinnedPlanDigest is required"},
		{"digest without pinned", func(r *v1alpha1.ClusterRuntime) { r.Spec.Selection.PinnedPlanDigest = digestA }, "pinnedPlanDigest is required"},
		{"malformed digest", func(r *v1alpha1.ClusterRuntime) {
			r.Spec.Selection = v1alpha1.Selection{Policy: v1alpha1.SelectionPinned, PinnedPlanDigest: "sha256:XYZ"}
		}, "pinnedPlanDigest"},
		{"identical selectors", func(r *v1alpha1.ClusterRuntime) {
			r.Spec.Placement.Accelerated.NodeSelector = r.Spec.Placement.System.NodeSelector
		}, "selectors must differ"},
		{"empty selector", func(r *v1alpha1.ClusterRuntime) {
			r.Spec.Placement.System.NodeSelector = map[string]v1alpha1.LabelValue{}
		}, "nodeSelector"},
		{"too many overrides", func(r *v1alpha1.ClusterRuntime) { r.Spec.Overrides = tooMany }, "overrides"},
		{"duplicate override", func(r *v1alpha1.ClusterRuntime) {
			o := v1alpha1.Override{Component: "gpu-operator", Path: "driver.enabled", Value: apiextensionsv1.JSON{Raw: []byte("false")}}
			r.Spec.Overrides = []v1alpha1.Override{o, o}
		}, "Duplicate value"},
		{"oversized override path", func(r *v1alpha1.ClusterRuntime) {
			r.Spec.Overrides = []v1alpha1.Override{{Component: "gpu-operator", Path: strings.Repeat("a", 257), Value: apiextensionsv1.JSON{Raw: []byte("1")}}}
		}, "path"},
		{"oversized intent", func(r *v1alpha1.ClusterRuntime) { r.Spec.Intent = strings.Repeat("a", 64) }, "intent"},
		{"step outlasts rollout", func(r *v1alpha1.ClusterRuntime) {
			r.Spec.Rollout.ActionTimeout = &metav1.Duration{Duration: 7 * time.Hour}
		}, "must not exceed deadline"},
		{"zero action timeout", func(r *v1alpha1.ClusterRuntime) {
			r.Spec.Rollout.ActionTimeout = &metav1.Duration{}
		}, "must be positive"},
		{"malformed adoption UID", func(r *v1alpha1.ClusterRuntime) {
			r.Spec.Adoption = &v1alpha1.Adoption{RuntimeUID: "not-a-uid"}
		}, "runtimeUID"},
		{"unknown deletion policy", func(r *v1alpha1.ClusterRuntime) { r.Spec.DeletionPolicy = "Purge" }, "deletionPolicy"},
		{"duplicate TCPXO interface", func(r *v1alpha1.ClusterRuntime) {
			m := v1alpha1.NetworkInterfaceMapping{InterfaceName: "eth1", Network: "c-gpu-nic-0"}
			r.Spec.Configuration = &v1alpha1.RecipeConfiguration{GKE: &v1alpha1.GKEConfiguration{TCPXOInterfaces: []v1alpha1.NetworkInterfaceMapping{m, m}}}
		}, "Duplicate value"},
		{"empty TCPXO mapping", func(r *v1alpha1.ClusterRuntime) {
			r.Spec.Configuration = &v1alpha1.RecipeConfiguration{GKE: &v1alpha1.GKEConfiguration{TCPXOInterfaces: []v1alpha1.NetworkInterfaceMapping{}}}
		}, "tcpxoInterfaces"},
		{"oversized TCPXO network", func(r *v1alpha1.ClusterRuntime) {
			r.Spec.Configuration = &v1alpha1.RecipeConfiguration{GKE: &v1alpha1.GKEConfiguration{TCPXOInterfaces: []v1alpha1.NetworkInterfaceMapping{
				{InterfaceName: "eth1", Network: strings.Repeat("n", 64)}}}}
		}, "network"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := validRuntime()
			tt.mutate(rt)
			err := k8sClient.Create(ctx(t), rt)
			if err == nil {
				_ = k8sClient.Delete(context.Background(), rt)
			}
			expectRejected(t, err, tt.want)
		})
	}
}

func TestClusterRuntimePinnedAccepted(t *testing.T) {
	c := ctx(t)
	rt := validRuntime()
	rt.Spec.Selection = v1alpha1.Selection{Policy: v1alpha1.SelectionPinned, PinnedPlanDigest: digestA}
	rt.Spec.Adoption = &v1alpha1.Adoption{RuntimeUID: uidA}
	rt.Spec.Overrides = []v1alpha1.Override{{Component: "gpu-operator", Path: "dcgmExporter.enabled", Value: apiextensionsv1.JSON{Raw: []byte("false")}}}
	if err := k8sClient.Create(c, rt); err != nil {
		t.Fatalf("create pinned runtime: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rt) })
}

var rolloutSeq int

func validRollout(t *testing.T) *v1alpha1.RuntimeRollout {
	t.Helper()
	rolloutSeq++
	return &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("rollout-0123456789abcdef-%d", rolloutSeq)},
		Spec: v1alpha1.RuntimeRolloutSpec{
			SchemaVersion:           v1alpha1.RolloutSchemaVersion,
			RuntimeUID:              uidA,
			ToPlanDigest:            digestA,
			StartingInventoryDigest: digestB,
			Deadline:                metav1.NewTime(time.Now().Add(6 * time.Hour).Truncate(time.Second)),
			ActionTimeout:           metav1.Duration{Duration: 30 * time.Minute},
			StartingInventory: []v1alpha1.InventoryEntry{
				{ReleaseID: "nvsentinel", Name: "nvsentinel", UID: uidA, PlanDigest: digestB, SpecDigest: digestA},
			},
			ApprovalRequired: true,
			Steps: []v1alpha1.RolloutStep{
				{Release: "gpu-operator", Operation: v1alpha1.OperationInstall, Assessment: v1alpha1.AssessmentNotApplicable},
				{Release: "gpu-operator-readiness", Operation: v1alpha1.OperationVerify, Assessment: v1alpha1.AssessmentNotApplicable},
				{Release: "nvsentinel", Operation: v1alpha1.OperationRemove, Assessment: v1alpha1.AssessmentNotApplicable, UID: uidA},
			},
		},
	}
}

func createRollout(t *testing.T, ro *v1alpha1.RuntimeRollout) {
	t.Helper()
	if err := k8sClient.Create(ctx(t), ro); err != nil {
		t.Fatalf("create rollout: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ro) })
}

func approval() *v1alpha1.Approval {
	return &v1alpha1.Approval{
		ApprovedBy: "alice@example.com",
		ExpiresAt:  metav1.NewTime(time.Now().Add(2 * time.Hour).Truncate(time.Second)),
		Reason:     "Scheduled maintenance window",
	}
}

func TestRolloutSpecIsImmutable(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1alpha1.RuntimeRollout)
	}{
		{"target", func(r *v1alpha1.RuntimeRollout) { r.Spec.ToPlanDigest = digestB }},
		{"steps", func(r *v1alpha1.RuntimeRollout) { r.Spec.Steps = r.Spec.Steps[:1] }},
		{"deadline", func(r *v1alpha1.RuntimeRollout) { r.Spec.Deadline = metav1.NewTime(r.Spec.Deadline.Add(time.Hour)) }},
		{"approval required", func(r *v1alpha1.RuntimeRollout) { r.Spec.ApprovalRequired = false }},
		{"add from plan", func(r *v1alpha1.RuntimeRollout) { r.Spec.FromPlanDigest = digestB }},
		{"add assessment", func(r *v1alpha1.RuntimeRollout) {
			r.Spec.AssessmentRef = &v1alpha1.DigestedConfigMapRef{ConfigMap: "aicr-upgrade-check-1", Digest: digestA}
		}},
		{"schema version", func(r *v1alpha1.RuntimeRollout) { r.Spec.SchemaVersion++ }},
		{"runtime UID", func(r *v1alpha1.RuntimeRollout) { r.Spec.RuntimeUID = "1c2d3e4f-5061-7283-94a5-b6c7d8e9f0a1" }},
		{"starting inventory digest", func(r *v1alpha1.RuntimeRollout) { r.Spec.StartingInventoryDigest = digestA }},
		{"action timeout", func(r *v1alpha1.RuntimeRollout) { r.Spec.ActionTimeout = metav1.Duration{Duration: time.Hour} }},
		{"inventory entry", func(r *v1alpha1.RuntimeRollout) { r.Spec.StartingInventory[0].SpecDigest = digestB }},
		{"inventory dropped", func(r *v1alpha1.RuntimeRollout) { r.Spec.StartingInventory = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ro := validRollout(t)
			createRollout(t, ro)
			tt.mutate(ro)
			expectRejected(t, k8sClient.Update(ctx(t), ro), "immutable except spec.approval")
		})
	}
}

func TestApprovalLifecycle(t *testing.T) {
	c := ctx(t)
	ro := validRollout(t)
	createRollout(t, ro)

	ro.Spec.Approval = approval()
	if err := k8sClient.Update(c, ro); err != nil {
		t.Fatalf("approve: %v", err)
	}

	changed := ro.DeepCopy()
	changed.Spec.Approval.Reason = "different"
	expectRejected(t, k8sClient.Update(c, changed), "neither cleared nor changed")

	extended := ro.DeepCopy()
	extended.Spec.Approval.ExpiresAt = metav1.NewTime(ro.Spec.Approval.ExpiresAt.Add(time.Hour))
	expectRejected(t, k8sClient.Update(c, extended), "neither cleared nor changed")

	cleared := ro.DeepCopy()
	cleared.Spec.Approval = nil
	expectRejected(t, k8sClient.Update(c, cleared), "neither cleared nor changed")

	partial := ro.DeepCopy()
	partial.Spec.Approval.RevokedBy = "bob@example.com"
	expectRejected(t, k8sClient.Update(c, partial), "set together")

	now := metav1.NewTime(time.Now().Truncate(time.Second))
	ro.Spec.Approval.RevokedBy = "bob@example.com"
	ro.Spec.Approval.RevokedAt = &now
	ro.Spec.Approval.RevokeReason = "Training job not checkpointed"
	if err := k8sClient.Update(c, ro); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	rerevoked := ro.DeepCopy()
	rerevoked.Spec.Approval.RevokedBy = "carol@example.com"
	expectRejected(t, k8sClient.Update(c, rerevoked), "revocation can be neither cleared nor changed")
}

func TestRevocationNeedsExistingApproval(t *testing.T) {
	ro := validRollout(t)
	createRollout(t, ro)
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	a := approval()
	a.RevokedBy, a.RevokedAt, a.RevokeReason = "bob@example.com", &now, "changed my mind"
	ro.Spec.Approval = a
	expectRejected(t, k8sClient.Update(ctx(t), ro), "needs an existing approval")
}

func TestRolloutBounds(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1alpha1.RuntimeRollout)
		want   string
	}{
		{"too many steps", func(r *v1alpha1.RuntimeRollout) {
			r.Spec.Steps = make([]v1alpha1.RolloutStep, 257)
			for i := range r.Spec.Steps {
				r.Spec.Steps[i] = v1alpha1.RolloutStep{Release: fmt.Sprintf("r%d", i), Operation: v1alpha1.OperationInstall, Assessment: v1alpha1.AssessmentSafe}
			}
		}, "steps"},
		{"release name over Helm limit", func(r *v1alpha1.RuntimeRollout) { r.Spec.Steps[0].Release = strings.Repeat("a", 54) }, "release"},
		{"unknown operation", func(r *v1alpha1.RuntimeRollout) { r.Spec.Steps[0].Operation = "Restart" }, "operation"},
		{"remove without uid", func(r *v1alpha1.RuntimeRollout) { r.Spec.Steps[0].Operation = v1alpha1.OperationRemove }, "uid is set exactly on Remove steps"},
		{"uid on an install", func(r *v1alpha1.RuntimeRollout) { r.Spec.Steps[0].UID = uidA }, "uid is set exactly on Remove steps"},
		{"malformed runtime UID", func(r *v1alpha1.RuntimeRollout) { r.Spec.RuntimeUID = "x" }, "runtimeUID"},
		{"zero schema version", func(r *v1alpha1.RuntimeRollout) { r.Spec.SchemaVersion = 0 }, "schemaVersion"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ro := validRollout(t)
			tt.mutate(ro)
			err := k8sClient.Create(ctx(t), ro)
			if err == nil {
				_ = k8sClient.Delete(context.Background(), ro)
			}
			expectRejected(t, err, tt.want)
		})
	}
}

// The ClusterRuntime example in docs/operator/api.md, copied verbatim into
// testdata, must be accepted as written and read back through the typed API.
func TestAPISketchExampleIsAccepted(t *testing.T) {
	c := ctx(t)
	f, err := os.Open(filepath.Join("testdata", "clusterruntime-example.yaml"))
	if err != nil {
		t.Fatalf("open example: %v", err)
	}
	defer f.Close()
	obj := &unstructured.Unstructured{}
	if err := utilyaml.NewYAMLOrJSONDecoder(f, 4096).Decode(&obj.Object); err != nil {
		t.Fatalf("decode example: %v", err)
	}
	if err := k8sClient.Create(c, obj); err != nil {
		t.Fatalf("the api.md ClusterRuntime example was rejected: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })

	got := &v1alpha1.ClusterRuntime{}
	if err := k8sClient.Get(c, client.ObjectKey{Name: v1alpha1.SingletonName}, got); err != nil {
		t.Fatalf("get example: %v", err)
	}
	if got.Spec.Intent != "training" || got.Spec.Platform != "kubeflow" || len(got.Spec.Overrides) != 1 ||
		got.Spec.Rollout.Deadline == nil || got.Spec.Rollout.Deadline.Duration != 6*time.Hour {

		t.Fatalf("example did not round-trip: %+v", got.Spec)
	}
}
