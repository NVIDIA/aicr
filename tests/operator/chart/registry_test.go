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

package chart

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opregistry "github.com/NVIDIA/aicr/pkg/operator/registry"
)

const (
	inClusterRegistry = "oci://aicr-registry." + operatorNS + ".svc.cluster.local:5000/aicr"
	loginSecret       = "aicr-registry-login"
	credentialsSecret = "aicr-registry-auth"
	repoCredsSecret   = "aicr-registry"
)

// renderInCluster renders the chart with the in-cluster registry.
func renderInCluster(ctx context.Context, t *testing.T, set ...string) []unstructured.Unstructured {
	t.Helper()
	objs, out, err := renderFiles(ctx, operatorNS, []string{valuesFile, inClusterValuesFile}, set...)
	if err != nil {
		t.Fatal(out)
	}
	return objs
}

// decode converts a rendered object into a typed one.
func decode(t *testing.T, u *unstructured.Unstructured, into any) {
	t.Helper()
	raw, err := u.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

// login is the in-cluster registry's credentials as the chart rendered or
// installed them.
type login struct{ username, password, htpasswd string }

// checkCredentials requires every Secret the chart derives from the login to
// carry it: the htpasswd entry verifies the password, the docker config
// Secrets authenticate to the registry host the operator is given, and the
// Argo CD repo-creds use the same login over plain HTTP.
func checkCredentials(t *testing.T, l login, dockerConfigs map[string][]byte, repoCreds map[string]string) {
	t.Helper()
	user, hash, ok := strings.Cut(strings.TrimSpace(l.htpasswd), ":")
	if !ok || user != l.username || bcrypt.CompareHashAndPassword([]byte(hash), []byte(l.password)) != nil {
		t.Errorf("the htpasswd entry %q does not verify the login of %s", l.htpasswd, l.username)
	}
	repo, err := opregistry.ParseRepository(inClusterRegistry)
	if err != nil {
		t.Fatal(err)
	}
	for ns, cfg := range dockerConfigs {
		creds, credErr := opregistry.DockerConfigCredentials(cfg)
		if credErr != nil {
			t.Fatalf("%s/%s: %v", ns, credentialsSecret, credErr)
		}
		got, credErr := creds(context.Background(), repo.Host)
		if credErr != nil || got.Username != l.username || got.Password != l.password {
			t.Errorf("%s/%s does not authenticate %s with the login", ns, credentialsSecret, repo.Host)
		}
	}
	want := map[string]string{
		"type": "oci", "url": inClusterRegistry, "username": l.username, "password": l.password, "insecureOCIForceHttp": "true",
	}
	for k, v := range want {
		if repoCreds[k] != v {
			t.Errorf("repo-creds %s = %q, want %q", k, repoCreds[k], v)
		}
	}
}

// The in-cluster registry: the operator gets the static provider over plain
// HTTP to the chart's registry, every credential derives from one generated
// login, and only the operator, the pushing Jobs and Argo CD's repo server
// reach it.
func TestInClusterRegistryRender(t *testing.T) {
	ctx := ctxFor(t)
	objs := renderInCluster(ctx, t)

	args := deploymentArgs(t, objs)
	for _, want := range []string{
		"--registry-repository=" + inClusterRegistry, "--registry-plain-http=true",
		"--registry-auth-provider=static", "--registry-credentials-secret=" + credentialsSecret,
	} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s in %v", want, args)
		}
	}

	secret := func(ns, name string) map[string][]byte {
		for i := range objs {
			if objs[i].GetKind() == "Secret" && objs[i].GetNamespace() == ns && objs[i].GetName() == name {
				s := &corev1.Secret{}
				decode(t, &objs[i], s)
				return s.Data
			}
		}
		t.Fatalf("no Secret %s/%s rendered", ns, name)
		return nil
	}
	data := secret(operatorNS, loginSecret)
	l := login{string(data["username"]), string(data["password"]), string(data["htpasswd"])}
	if len(l.password) < 32 {
		t.Errorf("the generated password has %d characters", len(l.password))
	}
	configs := map[string][]byte{}
	for _, ns := range []string{operatorNS, jobsNS, validationNS} {
		configs[ns] = secret(ns, credentialsSecret)[corev1.DockerConfigJsonKey]
	}
	repoCreds := map[string]string{}
	for k, v := range secret(engineNS, repoCredsSecret) {
		repoCreds[k] = string(v)
	}
	checkCredentials(t, l, configs, repoCreds)
	if label := find(t, objs, "Secret", repoCredsSecret).GetLabels()["argocd.argoproj.io/secret-type"]; label != "repo-creds" {
		t.Errorf("the repo-creds Secret is labeled %q", label)
	}

	project := find(t, objs, "AppProject", "aicr")
	sources, _, _ := unstructured.NestedStringSlice(project.Object, "spec", "sourceRepos")
	if !slices.Equal(sources, []string{inClusterRegistry + "/charts/*", inClusterRegistry + "/wrappers/*"}) {
		t.Errorf("the project's sources are %v", sources)
	}

	policy := &networkingv1.NetworkPolicy{}
	decode(t, find(t, objs, "NetworkPolicy", "aicr-registry"), policy)
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 3 {
		t.Fatalf("the registry admits %+v", policy.Spec.Ingress)
	}
	from := policy.Spec.Ingress[0].From
	if from[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "aicr-operator" || from[0].NamespaceSelector != nil {
		t.Errorf("the first peer is %+v, want the operator's pods", from[0])
	}
	namespaces := from[1].NamespaceSelector.MatchExpressions[0].Values
	sort.Strings(namespaces)
	if !slices.Equal(namespaces, []string{jobsNS, validationNS}) {
		t.Errorf("the registry admits the namespaces %v, want the pushing Jobs'", namespaces)
	}
	if from[2].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != engineNS ||
		from[2].PodSelector.MatchLabels["app.kubernetes.io/name"] != "argocd-repo-server" {

		t.Errorf("the third peer is %+v, want Argo CD's repo server", from[2])
	}

	// No ECR token handling: the operator cannot write the repo-creds.
	for i := range objs {
		if objs[i].GetKind() == "Role" && objs[i].GetNamespace() == engineNS &&
			strings.Contains(string(must(objs[i].MarshalJSON())), `"`+repoCredsSecret+`"`) {

			t.Errorf("the operator's %s Role names the repo-creds Secret", engineNS)
		}
	}

	// Everything that keeps the registry serving survives uninstall; the
	// operator's and the Jobs' copies of the login do not.
	var kept []string
	for _, o := range objs {
		if o.GetAnnotations()["helm.sh/resource-policy"] == "keep" && strings.Contains(o.GetName(), "registry") {
			kept = append(kept, o.GetKind()+"/"+o.GetNamespace()+"/"+o.GetName())
		}
	}
	sort.Strings(kept)
	wantKept := []string{
		"ConfigMap/" + operatorNS + "/aicr-registry", "Deployment/" + operatorNS + "/aicr-registry",
		"NetworkPolicy/" + operatorNS + "/aicr-registry", "PersistentVolumeClaim/" + operatorNS + "/aicr-registry",
		"Secret/" + engineNS + "/" + repoCredsSecret, "Secret/" + operatorNS + "/" + loginSecret,
		"Service/" + operatorNS + "/aicr-registry",
	}
	sort.Strings(wantKept)
	if !slices.Equal(kept, wantKept) {
		t.Errorf("kept on uninstall %v\nwant %v", kept, wantKept)
	}

	// The registry runs on the operator's nodes but takes none of its pod
	// (anti-)affinity, which selects operator pods.
	objs = renderInCluster(ctx, t, "operator.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=pool",
		"operator.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=Exists",
		"operator.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].topologyKey=kubernetes.io/hostname")
	registryPod := &appsv1.Deployment{}
	decode(t, find(t, objs, "Deployment", "aicr-registry"), registryPod)
	if a := registryPod.Spec.Template.Spec.Affinity; a == nil || a.NodeAffinity == nil || a.PodAntiAffinity != nil || a.PodAffinity != nil {
		t.Errorf("the registry's affinity is %+v, want the operator's node affinity only", a)
	}

	// Without validation the registry admits only the Job namespace.
	objs = renderInCluster(ctx, t, "validation.enabled=false")
	decode(t, find(t, objs, "NetworkPolicy", "aicr-registry"), policy)
	if values := policy.Spec.Ingress[0].From[1].NamespaceSelector.MatchExpressions[0].Values; !slices.Equal(values, []string{jobsNS}) {
		t.Errorf("without validation the registry admits %v", values)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// installedLogin reads the installed login and every Secret derived from it.
func installedLogin(ctx context.Context, t *testing.T) (login, map[string][]byte, map[string]string) {
	t.Helper()
	get := func(ns, name string) map[string][]byte {
		s := &corev1.Secret{}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, s); err != nil {
			t.Fatalf("Secret %s/%s: %v", ns, name, err)
		}
		return s.Data
	}
	data := get(operatorNS, loginSecret)
	configs := map[string][]byte{}
	for _, ns := range []string{operatorNS, jobsNS, validationNS} {
		configs[ns] = get(ns, credentialsSecret)[corev1.DockerConfigJsonKey]
	}
	repoCreds := map[string]string{}
	for k, v := range get(engineNS, repoCredsSecret) {
		repoCreds[k] = string(v)
	}
	return login{string(data["username"]), string(data["password"]), string(data["htpasswd"])}, configs, repoCreds
}

// An installation on an external registry refuses an upgrade to the
// in-cluster one. The in-cluster registry installs under the operator
// namespace's restricted profile, keeps its login across helm upgrade without
// restarting, and keeps serving through uninstall with the same login, so a
// reinstall adopts it.
func TestInClusterRegistryInstall(t *testing.T) {
	ctx := ctxFor(t)
	ensureNamespace(ctx, t, operatorNS, map[string]string{"pod-security.kubernetes.io/enforce": "restricted"})
	ensureNamespace(ctx, t, engineNS, nil)
	files := []string{"-f", valuesFile, "-f", inClusterValuesFile}
	// An installation on an external registry refuses an upgrade that would
	// switch it, whatever the values say.
	if out, err := helm(ctx, "upgrade", "--install", release, chartDir, "--namespace", operatorNS, "-f", valuesFile); err != nil {
		t.Fatalf("helm upgrade --install: %v\n%s", err, out)
	}
	if out, err := helm(ctx, append([]string{"upgrade", release, chartDir, "--namespace", operatorNS}, files...)...); err == nil ||
		!strings.Contains(out, "switching an installation's registry is not supported") {

		t.Fatalf("switching the registry on upgrade: %v\n%s", err, out)
	}
	// Moving to the in-cluster registry is a new installation: its kept
	// project names the old registry.
	if out, err := helm(ctx, "uninstall", release, "--namespace", operatorNS); err != nil {
		t.Fatalf("helm uninstall: %v\n%s", err, out)
	}
	project := &unstructured.Unstructured{}
	project.SetGroupVersionKind(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "AppProject"})
	project.SetNamespace(engineNS)
	project.SetName("aicr")
	if err := admin.Delete(ctx, project); err != nil {
		t.Fatal(err)
	}
	if out, err := helm(ctx, append([]string{"install", release, chartDir, "--namespace", operatorNS}, files...)...); err != nil {
		t.Fatalf("helm install: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = helm(context.WithoutCancel(ctx), "uninstall", release, "--namespace", operatorNS, "--ignore-not-found")
	})
	installed, configs, repoCreds := installedLogin(ctx, t)
	checkCredentials(t, installed, configs, repoCreds)

	deployment := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: "aicr-registry"}, deployment); err != nil {
		t.Fatal(err)
	}
	if err := admitted(ctx, t, operatorNS, deployment.Spec.Template); err != nil {
		t.Errorf("the registry pod is not restricted: %v", err)
	}
	checksum := deployment.Spec.Template.Annotations["checksum/login"]

	if out, err := helm(ctx, append([]string{"upgrade", release, chartDir, "--namespace", operatorNS}, files...)...); err != nil {
		t.Fatalf("helm upgrade: %v\n%s", err, out)
	}
	upgraded, configs, repoCreds := installedLogin(ctx, t)
	if upgraded != installed {
		t.Error("helm upgrade changed the registry login")
	}
	checkCredentials(t, upgraded, configs, repoCreds)
	if err := admin.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Annotations["checksum/login"] != checksum {
		t.Error("helm upgrade restarts the registry")
	}

	if out, err := helm(ctx, "uninstall", release, "--namespace", operatorNS); err != nil {
		t.Fatalf("helm uninstall: %v\n%s", err, out)
	}
	present := func(obj client.Object) bool {
		err := admin.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		return err == nil
	}
	meta := func(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
	// The registry keeps serving the stack's charts to Argo CD.
	for _, kept := range []client.Object{
		&corev1.Secret{ObjectMeta: meta(operatorNS, loginSecret)},
		&corev1.PersistentVolumeClaim{ObjectMeta: meta(operatorNS, "aicr-registry")},
		&corev1.Secret{ObjectMeta: meta(engineNS, repoCredsSecret)},
		&appsv1.Deployment{ObjectMeta: meta(operatorNS, "aicr-registry")},
		&corev1.Service{ObjectMeta: meta(operatorNS, "aicr-registry")},
		&corev1.ConfigMap{ObjectMeta: meta(operatorNS, "aicr-registry")},
		&networkingv1.NetworkPolicy{ObjectMeta: meta(operatorNS, "aicr-registry")},
	} {
		if !present(kept) {
			t.Errorf("uninstall removed %T %s", kept, kept.GetName())
		}
	}
	for _, gone := range []client.Object{
		&corev1.Secret{ObjectMeta: meta(operatorNS, credentialsSecret)},
		&corev1.Secret{ObjectMeta: meta(jobsNS, credentialsSecret)},
		&corev1.Secret{ObjectMeta: meta(validationNS, credentialsSecret)},
	} {
		if present(gone) {
			t.Errorf("uninstall left %T %s", gone, gone.GetName())
		}
	}

	if out, err := helm(ctx, append([]string{"install", release, chartDir, "--namespace", operatorNS}, files...)...); err != nil {
		t.Fatalf("reinstall: %v\n%s", err, out)
	}
	reinstalled, configs, repoCreds := installedLogin(ctx, t)
	if reinstalled != installed {
		t.Error("a reinstall generated a new registry login")
	}
	checkCredentials(t, reinstalled, configs, repoCreds)
}
