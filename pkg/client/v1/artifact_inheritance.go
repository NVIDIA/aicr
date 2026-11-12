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

package aicr

import (
	"bytes"
	"context"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"k8s.io/apimachinery/pkg/util/validation"
)

// readInheritanceSource validates historical identity without attaching a
// provider, resolving defaults, or loading any values or manifests.
func readInheritanceSource(ctx context.Context, path string, mode recipe.InheritMode) (*recipe.RecipeResult, error) {
	data, format, err := serializer.ReadFileBytesWithKubeconfigContext(ctx, path, "")
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "read inheritance source")
	}
	result, err := recipe.DecodeRecipeResult(data, format)
	if err != nil {
		return nil, err
	}
	reader, err := serializer.NewReader(format, bytes.NewReader(data), serializer.WithStrict())
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "read historical recipe schema")
	}
	if err := reader.Deserialize(result); err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "validate historical recipe schema")
	}
	if err := result.ValidateCoherence(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range result.ComponentRefs {
		ref := &result.ComponentRefs[i]
		if ref.Name == "" || seen[ref.Name] {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "historical recipe component names must be nonempty and unique")
		}
		seen[ref.Name] = true
		if ref.Namespace != "" && len(validation.IsDNS1123Label(ref.Namespace)) != 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "historical recipe has invalid namespace for "+ref.Name)
		}
		if mode == recipe.InheritNamespace {
			*ref = recipe.ComponentRef{Name: ref.Name, Namespace: ref.Namespace}
		}
	}
	return result, nil
}

func loadInheritanceSource(ctx context.Context, path, version string, mode recipe.InheritMode, provider recipe.DataProvider) (*recipe.RecipeResult, error) {
	if mode == recipe.InheritNamespace {
		return readInheritanceSource(ctx, path, mode)
	}
	if _, active := provider.(*recipe.ArtifactProvider); active {
		prior, err := readInheritanceSource(ctx, path, mode)
		if err != nil {
			return nil, err
		}
		// Legacy identity may be inherited into a new composition without making
		// the old recipe executable under that inventory.
		if prior.Metadata.Artifacts == nil {
			return prior, nil
		}
	}
	return recipe.LoadFromFileWithProvider(ctx, path, "", version, provider)
}
