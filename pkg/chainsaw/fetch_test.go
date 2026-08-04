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

package chainsaw

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// testMapper returns a RESTMapper that knows about the two GVKs the
// tests use: namespaced Pods and cluster-scoped Nodes.
func testMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "", Version: "v1"},
	})
	m.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Node"}, meta.RESTScopeRoot)
	return m
}

func newPod(namespace, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("Pod")
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetLabels(labels)
	return u
}

func newNode(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("Node")
	u.SetName(name)
	return u
}

func TestClusterFetcher_Fetch(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	tests := []struct {
		name         string
		objects      []runtime.Object
		apiVersion   string
		kind         string
		namespace    string
		resourceName string
		wantErrCode  errors.ErrorCode // "" == success
	}{
		{
			name:         "namespaced get hits the object",
			objects:      []runtime.Object{newPod("ns", "p1", nil)},
			apiVersion:   "v1",
			kind:         "Pod",
			namespace:    "ns",
			resourceName: "p1",
		},
		{
			name:         "cluster-scoped get hits the object",
			objects:      []runtime.Object{newNode("n1")},
			apiVersion:   "v1",
			kind:         "Node",
			resourceName: "n1",
		},
		{
			name:         "missing resource maps to ErrCodeNotFound",
			objects:      nil,
			apiVersion:   "v1",
			kind:         "Pod",
			namespace:    "ns",
			resourceName: "missing",
			wantErrCode:  errors.ErrCodeNotFound,
		},
		{
			name:         "invalid apiVersion maps to ErrCodeInvalidRequest",
			apiVersion:   "//bad//",
			kind:         "Pod",
			namespace:    "ns",
			resourceName: "p1",
			wantErrCode:  errors.ErrCodeInvalidRequest,
		},
		{
			name:         "unknown kind maps to ErrCodeNotFound (no REST mapping)",
			apiVersion:   "v1",
			kind:         "DoesNotExist",
			namespace:    "ns",
			resourceName: "x",
			wantErrCode:  errors.ErrCodeNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := dynamicfake.NewSimpleDynamicClient(scheme, tt.objects...)
			f := NewClusterFetcher(client, testMapper())
			obj, err := f.Fetch(context.Background(), tt.apiVersion, tt.kind, tt.namespace, tt.resourceName)
			if tt.wantErrCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if obj["metadata"].(map[string]interface{})["name"] != tt.resourceName {
					t.Errorf("returned object name mismatch: %v", obj["metadata"])
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error code %q, got nil", tt.wantErrCode)
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected StructuredError, got %T %v", err, err)
			}
			if se.Code != tt.wantErrCode {
				t.Errorf("code = %q, want %q (err=%v)", se.Code, tt.wantErrCode, err)
			}
		})
	}
}

// TestClusterFetcher_Fetch_TransientErrorMapsToUnavailable verifies that
// non-NotFound apiserver failures (e.g. 5xx, forbidden) surface as
// ErrCodeUnavailable so chainsaw `error:` blocks fail closed rather
// than treating a transient failure as "resource absent".
func TestClusterFetcher_Fetch_TransientErrorMapsToUnavailable(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClient(scheme)
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver down")
	})
	f := NewClusterFetcher(client, testMapper())
	_, err := f.Fetch(context.Background(), "v1", "Pod", "ns", "p1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %T %v", err, err)
	}
	if se.Code != errors.ErrCodeUnavailable {
		t.Errorf("code = %q, want ErrCodeUnavailable (err=%v)", se.Code, err)
	}
}

func TestClusterFetcher_List(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	// Register the list kind so the fake client can route the List op.
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PodList"}, &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "NodeList"}, &unstructured.UnstructuredList{})

	freshPods := func() []runtime.Object {
		return []runtime.Object{
			newPod("ns", "match-1", map[string]string{"app": "foo", "tier": "web"}),
			newPod("ns", "match-2", map[string]string{"app": "foo", "tier": "db"}),
			newPod("ns", "no-match", map[string]string{"app": "bar"}),
			newPod("other", "elsewhere", map[string]string{"app": "foo"}),
		}
	}
	freshNodes := func() []runtime.Object {
		return []runtime.Object{newNode("n1"), newNode("n2")}
	}

	tests := []struct {
		name       string
		apiVersion string
		kind       string
		namespace  string
		labels     map[string]string
		wantNames  []string
	}{
		{
			name:       "namespaced list with no selector returns all in ns",
			apiVersion: "v1",
			kind:       "Pod",
			namespace:  "ns",
			wantNames:  []string{"match-1", "match-2", "no-match"},
		},
		{
			name:       "label selector filters to matching items",
			apiVersion: "v1",
			kind:       "Pod",
			namespace:  "ns",
			labels:     map[string]string{"app": "foo"},
			wantNames:  []string{"match-1", "match-2"},
		},
		{
			name:       "multi-key selector narrows further",
			apiVersion: "v1",
			kind:       "Pod",
			namespace:  "ns",
			labels:     map[string]string{"app": "foo", "tier": "web"},
			wantNames:  []string{"match-1"},
		},
		{
			name:       "cluster-scoped list ignores namespace argument",
			apiVersion: "v1",
			kind:       "Node",
			namespace:  "",
			wantNames:  []string{"n1", "n2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var objs []runtime.Object
			if tt.kind == "Node" {
				objs = freshNodes()
			} else {
				objs = freshPods()
			}
			client := dynamicfake.NewSimpleDynamicClient(scheme, objs...)
			f := NewClusterFetcher(client, testMapper())
			items, err := f.List(context.Background(), tt.apiVersion, tt.kind, tt.namespace, tt.labels)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			got := make([]string, 0, len(items))
			for _, it := range items {
				if md, ok := it["metadata"].(map[string]interface{}); ok {
					if n, ok := md["name"].(string); ok {
						got = append(got, n)
					}
				}
			}
			if !sameStringSet(got, tt.wantNames) {
				t.Errorf("names = %v, want %v", got, tt.wantNames)
			}
		})
	}
}

func TestClusterFetcher_List_ErrorCodes(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClient(scheme)
	f := NewClusterFetcher(client, testMapper())

	tests := []struct {
		name        string
		apiVersion  string
		kind        string
		wantErrCode errors.ErrorCode
	}{
		{"invalid apiVersion", "//bad//", "Pod", errors.ErrCodeInvalidRequest},
		{"unknown kind", "v1", "DoesNotExist", errors.ErrCodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := f.List(context.Background(), tt.apiVersion, tt.kind, "ns", nil)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected StructuredError, got %T %v", err, err)
			}
			if se.Code != tt.wantErrCode {
				t.Errorf("code = %q, want %q", se.Code, tt.wantErrCode)
			}
		})
	}
}

// Suppress unused-import warning for metav1 when none of the helpers
// reference it directly (the fake client wires it internally).
var _ = metav1.ObjectMeta{}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		if m[s] == 0 {
			return false
		}
		m[s]--
	}
	return true
}
