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

package recipe

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// LoadFromFileWithProvider loads a recipe from the given path bound to an explicit DataProvider.
// Overlay inputs (kind: RecipeMetadata) are hydrated through a builder bound to
// dp (so external --data overlays resolve against dp, not the package global),
// and the returned result carries dp via its provider field. A nil dp falls
// back to the package-global provider (matching LoadFromFile).
func LoadFromFileWithProvider(ctx context.Context, path, kubeconfig, version string, dp DataProvider) (*RecipeResult, error) {
	rec, err := serializer.FromFileWithKubeconfig[RecipeResult](path, kubeconfig)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to load recipe from %q", path), err)
	}

	// Users often pass overlay files directly; auto-hydrate so they don't need
	// a separate "aicr recipe" step before consuming the recipe.
	if rec.Kind == RecipeMetadataKind {
		slog.Info("input is a RecipeMetadata overlay; auto-hydrating via recipe builder",
			"file", path)

		overlay, parseErr := serializer.FromFileWithKubeconfig[RecipeMetadata](path, kubeconfig)
		if parseErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to parse overlay from %q", path), parseErr)
		}

		if overlay.Spec.Criteria == nil {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("overlay %q has no criteria; only leaf overlays (with spec.criteria) "+
					"can be used directly — run \"aicr recipe\" with explicit criteria instead",
					path))
		}

		// Bind the builder to dp when supplied so the overlay resolves
		// against the caller's provider; the hydrated result inherits dp
		// via the metadata store (finalizeRecipeResult threads it through).
		// A nil dp omits the option, matching LoadFromFile's package-global
		// behavior exactly.
		opts := []Option{WithVersion(version)}
		if dp != nil {
			opts = append(opts, WithDataProvider(dp))
		}
		builder := NewBuilder(opts...)
		rec, err = builder.BuildFromCriteria(ctx, overlay.Spec.Criteria)
		if err != nil {
			return nil, err
		}

		slog.Info("overlay hydrated successfully",
			"appliedOverlays", rec.Metadata.AppliedOverlays)
	} else if dp != nil {
		// Already-hydrated RecipeResult: the builder never runs, so bind the
		// caller's provider directly so downstream value/manifest reads route
		// through dp rather than the package global.
		rec.provider = dp
	}

	// Empty kind is allowed for backward compatibility with older RecipeResult files
	// that may omit the field; they fall through to existing downstream validation.
	if rec.Kind != "" && rec.Kind != RecipeResultKind {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("recipe file has kind %q, but %q is required; "+
				"run \"aicr recipe\" to generate a hydrated RecipeResult first",
				rec.Kind, RecipeResultKind))
	}

	return rec, nil
}
