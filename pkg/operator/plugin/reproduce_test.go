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

package plugin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

const (
	testNS    = "aicr-system"
	testImage = "ghcr.io/nvidia/aicr:v1.0.0@sha256:e91ba6f4916a5bfcd19d05933b5b4d07a4dd05a738a1ffebfab243a32cbff433"
)

var testFlags = []string{"--deployer", "helm", "--readiness-hooks", "--accelerated-node-selector", "pool=gpu",
	"--accelerated-node-toleration", "*", "--set-json", `gpu-operator:driver.env=[{"name":"A B"}]`}

// testPlan returns a resolved plan document, its ConfigMaps and the
// recipe's, with mutate applied to the document before it is sealed.
func testPlan(t *testing.T, recipeJSON string, mutate func(*plan.Document)) (*plan.Document, []client.Object) {
	t.Helper()
	recipe := plan.RecipeContent([]byte(recipeJSON))
	values := plan.ValuesContent("cert-manager", []byte(`{}`))
	doc := &plan.Document{
		SchemaVersion: plan.SchemaVersion, RuntimeUID: "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0", Engine: plan.EngineArgoCD, EnginePolicy: "argocd.v1",
		Provenance: plan.Provenance{
			Source: plan.SourceResolved, AICRImage: testImage, RenderFlags: testFlags,
			Bundle: "oci://registry.example.com/aicr/cluster/bundles/cluster@sha256:" + strings.Repeat("b", 64),
		},
		Recipe: &plan.Recipe{Name: "h100-eks-ubuntu-training-kubeflow", ConfigMapRef: recipe.Ref()},
		Releases: []plan.Release{{
			ID: "cert-manager", Component: "cert-manager", Phase: plan.PhasePrimary, TargetNamespace: "cert-manager",
			Chart: plan.Chart{
				Source: plan.ChartWrapper, Repository: "oci://registry.example.com/aicr/cluster/wrappers/cert-manager",
				Version: "1.0.0", Digest: "sha256:" + strings.Repeat("c", 64),
			},
			Values: values.Ref(),
		}},
	}
	if mutate != nil {
		mutate(doc)
	}
	digest, err := plan.ComputeDesiredStateDigest(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc.DesiredStateDigest = digest
	planCM, err := plan.ToConfigMap(doc, testNS, v1alpha1.SingletonName)
	if err != nil {
		t.Fatal(err)
	}
	recipeCM, err := recipe.ConfigMap(testNS, v1alpha1.SingletonName, "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0", nil)
	if err != nil {
		t.Fatal(err)
	}
	return doc, []client.Object{planCM, recipeCM}
}

func testReader(t *testing.T, status v1alpha1.ClusterRuntimeStatus, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	rt := &v1alpha1.ClusterRuntime{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName}, Status: status}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, rt)...).Build()
}

// reproduce writes the plan's own recipe and prints the commands that render
// it with the plan's AICR image and flags, quoted for a shell, and compare
// the result with the operator's bundle.
func TestReproduce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const recipeJSON = `{"kind":"RecipeResult","componentRefs":[]}`
	doc, objs := testPlan(t, recipeJSON, nil)
	c := testReader(t, v1alpha1.ClusterRuntimeStatus{CandidatePlanDigest: doc.DesiredStateDigest}, objs...)
	dir := filepath.Join(t.TempDir(), "check")

	var out bytes.Buffer
	if err := reproduce(ctx, c, testNS, "", dir, &out); err != nil {
		t.Fatalf("reproduce() = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, RecipeFile)); err != nil || string(got) != recipeJSON {
		t.Fatalf("recipe.json = %q, %v; want the plan's recipe", got, err)
	}
	name, _ := plan.Name(doc.DesiredStateDigest)
	for _, want := range []string{
		"Plan " + name + " (" + doc.DesiredStateDigest + ")",
		`docker run --rm --user "$(id -u):$(id -g)" -v ` + dir + ":/work -w /work " + testImage + " bundle --recipe recipe.json " +
			`--deployer helm --readiness-hooks --accelerated-node-selector pool=gpu --accelerated-node-toleration '*' ` +
			`--set-json 'gpu-operator:driver.env=[{"name":"A B"}]' --output cli-bundle`,
		"oras pull registry.example.com/aicr/cluster/bundles/cluster@sha256:" + strings.Repeat("b", 64) + " -o " + filepath.Join(dir, "operator-bundle"),
		"diff -r " + filepath.Join(dir, "operator-bundle") + " " + filepath.Join(dir, CLIBundle),
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	r, err := LoadReproduction(ctx, c, testNS, doc.DesiredStateDigest)
	if err != nil || r.Plan != name || !slices.Equal(r.Flags, testFlags) {
		t.Fatalf("LoadReproduction(digest) = %+v, %v", r, err)
	}
	if args := r.ContainerArgs("/d"); !slices.Equal(args[:8], []string{"-v", "/d:/work", "-w", "/work", testImage, "bundle", "--recipe", RecipeFile}) ||
		!slices.Equal(args[len(args)-2:], []string{"--output", CLIBundle}) {

		t.Fatalf("ContainerArgs() = %v", args)
	}
}

// A bundle in the chart's in-cluster registry is pulled through a
// port-forward with the login the chart generated, which nothing stores;
// another registry gets the generic instruction.
func TestReproduceInClusterRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	digest := "@sha256:" + strings.Repeat("b", 64)
	reproduceWith := func(t *testing.T, bundle string) string {
		t.Helper()
		doc, objs := testPlan(t, `{"kind":"RecipeResult","componentRefs":[]}`, func(d *plan.Document) {
			d.Provenance.Bundle = "oci://" + bundle
		})
		c := testReader(t, v1alpha1.ClusterRuntimeStatus{CandidatePlanDigest: doc.DesiredStateDigest}, objs...)
		var out bytes.Buffer
		if err := reproduce(ctx, c, testNS, "", filepath.Join(t.TempDir(), "check"), &out); err != nil {
			t.Fatalf("reproduce() = %v", err)
		}
		return out.String()
	}

	out := reproduceWith(t, "aicr-registry.aicr-system.svc.cluster.local:5000/aicr/bundles/cluster"+digest)
	for _, want := range []string{
		"get on Secret aicr-registry-login and\ncreate on pods/portforward in aicr-system",
		"kubectl -n aicr-system port-forward service/aicr-registry 15000:5000 &",
		"kubectl -n aicr-system get secret aicr-registry-login -o jsonpath='{.data.password}' | base64 -d |",
		`oras pull --plain-http -u "$(kubectl -n aicr-system get secret aicr-registry-login -o jsonpath='{.data.username}' | base64 -d)" ` +
			"--password-stdin localhost:15000/aicr/bundles/cluster" + digest,
		"kill %1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "oras login") {
		t.Errorf("output stores the in-cluster login:\n%s", out)
	}

	for name, bundle := range map[string]string{
		"a registry Service of another namespace": "aicr-registry.registry.svc.cluster.local:5000/aicr/bundles/cluster" + digest,
		"another registry Service":                "zot.aicr-system.svc:5000/aicr/bundles/cluster" + digest,
	} {
		out = reproduceWith(t, bundle)
		if strings.Contains(out, inClusterLogin) || strings.Contains(out, "--plain-http") ||
			!strings.Contains(out, "log in to the registry first") || !strings.Contains(out, "oras pull "+bundle) {

			t.Errorf("%s: output is not the generic instruction:\n%s", name, out)
		}
	}
}

func TestChartRegistryPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, ref string
		port      string
		ok        bool
	}{
		{"chart registry", "aicr-registry.aicr-system.svc.cluster.local:5000/aicr/bundles/x@sha256:aa", "5000", true},
		{"other cluster domain", "aicr-registry.aicr-system.svc.corp.example:5000/x", "5000", true},
		{"short Service name", "aicr-registry.aicr-system.svc:5000/x", "5000", true},
		{"another namespace", "aicr-registry.other.svc.cluster.local:5000/x", "", false},
		{"another Service", "registry.aicr-system.svc.cluster.local:5000/x", "", false},
		{"no port", "aicr-registry.aicr-system.svc.cluster.local/x", "", false},
		{"ECR", "123456789012.dkr.ecr.us-west-2.amazonaws.com/aicr/cluster/bundles/x", "", false},
		{"short name", "aicr-registry:5000/x", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			port, ok := chartRegistryPort(tt.ref, testNS)
			if port != tt.port || ok != tt.ok {
				t.Errorf("chartRegistryPort(%q) = %q, %t; want %q, %t", tt.ref, port, ok, tt.port, tt.ok)
			}
		})
	}
}

// The active plan wins over the candidate, and a plan that cannot be
// rendered again says why.
func TestLoadReproductionChoosesAndRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	active, activeObjs := testPlan(t, `{"kind":"RecipeResult","n":1}`, nil)
	candidate, candidateObjs := testPlan(t, `{"kind":"RecipeResult","n":2}`, func(d *plan.Document) { d.Releases[0].TargetNamespace = "other" })
	c := testReader(t, v1alpha1.ClusterRuntimeStatus{ActivePlanDigest: active.DesiredStateDigest, CandidatePlanDigest: candidate.DesiredStateDigest},
		append(activeObjs, candidateObjs...)...)
	if r, err := LoadReproduction(ctx, c, testNS, ""); err != nil || r.Digest != active.DesiredStateDigest {
		t.Fatalf("LoadReproduction() = %+v, %v; want the active plan", r, err)
	}
	if _, err := LoadReproduction(ctx, testReader(t, v1alpha1.ClusterRuntimeStatus{}), testNS, ""); err == nil {
		t.Fatal("a runtime without a plan reproduced one")
	}

	_, older := testPlan(t, `{"kind":"RecipeResult"}`, func(d *plan.Document) { d.Provenance.RenderFlags = nil })
	_, rebuilt := testPlan(t, `{"kind":"RecipeResult"}`, func(d *plan.Document) {
		d.Provenance, d.Recipe = plan.Provenance{Source: plan.SourceRebuilt}, nil
	})
	tamperedDoc, tampered := testPlan(t, `{"kind":"RecipeResult","n":3}`, func(d *plan.Document) { d.Releases[0].TargetNamespace = "third" })
	for _, o := range tampered {
		if cm := o.(*corev1.ConfigMap); cm.Name == tamperedDoc.Recipe.ConfigMapRef.ConfigMap {
			cm.Data[plan.RecipeDataKey] = `{"kind":"RecipeResult","n":4}`
		}
	}
	for want, objs := range map[string][]client.Object{
		"does not record its render": older, "rebuilt from engine objects": rebuilt, "not the digest it records": tampered,
	} {
		ref := objs[0].GetName()
		if _, err := LoadReproduction(ctx, testReader(t, v1alpha1.ClusterRuntimeStatus{}, objs...), testNS, ref); err == nil ||
			!strings.Contains(err.Error(), want) {

			t.Errorf("LoadReproduction(%s) = %v, want %q", ref, err, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"pool=gpu": "pool=gpu", "*": "'*'", "it's": `'it'\''s'`, `a:b=[{"x":1}]`: `'a:b=[{"x":1}]'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
