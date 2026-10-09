// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func TestCollectPerconaServerMongoDB_StateMatrix(t *testing.T) {
	t.Parallel()

	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: perconaAPIGroup, Resource: perconaResource},
		"",
		stderrors.New("forbidden"),
	)

	served := func() *stubSlinkyDiscovery {
		return &stubSlinkyDiscovery{
			groups: perconaGroup("v1"),
			resources: map[string]*metav1.APIResourceList{
				perconaAPIGroup + "/v1": perconaExactResourceList("v1"),
			},
		}
	}

	tests := []struct {
		name        string
		discovery   *stubSlinkyDiscovery
		crs         []*unstructured.Unstructured
		pods        []runtime.Object
		listError   error
		podError    error
		want        map[string]any
		wantMissing []string
	}{
		{
			name:        "missing group is absent",
			discovery:   &stubSlinkyDiscovery{groups: &metav1.APIGroupList{}},
			want:        map[string]any{perconaKeyCollectionState: perconaStateAbsent, perconaKeyAPIAvailable: false},
			wantMissing: []string{perconaKeyAPIVersion},
		},
		{
			name: "group without exact resource is API detected",
			discovery: &stubSlinkyDiscovery{
				groups: perconaGroup("v1"),
				resources: map[string]*metav1.APIResourceList{
					perconaAPIGroup + "/v1": {
						GroupVersion: perconaAPIGroup + "/v1",
						APIResources: []metav1.APIResource{{
							Name: "perconaservermongodbbackups", Kind: "PerconaServerMongoDBBackup", Namespaced: true,
						}},
					},
				},
			},
			want:        map[string]any{perconaKeyCollectionState: perconaStateAPIDetected, perconaKeyAPIAvailable: false},
			wantMissing: []string{perconaKeyAPIVersion},
		},
		{
			name:      "zero CRs is API detected",
			discovery: served(),
			want: map[string]any{
				perconaKeyCollectionState: perconaStateAPIDetected,
				perconaKeyAPIAvailable:    true,
				perconaKeyAPIVersion:      "v1",
			},
		},
		{
			name:      "existing CR is detected",
			discovery: served(),
			crs:       []*unstructured.Unstructured{newPerconaServerMongoDB("mongo", "rs")},
			want: map[string]any{
				perconaKeyCollectionState: perconaStateCRsDetected,
				perconaKeyAPIAvailable:    true,
				perconaKeyAPIVersion:      "v1",
			},
		},
		{
			name:      "only AICR's own CR is AICR-owned",
			discovery: served(),
			crs:       []*unstructured.Unstructured{newPerconaServerMongoDB(perconaAICRNamespace, perconaAICRName)},
			want:      map[string]any{perconaKeyCollectionState: perconaStateAICROwned},
		},
		{
			name:      "AICR's name in another namespace is foreign",
			discovery: served(),
			crs:       []*unstructured.Unstructured{newPerconaServerMongoDB("other", perconaAICRName)},
			want:      map[string]any{perconaKeyCollectionState: perconaStateCRsDetected},
		},
		{
			name:      "foreign CR beside AICR's own is detected",
			discovery: served(),
			crs: []*unstructured.Unstructured{
				newPerconaServerMongoDB(perconaAICRNamespace, perconaAICRName),
				newPerconaServerMongoDB(perconaAICRNamespace, "team-db"),
			},
			want: map[string]any{perconaKeyCollectionState: perconaStateCRsDetected},
		},
		{
			name:      "foreign operator pod without foreign CRs is operator detected",
			discovery: served(),
			pods:      []runtime.Object{newPerconaOperatorPod("percona", "percona-server-mongodb-operator")},
			want:      map[string]any{perconaKeyCollectionState: perconaStateOperatorDetected},
		},
		{
			name:      "foreign operator pod with AICR's own CR is operator detected",
			discovery: served(),
			crs:       []*unstructured.Unstructured{newPerconaServerMongoDB(perconaAICRNamespace, perconaAICRName)},
			pods:      []runtime.Object{newPerconaOperatorPod("db", "psmdb-operator")},
			want:      map[string]any{perconaKeyCollectionState: perconaStateOperatorDetected},
		},
		{
			name:      "AICR's own operator pod is not foreign",
			discovery: served(),
			crs:       []*unstructured.Unstructured{newPerconaServerMongoDB(perconaAICRNamespace, perconaAICRName)},
			pods:      []runtime.Object{newPerconaOperatorPod(perconaAICRNamespace, "psmdb-operator", "psmdb-operator")},
			want:      map[string]any{perconaKeyCollectionState: perconaStateAICROwned},
		},
		{
			name:      "AICR's own operator pod under Flux's release name is not foreign",
			discovery: served(),
			pods:      []runtime.Object{newPerconaOperatorPod(perconaAICRNamespace, "psmdb-operator", "nvsentinel-psmdb-operator")},
			want:      map[string]any{perconaKeyCollectionState: perconaStateAPIDetected},
		},
		{
			name:      "foreign operator in AICR's namespace is operator detected",
			discovery: served(),
			crs:       []*unstructured.Unstructured{newPerconaServerMongoDB(perconaAICRNamespace, perconaAICRName)},
			pods:      []runtime.Object{newPerconaOperatorPod(perconaAICRNamespace, "psmdb-operator", "team-percona")},
			want:      map[string]any{perconaKeyCollectionState: perconaStateOperatorDetected},
		},
		{
			name:      "unrelated pod is ignored",
			discovery: served(),
			pods:      []runtime.Object{newPerconaOperatorPod("apps", "web")},
			want:      map[string]any{perconaKeyCollectionState: perconaStateAPIDetected},
		},
		{
			name:      "operator pod list forbidden is unknown",
			discovery: served(),
			podError:  forbidden,
			want:      map[string]any{perconaKeyCollectionState: perconaStateUnknown},
		},
		{
			name:        "group discovery forbidden is unknown",
			discovery:   &stubSlinkyDiscovery{groupsErr: forbidden},
			want:        map[string]any{perconaKeyCollectionState: perconaStateUnknown},
			wantMissing: []string{perconaKeyAPIAvailable, perconaKeyAPIVersion},
		},
		{
			name:      "nil group discovery is unknown",
			discovery: &stubSlinkyDiscovery{},
			want:      map[string]any{perconaKeyCollectionState: perconaStateUnknown},
		},
		{
			name: "resource discovery failure is unknown",
			discovery: &stubSlinkyDiscovery{
				groups:         perconaGroup("v1"),
				resourceErrors: map[string]error{perconaAPIGroup + "/v1": forbidden},
			},
			want:        map[string]any{perconaKeyCollectionState: perconaStateUnknown},
			wantMissing: []string{perconaKeyAPIAvailable, perconaKeyAPIVersion},
		},
		{
			name:      "list forbidden is unknown",
			discovery: served(),
			listError: forbidden,
			want: map[string]any{
				perconaKeyCollectionState: perconaStateUnknown,
				perconaKeyAPIAvailable:    true,
				perconaKeyAPIVersion:      "v1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dynamicClient := newPerconaDynamicClient(tt.crs...)
			if tt.listError != nil {
				dynamicClient.PrependReactor("list", perconaResource,
					func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, tt.listError
					})
			}
			clientset := fakeclient.NewClientset(tt.pods...)
			if tt.podError != nil {
				clientset.PrependReactor("list", "pods",
					func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, tt.podError
					})
			}
			collector := &Collector{
				ClientSet:        clientset,
				DynamicClient:    dynamicClient,
				perconaDiscovery: tt.discovery,
			}

			subtype := collector.collectPerconaServerMongoDB(context.Background(), nil)

			assert.Equal(t, SubtypePerconaServerMongoDB, subtype.Name)
			for key, want := range tt.want {
				reading, ok := subtype.Data[key]
				if !ok {
					t.Errorf("missing key %q", key)
					continue
				}
				assert.Equal(t, want, reading.Any(), "key %q", key)
			}
			for _, key := range tt.wantMissing {
				assert.NotContains(t, subtype.Data, key)
			}
		})
	}
}

func TestListPerconaServerMongoDBs_Paging(t *testing.T) {
	t.Parallel()

	gvr := schema.GroupVersionResource{Group: perconaAPIGroup, Version: "v1", Resource: perconaResource}
	own := newPerconaServerMongoDB(perconaAICRNamespace, perconaAICRName)

	tests := []struct {
		name        string
		endless     bool
		wantForeign bool
		wantOwned   bool
		wantErr     bool
	}{
		{name: "foreign CR on the second page", wantForeign: true, wantOwned: true},
		{name: "never-ending continue token fails closed", endless: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := newPerconaDynamicClient()
			var calls int
			client.PrependReactor("list", perconaResource,
				func(ktesting.Action) (bool, runtime.Object, error) {
					calls++
					list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*own.DeepCopy()}}
					if tt.endless || calls == 1 {
						list.SetContinue("next")
						return true, list, nil
					}
					list.Items = []unstructured.Unstructured{*newPerconaServerMongoDB("team", "db")}
					return true, list, nil
				})

			foreign, owned, err := listPerconaServerMongoDBs(context.Background(), client, gvr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				assert.Equal(t, perconaMaxListPages, calls)
				return
			}
			assert.Equal(t, tt.wantForeign, foreign)
			assert.Equal(t, tt.wantOwned, owned)
		})
	}
}

func TestCollectPerconaServerMongoDB_FailureModesAreUnknown(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name      string
		ctx       context.Context
		collector *Collector
	}{
		{
			name:      "canceled context",
			ctx:       canceled,
			collector: &Collector{perconaDiscovery: &stubSlinkyDiscovery{}},
		},
		{
			name:      "no discovery client",
			ctx:       context.Background(),
			collector: &Collector{},
		},
		{
			name: "dynamic client unavailable",
			ctx:  context.Background(),
			collector: &Collector{
				perconaDiscovery: &stubSlinkyDiscovery{
					groups: perconaGroup("v1"),
					resources: map[string]*metav1.APIResourceList{
						perconaAPIGroup + "/v1": perconaExactResourceList("v1"),
					},
				},
				RestConfig: &rest.Config{Host: "http://[::1]:namedport"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			subtype := tt.collector.collectPerconaServerMongoDB(tt.ctx, nil)
			assert.Equal(t, perconaStateUnknown, subtype.Data[perconaKeyCollectionState].Any())
		})
	}
}

func newPerconaOperatorPod(namespace, appName string, release ...string) *corev1.Pod {
	labels := map[string]string{"app.kubernetes.io/name": appName}
	if len(release) > 0 {
		labels["app.kubernetes.io/instance"] = release[0]
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace,
		Name:      appName + "-0",
		Labels:    labels,
	}}
}

func perconaGroup(versions ...string) *metav1.APIGroupList {
	group := metav1.APIGroup{Name: perconaAPIGroup}
	for i, version := range versions {
		gv := metav1.GroupVersionForDiscovery{GroupVersion: perconaAPIGroup + "/" + version, Version: version}
		if i == 0 {
			group.PreferredVersion = gv
		}
		group.Versions = append(group.Versions, gv)
	}
	return &metav1.APIGroupList{Groups: []metav1.APIGroup{group}}
}

func perconaExactResourceList(version string) *metav1.APIResourceList {
	return &metav1.APIResourceList{
		GroupVersion: perconaAPIGroup + "/" + version,
		APIResources: []metav1.APIResource{{Name: perconaResource, Kind: perconaKind, Namespaced: true}},
	}
}

func newPerconaServerMongoDB(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": perconaAPIGroup + "/v1",
		"kind":       perconaKind,
		"metadata":   map[string]any{"namespace": namespace, "name": name},
	}}
}

func newPerconaDynamicClient(crs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	objects := make([]runtime.Object, 0, len(crs))
	for _, cr := range crs {
		objects = append(objects, cr)
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: perconaAPIGroup, Version: "v1", Resource: perconaResource}: "PerconaServerMongoDBList",
		},
		objects...,
	)
}
