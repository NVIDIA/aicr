// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package config

import (
	"fmt"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// Validate checks that an AICRConfig is well-formed and that all enumerated
// values are recognized by the corresponding recipe / bundler parsers.
//
// Validate does NOT check semantic interactions across the recipe and bundle
// commands (for example, that a bundle.input.recipe path actually exists);
// such checks belong to the caller that consumes the config.
func (c *AICRConfig) Validate() error {
	if c == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "config is nil")
	}
	if c.Kind != Kind {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid kind %q: expected %q", c.Kind, Kind))
	}
	if c.APIVersion != APIVersion {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid apiVersion %q: expected %q", c.APIVersion, APIVersion))
	}
	if c.Spec.Recipe == nil && c.Spec.Bundle == nil && c.Spec.Validate == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			"config has none of spec.recipe, spec.bundle, spec.validate; at least one is required")
	}
	if err := c.Spec.Recipe.validate(); err != nil {
		return err
	}
	if err := c.Spec.Bundle.validate(); err != nil {
		return err
	}
	if err := c.Spec.Validate.validate(); err != nil {
		return err
	}
	if err := c.Spec.validateRecipeBundleHandoff(); err != nil {
		return err
	}
	return nil
}

// validateRecipeBundleHandoff catches a silent footgun in workflow files:
// when both spec.recipe.output.path and spec.bundle.input.recipe are set,
// they typically describe the same artifact (the recipe written by `aicr
// recipe` and consumed by `aicr bundle`). Mismatched paths almost always
// indicate a typo rather than a deliberate two-recipe workflow, so reject
// them up-front. Setting only one side is fine.
//
// Comparison uses filepath.Clean on both sides so equivalent forms like
// "./recipe.yaml" and "recipe.yaml", or "dir//file" and "dir/file", do not
// trigger a false rejection. Mixing absolute and relative paths still
// fails — they are not equivalent without a known base directory.
func (s Spec) validateRecipeBundleHandoff() error {
	if s.Recipe == nil || s.Recipe.Output == nil || s.Recipe.Output.Path == "" {
		return nil
	}
	if s.Bundle == nil || s.Bundle.Input == nil || s.Bundle.Input.Recipe == "" {
		return nil
	}
	if filepath.Clean(s.Recipe.Output.Path) != filepath.Clean(s.Bundle.Input.Recipe) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("spec.recipe.output.path (%q) and spec.bundle.input.recipe (%q) must reference the same file when both are set",
				s.Recipe.Output.Path, s.Bundle.Input.Recipe))
	}
	return nil
}

// Wire-type validation delegates to the same Resolve methods callers use
// to consume the values. There is no parallel parser to maintain — if a
// field parses cleanly here, it will parse cleanly when consumed, and
// vice versa.

func (r *RecipeSpec) validate() error {
	if r == nil {
		return nil
	}
	if r.Criteria != nil && r.Input != nil && r.Input.Snapshot != "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"spec.recipe.criteria and spec.recipe.input.snapshot are mutually exclusive")
	}
	if _, err := r.ResolveCriteria(); err != nil {
		return err
	}
	if r.Output != nil && r.Output.Format != "" {
		if serializer.Format(r.Output.Format).IsUnknown() {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid spec.recipe.output.format %q (valid: yaml, json, table)", r.Output.Format))
		}
	}
	return nil
}

func (b *BundleSpec) validate() error {
	if b == nil {
		return nil
	}
	if _, err := b.Resolve(); err != nil {
		return err
	}
	return nil
}

func (v *ValidateSpec) validate() error {
	if v == nil {
		return nil
	}
	if _, err := v.Resolve(); err != nil {
		return err
	}
	return nil
}
