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
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"oras.land/oras-go/v2/registry/remote/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/controller"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

const (
	crdsRelease  = "prometheus-operator-crds"
	nfdRelease   = "nfd"
	nfdNamespace = "node-feature-discovery"
	nfdCRD       = "nodefeatures.nfd.k8s-sigs.io"
	repairWait   = 3 * time.Minute
)

// waitFor polls until done reports true, failing with what after timeout.
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

// waitEngine waits for EngineAvailable with the status and reason.
func waitEngine(ctx context.Context, t *testing.T, status metav1.ConditionStatus, reason string) string {
	t.Helper()
	var message string
	waitFor(t, repairWait, "EngineAvailable="+string(status)+" "+reason, func() bool {
		c := meta.FindStatusCondition(getRuntime(ctx, t).Status.Conditions, v1alpha1.ConditionEngineAvailable)
		if c != nil && c.Status == status && c.Reason == reason {
			message = c.Message
			return true
		}
		return false
	})
	return message
}

// eventFor finds an Event on an Application in the Argo CD namespace with
// the reason and a note containing note.
func eventFor(ctx context.Context, t *testing.T, name, reason, note string) bool {
	t.Helper()
	list := &eventsv1.EventList{}
	if err := k8s.List(ctx, list, client.InNamespace("argocd")); err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Items {
		if e.Regarding.Kind == "Application" && e.Regarding.Name == name && e.Reason == reason && strings.Contains(e.Note, note) {
			t.Logf("Event %s on Application %s: %s", e.Reason, name, e.Note)
			return true
		}
	}
	return false
}

// editForcedBack changes the owned Application's values as another field
// manager would with kubectl edit: the operator forces them back to the
// plan and records an Event naming that manager.
func editForcedBack(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	want := comparedCPU(ctx, t)
	app := application(ctx, t)
	edit := []byte(`{"spec":{"source":{"helm":{"valuesObject":{"resources":{"requests":{"cpu":"11m"}}}}}}}`)
	if err := k8s.Patch(ctx, app, client.RawPatch(types.MergePatchType, edit), client.FieldOwner("e2e-editor")); err != nil {
		t.Fatalf("edit Application: %v", err)
	}
	waitFor(t, repairWait, "the edit was not forced back", func() bool {
		cpu, _, _ := unstructured.NestedString(application(ctx, t).Object, "spec", "source", "helm", "valuesObject", "resources", "requests", "cpu")
		return cpu == want
	})
	waitFor(t, time.Minute, "no FieldOwnershipOverridden Event naming e2e-editor", func() bool {
		return eventFor(ctx, t, release, controller.EventFieldOwnershipOverridden, "e2e-editor")
	})
	waitReadyOn(ctx, t, active)
}

// deletedByHand deletes the owned Application: without the resources
// finalizer its resources stay, and the operator writes it again.
func deletedByHand(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	deployment := &appsv1.Deployment{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: release, Name: release}, deployment); err != nil {
		t.Fatal(err)
	}
	app := application(ctx, t)
	if err := k8s.Delete(ctx, app); err != nil {
		t.Fatalf("delete Application: %v", err)
	}
	waitFor(t, repairWait, "the Application was not written again", func() bool {
		again := &unstructured.Unstructured{}
		again.SetGroupVersionKind(argocd.ApplicationGVK)
		err := k8s.Get(ctx, client.ObjectKey{Namespace: "argocd", Name: release}, again)
		return err == nil && again.GetUID() != app.GetUID()
	})
	after := &appsv1.Deployment{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(deployment), after); err != nil || after.UID != deployment.UID {
		t.Fatalf("deleting the Application by hand deleted its resources: %v", err)
	}
	waitFor(t, time.Minute, "no ApplicationRecreated Event", func() bool {
		return eventFor(ctx, t, release, controller.EventApplicationRecreated, active)
	})
	waitReadyOn(ctx, t, active)
}

// crdGone reports whether a CustomResourceDefinition is gone.
func crdGone(ctx context.Context, t *testing.T, name string) bool {
	t.Helper()
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	err := k8s.Get(ctx, client.ObjectKey{Name: name}, crd)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return apierrors.IsNotFound(err)
}

// deployments counts the Deployments in a namespace.
func deployments(ctx context.Context, t *testing.T, namespace string) int {
	t.Helper()
	list := &appsv1.DeploymentList{}
	if err := k8s.List(ctx, list, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

// removeCascades adds node-feature-discovery, then takes it out again: the
// rollout's Remove step, bound to the Application's UID, deletes it, Argo CD
// deletes its workloads in foreground order, and the step waits for
// retirement. Argo CD never deletes CRDs on a cascade, so NFD's stay.
func removeCascades(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	base := getRuntime(ctx, t).Spec.Overrides
	// The topology updater needs kubelet pod resources, which kind does not
	// serve; it is not what this test is about.
	setOverrides(ctx, t, base, override(nfdRelease, "enabled", "true"), override(nfdRelease, "topologyUpdater.enable", "false"))
	added := waitRolledOut(ctx, t, active)
	if deployments(ctx, t, nfdNamespace) == 0 || crdGone(ctx, t, nfdCRD) {
		t.Fatal("node-feature-discovery rolled out without its Deployments or CRDs")
	}
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argocd.ApplicationGVK)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "argocd", Name: nfdRelease}, app); err != nil {
		t.Fatal(err)
	}

	setOverrides(ctx, t, base)
	if back := waitRolledOut(ctx, t, added); back != active {
		t.Errorf("removing the component resolved to %s, want %s", back, active)
	}
	var removal *v1alpha1.RuntimeRollout
	for _, ro := range rolloutsTo(ctx, t, active) {
		if ro.Spec.FromPlanDigest == added {
			removal = ro.DeepCopy()
		}
	}
	if removal == nil {
		t.Fatal("no rollout from the added plan back")
	}
	removed := false
	for _, s := range removal.Spec.Steps {
		removed = removed || (s.Operation == v1alpha1.OperationRemove && s.Release == nfdRelease && s.UID == string(app.GetUID()))
	}
	for _, s := range removal.Status.Releases {
		removed = removed && (s.ID != nfdRelease || s.State == v1alpha1.ReleaseRemoved)
	}
	if !removed {
		t.Fatalf("rollout %s steps %+v, releases %+v", removal.Name, removal.Spec.Steps, removal.Status.Releases)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(app), app); !apierrors.IsNotFound(err) {
		t.Errorf("the removed Application still exists: %v", err)
	}
	if n := deployments(ctx, t, nfdNamespace); n != 0 {
		t.Errorf("the cascade left %d Deployments", n)
	}
	if crdGone(ctx, t, nfdCRD) {
		t.Error("the cascade deleted a CRD; Argo CD v3.4.2 never should")
	}
}

// foreignConflict creates an Application without the operator's ownership
// under a plan release's name: the install conflicts, the rollout fails,
// and the foreign object is never written.
func foreignConflict(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": crdsRelease, "namespace": "argocd"},
		"spec": map[string]any{
			"project":     "default",
			"source":      map[string]any{"repoURL": "https://charts.example.invalid", "chart": "none", "targetRevision": "0.0.1"},
			"destination": map[string]any{"server": argocd.InClusterServer, "namespace": "monitoring"},
		},
	}}
	foreign.SetGroupVersionKind(argocd.ApplicationGVK)
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatalf("create foreign Application: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.WithoutCancel(ctx), foreign) })

	setOverrideFor(ctx, t, crdsRelease, "enabled", "true")
	waitFor(t, resolveTimeout+rolloutTimeout, "the runtime did not stall on the foreign Application", func() bool {
		c := meta.FindStatusCondition(getRuntime(ctx, t).Status.Conditions, v1alpha1.ConditionStalled)
		if c != nil && c.Status == metav1.ConditionTrue && c.Reason == "RolloutFailed" && strings.Contains(c.Message, "ownership") {
			t.Logf("stalled: %s: %s", c.Reason, c.Message)
			return true
		}
		return false
	})
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil {
		t.Fatal(err)
	}
	for _, m := range foreign.GetManagedFields() {
		if strings.HasPrefix(m.Manager, argocd.FieldManager) {
			t.Errorf("the operator wrote the foreign Application (%s)", m.Manager)
		}
	}
	if foreign.GetAnnotations()[v1alpha1.AnnotationPlanDigest] != "" {
		t.Error("the foreign Application gained the operator's annotations")
	}
	if err := k8s.Delete(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	setOverrideFor(ctx, t, crdsRelease, "enabled", "false")
	waitReadyOn(ctx, t, active)
}

// projectMissing deletes the aicr AppProject: EngineAvailable turns False,
// and a new candidate gets no rollout and no engine write until the project
// is back.
func projectMissing(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	project := &unstructured.Unstructured{}
	project.SetGroupVersionKind(argocd.AppProjectGVK)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "argocd", Name: "aicr"}, project); err != nil {
		t.Fatal(err)
	}
	// The restored project keeps Helm's ownership metadata, as the chart
	// would recreate it, so a later reinstall still owns it.
	restore := &unstructured.Unstructured{Object: map[string]any{"spec": project.Object["spec"]}}
	restore.SetGroupVersionKind(argocd.AppProjectGVK)
	restore.SetNamespace("argocd")
	restore.SetName("aicr")
	restore.SetLabels(project.GetLabels())
	restore.SetAnnotations(project.GetAnnotations())
	if err := k8s.Delete(ctx, project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Create(context.WithoutCancel(ctx), restore.DeepCopy()) })
	t.Logf("EngineAvailable=False: %s", waitEngine(ctx, t, metav1.ConditionFalse, argocd.ReasonProjectMissing))

	before := applicationVersion(ctx, t)
	cpu := comparedCPU(ctx, t)
	setCPU(ctx, t, `"35m"`)
	candidate := waitResolved(ctx, t)
	assertNoEngineWrites(ctx, t, before, active)
	if ros := rolloutsTo(ctx, t, candidate); len(ros) != 0 {
		t.Fatalf("rollout %s started without the AppProject", ros[0].Name)
	}

	if err := k8s.Create(ctx, restore); err != nil {
		t.Fatal(err)
	}
	waitEngine(ctx, t, metav1.ConditionTrue, controller.ReasonEngineAvailable)
	next := waitRolledOut(ctx, t, active)
	setCPU(ctx, t, `"`+cpu+`"`)
	if back := waitRolledOut(ctx, t, next); back != active {
		t.Errorf("restoring the CPU request resolved to %s, want %s", back, active)
	}
}

// sharedSettingsKept sets an administrator's ignore rule for Secrets in
// argocd-cm, as another Argo CD user would. The operator's Applications carry
// their own comparison exceptions, which the pinned Argo CD accepts and
// respects on sync, and the administrator's rule survives the operator.
func sharedSettingsKept(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	const key, rule = "resource.customizations.ignoreDifferences.Secret", "jsonPointers:\n- /data/e2e\n"
	cm := &corev1.ConfigMap{}
	cm.Namespace, cm.Name = "argocd", "argocd-cm"
	set := func(value any) {
		raw, err := json.Marshal(map[string]any{"data": map[string]any{key: value}})
		if err != nil {
			t.Fatal(err)
		}
		if err = k8s.Patch(ctx, cm, client.RawPatch(types.MergePatchType, raw), client.FieldOwner("e2e-admin")); err != nil {
			t.Fatal(err)
		}
	}
	set(rule)
	defer set(nil)

	app := application(ctx, t)
	ignores, _, _ := unstructured.NestedSlice(app.Object, "spec", "ignoreDifferences")
	opts, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "syncPolicy", "syncOptions")
	if len(ignores) == 0 || !slices.Contains(opts, "RespectIgnoreDifferences=true") {
		t.Fatalf("Application %s: ignoreDifferences %v, syncOptions %v", app.GetName(), ignores, opts)
	}
	waitReadyOn(ctx, t, active)
	got := &corev1.ConfigMap{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(cm), got); err != nil || got.Data[key] != rule {
		t.Fatalf("the administrator's argocd-cm rule = %q, %v", got.Data[key], err)
	}
}

// verifyRerunsGate checks the Verify mechanism against the pinned Argo CD
// with a readiness-style release the test packages itself: a ConfigMap and a
// post-install and post-upgrade hook Job. The install syncs explicitly and
// runs the gate. A later Verify step is not ready on that run; Verify starts
// a sync, Argo CD runs the gate Job again, and the step becomes ready.
func verifyRerunsGate(ctx context.Context, t *testing.T) {
	t.Helper()
	image := operatorArg(ctx, t, "--aicr-image")
	files := map[string][]byte{
		"Chart.yaml":  []byte("apiVersion: v2\nname: e2e-readiness\nversion: 1.0.0\n"),
		"values.yaml": []byte("{}\n"),
		// Like AICR's readiness charts: ordinary resources and the gate Job
		// as a post-install and post-upgrade hook.
		"templates/config.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: e2e-readiness\ndata:\n  gate: e2e\n"),
		"templates/gate.yaml": []byte(`apiVersion: batch/v1
kind: Job
metadata:
  name: e2e-readiness-gate
  annotations:
    helm.sh/hook: post-install,post-upgrade
    helm.sh/hook-delete-policy: before-hook-creation
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: gate
          image: ` + image + `
          args: ["--version"]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
`),
	}
	var pushed *registry.MirroredChart
	withRegistry(ctx, t, func(base string) {
		host := strings.TrimPrefix(base, "http://")
		creds, err := registry.DockerConfigCredentials(registryAuth)
		if err != nil {
			t.Fatal(err)
		}
		reg := registry.New(registry.Options{PlainHTTPHosts: []string{host},
			Credentials: func(ctx context.Context, _ string) (auth.Credential, error) { return creds(ctx, registryHost) }})
		repo, err := registry.ParseRepository("oci://" + host + "/aicr/wrappers/e2e-readiness")
		if err != nil {
			t.Fatal(err)
		}
		if pushed, err = reg.PushWrapper(ctx, repo, files); err != nil {
			t.Fatalf("push the gate chart: %v", err)
		}
	})

	adapter := &argocd.Adapter{Writer: k8s, Reader: k8s, Mapper: k8s.RESTMapper(),
		Target: argocd.Target{Namespace: "argocd", Project: "aicr", RuntimeName: "verify-e2e"}}
	p := &plan.Document{RuntimeUID: "5d0c1f9e-0a6b-4c1e-9d0e-3f7a2b8c4e61", DesiredStateDigest: plan.DigestOf([]byte("verify-e2e"))}
	rel := &plan.Release{ID: "e2e-readiness", Component: "e2e", Phase: plan.PhaseReadiness, ReadinessFor: "e2e", TargetNamespace: "aicr-e2e",
		Chart: plan.Chart{Source: plan.ChartWrapper, Repository: "oci://" + registryHost + "/aicr/wrappers/e2e-readiness", Digest: pushed.Digest}}
	values := []byte(`{}`)
	t.Cleanup(func() {
		bg := context.WithoutCancel(ctx)
		app := &unstructured.Unstructured{}
		app.SetGroupVersionKind(argocd.ApplicationGVK)
		app.SetNamespace("argocd")
		app.SetName(rel.ID)
		_ = k8s.Delete(bg, app)
		_ = k8s.DeleteAllOf(bg, &batchv1.Job{}, client.InNamespace("aicr-e2e"), client.PropagationPolicy(metav1.DeletePropagationBackground))
	})

	ready := func(since time.Time, verify bool) {
		t.Helper()
		waitFor(t, rolloutTimeout, "the gate release is not ready", func() bool {
			obs, err := adapter.Observe(ctx, p, rel, values, since, verify)
			if err != nil {
				t.Fatal(err)
			}
			if verify && obs.Reason == argocd.ReasonSyncNotStarted {
				if err = adapter.Verify(ctx, p, rel, values); err != nil {
					t.Fatal(err)
				}
			}
			if obs.State == argocd.StateFailed {
				t.Fatalf("gate release failed: %s", obs.Reason)
			}
			return obs.State == argocd.StateReady
		})
	}
	gateJob := func() *batchv1.Job {
		t.Helper()
		job := &batchv1.Job{}
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: "aicr-e2e", Name: "e2e-readiness-gate"}, job); err != nil {
			t.Fatalf("gate Job: %v", err)
		}
		return job
	}

	// The rollout controller syncs every step of a readiness release
	// explicitly; the install is one.
	start := time.Now()
	if err := adapter.Verify(ctx, p, rel, values); err != nil {
		t.Fatal(err)
	}
	ready(start, true)
	first := gateJob()

	time.Sleep(time.Second)
	verifyStart := time.Now()
	if obs, err := adapter.Observe(ctx, p, rel, values, verifyStart, true); err != nil || obs.Reason != argocd.ReasonSyncNotStarted {
		t.Fatalf("before Verify: Observe() = %+v, %v; the first gate run must not count", obs, err)
	}
	if err := adapter.Verify(ctx, p, rel, values); err != nil {
		t.Fatal(err)
	}
	ready(verifyStart, true)
	second := gateJob()
	if second.UID == first.UID || second.CreationTimestamp.Time.Before(verifyStart.Truncate(time.Second)) {
		t.Fatalf("Verify did not run the gate again: Job %s created %s, Verify at %s", second.UID, second.CreationTimestamp, verifyStart)
	}
	t.Logf("gate Job re-ran: %s -> %s", first.UID, second.UID)
}

// operatorArg returns the value of the operator's flag.
func operatorArg(ctx context.Context, t *testing.T, flag string) string {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: "aicr-operator"}, d); err != nil {
		t.Fatal(err)
	}
	for _, a := range d.Spec.Template.Spec.Containers[0].Args {
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v
		}
	}
	t.Fatalf("operator has no %s", flag)
	return ""
}

// rebuildMatches rebuilds the active plan from the live Applications, as
// re-adoption and a lost plan ConfigMap need: with automated sync
// suspended on the Application, the rebuilt digest is the active plan's.
func rebuildMatches(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	app := application(ctx, t)
	suspend := []byte(`{"spec":{"syncPolicy":{"automated":{"enabled":false}}}}`)
	if err := k8s.Patch(ctx, app, client.RawPatch(types.MergePatchType, suspend), client.FieldOwner("e2e-admin")); err != nil {
		t.Fatalf("suspend automated sync: %v", err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(argocd.ApplicationGVK.GroupVersion().WithKind("ApplicationList"))
	if err := k8s.List(ctx, list, client.InNamespace("argocd"), client.MatchingLabels{v1alpha1.LabelRuntime: v1alpha1.SingletonName}); err != nil {
		t.Fatal(err)
	}
	target := argocd.Target{Namespace: "argocd", Project: "aicr", RuntimeName: v1alpha1.SingletonName}
	doc, _, err := argocd.Rebuild(target, string(getRuntime(ctx, t).UID), list.Items)
	if err != nil {
		t.Fatalf("Rebuild() = %v", err)
	}
	if doc.DesiredStateDigest != active {
		t.Fatalf("rebuilt digest %s, want the active plan %s", doc.DesiredStateDigest, active)
	}
	t.Logf("rebuilt plan %s from %d live Applications", doc.DesiredStateDigest, len(list.Items))
	// The suspension was not the operator's: drift repair undoes it.
	waitFor(t, repairWait, "the foreign suspension was not repaired", func() bool {
		enabled, found, _ := unstructured.NestedBool(application(ctx, t).Object, "spec", "syncPolicy", "automated", "enabled")
		return found && enabled
	})
	waitReadyOn(ctx, t, active)
}
