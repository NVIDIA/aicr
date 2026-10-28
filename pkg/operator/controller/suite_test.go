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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/config"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

const (
	operatorNS  = "aicr-system"
	jobsNS      = "aicr-jobs"
	discoveryNS = "aicr-discovery"
	validateNS  = "aicr-validation"
	aicrImage   = "ghcr.io/nvidia/aicr:v1.0.0@sha256:e91ba6f4916a5bfcd19d05933b5b4d07a4dd05a738a1ffebfab243a32cbff433"
)

var (
	k8s client.Client
	// restConfig reaches envtest, for clients the fault tests wrap.
	restConfig *rest.Config
)

// TestMain runs the controllers' tests against a real API server, because
// they depend on server-side apply merging condition lists by type. There is
// no garbage collector or Job controller: tests complete Jobs themselves.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set; controller tests cannot run")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "skipping controller tests: KUBEBUILDER_ASSETS is not set (run `make test`)")
		os.Exit(0)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "charts", "aicr-operator", "files", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	restConfig = cfg
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err == nil {
		err = v1alpha1.AddToScheme(scheme)
	}
	if err == nil {
		k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	}
	if err == nil {
		for _, ns := range []string{operatorNS, jobsNS, discoveryNS, validateNS, "argocd"} {
			if err = k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
				break
			}
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "set up envtest: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	os.Exit(code)
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testConfig() *config.Config {
	c := config.Defaults()
	c.AICRVersion = "v1.0.0"
	c.AICRImage = aicrImage
	c.RegistryRepository = "oci://registry.test:5000/aicr/cluster"
	c.RegistryPlainHTTP = true
	// Resolver tests that need discovery enable it; the rest resolve from
	// declared criteria.
	c.SkipDiscovery = true
	return &c
}

var request = ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.SingletonName}}

// freshRuntime deletes everything an earlier test left and creates the
// singleton runtime anew, so each test sees a new UID.
func freshRuntime(t *testing.T, ctx context.Context, mutate func(*v1alpha1.ClusterRuntime)) *v1alpha1.ClusterRuntime {
	t.Helper()
	forceDelete(ctx)
	_ = k8s.DeleteAllOf(ctx, &v1alpha1.RuntimeRollout{})
	_ = k8s.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace(operatorNS), client.MatchingLabels{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue})
	_ = k8s.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace(jobsNS), client.MatchingLabels{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue})
	_ = k8s.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace(discoveryNS), client.MatchingLabels{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue})
	_ = k8s.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace(validateNS), client.MatchingLabels{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue})
	clearJobs(t, ctx)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		err := k8s.Get(ctx, request.NamespacedName, &v1alpha1.ClusterRuntime{})
		if apierrors.IsNotFound(err) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	rt := &v1alpha1.ClusterRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName},
		Spec: v1alpha1.ClusterRuntimeSpec{
			Intent: "training", Platform: "kubeflow",
			Criteria: &v1alpha1.DeclaredCriteria{Service: "eks", Accelerator: "h100", OS: "ubuntu"},
			Placement: v1alpha1.Placement{
				System:      v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{"pool": "system"}},
				Accelerated: v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{"pool": "gpu"}},
			},
			Validation: v1alpha1.ValidationPolicy{Deployment: v1alpha1.DeploymentValidationDisabled},
		},
	}
	if mutate != nil {
		mutate(rt)
	}
	if err := k8s.Create(ctx, rt); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	return rt
}

// forceDelete deletes the runtime as an administrator who removed its
// cleanup finalizer by hand would: no reconciler runs between tests.
func forceDelete(ctx context.Context) {
	rt := &v1alpha1.ClusterRuntime{}
	if err := k8s.Get(ctx, request.NamespacedName, rt); err != nil {
		return
	}
	if len(rt.Finalizers) > 0 {
		patch := client.MergeFrom(rt.DeepCopy())
		rt.Finalizers = nil
		_ = k8s.Patch(ctx, rt, patch)
	}
	_ = k8s.Delete(ctx, rt)
}

// clearJobs removes Jobs, including the foreground finalizer envtest's
// missing garbage collector would leave on them.
func clearJobs(t *testing.T, ctx context.Context) {
	t.Helper()
	jobs := &batchv1.JobList{}
	for _, ns := range []string{jobsNS, discoveryNS, validateNS} {
		list := &batchv1.JobList{}
		if err := k8s.List(ctx, list, client.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		jobs.Items = append(jobs.Items, list.Items...)
	}
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if len(j.Finalizers) > 0 {
			patch := client.MergeFrom(j.DeepCopy())
			j.Finalizers = nil
			_ = k8s.Patch(ctx, j, patch)
		}
		_ = k8s.Delete(ctx, j, client.PropagationPolicy(metav1.DeletePropagationBackground))
	}
}

func getRuntime(t *testing.T, ctx context.Context) *v1alpha1.ClusterRuntime {
	t.Helper()
	rt := &v1alpha1.ClusterRuntime{}
	if err := k8s.Get(ctx, request.NamespacedName, rt); err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	return rt
}

func cond(rt *v1alpha1.ClusterRuntime, conditionType string) metav1.Condition {
	c := meta.FindStatusCondition(rt.Status.Conditions, conditionType)
	if c == nil {
		return metav1.Condition{}
	}
	return *c
}

// liveJobs lists Jobs of a stage that are not being deleted, newest first.
func liveJobs(t *testing.T, ctx context.Context, stage string) []batchv1.Job {
	t.Helper()
	ns := jobsNS
	switch aicrjob.Stage(stage) {
	case aicrjob.StageSnapshot:
		ns = discoveryNS
	case aicrjob.StageValidation:
		ns = validateNS
	case aicrjob.StageRecipe, aicrjob.StageBaseRender, aicrjob.StageRender, aicrjob.StageAssessment:
	}
	list := &batchv1.JobList{}
	if err := k8s.List(ctx, list, client.InNamespace(ns), client.MatchingLabels{v1alpha1.LabelStage: stage}); err != nil {
		t.Fatal(err)
	}
	var out []batchv1.Job
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() {
			out = append(out, list.Items[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return attempt(&out[i]) > attempt(&out[j]) })
	return out
}

// succeed completes a Job as the Job controller would and writes its output
// as AICR would.
func succeed(t *testing.T, ctx context.Context, job *batchv1.Job, output []byte) {
	t.Helper()
	if output != nil {
		cm := &corev1.ConfigMap{}
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: job.Name}, cm); err != nil {
			t.Fatalf("get output of %s: %v", job.Name, err)
		}
		cm.Data = map[string]string{"snapshot.json": string(output), "format": "json"}
		if err := k8s.Update(ctx, cm); err != nil {
			t.Fatalf("write output of %s: %v", job.Name, err)
		}
	}
	finish(t, ctx, job, batchv1.JobComplete, batchv1.JobSuccessCriteriaMet)
}

// fail fails a Job; with exitCode >= 0 its pod exits with that code.
func fail(t *testing.T, ctx context.Context, job *batchv1.Job, exitCode int32) {
	t.Helper()
	failReporting(t, ctx, job, exitCode, "[INVALID_REQUEST] rejected")
}

// failReporting fails a Job whose pod exits with exitCode and leaves message
// as its termination message.
func failReporting(t *testing.T, ctx context.Context, job *batchv1.Job, exitCode int32, message string) {
	t.Helper()
	if exitCode >= 0 {
		exited(t, ctx, job, exitCode, message)
	}
	finish(t, ctx, job, batchv1.JobFailed, batchv1.JobFailureTarget)
}

// succeedReporting completes a Job whose pod leaves message as its
// termination message, as AICR v1.1.0 does with --image-refs.
func succeedReporting(t *testing.T, ctx context.Context, job *batchv1.Job, message string) {
	t.Helper()
	exited(t, ctx, job, 0, message)
	finish(t, ctx, job, batchv1.JobComplete, batchv1.JobSuccessCriteriaMet)
}

// exited creates a Job's pod with its aicr container terminated.
func exited(t *testing.T, ctx context.Context, job *batchv1.Job, exitCode int32, message string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace,
			Labels: map[string]string{"batch.kubernetes.io/job-name": job.Name}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "aicr", Image: aicrImage}}},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "aicr", Image: aicrImage, ImageID: aicrImage,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Message: message}},
	}}
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
}

func finish(t *testing.T, ctx context.Context, job *batchv1.Job, terminal, interim batchv1.JobConditionType) {
	t.Helper()
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	job.Status.StartTime = &now
	if terminal == batchv1.JobComplete {
		job.Status.CompletionTime = &now
		job.Status.Succeeded = 1
	} else {
		job.Status.Failed = 1
	}
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: interim, Status: corev1.ConditionTrue, LastTransitionTime: now, LastProbeTime: now, Reason: "Test"},
		{Type: terminal, Status: corev1.ConditionTrue, LastTransitionTime: now, LastProbeTime: now, Reason: "Test"},
	}
	if err := k8s.Status().Update(ctx, job); err != nil {
		t.Fatalf("finish Job %s: %v", job.Name, err)
	}
}

// putPlan stores a one-release plan with its values as the resolver would,
// and returns its digest.
func putPlan(t *testing.T, ctx context.Context, rt *v1alpha1.ClusterRuntime, cpu string) string {
	t.Helper()
	return putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", fmt.Sprintf(`{"cpu":%q}`, cpu)))
}

// testRelease is a mirrored release whose chart digest derives from its ID.
func testRelease(id string, phase plan.Phase, readinessFor, values string) releaseWithValues {
	return releaseWithValues{values: values, Release: plan.Release{
		ID: id, Component: id, Phase: phase, ReadinessFor: readinessFor, TargetNamespace: "cert-manager",
		Chart: plan.Chart{
			Source: plan.ChartMirrored, Repository: "oci://registry.test:5000/aicr/cluster/charts/" + id,
			Digest:   plan.DigestOf([]byte("chart-" + id)),
			Upstream: &plan.UpstreamChart{Repository: "https://charts.example.com", ArchiveDigest: plan.DigestOf([]byte("archive-" + id))},
		},
	}}
}

type releaseWithValues struct {
	plan.Release
	values string
}

// putReleases stores a plan of the releases, in order, with their values as
// the resolver would, and returns its digest.
func putReleases(t *testing.T, ctx context.Context, rt *v1alpha1.ClusterRuntime, releases ...releaseWithValues) string {
	t.Helper()
	return putResolved(t, ctx, rt, nil, releases...)
}

// putResolved is putReleases for a plan resolved with the given criteria.
func putResolved(t *testing.T, ctx context.Context, rt *v1alpha1.ClusterRuntime, criteria map[string]string, releases ...releaseWithValues) string {
	t.Helper()
	return putPolicy(t, ctx, rt, criteria, "argocd.v1", releases...)
}

// putPolicy is putResolved for a plan of the given engine policy.
func putPolicy(t *testing.T, ctx context.Context, rt *v1alpha1.ClusterRuntime, criteria map[string]string, policy string,
	releases ...releaseWithValues) string {

	t.Helper()
	recipeJSON := []byte(`{"kind":"RecipeResult"}`)
	if criteria != nil {
		// Plans resolved for other criteria come from other recipes.
		c, err := json.Marshal(criteria)
		if err != nil {
			t.Fatal(err)
		}
		recipeJSON = []byte(`{"kind":"RecipeResult","criteria":` + string(c) + `}`)
	}
	recipe := plan.RecipeContent(recipeJSON)
	doc := &plan.Document{
		SchemaVersion: plan.SchemaVersion, RuntimeUID: string(rt.UID), Engine: plan.EngineArgoCD, EnginePolicy: policy,
		Provenance: plan.Provenance{Source: plan.SourceResolved},
		Recipe:     &plan.Recipe{Name: "test", Criteria: criteria, ConfigMapRef: recipe.Ref()},
	}
	contents := make([]*plan.Content, 0, 1+len(releases))
	contents = append(contents, recipe)
	for _, r := range releases {
		values := plan.ValuesContent(r.ID, []byte(r.values))
		r.Values = values.Ref()
		doc.Releases = append(doc.Releases, r.Release)
		contents = append(contents, values)
	}
	if criteria != nil {
		doc.Provenance.SnapshotDigest = plan.DigestOf([]byte("snapshot"))
	}
	digest, err := plan.ComputeDesiredStateDigest(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc.DesiredStateDigest = digest
	for _, c := range contents {
		content, cmErr := c.ConfigMap(operatorNS, rt.Name, string(rt.UID), nil)
		if cmErr != nil {
			t.Fatal(cmErr)
		}
		if cmErr = k8s.Create(ctx, content); cmErr != nil && !apierrors.IsAlreadyExists(cmErr) {
			t.Fatal(cmErr)
		}
	}
	cm, err := plan.ToConfigMap(doc, operatorNS, rt.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err = k8s.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	return digest
}

// rebuild replaces the stored plan of digest with what its engine objects
// rebuild to, as adoption or a restored active plan leaves it: the same
// digest and runtime, and no recipe.
func rebuild(t *testing.T, ctx context.Context, digest string) {
	t.Helper()
	doc, err := readPlan(ctx, k8s, operatorNS, digest)
	if err != nil {
		t.Fatal(err)
	}
	name, err := plan.Name(digest)
	if err != nil {
		t.Fatal(err)
	}
	stored := &corev1.ConfigMap{}
	if err = k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: name}, stored); err != nil {
		t.Fatal(err)
	}
	doc.Provenance, doc.Recipe, doc.InputDigest = plan.Provenance{Source: plan.SourceRebuilt}, nil, ""
	cm, err := plan.ToConfigMap(doc, operatorNS, stored.Labels[v1alpha1.LabelRuntime])
	if err != nil {
		t.Fatal(err)
	}
	if err = k8s.Delete(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err = k8s.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
}
