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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"oras.land/oras-go/v2/registry/remote/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/evidence"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

const (
	validationNamespace = "aicr-validation"
	// validationAccount is AICR's launcher; validatorAccount the identity
	// AICR v1.1.0 and later run validators as.
	validationAccount = "aicr-validation"
	validatorAccount  = "aicr-validator"
)

// slowChecks are withheld so a kind run finishes in seconds: on a cluster
// without the GPU Operator every deployment check fails, and
// expected-resources waits out its timeout first. The operator never skips
// checks; operator-health still runs, fails, and is attested.
var slowChecks = []string{"--skip-check", "expected-resources", "--skip-check", "gpu-operator-version", "--skip-check", "check-nvidia-smi"}

// setupValidationIdentity creates the validation namespace and identities as
// the chart does with validation.enabled. AICR v1.0.x binds cluster-admin to
// its validators per run, so its launcher holds cluster-admin. From v1.1.0
// the validators run as a pre-provisioned cluster-admin ServiceAccount, and
// the launcher holds the chart's namespaced rights plus SubjectAccessReview
// create, and no RBAC write.
func setupValidationIdentity(ctx context.Context, t *testing.T) {
	t.Helper()
	launcher := []rbacv1.Subject{{Kind: "ServiceAccount", Name: validationAccount, Namespace: validationNamespace}}
	launcherRole := "cluster-admin"
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: validationNamespace, Labels: map[string]string{
			"pod-security.kubernetes.io/enforce": "privileged",
		}}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: validationAccount, Namespace: validationNamespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: validatorAccount, Namespace: validationNamespace}},
	}
	var scoped []client.Object
	if aicrjob.SupportsV11(aicrVersion) {
		launcherRole = "aicr-e2e-validation"
		scoped = []client.Object{
			&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: launcherRole, Namespace: validationNamespace}, Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"get"}},
				{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create", "get", "update", "patch", "delete"}},
				{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"create", "get", "list", "watch", "patch", "delete"}},
				{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
				{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
			}},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: launcherRole, Namespace: validationNamespace}, Subjects: launcher,
				RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: launcherRole}},
			&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: launcherRole}, Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"}},
			}},
		}
	}
	bindings := []client.Object{
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "aicr-e2e-" + validationAccount}, Subjects: launcher,
			RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: launcherRole}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "aicr-e2e-" + validatorAccount},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: validatorAccount, Namespace: validationNamespace}},
			RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}},
	}
	for _, o := range append(scoped, bindings...) {
		// A binding's role cannot change, so an earlier run's goes first.
		if err := k8s.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8s.Delete(context.WithoutCancel(ctx), o) })
	}
	for _, o := range append(append(objs, scoped...), bindings...) {
		if err := k8s.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
}

// launcherWritesRBAC reports whether the validation launcher may write any
// RBAC object or bind or escalate a role, in its namespace or cluster-wide.
func launcherWritesRBAC(ctx context.Context, t *testing.T) bool {
	t.Helper()
	account := validationNamespace + ":" + validationAccount
	for _, ns := range []string{validationNamespace, ""} {
		for _, resource := range []string{"rolebindings", "clusterrolebindings", "roles", "clusterroles"} {
			for _, verb := range []string{"create", "update", "patch", "bind", "escalate"} {
				if can(ctx, t, ns, account, verb, "rbac.authorization.k8s.io", resource) {
					t.Logf("the validation launcher may %s %s in %q", verb, resource, ns)
					return true
				}
			}
		}
	}
	return false
}

// validatableRecipe is the lane recipe with only the constraint a kind
// node's snapshot satisfies, so AICR's readiness pre-flight passes and the
// deployment checks run. It keeps the GPU Operator, without which AICR
// refuses the recipe before touching the cluster.
func validatableRecipe(ctx context.Context, t *testing.T) []byte {
	t.Helper()
	var recipe map[string]any
	if err := json.Unmarshal(resolveRecipe(ctx, t, "--platform", "kubeflow"), &recipe); err != nil {
		t.Fatal(err)
	}
	var kept []any
	for _, c := range recipe["constraints"].([]any) {
		if c.(map[string]any)["name"] == "K8s.server.version" {
			kept = append(kept, c)
		}
	}
	recipe["constraints"] = kept
	data, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// validatorPins are the pinned validator images for this AICR release.
func validatorPins(t *testing.T) map[string]string {
	t.Helper()
	pins := map[string]string{}
	for _, ref := range strings.Split(os.Getenv("AICR_OPERATOR_E2E_VALIDATOR_IMAGES"), ",") {
		if ref = strings.TrimSpace(ref); ref != "" {
			pins[evidence.Repository(ref)] = ref
		}
	}
	if len(pins) == 0 {
		t.Fatal("AICR_OPERATOR_E2E_VALIDATOR_IMAGES is required (run `make operator-e2e`)")
	}
	return pins
}

// runValidation runs a validation Job with the operator's arguments over
// run-scoped copies of a recipe and the kind snapshot, pushing to its run tag.
func runValidation(ctx context.Context, t *testing.T, mutate func(*batchv1.Job)) (*aicrjob.Request, aicrjob.Outcome) {
	t.Helper()
	setupValidationIdentity(ctx, t)
	recipe := validatableRecipe(ctx, t)
	snapshot, err := os.ReadFile("../../../pkg/operator/aicrjob/testdata/v1.0.0/snapshot-kind-no-gpu.json")
	if err != nil {
		t.Fatal(err)
	}
	if writes := launcherWritesRBAC(ctx, t); writes == aicrjob.SupportsV11(aicrVersion) {
		t.Fatalf("AICR %s: the validation launcher writes RBAC = %v", aicrVersion, writes)
	}
	r := newRequest(t, aicrjob.StageValidation)
	r.ServiceAccount = validationAccount
	r.Args = append(aicrjob.ValidationArgs(r, &aicrjob.Validation{
		AICRVersion: aicrVersion, Evidence: "oci://" + registryHost + "/evidence/cluster:run-" + r.RunID,
		PlainHTTP: true, ValidatorServiceAccount: validatorAccount,
	}), slowChecks...)
	_, outcome := runJobWith(ctx, t, r, mutate, func(job *batchv1.Job) []client.Object {
		return []client.Object{inputCopy(t, job, aicrjob.InputTo, recipe), inputCopy(t, job, aicrjob.InputSnapshot, snapshot)}
	})
	return r, outcome
}

// evidenceClient reaches the in-cluster registry through a port-forward.
func evidenceClient(ctx context.Context, t *testing.T) (*registry.Client, registry.Repository) {
	t.Helper()
	host := fmt.Sprintf("127.0.0.1:%d", forwardRegistry(ctx, t))
	c := registry.New(registry.Options{PlainHTTPHosts: []string{host}, Credentials: auth.StaticCredential(host, registryCredential(ctx, t))})
	return c, registry.Repository{Host: host, Path: "evidence/cluster"}
}

// Deployment validation on a cluster without the GPU Operator fails its
// checks, exits 8 and still pushes unsigned evidence to the run's tag. From
// AICR v1.1.0 the run's launcher holds no RBAC write, so the run creates no
// RBAC object, and the run reports the evidence digest through its
// termination message. The
// evidence names the pinned validators by tag, each tag resolves to its
// pinned digest, and the operator's binding pushed with AICR's evidence as
// its subject reads back by digest naming the plan, the operator digest and
// the AICR image digest.
func TestValidationJobPushesBindableEvidence(t *testing.T) {
	ctx := testContext(t)
	pins := validatorPins(t)
	r, outcome := runValidation(ctx, t, nil)
	t.Logf("outcome %s: %v", outcome.State, outcome.Err)
	if outcome.State != aicrjob.StateFailed || !strings.Contains(outcome.Err.Error(), "exited 8") {
		t.Fatalf("validation Job %s: %v (message %q); want failed checks, exit 8", outcome.State, outcome.Err, outcome.Message)
	}

	c, repo := evidenceClient(ctx, t)
	tag := "run-" + r.RunID
	dgst, err := c.Resolve(ctx, repo, tag)
	if err != nil {
		t.Fatalf("no evidence at %s:%s: %v", repo, tag, err)
	}
	reported, ok := aicrjob.PushedDigest(aicrVersion, outcome)
	if ok != aicrjob.SupportsV11(aicrVersion) || (ok && reported != dgst) {
		t.Fatalf("AICR %s reported %q (%v) through the termination message; the tag names %s", aicrVersion, reported, ok, dgst)
	}
	bundle, err := c.PullBundle(ctx, repo, dgst)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := evidence.ParseAICRStatement(bundle.Files[evidence.AICRStatementFile])
	if err != nil {
		t.Fatalf("ParseAICRStatement() = %v (files %v)", err, len(bundle.Files))
	}
	if err = aicrjob.CheckArtifactVersion(aicrVersion, summary.AICRVersion); err != nil || summary.Passed() || summary.Phases[aicrjob.ValidationPhase].Failed == 0 {
		t.Fatalf("summary = %+v (%v)", summary, err)
	}
	if err = evidence.CheckValidators(summary.ValidatorImages, pins); err != nil {
		t.Fatal(err)
	}
	upstream := registry.New(registry.Options{})
	for _, pin := range pins {
		ref, pinned, _ := strings.Cut(pin, "@")
		pinRepo, rerr := registry.ParseRepository(evidence.Repository(pin))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if got, rerr := upstream.Resolve(ctx, pinRepo, strings.TrimPrefix(ref, evidence.Repository(pin)+":")); rerr != nil || got != pinned {
			t.Fatalf("%s resolves to %s (%v), pinned %s", ref, got, rerr, pinned)
		}
	}
	t.Logf("evidence %s:%s = %s: %s %+v", repo, tag, dgst, summary.RecipeName, summary.Phases)

	operatorDigest := "sha256:" + strings.Repeat("0", 63) + "1"
	statement, err := evidence.NewBinding(repo.String()+"@"+dgst, dgst, summary, evidence.Predicate{
		RuntimeUID: runtimeUID, RunID: r.RunID, PlanDigest: "sha256:" + strings.Repeat("ab", 32),
		Operator: evidence.Image{Image: "ko.local/aicr-operator:e2e", Digest: operatorDigest},
		AICR:     evidence.Image{Image: aicrImage, Digest: aicrImage[strings.Index(aicrImage, "@")+1:], Version: aicrVersion},
		Approval: evidence.Approval{State: "NotRequired"},
	})
	if err != nil {
		t.Fatal(err)
	}
	subject, err := c.Describe(ctx, repo, dgst)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := c.PushStatement(ctx, repo, tag+"-binding", evidence.BindingArtifactType, statement, subject)
	if err != nil {
		t.Fatalf("PushStatement() = %v", err)
	}
	if again, perr := c.PushStatement(ctx, repo, tag+"-binding", evidence.BindingArtifactType, statement, subject); perr != nil || again != bound {
		t.Fatalf("pushing the same binding again = %s, %v", again, perr)
	}
	data, manifest, err := c.PullStatement(ctx, repo, bound)
	if err != nil {
		t.Fatal(err)
	}
	b, err := evidence.ParseBinding(data)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Subject == nil || manifest.Subject.Digest.String() != dgst || b.Predicate.PlanDigest != "sha256:"+strings.Repeat("ab", 32) ||
		b.Predicate.Operator.Digest != operatorDigest || !strings.HasSuffix(aicrImage, b.Predicate.AICR.Digest) || b.Predicate.Result != "Failed" {

		t.Fatalf("binding %s = %s (subject %+v)", bound, data, manifest.Subject)
	}
	t.Logf("binding %s@%s", repo, bound)
}

// AICR refuses to attest a validator image that resolves to a mutable tag
// unless --allow-mutable-validator-tags, which the operator never passes: a
// run whose validators resolve to :latest runs the same failing check as
// above, but exits 2 (INVALID_REQUEST) instead of 8 and pushes no evidence.
func TestValidationRefusesMutableValidatorTags(t *testing.T) {
	ctx := testContext(t)
	r, outcome := runValidation(ctx, t, func(job *batchv1.Job) {
		c := &job.Spec.Template.Spec.Containers[0]
		c.Env = append(c.Env, corev1.EnvVar{Name: "AICR_VALIDATOR_IMAGE_TAG", Value: "latest"})
	})
	t.Logf("outcome %s: %v", outcome.State, outcome.Err)
	if outcome.State != aicrjob.StateFailed || operrors.CodeOf(outcome.Err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("validation Job %s: %v (message %q); want INVALID_REQUEST", outcome.State, outcome.Err, outcome.Message)
	}
	c, repo := evidenceClient(ctx, t)
	if d, err := c.Resolve(ctx, repo, "run-"+r.RunID); operrors.CodeOf(err) != operrors.ErrCodeNotFound {
		t.Fatalf("evidence pushed for a mutable validator: %s %v", d, err)
	}
}
