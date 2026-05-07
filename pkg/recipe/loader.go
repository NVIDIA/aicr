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

// LoadFromFile loads a recipe from the given path and returns a hydrated RecipeResult.
// If the file contains a RecipeMetadata overlay, it is auto-hydrated via the recipe
// builder using the overlay's criteria. This allows callers to accept both overlay
// files and pre-hydrated RecipeResult files transparently.
func LoadFromFile(ctx context.Context, path, kubeconfig, version string) (*RecipeResult, error) {
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

		builder := NewBuilder(WithVersion(version))
		rec, err = builder.BuildFromCriteria(ctx, overlay.Spec.Criteria)
		if err != nil {
			return nil, err
		}

		slog.Info("overlay hydrated successfully",
			"appliedOverlays", rec.Metadata.AppliedOverlays)
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
