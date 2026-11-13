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

package bundler

import (
	"fmt"
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const (
	// promqlScrapeClassName is applied by being the default class; no monitor
	// names it.
	promqlScrapeClassName = "aicr-promql-consumers"

	// namespaceNameLabel is set by the API server on every Namespace.
	namespaceNameLabel = "kubernetes.io/metadata.name"

	errCtxKeyPath = "path"
)

const (
	scrapeClassNameField          = "name"
	selectorOperatorField         = "operator"
	selectorMatchExpressionsField = "matchExpressions"
)

// PromQLScrapeScope is what InjectPromQLScrapeScope wrote. Its slices are
// sorted and de-duplicated; an empty Server means nothing was written.
type PromQLScrapeScope struct {
	Server     string
	Consumers  []string
	Namespaces []string
	Series     []string
	KeepRegex  string
}

// InjectPromQLScrapeScope writes the enabled telemetry.promql consumers'
// union, plus the bundle's own runtimeMetrics, into the values of the enabled
// telemetry.promqlServer component (ADR-026), mutating values in place.
// DefaultBundler.Make and Client.BundleComponents both call it.
//
// platform is every component the platform runs, not only those in values,
// so a partial bundle still validates the whole platform; the server is
// written only when values holds it. With no enabled server it writes
// nothing, and checks only that runtimeMetrics is empty. It fails with
// ErrCodeInvalidRequest when a monitorsFrom component is absent or disabled,
// the server has no namespace, more than one server is enabled, the server's
// values already hold an owned path, or runtimeMetrics is invalid or has no
// server to apply to.
func InjectPromQLScrapeScope(
	provider recipe.DataProvider,
	platform []recipe.ComponentRef,
	values map[string]map[string]any,
	runtimeMetrics config.RuntimeMetrics,
) (PromQLScrapeScope, error) {

	if err := recipe.ValidateRuntimeMetrics(runtimeMetrics.Series, runtimeMetrics.Namespaces); err != nil {
		return PromQLScrapeScope{}, err
	}
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		return PromQLScrapeScope{}, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to load component registry for the PromQL scrape scope")
	}

	scope, server, err := planPromQLScrapeScope(registry, platform, runtimeMetrics)
	if err != nil {
		return PromQLScrapeScope{}, err
	}
	if scope == nil {
		if !runtimeMetrics.IsZero() {
			return PromQLScrapeScope{}, errors.New(errors.ErrCodeInvalidRequest,
				"runtimeMetrics is set, but this recipe enables no runtime Prometheus server "+
					"(no enabled component declares telemetry.promqlServer), so nothing would apply it")
		}
		return PromQLScrapeScope{}, nil
	}

	serverValues, emitted := values[scope.Server]
	if !emitted {
		return PromQLScrapeScope{}, nil
	}
	if serverValues == nil {
		serverValues = make(map[string]any)
		values[scope.Server] = serverValues
	}
	if err := applyPromQLScrapeScope(scope, server.Telemetry.PromQLServer, serverValues); err != nil {
		return PromQLScrapeScope{}, err
	}
	return *scope, nil
}

// planPromQLScrapeScope returns a nil scope when no server is enabled. It
// checks only what depends on the recipe: the registry loader has already
// validated each declaration and compiled each pattern, and the caller has
// validated runtimeMetrics.
func planPromQLScrapeScope(
	registry *recipe.ComponentRegistry,
	platform []recipe.ComponentRef,
	runtimeMetrics config.RuntimeMetrics,
) (*PromQLScrapeScope, *recipe.ComponentConfig, error) {

	namespaces := make(map[string]string, len(platform))
	var enabled []string
	for _, ref := range platform {
		if !ref.IsEnabled() {
			continue
		}
		namespaces[ref.Name] = ref.Namespace
		enabled = append(enabled, ref.Name)
	}
	slices.Sort(enabled)

	server, err := enabledPromQLServer(registry, enabled)
	if err != nil || server == nil {
		return nil, nil, err
	}

	// The server's own namespace is always selected: a monitor there may
	// select targets in any namespace, which is how a cluster owner adds a
	// workload without a new bundle (ADR-026).
	serverNamespace := namespaces[server.Name]
	if serverNamespace == "" {
		return nil, nil, errors.NewWithContext(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"component %q declares telemetry.promqlServer but has no namespace in this recipe", server.Name),
			map[string]any{errCtxKeyComponent: server.Name})
	}
	scope := &PromQLScrapeScope{Server: server.Name, Series: slices.Clone(runtimeMetrics.Series)}
	scopeNamespaces := append([]string{serverNamespace}, runtimeMetrics.Namespaces...)
	for _, name := range enabled {
		comp := registry.Get(name)
		if comp == nil || !comp.Telemetry.PromQL.IsDeclared() {
			continue
		}
		consumer := comp.Telemetry.PromQL
		for _, source := range consumer.MonitorsFrom {
			namespace, ok := namespaces[source]
			if !ok {
				return nil, nil, errors.NewWithContext(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"component %q queries series exposed by %q (telemetry.promql.monitorsFrom), but %q is absent "+
						"or disabled in this recipe, so %s would not scrape them; enable %q, or disable %q",
					name, source, source, server.Name, source, name),
					map[string]any{errCtxKeyComponent: name, "monitorsFrom": source})
			}
			if namespace == "" {
				return nil, nil, errors.NewWithContext(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"component %q: telemetry.promql.monitorsFrom component %q has no namespace in this recipe",
					name, source),
					map[string]any{errCtxKeyComponent: name, "monitorsFrom": source})
			}
			scopeNamespaces = append(scopeNamespaces, namespace)
		}
		scope.Series = append(scope.Series, consumer.Series...)
		scope.Consumers = append(scope.Consumers, name)
	}

	slices.Sort(scopeNamespaces)
	scope.Namespaces = slices.Compact(scopeNamespaces)
	slices.Sort(scope.Series)
	scope.Series = slices.Compact(scope.Series)
	scope.KeepRegex = promqlKeepRegex(scope.Series)
	return scope, server, nil
}

// promqlKeepRegex groups each pattern so a flag it sets, such as (?i), does
// not reach the patterns after it.
func promqlKeepRegex(series []string) string {
	groups := make([]string, len(series))
	for i, pattern := range series {
		groups[i] = "(?:" + pattern + ")"
	}
	return "^(?:" + strings.Join(groups, "|") + ")$"
}

// enabledPromQLServer returns nil when no enabled component is a server.
func enabledPromQLServer(registry *recipe.ComponentRegistry, enabled []string) (*recipe.ComponentConfig, error) {
	var server *recipe.ComponentConfig
	var names []string
	for _, name := range enabled {
		if comp := registry.Get(name); comp != nil && comp.Telemetry.PromQLServer.IsDeclared() {
			server = comp
			names = append(names, name)
		}
	}
	if len(names) > 1 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"more than one enabled component declares telemetry.promqlServer (%s); disable all but one",
			strings.Join(names, ", ")))
	}
	return server, nil
}

// applyPromQLScrapeScope fails rather than overwrite a value an overlay or
// values file set at an owned path.
func applyPromQLScrapeScope(scope *PromQLScrapeScope, paths recipe.PromQLServerConfig, values map[string]any) error {
	for _, path := range paths.OwnedPaths() {
		if valuePathOccupied(values, path) {
			return errors.NewWithContext(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"%s values set %s, which the bundler writes from the registry's telemetry.promql declarations "+
					"and the bundle's runtimeMetrics; remove it from the values file or overlay", scope.Server, path),
				map[string]any{errCtxKeyComponent: scope.Server, errCtxKeyPath: path})
		}
	}

	for _, path := range paths.NamespaceSelectorPaths {
		component.SetValueByPath(values, path, promqlNamespaceSelector(scope.Namespaces))
	}
	for _, path := range paths.MonitorSelectorPaths {
		component.SetValueByPath(values, path, promqlMatchAllSelector())
	}
	component.SetValueByPath(values, paths.ScrapeClassesPath, []any{
		map[string]any{
			scrapeClassNameField: promqlScrapeClassName,
			"default":            true,
			"metricRelabelings": []any{
				map[string]any{
					"action":       "keep",
					"sourceLabels": []any{"__name__"},
					"regex":        scope.KeepRegex,
				},
			},
		},
	})
	return nil
}

// valuePathOccupied also reports a non-map ancestor, which
// component.SetValueByPath would replace silently.
func valuePathOccupied(values map[string]any, path string) bool {
	current := values
	parts := strings.Split(path, ".")
	for i, part := range parts {
		next, ok := current[part]
		if !ok || next == nil {
			return false
		}
		if i == len(parts)-1 {
			return true
		}
		nextMap, isMap := next.(map[string]any)
		if !isMap {
			return true
		}
		current = nextMap
	}
	return false
}

// promqlMatchAllSelector is not the empty map, which kube-prometheus-stack
// treats as unset and replaces with its release-label selector.
func promqlMatchAllSelector() map[string]any {
	return map[string]any{selectorMatchExpressionsField: []any{}}
}

// promqlNamespaceSelector selects no namespace for an empty list, since an In
// requirement may not have empty values.
func promqlNamespaceSelector(namespaces []string) map[string]any {
	requirement := map[string]any{"key": namespaceNameLabel, selectorOperatorField: "DoesNotExist"}
	if len(namespaces) > 0 {
		names := make([]any, len(namespaces))
		for i, namespace := range namespaces {
			names[i] = namespace
		}
		requirement = map[string]any{"key": namespaceNameLabel, selectorOperatorField: "In", "values": names}
	}
	return map[string]any{selectorMatchExpressionsField: []any{requirement}}
}

// enforcePromQLServerOwnership rejects --set, --set-json, --set-file, and
// --dynamic on the enabled server's owned paths.
func (b *DefaultBundler) enforcePromQLServerOwnership(recipeResult *recipe.RecipeResult, platform []recipe.ComponentRef) error {
	registry, err := recipe.GetComponentRegistryFor(recipeResult.DataProvider())
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to load component registry for PromQL server ownership")
	}
	owned := make(map[string][]string)
	for _, ref := range platform {
		if comp := registry.Get(ref.Name); comp != nil && comp.Telemetry.PromQLServer.IsDeclared() {
			owned[ref.Name] = comp.Telemetry.PromQLServer.OwnedPaths()
		}
	}
	if len(owned) == 0 {
		return nil
	}
	return b.enforceOwnedPaths(recipeResult, recipe.OwnershipDomain{
		Name:  "the PromQL scrape scope (registry telemetry.promql declarations and the bundle's runtimeMetrics)",
		Paths: owned,
	})
}

// injectPromQLScrapeScope warns when an enabled server keeps no series.
func (r *bundleRun) injectPromQLScrapeScope(
	recipeResult *recipe.RecipeResult,
	platform []recipe.ComponentRef,
	values map[string]map[string]any,
) error {

	scope, err := InjectPromQLScrapeScope(recipeResult.DataProvider(), platform, values, r.Config.RuntimeMetrics())
	if err != nil {
		return err
	}
	if scope.Server != "" && len(scope.Series) == 0 {
		r.appendWarning(fmt.Sprintf("%s is enabled but no enabled component declares telemetry.promql "+
			"and the bundle sets no runtimeMetrics.series, so its Prometheus keeps no series", scope.Server))
	}
	return nil
}
