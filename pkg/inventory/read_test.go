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
	stderrors "errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
)

// The component fixtures below all carry a namespace that is not their name.
// Every deployer's transform combines the two, so a rule that reached for the
// wrong one still produces a plausible string when they agree.
var (
	fixtureGPUOperator = Component{Name: "gpu-operator", Namespace: "nvidia-gpu-operator", HasUpstreamChart: true}
	fixtureCertManager = Component{Name: "cert-manager", Namespace: "cert-manager-system", HasUpstreamChart: true}
	// A Kustomize or manifest-only component: no upstream chart, so the
	// chart version of whatever wrapper carries it is never its version.
	fixtureDRALabeler = Component{Name: "dra-node-labeler", Namespace: "kube-system"}
)

// helmRec is a Helm-sourced record. chartVersion and the stamp are separate
// arguments so a fixture can make them disagree, which is the only way to see
// which one the version rule read.
func helmRec(name, namespace, status, chartVersion string, annotations map[string]string) installedRelease {
	return installedRelease{
		Source:       sourceHelm,
		Name:         name,
		Namespace:    namespace,
		Revision:     1,
		Status:       status,
		ChartName:    name,
		ChartVersion: chartVersion,
		AppVersion:   "ignored-app-version",
		Annotations:  annotations,
	}
}

// argoRec is an Argo-sourced record. Annotations are settable even though the
// reader never fills them, so a test can prove the version rule branches on
// Source rather than on the map being nil.
func argoRec(name, namespace, chartVersion string, annotations map[string]string) installedRelease {
	return installedRelease{
		Source:       sourceArgo,
		Name:         name,
		Namespace:    namespace,
		ChartName:    name,
		ChartVersion: chartVersion,
		Annotations:  annotations,
	}
}

func stamp(version string) map[string]string {
	return map[string]string{
		header.AnnotationComponentVersion: version,
		header.AnnotationGeneratedBy:      "v0.23.0",
	}
}

// TestAttributeRecord pins the per-deployer name transform.
//
// scope.covers is deliberately loose and matches all of these shapes under
// every deployer; this is the layer that decides which one is actually the
// component's, so each case names a shape another deployer would have written
// and asserts it does not match here.
func TestAttributeRecord(t *testing.T) {
	// A component whose own name ends in an injected phase. It exists so the
	// "-post" record below has two readings, and the exact one must win.
	// Deliberately in gpu-operator's namespace: under the Argo rule a
	// different namespace would exclude gpu-operator before the tiers were
	// compared, and the tier comparison is the claim.
	postNamed := Component{Name: "gpu-operator-post", Namespace: "nvidia-gpu-operator", HasUpstreamChart: true}
	// A component whose name is a suffix of another's, for the Argo rule.
	shortNamed := Component{Name: "operator", Namespace: "nvidia-gpu-operator", HasUpstreamChart: true}
	noNamespace := Component{Name: "bare", HasUpstreamChart: true}

	comps := []Component{fixtureGPUOperator, fixtureCertManager, fixtureDRALabeler}

	tests := []struct {
		name     string
		deployer Deployer
		comps    []Component
		record   installedRelease
		want     string
		wantKind matchKind
	}{
		{
			name: "helm takes the component name exactly", deployer: DeployerHelm, comps: comps,
			record: helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "v25.3.3", nil),
			want:   "gpu-operator", wantKind: matchPrimary,
		},
		{
			name: "helmfile shares the helm transform", deployer: DeployerHelmfile, comps: comps,
			record: helmRec("cert-manager", "cert-manager-system", "deployed", "1.20.2", nil),
			want:   "cert-manager", wantKind: matchPrimary,
		},
		{
			name: "helm folds an injected pre folder into its parent", deployer: DeployerHelm, comps: comps,
			record: helmRec("gpu-operator-pre", "nvidia-gpu-operator", "deployed", "0.1.0", nil),
			want:   "gpu-operator", wantKind: matchInjected,
		},
		{
			name: "helm folds an injected post folder into its parent", deployer: DeployerHelm, comps: comps,
			record: helmRec("gpu-operator-post", "nvidia-gpu-operator", "deployed", "0.1.0", nil),
			want:   "gpu-operator", wantKind: matchInjected,
		},
		{
			name: "helm folds an injected readiness folder into its parent", deployer: DeployerHelm, comps: comps,
			record: helmRec("gpu-operator-readiness", "nvidia-gpu-operator", "deployed", "0.1.0", nil),
			want:   "gpu-operator", wantKind: matchInjected,
		},
		{
			name:     "a component named for a phase beats stripping the phase",
			deployer: DeployerHelm, comps: append(append([]Component{}, comps...), postNamed),
			record: helmRec("gpu-operator-post", "nvidia-gpu-operator", "deployed", "3.0.0", nil),
			want:   "gpu-operator-post", wantKind: matchPrimary,
		},
		{
			name: "helm does not read a flux composed name", deployer: DeployerHelm, comps: comps,
			record:   helmRec("nvidia-gpu-operator-gpu-operator", "nvidia-gpu-operator", "deployed", "v25.3.3", nil),
			wantKind: matchNone,
		},
		{
			name: "helm does not read an argo prefixed name", deployer: DeployerHelm, comps: comps,
			record:   helmRec("tenant-a-gpu-operator", "nvidia-gpu-operator", "deployed", "v25.3.3", nil),
			wantKind: matchNone,
		},
		{
			name: "flux composes the target namespace onto the name", deployer: DeployerFlux, comps: comps,
			record: helmRec("nvidia-gpu-operator-gpu-operator", "nvidia-gpu-operator", "deployed", "v25.3.3", nil),
			want:   "gpu-operator", wantKind: matchPrimary,
		},
		{
			name: "flux does not read a bare component name", deployer: DeployerFlux, comps: comps,
			record:   helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "v25.3.3", nil),
			wantKind: matchNone,
		},
		{
			name: "flux composes onto an injected folder too", deployer: DeployerFlux, comps: comps,
			record: helmRec("nvidia-gpu-operator-gpu-operator-post", "nvidia-gpu-operator", "deployed", "0.1.0", nil),
			want:   "gpu-operator", wantKind: matchInjected,
		},
		{
			name:     "flux falls back to the bare name with no target namespace",
			deployer: DeployerFlux, comps: []Component{noNamespace},
			record: helmRec("bare", "default", "deployed", "1.0.0", nil),
			want:   "bare", wantKind: matchPrimary,
		},
		{
			name:     "flux with no target namespace does not compose a leading separator",
			deployer: DeployerFlux, comps: []Component{noNamespace},
			record:   helmRec("-bare", "default", "deployed", "1.0.0", nil),
			wantKind: matchNone,
		},
		{
			name:     "argocd accepts a hyphenated prefix in the component namespace",
			deployer: DeployerArgoCD, comps: comps,
			record: argoRec("tenant-a-gpu-operator", "nvidia-gpu-operator", "v25.3.3", nil),
			want:   "gpu-operator", wantKind: matchPrimary,
		},
		{
			name:     "argocd accepts a prefix fused onto the first token",
			deployer: DeployerArgoCD, comps: comps,
			record: argoRec("tenantgpu-operator", "nvidia-gpu-operator", "v25.3.3", nil),
			want:   "gpu-operator", wantKind: matchPrimary,
		},
		{
			name: "argocd-helm shares the argocd transform", deployer: DeployerArgoCDHelm, comps: comps,
			record: argoRec("aicr-cert-manager", "cert-manager-system", "1.20.2", nil),
			want:   "cert-manager", wantKind: matchPrimary,
		},
		{
			name:     "argocd rejects a suffix match in another namespace",
			deployer: DeployerArgoCD, comps: comps,
			record:   argoRec("tenant-a-gpu-operator", "some-other-tenant", "v25.3.3", nil),
			wantKind: matchNone,
		},
		{
			name:     "argocd folds a prefixed injected folder into its parent",
			deployer: DeployerArgoCD, comps: comps,
			record: argoRec("tenant-a-gpu-operator-readiness", "nvidia-gpu-operator", "", nil),
			want:   "gpu-operator", wantKind: matchInjected,
		},
		{
			name:     "argocd prefers the longer of two suffix matches",
			deployer: DeployerArgoCD, comps: append([]Component{shortNamed}, comps...),
			record: argoRec("tenant-a-gpu-operator", "nvidia-gpu-operator", "v25.3.3", nil),
			want:   "gpu-operator", wantKind: matchPrimary,
		},
		{
			name:     "argocd prefers a component named for a phase over stripping it",
			deployer: DeployerArgoCD, comps: append(append([]Component{}, comps...), postNamed),
			record: argoRec("tenant-a-gpu-operator-post", "nvidia-gpu-operator", "3.0.0", nil),
			want:   "gpu-operator-post", wantKind: matchPrimary,
		},
		{
			name: "an unrelated workload matches nothing", deployer: DeployerHelm, comps: comps,
			record:   helmRec("prometheus", "monitoring", "deployed", "1.0.0", nil),
			wantKind: matchNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, kind := attributeRecord(tt.deployer, tt.comps, tt.record)
			if kind != tt.wantKind {
				t.Fatalf("attributeRecord(%q, %q) kind = %v, want %v",
					tt.deployer, tt.record.Name, kind, tt.wantKind)
			}
			name := ""
			if got >= 0 {
				name = tt.comps[got].Name
			}
			if name != tt.want {
				t.Errorf("attributeRecord(%q, %q) component = %q, want %q",
					tt.deployer, tt.record.Name, name, tt.want)
			}
		})
	}
}

// TestVersionFor pins ADR-021 Decision 5 plus the registry guard on rule 2.
//
// Every case makes the stamp and the chart version disagree, so a rule that
// read the wrong one cannot produce the expected string by coincidence.
func TestVersionFor(t *testing.T) {
	tests := []struct {
		name      string
		record    installedRelease
		component Component
		want      string
	}{
		{
			name:      "the stamp beats the wrapper chart version",
			record:    helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "0.1.0", stamp("v25.3.3")),
			component: fixtureGPUOperator,
			want:      "v25.3.3",
		},
		{
			name:      "the stamp answers for a component with no upstream chart",
			record:    helmRec("dra-node-labeler", "kube-system", "deployed", "0.1.0", stamp("release-1.4")),
			component: fixtureDRALabeler,
			want:      "release-1.4",
		},
		{
			name:      "an unstamped upstream release reports its chart version",
			record:    helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "v25.3.3", nil),
			component: fixtureGPUOperator,
			want:      "v25.3.3",
		},
		{
			name:      "an unstamped wrapper with no upstream chart reports nothing",
			record:    helmRec("dra-node-labeler", "kube-system", "deployed", "0.1.0", nil),
			component: fixtureDRALabeler,
			want:      "",
		},
		{
			name: "a generated-by stamp alone does not answer the version",
			record: helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "0.1.0",
				map[string]string{header.AnnotationGeneratedBy: "v0.23.0"}),
			component: fixtureGPUOperator,
			want:      "0.1.0",
		},
		{
			name: "an empty stamp is not a fall-through to the chart version",
			record: helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "v25.3.3",
				map[string]string{header.AnnotationComponentVersion: ""}),
			component: fixtureGPUOperator,
			want:      "",
		},
		{
			// localformat.stampFor writes the AICR build version to both
			// annotations when a component pins neither a chart version nor
			// a Kustomize tag. That agreement is the fallback's signature,
			// and reading it as a payload version makes an unchanged
			// component report as changed: the artifact side answers "" for
			// the same component.
			name: "a stamp that repeats the generated-by version is no payload version",
			record: helmRec("dra-node-labeler", "kube-system", "deployed", "0.0.0-dev",
				map[string]string{
					header.AnnotationComponentVersion: "0.0.0-dev",
					header.AnnotationGeneratedBy:      "0.0.0-dev",
				}),
			component: fixtureDRALabeler,
			want:      "",
		},
		{
			// The discriminator is not equality alone. A component pinned to
			// the string AICR happens to be released at would collide; an
			// upstream chart pin is exactly what the fallback is not.
			name: "an upstream pin that coincides with the AICR version still answers",
			record: helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "0.1.0",
				map[string]string{
					header.AnnotationComponentVersion: "0.23.0",
					header.AnnotationGeneratedBy:      "0.23.0",
				}),
			component: fixtureGPUOperator,
			want:      "0.23.0",
		},
		{
			name:      "an argo application ignores annotations it could not have carried",
			record:    argoRec("gpu-operator", "nvidia-gpu-operator", "v25.3.3", stamp("v9.9.9")),
			component: fixtureGPUOperator,
			want:      "v25.3.3",
		},
		{
			name:      "an argo path source reports nothing for a component with no chart",
			record:    argoRec("dra-node-labeler", "kube-system", "0.1.0", nil),
			component: fixtureDRALabeler,
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionFor(tt.record, tt.component); got != tt.want {
				t.Errorf("versionFor(%q) = %q, want %q", tt.record.Name, got, tt.want)
			}
		})
	}
}

// TestInstalledVersions covers the rules that need more than one record to be
// visible: status exclusion, the injected folder's silence, and the namespace
// tiebreak between two tenants' installs of one chart.
func TestInstalledVersions(t *testing.T) {
	comps := []Component{fixtureGPUOperator, fixtureCertManager, fixtureDRALabeler}

	tests := []struct {
		name     string
		deployer Deployer
		comps    []Component
		records  []installedRelease
		want     map[string]string
		counts   mappingCounts
		wantErr  bool
	}{
		{
			name: "an uninstalled release is excluded and counted", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator", "nvidia-gpu-operator", "uninstalled", "v25.3.3", nil),
				helmRec("cert-manager", "cert-manager-system", "deployed", "1.20.2", nil),
			},
			want:   map[string]string{"cert-manager": "1.20.2"},
			counts: mappingCounts{uninstalled: 1},
		},
		{
			name: "a failed release is reported, not excluded", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator", "nvidia-gpu-operator", "failed", "v25.3.3", nil),
			},
			want: map[string]string{"gpu-operator": "v25.3.3"},
		},
		{
			name: "a pending-upgrade release is reported, not excluded", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator", "nvidia-gpu-operator", "pending-upgrade", "v25.3.3", nil),
			},
			want: map[string]string{"gpu-operator": "v25.3.3"},
		},
		{
			name: "an uninstalling release is reported, not excluded", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator", "nvidia-gpu-operator", "uninstalling", "v25.3.3", nil),
			},
			want: map[string]string{"gpu-operator": "v25.3.3"},
		},
		{
			name: "an injected folder never supplies the version", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator-post", "nvidia-gpu-operator", "deployed", "0.1.0", stamp("v0.23.0")),
				helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "0.1.0", stamp("v25.3.3")),
			},
			want: map[string]string{"gpu-operator": "v25.3.3"},
		},
		{
			name: "an injected folder alone leaves the component unanswered", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator-readiness", "nvidia-gpu-operator", "deployed", "0.1.0", stamp("v0.23.0")),
			},
			want: map[string]string{},
		},
		{
			name: "two tenants resolve against the component namespace", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator", "tenant-a", "deployed", "v24.9.0", nil),
				helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "v25.3.3", nil),
				helmRec("gpu-operator", "tenant-b", "deployed", "v23.6.0", nil),
			},
			want: map[string]string{"gpu-operator": "v25.3.3"},
		},
		{
			name: "two tenants and no expected namespace is an error", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator", "tenant-a", "deployed", "v24.9.0", nil),
				helmRec("gpu-operator", "tenant-b", "deployed", "v23.6.0", nil),
			},
			wantErr: true,
		},
		{
			name: "a lone install in an unexpected namespace still answers", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator", "tenant-a", "deployed", "v25.3.3", nil),
			},
			want: map[string]string{"gpu-operator": "v25.3.3"},
		},
		{
			name: "an uninstalled tenant does not create an ambiguity", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator", "tenant-a", "uninstalled", "v24.9.0", nil),
				helmRec("gpu-operator", "tenant-b", "deployed", "v23.6.0", nil),
			},
			want:   map[string]string{"gpu-operator": "v23.6.0"},
			counts: mappingCounts{uninstalled: 1},
		},
		{
			name: "a stamped record matching no component is counted", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				// The flux shape, read with the helm rule: exactly the
				// mis-mapping this counter exists to surface.
				helmRec("nvidia-gpu-operator-gpu-operator", "nvidia-gpu-operator", "deployed", "0.1.0",
					stamp("v25.3.3")),
			},
			want:   map[string]string{},
			counts: mappingCounts{stampedUnmatched: 1},
		},
		{
			name: "an unstamped record matching no component is not counted", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("prometheus-gpu-operator", "monitoring", "deployed", "1.0.0", nil),
			},
			want: map[string]string{},
		},
		{
			name: "a matched stamped record is not counted as unmatched", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "0.1.0", stamp("v25.3.3")),
			},
			want: map[string]string{"gpu-operator": "v25.3.3"},
		},
		{
			name: "a stamped injected folder is matched, not counted", deployer: DeployerHelm, comps: comps,
			records: []installedRelease{
				helmRec("gpu-operator-pre", "nvidia-gpu-operator", "deployed", "0.1.0", stamp("v0.23.0")),
			},
			want: map[string]string{},
		},
		{
			// An Application carries no chart annotations, so annotations on
			// one are not AICR's stamp and counting them would report a
			// mapping failure that did not happen.
			name: "an unmatched argo application is never a stamp finding", deployer: DeployerArgoCD, comps: comps,
			records: []installedRelease{
				argoRec("prometheus-gpu-operator", "monitoring", "1.0.0", stamp("v9.9.9")),
			},
			want: map[string]string{},
		},
		{
			// The uninstalled exclusion is Helm's alone: an Application has no
			// release status, so a Status set on one must not be read.
			name: "an argo application is not excluded on a status", deployer: DeployerArgoCD, comps: comps,
			records: []installedRelease{
				{
					Source: sourceArgo, Name: "aicr-gpu-operator", Namespace: "nvidia-gpu-operator",
					Status: helmStatusUninstalled, ChartVersion: "v25.3.3",
				},
			},
			want: map[string]string{"gpu-operator": "v25.3.3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, counts, err := installedVersions(tt.deployer, tt.comps, tt.records)
			if (err != nil) != tt.wantErr {
				t.Fatalf("installedVersions() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeConflict, "")) {
					t.Errorf("installedVersions() error code = %v, want CONFLICT", err)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("installedVersions() = %v, want %v", got, tt.want)
			}
			if counts != tt.counts {
				t.Errorf("installedVersions() counts = %+v, want %+v", counts, tt.counts)
			}
		})
	}
}

// TestCombineMergesHelmFirst pins the merge order argocd-helm depends on: its
// app-of-apps is a Helm release and its per-component Applications are the
// per-component truth, so Argo may only fill what Helm left unanswered.
func TestCombineMergesHelmFirst(t *testing.T) {
	comps := []Component{fixtureGPUOperator, fixtureCertManager}

	helmRead := inventoryRead{
		Records:      7,
		Unattributed: 2,
		Unreadable:   1,
		Releases: []installedRelease{
			helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "v25.3.3", nil),
		},
	}
	argoRead := inventoryRead{
		Records:      4,
		Unattributed: 3,
		Unreadable:   2,
		Releases: []installedRelease{
			// Same component, a different version: Helm's answer must stand.
			argoRec("aicr-gpu-operator", "nvidia-gpu-operator", "v24.9.0", nil),
			argoRec("aicr-cert-manager", "cert-manager-system", "1.20.2", nil),
		},
	}

	got, err := combine(DeployerArgoCDHelm, comps, helmRead, argoRead)
	if err != nil {
		t.Fatalf("combine() error = %v", err)
	}

	want := Result{
		Versions: map[string]string{"gpu-operator": "v25.3.3", "cert-manager": "1.20.2"},
		Source: SourceInfo{
			Helm: HelmInfo{Records: 7, Unattributed: 2, Unreadable: 1},
			Argo: ArgoInfo{Applications: 4, Unattributed: 3, Unreadable: 2},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("combine() = %+v, want %+v", got, want)
	}
}

// TestReadOptionsValidation refuses a request that cannot be answered before
// any cluster is contacted.
func TestReadOptionsValidation(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		code errors.ErrorCode
	}{
		{
			name: "no deployer",
			opts: Options{Components: []Component{fixtureGPUOperator}},
			code: errors.ErrCodeInvalidRequest,
		},
		{
			name: "unknown deployer",
			opts: Options{Deployer: "kustomize", Components: []Component{fixtureGPUOperator}},
			code: errors.ErrCodeInvalidRequest,
		},
		{
			name: "no components",
			opts: Options{Deployer: DeployerHelm},
			code: errors.ErrCodeInvalidRequest,
		},
		{
			name: "an unnamed component",
			opts: Options{Deployer: DeployerHelm, Components: []Component{{Namespace: "x"}}},
			code: errors.ErrCodeInvalidRequest,
		},
		{
			name: "a duplicate component",
			opts: Options{Deployer: DeployerHelm, Components: []Component{fixtureGPUOperator, fixtureGPUOperator}},
			code: errors.ErrCodeInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validate()
			if err == nil {
				t.Fatal("validate() = nil, want an error")
			}
			if !stderrors.Is(err, errors.New(tt.code, "")) {
				t.Errorf("validate() error = %v, want code %v", err, tt.code)
			}
		})
	}

	valid := Options{Deployer: DeployerFlux, Components: []Component{fixtureGPUOperator, fixtureCertManager}}
	if err := valid.validate(); err != nil {
		t.Errorf("validate() on a well-formed request = %v, want nil", err)
	}
}

// TestReadRefusesBeforeContactingAnyCluster pins that the request is checked
// before a client is built. The test suite has no cluster, so a Read that
// reached the client would fail on that instead — and in production the order
// is what keeps an operator's typo from being reported as a connection
// problem.
func TestReadRefusesBeforeContactingAnyCluster(t *testing.T) {
	_, err := Read(context.Background(), Options{Components: []Component{fixtureGPUOperator}})
	if err == nil {
		t.Fatal("Read() with no deployer = nil, want an error")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("Read() error = %v, want INVALID_REQUEST", err)
	}
}

// stampedSecret is a Helm storage record whose chart version and stamped
// payload version disagree, which is the shape every generated bundle has.
func stampedSecret(t *testing.T, namespace, release, chartVersion, payloadVersion string) *corev1.Secret {
	t.Helper()
	annotations := ""
	if payloadVersion != "" {
		annotations = fmt.Sprintf(`,"annotations":{%q:%q,%q:"v0.23.0"}`,
			header.AnnotationComponentVersion, payloadVersion, header.AnnotationGeneratedBy)
	}
	payload := encodeReleaseFixture(t, fmt.Sprintf(
		`{"info":{"status":"deployed"},"chart":{"metadata":{"name":%q,"version":%q,"appVersion":"1.0.0"%s}}}`,
		release, chartVersion, annotations))

	return helmSecret(namespace, release, 1, "deployed", payload)
}

// TestReadWiresBothReaders drives the whole mapping layer through the two
// readers, so the record counts, the scope the readers are given, and the
// merge are all exercised against objects rather than against hand-built
// projections.
func TestReadWiresBothReaders(t *testing.T) {
	comps := []Component{fixtureGPUOperator, fixtureCertManager, fixtureDRALabeler}

	// Flux names the release "<targetNamespace>-<name>", so the Helm reader
	// sees nvidia-gpu-operator-gpu-operator and the mapping layer has to undo
	// exactly that transform.
	client := fake.NewSimpleClientset(
		stampedSecret(t, "nvidia-gpu-operator", "nvidia-gpu-operator-gpu-operator", "0.1.0", "v25.3.3"),
		stampedSecret(t, "kube-system", "kube-system-dra-node-labeler", "0.1.0", ""),
		// A foreign release that names no component: dropped, but its record
		// still counts toward what was read.
		stampedSecret(t, "monitoring", "prometheus", "70.1.0", ""),
	)
	dyn := argoClient()

	got, err := read(context.Background(), client, dyn, Options{Deployer: DeployerFlux, Components: comps})
	if err != nil {
		t.Fatalf("read() error = %v", err)
	}

	// dra-node-labeler is present with no comparable version: the wrapper
	// chart carrying it is unstamped and the component has no upstream chart,
	// so the only version in the cluster is the wrapper's own.
	want := map[string]string{"gpu-operator": "v25.3.3", "dra-node-labeler": ""}
	if !reflect.DeepEqual(got.Versions, want) {
		t.Errorf("read() versions = %v, want %v", got.Versions, want)
	}
	if got.Source.Helm.Records != 3 {
		t.Errorf("read() helm records = %d, want 3", got.Source.Helm.Records)
	}
	if got.Source.Argo != (ArgoInfo{}) {
		t.Errorf("read() argo info = %+v, want zero", got.Source.Argo)
	}
}

// TestDeployersMatchBundlerConfig fails when a deployer is added to the
// bundler without a name transform here.
//
// The constants in read.go are declared rather than imported, for the reason
// pkg/upgrade's canonicalDeployers gives: pkg/bundler/config is the wrong
// dependency for a package that reads a cluster. This test carries the import
// instead, so the copy cannot drift silently — and a new deployer that nobody
// taught this package would otherwise map every one of its releases to
// nothing, which reads as a cluster with no components installed.
func TestDeployersMatchBundlerConfig(t *testing.T) {
	canonical := config.GetDeployerTypes()

	got := make([]string, 0, len(allDeployers))
	for _, d := range allDeployers {
		got = append(got, string(d))
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, canonical) {
		t.Errorf("allDeployers = %v, want %v", got, canonical)
	}
}

// TestAttributeRecordGuards covers the branches a validated Options cannot
// reach but the function still has to hold: an unknown deployer, a component
// with no name, and a worse match arriving after a better one.
func TestAttributeRecordGuards(t *testing.T) {
	postNamed := Component{Name: "gpu-operator-post", Namespace: "nvidia-gpu-operator", HasUpstreamChart: true}

	tests := []struct {
		name     string
		deployer Deployer
		comps    []Component
		record   installedRelease
		want     string
		wantKind matchKind
	}{
		{
			name:     "a deployer with no transform matches nothing",
			deployer: Deployer("kustomize"), comps: []Component{fixtureGPUOperator},
			record:   helmRec("gpu-operator", "nvidia-gpu-operator", "deployed", "v25.3.3", nil),
			wantKind: matchNone,
		},
		{
			// strings.HasSuffix(anything, "") is true, so an unnamed component
			// would otherwise claim every Application in its namespace.
			name:     "a component with no name does not match every record",
			deployer: DeployerArgoCD, comps: []Component{{Namespace: "nvidia-gpu-operator"}},
			record:   argoRec("tenant-a-gpu-operator", "nvidia-gpu-operator", "v25.3.3", nil),
			wantKind: matchNone,
		},
		{
			// The reverse declaration order of the phase-precedence case
			// above: here the primary match is found first and the injected
			// one has to be rejected rather than merely not preferred.
			name:     "a later injected match does not displace an earlier primary",
			deployer: DeployerHelm, comps: []Component{postNamed, fixtureGPUOperator},
			record: helmRec("gpu-operator-post", "nvidia-gpu-operator", "deployed", "3.0.0", nil),
			want:   "gpu-operator-post", wantKind: matchPrimary,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, kind := attributeRecord(tt.deployer, tt.comps, tt.record)
			if kind != tt.wantKind {
				t.Fatalf("attributeRecord(%q, %q) kind = %v, want %v",
					tt.deployer, tt.record.Name, kind, tt.wantKind)
			}
			name := ""
			if got >= 0 {
				name = tt.comps[got].Name
			}
			if name != tt.want {
				t.Errorf("attributeRecord(%q, %q) component = %q, want %q",
					tt.deployer, tt.record.Name, name, tt.want)
			}
		})
	}
}

// TestCombineSurfacesAmbiguityFromEitherReader pins that neither side can
// swallow the conflict. The Argo side is the one worth stating: its versions
// only fill what Helm left unanswered, so a merge that skipped them would also
// skip the error they carry.
func TestCombineSurfacesAmbiguityFromEitherReader(t *testing.T) {
	comps := []Component{fixtureGPUOperator}

	helmAmbiguous := inventoryRead{Releases: []installedRelease{
		helmRec("gpu-operator", "tenant-a", "deployed", "v24.9.0", nil),
		helmRec("gpu-operator", "tenant-b", "deployed", "v23.6.0", nil),
	}}
	argoAmbiguous := inventoryRead{Releases: []installedRelease{
		argoRec("blue-gpu-operator", "nvidia-gpu-operator", "v24.9.0", nil),
		argoRec("green-gpu-operator", "nvidia-gpu-operator", "v23.6.0", nil),
	}}

	tests := []struct {
		name              string
		deployer          Deployer
		helm, argo        inventoryRead
		wantErrContains   []string
		rejectErrContains []string
	}{
		{
			name: "from the helm reader", deployer: DeployerHelm, helm: helmAmbiguous,
			wantErrContains: []string{"tenant-a, tenant-b"},
		},
		{
			// One destination namespace, two Applications. The count of
			// installs and the list of namespaces are different facts here.
			name: "from the argo reader", deployer: DeployerArgoCD, argo: argoAmbiguous,
			wantErrContains:   []string{"blue-gpu-operator, green-gpu-operator", "(nvidia-gpu-operator)"},
			rejectErrContains: []string{"nvidia-gpu-operator, nvidia-gpu-operator"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := combine(tt.deployer, comps, tt.helm, tt.argo)
			if err == nil {
				t.Fatal("combine() = nil, want an error")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeConflict, "")) {
				t.Errorf("combine() error = %v, want CONFLICT", err)
			}
			for _, want := range tt.wantErrContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			for _, reject := range tt.rejectErrContains {
				if strings.Contains(err.Error(), reject) {
					t.Errorf("error %q unexpectedly contains %q", err, reject)
				}
			}
		})
	}
}

// TestResolveInstallReportsNamespacesOnce is the Argo shape from the test
// above, read for what it says rather than only for its code.
//
// Two Applications under different name prefixes share one destination
// namespace. Listing a namespace per record made that read "installed in 2
// namespaces (nvidia-gpu-operator, nvidia-gpu-operator)", which asserts a
// namespace conflict that does not exist and points an operator at the wrong
// thing: the discriminator is the two Application names.
func TestResolveInstallReportsNamespacesOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []installedRelease
		want    []string
		reject  []string
	}{
		{
			name: "two installs sharing one namespace",
			records: []installedRelease{
				argoRec("green-gpu-operator", "nvidia-gpu-operator", "v23.6.0", nil),
				argoRec("blue-gpu-operator", "nvidia-gpu-operator", "v24.9.0", nil),
			},
			want: []string{
				"2 installs (blue-gpu-operator, green-gpu-operator)",
				"in namespaces (nvidia-gpu-operator)",
				"2 of them are in the \"nvidia-gpu-operator\"",
			},
			reject: []string{"nvidia-gpu-operator, nvidia-gpu-operator"},
		},
		{
			name: "two installs in two namespaces",
			records: []installedRelease{
				helmRec("gpu-operator", "tenant-b", "deployed", "v23.6.0", nil),
				helmRec("gpu-operator", "tenant-a", "deployed", "v24.9.0", nil),
			},
			want: []string{
				"2 installs (gpu-operator, gpu-operator)",
				"in namespaces (tenant-a, tenant-b)",
				"0 of them are in the \"nvidia-gpu-operator\"",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := resolveInstall(fixtureGPUOperator, tt.records)
			if err == nil {
				t.Fatal("resolveInstall() = nil, want an error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
			for _, reject := range tt.reject {
				if strings.Contains(err.Error(), reject) {
					t.Errorf("error %q repeats a namespace: %q", err, reject)
				}
			}
		})
	}
}

// TestReadPropagatesReaderFailures pins that a reader's error ends the read.
// An inventory assembled from a partial answer reports the components it could
// not see as newly installed, which is the verdict this command exists to get
// right.
func TestReadPropagatesReaderFailures(t *testing.T) {
	opts := Options{Deployer: DeployerHelm, Components: []Component{fixtureGPUOperator}}

	t.Run("the helm reader", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		client.PrependReactor("list", "secrets", func(_ k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Resource: "secrets"}, "", stderrors.New("denied"))
		})

		if _, err := read(context.Background(), client, argoClient(), opts); err == nil {
			t.Fatal("read() = nil, want an error")
		}
	})

	t.Run("the argo reader", func(t *testing.T) {
		dyn := argoClient()
		dyn.PrependReactor("list", "applications", func(_ k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "argoproj.io", Resource: "applications"}, "",
				stderrors.New("denied"))
		})

		if _, err := read(context.Background(), fake.NewSimpleClientset(), dyn, opts); err == nil {
			t.Fatal("read() = nil, want an error")
		}
	})
}
