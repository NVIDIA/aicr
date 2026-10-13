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
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	metadatafake "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
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

// discovered is the discovery the mapper resolves through. A zero value
// enumerated every group; err is what ServerGroupsAndResources reports.
type discovered struct {
	err error
}

func (d discovered) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return nil, nil, d.err
}

// partialObject returns the metadata of a gvk object carrying exactly
// objLabels and annotations.
func partialObject(gvk schema.GroupVersionKind, namespace, name string,
	objLabels, annotations map[string]string) *metav1.PartialObjectMetadata {

	obj := &metav1.PartialObjectMetadata{}
	obj.SetGroupVersionKind(gvk)
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

func topologyObject(namespace, name string, objLabels, annotations map[string]string) *metav1.PartialObjectMetadata {
	return partialObject(topologyGVK, namespace, name, objLabels, annotations)
}

func policyObject(name string, objLabels, annotations map[string]string) *metav1.PartialObjectMetadata {
	return partialObject(policyGVK, "", name, objLabels, annotations)
}

// fakeLister is a k8sclient.MetadataLister backed by the client-go fake
// metadata client.
type fakeLister struct {
	*metadatafake.FakeMetadataClient
}

func (f fakeLister) List(ctx context.Context, gvr schema.GroupVersionResource,
	opts metav1.ListOptions) (*metav1.PartialObjectMetadataList, error) {

	return f.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, opts)
}

func scanClient(objects ...runtime.Object) fakeLister {
	scheme := metadatafake.NewTestScheme()
	metav1.AddMetaToScheme(scheme)

	return fakeLister{metadatafake.NewSimpleMetadataClient(scheme, objects...)}
}

// metadataPage returns a reactor response holding items and continueToken. The
// fake metadata client accepts only a metav1.List from a reactor, not a
// PartialObjectMetadataList.
func metadataPage(continueToken string, items ...*metav1.PartialObjectMetadata) *metav1.List {
	page := &metav1.List{ListMeta: metav1.ListMeta{Continue: continueToken}}
	for _, item := range items {
		page.Items = append(page.Items, runtime.RawExtension{Object: item})
	}

	return page
}

func topologyKind() ResourceKind {
	return ResourceKind{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"}}
}

// TestScanAtRiskOwnership checks each marker owned recognizes, and that neither
// the managed-by label nor a Helm annotation is ownership on its own.
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
			name:        "a helm hook is owned",
			objLabels:   map[string]string{labels.ManagedBy: "Helm"},
			annotations: map[string]string{helmHookAnnotation: "post-install,post-upgrade"},
			wantAtRisk:  false,
		},
		{
			name:        "hook annotation alone is not ownership",
			annotations: map[string]string{helmHookAnnotation: "post-install,post-upgrade"},
			wantAtRisk:  true,
		},
		{
			name:        "a hook managed by something other than Helm",
			objLabels:   map[string]string{labels.ManagedBy: "kustomize"},
			annotations: map[string]string{helmHookAnnotation: "post-install"},
			wantAtRisk:  true,
		},
		{
			name:        "an empty hook annotation is not a hook",
			objLabels:   map[string]string{labels.ManagedBy: "Helm"},
			annotations: map[string]string{helmHookAnnotation: "  "},
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
			got, err := scanAtRisk(t.Context(), client, scanMapper(), discovered{}, []ResourceKind{topologyKind()})
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
	got, err := scanAtRisk(t.Context(), client, scanMapper(), discovered{}, []ResourceKind{topologyKind(), policyKind})
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
	got, err := scanAtRisk(t.Context(), client, scanMapper(), discovered{}, []ResourceKind{
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

			_, err := scanAtRisk(t.Context(), scanClient(), brokenMapper{err: tt.err}, discovered{}, []ResourceKind{tt.kind})
			if err == nil {
				t.Fatal("scanAtRisk reported success on a discovery failure")
			}
			if errorstest.ReportedCode(err) != tt.code {
				t.Errorf("error = %v, want %s", err, tt.code)
			}
			if !strings.Contains(err.Error(), kindDescription(tt.kind)) {
				t.Errorf("error = %v, want it to name %s", err, kindDescription(tt.kind))
			}
		})
	}
}

// TestScanAtRiskRefusesAbsenceDiscoveryCannotEstablish keeps the skip above
// narrow in the other direction. client-go discards a partial discovery
// failure whenever other groups answered, so a kind in an unreachable group
// comes back as a bare no-match. Reading that as "not installed" skips its
// objects and reports a clean scan, so absence stands only once the kind's own
// group is known to have been enumerated.
func TestScanAtRiskRefusesAbsenceDiscoveryCannotEstablish(t *testing.T) {
	t.Parallel()

	skyhook := ResourceKind{Group: "skyhook.nvidia.com", Kind: "Skyhook", Components: []string{"nodewright-operator"}}
	groupFailed := func(group string) error {
		return &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
			{Group: group, Version: "v1alpha1"}: stderrors.New("the server is currently unable to handle the request"),
		}}
	}

	tests := []struct {
		name        string
		mapper      meta.RESTMapper
		groups      k8sclient.GroupResourceLister
		wantAbsent  bool
		wantContain []string
	}{
		{
			name:        "the kind's own group failed discovery",
			mapper:      scanMapper(),
			groups:      discovered{err: groupFailed("skyhook.nvidia.com")},
			wantContain: []string{"Skyhook.skyhook.nvidia.com", `API group "skyhook.nvidia.com" is incomplete`},
		},
		{
			// A broken aggregated APIService elsewhere is the steady state on
			// many clusters, and says nothing about this kind.
			name:       "only another group failed discovery",
			mapper:     scanMapper(),
			groups:     discovered{err: groupFailed("metrics.k8s.io")},
			wantAbsent: true,
		},
		{
			name:        "discovery failed outright",
			mapper:      scanMapper(),
			groups:      discovered{err: stderrors.New("i/o timeout reaching discovery")},
			wantContain: []string{"Skyhook.skyhook.nvidia.com", "incomplete"},
		},
		{
			name:        "no discovery to confirm against",
			mapper:      scanMapper(),
			wantContain: []string{"Skyhook.skyhook.nvidia.com", "no discovery to confirm"},
		},
		{
			// The shape client-go returns when it does keep the wrapper.
			name: "a no-match that carries the group failure",
			mapper: brokenMapper{err: fmt.Errorf("%w: %w", groupFailed("skyhook.nvidia.com"),
				&meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "skyhook.nvidia.com", Kind: "Skyhook"}})},
			groups:      discovered{},
			wantContain: []string{"Skyhook.skyhook.nvidia.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := scanAtRisk(t.Context(), scanClient(), tt.mapper, tt.groups, []ResourceKind{skyhook})
			if tt.wantAbsent {
				if err != nil {
					t.Fatalf("scanAtRisk() error = %v, want the kind reported absent", err)
				}
				want := []ScannedKind{{Group: skyhook.Group, Kind: skyhook.Kind, Components: skyhook.Components}}
				if !reflect.DeepEqual(got.Kinds, want) {
					t.Errorf("Kinds = %#v, want %#v", got.Kinds, want)
				}

				return
			}
			if err == nil {
				t.Fatalf("scanAtRisk() = %#v, want an error rather than the kind reported absent", got)
			}
			if errorstest.ReportedCode(err) != errors.ErrCodeUnavailable {
				t.Errorf("error = %v, want %s", err, errors.ErrCodeUnavailable)
			}
			for _, want := range tt.wantContain {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to contain %q", err, want)
				}
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

			got, err := scanAtRisk(t.Context(), client, scanMapper(), discovered{}, []ResourceKind{topologyKind()})
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

	_, err := scanAtRisk(t.Context(), client, scanMapper(), discovered{}, []ResourceKind{topologyKind()})
	if err == nil {
		t.Fatal("scanAtRisk reported success on a forbidden List")
	}
	if errorstest.ReportedCode(err) != errors.ErrCodeUnavailable {
		t.Errorf("error = %v, want ErrCodeUnavailable", err)
	}
}

// TestScanAtRiskPagesAndRefusesARepeatedToken mirrors the inventory readers:
// the whole cluster's objects of a kind are read, and a server echoing one
// continue token must not turn that into an unbounded request flood.
func TestScanAtRiskPagesAndRefusesARepeatedToken(t *testing.T) {
	t.Parallel()

	// The fake client's tracker answers every List with everything it holds
	// and no continue token, whatever Limit asks for, so this case proves only
	// that a response larger than one page is read whole. The paging loop
	// itself is exercised by the two reactor cases below.
	t.Run("reads a response larger than one page", func(t *testing.T) {
		t.Parallel()

		const total = int(defaults.AtRiskListPageSize) + 7
		objects := make([]runtime.Object, 0, total)
		for i := range total {
			objects = append(objects, topologyObject("tenant-a", "topology-"+strconv.Itoa(i), nil, nil))
		}
		got, err := scanAtRisk(t.Context(), scanClient(objects...), scanMapper(), discovered{}, []ResourceKind{topologyKind()})
		if err != nil {
			t.Fatalf("scanAtRisk: %v", err)
		}
		if got.Kinds[0].Examined != total {
			t.Errorf("Examined = %d, want %d: the walk dropped items it was handed", got.Kinds[0].Examined, total)
		}
		if len(got.Findings) != total {
			t.Errorf("Findings = %d, want %d", len(got.Findings), total)
		}
	})

	// The accumulation half of paging, which the case above cannot reach: a
	// loop that returns the first page alone still counts and reports every
	// object there, so only a second page carrying a different object shows
	// whether what it holds survives into the result.
	t.Run("keeps the findings from every page", func(t *testing.T) {
		t.Parallel()

		pages := []*metav1.List{
			metadataPage("page-2", topologyObject("tenant-a", "first-page", nil, nil)),
			metadataPage("", topologyObject("tenant-b", "second-page", nil, nil)),
		}

		listed := 0
		client := scanClient()
		client.PrependReactor("list", "clustertopologies",
			func(k8stesting.Action) (bool, runtime.Object, error) {
				page := pages[min(listed, len(pages)-1)]
				listed++

				return true, page, nil
			})

		got, err := scanAtRisk(t.Context(), client, scanMapper(), discovered{}, []ResourceKind{topologyKind()})
		if err != nil {
			t.Fatalf("scanAtRisk: %v", err)
		}
		if listed != len(pages) {
			t.Errorf("listed %d times, want %d", listed, len(pages))
		}
		if got.Kinds[0].Examined != len(pages) {
			t.Errorf("Examined = %d, want %d", got.Kinds[0].Examined, len(pages))
		}
		want := []AtRiskObject{
			{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
				Namespace: "tenant-a", Name: "first-page"},
			{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
				Namespace: "tenant-b", Name: "second-page"},
		}
		if !reflect.DeepEqual(got.Findings, want) {
			t.Errorf("Findings = %#v, want %#v", got.Findings, want)
		}
	})

	t.Run("refuses a repeated continue token", func(t *testing.T) {
		t.Parallel()

		client := scanClient()
		client.PrependReactor("list", "clustertopologies",
			func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, metadataPage("same-token-forever"), nil
			})

		_, err := scanAtRisk(t.Context(), client, scanMapper(), discovered{}, []ResourceKind{topologyKind()})
		if err == nil {
			t.Fatal("scanAtRisk followed a repeated continue token without failing")
		}
		if errorstest.ReportedCode(err) != errors.ErrCodeInternal {
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
				scanMapper(), discovered{}, []ResourceKind{topologyKind()})
			if err == nil {
				t.Fatal("scanAtRisk on an ended context returned no error")
			}
			if errorstest.ReportedCode(err) != tt.wantCode {
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
			_, err := scanAtRisk(t.Context(), client, scanMapper(), discovered{}, []ResourceKind{topologyKind()})
			if err == nil {
				t.Fatal("scanAtRisk reported success on an aborted List")
			}
			if errorstest.ReportedCode(err) != tt.wantCode {
				t.Errorf("error = %v, want %s", err, tt.wantCode)
			}
		})
	}
}

// TestScanAtRiskRejectsAnEmptyKind refuses a request that would resolve to no
// resource at best and to an unintended one at worst.
func TestScanAtRiskRejectsAnEmptyKind(t *testing.T) {
	t.Parallel()

	_, err := scanAtRisk(t.Context(), scanClient(), scanMapper(), discovered{}, []ResourceKind{{Group: "grove.io"}})
	if err == nil {
		t.Fatal("scanAtRisk accepted a kind with no name")
	}
	if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
		t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
	}
}

// TestScanAtRiskRejectsAnEmptyKindBeforeDialing pins caller error winning over
// environment error. The kubeconfig names nothing, so a scan that builds its
// clients first would answer a malformed request with an infrastructure
// failure and bury the input the operator has to fix.
func TestScanAtRiskRejectsAnEmptyKindBeforeDialing(t *testing.T) {
	t.Parallel()

	_, err := ScanAtRisk(t.Context(), AtRiskOptions{
		Kubeconfig: "/nonexistent/kubeconfig",
		Kinds:      []ResourceKind{{Group: "grove.io"}},
	})
	if err == nil {
		t.Fatal("ScanAtRisk accepted a kind with no name")
	}
	if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
		t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "no kind") {
		t.Errorf("error = %v, want the empty-kind message rather than a client failure", err)
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

			var noClient k8sclient.MetadataLister
			got, err := scanAtRisk(t.Context(), noClient, nil, nil, tt.kinds)
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
		got, err := scanAtRisk(t.Context(), client, scanMapper(), discovered{},
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
			return true, metadataPage("", topologyObject("tenant-a", "", nil, nil)), nil
		})

	got, err := scanAtRisk(t.Context(), client, scanMapper(), discovered{}, []ResourceKind{topologyKind()})
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

// TestScanAtRiskReadsOnlyMetadataLists runs the scan through a real
// MetadataListClient. It checks that each kind is listed cluster-wide, and
// that a full-object response fails the scan without its body reaching the
// result or the error.
func TestScanAtRiskReadsOnlyMetadataLists(t *testing.T) {
	t.Parallel()

	const (
		sentinel     = "c2VjcmV0LXNlbnRpbmVs"
		topologyPath = "/apis/grove.io/v1alpha1/clustertopologies"
	)
	secretGVK := schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{topologyGVK.GroupVersion(), secretGVK.GroupVersion()})
	mapper.AddSpecific(topologyGVK, topologyGVR, topologyGVR.GroupVersion().WithResource("clustertopology"),
		meta.RESTScopeNamespace)
	mapper.AddSpecific(secretGVK, secretGVR, secretGVR.GroupVersion().WithResource("secret"), meta.RESTScopeNamespace)
	secretKind := ResourceKind{Kind: "Secret", Components: []string{"grove"}}

	tests := []struct {
		name         string
		kind         ResourceKind
		wantPath     string
		status       int
		body         string
		wantFindings []AtRiskObject
		wantErr      bool
	}{
		{
			name:     "a metadata list is scanned",
			kind:     topologyKind(),
			wantPath: topologyPath,
			status:   http.StatusOK,
			body: `{"kind":"PartialObjectMetadataList","apiVersion":"meta.k8s.io/v1","items":[` +
				`{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",` +
				`"metadata":{"name":"topology-1","namespace":"tenant-a"}}]}`,
			wantFindings: []AtRiskObject{{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
				Namespace: "tenant-a", Name: "topology-1"}},
		},
		{
			name:     "a full custom resource list is refused",
			kind:     topologyKind(),
			wantPath: topologyPath,
			status:   http.StatusOK,
			body: `{"kind":"ClusterTopologyList","apiVersion":"grove.io/v1alpha1","items":[` +
				`{"metadata":{"name":"topology-1","namespace":"tenant-a"},"spec":{"token":"` + sentinel + `"}}]}`,
			wantErr: true,
		},
		{
			name:     "a full Secret list is refused",
			kind:     secretKind,
			wantPath: "/api/v1/secrets",
			status:   http.StatusOK,
			body: `{"kind":"SecretList","apiVersion":"v1","items":[` +
				`{"metadata":{"name":"creds","namespace":"tenant-a"},"data":{"password":"` + sentinel + `"}}]}`,
			wantErr: true,
		},
		{
			name:     "a 406 is refused",
			kind:     topologyKind(),
			wantPath: topologyPath,
			status:   http.StatusNotAcceptable,
			body: `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotAcceptable","code":406,` +
				`"message":"only the following media types are accepted: application/json"}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			requests := make(chan *url.URL, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.URL
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			lister, err := k8sclient.NewMetadataListClientForConfig(&rest.Config{Host: srv.URL})
			if err != nil {
				t.Fatalf("NewMetadataListClientForConfig: %v", err)
			}

			got, err := scanAtRisk(t.Context(), lister, mapper, discovered{}, []ResourceKind{tt.kind})
			select {
			case requested := <-requests:
				if requested.Path != tt.wantPath {
					t.Errorf("listed %q, want the cluster-wide %q", requested.Path, tt.wantPath)
				}
			default:
				t.Fatalf("scanAtRisk sent no List, result %#v, error %v", got, err)
			}
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeUnavailable)
				if err != nil && strings.Contains(err.Error(), sentinel) {
					t.Errorf("error = %q, which quotes the response body", err)
				}
				if !reflect.DeepEqual(got, AtRiskResult{}) {
					t.Errorf("result = %#v, want nothing from a refused response", got)
				}

				return
			}
			if err != nil {
				t.Fatalf("scanAtRisk: %v", err)
			}
			if len(got.Kinds) != 1 {
				t.Fatalf("Kinds = %#v, want the one kind scanned", got.Kinds)
			}
			if got.Kinds[0].Examined != len(tt.wantFindings) {
				t.Errorf("Examined = %d, want %d", got.Kinds[0].Examined, len(tt.wantFindings))
			}
			if !reflect.DeepEqual(got.Findings, tt.wantFindings) {
				t.Errorf("Findings = %#v, want %#v", got.Findings, tt.wantFindings)
			}
		})
	}
}

func TestScanAtRiskRetriesADroppedConnection(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			_ = conn.Close()

			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"PartialObjectMetadataList","apiVersion":"meta.k8s.io/v1","items":[` +
			`{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",` +
			`"metadata":{"name":"topology-1","namespace":"tenant-a"}}]}`))
	}))
	t.Cleanup(srv.Close)
	lister, err := k8sclient.NewMetadataListClientForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("NewMetadataListClientForConfig: %v", err)
	}

	got, err := scanAtRisk(t.Context(), lister, scanMapper(), discovered{}, []ResourceKind{topologyKind()})
	if err != nil {
		t.Fatalf("scanAtRisk: %v", err)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("requests = %d, want the dropped List and its retry", n)
	}
	want := []AtRiskObject{{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
		Namespace: "tenant-a", Name: "topology-1"}}
	if !reflect.DeepEqual(got.Findings, want) {
		t.Errorf("Findings = %#v, want %#v", got.Findings, want)
	}
}
