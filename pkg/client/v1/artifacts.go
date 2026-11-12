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
	stderrors "errors"
	"strings"

	"github.com/NVIDIA/aicr/pkg/artifact"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// ArtifactOptions selects the same inventory and exceptions as AICRConfig.
type ArtifactOptions = artifact.Options

// WithArtifacts configures immutable catalog composition before resolution.
func WithArtifacts(options ArtifactOptions) Option {
	options.AllowCatalogPins = append([]string(nil), options.AllowCatalogPins...)
	return func(c *Client) {
		copyOptions := options
		copyOptions.AllowCatalogPins = append([]string(nil), options.AllowCatalogPins...)
		c.artifactOptions = &copyOptions
	}
}

// artifactConsumerError changes only a recognized provenance rejection at an
// unsupported consumer boundary; parse and schema errors retain their cause.
func artifactConsumerError(err error, reason string) error {
	var structured *errors.StructuredError
	if stderrors.As(err, &structured) && structured.Code == errors.ErrCodeInvalidRequest {
		switch structured.Context["reason"] {
		case "artifact-inputs-required", "artifact-inputs-mismatch", "artifact-source-unsupported":
			return artifact.Invalid(reason, "artifact-bearing recipes are unsupported by this consumer")
		}
	}
	return err
}

func validateArtifactFacade(r *RecipeResult) error {
	if r.internal.Metadata.Artifacts == nil {
		return nil
	}
	expected := facadeResultFromInternal(r.internal, r.Name).Components
	if len(expected) != len(r.Components) {
		return artifact.Invalid("artifact-identity-conflict", "SDK component set differs from resolved recipe")
	}
	for i, component := range r.Components {
		component.Kind = strings.ToLower(component.Kind)
		canonical := expected[i]
		canonical.Kind = strings.ToLower(canonical.Kind)
		if component != canonical {
			return artifact.Invalid("artifact-identity-conflict", "SDK component differs from resolved recipe: "+component.Name)
		}
	}
	return nil
}
