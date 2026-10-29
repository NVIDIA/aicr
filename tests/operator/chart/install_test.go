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

package chart

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/plugin"
)

const (
	runtimeUID = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	digestA    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB    = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestInstallation installs the chart into envtest and checks what the API
// server enforces, then upgrades and uninstalls it. Subtests run in order.
func TestInstallation(t *testing.T) {
	ctx := ctxFor(t)
	// The release namespace is the operator namespace; administrators create
	// it with the restricted profile (charts/aicr-operator/README.md).
	ensureNamespace(ctx, t, operatorNS, map[string]string{"pod-security.kubernetes.io/enforce": "restricted"})
	ensureNamespace(ctx, t, engineNS, nil)
	if out, err := helm(ctx, "install", release, chartDir, "--namespace", operatorNS, "-f", valuesFile); err != nil {
		t.Fatalf("helm install: %v\n%s", err, out)
	}
	enforced(ctx, t)

	t.Run("the operator is denied Applications outside its constraints", operatorApplications)
	t.Run("the operator deletes only Applications an approved Remove step marked", operatorApplicationDeletes)
	t.Run("drift repair passes the Application policy", driftRepair)
	t.Run("the operator is denied Jobs outside its constraints", operatorJobs)
	t.Run("the operator cannot change a set outcome or evidence", operatorOutcome)
	t.Run("the operator changes a runtime only through its cleanup finalizer", operatorRuntime)
	t.Run("the operator holds no Secret listing, Namespace writes or escalation", operatorRBAC)
	t.Run("only approvers approve or revoke, as themselves, and change nothing else", approvals)
	t.Run("kubectl aicr describes, approves and revokes as the signed-in approver", pluginCommands)
	t.Run("only the operator writes operator-labeled ConfigMaps", operatorState)
	t.Run("tenants are denied every administrative surface", tenants)
	t.Run("workloads satisfy their namespaces' Pod Security profiles", podSecurity)
	t.Run("helm upgrade updates the templated CRDs", upgradeCRDs)
	t.Run("helm uninstall keeps the stack, operator state and its protection", uninstall)
}

// enforced waits until the API server enforces every policy the chart
// installs: a policy takes effect shortly after it is created, not with it.
func enforced(ctx context.Context, t *testing.T) {
	t.Helper()
	op := operator(t)
	outcome := rollout("rollout-policy-probe")
	outcome.Labels[v1alpha1.LabelOutcome] = string(v1alpha1.PhaseSucceeded)
	// The operator's RBAC also takes effect shortly after it is created.
	var createErr error
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(context.Context) (bool, error) {
		createErr = op.Create(ctx, outcome.DeepCopy())
		return client.IgnoreAlreadyExists(createErr) == nil, nil
	}); err != nil {
		t.Fatalf("the operator cannot create a rollout: %v", createErr)
	}
	defer func() { _ = admin.Delete(context.WithoutCancel(ctx), outcome) }()
	probeRuntime := runtimeObject(t)
	if err := admin.Create(ctx, probeRuntime); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Delete(context.WithoutCancel(ctx), probeRuntime) }()
	probes := map[string]func() error{
		"aicr-operator-applications": func() error {
			app := application(t)
			app.Object["spec"].(map[string]any)["project"] = "default"
			return op.Create(ctx, app, client.DryRunAll)
		},
		"aicr-operator-jobs": func() error {
			j := job(t, aicrjob.StageRecipe, "aicr-jobs")
			j.Spec.TTLSecondsAfterFinished = nil
			return op.Create(ctx, j, client.DryRunAll)
		},
		"aicr-operator-rollout-outcome": func() error {
			live := &v1alpha1.RuntimeRollout{}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(outcome), live); err != nil {
				return err
			}
			base := live.DeepCopy()
			live.Labels[v1alpha1.LabelOutcome] = string(v1alpha1.PhaseFailed)
			return op.Patch(ctx, live, client.MergeFrom(base), client.DryRunAll)
		},
		"aicr-operator-state": func() error {
			return admin.Create(ctx, labeledConfigMap("aicr-policy-probe"), client.DryRunAll)
		},
		"aicr-operator-runtime": func() error {
			live := &v1alpha1.ClusterRuntime{}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(probeRuntime), live); err != nil {
				return err
			}
			base := live.DeepCopy()
			live.Spec.Paused = true
			return op.Patch(ctx, live, client.MergeFrom(base), client.DryRunAll)
		},
		"aicr-operator-approval": func() error {
			approved := rollout("rollout-approval-probe")
			approved.Spec.Approval = &v1alpha1.Approval{ApprovedBy: operatorUser, Reason: "probe", ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour))}
			return op.Create(ctx, approved, client.DryRunAll)
		},
	}
	for name, probe := range probes {
		if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(context.Context) (bool, error) {
			err := probe()
			return err != nil && strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+name+"'"), nil
		}); err != nil {
			t.Fatalf("policy %s is not enforced: %v", name, err)
		}
	}
}

// application is what the operator writes for a gpu-operator release.
func application(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	app, err := argocd.Application(argocd.Target{Namespace: engineNS, Project: "aicr", RuntimeName: v1alpha1.SingletonName},
		&plan.Document{RuntimeUID: runtimeUID, DesiredStateDigest: digestA},
		&plan.Release{ID: "gpu-operator", Component: "gpu-operator", Phase: plan.PhasePrimary, TargetNamespace: "gpu-operator",
			Chart: plan.Chart{Source: plan.ChartMirrored, Repository: registry + "/charts/gpu-operator", Digest: digestB}},
		[]byte(`{"driver":{"enabled":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func operatorApplications(t *testing.T) {
	ctx := ctxFor(t)
	op := operator(t)
	create := func(app *unstructured.Unstructured) error { return op.Create(ctx, app, client.DryRunAll) }
	set := func(mutate func(o map[string]any)) *unstructured.Unstructured {
		app := application(t)
		mutate(app.Object)
		return app
	}
	spec := func(o map[string]any) map[string]any { return o["spec"].(map[string]any) }
	source := func(o map[string]any) map[string]any { return spec(o)["source"].(map[string]any) }
	destination := func(o map[string]any) map[string]any { return spec(o)["destination"].(map[string]any) }

	if err := create(application(t)); err != nil {
		t.Fatalf("the operator's own Application is denied: %v", err)
	}
	if err := create(set(func(o map[string]any) { destination(o)["namespace"] = "kube-system" })); err != nil {
		t.Errorf("an existing component namespace is denied: %v", err)
	}
	armed := application(t)
	armed.SetFinalizers([]string{argocd.ResourcesFinalizer})
	armed.SetAnnotations(map[string]string{v1alpha1.AnnotationDeleteRollout: "rollout-x"})
	if err := create(armed); err != nil {
		t.Errorf("the finalizer of an approved Remove step is denied: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(o map[string]any)
		want   string
	}{
		{"another project", func(o map[string]any) { spec(o)["project"] = "default" }, "use the aicr project"},
		{"several sources", func(o map[string]any) { spec(o)["sources"] = []any{source(o)} }, "single source"},
		{"no source", func(o map[string]any) { delete(spec(o), "source") }, "single source"},
		{"a foreign registry", func(o map[string]any) { source(o)["repoURL"] = "oci://ghcr.io/example/charts/gpu-operator" }, "pull from the runtime registry"},
		{"a sibling prefix", func(o map[string]any) { source(o)["repoURL"] = registry + "-other/charts/gpu-operator" }, "pull from the runtime registry"},
		{"a tag", func(o map[string]any) { source(o)["targetRevision"] = "v25.10.0" }, "pin a sha256 digest"},
		{"a plugin", func(o map[string]any) { source(o)["plugin"] = map[string]any{"name": "sh"} }, "no config management plugin"},
		{"value files", func(o map[string]any) {
			source(o)["helm"].(map[string]any)["valueFiles"] = []any{"https://example.com/values.yaml"}
		}, "values only in valuesObject"},
		{"parameters", func(o map[string]any) {
			source(o)["helm"].(map[string]any)["parameters"] = []any{map[string]any{"name": "a", "value": "b"}}
		}, "values only in valuesObject"},
		{"another cluster", func(o map[string]any) { destination(o)["server"] = "https://example.com" }, "in-cluster server"},
		{"a named cluster", func(o map[string]any) { destination(o)["name"] = "in-cluster" }, "in-cluster server"},
		{"another namespace", func(o map[string]any) { destination(o)["namespace"] = "default" }, "not an allowed component namespace"},
		{"the resources finalizer alone", func(o map[string]any) {
			o["metadata"].(map[string]any)["finalizers"] = []any{argocd.ResourcesFinalizer}
		}, "resources finalizer is set only"},
		{"the background resources finalizer alone", func(o map[string]any) {
			o["metadata"].(map[string]any)["finalizers"] = []any{argocd.ResourcesFinalizer + "/background"}
		}, "resources finalizer is set only"},
		{"the resources finalizer with an empty annotation", func(o map[string]any) {
			o["metadata"].(map[string]any)["finalizers"] = []any{argocd.ResourcesFinalizer}
			o["metadata"].(map[string]any)["annotations"] = map[string]any{v1alpha1.AnnotationDeleteRollout: ""}
		}, "non-empty operator.aicr.run/delete-rollout annotation"},
	}
	for _, tc := range cases {
		denied(t, tc.name, create(set(tc.mutate)), tc.want)
	}

	// The policy holds on update too, and binds only the operator.
	app := application(t)
	if err := op.Create(ctx, app); err != nil {
		t.Fatal(err)
	}
	edited := app.DeepCopy()
	_ = unstructured.SetNestedStringSlice(edited.Object, []string{"x.yaml"}, "spec", "source", "helm", "valueFiles")
	denied(t, "an update adding value files", op.Update(ctx, edited, client.DryRunAll), "values only in valuesObject")
	if err := admin.Update(ctx, edited, client.DryRunAll); err != nil {
		t.Errorf("an administrator's edit is denied: %v", err)
	}
	elsewhere := application(t)
	elsewhere.SetNamespace("default")
	if err := create(elsewhere); !apierrors.IsForbidden(err) {
		t.Errorf("an Application outside the engine namespace: %v, want forbidden", err)
	}
	// Cleanup: the operator deletes only Applications a Remove step marked.
	if err := admin.Delete(ctx, app); err != nil {
		t.Fatal(err)
	}
}

// driftRepair runs the operator's Argo CD adapter as the operator's identity:
// fields a person adds by hand, which the policy forbids the operator to
// write, are replaced rather than merged into a denied write.
func driftRepair(t *testing.T) {
	ctx := ctxFor(t)
	op := operator(t)
	adapter := &argocd.Adapter{Writer: op, Reader: op, Target: argocd.Target{Namespace: engineNS, Project: "aicr", RuntimeName: v1alpha1.SingletonName}}
	p := &plan.Document{RuntimeUID: runtimeUID, DesiredStateDigest: digestA}
	r := &plan.Release{ID: "cert-manager", Component: "cert-manager", Phase: plan.PhasePrimary, TargetNamespace: "cert-manager",
		Chart: plan.Chart{Source: plan.ChartMirrored, Repository: registry + "/charts/cert-manager", Digest: digestB}}
	values := []byte(`{"crds":{"enabled":true}}`)
	if _, err := adapter.Ensure(ctx, p, r, values); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(argocd.ApplicationGVK)
	key := client.ObjectKey{Namespace: engineNS, Name: r.ID}
	if err := admin.Get(ctx, key, live); err != nil {
		t.Fatal(err)
	}
	edited := live.DeepCopy()
	_ = unstructured.SetNestedStringSlice(edited.Object, []string{"https://example.com/values.yaml"}, "spec", "source", "helm", "valueFiles")
	if err := admin.Patch(ctx, edited, client.MergeFrom(live), client.FieldOwner("kubectl-edit")); err != nil {
		t.Fatal(err)
	}
	ensured, err := adapter.Ensure(ctx, p, r, values)
	if err != nil {
		t.Fatalf("Ensure after a hand edit: %v", err)
	}
	if err = admin.Get(ctx, key, live); err != nil {
		t.Fatal(err)
	}
	if files, found, _ := unstructured.NestedSlice(live.Object, "spec", "source", "helm", "valueFiles"); found {
		t.Errorf("value files survived the repair: %v (overridden %v)", files, ensured.Overridden)
	}
	// Cleanup: the operator deletes only Applications a Remove step marked.
	if err = admin.Delete(ctx, live); err != nil {
		t.Fatal(err)
	}
}

// job is the Job the operator builds for a stage.
func job(t *testing.T, stage aicrjob.Stage, serviceAccount string) *batchv1.Job {
	t.Helper()
	j, err := aicrjob.Build(&aicrjob.Request{
		Stage: stage, RunID: "0123456789ab", RuntimeName: v1alpha1.SingletonName, RuntimeUID: runtimeUID, Image: aicrImage,
		Args:           []string{"--output", "cm://x"},
		Namespaces:     aicrjob.Namespaces{Jobs: jobsNS, Discovery: discoveryNS, Validation: validationNS},
		ServiceAccount: serviceAccount, CredentialSecret: "aicr-run-0123456789ab",
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// The operator deletes an Application only once an approved Remove step has
// marked it, and never another team's.
func operatorApplicationDeletes(t *testing.T) {
	ctx := ctxFor(t)
	op := operator(t)
	put := func(name string, mutate func(*unstructured.Unstructured)) *unstructured.Unstructured {
		app := application(t)
		app.SetName(name)
		mutate(app)
		if err := admin.Create(ctx, app); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			app.SetFinalizers(nil)
			_ = admin.Update(context.WithoutCancel(ctx), app)
			_ = admin.Delete(context.WithoutCancel(ctx), app)
		})
		return app
	}
	marked := put("marked-for-removal", func(a *unstructured.Unstructured) {
		a.SetFinalizers([]string{argocd.ResourcesFinalizer})
		a.SetAnnotations(map[string]string{v1alpha1.AnnotationDeleteRollout: "rollout-x"})
	})
	if err := op.Delete(ctx, marked, client.DryRunAll); err != nil {
		t.Errorf("deleting an Application an approved Remove step marked is denied: %v", err)
	}
	const marks = "non-empty operator.aicr.run/delete-rollout annotation and the resources finalizer"
	unmarked := put("owned-unmarked", func(*unstructured.Unstructured) {})
	denied(t, "an owned Application no Remove step marked", op.Delete(ctx, unmarked, client.DryRunAll), marks)
	emptyMark := put("owned-empty-mark", func(a *unstructured.Unstructured) {
		a.SetFinalizers([]string{argocd.ResourcesFinalizer})
		a.SetAnnotations(map[string]string{v1alpha1.AnnotationDeleteRollout: ""})
	})
	denied(t, "an owned Application with an empty delete-rollout annotation", op.Delete(ctx, emptyMark, client.DryRunAll), marks)
	foreign := put("another-teams-app", func(a *unstructured.Unstructured) {
		a.SetLabels(map[string]string{"team": "other"})
		a.SetFinalizers([]string{argocd.ResourcesFinalizer})
		a.SetAnnotations(map[string]string{v1alpha1.AnnotationDeleteRollout: "rollout-x"})
	})
	denied(t, "another team's Application", op.Delete(ctx, foreign, client.DryRunAll), marks)
}

func operatorJobs(t *testing.T) {
	ctx := ctxFor(t)
	op := operator(t)
	create := func(j *batchv1.Job) error { return op.Create(ctx, j, client.DryRunAll) }
	for _, ok := range []*batchv1.Job{
		job(t, aicrjob.StageRecipe, "aicr-jobs"),
		job(t, aicrjob.StageBaseRender, "aicr-jobs"),
		job(t, aicrjob.StageRender, "aicr-jobs"),
		job(t, aicrjob.StageAssessment, "aicr-jobs"),
		job(t, aicrjob.StageSnapshot, "aicr-snapshot"),
		job(t, aicrjob.StageValidation, "aicr-validation"),
	} {
		if err := create(ok); err != nil {
			t.Errorf("the operator's %s Job is denied: %v", ok.Labels[v1alpha1.LabelStage], err)
		}
	}
	recipe := func(mutate func(j *batchv1.Job)) *batchv1.Job {
		j := job(t, aicrjob.StageRecipe, "aicr-jobs")
		mutate(j)
		return j
	}
	validationJob := func(mutate func(j *batchv1.Job)) *batchv1.Job {
		j := job(t, aicrjob.StageValidation, "aicr-validation")
		mutate(j)
		return j
	}
	pod := func(j *batchv1.Job) *corev1.PodSpec { return &j.Spec.Template.Spec }
	cases := []struct {
		name string
		job  *batchv1.Job
		want string
	}{
		{"another image", recipe(func(j *batchv1.Job) { pod(j).Containers[0].Image = "busybox@" + digestA }), "pinned AICR image"},
		{"another ServiceAccount", recipe(func(j *batchv1.Job) { pod(j).ServiceAccountName = "default" }), "namespace's AICR identity"},
		{"the snapshot identity in the Job namespace", recipe(func(j *batchv1.Job) { pod(j).ServiceAccountName = "aicr-snapshot" }), "namespace's AICR identity"},
		{"a command", recipe(func(j *batchv1.Job) { pod(j).Containers[0].Command = []string{"/bin/sh", "-c"} }), "image's entrypoint"},
		{"another subcommand", recipe(func(j *batchv1.Job) { pod(j).Containers[0].Args[0] = "validate" }), "subcommand allowed"},
		{"no arguments", recipe(func(j *batchv1.Job) { pod(j).Containers[0].Args = nil }), "subcommand allowed"},
		{"no TTL", recipe(func(j *batchv1.Job) { j.Spec.TTLSecondsAfterFinished = nil }), "ttlSecondsAfterFinished"},
		{"an init container", recipe(func(j *batchv1.Job) {
			pod(j).InitContainers = []corev1.Container{*pod(j).Containers[0].DeepCopy()}
			pod(j).InitContainers[0].Name = "init"
		}), "one container"},
		{"a second container", recipe(func(j *batchv1.Job) {
			pod(j).Containers = append(pod(j).Containers, *pod(j).Containers[0].DeepCopy())
			pod(j).Containers[1].Name = "second"
		}), "one container"},
		{"an image override in the environment", recipe(func(j *batchv1.Job) {
			pod(j).Containers[0].Env = append(pod(j).Containers[0].Env, corev1.EnvVar{Name: "AICR_IMAGE", Value: "busybox@" + digestA})
		}), "only HOME, TMPDIR and DOCKER_CONFIG"},
		{"a validator image override in the environment", validationJob(func(j *batchv1.Job) {
			pod(j).Containers[0].Env = append(pod(j).Containers[0].Env, corev1.EnvVar{Name: "AICR_VALIDATOR_IMAGE", Value: "busybox@" + digestA})
		}), "only HOME, TMPDIR and DOCKER_CONFIG"},
		{"an environment from a ConfigMap", recipe(func(j *batchv1.Job) {
			pod(j).Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "x"}}}}
		}), "only HOME, TMPDIR and DOCKER_CONFIG"},
		{"an allowed variable from a ConfigMap", recipe(func(j *batchv1.Job) {
			pod(j).Containers[0].Env[0] = corev1.EnvVar{Name: "HOME", ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "x"}, Key: "home"},
			}}
		}), "only HOME, TMPDIR and DOCKER_CONFIG"},
		{"a hostPath volume", recipe(func(j *batchv1.Job) {
			pod(j).Volumes = append(pod(j).Volumes, corev1.Volume{Name: "host",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		}), "only emptyDir and Secret volumes"},
		{"a ConfigMap volume", recipe(func(j *batchv1.Job) {
			pod(j).Volumes = append(pod(j).Volumes, corev1.Volume{Name: "config",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "x"}}}})
		}), "only emptyDir and Secret volumes"},
	}
	for _, tc := range cases {
		denied(t, tc.name, create(tc.job), tc.want)
	}
	snapshot := func(args ...string) *batchv1.Job {
		j := job(t, aicrjob.StageSnapshot, "aicr-snapshot")
		pod(j).Containers[0].Args = append(pod(j).Containers[0].Args, args...)
		return j
	}
	if err := create(snapshot("--image", aicrImage)); err != nil {
		t.Errorf("a snapshot agent on the pinned AICR image is denied: %v", err)
	}
	denied(t, "a snapshot agent on another image", create(snapshot("--image", "busybox@"+digestA)), "snapshot agent")
	denied(t, "a snapshot agent on another image, joined", create(snapshot("--image=busybox@"+digestA)), "snapshot agent")
	snapshotInJobs := job(t, aicrjob.StageSnapshot, "aicr-snapshot")
	snapshotInJobs.Namespace = jobsNS
	denied(t, "snapshot in the Job namespace", create(snapshotInJobs), "AICR identity")
	recipeInDiscovery := job(t, aicrjob.StageRecipe, "aicr-snapshot")
	recipeInDiscovery.Namespace = discoveryNS
	denied(t, "recipe in the discovery namespace", create(recipeInDiscovery), "subcommand allowed")
	elsewhere := job(t, aicrjob.StageRecipe, "aicr-jobs")
	elsewhere.Namespace = "default"
	if err := create(elsewhere); !apierrors.IsForbidden(err) {
		t.Errorf("a Job outside the AICR namespaces: %v, want forbidden", err)
	}
}

func rollout(name string) *v1alpha1.RuntimeRollout {
	return &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{v1alpha1.LabelRuntime: v1alpha1.SingletonName}},
		Spec: v1alpha1.RuntimeRolloutSpec{
			SchemaVersion: v1alpha1.RolloutSchemaVersion, RuntimeUID: runtimeUID, ToPlanDigest: digestA,
			StartingInventoryDigest: digestB,
			Deadline:                metav1.NewTime(time.Now().Add(6 * time.Hour).Truncate(time.Second)),
			ActionTimeout:           metav1.Duration{Duration: 30 * time.Minute},
			Steps: []v1alpha1.RolloutStep{
				{Release: "gpu-operator", Operation: v1alpha1.OperationInstall, Assessment: v1alpha1.AssessmentNotApplicable},
			},
		},
	}
}

func operatorOutcome(t *testing.T) {
	ctx := ctxFor(t)
	op := operator(t)
	ro := rollout("rollout-outcome-test")
	if err := op.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), ro) })
	patch := func(mutate func(r *v1alpha1.RuntimeRollout)) error {
		live := &v1alpha1.RuntimeRollout{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(ro), live); err != nil {
			return err
		}
		base := live.DeepCopy()
		mutate(live)
		return op.Patch(ctx, live, client.MergeFrom(base))
	}
	if err := patch(func(r *v1alpha1.RuntimeRollout) {
		r.Labels[v1alpha1.LabelOutcome] = string(v1alpha1.PhaseSucceeded)
		r.Annotations = map[string]string{v1alpha1.AnnotationEvidence: "oci://example/evidence@" + digestA}
	}); err != nil {
		t.Fatalf("the operator cannot set an outcome: %v", err)
	}
	if err := patch(func(r *v1alpha1.RuntimeRollout) { r.Labels["example.com/note"] = "kept" }); err != nil {
		t.Errorf("an unrelated label is denied: %v", err)
	}
	denied(t, "changing the outcome", patch(func(r *v1alpha1.RuntimeRollout) {
		r.Labels[v1alpha1.LabelOutcome] = string(v1alpha1.PhaseFailed)
	}), "outcome label never changes")
	denied(t, "removing the outcome", patch(func(r *v1alpha1.RuntimeRollout) {
		delete(r.Labels, v1alpha1.LabelOutcome)
	}), "outcome label never changes")
	denied(t, "changing the evidence", patch(func(r *v1alpha1.RuntimeRollout) {
		r.Annotations[v1alpha1.AnnotationEvidence] = "oci://example/evidence@" + digestB
	}), "evidence annotation never changes")
	denied(t, "removing the evidence", patch(func(r *v1alpha1.RuntimeRollout) {
		delete(r.Annotations, v1alpha1.AnnotationEvidence)
	}), "evidence annotation never changes")
}

// runtimeObject is the runtime in testdata/runtime.yaml.
func runtimeObject(t *testing.T) *v1alpha1.ClusterRuntime {
	t.Helper()
	rt := &v1alpha1.ClusterRuntime{}
	raw, err := os.ReadFile(filepath.Join("testdata", "runtime.yaml"))
	if err == nil {
		err = yaml.Unmarshal(raw, rt)
	}
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func operatorRuntime(t *testing.T) {
	ctx := ctxFor(t)
	op := operator(t)
	rt := runtimeObject(t)
	rt.Finalizers = []string{"example.com/backup"}
	if err := admin.Create(ctx, rt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		live := &v1alpha1.ClusterRuntime{}
		if admin.Get(context.WithoutCancel(ctx), client.ObjectKeyFromObject(rt), live) == nil {
			live.Finalizers = nil
			_ = admin.Update(context.WithoutCancel(ctx), live)
			_ = admin.Delete(context.WithoutCancel(ctx), live)
		}
	})
	patch := func(mutate func(*v1alpha1.ClusterRuntime)) error {
		live := &v1alpha1.ClusterRuntime{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(rt), live); err != nil {
			return err
		}
		base := live.DeepCopy()
		mutate(live)
		return op.Patch(ctx, live, client.MergeFrom(base))
	}
	if err := patch(func(r *v1alpha1.ClusterRuntime) { r.Finalizers = append(r.Finalizers, v1alpha1.FinalizerCleanup) }); err != nil {
		t.Fatalf("adding the cleanup finalizer is denied: %v", err)
	}
	const only = "only through its cleanup finalizer"
	denied(t, "a spec change", patch(func(r *v1alpha1.ClusterRuntime) { r.Spec.Paused = true }), only)
	denied(t, "an annotation", patch(func(r *v1alpha1.ClusterRuntime) {
		r.Annotations = map[string]string{v1alpha1.AnnotationRetry: "x"}
	}), only)
	denied(t, "removing another finalizer", patch(func(r *v1alpha1.ClusterRuntime) {
		r.Finalizers = []string{v1alpha1.FinalizerCleanup}
	}), "only its own finalizer")
	if err := patch(func(r *v1alpha1.ClusterRuntime) { r.Finalizers = []string{"example.com/backup"} }); err != nil {
		t.Fatalf("removing the cleanup finalizer is denied: %v", err)
	}
	if err := op.Create(ctx, runtimeObject(t), client.DryRunAll); !apierrors.IsForbidden(err) {
		t.Errorf("the operator creating a runtime: %v, want forbidden", err)
	}
}

// allowed asks the API server whether a user may perform an action.
func allowed(ctx context.Context, t *testing.T, user string, groups []string, attrs authorizationv1.ResourceAttributes) bool {
	t.Helper()
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: user, Groups: append(groups, "system:authenticated"), ResourceAttributes: &attrs,
	}}
	if err := admin.Create(ctx, sar); err != nil {
		t.Fatal(err)
	}
	return sar.Status.Allowed
}

func operatorRBAC(t *testing.T) {
	ctx := ctxFor(t)
	groups := []string{"system:serviceaccounts", "system:serviceaccounts:" + operatorNS}
	deny := []authorizationv1.ResourceAttributes{
		{Verb: "list", Resource: "secrets"},
		{Verb: "watch", Resource: "secrets"},
		{Verb: "get", Resource: "secrets", Namespace: "kube-system", Name: "anything"},
		{Verb: "get", Resource: "secrets", Namespace: engineNS, Name: "argocd-secret"},
		{Verb: "update", Resource: "secrets", Namespace: engineNS, Name: "argocd-secret"},
		{Verb: "create", Resource: "namespaces"},
		{Verb: "patch", Resource: "namespaces", Name: jobsNS},
		{Verb: "patch", Resource: "nodes"},
		{Verb: "create", Resource: "pods", Namespace: jobsNS},
		{Verb: "create", Resource: "pods", Subresource: "exec", Namespace: jobsNS},
		{Verb: "escalate", Group: "rbac.authorization.k8s.io", Resource: "clusterroles"},
		{Verb: "bind", Group: "rbac.authorization.k8s.io", Resource: "clusterroles"},
		{Verb: "impersonate", Resource: "users"},
		{Verb: "impersonate", Resource: "serviceaccounts"},
		{Verb: "update", Group: v1alpha1.GroupVersion.Group, Resource: "clusterruntimes"},
		{Verb: "update", Group: v1alpha1.GroupVersion.Group, Resource: "runtimerollouts", Subresource: "approve"},
		{Verb: "create", Resource: "secrets", Namespace: discoveryNS},
		{Verb: "update", Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicies"},
		{Verb: "delete", Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicybindings"},
		{Verb: "update", Group: "argoproj.io", Resource: "appprojects", Namespace: engineNS, Name: "aicr"},
		{Verb: "patch", Resource: "configmaps", Namespace: engineNS, Name: "argocd-cm"},
		{Verb: "get", Resource: "configmaps", Namespace: engineNS, Name: "argocd-cm"},
	}
	for _, a := range deny {
		if allowed(ctx, t, operatorUser, groups, a) {
			t.Errorf("the operator may %s %s/%s in %q", a.Verb, a.Resource, a.Subresource, a.Namespace)
		}
	}
	allow := []authorizationv1.ResourceAttributes{
		{Verb: "create", Group: "batch", Resource: "jobs", Namespace: jobsNS},
		{Verb: "create", Group: "batch", Resource: "jobs", Namespace: validationNS},
		{Verb: "create", Resource: "secrets", Namespace: jobsNS},
		{Verb: "update", Resource: "secrets", Namespace: engineNS, Name: "aicr-registry"},
		{Verb: "delete", Group: v1alpha1.GroupVersion.Group, Resource: "runtimerollouts"},
		{Verb: "create", Group: "events.k8s.io", Resource: "events", Namespace: "default"},
	}
	for _, a := range allow {
		if !allowed(ctx, t, operatorUser, groups, a) {
			t.Errorf("the operator may not %s %s in %q", a.Verb, a.Resource, a.Namespace)
		}
	}
}

// setApproval patches a rollout's approval as user, optionally changing
// something else in the same request.
func setApproval(ctx context.Context, t *testing.T, c client.Client, name, approvedBy string, also func(*v1alpha1.RuntimeRollout)) error {
	t.Helper()
	ro := &v1alpha1.RuntimeRollout{}
	if err := admin.Get(ctx, client.ObjectKey{Name: name}, ro); err != nil {
		t.Fatal(err)
	}
	base := ro.DeepCopy()
	ro.Spec.Approval = &v1alpha1.Approval{ApprovedBy: approvedBy, Reason: "window", ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour).Truncate(time.Second))}
	if also != nil {
		also(ro)
	}
	return c.Patch(ctx, ro, client.MergeFrom(base))
}

// setRevocation patches a revocation onto an approved rollout as user.
func setRevocation(ctx context.Context, c client.Client, name, revokedBy string) error {
	ro := &v1alpha1.RuntimeRollout{}
	if err := admin.Get(ctx, client.ObjectKey{Name: name}, ro); err != nil {
		return err
	}
	base := ro.DeepCopy()
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	ro.Spec.Approval.RevokedBy, ro.Spec.Approval.RevokedAt, ro.Spec.Approval.RevokeReason = revokedBy, &now, "incident"
	return c.Patch(ctx, ro, client.MergeFrom(base))
}

// setAcknowledgement adds an acknowledgement of the fixture release's
// post-apply steps to an approved rollout as user.
func setAcknowledgement(ctx context.Context, c client.Client, name, by string) error {
	ro := &v1alpha1.RuntimeRollout{}
	if err := admin.Get(ctx, client.ObjectKey{Name: name}, ro); err != nil {
		return err
	}
	base := ro.DeepCopy()
	ro.Spec.Approval.Acknowledgements = append(ro.Spec.Approval.Acknowledgements, v1alpha1.Acknowledgement{Release: "gpu-operator", AcknowledgedBy: by})
	return c.Patch(ctx, ro, client.MergeFrom(base))
}

// acknowledgements: only an approver acknowledges a release's post-apply
// steps, as itself, while the rollout awaits it, and an acknowledgement is
// never withdrawn.
func acknowledgements(ctx context.Context, t *testing.T, alice, bob, eve client.Client) {
	t.Helper()
	ro := rollout("rollout-acknowledgement-test")
	if err := operator(t).Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Delete(context.WithoutCancel(ctx), ro) })
	if err := setApproval(ctx, t, alice, ro.Name, "alice", nil); err != nil {
		t.Fatal(err)
	}
	const early = "acknowledge only while the rollout awaits acknowledgement"
	denied(t, "acknowledging before the apply", setAcknowledgement(ctx, alice, ro.Name, "alice"), early)
	if err := admin.Get(ctx, client.ObjectKeyFromObject(ro), ro); err != nil {
		t.Fatal(err)
	}
	ro.Status.Phase = v1alpha1.PhaseAwaitingAcknowledgement
	if err := admin.Status().Update(ctx, ro); err != nil {
		t.Fatal(err)
	}
	denied(t, "an editor acknowledging", setAcknowledgement(ctx, eve, ro.Name, "eve"), "needs update on runtimerollouts/approve")
	denied(t, "an acknowledgement naming someone else", setAcknowledgement(ctx, bob, ro.Name, "alice"), "acknowledgedBy must be the approver's own username")
	denied(t, "the operator acknowledging", setAcknowledgement(ctx, operator(t), ro.Name, operatorUser), "the operator never writes spec.approval")
	if err := setAcknowledgement(ctx, bob, ro.Name, "bob"); err != nil {
		t.Fatalf("an approver's own acknowledgement is denied: %v", err)
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(ro), ro); err != nil {
		t.Fatal(err)
	}
	withdrawn := ro.DeepCopy()
	withdrawn.Spec.Approval.Acknowledgements = nil
	denied(t, "withdrawing an acknowledgement", admin.Patch(ctx, withdrawn, client.MergeFrom(ro)), "append-only")
}

func approvals(t *testing.T) {
	ctx := ctxFor(t)
	const needs = "needs update on runtimerollouts/approve"
	alice := as(t, "alice", "aicr-approvers")
	bob := as(t, "bob", "aicr-approvers")
	// An editor may patch rollouts but holds no approve permission.
	editor := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "rollout-editor"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"runtimerollouts"}, Verbs: []string{"get", "patch"}}},
	}
	editorBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "rollout-editor"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: editor.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: "eve"}},
	}
	for _, o := range []client.Object{editor, editorBinding} {
		if err := admin.Create(ctx, o); client.IgnoreAlreadyExists(err) != nil {
			t.Fatal(err)
		}
	}
	eve := as(t, "eve")

	// A rollout someone else creates could skip approval: approvalRequired
	// false, or an outcome already set.
	forgedOutcome := rollout("rollout-forged-outcome")
	forgedOutcome.Labels[v1alpha1.LabelOutcome] = string(v1alpha1.PhaseSucceeded)
	for _, forged := range []*v1alpha1.RuntimeRollout{rollout("rollout-forged"), forgedOutcome} {
		denied(t, "an administrator creating "+forged.Name, admin.Create(ctx, forged, client.DryRunAll), "only the AICR Operator creates rollouts")
	}

	ro := rollout("rollout-approval-test")
	if err := operator(t).Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), ro) })

	denied(t, "an editor approving", setApproval(ctx, t, eve, ro.Name, "eve", nil), needs)
	denied(t, "an approver naming someone else", setApproval(ctx, t, alice, ro.Name, "bob", nil), "approvedBy must be the approver's own username")
	denied(t, "an approval that also relabels", setApproval(ctx, t, alice, ro.Name, "alice", func(r *v1alpha1.RuntimeRollout) {
		r.Labels[v1alpha1.LabelOutcome] = string(v1alpha1.PhaseSucceeded)
	}), "only spec.approval may change")
	denied(t, "the operator approving", setApproval(ctx, t, operator(t), ro.Name, operatorUser, nil), "the operator never writes spec.approval")
	if err := setApproval(ctx, t, alice, ro.Name, "alice", nil); err != nil {
		t.Fatalf("an approver's own approval is denied: %v", err)
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(ro), ro); err != nil || ro.Spec.Approval == nil || ro.Spec.Approval.ApprovedBy != "alice" {
		t.Fatalf("approval not recorded: %v %+v", err, ro.Spec.Approval)
	}
	relabel := ro.DeepCopy()
	relabel.Labels["example.com/note"] = "x"
	denied(t, "an approver relabeling", alice.Patch(ctx, relabel, client.MergeFrom(ro), client.DryRunAll), "only spec.approval may change")

	denied(t, "an editor revoking", setRevocation(ctx, eve, ro.Name, "eve"), needs)
	denied(t, "a revocation naming someone else", setRevocation(ctx, bob, ro.Name, "alice"), "revokedBy must be the revoker's own username")
	if err := setRevocation(ctx, bob, ro.Name, "bob"); err != nil {
		t.Fatalf("an approver's own revocation is denied: %v", err)
	}

	// Revocation is refused once the steps are done.
	validating := rollout("rollout-validating-test")
	ended := rollout("rollout-ended-test")
	ended.Labels[v1alpha1.LabelOutcome] = string(v1alpha1.PhaseSucceeded)
	for _, r := range []*v1alpha1.RuntimeRollout{validating, ended} {
		if err := operator(t).Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Delete(context.Background(), r) })
		if err := setApproval(ctx, t, alice, r.Name, "alice", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(validating), validating); err != nil {
		t.Fatal(err)
	}
	validating.Status.Phase = v1alpha1.PhaseValidating
	if err := admin.Status().Update(ctx, validating); err != nil {
		t.Fatal(err)
	}
	const late = "can no longer be revoked"
	denied(t, "revoking a validating rollout", setRevocation(ctx, bob, validating.Name, "bob"), late)
	denied(t, "revoking an ended rollout", setRevocation(ctx, bob, ended.Name, "bob"), late)

	acknowledgements(ctx, t, alice, bob, eve)

	approvers := []string{"aicr-approvers"}
	secrets := allowed(ctx, t, "alice", approvers, authorizationv1.ResourceAttributes{Verb: "get", Resource: "secrets", Namespace: operatorNS})
	reports := allowed(ctx, t, "alice", approvers, authorizationv1.ResourceAttributes{Verb: "get", Resource: "configmaps", Namespace: operatorNS})
	if secrets || !reports {
		t.Errorf("approvers read Secrets %t, assessment reports %t", secrets, reports)
	}
}

func pluginCommands(t *testing.T) {
	ctx := ctxFor(t)
	ro := rollout("rollout-plugin-test")
	ro.Spec.ApprovalRequired = true
	if err := operator(t).Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), ro) })
	alice := kubeconfigFor(t, "alice", "aicr-approvers")
	bob := kubeconfigFor(t, "bob", "aicr-approvers")
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := plugin.Run(ctx, args, &out)
		return out.String(), err
	}

	out, err := run("describe", ro.Name, "--kubeconfig", alice)
	if err != nil || !strings.Contains(out, "gpu-operator") || !strings.Contains(out, "required, pending") {
		t.Fatalf("describe: %v\n%s", err, out)
	}
	out, err = run("approve", ro.Name, "--kubeconfig", alice, "--reason", "maintenance window", "--expires", "90m")
	if err != nil || !strings.Contains(out, "approved by alice") {
		t.Fatalf("approve: %v\n%s", err, out)
	}
	out, err = run("revoke", ro.Name, "--kubeconfig", bob, "--reason", "training job not checkpointed")
	if err != nil || !strings.Contains(out, "revoked by bob") {
		t.Fatalf("revoke: %v\n%s", err, out)
	}
	if err = admin.Get(ctx, client.ObjectKeyFromObject(ro), ro); err != nil {
		t.Fatal(err)
	}
	a := ro.Spec.Approval
	if a == nil || a.ApprovedBy != "alice" || a.RevokedBy != "bob" || time.Until(a.ExpiresAt.Time) > 91*time.Minute {
		t.Fatalf("recorded approval = %+v", a)
	}
	if out, err = run("describe", ro.Name, "--kubeconfig", alice); err != nil || !strings.Contains(out, "revoked by bob") {
		t.Fatalf("describe after revoke: %v\n%s", err, out)
	}
	// Someone without the approver roles cannot approve through the plugin.
	eve := kubeconfigFor(t, "mallory")
	other := rollout("rollout-plugin-denied")
	other.Spec.ApprovalRequired = true
	if err = operator(t).Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), other) })
	if _, err = run("approve", other.Name, "--kubeconfig", eve, "--reason", "x"); !apierrors.IsForbidden(err) {
		t.Fatalf("a non-approver's plugin approval: %v", err)
	}

	// A paused manual release is acknowledged as the signed-in approver.
	manual := rollout("rollout-plugin-acknowledge")
	manual.Spec.ApprovalRequired = true
	manual.Spec.Steps[0].PostApplySteps = []string{"reset-breaker", "verify"}
	if err = operator(t).Create(ctx, manual); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), manual) })
	if _, err = run("approve", manual.Name, "--kubeconfig", alice, "--reason", "window"); err != nil {
		t.Fatal(err)
	}
	if err = admin.Get(ctx, client.ObjectKeyFromObject(manual), manual); err != nil {
		t.Fatal(err)
	}
	manual.Status.Phase = v1alpha1.PhaseAwaitingAcknowledgement
	manual.Status.Releases = []v1alpha1.ReleaseStatus{{ID: "gpu-operator", State: v1alpha1.ReleaseReady}}
	if err = admin.Status().Update(ctx, manual); err != nil {
		t.Fatal(err)
	}
	out, err = run("acknowledge", manual.Name, "--kubeconfig", bob)
	if err != nil || !strings.Contains(out, "bob acknowledged the post-apply steps of gpu-operator (reset-breaker, verify)") {
		t.Fatalf("acknowledge: %v\n%s", err, out)
	}
	if out, err = run("describe", manual.Name, "--kubeconfig", alice); err != nil || !strings.Contains(out, "gpu-operator by bob") {
		t.Fatalf("describe after acknowledge: %v\n%s", err, out)
	}
}

func labeledConfigMap(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: operatorNS, Labels: map[string]string{
			v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue, v1alpha1.LabelRole: v1alpha1.RolePlan,
		}},
		Data: map[string]string{"plan.json": "{}"},
	}
}

func operatorState(t *testing.T) {
	ctx := ctxFor(t)
	op := operator(t)
	const message = "only the AICR Operator creates or updates its ConfigMaps"
	plan := labeledConfigMap("aicr-plan-state-test")
	if err := op.Create(ctx, plan); err != nil {
		t.Fatalf("the operator cannot write its plan: %v", err)
	}
	denied(t, "an administrator creating a labeled ConfigMap", admin.Create(ctx, labeledConfigMap("forged"), client.DryRunAll), message)
	edited := plan.DeepCopy()
	edited.Data["plan.json"] = `{"forged":true}`
	denied(t, "an administrator editing a plan", admin.Update(ctx, edited, client.DryRunAll), message)
	relabeled := plan.DeepCopy()
	delete(relabeled.Labels, v1alpha1.LabelManagedBy)
	denied(t, "an administrator removing the label", admin.Update(ctx, relabeled, client.DryRunAll), message)

	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unlabeled", Namespace: operatorNS}}
	if err := admin.Create(ctx, other); err != nil {
		t.Fatalf("an unlabeled ConfigMap is denied: %v", err)
	}
	other.Labels = map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue}
	denied(t, "an administrator adding the label", admin.Update(ctx, other, client.DryRunAll), message)
	elsewhere := labeledConfigMap("elsewhere")
	elsewhere.Namespace = "default"
	if err := admin.Create(ctx, elsewhere, client.DryRunAll); err != nil {
		t.Errorf("a labeled ConfigMap outside the operator namespace is denied: %v", err)
	}
	// Deletion is RBAC's: the namespace controller deletes plans this way.
	if err := admin.Delete(ctx, plan); err != nil {
		t.Errorf("deleting a plan is denied: %v", err)
	}
	if err := admin.Delete(ctx, other); err != nil {
		t.Fatal(err)
	}
}

func tenants(t *testing.T) {
	ctx := ctxFor(t)
	ensureNamespace(ctx, t, "team-a", nil)
	// The built-in admin role in team-a: envtest runs no aggregation
	// controller, so bind the roles admin aggregates. The chart's roles never
	// aggregate into them (TestRolesStayExplicit).
	for _, role := range []string{"system:aggregate-to-admin", "system:aggregate-to-edit", "system:aggregate-to-view"} {
		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ReplaceAll(role, ":", "-"), Namespace: "team-a"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "Group", Name: "tenants"}},
		}
		if err := admin.Create(ctx, binding); client.IgnoreAlreadyExists(err) != nil {
			t.Fatal(err)
		}
	}
	const tenant = "alice"
	groups := []string{"tenants"}
	if !allowed(ctx, t, tenant, groups, authorizationv1.ResourceAttributes{Verb: "create", Group: "apps", Resource: "deployments", Namespace: "team-a"}) {
		t.Fatal("the tenant cannot manage its own namespace; the check proves nothing")
	}
	operatorGroup := v1alpha1.GroupVersion.Group
	surfaces := make([]authorizationv1.ResourceAttributes, 0, 64)
	for _, verb := range []string{"get", "list", "create", "update", "patch", "delete"} {
		surfaces = append(surfaces,
			authorizationv1.ResourceAttributes{Verb: verb, Group: operatorGroup, Resource: "clusterruntimes"},
			authorizationv1.ResourceAttributes{Verb: verb, Group: operatorGroup, Resource: "runtimerollouts"},
			authorizationv1.ResourceAttributes{Verb: verb, Group: "argoproj.io", Resource: "applications", Namespace: engineNS},
			authorizationv1.ResourceAttributes{Verb: verb, Group: "argoproj.io", Resource: "appprojects", Namespace: engineNS},
		)
	}
	for _, ns := range []string{operatorNS, engineNS, jobsNS, discoveryNS, validationNS, "gpu-operator"} {
		surfaces = append(surfaces,
			authorizationv1.ResourceAttributes{Verb: "get", Resource: "configmaps", Namespace: ns},
			authorizationv1.ResourceAttributes{Verb: "create", Resource: "configmaps", Namespace: ns},
			authorizationv1.ResourceAttributes{Verb: "get", Resource: "secrets", Namespace: ns},
			authorizationv1.ResourceAttributes{Verb: "create", Group: "batch", Resource: "jobs", Namespace: ns},
			authorizationv1.ResourceAttributes{Verb: "create", Resource: "pods", Namespace: ns},
		)
	}
	surfaces = append(surfaces,
		authorizationv1.ResourceAttributes{Verb: "update", Group: operatorGroup, Resource: "runtimerollouts", Subresource: "approve"},
		authorizationv1.ResourceAttributes{Verb: "update", Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicies"},
		authorizationv1.ResourceAttributes{Verb: "delete", Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicybindings"},
		authorizationv1.ResourceAttributes{Verb: "update", Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"},
		authorizationv1.ResourceAttributes{Verb: "patch", Resource: "nodes"},
		authorizationv1.ResourceAttributes{Verb: "create", Resource: "namespaces"},
		authorizationv1.ResourceAttributes{Verb: "patch", Resource: "namespaces", Name: "team-a"},
		authorizationv1.ResourceAttributes{Verb: "create", Group: "resource.k8s.io", Resource: "deviceclasses"},
		authorizationv1.ResourceAttributes{Verb: "update", Group: "resource.k8s.io", Resource: "resourceslices"},
		authorizationv1.ResourceAttributes{Verb: "create", Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings"},
	)
	for _, a := range surfaces {
		if allowed(ctx, t, tenant, groups, a) {
			t.Errorf("a tenant may %s %s/%s %s in %q", a.Verb, a.Group, a.Resource, a.Subresource, a.Namespace)
		}
	}
}

// admitted creates a Pod from a template in a namespace with a server-side
// dry run, so Pod Security admission judges it at the namespace's level.
func admitted(ctx context.Context, t *testing.T, ns string, tmpl corev1.PodTemplateSpec) error {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: "pss-", Namespace: ns, Labels: tmpl.Labels}, Spec: tmpl.Spec}
	return admin.Create(ctx, pod, client.DryRunAll)
}

func podSecurity(t *testing.T) {
	ctx := ctxFor(t)
	levels := map[string]string{
		jobsNS: "restricted", discoveryNS: "privileged", validationNS: "privileged",
		"gpu-operator": "privileged", "cert-manager": "restricted",
	}
	for ns, want := range levels {
		got := &corev1.Namespace{}
		if err := admin.Get(ctx, client.ObjectKey{Name: ns}, got); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"enforce", "audit", "warn"} {
			if level := got.Labels["pod-security.kubernetes.io/"+mode]; level != want {
				t.Errorf("namespace %s %s level %q, want %q", ns, mode, level, want)
			}
		}
	}
	deployment := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: "aicr-operator"}, deployment); err != nil {
		t.Fatal(err)
	}
	if err := admitted(ctx, t, operatorNS, deployment.Spec.Template); err != nil {
		t.Errorf("the operator pod is not restricted: %v", err)
	}
	for _, stage := range []aicrjob.Stage{aicrjob.StageRecipe, aicrjob.StageRender, aicrjob.StageAssessment} {
		if err := admitted(ctx, t, jobsNS, job(t, stage, "aicr-jobs").Spec.Template); err != nil {
			t.Errorf("the %s Job's pod is not restricted: %v", stage, err)
		}
	}
	// The probe: a host-network pod is refused where the profile is restricted.
	host := job(t, aicrjob.StageRecipe, "aicr-jobs").Spec.Template
	host.Spec.HostNetwork = true
	if err := admitted(ctx, t, jobsNS, host); err == nil || !strings.Contains(err.Error(), "violates PodSecurity") {
		t.Errorf("a host-network pod in %s: %v, want a PodSecurity violation", jobsNS, err)
	}
}

func upgradeCRDs(t *testing.T) {
	ctx := ctxFor(t)
	// Copy the chart and change one CRD's schema, as an operator upgrade does.
	copyDir := filepath.Join(t.TempDir(), "aicr-operator")
	if err := os.CopyFS(copyDir, os.DirFS(chartDir)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(copyDir, "files", "crds", "operator.aicr.run_clusterruntimes.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err = yaml.Unmarshal(raw, crd); err != nil {
		t.Fatal(err)
	}
	const marker = "upgraded schema"
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = marker
	if raw, err = yaml.Marshal(crd); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, upErr := helm(ctx, "upgrade", release, copyDir, "--namespace", operatorNS, "-f", valuesFile); upErr != nil {
		t.Fatalf("helm upgrade: %v\n%s", upErr, out)
	}
	live := &apiextensionsv1.CustomResourceDefinition{}
	key := client.ObjectKey{Name: crd.Name}
	if err = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		return admin.Get(ctx, key, live) == nil && live.Spec.Versions[0].Schema.OpenAPIV3Schema.Description == marker, nil
	}); err != nil {
		t.Errorf("the CRD was not upgraded: %v", err)
	}
	if out, upErr := helm(ctx, "upgrade", release, chartDir, "--namespace", operatorNS, "-f", valuesFile); upErr != nil {
		t.Fatalf("helm upgrade back: %v\n%s", upErr, out)
	}
	if err = admin.Get(ctx, key, live); err != nil || live.Spec.Versions[0].Schema.OpenAPIV3Schema.Description == marker {
		t.Errorf("the CRD was not upgraded back: %v", err)
	}
}

func uninstall(t *testing.T) {
	ctx := ctxFor(t)
	err := admin.Create(ctx, runtimeObject(t))
	if err != nil {
		t.Fatal(err)
	}
	ro := rollout("rollout-kept")
	if err = operator(t).Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	plan := labeledConfigMap("aicr-plan-kept")
	if err = operator(t).Create(ctx, plan); err != nil {
		t.Fatal(err)
	}

	if out, unErr := helm(ctx, "uninstall", release, "--namespace", operatorNS); unErr != nil {
		t.Fatalf("helm uninstall: %v\n%s", unErr, out)
	}
	present := func(obj client.Object, gvk schema.GroupVersionKind) bool {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		getErr := admin.Get(ctx, client.ObjectKeyFromObject(obj), u)
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			t.Fatal(getErr)
		}
		return getErr == nil
	}
	named := func(ns, name string) client.Object {
		return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}
	gvk := func(group, version, kind string) schema.GroupVersionKind {
		return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
	}
	crdGVK := gvk("apiextensions.k8s.io", "v1", "CustomResourceDefinition")
	vapGVK := gvk("admissionregistration.k8s.io", "v1", "ValidatingAdmissionPolicy")
	bindingGVK := gvk("admissionregistration.k8s.io", "v1", "ValidatingAdmissionPolicyBinding")
	namespaceGVK := gvk("", "v1", "Namespace")
	kept := []struct {
		obj client.Object
		gvk schema.GroupVersionKind
	}{
		{named("", jobsNS), namespaceGVK},
		{named("", discoveryNS), namespaceGVK},
		{named("", validationNS), namespaceGVK},
		{named("", "gpu-operator"), namespaceGVK},
		{named("", "cert-manager"), namespaceGVK},
		{named("", "clusterruntimes.operator.aicr.run"), crdGVK},
		{named("", "runtimerollouts.operator.aicr.run"), crdGVK},
		{named("", v1alpha1.SingletonName), v1alpha1.GroupVersion.WithKind("ClusterRuntime")},
		{named("", ro.Name), v1alpha1.GroupVersion.WithKind("RuntimeRollout")},
		{named(operatorNS, plan.Name), gvk("", "v1", "ConfigMap")},
		{named(engineNS, "aicr"), gvk("argoproj.io", "v1alpha1", "AppProject")},
		{named(engineNS, "aicr-registry"), gvk("", "v1", "Secret")},
		{named("", "aicr-operator-state"), vapGVK},
		{named("", "aicr-operator-state"), bindingGVK},
		{named("", "aicr-operator-approval"), vapGVK},
		{named("", "aicr-operator-approval"), bindingGVK},
	}
	for _, k := range kept {
		if !present(k.obj, k.gvk) {
			t.Errorf("uninstall removed %s %s", k.gvk.Kind, k.obj.GetName())
		}
	}
	for _, gone := range []struct {
		obj client.Object
		gvk schema.GroupVersionKind
	}{
		{named(operatorNS, "aicr-operator"), gvk("apps", "v1", "Deployment")},
		{named(operatorNS, "aicr-operator"), gvk("", "v1", "ServiceAccount")},
		{named("", "aicr-operator"), gvk("rbac.authorization.k8s.io", "v1", "ClusterRole")},
		{named("", "aicr-operator-applications"), vapGVK},
		{named("", "aicr-operator-application-deletes"), vapGVK},
		{named("", "aicr-operator-jobs"), vapGVK},
		{named("", "aicr-operator-rollout-outcome"), vapGVK},
		{named("", "aicr-operator-runtime"), vapGVK},
	} {
		if present(gone.obj, gone.gvk) {
			t.Errorf("uninstall left %s %s", gone.gvk.Kind, gone.obj.GetName())
		}
	}

	// With no operator installed, operator state stays protected.
	const message = "only the AICR Operator creates or updates its ConfigMaps"
	denied(t, "creating a plan after uninstall", admin.Create(ctx, labeledConfigMap("forged"), client.DryRunAll), message)
	edited := plan.DeepCopy()
	if err = admin.Get(ctx, client.ObjectKeyFromObject(plan), edited); err != nil {
		t.Fatal(err)
	}
	edited.Data["plan.json"] = `{"forged":true}`
	denied(t, "editing a plan after uninstall", admin.Update(ctx, edited, client.DryRunAll), message)
	// Approval stays guarded too: an identity that may patch rollouts but
	// not approve them cannot approve a kept rollout.
	denied(t, "approving after uninstall", setApproval(ctx, t, as(t, "eve"), ro.Name, "eve", nil),
		"needs update on runtimerollouts/approve")
	denied(t, "creating a rollout after uninstall", admin.Create(ctx, rollout("rollout-forged"), client.DryRunAll),
		"only the AICR Operator creates rollouts")

	// A reinstall adopts what the uninstall kept.
	if out, inErr := helm(ctx, "install", release, chartDir, "--namespace", operatorNS, "-f", valuesFile); inErr != nil {
		t.Fatalf("reinstall: %v\n%s", inErr, out)
	}
	policy := &admissionv1.ValidatingAdmissionPolicy{}
	if err = admin.Get(ctx, client.ObjectKey{Name: "aicr-operator-applications"}, policy); err != nil {
		t.Errorf("reinstall did not restore the operator's constraints: %v", err)
	}
}
