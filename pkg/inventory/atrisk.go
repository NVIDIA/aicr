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
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
)

// The markers that make an object somebody's. Helm writes the label and the
// annotation together on everything it installs; Argo CD writes its tracking
// id on everything it syncs.
const (
	helmManagedByValue        = "Helm"
	helmReleaseNameAnnotation = "meta.helm.sh/release-name"
	argoTrackingIDAnnotation  = "argocd.argoproj.io/tracking-id"
)

// atRiskSubject is what a canceled scan was reading, as ctxErr names it.
const atRiskSubject = "resources an upgrade could disturb"

// ResourceKind is one group and kind the at-risk scan examines. An empty Group
// is the core API group.
//
// It is a local type for the reason Component is: this package must not import
// pkg/upgrade, whose records declare these. upgrade.AffectedKinds produces the
// list and the caller holding both converts; atrisk_test.go carries that
// import so the conversion cannot drift silently.
type ResourceKind struct {
	Group string
	Kind  string

	// Components is whose upgrade named this kind, which the scan copies onto
	// every finding rather than interpreting. It is what makes a finding
	// actionable: the object's identity says what might be lost, and this says
	// which upgrade would do it.
	Components []string
}

// AtRiskOptions is one advisory scan.
type AtRiskOptions struct {
	// Kubeconfig is the path to read the cluster from. Empty uses the ambient
	// resolution pkg/k8s/client applies.
	Kubeconfig string

	// Kinds is what to look for. An empty list contacts no cluster: an upgrade
	// whose crossed records name no resources has nothing to put at risk.
	Kinds []ResourceKind
}

// AtRiskResult is what the scan looked at and what it found.
type AtRiskResult struct {
	// Kinds is one entry per requested kind, in request order, whatever came
	// of it. It is what separates "checked and clean" from "the kind is not
	// installed here", which an empty Findings alone conflates.
	Kinds []ScannedKind

	// Findings is every object carrying no recognized ownership marker.
	Findings []AtRiskObject
}

// ScannedKind accounts for one kind the scan looked for.
type ScannedKind struct {
	Group      string
	Kind       string
	Components []string

	// Present reports the cluster serving this kind. Examined is then how many
	// objects of it were read, at risk or not; both are zero when it is not.
	Present  bool
	Examined int
}

// AtRiskObject is one object an upgrade could disturb and nothing appears to
// own. Namespace is empty for a cluster-scoped object.
type AtRiskObject struct {
	Group      string
	Kind       string
	Components []string
	Namespace  string
	Name       string
}

// ScanAtRisk reports the objects an upgrade could disturb that carry no
// deployer ownership marker, reading the cluster opts names.
//
// It is advisory: the caller reports what it returns and never fails a run on
// it, per ADR-021 Decision 3. AICR blocking an upgrade over resources it does
// not own is a claim it has not earned, and the same reasoning is what allows
// a kind the cluster does not serve to be skipped rather than raised.
func ScanAtRisk(ctx context.Context, opts AtRiskOptions) (AtRiskResult, error) {
	if len(opts.Kinds) == 0 {
		return AtRiskResult{}, nil
	}

	_, restConfig, err := k8sclient.GetKubeClientWithConfig(opts.Kubeconfig)
	if err != nil {
		return AtRiskResult{}, err
	}
	dyn, err := k8sclient.NewDynamicClientForConfig(restConfig)
	if err != nil {
		return AtRiskResult{}, err
	}
	mapper, err := k8sclient.NewRESTMapperForConfig(restConfig)
	if err != nil {
		return AtRiskResult{}, err
	}

	return scanAtRisk(ctx, dyn, mapper, opts.Kinds)
}

// scanAtRisk is ScanAtRisk with the client and mapper supplied.
func scanAtRisk(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper,
	kinds []ResourceKind) (AtRiskResult, error) {

	if len(kinds) == 0 {
		return AtRiskResult{}, nil
	}
	for _, kind := range kinds {
		if strings.TrimSpace(kind.Kind) == "" {
			return AtRiskResult{}, errors.New(errors.ErrCodeInvalidRequest,
				"the at-risk scan was asked for a resource with no kind, which names nothing to look for")
		}
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.AtRiskScanTimeout)
	defer cancel()

	out := AtRiskResult{Kinds: make([]ScannedKind, 0, len(kinds))}
	for _, kind := range kinds {
		// Per kind rather than per object, for the reason listOptions gives
		// the Helm reader: a page is bounded work over data already in hand,
		// and the page loop below checks for itself.
		if err := ctxErr(ctx, atRiskSubject); err != nil {
			return AtRiskResult{}, err
		}

		gvr, served, err := resolveKind(mapper, kind)
		if err != nil {
			return AtRiskResult{}, err
		}
		if !served {
			out.Kinds = append(out.Kinds, ScannedKind{
				Group: kind.Group, Kind: kind.Kind, Components: kind.Components,
			})

			continue
		}

		scanned, findings, err := scanKind(ctx, client, kind, gvr)
		if err != nil {
			return AtRiskResult{}, err
		}
		out.Kinds = append(out.Kinds, scanned)
		out.Findings = append(out.Findings, findings...)
	}

	return out, nil
}

// resolveKind maps a kind onto the resource the apiserver serves it as,
// reporting whether it serves it at all.
//
// A no-match is not an error: the CRD a transition record names may simply not
// be installed, which is the common case for a component an operator does not
// run. Every other discovery failure is, and the distinction is the point —
// reporting an unreachable apiserver as "the kind is absent" would turn an
// outage into an all-clear on exactly the objects this scan exists to warn
// about.
func resolveKind(mapper meta.RESTMapper, kind ResourceKind) (schema.GroupVersionResource, bool, error) {
	if mapper == nil {
		return schema.GroupVersionResource{}, false, errors.New(errors.ErrCodeInvalidRequest,
			"the at-risk scan was given no RESTMapper, so no kind can be resolved to a resource")
	}

	gk := schema.GroupKind{Group: kind.Group, Kind: kind.Kind}
	mapping, err := mapper.RESTMapping(gk)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return schema.GroupVersionResource{}, false, nil
		}

		return schema.GroupVersionResource{}, false, errors.WrapWithContext(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to resolve %s while scanning for resources an upgrade could disturb",
				kindDescription(kind)),
			err, map[string]any{ctxKeyResource: kindDescription(kind)})
	}

	return mapping.Resource, true, nil
}

// scanKind lists one kind cluster-wide and returns the objects nothing owns.
func scanKind(ctx context.Context, client dynamic.Interface, kind ResourceKind,
	gvr schema.GroupVersionResource) (ScannedKind, []AtRiskObject, error) {

	scanned := ScannedKind{Group: kind.Group, Kind: kind.Kind, Components: kind.Components, Present: true}
	var findings []AtRiskObject
	opts := metav1.ListOptions{Limit: defaults.AtRiskListPageSize}
	for {
		if err := ctxErr(ctx, atRiskSubject); err != nil {
			return ScannedKind{}, nil, err
		}
		page, err := client.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			// A CRD deleted between the mapping and this List answers the same
			// way one that was never installed does. Only the first request
			// can say that: a CRD does not disappear between pages, so the
			// same error later would silently drop everything already read.
			if opts.Continue == "" && (apierrors.IsNotFound(err) || meta.IsNoMatchError(err)) {
				return ScannedKind{Group: kind.Group, Kind: kind.Kind, Components: kind.Components}, nil, nil
			}
			// Before the generic wrap below, which would report an operator's
			// Ctrl-C as an unavailable apiserver. The per-page ctxErr catches
			// most aborts, but one landing mid-request surfaces here instead.
			if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
				return ScannedKind{}, nil, abortError(err, atRiskSubject,
					map[string]any{ctxKeyResource: gvr.Resource})
			}

			return ScannedKind{}, nil, errors.WrapWithContext(errors.ErrCodeUnavailable,
				fmt.Sprintf("failed to list %s while scanning for resources an upgrade could disturb",
					kindDescription(kind)),
				err, map[string]any{ctxKeyResource: gvr.Resource})
		}

		for i := range page.Items {
			item := &page.Items[i]
			scanned.Examined++
			if owned(item) {
				continue
			}
			if item.GetName() == "" {
				// A finding an operator cannot look up is worse than none:
				// this is advisory output, and an unactionable line in it
				// costs the actionable ones their credibility.
				continue
			}
			findings = append(findings, AtRiskObject{
				Group:      kind.Group,
				Kind:       kind.Kind,
				Components: kind.Components,
				Namespace:  item.GetNamespace(),
				Name:       item.GetName(),
			})
		}

		if page.GetContinue() == "" {
			break
		}
		if err := advance(&opts, page.GetContinue(), gvr.Resource); err != nil {
			return ScannedKind{}, nil, err
		}
	}

	// Sorted so the report does not depend on List order, which paging and
	// apiserver-side sharding both perturb.
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Namespace != findings[j].Namespace {
			return findings[i].Namespace < findings[j].Namespace
		}

		return findings[i].Name < findings[j].Name
	})

	return scanned, findings, nil
}

// owned reports an object carrying a deployer's ownership marker.
//
// The check is positive, so an object with no recognized marker is at risk by
// default. That is the correct direction here: the failure being guarded is an
// operator's own custom resources being cascade-deleted when a CRD is removed,
// so a marker this build does not recognize over-warns rather than
// under-warns, and the scan is advisory either way. The reverse — a denylist
// of markers meaning "not ours" — would let every unrecognized shape pass
// silently, which is the failure mode with no recovery.
//
// Helm's label alone is not ownership. Any controller, any hand-written
// manifest and any chart rendered by something other than Helm can carry
// app.kubernetes.io/managed-by, while meta.helm.sh/release-name is written by
// Helm's own install path and by nothing else.
func owned(item *unstructured.Unstructured) bool {
	annotations := item.GetAnnotations()
	if strings.TrimSpace(annotations[argoTrackingIDAnnotation]) != "" {
		return true
	}

	return item.GetLabels()[labels.ManagedBy] == helmManagedByValue &&
		strings.TrimSpace(annotations[helmReleaseNameAnnotation]) != ""
}

// kindDescription names a kind the way an operator writes it into a kubectl
// command or an RBAC rule.
func kindDescription(kind ResourceKind) string {
	if kind.Group == "" {
		return kind.Kind
	}

	return kind.Kind + "." + kind.Group
}
