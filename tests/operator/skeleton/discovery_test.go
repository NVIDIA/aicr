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
	"encoding/json"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/discovery"
)

// nodeState is every node's labels and taints, as discovery must leave them.
func nodeState(ctx context.Context, t *testing.T) string {
	t.Helper()
	nodes := &corev1.NodeList{}
	if err := k8s.List(ctx, nodes); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{}
	for _, n := range nodes.Items {
		state[n.Name] = map[string]any{"labels": n.Labels, "taints": n.Spec.Taints}
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// waitResolvedReason waits for Resolved with the status and reason on the
// current generation.
func waitResolvedReason(ctx context.Context, t *testing.T, status metav1.ConditionStatus, reason string) *v1alpha1.ClusterRuntime {
	t.Helper()
	var rt *v1alpha1.ClusterRuntime
	waitFor(t, resolveTimeout, "Resolved="+string(status)+" "+reason, func() bool {
		rt = getRuntime(ctx, t)
		c := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionResolved)
		return c != nil && c.Status == status && c.Reason == reason && c.ObservedGeneration == rt.Generation
	})
	return rt
}

// allowed asks the API server whether a ServiceAccount may do something.
func allowed(ctx context.Context, t *testing.T, namespace, account string, attrs authorizationv1.ResourceAttributes) bool {
	t.Helper()
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: "system:serviceaccount:" + namespace + ":" + account, ResourceAttributes: &attrs,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace},
	}}
	if err := k8s.Create(ctx, sar); err != nil {
		t.Fatal(err)
	}
	return sar.Status.Allowed
}

// discoveryResolves turns discovery on with criteria a kind cluster can
// satisfy: the operator observes the groups, takes a snapshot with the agent
// pinned to the accelerated group, keeps it, and resolves the recipe from it
// without changing a node. Neither the operator nor the discovery identities
// can write Nodes or read Secrets. A node in both groups blocks with
// PlacementOverlap until it leaves one.
func discoveryResolves(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	rt := getRuntime(ctx, t)
	saved := rt.Spec.DeepCopy()
	before := nodeState(ctx, t)

	patch := client.MergeFrom(rt.DeepCopy())
	rt.Spec.Paused = true
	// service comes from the snapshot (kind); kind's Debian has no recipe OS.
	rt.Spec.Criteria = &v1alpha1.DeclaredCriteria{Accelerator: "h100", OS: "any"}
	rt.Spec.Overrides = nil
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	setOperatorArg(ctx, t, "--development-skip-discovery", "false")

	rt = waitResolvedReason(ctx, t, metav1.ConditionTrue, "Resolved")
	d := rt.Status.Discovery
	if d == nil || d.Snapshot == nil || d.SystemNodes != 1 || d.AcceleratedNodes != 1 || d.Criteria == nil ||
		d.Criteria.Service != "kind" || d.Criteria.Accelerator != "h100" {

		t.Fatalf("status.discovery = %s", statusJSON(ctx, t))
	}
	if doc := readPlan(ctx, t, rt.Status.CandidatePlanDigest); doc.Provenance.SnapshotDigest != d.Snapshot.Digest {
		t.Errorf("plan snapshot digest = %s, want %s", doc.Provenance.SnapshotDigest, d.Snapshot.Digest)
	}
	stored := &corev1.ConfigMap{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: d.Snapshot.ConfigMap}, stored); err != nil {
		t.Fatalf("stored snapshot: %v", err)
	}
	t.Logf("snapshot %s (%s) resolved plan %s from %s/%s", d.Snapshot.ConfigMap, d.Snapshot.Digest, rt.Status.CandidatePlanDigest,
		d.Criteria.Service, d.Criteria.Accelerator)
	if after := nodeState(ctx, t); after != before {
		t.Errorf("discovery changed nodes:\n%s\n%s", before, after)
	}
	for _, who := range [][2]string{{operatorNamespace, "aicr-operator"}, {"aicr-discovery", "aicr-snapshot"}, {"aicr-discovery", "aicr-snapshot-agent"}} {
		for _, attrs := range []authorizationv1.ResourceAttributes{
			{Verb: "patch", Resource: "nodes"}, {Verb: "update", Resource: "nodes"},
			{Verb: "list", Resource: "secrets"}, {Verb: "get", Resource: "secrets", Namespace: "kube-system"},
		} {
			if allowed(ctx, t, who[0], who[1], attrs) {
				t.Errorf("%s/%s may %s %s", who[0], who[1], attrs.Verb, attrs.Resource)
			}
		}
	}

	// A node in both groups blocks resolution.
	nodes := &corev1.NodeList{}
	if err := k8s.List(ctx, nodes, client.MatchingLabels{systemLabel: "true"}); err != nil || len(nodes.Items) == 0 {
		t.Fatalf("system nodes: %v", err)
	}
	both := &nodes.Items[0]
	edit := client.MergeFrom(both.DeepCopy())
	both.Labels[gpuLabel] = "true"
	if err := k8s.Patch(ctx, both, edit); err != nil {
		t.Fatal(err)
	}
	waitResolvedReason(ctx, t, metav1.ConditionFalse, discovery.ReasonPlacementOverlap)
	edit = client.MergeFrom(both.DeepCopy())
	delete(both.Labels, gpuLabel)
	if err := k8s.Patch(ctx, both, edit); err != nil {
		t.Fatal(err)
	}
	waitResolvedReason(ctx, t, metav1.ConditionTrue, "Resolved")

	setOperatorArg(ctx, t, "--development-skip-discovery", "true")
	rt = getRuntime(ctx, t)
	patch = client.MergeFrom(rt.DeepCopy())
	rt.Spec = *saved
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	waitReadyOn(ctx, t, active)
}
