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
	"slices"
	"sort"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/evidence"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
	"github.com/NVIDIA/aicr/pkg/operator/resolver"
)

// Deployment validation reasons.
const (
	// ReasonValidating: the rollout's steps are ready and AICR's deployment
	// validation runs.
	ReasonValidating = "Validating"
	// ReasonValidationDisabled: the runtime is rolled out and ready, with
	// deployment validation disabled.
	ReasonValidationDisabled = "ValidationDisabled"
)

// Validation results recorded in status.validation.result.
const (
	resultPending = "Pending"
	resultPassed  = "Passed"
	resultFailed  = "Failed"
)

// Approval states the evidence records.
const (
	approvalNotRequired = "NotRequired"
	approvalApproved    = "Approved"
	approvalBypassed    = "Bypassed"
)

// validationRuns counts finished validation runs by result.
var validationRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "aicr_operator_validation_runs_total",
	Help: "Finished deployment validation runs, by result.",
}, []string{labelResult})

// EvidenceRegistry is what deployment validation needs from the runtime
// registry: AICR's evidence by run tag with the signatures attached to it,
// and the operator's binding pushed next to it.
type EvidenceRegistry interface {
	Resolve(ctx context.Context, r registry.Repository, tag string) (string, error)
	PullBundle(ctx context.Context, r registry.Repository, digest string) (*registry.Bundle, error)
	SigstoreBundles(ctx context.Context, r registry.Repository, subject string) ([][]byte, error)
	Describe(ctx context.Context, r registry.Repository, digest string) (ocispec.Descriptor, error)
	PushStatement(ctx context.Context, r registry.Repository, tag, artifactType string, statement []byte, subject ocispec.Descriptor) (string, error)
}

// validationRequired reports whether a rollout ends in deployment
// validation: removals and adoptions change no plan release, so they do not.
func validationRequired(rt *v1alpha1.ClusterRuntime, ro *v1alpha1.RuntimeRollout) bool {
	return rt.Spec.Validation.Deployment != v1alpha1.DeploymentValidationDisabled && !isRemoval(ro) && !isAdopt(ro)
}

// validate runs AICR's deployment validation once every step is ready, and
// ends the rollout: Succeeded with the evidence reference when it passes,
// Failed otherwise. The run is bound to this rollout, its target
// plan, the snapshot, the AICR image and the validator pins, so a run for
// anything else never promotes it.
func (r *RolloutReconciler) validate(ctx context.Context, rt *v1alpha1.ClusterRuntime, ro *v1alpha1.RuntimeRollout, p *plan.Document,
	v *runtimeView) error {

	ro.Status.Phase = v1alpha1.PhaseValidating
	v.reconcilingReason = ReasonValidating
	unavailable := func(message string) error {
		v.stalled, v.stalledReason, v.stalledMsg = true, ReasonValidationMissing, message
		return r.saveStatus(ctx, ro, message)
	}
	if !r.Config.ValidationEnabled {
		return unavailable("deployment validation is not installed (validation.enabled); enable it or set spec.validation.deployment: Disabled")
	}
	if p.Recipe == nil {
		return unavailable("plan " + p.DesiredStateDigest + " records no recipe to validate against")
	}
	snapshot, err := r.awaitSnapshot(ctx, rt, ro, v)
	if err != nil || snapshot == nil {
		return err
	}
	recipeJSON, err := r.recipe(ctx, p)
	if err != nil {
		if operrors.IsPermanent(err) {
			return unavailable(errorMessage(err))
		}
		return err
	}
	// Only the name AICR gives the recipe in its evidence is read; the
	// recipe passed every artifact check when it was resolved.
	var recipe aicrjob.RecipeResult
	if err = json.Unmarshal(recipeJSON, &recipe); err != nil {
		return operrors.Wrap(operrors.ErrCodeInvalidRequest, "decode the plan's recipe", err)
	}
	input := validationInput(ro, snapshot.Digest, r.Config.AICRImage, r.Config.ValidatorImages)
	st := r.validationStages()
	if err = r.pruneValidation(ctx, st, rt.Name, input); err != nil {
		return err
	}
	repo := r.Config.Registry().Child("evidence/" + rt.Name)
	run := &stageRun{
		stage: aicrjob.StageValidation, input: input, serviceAccount: r.Config.ValidationServiceAccount,
		// A failed run fails the rollout; a retry is a new rollout.
		maxRuns:  1,
		deadline: ro.Spec.Deadline.Sub(r.Now()),
		args: func(req *aicrjob.Request) ([]string, error) {
			return aicrjob.ValidationArgs(req, &aicrjob.Validation{
				AICRVersion: r.Config.AICRVersion, Evidence: repo.URI() + ":" + runTag(req.RunID),
				PlainHTTP: r.Config.RegistryPlainHTTP, ValidatorServiceAccount: r.Config.ValidatorServiceAccount,
				SigningKey: r.Config.ValidationSigningKey, TransparencyLog: r.Config.ValidationTransparencyLog,
			}), nil
		},
		inputs: func(job *batchv1.Job) ([]client.Object, error) {
			recipeCopy, copyErr := aicrjob.InputCopy(job, aicrjob.InputTo, recipeJSON)
			if copyErr != nil {
				return nil, copyErr
			}
			snapshotCopy, copyErr := aicrjob.InputCopy(job, aicrjob.InputSnapshot, snapshot.Data)
			return []client.Object{recipeCopy, snapshotCopy}, copyErr
		},
	}
	job, wait, runErr := st.drive(ctx, rt, run)
	if runErr != nil && !isTerminal(runErr) {
		return runErr
	}
	jobs, err := st.jobs(ctx, rt, run.stage, input)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		if runErr != nil {
			// The run could not be created: nothing ran, nothing was pushed.
			ro.Status.Validation = &v1alpha1.ValidationStatus{Result: resultFailed}
			return r.failValidation(ctx, ro, "", "deployment validation could not run: "+errorMessage(runErr))
		}
		v.requeue = pollInterval
		return r.saveStatus(ctx, ro, "")
	}
	newest := &jobs[0]
	ro.Status.Validation = &v1alpha1.ValidationStatus{Job: newest.Name, RunID: newest.Labels[v1alpha1.LabelRunID], Result: resultPending}
	if job == nil && runErr == nil {
		v.requeue = wait
		return r.saveStatus(ctx, ro, "deployment validation run "+ro.Status.Validation.RunID+" in progress"+detail(st.waiting))
	}
	b := &binding{rt: rt, ro: ro, plan: p, recipeName: recipe.EvidenceName(), repo: repo, job: newest,
		runID: ro.Status.Validation.RunID, snapshot: snapshot.Digest}
	if runErr != nil {
		// A failed run still pushes the evidence of what did not pass.
		ref, _, bindErr := r.bind(ctx, b)
		message := "deployment validation failed: " + errorMessage(runErr)
		switch {
		case bindErr == nil:
		case operrors.CodeOf(bindErr) == operrors.ErrCodeNotFound:
		case isTerminal(bindErr):
			message += "; its evidence was rejected: " + errorMessage(bindErr)
		default:
			return bindErr
		}
		return r.failValidation(ctx, ro, ref, message)
	}
	ref, passed, err := r.bind(ctx, b)
	switch {
	case err != nil && !isTerminal(err):
		return err
	case err != nil:
		return r.failValidation(ctx, ro, "", "deployment validation evidence rejected: "+errorMessage(err))
	case !passed:
		return r.failValidation(ctx, ro, ref, "deployment validation run "+b.runID+" exited 0, but its evidence records failed checks")
	}
	ro.Status.Validation.Result, ro.Status.Validation.Evidence = resultPassed, ref
	validationRuns.WithLabelValues("passed").Inc()
	return r.finishWith(ctx, ro, v1alpha1.PhaseSucceeded, "", ref)
}

// failValidation ends the rollout Failed, recording the evidence a failed
// run pushed, if any: failed runs document what did not pass.
func (r *RolloutReconciler) failValidation(ctx context.Context, ro *v1alpha1.RuntimeRollout, ref, message string) error {
	ro.Status.Validation.Result, ro.Status.Validation.Evidence = resultFailed, ref
	validationRuns.WithLabelValues("failed").Inc()
	return r.finish(ctx, ro, v1alpha1.PhaseFailed, message)
}

// binding is one validation run to bind.
type binding struct {
	rt   *v1alpha1.ClusterRuntime
	ro   *v1alpha1.RuntimeRollout
	plan *plan.Document
	// recipeName is the name AICR gives the plan's recipe in its evidence,
	// which carries the selected profile the plan's overlay name does not.
	recipeName string
	repo       registry.Repository
	job        *batchv1.Job
	runID      string
	snapshot   string
}

// bind reads the evidence the run pushed, checks that it is about the plan's
// recipe, this AICR release and the pinned validators, and signed with the
// configured key, and pushes the operator's binding statement with AICR's
// evidence as its subject. It returns the binding's reference and whether
// every check passed; a run that pushed nothing is NotFound. Binding is
// idempotent: the same run binds to the same digest.
func (r *RolloutReconciler) bind(ctx context.Context, b *binding) (string, bool, error) {
	tag := runTag(b.runID)
	dgst, err := r.validationStages().pushed(ctx, r.Registry, aicrjob.StageValidation, b.job, b.repo)
	if err != nil {
		if operrors.CodeOf(err) == operrors.ErrCodeNotFound {
			return "", false, terminal(operrors.Wrap(operrors.ErrCodeNotFound, "the validation run pushed no evidence to "+b.repo.String()+":"+tag, err))
		}
		return "", false, err
	}
	bundle, err := r.Registry.PullBundle(ctx, b.repo, dgst)
	if err != nil {
		return "", false, err
	}
	data, ok := bundle.Files[evidence.AICRStatementFile]
	if !ok {
		return "", false, operrors.New(operrors.ErrCodeInvalidRequest, "AICR evidence "+dgst+" has no "+evidence.AICRStatementFile)
	}
	summary, err := evidence.ParseAICRStatement(data)
	if err != nil {
		return "", false, err
	}
	if summary.RecipeName != b.recipeName {
		return "", false, operrors.New(operrors.ErrCodeInvalidRequest,
			"AICR evidence is about recipe "+summary.RecipeName+", not the plan's "+b.recipeName)
	}
	if err = aicrjob.CheckArtifactVersion(r.Config.AICRVersion, summary.AICRVersion); err != nil {
		return "", false, err
	}
	if r.Config.SignsEvidence() {
		if summary.KeyFingerprint, err = r.signer(ctx, b.repo, dgst); err != nil {
			return "", false, err
		}
	}
	validators, err := r.verifyValidators(ctx, summary.ValidatorImages)
	if err != nil {
		return "", false, err
	}
	predicate, err := r.predicate(ctx, b, validators)
	if err != nil {
		return "", false, err
	}
	statement, err := evidence.NewBinding(b.repo.String()+"@"+dgst, dgst, summary, predicate)
	if err != nil {
		return "", false, err
	}
	subject, err := r.Registry.Describe(ctx, b.repo, dgst)
	if err != nil {
		return "", false, err
	}
	bound, err := r.Registry.PushStatement(ctx, b.repo, tag+"-binding", evidence.BindingArtifactType, statement, subject)
	if err != nil {
		return "", false, err
	}
	log.FromContext(ctx).Info("bound deployment validation evidence", "rollout", b.ro.Name, "runID", b.runID,
		"evidence", dgst, "binding", bound, "passed", summary.Passed())
	return b.repo.URI() + "@" + bound, summary.Passed(), nil
}

// signer is the fingerprint of the key that signed AICR's evidence. AICR
// attaches the signature to the evidence as a Sigstore bundle referrer, never
// inside the evidence bundle. Evidence with no signature, or signed by more
// than one key, is rejected.
func (r *RolloutReconciler) signer(ctx context.Context, repo registry.Repository, dgst string) (string, error) {
	signatures, err := r.Registry.SigstoreBundles(ctx, repo, dgst)
	if err != nil {
		return "", err
	}
	keys := map[string]bool{}
	for _, s := range signatures {
		fingerprint, fpErr := evidence.KeyFingerprint(s)
		if fpErr != nil {
			return "", fpErr
		}
		keys[fingerprint] = true
	}
	switch len(keys) {
	case 0:
		return "", operrors.New(operrors.ErrCodeInvalidRequest, "AICR evidence "+dgst+" is not signed, but validation.signingKey is set")
	case 1:
		for fingerprint := range keys {
			return fingerprint, nil
		}
	}
	return "", operrors.New(operrors.ErrCodeInvalidRequest, "AICR evidence "+dgst+" is signed by more than one key")
}

// verifyValidators checks the validators AICR names against the pins and
// that each pinned tag still resolves to its pinned digest, so the digests
// the binding records are the ones that ran: AICR v1.0.x records tags only.
func (r *RolloutReconciler) verifyValidators(ctx context.Context, named []evidence.ValidatorImage) ([]evidence.ValidatorImage, error) {
	pins := r.Config.ValidatorPins()
	if err := evidence.CheckValidators(named, pins); err != nil {
		return nil, err
	}
	verified := make([]evidence.ValidatorImage, 0, len(named))
	for _, n := range named {
		pin := pins[evidence.Repository(n.Image)]
		ref, pinned, _ := strings.Cut(pin, "@")
		repo, err := registry.ParseRepository(evidence.Repository(pin))
		if err != nil {
			return nil, err
		}
		got, err := r.Registry.Resolve(ctx, repo, strings.TrimPrefix(ref, evidence.Repository(pin)+":"))
		if err != nil {
			return nil, err
		}
		if got != pinned {
			return nil, operrors.New(operrors.ErrCodeConflict, "validator image "+ref+" resolves to "+got+", not the pinned "+pinned)
		}
		verified = append(verified, evidence.ValidatorImage{Image: ref, Digest: pinned})
	}
	sort.Slice(verified, func(i, j int) bool { return verified[i].Image < verified[j].Image })
	return slices.CompactFunc(verified, func(a, b evidence.ValidatorImage) bool { return a == b }), nil
}

// predicate is what the operator binds AICR's evidence to.
func (r *RolloutReconciler) predicate(ctx context.Context, b *binding, validators []evidence.ValidatorImage) (evidence.Predicate, error) {
	var operator evidence.Image
	if r.Operator != nil {
		var err error
		if operator, err = r.Operator(ctx); err != nil {
			return evidence.Predicate{}, err
		}
	}
	approval := approvalRecord(b.ro)
	charts := make([]evidence.Chart, 0, len(b.plan.Releases))
	for i := range b.plan.Releases {
		rel := &b.plan.Releases[i]
		charts = append(charts, evidence.Chart{Release: rel.ID, Repository: rel.Chart.Repository, Digest: rel.Chart.Digest})
	}
	return evidence.Predicate{
		// Development settings never yield qualifying evidence.
		Qualifying: approval.State != approvalBypassed && !r.Config.BypassApprovals && !r.Config.SkipDiscovery,
		RuntimeUID: string(b.rt.UID), Rollout: b.ro.Name, RunID: b.runID,
		PlanDigest: b.plan.DesiredStateDigest, InputDigest: b.plan.InputDigest, SnapshotDigest: b.snapshot,
		Criteria: b.plan.Recipe.Criteria, Charts: charts,
		Operator:        operator,
		AICR:            evidence.Image{Image: r.Config.AICRImage, Digest: imageDigest(r.Config.AICRImage), Version: r.Config.AICRVersion},
		ValidatorImages: validators,
		Approval:        approval,
	}, nil
}

// approvalRecord is the rollout's approval as the evidence records it: the
// approver and reason with any acknowledgements of post-apply steps,
// NotRequired, or the development bypass.
func approvalRecord(ro *v1alpha1.RuntimeRollout) evidence.Approval {
	switch a := ro.Spec.Approval; {
	case !ro.Spec.ApprovalRequired:
		return evidence.Approval{State: approvalNotRequired}
	case a != nil:
		record := evidence.Approval{State: approvalApproved, ApprovedBy: a.ApprovedBy, Reason: a.Reason, ExpiresAt: a.ExpiresAt.UTC().Format(time.RFC3339)}
		for _, k := range a.Acknowledgements {
			ack := evidence.Acknowledgement{Release: k.Release, AcknowledgedBy: k.AcknowledgedBy}
			if k.AcknowledgedAt != nil {
				ack.AcknowledgedAt = k.AcknowledgedAt.UTC().Format(time.RFC3339)
			}
			record.Acknowledgements = append(record.Acknowledgements, ack)
		}
		return record
	default:
		return evidence.Approval{State: approvalBypassed}
	}
}

// imageDigest is the digest of an image reference pinned by digest.
func imageDigest(ref string) string {
	if _, d, ok := strings.Cut(ref, "@"); ok {
		return d
	}
	return ""
}

// validationInput binds a run to the rollout, its target plan, the snapshot,
// the AICR image and the validator pins; changing any of them needs a new
// run.
func validationInput(ro *v1alpha1.RuntimeRollout, snapshotDigest, aicrImage string, validators []string) string {
	pins := slices.Sorted(slices.Values(validators))
	parts := append([]string{string(ro.UID), ro.Spec.ToPlanDigest, snapshotDigest, aicrImage}, pins...)
	return resolver.ShortDigest(plan.DigestOf([]byte(strings.Join(parts, "\n"))))
}

// validationStages runs validation Jobs with the runtime registry's
// per-run credentials.
func (r *RolloutReconciler) validationStages() *stages {
	creds := r.JobCredentials
	if creds == nil {
		creds = &registry.StaticProvider{}
	}
	return &stages{client: r.Client, reader: r.Reader, config: r.Config, creds: creds, now: r.Now, events: r.Events}
}

// pruneValidation deletes the runtime's validation Jobs except those for
// keepInput; an empty keepInput deletes them all. A run for another rollout
// or input is never read.
func (r *RolloutReconciler) pruneValidation(ctx context.Context, st *stages, runtimeName, keepInput string) error {
	list := &batchv1.JobList{}
	err := r.Reader.List(ctx, list, client.InNamespace(r.Config.ValidationNamespace),
		client.MatchingLabels{v1alpha1.LabelRuntime: runtimeName, v1alpha1.LabelStage: string(aicrjob.StageValidation)})
	if err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list validation Jobs")
	}
	for i := range list.Items {
		j := &list.Items[i]
		if (keepInput != "" && j.Labels[v1alpha1.LabelInput] == keepInput) || !j.DeletionTimestamp.IsZero() {
			continue
		}
		if err = st.remove(ctx, j); err != nil {
			return err
		}
	}
	return nil
}

// detail is "; " and s, or nothing for an empty s.
func detail(s string) string {
	if s == "" {
		return ""
	}
	return "; " + s
}

// awaitSnapshot returns the rollout's deployment snapshot, or nil once it has
// recorded that validation waits for it or that it failed, which fails the
// rollout.
func (r *RolloutReconciler) awaitSnapshot(ctx context.Context, rt *v1alpha1.ClusterRuntime, ro *v1alpha1.RuntimeRollout,
	v *runtimeView) (*plan.Content, error) {

	st := r.validationStages()
	snapshot, wait, err := r.deploymentSnapshot(ctx, st, rt, ro)
	switch {
	case err != nil && isTerminal(err):
		ro.Status.Validation = &v1alpha1.ValidationStatus{Result: resultFailed}
		return nil, r.failValidation(ctx, ro, "", "the deployment snapshot failed: "+errorMessage(err))
	case err != nil:
		return nil, err
	case snapshot == nil:
		v.requeue = wait
		return nil, r.saveStatus(ctx, ro, "taking the deployment snapshot"+detail(st.waiting))
	}
	return snapshot, nil
}

// deploymentSnapshot is the snapshot a rollout validates against, taken for
// it once every step is ready, so validation never judges the cluster as it
// was before the rollout. It is stored owned by the rollout,
// which keeps it while validation reads it; nil while the run is in progress.
func (r *RolloutReconciler) deploymentSnapshot(ctx context.Context, st *stages, rt *v1alpha1.ClusterRuntime,
	ro *v1alpha1.RuntimeRollout) (*plan.Content, time.Duration, error) {

	input := resolver.ShortDigest(plan.DigestOf([]byte(string(ro.UID) + "\n" + ro.Spec.ToPlanDigest + "\n" + r.Config.AICRImage)))
	list := &corev1.ConfigMapList{}
	err := r.Reader.List(ctx, list, client.InNamespace(r.Config.OperatorNamespace), client.MatchingLabels{
		v1alpha1.LabelRole: v1alpha1.RoleSnapshot, v1alpha1.LabelRuntime: rt.Name, v1alpha1.LabelInput: input,
	})
	if err != nil {
		return nil, 0, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list snapshots")
	}
	for i := range list.Items {
		if cm := &list.Items[i]; cm.Annotations[v1alpha1.AnnotationRuntimeUID] == string(rt.UID) {
			if data, readErr := plan.ReadContent(cm, plan.SnapshotDataKey); readErr == nil {
				return plan.RolloutSnapshotContent(data, input), 0, nil
			}
		}
	}
	job, wait, err := st.drive(ctx, rt, snapshotRun(r.Config, rt, input))
	if err != nil || job == nil {
		return nil, wait, err
	}
	data, err := st.output(ctx, job)
	if operrors.CodeOf(err) == operrors.ErrCodeNotFound {
		return nil, pollInterval, st.remove(ctx, job)
	}
	if err != nil {
		return nil, 0, err
	}
	if _, err = aicrjob.ParseSnapshot(data, r.Config.AICRVersion); err != nil {
		return nil, 0, terminal(err)
	}
	content := plan.RolloutSnapshotContent(data, input)
	cm, err := content.ConfigMap(r.Config.OperatorNamespace, rt.Name, string(rt.UID), map[string]string{v1alpha1.LabelInput: input})
	if err != nil {
		return nil, 0, terminal(err)
	}
	cm.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: "RuntimeRollout", Name: ro.Name, UID: ro.UID,
	}}
	if err = createOnce(ctx, r.Client, r.Reader, cm, v1alpha1.AnnotationDigest, content.Digest); err != nil {
		return nil, 0, err
	}
	return content, 0, st.remove(ctx, job)
}
