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
	"log/slog"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	// SubtypeSlinkySlurm is the K8s measurement subtype containing the
	// intentionally lossy Slinky Slurm installation summary.
	SubtypeSlinkySlurm = "slinky-slurm"

	slinkyAPIGroup           = "slinky.slurm.net"
	slinkyControllerResource = "controllers"
	slinkyControllerKind     = "Controller"
	slinkyNodeSetResource    = "nodesets"
	slinkyNodeSetKind        = "NodeSet"
	slinkyLoginSetResource   = "loginsets"
	slinkyLoginSetKind       = "LoginSet"
	slinkyRestAPIResource    = "restapis"
	slinkyRestAPIKind        = "RestApi"
	slinkyAccountingResource = "accountings"
	slinkyAccountingKind     = "Accounting"

	slinkyKeyAPIAvailable    = "api-available"
	slinkyKeyAPIVersion      = "api-version"
	slinkyKeyCollectionState = "collection-state"
	slinkyKeyControllerCount = "controller-count"
	slinkyKeyDetected        = "detected"
	slinkyKeyNodeSetCount    = "nodeset-count"
	slinkyKeyLoginSetCount   = "loginset-count"
	slinkyKeyRestAPICount    = "restapi-count"
	slinkyKeyAccountingCount = "accounting-count"

	slinkyStateAbsent                  = "absent"
	slinkyStateDetected                = "detected"
	slinkyStateUnknown                 = "unknown"
	slinkyStateUnsupportedMulticluster = "unsupported-multicluster"

	slinkyContextID           = "id"
	slinkyContextKind         = "kind"
	slinkyContextNamespace    = "namespace"
	slinkyContextName         = "name"
	slinkyContextAPIVersion   = "api-version"
	slinkyContextControllerID = "controller-id"

	slinkyItemClusterName          = "cluster-name"
	slinkyItemExternal             = "external"
	slinkyItemAccountingRefPresent = "accounting-ref-present"
	slinkyItemPartitionEnabled     = "partition-enabled"

	slinkyFieldSpec          = "spec"
	slinkyFieldClusterName   = "clusterName"
	slinkyFieldExternal      = "external"
	slinkyFieldAccountingRef = "accountingRef"
	slinkyFieldControllerRef = "controllerRef"
	slinkyFieldName          = "name"
	slinkyFieldPartition     = "partition"
	slinkyFieldEnabled       = "enabled"
)

// apiResourceDiscovery is the narrow discovery seam shared by custom-resource
// sub-collectors. Production uses ClientSet.Discovery(); tests inject stubs.
type apiResourceDiscovery interface {
	ServerGroups() (*metav1.APIGroupList, error)
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// Keep the existing name for the Collector test seam and downstream package
// tests while the implementation is shared with MariaDB discovery.
type slinkyDiscovery = apiResourceDiscovery

// slinkySummary is reduced to scalar measurement data. Pointer fields preserve
// the distinction between a conclusive false/zero and an unknown value.
type slinkySummary struct {
	state             string
	apiAvailable      *bool
	apiVersion        string
	detected          *bool
	controllerCount   *int
	childrenCollected bool
	items             []measurement.ItemEntry
}

func (s slinkySummary) subtype() measurement.Subtype {
	data := map[string]measurement.Reading{
		slinkyKeyCollectionState: measurement.Str(s.state),
	}
	if s.apiAvailable != nil {
		data[slinkyKeyAPIAvailable] = measurement.Bool(*s.apiAvailable)
	}
	if s.apiVersion != "" {
		data[slinkyKeyAPIVersion] = measurement.Str(s.apiVersion)
	}
	if s.detected != nil {
		data[slinkyKeyDetected] = measurement.Bool(*s.detected)
	}
	if s.controllerCount != nil {
		data[slinkyKeyControllerCount] = measurement.Int(*s.controllerCount)
	}
	if s.childrenCollected {
		counts := countSlinkyItems(s.items)
		data[slinkyKeyNodeSetCount] = measurement.Int(counts[slinkyNodeSetKind])
		data[slinkyKeyLoginSetCount] = measurement.Int(counts[slinkyLoginSetKind])
		data[slinkyKeyRestAPICount] = measurement.Int(counts[slinkyRestAPIKind])
		data[slinkyKeyAccountingCount] = measurement.Int(counts[slinkyAccountingKind])
	}
	return measurement.Subtype{Name: SubtypeSlinkySlurm, Data: data, Items: s.items}
}

func valuePtr[T any](value T) *T {
	return &value
}

func unknownSlinkySubtype() measurement.Subtype {
	return slinkySummary{state: slinkyStateUnknown}.subtype()
}

// collectSlinkySlurm returns installation state as data and never as a Go
// error. This is deliberate: the generic K8s collectSafe helper would erase
// deterministic failures or drop the whole K8s measurement for transient
// failures, making "absent" indistinguishable from "could not inspect".
func (k *Collector) collectSlinkySlurm(
	ctx context.Context,
	defaultDiscovery apiResourceDiscovery,
) measurement.Subtype {

	if err := ctx.Err(); err != nil {
		slog.Warn("Slinky Slurm discovery cancelled", slog.String("error", err.Error()))
		return unknownSlinkySubtype()
	}

	discoveryClient := k.slinkyDiscovery
	if discoveryClient == nil {
		discoveryClient = defaultDiscovery
	}
	if discoveryClient == nil {
		slog.Warn("Slinky Slurm discovery client unavailable")
		return unknownSlinkySubtype()
	}

	groups, err := discoveryClient.ServerGroups()
	if err != nil {
		slog.Warn("failed to discover Kubernetes API groups for Slinky Slurm",
			slog.String("error", err.Error()))
		return unknownSlinkySubtype()
	}
	if groups == nil {
		slog.Warn("Kubernetes API group discovery returned no response for Slinky Slurm")
		return unknownSlinkySubtype()
	}

	group := findAPIGroup(groups, slinkyAPIGroup)
	if group == nil {
		return slinkySummary{
			state:        slinkyStateAbsent,
			apiAvailable: valuePtr(false),
			detected:     valuePtr(false),
		}.subtype()
	}

	gvr, ambiguous := discoverSlinkyControllerGVR(ctx, discoveryClient, group)
	if gvr == nil {
		if ambiguous {
			return unknownSlinkySubtype()
		}
		return slinkySummary{
			state:        slinkyStateAbsent,
			apiAvailable: valuePtr(false),
			detected:     valuePtr(false),
		}.subtype()
	}

	apiVersion := gvr.Version
	apiAvailable := valuePtr(true)
	dynamicClient, err := k.getDynamicClient()
	if err != nil {
		slog.Warn("failed to initialize dynamic client for Slinky Slurm",
			slog.String("apiVersion", apiVersion),
			slog.String("error", err.Error()))
		return slinkySummary{
			state:        slinkyStateUnknown,
			apiAvailable: apiAvailable,
			apiVersion:   apiVersion,
		}.subtype()
	}

	controllers, err := dynamicClient.Resource(*gvr).
		Namespace(metav1.NamespaceAll).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		// NotFound after successful discovery is an inconsistent discovery/List
		// race. Treat every List failure as unknown rather than falsely absent.
		slog.Warn("failed to list Slinky Slurm Controllers",
			slog.String("apiVersion", apiVersion),
			slog.String("error", err.Error()))
		return slinkySummary{
			state:        slinkyStateUnknown,
			apiAvailable: apiAvailable,
			apiVersion:   apiVersion,
		}.subtype()
	}
	if controllers == nil {
		slog.Warn("Slinky Slurm Controller list returned no response",
			slog.String("apiVersion", apiVersion))
		return slinkySummary{
			state:        slinkyStateUnknown,
			apiAvailable: apiAvailable,
			apiVersion:   apiVersion,
		}.subtype()
	}

	count := len(controllers.Items)
	summary := slinkySummary{
		apiAvailable:    apiAvailable,
		apiVersion:      apiVersion,
		controllerCount: valuePtr(count),
	}
	switch count {
	case 0:
		summary.state = slinkyStateAbsent
		summary.detected = valuePtr(false)
	case 1:
		summary.state = slinkyStateDetected
		summary.detected = valuePtr(true)
		summary.items, summary.childrenCollected = collectSlinkyProjection(
			ctx,
			discoveryClient,
			group,
			dynamicClient,
			&controllers.Items[0],
			apiVersion,
		)
	default:
		summary.state = slinkyStateUnsupportedMulticluster
		summary.detected = valuePtr(true)
	}
	return summary.subtype()
}

type slinkyResourceDescriptor struct {
	resource string
	kind     string
}

type slinkyResourceList struct {
	descriptor slinkyResourceDescriptor
	apiVersion string
	items      []unstructured.Unstructured
	complete   bool
}

func collectSlinkyProjection(
	ctx context.Context,
	discoveryClient apiResourceDiscovery,
	group *metav1.APIGroup,
	dynamicClient dynamic.Interface,
	controller *unstructured.Unstructured,
	controllerAPIVersion string,
) ([]measurement.ItemEntry, bool) {

	if controller == nil || strings.TrimSpace(controller.GetName()) == "" ||
		strings.TrimSpace(controller.GetNamespace()) == "" {

		return nil, false
	}

	complete := true
	controllerID := slinkyItemID(slinkyControllerKind, controller.GetNamespace(), controller.GetName())
	controllerItem, accountingName, itemComplete := projectSlinkyController(
		controller,
		controllerAPIVersion,
	)
	if !itemComplete {
		complete = false
	}
	items := []measurement.ItemEntry{controllerItem}

	descriptors := []slinkyResourceDescriptor{
		{resource: slinkyNodeSetResource, kind: slinkyNodeSetKind},
		{resource: slinkyLoginSetResource, kind: slinkyLoginSetKind},
		{resource: slinkyRestAPIResource, kind: slinkyRestAPIKind},
	}
	if accountingName != "" {
		descriptors = append(descriptors, slinkyResourceDescriptor{
			resource: slinkyAccountingResource,
			kind:     slinkyAccountingKind,
		})
	}

	results := make([]slinkyResourceList, len(descriptors))
	g, gctx := errgroup.WithContext(ctx)
	for i := range descriptors {
		g.Go(func() error {
			results[i] = collectSlinkyResourceList(
				gctx,
				discoveryClient,
				group,
				dynamicClient,
				descriptors[i],
			)
			if err := gctx.Err(); err != nil {
				return errors.Wrap(errors.ErrCodeTimeout, "Slinky projection cancelled", err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		complete = false
	}

	for i := range results {
		if ctx.Err() != nil {
			complete = false
			break
		}
		result := &results[i]
		if !result.complete {
			complete = false
			continue
		}

		switch result.descriptor.kind {
		case slinkyAccountingKind:
			accountingItems, selected, selectionComplete := projectReferencedAccounting(
				ctx,
				result,
				controller.GetNamespace(),
				accountingName,
				controllerID,
			)
			items = append(items, accountingItems...)
			if !selected || !selectionComplete {
				complete = false
			}
		default:
			childItems, selectionComplete := projectSlinkyChildren(
				ctx,
				result,
				controller.GetNamespace(),
				controller.GetName(),
				controllerID,
			)
			items = append(items, childItems...)
			if !selectionComplete {
				complete = false
			}
		}
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].Context[slinkyContextID] < items[j].Context[slinkyContextID]
	})
	if !complete {
		return []measurement.ItemEntry{controllerItem}, false
	}
	return items, true
}

func collectSlinkyResourceList(
	ctx context.Context,
	discoveryClient apiResourceDiscovery,
	group *metav1.APIGroup,
	dynamicClient dynamic.Interface,
	descriptor slinkyResourceDescriptor,
) slinkyResourceList {

	result := slinkyResourceList{descriptor: descriptor}
	gvr, ambiguous := discoverAPIResourceGVR(
		ctx,
		discoveryClient,
		group,
		slinkyAPIGroup,
		descriptor.resource,
		descriptor.kind,
	)
	if gvr == nil {
		slog.Warn("failed to discover Slinky Slurm resource for projection",
			slog.String("resource", descriptor.resource),
			slog.Bool("ambiguous", ambiguous))
		return result
	}

	list, err := dynamicClient.Resource(*gvr).
		Namespace(metav1.NamespaceAll).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Warn("failed to list Slinky Slurm resource for projection",
			slog.String("resource", descriptor.resource),
			slog.String("apiVersion", gvr.Version),
			slog.String("error", err.Error()))
		return result
	}
	if list == nil {
		slog.Warn("Slinky Slurm resource list returned no response",
			slog.String("resource", descriptor.resource),
			slog.String("apiVersion", gvr.Version))
		return result
	}

	result.apiVersion = gvr.Version
	result.items = list.Items
	result.complete = true
	return result
}

func projectSlinkyController(
	controller *unstructured.Unstructured,
	apiVersion string,
) (measurement.ItemEntry, string, bool) {

	data := make(map[string]measurement.Reading)
	complete := true

	clusterName, found, err := unstructured.NestedString(
		controller.Object,
		slinkyFieldSpec,
		slinkyFieldClusterName,
	)
	if err != nil {
		complete = false
	} else if found && strings.TrimSpace(clusterName) != "" {
		data[slinkyItemClusterName] = measurement.Str(clusterName)
	}

	external, found, err := unstructured.NestedBool(
		controller.Object,
		slinkyFieldSpec,
		slinkyFieldExternal,
	)
	if err != nil {
		complete = false
	} else if found {
		data[slinkyItemExternal] = measurement.Bool(external)
	}

	accountingName := ""
	accountingRef, found, err := unstructured.NestedMap(
		controller.Object,
		slinkyFieldSpec,
		slinkyFieldAccountingRef,
	)
	switch {
	case err != nil:
		complete = false
	case !found:
		data[slinkyItemAccountingRefPresent] = measurement.Bool(false)
	default:
		name, ok := accountingRef[slinkyFieldName].(string)
		if !ok || strings.TrimSpace(name) == "" {
			complete = false
			break
		}
		accountingName = name
		data[slinkyItemAccountingRefPresent] = measurement.Bool(true)
	}

	return newSlinkyItem(
		slinkyControllerKind,
		controller.GetNamespace(),
		controller.GetName(),
		apiVersion,
		"",
		data,
	), accountingName, complete
}

func projectSlinkyChildren(
	ctx context.Context,
	result *slinkyResourceList,
	controllerNamespace string,
	controllerName string,
	controllerID string,
) ([]measurement.ItemEntry, bool) {

	items := make([]measurement.ItemEntry, 0, len(result.items))
	complete := true
	for i := range result.items {
		if ctx.Err() != nil {
			return items, false
		}
		resource := &result.items[i]
		if strings.TrimSpace(resource.GetName()) == "" ||
			strings.TrimSpace(resource.GetNamespace()) == "" {

			complete = false
			continue
		}

		refName, found, err := unstructured.NestedString(
			resource.Object,
			slinkyFieldSpec,
			slinkyFieldControllerRef,
			slinkyFieldName,
		)
		if err != nil || !found || strings.TrimSpace(refName) == "" {
			complete = false
			continue
		}
		if resource.GetNamespace() != controllerNamespace || refName != controllerName {
			// The projection is cluster-wide. A foreign or dangling reference
			// means the observed topology cannot be claimed complete, even
			// though the resource is intentionally excluded from this Controller.
			complete = false
			continue
		}

		data := make(map[string]measurement.Reading)
		if result.descriptor.kind == slinkyNodeSetKind {
			enabled, found, err := unstructured.NestedBool(
				resource.Object,
				slinkyFieldSpec,
				slinkyFieldPartition,
				slinkyFieldEnabled,
			)
			if err != nil {
				complete = false
			} else if found {
				data[slinkyItemPartitionEnabled] = measurement.Bool(enabled)
			}
		}

		items = append(items, newSlinkyItem(
			result.descriptor.kind,
			resource.GetNamespace(),
			resource.GetName(),
			result.apiVersion,
			controllerID,
			data,
		))
	}
	return items, complete
}

func projectReferencedAccounting(
	ctx context.Context,
	result *slinkyResourceList,
	namespace string,
	name string,
	controllerID string,
) ([]measurement.ItemEntry, bool, bool) {

	complete := true
	for i := range result.items {
		if ctx.Err() != nil {
			return nil, false, false
		}
		resource := &result.items[i]
		if resource.GetNamespace() != namespace || resource.GetName() != name {
			continue
		}

		data := make(map[string]measurement.Reading)
		external, found, err := unstructured.NestedBool(
			resource.Object,
			slinkyFieldSpec,
			slinkyFieldExternal,
		)
		if err != nil {
			complete = false
		} else if found {
			data[slinkyItemExternal] = measurement.Bool(external)
		}
		return []measurement.ItemEntry{newSlinkyItem(
			slinkyAccountingKind,
			namespace,
			name,
			result.apiVersion,
			controllerID,
			data,
		)}, true, complete
	}
	return nil, false, complete
}

func newSlinkyItem(
	kind string,
	namespace string,
	name string,
	apiVersion string,
	controllerID string,
	data map[string]measurement.Reading,
) measurement.ItemEntry {

	itemContext := map[string]string{
		slinkyContextID:         slinkyItemID(kind, namespace, name),
		slinkyContextKind:       kind,
		slinkyContextNamespace:  namespace,
		slinkyContextName:       name,
		slinkyContextAPIVersion: apiVersion,
	}
	if controllerID != "" {
		itemContext[slinkyContextControllerID] = controllerID
	}
	return measurement.ItemEntry{Context: itemContext, Data: data}
}

func slinkyItemID(kind string, namespace string, name string) string {
	return strings.ToLower(kind) + "/" + namespace + "/" + name
}

func countSlinkyItems(items []measurement.ItemEntry) map[string]int {
	counts := map[string]int{
		slinkyNodeSetKind:    0,
		slinkyLoginSetKind:   0,
		slinkyRestAPIKind:    0,
		slinkyAccountingKind: 0,
	}
	for i := range items {
		counts[items[i].Context[slinkyContextKind]]++
	}
	return counts
}

func findAPIGroup(groups *metav1.APIGroupList, name string) *metav1.APIGroup {
	if groups == nil {
		return nil
	}
	for i := range groups.Groups {
		if groups.Groups[i].Name == name {
			return &groups.Groups[i]
		}
	}
	return nil
}

// discoverSlinkyControllerGVR tries the preferred version first, then every
// remaining served version. The bool result is true when malformed or failed
// discovery made absence ambiguous.
func discoverSlinkyControllerGVR(
	ctx context.Context,
	discoveryClient apiResourceDiscovery,
	group *metav1.APIGroup,
) (*schema.GroupVersionResource, bool) {

	return discoverAPIResourceGVR(
		ctx,
		discoveryClient,
		group,
		slinkyAPIGroup,
		slinkyControllerResource,
		slinkyControllerKind,
	)
}

func discoverAPIResourceGVR(
	ctx context.Context,
	discoveryClient apiResourceDiscovery,
	group *metav1.APIGroup,
	expectedGroup string,
	resourceName string,
	kind string,
) (*schema.GroupVersionResource, bool) {

	versions, ambiguous := orderedGroupVersions(group, expectedGroup)
	for _, groupVersion := range versions {
		if err := ctx.Err(); err != nil {
			slog.Warn("Kubernetes API resource discovery cancelled",
				slog.String("groupVersion", groupVersion),
				slog.String("resource", resourceName),
				slog.String("error", err.Error()))
			return nil, true
		}

		resources, err := discoveryClient.ServerResourcesForGroupVersion(groupVersion)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			slog.Warn("failed to discover Kubernetes API resources",
				slog.String("groupVersion", groupVersion),
				slog.String("resource", resourceName),
				slog.String("error", err.Error()))
			ambiguous = true
			continue
		}
		if resources == nil {
			slog.Warn("Kubernetes API resource discovery returned no response",
				slog.String("groupVersion", groupVersion),
				slog.String("resource", resourceName))
			ambiguous = true
			continue
		}

		gv, err := schema.ParseGroupVersion(resources.GroupVersion)
		if err != nil || gv.String() != groupVersion {
			slog.Warn("ignoring malformed Kubernetes API discovery response",
				slog.String("requestedGroupVersion", groupVersion),
				slog.String("returnedGroupVersion", resources.GroupVersion),
				slog.String("resource", resourceName))
			ambiguous = true
			continue
		}
		for _, resource := range resources.APIResources {
			if resource.Name == resourceName && resource.Kind == kind {
				return &schema.GroupVersionResource{
					Group:    gv.Group,
					Version:  gv.Version,
					Resource: resource.Name,
				}, ambiguous
			}
		}
	}
	return nil, ambiguous
}

func orderedGroupVersions(group *metav1.APIGroup, expectedGroup string) ([]string, bool) {
	if group == nil {
		return nil, true
	}

	versions := make([]string, 0, len(group.Versions))
	seen := make(map[string]struct{}, len(group.Versions))
	ambiguous := false
	add := func(raw string) {
		if raw == "" {
			return
		}
		gv, err := schema.ParseGroupVersion(raw)
		if err != nil || gv.Group != expectedGroup || gv.Version == "" {
			ambiguous = true
			return
		}
		normalized := gv.String()
		if _, ok := seen[normalized]; ok {
			return
		}
		seen[normalized] = struct{}{}
		versions = append(versions, normalized)
	}

	add(group.PreferredVersion.GroupVersion)
	for _, version := range group.Versions {
		add(version.GroupVersion)
	}
	if len(versions) == 0 {
		ambiguous = true
	}
	return versions, ambiguous
}
