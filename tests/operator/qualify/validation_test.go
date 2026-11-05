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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
)

// validationNamespace is the chart's default validation namespace.
const validationNamespace = "aicr-validation"

// deploymentValidation upgrades the operator release to the next candidate
// with validation enabled, requires deployment validation for a new plan,
// and approves its rollout. The candidate's operator runs AICR's validation
// stage with the candidate's AICR image, as the chart's launcher in the
// validation namespace. A kind cluster has no GPU, so validation fails: the
// rollout ends Failed and the plan is never promoted. The failed rollout
// left the release at its values, so returning the target to the active plan
// is an approved rollout back to it. The operator, validating, is denied
// nothing.
func deploymentValidation(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	t.Setenv("CANDIDATE_NEXT_DIR", filepath.Dir(os.Getenv("AICR_OPERATOR_QUALIFY_NEXT")))
	t.Setenv("QUALIFY_VALIDATION", "1")
	run(ctx, t, "tools/operator-qualify", "upgrade")
	runsCandidate(ctx, t, &next)
	waitReadyOn(ctx, t, active)

	saved := getRuntime(ctx, t).Spec.DeepCopy()
	restored := false
	restore := func() {
		patchRuntime(ctx, t, func(rt *v1alpha1.ClusterRuntime) { rt.Spec = *saved })
		restored = true
		// Earlier rollouts to the active plan succeeded; the one back to it
		// waits for an approval. A scenario that failed before its rollout
		// wrote the release returns to the active plan with none.
		var back *v1alpha1.RuntimeRollout
		waitFor(t, rolloutTimeout, "a rollout back to "+active+" awaiting approval, or Ready on it", func() bool {
			for _, r := range rolloutsTo(ctx, t, active) {
				if r.Status.Phase == v1alpha1.PhaseAwaitingApproval {
					back = &r
					return true
				}
			}
			rt := getRuntime(ctx, t)
			return cond(rt, v1alpha1.ConditionReady).Status == metav1.ConditionTrue && rt.Status.ActivePlanDigest == active
		})
		if back != nil {
			approve(ctx, t, back.Name)
		}
		waitReadyOn(ctx, t, active)
	}
	// Later scenarios start from the active plan, whatever happens here.
	defer func() {
		if !restored {
			restore()
		}
	}()

	patchRuntime(ctx, t, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Validation.Deployment = v1alpha1.DeploymentValidationRequired
	})
	// A CPU request no other scenario uses, under cert-manager's 50m limit,
	// so the plan is new and its sync succeeds.
	digest, ro := nextPlan(ctx, t, active, "10m")
	approve(ctx, t, ro.Name)

	jobs := map[string]batchv1.Job{}
	var ended *v1alpha1.RuntimeRollout
	waitFor(t, rolloutTimeout+5*time.Minute, "the validation gate to end the rollout to "+digest, func() bool {
		list := &batchv1.JobList{}
		if err := k8s.List(ctx, list, client.InNamespace(validationNamespace),
			client.MatchingLabels{v1alpha1.LabelStage: string(aicrjob.StageValidation)}); err == nil {
			for _, j := range list.Items {
				jobs[j.Name] = j
			}
		}
		// The outcome label lands before the status that explains it.
		for _, r := range rolloutsTo(ctx, t, digest) {
			if outcome := r.Labels[v1alpha1.LabelOutcome]; outcome != "" && string(r.Status.Phase) == outcome {
				ended = &r
				return true
			}
		}
		return false
	})
	// AICR ran and refused the request: exit 2 is a structured
	// INVALID_REQUEST, which an argument it cannot parse (exit 1) or a
	// container that never starts does not produce, and the excerpt is AICR's
	// own log output. On kind the suite's recipe has its GPU advertisers off.
	v := ended.Status.Validation
	if ended.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || v == nil || v.Result != "Failed" ||
		!strings.Contains(ended.Status.Message, "deployment validation failed") ||
		!strings.Contains(ended.Status.Message, "aicr validation exited 2: [cli] ") {

		t.Fatalf("rollout %s: labels %v, status %+v; want deployment validation to fail it", ended.Name, ended.Labels, ended.Status)
	}
	job, ok := jobs[v.Job]
	if !ok {
		t.Fatalf("the operator ran no validation Job named %q in %s", v.Job, validationNamespace)
	}
	// tools/operator-qualify installs AICR_E2E_IMAGE over the candidate's.
	image := next.AICR.Image
	if override := os.Getenv("AICR_E2E_IMAGE"); override != "" {
		image = override
	}
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != validationNamespace || len(pod.Containers) == 0 || pod.Containers[0].Image != image {
		t.Fatalf("validation Job %s ran as %s with %v; want the launcher running the installed AICR %s",
			job.Name, pod.ServiceAccountName, pod.Containers, image)
	}
	t.Logf("rollout %s failed validation run %s with %s: %s", ended.Name, v.RunID, image, ended.Status.Message)

	waitFor(t, 2*time.Minute, "the runtime to stall on the failed rollout", func() bool {
		rt := getRuntime(ctx, t)
		if rt.Status.ActivePlanDigest != active {
			t.Fatalf("a failed validation promoted %s", rt.Status.ActivePlanDigest)
		}
		c := cond(rt, v1alpha1.ConditionStalled)
		return c.Status == metav1.ConditionTrue && c.ObservedGeneration == rt.Generation
	})
	restore()
	nothingForbidden(ctx, t)
}
