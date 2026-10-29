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

package qualify

import (
	"context"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
)

// release is the one release the suite rolls out.
const release = "cert-manager"

const (
	resolveTimeout = 15 * time.Minute
	rolloutTimeout = 10 * time.Minute
)

func waitFor(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s within %s", what, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

func getRuntime(ctx context.Context, t *testing.T) *v1alpha1.ClusterRuntime {
	t.Helper()
	rt := &v1alpha1.ClusterRuntime{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, rt); err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	return rt
}

// cond is a runtime condition of the current generation, or the zero value.
func cond(rt *v1alpha1.ClusterRuntime, conditionType string) metav1.Condition {
	c := meta.FindStatusCondition(rt.Status.Conditions, conditionType)
	if c == nil || c.ObservedGeneration != rt.Generation {
		return metav1.Condition{}
	}
	return *c
}

func patchRuntime(ctx context.Context, t *testing.T, mutate func(*v1alpha1.ClusterRuntime)) {
	t.Helper()
	rt := getRuntime(ctx, t)
	patch := client.MergeFrom(rt.DeepCopy())
	mutate(rt)
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatalf("patch runtime: %v", err)
	}
}

// setCPU sets the release's CPU request; each value is a new plan.
func setCPU(ctx context.Context, t *testing.T, cpu string) {
	t.Helper()
	patchRuntime(ctx, t, func(rt *v1alpha1.ClusterRuntime) {
		kept := make([]v1alpha1.Override, 0, len(rt.Spec.Overrides)+1)
		for _, o := range rt.Spec.Overrides {
			if o.Component != release || o.Path != "resources.requests.cpu" {
				kept = append(kept, o)
			}
		}
		kept = append(kept, v1alpha1.Override{Component: release, Path: "resources.requests.cpu",
			Value: apiextensionsv1.JSON{Raw: []byte(`"` + cpu + `"`)}})
		rt.Spec.Overrides = kept
	})
}

// waitResolved waits for Resolved=True on the current generation and
// returns the candidate plan.
func waitResolved(ctx context.Context, t *testing.T) string {
	t.Helper()
	var digest string
	waitFor(t, resolveTimeout, "Resolved=True", func() bool {
		rt := getRuntime(ctx, t)
		c := cond(rt, v1alpha1.ConditionResolved)
		if c.Status == metav1.ConditionFalse && c.Reason != "Resolving" {
			t.Fatalf("resolution stalled: %s: %s", c.Reason, c.Message)
		}
		digest = rt.Status.CandidatePlanDigest
		return c.Status == metav1.ConditionTrue && digest != ""
	})
	return digest
}

func rolloutsTo(ctx context.Context, t *testing.T, digest string) []v1alpha1.RuntimeRollout {
	t.Helper()
	list := &v1alpha1.RuntimeRolloutList{}
	if err := k8s.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	var out []v1alpha1.RuntimeRollout
	for i := range list.Items {
		if list.Items[i].Spec.ToPlanDigest == digest {
			out = append(out, list.Items[i])
		}
	}
	return out
}

// awaitingApproval waits for the one rollout to digest to wait for an
// approval, with the runtime Stalled for it and nothing written yet.
func awaitingApproval(ctx context.Context, t *testing.T, digest string) *v1alpha1.RuntimeRollout {
	t.Helper()
	var ro *v1alpha1.RuntimeRollout
	waitFor(t, rolloutTimeout, "a rollout to "+digest+" awaiting approval", func() bool {
		ros := rolloutsTo(ctx, t, digest)
		if len(ros) > 1 {
			t.Fatalf("%d rollouts to %s", len(ros), digest)
		}
		if len(ros) == 1 && ros[0].Status.Phase == v1alpha1.PhaseAwaitingApproval {
			ro = &ros[0]
			return true
		}
		return false
	})
	// The rollout's status is written before the runtime's in the same
	// reconcile, so the runtime can trail it by one write: wait, bounded,
	// for Stalled to name it rather than reading it once.
	waitFor(t, 30*time.Second, "Stalled=AwaitingApproval naming "+ro.Name, func() bool {
		c := cond(getRuntime(ctx, t), v1alpha1.ConditionStalled)
		return c.Reason == "AwaitingApproval" && strings.Contains(c.Message, ro.Name)
	})
	return ro
}

// approve approves a rollout as the approver, the way `kubectl aicr approve`
// does: under its own username, with a reason, valid until the rollout's
// deadline.
func approve(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	ro := &v1alpha1.RuntimeRollout{}
	if err := approver.Get(ctx, client.ObjectKey{Name: name}, ro); err != nil {
		t.Fatalf("the approver cannot read %s: %v", name, err)
	}
	patch := client.MergeFrom(ro.DeepCopy())
	ro.Spec.Approval = &v1alpha1.Approval{ApprovedBy: approverUser, Reason: "qualification",
		ExpiresAt: ro.Spec.Deadline}
	if err := approver.Patch(ctx, ro, patch); err != nil {
		t.Fatalf("approve %s: %v", name, err)
	}
}

// waitReadyOn waits until the runtime is Ready with digest active.
func waitReadyOn(ctx context.Context, t *testing.T, digest string) {
	t.Helper()
	waitFor(t, rolloutTimeout, "Ready on "+digest, func() bool {
		rt := getRuntime(ctx, t)
		return cond(rt, v1alpha1.ConditionReady).Status == metav1.ConditionTrue && rt.Status.ActivePlanDigest == digest
	})
}

// rolledOut checks a transition ended with one Succeeded rollout.
func rolledOut(ctx context.Context, t *testing.T, digest string) {
	t.Helper()
	ros := rolloutsTo(ctx, t, digest)
	if len(ros) != 1 || ros[0].Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) {
		t.Fatalf("rollouts to %s: %d, outcome %v", digest, len(ros), ros)
	}
}

func application(ctx context.Context, t *testing.T) *unstructured.Unstructured {
	t.Helper()
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argocd.ApplicationGVK)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "argocd", Name: release}, app); err != nil {
		t.Fatalf("get Application: %v", err)
	}
	return app
}

// syncs counts the Application's completed syncs: each spec write that
// changes the target starts one.
func syncs(ctx context.Context, t *testing.T) int {
	t.Helper()
	history, _, _ := unstructured.NestedSlice(application(ctx, t).Object, "status", "history")
	return len(history)
}

// waitStepStarted waits until the rollout records a step in flight.
func waitStepStarted(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	waitFor(t, rolloutTimeout, name+" to start a step", func() bool {
		ro := &v1alpha1.RuntimeRollout{}
		return k8s.Get(ctx, client.ObjectKey{Name: name}, ro) == nil && ro.Status.Current != nil && ro.Status.Current.StartedAt != nil
	})
}

func operatorDeployment(ctx context.Context, t *testing.T) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: "aicr-operator"}, d); err != nil {
		t.Fatal(err)
	}
	return d
}

func operatorPods(ctx context.Context, t *testing.T) []corev1.Pod {
	t.Helper()
	pods := &corev1.PodList{}
	if err := k8s.List(ctx, pods, client.InNamespace(operatorNamespace), client.MatchingLabels{"app.kubernetes.io/name": "aicr-operator"}); err != nil {
		t.Fatal(err)
	}
	var live []corev1.Pod
	for _, p := range pods.Items {
		if p.DeletionTimestamp.IsZero() {
			live = append(live, p)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Name < live[j].Name })
	return live
}

// scaleOperator sets the operator's replicas and waits for them to be ready.
func scaleOperator(ctx context.Context, t *testing.T, replicas int32) {
	t.Helper()
	d := operatorDeployment(ctx, t)
	patch := client.MergeFrom(d.DeepCopy())
	d.Spec.Replicas = &replicas
	if err := k8s.Patch(ctx, d, patch); err != nil {
		t.Fatal(err)
	}
	waitOperatorReady(ctx, t, replicas)
}

func waitOperatorReady(ctx context.Context, t *testing.T, replicas int32) {
	t.Helper()
	waitFor(t, 3*time.Minute, "the operator to be ready", func() bool {
		d := operatorDeployment(ctx, t)
		return d.Status.ObservedGeneration == d.Generation && d.Status.ReadyReplicas == replicas &&
			d.Status.UpdatedReplicas == replicas && d.Status.Replicas == replicas
	})
}

// killPod deletes a pod at once, as a node failure or OOM kill would.
func killPod(ctx context.Context, t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if err := k8s.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatalf("kill %s: %v", pod.Name, err)
	}
}

// leader is the pod holding the operator's leader-election Lease.
func leader(ctx context.Context, t *testing.T) string {
	t.Helper()
	lease := &coordinationv1.Lease{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: defaults.LeaderElectionID}, lease); err != nil {
		return ""
	}
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	pod, _, _ := strings.Cut(*lease.Spec.HolderIdentity, "_")
	return pod
}

// recorded reports whether the operator recorded an Event with the reason
// about the named object, mentioning a string.
func recorded(ctx context.Context, t *testing.T, name, reason, mentions string) bool {
	t.Helper()
	list := &eventsv1.EventList{}
	if err := k8s.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Items {
		if e.Regarding.Name == name && e.Reason == reason && strings.Contains(e.Note, mentions) {
			return true
		}
	}
	return false
}

// run runs a repository tool, failing the test on error.
func run(ctx context.Context, t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "../../.."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
