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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// argoApplicationResource names the CRD in messages and in error context, in
// the form an operator writes it into an RBAC rule.
const argoApplicationResource = "applications.argoproj.io"

// argoSynced is status.sync.status for an Application whose live state
// matches the revision it last compared against.
const argoSynced = "Synced"

// argoApplicationGVR is read directly rather than through a RESTMapper: the
// group, version and resource are fixed by Argo's API and a mapper would add
// a discovery round trip to learn them.
var argoApplicationGVR = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "applications",
}

// argoChart is the chart identity one Argo source names. A source that names
// none, which is every git and path source, yields the zero value, so an
// empty name is how "this source is not the chart" is reported.
type argoChart struct {
	name    string
	version string
}

// argoApplications reads the components Argo CD deploys, as installedRelease
// values the Helm reader's results can be merged with.
//
// Argo renders a `helm:` source with `helm template` and applies the result,
// so it writes no Helm release record: under the argocd and argocd-helm
// deployers these Applications are the only evidence a component is installed
// at all, and helmReleases returns nothing.
//
// What Argo cannot answer is left zero rather than approximated. It keeps no
// counterpart to a chart's annotations and has no revision in Helm's sense.
// The chart version is read from status, never from spec: see
// deployedRevision.
//
// Only in-scope Applications are read, and scope is decided before an item is
// validated. A cluster runs Applications belonging to teams that have never
// heard of this project, and one of them omitting spec.destination.namespace
// is well-formed for its own purposes; failing the run on it would be this
// reader's bug. See the scope type. The count returned alongside is of items
// that named nothing at all, which are excluded but not hidden.
func argoApplications(ctx context.Context, client dynamic.Interface, within scope) (inventoryRead, error) {
	if err := within.validate(); err != nil {
		return inventoryRead{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.ArgoInventoryTimeout)
	defer cancel()

	var applications []installedRelease
	records, unattributed, unreadable, remote := 0, 0, 0, 0
	dests := &argoDestinations{client: client}
	opts := metav1.ListOptions{Limit: defaults.ArgoApplicationListPageSize}
	for {
		// Cancellation is checked per page rather than per item, for the
		// reason listOptions gives for the Helm reader: the page loop is what
		// can run long, and a page is bounded work over data already in hand.
		if err := ctxErr(ctx, argoSubject); err != nil {
			return inventoryRead{}, err
		}
		page, err := client.Resource(argoApplicationGVR).Namespace(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			// A cluster that deploys with Helm alone has no Argo CRDs, which
			// is an empty answer and not a failure. Only the first request can
			// say that: a CRD does not disappear between pages, so the same
			// error later means pages already read would be silently dropped.
			if opts.Continue == "" && (apierrors.IsNotFound(err) || meta.IsNoMatchError(err)) {
				return inventoryRead{}, nil
			}

			return inventoryRead{}, argoListError(err)
		}

		for i := range page.Items {
			item := &page.Items[i]
			records++
			// Before applicationFrom, so a foreign Application is never
			// validated: an item this read does not answer for cannot reach
			// the comparison, so it must not be able to fail it either.
			if item.GetName() == "" {
				unattributed++

				continue
			}
			conf := within.covers(item.GetName())
			if conf == outOfScope {
				continue
			}

			// Before the projection, so an Application deploying to another
			// cluster is counted and never read as this cluster's install.
			local, err := dests.local(ctx, item)
			if err != nil {
				if conf != confident {
					unreadable++
					applications = append(applications, unprovenApplication(item))

					continue
				}

				return inventoryRead{}, err
			}
			if !local {
				remote++

				continue
			}

			application, err := applicationFrom(item)
			if err != nil {
				// A loosely matched Application is as likely to be a foreign
				// workload sharing a token as a component's own, so it is
				// counted rather than allowed to fail the run. Only a name
				// this project itself would have written is worth failing on.
				// It is kept as unproven rather than dropped: if it does
				// belong to a component, dropping it reads that component as
				// newly installed.
				if conf != confident {
					unreadable++
					applications = append(applications, unprovenApplication(item))

					continue
				}

				return inventoryRead{}, err
			}
			applications = append(applications, application)
		}

		if page.GetContinue() == "" {
			break
		}
		if err := advance(&opts, page.GetContinue(), argoApplicationResource); err != nil {
			return inventoryRead{}, err
		}
	}

	// Sorted by name so a caller's output does not depend on List order, with
	// the namespace breaking ties: two Argo instances can each run an
	// Application of the same name against different destinations.
	//
	// Stable because the name and the destination namespace are not a unique
	// key here, unlike the Helm reader's, where they name one storage record.
	// Two Argo instances can run same-named Applications against the same
	// destination, and an unstable sort would order that pair differently from
	// run to run over identical cluster state.
	sort.SliceStable(applications, func(i, j int) bool {
		return compareInstallOrder(applications[i].Name, applications[i].Namespace,
			applications[j].Name, applications[j].Namespace) < 0
	})

	return inventoryRead{
		Releases:     applications,
		Records:      records,
		Unattributed: unattributed,
		Unreadable:   unreadable,
		Remote:       remote,
	}, nil
}

// unprovenApplication is an Application the read could not project. The
// destination namespace is taken if it reads as a string and left empty if
// not, since attribution then simply has less to go on.
func unprovenApplication(item *unstructured.Unstructured) installedRelease {
	namespace, _, _ := unstructured.NestedString(item.Object, "spec", "destination", "namespace")

	return installedRelease{Source: sourceArgo, Name: item.GetName(), Namespace: namespace, Unproven: true}
}

// applicationFrom projects one Application onto the inventory's shape.
//
// Every failure is fatal rather than skipped, for the reason helmReleases
// gives: a component missing from the inventory reads as one being installed
// for the first time, and that is the verdict this command exists to get right.
func applicationFrom(item *unstructured.Unstructured) (installedRelease, error) {
	// The workload namespace is the destination's, never the Application's
	// own: every generated Application is written into argocd whatever it
	// deploys, and the namespace is half of how a release is identified.
	namespace, _, err := unstructured.NestedString(item.Object, "spec", "destination", "namespace")
	if err != nil {
		return installedRelease{}, fieldError(item, err, "spec.destination.namespace", "a string")
	}
	if namespace == "" {
		return installedRelease{}, errors.NewWithContext(errors.ErrCodeInternal,
			fmt.Sprintf("the Argo CD Application %q in namespace %q sets no spec.destination.namespace, so the "+
				"namespace it installs into cannot be determined", item.GetName(), item.GetNamespace()),
			applicationContext(item))
	}

	chart, err := applicationChart(item)
	if err != nil {
		return installedRelease{}, err
	}
	version := ""
	if chart.name != "" {
		if version, err = deployedRevision(item, chart.name); err != nil {
			return installedRelease{}, err
		}
	}

	return installedRelease{
		Source:       sourceArgo,
		Name:         item.GetName(),
		Namespace:    namespace,
		ChartName:    chart.name,
		ChartVersion: version,
	}, nil
}

// deployedRevision is the version of the named chart the cluster is running,
// or empty when status establishes none.
//
// spec.source(s).targetRevision is the pin Argo has been asked to reach, not
// the one it reached: a pin changed while its sync is pending or has failed
// leaves the old chart deployed. Reporting the pin would read an upgrade not
// yet made as already made, and hide that transition's steps. So only status
// is evidence, in this order:
//
//  1. status.sync, when Synced: the live state matches the rendering of the
//     sources it last compared, which is what is deployed. Those are the
//     comparison's own copy and not the spec's, so a pin edited since the
//     last refresh is not read as applied.
//  2. Otherwise the newest status.history entry, whose sources Argo records
//     only when a sync operation completes.
//
// Neither is waited for. A Synced Application whose operation never finishes,
// as a health-gated one does on a cluster that reports no pod readiness,
// still answers from the first; one with neither answers empty, which reads
// as unversioned rather than as a version it may not be running.
//
// The version is the targetRevision of the chart's source in that copy, not
// the revision Argo recorded beside it: for an OCI chart that revision is the
// manifest digest, which names the same chart but is not a version.
func deployedRevision(item *unstructured.Unstructured, chartName string) (string, error) {
	status, _, err := unstructured.NestedString(item.Object, "status", "sync", "status")
	if err != nil {
		return "", fieldError(item, err, "status.sync.status", "a string")
	}
	if status == argoSynced {
		compared, _, syncErr := unstructured.NestedMap(item.Object, "status", "sync", "comparedTo")
		if syncErr != nil {
			return "", fieldError(item, syncErr, "status.sync.comparedTo", "an object")
		}
		version, syncErr := chartVersionIn(item, chartName, compared, "status.sync.comparedTo")
		if syncErr != nil || version != "" {
			return version, syncErr
		}
	}

	history, _, err := unstructured.NestedSlice(item.Object, "status", "history")
	if err != nil {
		return "", fieldError(item, err, "status.history", "a list")
	}
	if len(history) == 0 {
		return "", nil
	}
	// Argo appends and trims from the front, so the last entry is the newest.
	field := fmt.Sprintf("status.history[%d]", len(history)-1)
	entry, ok := history[len(history)-1].(map[string]any)
	if !ok {
		return "", fieldError(item, nil, field, "an object")
	}

	return chartVersionIn(item, chartName, entry, field)
}

// chartVersionIn is the version of the named chart among the source or
// sources holder records. The chart is found by name rather than position,
// and a holder naming only other charts answers empty: that is not evidence
// about the chart the spec names.
func chartVersionIn(item *unstructured.Unstructured, chartName string,
	holder map[string]any, field string) (string, error) {

	source, _, err := unstructured.NestedMap(holder, "source")
	if err != nil {
		return "", fieldError(item, err, field+".source", "an object")
	}
	chart, err := chartFrom(item, source, field+".source")
	if err != nil || chart.name == chartName {
		return chart.version, err
	}

	sources, _, err := unstructured.NestedSlice(holder, "sources")
	if err != nil {
		return "", fieldError(item, err, field+".sources", "a list")
	}
	for i, entry := range sources {
		sourceField := fmt.Sprintf("%s.sources[%d]", field, i)
		source, ok := entry.(map[string]any)
		if !ok {
			return "", fieldError(item, nil, sourceField, "an object")
		}
		chart, err := chartFrom(item, source, sourceField)
		if err != nil || chart.name == chartName {
			return chart.version, err
		}
	}

	return "", nil
}

// applicationChart finds the source that names a Helm chart, the only source
// whose revision is a component version. The others carry a git revision: the
// bundle repository's branch or tag, which would read as every component
// sitting at "main". Its version is the pin, which deployedRevision does not
// trust.
//
// Selection is by which source carries a chart, never by position. Argo puts
// no ordering requirement on spec.sources, and the generated multi-source
// Application happens to list the chart first only by convention.
//
// A path-based Application matches no source and yields the zero value. That
// covers the generated wrappers and Kustomize components, whose payload
// version is not in the cluster at all.
func applicationChart(item *unstructured.Unstructured) (argoChart, error) {
	source, _, err := unstructured.NestedMap(item.Object, "spec", "source")
	if err != nil {
		return argoChart{}, fieldError(item, err, "spec.source", "an object")
	}
	chart, err := chartFrom(item, source, "spec.source")
	if err != nil || chart.name != "" {
		return chart, err
	}

	sources, _, err := unstructured.NestedSlice(item.Object, "spec", "sources")
	if err != nil {
		return argoChart{}, fieldError(item, err, "spec.sources", "a list")
	}
	for i, entry := range sources {
		field := fmt.Sprintf("spec.sources[%d]", i)
		source, ok := entry.(map[string]any)
		if !ok {
			return argoChart{}, fieldError(item, nil, field, "an object")
		}
		chart, err := chartFrom(item, source, field)
		if err != nil || chart.name != "" {
			return chart, err
		}
	}

	return argoChart{}, nil
}

// chartFrom reads one source's chart identity.
func chartFrom(item *unstructured.Unstructured, source map[string]any, field string) (argoChart, error) {
	name, _, err := unstructured.NestedString(source, "chart")
	if err != nil {
		return argoChart{}, fieldError(item, err, field+".chart", "a string")
	}
	if name == "" {
		return argoChart{}, nil
	}

	version, _, err := unstructured.NestedString(source, "targetRevision")
	if err != nil {
		return argoChart{}, fieldError(item, err, field+".targetRevision", "a string")
	}

	return argoChart{name: name, version: version}, nil
}

// fieldError reports an Application field of a type this cannot read. The
// cause is nil where the type was rejected without a helper reporting it.
func fieldError(item *unstructured.Unstructured, cause error, field, want string) error {
	message := fmt.Sprintf("the Argo CD Application %q in namespace %q carries a %s that is not %s",
		item.GetName(), item.GetNamespace(), field, want)
	if cause == nil {
		return errors.NewWithContext(errors.ErrCodeInternal, message, applicationContext(item))
	}

	return errors.WrapWithContext(errors.ErrCodeInternal, message, cause, applicationContext(item))
}

// applicationContext is the error context for one Application, whose namespace
// is where the object itself lives rather than where it deploys.
func applicationContext(item *unstructured.Unstructured) map[string]any {
	return map[string]any{
		ctxKeyObject:    item.GetName(),
		ctxKeyNamespace: item.GetNamespace(),
		ctxKeyResource:  argoApplicationResource,
	}
}

// argoListError classifies a failed List, on the terms listError sets for the
// Helm reader's own Lists.
func argoListError(err error) error {
	errCtx := map[string]any{ctxKeyResource: argoApplicationResource}

	if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
		return abortError(err, argoSubject, errCtx)
	}

	if apierrors.IsResourceExpired(err) {
		return errors.WrapWithContext(errors.ErrCodeUnavailable,
			fmt.Sprintf("the paged list of %s outlived the apiserver's window for it, so the Argo CD "+
				"Applications were read only in part; re-run", argoApplicationResource),
			err, errCtx)
	}

	if apierrors.IsForbidden(err) {
		return errors.WrapWithContext(errors.ErrCodeUnauthorized,
			fmt.Sprintf("cannot list %s across all namespaces, so the components Argo CD deploys cannot be read; "+
				"grant 'list %s' at cluster scope and re-run", argoApplicationResource, argoApplicationResource),
			err, errCtx)
	}

	return errors.WrapWithContext(errors.ErrCodeInternal,
		"failed to list Argo CD Applications", err, errCtx)
}
