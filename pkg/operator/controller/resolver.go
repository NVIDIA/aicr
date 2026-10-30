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
	"errors"
	"fmt"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/config"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
	"github.com/NVIDIA/aicr/pkg/operator/resolver"
)

// ResolverFieldManager owns the resolver's status fields.
const ResolverFieldManager = "aicr-operator-resolver"

// Resolved condition reasons beyond the error-specific ones.
const (
	ReasonResolved  = "Resolved"
	ReasonResolving = "Resolving"
)

// pollInterval paces a reconcile that waits on a Job or an engine.
const pollInterval = 5 * time.Second

// Registry is what the resolver needs from the runtime registry.
type Registry interface {
	Resolve(ctx context.Context, r registry.Repository, tag string) (string, error)
	PullBundle(ctx context.Context, r registry.Repository, digest string) (*registry.Bundle, error)
	resolver.ChartDelivery
}

// ResolverReconciler turns the ClusterRuntime into a candidate plan through
// AICR recipe and render Jobs. It writes plans and its own status
// fields, never engine objects. Recipe and render outputs stay owned by their
// Jobs until the plan exists, so a restart resumes at the first stage whose
// output is missing.
type ResolverReconciler struct {
	Client          client.Client
	Reader          client.Reader
	Config          *config.Config
	Registry        Registry
	OperatorVersion string
	// Credentials supplies pushing Jobs' registry credentials; nil is an
	// anonymous registry.
	Credentials registry.Provider
	// Now is the clock for the snapshot refresh period; nil is time.Now.
	Now func() time.Time
	// Events records failed AICR runs on the runtime; nil records none.
	Events events.EventRecorder

	mu sync.Mutex
	// refused is the last plan the checks refused for its inputs alone, so a
	// resync reports it again without pulling bundles and mirroring charts.
	refused *refusal
}

// refusal is a plan check's verdict on an input digest.
type refusal struct {
	inputDigest string
	err         error
}

// refusedFor returns the remembered verdict on inputDigest, if any.
func (r *ResolverReconciler) refusedFor(inputDigest string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refused == nil || r.refused.inputDigest != inputDigest {
		return nil
	}
	return r.refused.err
}

// remember keeps a verdict that depends on the inputs alone: an override
// path neither render knows, or a namespace outside the allowlist. A stall
// on anything outside the inputs, such as credentials or a conflict, is
// checked again on every resync.
func (r *ResolverReconciler) remember(inputDigest string, err error) {
	switch operrors.ReasonOf(err) {
	case resolver.ReasonOverridePathUnknown, resolver.ReasonNamespaceNotAllowed:
	default:
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refused = &refusal{inputDigest: inputDigest, err: err}
}

// resolution is a reconcile's result for the Resolved condition.
type resolution struct {
	status      metav1.ConditionStatus
	reason      string
	message     string
	candidate   string
	inputDigest string
	discovery   *v1alpha1.DiscoveryStatus
	requeue     time.Duration
}

// Reconcile resolves the runtime named in req.
func (r *ResolverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Uncached: the status decides whether to start runs, and the
	// status apply removes what it omits, so a copy one write behind would
	// restart finished work.
	rt := &v1alpha1.ClusterRuntime{}
	if err := r.Reader.Get(ctx, req.NamespacedName, rt); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read ClusterRuntime "+req.Name)
	}
	if !rt.DeletionTimestamp.IsZero() || rt.Name != v1alpha1.SingletonName {
		return ctrl.Result{}, nil
	}
	res, err := r.resolve(ctx, rt)
	if err != nil {
		if !isTerminal(err) {
			log.FromContext(ctx).Info("resolution will retry", "error", err.Error())
			// A runtime resolved for this generation stays resolved while it
			// re-resolves for changed inputs, such as a snapshot refresh.
			// Otherwise an error that names its cause, such as
			// missing credentials, gives the reason. Either way the
			// resolution retries, and discovery is kept.
			res = waitingFor(rt)
			if res.status == metav1.ConditionTrue {
				res.message = "plan " + rt.Status.CandidatePlanDigest + " resolved; resolving again for changed inputs, which will retry: " + errorMessage(err)
			} else {
				res.reason, res.message = ReasonResolving, errorMessage(err)
				if r := operrors.ReasonOf(err); r != "" {
					res.reason = r
				}
			}
			if applyErr := r.applyStatus(ctx, rt, res); applyErr != nil {
				return ctrl.Result{}, applyErr
			}
			return ctrl.Result{}, err
		}
		// A stall is checked again on the resync: what stalled it may be
		// outside the runtime, such as a missing credentials Secret, whose
		// return no watch reports. Failed runs are not repeated.
		res = &resolution{status: metav1.ConditionFalse, reason: reasonOf(err), message: errorMessage(err), candidate: rt.Status.CandidatePlanDigest,
			inputDigest: rt.Status.InputDigest, discovery: rt.Status.Discovery, requeue: resyncInterval}
	}
	if err := r.applyStatus(ctx, rt, res); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: res.requeue}, nil
}

func (r *ResolverReconciler) stages() *stages {
	creds := r.Credentials
	if creds == nil {
		creds = &registry.StaticProvider{}
	}
	return &stages{client: r.Client, reader: r.Reader, config: r.Config, creds: creds, now: r.Now, events: r.Events}
}

func (r *ResolverReconciler) resolve(ctx context.Context, rt *v1alpha1.ClusterRuntime) (*resolution, error) {
	if err := aicrjob.CheckSupportedVersion(r.Config.AICRVersion); err != nil {
		return nil, err
	}
	waiting := waitingFor(rt)
	st := r.stages()

	var found *discovered
	if !r.Config.SkipDiscovery {
		d, wait, err := r.discover(ctx, rt, st)
		if err != nil || d == nil {
			waiting.requeue = wait
			return waiting.retrying(st), err
		}
		found = d
		waiting.discovery = discoveryStatus(d, rt.Status.Discovery)
	}

	in := resolver.NewInputs(rt, r.Config.AICRImage, r.Config.AICRVersion, r.OperatorVersion,
		r.Config.Engine, argocd.PolicyVersion, r.Config.RegistryRepository)
	in.SnapshotDigest = found.digest()
	inputDigest, err := in.Digest()
	if err != nil {
		return nil, err
	}
	if err = r.refusedFor(inputDigest); err != nil {
		return nil, err
	}
	input := resolver.ShortDigest(inputDigest)

	// Already resolved for these inputs, and the plan is still there as this
	// runtime's resolved plan: an earlier runtime's plan of that digest, or
	// one rebuilt from engine objects, is resolved again, which replaces it.
	if rt.Status.InputDigest == inputDigest && rt.Status.CandidatePlanDigest != "" {
		doc, planErr := r.readPlan(ctx, rt.Status.CandidatePlanDigest)
		if planErr == nil && doc.RuntimeUID == string(rt.UID) && doc.Provenance.Source == plan.SourceResolved {
			return r.done(ctx, rt, inputDigest, rt.Status.CandidatePlanDigest, waiting.discovery)
		}
		if planErr != nil && !operrors.IsPermanent(planErr) {
			return nil, planErr
		}
	}
	if err = st.prune(ctx, rt, input); err != nil {
		return nil, err
	}

	recipeJob, wait, err := st.drive(ctx, rt, r.recipeRun(rt, input, found))
	if err != nil || recipeJob == nil {
		waiting.requeue = wait
		return waiting.retrying(st), err
	}
	recipeJSON, err := st.output(ctx, recipeJob)
	if operrors.CodeOf(err) == operrors.ErrCodeNotFound {
		// The output is gone: the run's result is lost, so run it again.
		log.FromContext(ctx).Info("recipe run output is gone; running it again", "job", recipeJob.Name)
		return waiting, st.remove(ctx, recipeJob)
	}
	if err != nil {
		return nil, err
	}
	recipe, err := aicrjob.ParseRecipeResult(recipeJSON, r.Config.AICRVersion)
	if err != nil {
		return nil, err
	}
	recordCriteria(waiting.discovery, recipe)

	// With value overrides, a base render without them runs alongside the
	// render, for the override path check.
	bundles := r.Config.Registry().Child("bundles/" + rt.Name)
	stagesToRun := []*renderStage{{stage: aicrjob.StageRender, spec: &rt.Spec}}
	if resolver.ValueOverrides(&rt.Spec) {
		stagesToRun = append(stagesToRun, &renderStage{stage: aicrjob.StageBaseRender, spec: resolver.BaseSpec(&rt.Spec)})
	}
	for _, rs := range stagesToRun {
		job, renderWait, driveErr := st.drive(ctx, rt, r.renderRun(rs, input, recipeJSON, bundles))
		if driveErr != nil {
			return nil, driveErr
		}
		if job == nil {
			waiting.requeue = renderWait
			continue
		}
		rs.digest, err = st.pushed(ctx, r.Registry, rs.stage, job, bundles)
		if operrors.CodeOf(err) == operrors.ErrCodeNotFound {
			log.FromContext(ctx).Info("render run pushed no bundle; running it again", "job", job.Name)
			return waiting, st.remove(ctx, job)
		}
		if err != nil {
			return nil, err
		}
	}
	for _, rs := range stagesToRun {
		if rs.digest == "" {
			return waiting.retrying(st), nil
		}
	}
	baseDigest := ""
	if len(stagesToRun) > 1 {
		baseDigest = stagesToRun[1].digest
	}
	digest, err := r.persist(ctx, rt, recipe, recipeJSON, bundles, stagesToRun[0].digest, baseDigest, inputDigest, found.digest())
	if err != nil {
		r.remember(inputDigest, err)
		return nil, err
	}
	return r.done(ctx, rt, inputDigest, digest, waiting.discovery)
}

// recipeRun is the recipe stage: criteria from the spec and, with discovery,
// the snapshot through a run-scoped copy.
func (r *ResolverReconciler) recipeRun(rt *v1alpha1.ClusterRuntime, input string, found *discovered) *stageRun {
	run := &stageRun{
		stage: aicrjob.StageRecipe, input: input,
		args: func(req *aicrjob.Request) ([]string, error) {
			snapshotURI, derived := "", map[string]string(nil)
			if found != nil {
				snapshotURI, derived = aicrjob.InputURI(req, aicrjob.InputSnapshot), found.criteria
			}
			return resolver.RecipeArgs(&rt.Spec, derived, snapshotURI, aicrjob.OutputURI(req)), nil
		},
	}
	if found != nil {
		run.inputs = func(job *batchv1.Job) ([]client.Object, error) {
			cm, err := aicrjob.InputCopy(job, aicrjob.InputSnapshot, found.snapshot.Data)
			return []client.Object{cm}, err
		}
	}
	return run
}

// recordCriteria reports the criteria a recipe run resolved with.
func recordCriteria(d *v1alpha1.DiscoveryStatus, recipe *aicrjob.RecipeResult) {
	if d == nil || recipe.Criteria == nil {
		return
	}
	c := recipe.Criteria
	d.Criteria = &v1alpha1.ResolvedCriteria{Service: c.Service, Accelerator: c.Accelerator, OS: c.OS, Intent: c.Intent, Platform: c.Platform}
}

// discoveryStatus reports the node groups and the snapshot, keeping the
// resolved criteria until a recipe run replaces them.
func discoveryStatus(d *discovered, previous *v1alpha1.DiscoveryStatus) *v1alpha1.DiscoveryStatus {
	s := &v1alpha1.DiscoveryStatus{
		Snapshot:         &v1alpha1.SnapshotRef{ConfigMap: d.snapshot.Name, Digest: d.snapshot.Digest, ObservedAt: &d.observed},
		AcceleratedNodes: int32(min(len(d.groups.Accelerated), 1<<30)), //nolint:gosec // bounded above
		SystemNodes:      int32(min(len(d.groups.System), 1<<30)),      //nolint:gosec // bounded above
	}
	if previous != nil {
		s.Criteria = previous.Criteria
	}
	return s
}

// waitingFor is the resolution reported while AICR Jobs run. Inputs that
// changed without a new generation, such as a snapshot refresh, leave the
// current candidate the runtime's plan until a new one is resolved:
// Resolved stays True meanwhile.
func waitingFor(rt *v1alpha1.ClusterRuntime) *resolution {
	w := &resolution{status: metav1.ConditionFalse, reason: ReasonResolving, message: "AICR Jobs are running",
		candidate: rt.Status.CandidatePlanDigest, inputDigest: rt.Status.InputDigest, discovery: rt.Status.Discovery,
		requeue: pollInterval}
	if c := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionResolved); c != nil && c.Status == metav1.ConditionTrue &&
		c.ObservedGeneration == rt.Generation && rt.Status.CandidatePlanDigest != "" {

		w.status, w.reason = metav1.ConditionTrue, ReasonResolved
		w.message = "plan " + rt.Status.CandidatePlanDigest + " resolved; resolving again for changed inputs"
	}
	return w
}

// retrying names, while resolution waits, the transient AICR failure a
// stage waits to retry, so a registry or API outage is visible on the
// runtime.
func (w *resolution) retrying(st *stages) *resolution {
	switch {
	case st.retrying != "":
		w.message = "retrying after a transient failure: " + st.retrying
	case st.waiting != "":
		// A pod that cannot start is named, not reported as running.
		w.message += "; " + st.waiting
	}
	return w
}

// renderStage is one bundle render of a resolution and the digest it pushed.
type renderStage struct {
	stage  aicrjob.Stage
	spec   *v1alpha1.ClusterRuntimeSpec
	digest string
}

// renderRun is a render stage over a run-scoped copy of the recipe, pushing
// to the run's tag. AICR v1.1.0 and later also report the pushed digest.
func (r *ResolverReconciler) renderRun(rs *renderStage, input string, recipeJSON []byte, bundles registry.Repository) *stageRun {
	return &stageRun{
		stage: rs.stage, input: input,
		args: func(req *aicrjob.Request) ([]string, error) {
			args, err := resolver.RenderArgs(rs.spec, aicrjob.InputURI(req, aicrjob.InputTo),
				bundles.URI()+":"+runTag(req.RunID), r.Config.RegistryPlainHTTP)
			return append(args, aicrjob.ImageRefsArgs(r.Config.AICRVersion)...), err
		},
		inputs: func(job *batchv1.Job) ([]client.Object, error) {
			cm, copyErr := aicrjob.InputCopy(job, aicrjob.InputTo, recipeJSON)
			return []client.Object{cm}, copyErr
		},
	}
}

// done reports a resolved plan and removes the Jobs that produced it.
func (r *ResolverReconciler) done(ctx context.Context, rt *v1alpha1.ClusterRuntime, inputDigest, planDigest string,
	d *v1alpha1.DiscoveryStatus) (*resolution, error) {

	if err := r.stages().prune(ctx, rt, ""); err != nil {
		return nil, err
	}
	return &resolution{status: metav1.ConditionTrue, reason: ReasonResolved,
		message: "plan " + planDigest + " resolved", candidate: planDigest, inputDigest: inputDigest, discovery: d}, nil
}

// persist builds the plan from the bundle and writes it with its recipe and
// values, all content-addressed and immutable.
func (r *ResolverReconciler) persist(ctx context.Context, rt *v1alpha1.ClusterRuntime, recipe *aicrjob.RecipeResult,
	recipeJSON []byte, bundles registry.Repository, bundleDigest, baseDigest, inputDigest, snapshotDigest string) (string, error) {

	bundle, err := r.Registry.PullBundle(ctx, bundles, bundleDigest)
	if err != nil {
		return "", err
	}
	var baseValues map[string]map[string]any
	if baseDigest != "" {
		base, baseErr := r.Registry.PullBundle(ctx, bundles, baseDigest)
		if baseErr != nil {
			return "", baseErr
		}
		if baseValues, err = resolver.BundleValues(base, r.Config.AICRVersion); err != nil {
			return "", err
		}
	}
	flags, err := resolver.RenderFlags(&rt.Spec)
	if err != nil {
		return "", err
	}
	recipeContent := plan.RecipeContent(recipeJSON)
	criteria := map[string]string{}
	if c := recipe.Criteria; c != nil {
		for k, v := range map[string]string{"service": c.Service, "accelerator": c.Accelerator, "os": c.OS, "intent": c.Intent, "platform": c.Platform} {
			if v != "" {
				criteria[k] = v
			}
		}
	}
	built, err := resolver.Build(ctx, r.Registry, &resolver.BuildInput{
		Bundle: bundle, BundleURI: bundles.URI() + "@" + bundleDigest, DeclaredVersion: r.Config.AICRVersion,
		RuntimeUID: string(rt.UID), Engine: r.Config.Engine, EnginePolicy: argocd.PolicyVersion,
		Charts: r.Config.Registry().Child("charts"), Wrappers: r.Config.Registry().Child("wrappers"), InputDigest: inputDigest,
		Provenance: plan.Provenance{
			OperatorVersion: r.OperatorVersion, AICRVersion: r.Config.AICRVersion, AICRImage: r.Config.AICRImage,
			SnapshotDigest: snapshotDigest, RuntimeGeneration: rt.Generation, RenderFlags: flags,
		},
		Recipe: &plan.Recipe{Name: recipeName(recipe), Criteria: criteria, ConfigMapRef: recipeContent.Ref()},
	})
	if err != nil {
		return "", err
	}
	if baseDigest != "" {
		if err = resolver.CheckOverridePaths(rt.Spec.Overrides, baseValues, built.ChartDefaults); err != nil {
			return "", err
		}
	}
	if r.Config.RestrictNamespaces {
		if err = resolver.CheckNamespaces(built.Plan, r.Config.ComponentNamespaces); err != nil {
			return "", err
		}
	}
	contents := append([]*plan.Content{recipeContent}, built.Values...)
	if built.ValidationRecipe != nil {
		contents = append(contents, built.ValidationRecipe)
	}
	for _, c := range contents {
		content, cmErr := c.ConfigMap(r.Config.OperatorNamespace, rt.Name, string(rt.UID), nil)
		if cmErr != nil {
			return "", cmErr
		}
		if err = r.createOnce(ctx, content, v1alpha1.AnnotationDigest, c.Digest); err != nil {
			return "", err
		}
	}
	cm, err := plan.ToConfigMap(built.Plan, r.Config.OperatorNamespace, rt.Name)
	if err != nil {
		return "", err
	}
	if err = createPlanOnce(ctx, r.Client, r.Reader, cm); err != nil {
		return "", err
	}
	return built.Plan.DesiredStateDigest, nil
}

// createOnce creates an immutable, content-addressed ConfigMap. AlreadyExists
// is success when the existing object records the same digest; anything
// else is a conflict, never an overwrite.
func (r *ResolverReconciler) createOnce(ctx context.Context, cm *corev1.ConfigMap, annotation, digest string) error {
	return createOnce(ctx, r.Client, r.Reader, cm, annotation, digest)
}

func createOnce(ctx context.Context, c client.Client, reader client.Reader, cm *corev1.ConfigMap, annotation, digest string) error {
	_, err := createOrMatch(ctx, c, reader, cm, annotation, digest)
	return err
}

// createOrMatch returns the stored ConfigMap: cm once created, or the
// existing one when it records the same digest.
func createOrMatch(ctx context.Context, c client.Client, reader client.Reader, cm *corev1.ConfigMap, annotation, digest string) (*corev1.ConfigMap, error) {
	err := c.Create(ctx, cm)
	if err == nil {
		return cm, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "create ConfigMap "+cm.Name)
	}
	existing := &corev1.ConfigMap{}
	if err = reader.Get(ctx, client.ObjectKeyFromObject(cm), existing); err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read ConfigMap "+cm.Name)
	}
	if existing.Annotations[annotation] != digest {
		return nil, operrors.New(operrors.ErrCodeConflict,
			fmt.Sprintf("ConfigMap %s exists with digest %q, not %s", cm.Name, existing.Annotations[annotation], digest))
	}
	return existing, nil
}

// createPlanOnce is createOnce for a plan ConfigMap, which belongs to the
// runtime UID it records. A plan of the same digest that records
// another UID, left by an earlier runtime, is replaced rather than reused,
// and so is a rebuilt plan when resolution reproduces its digest: the
// resolved plan brings back the recipe a rebuild cannot know. The
// delete is bound to the object read, so a concurrent change fails it and
// the next reconcile decides again.
func createPlanOnce(ctx context.Context, c client.Client, reader client.Reader, cm *corev1.ConfigMap) error {
	stored, err := createOrMatch(ctx, c, reader, cm, v1alpha1.AnnotationPlanDigest, cm.Annotations[v1alpha1.AnnotationPlanDigest])
	if err != nil {
		return err
	}
	owner := stored.Annotations[v1alpha1.AnnotationRuntimeUID]
	replaced := "replaced the plan of an earlier runtime"
	switch {
	case owner != cm.Annotations[v1alpha1.AnnotationRuntimeUID]:
	case stored != cm && provenance(stored) == plan.SourceRebuilt && provenance(cm) == plan.SourceResolved:
		replaced = "replaced a rebuilt plan with the resolved plan of its digest"
	default:
		return nil
	}
	precondition := client.Preconditions{UID: &stored.UID, ResourceVersion: &stored.ResourceVersion}
	if err = c.Delete(ctx, stored, precondition); client.IgnoreNotFound(err) != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "delete plan "+cm.Name+" of runtime "+owner)
	}
	if err = c.Create(ctx, cm); err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "create ConfigMap "+cm.Name)
	}
	log.FromContext(ctx).Info(replaced, "plan", cm.Name, "runtimeUID", owner)
	return nil
}

// provenance is the source a plan ConfigMap records; an unreadable plan has
// none and is never replaced for it.
func provenance(cm *corev1.ConfigMap) plan.Source {
	doc, err := plan.FromConfigMap(cm)
	if err != nil {
		return ""
	}
	return doc.Provenance.Source
}

// readPlan reads a plan ConfigMap through the uncached reader.
func (r *ResolverReconciler) readPlan(ctx context.Context, digest string) (*plan.Document, error) {
	return readPlan(ctx, r.Reader, r.Config.OperatorNamespace, digest)
}

func readPlan(ctx context.Context, reader client.Reader, namespace, digest string) (*plan.Document, error) {
	name, err := plan.Name(digest)
	if err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{}
	if err = reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, operrors.Wrap(operrors.ErrCodeNotFound, "plan "+name+" is missing", err)
		}
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read plan "+name)
	}
	return plan.FromConfigMap(cm)
}

// applyStatus applies only the resolver's status fields with its own field
// manager: the Resolved condition, the candidate digest, its input digest
// and the AICR release in use (api.md).
func (r *ResolverReconciler) applyStatus(ctx context.Context, rt *v1alpha1.ClusterRuntime, res *resolution) error {
	cond := condition(rt, v1alpha1.ConditionResolved, res.status, res.reason, res.message)
	status := map[string]any{
		"conditions": []any{cond},
		"aicr":       map[string]any{"version": r.Config.AICRVersion, "image": r.Config.AICRImage},
	}
	if res.candidate != "" {
		status["candidatePlanDigest"] = res.candidate
	}
	if res.inputDigest != "" {
		status["inputDigest"] = res.inputDigest
	}
	if res.discovery != nil {
		status["discovery"] = res.discovery
	}
	return applyRuntimeStatus(ctx, r.Client, rt, status, ResolverFieldManager)
}

// runTag is the registry tag a render run pushes to.
func runTag(runID string) string { return "run-" + runID }

// recipeName names a recipe by its most specific applied overlay.
func recipeName(r *aicrjob.RecipeResult) string {
	if n := len(r.Metadata.AppliedOverlays); n > 0 {
		return r.Metadata.AppliedOverlays[n-1]
	}
	return "unnamed"
}

// terminalError marks an error that stalls resolution until an input
// changes: a stage that exhausted its runs, or AICR's permanent verdict.
type terminalError struct{ err error }

func (e *terminalError) Error() string { return e.err.Error() }
func (e *terminalError) Unwrap() error { return e.err }

func terminal(err error) error { return &terminalError{err: err} }

// isTerminal reports whether an error stalls resolution rather than being
// retried with backoff: a terminal stage, or a permanent operator error
// (invalid input, an artifact contract violation, a digest conflict).
func isTerminal(err error) bool {
	var te *terminalError
	return errors.As(err, &te) || operrors.IsPermanent(err)
}
