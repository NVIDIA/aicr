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

package attestation

import (
	"context"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// ComputeRecipeDigest loads a recipe or overlay from path (auto-hydrating
// overlays through the recipe builder, the same path aicr validate -r
// takes) and returns the canonical subject digest — the value that
// aicr validate --emit-attestation stamps into predicate.recipe.digest.
//
// kubeconfig is consulted only when path is a cm://namespace/name URI.
// version is the running aicr version threaded into the recipe builder.
// dp is the DataProvider used to resolve recipe references (overlays,
// component values); pass nil to fall back to the embedded provider when
// no external data source is configured.
func ComputeRecipeDigest(ctx context.Context, dp recipe.DataProvider, path, kubeconfig, version string) (string, error) {
	if path == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest, "recipe path is required")
	}

	rec, err := recipe.LoadFromFileWithProvider(ctx, path, kubeconfig, version, dp)
	if err != nil {
		return "", err
	}

	recipeYAML, err := serializer.MarshalYAMLDeterministic(rec)
	if err != nil {
		return "", errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to marshal recipe for digest")
	}

	return SubjectDigest(recipeYAML)
}
