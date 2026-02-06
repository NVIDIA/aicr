// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// yaml_test.go validates the embedded recipe YAML files in data/.
//
// Area of Concern: Static YAML data file validation
// - Schema conformance - all YAML files parse into RecipeMetadata
// - Reference validation - spec.base, valuesFile, dependencies exist
// - Enum validation - service, accelerator, os, intent use valid values
// - Constraint syntax - all constraint expressions are parseable
// - Inheritance chains - no circular spec.base references, reasonable depth
// - Criteria completeness - leaf recipes have accelerator, os, intent
//
// These tests iterate over actual embedded YAML files to catch data errors
// at build/test time before runtime.
//
// Related test files:
// - metadata_test.go: Tests RecipeMetadata types, Merge(), TopologicalSort(),
//   ValidateDependencies(), and MetadataStore inheritance chain resolution
// - recipe_test.go: Tests Recipe struct validation methods after recipes
//   are built (Validate, ValidateStructure, ValidateMeasurementExists)

package recipe

import (
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"k8s.io/client-go/kubernetes/scheme"
)

// testMetadataFS embeds all recipe data files for testing.
// This uses a separate embed directive to include component values files and manifests.
//
//go:embed data/overlays/*.yaml data/components/*/*.yaml data/components/*/manifests/*.yaml
var testMetadataFS embed.FS

// validMeasurementTypes are the valid top-level measurement types for constraints.
var validMeasurementTypes = map[string]bool{
	"K8s":     true,
	"OS":      true,
	"GPU":     true,
	"SystemD": true,
}

// validConstraintOperators are the supported constraint operators.
var validConstraintOperators = []string{">=", "<=", ">", "<", "==", "!="}

// baseYAMLFile is the base recipe filename (relative to data/).
const baseYAMLFile = "overlays/base.yaml"

// ============================================================================
// Schema Conformance Tests
// ============================================================================

// TestAllMetadataFilesParseCorrectly verifies that all YAML files in data/
// parse into valid RecipeMetadata structures.
func TestAllMetadataFilesParseCorrectly(t *testing.T) {
	files := collectMetadataFiles(t)
	if len(files) == 0 {
		t.Fatal("no metadata files found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var metadata RecipeMetadata
			if err := yaml.Unmarshal(content, &metadata); err != nil {
				t.Errorf("failed to parse %s: %v", path, err)
			}
		})
	}
}

// TestAllMetadataFilesHaveRequiredFields verifies that all metadata files
// contain the required fields: kind, apiVersion, metadata.name.
func TestAllMetadataFilesHaveRequiredFields(t *testing.T) {
	files := collectMetadataFiles(t)

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var metadata RecipeMetadata
			if err := yaml.Unmarshal(content, &metadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			// Check required fields
			if metadata.Kind == "" {
				t.Error("missing required field: kind")
			}
			if metadata.APIVersion == "" {
				t.Error("missing required field: apiVersion")
			}
			if metadata.Metadata.Name == "" {
				t.Error("missing required field: metadata.name")
			}

			// Validate kind and apiVersion values
			if metadata.Kind != "recipeMetadata" {
				t.Errorf("invalid kind: got %q, want %q", metadata.Kind, "recipeMetadata")
			}
			if metadata.APIVersion != "eidos.nvidia.com/v1alpha1" {
				t.Errorf("invalid apiVersion: got %q, want %q", metadata.APIVersion, "eidos.nvidia.com/v1alpha1")
			}
		})
	}
}

// ============================================================================
// Criteria Validation Tests
// ============================================================================

// TestAllOverlayCriteriaUseValidEnums verifies that all overlay files use
// only valid enum values for criteria fields (service, accelerator, os, intent).
func TestAllOverlayCriteriaUseValidEnums(t *testing.T) {
	files := collectMetadataFiles(t)

	for _, path := range files {
		filename := filepath.Base(path)
		// Skip base.yaml - it doesn't have criteria
		if filename == filepath.Base(baseYAMLFile) {
			continue
		}

		t.Run(filename, func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var metadata RecipeMetadata
			if err := yaml.Unmarshal(content, &metadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			criteria := metadata.Spec.Criteria
			if criteria == nil {
				t.Error("overlay missing criteria field")
				return
			}

			// Validate service type
			if criteria.Service != "" && criteria.Service != CriteriaServiceAny {
				if _, err := ParseCriteriaServiceType(string(criteria.Service)); err != nil {
					t.Errorf("invalid service type %q: %v", criteria.Service, err)
				}
			}

			// Validate accelerator type
			if criteria.Accelerator != "" && criteria.Accelerator != CriteriaAcceleratorAny {
				if _, err := ParseCriteriaAcceleratorType(string(criteria.Accelerator)); err != nil {
					t.Errorf("invalid accelerator type %q: %v", criteria.Accelerator, err)
				}
			}

			// Validate intent type
			if criteria.Intent != "" && criteria.Intent != CriteriaIntentAny {
				if _, err := ParseCriteriaIntentType(string(criteria.Intent)); err != nil {
					t.Errorf("invalid intent type %q: %v", criteria.Intent, err)
				}
			}

			// Validate OS type
			if criteria.OS != "" && criteria.OS != CriteriaOSAny {
				if _, err := ParseCriteriaOSType(string(criteria.OS)); err != nil {
					t.Errorf("invalid OS type %q: %v", criteria.OS, err)
				}
			}
		})
	}
}

// ============================================================================
// Reference Validation Tests
// ============================================================================

// TestAllValuesFileReferencesExist verifies that all valuesFile references
// in componentRefs point to existing files in the data/components/ directory.
func TestAllValuesFileReferencesExist(t *testing.T) {
	files := collectMetadataFiles(t)

	// Build set of available values files
	availableFiles := collectValuesFiles(t)

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var metadata RecipeMetadata
			if err := yaml.Unmarshal(content, &metadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			for _, comp := range metadata.Spec.ComponentRefs {
				if comp.ValuesFile == "" {
					continue
				}

				if !availableFiles[comp.ValuesFile] {
					t.Errorf("componentRef %q references non-existent valuesFile: %q", comp.Name, comp.ValuesFile)
					t.Logf("available values files: %v", getKeys(availableFiles))
				}
			}
		})
	}
}

// TestAllDependencyReferencesExist verifies that all dependencyRefs
// reference components that are defined in the same file or base.yaml.
func TestAllDependencyReferencesExist(t *testing.T) {
	// Load base components first
	baseContent, err := testMetadataFS.ReadFile("data/" + baseYAMLFile)
	if err != nil {
		t.Fatalf("failed to read %s: %v", baseYAMLFile, err)
	}

	var baseMetadata RecipeMetadata
	if err := yaml.Unmarshal(baseContent, &baseMetadata); err != nil {
		t.Fatalf("failed to parse %s: %v", baseYAMLFile, err)
	}

	baseComponents := make(map[string]bool)
	for _, comp := range baseMetadata.Spec.ComponentRefs {
		baseComponents[comp.Name] = true
	}

	files := collectMetadataFiles(t)

	for _, path := range files {
		filename := filepath.Base(path)
		if filename == filepath.Base(baseYAMLFile) {
			continue // Already validated by ValidateDependencies
		}

		t.Run(filename, func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var metadata RecipeMetadata
			if err := yaml.Unmarshal(content, &metadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			// Build set of components defined in this overlay
			overlayComponents := make(map[string]bool)
			for _, comp := range metadata.Spec.ComponentRefs {
				overlayComponents[comp.Name] = true
			}

			// Check all dependency references
			for _, comp := range metadata.Spec.ComponentRefs {
				for _, dep := range comp.DependencyRefs {
					if !baseComponents[dep] && !overlayComponents[dep] {
						t.Errorf("componentRef %q references unknown dependency %q", comp.Name, dep)
					}
				}
			}
		})
	}
}

// TestAllComponentNamesMatchKnownComponents verifies that all component names
// in recipes match known components from the component registry.
func TestAllComponentNamesMatchKnownComponents(t *testing.T) {
	files := collectMetadataFiles(t)

	// Get all supported component names from the registry
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	supportedComponents := make(map[string]bool)
	for _, name := range registry.Names() {
		supportedComponents[name] = true
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var metadata RecipeMetadata
			if err := yaml.Unmarshal(content, &metadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			for _, comp := range metadata.Spec.ComponentRefs {
				if !supportedComponents[comp.Name] {
					t.Errorf("componentRef uses unknown component name %q; valid components: %v",
						comp.Name, registry.Names())
				}
			}
		})
	}
}

// ============================================================================
// Constraint Syntax Tests
// ============================================================================

// TestAllConstraintsSyntaxValid verifies that all constraints use valid syntax:
// - Measurement path format: {type}.{subtype}.{key}
// - Valid operators: >=, <=, >, <, ==, !=, or exact match
func TestAllConstraintsSyntaxValid(t *testing.T) {
	files := collectMetadataFiles(t)

	// Pattern for measurement path: Type.subtype.key (at least 3 parts)
	pathPattern := regexp.MustCompile(`^[A-Za-z0-9]+\.[A-Za-z0-9_/.-]+\.[A-Za-z0-9_/.-]+$`)

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var metadata RecipeMetadata
			if err := yaml.Unmarshal(content, &metadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			for _, constraint := range metadata.Spec.Constraints {
				// Validate constraint name (measurement path)
				if !pathPattern.MatchString(constraint.Name) {
					t.Errorf("constraint %q has invalid path format; expected {Type}.{subtype}.{key}", constraint.Name)
				}

				// Validate measurement type
				parts := strings.Split(constraint.Name, ".")
				if len(parts) >= 1 {
					measurementType := parts[0]
					if !validMeasurementTypes[measurementType] {
						t.Errorf("constraint %q uses unknown measurement type %q; valid types: %v",
							constraint.Name, measurementType, getKeys(validMeasurementTypes))
					}
				}

				// Validate constraint value (operator + value)
				if err := validateConstraintValue(constraint.Value); err != nil {
					t.Errorf("constraint %q has invalid value %q: %v", constraint.Name, constraint.Value, err)
				}
			}
		})
	}
}

// validateConstraintValue checks if a constraint value has valid syntax.
func validateConstraintValue(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("empty constraint value")
	}

	// Check for operator prefix
	for _, op := range validConstraintOperators {
		if strings.HasPrefix(value, op) {
			remainder := strings.TrimSpace(strings.TrimPrefix(value, op))
			if remainder == "" {
				return fmt.Errorf("operator %q without value", op)
			}
			return nil // Valid operator + value
		}
	}

	// No operator - valid as exact match
	return nil
}

// ============================================================================
// Inheritance Chain Validation Tests
// ============================================================================

// TestAllBaseReferencesPointToExistingRecipes verifies that all spec.base
// references in recipe files point to existing recipe files.
func TestAllBaseReferencesPointToExistingRecipes(t *testing.T) {
	files := collectMetadataFiles(t)

	// Build map of recipe names to files
	recipeNames := make(map[string]string)
	for _, path := range files {
		content, err := testMetadataFS.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}

		var metadata RecipeMetadata
		if err := yaml.Unmarshal(content, &metadata); err != nil {
			t.Fatalf("failed to parse %s: %v", path, err)
		}

		recipeNames[metadata.Metadata.Name] = path
	}

	// Check all base references
	for _, path := range files {
		filename := filepath.Base(path)
		if filename == filepath.Base(baseYAMLFile) {
			continue // base.yaml doesn't have a base reference
		}

		t.Run(filename, func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var metadata RecipeMetadata
			if err := yaml.Unmarshal(content, &metadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			// If spec.base is defined, verify it points to an existing recipe
			if metadata.Spec.Base != "" {
				if _, exists := recipeNames[metadata.Spec.Base]; !exists {
					t.Errorf("spec.base references non-existent recipe %q; available recipes: %v",
						metadata.Spec.Base, getKeys(recipeNames))
				}
			}
		})
	}
}

// TestNoCircularBaseReferences verifies that there are no circular references
// in the spec.base inheritance chain.
func TestNoCircularBaseReferences(t *testing.T) {
	files := collectMetadataFiles(t)

	// Build map of recipe name -> base reference
	baseRefs := make(map[string]string)
	for _, path := range files {
		content, err := testMetadataFS.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}

		var metadata RecipeMetadata
		if err := yaml.Unmarshal(content, &metadata); err != nil {
			t.Fatalf("failed to parse %s: %v", path, err)
		}

		if metadata.Spec.Base != "" {
			baseRefs[metadata.Metadata.Name] = metadata.Spec.Base
		}
	}

	// Check for cycles in each recipe's inheritance chain
	for recipeName := range baseRefs {
		t.Run(recipeName, func(t *testing.T) {
			visited := make(map[string]bool)
			current := recipeName

			for current != "" {
				if visited[current] {
					t.Errorf("circular inheritance detected: recipe %q leads back to itself", recipeName)
					return
				}
				visited[current] = true
				current = baseRefs[current]
			}
		})
	}
}

// TestInheritanceChainDepthReasonable verifies that inheritance chains
// don't exceed a reasonable depth (prevents accidental deep nesting).
func TestInheritanceChainDepthReasonable(t *testing.T) {
	const maxDepth = 10 // Reasonable limit for inheritance depth

	files := collectMetadataFiles(t)

	// Build map of recipe name -> base reference
	baseRefs := make(map[string]string)
	for _, path := range files {
		content, err := testMetadataFS.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}

		var metadata RecipeMetadata
		if err := yaml.Unmarshal(content, &metadata); err != nil {
			t.Fatalf("failed to parse %s: %v", path, err)
		}

		if metadata.Spec.Base != "" {
			baseRefs[metadata.Metadata.Name] = metadata.Spec.Base
		}
	}

	// Check depth of each recipe's inheritance chain
	for recipeName := range baseRefs {
		t.Run(recipeName, func(t *testing.T) {
			depth := 0
			current := recipeName

			for current != "" && depth <= maxDepth {
				depth++
				current = baseRefs[current]
			}

			if depth > maxDepth {
				t.Errorf("inheritance chain for %q exceeds max depth of %d", recipeName, maxDepth)
			}
		})
	}
}

// TestIntermediateRecipesHavePartialCriteria verifies that intermediate recipes
// (those with a spec.base but incomplete criteria) are properly structured.
// Intermediate recipes should have at least one criteria field set.
func TestIntermediateRecipesHavePartialCriteria(t *testing.T) {
	files := collectMetadataFiles(t)

	for _, path := range files {
		filename := filepath.Base(path)
		if filename == filepath.Base(baseYAMLFile) {
			continue
		}

		t.Run(filename, func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var metadata RecipeMetadata
			if err := yaml.Unmarshal(content, &metadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			// If this recipe has a base reference, it's part of an inheritance chain
			// Verify it has at least one criteria field to differentiate it
			if metadata.Spec.Base != "" && metadata.Spec.Criteria != nil {
				c := metadata.Spec.Criteria
				hasSomeCriteria := c.Service != "" || c.Accelerator != "" || c.OS != "" || c.Intent != ""
				if !hasSomeCriteria {
					t.Errorf("recipe with spec.base should have at least one criteria field set")
				}
			}
		})
	}
}

// TestLeafRecipesHaveCompleteCriteria verifies that leaf recipes
// (those that are intended to be matched directly) have complete criteria.
// A leaf recipe is one where no other recipe references it as a base.
func TestLeafRecipesHaveCompleteCriteria(t *testing.T) {
	files := collectMetadataFiles(t)

	// Build set of recipes that are referenced as base by other recipes
	referencedAsBases := make(map[string]bool)
	for _, path := range files {
		content, err := testMetadataFS.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}

		var metadata RecipeMetadata
		if err := yaml.Unmarshal(content, &metadata); err != nil {
			t.Fatalf("failed to parse %s: %v", path, err)
		}

		if metadata.Spec.Base != "" {
			referencedAsBases[metadata.Spec.Base] = true
		}
	}

	// Check leaf recipes (not referenced by others) have complete criteria
	for _, path := range files {
		filename := filepath.Base(path)
		if filename == filepath.Base(baseYAMLFile) {
			continue
		}

		t.Run(filename, func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var metadata RecipeMetadata
			if err := yaml.Unmarshal(content, &metadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			// Skip if this recipe is referenced as a base by another recipe
			// (it's an intermediate recipe, not a leaf)
			if referencedAsBases[metadata.Metadata.Name] {
				return
			}

			// Leaf recipes should have at least some criteria for matching.
			// They don't need ALL fields - partial criteria are valid for recipes
			// that should match multiple scenarios (e.g., a GKE recipe that works
			// with any accelerator or intent).
			c := metadata.Spec.Criteria
			if c == nil {
				t.Error("leaf recipe missing criteria")
				return
			}

			// Leaf recipes should have at least one criteria field to distinguish them
			// Empty/missing fields act as wildcards and match everything, which is valid
			hasSomeCriteria := c.Service != "" || c.Accelerator != "" || c.OS != "" || c.Intent != ""
			if !hasSomeCriteria {
				t.Error("leaf recipe should have at least one criteria field set")
			}
		})
	}
}

// ============================================================================
// Criteria Uniqueness Tests
// ============================================================================

// TestNoDuplicateCriteriaAcrossOverlays ensures no two overlays have
// identical criteria, which would cause non-deterministic matching.
func TestNoDuplicateCriteriaAcrossOverlays(t *testing.T) {
	files := collectMetadataFiles(t)

	// Map criteria string to file name
	criteriaMap := make(map[string]string)

	for _, path := range files {
		filename := filepath.Base(path)
		// Skip base.yaml - it doesn't have criteria
		if filename == filepath.Base(baseYAMLFile) {
			continue
		}

		content, err := testMetadataFS.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}

		var metadata RecipeMetadata
		if err := yaml.Unmarshal(content, &metadata); err != nil {
			t.Fatalf("failed to parse %s: %v", path, err)
		}

		// Create criteria key
		c := metadata.Spec.Criteria
		key := fmt.Sprintf("service=%s,accelerator=%s,os=%s,intent=%s,platform=%s",
			c.Service, c.Accelerator, c.OS, c.Intent, c.Platform)

		if existing, found := criteriaMap[key]; found {
			t.Errorf("duplicate criteria found:\n  %s: %s\n  %s: %s",
				existing, key, filename, key)
		}
		criteriaMap[key] = filename
	}
}

// ============================================================================
// Merge Consistency Tests
// ============================================================================

// TestBaseAndOverlaysMergeWithoutConflict verifies that each overlay
// can be merged with base without errors.
func TestBaseAndOverlaysMergeWithoutConflict(t *testing.T) {
	// Load base
	baseContent, err := testMetadataFS.ReadFile("data/" + baseYAMLFile)
	if err != nil {
		t.Fatalf("failed to read %s: %v", baseYAMLFile, err)
	}

	var baseMetadata RecipeMetadata
	if err := yaml.Unmarshal(baseContent, &baseMetadata); err != nil {
		t.Fatalf("failed to parse %s: %v", baseYAMLFile, err)
	}

	files := collectMetadataFiles(t)

	for _, path := range files {
		filename := filepath.Base(path)
		if filename == filepath.Base(baseYAMLFile) {
			continue
		}

		t.Run(filename, func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var overlayMetadata RecipeMetadata
			if err := yaml.Unmarshal(content, &overlayMetadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			// Create a copy of base spec for merging
			mergedSpec := baseMetadata.Spec

			// Attempt merge (Merge doesn't return error, panics on nil)
			mergedSpec.Merge(&overlayMetadata.Spec)

			// Verify merge produced valid result
			if len(mergedSpec.ComponentRefs) == 0 {
				t.Error("merged spec has no component refs")
			}
		})
	}
}

// TestMergedRecipesHaveNoCycles verifies that after merging base + overlay,
// the resulting recipe has no circular dependencies.
func TestMergedRecipesHaveNoCycles(t *testing.T) {
	// Load base
	baseContent, err := testMetadataFS.ReadFile("data/" + baseYAMLFile)
	if err != nil {
		t.Fatalf("failed to read %s: %v", baseYAMLFile, err)
	}

	var baseMetadata RecipeMetadata
	if err := yaml.Unmarshal(baseContent, &baseMetadata); err != nil {
		t.Fatalf("failed to parse %s: %v", baseYAMLFile, err)
	}

	files := collectMetadataFiles(t)

	for _, path := range files {
		filename := filepath.Base(path)
		if filename == filepath.Base(baseYAMLFile) {
			continue
		}

		t.Run(filename, func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var overlayMetadata RecipeMetadata
			if err := yaml.Unmarshal(content, &overlayMetadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			// Create a copy of base spec for merging
			mergedSpec := baseMetadata.Spec

			// Merge overlay
			mergedSpec.Merge(&overlayMetadata.Spec)

			// Validate no cycles in merged result
			if err := mergedSpec.ValidateDependencies(); err != nil {
				t.Errorf("merged recipe has dependency issues: %v", err)
			}
		})
	}
}

// ============================================================================
// Values File Parsing Tests
// ============================================================================

// TestAllValuesFilesParseAsValidYAML ensures all component values files
// are valid YAML.
func TestAllValuesFilesParseAsValidYAML(t *testing.T) {
	valuesFiles := collectValuesFiles(t)

	for path := range valuesFiles {
		t.Run(path, func(t *testing.T) {
			content, err := testMetadataFS.ReadFile("data/" + path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			// Parse as generic YAML to verify syntax
			var parsed any
			if err := yaml.Unmarshal(content, &parsed); err != nil {
				t.Errorf("failed to parse values file as YAML: %v", err)
			}
		})
	}
}

// ============================================================================
// Base Recipe Validation Tests
// ============================================================================

// TestBaseRecipeValidation verifies the base recipe passes all validations.
func TestBaseRecipeValidation(t *testing.T) {
	content, err := testMetadataFS.ReadFile("data/" + baseYAMLFile)
	if err != nil {
		t.Fatalf("failed to read %s: %v", baseYAMLFile, err)
	}

	var metadata RecipeMetadata
	if parseErr := yaml.Unmarshal(content, &metadata); parseErr != nil {
		t.Fatalf("failed to parse %s: %v", baseYAMLFile, parseErr)
	}

	// Validate dependencies
	if depErr := metadata.Spec.ValidateDependencies(); depErr != nil {
		t.Errorf("base recipe dependency validation failed: %v", depErr)
	}

	// Validate topological sort works
	order, sortErr := metadata.Spec.TopologicalSort()
	if sortErr != nil {
		t.Errorf("base recipe topological sort failed: %v", sortErr)
	}

	if len(order) != len(metadata.Spec.ComponentRefs) {
		t.Errorf("topological sort returned %d components, expected %d",
			len(order), len(metadata.Spec.ComponentRefs))
	}
}

// ============================================================================
// Component Type Validation Tests
// ============================================================================

// TestAllComponentTypesValid verifies that all componentRefs use valid types.
func TestAllComponentTypesValid(t *testing.T) {
	files := collectMetadataFiles(t)

	validTypes := map[ComponentType]bool{
		ComponentTypeHelm:      true,
		ComponentTypeKustomize: true,
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			var metadata RecipeMetadata
			if err := yaml.Unmarshal(content, &metadata); err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}

			for _, comp := range metadata.Spec.ComponentRefs {
				if comp.Type == "" {
					t.Errorf("componentRef %q missing type field", comp.Name)
					continue
				}
				if !validTypes[comp.Type] {
					t.Errorf("componentRef %q has invalid type %q; valid types: Helm, Kustomize",
						comp.Name, comp.Type)
				}
			}
		})
	}
}

// ============================================================================
// Manifest Helm Hooks Validation Tests
// ============================================================================

// standardK8sAPIVersions returns the set of Kubernetes API versions that don't require CRDs.
// Resources with these apiVersions don't need Helm hook annotations.
// The list is derived from k8s.io/client-go/kubernetes/scheme which contains all standard K8s types.
var standardK8sAPIVersions = func() map[string]bool {
	versions := make(map[string]bool)
	for gv := range scheme.Scheme.AllKnownTypes() {
		// Format: "group/version" or just "version" for core API
		var apiVersion string
		if gv.Group == "" {
			apiVersion = gv.Version // e.g., "v1"
		} else {
			apiVersion = gv.Group + "/" + gv.Version // e.g., "apps/v1"
		}
		versions[apiVersion] = true
	}
	return versions
}()

// TestManifestHelmHooksRequired validates that CRD-dependent manifests
// have the required Helm hook annotations for proper deployment ordering.
//
// Custom Resources (CRs) depend on CRDs installed by sub-charts. Without
// Helm hooks, the CR may be applied before its CRD exists, causing installation
// failures. This test ensures manifest authors add the required annotations.
//
// Required annotations for CRD-dependent resources:
//
//	metadata:
//	  annotations:
//	    "helm.sh/hook": post-install,post-upgrade
//	    "helm.sh/hook-weight": "10"
//	    "helm.sh/hook-delete-policy": before-hook-creation
//
// To opt-out (not recommended), add:
//
//	metadata:
//	  annotations:
//	    eidos/skip-hook-validation: "true"
func TestManifestHelmHooksRequired(t *testing.T) {
	// Patterns to extract apiVersion and check for annotations
	// Using regex to avoid YAML parsing issues with Helm template syntax
	apiVersionPattern := regexp.MustCompile(`(?m)^apiVersion:\s*(\S+)`)
	helmHookPattern := regexp.MustCompile(`(?m)["']?helm\.sh/hook["']?:\s*`)
	skipValidationPattern := regexp.MustCompile(`(?m)["']?eidos/skip-hook-validation["']?:\s*["']?true["']?`)

	manifestFiles := collectManifestFiles(t)
	if len(manifestFiles) == 0 {
		t.Log("no manifest files found in data/components/*/manifests/")
		return
	}

	for _, path := range manifestFiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			content, err := testMetadataFS.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read %s: %v", path, err)
			}

			contentStr := string(content)

			// Extract apiVersion
			matches := apiVersionPattern.FindStringSubmatch(contentStr)
			if len(matches) < 2 {
				t.Logf("no apiVersion found in %s, skipping", path)
				return
			}
			apiVersion := matches[1]

			// Check if this is a standard K8s API (no hooks needed)
			if isStandardK8sAPI(apiVersion) {
				return // Standard K8s resource, no hooks required
			}

			// This is a CRD-dependent resource - check for required annotations

			// Check for opt-out annotation
			if skipValidationPattern.MatchString(contentStr) {
				t.Logf("%s has eidos/skip-hook-validation annotation, skipping validation", path)
				return
			}

			// Check for helm.sh/hook annotation
			if !helmHookPattern.MatchString(contentStr) {
				t.Errorf(`manifest %q has custom apiVersion %q but is missing required Helm hook annotations.

CRD-dependent resources must include these annotations to ensure proper deployment ordering:

  metadata:
    annotations:
      "helm.sh/hook": post-install,post-upgrade
      "helm.sh/hook-weight": "10"
      "helm.sh/hook-delete-policy": before-hook-creation

To skip this validation (not recommended), add:
  metadata:
    annotations:
      eidos/skip-hook-validation: "true"
`, filepath.Base(path), apiVersion)
			}
		})
	}
}

// TestManifestHelmHooksValidation tests the validation logic with controlled inputs
// to ensure missing annotations are caught and skip annotations work correctly.
func TestManifestHelmHooksValidation(t *testing.T) {
	apiVersionPattern := regexp.MustCompile(`(?m)^apiVersion:\s*(\S+)`)
	helmHookPattern := regexp.MustCompile(`(?m)["']?helm\.sh/hook["']?:\s*`)
	skipValidationPattern := regexp.MustCompile(`(?m)["']?eidos/skip-hook-validation["']?:\s*["']?true["']?`)

	tests := []struct {
		name        string
		content     string
		expectError bool
	}{
		{
			name: "custom_resource_missing_hooks_should_fail",
			content: `apiVersion: skyhook.nvidia.com/v1alpha1
kind: Recipe
metadata:
  name: test-recipe
spec:
  template: {}`,
			expectError: true,
		},
		{
			name: "custom_resource_with_hooks_should_pass",
			content: `apiVersion: skyhook.nvidia.com/v1alpha1
kind: Recipe
metadata:
  name: test-recipe
  annotations:
    "helm.sh/hook": post-install,post-upgrade
    "helm.sh/hook-weight": "10"
spec:
  template: {}`,
			expectError: false,
		},
		{
			name: "custom_resource_with_skip_annotation_should_pass",
			content: `apiVersion: skyhook.nvidia.com/v1alpha1
kind: Recipe
metadata:
  name: test-recipe
  annotations:
    eidos/skip-hook-validation: "true"
spec:
  template: {}`,
			expectError: false,
		},
		{
			name: "standard_k8s_resource_without_hooks_should_pass",
			content: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
data:
  key: value`,
			expectError: false,
		},
		{
			name: "apps_v1_resource_without_hooks_should_pass",
			content: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-deployment
spec:
  replicas: 1`,
			expectError: false,
		},
		{
			name: "custom_resource_with_helm_template_syntax_missing_hooks_should_fail",
			content: `apiVersion: skyhook.nvidia.com/v1alpha1
kind: Recipe
metadata:
  name: {{ .Release.Name }}-recipe
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
spec:
  template: {}`,
			expectError: true,
		},
		{
			name: "custom_resource_with_helm_template_and_hooks_should_pass",
			content: `apiVersion: skyhook.nvidia.com/v1alpha1
kind: Recipe
metadata:
  name: {{ .Release.Name }}-recipe
  annotations:
    "helm.sh/hook": post-install,post-upgrade
    "helm.sh/hook-weight": "10"
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
spec:
  template: {}`,
			expectError: false,
		},
		{
			name: "networking_k8s_io_resource_should_pass",
			content: `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: test-policy
spec:
  podSelector: {}`,
			expectError: false,
		},
		{
			name: "rbac_resource_should_pass",
			content: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: test-role
rules: []`,
			expectError: false,
		},
		{
			name: "custom_api_different_domain_missing_hooks_should_fail",
			content: `apiVersion: mycompany.example.com/v1
kind: CustomThing
metadata:
  name: test-thing
spec: {}`,
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contentStr := tc.content

			// Extract apiVersion
			matches := apiVersionPattern.FindStringSubmatch(contentStr)
			if len(matches) < 2 {
				t.Fatalf("no apiVersion found in test case")
			}
			apiVersion := matches[1]

			// Check if this is a standard K8s API (no hooks needed)
			if isStandardK8sAPI(apiVersion) {
				if tc.expectError {
					t.Error("expected error for standard K8s resource, but it would be skipped")
				}
				return
			}

			// Check for opt-out annotation
			if skipValidationPattern.MatchString(contentStr) {
				if tc.expectError {
					t.Error("expected error, but skip annotation would bypass validation")
				}
				return
			}

			// Check for helm.sh/hook annotation
			hasHook := helmHookPattern.MatchString(contentStr)

			if tc.expectError && hasHook {
				t.Error("expected error (missing hooks), but helm.sh/hook was found")
			}
			if !tc.expectError && !hasHook {
				t.Error("expected pass (hooks present), but helm.sh/hook was not found")
			}
		})
	}
}

// isStandardK8sAPI checks if an apiVersion is a standard Kubernetes API
// that doesn't require a CRD.
func isStandardK8sAPI(apiVersion string) bool {
	return standardK8sAPIVersions[apiVersion]
}

// collectManifestFiles returns all manifest YAML files in data/components/*/manifests/.
func collectManifestFiles(t *testing.T) []string {
	t.Helper()

	var files []string
	err := fs.WalkDir(testMetadataFS, "data/components", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Only include files in manifests/ directories
		if !strings.Contains(path, "/manifests/") {
			return nil
		}
		// Only YAML files
		if !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk components directory: %v", err)
	}

	return files
}

// ============================================================================
// Helper Functions
// ============================================================================

// collectMetadataFiles returns all YAML files in data/ (excluding components/).
func collectMetadataFiles(t *testing.T) []string {
	t.Helper()

	var files []string
	err := fs.WalkDir(testMetadataFS, "data", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip component files
		if strings.Contains(path, "components/") {
			return nil
		}
		// Skip non-YAML files
		if !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		// Skip README
		if strings.HasSuffix(path, "README.md") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk data directory: %v", err)
	}

	return files
}

// collectValuesFiles returns all values files in data/components/ (excluding manifests/).
func collectValuesFiles(t *testing.T) map[string]bool {
	t.Helper()

	files := make(map[string]bool)
	err := fs.WalkDir(testMetadataFS, "data/components", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip manifest files (they contain Helm templates, not plain YAML)
		if strings.Contains(path, "/manifests/") {
			return nil
		}
		// Store relative path from data/
		relPath := strings.TrimPrefix(path, "data/")
		files[relPath] = true
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk components directory: %v", err)
	}

	return files
}

// getKeys returns the keys of a map as a slice.
func getKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
