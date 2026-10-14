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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
)

// Ownership markers recognized by owned.
const (
	helmManagedByValue        = "Helm"
	helmReleaseNameAnnotation = "meta.helm.sh/release-name"
	helmHookAnnotation        = "helm.sh/hook"
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
	// Before any client is built, so a caller who asked for something
	// malformed is told that rather than told their kubeconfig is broken.
	if err := validateKinds(opts.Kinds); err != nil {
		return AtRiskResult{}, err
	}

	_, restConfig, err := k8sclient.GetKubeClientWithConfig(opts.Kubeconfig)
	if err != nil {
		return AtRiskResult{}, err
	}
	metaClient, err := k8sclient.NewMetadataClientForConfig(restConfig)
	if err != nil {
		return AtRiskResult{}, err
	}
	mapper, groups, err := k8sclient.NewRESTMapperAndDiscovery(restConfig)
	if err != nil {
		return AtRiskResult{}, err
	}

	return scanAtRisk(ctx, metaClient, mapper, groups, opts.Kinds)
}

// scanAtRisk is ScanAtRisk with the client, the mapper and the discovery the
// mapper resolves through supplied.
func scanAtRisk(ctx context.Context, client metadata.Interface, mapper meta.RESTMapper,
	groups k8sclient.GroupResourceLister, kinds []ResourceKind) (AtRiskResult, error) {

	if len(kinds) == 0 {
		return AtRiskResult{}, nil
	}
	if err := validateKinds(kinds); err != nil {
		return AtRiskResult{}, err
	}

	out := AtRiskResult{Kinds: make([]ScannedKind, 0, len(kinds))}
	for _, kind := range kinds {
		// Per kind rather than per object, for the reason listOptions gives
		// the Helm reader: a page is bounded work over data already in hand,
		// and the page loop below checks for itself.
		if err := ctxErr(ctx, atRiskSubject); err != nil {
			return AtRiskResult{}, err
		}

		stepCtx, cancel := context.WithTimeout(ctx, defaults.AtRiskStepTimeout)
		gvr, served, err := resolveKind(stepCtx, mapper, groups, kind)
		cancel()
		if err != nil {
			return AtRiskResult{}, err
		}
		if !served {
			out.Kinds = append(out.Kinds, ScannedKind{
				Group: kind.Group, Kind: kind.Kind, Components: kind.Components,
			})

			continue
		}

		scanned, findings, err := scanKind(ctx, client, kind, gvr, defaults.AtRiskStepTimeout)
		if err != nil {
			return AtRiskResult{}, err
		}
		out.Kinds = append(out.Kinds, scanned)
		out.Findings = append(out.Findings, findings...)
	}

	return out, nil
}

// validateKinds rejects a request naming a resource the scan cannot look for.
// Both entry points call it, so scanAtRisk stays self-checking for the callers
// that supply their own client.
func validateKinds(kinds []ResourceKind) error {
	for _, kind := range kinds {
		if strings.TrimSpace(kind.Kind) == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				"the at-risk scan was asked for a resource with no kind, which names nothing to look for")
		}
	}

	return nil
}

// resolveKind maps a kind onto the resource the apiserver serves it as,
// reporting whether it serves it at all.
//
// A no-match is not an error when it is established: the CRD a transition
// record names may simply not be installed, which is the common case for a
// component an operator does not run. Establishing it takes two things, since
// client-go discards a partial discovery failure whenever other groups
// answered and reports the kind as a bare no-match: the error must not wrap a
// group failure, and the kind's own group must have been enumerated in full.
// Anything short of that, and every other discovery failure, is an error: an
// unreachable group read as "not installed" is an all-clear on exactly the
// objects this scan exists to warn about.
func resolveKind(ctx context.Context, mapper meta.RESTMapper, groups k8sclient.GroupResourceLister,
	kind ResourceKind) (schema.GroupVersionResource, bool, error) {

	if mapper == nil {
		return schema.GroupVersionResource{}, false, errors.New(errors.ErrCodeInvalidRequest,
			"the at-risk scan was given no RESTMapper, so no kind can be resolved to a resource")
	}

	errCtx := map[string]any{ctxKeyResource: kindDescription(kind)}
	gk := schema.GroupKind{Group: kind.Group, Kind: kind.Kind}
	mapping, err := mapper.RESTMapping(gk)
	if err == nil {
		return mapping.Resource, true, nil
	}
	if !k8sclient.IsGenuineNoMatch(err) {
		return schema.GroupVersionResource{}, false, errors.WrapWithContext(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to resolve %s while scanning for resources an upgrade could disturb",
				kindDescription(kind)),
			err, errCtx)
	}
	if groups == nil {
		return schema.GroupVersionResource{}, false, errors.NewWithContext(errors.ErrCodeUnavailable,
			fmt.Sprintf("the cluster reported no %s, but the scan has no discovery to confirm API group %q "+
				"was enumerated, so it cannot say the kind is absent", kindDescription(kind), kind.Group),
			errCtx)
	}
	if groupErr := k8sclient.GroupDiscoveryFailure(ctx, groups, kind.Group); groupErr != nil {
		return schema.GroupVersionResource{}, false, errors.WrapWithContext(errors.ErrCodeUnavailable,
			fmt.Sprintf("the cluster reported no %s, but discovery for API group %q is incomplete, so the "+
				"kind cannot be said to be absent", kindDescription(kind), kind.Group),
			groupErr, errCtx)
	}

	return schema.GroupVersionResource{}, false, nil
}

// scanKind lists kind cluster-wide and returns the objects nothing owns. Each
// page, retries included, must arrive within pageTimeout.
func scanKind(ctx context.Context, client metadata.Interface, kind ResourceKind,
	gvr schema.GroupVersionResource, pageTimeout time.Duration) (ScannedKind, []AtRiskObject, error) {

	scanned := ScannedKind{Group: kind.Group, Kind: kind.Kind, Components: kind.Components, Present: true}
	var findings []AtRiskObject
	opts := metav1.ListOptions{Limit: defaults.AtRiskListPageSize}
	for {
		if err := ctxErr(ctx, atRiskSubject); err != nil {
			return ScannedKind{}, nil, err
		}
		// Metadata only, though kubectl's last-applied-configuration
		// annotation can still hold a whole object.
		pageCtx, cancel := context.WithTimeout(ctx, pageTimeout)
		page, err := client.Resource(gvr).Namespace(metav1.NamespaceAll).List(pageCtx, opts)
		cancel()
		if err != nil {
			// For an advisory scan an inconsistent list beats a failed one.
			if fresh := expiredContinue(err); opts.Continue != "" && fresh != "" {
				if advErr := advance(&opts, fresh, gvr.Resource); advErr != nil {
					return ScannedKind{}, nil, advErr
				}

				continue
			}
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
				// An unnamed object cannot be looked up.
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

// expiredContinue returns the token that err, a 410 for an expired continue
// token, carries to resume the list at the latest snapshot, or "" if err is
// not such a 410 or carries none.
func expiredContinue(err error) string {
	var status apierrors.APIStatus
	if !apierrors.IsResourceExpired(err) || !stderrors.As(err, &status) {
		return ""
	}

	return status.Status().Continue
}

// owned reports whether item carries a deployer ownership marker. The markers
// are:
//   - a non-blank argocd.argoproj.io/tracking-id annotation
//   - the app.kubernetes.io/managed-by=Helm label with a non-blank
//     meta.helm.sh/release-name annotation
//   - the app.kubernetes.io/managed-by=Helm label with a non-blank
//     helm.sh/hook annotation
func owned(item metav1.Object) bool {
	// An allowlist, since over-warning costs a line of output and
	// under-warning costs an object a CRD removal deletes for good.
	annotations := item.GetAnnotations()
	if strings.TrimSpace(annotations[argoTrackingIDAnnotation]) != "" {
		return true
	}
	// Anything can write the label, but only Helm writes the release name,
	// or helm.sh/hook on a hook, which gets no release name.
	if item.GetLabels()[labels.ManagedBy] != helmManagedByValue {
		return false
	}

	return strings.TrimSpace(annotations[helmReleaseNameAnnotation]) != "" ||
		strings.TrimSpace(annotations[helmHookAnnotation]) != ""
}

// kindDescription names a kind the way an operator writes it into a kubectl
// command or an RBAC rule.
func kindDescription(kind ResourceKind) string {
	if kind.Group == "" {
		return kind.Kind
	}

	return kind.Kind + "." + kind.Group
}
