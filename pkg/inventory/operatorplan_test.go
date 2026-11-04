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
	"reflect"
	"strconv"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

const (
	opDigestDeployed = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	opDigestNext     = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	opDigestWrapper  = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	opPlanDigestA    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	opPlanDigestB    = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	testOperatorNamespace = "aicr-system"
	opWrapperVersion      = "v1.2.0"
)

// operatorApp is an Application as the AICR Operator writes it: an OCI chart
// repository at path ".", pinned by digest, with no chart field.
func operatorApp(name, namespace, pinned, planDigest string) *unstructured.Unstructured {
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"spec": map[string]any{
			"project": "aicr",
			"source": map[string]any{
				"repoURL":        "oci://aicr-registry.aicr-system.svc.cluster.local:5000/charts/" + name,
				"path":           ".",
				"targetRevision": pinned,
			},
			"destination": map[string]any{
				"server":    argoInClusterServer,
				"namespace": namespace,
			},
		},
	}}
	app.SetName(name)
	app.SetNamespace("argocd")
	app.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": "aicr-operator",
		"operator.aicr.run/runtime":    "default",
	})
	app.SetAnnotations(map[string]string{
		"operator.aicr.run/release-id":  name,
		"operator.aicr.run/plan-digest": planDigest,
	})

	return app
}

// syncedAt marks app Synced against the given digest of its own repository.
func syncedAt(t *testing.T, app *unstructured.Unstructured, digest string) *unstructured.Unstructured {
	t.Helper()
	source, _, _ := unstructured.NestedMap(app.Object, "spec", "source")
	source["targetRevision"] = digest
	sync := map[string]any{"status": argoSynced, "revision": digest, "comparedTo": map[string]any{"source": source}}
	if err := unstructured.SetNestedField(app.Object, sync, "status", "sync"); err != nil {
		t.Fatalf("set status.sync: %v", err)
	}

	return app
}

// outOfSyncFrom marks app OutOfSync, with its last completed sync at digest.
func outOfSyncFrom(t *testing.T, app *unstructured.Unstructured, digest string) *unstructured.Unstructured {
	t.Helper()
	source, _, _ := unstructured.NestedMap(app.Object, "spec", "source")
	source["targetRevision"] = digest
	if err := unstructured.SetNestedField(app.Object, "OutOfSync", "status", "sync", "status"); err != nil {
		t.Fatalf("set status.sync.status: %v", err)
	}
	history := []any{map[string]any{"id": int64(1), "revision": digest, "source": source}}
	if err := unstructured.SetNestedSlice(app.Object, history, "status", "history"); err != nil {
		t.Fatalf("set status.history: %v", err)
	}

	return app
}

// opRelease is the part of a plan release the inventory reads.
type opRelease struct {
	id      string
	digest  string
	version string
}

// planConfigMap is a plan ConfigMap in the operator's namespace, of the schema
// version this package reads.
func planConfigMap(t *testing.T, name string, releases ...opRelease) *unstructured.Unstructured {
	t.Helper()
	list := make([]any, 0, len(releases))
	for _, r := range releases {
		chart := map[string]any{"digest": r.digest}
		if r.version != "" {
			chart["version"] = r.version
		}
		list = append(list, map[string]any{"id": r.id, "chart": chart})
	}
	doc, err := json.Marshal(map[string]any{
		"schemaVersion": operatorPlanSchema,
		"provenance":    map[string]any{"source": "Resolved"},
		"releases":      list,
	})
	if err != nil {
		t.Fatalf("encode plan: %v", err)
	}

	return planConfigMapData(name, strconv.Itoa(operatorPlanSchema), string(doc))
}

// rebuiltPlanConfigMap is the plan aicr-plan-aaaaaaaaaaaaaaaa as the operator
// rebuilds it from its Applications: chart digests and no versions.
func rebuiltPlanConfigMap(t *testing.T, releases ...opRelease) *unstructured.Unstructured {
	t.Helper()
	list := make([]any, 0, len(releases))
	for _, r := range releases {
		list = append(list, map[string]any{"id": r.id, "chart": map[string]any{"digest": r.digest}})
	}
	doc, err := json.Marshal(map[string]any{
		"schemaVersion": operatorPlanSchema,
		"provenance":    map[string]any{"source": "Rebuilt"},
		"releases":      list,
	})
	if err != nil {
		t.Fatalf("encode plan: %v", err)
	}

	return planConfigMapData("aicr-plan-aaaaaaaaaaaaaaaa", strconv.Itoa(operatorPlanSchema), string(doc))
}

// inNamespace moves a fixture to another namespace.
func inNamespace(object *unstructured.Unstructured, namespace string) *unstructured.Unstructured {
	object.SetNamespace(namespace)

	return object
}

// planConfigMapData is a plan ConfigMap with the given schema label and raw
// plan.json, for documents planConfigMap cannot produce.
func planConfigMapData(name, schemaLabel, document string) *unstructured.Unstructured {
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"data":       map[string]any{"plan.json": document},
	}}
	cm.SetName(name)
	cm.SetNamespace(testOperatorNamespace)
	cm.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by":     "aicr-operator",
		"operator.aicr.run/role":           "plan",
		"operator.aicr.run/schema-version": schemaLabel,
	})

	return cm
}

// configMapsListed counts the Lists of ConfigMaps a client served.
func configMapsListed(client *k8stesting.Fake) int {
	n := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "configmaps" {
			n++
		}
	}

	return n
}

// TestArgoApplicationsVersionsOperatorApplications pins how an Application
// the AICR Operator wrote gets its version: from the plan release whose chart
// digest is the one Argo CD reports deployed.
func TestArgoApplicationsVersionsOperatorApplications(t *testing.T) {
	gpuOperatorAt := func(t *testing.T, version string) installedRelease {
		t.Helper()

		return installedRelease{Source: sourceArgo, Name: "gpu-operator", Namespace: "gpu-operator", ChartVersion: version}
	}

	tests := []struct {
		name        string
		objects     func(t *testing.T) []runtime.Object
		listErr     error
		want        []installedRelease
		wantCode    errors.ErrorCode
		wantInError []string
		notInError  []string
		wantLists   int
	}{
		{
			name: "a synced Application reads the version its plan records for the deployed digest",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
					planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa", opRelease{"gpu-operator", opDigestDeployed, "v26.7.2"}),
				}
			},
			want:      []installedRelease{gpuOperatorAt(t, "v26.7.2")},
			wantLists: 1,
		},
		{
			// The pin and the plan-digest annotation already name the next
			// plan, whose sync has not completed: the old chart still runs.
			name: "a rollout in progress reads the deployed digest, not the pin",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					outOfSyncFrom(t, operatorApp("gpu-operator", "gpu-operator", opDigestNext, opPlanDigestB), opDigestDeployed),
					planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa", opRelease{"gpu-operator", opDigestDeployed, "v26.7.2"}),
					planConfigMap(t, "aicr-plan-bbbbbbbbbbbbbbbb", opRelease{"gpu-operator", opDigestNext, "v26.10.0"}),
				}
			},
			want:      []installedRelease{gpuOperatorAt(t, "v26.7.2")},
			wantLists: 1,
		},
		{
			name: "a wrapper release reads its own chart version",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator-readiness", "gpu-operator", opDigestWrapper, opPlanDigestA), opDigestWrapper),
					planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa", opRelease{"gpu-operator-readiness", opDigestWrapper, opWrapperVersion}),
				}
			},
			want:      []installedRelease{{Source: sourceArgo, Name: "gpu-operator-readiness", Namespace: "gpu-operator", ChartVersion: opWrapperVersion}},
			wantLists: 1,
		},
		{
			name: "an Application with no completed sync stays unversioned without reading plans",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA)}
			},
			listErr: apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", nil),
			want:    []installedRelease{gpuOperatorAt(t, "")},
		},
		{
			name: "plans are read once for every operator Application",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
					syncedAt(t, operatorApp("gpu-operator-readiness", "gpu-operator", opDigestWrapper, opPlanDigestA), opDigestWrapper),
					planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa",
						opRelease{"gpu-operator", opDigestDeployed, "v26.7.2"},
						opRelease{"gpu-operator-readiness", opDigestWrapper, opWrapperVersion}),
				}
			},
			want: []installedRelease{
				gpuOperatorAt(t, "v26.7.2"),
				{Source: sourceArgo, Name: "gpu-operator-readiness", Namespace: "gpu-operator", ChartVersion: opWrapperVersion},
			},
			wantLists: 1,
		},
		{
			name: "an Application the operator did not write never reads plans",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{argoApp(t, certManagerApp)}
			},
			listErr: apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", nil),
			want:    []installedRelease{wantCertManager},
		},
		{
			name: "a readable plan answers even when another plan cannot be read",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
					planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa", opRelease{"gpu-operator", opDigestDeployed, "v26.7.2"}),
					planConfigMapData("aicr-plan-bbbbbbbbbbbbbbbb", "2", `{"schemaVersion":2,"releases":[]}`),
				}
			},
			want:      []installedRelease{gpuOperatorAt(t, "v26.7.2")},
			wantLists: 1,
		},
		{
			name: "no plan recording the deployed digest fails and names the alternatives",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestB), opDigestDeployed),
					planConfigMap(t, "aicr-plan-bbbbbbbbbbbbbbbb", opRelease{"gpu-operator", opDigestNext, "v26.10.0"}),
				}
			},
			wantCode:    errors.ErrCodeNotFound,
			wantInError: []string{`"gpu-operator"`, opDigestDeployed, "--from <bundle>", "kubectl aicr reproduce", "kubectl aicr describe"},
			wantLists:   1,
		},
		{
			name: "no plans at all fails and names the alternatives",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
				}
			},
			wantCode:    errors.ErrCodeNotFound,
			wantInError: []string{"namespace aicr-system holds no AICR Operator plans", "--operator-namespace", "--from <bundle>"},
			notInError:  []string{"kubectl aicr reproduce", "kubectl aicr describe"},
			wantLists:   1,
		},
		{
			name: "plans that cannot be read are named when nothing resolves",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
					planConfigMapData("aicr-plan-aaaaaaaaaaaaaaaa", "2", `{"schemaVersion":2,"releases":[]}`),
					planConfigMapData("aicr-plan-bbbbbbbbbbbbbbbb", "1", `{"schemaVersion":1,"releases":`),
				}
			},
			wantCode: errors.ErrCodeNotFound,
			wantInError: []string{
				"aicr-system/aicr-plan-aaaaaaaaaaaaaaaa", "schema version",
				"aicr-system/aicr-plan-bbbbbbbbbbbbbbbb", "--from <bundle>",
			},
			wantLists: 1,
		},
		{
			name: "plans disagreeing on the version of one digest fail",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
					planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa", opRelease{"gpu-operator", opDigestDeployed, "v26.7.2"}),
					planConfigMap(t, "aicr-plan-bbbbbbbbbbbbbbbb", opRelease{"gpu-operator", opDigestDeployed, "v26.7.3"}),
				}
			},
			wantCode:    errors.ErrCodeConflict,
			wantInError: []string{"v26.7.2", "v26.7.3"},
			wantLists:   1,
		},
		{
			name: "a denied plan read names the grant and the alternatives",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
				}
			},
			listErr:     apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", nil),
			wantCode:    errors.ErrCodeUnauthorized,
			wantInError: []string{"list configmaps in namespace aicr-system", "--from <bundle>"},
			wantLists:   1,
		},
		{
			// Only the operator's namespace is guarded by its admission
			// policy, so a plan anywhere else could be anyone's.
			name: "a plan outside the operator namespace is not read",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
					inNamespace(planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa",
						opRelease{"gpu-operator", opDigestDeployed, "v26.7.2"}), "tenant"),
				}
			},
			wantCode:    errors.ErrCodeNotFound,
			wantInError: []string{"namespace aicr-system holds no AICR Operator plans", "--operator-namespace"},
			wantLists:   1,
		},
		{
			name: "a plan rebuilt from Applications records no version",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
					rebuiltPlanConfigMap(t, opRelease{id: "gpu-operator", digest: opDigestDeployed}),
				}
			},
			wantCode:    errors.ErrCodeNotFound,
			wantInError: []string{"no resolved AICR Operator plan", "rebuilt from its Applications records no chart versions", "--from <bundle>"},
			notInError:  []string{"kubectl aicr reproduce", "kubectl aicr describe"},
			wantLists:   1,
		},
		{
			// A rebuilt plan shares its digest, and so its name, with the
			// resolved plan it replaced; a retained resolved plan of another
			// digest still records the release.
			name: "a resolved plan answers beside a rebuilt plan recording the same chart",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
					rebuiltPlanConfigMap(t, opRelease{id: "gpu-operator", digest: opDigestDeployed}),
					planConfigMap(t, "aicr-plan-bbbbbbbbbbbbbbbb", opRelease{"gpu-operator", opDigestDeployed, "v26.7.2"}),
				}
			},
			want:      []installedRelease{gpuOperatorAt(t, "v26.7.2")},
			wantLists: 1,
		},
		{
			// Rebuilt from an Application whose last sync failed, the plan
			// records the pin rather than the digest still deployed.
			name: "rebuilt plans alone recording another digest do not route to the operator's state",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					outOfSyncFrom(t, operatorApp("gpu-operator", "gpu-operator", opDigestNext, opPlanDigestA), opDigestDeployed),
					rebuiltPlanConfigMap(t, opRelease{id: "gpu-operator", digest: opDigestNext}),
				}
			},
			wantCode:    errors.ErrCodeNotFound,
			wantInError: []string{"no resolved AICR Operator plan", "--from <bundle>"},
			notInError:  []string{"kubectl aicr reproduce", "kubectl aicr describe"},
			wantLists:   1,
		},
		{
			name: "a digest no plan records is reported with the plans that were read",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
					rebuiltPlanConfigMap(t, opRelease{id: "gpu-operator", digest: opDigestWrapper}),
					planConfigMap(t, "aicr-plan-bbbbbbbbbbbbbbbb", opRelease{"gpu-operator", opDigestNext, "v26.10.0"}),
				}
			},
			wantCode:    errors.ErrCodeNotFound,
			wantInError: []string{"1 readable, 1 rebuilt from Argo CD Applications", "kubectl aicr reproduce"},
			wantLists:   1,
		},
		{
			name: "a plan recording an implausibly long version is set aside",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
					planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa",
						opRelease{"gpu-operator", opDigestDeployed, "v" + strings.Repeat("1", 300)}),
				}
			},
			wantCode:    errors.ErrCodeNotFound,
			wantInError: []string{"aicr-system/aicr-plan-aaaaaaaaaaaaaaaa", "version longer than"},
			wantLists:   1,
		},
		{
			name: "a deployed revision that is not a digest fails",
			objects: func(t *testing.T) []runtime.Object {
				return []runtime.Object{
					syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), "v26.7.2"),
				}
			},
			wantCode:    errors.ErrCodeInternal,
			wantInError: []string{`"gpu-operator"`, "sha256 digest"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := argoClient(tt.objects(t)...)
			if tt.listErr != nil {
				client.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.listErr
				})
			}

			got, err := argoApplications(context.Background(), client, argoTestScope(), testOperatorNamespace)
			if lists := configMapsListed(&client.Fake); lists != tt.wantLists {
				t.Errorf("listed ConfigMaps %d times, want %d", lists, tt.wantLists)
			}
			if tt.wantCode != "" {
				if err == nil {
					t.Fatalf("argoApplications() = %+v, want an error", got.Releases)
				}
				if code := errorstest.ReportedCode(err); code != tt.wantCode {
					t.Errorf("error code = %s, want %s (%v)", code, tt.wantCode, err)
				}
				for _, want := range tt.wantInError {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}
				for _, unwanted := range tt.notInError {
					if strings.Contains(err.Error(), unwanted) {
						t.Errorf("error %q suggests %q, which cannot help here", err, unwanted)
					}
				}

				return
			}
			if err != nil {
				t.Fatalf("argoApplications() error = %v", err)
			}
			if !reflect.DeepEqual(got.Releases, tt.want) {
				t.Errorf("releases = %+v, want %+v", got.Releases, tt.want)
			}
		})
	}
}

// TestOperatorPlanReadsWhatTheOperatorWrites builds the Application and its
// plan with the operator's own writers, so a change to the labels,
// annotations, data key or document shape the read depends on fails here
// rather than on a cluster.
func TestOperatorPlanReadsWhatTheOperatorWrites(t *testing.T) {
	target := argocd.Target{Namespace: "argocd", Project: "aicr", RuntimeName: "default"}
	valuesJSON := []byte(`{}`)
	values := plan.ValuesContent("gpu-operator", valuesJSON)
	doc := &plan.Document{
		SchemaVersion: plan.SchemaVersion,
		RuntimeUID:    "0b8f4a52-5f2e-4b8c-9d3a-2f6c1e7a9b10",
		Engine:        plan.EngineArgoCD,
		EnginePolicy:  "argocd.v1",
		Provenance:    plan.Provenance{Source: plan.SourceResolved, OperatorVersion: "v0.0.0-dev", AICRVersion: "v1.2.0"},
		Recipe: &plan.Recipe{
			Name:         "h100-eks-ubuntu-training",
			ConfigMapRef: plan.DigestedRef{ConfigMap: "aicr-recipe-aaaaaaaaaaaaaaaa", Digest: opPlanDigestB},
		},
		Releases: []plan.Release{{
			ID: "gpu-operator", Component: "gpu-operator", Phase: plan.PhasePrimary, TargetNamespace: "gpu-operator",
			Chart: plan.Chart{
				Source: plan.ChartMirrored, Repository: "oci://registry.example.com/charts/gpu-operator",
				Version: "v26.7.2", Digest: opDigestDeployed,
				Upstream: &plan.UpstreamChart{Repository: "https://helm.ngc.nvidia.com/nvidia", ArchiveDigest: opDigestNext},
			},
			Values:   values.Ref(),
			OwnsCRDs: true,
		}},
	}
	digest, err := plan.ComputeDesiredStateDigest(doc)
	if err != nil {
		t.Fatalf("ComputeDesiredStateDigest() error = %v", err)
	}
	doc.DesiredStateDigest = digest
	app, err := argocd.Application(target, doc, &doc.Releases[0], valuesJSON)
	if err != nil {
		t.Fatalf("Application() error = %v", err)
	}
	app = syncedAt(t, app, opDigestDeployed)
	rebuilt, _, err := argocd.Rebuild(target, doc.RuntimeUID, []unstructured.Unstructured{*app})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	planObject := func(t *testing.T, d *plan.Document) *unstructured.Unstructured {
		t.Helper()
		cm, err := plan.ToConfigMap(d, testOperatorNamespace, target.RuntimeName)
		if err != nil {
			t.Fatalf("ToConfigMap() error = %v", err)
		}
		object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cm)
		if err != nil {
			t.Fatalf("convert plan ConfigMap: %v", err)
		}

		return &unstructured.Unstructured{Object: object}
	}

	tests := []struct {
		name     string
		plan     *plan.Document
		want     string
		wantCode errors.ErrorCode
		wantText string
	}{
		{name: "a resolved plan versions the Application", plan: doc, want: "v26.7.2"},
		{
			name: "a rebuilt plan alone does not", plan: rebuilt, wantCode: errors.ErrCodeNotFound,
			wantText: "rebuilt from its Applications records no chart versions",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := argoClient(app.DeepCopy(), planObject(t, tt.plan))
			got, err := argoApplications(context.Background(), client, argoTestScope(), testOperatorNamespace)
			if tt.wantCode != "" {
				if code := errorstest.ReportedCode(err); code != tt.wantCode {
					t.Fatalf("error code = %s, want %s (%v)", code, tt.wantCode, err)
				}
				if !strings.Contains(err.Error(), tt.wantText) {
					t.Errorf("error %q does not mention %q", err, tt.wantText)
				}

				return
			}
			if err != nil {
				t.Fatalf("argoApplications() error = %v", err)
			}
			if len(got.Releases) != 1 || got.Releases[0].ChartVersion != tt.want {
				t.Errorf("releases = %+v, want gpu-operator at %s", got.Releases, tt.want)
			}
		})
	}
}

// TestReadVersionsAnOperatorManagedComponent drives an operator-managed
// component, with its readiness wrapper beside it, through the whole read.
func TestReadVersionsAnOperatorManagedComponent(t *testing.T) {
	ns := fixtureGPUOperator.Namespace
	dyn := argoClient(
		syncedAt(t, operatorApp("gpu-operator", ns, opDigestDeployed, opPlanDigestA), opDigestDeployed),
		syncedAt(t, operatorApp("gpu-operator-readiness", ns, opDigestWrapper, opPlanDigestA), opDigestWrapper),
		planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa",
			opRelease{"gpu-operator", opDigestDeployed, "v26.7.2"},
			opRelease{"gpu-operator-readiness", opDigestWrapper, opWrapperVersion}),
	)

	got, err := read(context.Background(), fake.NewSimpleClientset(), dyn,
		Options{Deployer: DeployerArgoCD, Components: []Component{fixtureGPUOperator}})
	if err != nil {
		t.Fatalf("read() error = %v", err)
	}
	if want := map[string]string{"gpu-operator": "v26.7.2"}; !reflect.DeepEqual(got.Versions, want) {
		t.Errorf("read() versions = %v, want %v", got.Versions, want)
	}
}

// TestOperatorDeployedDigestRejectsMalformedStatus pins that a status the
// reader cannot type fails rather than reading as no completed sync, which
// would report a deployed component as unversioned.
func TestOperatorDeployedDigestRejectsMalformedStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    map[string]any
		wantField string
	}{
		{"sync status not a string", map[string]any{"sync": map[string]any{"status": int64(1)}}, "status.sync.status"},
		{
			"compared revision not a string",
			map[string]any{"sync": map[string]any{
				"status":     argoSynced,
				"comparedTo": map[string]any{"source": map[string]any{"targetRevision": int64(1)}},
			}},
			"status.sync.comparedTo.source.targetRevision",
		},
		{"history not a list", map[string]any{"history": "none"}, "status.history"},
		{"history entry not an object", map[string]any{"history": []any{"entry"}}, "status.history[0]"},
		{
			"history revision not a string",
			map[string]any{"history": []any{map[string]any{"source": map[string]any{"targetRevision": int64(1)}}}},
			"status.history[0].source.targetRevision",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA)
			app.Object["status"] = tt.status

			digest, err := operatorDeployedDigest(app)
			if err == nil {
				t.Fatalf("operatorDeployedDigest() = %q, want an error", digest)
			}
			if code := errorstest.ReportedCode(err); code != errors.ErrCodeInternal {
				t.Errorf("error code = %s, want %s", code, errors.ErrCodeInternal)
			}
			if !strings.Contains(err.Error(), tt.wantField) {
				t.Errorf("error %q does not name %s", err, tt.wantField)
			}
		})
	}
}

// TestDecodeOperatorPlanSetsAsideWhatItCannotRead pins each reason a plan is
// set aside, since the reason is what an unresolved digest reports.
func TestDecodeOperatorPlanSetsAsideWhatItCannotRead(t *testing.T) {
	tests := []struct {
		name        string
		cm          *unstructured.Unstructured
		wantProblem string
	}{
		{
			name:        "no plan document",
			cm:          planConfigMapData("aicr-plan-aaaaaaaaaaaaaaaa", "1", ""),
			wantProblem: "no plan.json",
		},
		{
			name:        "document schema disagrees with its label",
			cm:          planConfigMapData("aicr-plan-aaaaaaaaaaaaaaaa", "1", `{"schemaVersion":2,"releases":[]}`),
			wantProblem: "schema version 2",
		},
		{
			name: "release without a chart digest",
			cm: planConfigMapData("aicr-plan-aaaaaaaaaaaaaaaa", "1",
				`{"schemaVersion":1,"releases":[{"id":"gpu-operator","chart":{"version":"v26.7.2"}}]}`),
			wantProblem: "release 0",
		},
		{
			name: "release without an ID",
			cm: planConfigMapData("aicr-plan-aaaaaaaaaaaaaaaa", "1",
				`{"schemaVersion":1,"releases":[{"chart":{"digest":"`+opDigestDeployed+`"}}]}`),
			wantProblem: "release 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			releases, _, problem := decodeOperatorPlan(tt.cm)
			if releases != nil || !strings.Contains(problem, tt.wantProblem) {
				t.Errorf("decodeOperatorPlan() = %v, %q, want no releases and a problem naming %q",
					releases, problem, tt.wantProblem)
			}
		})
	}
}

// TestOperatorPlansNameEveryDisagreeingVersion pins that a third plan
// disagreeing with two that already disagree is named too.
func TestOperatorPlansNameEveryDisagreeingVersion(t *testing.T) {
	client := argoClient(
		syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed),
		planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa", opRelease{"gpu-operator", opDigestDeployed, "v26.7.2"}),
		planConfigMap(t, "aicr-plan-bbbbbbbbbbbbbbbb", opRelease{"gpu-operator", opDigestDeployed, "v26.7.3"}),
		planConfigMap(t, "aicr-plan-cccccccccccccccc", opRelease{"gpu-operator", opDigestDeployed, "v26.7.4"}),
	)

	_, err := argoApplications(context.Background(), client, argoTestScope(), testOperatorNamespace)
	if code := errorstest.ReportedCode(err); code != errors.ErrCodeConflict {
		t.Fatalf("error code = %s, want %s (%v)", code, errors.ErrCodeConflict, err)
	}
	for _, version := range []string{"v26.7.2", "v26.7.3", "v26.7.4"} {
		if !strings.Contains(err.Error(), version) {
			t.Errorf("error %q does not name %s", err, version)
		}
	}
}

// TestOperatorPlansFollowListPages proves the plan read keeps listing while
// the server hands back a continue token, and fails on a page that fails.
func TestOperatorPlansFollowListPages(t *testing.T) {
	page := func(token string, cms ...*unstructured.Unstructured) *unstructured.UnstructuredList {
		list := &unstructured.UnstructuredList{}
		list.SetContinue(token)
		for _, cm := range cms {
			list.Items = append(list.Items, *cm)
		}

		return list
	}

	tests := []struct {
		name     string
		answers  []runtime.Object
		lastErr  error
		want     string
		wantCode errors.ErrorCode
	}{
		{
			name: "the digest is found on the second page",
			answers: []runtime.Object{
				page("page-2", planConfigMap(t, "aicr-plan-bbbbbbbbbbbbbbbb", opRelease{"gpu-operator", opDigestNext, "v26.10.0"})),
				page("", planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa", opRelease{"gpu-operator", opDigestDeployed, "v26.7.2"})),
			},
			want: "v26.7.2",
		},
		{
			name:     "a failed later page fails the read",
			answers:  []runtime.Object{page("page-2")},
			lastErr:  stderrors.New("connection reset"),
			wantCode: errors.ErrCodeInternal,
		},
		{
			name:     "a deadline mid-read is a timeout",
			answers:  []runtime.Object{page("page-2")},
			lastErr:  context.DeadlineExceeded,
			wantCode: errors.ErrCodeTimeout,
		},
		{
			name:     "an expired continue token asks for a re-run",
			answers:  []runtime.Object{page("page-2")},
			lastErr:  apierrors.NewResourceExpired("continue token expired"),
			wantCode: errors.ErrCodeUnavailable,
		},
		{
			name:     "a cancellation mid-read is a cancellation",
			answers:  []runtime.Object{page("page-2")},
			lastErr:  context.Canceled,
			wantCode: errors.ErrCodeCanceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := argoClient(
				syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed))
			calls := 0
			client.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				defer func() { calls++ }()
				if calls < len(tt.answers) {
					return true, tt.answers[calls], nil
				}

				return true, nil, tt.lastErr
			})

			got, err := argoApplications(context.Background(), client, argoTestScope(), testOperatorNamespace)
			if tt.wantCode != "" {
				if code := errorstest.ReportedCode(err); code != tt.wantCode {
					t.Fatalf("error code = %s, want %s (%v)", code, tt.wantCode, err)
				}

				return
			}
			if err != nil {
				t.Fatalf("argoApplications() error = %v", err)
			}
			if len(got.Releases) != 1 || got.Releases[0].ChartVersion != tt.want {
				t.Errorf("releases = %+v, want gpu-operator at %s", got.Releases, tt.want)
			}
		})
	}
}

// TestOperatorPlansNameABoundedNumberOfUnreadablePlans pins that the failure
// stays readable however many plans could not be read.
func TestOperatorPlansNameABoundedNumberOfUnreadablePlans(t *testing.T) {
	const unreadable = 7
	objects := make([]runtime.Object, 0, unreadable+1)
	objects = append(objects,
		syncedAt(t, operatorApp("gpu-operator", "gpu-operator", opDigestDeployed, opPlanDigestA), opDigestDeployed))
	for i := range unreadable {
		objects = append(objects, planConfigMapData(fmt.Sprintf("aicr-plan-%016d", i), "2", "{}"))
	}

	_, err := argoApplications(context.Background(), argoClient(objects...), argoTestScope(), testOperatorNamespace)
	if code := errorstest.ReportedCode(err); code != errors.ErrCodeNotFound {
		t.Fatalf("error code = %s, want %s (%v)", code, errors.ErrCodeNotFound, err)
	}
	for _, want := range []string{"7 could not be read", "aicr-plan-0000000000000000", "and 2 more"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "aicr-plan-0000000000000006") {
		t.Errorf("error %q names more plans than the bound", err)
	}
}

// TestReadVersionsFromANamedOperatorNamespace drives a non-default operator
// namespace through the whole read, so the namespace the caller names is the
// one listed.
func TestReadVersionsFromANamedOperatorNamespace(t *testing.T) {
	const named = "aicr-ops"
	app := func(t *testing.T) *unstructured.Unstructured {
		t.Helper()

		return syncedAt(t, operatorApp("gpu-operator", fixtureGPUOperator.Namespace, opDigestDeployed, opPlanDigestA),
			opDigestDeployed)
	}
	plan := func(t *testing.T, namespace string) *unstructured.Unstructured {
		t.Helper()

		return inNamespace(planConfigMap(t, "aicr-plan-aaaaaaaaaaaaaaaa",
			opRelease{"gpu-operator", opDigestDeployed, "v26.7.2"}), namespace)
	}
	opts := Options{Deployer: DeployerArgoCD, Components: []Component{fixtureGPUOperator}, OperatorNamespace: named}

	got, err := read(context.Background(), fake.NewSimpleClientset(), argoClient(app(t), plan(t, named)), opts)
	if err != nil {
		t.Fatalf("read() error = %v", err)
	}
	if want := map[string]string{"gpu-operator": "v26.7.2"}; !reflect.DeepEqual(got.Versions, want) {
		t.Errorf("read() versions = %v, want %v", got.Versions, want)
	}

	_, err = read(context.Background(), fake.NewSimpleClientset(),
		argoClient(app(t), plan(t, testOperatorNamespace)), opts)
	if code := errorstest.ReportedCode(err); code != errors.ErrCodeNotFound {
		t.Errorf("a plan only in the default namespace: error code = %s, want %s (%v)", code, errors.ErrCodeNotFound, err)
	}
}

// TestReadReleasesNeverReadsOperatorPlans pins that the attribution read,
// which returns no versions, neither needs the operator's plans nor fails
// for want of them.
func TestReadReleasesNeverReadsOperatorPlans(t *testing.T) {
	client := argoClient(
		syncedAt(t, operatorApp("gpu-operator", fixtureGPUOperator.Namespace, opDigestDeployed, opPlanDigestA),
			opDigestDeployed))

	got, err := readReleases(context.Background(), fake.NewSimpleClientset(), client,
		Options{Deployer: DeployerArgoCD, Components: []Component{fixtureGPUOperator}})
	if err != nil {
		t.Fatalf("readReleases() error = %v", err)
	}
	if records := got.Components["gpu-operator"]; len(records) != 1 || records[0].Name != "gpu-operator" {
		t.Errorf("readReleases() gpu-operator records = %+v, want its Application", records)
	}
	if lists := configMapsListed(&client.Fake); lists != 0 {
		t.Errorf("listed ConfigMaps %d times, want none", lists)
	}
}
