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

package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// WhoAmI returns the requester's username as the API server authenticates
// it, as `kubectl auth whoami` does: approvers record it, and admission
// checks it.
func WhoAmI(ctx context.Context, c client.Client) (string, error) {
	review := &authenticationv1.SelfSubjectReview{}
	if err := c.Create(ctx, review); err != nil {
		return "", operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "review the requester's identity")
	}
	if review.Status.UserInfo.Username == "" {
		return "", operrors.New(operrors.ErrCodeUnauthorized, "the API server reported no username")
	}
	return review.Status.UserInfo.Username, nil
}

// Approve sets the rollout's set-once approval as user, valid until expires.
// It refuses a rollout that is approved already, needs no approval or has
// ended, and writes with an optimistic lock, so it approves exactly the
// rollout it read.
func Approve(ctx context.Context, c client.Client, name, user, reason string, expires time.Time) (*v1alpha1.RuntimeRollout, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, operrors.New(operrors.ErrCodeInvalidRequest, "an approval needs a reason")
	}
	ro, err := get(ctx, c, name)
	if err != nil {
		return nil, err
	}
	switch {
	case ro.Labels[v1alpha1.LabelOutcome] != "":
		return nil, operrors.New(operrors.ErrCodeConflict, fmt.Sprintf("rollout %s has ended %s", name, ro.Labels[v1alpha1.LabelOutcome]))
	case ro.Spec.Approval != nil:
		return nil, operrors.New(operrors.ErrCodeConflict, fmt.Sprintf("rollout %s is already approved by %s", name, ro.Spec.Approval.ApprovedBy))
	case !ro.Spec.ApprovalRequired:
		return nil, operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf("rollout %s needs no approval", name))
	}
	patch := client.MergeFromWithOptions(ro.DeepCopy(), client.MergeFromWithOptimisticLock{})
	ro.Spec.Approval = &v1alpha1.Approval{ApprovedBy: user, ExpiresAt: metav1.NewTime(expires.UTC().Truncate(time.Second)), Reason: reason}
	if err = c.Patch(ctx, ro, patch); err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "approve "+name)
	}
	return ro, nil
}

// Revoke adds the set-once revocation to an approved rollout as user. The
// step in flight finishes and no further step starts.
func Revoke(ctx context.Context, c client.Client, name, user, reason string, now time.Time) (*v1alpha1.RuntimeRollout, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, operrors.New(operrors.ErrCodeInvalidRequest, "a revocation needs a reason")
	}
	ro, err := get(ctx, c, name)
	if err != nil {
		return nil, err
	}
	switch {
	case ro.Labels[v1alpha1.LabelOutcome] != "":
		return nil, operrors.New(operrors.ErrCodeConflict, fmt.Sprintf("rollout %s has ended %s", name, ro.Labels[v1alpha1.LabelOutcome]))
	case ro.Spec.Approval == nil:
		return nil, operrors.New(operrors.ErrCodeConflict, fmt.Sprintf("rollout %s is not approved; nothing to revoke", name))
	case ro.Spec.Approval.RevokedBy != "":
		return nil, operrors.New(operrors.ErrCodeConflict, fmt.Sprintf("rollout %s is already revoked by %s", name, ro.Spec.Approval.RevokedBy))
	}
	patch := client.MergeFromWithOptions(ro.DeepCopy(), client.MergeFromWithOptimisticLock{})
	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	ro.Spec.Approval.RevokedBy, ro.Spec.Approval.RevokedAt, ro.Spec.Approval.RevokeReason = user, &at, reason
	if err = c.Patch(ctx, ro, patch); err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "revoke "+name)
	}
	return ro, nil
}

// Acknowledge records, as user, that the post-apply steps of the release
// the rollout is paused on are done, and returns that step. It refuses
// a rollout that is not waiting for an acknowledgement, and writes with an
// optimistic lock.
func Acknowledge(ctx context.Context, c client.Client, name, user string, now time.Time) (*v1alpha1.RuntimeRollout, v1alpha1.RolloutStep, error) {
	ro, err := get(ctx, c, name)
	if err != nil {
		return nil, v1alpha1.RolloutStep{}, err
	}
	step, ok := awaiting(ro)
	if !ok {
		return nil, v1alpha1.RolloutStep{}, operrors.New(operrors.ErrCodeConflict,
			fmt.Sprintf("rollout %s is not waiting for an acknowledgement (phase %s)", name, or(string(ro.Status.Phase), "Pending")))
	}
	patch := client.MergeFromWithOptions(ro.DeepCopy(), client.MergeFromWithOptimisticLock{})
	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	ro.Spec.Approval.Acknowledgements = append(ro.Spec.Approval.Acknowledgements,
		v1alpha1.Acknowledgement{Release: step.Release, AcknowledgedBy: user, AcknowledgedAt: &at})
	if err = c.Patch(ctx, ro, patch); err != nil {
		return nil, v1alpha1.RolloutStep{}, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "acknowledge "+name)
	}
	return ro, step, nil
}

// awaiting is the ready step a paused rollout waits on: the first whose
// post-apply steps are not acknowledged.
func awaiting(ro *v1alpha1.RuntimeRollout) (v1alpha1.RolloutStep, bool) {
	if ro.Status.Phase != v1alpha1.PhaseAwaitingAcknowledgement || ro.Spec.Approval == nil {
		return v1alpha1.RolloutStep{}, false
	}
	for _, s := range ro.Spec.Steps {
		done := slices.ContainsFunc(ro.Spec.Approval.Acknowledgements, func(a v1alpha1.Acknowledgement) bool { return a.Release == s.Release })
		ready := slices.ContainsFunc(ro.Status.Releases, func(r v1alpha1.ReleaseStatus) bool {
			return r.ID == s.Release && r.State == v1alpha1.ReleaseReady
		})
		if len(s.PostApplySteps) > 0 && ready && !done {
			return s, true
		}
	}
	return v1alpha1.RolloutStep{}, false
}

// Describe writes what an approver decides on: the transition, its steps// Describe writes what an approver decides on: the transition, its steps
// with their verdicts and CRD changes, the assessment report with the
// recorded steps an approval takes on, the approval and why the rollout is
// where it is, including inventory drift.
func Describe(ctx context.Context, c client.Reader, operatorNamespace, name string, w io.Writer) error {
	ro, err := get(ctx, c, name)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	line := func(k, format string, args ...any) { fmt.Fprintf(tw, "%s:\t%s\n", k, fmt.Sprintf(format, args...)) }
	line("Rollout", "%s", ro.Name)
	line("Runtime", "%s (uid %s)", ro.Labels[v1alpha1.LabelRuntime], ro.Spec.RuntimeUID)
	line("Transition", "%s -> %s", or(ro.Spec.FromPlanDigest, "none (initial install)"), ro.Spec.ToPlanDigest)
	line("Phase", "%s", or(string(ro.Status.Phase), "Pending"))
	if outcome := ro.Labels[v1alpha1.LabelOutcome]; outcome != "" {
		line("Outcome", "%s", outcome)
	}
	line("Deadline", "%s", ro.Spec.Deadline.UTC().Format(time.RFC3339))
	line("Approval", "%s", approvalLine(ro))
	if a := ro.Spec.Approval; a != nil {
		for _, k := range a.Acknowledgements {
			line("Acknowledged", "%s by %s", k.Release, k.AcknowledgedBy)
		}
	}
	if ro.Status.Message != "" {
		line("Message", "%s", ro.Status.Message)
	}
	line("Inventory", "%d objects at start (%s)", len(ro.Spec.StartingInventory), ro.Spec.StartingInventoryDigest)
	if err = tw.Flush(); err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "write description", err)
	}

	fmt.Fprintln(w, "\nSteps:")
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  RELEASE\tOPERATION\tASSESSMENT\tCRD CHANGE\tSTATE")
	for _, s := range ro.Spec.Steps {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%t\t%s\n", s.Release, s.Operation, s.Assessment, s.CRDChange, stepState(ro, s.Release))
	}
	if err = tw.Flush(); err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "write steps", err)
	}

	if ref := ro.Spec.AssessmentRef; ref != nil {
		fmt.Fprintf(w, "\nAssessment report %s (%s):\n", ref.ConfigMap, ref.Digest)
		return describeReport(ctx, c, operatorNamespace, ref, w)
	}
	fmt.Fprintln(w, "\nAssessment: none (the transition keeps the recipe, or the plan has none)")
	return nil
}

func describeReport(ctx context.Context, c client.Reader, namespace string, ref *v1alpha1.DigestedConfigMapRef, w io.Writer) error {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.ConfigMap}, cm); err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read assessment report "+ref.ConfigMap)
	}
	data, err := plan.ReadContent(cm, plan.AssessmentDataKey)
	if err != nil {
		return err
	}
	if plan.DigestOf(data) != ref.Digest {
		return operrors.New(operrors.ErrCodeConflict, "assessment report "+ref.ConfigMap+" does not match the digest the rollout records")
	}
	report := &aicrjob.UpgradeReport{}
	if err = json.Unmarshal(data, report); err != nil {
		return operrors.Wrap(operrors.ErrCodeInvalidRequest, "parse assessment report "+ref.ConfigMap, err)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  COMPONENT\tCHANGE\tFROM\tTO\tVERDICT\tNOTES")
	for _, ch := range report.Components {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", ch.Component, ch.Change, or(ch.From, "-"), or(ch.To, "-"), or(ch.Verdict, "-"),
			strings.TrimSpace(strings.Join([]string{ch.Notes, ch.Reason}, " ")))
	}
	if err = tw.Flush(); err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "write assessment report", err)
	}
	describeRecordedSteps(report, w)
	return nil
}

// describeRecordedSteps lists each transition record's precondition and
// steps. The operator performs only the apply; approving the rollout takes
// on the precondition and every other step.
func describeRecordedSteps(report *aicrjob.UpgradeReport, w io.Writer) {
	header := false
	for _, ch := range report.Components {
		if ch.Precondition == "" && len(ch.Steps) == 0 {
			continue
		}
		if !header {
			fmt.Fprintln(w, "\nRecorded steps (the operator performs the apply; approving acknowledges the steps before it, and once the")
			fmt.Fprintln(w, "release is applied the rollout waits for `kubectl aicr acknowledge` of the steps after it):")
			header = true
		}
		fmt.Fprintf(w, "  %s %s -> %s (%s)\n", ch.Component, or(ch.From, "-"), or(ch.To, "-"), or(ch.Verdict, "-"))
		if ch.Precondition != "" {
			fmt.Fprintf(w, "    before: %s\n", ch.Precondition)
		}
		for _, s := range ch.Steps {
			fmt.Fprintf(w, "    %s: %s\n", s.ID, s.Description)
		}
	}
}

func approvalLine(ro *v1alpha1.RuntimeRollout) string {
	a := ro.Spec.Approval
	switch {
	case !ro.Spec.ApprovalRequired:
		return "not required"
	case a == nil:
		return "required, pending (" + or(string(ro.Status.Approval), "Pending") + ")"
	case a.RevokedBy != "":
		at := ""
		if a.RevokedAt != nil {
			at = " at " + a.RevokedAt.UTC().Format(time.RFC3339)
		}
		return fmt.Sprintf("approved by %s (%s), revoked by %s%s: %s", a.ApprovedBy, a.Reason, a.RevokedBy, at, a.RevokeReason)
	default:
		return fmt.Sprintf("approved by %s until %s: %s", a.ApprovedBy, a.ExpiresAt.UTC().Format(time.RFC3339), a.Reason)
	}
}

func stepState(ro *v1alpha1.RuntimeRollout, release string) string {
	if cur := ro.Status.Current; cur != nil && cur.Release == release {
		return "in flight"
	}
	for _, s := range ro.Status.Releases {
		if s.ID == release {
			return string(s.State)
		}
	}
	return string(v1alpha1.ReleasePending)
}

func get(ctx context.Context, c client.Reader, name string) (*v1alpha1.RuntimeRollout, error) {
	ro := &v1alpha1.RuntimeRollout{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, ro); err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeNotFound, "read rollout "+name)
	}
	return ro, nil
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
