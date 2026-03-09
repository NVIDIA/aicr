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

// Package catalog provides the declarative validator catalog.
// The catalog defines which validator containers exist, what phase they belong to,
// and how they should be executed as Kubernetes Jobs.
package catalog

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/recipes"
	"gopkg.in/yaml.v3"
)

const (
	// expectedAPIVersion is the supported catalog API version.
	expectedAPIVersion = "aicr.nvidia.com/v1"

	// expectedKind is the supported catalog kind.
	expectedKind = "ValidatorCatalog"
)

// ValidatorCatalog is the top-level catalog document.
type ValidatorCatalog struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   CatalogMetadata  `yaml:"metadata"`
	Validators []ValidatorEntry `yaml:"validators"`
}

// CatalogMetadata contains catalog-level metadata.
type CatalogMetadata struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"` // SemVer
}

// ValidatorEntry defines a single validator container.
type ValidatorEntry struct {
	// Name is the unique identifier for this validator, used in Job names.
	Name string `yaml:"name"`

	// Phase is the validation phase: "deployment", "performance", or "conformance".
	Phase string `yaml:"phase"`

	// Description is a human-readable description of what this validator checks.
	Description string `yaml:"description"`

	// Image is the OCI image reference for the validator container.
	Image string `yaml:"image"`

	// Timeout is the maximum execution time for this validator.
	// Maps to Job activeDeadlineSeconds.
	Timeout time.Duration `yaml:"timeout"`

	// Args are the container arguments.
	Args []string `yaml:"args"`

	// Env are environment variables to set in the container.
	Env []EnvVar `yaml:"env"`

	// Resources specifies container resource requests/limits.
	// If nil, defaults from pkg/defaults are used.
	Resources *ResourceRequirements `yaml:"resources,omitempty"`
}

// ResourceRequirements defines CPU and memory for a validator container.
type ResourceRequirements struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
}

// EnvVar is a name/value pair for container environment variables.
type EnvVar struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// Load reads and parses the embedded catalog.
//
// Image tag resolution (applied in order):
//  1. If a catalog entry uses :latest and version is a release (vX.Y.Z),
//     the tag is replaced with the CLI version for reproducibility.
//  2. If AICR_VALIDATOR_IMAGE_REGISTRY is set, the registry prefix is replaced.
//
// Entries with explicit version tags (e.g., :v1.2.3) are never modified.
func Load(version string) (*ValidatorCatalog, error) {
	data, err := recipes.FS.ReadFile("validators/catalog.yaml")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read embedded catalog", err)
	}

	cat, err := Parse(data)
	if err != nil {
		return nil, err
	}

	// Replace :latest with CLI version for reproducibility.
	if isReleaseVersion(version) {
		for i := range cat.Validators {
			cat.Validators[i].Image = replaceLatestTag(cat.Validators[i].Image, version)
		}
	}

	// Apply image registry override if set.
	if override := os.Getenv("AICR_VALIDATOR_IMAGE_REGISTRY"); override != "" {
		for i := range cat.Validators {
			cat.Validators[i].Image = replaceRegistry(cat.Validators[i].Image, override)
		}
	}

	return cat, nil
}

// isReleaseVersion returns true for semantic version strings (vX.Y.Z),
// false for dev builds ("dev", "v0.0.0-next", empty).
func isReleaseVersion(version string) bool {
	if version == "" || version == "dev" || strings.Contains(version, "-next") {
		return false
	}
	return true
}

// replaceLatestTag replaces :latest with the given version tag.
// Images with explicit version tags are not modified.
// Ensures the tag has a "v" prefix to match the on-tag release workflow
// (GoReleaser strips the "v" from the version but tags keep it).
func replaceLatestTag(image, version string) string {
	if strings.HasSuffix(image, ":latest") {
		tag := version
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		return strings.TrimSuffix(image, ":latest") + ":" + tag
	}
	return image
}

// Parse parses a catalog from raw YAML bytes. Exported for testing with
// inline catalogs without depending on the embedded file.
func Parse(data []byte) (*ValidatorCatalog, error) {
	var catalog ValidatorCatalog
	if err := yaml.Unmarshal(data, &catalog); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse catalog YAML", err)
	}

	if err := validate(&catalog); err != nil {
		return nil, err
	}

	return &catalog, nil
}

// ForPhase returns validators filtered by phase name.
func (c *ValidatorCatalog) ForPhase(phase string) []ValidatorEntry {
	var result []ValidatorEntry
	for _, v := range c.Validators {
		if v.Phase == phase {
			result = append(result, v)
		}
	}
	return result
}

// validate checks the catalog for structural correctness.
func validate(c *ValidatorCatalog) error {
	if c.APIVersion != expectedAPIVersion {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported apiVersion %q, expected %q", c.APIVersion, expectedAPIVersion))
	}
	if c.Kind != expectedKind {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported kind %q, expected %q", c.Kind, expectedKind))
	}

	validPhases := map[string]bool{
		"deployment":  true,
		"performance": true,
		"conformance": true,
	}

	seen := make(map[string]bool)
	for i, v := range c.Validators {
		if v.Name == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("validator[%d]: name is required", i))
		}
		if seen[v.Name] {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("validator[%d]: duplicate name %q", i, v.Name))
		}
		seen[v.Name] = true

		if !validPhases[v.Phase] {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("validator %q: invalid phase %q, must be one of: deployment, performance, conformance", v.Name, v.Phase))
		}
		if v.Image == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("validator %q: image is required", v.Name))
		}
	}

	return nil
}

// replaceRegistry replaces the registry prefix of an image reference.
// Example: replaceRegistry("ghcr.io/nvidia/aicr-validators/deployment:latest", "localhost:5001")
// returns "localhost:5001/aicr-validators/deployment:latest".
func replaceRegistry(image, newRegistry string) string {
	// Find the first path segment after the registry.
	// Registry is everything before the first "/" that contains a "." or ":"
	// (e.g., "ghcr.io/nvidia" or "localhost:5001").
	parts := strings.SplitN(image, "/", 3)
	if len(parts) < 3 {
		// Simple image like "registry/image:tag" — replace registry
		if len(parts) == 2 {
			return newRegistry + "/" + parts[1]
		}
		return image
	}
	// parts[0] = "ghcr.io", parts[1] = "nvidia", parts[2] = "aicr-validators/deployment:latest"
	// We want: newRegistry + "/" + parts[2]
	return newRegistry + "/" + parts[2]
}
