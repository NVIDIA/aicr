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
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	"github.com/NVIDIA/aicr/pkg/upgrade"
)

// TestResourceKindMatchesUpgrade fails when the two declarations drift apart.
//
// ResourceKind is declared here rather than imported, for the reason
// TestDeployersMatchBundlerConfig gives about the deployer names: pkg/upgrade
// is the wrong dependency for a package that reads a cluster, and the report
// types there must stay free of this one. This test carries the import
// instead, so the conversion every caller has to write is checked by the
// compiler somewhere — a field added on one side and not the other would
// otherwise leave the scan silently looking for the wrong thing.
func TestResourceKindMatchesUpgrade(t *testing.T) {
	t.Parallel()

	from := upgrade.ResourceKind{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"}}
	got := ResourceKind(from)
	want := ResourceKind{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("converted kind = %#v, want %#v", got, want)
	}
}

// The scan's fixtures are a namespaced CRD (ClusterTopology, standing in for
// the grove record's real one) and a cluster-scoped one, so scope is exercised
// in both directions.
var (
	topologyGVK = schema.GroupVersionKind{Group: "grove.io", Version: "v1alpha1", Kind: "ClusterTopology"}
	topologyGVR = schema.GroupVersionResource{Group: "grove.io", Version: "v1alpha1", Resource: "clustertopologies"}
	policyGVK   = schema.GroupVersionKind{Group: "grove.io", Version: "v1alpha1", Kind: "ClusterPolicy"}
	policyGVR   = schema.GroupVersionResource{Group: "grove.io", Version: "v1alpha1", Resource: "clusterpolicies"}
)

// scanMapper resolves only the two kinds above, so every other kind produces
// the no-match a cluster without the CRD installed produces.
func scanMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{topologyGVK.GroupVersion()})
	mapper.AddSpecific(topologyGVK, topologyGVR, topologyGVR.GroupVersion().WithResource("clustertopology"),
		meta.RESTScopeNamespace)
	mapper.AddSpecific(policyGVK, policyGVR, policyGVR.GroupVersion().WithResource("clusterpolicy"),
		meta.RESTScopeRoot)

	return mapper
}

// brokenMapper is a discovery failure rather than a no-match: the difference
// between "the CRD is not installed" and "we could not find out".
type brokenMapper struct {
	meta.RESTMapper
	err error
}

func (m brokenMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, m.err
}

// topologyObject is one ClusterTopology carrying exactly the labels and
// annotations given.
func topologyObject(namespace, name string, objLabels, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(topologyGVK)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if len(objLabels) > 0 {
		obj.SetLabels(objLabels)
	}
	if len(annotations) > 0 {
		obj.SetAnnotations(annotations)
	}

	return obj
}

func policyObject(name string, objLabels, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(policyGVK)
	obj.SetName(name)
	if len(objLabels) > 0 {
		obj.SetLabels(objLabels)
	}
	if len(annotations) > 0 {
		obj.SetAnnotations(annotations)
	}

	return obj
}

func scanClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			topologyGVR: "ClusterTopologyList",
			policyGVR:   "ClusterPolicyList",
		}, objects...)
}

func topologyKind() ResourceKind {
	return ResourceKind{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"}}
}

// TestScanAtRiskOwnership is the ownership rule, one object per case.
//
// The Helm pair is the mutation this table exists for: the managed-by label
// alone is not ownership. Any controller, any hand-written manifest and any
// chart rendered by something other than Helm can carry it, while
// meta.helm.sh/release-name is written only by Helm's own install path.
func TestScanAtRiskOwnership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		objLabels   map[string]string
		annotations map[string]string
		wantAtRisk  bool
	}{
		{
			name:        "helm owns it",
			objLabels:   map[string]string{labels.ManagedBy: "Helm"},
			annotations: map[string]string{helmReleaseNameAnnotation: "grove-operator"},
			wantAtRisk:  false,
		},
		{
			name:       "managed-by label alone is not ownership",
			objLabels:  map[string]string{labels.ManagedBy: "Helm"},
			wantAtRisk: true,
		},
		{
			name:        "release annotation alone is not ownership",
			annotations: map[string]string{helmReleaseNameAnnotation: "grove-operator"},
			wantAtRisk:  true,
		},
		{
			name:        "managed by something other than Helm",
			objLabels:   map[string]string{labels.ManagedBy: "kustomize"},
			annotations: map[string]string{helmReleaseNameAnnotation: "grove-operator"},
			wantAtRisk:  true,
		},
		{
			name:        "an empty release name is not a release",
			objLabels:   map[string]string{labels.ManagedBy: "Helm"},
			annotations: map[string]string{helmReleaseNameAnnotation: ""},
			wantAtRisk:  true,
		},
		{
			name:        "argo owns it",
			annotations: map[string]string{argoTrackingIDAnnotation: "grove:grove.io/ClusterTopology:ns/name"},
			wantAtRisk:  false,
		},
		{
			name:        "an empty tracking id is not ownership",
			annotations: map[string]string{argoTrackingIDAnnotation: "   "},
			wantAtRisk:  true,
		},
		{
			name:       "no marker at all",
			wantAtRisk: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := scanClient(topologyObject("tenant-a", "topology-1", tt.objLabels, tt.annotations))
			got, err := scanAtRisk(t.Context(), client, scanMapper(), []ResourceKind{topologyKind()})
			if err != nil {
				t.Fatalf("scanAtRisk: %v", err)
			}
			wantKinds := []ScannedKind{
				{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"}, Present: true, Examined: 1},
			}
			if !reflect.DeepEqual(got.Kinds, wantKinds) {
				t.Errorf("Kinds = %#v, want %#v", got.Kinds, wantKinds)
			}
			if tt.wantAtRisk {
				want := []AtRiskObject{
					{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
						Namespace: "tenant-a", Name: "topology-1"},
				}
				if !reflect.DeepEqual(got.Findings, want) {
					t.Errorf("Findings = %#v, want %#v", got.Findings, want)
				}

				return
			}
			if len(got.Findings) != 0 {
				t.Errorf("Findings = %#v, want none: the object carries an ownership marker", got.Findings)
			}
		})
	}
}

// TestScanAtRiskSeparatesOwnedFromUnowned proves Examined is a real
// denominator rather than a copy of the finding count: an all-clean kind and a
// partly-owned one must both report every object they read.
func TestScanAtRiskSeparatesOwnedFromUnowned(t *testing.T) {
	t.Parallel()

	client := scanClient(
		topologyObject("tenant-a", "owned-by-helm",
			map[string]string{labels.ManagedBy: "Helm"},
			map[string]string{helmReleaseNameAnnotation: "grove-operator"}),
		topologyObject("tenant-b", "hand-written", nil, nil),
		topologyObject("tenant-a", "owned-by-argo", nil,
			map[string]string{argoTrackingIDAnnotation: "grove:grove.io/ClusterTopology:tenant-a/owned-by-argo"}),
		policyObject("cluster-policy", nil, nil),
	)
	// Two owners on the policy kind, so the scan is shown copying the whole
	// list onto a finding rather than picking one.
	policyKind := ResourceKind{Group: "grove.io", Kind: "ClusterPolicy", Components: []string{"grove", "kai-scheduler"}}
	got, err := scanAtRisk(t.Context(), client, scanMapper(), []ResourceKind{topologyKind(), policyKind})
	if err != nil {
		t.Fatalf("scanAtRisk: %v", err)
	}

	wantKinds := []ScannedKind{
		{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"}, Present: true, Examined: 3},
		{Group: "grove.io", Kind: "ClusterPolicy", Components: []string{"grove", "kai-scheduler"},
			Present: true, Examined: 1},
	}
	if !reflect.DeepEqual(got.Kinds, wantKinds) {
		t.Errorf("Kinds = %#v, want %#v", got.Kinds, wantKinds)
	}
	// The cluster-scoped finding carries no namespace, which is how a reader
	// tells "cluster-scoped" from "namespace unknown".
	wantFindings := []AtRiskObject{
		{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
			Namespace: "tenant-b", Name: "hand-written"},
		{Group: "grove.io", Kind: "ClusterPolicy", Components: []string{"grove", "kai-scheduler"},
			Name: "cluster-policy"},
	}
	if !reflect.DeepEqual(got.Findings, wantFindings) {
		t.Errorf("Findings = %#v, want %#v", got.Findings, wantFindings)
	}
}

// TestScanAtRiskSkipsKindsTheClusterDoesNotServe is the fail-open direction
// the scan is allowed: a record naming a CRD that is not installed is the
// ordinary case, not a failure.
func TestScanAtRiskSkipsKindsTheClusterDoesNotServe(t *testing.T) {
	t.Parallel()

	client := scanClient(topologyObject("tenant-a", "topology-1", nil, nil))
	got, err := scanAtRisk(t.Context(), client, scanMapper(), []ResourceKind{
		{Group: "absent.io", Kind: "NeverInstalled", Components: []string{"absent-operator"}},
		// Core group, which a record names with no group at all. It resolves
		// the same way and is named without a suffix in any message.
		{Kind: "Widget"},
		topologyKind(),
	})
	if err != nil {
		t.Fatalf("scanAtRisk on an uninstalled kind: %v", err)
	}
	// An uninstalled kind keeps its attribution: it produces no finding row,
	// so the accounting line is the only place a reader learns whose upgrade
	// named it.
	wantKinds := []ScannedKind{
		{Group: "absent.io", Kind: "NeverInstalled", Components: []string{"absent-operator"}},
		{Kind: "Widget", Present: false, Examined: 0},
		{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"}, Present: true, Examined: 1},
	}
	if !reflect.DeepEqual(got.Kinds, wantKinds) {
		t.Errorf("Kinds = %#v, want %#v", got.Kinds, wantKinds)
	}
	if len(got.Findings) != 1 {
		t.Errorf("Findings = %#v, want the one installed kind's object", got.Findings)
	}
}

// TestScanAtRiskFailsOnADiscoveryOutage keeps the skip above narrow. A mapper
// that could not reach the apiserver has not told us the kind is absent, and
// reporting it as absent turns an outage into an all-clear.
func TestScanAtRiskFailsOnADiscoveryOutage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind ResourceKind
		err  error
		code errors.ErrorCode
	}{
		{"timeout", topologyKind(), stderrors.New("i/o timeout reaching discovery"), errors.ErrCodeUnavailable},
		{"forbidden", topologyKind(), apierrors.NewForbidden(
			schema.GroupResource{Group: "grove.io", Resource: "clustertopologies"}, "",
			stderrors.New("no discovery access")), errors.ErrCodeUnavailable},
		// A core-group kind, whose name carries no group suffix in the message.
		{"core group", ResourceKind{Kind: "PersistentVolume"},
			stderrors.New("i/o timeout reaching discovery"), errors.ErrCodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := scanAtRisk(t.Context(), scanClient(), brokenMapper{err: tt.err}, []ResourceKind{tt.kind})
			if err == nil {
				t.Fatal("scanAtRisk reported success on a discovery failure")
			}
			if !stderrors.Is(err, errors.New(tt.code, "")) {
				t.Errorf("error = %v, want %s", err, tt.code)
			}
			if !strings.Contains(err.Error(), kindDescription(tt.kind)) {
				t.Errorf("error = %v, want it to name %s", err, kindDescription(tt.kind))
			}
		})
	}
}

// TestScanAtRiskSkipsAKindThatVanished covers the CRD removed between the
// mapping and the List, which the apiserver answers the same way it answers a
// kind that was never there.
func TestScanAtRiskSkipsAKindThatVanished(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{"not found", apierrors.NewNotFound(
			schema.GroupResource{Group: "grove.io", Resource: "clustertopologies"}, "")},
		{"no match", &meta.NoKindMatchError{GroupKind: topologyGVK.GroupKind()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := scanClient()
			client.PrependReactor("list", "clustertopologies",
				func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, tt.err })

			got, err := scanAtRisk(t.Context(), client, scanMapper(), []ResourceKind{topologyKind()})
			if err != nil {
				t.Fatalf("scanAtRisk: %v", err)
			}
			want := []ScannedKind{{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"}}}
			if !reflect.DeepEqual(got.Kinds, want) {
				t.Errorf("Kinds = %#v, want %#v", got.Kinds, want)
			}
		})
	}
}

// TestScanAtRiskFailsOnAListDenial separates the skip above from an RBAC
// denial, which says nothing about whether objects exist.
func TestScanAtRiskFailsOnAListDenial(t *testing.T) {
	t.Parallel()

	client := scanClient()
	client.PrependReactor("list", "clustertopologies",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "grove.io", Resource: "clustertopologies"}, "",
				stderrors.New("User cannot list resource at the cluster scope"))
		})

	_, err := scanAtRisk(t.Context(), client, scanMapper(), []ResourceKind{topologyKind()})
	if err == nil {
		t.Fatal("scanAtRisk reported success on a forbidden List")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")) {
		t.Errorf("error = %v, want ErrCodeUnavailable", err)
	}
}

// TestScanAtRiskPagesAndRefusesARepeatedToken mirrors the inventory readers:
// the whole cluster's objects of a kind are read, and a server echoing one
// continue token must not turn that into an unbounded request flood.
func TestScanAtRiskPagesAndRefusesARepeatedToken(t *testing.T) {
	t.Parallel()

	t.Run("pages through every object", func(t *testing.T) {
		t.Parallel()

		const total = int(defaults.AtRiskListPageSize) + 7
		objects := make([]runtime.Object, 0, total)
		for i := range total {
			objects = append(objects, topologyObject("tenant-a", "topology-"+strconv.Itoa(i), nil, nil))
		}
		got, err := scanAtRisk(t.Context(), scanClient(objects...), scanMapper(), []ResourceKind{topologyKind()})
		if err != nil {
			t.Fatalf("scanAtRisk: %v", err)
		}
		if got.Kinds[0].Examined != total {
			t.Errorf("Examined = %d, want %d: the walk stopped at a page boundary", got.Kinds[0].Examined, total)
		}
		if len(got.Findings) != total {
			t.Errorf("Findings = %d, want %d", len(got.Findings), total)
		}
	})

	t.Run("refuses a repeated continue token", func(t *testing.T) {
		t.Parallel()

		client := scanClient()
		client.PrependReactor("list", "clustertopologies",
			func(k8stesting.Action) (bool, runtime.Object, error) {
				page := &unstructured.UnstructuredList{}
				page.SetContinue("same-token-forever")

				return true, page, nil
			})

		_, err := scanAtRisk(t.Context(), client, scanMapper(), []ResourceKind{topologyKind()})
		if err == nil {
			t.Fatal("scanAtRisk followed a repeated continue token without failing")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
			t.Errorf("error = %v, want ErrCodeInternal", err)
		}
	})
}

// TestScanAtRiskHonorsCancellation pins that an aborted scan returns an error
// rather than the short list it had reached, which would read as an all-clear,
// and that the two ways a context ends stay apart.
//
// A cancellation is an operator instruction to stop and must be terminal; a
// deadline is an environmental fault a caller may retry. Collapsing them puts
// a Ctrl-C into a retry loop and exits with the wrong status.
func TestScanAtRiskHonorsCancellation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		ctx         func(t *testing.T) context.Context
		wantCode    errors.ErrorCode
		wantMessage string
		transient   bool
	}{
		{
			name: "operator abort",
			ctx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				return ctx
			},
			wantCode:    errors.ErrCodeCanceled,
			wantMessage: "canceled while reading",
			transient:   false,
		},
		{
			name: "deadline",
			ctx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)

				return ctx
			},
			wantCode:    errors.ErrCodeTimeout,
			wantMessage: "timed out reading",
			transient:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := scanAtRisk(tt.ctx(t), scanClient(topologyObject("tenant-a", "topology-1", nil, nil)),
				scanMapper(), []ResourceKind{topologyKind()})
			if err == nil {
				t.Fatal("scanAtRisk on an ended context returned no error")
			}
			if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("error = %v, want %s", err, tt.wantCode)
			}
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Errorf("error = %q, want it to say %q", err, tt.wantMessage)
			}
			if got := errors.IsTransient(err); got != tt.transient {
				t.Errorf("IsTransient = %v, want %v", got, tt.transient)
			}
		})
	}
}

// TestScanAtRiskClassifiesAnAbortMidList covers the abort that arrives while a
// List is in flight, which the per-page check cannot see. Without the
// classification it reports as an unavailable apiserver, which is both the
// wrong code and the wrong diagnosis.
func TestScanAtRiskClassifiesAnAbortMidList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		listErr  error
		wantCode errors.ErrorCode
	}{
		{"canceled", fmt.Errorf("get %q: %w", "/apis/grove.io/v1alpha1/clustertopologies", context.Canceled),
			errors.ErrCodeCanceled},
		{"deadline", fmt.Errorf("client rate limiter Wait: %w", context.DeadlineExceeded), errors.ErrCodeTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := scanClient()
			client.PrependReactor("list", "clustertopologies",
				func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, tt.listErr })

			// The context itself is live, so only the returned error can carry
			// the classification.
			_, err := scanAtRisk(t.Context(), client, scanMapper(), []ResourceKind{topologyKind()})
			if err == nil {
				t.Fatal("scanAtRisk reported success on an aborted List")
			}
			if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("error = %v, want %s", err, tt.wantCode)
			}
		})
	}
}

// TestScanAtRiskRejectsAnEmptyKind refuses a request that would resolve to no
// resource at best and to an unintended one at worst.
func TestScanAtRiskRejectsAnEmptyKind(t *testing.T) {
	t.Parallel()

	_, err := scanAtRisk(t.Context(), scanClient(), scanMapper(), []ResourceKind{{Group: "grove.io"}})
	if err == nil {
		t.Fatal("scanAtRisk accepted a kind with no name")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
	}
}

// TestScanAtRiskWithNoKindsContactsNothing covers the ordinary case of an
// upgrade whose crossed records name no resources: a nil client would panic if
// the scan reached for one.
func TestScanAtRiskWithNoKindsContactsNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		kinds []ResourceKind
	}{
		{"nil", nil},
		{"empty", []ResourceKind{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var noClient dynamic.Interface
			got, err := scanAtRisk(t.Context(), noClient, nil, tt.kinds)
			if err != nil {
				t.Fatalf("scanAtRisk: %v", err)
			}
			if len(got.Kinds) != 0 || len(got.Findings) != 0 {
				t.Errorf("result = %#v, want empty", got)
			}

			// Through the exported entry too, which would otherwise resolve a
			// kubeconfig and dial. A test host has none, so reaching that far
			// is itself the failure.
			got, err = ScanAtRisk(t.Context(), AtRiskOptions{
				Kubeconfig: "/nonexistent/kubeconfig",
				Kinds:      tt.kinds,
			})
			if err != nil {
				t.Fatalf("ScanAtRisk: %v", err)
			}
			if len(got.Kinds) != 0 || len(got.Findings) != 0 {
				t.Errorf("result = %#v, want empty", got)
			}
		})
	}
}

// TestScanAtRiskFindingsFollowRequestOrder keeps the report stable across runs
// over identical cluster state.
func TestScanAtRiskFindingsFollowRequestOrder(t *testing.T) {
	t.Parallel()

	client := scanClient(
		topologyObject("tenant-b", "b-thing", nil, nil),
		topologyObject("tenant-a", "a-thing", nil, nil),
		policyObject("policy-1", nil, nil),
	)
	want := []AtRiskObject{
		{Group: "grove.io", Kind: "ClusterPolicy", Components: []string{"grove"}, Name: "policy-1"},
		{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
			Namespace: "tenant-a", Name: "a-thing"},
		{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
			Namespace: "tenant-b", Name: "b-thing"},
	}
	for range 10 {
		got, err := scanAtRisk(t.Context(), client, scanMapper(),
			[]ResourceKind{{Group: "grove.io", Kind: "ClusterPolicy", Components: []string{"grove"}}, topologyKind()})
		if err != nil {
			t.Fatalf("scanAtRisk: %v", err)
		}
		if !reflect.DeepEqual(got.Findings, want) {
			t.Fatalf("Findings = %#v, want %#v", got.Findings, want)
		}
	}
}

// TestScanAtRiskSkipsAnUnnamedObject covers an object a List returned with no
// name: reporting it would print a finding an operator cannot look up.
func TestScanAtRiskSkipsAnUnnamedObject(t *testing.T) {
	t.Parallel()

	client := scanClient()
	client.PrependReactor("list", "clustertopologies",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			nameless := &unstructured.Unstructured{}
			nameless.SetGroupVersionKind(topologyGVK)
			nameless.SetNamespace("tenant-a")
			page := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*nameless}}

			return true, page, nil
		})

	got, err := scanAtRisk(t.Context(), client, scanMapper(), []ResourceKind{topologyKind()})
	if err != nil {
		t.Fatalf("scanAtRisk: %v", err)
	}
	if got.Kinds[0].Examined != 1 {
		t.Errorf("Examined = %d, want 1: an unnamed object was still read", got.Kinds[0].Examined)
	}
	if len(got.Findings) != 0 {
		t.Errorf("Findings = %#v, want none: an unnamed object cannot be looked up", got.Findings)
	}
}

// TestScanAtRiskListsEveryNamespace pins the cluster-wide read. A scan scoped
// to one namespace would miss exactly the tenant objects this warning exists
// for.
func TestScanAtRiskListsEveryNamespace(t *testing.T) {
	t.Parallel()

	client := scanClient(topologyObject("tenant-a", "a", nil, nil))
	var seen []string
	client.PrependReactor("list", "clustertopologies", func(action k8stesting.Action) (bool, runtime.Object, error) {
		seen = append(seen, action.GetNamespace())

		return false, nil, nil
	})

	if _, err := scanAtRisk(t.Context(), client, scanMapper(), []ResourceKind{topologyKind()}); err != nil {
		t.Fatalf("scanAtRisk: %v", err)
	}
	if !reflect.DeepEqual(seen, []string{metav1.NamespaceAll}) {
		t.Errorf("listed namespaces = %#v, want one cluster-wide List", seen)
	}
}
