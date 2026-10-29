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
	"sort"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// rolloutOutcomes counts terminal rollouts by outcome; low cardinality by
// design.
var rolloutOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "aicr_operator_rollout_outcomes_total",
	Help: "Rollouts that reached a terminal phase, by outcome.",
}, []string{"outcome"})

// jobSeconds is the duration of finished AICR Jobs by stage and result. The
// top bucket, 5120s, is above the longest stage deadline.
var jobSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "aicr_operator_job_duration_seconds",
	Help:    "Duration of finished AICR Jobs, by stage and result.",
	Buckets: prometheus.ExponentialBuckets(5, 2, 11),
}, []string{"stage", labelResult})

// labelResult is the metric label for how a run or Job ended.
const labelResult = "result"

// aicrJobRuns counts AICR runs when first seen finished.
var aicrJobRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "aicr_operator_job_runs_total",
	Help: "Finished AICR runs, by stage, result and, for failures, retry class.",
}, []string{"stage", labelResult, "class"})

// metricStages are the stages AICR Jobs record metrics under.
var metricStages = []aicrjob.Stage{
	aicrjob.StageSnapshot, aicrjob.StageRecipe, aicrjob.StageBaseRender,
	aicrjob.StageRender, aicrjob.StageAssessment, aicrjob.StageValidation,
}

// terminalPhases are the rollout outcomes.
var terminalPhases = []v1alpha1.RolloutPhase{
	v1alpha1.PhaseSucceeded, v1alpha1.PhaseFailed, v1alpha1.PhaseExpired,
	v1alpha1.PhaseRevoked, v1alpha1.PhaseSuperseded, v1alpha1.PhaseOrphaned,
}

// Collectors are the controllers' metrics, for the manager's registry, with
// every known label value already at zero, so a rate reads zero rather than
// nothing before the first event.
func Collectors() []prometheus.Collector {
	for _, phase := range terminalPhases {
		rolloutOutcomes.WithLabelValues(string(phase))
	}
	validationRuns.WithLabelValues("passed")
	validationRuns.WithLabelValues("failed")
	for _, stage := range metricStages {
		jobSeconds.WithLabelValues(string(stage), "succeeded")
		jobSeconds.WithLabelValues(string(stage), "failed")
		aicrJobRuns.WithLabelValues(string(stage), "succeeded", "")
		for _, class := range retryClasses {
			aicrJobRuns.WithLabelValues(string(stage), "failed", class)
		}
	}
	return []prometheus.Collector{rolloutOutcomes, jobSeconds, validationRuns, aicrJobRuns}
}

// orphan ends the unfinished rollouts of a runtime name that do not belong
// to the runtime UID keep (all of them when keep is empty): their runtime
// was deleted, so they can never progress. A recreated runtime
// thereby never sees an earlier runtime's rollout as in progress.
func (r *RolloutReconciler) orphan(ctx context.Context, name, keep string) error {
	list := &v1alpha1.RuntimeRolloutList{}
	if err := r.Reader.List(ctx, list, client.MatchingLabels{v1alpha1.LabelRuntime: name}); err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list rollouts")
	}
	for i := range list.Items {
		ro := &list.Items[i]
		if ro.Spec.RuntimeUID == keep || ro.Labels[v1alpha1.LabelOutcome] != "" {
			continue
		}
		if err := r.finish(ctx, ro, v1alpha1.PhaseOrphaned, "runtime "+ro.Spec.RuntimeUID+" no longer exists"); err != nil {
			return err
		}
	}
	return nil
}

// restoreOutcomes rebuilds the status phase of terminal rollouts from their
// outcome label, and a validated rollout's evidence from its annotation,
// after a restore dropped status.
func (r *RolloutReconciler) restoreOutcomes(ctx context.Context, rollouts []v1alpha1.RuntimeRollout) error {
	for i := range rollouts {
		ro := &rollouts[i]
		outcome := v1alpha1.RolloutPhase(ro.Labels[v1alpha1.LabelOutcome])
		ref := ro.Annotations[v1alpha1.AnnotationEvidence]
		lostEvidence := ref != "" && (ro.Status.Validation == nil || ro.Status.Validation.Evidence != ref)
		if outcome == "" || (ro.Status.Phase == outcome && !lostEvidence) {
			continue
		}
		if lostEvidence {
			ro.Status.Validation = &v1alpha1.ValidationStatus{Result: resultPassed, Evidence: ref}
		}
		ro.Status.Phase, ro.Status.Current = outcome, nil
		if err := r.saveStatus(ctx, ro, ro.Status.Message); err != nil {
			return err
		}
	}
	return nil
}

// retryToken is the runtime's retry request, when no rollout has consumed it
// yet: each retry needs a new token (api.md).
func retryToken(rt *v1alpha1.ClusterRuntime, rollouts []v1alpha1.RuntimeRollout) string {
	token := rt.Annotations[v1alpha1.AnnotationRetry]
	if token == "" {
		return ""
	}
	for i := range rollouts {
		if rollouts[i].Annotations[v1alpha1.AnnotationRetry] == token {
			return ""
		}
	}
	return token
}

// restorePlan rebuilds a lost active plan ConfigMap from the live engine
// objects. It writes the plan and its values when the rebuilt
// digest is the active one; otherwise the runtime stalls with PlanMissing
// and the next rollout assesses every changed step as unknown.
func (r *RolloutReconciler) restorePlan(ctx context.Context, rt *v1alpha1.ClusterRuntime, active string) (*plan.Document, error) {
	doc, contents, err := r.Engine.RebuildLive(ctx, string(rt.UID))
	if err != nil {
		if operrors.IsPermanent(err) {
			return nil, operrors.Wrap(operrors.ErrCodeNotFound, "the active plan "+active+" is missing and cannot be rebuilt", err).
				WithReason(ReasonPlanMissing)
		}
		return nil, err
	}
	if doc.DesiredStateDigest != active {
		return nil, operrors.New(operrors.ErrCodeNotFound, "the active plan "+active+" is missing; the live engine objects rebuild to "+
			doc.DesiredStateDigest).WithReason(ReasonPlanMissing)
	}
	for _, c := range contents {
		cm, cmErr := c.ConfigMap(r.Config.OperatorNamespace, rt.Name, string(rt.UID), nil)
		if cmErr != nil {
			return nil, cmErr
		}
		if err = createOnce(ctx, r.Client, r.Reader, cm, v1alpha1.AnnotationDigest, c.Digest); err != nil {
			return nil, err
		}
	}
	cm, err := plan.ToConfigMap(doc, r.Config.OperatorNamespace, rt.Name)
	if err != nil {
		return nil, err
	}
	if err = createPlanOnce(ctx, r.Client, r.Reader, cm); err != nil {
		return nil, err
	}
	log.FromContext(ctx).Info("rebuilt the active plan from the engine objects", "plan", active)
	r.event(rt, corev1.EventTypeNormal, "PlanRebuilt", "the active plan %s was rebuilt from the live engine objects", active)
	return doc, nil
}

// retain deletes what the retention rules no longer keep (api.md): the
// runtime's rollouts beyond the newest succeeded one, the unfinished ones and
// the ten newest terminal ones; plans beyond the active, candidate and pinned
// ones, those retained rollouts reference and the five newest others; and
// recipe, values and assessment content nothing retained references, once
// older than a grace period that covers a resolution in flight. The
// assessment report of a blocked transition, which no rollout references,
// is kept.
func (r *RolloutReconciler) retain(ctx context.Context, rt *v1alpha1.ClusterRuntime, active, target, blockedReport string) error {
	rollouts, err := r.rollouts(ctx, rt)
	if err != nil {
		return err
	}
	kept, drop := retainedRollouts(rollouts)
	for i := range drop {
		if err = r.Client.Delete(ctx, &drop[i]); client.IgnoreNotFound(err) != nil {
			return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "delete rollout "+drop[i].Name)
		}
	}

	cms := &corev1.ConfigMapList{}
	if err = r.Reader.List(ctx, cms, client.InNamespace(r.Config.OperatorNamespace),
		client.MatchingLabels{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue, v1alpha1.LabelRuntime: rt.Name}); err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list ConfigMaps")
	}
	keepPlans := map[string]bool{active: true, target: true, rt.Status.CandidatePlanDigest: true, rt.Spec.Selection.PinnedPlanDigest: true}
	keepContent := map[string]bool{blockedReport: true}
	for i := range kept {
		keepPlans[kept[i].Spec.FromPlanDigest], keepPlans[kept[i].Spec.ToPlanDigest] = true, true
		if ref := kept[i].Spec.AssessmentRef; ref != nil {
			keepContent[ref.ConfigMap] = true
		}
	}
	var plans []corev1.ConfigMap
	for _, cm := range cms.Items {
		if cm.Labels[v1alpha1.LabelRole] == v1alpha1.RolePlan {
			plans = append(plans, cm)
		}
	}
	sort.Slice(plans, func(i, j int) bool { return plans[j].CreationTimestamp.Before(&plans[i].CreationTimestamp) })
	others := 0
	for i := range plans {
		cm := &plans[i]
		digest := cm.Annotations[v1alpha1.AnnotationPlanDigest]
		if !keepPlans[digest] {
			if others >= defaults.RetainedPlans {
				if err = r.Client.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
					return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "delete plan "+cm.Name)
				}
				continue
			}
			others++
		}
		doc, docErr := plan.FromConfigMap(cm)
		if docErr != nil {
			continue
		}
		if doc.Recipe != nil {
			keepContent[doc.Recipe.ConfigMapRef.ConfigMap] = true
			keepContent[doc.Recipe.Validation().ConfigMap] = true
		}
		for _, rel := range doc.Releases {
			keepContent[rel.Values.ConfigMap] = true
		}
	}
	cutoff := r.Now().Add(-defaults.ContentGracePeriod)
	for i := range cms.Items {
		cm := &cms.Items[i]
		switch cm.Labels[v1alpha1.LabelRole] {
		case v1alpha1.RoleRecipe, v1alpha1.RoleValues, v1alpha1.RoleAssessment:
		default:
			continue
		}
		if keepContent[cm.Name] || cm.CreationTimestamp.After(cutoff) {
			continue
		}
		if err = r.Client.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "delete "+cm.Name)
		}
	}
	return nil
}

// retainedRollouts splits a runtime's rollouts, newest first, into those
// retention keeps and those it deletes.
func retainedRollouts(rollouts []v1alpha1.RuntimeRollout) (kept, drop []v1alpha1.RuntimeRollout) {
	succeeded, terminal := false, 0
	for i := range rollouts {
		ro := rollouts[i]
		outcome := v1alpha1.RolloutPhase(ro.Labels[v1alpha1.LabelOutcome])
		switch {
		case outcome == "":
			kept = append(kept, ro)
		case outcome == v1alpha1.PhaseSucceeded && !succeeded:
			succeeded = true
			kept = append(kept, ro)
		case terminal < defaults.RetainedRollouts:
			terminal++
			kept = append(kept, ro)
		default:
			drop = append(drop, ro)
		}
	}
	return kept, drop
}
