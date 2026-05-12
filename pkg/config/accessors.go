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

// Accessors here are nil-receiver tolerant so callers can write
// `cfg.Recipe().OutputPath()` without nil-checking every intermediate pointer.
//
// Bundle-section accessors that previously returned untyped strings are
// gone — callers now consume (*BundleSpec).Resolve which performs the
// wire→domain conversion exactly once. The recipe-section accessors
// here remain because their fields are simple strings with no enum
// parsing beyond Output.Format (handled inline at the call site).

// Recipe returns the recipe section, or nil if cfg or the section is unset.
func (c *AICRConfig) Recipe() *RecipeSpec {
	if c == nil {
		return nil
	}
	return c.Spec.Recipe
}

// Bundle returns the bundle section, or nil if cfg or the section is unset.
func (c *AICRConfig) Bundle() *BundleSpec {
	if c == nil {
		return nil
	}
	return c.Spec.Bundle
}

// Validation returns the validate section, or nil if cfg or the section is
// unset. Named Validation (not Validate) to avoid colliding with
// (*AICRConfig).Validate, which checks well-formedness of the whole config.
func (c *AICRConfig) Validation() *ValidateSpec {
	if c == nil {
		return nil
	}
	return c.Spec.Validate
}

// SnapshotPath returns spec.recipe.input.snapshot, or "" when unset.
func (r *RecipeSpec) SnapshotPath() string {
	if r == nil || r.Input == nil {
		return ""
	}
	return r.Input.Snapshot
}

// OutputPath returns spec.recipe.output.path, or "" when unset.
func (r *RecipeSpec) OutputPath() string {
	if r == nil || r.Output == nil {
		return ""
	}
	return r.Output.Path
}

// OutputFormat returns spec.recipe.output.format, or "" when unset.
func (r *RecipeSpec) OutputFormat() string {
	if r == nil || r.Output == nil {
		return ""
	}
	return r.Output.Format
}

// DataDir returns spec.recipe.data, or "" when unset.
func (r *RecipeSpec) DataDir() string {
	if r == nil {
		return ""
	}
	return r.Data
}
