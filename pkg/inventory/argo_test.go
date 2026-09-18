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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// The fixtures below are the deployer's golden Applications
// (pkg/bundler/deployer/argocd/testdata/), trimmed of the syncPolicy comment
// block and edited in one direction only: every field whose value this reader
// could plausibly confuse with another is given a value the other does not
// share. metadata.namespace is never the destination namespace, the git
// source's targetRevision is never the chart's, and both a status block and
// metadata annotations are present so a reader that harvested either would be
// caught by the exact struct comparison.

// certManagerApp is the multi-source shape: an upstream chart plus a git
// source carrying the values file by ref.
const certManagerApp = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: "cert-manager"
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "1"
spec:
  project: "default"
  sources:
    - repoURL: https://charts.jetstack.io
      chart: cert-manager
      targetRevision: 1.20.2
      helm:
        valueFiles:
          - $values/001-cert-manager/values.yaml
    - repoURL: 'https://github.com/example/aicr-bundles.git'
      targetRevision: main
      ref: values
  destination:
    server: "https://kubernetes.default.svc"
    namespace: "cert-manager"
status:
  sync:
    status: Synced
    revision: 9f3c1de
  health:
    status: Healthy
`

// certManagerAppReversed is certManagerApp with the two sources swapped, so
// sources[0] is the git ref entry whose targetRevision is a branch name.
const certManagerAppReversed = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: "cert-manager"
  namespace: argocd
spec:
  project: "default"
  sources:
    - repoURL: 'https://github.com/example/aicr-bundles.git'
      targetRevision: main
      ref: values
    - repoURL: https://charts.jetstack.io
      chart: cert-manager
      targetRevision: 1.20.2
      helm:
        valueFiles:
          - $values/001-cert-manager/values.yaml
  destination:
    server: "https://kubernetes.default.svc"
    namespace: "cert-manager"
`

// gpuOperatorApp is the single-source InlineValues shape: one spec.source
// naming the chart directly, with no git source at all.
const gpuOperatorApp = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: "gpu-operator"
  namespace: argocd
spec:
  project: "default"
  source:
    repoURL: https://helm.ngc.nvidia.com/nvidia
    chart: gpu-operator
    targetRevision: v25.10.0
    helm:
      valuesObject:
        driver:
          enabled: true
  destination:
    server: "https://kubernetes.default.svc"
    namespace: "gpu-operator"
`

// nodewrightApp is the path-based manifest-only shape. Its targetRevision is
// the bundle repository's branch, which is not a component version.
const nodewrightApp = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: "nodewright-customizations"
  namespace: argocd
spec:
  project: "default"
  source:
    repoURL: 'https://github.com/example/aicr-bundles.git'
    targetRevision: main
    path: 002-nodewright-customizations
  destination:
    server: "https://kubernetes.default.svc"
    namespace: "skyhook"
`

// kustomizeApp is the path-based Kustomize shape, which reaches the cluster
// looking exactly like the manifest-only one.
const kustomizeApp = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: "my-kustomize-app"
  namespace: argocd
spec:
  project: "default"
  source:
    repoURL: 'https://github.com/example/aicr-bundles.git'
    targetRevision: release-0.19
    path: 002-my-kustomize-app
  destination:
    server: "https://kubernetes.default.svc"
    namespace: "my-app"
`

// argoApp parses one Application fixture the way the apiserver would hand it
// to a dynamic client: JSON-compatible scalars, no typed schema.
func argoApp(t *testing.T, manifest string) *unstructured.Unstructured {
	t.Helper()
	object := map[string]any{}
	if err := yaml.Unmarshal([]byte(manifest), &object); err != nil {
		t.Fatalf("parse Application fixture: %v", err)
	}

	return &unstructured.Unstructured{Object: object}
}

// argoAppWith parses a fixture and applies an edit, for cases that differ
// from a golden shape in one field.
func argoAppWith(t *testing.T, manifest string, edit func(*unstructured.Unstructured)) *unstructured.Unstructured {
	t.Helper()
	app := argoApp(t, manifest)
	edit(app)

	return app
}

// argoClient is a dynamic client holding the given Applications. The list
// kind has to be declared because there is no typed scheme to infer it from.
func argoClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{argoApplicationGVR: "ApplicationList"}, objects...)
}

// argoForbidden is the RBAC denial for a cluster-scoped List of the CRD.
func argoForbidden() error {
	return apierrors.NewForbidden(
		schema.GroupResource{Group: "argoproj.io", Resource: "applications"}, "",
		stderrors.New("User cannot list resource at the cluster scope"))
}

// argoNotFound is what the apiserver answers when the CRD is not installed.
func argoNotFound() error {
	return apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "applications"}, "")
}

// argoNoMatch is what a RESTMapper-backed client answers for the same cluster.
func argoNoMatch() error {
	return &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "argoproj.io", Kind: "Application"},
		SearchedVersions: []string{"v1alpha1"},
	}
}

// wantCertManager is the projection of certManagerApp. Every field Argo has
// no equivalent of stays zero: a reader that filled Status from the sync
// state, Annotations from metadata, or Revision from anything at all fails
// the comparison here.
var wantCertManager = installedRelease{
	Source:       sourceArgo,
	Name:         "cert-manager",
	Namespace:    "cert-manager",
	ChartName:    "cert-manager",
	ChartVersion: "1.20.2",
}

func TestArgoApplications(t *testing.T) {
	tests := []struct {
		name string
		// objects seed the fake cluster in the order given.
		objects []runtime.Object
		// page, when set, is returned by a List reactor instead of the
		// tracker's own answer, so a case can fix the order items arrive in.
		page *unstructured.UnstructuredList
		// listErr, when set, makes every List fail with it.
		listErr error
		// within defaults to argoTestScope when nil.
		within           scope
		cancelContext    bool
		want             []installedRelease
		wantUnattributed int
		wantUnreadable   int
		wantErr          bool
		wantErrCode      errors.ErrorCode
		wantErrContains  []string
		wantErrContext   map[string]any
	}{
		{
			name:    "multi-source upstream chart",
			objects: []runtime.Object{argoApp(t, certManagerApp)},
			want:    []installedRelease{wantCertManager},
		},
		{
			name:    "chart source is not first",
			objects: []runtime.Object{argoApp(t, certManagerAppReversed)},
			want:    []installedRelease{wantCertManager},
		},
		{
			name:    "single-source with chart",
			objects: []runtime.Object{argoApp(t, gpuOperatorApp)},
			want: []installedRelease{{
				Source:       sourceArgo,
				Name:         "gpu-operator",
				Namespace:    "gpu-operator",
				ChartName:    "gpu-operator",
				ChartVersion: "v25.10.0",
			}},
		},
		{
			// The payload version of a generated wrapper is not in the
			// cluster. targetRevision here is the bundle repository's branch,
			// so reporting it would claim every such component sits at "main".
			name:    "path-based manifest-only",
			objects: []runtime.Object{argoApp(t, nodewrightApp)},
			want:    []installedRelease{{Source: sourceArgo, Name: "nodewright-customizations", Namespace: "skyhook"}},
		},
		{
			name:    "path-based kustomize",
			objects: []runtime.Object{argoApp(t, kustomizeApp)},
			want:    []installedRelease{{Source: sourceArgo, Name: "my-kustomize-app", Namespace: "my-app"}},
		},
		{
			// Every generated Application is written into argocd whatever it
			// deploys, so the Application's own namespace answers a different
			// question from the one the inventory asks.
			name: "destination namespace wins over the Application's own",
			objects: []runtime.Object{argoAppWith(t, certManagerApp, func(app *unstructured.Unstructured) {
				app.SetNamespace("platform-gitops")
			})},
			want: []installedRelease{wantCertManager},
		},
		{
			name: "sources without a chart fall through to the one that has it",
			objects: []runtime.Object{argoApp(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: "kai-scheduler"
  namespace: argocd
spec:
  sources:
    - repoURL: 'https://github.com/example/aicr-bundles.git'
      targetRevision: main
      ref: values
    - repoURL: 'https://github.com/example/overlays.git'
      targetRevision: overlay-branch
      path: kai
    - repoURL: https://charts.example.com
      chart: kai-scheduler
      targetRevision: v0.9.4
  destination:
    namespace: "kai-scheduler"
`)},
			want: []installedRelease{{
				Source:       sourceArgo,
				Name:         "kai-scheduler",
				Namespace:    "kai-scheduler",
				ChartName:    "kai-scheduler",
				ChartVersion: "v0.9.4",
			}},
		},
		{
			// Only the chart-bearing source is read, so a git source whose
			// targetRevision is not a string is not this reader's business.
			// Failing on it would refuse an Application it can answer for, and
			// the git source is the one Argo does not schema-check as tightly.
			name: "a source without a chart is not type-checked",
			objects: []runtime.Object{argoApp(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: "cert-manager"
  namespace: argocd
spec:
  sources:
    - repoURL: 'https://github.com/example/aicr-bundles.git'
      targetRevision: 1.20
      ref: values
    - repoURL: https://charts.jetstack.io
      chart: cert-manager
      targetRevision: 1.20.2
  destination:
    namespace: "cert-manager"
`)},
			want: []installedRelease{wantCertManager},
		},
		{
			// Another team's Application, well-formed for its own purposes and
			// missing the one field this reader insists on. Failing the run on
			// it would be this reader's bug, not that team's.
			name: "foreign application without a destination namespace is skipped",
			objects: []runtime.Object{
				argoApp(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: "their-platform-crds"
  namespace: argocd
spec:
  source:
    repoURL: 'https://github.com/example/their-app.git'
    targetRevision: main
    path: crds
  destination:
    server: "https://kubernetes.default.svc"
`),
				argoApp(t, certManagerApp),
			},
			want: []installedRelease{wantCertManager},
		},
		{
			// Every other malformed shape is equally out of this read's
			// business once the name is foreign.
			name: "foreign application with unreadable sources is skipped",
			objects: []runtime.Object{
				argoAppWith(t, certManagerApp, func(app *unstructured.Unstructured) {
					app.SetName("their-app")
					if err := unstructured.SetNestedField(app.Object,
						map[string]any{"chart": "theirs"}, "spec", "sources"); err != nil {
						t.Fatalf("set sources: %v", err)
					}
				}),
				argoApp(t, gpuOperatorApp),
			},
			want: []installedRelease{{
				Source:       sourceArgo,
				Name:         "gpu-operator",
				Namespace:    "gpu-operator",
				ChartName:    "gpu-operator",
				ChartVersion: "v25.10.0",
			}},
		},
		{
			// The bundle writer names an injected folder "<component>-<phase>",
			// and that is the Application name Argo holds.
			name: "injected folder applications are in their component's scope",
			objects: []runtime.Object{
				argoAppWith(t, nodewrightApp, func(app *unstructured.Unstructured) {
					app.SetName("gpu-operator-readiness")
				}),
				argoAppWith(t, nodewrightApp, func(app *unstructured.Unstructured) {
					app.SetName("gpu-operator-pre")
				}),
				argoAppWith(t, nodewrightApp, func(app *unstructured.Unstructured) {
					app.SetName("gpu-operator-post")
				}),
			},
			want: []installedRelease{
				{Source: sourceArgo, Name: "gpu-operator-post", Namespace: "skyhook"},
				{Source: sourceArgo, Name: "gpu-operator-pre", Namespace: "skyhook"},
				{Source: sourceArgo, Name: "gpu-operator-readiness", Namespace: "skyhook"},
			},
		},
		{
			name: "application naming nothing at all is counted, not fatal",
			page: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				*argoAppWith(t, certManagerApp, func(app *unstructured.Unstructured) {
					unstructured.RemoveNestedField(app.Object, "metadata", "name")
					unstructured.RemoveNestedField(app.Object, "spec", "destination", "namespace")
				}),
				*argoApp(t, gpuOperatorApp),
			}},
			want: []installedRelease{{
				Source:       sourceArgo,
				Name:         "gpu-operator",
				Namespace:    "gpu-operator",
				ChartName:    "gpu-operator",
				ChartVersion: "v25.10.0",
			}},
			wantUnattributed: 1,
		},
		{
			// tenant-a-cert-manager is what --set deployer:namePrefix=tenant-a-
			// produces, and it is only a possible match, so an Application in
			// that shape that cannot be read is counted rather than fatal. A
			// foreign workload sharing a token lands in the same tier, and
			// this reader cannot tell the two apart.
			name: "malformed possible application is skipped and counted",
			objects: []runtime.Object{
				argoAppWith(t, certManagerApp, func(app *unstructured.Unstructured) {
					app.SetName("tenant-a-cert-manager")
					unstructured.RemoveNestedField(app.Object, "spec", "destination", "namespace")
				}),
				argoApp(t, gpuOperatorApp),
			},
			want: []installedRelease{{
				Source:       sourceArgo,
				Name:         "gpu-operator",
				Namespace:    "gpu-operator",
				ChartName:    "gpu-operator",
				ChartVersion: "v25.10.0",
			}},
			wantUnreadable: 1,
		},
		{
			name: "possible application with unreadable sources is skipped and counted",
			objects: []runtime.Object{
				argoAppWith(t, certManagerApp, func(app *unstructured.Unstructured) {
					app.SetName("tenantcert-manager")
					if err := unstructured.SetNestedField(app.Object,
						map[string]any{"chart": "theirs"}, "spec", "sources"); err != nil {
						t.Fatalf("set sources: %v", err)
					}
				}),
			},
			want:           nil,
			wantUnreadable: 1,
		},
		{
			// The confident tier is unchanged: an injected folder is a name
			// this project wrote, so a broken one still fails the run.
			name: "malformed injected-folder application still fails the run",
			objects: []runtime.Object{
				argoAppWith(t, certManagerApp, func(app *unstructured.Unstructured) {
					app.SetName("cert-manager-post")
					unstructured.RemoveNestedField(app.Object, "spec", "destination", "namespace")
				}),
			},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"cert-manager-post", "spec.destination.namespace"},
		},
		{
			name:            "an empty scope is refused, not answered",
			objects:         []runtime.Object{argoApp(t, certManagerApp)},
			within:          newScope(),
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInvalidRequest,
			wantErrContains: []string{"no components"},
		},
		{
			name:    "no applications",
			objects: nil,
			want:    nil,
		},
		{
			name:    "argo CRDs are absent",
			listErr: argoNoMatch(),
			want:    nil,
		},
		{
			name:    "argo CRDs return not found",
			listErr: argoNotFound(),
			want:    nil,
		},
		{
			name:            "listing applications is forbidden",
			listErr:         argoForbidden(),
			wantErr:         true,
			wantErrCode:     errors.ErrCodeUnauthorized,
			wantErrContains: []string{"list", "applications.argoproj.io", "cluster scope"},
			wantErrContext:  map[string]any{"resource": "applications.argoproj.io"},
		},
		{
			name:            "the paged list outlives the apiserver's window",
			listErr:         apierrors.NewResourceExpired("continue token expired"),
			wantErr:         true,
			wantErrCode:     errors.ErrCodeUnavailable,
			wantErrContains: []string{"applications.argoproj.io", "re-run"},
			wantErrContext:  map[string]any{"resource": "applications.argoproj.io"},
		},
		{
			name:            "listing applications runs out of deadline",
			listErr:         fmt.Errorf("client rate limiter Wait: %w", context.DeadlineExceeded),
			wantErr:         true,
			wantErrCode:     errors.ErrCodeTimeout,
			wantErrContains: []string{"Argo CD Application"},
			wantErrContext:  map[string]any{"resource": "applications.argoproj.io"},
		},
		{
			name:            "listing applications fails for a reason that is not RBAC",
			listErr:         apierrors.NewServiceUnavailable("the server is currently unable to handle the request"),
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"Argo CD Application"},
			wantErrContext:  map[string]any{"resource": "applications.argoproj.io"},
		},
		{
			// Skipping it would report its component as newly added, which is
			// the verdict this command exists to get right.
			name: "application without a destination namespace",
			objects: []runtime.Object{argoAppWith(t, certManagerApp, func(app *unstructured.Unstructured) {
				unstructured.RemoveNestedField(app.Object, "spec", "destination", "namespace")
			})},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"cert-manager", "argocd", "spec.destination.namespace"},
			wantErrContext: map[string]any{
				"object":    "cert-manager",
				"namespace": "argocd",
				"resource":  "applications.argoproj.io",
			},
		},
		{
			name: "application with an empty destination namespace",
			objects: []runtime.Object{argoAppWith(t, certManagerApp, func(app *unstructured.Unstructured) {
				if err := unstructured.SetNestedField(app.Object, "", "spec", "destination", "namespace"); err != nil {
					t.Fatalf("set destination namespace: %v", err)
				}
			})},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"cert-manager", "spec.destination.namespace"},
		},
		{
			name: "destination namespace that is not a string",
			objects: []runtime.Object{argoAppWith(t, certManagerApp, func(app *unstructured.Unstructured) {
				if err := unstructured.SetNestedField(app.Object, int64(7),
					"spec", "destination", "namespace"); err != nil {
					t.Fatalf("set destination namespace: %v", err)
				}
			})},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"cert-manager", "spec.destination.namespace", "string"},
		},
		{
			name: "spec.sources that is not a list",
			objects: []runtime.Object{argoAppWith(t, certManagerApp, func(app *unstructured.Unstructured) {
				if err := unstructured.SetNestedField(app.Object,
					map[string]any{"chart": "cert-manager"}, "spec", "sources"); err != nil {
					t.Fatalf("set sources: %v", err)
				}
			})},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"cert-manager", "spec.sources", "list"},
			wantErrContext: map[string]any{
				"object":    "cert-manager",
				"namespace": "argocd",
				"resource":  "applications.argoproj.io",
			},
		},
		{
			name: "spec.source that is not an object",
			objects: []runtime.Object{argoAppWith(t, nodewrightApp, func(app *unstructured.Unstructured) {
				if err := unstructured.SetNestedField(app.Object, "main", "spec", "source"); err != nil {
					t.Fatalf("set source: %v", err)
				}
			})},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"nodewright-customizations", "spec.source"},
		},
		{
			name: "spec.sources entry that is not an object",
			objects: []runtime.Object{argoAppWith(t, certManagerApp, func(app *unstructured.Unstructured) {
				if err := unstructured.SetNestedSlice(app.Object,
					[]any{"https://charts.jetstack.io"}, "spec", "sources"); err != nil {
					t.Fatalf("set sources: %v", err)
				}
			})},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"cert-manager", "spec.sources[0]"},
		},
		{
			// YAML pins written unquoted are the realistic way a chart version
			// stops being a string. Reading past it would report no version.
			name: "targetRevision that is not a string",
			objects: []runtime.Object{argoAppWith(t, gpuOperatorApp, func(app *unstructured.Unstructured) {
				if err := unstructured.SetNestedField(app.Object, float64(1.2),
					"spec", "source", "targetRevision"); err != nil {
					t.Fatalf("set targetRevision: %v", err)
				}
			})},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"gpu-operator", "spec.source.targetRevision", "string"},
		},
		{
			name: "chart that is not a string",
			objects: []runtime.Object{argoAppWith(t, gpuOperatorApp, func(app *unstructured.Unstructured) {
				if err := unstructured.SetNestedField(app.Object, int64(3),
					"spec", "source", "chart"); err != nil {
					t.Fatalf("set chart: %v", err)
				}
			})},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"gpu-operator", "spec.source.chart", "string"},
		},
		{
			// The page arrives deliberately unsorted, so the order below is
			// the reader's doing and not the apiserver's.
			name: "applications are returned sorted by name, then by namespace",
			page: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				*argoApp(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: "zeta", namespace: argocd}
spec: {source: {path: 003-zeta}, destination: {namespace: "zeta-ns"}}
`),
				*argoAppWith(t, gpuOperatorApp, func(app *unstructured.Unstructured) {
					app.SetNamespace("argocd-prod")
					if err := unstructured.SetNestedField(app.Object, "tenant-b",
						"spec", "destination", "namespace"); err != nil {
						t.Fatalf("set destination namespace: %v", err)
					}
				}),
				*argoApp(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: "alpha", namespace: argocd}
spec: {source: {path: 001-alpha}, destination: {namespace: "alpha-ns"}}
`),
				*argoAppWith(t, gpuOperatorApp, func(app *unstructured.Unstructured) {
					if err := unstructured.SetNestedField(app.Object, "tenant-a",
						"spec", "destination", "namespace"); err != nil {
						t.Fatalf("set destination namespace: %v", err)
					}
				}),
			}},
			want: []installedRelease{
				{Source: sourceArgo, Name: "alpha", Namespace: "alpha-ns"},
				{Source: sourceArgo, Name: "gpu-operator", Namespace: "tenant-a", ChartName: "gpu-operator", ChartVersion: "v25.10.0"},
				{Source: sourceArgo, Name: "gpu-operator", Namespace: "tenant-b", ChartName: "gpu-operator", ChartVersion: "v25.10.0"},
				{Source: sourceArgo, Name: "zeta", Namespace: "zeta-ns"},
			},
		},
		{
			// The name and the destination namespace do not identify an Argo
			// Application, so this pair is orderable only by the order the
			// apiserver returned it in. This pins that order; it cannot pin
			// the stable sort that guarantees it, because sort.Slice is
			// deterministic for a given input today and so answers the same
			// way. The guarantee is what survives a change to Go's sort.
			name: "same-named applications against one destination keep list order",
			page: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				*argoAppWith(t, gpuOperatorApp, func(app *unstructured.Unstructured) {
					app.SetNamespace("argocd-prod")
					if err := unstructured.SetNestedField(app.Object, "v25.7.0",
						"spec", "source", "targetRevision"); err != nil {
						t.Fatalf("set targetRevision: %v", err)
					}
				}),
				*argoApp(t, gpuOperatorApp),
			}},
			want: []installedRelease{
				{Source: sourceArgo, Name: "gpu-operator", Namespace: "gpu-operator", ChartName: "gpu-operator", ChartVersion: "v25.7.0"},
				{Source: sourceArgo, Name: "gpu-operator", Namespace: "gpu-operator", ChartName: "gpu-operator", ChartVersion: "v25.10.0"},
			},
		},
		{
			name:            "canceled context",
			objects:         []runtime.Object{argoApp(t, certManagerApp)},
			cancelContext:   true,
			wantErr:         true,
			wantErrCode:     errors.ErrCodeTimeout,
			wantErrContains: []string{"Argo CD Application"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := argoClient(tt.objects...)
			switch {
			case tt.listErr != nil:
				client.PrependReactor("list", "applications",
					func(_ k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, tt.listErr
					})
			case tt.page != nil:
				client.PrependReactor("list", "applications",
					func(_ k8stesting.Action) (bool, runtime.Object, error) {
						return true, tt.page, nil
					})
			}

			ctx := context.Background()
			if tt.cancelContext {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}

			within := tt.within
			if within == nil {
				within = argoTestScope()
			}

			read, err := argoApplications(ctx, client, within)
			if (err != nil) != tt.wantErr {
				t.Fatalf("argoApplications() error = %v, wantErr %v", err, tt.wantErr)
			}
			got := read.Releases
			if tt.wantErr {
				if !reflect.DeepEqual(read, inventoryRead{}) {
					t.Errorf("argoApplications() returned %+v alongside an error", read)
				}
				assertError(t, err, tt.wantErrCode, tt.wantErrContext)
				for _, want := range tt.wantErrContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}

				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("argoApplications() = %+v, want %+v", got, tt.want)
			}
			if read.Unattributed != tt.wantUnattributed {
				t.Errorf("argoApplications() counted %d unattributable items, want %d",
					read.Unattributed, tt.wantUnattributed)
			}
			if read.Unreadable != tt.wantUnreadable {
				t.Errorf("argoApplications() counted %d unreadable items, want %d",
					read.Unreadable, tt.wantUnreadable)
			}
		})
	}
}

// argoTestScope names the components the fixtures in this file deploy.
// Anything outside it stands in for the rest of a real cluster.
func argoTestScope() scope {
	return newScope("cert-manager", "gpu-operator", "my-kustomize-app", "nodewright-customizations",
		"kai-scheduler", "alpha", "zeta")
}

// TestArgoApplicationsFollowsListPages proves the walk keeps listing while the
// server hands back a continue token, that the fold spans pages, and that a
// page which fails after the first one is never mistaken for an absent CRD.
func TestArgoApplicationsFollowsListPages(t *testing.T) {
	page := func(token string, apps ...*unstructured.Unstructured) *unstructured.UnstructuredList {
		list := &unstructured.UnstructuredList{}
		list.SetContinue(token)
		for _, app := range apps {
			list.Items = append(list.Items, *app)
		}

		return list
	}

	wantGPUOperator := installedRelease{
		Source:       sourceArgo,
		Name:         "gpu-operator",
		Namespace:    "gpu-operator",
		ChartName:    "gpu-operator",
		ChartVersion: "v25.10.0",
	}

	tests := []struct {
		name string
		// answers are returned one per List, in order.
		answers         []*unstructured.UnstructuredList
		lastErr         error
		wantLists       int
		want            []installedRelease
		wantErr         bool
		wantErrCode     errors.ErrorCode
		wantErrContains []string
	}{
		{
			name: "a second page is read and folded in",
			answers: []*unstructured.UnstructuredList{
				page("page-2", argoApp(t, gpuOperatorApp)),
				page("", argoApp(t, certManagerApp)),
			},
			wantLists: 2,
			want:      []installedRelease{wantCertManager, wantGPUOperator},
		},
		{
			// A CRD cannot vanish between pages, so the answer is a failure
			// and not the empty inventory an absent CRD produces. Returning
			// nothing here would drop the first page's Applications.
			name:            "not found after the first page is a failure, not an absent CRD",
			answers:         []*unstructured.UnstructuredList{page("page-2", argoApp(t, gpuOperatorApp))},
			lastErr:         argoNotFound(),
			wantLists:       2,
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"Argo CD Application"},
		},
		{
			name: "a repeated continue token is refused",
			answers: []*unstructured.UnstructuredList{
				page("stuck", argoApp(t, gpuOperatorApp)),
				page("stuck", argoApp(t, certManagerApp)),
			},
			// Two Lists: the first hands out the token, the second repeats it.
			wantLists:       2,
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"continue token", "applications.argoproj.io"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := argoClient()
			listed := 0
			client.PrependReactor("list", "applications", func(_ k8stesting.Action) (bool, runtime.Object, error) {
				listed++
				if listed > len(tt.answers) {
					if tt.lastErr != nil {
						return true, nil, tt.lastErr
					}

					return true, nil, stderrors.New("listed again after the last page")
				}

				return true, tt.answers[listed-1], nil
			})

			read, err := argoApplications(context.Background(), client, argoTestScope())
			if (err != nil) != tt.wantErr {
				t.Fatalf("argoApplications() error = %v, wantErr %v", err, tt.wantErr)
			}
			got := read.Releases
			if listed != tt.wantLists {
				t.Errorf("listed %d times, want %d", listed, tt.wantLists)
			}
			if tt.wantErr {
				if !reflect.DeepEqual(read, inventoryRead{}) {
					t.Errorf("argoApplications() returned %+v alongside an error", read)
				}
				assertError(t, err, tt.wantErrCode, nil)
				for _, want := range tt.wantErrContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}

				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("argoApplications() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestArgoApplicationsCancellationBetweenPages covers the check a canceled
// context meets after the first page has already been read, which the
// single-page cases cannot reach.
func TestArgoApplicationsCancellationBetweenPages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	client := argoClient()
	listed := 0
	client.PrependReactor("list", "applications", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		listed++
		list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*argoApp(t, gpuOperatorApp)}}
		list.SetContinue(fmt.Sprintf("page-%d", listed+1))
		cancel()

		return true, list, nil
	})

	read, err := argoApplications(ctx, client, argoTestScope())
	if err == nil {
		t.Fatalf("argoApplications() = %+v, want an error", read)
	}
	if !reflect.DeepEqual(read, inventoryRead{}) {
		t.Errorf("argoApplications() returned %+v alongside an error", read)
	}
	if listed != 1 {
		t.Errorf("listed %d times after cancellation, want 1", listed)
	}
	assertError(t, err, errors.ErrCodeTimeout, nil)
}

// TestArgoApplicationsEchoesPagingOptions serves the Applications over HTTP,
// through a real dynamic client, because that is the only place the paging
// request is observable: the fake client discards Limit and Continue before
// any reactor runs. Without it, dropping the Limit entirely still passes every
// other test here.
func TestArgoApplicationsEchoesPagingOptions(t *testing.T) {
	pages := []*unstructured.UnstructuredList{
		{Items: []unstructured.Unstructured{*argoApp(t, gpuOperatorApp)}},
		{Items: []unstructured.Unstructured{*argoApp(t, certManagerApp)}},
	}
	pages[0].SetContinue("page-2")

	var mu sync.Mutex
	var queries []url.Values
	served := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/argoproj.io/v1alpha1/applications" {
			http.NotFound(w, r)

			return
		}
		w.Header().Set("Content-Type", "application/json")

		mu.Lock()
		queries = append(queries, r.URL.Query())
		page := pages[min(served, len(pages)-1)]
		served++
		mu.Unlock()

		page.SetAPIVersion("argoproj.io/v1alpha1")
		page.SetKind("ApplicationList")
		if err := json.NewEncoder(w).Encode(page); err != nil {
			t.Errorf("encode application page: %v", err)
		}
	}))
	defer server.Close()

	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("dynamic client for %s: %v", server.URL, err)
	}

	read, err := argoApplications(context.Background(), client, argoTestScope())
	if err != nil {
		t.Fatalf("argoApplications() error = %v", err)
	}
	got := read.Releases
	if read.Unattributed != 0 {
		t.Errorf("counted %d unattributable items, want 0", read.Unattributed)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(queries) != len(pages) {
		t.Fatalf("served %d requests, want %d", len(queries), len(pages))
	}
	wantLimit := strconv.FormatInt(defaults.ArgoApplicationListPageSize, 10)
	for i, query := range queries {
		if query.Get("limit") != wantLimit {
			t.Errorf("request %d sent limit=%q, want %q", i+1, query.Get("limit"), wantLimit)
		}
	}
	if token := queries[0].Get("continue"); token != "" {
		t.Errorf("first request sent continue=%q, want it unset", token)
	}
	if token := queries[1].Get("continue"); token != "page-2" {
		t.Errorf("second request sent continue=%q, want %q", token, "page-2")
	}

	want := []installedRelease{wantCertManager, {
		Source:       sourceArgo,
		Name:         "gpu-operator",
		Namespace:    "gpu-operator",
		ChartName:    "gpu-operator",
		ChartVersion: "v25.10.0",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argoApplications() = %+v, want %+v", got, want)
	}
}
