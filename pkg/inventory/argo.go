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
// counterpart to a chart's annotations, has no revision in Helm's sense, and
// its status describes sync and health, which is a different question from
// which version is installed.
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
	records, unattributed, unreadable := 0, 0, 0
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

			application, err := applicationFrom(item)
			if err != nil {
				// A loosely matched Application is as likely to be a foreign
				// workload sharing a token as a component's own, so it is
				// counted rather than allowed to fail the run. Only a name
				// this project itself would have written is worth failing on.
				if conf != confident {
					unreadable++

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
	}, nil
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

	return installedRelease{
		Source:       sourceArgo,
		Name:         item.GetName(),
		Namespace:    namespace,
		ChartName:    chart.name,
		ChartVersion: chart.version,
	}, nil
}

// applicationChart finds the source that names a Helm chart, which is the only
// source whose targetRevision is a component version. The others carry a git
// revision: the bundle repository's branch or tag, which would read as every
// component sitting at "main".
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
