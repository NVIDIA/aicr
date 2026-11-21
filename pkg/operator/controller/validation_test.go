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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "aicr.run/api/operator/v1alpha1"
	"aicr.run/pkg/operator/aicrjob"
	"aicr.run/pkg/operator/defaults"
	operrors "aicr.run/pkg/operator/errors"
	"aicr.run/pkg/operator/evidence"
	"aicr.run/pkg/operator/plan"
	"aicr.run/pkg/operator/registry"
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
	// pullErr fails every evidence bundle read.
	pullErr error
	// aicrVersion is the AICR version evidence reports; empty is 1.0.0.
	aicrVersion string
	// attestation, when set, is AICR's signed statement, which AICR
	// attaches to the evidence as a Sigstore bundle referrer, never inside
	// the evidence bundle.
	attestation []byte
	// signatures are the Sigstore bundle referrers of each evidence digest.
	signatures map[string][][]byte
	// indexes are the manifests each image index digest lists, set before
	// the reconciler runs.
	indexes map[string][]string
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
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	if b, ok := f.bundles[d]; ok {
		return b, nil
	}
	return nil, operrors.New(operrors.ErrCodeNotFound, d+" not found")
}

func (f *fakeEvidence) IndexManifests(_ context.Context, _ registry.Repository, d string) ([]string, error) {
	return f.indexes[d], nil
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
// statement names the recipe and validators, with the checks' results. A
// validator given as image@digest records that digest.
func (f *fakeEvidence) pushEvidence(t *testing.T, runID, recipe string, failed int, validators ...string) string {
	t.Helper()
	version := f.aicrVersion
	if version == "" {
		version = "1.0.0"
	}
	images := make([]map[string]string, 0, len(validators))
	for _, v := range validators {
		image, dgst, _ := strings.Cut(v, "@")
		images = append(images, map[string]string{"image": image, "digest": dgst})
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
		"unauthorized":      {exit: aicrjob.ExitUnauthorized},
		"no exit":           {exit: -1},
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
				!strings.Contains(ro.Status.Message, "deployment validation failed: ") {

				t.Fatalf("rollout = %v %+v", ro.Labels, ro.Status)
			}
			if tc.evidence && f.binding(t, ro.Status.Validation.Evidence).Predicate.Result != "Failed" {
				t.Fatal("failed run bound as passed")
			}
			if strings.Contains(ro.Status.Message, "ended on a transient error") {
				t.Fatalf("a run that did not fail on a transient exit is reported as one: %q", ro.Status.Message)
			}
			if got := getRuntime(t, ctx); got.Status.ActivePlanDigest != "" {
				t.Fatalf("a failed validation promoted %s", got.Status.ActivePlanDigest)
			}
		})
	}
}

// A validation run AICR ended on a transient exit judged nothing: it is run
// again after a backoff, within the rollout and its approval, up to
// maxStageRuns runs, and the rollout says so meanwhile. Any other failure
// still fails the rollout on that run.
func TestValidationRetriesTransientExits(t *testing.T) {
	tests := []struct {
		name string
		// exits are the exit codes of the runs that fail, in order.
		exits []int32
		// passes: the run after them passes.
		passes bool
		// wantFailure is in the message of the rollout the last of them fails.
		wantFailure string
	}{
		{name: "the run after a transient exit passes", exits: []int32{aicrjob.ExitUnavailable}, passes: true},
		{name: "transient exits fail the rollout at the bound",
			exits:       []int32{aicrjob.ExitUnavailable, aicrjob.ExitTimeout, aicrjob.ExitRateLimited},
			wantFailure: "deployment validation failed after 3 runs"},
		{name: "another failure after a transient exit fails at once",
			exits: []int32{aicrjob.ExitCanceled, aicrjob.ExitInternal}, wantFailure: "deployment validation failed after 2 runs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testContext(t)
			validatedRuntime(t, ctx)
			r, f := newValidating(t, newEngine(), true)
			now := time.Now()
			r.Now = func() time.Time { return now }
			job := untilValidating(t, ctx, r)
			for run, exit := range tt.exits {
				// The Job's failure is stamped with the wall clock.
				now = time.Now()
				failReporting(t, ctx, job, exit, "[SERVICE_UNAVAILABLE] the registry is down")
				reconcileRollout(t, ctx, r)
				ro := rollout(t, ctx)
				if run == len(tt.exits)-1 && !tt.passes {
					if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || ro.Status.Validation.Result != resultFailed ||
						!strings.Contains(ro.Status.Message, tt.wantFailure) {

						t.Fatalf("run %d exiting %d: rollout = %v %+v, want Failed with %q", run+1, exit, ro.Labels, ro.Status, tt.wantFailure)
					}
					if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReconciling); c.Message != "" {
						t.Fatalf("Reconciling after the rollout failed = %+v, want no message", c)
					}
					return
				}
				if ro.Labels[v1alpha1.LabelOutcome] != "" || ro.Status.Phase != v1alpha1.PhaseValidating ||
					ro.Status.Validation.Result != resultPending || ro.Status.Validation.Job != job.Name ||
					!strings.Contains(ro.Status.Message, "retrying after a transient failure") ||
					!strings.Contains(ro.Status.Message, "the registry is down") {

					t.Fatalf("run %d exiting %d: rollout = %v %+v, want it waiting to retry", run+1, exit, ro.Labels, ro.Status)
				}
				if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReconciling); c.Reason != ReasonValidating ||
					!strings.HasPrefix(c.Message, retryingNote) || !strings.Contains(c.Message, "the registry is down") {

					t.Fatalf("run %d exiting %d: Reconciling = %+v, want the retry and its cause", run+1, exit, c)
				}
				// Nothing starts before the backoff passes.
				reconcileRollout(t, ctx, r)
				if got := rollout(t, ctx).Status.Validation.Job; got != job.Name {
					t.Fatalf("run %d: a new run %s started within the backoff", run+1, got)
				}
				now = time.Now().Add(defaults.StageRetryMaxBackoff + time.Minute)
				reconcileRollout(t, ctx, r)
				ro = rollout(t, ctx)
				next := &batchv1.Job{}
				if err := k8s.Get(ctx, client.ObjectKey{Namespace: validateNS, Name: ro.Status.Validation.Job}, next); err != nil {
					t.Fatal(err)
				}
				if next.Name == job.Name || attempt(next) != run+2 || strings.Contains(ro.Status.Message, "retrying") ||
					cond(getRuntime(t, ctx), v1alpha1.ConditionReconciling).Message != "" {

					t.Fatalf("after the backoff: Job %s (attempt %d), message %q; want run %d in progress",
						next.Name, attempt(next), ro.Status.Message, run+2)
				}
				job = next
			}
			f.pushEvidence(t, job.Labels[v1alpha1.LabelRunID], "h100", 0, deploymentValidator, conformanceValidator)
			succeed(t, ctx, job, nil)
			reconcileRollout(t, ctx, r)
			if ro := rollout(t, ctx); ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || ro.Status.Validation.Result != resultPassed ||
				ro.Status.Validation.RunID != job.Labels[v1alpha1.LabelRunID] {

				t.Fatalf("rollout after the passing run = %v %+v", ro.Labels, ro.Status)
			}
			if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReconciling); c.Message != "" {
				t.Fatalf("Reconciling after the rollout succeeded = %+v, want no message", c)
			}
		})
	}
}

// The run the stage judged is the one recorded and bound, even when it has
// left the Job listing by the time the rollout reads it again: whether an
// earlier run that was retried is then the newest one listed, or none is.
func TestValidationBindsTheRunItJudged(t *testing.T) {
	tests := []struct {
		name    string
		retried bool
		// exit is how the judged run ends; zero passes.
		exit int32
	}{
		{name: "a first run that passed"},
		{name: "a run that passed after a retried one", retried: true},
		{name: "a first run that failed its checks", exit: aicrjob.ExitInternal},
		{name: "a run that failed its checks after a retried one", retried: true, exit: aicrjob.ExitInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testContext(t)
			validatedRuntime(t, ctx)
			r, f := newValidating(t, newEngine(), true)
			now := time.Now()
			r.Now = func() time.Time { return now }
			judged := untilValidating(t, ctx, r)
			if tt.retried {
				first := judged
				failReporting(t, ctx, first, aicrjob.ExitUnavailable, "[SERVICE_UNAVAILABLE] the registry is down")
				reconcileRollout(t, ctx, r)
				now = time.Now().Add(defaults.StageRetryMaxBackoff + time.Minute)
				reconcileRollout(t, ctx, r)
				judged = &batchv1.Job{}
				if err := k8s.Get(ctx, client.ObjectKey{Namespace: validateNS, Name: rollout(t, ctx).Status.Validation.Job}, judged); err != nil {
					t.Fatal(err)
				}
				if judged.Name == first.Name {
					t.Fatal("no second run started")
				}
			}
			wantOutcome, wantResult, failedChecks := v1alpha1.PhaseSucceeded, resultPassed, 0
			if tt.exit != 0 {
				wantOutcome, wantResult, failedChecks = v1alpha1.PhaseFailed, resultFailed, 3
			}
			f.pushEvidence(t, judged.Labels[v1alpha1.LabelRunID], "h100", failedChecks, deploymentValidator, conformanceValidator)
			if tt.exit == 0 {
				succeed(t, ctx, judged, nil)
			} else {
				fail(t, ctx, judged, tt.exit)
			}

			// The stage reads the validation Jobs once to drive them; every
			// later listing in the reconcile no longer shows the run it
			// judged.
			base, err := client.NewWithWatch(restConfig, client.Options{Scheme: k8s.Scheme()})
			if err != nil {
				t.Fatal(err)
			}
			seen := 0
			r.Reader = interceptor.NewClient(base, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					jobs, isJobs := list.(*batchv1.JobList)
					if !isJobs || !slices.ContainsFunc(jobs.Items, func(j batchv1.Job) bool { return j.Name == judged.Name }) {
						return nil
					}
					// pruneValidation lists them first, then the stage.
					if seen++; seen > 2 {
						jobs.Items = slices.DeleteFunc(jobs.Items, func(j batchv1.Job) bool { return j.Name == judged.Name })
					}
					return nil
				},
			})
			reconcileRollout(t, ctx, r)
			ro := rollout(t, ctx)
			if seen <= 2 {
				t.Fatalf("the validation Jobs were listed %d times, so the judged run never left the listing", seen)
			}
			val := ro.Status.Validation
			if ro.Labels[v1alpha1.LabelOutcome] != string(wantOutcome) || val.Job != judged.Name ||
				val.RunID != judged.Labels[v1alpha1.LabelRunID] || val.Result != wantResult || val.Evidence == "" {

				t.Fatalf("rollout = %v %+v, want %s on run %s with its evidence", ro.Labels, val, wantOutcome, judged.Name)
			}
		})
	}
}

// A run that failed on a transient exit is run again only when it left no
// verdict of failure: evidence it pushed that records a failed check ends
// the rollout Failed on that run, with that evidence, and nothing is run
// again. Evidence that cannot be read as a verdict is no verdict. While the
// registry does not answer, or refuses the operator, no run starts.
func TestValidationTransientExitReadsTheRunsEvidence(t *testing.T) {
	const (
		failed  = "the rollout fails on the run"
		rerun   = "a second run starts"
		blocked = "no run starts until the registry answers, then the rollout fails on the run"
	)
	unavailable := operrors.New(operrors.ErrCodeUnavailable, "registry down")
	refused := operrors.New(operrors.ErrCodeUnauthorized, "registry refused the credential")
	tests := []struct {
		name string
		// failedChecks is the number of failed checks in the evidence the
		// run pushed; negative pushes none.
		failedChecks int
		// spoil changes the pushed evidence bundle before the run fails.
		spoil               func(*registry.Bundle)
		resolveErr, pullErr error
		want                string
		// wantVerdict is what the failure message says the evidence
		// records; empty is failed checks.
		wantVerdict string
	}{
		{name: "evidence records a failed check", failedChecks: 3, want: failed},
		{name: "evidence records a phase with no check passed or failed", failedChecks: 0, want: failed,
			wantVerdict: "records no failed check, but not that validation passed",
			spoil: func(b *registry.Bundle) {
				b.Files[evidence.AICRStatementFile] = []byte(strings.Replace(string(b.Files[evidence.AICRStatementFile]),
					`"passed":4`, `"passed":0,"skipped":4`, 1))
			}},
		{name: "evidence records every check passed", failedChecks: 0, want: rerun},
		{name: "no evidence was pushed", failedChecks: -1, want: rerun},
		{name: "evidence without a statement", failedChecks: 3, want: rerun,
			spoil: func(b *registry.Bundle) { delete(b.Files, evidence.AICRStatementFile) }},
		{name: "evidence whose statement does not parse", failedChecks: 3, want: rerun,
			spoil: func(b *registry.Bundle) { b.Files[evidence.AICRStatementFile] = []byte("not a statement") }},
		{name: "evidence the registry cannot serve as a bundle", failedChecks: 3, want: rerun,
			pullErr: operrors.New(operrors.ErrCodeInvalidRequest, "not an evidence bundle")},
		{name: "the registry does not answer for the tag", failedChecks: 3, resolveErr: unavailable, want: blocked},
		{name: "the registry refuses the tag lookup", failedChecks: 3, resolveErr: refused, want: blocked},
		{name: "the registry does not answer for the evidence", failedChecks: 3, pullErr: unavailable, want: blocked},
		{name: "the registry refuses the evidence read", failedChecks: 3, pullErr: refused, want: blocked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testContext(t)
			validatedRuntime(t, ctx)
			r, f := newValidating(t, newEngine(), true)
			now := time.Now()
			r.Now = func() time.Time { return now }
			job := untilValidating(t, ctx, r)
			if tt.failedChecks >= 0 {
				d := f.pushEvidence(t, job.Labels[v1alpha1.LabelRunID], "h100", tt.failedChecks, deploymentValidator, conformanceValidator)
				if tt.spoil != nil {
					tt.spoil(f.bundles[d])
				}
			}
			now = time.Now()
			failReporting(t, ctx, job, aicrjob.ExitUnavailable, "[SERVICE_UNAVAILABLE] signing timed out")
			past := func() { now = time.Now().Add(defaults.StageRetryMaxBackoff + time.Minute) }
			runs := func() int { return len(liveJobs(t, ctx, string(aicrjob.StageValidation))) }

			f.resolveErr, f.pullErr = tt.resolveErr, tt.pullErr
			if tt.want == blocked {
				for range 2 {
					if _, err := r.Reconcile(ctx, request); operrors.CodeOf(err) != operrors.ErrCodeUnavailable {
						t.Fatalf("Reconcile() = %v, want UNAVAILABLE while the evidence cannot be read", err)
					}
					past()
				}
				ro := rollout(t, ctx)
				if ro.Labels[v1alpha1.LabelOutcome] != "" || runs() != 1 {
					t.Fatalf("while the registry does not answer: rollout %v, %d runs; want no outcome and no new run", ro.Labels, runs())
				}
				if want := "deployment validation run " + job.Labels[v1alpha1.LabelRunID] + " failed; its evidence cannot be read yet"; !strings.Contains(ro.Status.Message, want) {
					t.Fatalf("while the registry does not answer: rollout message %q, want %q", ro.Status.Message, want)
				}
				f.resolveErr, f.pullErr = nil, nil
			}
			reconcileRollout(t, ctx, r)
			past()
			reconcileRollout(t, ctx, r)
			ro := rollout(t, ctx)
			if tt.want == rerun {
				if ro.Labels[v1alpha1.LabelOutcome] != "" || ro.Status.Validation.Job == job.Name || runs() != 2 {
					t.Fatalf("rollout %v %+v with %d runs, want a second run in progress", ro.Labels, ro.Status.Validation, runs())
				}
				return
			}
			val := ro.Status.Validation
			verdict := cmp.Or(tt.wantVerdict, "records failed checks")
			if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || val.Result != resultFailed || val.Job != job.Name ||
				val.RunID != job.Labels[v1alpha1.LabelRunID] || val.Evidence == "" ||
				!strings.HasPrefix(ro.Status.Message, "deployment validation failed: the evidence run "+val.RunID+" pushed "+verdict+"; ") ||
				!strings.Contains(ro.Status.Message, "signing timed out") {

				t.Fatalf("rollout %v %+v %q, want Failed on run %s with its evidence", ro.Labels, val, ro.Status.Message, job.Name)
			}
			if f.binding(t, val.Evidence).Predicate.Result != "Failed" {
				t.Fatal("the failed run's evidence is bound as passed")
			}
			if got := getRuntime(t, ctx); got.Status.ActivePlanDigest != "" {
				t.Fatalf("a run with failed checks promoted %s", got.Status.ActivePlanDigest)
			}
		})
	}
}

// A run whose evidence cannot be read yet is named from the Job the stage
// observed, not from the rollout's stored status, which a failed status
// write can leave naming an earlier run.
func TestValidationNamesTheRunWhoseEvidenceCannotBeRead(t *testing.T) {
	ctx := testContext(t)
	validatedRuntime(t, ctx)
	r, f := newValidating(t, newEngine(), true)
	now := time.Now()
	r.Now = func() time.Time { return now }
	first := untilValidating(t, ctx, r)
	failReporting(t, ctx, first, aicrjob.ExitUnavailable, "[SERVICE_UNAVAILABLE] the registry is down")
	reconcileRollout(t, ctx, r)
	now = time.Now().Add(defaults.StageRetryMaxBackoff + time.Minute)
	reconcileRollout(t, ctx, r)
	ro := rollout(t, ctx)
	second := &batchv1.Job{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: validateNS, Name: ro.Status.Validation.Job}, second); err != nil {
		t.Fatal(err)
	}
	if second.Name == first.Name {
		t.Fatal("no second run started")
	}
	// The status write that recorded the second run is lost.
	ro.Status.Validation = &v1alpha1.ValidationStatus{Job: first.Name, RunID: first.Labels[v1alpha1.LabelRunID], Result: resultPending}
	if err := k8s.Status().Update(ctx, ro); err != nil {
		t.Fatal(err)
	}
	now = time.Now()
	failReporting(t, ctx, second, aicrjob.ExitUnavailable, "[SERVICE_UNAVAILABLE] the registry is down")
	f.resolveErr = operrors.New(operrors.ErrCodeUnavailable, "registry down")
	if _, err := r.Reconcile(ctx, request); operrors.CodeOf(err) != operrors.ErrCodeUnavailable {
		t.Fatalf("Reconcile() = %v, want UNAVAILABLE", err)
	}
	ro = rollout(t, ctx)
	runID := second.Labels[v1alpha1.LabelRunID]
	if ro.Status.Validation.Job != second.Name || ro.Status.Validation.RunID != runID ||
		!strings.Contains(ro.Status.Message, "deployment validation run "+runID+" failed; its evidence cannot be read yet") {

		t.Fatalf("rollout %+v %q, want the second run %s named", ro.Status.Validation, ro.Status.Message, runID)
	}
}

// With a signing key, a run whose checks failed and whose signing then timed
// out leaves unsigned evidence: the rollout fails on that run and nothing is
// run again. The evidence cannot be bound without its signature, so the
// rollout names it in its message and records no evidence reference.
func TestValidationSigningTimeoutAfterFailedChecks(t *testing.T) {
	ctx := testContext(t)
	validatedRuntime(t, ctx)
	r, f := newValidating(t, newEngine(), true)
	r.Config.AICRVersion, f.aicrVersion = "v1.1.2", "1.1.2"
	r.Config.ValidationSigningKey, r.Config.ValidationTransparencyLog = "awskms:///alias/aicr-evidence", false
	now := time.Now()
	r.Now = func() time.Time { return now }
	job := untilValidating(t, ctx, r)
	d := f.pushEvidence(t, job.Labels[v1alpha1.LabelRunID], "h100", 3, deploymentValidator, conformanceValidator)
	now = time.Now()
	// The termination message is the end of AICR's log, not a pushed digest.
	failReporting(t, ctx, job, aicrjob.ExitTimeout, "[TIMEOUT] sign evidence: context deadline exceeded")
	reconcileRollout(t, ctx, r)
	now = time.Now().Add(defaults.StageRetryMaxBackoff + time.Minute)
	reconcileRollout(t, ctx, r)
	ro := rollout(t, ctx)
	val := ro.Status.Validation
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || val.Result != resultFailed || val.Job != job.Name || val.Evidence != "" {
		t.Fatalf("rollout %v %+v, want Failed on run %s with no bound evidence", ro.Labels, val, job.Name)
	}
	for _, want := range []string{"deployment validation failed: the evidence run " + val.RunID + " pushed records failed checks",
		"sign evidence: context deadline exceeded", "that evidence is not recorded on the rollout", d, "is not signed"} {
		if !strings.Contains(ro.Status.Message, want) {
			t.Fatalf("rollout message %q lacks %q", ro.Status.Message, want)
		}
	}
	if got := getRuntime(t, ctx); got.Status.ActivePlanDigest != "" {
		t.Fatalf("a run with failed checks promoted %s", got.Status.ActivePlanDigest)
	}
}

// A failed run whose evidence cannot be read for now does not end the
// rollout without it: the reconcile is retried, and the evidence is bound
// once the registry answers.
func TestValidationFailureWaitsForItsEvidence(t *testing.T) {
	ctx := testContext(t)
	validatedRuntime(t, ctx)
	r, f := newValidating(t, newEngine(), true)
	job := untilValidating(t, ctx, r)
	f.pushEvidence(t, job.Labels[v1alpha1.LabelRunID], "h100", 3, deploymentValidator)
	fail(t, ctx, job, aicrjob.ExitInternal)
	f.pullErr = operrors.New(operrors.ErrCodeUnavailable, "registry down")
	if _, err := r.Reconcile(ctx, request); operrors.CodeOf(err) != operrors.ErrCodeUnavailable {
		t.Fatalf("Reconcile() = %v, want UNAVAILABLE", err)
	}
	if ro := rollout(t, ctx); ro.Labels[v1alpha1.LabelOutcome] != "" {
		t.Fatalf("outcome while the evidence cannot be read: %v", ro.Labels)
	}
	f.pullErr = nil
	reconcileRollout(t, ctx, r)
	if ro := rollout(t, ctx); ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || ro.Status.Validation.Evidence == "" {
		t.Fatalf("rollout %v %+v, want Failed with its evidence", ro.Labels, ro.Status.Validation)
	}
}

// A retry that cannot be started for now is retried by the reconcile and is
// not reported as evidence that cannot be read: the rollout keeps the
// failed run's retry message and no outcome.
func TestValidationRetryThatCannotStartForNow(t *testing.T) {
	ctx := testContext(t)
	validatedRuntime(t, ctx)
	r, _ := newValidating(t, newEngine(), true)
	now := time.Now()
	r.Now = func() time.Time { return now }
	job := untilValidating(t, ctx, r)
	failReporting(t, ctx, job, aicrjob.ExitUnavailable, "[SERVICE_UNAVAILABLE] the registry is down")
	reconcileRollout(t, ctx, r)

	base, err := client.NewWithWatch(restConfig, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isJob := obj.(*batchv1.Job); isJob {
				return apierrors.NewServiceUnavailable("the API server is busy")
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	now = time.Now().Add(defaults.StageRetryMaxBackoff + time.Minute)
	if _, err = r.Reconcile(ctx, request); operrors.CodeOf(err) != operrors.ErrCodeUnavailable {
		t.Fatalf("Reconcile() = %v, want UNAVAILABLE", err)
	}
	ro := rollout(t, ctx)
	if ro.Labels[v1alpha1.LabelOutcome] != "" || strings.Contains(ro.Status.Message, "its evidence cannot be read yet") ||
		!strings.Contains(ro.Status.Message, retryingNote) {

		t.Fatalf("rollout %v %q, want no outcome and the retry message kept", ro.Labels, ro.Status.Message)
	}
}

// A validation Job left suspended, as by an operator restart between
// creating and starting it, whose start is then refused for good fails the
// rollout as a run that could not run, naming no run.
func TestValidationResumeThatIsRefused(t *testing.T) {
	ctx := testContext(t)
	validatedRuntime(t, ctx)
	r, _ := newValidating(t, newEngine(), true)
	job := untilValidating(t, ctx, r)
	suspend := client.MergeFrom(job.DeepCopy())
	yes := true
	job.Spec.Suspend = &yes
	if err := k8s.Patch(ctx, job, suspend); err != nil {
		t.Fatal(err)
	}
	base, err := client.NewWithWatch(restConfig, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isJob := obj.(*batchv1.Job); isJob {
				return apierrors.NewForbidden(batchv1.Resource("jobs"), obj.GetName(), errors.New("denied by policy"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	reconcileRollout(t, ctx, r)
	ro := rollout(t, ctx)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || !strings.Contains(ro.Status.Message, "deployment validation could not run") ||
		!strings.Contains(ro.Status.Message, "denied by policy") || ro.Status.Validation.Job != "" || ro.Status.Validation.Result != resultFailed {

		t.Fatalf("rollout = %v %+v, want Failed because validation could not run, naming no run", ro.Labels, ro.Status)
	}
}

// What a validation stage waits on is reported on the rollout and on the
// runtime's Reconciling condition: a deployment snapshot waiting to be
// retried, and a validation run whose pod is not running.
func TestValidationReportsWhatItWaitsOn(t *testing.T) {
	const unschedulable = "0/9 nodes are available: 9 node(s) had untolerated taint {CriticalAddonsOnly: }"
	t.Run("a deployment snapshot that is retried", func(t *testing.T) {
		ctx := testContext(t)
		validatedRuntime(t, ctx)
		r, _ := newValidating(t, newEngine(), true)
		var snapshots []batchv1.Job
		for range 6 {
			reconcileRollout(t, ctx, r)
			if snapshots = liveJobs(t, ctx, string(aicrjob.StageSnapshot)); len(snapshots) > 0 {
				break
			}
		}
		if len(snapshots) != 1 {
			t.Fatalf("deployment snapshot Jobs = %d", len(snapshots))
		}
		now := time.Now()
		r.Now = func() time.Time { return now }
		failReporting(t, ctx, &snapshots[0], aicrjob.ExitUnavailable, "[SERVICE_UNAVAILABLE] the API server is busy")
		reconcileRollout(t, ctx, r)
		ro, c := rollout(t, ctx), cond(getRuntime(t, ctx), v1alpha1.ConditionReconciling)
		if !strings.Contains(ro.Status.Message, "taking the deployment snapshot; "+retryingNote) || c.Reason != ReasonValidating ||
			!strings.HasPrefix(c.Message, retryingNote) || !strings.Contains(c.Message, "the API server is busy") {

			t.Fatalf("rollout message %q, Reconciling %+v; want the snapshot's retry on both", ro.Status.Message, c)
		}
	})
	t.Run("a run whose pod is not running", func(t *testing.T) {
		ctx := testContext(t)
		validatedRuntime(t, ctx)
		r, _ := newValidating(t, newEngine(), true)
		job := untilValidating(t, ctx, r)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace,
				Labels: map[string]string{"batch.kubernetes.io/job-name": job.Name}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "aicr", Image: aicrImage}}},
		}
		if err := k8s.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.Phase = corev1.PodPending
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: unschedulable}}
		if err := k8s.Status().Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
		reconcileRollout(t, ctx, r)
		ro, c := rollout(t, ctx), cond(getRuntime(t, ctx), v1alpha1.ConditionReconciling)
		if !strings.Contains(ro.Status.Message, "in progress; ") || !strings.Contains(ro.Status.Message, unschedulable) ||
			c.Reason != ReasonValidating || !strings.Contains(c.Message, unschedulable) {

			t.Fatalf("rollout message %q, Reconciling %+v; want why the pod is not running on both", ro.Status.Message, c)
		}
	})
}

// A run that cannot be started after a transient exit fails the rollout as
// one that could not run: the earlier run, and any evidence it pushed, is not
// reported as the failure.
func TestValidationRetryThatCannotStart(t *testing.T) {
	ctx := testContext(t)
	validatedRuntime(t, ctx)
	r, f := newValidating(t, newEngine(), true)
	now := time.Now()
	r.Now = func() time.Time { return now }
	job := untilValidating(t, ctx, r)
	f.pushEvidence(t, job.Labels[v1alpha1.LabelRunID], "h100", 0, deploymentValidator, conformanceValidator)
	failReporting(t, ctx, job, aicrjob.ExitUnavailable, "[SERVICE_UNAVAILABLE] the registry is down")
	reconcileRollout(t, ctx, r)

	base, err := client.NewWithWatch(restConfig, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isJob := obj.(*batchv1.Job); isJob {
				return apierrors.NewForbidden(batchv1.Resource("jobs"), obj.GetName(), errors.New("exceeded quota"))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	now = time.Now().Add(defaults.StageRetryMaxBackoff + time.Minute)
	reconcileRollout(t, ctx, r)
	ro := rollout(t, ctx)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || !strings.Contains(ro.Status.Message, "deployment validation could not run") ||
		!strings.Contains(ro.Status.Message, "exceeded quota") {

		t.Fatalf("rollout = %v %+v, want Failed because validation could not run", ro.Labels, ro.Status)
	}
	if val := ro.Status.Validation; val.Result != resultFailed || val.Job != "" || val.Evidence != "" {
		t.Fatalf("validation status = %+v, want no run and no evidence named", val)
	}
}

// Evidence naming a validator image that is not pinned, a mutable tag, a
// digest the pinned image does not hold, a pinned tag that moved, another
// recipe or a failed check fails the rollout, even when the run exited 0.
func TestValidationRejectsEvidence(t *testing.T) {
	for name, tc := range map[string]struct {
		recipe     string
		failed     int
		validators []string
		moved      bool
		want       string
	}{
		"mutable tag":   {recipe: "h100", validators: []string{"ghcr.io/nvidia/aicr-validators/deployment:latest"}, want: "validator images differ"},
		"other digest":  {recipe: "h100", validators: []string{deploymentValidator + "@" + conformanceDigest}, want: "validator images differ"},
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

// Evidence recording the manifest digest of each node's platform, as CRI-O
// reports it, passes when the pinned image index lists those manifests, and
// the binding records the pinned digest.
func TestValidationAcceptsPlatformManifests(t *testing.T) {
	ctx := testContext(t)
	validatedRuntime(t, ctx)
	r, f := newValidating(t, newEngine(), true)
	amd64, arm64 := digest.FromString("amd64").String(), digest.FromString("arm64").String()
	f.indexes = map[string][]string{deploymentDigest: {amd64, arm64}}
	job := untilValidating(t, ctx, r)
	f.pushEvidence(t, job.Labels[v1alpha1.LabelRunID], "h100", 0, deploymentValidator+"@"+amd64, deploymentValidator+"@"+arm64)
	succeed(t, ctx, job, nil)
	reconcileRollout(t, ctx, r)
	ro := rollout(t, ctx)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) {
		t.Fatalf("rollout = %v %+v", ro.Labels, ro.Status)
	}
	got := f.binding(t, ro.Annotations[v1alpha1.AnnotationEvidence]).Predicate.ValidatorImages
	if want := []evidence.ValidatorImage{{Image: deploymentValidator, Digest: deploymentDigest}}; !slices.Equal(got, want) {
		t.Fatalf("binding validators = %+v, want %+v", got, want)
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
