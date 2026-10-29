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

// Package e2e runs the pinned AICR image as operator-built Jobs on a real
// cluster: it confirms the cm:// inputs and outputs, Job-owned run
// objects surviving AICR's writes and going away with the Job, the snapshot
// identities, and that the image runs under the restricted security context.
// Run with `make operator-e2e`.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"oras.land/oras-go/v2/registry/remote/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/discovery"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
	"github.com/NVIDIA/aicr/pkg/operator/resolver"
)

const (
	jobsNamespace  = "aicr-jobs"
	serviceAccount = "aicr-jobs"
	runtimeUID     = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	registryHost   = "registry.aicr-registry.svc.cluster.local:5000"
	jobTimeout     = 5 * time.Minute
)

var (
	// registryAuth is the docker config for the kind cluster's registry,
	// which requires a password (tools/operator-kind).
	registryAuth  []byte
	restConfig    *rest.Config
	k8s           client.Client
	clientset     kubernetes.Interface
	aicrImage     string
	aicrVersion   string
	laneArgs      = []string{"--service", "eks", "--accelerator", "h100", "--os", "ubuntu", "--intent", "training"}
	namespaceSpec = aicrjob.Namespaces{Jobs: jobsNamespace, Discovery: "aicr-discovery", Validation: "aicr-validation"}
)

// TestMain needs an explicit kubeconfig: the suite never falls back to an
// ambient cluster. Without one it is skipped; with OPERATOR_E2E_REQUIRED set
// (CI), a missing cluster fails.
func TestMain(m *testing.M) {
	path := os.Getenv("AICR_OPERATOR_E2E_KUBECONFIG")
	aicrImage = os.Getenv("AICR_OPERATOR_E2E_IMAGE")
	aicrVersion = os.Getenv("AICR_OPERATOR_E2E_AICR_VERSION")
	if authPath := os.Getenv("AICR_OPERATOR_E2E_REGISTRY_AUTH"); authPath != "" {
		data, err := os.ReadFile(authPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read registry credentials: %v\n", err)
			os.Exit(1)
		}
		registryAuth = data
	}
	if path == "" || aicrImage == "" || aicrVersion == "" {
		msg := "AICR_OPERATOR_E2E_KUBECONFIG, AICR_OPERATOR_E2E_IMAGE and AICR_OPERATOR_E2E_AICR_VERSION are required (run `make operator-e2e`)"
		if os.Getenv("OPERATOR_E2E_REQUIRED") != "" {
			fmt.Fprintln(os.Stderr, msg)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "skipping e2e: "+msg)
		os.Exit(0)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	restConfig = cfg
	if err != nil {
		fmt.Fprintf(os.Stderr, "load kubeconfig %s: %v\n", path, err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "register scheme: %v\n", err)
		os.Exit(1)
	}
	if clientset, err = kubernetes.NewForConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "create clientset: %v\n", err)
		os.Exit(1)
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintf(os.Stderr, "create client: %v\n", err)
		os.Exit(1)
	}
	if err = setupJobIdentity(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "set up job namespace: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// setupJobIdentity creates the Job namespace and the identity the chart gives
// AICR Jobs there: run-scoped ConfigMaps in their own namespace, nothing else.
func setupJobIdentity(ctx context.Context) error {
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: jobsNamespace, Labels: map[string]string{
			"pod-security.kubernetes.io/enforce": "restricted",
		}}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccount, Namespace: jobsNamespace}},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: serviceAccount, Namespace: jobsNamespace},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"configmaps"},
				Verbs: []string{"get", "list", "create", "update", "patch"},
			}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: serviceAccount, Namespace: jobsNamespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: serviceAccount},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: serviceAccount, Namespace: jobsNamespace}},
		},
	}
	for _, o := range objs {
		if err := k8s.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*jobTimeout)
	t.Cleanup(cancel)
	return ctx
}

func newRequest(t *testing.T, stage aicrjob.Stage) *aicrjob.Request {
	t.Helper()
	runID, err := aicrjob.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	r := &aicrjob.Request{
		Stage: stage, RunID: runID, RuntimeName: "cluster", RuntimeUID: runtimeUID,
		Image: aicrImage, Namespaces: namespaceSpec, ServiceAccount: serviceAccount,
	}
	if stage.Pushes() {
		r.CredentialSecret = "aicr-registry-" + runID
	}
	return r
}

// runJob creates the suspended Job, then its Job-owned run objects (output
// ConfigMap, credential Secret and any inputs), resumes it, waits for it to
// finish and returns its outcome.
func runJob(ctx context.Context, t *testing.T, r *aicrjob.Request,
	inputs func(*batchv1.Job) []client.Object) (*batchv1.Job, aicrjob.Outcome) {

	t.Helper()
	return runJobWith(ctx, t, r, nil, inputs)
}

// runJobWith is runJob with the built Job changed by mutate before it is
// created, for tests that put AICR in a state the operator never does.
func runJobWith(ctx context.Context, t *testing.T, r *aicrjob.Request, mutate func(*batchv1.Job),
	inputs func(*batchv1.Job) []client.Object) (*batchv1.Job, aicrjob.Outcome) {

	t.Helper()
	job, err := aicrjob.Build(r)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if mutate != nil {
		mutate(job)
	}
	if err = k8s.Create(ctx, job); err != nil {
		t.Fatalf("create Job: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(context.WithoutCancel(ctx), job, client.PropagationPolicy(aicrjob.DeletePropagation))
	})
	var objs []client.Object
	if r.Stage.WritesOutput() {
		out, oerr := aicrjob.Output(job)
		if oerr != nil {
			t.Fatalf("Output() = %v", oerr)
		}
		objs = append(objs, out)
	}
	if r.Stage.Pushes() {
		secret, serr := aicrjob.CredentialSecret(job, r.CredentialSecret, registryAuth)
		if serr != nil {
			t.Fatalf("CredentialSecret() = %v", serr)
		}
		objs = append(objs, secret)
	}
	if inputs != nil {
		objs = append(objs, inputs(job)...)
	}
	for _, o := range objs {
		if err = k8s.Create(ctx, o); err != nil {
			t.Fatalf("create %T %s: %v", o, o.GetName(), err)
		}
	}
	patch := client.MergeFrom(job.DeepCopy())
	job.Spec.Suspend = new(bool)
	if err = k8s.Patch(ctx, job, patch); err != nil {
		t.Fatalf("resume Job: %v", err)
	}

	deadline := time.Now().Add(jobTimeout)
	for time.Now().Before(deadline) {
		if err = k8s.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
			t.Fatalf("get Job: %v", err)
		}
		var pod *corev1.Pod
		pods := &corev1.PodList{}
		if err = k8s.List(ctx, pods, client.InNamespace(job.Namespace),
			client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name}); err == nil && len(pods.Items) > 0 {
			pod = &pods.Items[0]
		}
		outcome, oerr := aicrjob.ObserveOutcome(r.Stage, r.RunID, job, pod)
		if oerr != nil {
			t.Fatalf("ObserveOutcome() = %v", oerr)
		}
		if outcome.State == aicrjob.StateFailed {
			if pod != nil {
				logPod(ctx, t, pod)
			}
			logEvents(ctx, t, job.Namespace)
		}
		if outcome.State != aicrjob.StateRunning {
			return job, outcome
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Job %s did not finish within %s", job.Name, jobTimeout)
	return nil, aicrjob.Outcome{}
}

// logPod records the tail of a failed AICR pod's log for diagnosis.
func logPod(ctx context.Context, t *testing.T, pod *corev1.Pod) {
	t.Helper()
	tail := int64(40)
	raw, err := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx)
	if err != nil {
		t.Logf("logs for %s unavailable: %v", pod.Name, err)
		return
	}
	t.Logf("logs for failed pod %s:\n%s", pod.Name, raw)
}

// can reports whether a ServiceAccount, named namespace:name, may perform a
// verb on a resource, cluster-wide when namespace is empty.
func can(ctx context.Context, t *testing.T, namespace, account, verb, group, resource string) bool {
	t.Helper()
	review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:               "system:serviceaccount:" + account,
		Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:" + strings.SplitN(account, ":", 2)[0], "system:authenticated"},
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: verb, Group: group, Resource: resource},
	}}
	got, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("SubjectAccessReview for %s: %v", account, err)
	}
	return got.Status.Allowed
}

// logEvents records the namespace's recent events: AICR deletes its own agent
// Jobs on failure, so events are what remains of their pods.
func logEvents(ctx context.Context, t *testing.T, namespace string) {
	t.Helper()
	events, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Logf("events in %s unavailable: %v", namespace, err)
		return
	}
	for i := range events.Items {
		e := &events.Items[i]
		t.Logf("event %s %s/%s %s: %s", e.Type, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, e.Message)
	}
}

// readOutput returns the document AICR wrote into a Job's output ConfigMap,
// after checking that AICR's server-side apply kept the Job's ownership.
func readOutput(ctx context.Context, t *testing.T, job *batchv1.Job) []byte {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: job.Name}, cm); err != nil {
		t.Fatalf("get output ConfigMap %s: %v", job.Name, err)
	}
	if refs := cm.OwnerReferences; len(refs) != 1 || refs[0].UID != job.UID {
		t.Fatalf("AICR's write dropped the Job ownership of %s: %+v", cm.Name, refs)
	}
	payload, err := aicrjob.ReadOutput(cm)
	if err != nil {
		t.Fatalf("ReadOutput(%s) = %v (keys %v)", cm.Name, err, keys(cm.Data))
	}
	t.Logf("output %s: keys %v, labels %v", cm.Name, keys(cm.Data), cm.Labels)
	return payload
}

// resolveRegistryDigest resolves a tag in the in-cluster registry to its
// manifest digest, through a port-forward to the registry pod.
func resolveRegistryDigest(ctx context.Context, t *testing.T, repo, tag string) string {
	t.Helper()
	port := forwardRegistry(ctx, t)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		fmt.Sprintf("http://127.0.0.1:%d/v2/%s/manifests/%s", port, repo, tag), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json")
	if c := registryCredential(ctx, t); c.Username != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD manifest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD %s:%s = %s", repo, tag, resp.Status)
	}
	return resp.Header.Get("Docker-Content-Digest")
}

// registryCredential is the in-cluster registry's credential.
func registryCredential(ctx context.Context, t *testing.T) auth.Credential {
	t.Helper()
	creds, err := registry.DockerConfigCredentials(registryAuth)
	if err != nil {
		t.Fatal(err)
	}
	c, err := creds(ctx, registryHost)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// forwardRegistry forwards a local port to the in-cluster registry for the
// test's life and returns it.
func forwardRegistry(ctx context.Context, t *testing.T) uint16 {
	t.Helper()
	pods := &corev1.PodList{}
	if err := k8s.List(ctx, pods, client.InNamespace("aicr-registry"), client.MatchingLabels{"app": "registry"}); err != nil || len(pods.Items) == 0 {
		t.Fatalf("find registry pod: %v (%d pods)", err, len(pods.Items))
	}
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		t.Fatalf("port-forward transport: %v", err)
	}
	url := clientset.CoreV1().RESTClient().Post().Resource("pods").
		Namespace("aicr-registry").Name(pods.Items[0].Name).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
	stop, ready := make(chan struct{}), make(chan struct{})
	fw, err := portforward.New(dialer, []string{"0:5000"}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	t.Cleanup(func() { close(stop) })
	go func() { _ = fw.ForwardPorts() }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("port-forward did not become ready")
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		t.Fatalf("port-forward ports: %v", err)
	}
	return ports[0].Local
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// resolveRecipe runs a recipe Job and returns the RecipeResult it wrote.
func resolveRecipe(ctx context.Context, t *testing.T, extra ...string) []byte {
	t.Helper()
	return recipeFor(ctx, t, append(append([]string{}, laneArgs...), extra...)...)
}

// recipeFor runs a recipe Job for the given criteria flags.
func recipeFor(ctx context.Context, t *testing.T, criteria ...string) []byte {
	t.Helper()
	r := newRequest(t, aicrjob.StageRecipe)
	r.Args = append(append([]string{}, criteria...), "--format", "json", "--output", aicrjob.OutputURI(r))
	job, outcome := runJob(ctx, t, r, nil)
	if outcome.State != aicrjob.StateSucceeded {
		t.Fatalf("recipe Job %s: %v (message %q)", outcome.State, outcome.Err, outcome.Message)
	}
	return readOutput(ctx, t, job)
}

// The recipe Job runs as non-root on a read-only root file system and writes
// the RecipeResult into its Job-owned output ConfigMap; the operator's parser
// accepts it.
func TestRecipeJobWritesConfigMap(t *testing.T) {
	ctx := testContext(t)
	r, err := aicrjob.ParseRecipeResult(resolveRecipe(ctx, t, "--platform", "kubeflow"), aicrVersion)
	if err != nil {
		t.Fatalf("ParseRecipeResult(cm://) = %v", err)
	}
	if len(r.ComponentRefs) == 0 || r.Criteria.Platform != "kubeflow" {
		t.Fatalf("recipe = %d components, criteria %+v", len(r.ComponentRefs), *r.Criteria)
	}
}

// The recipe Job resolves from a run-scoped snapshot copy in its own
// namespace. Declared criteria override the values AICR detects, but AICR
// still evaluates overlay constraints against the snapshot's measurements: the
// lane criteria that resolve without a snapshot fail against a kind node's
// snapshot, as a permanent invalid-request error.
func TestRecipeJobEvaluatesSnapshotConstraints(t *testing.T) {
	ctx := testContext(t)
	snapshot, err := os.ReadFile("../../../pkg/operator/aicrjob/testdata/v1.0.0/snapshot-kind-no-gpu.json")
	if err != nil {
		t.Fatal(err)
	}
	r := newRequest(t, aicrjob.StageRecipe)
	r.Args = append(append([]string{"--snapshot", aicrjob.InputURI(r, aicrjob.InputSnapshot)}, laneArgs...),
		"--platform", "kubeflow", "--format", "json", "--output", aicrjob.OutputURI(r))
	_, outcome := runJob(ctx, t, r, func(job *batchv1.Job) []client.Object {
		return []client.Object{inputCopy(t, job, aicrjob.InputSnapshot, snapshot)}
	})
	if outcome.State != aicrjob.StateFailed || operrors.CodeOf(outcome.Err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("recipe Job %s: %v, want a failed run with INVALID_REQUEST", outcome.State, outcome.Err)
	}
	if !strings.Contains(outcome.Message, "nodes=1") {
		t.Errorf("message %q does not show the snapshot's criteria", outcome.Message)
	}
}

// snapshotFixture reads an edited v1.0.0 snapshot fixture as a snapshot of
// the AICR release under test: AICR's Snapshot schema is the same in every
// fixture version, so only the release stamp the version gate checks differs.
func snapshotFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../pkg/operator/aicrjob/testdata/v1.0.0/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var snap map[string]any
	if err = json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	snap["metadata"].(map[string]any)["version"] = strings.TrimPrefix(aicrVersion, "v")
	if data, err = json.Marshal(snap); err != nil {
		t.Fatal(err)
	}
	return data
}

// The operator's recipe arguments over a snapshot state its derived
// dimensions: an Amazon Linux EKS node then fails with AICR's
// INVALID_REQUEST instead of a relaxed generic recipe, and a pre-driver node
// resolves the lane from its PCI-detected accelerator.
func TestRecipeJobFromSnapshotFixtures(t *testing.T) {
	ctx := testContext(t)
	tests := []struct {
		fixture  string
		declared *v1alpha1.DeclaredCriteria
		rejected string
	}{
		{"snapshot-eks-amazonlinux.json", &v1alpha1.DeclaredCriteria{Accelerator: "h100"}, "no recipe provides os 'amazonlinux'"},
		{"snapshot-eks-pre-driver.json", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			data := snapshotFixture(t, tt.fixture)
			snap, err := aicrjob.ParseSnapshot(data, aicrVersion)
			if err != nil {
				t.Fatal(err)
			}
			derived, err := discovery.CheckSnapshot(snap, tt.declared, nil)
			if err != nil {
				t.Fatalf("CheckSnapshot() = %v", err)
			}
			spec := &v1alpha1.ClusterRuntimeSpec{Intent: "training", Platform: "kubeflow", Criteria: tt.declared}
			r := newRequest(t, aicrjob.StageRecipe)
			r.Args = resolver.RecipeArgs(spec, derived, aicrjob.InputURI(r, aicrjob.InputSnapshot), aicrjob.OutputURI(r))
			job, outcome := runJob(ctx, t, r, func(job *batchv1.Job) []client.Object {
				return []client.Object{inputCopy(t, job, aicrjob.InputSnapshot, data)}
			})
			if tt.rejected != "" {
				if outcome.State != aicrjob.StateFailed || operrors.CodeOf(outcome.Err) != operrors.ErrCodeInvalidRequest ||
					!strings.Contains(outcome.Err.Error(), tt.rejected) {

					t.Fatalf("recipe Job %s: %v; want INVALID_REQUEST with %q", outcome.State, outcome.Err, tt.rejected)
				}
				return
			}
			if outcome.State != aicrjob.StateSucceeded {
				t.Fatalf("recipe Job %s: %v", outcome.State, outcome.Err)
			}
			result, err := aicrjob.ParseRecipeResult(readOutput(ctx, t, job), aicrVersion)
			if err != nil || result.Criteria == nil || result.Criteria.Accelerator != "h100" || result.Criteria.Service != "eks" {
				t.Fatalf("recipe = %+v, %v", result, err)
			}
		})
	}
}

// The render Job pushes the bundle to a run-scoped tag. AICR v1.1.0 and
// later also write the pushed digest to the termination message
// (--image-refs), and it names what the tag does; v1.0.x cannot write there,
// because it renames a temporary file onto the target, so its digest is
// resolved from the registry. The Job's credential Secret
// is owned by the Job and garbage-collected with it.
func TestRenderJobPushesRunScopedTag(t *testing.T) {
	ctx := testContext(t)
	recipe := resolveRecipe(ctx, t, "--platform", "kubeflow")
	r := newRequest(t, aicrjob.StageRender)
	tag := "run-" + r.RunID
	r.Args = []string{
		"--recipe", aicrjob.InputURI(r, aicrjob.InputTo),
		"--deployer", "helm", "--readiness-hooks",
		"--output", "oci://" + registryHost + "/bundles/cluster:" + tag,
		"--plain-http",
	}
	r.Args = append(r.Args, aicrjob.ImageRefsArgs(aicrVersion)...)
	job, outcome := runJob(ctx, t, r, func(job *batchv1.Job) []client.Object {
		return []client.Object{inputCopy(t, job, aicrjob.InputTo, recipe)}
	})
	if outcome.State != aicrjob.StateSucceeded {
		t.Fatalf("render Job %s: %v (message %q)", outcome.State, outcome.Err, outcome.Message)
	}
	digest := resolveRegistryDigest(ctx, t, "bundles/cluster", tag)
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		t.Fatalf("registry digest for tag %s = %q", tag, digest)
	}
	reported, ok := aicrjob.PushedDigest(aicrVersion, outcome)
	if ok != aicrjob.SupportsV11(aicrVersion) || (ok && reported != digest) {
		t.Fatalf("AICR %s reported %q (%v) through the termination message; the tag names %s", aicrVersion, reported, ok, digest)
	}
	t.Logf("bundle bundles/cluster:%s pinned as %s (reported: %v)", tag, digest, ok)

	if err := k8s.Delete(ctx, job, client.PropagationPolicy(aicrjob.DeletePropagation)); err != nil {
		t.Fatalf("delete Job: %v", err)
	}
	owned := []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: jobsNamespace, Name: r.CredentialSecret}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: jobsNamespace, Name: aicrjob.InputName(r.Stage, r.RunID, aicrjob.InputTo)}},
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		pods := &corev1.PodList{}
		if err := k8s.List(ctx, pods, client.InNamespace(jobsNamespace),
			client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name}); err != nil {
			t.Fatalf("list pods: %v", err)
		}
		remaining := len(pods.Items)
		for _, o := range owned {
			if err := k8s.Get(ctx, client.ObjectKeyFromObject(o), o); !apierrors.IsNotFound(err) {
				remaining++
			}
		}
		if remaining == 0 {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("deleting the Job left its pods or run-scoped objects behind")
}

// AICR's render gates reject overrides that break driver ownership, touch
// profile-locked paths, target disabled components or enable components the
// recipe lacks, with exit category INVALID_REQUEST. A misspelled path renders
// silently, which is why the operator checks paths against a base render.
// Render arguments come from the operator's own builder.
func TestRenderJobRejectsOverrides(t *testing.T) {
	ctx := testContext(t)
	eks := resolveRecipe(ctx, t, "--platform", "kubeflow")
	gke := recipeFor(ctx, t, "--service", "gke", "--accelerator", "h100", "--os", "cos", "--intent", "training")
	set := func(component, path, value string) v1alpha1.Override {
		return v1alpha1.Override{Component: component, Path: path, Value: apiextensionsv1.JSON{Raw: []byte(value)}}
	}
	tests := []struct {
		name      string
		recipe    []byte
		overrides []v1alpha1.Override
		// rejected is a fragment of AICR's error; empty when the render succeeds.
		rejected string
	}{
		{"driver ownership", eks, []v1alpha1.Override{set("gpu-operator", "driver.enabled", "false")},
			"nvidia-dra-driver-gpu still reads nvidiaDriverRoot"},
		{"profile-locked path", gke, []v1alpha1.Override{set("nvsentinel", "labeler.assumeDriverInstalled", "false")},
			"profile-owned path nvsentinel.labeler.assumeDriverInstalled diverged from selected profile gpuStack="},
		{"disabled component", eks, []v1alpha1.Override{set("nvsentinel", "enabled", "false"), set("nvsentinel", "labeler.logLevel", `"debug"`)},
			`component "nvsentinel" is not in the generated bundle`},
		{"enabling a component the recipe lacks", eks, []v1alpha1.Override{set("network-operator", "enabled", "true")},
			`unknown component "network-operator"`},
		{"misspelled path", eks, []v1alpha1.Override{set("cert-manager", "replicaCnt", "2")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &v1alpha1.ClusterRuntimeSpec{
				Placement: v1alpha1.Placement{
					System:      v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{"pool": "system"}},
					Accelerated: v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{"pool": "gpu"}},
				},
				Overrides: tt.overrides,
			}
			r := newRequest(t, aicrjob.StageRender)
			args, err := resolver.RenderArgs(spec, aicrjob.InputURI(r, aicrjob.InputTo),
				"oci://"+registryHost+"/bundles/overrides:run-"+r.RunID, true)
			if err != nil {
				t.Fatalf("RenderArgs() = %v", err)
			}
			r.Args = args
			_, outcome := runJob(ctx, t, r, func(job *batchv1.Job) []client.Object {
				return []client.Object{inputCopy(t, job, aicrjob.InputTo, tt.recipe)}
			})
			if tt.rejected == "" {
				if outcome.State != aicrjob.StateSucceeded {
					t.Fatalf("render Job %s: %v", outcome.State, outcome.Err)
				}
				return
			}
			if outcome.State != aicrjob.StateFailed || operrors.CodeOf(outcome.Err) != operrors.ErrCodeInvalidRequest ||
				!strings.Contains(outcome.Err.Error(), tt.rejected) {

				t.Fatalf("render Job %s: %v; want INVALID_REQUEST with %q", outcome.State, outcome.Err, tt.rejected)
			}
			msg := outcome.Err.Error()
			t.Logf("rejected: %s", msg[strings.LastIndex(msg, "\n")+1:])
		})
	}
}

func inputCopy(t *testing.T, job *batchv1.Job, input aicrjob.Input, document []byte) client.Object {
	t.Helper()
	cm, err := aicrjob.InputCopy(job, input, document)
	if err != nil {
		t.Fatalf("InputCopy() = %v", err)
	}
	return cm
}

// upgrade-check reads the run-scoped recipe copies, writes its report to its
// output ConfigMap and, with --fail-on-error=false, succeeds even when
// components need attention.
func TestAssessmentJobReadsRunScopedCopies(t *testing.T) {
	ctx := testContext(t)
	from := resolveRecipe(ctx, t)
	to := resolveRecipe(ctx, t, "--platform", "kubeflow")
	r := newRequest(t, aicrjob.StageAssessment)
	r.Args = aicrjob.AssessmentArgs(r, "argocd")
	job, outcome := runJob(ctx, t, r, func(job *batchv1.Job) []client.Object {
		return []client.Object{inputCopy(t, job, aicrjob.InputFrom, from), inputCopy(t, job, aicrjob.InputTo, to)}
	})
	if outcome.State != aicrjob.StateSucceeded {
		t.Fatalf("assessment Job %s: %v (message %q)", outcome.State, outcome.Err, outcome.Message)
	}
	parsed, err := aicrjob.ParseUpgradeReport(readOutput(ctx, t, job), "argocd")
	if err != nil {
		t.Fatalf("ParseUpgradeReport(cm://) = %v", err)
	}
	if len(parsed.Components) == 0 {
		t.Fatal("report lists no component changes between training and training+kubeflow")
	}
}

const (
	discoveryNamespace = "aicr-discovery"
	snapshotAccount    = "aicr-snapshot"
	agentAccount       = "aicr-snapshot-agent"
)

// setupDiscoveryIdentity provisions the snapshot identities the chart
// creates: the snapshot Job's own ServiceAccount, limited to its
// namespace, and the pre-provisioned agent ServiceAccount AICR uses in
// exact-ServiceAccount mode, with the rules `aicr snapshot
// --add-roles-to-service-account` renders at v1.0.0. AICR creates no RBAC.
// The agent is privileged, so the namespace admits privileged pods.
//
// AICR v1.0.x creates the agent namespace before checking that it exists, and
// RBAC answers Forbidden before AlreadyExists, so the snapshot Job needs
// cluster-scoped namespace create even for this pre-existing namespace (it
// then labels the namespace managed-by=aicr). Read and patch are limited to
// this namespace; create cannot be limited by name. From v1.1.0, in
// exact-ServiceAccount mode, AICR sends no namespace request, and the grant
// is removed.
func setupDiscoveryIdentity(ctx context.Context) error {
	ns := discoveryNamespace
	agentClusterRole := "aicr-agent-" + ns + "." + agentAccount
	namespaceRole := snapshotAccount + "-namespace"
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{
			"pod-security.kubernetes.io/enforce": "privileged",
		}}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: snapshotAccount, Namespace: ns}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: agentAccount, Namespace: ns}},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: snapshotAccount, Namespace: ns},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"get"}},
				{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"create", "get", "list", "watch", "delete"}},
				{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
				{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
				{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "create", "update", "patch"}},
			},
		},
		roleBinding(ns, snapshotAccount, "Role", snapshotAccount),
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: namespaceRole},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"create"}},
				{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "patch"}, ResourceNames: []string{ns}},
			},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: namespaceRole},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: namespaceRole},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: snapshotAccount, Namespace: ns}},
		},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: agentAccount, Namespace: ns},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create", "get", "update", "patch"}},
				{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
			},
		},
		roleBinding(ns, agentAccount, "Role", agentAccount),
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: agentClusterRole},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"nodes", "pods"}, Verbs: []string{"get", "list"}},
				{APIGroups: []string{"nvidia.com"}, Resources: []string{"clusterpolicies"}, Verbs: []string{"get", "list"}},
				{APIGroups: []string{"slinky.slurm.net"}, Resources: []string{"controllers", "nodesets", "loginsets", "restapis", "accountings"}, Verbs: []string{"list"}},
				{APIGroups: []string{"k8s.mariadb.com"}, Resources: []string{"mariadbs"}, Verbs: []string{"list"}},
				{APIGroups: []string{"apps"}, Resources: []string{"daemonsets"}, Verbs: []string{"get", "list"}},
			},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: agentClusterRole},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: agentClusterRole},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: agentAccount, Namespace: ns}},
		},
	}
	for _, o := range objs {
		grant := o.GetName() == namespaceRole
		if grant && aicrjob.SupportsV11(aicrVersion) {
			if err := k8s.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			continue
		}
		if err := k8s.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func roleBinding(namespace, name, kind, role string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: kind, Name: role},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: namespace}},
	}
}

// The snapshot Job runs AICR in exact-ServiceAccount mode: its agent runs as
// the pre-provisioned ServiceAccount on the selected node, and the result
// lands in the Job-owned output ConfigMap. The Snapshot parser accepts it.
// From AICR v1.1.0 the launcher holds no namespace verbs, and the snapshot
// leaves the existing namespace's labels as they were.
func TestSnapshotJobWritesConfigMap(t *testing.T) {
	ctx := testContext(t)
	if err := setupDiscoveryIdentity(ctx); err != nil {
		t.Fatalf("set up discovery identity: %v", err)
	}
	launcher := discoveryNamespace + ":" + snapshotAccount
	if want := !aicrjob.SupportsV11(aicrVersion); can(ctx, t, "", launcher, "create", "", "namespaces") != want {
		t.Fatalf("AICR %s: snapshot launcher namespace create allowed = %v, want %v", aicrVersion, !want, want)
	}
	ns := &corev1.Namespace{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: discoveryNamespace}, ns); err != nil {
		t.Fatal(err)
	}
	labels := ns.Labels
	nodes := &corev1.NodeList{}
	if err := k8s.List(ctx, nodes); err != nil || len(nodes.Items) == 0 {
		t.Fatalf("list nodes: %v", err)
	}
	r := newRequest(t, aicrjob.StageSnapshot)
	r.ServiceAccount = snapshotAccount
	r.Args = []string{
		"--namespace", discoveryNamespace,
		"--service-account-name", agentAccount,
		"--image", aicrImage,
		"--node-selector", "kubernetes.io/hostname=" + nodes.Items[0].Name,
		// The agent's defaults (1 CPU, 4Gi memory, 2Gi ephemeral storage) are
		// sized for GPU nodes, not a CI runner's single kind node.
		"--requests", "cpu=250m,memory=512Mi,ephemeral-storage=512Mi",
		"--timeout", "4m",
		"--format", "json", "--output", aicrjob.OutputURI(r),
	}
	job, outcome := runJob(ctx, t, r, nil)
	if outcome.State != aicrjob.StateSucceeded {
		t.Fatalf("snapshot Job %s: %v (message %q)", outcome.State, outcome.Err, outcome.Message)
	}
	payload := readOutput(ctx, t, job)
	if dir := os.Getenv("AICR_OPERATOR_E2E_ARTIFACTS"); dir != "" {
		if err := os.WriteFile(dir+"/snapshot.json", payload, 0o600); err != nil {
			t.Logf("save snapshot: %v", err)
		}
	}
	snap, err := aicrjob.ParseSnapshot(payload, aicrVersion)
	if err != nil {
		t.Fatalf("ParseSnapshot(cm://) = %v", err)
	}
	if len(snap.Measurements) == 0 {
		t.Fatal("snapshot holds no measurements")
	}
	if err = k8s.Get(ctx, client.ObjectKey{Name: discoveryNamespace}, ns); err != nil {
		t.Fatal(err)
	}
	if aicrjob.SupportsV11(aicrVersion) && !maps.Equal(ns.Labels, labels) {
		t.Fatalf("the snapshot relabeled %s: %v, was %v", discoveryNamespace, ns.Labels, labels)
	}
}

// With no operator running, the TTL backstop removes a finished Job with its
// pods and its run-scoped objects. The test shortens the 24-hour TTL of a
// finished Job; Kubernetes allows that field to change.
func TestFinishedJobTTLRemovesRunObjects(t *testing.T) {
	ctx := testContext(t)
	snapshot, err := os.ReadFile("../../../pkg/operator/aicrjob/testdata/v1.0.0/snapshot-kind-no-gpu.json")
	if err != nil {
		t.Fatal(err)
	}
	r := newRequest(t, aicrjob.StageRecipe)
	r.Args = append(append([]string{}, laneArgs...), "--format", "json", "--output", aicrjob.OutputURI(r))
	job, outcome := runJob(ctx, t, r, func(job *batchv1.Job) []client.Object {
		return []client.Object{inputCopy(t, job, aicrjob.InputSnapshot, snapshot)}
	})
	if outcome.State != aicrjob.StateSucceeded {
		t.Fatalf("recipe Job %s: %v (message %q)", outcome.State, outcome.Err, outcome.Message)
	}
	patch := client.MergeFrom(job.DeepCopy())
	job.Spec.TTLSecondsAfterFinished = new(int32)
	if err = k8s.Patch(ctx, job, patch); err != nil {
		t.Fatalf("shorten TTL: %v", err)
	}
	owned := []client.Object{
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: jobsNamespace, Name: job.Name}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: jobsNamespace, Name: aicrjob.OutputName(r.Stage, r.RunID)}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: jobsNamespace, Name: aicrjob.InputName(r.Stage, r.RunID, aicrjob.InputSnapshot)}},
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		pods := &corev1.PodList{}
		if err = k8s.List(ctx, pods, client.InNamespace(jobsNamespace),
			client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name}); err != nil {
			t.Fatalf("list pods: %v", err)
		}
		remaining := len(pods.Items)
		for _, o := range owned {
			if err = k8s.Get(ctx, client.ObjectKeyFromObject(o), o); !apierrors.IsNotFound(err) {
				remaining++
			}
		}
		if remaining == 0 {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("the TTL left the Job, its pods or its run-scoped objects behind")
}
