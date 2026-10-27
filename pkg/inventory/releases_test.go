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

package inventory

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// fluxRec is a Helm record as helm-controller stores it: in the HelmRelease's
// namespace, installed into another.
func fluxRec(name, installNamespace string, annotations map[string]string) installedRelease {
	r := helmRec(name, "flux-system", "deployed", "v25.3.3", annotations)
	r.TargetNamespace = installNamespace

	return r
}

func TestAttributeReleases(t *testing.T) {
	comps := []Component{fixtureGPUOperator, fixtureCertManager}
	helmNamed := []installedRelease{
		helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "v25.3.3", nil),
		helmRec("gpu-operator-pre", "nvidia-gpu-operator", "deployed", "0.1.0", stamp("v25.3.3")),
		helmRec("cert-manager", "cert-manager-system", "uninstalled", "v1.20.2", nil),
		helmRec("prometheus", "monitoring", "deployed", "70.1.0", nil),
		helmRec("rdma-netns-exclusive", "nodewright", "deployed", "0.1.0", stamp("v0.23.0")),
	}
	helmWant := Releases{
		Components: map[string][]Record{
			"gpu-operator": {
				{Source: "helm", Name: "gpu-operator", Namespace: "nvidia-gpu-operator",
					InstallNamespace: "nvidia-gpu-operator", Status: "deployed"},
				{Source: "helm", Name: "gpu-operator-pre", Namespace: "nvidia-gpu-operator",
					InstallNamespace: "nvidia-gpu-operator", Status: "deployed", Injected: true},
			},
			"cert-manager": {
				{Source: "helm", Name: "cert-manager", Namespace: "cert-manager-system",
					InstallNamespace: "cert-manager-system", Status: "uninstalled"},
			},
		},
		Stamped: []Record{
			{Source: "helm", Name: "rdma-netns-exclusive", Namespace: "nodewright",
				InstallNamespace: "nodewright", Status: "deployed"},
		},
	}

	type deployerCase struct {
		name     string
		deployer Deployer
		records  []installedRelease
		want     Releases
	}
	tests := make([]deployerCase, 0, len(allDeployers))
	for _, d := range []Deployer{DeployerHelm, DeployerHelmfile, DeployerTerraform, DeployerFleet} {
		tests = append(tests, deployerCase{string(d) + " installs under the component name", d, helmNamed, helmWant})
	}
	tests = append(tests,
		deployerCase{
			name: "flux composes the install namespace into the name", deployer: DeployerFlux,
			records: []installedRelease{
				fluxRec("nvidia-gpu-operator-gpu-operator", "nvidia-gpu-operator", nil),
				fluxRec("nvidia-gpu-operator-gpu-operator-post", "nvidia-gpu-operator", stamp("v25.3.3")),
				// A namespace the caller did not name is recovered from the
				// payload's own target.
				fluxRec("moved-cert-manager", "moved", nil),
				// Under flux a bare component name is no component's, and its
				// stamp makes it a finding.
				helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "0.1.0", stamp("v25.3.3")),
			},
			want: Releases{
				Components: map[string][]Record{
					"gpu-operator": {
						{Source: "helm", Name: "nvidia-gpu-operator-gpu-operator", Namespace: "flux-system",
							InstallNamespace: "nvidia-gpu-operator", Status: "deployed"},
						{Source: "helm", Name: "nvidia-gpu-operator-gpu-operator-post", Namespace: "flux-system",
							InstallNamespace: "nvidia-gpu-operator", Status: "deployed", Injected: true},
					},
					"cert-manager": {
						{Source: "helm", Name: "moved-cert-manager", Namespace: "flux-system",
							InstallNamespace: "moved", Status: "deployed"},
					},
				},
				Stamped: []Record{
					{Source: "helm", Name: "gpu-operator", Namespace: "nvidia-gpu-operator",
						InstallNamespace: "nvidia-gpu-operator", Status: "deployed"},
				},
			},
		},
	)
	argoRecords := []installedRelease{
		argoRec("cert-manager", "elsewhere", "v1.20.2", nil),
		argoRec("teamfoo-gpu-operator", "nvidia-gpu-operator", "v25.3.3", nil),
		argoRec("teamfoo-gpu-operator-readiness", "nvidia-gpu-operator", "", nil),
		// A prefixed name deploying outside the component's namespaces is
		// unplaced, which is too loose to attribute.
		argoRec("teambar-gpu-operator", "team-bar", "v25.3.3", nil),
	}
	argoWant := Releases{Components: map[string][]Record{
		"cert-manager": {
			{Source: "argocd", Name: "cert-manager", Namespace: "elsewhere", InstallNamespace: "elsewhere"},
		},
		"gpu-operator": {
			{Source: "argocd", Name: "teamfoo-gpu-operator", Namespace: "nvidia-gpu-operator",
				InstallNamespace: "nvidia-gpu-operator"},
			{Source: "argocd", Name: "teamfoo-gpu-operator-readiness", Namespace: "nvidia-gpu-operator",
				InstallNamespace: "nvidia-gpu-operator", Injected: true},
		},
	}}
	for _, d := range []Deployer{DeployerArgoCD, DeployerArgoCDHelm} {
		tests = append(tests, deployerCase{string(d) + " matches a prefixed Application in place", d, argoRecords, argoWant})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := attributeReleases(tt.deployer, comps, tt.records)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("attributeReleases() =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

// TestReadReleasesKeepsEveryInstall drives two tenants' installs of one chart,
// neither in the registry namespace, through the whole read. Read cannot
// choose between them and fails; ReadReleases has no choice to make.
func TestReadReleasesKeepsEveryInstall(t *testing.T) {
	comps := []Component{fixtureGPUOperator, fixtureCertManager}
	client := fake.NewSimpleClientset(
		helmSecret("tenant-a", "gpu-operator", 1, "deployed",
			releasePayload(t, "gpu-operator", "tenant-a", "deployed", "gpu-operator", "v25.3.3", 1)),
		helmSecret("tenant-b", "gpu-operator", 2, "deployed",
			releasePayload(t, "gpu-operator", "tenant-b", "deployed", "gpu-operator", "v25.10.0", 2)),
		// Named like no component, so only the widened read decodes it.
		stampedSecret(t, "nodewright", "rdma-netns-exclusive", "0.1.0", "v0.23.0"),
		// A foreign record the widened read cannot decode is skipped.
		helmSecret("monitoring", "prometheus", 1, "deployed", undecodablePayload),
	)
	opts := Options{Deployer: DeployerHelm, Components: comps}

	if _, err := read(context.Background(), client, argoClient(), opts); errorstest.ReportedCode(err) != errors.ErrCodeConflict {
		t.Fatalf("read() error = %v, want CONFLICT", err)
	}

	got, err := readReleases(context.Background(), client, argoClient(), opts)
	if err != nil {
		t.Fatalf("readReleases() error = %v", err)
	}
	want := Releases{
		Components: map[string][]Record{"gpu-operator": {
			{Source: "helm", Name: "gpu-operator", Namespace: "tenant-a", InstallNamespace: "tenant-a", Status: "deployed"},
			{Source: "helm", Name: "gpu-operator", Namespace: "tenant-b", InstallNamespace: "tenant-b", Status: "deployed"},
		}},
		Stamped: []Record{
			{Source: "helm", Name: "rdma-netns-exclusive", Namespace: "nodewright", InstallNamespace: "nodewright",
				Status: "deployed"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readReleases() =\n%+v\nwant\n%+v", got, want)
	}
}

// TestReadReleasesReadsTheDeployersSource proves each deployer reads only the
// source it writes: Applications for Argo CD, Helm records otherwise.
func TestReadReleasesReadsTheDeployersSource(t *testing.T) {
	comps := []Component{fixtureGPUOperator, fixtureCertManager}
	client := fake.NewSimpleClientset(
		helmSecret("flux-system", "nvidia-gpu-operator-gpu-operator", 1, "deployed",
			releasePayload(t, "nvidia-gpu-operator-gpu-operator", "nvidia-gpu-operator", "deployed",
				"gpu-operator", "v25.3.3", 1)),
	)
	dyn := argoClient(argoAppWith(t, gpuOperatorApp, func(app *unstructured.Unstructured) {
		app.SetName("teamfoo-gpu-operator")
		if err := unstructured.SetNestedField(app.Object, "nvidia-gpu-operator", "spec", "destination", "namespace"); err != nil {
			t.Fatalf("set destination namespace: %v", err)
		}
	}))

	tests := []struct {
		deployer Deployer
		want     []Record
	}{
		{DeployerFlux, []Record{{Source: "helm", Name: "nvidia-gpu-operator-gpu-operator", Namespace: "flux-system",
			InstallNamespace: "nvidia-gpu-operator", Status: "deployed"}}},
		{DeployerArgoCD, []Record{{Source: "argocd", Name: "teamfoo-gpu-operator", Namespace: "nvidia-gpu-operator",
			InstallNamespace: "nvidia-gpu-operator"}}},
		{DeployerHelm, nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.deployer), func(t *testing.T) {
			got, err := readReleases(context.Background(), client, dyn, Options{Deployer: tt.deployer, Components: comps})
			if err != nil {
				t.Fatalf("readReleases() error = %v", err)
			}
			if records := got.Components["gpu-operator"]; !reflect.DeepEqual(records, tt.want) {
				t.Errorf("gpu-operator records = %+v, want %+v", records, tt.want)
			}
		})
	}
}

func TestReadReleasesRefusesBeforeContactingAnyCluster(t *testing.T) {
	got, err := ReadReleases(context.Background(), Options{Components: []Component{fixtureGPUOperator}})
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if !reflect.DeepEqual(got, Releases{}) {
		t.Errorf("ReadReleases() = %+v alongside an error, want zero Releases", got)
	}
}

func TestReadReleasesErrors(t *testing.T) {
	failingList := fake.NewSimpleClientset()
	failingList.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New(errors.ErrCodeUnavailable, "apiserver unavailable")
	})
	tests := []struct {
		name  string
		typed *fake.Clientset
		opts  Options
		want  errors.ErrorCode
	}{
		{"invalid request", fake.NewSimpleClientset(), Options{Components: []Component{fixtureGPUOperator}}, errors.ErrCodeInvalidRequest},
		{"failed list", failingList, Options{Deployer: DeployerHelm, Components: []Component{fixtureGPUOperator}}, errors.ErrCodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readReleases(context.Background(), tt.typed, nil, tt.opts)
			if !stderrors.Is(err, errors.New(tt.want, "")) {
				t.Errorf("readReleases() error = %v, want %s", err, tt.want)
			}
		})
	}
}

func TestReadReleasesFromKubeconfig(t *testing.T) {
	secret := helmSecret("nvidia-gpu-operator", "gpu-operator", 1, "deployed",
		releasePayload(t, "gpu-operator", "nvidia-gpu-operator", "deployed", "gpu-operator", "v25.3.3", 1))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var list any
		switch r.URL.Path {
		case "/api/v1/secrets":
			list = &corev1.SecretList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"},
				Items:    []corev1.Secret{*secret},
			}
		case "/api/v1/configmaps":
			list = &corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"}}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(list); err != nil {
			t.Errorf("encode %s: %v", r.URL.Path, err)
		}
	}))
	defer server.Close()

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: `+server.URL+`
users:
- name: fake
  user: {}
contexts:
- name: fake
  context:
    cluster: fake
    user: fake
current-context: fake
`), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	got, err := ReadReleases(context.Background(), Options{
		Kubeconfig: kubeconfig,
		Deployer:   DeployerHelm,
		Components: []Component{fixtureGPUOperator},
	})
	if err != nil {
		t.Fatalf("ReadReleases() error = %v", err)
	}
	want := Releases{Components: map[string][]Record{"gpu-operator": {{
		Source: "helm", Name: "gpu-operator", Namespace: "nvidia-gpu-operator",
		InstallNamespace: "nvidia-gpu-operator", Status: "deployed",
	}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadReleases() = %+v, want %+v", got, want)
	}
}

func TestReadReleasesFailsWithoutACluster(t *testing.T) {
	got, err := ReadReleases(context.Background(), Options{
		Kubeconfig: "/nonexistent/kubeconfig",
		Deployer:   DeployerHelm,
		Components: []Component{fixtureGPUOperator},
	})
	if err == nil {
		t.Fatal("ReadReleases read a cluster from a kubeconfig that does not exist")
	}
	if !strings.Contains(err.Error(), "kube config") {
		t.Errorf("error = %v, want a client failure rather than a rejected request", err)
	}
	if !reflect.DeepEqual(got, Releases{}) {
		t.Errorf("ReadReleases() = %+v alongside an error, want zero Releases", got)
	}
}

func TestHelmReleaseName(t *testing.T) {
	tests := []struct {
		deployer  Deployer
		name      string
		namespace string
		want      string
		wantOK    bool
	}{
		{DeployerHelm, "gpu-operator-pre", "gpu-operator", "gpu-operator-pre", true},
		{DeployerHelmfile, "gpu-operator", "gpu-operator", "gpu-operator", true},
		{DeployerTerraform, "gpu-operator", "gpu-operator", "gpu-operator", true},
		{DeployerFleet, "gpu-operator", "gpu-operator", "gpu-operator", true},
		{DeployerFlux, "gpu-operator-post", "nvidia-gpu-operator", "nvidia-gpu-operator-gpu-operator-post", true},
		{DeployerFlux, "bare", "", "bare", true},
		{DeployerArgoCD, "gpu-operator", "gpu-operator", "", false},
		{DeployerArgoCDHelm, "gpu-operator", "gpu-operator", "", false},
		{Deployer("kubectl"), "gpu-operator", "gpu-operator", "", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.deployer)+"/"+tt.name, func(t *testing.T) {
			got, ok := HelmReleaseName(tt.deployer, tt.name, tt.namespace)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("HelmReleaseName(%q, %q, %q) = %q, %v, want %q, %v",
					tt.deployer, tt.name, tt.namespace, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
