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
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"

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

// sizeComponent sizes ref's resolved values at the bundle's node count. It
// returns the sizing document and count so the request-within-limit check can
// run once every override is applied.
func (b *DefaultBundler) sizeComponent(ctx context.Context, rr *recipe.RecipeResult, ref *recipe.ComponentRef,
	values map[string]any) (*recipe.ComponentSizing, int, error) {

	n := b.Config.EstimatedNodeCount()
	if n <= 0 {
		return nil, 0, nil
	}
	file, err := sizingFileFor(rr.DataProvider(), ref.Name)
	if err != nil || file == "" {
		return nil, 0, err
	}
	sz, err := recipe.LoadComponentSizing(ctx, rr.DataProvider(), ref.Name, file)
	if err != nil {
		return nil, 0, err
	}
	applied, err := applySizing(values, ref.Overrides, rr.EffectiveLockSet()[ref.Name], sz, n)
	if err != nil {
		return nil, 0, errors.WrapWithContext(errors.ErrCodeInvalidRequest, "failed to size component memory", err,
			map[string]any{errCtxKeyComponent: ref.Name})
	}
	for _, a := range applied {
		slog.Info("sized memory from node count",
			"component", ref.Name, "path", a.Path, "from", a.From, "to", a.To, "gpuNodes", n)
	}
	return sz, n, nil
}

// applySizing lowers each memory request and limit the sizing document names
// to the tier covering n, writing min(resolved, tier). A field the values do
// not set is skipped, as is one that inlineOverrides or locked reach.
func applySizing(values, inlineOverrides map[string]any, locked []string, sz *recipe.ComponentSizing, n int) ([]sizedField, error) {
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
			if setsPathOrAncestor(inlineOverrides, path) || lockReaches(locked, path) {
				slog.Debug("sizing skipped a path set by the recipe", "path", path)
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

func lockReaches(locked []string, path string) bool {
	for _, l := range locked {
		if recipe.PathsIntersect(l, path) {
			return true
		}
	}
	return false
}
