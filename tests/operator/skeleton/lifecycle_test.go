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
	"bytes"
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
)

// suspendResumes sets spec.suspendEngine on the real Argo CD: the owned
// Application's automated sync goes off and the runtime reports
// EngineSuspended; clearing it turns sync back on and the runtime is Ready
// again.
func suspendResumes(ctx context.Context, t *testing.T) {
	setSuspendEngine := func(suspended bool) {
		rt := getRuntime(ctx, t)
		patch := client.MergeFrom(rt.DeepCopy())
		rt.Spec.SuspendEngine = suspended
		if err := k8s.Patch(ctx, rt, patch); err != nil {
			t.Fatalf("set suspendEngine: %v", err)
		}
	}
	automated := func() bool {
		enabled, found, _ := unstructured.NestedBool(application(ctx, t).Object, "spec", "syncPolicy", "automated", "enabled")
		return !found || enabled
	}
	readyReason := func() string {
		c := meta.FindStatusCondition(getRuntime(ctx, t).Status.Conditions, v1alpha1.ConditionReady)
		if c == nil {
			return ""
		}
		return c.Reason
	}

	setSuspendEngine(true)
	t.Cleanup(func() { setSuspendEngine(false) })
	waitFor(t, resolveTimeout, "automated sync off and Ready=False EngineSuspended", func() bool {
		return !automated() && readyReason() == "EngineSuspended"
	})
	setSuspendEngine(false)
	waitFor(t, rolloutTimeout, "automated sync on and Ready=True again", func() bool {
		c := meta.FindStatusCondition(getRuntime(ctx, t).Status.Conditions, v1alpha1.ConditionReady)
		return automated() && c != nil && c.Status == "True"
	})
}

// runtimeGone waits until no runtime exists.
func runtimeGone(ctx context.Context, t *testing.T) {
	t.Helper()
	waitFor(t, 2*time.Minute, "the runtime to be deleted", func() bool {
		return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, &v1alpha1.ClusterRuntime{}))
	})
}

// workloadPods are the UIDs of the release's pods: unchanged UIDs mean
// nothing touched the workload.
func workloadPods(ctx context.Context, t *testing.T) []string {
	t.Helper()
	pods := &corev1.PodList{}
	if err := k8s.List(ctx, pods, client.InNamespace(release)); err != nil {
		t.Fatal(err)
	}
	uids := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		uids = append(uids, string(p.UID))
	}
	slices.Sort(uids)
	return uids
}

// abandon removes the runtime's cleanup finalizer by hand and deletes the
// runtime as it then is, so its deletion policy never runs. The operator adds
// the finalizer back to a live runtime as soon as it sees it missing, so the
// delete is preconditioned on the version without it, and a conflict starts
// over.
func abandon(ctx context.Context, t *testing.T) {
	t.Helper()
	for range 10 {
		live := getRuntime(ctx, t)
		base := live.DeepCopy()
		live.Finalizers = nil
		err := k8s.Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err == nil {
			rv := live.ResourceVersion
			err = k8s.Delete(ctx, live, client.Preconditions{ResourceVersion: &rv})
		}
		switch {
		case err == nil, apierrors.IsNotFound(err):
			return
		case apierrors.IsConflict(err):
			t.Logf("the operator added its cleanup finalizer back before the delete; retrying: %v", err)
		default:
			t.Fatal(err)
		}
	}
	t.Fatal("the operator added its cleanup finalizer back before every delete")
}

func stalledWith(ctx context.Context, t *testing.T, reason, mentions string) {
	t.Helper()
	waitFor(t, resolveTimeout, "Stalled "+reason+" mentioning "+mentions, func() bool {
		c := meta.FindStatusCondition(getRuntime(ctx, t).Status.Conditions, v1alpha1.ConditionStalled)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == reason && strings.Contains(c.Message, mentions)
	})
}

// adopted waits for a succeeded Adopt rollout from the earlier runtime and
// the rebuilt plan active with digest.
func adopted(ctx context.Context, t *testing.T, earlier types.UID, digest string) {
	t.Helper()
	waitFor(t, rolloutTimeout, "adoption of runtime "+string(earlier)+" as plan "+digest, func() bool {
		list := &v1alpha1.RuntimeRolloutList{}
		if err := k8s.List(ctx, list); err != nil {
			t.Fatal(err)
		}
		done := false
		for _, ro := range list.Items {
			done = done || (ro.Annotations[v1alpha1.AnnotationAdoptedFrom] == string(earlier) &&
				ro.Labels[v1alpha1.LabelOutcome] == string(v1alpha1.PhaseSucceeded))
		}
		return done && getRuntime(ctx, t).Status.ActivePlanDigest == digest
	})
}

// run runs a command against the kind cluster from the repository root.
func run(ctx context.Context, t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "../../.."
	cmd.Env = append(os.Environ(), "AICR_E2E_VERSION="+aicrVersion)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out.String())
	}
}

// lifecycleAdopts walks the safe lifecycle on kind with the real Argo CD:
//
//  1. the cleanup finalizer removed by hand deletes and annotates nothing;
//  2. a new runtime stalls on the earlier runtime's objects until it adopts
//     them by UID, and the rebuilt plan has the original digest;
//  3. deletion with Orphan annotates the objects and Argo CD keeps them
//     healthy, with the workload untouched;
//  4. after helm uninstall and deleting the operator namespace, a reinstall
//     adopts from the engine objects alone; an out-of-band edit stalls the
//     adoption naming the object until it is undone.
func lifecycleAdopts(ctx context.Context, t *testing.T) {
	first := getRuntime(ctx, t)
	app := application(ctx, t)
	appUID, digest := app.GetUID(), app.GetAnnotations()[v1alpha1.AnnotationPlanDigest]
	pods := workloadPods(ctx, t)
	if digest != first.Status.ActivePlanDigest || len(pods) == 0 {
		t.Fatalf("start: Application at %s, active %s, %d pods", digest, first.Status.ActivePlanDigest, len(pods))
	}

	// 1. The finalizer removed by hand: nothing cascades.
	abandon(ctx, t)
	runtimeGone(ctx, t)
	time.Sleep(10 * time.Second)
	app = application(ctx, t)
	a := app.GetAnnotations()
	untouched := app.GetUID() == appUID && a[v1alpha1.AnnotationRuntimeUID] == string(first.UID) && a[v1alpha1.AnnotationOrphanedFrom] == ""
	if !untouched || !slices.Equal(workloadPods(ctx, t), pods) {
		t.Fatalf("after the finalizer was removed by hand: Application %s %v, pods %v", app.GetUID(), a, workloadPods(ctx, t))
	}

	// 2. A new runtime never adopts silently; it adopts by the old UID.
	createRuntime(ctx, t)
	stalledWith(ctx, t, "OrphanedObjectsPresent", string(first.UID))
	second := getRuntime(ctx, t)
	patch := client.MergeFrom(second.DeepCopy())
	second.Spec.Adoption = &v1alpha1.Adoption{RuntimeUID: string(first.UID)}
	if err := k8s.Patch(ctx, second, patch); err != nil {
		t.Fatal(err)
	}
	adopted(ctx, t, first.UID, digest)
	if app = application(ctx, t); app.GetUID() != appUID || app.GetAnnotations()[v1alpha1.AnnotationRuntimeUID] != string(second.UID) {
		t.Fatalf("after adoption: Application %s %v", app.GetUID(), app.GetAnnotations())
	}

	// 3. Orphan: the engine keeps reconciling, the workload is untouched.
	if err := k8s.Delete(ctx, getRuntime(ctx, t)); err != nil {
		t.Fatal(err)
	}
	runtimeGone(ctx, t)
	app = application(ctx, t)
	health, _, _ := unstructured.NestedString(app.Object, "status", "health", "status")
	if app.GetAnnotations()[v1alpha1.AnnotationOrphanedFrom] != string(second.UID) || health != "Healthy" || !slices.Equal(workloadPods(ctx, t), pods) {
		t.Fatalf("after Orphan: annotations %v, health %q, pods %v", app.GetAnnotations(), health, workloadPods(ctx, t))
	}

	// 4. Uninstall, delete the operator namespace, reinstall.
	kubeconfig := os.Getenv("AICR_OPERATOR_E2E_KUBECONFIG")
	run(ctx, t, "helm", "uninstall", "aicr-operator", "--namespace", operatorNamespace, "--kubeconfig", kubeconfig, "--wait")
	if err := k8s.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: operatorNamespace}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Minute, "the operator namespace to be deleted", func() bool {
		return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: operatorNamespace}, &corev1.Namespace{}))
	})
	run(ctx, t, "tools/operator-skeleton", "install")

	// An out-of-band edit stalls the adoption, naming the object.
	edit := []byte(`{"spec":{"source":{"helm":{"valuesObject":{"resources":{"requests":{"cpu":"41m"}}}}}}}`)
	if err := k8s.Patch(ctx, application(ctx, t), client.RawPatch(types.MergePatchType, edit), client.FieldOwner("e2e-editor")); err != nil {
		t.Fatal(err)
	}
	createRuntimeWith(ctx, t, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Adoption = &v1alpha1.Adoption{RuntimeUID: string(second.UID)}
	})
	stalledWith(ctx, t, "AdoptionBlocked", "argocd/"+release)
	cpu, _, _ := unstructured.NestedString(app.Object, "spec", "source", "helm", "valuesObject", "resources", "requests", "cpu")
	undo := []byte(`{"spec":{"source":{"helm":{"valuesObject":{"resources":{"requests":{"cpu":"` + cpu + `"}}}}}}}`)
	if err := k8s.Patch(ctx, application(ctx, t), client.RawPatch(types.MergePatchType, undo), client.FieldOwner("e2e-editor")); err != nil {
		t.Fatal(err)
	}
	adopted(ctx, t, second.UID, digest)
	third := getRuntime(ctx, t)
	if app = application(ctx, t); app.GetUID() != appUID || app.GetAnnotations()[v1alpha1.AnnotationRuntimeUID] != string(third.UID) {
		t.Fatalf("after the reinstall: Application %s %v", app.GetUID(), app.GetAnnotations())
	}
	t.Logf("adopted plan %s from the engine objects alone, through runtimes %s, %s and %s", digest, first.UID, second.UID, third.UID)
}
