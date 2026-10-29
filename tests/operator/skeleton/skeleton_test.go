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

// Package skeleton runs the walking skeleton against the operator
// deployed on kind with the pinned Argo CD: one ClusterRuntime becomes one
// Argo CD Application through AICR recipe and render Jobs, a mirrored chart,
// a plan and a rollout. Run with `make operator-skeleton`.
package skeleton

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
)

const (
	operatorNamespace = "aicr-system"
	jobsNamespace     = "aicr-jobs"
	systemLabel       = "skeleton.operator.aicr.run/system"
	gpuLabel          = "skeleton.operator.aicr.run/accelerated"
	release           = "cert-manager"
	resolveTimeout    = 10 * time.Minute
	rolloutTimeout    = 15 * time.Minute
)

var (
	k8s          client.Client
	clientset    kubernetes.Interface
	restConfig   *rest.Config
	aicrVersion  string
	registryAuth []byte
)

func TestMain(m *testing.M) {
	path := os.Getenv("AICR_OPERATOR_E2E_KUBECONFIG")
	aicrVersion = os.Getenv("AICR_OPERATOR_E2E_AICR_VERSION")
	if path == "" || aicrVersion == "" {
		msg := "AICR_OPERATOR_E2E_KUBECONFIG and AICR_OPERATOR_E2E_AICR_VERSION are required (run `make operator-skeleton`)"
		if os.Getenv("OPERATOR_E2E_REQUIRED") != "" {
			fmt.Fprintln(os.Stderr, msg)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "skipping skeleton: "+msg)
		os.Exit(0)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load kubeconfig %s: %v\n", path, err)
		os.Exit(1)
	}
	restConfig = cfg
	if registryAuth, err = os.ReadFile(os.Getenv("AICR_OPERATOR_E2E_REGISTRY_AUTH")); err != nil {
		fmt.Fprintf(os.Stderr, "read registry credentials: %v\n", err)
		os.Exit(1)
	}
	if clientset, err = kubernetes.NewForConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "create clientset: %v\n", err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err == nil {
		err = v1alpha1.AddToScheme(scheme)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "register scheme: %v\n", err)
		os.Exit(1)
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintf(os.Stderr, "create client: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestSkeleton runs the acceptance criteria in order against the singleton
// runtime; each step starts from the state the previous one left.
func TestSkeleton(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	t.Cleanup(cancel)
	reset(ctx, t)
	labelNode(ctx, t)

	var first string
	t.Run("one runtime yields one Application at the plan digest", func(t *testing.T) {
		createRuntime(ctx, t)
		first = waitRolledOut(ctx, t, "")
		assertApplication(ctx, t, first, "")
	})
	t.Run("the AICR CLI renders the operator's bundle with zero diff, and Argo CD gets its values", func(t *testing.T) {
		reproduceMatches(ctx, t, first, true)
	})
	t.Run("an override change is not Ready until the comparison is current", func(t *testing.T) {
		setCPU(ctx, t, `"40m"`)
		second := waitRolledOutObserving(ctx, t, first, "40m")
		assertApplication(ctx, t, second, "40m")
		first = second
	})
	t.Run("suspendEngine turns automated sync off and back on", func(t *testing.T) {
		suspendResumes(ctx, t)
	})
	t.Run("a failed stage writes nothing to the engine", func(t *testing.T) {
		before := applicationVersion(ctx, t)
		setOverrideFor(ctx, t, "no-such-component", "x", `1`)
		reason := waitStalled(ctx, t, "no-such-component")
		if !strings.HasPrefix(reason, "AICR") {
			t.Errorf("Stalled reason = %q, want an AICR exit category", reason)
		}
		assertNoEngineWrites(ctx, t, before, first)
		removeOverride(ctx, t, "no-such-component")
		waitReadyOn(ctx, t, first)
	})
	t.Run("an AICR version mismatch stalls with zero engine writes", func(t *testing.T) {
		before := applicationVersion(ctx, t)
		for declared, message := range map[string]string{"v1.0.1": "aicr.version is v1.0.1", "v2.0.0": "v2.0.0 is outside"} {
			setOperatorAICRVersion(ctx, t, declared)
			if reason := waitStalled(ctx, t, message); reason != aicrjob.ReasonVersionMismatch {
				t.Errorf("declared %s: Stalled reason = %q, want %s", declared, reason, aicrjob.ReasonVersionMismatch)
			}
			assertNoEngineWrites(ctx, t, before, first)
		}
		setOperatorAICRVersion(ctx, t, aicrVersion)
		waitReadyOn(ctx, t, first)
	})
	t.Run("a killed Job is replaced by a new run", func(t *testing.T) {
		setCPU(ctx, t, `"30m"`)
		killed := killRunningJob(ctx, t, aicrjob.StageRender)
		if next := nextRun(ctx, t, aicrjob.StageRender, killed); next == killed {
			t.Fatalf("no new render run after killing %s", killed)
		}
		third := waitRolledOut(ctx, t, first)
		assertApplication(ctx, t, third, "30m")
		first = third
	})
	t.Run("overrides AICR rejects stall with its exit category and zero engine writes", func(t *testing.T) {
		rejectedOverrides(ctx, t, first)
	})
	t.Run("an unknown override path stalls with zero engine writes unless additive", func(t *testing.T) {
		typoProtection(ctx, t, first)
	})
	t.Run("identical inputs resolve to the same plan after the status cache is lost", func(t *testing.T) {
		statusCacheLoss(ctx, t, first)
	})
	t.Run("the active plan rebuilds from its live Applications, suspended or not", func(t *testing.T) {
		rebuildMatches(ctx, t, first)
	})
	t.Run("a kubectl edit on an owned Application is forced back with an Event", func(t *testing.T) {
		editForcedBack(ctx, t, first)
	})
	t.Run("deleting an owned Application by hand leaves its resources", func(t *testing.T) {
		deletedByHand(ctx, t, first)
	})
	t.Run("an approved Remove step cascades and observes retirement", func(t *testing.T) {
		removeCascades(ctx, t, first)
	})
	t.Run("a foreign Application with a release's name conflicts with no write", func(t *testing.T) {
		foreignConflict(ctx, t, first)
	})
	t.Run("a missing AppProject sets EngineAvailable=False with zero writes", func(t *testing.T) {
		projectMissing(ctx, t, first)
	})
	t.Run("Applications carry their own comparison exceptions; argocd-cm is left alone", func(t *testing.T) {
		sharedSettingsKept(ctx, t, first)
	})
	t.Run("Verify re-runs a readiness gate through an explicit sync", func(t *testing.T) {
		verifyRerunsGate(ctx, t)
	})
	t.Run("discovery resolves from a snapshot pinned to the accelerated group", func(t *testing.T) {
		discoveryResolves(ctx, t, first)
	})
	t.Run("a failed deployment validation fails the rollout and promotes nothing", func(t *testing.T) {
		validationGates(ctx, t, first)
	})
	t.Run("the full lane recipe resolves with every chart from the runtime registry", func(t *testing.T) {
		resolveFullLane(ctx, t)
	})
	t.Run("tampered registry content blocks the plan with zero engine writes", func(t *testing.T) {
		tamperBlocksPlan(ctx, t, first)
	})
	t.Run("the chart's admission policies hold on the real Argo CD and RBAC", func(t *testing.T) {
		admissionHolds(ctx, t)
	})
	t.Run("no credential appears in plans, status, logs, Events or Jobs", func(t *testing.T) {
		assertNoCredentialLeaks(ctx, t)
	})
	t.Run("orphaned or abandoned objects are adopted from the engine objects alone", func(t *testing.T) {
		lifecycleAdopts(ctx, t)
	})
}

// reset removes what an earlier run left: the runtime, its rollouts, plans
// and Applications.
func reset(ctx context.Context, t *testing.T) {
	t.Helper()
	// The operator runs the deletion policy (Orphan) and removes its cleanup
	// finalizer; the objects it orphans are deleted below.
	_ = k8s.Delete(ctx, &v1alpha1.ClusterRuntime{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName}})
	waitFor(t, 2*time.Minute, "the earlier runtime to be deleted", func() bool {
		return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, &v1alpha1.ClusterRuntime{}))
	})
	_ = k8s.DeleteAllOf(ctx, &v1alpha1.RuntimeRollout{})
	_ = k8s.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace(operatorNamespace),
		client.MatchingLabels{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue})
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argocd.ApplicationGVK)
	_ = k8s.DeleteAllOf(ctx, app, client.InNamespace("argocd"), client.MatchingLabels{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue})
	setOperatorAICRVersion(ctx, t, aicrVersion)
}

// labelNode puts the kind control plane in the system group and the
// workers in the accelerated group (tools/operator-kind creates both).
func labelNode(ctx context.Context, t *testing.T) {
	t.Helper()
	nodes := &corev1.NodeList{}
	if err := k8s.List(ctx, nodes); err != nil || len(nodes.Items) < 2 {
		t.Fatalf("list nodes: %v (%d nodes; recreate the cluster with make operator-kind-up)", err, len(nodes.Items))
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		patch := client.MergeFrom(n.DeepCopy())
		delete(n.Labels, systemLabel)
		delete(n.Labels, gpuLabel)
		if _, cp := n.Labels["node-role.kubernetes.io/control-plane"]; cp {
			n.Labels[systemLabel] = "true"
		} else {
			n.Labels[gpuLabel] = "true"
		}
		if err := k8s.Patch(ctx, n, patch); err != nil {
			t.Fatalf("label node: %v", err)
		}
	}
}

// createRuntime creates the runtime for the lane criteria with every
// component disabled except cert-manager, so exactly one release renders.
func createRuntime(ctx context.Context, t *testing.T) {
	t.Helper()
	createRuntimeWith(ctx, t, nil)
}

// createRuntimeWith creates the skeleton's runtime, changed by mutate.
func createRuntimeWith(ctx context.Context, t *testing.T, mutate func(*v1alpha1.ClusterRuntime)) {
	t.Helper()
	// The lane recipe of the AICR release under test names the components to
	// disable; every release the suite runs has golden fixtures.
	raw, err := os.ReadFile("../../../pkg/operator/aicrjob/testdata/" + aicrVersion + "/recipe-h100-eks-ubuntu-training-kubeflow.json")
	if err != nil {
		t.Fatalf("no lane recipe fixture for AICR %s: %v", aicrVersion, err)
	}
	recipe, err := aicrjob.ParseRecipeResult(raw, aicrVersion)
	if err != nil {
		t.Fatal(err)
	}
	var overrides []v1alpha1.Override
	for _, c := range recipe.DeploymentOrder {
		if c != release {
			overrides = append(overrides, v1alpha1.Override{Component: c, Path: "enabled", Value: apiextensionsv1.JSON{Raw: []byte("false")}})
		}
	}
	rt := &v1alpha1.ClusterRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName},
		Spec: v1alpha1.ClusterRuntimeSpec{
			Intent: "training", Platform: "kubeflow",
			Criteria: &v1alpha1.DeclaredCriteria{Service: "eks", Accelerator: "h100", OS: "ubuntu"},
			Placement: v1alpha1.Placement{
				System:      v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{systemLabel: "true"}},
				Accelerated: v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{gpuLabel: "true"}},
			},
			Overrides: overrides,
			Rollout: v1alpha1.RolloutPolicy{
				StabilizationWindow: &metav1.Duration{Duration: time.Second},
				ActionTimeout:       &metav1.Duration{Duration: 10 * time.Minute},
				Deadline:            &metav1.Duration{Duration: time.Hour},
			},
			Validation: v1alpha1.ValidationPolicy{Deployment: v1alpha1.DeploymentValidationDisabled},
		},
	}
	if mutate != nil {
		mutate(rt)
	}
	if err = k8s.Create(ctx, rt); err != nil {
		t.Fatalf("create runtime: %v", err)
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

// setCPU overrides the release's CPU request; value is JSON.
func setCPU(ctx context.Context, t *testing.T, value string) {
	t.Helper()
	setOverrideFor(ctx, t, release, "resources.requests.cpu", value)
}

func setOverrideFor(ctx context.Context, t *testing.T, component, path, value string) {
	t.Helper()
	rt := getRuntime(ctx, t)
	patch := client.MergeFrom(rt.DeepCopy())
	kept := rt.Spec.Overrides[:0]
	for _, o := range rt.Spec.Overrides {
		if o.Component != component || o.Path != path {
			kept = append(kept, o)
		}
	}
	kept = append(kept, v1alpha1.Override{Component: component, Path: path, Value: apiextensionsv1.JSON{Raw: []byte(value)}})
	rt.Spec.Overrides = kept
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatalf("set override %s:%s: %v", component, path, err)
	}
}

func removeOverride(ctx context.Context, t *testing.T, component string) {
	t.Helper()
	rt := getRuntime(ctx, t)
	patch := client.MergeFrom(rt.DeepCopy())
	kept := rt.Spec.Overrides[:0]
	for _, o := range rt.Spec.Overrides {
		if o.Component != component {
			kept = append(kept, o)
		}
	}
	rt.Spec.Overrides = kept
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatalf("remove override %s: %v", component, err)
	}
}

// ready reports the runtime's Ready condition for its current generation.
func ready(rt *v1alpha1.ClusterRuntime) bool {
	c := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionReady)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == rt.Generation
}

// waitRolledOut waits until the runtime is Ready on an active plan other
// than previous, and returns it.
func waitRolledOut(ctx context.Context, t *testing.T, previous string) string {
	t.Helper()
	return waitRolledOutObserving(ctx, t, previous, "")
}

// waitRolledOutObserving is waitRolledOut that also checks, on every poll,
// that Ready is never reported for the new generation before Argo CD has
// compared the Application with the expected CPU request.
func waitRolledOutObserving(ctx context.Context, t *testing.T, previous, cpu string) string {
	t.Helper()
	deadline := time.Now().Add(resolveTimeout + rolloutTimeout)
	sawNotReady := previous == ""
	for time.Now().Before(deadline) {
		rt := getRuntime(ctx, t)
		if !ready(rt) {
			sawNotReady = true
		}
		if ready(rt) && rt.Status.ActivePlanDigest != "" && rt.Status.ActivePlanDigest != previous &&
			rt.Status.ActivePlanDigest == rt.Status.CandidatePlanDigest {

			if cpu != "" && comparedCPU(ctx, t) != cpu {
				t.Fatalf("Ready on plan %s while Argo CD compared CPU %q, want %q", rt.Status.ActivePlanDigest, comparedCPU(ctx, t), cpu)
			}
			if !sawNotReady {
				t.Fatal("the runtime never left Ready while rolling out a new plan")
			}
			return rt.Status.ActivePlanDigest
		}
		if c := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionStalled); c != nil && c.Status == metav1.ConditionTrue &&
			c.ObservedGeneration == rt.Generation && c.Reason != "Progressing" {

			t.Fatalf("runtime stalled: %s: %s", c.Reason, c.Message)
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("runtime not rolled out within %s; status %s", resolveTimeout+rolloutTimeout, statusJSON(ctx, t))
	return ""
}

// waitReadyOn waits until the runtime is Ready on the given active plan.
func waitReadyOn(ctx context.Context, t *testing.T, digest string) {
	t.Helper()
	deadline := time.Now().Add(resolveTimeout)
	for time.Now().Before(deadline) {
		rt := getRuntime(ctx, t)
		if ready(rt) && rt.Status.ActivePlanDigest == digest {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("runtime not Ready on %s within %s; status %s", digest, resolveTimeout, statusJSON(ctx, t))
}

// nextRun waits for a Job of the stage whose run differs from previous.
func nextRun(ctx context.Context, t *testing.T, stage aicrjob.Stage, previous string) string {
	t.Helper()
	deadline := time.Now().Add(resolveTimeout)
	for time.Now().Before(deadline) {
		jobs := &batchv1.JobList{}
		if err := k8s.List(ctx, jobs, client.InNamespace(jobsNamespace), client.MatchingLabels{v1alpha1.LabelStage: string(stage)}); err != nil {
			t.Fatal(err)
		}
		for i := range jobs.Items {
			if id := jobs.Items[i].Labels[v1alpha1.LabelRunID]; id != previous {
				t.Logf("%s run %s replaced %s", stage, id, previous)
				return id
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return previous
}

// waitStalled waits for a Stalled runtime whose message contains message,
// and returns the reason.
func waitStalled(ctx context.Context, t *testing.T, message string) string {
	t.Helper()
	deadline := time.Now().Add(resolveTimeout)
	for time.Now().Before(deadline) {
		rt := getRuntime(ctx, t)
		c := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionStalled)
		if c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == rt.Generation && strings.Contains(c.Message, message) {
			r := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionResolved)
			t.Logf("stalled: %s: %s", c.Reason, c.Message)
			if r == nil || r.Status != metav1.ConditionFalse || r.Reason != c.Reason {
				t.Errorf("Stalled reason %q does not come from Resolved %+v", c.Reason, r)
			}
			return c.Reason
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("runtime did not stall within %s; status %s", resolveTimeout, statusJSON(ctx, t))
	return ""
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

func applicationVersion(ctx context.Context, t *testing.T) string {
	t.Helper()
	return application(ctx, t).GetResourceVersion()
}

func comparedCPU(ctx context.Context, t *testing.T) string {
	t.Helper()
	cpu, _, _ := unstructured.NestedString(application(ctx, t).Object,
		"status", "sync", "comparedTo", "source", "helm", "valuesObject", "resources", "requests", "cpu")
	return cpu
}

// assertApplication checks the runtime's only Application is at the active
// plan: annotations, digest-pinned source, Synced at that digest, Healthy.
func assertApplication(ctx context.Context, t *testing.T, planDigest, cpu string) {
	t.Helper()
	apps := &unstructured.UnstructuredList{}
	apps.SetGroupVersionKind(argocd.ApplicationGVK.GroupVersion().WithKind("ApplicationList"))
	if err := k8s.List(ctx, apps, client.InNamespace("argocd"), client.MatchingLabels{v1alpha1.LabelRuntime: v1alpha1.SingletonName}); err != nil {
		t.Fatal(err)
	}
	if len(apps.Items) != 1 {
		t.Fatalf("runtime has %d Applications, want 1", len(apps.Items))
	}
	app := &apps.Items[0]
	if got := app.GetAnnotations()[v1alpha1.AnnotationPlanDigest]; got != planDigest {
		t.Errorf("Application plan digest = %s, want %s", got, planDigest)
	}
	rev, _, _ := unstructured.NestedString(app.Object, "spec", "source", "targetRevision")
	synced, _, _ := unstructured.NestedString(app.Object, "status", "sync", "revision")
	health, _, _ := unstructured.NestedString(app.Object, "status", "health", "status")
	if !strings.HasPrefix(rev, "sha256:") || synced != rev || health != "Healthy" {
		t.Errorf("Application targetRevision=%s synced=%s health=%s", rev, synced, health)
	}
	if len(app.GetFinalizers()) != 0 {
		t.Errorf("Application finalizers = %v, want none", app.GetFinalizers())
	}
	if cpu != "" {
		if got, _, _ := unstructured.NestedString(app.Object, "spec", "source", "helm", "valuesObject", "resources", "requests", "cpu"); got != cpu {
			t.Errorf("Application CPU request = %q, want %q", got, cpu)
		}
	}
}

// assertNoEngineWrites checks the Application is untouched and no rollout
// targets anything but the active plan.
func assertNoEngineWrites(ctx context.Context, t *testing.T, resourceVersion, active string) {
	t.Helper()
	time.Sleep(10 * time.Second)
	app := application(ctx, t)
	if app.GetAnnotations()[v1alpha1.AnnotationPlanDigest] != active {
		t.Errorf("Application was rewritten for plan %s", app.GetAnnotations()[v1alpha1.AnnotationPlanDigest])
	}
	if app.GetResourceVersion() != resourceVersion {
		// Argo CD itself updates status; the operator's spec must not change.
		t.Logf("Application resourceVersion moved from %s to %s (Argo CD status)", resourceVersion, app.GetResourceVersion())
	}
	rollouts := &v1alpha1.RuntimeRolloutList{}
	if err := k8s.List(ctx, rollouts); err != nil {
		t.Fatal(err)
	}
	for i := range rollouts.Items {
		if ro := &rollouts.Items[i]; ro.Labels[v1alpha1.LabelOutcome] == "" {
			t.Errorf("rollout %s to %s started during a failed resolution", ro.Name, ro.Spec.ToPlanDigest)
		}
	}
}

// setOperatorAICRVersion sets the operator's declared AICR version and waits
// for the new pod.
func setOperatorAICRVersion(ctx context.Context, t *testing.T, version string) {
	t.Helper()
	setOperatorArg(ctx, t, "--aicr-version", version)
}

// setOperatorArg sets one of the operator's flags, adding it when absent,
// and waits for the new pod.
func setOperatorArg(ctx context.Context, t *testing.T, flag, value string) {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: "aicr-operator"}, d); err != nil {
		t.Fatalf("get operator Deployment: %v", err)
	}
	c := &d.Spec.Template.Spec.Containers[0]
	want := flag + "=" + value
	found := false
	for i, a := range c.Args {
		if strings.HasPrefix(a, flag+"=") {
			found = true
			if a == want {
				return
			}
			c.Args[i] = want
		}
	}
	if !found {
		c.Args = append(c.Args, want)
	}
	if err := k8s.Update(ctx, d); err != nil {
		t.Fatalf("update operator Deployment: %v", err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(d), d); err != nil {
			t.Fatal(err)
		}
		if d.Status.ObservedGeneration == d.Generation && d.Status.UpdatedReplicas == 1 && d.Status.ReadyReplicas == 1 && d.Status.Replicas == 1 {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("operator did not roll out with %s", want)
}

// killRunningJob deletes the first running Job of a stage and returns its
// run ID.
func killRunningJob(ctx context.Context, t *testing.T, stage aicrjob.Stage) string {
	t.Helper()
	deadline := time.Now().Add(resolveTimeout)
	for time.Now().Before(deadline) {
		jobs := &batchv1.JobList{}
		if err := k8s.List(ctx, jobs, client.InNamespace(jobsNamespace), client.MatchingLabels{v1alpha1.LabelStage: string(stage)}); err != nil {
			t.Fatal(err)
		}
		for i := range jobs.Items {
			j := &jobs.Items[i]
			if j.Status.Active > 0 && j.DeletionTimestamp.IsZero() {
				runID := j.Labels[v1alpha1.LabelRunID]
				err := k8s.Delete(ctx, j, client.PropagationPolicy(aicrjob.DeletePropagation))
				if err != nil && !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}
				t.Logf("killed %s run %s", stage, runID)
				return runID
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no running %s Job appeared", stage)
	return ""
}

func statusJSON(ctx context.Context, t *testing.T) string {
	t.Helper()
	data, _ := json.MarshalIndent(getRuntime(ctx, t).Status, "", "  ")
	return string(data)
}
