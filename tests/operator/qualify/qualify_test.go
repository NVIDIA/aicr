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
	"bufio"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// groupLabel puts the kind control plane in the system group and the
// workers in the accelerated group.
const groupLabel = "qualify.aicr.run/group"

// TestQualification runs the scenarios in order against one runtime; each
// starts from the active plan the previous one left.
func TestQualification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Minute)
	t.Cleanup(cancel)
	reset(ctx, t)
	labelNodes(ctx, t)

	var active string
	t.Run("the candidate runs by digest as a release build", func(t *testing.T) {
		runsCandidate(ctx, t, &candidate)
	})
	t.Run("a first rollout lands only once approved", func(t *testing.T) {
		active = firstRollout(ctx, t)
	})
	t.Run("killing the operator mid-step neither repeats nor loses the step", func(t *testing.T) {
		active = operatorKilled(ctx, t, active)
	})
	t.Run("a leader change mid-step hands the rollout to the other replica", func(t *testing.T) {
		active = leaderChange(ctx, t, active)
	})
	t.Run("a registry outage writes nothing until the registry returns", func(t *testing.T) {
		active = registryOutage(ctx, t, active)
	})
	t.Run("a missing credentials Secret stalls with its cause until restored", func(t *testing.T) {
		active = credentialsMissing(ctx, t, active)
	})
	t.Run("a newer rollout schema stops the operator with zero engine writes", func(t *testing.T) {
		active = newerSchema(ctx, t, active)
	})
	t.Run("an operator upgrade mid-rollout finishes the rollout", func(t *testing.T) {
		active = upgradeMidRollout(ctx, t, active)
	})
	t.Run("helm uninstall deletes no workload and a reinstall resumes", func(t *testing.T) {
		uninstallReinstall(ctx, t, active)
	})
	t.Run("the shipped RBAC denied the operator nothing", func(t *testing.T) {
		nothingForbidden(ctx, t)
	})
}

// reset removes what an earlier suite left on the cluster.
func reset(ctx context.Context, t *testing.T) {
	t.Helper()
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
}

func labelNodes(ctx context.Context, t *testing.T) {
	t.Helper()
	nodes := &corev1.NodeList{}
	if err := k8s.List(ctx, nodes); err != nil || len(nodes.Items) < 2 {
		t.Fatalf("list nodes: %v (%d nodes; recreate the cluster with make operator-kind-up)", err, len(nodes.Items))
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		patch := client.MergeFrom(n.DeepCopy())
		n.Labels[groupLabel] = "accelerated"
		if _, cp := n.Labels["node-role.kubernetes.io/control-plane"]; cp {
			n.Labels[groupLabel] = "system"
		}
		if err := k8s.Patch(ctx, n, patch); err != nil {
			t.Fatal(err)
		}
	}
}

// runsCandidate checks the operator runs the candidate's image by digest, as
// a release build with no development setting.
func runsCandidate(ctx context.Context, t *testing.T, c *candidateManifest) {
	t.Helper()
	want := c.Operator.Repository + "@" + c.Operator.Digest
	var pods []corev1.Pod
	waitFor(t, 3*time.Minute, "the operator to run "+want, func() bool {
		pods = operatorPods(ctx, t)
		for _, p := range pods {
			if len(p.Status.ContainerStatuses) == 0 || !p.Status.ContainerStatuses[0].Ready || p.Spec.Containers[0].Image != want {
				return false
			}
		}
		return len(pods) > 0
	})
	// The pods name the candidate by digest and the nodes hold it under that
	// digest only (tools/operator-qualify), so a running pod runs exactly
	// the candidate's image.
	args := operatorDeployment(ctx, t).Spec.Template.Spec.Containers[0].Args
	for _, flag := range []string{"--development-bypass-approvals=false", "--development-skip-discovery=false"} {
		if !slices.Contains(args, flag) {
			t.Errorf("the operator runs without %s: %v", flag, args)
		}
	}
	logs, err := clientset.CoreV1().Pods(operatorNamespace).GetLogs(pods[0].Name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logs), `"version":"`+c.Version+`"`) || !strings.Contains(string(logs), `"release":true`) {
		t.Fatalf("the operator does not report release build %s", c.Version)
	}
	t.Logf("operator %s runs candidate %s (%s)", want, c.Version, c.Digest)
}

// firstRollout creates the runtime paused, learns the kind recipe's
// components from its first plan, disables all but the release, and rolls
// that out: the rollout waits for an approval with nothing written, then
// lands with its Events recorded.
func firstRollout(ctx context.Context, t *testing.T) string {
	t.Helper()
	rt := &v1alpha1.ClusterRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName},
		Spec: v1alpha1.ClusterRuntimeSpec{
			Intent: "training", Platform: "kubeflow",
			// service comes from the snapshot (kind); kind's Debian has no recipe OS.
			Criteria: &v1alpha1.DeclaredCriteria{Accelerator: "h100", OS: "any"},
			Placement: v1alpha1.Placement{
				System:      v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{groupLabel: "system"}},
				Accelerated: v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{groupLabel: "accelerated"}},
			},
			Rollout: v1alpha1.RolloutPolicy{
				StabilizationWindow: &metav1.Duration{Duration: time.Second},
				ActionTimeout:       &metav1.Duration{Duration: 10 * time.Minute},
				Deadline:            &metav1.Duration{Duration: time.Hour},
			},
			Validation: v1alpha1.ValidationPolicy{Deployment: v1alpha1.DeploymentValidationDisabled},
			Paused:     true,
		},
	}
	if err := k8s.Create(ctx, rt); err != nil {
		t.Fatal(err)
	}
	components := recipeComponents(ctx, t, waitResolved(ctx, t))
	if !slices.Contains(components, release) {
		t.Fatalf("the kind recipe has no %s: %v", release, components)
	}
	patchRuntime(ctx, t, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Paused = false
		for _, c := range components {
			if c != release {
				rt.Spec.Overrides = append(rt.Spec.Overrides, v1alpha1.Override{Component: c, Path: "enabled", Value: apiextensionsv1.JSON{Raw: []byte("false")}})
			}
		}
		rt.Spec.Overrides = append(rt.Spec.Overrides, v1alpha1.Override{Component: release, Path: "resources.requests.cpu",
			Value: apiextensionsv1.JSON{Raw: []byte(`"50m"`)}})
	})
	digest := waitResolved(ctx, t)
	ro := awaitingApproval(ctx, t, digest)
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argocd.ApplicationGVK)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "argocd", Name: release}, app); err == nil &&
		app.GetAnnotations()[v1alpha1.AnnotationPlanDigest] == digest {

		t.Fatal("the Application was written before the approval")
	}
	approve(ctx, t, ro.Name)
	waitReadyOn(ctx, t, digest)
	rolledOut(ctx, t, digest)
	waitFor(t, time.Minute, "RolloutCreated on the runtime and RolloutSucceeded on "+ro.Name, func() bool {
		return recorded(ctx, t, v1alpha1.SingletonName, "RolloutCreated", ro.Name) && recorded(ctx, t, ro.Name, "RolloutSucceeded", ro.Name)
	})
	return digest
}

// recipeComponents reads a plan's recipe and returns its components.
func recipeComponents(ctx context.Context, t *testing.T, digest string) []string {
	t.Helper()
	doc := readPlan(ctx, t, digest)
	if doc.Recipe == nil {
		t.Fatalf("plan %s records no recipe", digest)
	}
	cm := &corev1.ConfigMap{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: doc.Recipe.ConfigMapRef.ConfigMap}, cm); err != nil {
		t.Fatal(err)
	}
	data, err := plan.ReadContent(cm, plan.RecipeDataKey)
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := aicrjob.ParseRecipeResult(data, aicrVersion)
	if err != nil {
		t.Fatal(err)
	}
	return recipe.DeploymentOrder
}

func readPlan(ctx context.Context, t *testing.T, digest string) *plan.Document {
	t.Helper()
	name, err := plan.Name(digest)
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err = k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: name}, cm); err != nil {
		t.Fatal(err)
	}
	doc, err := plan.FromConfigMap(cm)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// nextPlan changes the release's CPU request and returns the new plan with
// its rollout, awaiting approval.
func nextPlan(ctx context.Context, t *testing.T, active, cpu string) (string, *v1alpha1.RuntimeRollout) {
	t.Helper()
	setCPU(ctx, t, cpu)
	digest := waitResolved(ctx, t)
	if digest == active {
		t.Fatalf("CPU %s resolved to the active plan", cpu)
	}
	return digest, awaitingApproval(ctx, t, digest)
}

// operatorKilled kills the operator twice while a step is in flight: the
// replacement observes the step instead of starting it again, and the
// rollout ends with one rollout and one sync.
func operatorKilled(ctx context.Context, t *testing.T, active string) string {
	t.Helper()
	before := syncs(ctx, t)
	digest, ro := nextPlan(ctx, t, active, "40m")
	approve(ctx, t, ro.Name)
	waitStepStarted(ctx, t, ro.Name)
	for range 2 {
		for _, p := range operatorPods(ctx, t) {
			killPod(ctx, t, &p)
		}
		waitOperatorReady(ctx, t, 1)
	}
	waitReadyOn(ctx, t, digest)
	rolledOut(ctx, t, digest)
	if after := syncs(ctx, t); after != before+1 {
		t.Fatalf("the Application synced %d times for one step", after-before)
	}
	return digest
}

// leaderChange runs two replicas and kills the leader while a step is in
// flight: the other replica takes the Lease and finishes the rollout.
func leaderChange(ctx context.Context, t *testing.T, active string) string {
	t.Helper()
	scaleOperator(ctx, t, 2)
	defer scaleOperator(ctx, t, 1)
	before := syncs(ctx, t)
	digest, ro := nextPlan(ctx, t, active, "30m")
	approve(ctx, t, ro.Name)
	waitStepStarted(ctx, t, ro.Name)
	first := leader(ctx, t)
	if first == "" {
		t.Fatal("no leader holds the Lease")
	}
	for _, p := range operatorPods(ctx, t) {
		if p.Name == first {
			killPod(ctx, t, &p)
		}
	}
	waitFor(t, 2*time.Minute, "another replica to lead", func() bool {
		l := leader(ctx, t)
		return l != "" && l != first
	})
	waitReadyOn(ctx, t, digest)
	rolledOut(ctx, t, digest)
	if after := syncs(ctx, t); after != before+1 {
		t.Fatalf("the Application synced %d times for one step", after-before)
	}
	t.Logf("leader %s killed mid-step; %s finished rollout %s", first, leader(ctx, t), ro.Name)
	return digest
}

// written is the plan the Application was last written from.
func written(ctx context.Context, t *testing.T) string {
	t.Helper()
	return application(ctx, t).GetAnnotations()[v1alpha1.AnnotationPlanDigest]
}

// registryOutage points the registry Service at no pod, so every registry
// call fails, and changes the inputs: resolution reports the outage and
// retries, no rollout starts and the engine is not written; restoring the
// Service resumes resolution with no intervention.
func registryOutage(ctx context.Context, t *testing.T, active string) string {
	t.Helper()
	svc := &corev1.Service{}
	key := client.ObjectKey{Namespace: "aicr-registry", Name: "registry"}
	if err := k8s.Get(ctx, key, svc); err != nil {
		t.Fatal(err)
	}
	selector := svc.Spec.Selector
	setSelector := func(s map[string]string) {
		if err := k8s.Get(ctx, key, svc); err != nil {
			t.Fatal(err)
		}
		patch := client.MergeFrom(svc.DeepCopy())
		svc.Spec.Selector = s
		if err := k8s.Patch(ctx, svc, patch); err != nil {
			t.Fatal(err)
		}
	}
	setSelector(map[string]string{"app": "registry-outage"})
	restored := false
	defer func() {
		if !restored {
			setSelector(selector)
		}
	}()
	setCPU(ctx, t, "20m")
	var message string
	waitFor(t, resolveTimeout, "Resolved=False naming the registry outage", func() bool {
		c := cond(getRuntime(ctx, t), v1alpha1.ConditionResolved)
		message = c.Message
		return c.Status == metav1.ConditionFalse && strings.Contains(strings.ToLower(c.Message), "registry")
	})
	time.Sleep(30 * time.Second)
	rt := getRuntime(ctx, t)
	if c := cond(rt, v1alpha1.ConditionResolved); c.Status != metav1.ConditionFalse || written(ctx, t) != active || rt.Status.Rollout != "" {
		t.Fatalf("during the outage: Resolved %+v, Application at %s, rollout %q", c, written(ctx, t), rt.Status.Rollout)
	}
	t.Logf("during the outage, Resolved=False: %s", message)
	setSelector(selector)
	restored = true
	digest := waitResolved(ctx, t)
	ro := awaitingApproval(ctx, t, digest)
	approve(ctx, t, ro.Name)
	waitReadyOn(ctx, t, digest)
	return digest
}

// credentialsMissing deletes the operator's static registry credentials and
// changes the inputs: resolution stalls with CredentialsUnavailable and
// nothing is written; restoring the Secret resumes it on the resync.
func credentialsMissing(ctx context.Context, t *testing.T, active string) string {
	t.Helper()
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: operatorNamespace, Name: "registry-auth"}
	if err := k8s.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	saved := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secret.Name, Namespace: secret.Namespace}, Type: secret.Type, Data: secret.Data}
	if err := k8s.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	restored := false
	defer func() {
		if !restored {
			_ = k8s.Create(context.WithoutCancel(ctx), saved.DeepCopy())
		}
	}()
	setCPU(ctx, t, "25m")
	waitFor(t, resolveTimeout, "Resolved=False with CredentialsUnavailable", func() bool {
		c := cond(getRuntime(ctx, t), v1alpha1.ConditionResolved)
		return c.Status == metav1.ConditionFalse && c.Reason == registry.ReasonCredentialsUnavailable
	})
	if rt := getRuntime(ctx, t); written(ctx, t) != active || rt.Status.Rollout != "" {
		t.Fatalf("without credentials: Application at %s, rollout %q", written(ctx, t), rt.Status.Rollout)
	}
	if err := k8s.Create(ctx, saved); err != nil {
		t.Fatal(err)
	}
	restored = true
	waitFor(t, 2*time.Minute, "resolution to resume once the Secret is back", func() bool {
		return cond(getRuntime(ctx, t), v1alpha1.ConditionResolved).Reason != registry.ReasonCredentialsUnavailable
	})
	digest := waitResolved(ctx, t)
	ro := awaitingApproval(ctx, t, digest)
	approve(ctx, t, ro.Name)
	waitReadyOn(ctx, t, digest)
	return digest
}

// newerSchema adds a rollout as a newer operator would write it: the
// candidate stops with PlanUnreadable naming it, starts no rollout for new
// inputs and writes nothing to the engine; removing it resumes.
func newerSchema(ctx context.Context, t *testing.T, active string) string {
	t.Helper()
	rt := getRuntime(ctx, t)
	newer := &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "rollout-from-a-newer-operator", Labels: map[string]string{v1alpha1.LabelRuntime: rt.Name}},
		Spec: v1alpha1.RuntimeRolloutSpec{
			SchemaVersion: v1alpha1.RolloutSchemaVersion + 1, RuntimeUID: string(rt.UID), ToPlanDigest: active,
			StartingInventoryDigest: active, Deadline: metav1.NewTime(time.Now().Add(time.Hour)),
			ActionTimeout: metav1.Duration{Duration: time.Minute},
			Steps:         []v1alpha1.RolloutStep{{Release: release, Operation: v1alpha1.OperationUpgrade, Assessment: v1alpha1.AssessmentNotApplicable}},
		},
	}
	if err := k8s.Create(ctx, newer); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Minute, "Stalled with PlanUnreadable naming the newer rollout", func() bool {
		c := cond(getRuntime(ctx, t), v1alpha1.ConditionStalled)
		return c.Reason == plan.ReasonPlanUnreadable && strings.Contains(c.Message, newer.Name)
	})
	setCPU(ctx, t, "45m")
	digest := waitResolved(ctx, t)
	time.Sleep(45 * time.Second)
	if n := len(rolloutsTo(ctx, t, digest)); n != 0 || written(ctx, t) != active {
		t.Fatalf("with a newer rollout: %d rollouts to %s, Application at %s", n, digest, written(ctx, t))
	}
	if err := k8s.Delete(ctx, newer); err != nil {
		t.Fatal(err)
	}
	ro := awaitingApproval(ctx, t, digest)
	approve(ctx, t, ro.Name)
	waitReadyOn(ctx, t, digest)
	return digest
}

// upgradeMidRollout upgrades the operator to the next candidate, chart and
// image, while a step is in flight: the new operator finishes the rollout
// with one sync and creates no other.
func upgradeMidRollout(ctx context.Context, t *testing.T, active string) string {
	t.Helper()
	before := syncs(ctx, t)
	digest, ro := nextPlan(ctx, t, active, "35m")
	approve(ctx, t, ro.Name)
	waitStepStarted(ctx, t, ro.Name)
	t.Setenv("CANDIDATE_NEXT_DIR", filepath.Dir(os.Getenv("AICR_OPERATOR_QUALIFY_NEXT")))
	run(ctx, t, "tools/operator-qualify", "upgrade")
	runsCandidate(ctx, t, &next)
	waitReadyOn(ctx, t, digest)
	rolledOut(ctx, t, digest)
	if after := syncs(ctx, t); after != before+1 {
		t.Fatalf("the Application synced %d times for one step", after-before)
	}
	return digest
}

// workloadPods are the release's pods by UID.
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

// uninstallReinstall uninstalls the operator with the runtime present: the
// workload, the Application and the runtime stay; a reinstall resumes on the
// same runtime and active plan with no rollout.
func uninstallReinstall(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	rt := getRuntime(ctx, t)
	pods := workloadPods(ctx, t)
	app := application(ctx, t)
	rollouts := &v1alpha1.RuntimeRolloutList{}
	if err := k8s.List(ctx, rollouts); err != nil {
		t.Fatal(err)
	}
	run(ctx, t, "helm", "uninstall", "aicr-operator", "--namespace", operatorNamespace,
		"--kubeconfig", os.Getenv("AICR_OPERATOR_E2E_KUBECONFIG"), "--wait")
	time.Sleep(20 * time.Second)
	if got := getRuntime(ctx, t); got.UID != rt.UID || application(ctx, t).GetUID() != app.GetUID() || !slices.Equal(workloadPods(ctx, t), pods) {
		t.Fatal("helm uninstall changed the runtime, the Application or the workload")
	}
	t.Setenv("CANDIDATE_NEXT_DIR", filepath.Dir(os.Getenv("AICR_OPERATOR_QUALIFY_NEXT")))
	run(ctx, t, "tools/operator-qualify", "upgrade")
	waitReadyOn(ctx, t, active)
	after := &v1alpha1.RuntimeRolloutList{}
	if err := k8s.List(ctx, after); err != nil {
		t.Fatal(err)
	}
	if len(after.Items) != len(rollouts.Items) || !slices.Equal(workloadPods(ctx, t), pods) {
		t.Fatalf("the reinstall made %d rollouts (from %d) or changed the workload", len(after.Items), len(rollouts.Items))
	}
}

// nothingForbidden scans the running operator's log for an API denial: the
// suite installs the RBAC the chart ships, so a missing grant shows here.
func nothingForbidden(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, p := range operatorPods(ctx, t) {
		logs, err := clientset.CoreV1().Pods(operatorNamespace).GetLogs(p.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(strings.NewReader(string(logs)))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if strings.Contains(strings.ToLower(sc.Text()), "forbidden") {
				t.Errorf("%s: %s", p.Name, sc.Text())
			}
		}
	}
}
