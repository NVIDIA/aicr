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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestCollectMariaDBOperator_StateMatrix(t *testing.T) {
	t.Parallel()

	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: mariaDBAPIGroup, Resource: mariaDBResource},
		"",
		stderrors.New("forbidden"),
	)
	notFound := apierrors.NewNotFound(
		schema.GroupResource{Group: mariaDBAPIGroup, Resource: mariaDBResource},
		"",
	)

	tests := []struct {
		name        string
		discovery   *stubSlinkyDiscovery
		mariaDBs    []*unstructured.Unstructured
		listError   error
		want        map[string]any
		wantMissing []string
	}{
		{
			name:      "missing group is absent",
			discovery: &stubSlinkyDiscovery{groups: &metav1.APIGroupList{}},
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateAbsent,
				mariaDBKeyAPIAvailable:    false,
			},
			wantMissing: []string{mariaDBKeyAPIVersion},
		},
		{
			name: "group without exact resource is API detected",
			discovery: newMariaDBDiscovery(
				mariaDBGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					mariaDBAPIGroup + "/v1alpha1": mariaDBResourceList(
						"v1alpha1",
						metav1.APIResource{Name: "backups", Kind: "Backup", Namespaced: true},
					),
				},
			),
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateAPIDetected,
				mariaDBKeyAPIAvailable:    false,
			},
			wantMissing: []string{mariaDBKeyAPIVersion},
		},
		{
			name: "zero CRs is API detected",
			discovery: newMariaDBDiscovery(
				mariaDBGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					mariaDBAPIGroup + "/v1alpha1": mariaDBExactResourceList("v1alpha1"),
				},
			),
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateAPIDetected,
				mariaDBKeyAPIAvailable:    true,
				mariaDBKeyAPIVersion:      "v1alpha1",
			},
		},
		{
			name: "one CR is detected",
			discovery: newMariaDBDiscovery(
				mariaDBGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					mariaDBAPIGroup + "/v1alpha1": mariaDBExactResourceList("v1alpha1"),
				},
			),
			mariaDBs: []*unstructured.Unstructured{
				newMariaDB("slurm", "accounting"),
			},
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateCRsDetected,
				mariaDBKeyAPIAvailable:    true,
				mariaDBKeyAPIVersion:      "v1alpha1",
			},
		},
		{
			name: "multiple CRs use the same detected state",
			discovery: newMariaDBDiscovery(
				mariaDBGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					mariaDBAPIGroup + "/v1alpha1": mariaDBExactResourceList("v1alpha1"),
				},
			),
			mariaDBs: []*unstructured.Unstructured{
				newMariaDB("slurm-a", "accounting-a"),
				newMariaDB("slurm-b", "accounting-b"),
			},
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateCRsDetected,
				mariaDBKeyAPIAvailable:    true,
				mariaDBKeyAPIVersion:      "v1alpha1",
			},
		},
		{
			name: "fallback version is selected",
			discovery: &stubSlinkyDiscovery{
				groups: mariaDBGroup("v1alpha1", "v1alpha1", "v1"),
				resources: map[string]*metav1.APIResourceList{
					mariaDBAPIGroup + "/v1": mariaDBExactResourceList("v1"),
				},
				resourceErrors: map[string]error{
					mariaDBAPIGroup + "/v1alpha1": notFound,
				},
			},
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateAPIDetected,
				mariaDBKeyAPIAvailable:    true,
				mariaDBKeyAPIVersion:      "v1",
			},
		},
		{
			name:      "group discovery forbidden is unknown",
			discovery: &stubSlinkyDiscovery{groupsErr: forbidden},
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateUnknown,
			},
			wantMissing: []string{
				mariaDBKeyAPIAvailable, mariaDBKeyAPIVersion,
			},
		},
		{
			name:      "nil group discovery is unknown",
			discovery: &stubSlinkyDiscovery{},
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateUnknown,
			},
		},
		{
			name: "malformed group version is unknown",
			discovery: &stubSlinkyDiscovery{
				groups: &metav1.APIGroupList{Groups: []metav1.APIGroup{{
					Name: mariaDBAPIGroup,
					PreferredVersion: metav1.GroupVersionForDiscovery{
						GroupVersion: "not/a/group/version",
					},
					Versions: []metav1.GroupVersionForDiscovery{{
						GroupVersion: "also-invalid",
					}},
				}}},
			},
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateUnknown,
			},
			wantMissing: []string{mariaDBKeyAPIAvailable, mariaDBKeyAPIVersion},
		},
		{
			name: "malformed resource response is unknown",
			discovery: newMariaDBDiscovery(
				mariaDBGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					mariaDBAPIGroup + "/v1alpha1": {
						GroupVersion: "other.example/v1",
						APIResources: []metav1.APIResource{{
							Name: mariaDBResource, Kind: mariaDBKind, Namespaced: true,
						}},
					},
				},
			),
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateUnknown,
			},
			wantMissing: []string{mariaDBKeyAPIAvailable, mariaDBKeyAPIVersion},
		},
		{
			name: "resource discovery failure is unknown",
			discovery: &stubSlinkyDiscovery{
				groups: mariaDBGroup("v1alpha1", "v1alpha1"),
				resourceErrors: map[string]error{
					mariaDBAPIGroup + "/v1alpha1": forbidden,
				},
			},
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateUnknown,
			},
			wantMissing: []string{mariaDBKeyAPIAvailable, mariaDBKeyAPIVersion},
		},
		{
			name: "list forbidden is unknown",
			discovery: newMariaDBDiscovery(
				mariaDBGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					mariaDBAPIGroup + "/v1alpha1": mariaDBExactResourceList("v1alpha1"),
				},
			),
			listError: forbidden,
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateUnknown,
				mariaDBKeyAPIAvailable:    true,
				mariaDBKeyAPIVersion:      "v1alpha1",
			},
		},
		{
			name: "list-time disappearance is unknown",
			discovery: newMariaDBDiscovery(
				mariaDBGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					mariaDBAPIGroup + "/v1alpha1": mariaDBExactResourceList("v1alpha1"),
				},
			),
			listError: notFound,
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateUnknown,
				mariaDBKeyAPIAvailable:    true,
				mariaDBKeyAPIVersion:      "v1alpha1",
			},
		},
		{
			name: "list timeout is unknown",
			discovery: newMariaDBDiscovery(
				mariaDBGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					mariaDBAPIGroup + "/v1alpha1": mariaDBExactResourceList("v1alpha1"),
				},
			),
			listError: apierrors.NewTimeoutError("timeout", 1),
			want: map[string]any{
				mariaDBKeyCollectionState: mariaDBStateUnknown,
				mariaDBKeyAPIAvailable:    true,
				mariaDBKeyAPIVersion:      "v1alpha1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dynamicClient := newMariaDBDynamicClient(tt.mariaDBs...)
			if tt.listError != nil {
				dynamicClient.PrependReactor(
					"list",
					mariaDBResource,
					func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, tt.listError
					},
				)
			}
			collector := &Collector{
				ClientSet:        fakeclient.NewClientset(),
				DynamicClient:    dynamicClient,
				mariaDBDiscovery: tt.discovery,
			}

			subtype := collector.collectMariaDBOperator(context.Background(), nil)

			assert.Equal(t, SubtypeMariaDBOperator, subtype.Name)
			assert.Empty(t, subtype.Items)
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
			assert.NotContains(t, subtype.Data, "mariadb-detected")
			assert.NotContains(t, subtype.Data, "mariadb-cr-count")
			assert.NotContains(t, subtype.Data, "group-available")
			assert.NotContains(t, subtype.Data, "api-group-available")
			assert.NotContains(t, subtype.Data, "resource-available")
			assert.NotContains(t, subtype.Data, "database-available")
			assert.NotContains(t, subtype.Data, "operator-healthy")
		})
	}
}

func TestCollectMariaDBOperator_CanceledContextIsUnknown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	subtype := (&Collector{
		ClientSet:        fakeclient.NewClientset(),
		mariaDBDiscovery: &stubSlinkyDiscovery{},
	}).collectMariaDBOperator(ctx, nil)

	assert.Equal(t, mariaDBStateUnknown, subtype.Data[mariaDBKeyCollectionState].Any())
	assert.Len(t, subtype.Data, 1)
}

func TestCollectMariaDBOperator_LimitsPresenceQuery(t *testing.T) {
	t.Parallel()

	dynamicClient := &listOptionsCapturingDynamicClient{
		resources: &unstructured.UnstructuredList{
			Items: []unstructured.Unstructured{
				*newMariaDB("slurm", "accounting"),
			},
		},
	}
	collector := &Collector{
		ClientSet:     fakeclient.NewClientset(),
		DynamicClient: dynamicClient,
		mariaDBDiscovery: newMariaDBDiscovery(
			mariaDBGroup("v1alpha1", "v1alpha1"),
			map[string]*metav1.APIResourceList{
				mariaDBAPIGroup + "/v1alpha1": mariaDBExactResourceList("v1alpha1"),
			},
		),
	}

	subtype := collector.collectMariaDBOperator(context.Background(), nil)

	assert.Equal(t, mariaDBStateCRsDetected, subtype.Data[mariaDBKeyCollectionState].Any())
	assert.Equal(t, int64(1), dynamicClient.listOptions.Limit)
}

type listOptionsCapturingDynamicClient struct {
	dynamic.NamespaceableResourceInterface
	resources   *unstructured.UnstructuredList
	listOptions metav1.ListOptions
}

func (c *listOptionsCapturingDynamicClient) Resource(
	schema.GroupVersionResource,
) dynamic.NamespaceableResourceInterface {

	return c
}

func (c *listOptionsCapturingDynamicClient) Namespace(
	string,
) dynamic.ResourceInterface {

	return c
}

func (c *listOptionsCapturingDynamicClient) List(
	_ context.Context,
	options metav1.ListOptions,
) (*unstructured.UnstructuredList, error) {

	c.listOptions = options
	return c.resources.DeepCopy(), nil
}

func newMariaDBDiscovery(
	groups *metav1.APIGroupList,
	resources map[string]*metav1.APIResourceList,
) *stubSlinkyDiscovery {

	return &stubSlinkyDiscovery{
		groups:         groups,
		resources:      resources,
		resourceErrors: make(map[string]error),
	}
}

func mariaDBGroup(versions ...string) *metav1.APIGroupList {
	group := metav1.APIGroup{Name: mariaDBAPIGroup}
	preferredVersion := ""
	if len(versions) > 0 {
		preferredVersion = versions[0]
		versions = versions[1:]
	}
	if preferredVersion != "" {
		group.PreferredVersion = metav1.GroupVersionForDiscovery{
			GroupVersion: mariaDBAPIGroup + "/" + preferredVersion,
			Version:      preferredVersion,
		}
	}
	for _, version := range versions {
		group.Versions = append(group.Versions, metav1.GroupVersionForDiscovery{
			GroupVersion: mariaDBAPIGroup + "/" + version,
			Version:      version,
		})
	}
	return &metav1.APIGroupList{Groups: []metav1.APIGroup{group}}
}

func mariaDBResourceList(
	version string,
	resources ...metav1.APIResource,
) *metav1.APIResourceList {

	return &metav1.APIResourceList{
		GroupVersion: mariaDBAPIGroup + "/" + version,
		APIResources: resources,
	}
}

func mariaDBExactResourceList(version string) *metav1.APIResourceList {
	return mariaDBResourceList(version, metav1.APIResource{
		Name: mariaDBResource, Kind: mariaDBKind, Namespaced: true,
	})
}

func newMariaDB(namespace string, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": mariaDBAPIGroup + "/v1alpha1",
		"kind":       mariaDBKind,
		"metadata": map[string]any{
			"namespace": namespace,
			"name":      name,
		},
	}}
}

func newMariaDBDynamicClient(
	mariaDBs ...*unstructured.Unstructured,
) *dynamicfake.FakeDynamicClient {

	objects := make([]runtime.Object, 0, len(mariaDBs))
	for _, mariaDB := range mariaDBs {
		objects = append(objects, mariaDB)
	}
	listKinds := map[schema.GroupVersionResource]string{
		{Group: mariaDBAPIGroup, Version: "v1alpha1", Resource: mariaDBResource}: "MariaDBList",
		{Group: mariaDBAPIGroup, Version: "v1", Resource: mariaDBResource}:       "MariaDBList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		listKinds,
		objects...,
	)
}
