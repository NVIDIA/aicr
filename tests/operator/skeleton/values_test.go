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
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/resolver"
)

func override(component, path, value string) v1alpha1.Override {
	return v1alpha1.Override{Component: component, Path: path, Value: apiextensionsv1.JSON{Raw: []byte(value)}}
}

// setOverrides replaces the runtime's overrides with base plus extra, an
// extra override replacing a base one with the same component and path.
func setOverrides(ctx context.Context, t *testing.T, base []v1alpha1.Override, extra ...v1alpha1.Override) {
	t.Helper()
	rt := getRuntime(ctx, t)
	patch := client.MergeFrom(rt.DeepCopy())
	var out []v1alpha1.Override
	for _, o := range base {
		replaced := false
		for _, e := range extra {
			replaced = replaced || (e.Component == o.Component && e.Path == o.Path)
		}
		if !replaced {
			out = append(out, o)
		}
	}
	out = append(out, extra...)
	rt.Spec.Overrides = out
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatalf("set overrides: %v", err)
	}
}

// rejectedOverrides applies overrides that AICR's render rejects: each
// stalls with AICR's exit category and AICR's message, and nothing reaches
// the engine. Profile-locked paths need a GKE recipe and are covered by
// the AICR e2e suite (TestRenderJobRejectsOverrides).
func rejectedOverrides(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	base := getRuntime(ctx, t).Spec.Overrides
	before := applicationVersion(ctx, t)
	for _, c := range []struct {
		name     string
		set      []v1alpha1.Override
		rejected string
	}{
		{"driver ownership", []v1alpha1.Override{
			override("gpu-operator", "enabled", "true"),
			override("nvidia-dra-driver-gpu", "enabled", "true"),
			override("gpu-operator", "driver.enabled", "false"),
		}, "nvidia-dra-driver-gpu still reads nvidiaDriverRoot"},
		{"disabled component", []v1alpha1.Override{override("nvsentinel", "labeler.logLevel", `"debug"`)},
			`component "nvsentinel" is not in the generated bundle`},
		{"enabling a component the recipe lacks", []v1alpha1.Override{override("network-operator", "enabled", "true")},
			`unknown component "network-operator"`},
	} {
		setOverrides(ctx, t, base, c.set...)
		if reason := waitStalled(ctx, t, c.rejected); reason != "AICRInvalidRequest" {
			t.Errorf("%s: Stalled reason = %q, want AICRInvalidRequest", c.name, reason)
		}
		assertNoEngineWrites(ctx, t, before, active)
	}
	setOverrides(ctx, t, base)
	waitReadyOn(ctx, t, active)
}

// typoProtection checks override paths against the base render and the
// chart defaults: a misspelled path and a new key both stall with zero
// engine writes; the new key, marked additive, rolls out. Removing it
// restores the earlier inputs, which resolve to the earlier plan digest.
func typoProtection(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	base := getRuntime(ctx, t).Spec.Overrides
	before := applicationVersion(ctx, t)
	label := override(release, "podLabels.e2e", `"additive"`)
	setOverrides(ctx, t, base, override(release, "resources.requsts.cpu", `"20m"`), label)
	reason := waitStalled(ctx, t, "cert-manager:podLabels.e2e, cert-manager:resources.requsts.cpu")
	if reason != resolver.ReasonOverridePathUnknown {
		t.Errorf("Stalled reason = %q, want %s", reason, resolver.ReasonOverridePathUnknown)
	}
	assertNoEngineWrites(ctx, t, before, active)

	label.Additive = true
	setOverrides(ctx, t, base, label)
	added := waitRolledOut(ctx, t, active)
	if got, _, _ := unstructured.NestedString(application(ctx, t).Object, "spec", "source", "helm", "valuesObject", "podLabels", "e2e"); got != "additive" {
		t.Errorf("Application podLabels.e2e = %q, want additive", got)
	}
	deployment := &appsv1.Deployment{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: release, Name: release}, deployment); err != nil {
		t.Fatal(err)
	}
	if got := deployment.Spec.Template.Labels["e2e"]; got != "additive" {
		t.Errorf("cert-manager pod template label e2e = %q, want additive", got)
	}
	t.Logf("additive path rolled out as plan %s", added)

	setOverrides(ctx, t, base)
	if again := waitRolledOut(ctx, t, added); again != active {
		t.Errorf("the earlier inputs resolved to plan %s, want %s", again, active)
	}
}

// statusCacheLoss clears the runtime's cached input and candidate digests:
// resolution runs every stage again and yields the same plan, which starts
// no rollout and leaves the Application as it is.
func statusCacheLoss(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	app := application(ctx, t)
	revision, _, _ := unstructured.NestedString(app.Object, "spec", "source", "targetRevision")
	rollouts := &v1alpha1.RuntimeRolloutList{}
	if err := k8s.List(ctx, rollouts); err != nil {
		t.Fatal(err)
	}
	count := len(rollouts.Items)

	rt := getRuntime(ctx, t)
	patch := client.MergeFrom(rt.DeepCopy())
	rt.Status.InputDigest, rt.Status.CandidatePlanDigest = "", ""
	if err := k8s.Status().Patch(ctx, rt, patch); err != nil {
		t.Fatalf("clear status digests: %v", err)
	}
	if run := nextRun(ctx, t, aicrjob.StageRecipe, ""); run == "" {
		t.Fatal("no recipe run after the status cache was cleared")
	}
	deadline := time.Now().Add(resolveTimeout)
	for {
		rt = getRuntime(ctx, t)
		c := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionResolved)
		if c != nil && c.Status == metav1.ConditionTrue && rt.Status.InputDigest != "" && rt.Status.CandidatePlanDigest != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not resolved again within %s; status %s", resolveTimeout, statusJSON(ctx, t))
		}
		time.Sleep(2 * time.Second)
	}
	if rt.Status.CandidatePlanDigest != active {
		t.Fatalf("identical inputs resolved to plan %s, want %s", rt.Status.CandidatePlanDigest, active)
	}
	waitReadyOn(ctx, t, active)
	if err := k8s.List(ctx, rollouts); err != nil {
		t.Fatal(err)
	}
	app = application(ctx, t)
	after, _, _ := unstructured.NestedString(app.Object, "spec", "source", "targetRevision")
	if len(rollouts.Items) != count || after != revision || app.GetAnnotations()[v1alpha1.AnnotationPlanDigest] != active {
		t.Errorf("re-resolving identical inputs created %d rollouts and moved the Application from %s to %s",
			len(rollouts.Items)-count, revision, after)
	}
}
