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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/evidence"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

const (
	deploymentValidator  = "ghcr.io/nvidia/aicr-validators/deployment:v1.0.0"
	deploymentDigest     = "sha256:3c0a8553a346e6ba5a273a10f0b91574945b8b23cffc9d83db3107701e467790"
	conformanceValidator = "ghcr.io/nvidia/aicr-validators/conformance:v1.0.0"
	conformanceDigest    = "sha256:e091d51f600255c2166e5c1b42afd556880d498f2a9bb913a32da38fa28e038b"
	evidenceRepo         = "registry.test:5000/aicr/cluster/evidence/cluster"
	operatorDigest       = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
)

// fakeEvidence is the runtime registry and the validator image registry: tags,
// AICR's evidence bundles and the statements the operator pushes.
type fakeEvidence struct {
	mu         sync.Mutex
	tags       map[string]string
	bundles    map[string]*registry.Bundle
	statements map[string][]byte
	resolveErr error
	// aicrVersion is the AICR version evidence reports; empty is 1.0.0.
	aicrVersion string
	// attestation, when set, is AICR's signed statement, which AICR
	// attaches to the evidence as a Sigstore bundle referrer, never inside
	// the evidence bundle.
	attestation []byte
	// signatures are the Sigstore bundle referrers of each evidence digest.
	signatures map[string][][]byte
}

func newFakeEvidence() *fakeEvidence {
	return &fakeEvidence{
		tags: map[string]string{
			deploymentValidator: deploymentDigest, conformanceValidator: conformanceDigest,
		},
		bundles: map[string]*registry.Bundle{}, statements: map[string][]byte{}, signatures: map[string][][]byte{},
	}
}

func (f *fakeEvidence) SigstoreBundles(_ context.Context, _ registry.Repository, subject string) ([][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signatures[subject], nil
}

func (f *fakeEvidence) Resolve(_ context.Context, r registry.Repository, tag string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	if d, ok := f.tags[r.String()+":"+tag]; ok {
		return d, nil
	}
	return "", operrors.New(operrors.ErrCodeNotFound, r.String()+":"+tag+" not found")
}

func (f *fakeEvidence) PullBundle(_ context.Context, _ registry.Repository, d string) (*registry.Bundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.bundles[d]; ok {
		return b, nil
	}
	return nil, operrors.New(operrors.ErrCodeNotFound, d+" not found")
}

func (f *fakeEvidence) Describe(_ context.Context, _ registry.Repository, d string) (ocispec.Descriptor, error) {
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(d), Size: 512}, nil
}

func (f *fakeEvidence) PushStatement(_ context.Context, r registry.Repository, tag, artifactType string, statement []byte,
	subject ocispec.Descriptor) (string, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	if artifactType != evidence.BindingArtifactType || subject.Digest == "" {
		return "", operrors.New(operrors.ErrCodeInvalidRequest, "bad statement push")
	}
	d := digest.FromBytes(statement).String()
	key := r.String() + ":" + tag
	if existing, ok := f.tags[key]; ok && existing != d {
		return "", operrors.New(operrors.ErrCodeConflict, key+" holds other content")
	}
	f.tags[key], f.statements[d] = d, statement
	return d, nil
}

// pushEvidence stores the evidence AICR would push for a run: its
// statement names the recipe and validators, with the checks' results.
func (f *fakeEvidence) pushEvidence(t *testing.T, runID, recipe string, failed int, validators ...string) string {
	t.Helper()
	version := f.aicrVersion
	if version == "" {
		version = "1.0.0"
	}
	images := make([]map[string]string, 0, len(validators))
	for _, v := range validators {
		images = append(images, map[string]string{"image": v, "digest": ""})
	}
	statement, err := json.Marshal(map[string]any{
		"_type":         evidence.StatementType,
		"predicateType": "https://aicr.run/recipe-evidence/v3",
		"predicate": map[string]any{
			"aicrVersion": version, "validatorImages": images,
			"recipe": map[string]string{"name": recipe, "digest": strings.Repeat("a", 64)},
			"phases": map[string]any{"deployment": map[string]int{"passed": 4 - failed, "failed": failed}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := digest.FromString("evidence-" + runID).String()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags[evidenceRepo+":run-"+runID] = d
	f.bundles[d] = &registry.Bundle{Digest: d, Files: map[string][]byte{evidence.AICRStatementFile: statement, "manifest.json": []byte("{}")}}
	if f.attestation != nil {
		f.signatures[d] = append(f.signatures[d], f.attestation)
	}
	return d
}

func (f *fakeEvidence) binding(t *testing.T, ref string) *evidence.Binding {
	t.Helper()
	_, d, _ := strings.Cut(ref, "@")
	f.mu.Lock()
	data, ok := f.statements[d]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("no binding pushed as %s", ref)
	}
	b, err := evidence.ParseBinding(data)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newValidating is a rollout controller in an installation with validation.
func newValidating(t *testing.T, engine *fakeEngine, bypass bool) (*RolloutReconciler, *fakeEvidence) {
	t.Helper()
	r, _ := newRollout(t, engine, bypass)
	f := newFakeEvidence()
	// A release installation: development settings never qualify evidence.
	r.Config.SkipDiscovery = false
	r.Config.ValidationEnabled = true
	r.Config.ValidationNamespace = validateNS
	r.Config.ValidatorImages = []string{deploymentValidator + "@" + deploymentDigest, conformanceValidator + "@" + conformanceDigest}
	r.Registry = f
	r.Operator = func(context.Context) (evidence.Image, error) {
		return evidence.Image{Image: "ko.local/aicr-operator:test", Digest: operatorDigest}, nil
	}
	return r, f
}

// validatedRuntime creates a runtime that requires validation, its plan and
// a discovery snapshot from before any rollout, and returns the plan digest
// and the deployment snapshot its rollout's validation runs against.
func validatedRuntime(t *testing.T, ctx context.Context) (*v1alpha1.ClusterRuntime, string, *plan.Content) {
	t.Helper()
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
		rt.Spec.Validation.Deployment = v1alpha1.DeploymentValidationRequired
	})
	digest := putResolved(t, ctx, rt, map[string]string{"accelerator": "h100"}, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"50m"}`))
	putSnapshot(t, ctx, rt, `{"kind":"Snapshot","n":1}`)
	resolved(t, ctx, digest)
	return rt, digest, plan.SnapshotContent(freshSnapshot(t, testConfig().AICRVersion))
}

// freshSnapshot is what the tests' deployment snapshot runs return, as the
// AICR release aicrVersion writes it.
func freshSnapshot(t *testing.T, aicrVersion string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(snapshotWith(t, "eks", "h100"), &doc); err != nil {
		t.Fatal(err)
	}
	doc["metadata"].(map[string]any)["version"] = strings.TrimPrefix(aicrVersion, "v")
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func putSnapshot(t *testing.T, ctx context.Context, rt *v1alpha1.ClusterRuntime, data string) *plan.Content {
	t.Helper()
	c := plan.SnapshotContent([]byte(data))
	cm, err := c.ConfigMap(operatorNS, rt.Name, string(rt.UID), map[string]string{v1alpha1.LabelInput: "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if err = k8s.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	return c
}

// untilValidating reconciles a bypassed rollout through its steps until its
// validation run exists, and returns the run's Job.
func untilValidating(t *testing.T, ctx context.Context, r *RolloutReconciler) *batchv1.Job {
	t.Helper()
	for range 6 {
		reconcileRollout(t, ctx, r)
		for _, j := range liveJobs(t, ctx, string(aicrjob.StageSnapshot)) {
			succeed(t, ctx, &j, freshSnapshot(t, r.Config.AICRVersion))
		}
		ros := rollouts(t, ctx)
		if len(ros) == 1 && ros[0].Status.Validation != nil && ros[0].Status.Validation.Job != "" {
			job := &batchv1.Job{}
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: validateNS, Name: ros[0].Status.Validation.Job}, job); err != nil {
				t.Fatal(err)
			}
			return job
		}
	}
	t.Fatalf("no validation Job; rollouts = %+v", rollouts(t, ctx))
	return nil
}

// Once every step is ready the rollout takes its own snapshot, and only
// then starts validation, against that snapshot: never the one discovery
// stored before the rollout. The snapshot is owned by the rollout.
func TestValidationTakesASnapshotAfterTheSteps(t *testing.T) {
	ctx := testContext(t)
	_, _, fresh := validatedRuntime(t, ctx)
	r, _ := newValidating(t, newEngine(), true)
	var snaps []batchv1.Job
	for range 4 {
		reconcileRollout(t, ctx, r)
		if snaps = liveJobs(t, ctx, string(aicrjob.StageSnapshot)); len(snaps) > 0 {
			break
		}
	}
	ro := rollout(t, ctx)
	if len(snaps) != 1 || len(liveJobs(t, ctx, string(aicrjob.StageValidation))) != 0 || ro.Status.Phase != v1alpha1.PhaseValidating ||
		releaseState(ro, "cert-manager") != v1alpha1.ReleaseReady || !strings.Contains(ro.Status.Message, "snapshot") {

		t.Fatalf("after the steps: %d snapshot runs, %d validation runs, rollout %+v", len(snaps),
			len(liveJobs(t, ctx, string(aicrjob.StageValidation))), ro.Status)
	}

	job := untilValidating(t, ctx, r)
	copied := &corev1.ConfigMap{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: validateNS, Name: job.Name + "-snapshot"}, copied); err != nil ||
		copied.Data[aicrjob.PayloadKey] != string(fresh.Data) {

		t.Fatalf("validation reads a snapshot other than the rollout's: %v", err)
	}
	stored := &corev1.ConfigMapList{}
	if err := k8s.List(ctx, stored, client.InNamespace(operatorNS), client.MatchingLabels{v1alpha1.LabelRole: v1alpha1.RoleSnapshot}); err != nil {
		t.Fatal(err)
	}
	owned := false
	for _, cm := range stored.Items {
		if cm.Annotations[v1alpha1.AnnotationDigest] == fresh.Digest {
			owned = len(cm.OwnerReferences) == 1 && cm.OwnerReferences[0].UID == rollout(t, ctx).UID
		}
	}
	if !owned {
		t.Fatalf("the deployment snapshot is not stored owned by its rollout: %+v", stored.Items)
	}
}

// withValidationRecipe gives the stored plan of digest a validation recipe
// with data, as a resolution records the bundle's recipe.
func withValidationRecipe(t *testing.T, ctx context.Context, rt *v1alpha1.ClusterRuntime, digest, data string) *plan.Content {
	t.Helper()
	c := plan.RecipeContent([]byte(data))
	content, err := c.ConfigMap(operatorNS, rt.Name, string(rt.UID), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = k8s.Create(ctx, content); err != nil {
		t.Fatal(err)
	}
	doc, err := readPlan(ctx, k8s, operatorNS, digest)
	if err != nil {
		t.Fatal(err)
	}
	ref := c.Ref()
	doc.Recipe.ValidationConfigMapRef = &ref
	name, err := plan.Name(digest)
	if err != nil {
		t.Fatal(err)
	}
	stored := &corev1.ConfigMap{}
	if err = k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: name}, stored); err != nil {
		t.Fatal(err)
	}
	cm, err := plan.ToConfigMap(doc, operatorNS, rt.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err = k8s.Delete(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err = k8s.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	return c
}

// Validation reads the plan's validation recipe, the bundle's, not the
// recipe Job's output: a component the render flags turned off is not
// required. A plan without one validates against its render recipe.
func TestValidationReadsTheValidationRecipe(t *testing.T) {
	for name, bundled := range map[string]bool{"bundle recipe": true, "render recipe only": false} {
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			rt, digest, _ := validatedRuntime(t, ctx)
			doc, err := readPlan(ctx, k8s, operatorNS, digest)
			if err != nil {
				t.Fatal(err)
			}
			want, err := readContent(ctx, k8s, operatorNS, doc.Recipe.ConfigMapRef, plan.RecipeDataKey, "recipe")
			if err != nil {
				t.Fatal(err)
			}
			if bundled {
				want = withValidationRecipe(t, ctx, rt, digest, `{"kind":"RecipeResult","criteria":{"accelerator":"h100"},"componentRefs":[]}`).Data
			}
			r, _ := newValidating(t, newEngine(), true)
			job := untilValidating(t, ctx, r)
			copied := &corev1.ConfigMap{}
			if err = k8s.Get(ctx, client.ObjectKey{Namespace: validateNS, Name: job.Name + "-to"}, copied); err != nil ||
				copied.Data[aicrjob.PayloadKey] != string(want) {

				t.Fatalf("validation recipe = %q, want %q (%v)", copied.Data[aicrjob.PayloadKey], want, err)
			}
		})
	}
}

func rollout(t *testing.T, ctx context.Context) *v1alpha1.RuntimeRollout {
	t.Helper()
	ros := rollouts(t, ctx)
	if len(ros) != 1 {
		t.Fatalf("rollouts = %d", len(ros))
	}
	return &ros[0]
}

// A rollout whose steps are ready runs AICR's deployment validation over
// run-scoped copies of the plan's recipe and the snapshot, bounded by the
// rollout deadline; a passing run promotes the plan with evidence bound to
// it, recorded once as the outcome's annotation, and kept across a restore.
func TestValidationPromotesWithBoundEvidence(t *testing.T) {
	ctx := testContext(t)
	rt, digest, snapshot := validatedRuntime(t, ctx)
	r, f := newValidating(t, newEngine(), false)
	reconcileRollout(t, ctx, r)
	ro := rollout(t, ctx)
	approve(t, ctx, ro.Name, time.Now().Add(time.Hour))
	job := untilValidating(t, ctx, r)

	ro = rollout(t, ctx)
	runID := job.Labels[v1alpha1.LabelRunID]
	args := job.Spec.Template.Spec.Containers[0].Args
	if ro.Status.Phase != v1alpha1.PhaseValidating || ro.Labels[v1alpha1.LabelOutcome] != "" ||
		ro.Status.Validation == nil || ro.Status.Validation.RunID != runID || ro.Status.Validation.Result != resultPending {

		t.Fatalf("rollout while validating = %+v", ro.Status)
	}
	wantArgs := []string{"validate", "--recipe", "cm://aicr-validation/" + job.Name + "-to", "--snapshot", "cm://aicr-validation/" + job.Name + "-snapshot"}
	if !slices.Equal(args[:5], wantArgs) || !slices.Contains(args, "oci://"+evidenceRepo+":run-"+runID) ||
		job.Spec.Template.Spec.ServiceAccountName != "aicr-validation" || *job.Spec.ActiveDeadlineSeconds > int64(time.Until(ro.Spec.Deadline.Time).Seconds())+1 {

		t.Fatalf("validation Job %s: %v, account %s, deadline %d", job.Name, args, job.Spec.Template.Spec.ServiceAccountName, *job.Spec.ActiveDeadlineSeconds)
	}
	copies := &corev1.ConfigMap{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: validateNS, Name: job.Name + "-snapshot"}, copies); err != nil ||
		copies.Data[aicrjob.PayloadKey] != string(snapshot.Data) {

		t.Fatalf("snapshot copy: %v %v", err, copies.Data)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReconciling); c.Reason != ReasonValidating {
		t.Fatalf("Reconciling while validating = %+v", c)
	}

	evidenceDigest := f.pushEvidence(t, runID, "h100", 0, deploymentValidator, conformanceValidator)
	succeed(t, ctx, job, nil)
	reconcileRollout(t, ctx, r)
	ro = rollout(t, ctx)
	ref := ro.Annotations[v1alpha1.AnnotationEvidence]
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || !strings.HasPrefix(ref, "oci://"+evidenceRepo+"@sha256:") ||
		ro.Status.Validation.Result != resultPassed || ro.Status.Validation.Evidence != ref {

		t.Fatalf("validated rollout = %v %v %+v", ro.Labels, ro.Annotations, ro.Status.Validation)
	}
	b := f.binding(t, ref)
	p := b.Predicate
	if b.Subject[0].Digest["sha256"] != strings.TrimPrefix(evidenceDigest, "sha256:") || p.PlanDigest != digest ||
		p.RuntimeUID != string(rt.UID) || p.Rollout != ro.Name || p.RunID != runID || p.SnapshotDigest != snapshot.Digest ||
		p.Operator.Digest != operatorDigest || p.AICR.Digest != imageDigest(aicrImage) || p.AICR.Version != "v1.0.0" ||
		p.Approval.State != approvalApproved || p.Approval.ApprovedBy != "alice" || !p.Qualifying || p.Result != "Passed" ||
		len(p.Charts) != 1 || p.Criteria["accelerator"] != "h100" ||
		!slices.Equal(p.ValidatorImages, []evidence.ValidatorImage{{Image: conformanceValidator, Digest: conformanceDigest}, {Image: deploymentValidator, Digest: deploymentDigest}}) {

		t.Fatalf("binding = %+v", b)
	}
	if jobs := liveJobs(t, ctx, string(aicrjob.StageValidation)); len(jobs) != 0 {
		t.Fatalf("validation Jobs after the outcome: %d", len(jobs))
	}
	reconcileRollout(t, ctx, r)
	if got := getRuntime(t, ctx); got.Status.ActivePlanDigest != digest || cond(got, v1alpha1.ConditionReady).Reason != ReasonRolledOut {
		t.Fatalf("runtime after validation = %+v", got.Status)
	}

	// A restore loses status; the outcome and evidence come back from the
	// rollout's own metadata.
	ro.Status = v1alpha1.RuntimeRolloutStatus{}
	if err := k8s.Status().Update(ctx, ro); err != nil {
		t.Fatal(err)
	}
	reconcileRollout(t, ctx, r)
	ro = rollout(t, ctx)
	if ro.Status.Phase != v1alpha1.PhaseSucceeded || ro.Status.Validation == nil || ro.Status.Validation.Evidence != ref ||
		ro.Annotations[v1alpha1.AnnotationEvidence] != ref {

		t.Fatalf("restored rollout = %+v", ro.Status)
	}
}

// A validated rollout counts its outcome and its validation run once each,
// when the outcome is recorded: an outcome write that fails is not counted,
// and one whose status write then conflicts still is.
func TestValidationOutcomeIsCountedOnce(t *testing.T) {
	ctx := testContext(t)
	validatedRuntime(t, ctx)
	r, f := newValidating(t, newEngine(), true)
	job := untilValidating(t, ctx, r)
	f.pushEvidence(t, job.Labels[v1alpha1.LabelRunID], "h100", 0, deploymentValidator)
	succeed(t, ctx, job, nil)
	passed := testutil.ToFloat64(validationRuns.WithLabelValues("passed"))
	succeeded := testutil.ToFloat64(rolloutOutcomes.WithLabelValues(string(v1alpha1.PhaseSucceeded)))

	r.Client = faultOnce(t, withOutcome)
	for range 2 { // the outcome patch fails; then it lands and the status update conflicts
		if _, err := r.Reconcile(ctx, request); err == nil {
			t.Fatal("an injected failure was not returned")
		}
	}
	for range 2 {
		reconcileRollout(t, ctx, r)
	}
	if ro := rollout(t, ctx); ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || ro.Status.Phase != v1alpha1.PhaseSucceeded {
		t.Fatalf("rollout = %v %+v", ro.Labels, ro.Status)
	}
	if got := testutil.ToFloat64(validationRuns.WithLabelValues("passed")) - passed; got != 1 {
		t.Errorf("validation runs counted %v times, want once", got)
	}
	if got := testutil.ToFloat64(rolloutOutcomes.WithLabelValues(string(v1alpha1.PhaseSucceeded))) - succeeded; got != 1 {
		t.Errorf("the outcome was counted %v times, want once", got)
	}
}

// Evidence from the development approval bypass, or from any development
// installation, never qualifies.
func TestValidationUnderBypassNeverQualifies(t *testing.T) {
	ctx := testContext(t)
	validatedRuntime(t, ctx)
	r, f := newValidating(t, newEngine(), true)
	job := untilValidating(t, ctx, r)
	f.pushEvidence(t, job.Labels[v1alpha1.LabelRunID], "h100", 0, deploymentValidator)
	succeed(t, ctx, job, nil)
	reconcileRollout(t, ctx, r)
	ro := rollout(t, ctx)
	b := f.binding(t, ro.Annotations[v1alpha1.AnnotationEvidence])
	if ro.Status.Approval != v1alpha1.ApprovalBypassed || b.Predicate.Approval.State != approvalBypassed || b.Predicate.Qualifying {
		t.Fatalf("bypassed binding = %+v", b.Predicate)
	}
	for name, mutate := range map[string]func(*RolloutReconciler){
		"bypass installation":  func(r *RolloutReconciler) { r.Config.BypassApprovals = true },
		"discovery skipped":    func(r *RolloutReconciler) { r.Config.SkipDiscovery = true },
		"release installation": func(*RolloutReconciler) {},
	} {
		r, _ := newValidating(t, newEngine(), false)
		mutate(r)
		ro := &v1alpha1.RuntimeRollout{Spec: v1alpha1.RuntimeRolloutSpec{ToPlanDigest: "sha256:x"}}
		p, err := r.predicate(ctx, &binding{rt: getRuntime(t, ctx), ro: ro, plan: &plan.Document{Recipe: &plan.Recipe{}}}, nil)
		if err != nil || p.Approval.State != approvalNotRequired || p.Qualifying != (name == "release installation") {
			t.Errorf("%s: predicate = %+v, %v", name, p, err)
		}
	}
}

// A run for an earlier rollout or another input never promotes the rollout:
// it is deleted unread, and a change of validator pins starts a new run.
func TestValidationIgnoresOtherRuns(t *testing.T) {
	ctx := testContext(t)
	rt, _, _ := validatedRuntime(t, ctx)
	r, f := newValidating(t, newEngine(), true)
	stale, err := aicrjob.Build(&aicrjob.Request{
		Stage: aicrjob.StageValidation, RunID: "0123456789ab", RuntimeName: rt.Name, RuntimeUID: string(rt.UID), Image: aicrImage,
		Namespaces: r.Config.Namespaces(), ServiceAccount: "aicr-validation", CredentialSecret: "creds", Input: "fedcba9876543210",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = k8s.Create(ctx, stale); err != nil {
		t.Fatal(err)
	}
	succeed(t, ctx, stale, nil)
	f.pushEvidence(t, "0123456789ab", "h100", 0, deploymentValidator)

	job := untilValidating(t, ctx, r)
	if job.Name == stale.Name {
		t.Fatal("the stale run was adopted")
	}
	if err = k8s.Get(ctx, client.ObjectKeyFromObject(stale), stale); err != nil || stale.DeletionTimestamp.IsZero() {
		t.Fatalf("stale run not deleted: %v", err)
	}
	reconcileRollout(t, ctx, r)
	if ro := rollout(t, ctx); ro.Labels[v1alpha1.LabelOutcome] != "" || ro.Status.Validation.RunID != job.Labels[v1alpha1.LabelRunID] {
		t.Fatalf("rollout = %v %+v", ro.Labels, ro.Status.Validation)
	}

	// New validator pins are a new input: the earlier run is no longer used.
	r.Config.ValidatorImages = r.Config.ValidatorImages[:1]
	reconcileRollout(t, ctx, r)
	jobs := liveJobs(t, ctx, string(aicrjob.StageValidation))
	if len(jobs) != 1 || jobs[0].Name == job.Name {
		t.Fatalf("validation Jobs after a pin change = %d", len(jobs))
	}
}

// A failed run fails the rollout, recording the evidence it pushed; nothing
// is promoted and no evidence annotation is set.
func TestValidationFailureFailsRollout(t *testing.T) {
	for name, tc := range map[string]struct {
		exit     int32
		pushed   bool
		evidence bool
	}{
		"checks failed":     {exit: aicrjob.ExitInternal, pushed: true, evidence: true},
		"pre-flight failed": {exit: aicrjob.ExitInvalidInput},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			validatedRuntime(t, ctx)
			r, f := newValidating(t, newEngine(), true)
			job := untilValidating(t, ctx, r)
			if tc.pushed {
				f.pushEvidence(t, job.Labels[v1alpha1.LabelRunID], "h100", 3, deploymentValidator)
			}
			fail(t, ctx, job, tc.exit)
			reconcileRollout(t, ctx, r)
			ro := rollout(t, ctx)
			if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || ro.Annotations[v1alpha1.AnnotationEvidence] != "" ||
				ro.Status.Validation.Result != resultFailed || (ro.Status.Validation.Evidence != "") != tc.evidence ||
				!strings.Contains(ro.Status.Message, "deployment validation failed") {

				t.Fatalf("rollout = %v %+v", ro.Labels, ro.Status)
			}
			if tc.evidence && f.binding(t, ro.Status.Validation.Evidence).Predicate.Result != "Failed" {
				t.Fatal("failed run bound as passed")
			}
			if got := getRuntime(t, ctx); got.Status.ActivePlanDigest != "" {
				t.Fatalf("a failed validation promoted %s", got.Status.ActivePlanDigest)
			}
		})
	}
}

// Evidence naming a validator image that is not pinned, a mutable tag, a
// pinned tag that moved, another recipe or a failed check fails the
// rollout, even when the run exited 0.
func TestValidationRejectsEvidence(t *testing.T) {
	for name, tc := range map[string]struct {
		recipe     string
		failed     int
		validators []string
		moved      bool
		want       string
	}{
		"mutable tag":   {recipe: "h100", validators: []string{"ghcr.io/nvidia/aicr-validators/deployment:latest"}, want: "validator images differ"},
		"unpinned":      {recipe: "h100", validators: []string{deploymentValidator, "ghcr.io/nvidia/aicr-validators/performance:v1.0.0"}, want: "is not pinned"},
		"moved tag":     {recipe: "h100", validators: []string{deploymentValidator}, moved: true, want: "not the pinned"},
		"other recipe":  {recipe: "other", validators: []string{deploymentValidator}, want: "not the plan's h100"},
		"failed checks": {recipe: "h100", failed: 1, validators: []string{deploymentValidator}, want: "records failed checks"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			validatedRuntime(t, ctx)
			r, f := newValidating(t, newEngine(), true)
			if tc.moved {
				f.tags[deploymentValidator] = conformanceDigest
			}
			job := untilValidating(t, ctx, r)
			f.pushEvidence(t, job.Labels[v1alpha1.LabelRunID], tc.recipe, tc.failed, tc.validators...)
			succeed(t, ctx, job, nil)
			reconcileRollout(t, ctx, r)
			ro := rollout(t, ctx)
			if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || ro.Annotations[v1alpha1.AnnotationEvidence] != "" ||
				!strings.Contains(ro.Status.Message, tc.want) {

				t.Fatalf("rollout = %v %v %+v", ro.Labels, ro.Annotations, ro.Status)
			}
		})
	}
}

// On AICR v1.1.0 and later the validation run signs its evidence with the
// configured KMS key and reports the evidence digest through its termination
// message, so binding needs no registry lookup of the run tag. AICR attaches
// the signature to the evidence as a Sigstore bundle referrer (the operator once read it from inside the bundle and rejected every signed
// run). The binding records the key's fingerprint; evidence without a
// signature, or signed by more than one key, is rejected.
func TestValidationOnV11(t *testing.T) {
	const key = "awskms:///alias/aicr-evidence"
	hinted := func(publicKey string) []byte {
		sum := sha256.Sum256([]byte(publicKey))
		return []byte(`{"verificationMaterial":{"publicKey":{"hint":"` + base64.StdEncoding.EncodeToString(sum[:]) + `"}}}` + "\n")
	}
	sum := sha256.Sum256([]byte("public key"))
	signed := hinted("public key")
	for name, tc := range map[string]struct {
		attestation []byte
		other       []byte
		exit        int32
		outcome     v1alpha1.RolloutPhase
		result      string
		want        string
	}{
		"signed and passed":       {attestation: signed, outcome: v1alpha1.PhaseSucceeded, result: "Passed"},
		"signed twice by one key": {attestation: signed, other: signed, outcome: v1alpha1.PhaseSucceeded, result: "Passed"},
		"checks failed":           {attestation: signed, exit: aicrjob.ExitInternal, outcome: v1alpha1.PhaseFailed, result: "Failed", want: "deployment validation failed"},
		"unsigned":                {outcome: v1alpha1.PhaseFailed, want: "is not signed, but validation.signingKey is set"},
		"signed by two keys":      {attestation: signed, other: hinted("another key"), outcome: v1alpha1.PhaseFailed, want: "signed by more than one key"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			validatedRuntime(t, ctx)
			r, f := newValidating(t, newEngine(), true)
			r.Config.AICRVersion, f.aicrVersion = "v1.1.2", "1.1.2"
			r.Config.ValidationSigningKey, r.Config.ValidationTransparencyLog = key, false
			f.attestation = tc.attestation
			job := untilValidating(t, ctx, r)
			args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
			wantArgs := []string{"--signing-key " + key + " --tlog-upload=false", "--validator-service-account-name aicr-validator",
				"--image-refs /dev/termination-log"}
			for _, want := range wantArgs {
				if !strings.Contains(args, want) || strings.Contains(args, "--no-sign") {
					t.Fatalf("validation args %q lack %q", args, want)
				}
			}
			runID := job.Labels[v1alpha1.LabelRunID]
			evidenceDigest := f.pushEvidence(t, runID, "h100", min(int(tc.exit), 1), deploymentValidator)
			if tc.other != nil {
				f.signatures[evidenceDigest] = append(f.signatures[evidenceDigest], tc.other)
			}
			// The run tag is not consulted: the digest comes from the run.
			delete(f.tags, evidenceRepo+":run-"+runID)
			if tc.exit == 0 {
				succeedReporting(t, ctx, job, evidenceDigest)
			} else {
				failReporting(t, ctx, job, tc.exit, evidenceDigest)
			}
			reconcileRollout(t, ctx, r)
			ro := rollout(t, ctx)
			if ro.Labels[v1alpha1.LabelOutcome] != string(tc.outcome) || !strings.Contains(ro.Status.Message, tc.want) {
				t.Fatalf("rollout = %v %+v", ro.Labels, ro.Status)
			}
			if tc.result == "" {
				return
			}
			b := f.binding(t, ro.Status.Validation.Evidence)
			if b.Predicate.Result != tc.result || b.Subject[0].Digest["sha256"] != strings.TrimPrefix(evidenceDigest, "sha256:") ||
				b.Predicate.AICREvidence.KeyFingerprint != "sha256:"+hex.EncodeToString(sum[:]) || b.Predicate.AICR.Version != "v1.1.2" {

				t.Fatalf("binding = %+v", b.Predicate)
			}
		})
	}
}

// A registry outage while binding is retried, never a verdict.
func TestValidationRetriesRegistryOutage(t *testing.T) {
	ctx := testContext(t)
	validatedRuntime(t, ctx)
	r, f := newValidating(t, newEngine(), true)
	job := untilValidating(t, ctx, r)
	f.pushEvidence(t, job.Labels[v1alpha1.LabelRunID], "h100", 0, deploymentValidator)
	succeed(t, ctx, job, nil)
	f.resolveErr = operrors.New(operrors.ErrCodeUnavailable, "registry down")
	if _, err := r.Reconcile(ctx, request); operrors.CodeOf(err) != operrors.ErrCodeUnavailable {
		t.Fatalf("Reconcile() = %v", err)
	}
	if ro := rollout(t, ctx); ro.Labels[v1alpha1.LabelOutcome] != "" {
		t.Fatalf("outcome during an outage: %v", ro.Labels)
	}
	f.resolveErr = nil
	reconcileRollout(t, ctx, r)
	if ro := rollout(t, ctx); ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) {
		t.Fatalf("outcome after the outage: %v %+v", ro.Labels, ro.Status)
	}
}

// Without a stored snapshot the rollout takes its own; without the
// installation's validation identities it stalls Validating; with
// validation disabled the runtime is Ready with reason ValidationDisabled.
func TestValidationUnavailableOrDisabled(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
		rt.Spec.Validation.Deployment = v1alpha1.DeploymentValidationRequired
	})
	resolved(t, ctx, putResolved(t, ctx, rt, map[string]string{"accelerator": "h100"}, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"50m"}`)))
	r, _ := newValidating(t, newEngine(), true)
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	ro := rollout(t, ctx)
	c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled)
	if ro.Status.Phase != v1alpha1.PhaseValidating || c.Status == metav1.ConditionTrue || len(liveJobs(t, ctx, string(aicrjob.StageSnapshot))) != 1 {
		t.Fatalf("without a stored snapshot the rollout takes one: rollout %+v, Stalled %+v", ro.Status, c)
	}
	r.Config.ValidationEnabled = false
	reconcileRollout(t, ctx, r)
	if c = cond(getRuntime(t, ctx), v1alpha1.ConditionStalled); c.Reason != ReasonValidationMissing {
		t.Fatalf("without validation installed: Stalled %+v", c)
	}

	rt = freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	digest := putPlan(t, ctx, rt, "50m")
	resolved(t, ctx, digest)
	r, _ = newRollout(t, newEngine(), true)
	for range 4 {
		reconcileRollout(t, ctx, r)
	}
	got := getRuntime(t, ctx)
	if ready := cond(got, v1alpha1.ConditionReady); got.Status.ActivePlanDigest != digest || ready.Status != metav1.ConditionTrue ||
		ready.Reason != ReasonValidationDisabled {

		t.Fatalf("Ready = %+v", ready)
	}
}

func TestOperatorImage(t *testing.T) {
	ctx := testContext(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "aicr-operator-0", Namespace: operatorNS},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "ko.local/aicr-operator:test"}}},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.WithoutCancel(ctx), pod) })
	o := &OperatorImage{Reader: k8s, Namespace: operatorNS, Pod: pod.Name, Container: "manager"}
	if _, err := o.Get(ctx); operrors.CodeOf(err) != operrors.ErrCodeUnavailable {
		t.Fatalf("Get() before the kubelet reports = %v", err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "manager", ImageID: "docker.io/library/import@" + operatorDigest}}
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	got, err := o.Get(ctx)
	if err != nil || got.Image != "ko.local/aicr-operator:test" || got.Digest != operatorDigest {
		t.Fatalf("Get() = %+v, %v", got, err)
	}
	// A reference pinned by digest is its own digest, whatever image ID the
	// runtime reports for an image it loaded.
	pinned := "ghcr.io/nvidia/aicr-operator@sha256:" + strings.Repeat("2", 64)
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "aicr-operator-1", Namespace: operatorNS},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: pinned}}},
	}
	if err = k8s.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.WithoutCancel(ctx), other) })
	other.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "manager", ImageID: "docker.io/library/import-2026-10-16@" + operatorDigest}}
	if err = k8s.Status().Update(ctx, other); err != nil {
		t.Fatal(err)
	}
	o = &OperatorImage{Reader: k8s, Namespace: operatorNS, Pod: other.Name, Container: "manager"}
	if got, err = o.Get(ctx); err != nil || got.Digest != "sha256:"+strings.Repeat("2", 64) {
		t.Fatalf("Get() of a digest-pinned pod = %+v, %v", got, err)
	}
	for id, want := range map[string]string{operatorDigest: operatorDigest, "sha256:abc": "", "": ""} {
		if d := digestOfImageID(id); d != want {
			t.Errorf("digestOfImageID(%q) = %q", id, d)
		}
	}
}

// The evidence records who acknowledged a manual release's post-apply
// steps, with the approval it belongs to, and a revocation made after the
// last step, which stopped nothing.
func TestApprovalRecordCarriesAcknowledgements(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 10, 28, 19, 25, 50, 0, time.UTC))
	revokedAt := metav1.NewTime(at.Add(time.Minute))
	ro := &v1alpha1.RuntimeRollout{Spec: v1alpha1.RuntimeRolloutSpec{ApprovalRequired: true, Approval: &v1alpha1.Approval{
		ApprovedBy: "alice", Reason: "window", ExpiresAt: metav1.NewTime(at.Add(time.Hour)),
		RevokedBy: "carol", RevokedAt: &revokedAt, RevokeReason: "incident",
		Acknowledgements: []v1alpha1.Acknowledgement{{Release: "nvsentinel", AcknowledgedBy: "bob", AcknowledgedAt: &at}},
	}}}
	got := approvalRecord(ro)
	want := []evidence.Acknowledgement{{Release: "nvsentinel", AcknowledgedBy: "bob", AcknowledgedAt: "2026-10-28T19:25:50Z"}}
	if got.ApprovedBy != "alice" || !slices.Equal(got.Acknowledgements, want) {
		t.Fatalf("approvalRecord() = %+v", got)
	}
	if got.RevokedBy != "carol" || got.RevokedAt != "2026-10-28T19:26:50Z" || got.RevokeReason != "incident" {
		t.Fatalf("approvalRecord() revocation = %+v", got)
	}
	ro.Spec.Approval.RevokedBy, ro.Spec.Approval.RevokedAt, ro.Spec.Approval.RevokeReason = "", nil, ""
	if got = approvalRecord(ro); got.RevokedBy != "" || got.RevokedAt != "" || got.RevokeReason != "" {
		t.Fatalf("approvalRecord() without a revocation = %+v", got)
	}
}
