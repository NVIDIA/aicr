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
	"encoding/json"
	stderrors "errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
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

type stubSlinkyDiscovery struct {
	mu             sync.Mutex
	groups         *metav1.APIGroupList
	groupsErr      error
	resources      map[string]*metav1.APIResourceList
	resourceErrors map[string]error
	resourceCalls  []string
}

func (s *stubSlinkyDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	return s.groups, s.groupsErr
}

func (s *stubSlinkyDiscovery) ServerResourcesForGroupVersion(
	groupVersion string,
) (*metav1.APIResourceList, error) {

	s.mu.Lock()
	s.resourceCalls = append(s.resourceCalls, groupVersion)
	s.mu.Unlock()
	if err := s.resourceErrors[groupVersion]; err != nil {
		return nil, err
	}
	return s.resources[groupVersion], nil
}

func (s *stubSlinkyDiscovery) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.resourceCalls...)
}

func TestCollectSlinkySlurm_StateMatrix(t *testing.T) {
	t.Parallel()

	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: slinkyAPIGroup, Resource: slinkyControllerResource},
		"",
		stderrors.New("forbidden"),
	)
	notFound := apierrors.NewNotFound(
		schema.GroupResource{Group: slinkyAPIGroup, Resource: slinkyControllerResource},
		"",
	)
	networkErr := &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}

	tests := []struct {
		name              string
		discovery         *stubSlinkyDiscovery
		controllers       []*unstructured.Unstructured
		listError         error
		want              map[string]any
		wantAbsent        []string
		wantResourceCalls []string
	}{
		{
			name: "group missing is conclusively absent",
			discovery: &stubSlinkyDiscovery{
				groups: &metav1.APIGroupList{},
			},
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateAbsent,
				slinkyKeyAPIAvailable:    false,
				slinkyKeyDetected:        false,
			},
			wantAbsent: []string{slinkyKeyAPIVersion, slinkyKeyControllerCount},
		},
		{
			name: "Controller resource missing is conclusively absent",
			discovery: newSlinkyDiscovery(
				slinkyGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					"slinky.slurm.net/v1alpha1": resourceList("v1alpha1", metav1.APIResource{
						Name: "nodesets", Kind: "NodeSet", Namespaced: true,
					}),
				},
			),
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateAbsent,
				slinkyKeyAPIAvailable:    false,
				slinkyKeyDetected:        false,
			},
			wantAbsent:        []string{slinkyKeyAPIVersion, slinkyKeyControllerCount},
			wantResourceCalls: []string{"slinky.slurm.net/v1alpha1"},
		},
		{
			name: "zero Controllers is absent with available API",
			discovery: newSlinkyDiscovery(
				slinkyGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					"slinky.slurm.net/v1alpha1": controllerResourceList("v1alpha1"),
				},
			),
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateAbsent,
				slinkyKeyAPIAvailable:    true,
				slinkyKeyAPIVersion:      "v1alpha1",
				slinkyKeyDetected:        false,
				slinkyKeyControllerCount: 0,
			},
		},
		{
			name: "one Controller is detected",
			discovery: newSlinkyDiscovery(
				slinkyGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					"slinky.slurm.net/v1alpha1": controllerResourceList("v1alpha1"),
				},
			),
			controllers: []*unstructured.Unstructured{
				newController("v1alpha1", "slurm", "cluster", true),
			},
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateDetected,
				slinkyKeyAPIAvailable:    true,
				slinkyKeyAPIVersion:      "v1alpha1",
				slinkyKeyDetected:        true,
				slinkyKeyControllerCount: 1,
			},
		},
		{
			name: "multiple Controllers are unsupported multicluster",
			discovery: newSlinkyDiscovery(
				slinkyGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					"slinky.slurm.net/v1alpha1": controllerResourceList("v1alpha1"),
				},
			),
			controllers: []*unstructured.Unstructured{
				newController("v1alpha1", "slurm-a", "cluster-a", false),
				newController("v1alpha1", "slurm-b", "cluster-b", false),
			},
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateUnsupportedMulticluster,
				slinkyKeyAPIAvailable:    true,
				slinkyKeyAPIVersion:      "v1alpha1",
				slinkyKeyDetected:        true,
				slinkyKeyControllerCount: 2,
			},
		},
		{
			name: "fallback served version is selected",
			discovery: &stubSlinkyDiscovery{
				groups: slinkyGroup("v1alpha1", "v1alpha1", "v1"),
				resources: map[string]*metav1.APIResourceList{
					"slinky.slurm.net/v1": controllerResourceList("v1"),
				},
				resourceErrors: map[string]error{
					"slinky.slurm.net/v1alpha1": notFound,
				},
			},
			controllers: []*unstructured.Unstructured{
				newController("v1", "slurm", "cluster", false),
			},
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateDetected,
				slinkyKeyAPIAvailable:    true,
				slinkyKeyAPIVersion:      "v1",
				slinkyKeyDetected:        true,
				slinkyKeyControllerCount: 1,
			},
			wantResourceCalls: []string{"slinky.slurm.net/v1alpha1", "slinky.slurm.net/v1"},
		},
		{
			name: "discovery forbidden is unknown",
			discovery: &stubSlinkyDiscovery{
				groupsErr: forbidden,
			},
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateUnknown,
			},
			wantAbsent: []string{
				slinkyKeyAPIAvailable, slinkyKeyAPIVersion,
				slinkyKeyDetected, slinkyKeyControllerCount,
			},
		},
		{
			name: "discovery unauthorized is unknown",
			discovery: &stubSlinkyDiscovery{
				groupsErr: apierrors.NewUnauthorized("unauthorized"),
			},
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateUnknown,
			},
			wantAbsent: []string{
				slinkyKeyAPIAvailable, slinkyKeyAPIVersion,
				slinkyKeyDetected, slinkyKeyControllerCount,
			},
		},
		{
			name: "resource discovery network failure is unknown",
			discovery: &stubSlinkyDiscovery{
				groups: slinkyGroup("v1alpha1", "v1alpha1"),
				resourceErrors: map[string]error{
					"slinky.slurm.net/v1alpha1": networkErr,
				},
			},
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateUnknown,
			},
			wantAbsent: []string{
				slinkyKeyAPIAvailable, slinkyKeyAPIVersion,
				slinkyKeyDetected, slinkyKeyControllerCount,
			},
		},
		{
			name:      "nil group discovery response is unknown",
			discovery: &stubSlinkyDiscovery{},
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateUnknown,
			},
			wantAbsent: []string{
				slinkyKeyAPIAvailable, slinkyKeyAPIVersion,
				slinkyKeyDetected, slinkyKeyControllerCount,
			},
		},
		{
			name: "malformed group version is unknown",
			discovery: &stubSlinkyDiscovery{
				groups: slinkyGroup("", "not/a/group/version"),
			},
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateUnknown,
			},
			wantAbsent: []string{
				slinkyKeyAPIAvailable, slinkyKeyAPIVersion,
				slinkyKeyDetected, slinkyKeyControllerCount,
			},
		},
		{
			name: "malformed resource response is unknown",
			discovery: newSlinkyDiscovery(
				slinkyGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					"slinky.slurm.net/v1alpha1": {
						GroupVersion: "other.example/v1",
						APIResources: []metav1.APIResource{controllerAPIResource()},
					},
				},
			),
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateUnknown,
			},
			wantAbsent: []string{
				slinkyKeyAPIAvailable, slinkyKeyAPIVersion,
				slinkyKeyDetected, slinkyKeyControllerCount,
			},
		},
		{
			name: "list forbidden is unknown with API available",
			discovery: newSlinkyDiscovery(
				slinkyGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					"slinky.slurm.net/v1alpha1": controllerResourceList("v1alpha1"),
				},
			),
			listError: forbidden,
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateUnknown,
				slinkyKeyAPIAvailable:    true,
				slinkyKeyAPIVersion:      "v1alpha1",
			},
			wantAbsent: []string{slinkyKeyDetected, slinkyKeyControllerCount},
		},
		{
			name: "list timeout is unknown with API available",
			discovery: newSlinkyDiscovery(
				slinkyGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					"slinky.slurm.net/v1alpha1": controllerResourceList("v1alpha1"),
				},
			),
			listError: apierrors.NewTimeoutError("timeout", 1),
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateUnknown,
				slinkyKeyAPIAvailable:    true,
				slinkyKeyAPIVersion:      "v1alpha1",
			},
			wantAbsent: []string{slinkyKeyDetected, slinkyKeyControllerCount},
		},
		{
			name: "list-time disappearance is unknown with API available",
			discovery: newSlinkyDiscovery(
				slinkyGroup("v1alpha1", "v1alpha1"),
				map[string]*metav1.APIResourceList{
					"slinky.slurm.net/v1alpha1": controllerResourceList("v1alpha1"),
				},
			),
			listError: notFound,
			want: map[string]any{
				slinkyKeyCollectionState: slinkyStateUnknown,
				slinkyKeyAPIAvailable:    true,
				slinkyKeyAPIVersion:      "v1alpha1",
			},
			wantAbsent: []string{slinkyKeyDetected, slinkyKeyControllerCount},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dynamicClient := newSlinkyDynamicClient(tt.controllers...)
			if tt.listError != nil {
				dynamicClient.PrependReactor(
					"list",
					slinkyControllerResource,
					func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, tt.listError
					},
				)
			}

			collector := &Collector{
				ClientSet:       fakeclient.NewClientset(),
				DynamicClient:   dynamicClient,
				slinkyDiscovery: tt.discovery,
			}
			subtype := collector.collectSlinkySlurm(context.Background(), nil)

			assert.Equal(t, SubtypeSlinkySlurm, subtype.Name)
			for key, want := range tt.want {
				reading, ok := subtype.Data[key]
				if !ok {
					t.Errorf("missing key %q", key)
					continue
				}
				assert.Equal(t, want, reading.Any(), "key %q", key)
			}
			for _, key := range tt.wantAbsent {
				assert.NotContains(t, subtype.Data, key)
			}
			if tt.want[slinkyKeyCollectionState] == slinkyStateDetected {
				assert.Len(t, subtype.Items, 1)
			} else {
				assert.Empty(t, subtype.Items)
			}
			if tt.wantResourceCalls != nil {
				calls := tt.discovery.calls()
				if assert.GreaterOrEqual(t, len(calls), len(tt.wantResourceCalls)) {
					assert.Equal(t, tt.wantResourceCalls, calls[:len(tt.wantResourceCalls)])
				}
			}
		})
	}
}

func TestCollectSlinkySlurm_DoesNotExposeControllerOrPodData(t *testing.T) {
	t.Parallel()

	controller := newController("v1alpha1", "slurm", "cluster", true)
	kubeClient := fakeclient.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "slinky-slurm-controller-manager",
			Namespace: "operator-system",
		},
	})
	collector := &Collector{
		ClientSet:     kubeClient,
		DynamicClient: newSlinkyDynamicClient(controller),
		slinkyDiscovery: newSlinkyDiscovery(
			slinkyGroup("v1alpha1", "v1alpha1"),
			map[string]*metav1.APIResourceList{
				"slinky.slurm.net/v1alpha1": controllerResourceList("v1alpha1"),
			},
		),
	}

	subtype := collector.collectSlinkySlurm(context.Background(), nil)

	assert.Equal(t, slinkyStateDetected, subtype.Data[slinkyKeyCollectionState].Any())
	assert.Len(t, subtype.Items, 1)
	assert.NotContains(t, subtype.Data, "spec")
	assert.NotContains(t, subtype.Data, "passwordSecretRef")
	assert.NotContains(t, subtype.Data, "operator-pods")
	assert.Empty(t, kubeClient.Actions(), "Tier 1 must not inspect operator pods")
}

func TestCollectSlinkySlurm_CompleteProjectionIsAllowlisted(t *testing.T) {
	t.Parallel()

	// These v1beta1 paths mirror the pinned Slinky v1.2.0 API types:
	// https://github.com/SlinkyProject/slurm-operator/tree/v1.2.0/api/v1beta1
	// Association fields are LocalObjectReference values, so namespace comes
	// from the referring object's metadata rather than the reference itself.
	controller := newSlinkyResource("v1beta1", slinkyControllerKind, "slurm", "cluster", map[string]any{
		"clusterName": "cluster",
		"external":    false,
		"accountingRef": map[string]any{
			"name": "accounting",
		},
		"extraConf": "PrivateData=accounts SECRET-CONTROLLER",
		"jwtKeyRef": map[string]any{
			"name": "jwt-secret",
		},
	})
	nodeSet := newSlinkyResource("v1beta1", slinkyNodeSetKind, "slurm", "workers", map[string]any{
		"controllerRef": map[string]any{"name": "cluster"},
		"partition":     map[string]any{"enabled": true},
		"replicas":      int64(8),
		"extraConf":     "Gres=gpu:h100:8 SECRET-NODESET",
	})
	nodeSet.Object["status"] = map[string]any{"availableReplicas": int64(8)}
	loginSet := newSlinkyResource("v1beta1", slinkyLoginSetKind, "slurm", "login", map[string]any{
		"controllerRef":         map[string]any{"name": "cluster"},
		"replicas":              int64(2),
		"rootSshAuthorizedKeys": "SECRET-SSH-KEY",
	})
	restAPI := newSlinkyResource("v1beta1", slinkyRestAPIKind, "slurm", "rest", map[string]any{
		"controllerRef": map[string]any{"name": "cluster"},
		"replicas":      int64(2),
		"service":       map[string]any{"type": "LoadBalancer"},
	})
	accounting := newSlinkyResource("v1beta1", slinkyAccountingKind, "slurm", "accounting", map[string]any{
		"external": true,
		"storageConfig": map[string]any{
			"host":     "internal-db.example.com",
			"username": "slurm",
			"passwordKeyRef": map[string]any{
				"name": "SECRET-DB-PASSWORD",
			},
		},
	})

	discovery := newSlinkyDiscovery(
		slinkyGroup("v1beta1", "v1beta1"),
		map[string]*metav1.APIResourceList{
			"slinky.slurm.net/v1beta1": allSlinkyResourceList("v1beta1"),
		},
	)
	collector := &Collector{
		ClientSet:       fakeclient.NewClientset(),
		DynamicClient:   newSlinkyDynamicClient(controller, nodeSet, loginSet, restAPI, accounting),
		slinkyDiscovery: discovery,
	}

	subtype := collector.collectSlinkySlurm(context.Background(), nil)

	assert.Equal(t, slinkyStateDetected, subtype.Data[slinkyKeyCollectionState].Any())
	assert.NotContains(t, subtype.Data, "projection-state")
	assert.Equal(t, 1, subtype.Data[slinkyKeyNodeSetCount].Any())
	assert.Equal(t, 1, subtype.Data[slinkyKeyLoginSetCount].Any())
	assert.Equal(t, 1, subtype.Data[slinkyKeyRestAPICount].Any())
	assert.Equal(t, 1, subtype.Data[slinkyKeyAccountingCount].Any())

	wantIDs := []string{
		"accounting/slurm/accounting",
		"controller/slurm/cluster",
		"loginset/slurm/login",
		"nodeset/slurm/workers",
		"restapi/slurm/rest",
	}
	gotIDs := make([]string, 0, len(subtype.Items))
	for i := range subtype.Items {
		gotIDs = append(gotIDs, subtype.Items[i].Context[slinkyContextID])
	}
	assert.Equal(t, wantIDs, gotIDs)

	controllerItem := findSlinkyItem(t, subtype.Items, "controller/slurm/cluster")
	assert.Equal(t, "cluster", controllerItem.Data[slinkyItemClusterName].Any())
	assert.Equal(t, false, controllerItem.Data[slinkyItemExternal].Any())
	assert.Equal(t, true, controllerItem.Data[slinkyItemAccountingRefPresent].Any())
	assert.Len(t, controllerItem.Data, 3)

	nodeSetItem := findSlinkyItem(t, subtype.Items, "nodeset/slurm/workers")
	assert.Equal(t, true, nodeSetItem.Data[slinkyItemPartitionEnabled].Any())
	assert.Equal(t, "controller/slurm/cluster", nodeSetItem.Context[slinkyContextControllerID])
	assert.Len(t, nodeSetItem.Data, 1)

	loginSetItem := findSlinkyItem(t, subtype.Items, "loginset/slurm/login")
	assert.Empty(t, loginSetItem.Data)
	restAPIItem := findSlinkyItem(t, subtype.Items, "restapi/slurm/rest")
	assert.Empty(t, restAPIItem.Data)

	accountingItem := findSlinkyItem(t, subtype.Items, "accounting/slurm/accounting")
	assert.Equal(t, true, accountingItem.Data[slinkyItemExternal].Any())
	assert.Len(t, accountingItem.Data, 1)

	serialized, err := json.Marshal(subtype)
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	body := string(serialized)
	for _, forbidden := range []string{
		"SECRET-", "extraConf", "replicas", "rootSshAuthorizedKeys",
		"storageConfig", "passwordKeyRef", "availableReplicas",
	} {
		assert.NotContains(t, body, forbidden)
	}
}

func TestCollectSlinkySlurm_ProjectionUsesFallbackVersion(t *testing.T) {
	t.Parallel()

	controller := newSlinkyResource(
		"v1beta1", slinkyControllerKind, "slurm", "cluster", map[string]any{},
	)
	discovery := &stubSlinkyDiscovery{
		groups: slinkyGroup("v1alpha1", "v1alpha1", "v1beta1"),
		resources: map[string]*metav1.APIResourceList{
			slinkyAPIGroup + "/v1beta1": allSlinkyResourceList("v1beta1"),
		},
		resourceErrors: map[string]error{
			slinkyAPIGroup + "/v1alpha1": apierrors.NewNotFound(
				schema.GroupResource{Group: slinkyAPIGroup, Resource: slinkyControllerResource},
				"",
			),
		},
	}
	collector := &Collector{
		ClientSet:       fakeclient.NewClientset(),
		DynamicClient:   newSlinkyDynamicClient(controller),
		slinkyDiscovery: discovery,
	}

	subtype := collector.collectSlinkySlurm(context.Background(), nil)

	assert.Equal(t, slinkyStateDetected, subtype.Data[slinkyKeyCollectionState].Any())
	assert.Equal(t, "v1beta1", subtype.Data[slinkyKeyAPIVersion].Any())
	assert.Len(t, subtype.Items, 1)
	assert.Equal(t, "v1beta1", subtype.Items[0].Context[slinkyContextAPIVersion])
}

func TestCollectSlinkySlurm_PartialProjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		controller *unstructured.Unstructured
		resources  []*unstructured.Unstructured
		listError  string
	}{
		{
			name: "foreign Controller reference",
			controller: newSlinkyResource(
				"v1beta1", slinkyControllerKind, "slurm", "cluster", map[string]any{},
			),
			resources: []*unstructured.Unstructured{
				newSlinkyResource("v1beta1", slinkyNodeSetKind, "other", "workers", map[string]any{
					"controllerRef": map[string]any{"name": "cluster"},
				}),
			},
		},
		{
			name: "malformed Controller reference",
			controller: newSlinkyResource(
				"v1beta1", slinkyControllerKind, "slurm", "cluster", map[string]any{},
			),
			resources: []*unstructured.Unstructured{
				newSlinkyResource("v1beta1", slinkyLoginSetKind, "slurm", "login", map[string]any{
					"controllerRef": map[string]any{"name": int64(7)},
				}),
			},
		},
		{
			name: "changed Controller reference path",
			controller: newSlinkyResource(
				"v2", slinkyControllerKind, "slurm", "cluster", map[string]any{},
			),
			resources: []*unstructured.Unstructured{
				newSlinkyResource("v2", slinkyRestAPIKind, "slurm", "rest", map[string]any{
					"controllerReference": map[string]any{"name": "cluster"},
				}),
			},
		},
		{
			name: "referenced Accounting missing",
			controller: newSlinkyResource(
				"v1beta1", slinkyControllerKind, "slurm", "cluster", map[string]any{
					"accountingRef": map[string]any{"name": "missing"},
				},
			),
		},
		{
			name: "child List fails",
			controller: newSlinkyResource(
				"v1beta1", slinkyControllerKind, "slurm", "cluster", map[string]any{},
			),
			listError: slinkyNodeSetResource,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			version := strings.TrimPrefix(tt.controller.GetAPIVersion(), slinkyAPIGroup+"/")
			objects := []*unstructured.Unstructured{tt.controller}
			objects = append(objects, tt.resources...)
			dynamicClient := newSlinkyDynamicClient(objects...)
			if tt.listError != "" {
				dynamicClient.PrependReactor(
					"list",
					tt.listError,
					func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewForbidden(
							schema.GroupResource{
								Group: slinkyAPIGroup, Resource: tt.listError,
							},
							"",
							stderrors.New("forbidden"),
						)
					},
				)
			}
			collector := &Collector{
				ClientSet:     fakeclient.NewClientset(),
				DynamicClient: dynamicClient,
				slinkyDiscovery: newSlinkyDiscovery(
					slinkyGroup(version, version),
					map[string]*metav1.APIResourceList{
						slinkyAPIGroup + "/" + version: allSlinkyResourceList(version),
					},
				),
			}

			subtype := collector.collectSlinkySlurm(context.Background(), nil)

			assert.Equal(t, slinkyStateDetected, subtype.Data[slinkyKeyCollectionState].Any())
			assert.NotContains(t, subtype.Data, "projection-state")
			assert.Len(t, subtype.Items, 1, "only the Controller should be selected")
			assert.Equal(t, "controller/slurm/cluster", subtype.Items[0].Context[slinkyContextID])
			assert.NotContains(t, subtype.Data, slinkyKeyNodeSetCount)
			assert.NotContains(t, subtype.Data, slinkyKeyLoginSetCount)
			assert.NotContains(t, subtype.Data, slinkyKeyRestAPICount)
			assert.NotContains(t, subtype.Data, slinkyKeyAccountingCount)
		})
	}
}

func TestCollectSlinkySlurm_CanceledContextIsUnknown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	collector := &Collector{
		ClientSet:       fakeclient.NewClientset(),
		DynamicClient:   newSlinkyDynamicClient(),
		slinkyDiscovery: &stubSlinkyDiscovery{},
	}

	subtype := collector.collectSlinkySlurm(ctx, nil)

	assert.Equal(t, slinkyStateUnknown, subtype.Data[slinkyKeyCollectionState].Any())
	assert.Len(t, subtype.Data, 1)
}

func newSlinkyDiscovery(
	groups *metav1.APIGroupList,
	resources map[string]*metav1.APIResourceList,
) *stubSlinkyDiscovery {

	return &stubSlinkyDiscovery{
		groups:         groups,
		resources:      resources,
		resourceErrors: make(map[string]error),
	}
}

func slinkyGroup(preferredVersion string, versions ...string) *metav1.APIGroupList {
	group := metav1.APIGroup{Name: slinkyAPIGroup}
	if preferredVersion != "" {
		group.PreferredVersion = metav1.GroupVersionForDiscovery{
			GroupVersion: slinkyAPIGroup + "/" + preferredVersion,
			Version:      preferredVersion,
		}
	}
	for _, version := range versions {
		groupVersion := version
		if parsed, err := schema.ParseGroupVersion(version); err != nil || parsed.Group == "" {
			if err == nil && parsed.Version != "" {
				groupVersion = slinkyAPIGroup + "/" + parsed.Version
			}
		}
		group.Versions = append(group.Versions, metav1.GroupVersionForDiscovery{
			GroupVersion: groupVersion,
			Version:      version,
		})
	}
	return &metav1.APIGroupList{Groups: []metav1.APIGroup{group}}
}

func resourceList(version string, resources ...metav1.APIResource) *metav1.APIResourceList {
	return &metav1.APIResourceList{
		GroupVersion: slinkyAPIGroup + "/" + version,
		APIResources: resources,
	}
}

func controllerResourceList(version string) *metav1.APIResourceList {
	return resourceList(version, controllerAPIResource())
}

func allSlinkyResourceList(version string) *metav1.APIResourceList {
	return resourceList(
		version,
		controllerAPIResource(),
		slinkyAPIResource(slinkyNodeSetResource, slinkyNodeSetKind),
		slinkyAPIResource(slinkyLoginSetResource, slinkyLoginSetKind),
		slinkyAPIResource(slinkyRestAPIResource, slinkyRestAPIKind),
		slinkyAPIResource(slinkyAccountingResource, slinkyAccountingKind),
	)
}

func controllerAPIResource() metav1.APIResource {
	return slinkyAPIResource(slinkyControllerResource, slinkyControllerKind)
}

func slinkyAPIResource(resource string, kind string) metav1.APIResource {
	return metav1.APIResource{Name: resource, Kind: kind, Namespaced: true}
}

func newController(
	version string,
	namespace string,
	name string,
	withSecretData bool,
) *unstructured.Unstructured {

	object := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": slinkyAPIGroup + "/" + version,
			"kind":       slinkyControllerKind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
		},
	}
	if withSecretData {
		object.Object["spec"] = map[string]any{
			"accounting": map[string]any{
				"passwordSecretRef": map[string]any{
					"name": "database-password",
					"key":  "password",
				},
			},
		}
	}
	return object
}

func newSlinkyResource(
	version string,
	kind string,
	namespace string,
	name string,
	spec map[string]any,
) *unstructured.Unstructured {

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": slinkyAPIGroup + "/" + version,
			"kind":       kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": spec,
		},
	}
}

func findSlinkyItem(
	t *testing.T,
	items []measurement.ItemEntry,
	id string,
) measurement.ItemEntry {

	t.Helper()
	for i := range items {
		if items[i].Context[slinkyContextID] == id {
			return items[i]
		}
	}
	t.Fatalf("item %q not found", id)
	return measurement.ItemEntry{}
}

func newSlinkyDynamicClient(
	resources ...*unstructured.Unstructured,
) *dynamicfake.FakeDynamicClient {

	objects := make([]runtime.Object, 0, len(resources))
	for _, resource := range resources {
		objects = append(objects, resource)
	}
	listKinds := make(map[schema.GroupVersionResource]string)
	resourceKinds := []struct {
		resource string
		kind     string
	}{
		{slinkyControllerResource, slinkyControllerKind},
		{slinkyNodeSetResource, slinkyNodeSetKind},
		{slinkyLoginSetResource, slinkyLoginSetKind},
		{slinkyRestAPIResource, slinkyRestAPIKind},
		{slinkyAccountingResource, slinkyAccountingKind},
	}
	for _, version := range []string{"v1alpha1", "v1beta1", "v1", "v2"} {
		for _, resourceKind := range resourceKinds {
			listKinds[schema.GroupVersionResource{
				Group: slinkyAPIGroup, Version: version, Resource: resourceKind.resource,
			}] = resourceKind.kind + "List"
		}
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		listKinds,
		objects...,
	)
}

var _ slinkyDiscovery = (*stubSlinkyDiscovery)(nil)
var _ dynamic.Interface = (*dynamicfake.FakeDynamicClient)(nil)
