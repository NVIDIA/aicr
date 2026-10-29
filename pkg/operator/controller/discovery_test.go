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

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/discovery"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// putNode creates a node in a placement pool with a full node info tuple.
func putNode(t *testing.T, ctx context.Context, name, pool, product string) {
	t.Helper()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"pool": pool}}}
	if product != "" {
		n.Labels[discovery.GPUProductLabel] = product
	}
	if err := k8s.Create(ctx, n); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.WithoutCancel(ctx), n) })
	n.Status.NodeInfo = corev1.NodeSystemInfo{
		Architecture: "amd64", OSImage: "Ubuntu 24.04.3 LTS", KernelVersion: "6.8.0-1031-aws",
		ContainerRuntimeVersion: "containerd://2.1.4", KubeletVersion: "v1.34.2",
	}
	if err := k8s.Status().Update(ctx, n); err != nil {
		t.Fatalf("node status: %v", err)
	}
}

// snapshotWith is the kind fixture snapshot with AICR's fingerprint replaced
// by an Ubuntu node's.
func snapshotWith(t *testing.T, service, accelerator string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(fixture(t, "..", "aicrjob", "testdata", "v1.0.0", "snapshot-kind-no-gpu.json"), &doc); err != nil {
		t.Fatal(err)
	}
	dim := func(v, source string) map[string]any {
		if v == "" {
			return map[string]any{}
		}
		return map[string]any{"value": v, "source": source}
	}
	doc["fingerprint"] = map[string]any{
		"service": dim(service, "k8s.node.provider"), "accelerator": dim(accelerator, "nodeTopology.label.nvidia.com/gpu.product"),
		"os": dim("ubuntu", "os.release"),
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Storing a new discovery snapshot removes the runtime's older ones, but
// never the snapshot a rollout validates against, which goes with its
// rollout.
func TestResolverKeepsRolloutSnapshots(t *testing.T) {
	ctx := testContext(t)
	putNode(t, ctx, "sys-a", "system", "")
	putNode(t, ctx, "gpu-a", "gpu", "NVIDIA-H100-80GB-HBM3")
	rt := freshRuntime(t, ctx, nil)
	stale := putSnapshot(t, ctx, rt, `{"kind":"Snapshot","n":1}`)
	owned := plan.RolloutSnapshotContent([]byte(`{"kind":"Snapshot","n":2}`), "fedcba9876543210")
	cm, err := owned.ConfigMap(operatorNS, rt.Name, string(rt.UID), map[string]string{v1alpha1.LabelInput: "fedcba9876543210"})
	if err != nil {
		t.Fatal(err)
	}
	cm.OwnerReferences = []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "RuntimeRollout", Name: "r", UID: "u-r"}}
	if err = k8s.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	r, _ := discoveryResolver(t)
	if err = reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	snaps := liveJobs(t, ctx, "snapshot")
	if len(snaps) != 1 {
		t.Fatalf("snapshot Jobs = %d", len(snaps))
	}
	succeed(t, ctx, &snaps[0], snapshotWith(t, "eks", "h100"))
	if err = reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	gone := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: stale.Name}, &corev1.ConfigMap{})
	kept := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: owned.Name}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(gone) || kept != nil {
		t.Fatalf("older discovery snapshot: %v; rollout's snapshot: %v", gone, kept)
	}
}

func discoveryResolver(t *testing.T) (*ResolverReconciler, *fakeRegistry) {
	t.Helper()
	r, reg := newResolver(t)
	r.Config.SkipDiscovery = false
	now := time.Now()
	r.Now = func() time.Time { return now }
	return r, reg
}

// The resolver takes a snapshot with the agent pinned to the accelerated
// group, keeps it beyond its Job, resolves the recipe from it and reports
// the groups, the snapshot and the criteria. A homogeneous node added later
// takes no new snapshot and changes no inputs; a resnapshot request does.
func TestResolverDiscovers(t *testing.T) {
	ctx := testContext(t)
	putNode(t, ctx, "sys-a", "system", "")
	putNode(t, ctx, "gpu-a", "gpu", "NVIDIA-H100-80GB-HBM3")
	freshRuntime(t, ctx, nil)
	r, reg := discoveryResolver(t)

	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	snaps := liveJobs(t, ctx, "snapshot")
	if len(snaps) != 1 || len(liveJobs(t, ctx, "recipe")) != 0 {
		t.Fatalf("snapshot Jobs = %d, want one and no recipe run yet", len(snaps))
	}
	snapJob := snaps[0]
	args := strings.Join(snapJob.Spec.Template.Spec.Containers[0].Args, " ")
	if snapJob.Spec.Template.Spec.ServiceAccountName != "aicr-snapshot" || !strings.Contains(args, "--node-selector pool=gpu") ||
		!strings.Contains(args, "--service-account-name aicr-snapshot-agent") {

		t.Fatalf("snapshot Job sa=%s args=%s", snapJob.Spec.Template.Spec.ServiceAccountName, args)
	}

	succeed(t, ctx, &snapJob, snapshotWith(t, "eks", "h100"))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if len(liveJobs(t, ctx, "snapshot")) != 0 {
		t.Error("the snapshot Job outlived its stored result")
	}
	recipe := liveJobs(t, ctx, "recipe")
	if len(recipe) != 1 || !strings.Contains(strings.Join(recipe[0].Spec.Template.Spec.Containers[0].Args, " "), "--snapshot cm://aicr-jobs/") {
		t.Fatalf("recipe Jobs = %+v", recipe)
	}
	succeed(t, ctx, &recipe[0], lanRecipe(t))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	render := liveJobs(t, ctx, "render")[0]
	succeed(t, ctx, &render, nil)
	reg.resolved["run-"+render.Labels[v1alpha1.LabelRunID]] = plan.DigestOf([]byte("bundle"))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	rt := getRuntime(t, ctx)
	d := rt.Status.Discovery
	if c := cond(rt, v1alpha1.ConditionResolved); c.Status != metav1.ConditionTrue || d == nil || d.Snapshot == nil ||
		d.AcceleratedNodes != 1 || d.SystemNodes != 1 || d.Criteria == nil || d.Criteria.Accelerator != "h100" {

		t.Fatalf("Resolved = %+v, discovery = %+v", c, d)
	}
	doc, err := readPlan(ctx, k8s, operatorNS, rt.Status.CandidatePlanDigest)
	if err != nil || doc.Provenance.SnapshotDigest != d.Snapshot.Digest {
		t.Fatalf("plan provenance = %+v, %v", doc.Provenance, err)
	}
	inputDigest := rt.Status.InputDigest

	// Another node with a known tuple: no snapshot, same inputs.
	putNode(t, ctx, "gpu-b", "gpu", "NVIDIA-H100-80GB-HBM3")
	if err = reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	rt = getRuntime(t, ctx)
	if len(liveJobs(t, ctx, "snapshot")) != 0 || rt.Status.InputDigest != inputDigest || rt.Status.Discovery.AcceleratedNodes != 2 {
		t.Fatalf("a homogeneous node changed resolution: input %s, discovery %+v", rt.Status.InputDigest, rt.Status.Discovery)
	}

	// A resnapshot request takes a new snapshot; Resolved stays True meanwhile.
	patch := client.MergeFrom(rt.DeepCopy())
	rt.Annotations = map[string]string{v1alpha1.AnnotationResnapshot: "1"}
	if err = k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	if err = reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if len(liveJobs(t, ctx, "snapshot")) != 1 || cond(getRuntime(t, ctx), v1alpha1.ConditionResolved).Status != metav1.ConditionTrue {
		t.Fatalf("resnapshot: %d snapshot Jobs, Resolved %+v", len(liveJobs(t, ctx, "snapshot")), cond(getRuntime(t, ctx), v1alpha1.ConditionResolved))
	}
}

// Unsafe observations and snapshot facts block resolution with specific
// reasons before any recipe run.
func TestResolverDiscoveryBlocks(t *testing.T) {
	ctx := testContext(t)
	tests := []struct {
		name   string
		nodes  [][3]string
		snap   []byte
		reason string
	}{
		{"empty accelerated group", [][3]string{{"sys-a", "system", ""}}, nil, discovery.ReasonGroupEmpty},
		{"mixed GPU products", [][3]string{{"sys-a", "system", ""}, {"gpu-a", "gpu", "NVIDIA-H100-80GB-HBM3"}, {"gpu-b", "gpu", "NVIDIA-A100-SXM4-80GB"}},
			nil, discovery.ReasonHeterogeneous},
		{"criteria conflict", [][3]string{{"sys-a", "system", ""}, {"gpu-a", "gpu", ""}}, snapshotWith(t, "kind", "h100"),
			discovery.ReasonCriteriaConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, n := range tt.nodes {
				putNode(t, ctx, n[0], n[1], n[2])
			}
			freshRuntime(t, ctx, nil)
			r, _ := discoveryResolver(t)
			if err := reconcileResolver(t, ctx, r); err != nil {
				t.Fatal(err)
			}
			if tt.snap != nil {
				job := liveJobs(t, ctx, "snapshot")[0]
				succeed(t, ctx, &job, tt.snap)
				if err := reconcileResolver(t, ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			if c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved); c.Reason != tt.reason {
				t.Fatalf("Resolved = %+v, want %s", c, tt.reason)
			}
			if len(liveJobs(t, ctx, "recipe")) != 0 {
				t.Error("a recipe run started despite the block")
			}
		})
	}
}

// Installing the GPU stack takes a new snapshot, which must confirm the
// criteria the active plan was resolved with: a pre-driver H100 that GPU
// feature discovery labels as an A100 stalls with CriteriaConflict.
func TestResolverConfirmsAfterGPUStack(t *testing.T) {
	ctx := testContext(t)
	putNode(t, ctx, "sys-a", "system", "")
	putNode(t, ctx, "gpu-a", "gpu", "")
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) { rt.Spec.Criteria = nil })
	r, _ := discoveryResolver(t)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	first := liveJobs(t, ctx, "snapshot")[0]
	succeed(t, ctx, &first, snapshotWith(t, "eks", "h100"))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if len(liveJobs(t, ctx, "recipe")) != 1 {
		t.Fatal("the pre-driver snapshot did not reach the recipe run")
	}

	// The GPU stack is rolled out: the active plan now has gpu-operator.
	active := putResolved(t, ctx, rt, map[string]string{"service": "eks", "accelerator": "h100", "os": "ubuntu"},
		testRelease("gpu-operator", plan.PhasePrimary, "", `{}`))
	rt = getRuntime(t, ctx)
	if err := applyRuntimeStatus(ctx, k8s, rt, map[string]any{"activePlanDigest": active}, RolloutFieldManager); err != nil {
		t.Fatal(err)
	}
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	confirm := liveJobs(t, ctx, "snapshot")
	if len(confirm) != 1 || confirm[0].Labels[v1alpha1.LabelInput] == first.Labels[v1alpha1.LabelInput] {
		t.Fatalf("no new snapshot after the GPU stack: %d Jobs", len(confirm))
	}
	succeed(t, ctx, &confirm[0], snapshotWith(t, "eks", "a100"))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved)
	if c.Reason != discovery.ReasonCriteriaConflict || !strings.Contains(c.Message, "active plan was resolved for h100") {
		t.Fatalf("Resolved = %+v", c)
	}

	// A plan resolved from declared criteria alone has nothing to confirm.
	declared := putResolved(t, ctx, rt, nil, testRelease("gpu-operator", plan.PhasePrimary, "", `{"declared":true}`))
	if err := applyRuntimeStatus(ctx, k8s, getRuntime(t, ctx), map[string]any{"activePlanDigest": declared}, RolloutFieldManager); err != nil {
		t.Fatal(err)
	}
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	next := liveJobs(t, ctx, "snapshot")
	if len(next) != 1 {
		t.Fatalf("snapshot Jobs = %d", len(next))
	}
	succeed(t, ctx, &next[0], snapshotWith(t, "eks", "a100"))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if c = cond(getRuntime(t, ctx), v1alpha1.ConditionResolved); c.Reason == discovery.ReasonCriteriaConflict {
		t.Fatalf("a declared-criteria plan was confirmed against: %+v", c)
	}
}
