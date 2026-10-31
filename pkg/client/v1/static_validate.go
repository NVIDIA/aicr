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
	"context"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// RecipeDiagnostic describes a problem in a recipe authoring artifact.
type RecipeDiagnostic struct {
	File     string `json:"file"`
	Field    string `json:"field"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// ValidateRecipeCatalog statically validates an authoring file or catalog.
// An empty path validates the effective catalog; dataDir layers external data
// over the embedded catalog. Findings are returned as diagnostics, while err
// reports an operation that could not complete. Validation requires no cluster.
func ValidateRecipeCatalog(ctx context.Context, path, dataDir string) ([]RecipeDiagnostic, error) {
	return validateRecipeCatalog(ctx, path, dataDir, false)
}

// ValidateRecipeCatalogDirectory validates a complete catalog directory without
// embedded fallback, so missing working-tree references remain errors.
func ValidateRecipeCatalogDirectory(ctx context.Context, dataDir string) ([]RecipeDiagnostic, error) {
	return validateRecipeCatalog(ctx, "", dataDir, true)
}

func validateRecipeCatalog(ctx context.Context, path, dataDir string, externalOnly bool) ([]RecipeDiagnostic, error) {
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	var diagnostics []recipe.StaticDiagnostic
	var err error
	if externalOnly {
		diagnostics, err = recipe.ValidateStaticDirectory(ctx, dataDir)
	} else {
		diagnostics, err = recipe.ValidateStatic(ctx, path, dataDir)
	}
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to validate recipe catalog")
	}
	result := make([]RecipeDiagnostic, len(diagnostics))
	for i, diagnostic := range diagnostics {
		result[i] = RecipeDiagnostic{
			File:     diagnostic.File,
			Field:    diagnostic.Field,
			Severity: diagnostic.Severity,
			Message:  diagnostic.Message,
		}
	}
	return result, nil
}
