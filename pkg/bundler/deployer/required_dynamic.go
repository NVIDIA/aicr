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

package deployer

import (
	"context"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// ResolveRequiredDynamicPaths returns, per component in refs, the required
// value paths that a --dynamic declaration covers, exactly or through an
// ancestor or descendant. A local install script uses the result to refuse
// to run while such a path has no value in cluster-values.yaml. The result
// names the required path, not the declaration: a declaration on an
// ancestor map yields every required leaf beneath it, so the guard checks
// the leaves the operator fills in rather than the map line itself. dynamic
// is keyed by canonical component name.
func ResolveRequiredDynamicPaths(
	ctx context.Context,
	dp recipe.DataProvider,
	refs []recipe.ComponentRef,
	dynamic map[string][]string,
) (map[string][]string, error) {

	if len(dynamic) == 0 {
		return map[string][]string{}, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout,
			"context cancelled before resolving required dynamic paths", ctxErr)
	}
	registry, regErr := recipe.GetComponentRegistryFor(dp)
	if regErr != nil {
		return nil, errors.PropagateOrWrap(regErr, errors.ErrCodeInternal,
			"failed to resolve component registry for required dynamic paths")
	}
	out := make(map[string][]string)
	for _, ref := range refs {
		cfg := registry.Get(ref.Name)
		if cfg == nil || len(cfg.RequiredValues) == 0 {
			continue
		}
		seen := make(map[string]struct{})
		for _, dyn := range dynamic[ref.Name] {
			for _, required := range cfg.RequiredValues {
				if !valuePathsIntersect(dyn, required) {
					continue
				}
				path := required
				if len(dyn) > len(required) {
					path = dyn
				}
				if _, dup := seen[path]; dup {
					continue
				}
				seen[path] = struct{}{}
				out[ref.Name] = append(out[ref.Name], path)
			}
		}
	}
	return out, nil
}

// valuePathsIntersect reports whether two dotted value paths are equal or one
// is an ancestor of the other.
func valuePathsIntersect(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+".") || strings.HasPrefix(b, a+".")
}
