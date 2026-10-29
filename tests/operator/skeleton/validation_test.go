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
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
)

// validationGates requires deployment validation for a new plan. Its steps
// land, then the operator runs AICR's validation as the chart's launcher in
// the validation namespace against the snapshot discovery kept. The
// skeleton's EKS recipe requires Ubuntu, so AICR's readiness pre-flight
// refuses kind's Debian node before any check: the rollout ends Failed with
// AICR's exit category and cause, records no evidence, and the plan is
// never promoted. Turning validation off rolls the runtime back to the
// active plan, Ready with reason ValidationDisabled.
func validationGates(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	rt := getRuntime(ctx, t)
	saved := rt.Spec.DeepCopy()
	restored := false
	restore := func() {
		current := getRuntime(ctx, t)
		patch := client.MergeFrom(current.DeepCopy())
		current.Spec = *saved
		if err := k8s.Patch(ctx, current, patch); err != nil {
			t.Fatal(err)
		}
		restored = true
	}
	// Later subtests start from the active plan, whatever happens here.
	defer func() {
		if !restored {
			restore()
			waitReadyOn(ctx, t, active)
		}
	}()
	patch := client.MergeFrom(rt.DeepCopy())
	rt.Spec.Validation.Deployment = v1alpha1.DeploymentValidationRequired
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	setCPU(ctx, t, `"20m"`)
	candidate := waitResolved(ctx, t)
	if candidate == active {
		t.Fatalf("the CPU change resolved to the active plan %s", active)
	}

	seen := map[string]string{}
	var ro *v1alpha1.RuntimeRollout
	waitFor(t, rolloutTimeout, "the validation gate to end the rollout to "+candidate, func() bool {
		jobs := &batchv1.JobList{}
		if err := k8s.List(ctx, jobs, client.InNamespace("aicr-validation"),
			client.MatchingLabels{v1alpha1.LabelStage: string(aicrjob.StageValidation)}); err == nil {
			for _, j := range jobs.Items {
				seen[j.Name] = j.Spec.Template.Spec.ServiceAccountName
			}
		}
		// The outcome label lands before the status that explains it, so
		// wait for both.
		for _, r := range rolloutsTo(ctx, t, candidate) {
			if outcome := r.Labels[v1alpha1.LabelOutcome]; outcome != "" && string(r.Status.Phase) == outcome {
				ro = &r
				return true
			}
		}
		return false
	})
	v := ro.Status.Validation
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || v == nil || v.Result != "Failed" || v.Evidence != "" ||
		ro.Annotations[v1alpha1.AnnotationEvidence] != "" || !strings.HasPrefix(v.Job, "aicr-validation-") ||
		!strings.Contains(ro.Status.Message, "deployment validation failed") || !strings.Contains(ro.Status.Message, "aicr validation exited 2") ||
		!strings.Contains(ro.Status.Message, "readiness check failed") {

		t.Fatalf("rollout %s: labels %v, annotations %v, status %+v", ro.Name, ro.Labels, ro.Annotations, ro.Status)
	}
	if account, ok := seen[v.Job]; ok && account != "aicr-validation" {
		t.Errorf("validation Job %s ran as %s", v.Job, account)
	}
	t.Logf("rollout %s failed validation run %s: %s", ro.Name, v.RunID, ro.Status.Message)
	// The runtime reports the outcome on the rollout controller's next
	// reconcile.
	waitFor(t, 2*time.Minute, "the runtime to stall on the failed rollout", func() bool {
		current := getRuntime(ctx, t)
		stalled := meta.FindStatusCondition(current.Status.Conditions, v1alpha1.ConditionStalled)
		if current.Status.ActivePlanDigest != active {
			t.Fatalf("a failed validation promoted %s: %s", current.Status.ActivePlanDigest, statusJSON(ctx, t))
		}
		return stalled != nil && stalled.Status == metav1.ConditionTrue && stalled.ObservedGeneration == current.Generation
	})

	restore()
	waitReadyOn(ctx, t, active)
	waitFor(t, time.Minute, "Ready with reason ValidationDisabled", func() bool {
		c := meta.FindStatusCondition(getRuntime(ctx, t).Status.Conditions, v1alpha1.ConditionReady)
		return c != nil && c.Reason == "ValidationDisabled"
	})
}
