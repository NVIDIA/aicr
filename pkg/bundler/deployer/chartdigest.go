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

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// ResolveChartDigests returns the registry's helm.defaultDigest for each of
// refs that still points at the exact chart its registry entry pins, keyed by
// component name, in one registry round-trip. Refs without a digest are
// omitted.
//
// The digest identifies the pinned archive only, so a ref that overrides
// source, chart or version (see UsesRegistryChart) gets none. Shared by every
// deployer that vendors charts, so verification is identical across bundles.
//
// A registry failure is fatal rather than an empty map: an empty map reads as
// "nothing to verify" and would vendor every chart unchecked.
func ResolveChartDigests(
	ctx context.Context,
	dp recipe.DataProvider,
	refs []recipe.ComponentRef,
) (map[string]string, error) {

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.WrapCtxErr(ctxErr, errors.ErrCodeTimeout,
			"context canceled before resolving chart digests")
	}
	registry, regErr := recipe.GetComponentRegistryFor(dp)
	if regErr != nil {
		return nil, errors.PropagateOrWrap(regErr, errors.ErrCodeInternal,
			"failed to resolve component registry for chart digests")
	}
	digests := make(map[string]string, len(refs))
	for _, ref := range refs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.WrapCtxErr(ctxErr, errors.ErrCodeTimeout,
				"context canceled while resolving chart digests")
		}
		cfg := registry.Get(ref.Name)
		if cfg == nil || cfg.Helm.DefaultDigest == "" || !UsesRegistryChart(ref, cfg) {
			continue
		}
		digests[ref.Name] = cfg.Helm.DefaultDigest
	}
	return digests, nil
}
