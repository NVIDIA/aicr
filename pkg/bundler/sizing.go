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
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/argocdhelm"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// sizedField is one memory request or limit that sizing lowered.
type sizedField struct {
	Path string
	From string
	To   string
}

// sizingFileFor returns the component's registry sizing file, empty when it
// has none.
func sizingFileFor(provider recipe.DataProvider, componentName string) (string, error) {
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		return "", errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to load component registry for sizing")
	}
	comp := registry.Get(componentName)
	if comp == nil {
		return "", nil
	}
	return comp.Sizing.File, nil
}

// sizingPlan is how one component is sized in this bundle. A nil sz means
// it is not sized.
type sizingPlan struct {
	sz         *recipe.ComponentSizing
	atInstall  bool
	recipeSets func(string) bool
}

// planSizing loads ref's sizing document and, unless ref is sized at install,
// sizes values at the bundle's node count. It loads nothing when no count is
// stated and ref is not sized at install.
func (b *DefaultBundler) planSizing(ctx context.Context, rr *recipe.RecipeResult, ref *recipe.ComponentRef,
	values map[string]any) (sizingPlan, error) {

	atInstall := b.sizedAtInstall(ref)
	n := b.Config.EstimatedNodeCount()
	if n <= 0 && !atInstall {
		return sizingPlan{}, nil
	}
	file, err := sizingFileFor(rr.DataProvider(), ref.Name)
	if err != nil || file == "" {
		return sizingPlan{}, err
	}
	sz, err := recipe.LoadComponentSizing(ctx, rr.DataProvider(), ref.Name, file)
	if err != nil {
		return sizingPlan{}, err
	}
	p := sizingPlan{sz: sz, atInstall: atInstall, recipeSets: recipeSetsPath(rr, ref)}
	if !atInstall {
		if err := sizeComponent(ref, values, p.recipeSets, sz, n); err != nil {
			return sizingPlan{}, err
		}
	}
	return p, nil
}

// rejectDynamic applies rejectDynamicSizedPaths to a component sized at
// install.
func (p sizingPlan) rejectDynamic(componentName string, dynPaths map[string]struct{}) error {
	if p.sz == nil || !p.atInstall {
		return nil
	}
	return rejectDynamicSizedPaths(componentName, dynPaths, p.sz)
}

// finishSizing runs once every override is applied. A component sized at
// install gets its rows, leaving out every path an override reaches; any
// other sized component must keep each request within its limit.
func (b *DefaultBundler) finishSizing(p sizingPlan, ref *recipe.ComponentRef, values map[string]any,
	setOverrides map[string]string, provider recipe.DataProvider) error {

	if p.sz == nil {
		return nil
	}
	if !p.atInstall {
		if err := checkSizedRequestsWithinLimits(values, p.sz, b.Config.EstimatedNodeCount()); err != nil {
			return errors.WrapWithContext(errors.ErrCodeInvalidRequest, "sized memory request exceeds its limit", err,
				map[string]any{errCtxKeyComponent: ref.Name})
		}
		return nil
	}
	typed := b.getTypedValueOverridesForComponent(ref.Name, provider)
	skip := func(path string) bool {
		return p.recipeSets(path) || overrideReaches(setOverrides, path) || overrideReaches(typed, path)
	}
	rows, err := installSizingRows(ref, values, skip, p.sz)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		b.installSizing[ref.Name] = rows
	}
	return nil
}

// sizedAtInstall reports whether ref is sized from the argocd-helm root value
// sizing.nodeCount instead of --nodes. Only an upstream chart that is not
// vendored reads its values from static/ at install; every other component
// bakes them at generation time.
func (b *DefaultBundler) sizedAtInstall(ref *recipe.ComponentRef) bool {
	return b.Config.Deployer() == config.DeployerArgoCDHelm && !b.Config.VendorCharts() && ref.HasExternalChart()
}

// sizeComponent sizes ref's resolved values at the bundle's node count.
func sizeComponent(ref *recipe.ComponentRef, values map[string]any, skip func(string) bool,
	sz *recipe.ComponentSizing, n int) error {

	applied, err := applySizing(values, skip, sz, n)
	if err != nil {
		return errors.WrapWithContext(errors.ErrCodeInvalidRequest, "failed to size component memory", err,
			map[string]any{errCtxKeyComponent: ref.Name})
	}
	for _, a := range applied {
		slog.Info("sized memory from node count",
			"component", ref.Name, "path", a.Path, "from", a.From, "to", a.To, "gpuNodes", n)
	}
	return nil
}

// recipeSetsPath reports whether ref's inline overrides or its profile lock
// set reach path; sizing never writes such a path.
func recipeSetsPath(rr *recipe.RecipeResult, ref *recipe.ComponentRef) func(string) bool {
	locked := rr.EffectiveLockSet()[ref.Name]
	return func(path string) bool {
		return setsPathOrAncestor(ref.Overrides, path) || lockReaches(locked, path)
	}
}

// installSizingRows builds one row per tier bound for an argocd-helm
// component sized at install: each row holds the fields that tier lowers
// from the final values, skipping every path skip reaches. A row merged
// over values must keep each request within its limit. Rows above the
// highest one that lowers something are dropped.
func installSizingRows(ref *recipe.ComponentRef, values map[string]any, skip func(string) bool,
	sz *recipe.ComponentSizing) ([]argocdhelm.SizingRow, error) {

	var bounds []int
	for i := range sz.Resources {
		for j := range sz.Resources[i].Tiers {
			bounds = append(bounds, sz.Resources[i].Tiers[j].MaxNodes)
		}
	}
	slices.Sort(bounds)
	bounds = slices.Compact(bounds)

	rows := make([]argocdhelm.SizingRow, 0, len(bounds))
	last := -1
	for _, bound := range bounds {
		merged := component.DeepCopyMap(values)
		applied, err := applySizing(merged, skip, sz, bound)
		if err != nil {
			return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest, "failed to size component memory", err,
				map[string]any{errCtxKeyComponent: ref.Name})
		}
		if err := checkSizedRequestsWithinLimits(merged, sz, bound); err != nil {
			return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("sized memory request exceeds its limit at sizing.nodeCount %d", bound), err,
				map[string]any{errCtxKeyComponent: ref.Name})
		}
		row := argocdhelm.SizingRow{MaxNodes: bound, Values: map[string]any{}}
		for _, a := range applied {
			component.SetValueByPath(row.Values, a.Path, a.To)
		}
		if len(applied) > 0 {
			last = len(rows)
		}
		rows = append(rows, row)
	}
	return rows[:last+1], nil
}

// rejectDynamicSizedPaths fails when a --dynamic path overlaps a path sizing
// writes: its pre-filled root value would always mask the selected row.
func rejectDynamicSizedPaths(componentName string, dynPaths map[string]struct{}, sz *recipe.ComponentSizing) error {
	for i := range sz.Resources {
		for _, field := range []string{recipe.SizingFieldRequests, recipe.SizingFieldLimits} {
			sized := sz.Resources[i].Path + "." + field + ".memory"
			for dyn := range dynPaths {
				if recipe.PathsIntersect(dyn, sized) {
					return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
						"component %q: --dynamic path %q overlaps %q, which sizing.nodeCount sets at install; "+
							"override it at install time as a root-chart value instead",
						componentName, dyn, sized))
				}
			}
		}
	}
	return nil
}

// applySizing lowers each memory request and limit the sizing document names
// to the tier covering n, writing min(resolved, tier). A field the values do
// not set is skipped, as is one skip reports.
func applySizing(values map[string]any, skip func(string) bool, sz *recipe.ComponentSizing, n int) ([]sizedField, error) {
	if sz == nil {
		return nil, nil
	}
	var applied []sizedField
	for i := range sz.Resources {
		r := &sz.Resources[i]
		tier, ok := r.TierFor(n)
		if !ok {
			continue
		}
		for _, f := range []struct {
			name string
			q    *recipe.SizingQuantities
		}{{recipe.SizingFieldRequests, tier.Requests}, {recipe.SizingFieldLimits, tier.Limits}} {
			if f.q == nil {
				continue
			}
			path := r.Path + "." + f.name + ".memory"
			if skip != nil && skip(path) {
				slog.Debug("sizing skipped a path set by the recipe or an override", "path", path)
				continue
			}
			existing, found := component.GetValueByPath(values, path)
			if !found {
				continue
			}
			cur, err := quantityOf(existing)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("sizing cannot compare %s: resolved value %v is not a quantity", path, existing), err)
			}
			want, err := resource.ParseQuantity(f.q.Memory)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "sizing tier quantity is not a quantity", err)
			}
			if want.Cmp(cur) >= 0 {
				continue
			}
			component.SetValueByPath(values, path, f.q.Memory)
			applied = append(applied, sizedField{Path: path, From: fmt.Sprint(existing), To: f.q.Memory})
		}
	}
	return applied, nil
}

// checkSizedRequestsWithinLimits fails when a resource sizing would size at n
// ends with its memory request above its limit after every override.
func checkSizedRequestsWithinLimits(values map[string]any, sz *recipe.ComponentSizing, n int) error {
	if sz == nil {
		return nil
	}
	for i := range sz.Resources {
		r := &sz.Resources[i]
		if _, ok := r.TierFor(n); !ok {
			continue
		}
		reqV, hasReq := component.GetValueByPath(values, r.Path+".requests.memory")
		limV, hasLim := component.GetValueByPath(values, r.Path+".limits.memory")
		if !hasReq || !hasLim {
			continue
		}
		req, reqErr := quantityOf(reqV)
		lim, limErr := quantityOf(limV)
		if reqErr != nil || limErr != nil {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"%s memory request %v or limit %v is not a quantity", r.Path, reqV, limV))
		}
		if req.Cmp(lim) > 0 {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"%s memory request %v is above its limit %v after overrides; raise the limit or lower the request",
				r.Path, reqV, limV))
		}
	}
	return nil
}

// quantityOf parses a values-file quantity; YAML gives a bare number as int
// or float64, which Kubernetes reads as bytes.
func quantityOf(v any) (resource.Quantity, error) {
	switch t := v.(type) {
	case string:
		return resource.ParseQuantity(t)
	case int:
		return *resource.NewQuantity(int64(t), resource.DecimalSI), nil
	case int64:
		return *resource.NewQuantity(t, resource.DecimalSI), nil
	case float64:
		return resource.ParseQuantity(strconv.FormatFloat(t, 'f', -1, 64))
	default:
		return resource.Quantity{}, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("unsupported type %T", v))
	}
}

// setsPathOrAncestor reports whether m sets path, or replaces an ancestor of
// it with a non-map value.
func setsPathOrAncestor(m map[string]any, path string) bool {
	if len(m) == 0 {
		return false
	}
	for i := range len(path) {
		if path[i] != '.' {
			continue
		}
		if v, ok := component.GetValueByPath(m, path[:i]); ok {
			if _, isMap := v.(map[string]any); !isMap {
				return true
			}
		}
	}
	_, ok := component.GetValueByPath(m, path)
	return ok
}

// lockReaches reports whether a lock-set path intersects path.
func lockReaches(locked []string, path string) bool {
	for _, l := range locked {
		if recipe.PathsIntersect(l, path) {
			return true
		}
	}
	return false
}

// overrideReaches reports whether any override path in m is path, an
// ancestor of it, or a descendant of it.
func overrideReaches[V any](m map[string]V, path string) bool {
	for p := range m {
		if recipe.PathsIntersect(p, path) {
			return true
		}
	}
	return false
}
