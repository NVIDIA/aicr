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
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"oras.land/oras-go/v2/registry/remote/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// registryCallTimeout bounds one request to the in-cluster registry.
const registryCallTimeout = 30 * time.Second

// registryRestart replaces the in-cluster registry pod while a step is in
// flight: the replacement mounts the same volume, serves every chart and the
// bundle the plan names at its digest with the same login, and the rollout
// finishes with no intervention.
func registryRestart(ctx context.Context, t *testing.T, active string) string {
	t.Helper()
	digest, ro := nextPlan(ctx, t, active, "15m")
	refs := planArtifacts(ctx, t, digest)
	login := registryLogin(ctx, t)
	approve(ctx, t, ro.Name)
	waitStepStarted(ctx, t, ro.Name)
	before := readyRegistryPod(ctx, t)
	if before == nil {
		t.Fatal("no Ready registry pod")
	}
	claim := registryClaim(ctx, t, before)
	if err := k8s.Get(ctx, client.ObjectKey{Name: ro.Name}, ro); err != nil {
		t.Fatal(err)
	}
	if ro.Status.Current == nil {
		t.Fatalf("%s finished its step before the registry pod was deleted", ro.Name)
	}
	killPod(ctx, t, before)
	var after *corev1.Pod
	waitFor(t, 3*time.Minute, "a replacement registry pod to be Ready", func() bool {
		after = readyRegistryPod(ctx, t)
		return after != nil && after.UID != before.UID
	})
	if got := registryClaim(ctx, t, after); got != claim {
		t.Fatalf("the replacement registry pod mounts claim %s, not %s", got, claim)
	}
	withRegistry(ctx, t, func(base string) {
		for _, ref := range refs {
			resolves(ctx, t, base, ref, login)
		}
	})
	waitReadyOn(ctx, t, digest)
	rolledOut(ctx, t, digest)
	return digest
}

// rollbackToInstalled downgrades the operator with helm rollback to the
// installed candidate's release. The older operator runs by digest, reads every
// plan and rollout the newer one wrote without stalling, rewrites none of them
// and writes nothing to the engine; upgrading again returns to the next
// candidate.
func rollbackToInstalled(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	plans := planVersions(ctx, t)
	rollouts := rolloutGenerations(ctx, t)
	before := syncs(ctx, t)
	t.Setenv("CANDIDATE_DIR", filepath.Dir(os.Getenv("AICR_OPERATOR_QUALIFY_CANDIDATE")))
	run(ctx, t, "tools/operator-qualify", "rollback")
	runsCandidate(ctx, t, &candidate)
	// Long enough for the older operator to resolve and reconcile every
	// rollout once.
	time.Sleep(time.Minute)
	if c := cond(getRuntime(ctx, t), v1alpha1.ConditionStalled); c.Status == metav1.ConditionTrue {
		t.Fatalf("the older operator stalled: %s: %s", c.Reason, c.Message)
	}
	for name, rv := range planVersions(ctx, t) {
		if was, ok := plans[name]; ok && was != rv {
			t.Errorf("the older operator rewrote plan %s", name)
		}
	}
	for name, gen := range rolloutGenerations(ctx, t) {
		if was, ok := rollouts[name]; ok && was != gen {
			t.Errorf("the older operator changed the spec of rollout %s", name)
		}
	}
	if written(ctx, t) != active || syncs(ctx, t) != before {
		t.Fatalf("the older operator wrote the Application: plan %s, %d syncs (from %d)", written(ctx, t), syncs(ctx, t), before)
	}
	t.Setenv("CANDIDATE_NEXT_DIR", filepath.Dir(os.Getenv("AICR_OPERATOR_QUALIFY_NEXT")))
	run(ctx, t, "tools/operator-qualify", "upgrade")
	runsCandidate(ctx, t, &next)
	waitReadyOn(ctx, t, active)
}

// keptResource is an object docs/operator/lifecycle.md says helm uninstall
// keeps or deletes.
type keptResource struct {
	gvk             schema.GroupVersionKind
	namespace, name string
}

var (
	gvkCRD = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
	gvkVAP = schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicy"}
	gvkVAB = schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicyBinding"}
	gvkNS  = schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	core   = func(kind string) schema.GroupVersionKind { return schema.GroupVersionKind{Version: "v1", Kind: kind} }
)

// keptByUninstall names the objects lifecycle.md's table lists as kept, for
// the qualification values (the in-cluster registry on); every namespace the
// release created, the validation scenario's included, is checked as well.
var keptByUninstall = []keptResource{
	{gvkCRD, "", "clusterruntimes.operator.aicr.run"},
	{gvkCRD, "", "runtimerollouts.operator.aicr.run"},
	{gvkNS, "", operatorNamespace},
	{gvkNS, "", "aicr-jobs"},
	{gvkNS, "", "aicr-discovery"},
	{gvkNS, "", release},
	{schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "AppProject"}, "argocd", "aicr"},
	{core("Secret"), "argocd", "aicr-registry"},
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, operatorNamespace, "aicr-registry"},
	{core("Service"), operatorNamespace, "aicr-registry"},
	{core("ConfigMap"), operatorNamespace, "aicr-registry"},
	{schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}, operatorNamespace, "aicr-registry"},
	{core("Secret"), operatorNamespace, "aicr-registry-login"},
	{core("PersistentVolumeClaim"), operatorNamespace, "aicr-registry"},
	{gvkVAP, "", "aicr-operator-state"},
	{gvkVAB, "", "aicr-operator-state"},
	{gvkVAP, "", "aicr-operator-approval"},
	{gvkVAB, "", "aicr-operator-approval"},
}

// deletedByUninstall names the objects the same table lists as deleted: the
// operator, its identity and grants, the registry login's copies and the
// policies that constrain the operator.
var deletedByUninstall = []keptResource{
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, operatorNamespace, "aicr-operator"},
	{core("ServiceAccount"), operatorNamespace, "aicr-operator"},
	{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, "", "aicr-operator"},
	{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"}, "", "aicr-operator"},
	{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"}, operatorNamespace, "aicr-operator"},
	{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}, operatorNamespace, "aicr-operator"},
	{core("Secret"), operatorNamespace, "aicr-registry-auth"},
	{core("Secret"), "aicr-jobs", "aicr-registry-auth"},
	{gvkVAP, "", "aicr-operator-applications"},
	{gvkVAP, "", "aicr-operator-application-deletes"},
	{gvkVAP, "", "aicr-operator-jobs"},
	{gvkVAP, "", "aicr-operator-rollout-outcome"},
	{gvkVAP, "", "aicr-operator-runtime"},
}

// documentedUninstall uninstalls in the order docs/operator/lifecycle.md
// gives. Deleting the runtime with the default Orphan policy annotates the
// Applications, which keep the workload healthy; helm uninstall then keeps
// every namespace the release created and what the page lists as kept,
// deletes what it lists as deleted, every plan and rollout survives, and the
// kept registry still serves the active plan.
func documentedUninstall(ctx context.Context, t *testing.T, active string) {
	t.Helper()
	rt := getRuntime(ctx, t)
	pods := workloadPods(ctx, t)
	plans := planVersions(ctx, t)
	rollouts := rolloutGenerations(ctx, t)
	// helm uninstall deletes the operator's copy of the login.
	login := registryLogin(ctx, t)
	namespaces := releaseNamespaces(ctx, t)
	if err := k8s.Delete(ctx, rt); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Minute, "the runtime to be deleted", func() bool {
		return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: rt.Name}, &v1alpha1.ClusterRuntime{}))
	})
	if got := application(ctx, t).GetAnnotations()[v1alpha1.AnnotationOrphanedFrom]; got != string(rt.UID) {
		t.Fatalf("the Application is orphaned from %q, want the deleted runtime %s", got, rt.UID)
	}
	waitFor(t, 2*time.Minute, "the orphaned Application to stay Healthy", func() bool {
		health, _, _ := unstructured.NestedString(application(ctx, t).Object, "status", "health", "status")
		return health == "Healthy"
	})
	if !slices.Equal(workloadPods(ctx, t), pods) {
		t.Fatal("deleting the runtime changed the workload")
	}

	run(ctx, t, "helm", "uninstall", "aicr-operator", "--namespace", operatorNamespace,
		"--kubeconfig", os.Getenv("AICR_OPERATOR_E2E_KUBECONFIG"), "--wait")
	for _, ns := range namespaces {
		if err := kept(ctx, keptResource{gvkNS, "", ns}); err != nil {
			t.Errorf("helm uninstall did not keep namespace %s: %v", ns, err)
		}
	}
	for _, r := range keptByUninstall {
		if err := kept(ctx, r); err != nil {
			t.Errorf("helm uninstall did not keep %s %s/%s: %v", r.gvk.Kind, r.namespace, r.name, err)
		}
	}
	for _, r := range deletedByUninstall {
		if err := getResource(ctx, r); !apierrors.IsNotFound(err) {
			t.Errorf("helm uninstall kept %s %s/%s (%v)", r.gvk.Kind, r.namespace, r.name, err)
		}
	}
	for name := range plans {
		if _, ok := planVersions(ctx, t)[name]; !ok {
			t.Errorf("plan %s did not survive the uninstall", name)
		}
	}
	for name := range rollouts {
		if _, ok := rolloutGenerations(ctx, t)[name]; !ok {
			t.Errorf("rollout %s did not survive the uninstall", name)
		}
	}
	waitFor(t, time.Minute, "the kept registry to serve", func() bool { return readyRegistryPod(ctx, t) != nil })
	withRegistry(ctx, t, func(base string) {
		for _, ref := range planArtifacts(ctx, t, active) {
			resolves(ctx, t, base, ref, login)
		}
	})
	if !slices.Equal(workloadPods(ctx, t), pods) {
		t.Fatal("helm uninstall changed the workload")
	}
}

func getResource(ctx context.Context, r keptResource) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.gvk)
	return k8s.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: r.name}, obj)
}

// kept reports an error unless r exists and is not being deleted: a claim the
// registry still mounts stays visible, terminating, after a delete.
func kept(ctx context.Context, r keptResource) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.gvk)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: r.name}, obj); err != nil {
		return err
	}
	if obj.GetDeletionTimestamp() != nil {
		return fmt.Errorf("it is being deleted since %s", obj.GetDeletionTimestamp())
	}
	return nil
}

// releaseNamespaces names the namespaces the operator's Helm release created.
func releaseNamespaces(ctx context.Context, t *testing.T) []string {
	t.Helper()
	list := &corev1.NamespaceList{}
	if err := k8s.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ns := range list.Items {
		if ns.Annotations["meta.helm.sh/release-name"] == "aicr-operator" {
			out = append(out, ns.Name)
		}
	}
	if len(out) == 0 {
		t.Fatal("the release created no namespace")
	}
	return out
}

// planVersions maps each plan ConfigMap to its resourceVersion.
func planVersions(ctx context.Context, t *testing.T) map[string]string {
	t.Helper()
	cms := &corev1.ConfigMapList{}
	if err := k8s.List(ctx, cms, client.InNamespace(operatorNamespace)); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, cm := range cms.Items {
		if strings.HasPrefix(cm.Name, "aicr-plan-") {
			out[cm.Name] = cm.ResourceVersion
		}
	}
	return out
}

// rolloutGenerations maps each rollout to its generation, which only a spec
// change moves.
func rolloutGenerations(ctx context.Context, t *testing.T) map[string]int64 {
	t.Helper()
	list := &v1alpha1.RuntimeRolloutList{}
	if err := k8s.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]int64, len(list.Items))
	for _, ro := range list.Items {
		out[ro.Name] = ro.Generation
	}
	return out
}

// artifact is a manifest in the in-cluster registry: a repository path and a
// digest.
type artifact struct{ host, path, digest string }

// planArtifacts lists every chart and the bundle a plan names.
func planArtifacts(ctx context.Context, t *testing.T, digest string) []artifact {
	t.Helper()
	doc := readPlan(ctx, t, digest)
	uris := make([]string, 0, len(doc.Releases)+1)
	for _, r := range doc.Releases {
		uris = append(uris, r.Chart.Repository+"@"+r.Chart.Digest)
	}
	if doc.Provenance.Bundle != "" {
		uris = append(uris, doc.Provenance.Bundle)
	}
	out := make([]artifact, 0, len(uris))
	for _, uri := range uris {
		repo, dgst, ok := strings.Cut(uri, "@")
		if !ok {
			t.Fatalf("plan %s names %s, not pinned by digest", digest, uri)
		}
		r, err := registry.ParseRepository(repo)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, artifact{host: r.Host, path: r.Path, digest: dgst})
	}
	if len(out) == 0 {
		t.Fatalf("plan %s names no artifact", digest)
	}
	return out
}

// readyRegistryPod is the newest Ready, live registry pod, or nil.
func readyRegistryPod(ctx context.Context, t *testing.T) *corev1.Pod {
	t.Helper()
	pods := &corev1.PodList{}
	if err := k8s.List(ctx, pods, client.InNamespace(operatorNamespace), client.MatchingLabels{"app.kubernetes.io/name": "aicr-registry"}); err != nil {
		t.Fatal(err)
	}
	var newest *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning || !podReady(p) {
			continue
		}
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	return newest
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// registryClaim is the UID of the claim the registry pod stores its content on.
func registryClaim(ctx context.Context, t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: v.PersistentVolumeClaim.ClaimName}, pvc); err != nil {
			t.Fatal(err)
		}
		return string(pvc.UID)
	}
	t.Fatalf("registry pod %s mounts no claim", pod.Name)
	return ""
}

// withRegistry runs f with the base URL of the in-cluster registry, reached
// through a port-forward to its Ready pod, which its NetworkPolicy does not
// apply to.
func withRegistry(ctx context.Context, t *testing.T, f func(base string)) {
	t.Helper()
	pod := readyRegistryPod(ctx, t)
	if pod == nil {
		t.Fatal("no Ready registry pod")
	}
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	url := clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(operatorNamespace).Name(pod.Name).SubResource("portforward").URL()
	stop, ready := make(chan struct{}), make(chan struct{})
	defer close(stop)
	fw, err := portforward.New(spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url),
		[]string{"0:5000"}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	go func() { failed <- fw.ForwardPorts() }()
	select {
	case <-ready:
	case err = <-failed:
		t.Fatalf("port-forward to %s: %v", pod.Name, err)
	case <-time.After(registryCallTimeout):
		t.Fatalf("port-forward to %s did not become ready in %s", pod.Name, registryCallTimeout)
	case <-ctx.Done():
		t.Fatal("port-forward did not become ready")
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		t.Fatalf("port-forward: %v", err)
	}
	f(fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local))
}

// registryLogin is the operator's registry login, from its docker config
// Secret, by registry host.
func registryLogin(ctx context.Context, t *testing.T) auth.CredentialFunc {
	t.Helper()
	secret := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: "aicr-registry-auth"}, secret); err != nil {
		t.Fatal(err)
	}
	creds, err := registry.DockerConfigCredentials(secret.Data[corev1.DockerConfigJsonKey])
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

// resolves requires the registry at base to serve a's manifest by digest with
// login.
func resolves(ctx context.Context, t *testing.T, base string, a artifact, login auth.CredentialFunc) {
	t.Helper()
	cred, err := login(ctx, a.host)
	if err != nil {
		t.Fatal(err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, registryCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, fmt.Sprintf("%s/v2/%s/manifests/%s", base, a.path, a.digest), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(cred.Username, cred.Password)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json",
	}, ", "))
	resp, err := (&http.Client{Timeout: registryCallTimeout}).Do(req)
	if err != nil {
		t.Fatalf("HEAD %s@%s: %v", a.path, a.digest, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the registry answers %s for %s@%s", resp.Status, a.path, a.digest)
	}
}
