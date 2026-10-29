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
	"cmp"
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
	"github.com/NVIDIA/aicr/pkg/operator/resolver"
)

// Reasons for transitions the rollout controller does not start.
const (
	// ReasonAssessing: AICR's upgrade-check runs before the rollout exists.
	ReasonAssessing = "Assessing"
	// ReasonTransitionBlocked: a blocked verdict or an identity change needs
	// a separately qualified migration.
	ReasonTransitionBlocked = "TransitionBlocked"
)

// deployers maps the engine onto AICR's upgrade-check deployer.
var deployers = map[plan.Engine]string{plan.EngineArgoCD: "argocd", plan.EngineFlux: "flux"}

// assess runs, or reuses, AICR's upgrade-check between the active and the
// target plan's recipes. The report is kept in the operator
// namespace, labeled with the transition, and its run removed. It returns a
// nil report while the Job runs.
func (r *RolloutReconciler) assess(ctx context.Context, rt *v1alpha1.ClusterRuntime, from, to *plan.Document) (
	*aicrjob.UpgradeReport, *v1alpha1.DigestedConfigMapRef, time.Duration, error) {

	deployer := deployers[r.Config.Engine]
	// The AICR image is part of the input: verdicts come from its release's
	// transition records, so another AICR assesses the transition again,
	// and one that refused it does not stall the next.
	input := resolver.ShortDigest(plan.DigestOf([]byte(from.Recipe.ConfigMapRef.Digest + "\n" + to.Recipe.ConfigMapRef.Digest + "\n" +
		deployer + "\n" + r.Config.AICRImage)))
	if report, ref, err := r.storedAssessment(ctx, rt, input, deployer); report != nil || err != nil {
		return report, ref, 0, err
	}
	fromRecipe, err := r.recipe(ctx, from)
	if err != nil {
		return nil, nil, 0, err
	}
	toRecipe, err := r.recipe(ctx, to)
	if err != nil {
		return nil, nil, 0, err
	}
	st := &stages{client: r.Client, reader: r.Reader, config: r.Config, creds: &registry.StaticProvider{}, now: r.Now, events: r.Events}
	job, wait, err := st.drive(ctx, rt, &stageRun{
		stage: aicrjob.StageAssessment, input: input,
		args: func(req *aicrjob.Request) ([]string, error) { return aicrjob.AssessmentArgs(req, deployer), nil },
		inputs: func(job *batchv1.Job) ([]client.Object, error) {
			fromCopy, fromErr := aicrjob.InputCopy(job, aicrjob.InputFrom, fromRecipe)
			if fromErr != nil {
				return nil, fromErr
			}
			toCopy, toErr := aicrjob.InputCopy(job, aicrjob.InputTo, toRecipe)
			return []client.Object{fromCopy, toCopy}, toErr
		},
	})
	if err != nil || job == nil {
		return nil, nil, wait, err
	}
	data, err := st.output(ctx, job)
	if operrors.CodeOf(err) == operrors.ErrCodeNotFound {
		log.FromContext(ctx).Info("assessment output is gone; running it again", "job", job.Name)
		return nil, nil, pollInterval, st.remove(ctx, job)
	}
	if err != nil {
		return nil, nil, 0, err
	}
	report, err := aicrjob.ParseUpgradeReport(data, deployer)
	if err != nil {
		return nil, nil, 0, terminal(err)
	}
	content := plan.AssessmentContent(data)
	cm, err := content.ConfigMap(r.Config.OperatorNamespace, rt.Name, string(rt.UID), map[string]string{v1alpha1.LabelInput: input})
	if err != nil {
		return nil, nil, 0, terminal(err)
	}
	if err = createOnce(ctx, r.Client, r.Reader, cm, v1alpha1.AnnotationDigest, content.Digest); err != nil {
		return nil, nil, 0, err
	}
	ref := &v1alpha1.DigestedConfigMapRef{ConfigMap: content.Name, Digest: content.Digest}
	return report, ref, 0, st.remove(ctx, job)
}

// storedAssessment finds the report kept for a transition.
func (r *RolloutReconciler) storedAssessment(ctx context.Context, rt *v1alpha1.ClusterRuntime, input, deployer string) (
	*aicrjob.UpgradeReport, *v1alpha1.DigestedConfigMapRef, error) {

	list := &corev1.ConfigMapList{}
	err := r.Reader.List(ctx, list, client.InNamespace(r.Config.OperatorNamespace), client.MatchingLabels{
		v1alpha1.LabelRole: v1alpha1.RoleAssessment, v1alpha1.LabelRuntime: rt.Name, v1alpha1.LabelInput: input,
	})
	if err != nil {
		return nil, nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list assessments")
	}
	for i := range list.Items {
		cm := &list.Items[i]
		if cm.Annotations[v1alpha1.AnnotationRuntimeUID] != string(rt.UID) {
			continue
		}
		data, readErr := plan.ReadContent(cm, plan.AssessmentDataKey)
		if readErr != nil {
			continue
		}
		report, parseErr := aicrjob.ParseUpgradeReport(data, deployer)
		if parseErr != nil {
			continue
		}
		return report, &v1alpha1.DigestedConfigMapRef{ConfigMap: cm.Name, Digest: plan.DigestOf(data)}, nil
	}
	return nil, nil, nil
}

// recipe reads a plan's recipe content, verified against its digest.
func (r *RolloutReconciler) recipe(ctx context.Context, p *plan.Document) ([]byte, error) {
	return readContent(ctx, r.Reader, r.Config.OperatorNamespace, p.Recipe.ConfigMapRef, plan.RecipeDataKey, "recipe")
}

// baseline is what a transition is judged against: what is deployed, which
// a failed rollout leaves partly on its own target plan.
type baseline struct {
	// doc is the active plan with each release the inventory shows on
	// another plan replaced by that plan's release, or dropped when that
	// plan is gone, so its step is Unknown.
	doc *plan.Document
	// from is the plan upgrade-check runs from: the one plan every changed
	// release is on, or the active plan.
	from *plan.Document
	// unassessed are the components on a plan other than from, which no
	// verdict of that run covers.
	unassessed map[string]bool
}

// liveBaseline judges a transition from the active plan to `to` by the plan
// each live object carries rather than by the active plan alone: moving back
// a release a failed rollout moved is a real change, assessed like any other.
func (r *RolloutReconciler) liveBaseline(ctx context.Context, active, to *plan.Document, inventory []argocd.InventoryItem) (*baseline, error) {
	b := &baseline{doc: active, from: active, unassessed: map[string]bool{}}
	if active == nil {
		return b, nil
	}
	plans := map[string]*plan.Document{active.DesiredStateDigest: active, to.DesiredStateDigest: to}
	carried := map[string]string{}
	for _, it := range inventory {
		if it.PlanDigest == "" || it.PlanDigest == active.DesiredStateDigest {
			continue
		}
		carried[it.ReleaseID] = it.PlanDigest
		if _, ok := plans[it.PlanDigest]; !ok {
			doc, err := readPlan(ctx, r.Reader, r.Config.OperatorNamespace, it.PlanDigest)
			if err != nil && !operrors.IsPermanent(err) {
				return nil, err
			}
			plans[it.PlanDigest] = doc
		}
	}
	if len(carried) == 0 {
		return b, nil
	}
	live := *active
	live.Releases = nil
	seen := map[string]bool{}
	on := func(id string) string { return cmp.Or(carried[id], active.DesiredStateDigest) }
	add := func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		if rel := release(plans[on(id)], id); rel != nil {
			live.Releases = append(live.Releases, *rel)
		}
	}
	for i := range active.Releases {
		add(active.Releases[i].ID)
	}
	for _, it := range inventory {
		add(it.ReleaseID)
	}
	b.doc = &live
	// upgrade-check runs from the plan the changed releases are on when
	// they share one; otherwise from the active plan, and the rest have no
	// verdict.
	sources := map[string]bool{}
	for i := range to.Releases {
		id := to.Releases[i].ID
		if a, t := release(b.doc, id), &to.Releases[i]; a == nil || a.Chart.Digest != t.Chart.Digest || a.Values.Digest != t.Values.Digest {
			sources[on(id)] = true
		}
	}
	if len(sources) == 1 {
		for digest := range sources {
			if doc := plans[digest]; doc != nil {
				b.from = doc
			}
		}
	}
	for id := range seen {
		if on(id) == b.from.DesiredStateDigest {
			continue
		}
		if rel := cmp.Or(release(plans[on(id)], id), release(to, id)); rel != nil {
			b.unassessed[rel.Component] = true
		}
	}
	return b, nil
}

// without is the report with the verdicts of the given components removed:
// their steps are assessed Unknown.
func without(report *aicrjob.UpgradeReport, components map[string]bool) *aicrjob.UpgradeReport {
	if report == nil || len(components) == 0 {
		return report
	}
	out := *report
	out.Components = slices.DeleteFunc(slices.Clone(report.Components), func(c aicrjob.ComponentChange) bool { return components[c.Component] })
	return &out
}

// assessed reports whether a transition is assessed by upgrade-check: both
// plans name a recipe and the recipes differ. Identical recipes change no
// component version, so there is nothing for AICR to assess.
func assessed(from, to *plan.Document) bool {
	return from != nil && from.Recipe != nil && to.Recipe != nil && from.Recipe.ConfigMapRef.Digest != to.Recipe.ConfigMapRef.Digest
}

// afterApply is the IDs of a transition record's steps after its apply, the
// one step the operator performs; none without an apply step.
func afterApply(steps []aicrjob.UpgradeStep) []string {
	i := slices.IndexFunc(steps, func(s aicrjob.UpgradeStep) bool { return s.ID == "apply" })
	if i < 0 || i == len(steps)-1 {
		return nil
	}
	ids := make([]string, 0, len(steps)-i-1)
	for _, s := range steps[i+1:] {
		ids = append(ids, s.ID)
	}
	return ids
}

// assessSteps assesses each step and reports whether the rollout requires
// approval:
//
//   - installs, initial or added, and removals require approval;
//   - an upgrade that changes neither chart nor values only rewrites the
//     plan annotation: NotApplicable;
//   - an upgrade with a new chart takes AICR's verdict for its component
//     (Unknown when the report has none); only Safe without a CRD change is
//     automatic;
//   - an upgrade that changes only values is Unknown: AICR assesses versions,
//     not values;
//   - a Blocked verdict, or a release whose namespace or chart name changes,
//     blocks the transition.
func assessSteps(steps []v1alpha1.RolloutStep, from, to *plan.Document, report *aicrjob.UpgradeReport) (bool, error) {
	verdicts, postApply := map[string]v1alpha1.Assessment{}, map[string][]string{}
	var blocked []string
	if report != nil {
		for i := range report.Components {
			c := &report.Components[i]
			switch {
			case c.Change == "version":
				verdicts[c.Component] = c.Assessment()
				if c.Assessment() == v1alpha1.AssessmentManual {
					postApply[c.Component] = afterApply(c.Steps)
				}
			case c.Assessment() == v1alpha1.AssessmentBlocked:
				// A replacement surfaces as a removal and an install, which
				// carry no verdict of their own.
				blocked = append(blocked, fmt.Sprintf("%s is blocked by AICR's upgrade-check (%s)", c.Component, c.Change))
			}
		}
	}
	approval := false
	for i := range steps {
		s := &steps[i]
		s.Assessment, s.CRDChange = v1alpha1.AssessmentNotApplicable, false
		switch s.Operation {
		case v1alpha1.OperationInstall, v1alpha1.OperationRemove:
			approval = true
		case v1alpha1.OperationUpgrade:
			a, b := release(from, s.Release), release(to, s.Release)
			switch {
			case a == nil:
				s.Assessment = v1alpha1.AssessmentUnknown
			case a.TargetNamespace != b.TargetNamespace || chartName(a) != chartName(b):
				blocked = append(blocked, fmt.Sprintf("%s moves from %s/%s to %s/%s", s.Release,
					a.TargetNamespace, chartName(a), b.TargetNamespace, chartName(b)))
			case a.Chart.Digest == b.Chart.Digest && a.Values.Digest == b.Values.Digest:
			case a.Chart.Digest == b.Chart.Digest:
				s.Assessment = v1alpha1.AssessmentUnknown
			default:
				s.Assessment = v1alpha1.AssessmentUnknown
				if v, ok := verdicts[b.Component]; ok {
					s.Assessment, s.PostApplySteps = v, postApply[b.Component]
				}
				s.CRDChange = b.OwnsCRDs
			}
			if s.Assessment == v1alpha1.AssessmentBlocked {
				blocked = append(blocked, s.Release+" is blocked by AICR's upgrade-check")
			}
			if (s.Assessment != v1alpha1.AssessmentSafe && s.Assessment != v1alpha1.AssessmentNotApplicable) || s.CRDChange {
				approval = true
			}
		case v1alpha1.OperationVerify, v1alpha1.OperationAdopt:
		}
	}
	if len(blocked) > 0 {
		sort.Strings(blocked)
		return approval, operrors.New(operrors.ErrCodeInvalidRequest,
			"the transition needs a separately qualified migration: "+strings.Join(blocked, "; ")).WithReason(ReasonTransitionBlocked)
	}
	return approval, nil
}

// unfinished returns the rollouts to target that ended without succeeding
// since the newest success, newest first from rollouts. A rollout to target
// after them is a retry, which needs a fresh approval.
func unfinished(rollouts []v1alpha1.RuntimeRollout, target string) []*v1alpha1.RuntimeRollout {
	var out []*v1alpha1.RuntimeRollout
	for i := range rollouts {
		ro := &rollouts[i]
		outcome := ro.Labels[v1alpha1.LabelOutcome]
		if outcome == string(v1alpha1.PhaseSucceeded) {
			break
		}
		if outcome != "" && ro.Spec.ToPlanDigest == target {
			out = append(out, ro)
		}
	}
	return out
}

// carryPostApplySteps gives a step the post-apply steps of a manual release
// that an earlier rollout applied and nobody acknowledged: the release is
// already at its target, so the step's own assessment no longer sees the
// change that recorded them.
func carryPostApplySteps(steps []v1alpha1.RolloutStep, earlier []*v1alpha1.RuntimeRollout) {
	acknowledged := map[string]bool{}
	for _, ro := range earlier {
		if a := ro.Spec.Approval; a != nil {
			for _, k := range a.Acknowledgements {
				acknowledged[k.Release] = true
			}
		}
	}
	for i := range steps {
		s := &steps[i]
		if len(s.PostApplySteps) > 0 || acknowledged[s.Release] {
			continue
		}
		for _, ro := range earlier {
			idx := slices.IndexFunc(ro.Spec.Steps, func(e v1alpha1.RolloutStep) bool { return e.Release == s.Release })
			if idx >= 0 && len(ro.Spec.Steps[idx].PostApplySteps) > 0 && releaseState(ro, s.Release) == v1alpha1.ReleaseReady {
				s.Assessment, s.PostApplySteps = ro.Spec.Steps[idx].Assessment, ro.Spec.Steps[idx].PostApplySteps
				break
			}
		}
	}
}

// chartName is the chart a release installs: the last path segment of its
// runtime-registry repository.
func chartName(r *plan.Release) string { return path.Base(r.Chart.Repository) }
