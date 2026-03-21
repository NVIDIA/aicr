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

// Package recipe provides configuration recipe generation based on deployment criteria.
//
// # Overview
//
// The recipe package generates tailored configuration recommendations for GPU-accelerated
// Kubernetes clusters. It uses a metadata-driven model where base configurations are
// enhanced with criteria-specific overlays to produce deployment-ready component references.
//
// # Core Types
//
// Criteria: Specifies target deployment parameters
//
//	type Criteria struct {
//	    Service     CriteriaServiceType     // eks, gke, aks, any
//	    Accelerator CriteriaAcceleratorType // h100, gb200, b200, a100, l40, any
//	    Intent      CriteriaIntentType      // training, inference, any
//	    OS          CriteriaOSType          // ubuntu, cos, rhel, any
//	    Nodes       int                     // node count (0 = any)
//	}
//
// RecipeResult: Generated configuration result
//
//	type RecipeResult struct {
//	    Header                              // API version, kind, metadata
//	    Criteria      *Criteria             // Input criteria
//	    MatchedRules  []string              // Applied overlay rules
//	    ComponentRefs []ComponentRef        // Component references (Helm or Kustomize)
//	    Constraints   []ConstraintRef       // Validation constraints
//	}
//
// Recipe: Legacy format still used by bundlers
//
//	type Recipe struct {
//	    Header                              // API version, kind, metadata
//	    Request      *RequestInfo           // Input metadata (optional)
//	    MatchedRules []string               // Applied overlay rules
//	    Measurements []*measurement.Measurement // Configuration data
//	}
//
// Builder: Generates recipes from criteria
//
//	type Builder struct {
//	    Version string  // Builder version for tracking
//	}
//
// # Criteria Types
//
// Service types for Kubernetes environments:
//   - CriteriaServiceEKS: Amazon EKS
//   - CriteriaServiceGKE: Google GKE
//   - CriteriaServiceAKS: Azure AKS
//   - CriteriaServiceAny: Any service (wildcard)
//
// Accelerator types for GPU selection:
//   - CriteriaAcceleratorH100: NVIDIA H100
//   - CriteriaAcceleratorGB200: NVIDIA GB200
//   - CriteriaAcceleratorB200: NVIDIA B200
//   - CriteriaAcceleratorA100: NVIDIA A100
//   - CriteriaAcceleratorL40: NVIDIA L40
//   - CriteriaAcceleratorAny: Any accelerator (wildcard)
//
// Intent types for workload optimization:
//   - CriteriaIntentTraining: ML training workloads
//   - CriteriaIntentInference: Inference workloads
//   - CriteriaIntentAny: Generic workloads
//
// # Usage
//
// Basic recipe generation with criteria:
//
//	criteria := recipe.NewCriteria()
//	criteria.Service = recipe.CriteriaServiceEKS
//	criteria.Accelerator = recipe.CriteriaAcceleratorH100
//	criteria.Intent = recipe.CriteriaIntentTraining
//
//	ctx := context.Background()
//	builder := recipe.NewBuilder()
//	result, err := builder.BuildFromCriteria(ctx, criteria)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	fmt.Printf("Matched rules: %v\n", result.MatchedRules)
//	for _, ref := range result.ComponentRefs {
//	    fmt.Printf("Component: %s, Version: %s\n", ref.Name, ref.Version)
//	}
//
// HTTP handler for API server:
//
//	builder := recipe.NewBuilder()
//	http.HandleFunc("/v1/recipe", builder.HandleRecipes)
//
// Parse criteria from HTTP request:
//
//	criteria, err := recipe.ParseCriteriaFromRequest(r)
//	if err != nil {
//	    http.Error(w, err.Error(), http.StatusBadRequest)
//	    return
//	}
//
// # Query Parameters (HTTP API - GET)
//
// The HTTP handler accepts these query parameters for GET requests:
//   - service: eks, gke, aks, any (default: any)
//   - accelerator: h100, gb200, b200, a100, l40, any (default: any)
//   - gpu: alias for accelerator (backwards compatibility)
//   - intent: training, inference, any (default: any)
//   - os: ubuntu, cos, rhel, any (default: any)
//   - nodes: integer node count (default: 0 = any)
//
// # Criteria Files (CLI and HTTP API - POST)
//
// Criteria can be defined in a Kubernetes-style YAML or JSON file using the
// RecipeCriteria resource type. This provides an alternative to individual
// CLI flags or query parameters.
//
// RecipeCriteria: Kubernetes-style resource for criteria definition
//
//	type RecipeCriteria struct {
//	    Kind       string    // Must be "RecipeCriteria"
//	    APIVersion string    // Must be "aicr.nvidia.com/v1alpha1"
//	    Metadata   struct {
//	        Name string       // Optional descriptive name
//	    }
//	    Spec *Criteria        // The criteria specification
//	}
//
// Example criteria file (criteria.yaml):
//
//	kind: RecipeCriteria
//	apiVersion: aicr.nvidia.com/v1alpha1
//	metadata:
//	  name: gb200-eks-ubuntu-training
//	spec:
//	  service: eks
//	  os: ubuntu
//	  accelerator: gb200
//	  intent: training
//
// Load criteria from a file:
//
//	criteria, err := recipe.LoadCriteriaFromFile("/path/to/criteria.yaml")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	result, err := builder.BuildFromCriteria(ctx, criteria)
//
// Parse criteria from HTTP request body (POST):
//
//	criteria, err := recipe.ParseCriteriaFromBody(r.Body, r.Header.Get("Content-Type"))
//	if err != nil {
//	    http.Error(w, err.Error(), http.StatusBadRequest)
//	    return
//	}
//
// CLI usage with criteria file:
//
//	aicr recipe --criteria /path/to/criteria.yaml --output recipe.yaml
//
// CLI flags can override criteria file values:
//
//	aicr recipe --criteria criteria.yaml --service gke --output recipe.yaml
//
// HTTP API POST request:
//
//	curl -X POST http://localhost:8080/v1/recipe \
//	  -H "Content-Type: application/yaml" \
//	  -d @criteria.yaml
//
// # Criteria Matching
//
// Criteria use asymmetric matching with priority-based resolution:
//
// Recipe Wildcard (recipe field = "any"):
//   - Recipe "any" acts as a wildcard, matching any query value
//   - Example: Recipe with accelerator="any" matches query accelerator="h100"
//
// Query Wildcard (query field = "any"):
//   - Query "any" only matches recipes that also have "any"
//   - Prevents generic queries from matching overly-specific recipes
//   - Example: Query accelerator="any" does NOT match recipe accelerator="h100"
//
// Exact Match:
//   - Query service="eks", accelerator="h100" matches recipe with same values
//
// Priority:
//   - More specific overlays take precedence
//   - Multiple matching overlays are applied in priority order
//   - Later overlays can override earlier ones
//
// # Metadata Store Model
//
// Recipe generation uses YAML metadata files:
//
// 1. Load overlays/base.yaml (common component versions and settings)
// 2. Find matching overlay files based on criteria
// 3. Merge overlay configurations into result
// 4. Return RecipeResult with component references
//
// Base structure (recipes/overlays/base.yaml):
//
//	apiVersion: aicr.nvidia.com/v1alpha1
//	kind: Base
//	metadata:
//	  name: base
//	  version: v1.0.0
//	components:
//	  - name: gpu-operator
//	    version: v25.3.3
//	    repository: https://helm.ngc.nvidia.com/nvidia
//
// Overlay structure (recipes/overlays/*.yaml):
//
//	apiVersion: aicr.nvidia.com/v1alpha1
//	kind: Overlay
//	metadata:
//	  name: h100-training
//	  priority: 100
//	match:
//	  accelerator: h100
//	  intent: training
//	components:
//	  - name: gpu-operator
//	    version: v25.3.3
//	    values:
//	      mig.strategy: mixed
//
// # RecipeInput Interface
//
// The RecipeInput interface allows bundlers to work with both legacy Recipe
// and new RecipeResult formats:
//
//	type RecipeInput interface {
//	    GetMeasurements() []*measurement.Measurement
//	    GetComponentRef(name string) *ComponentRef
//	    GetValuesForComponent(name string) (map[string]any, error)
//	}
//
// # Error Handling
//
// BuildFromCriteria returns errors when:
//   - Criteria is nil
//   - Metadata store cannot be loaded
//   - No matching overlays found
//   - Component configuration is invalid
//
// ParseCriteriaFromRequest returns errors when:
//   - Service type is invalid
//   - Accelerator type is invalid
//   - Intent type is invalid
//   - Nodes count is negative or non-numeric
//
// # Data Source
//
// Recipe metadata is embedded at build time from:
//   - recipes/overlays/base.yaml (base component versions)
//   - recipes/overlays/*.yaml (criteria-specific overlays)
//
// The metadata store is loaded once and cached (singleton pattern with sync.Once).
//
// # Observability
//
// The recipe builder exports Prometheus metrics:
//   - recipe_built_duration_seconds: Time to build recipe
//   - recipe_rule_match_total{status}: Rule matching statistics
//
// # Integration
//
// The recipe package is used by:
//   - pkg/cli - recipe command for CLI usage
//   - pkg/api - API recipe endpoint
//   - pkg/bundler - Bundle generation from recipes
//
// It depends on:
//   - pkg/measurement - Measurement data structures
//   - pkg/version - Version parsing
//   - pkg/header - Common header types
//   - pkg/errors - Structured error handling
//
// # Component Types
//
// The recipe system supports two component deployment types:
//
// Helm Components:
//   - Use Helm charts for deployment
//   - Configured via helm section in registry.yaml
//   - Support values files and inline overrides
//
// Kustomize Components:
//   - Use Kustomize for deployment
//   - Configured via kustomize section in registry.yaml
//   - Support Git/OCI sources with path and tag
//
// The component registry (recipes/registry.yaml) determines component
// defaults. Components must have either helm OR kustomize configuration.
//
// # Subpackages
//
//   - recipe/version - Semantic version parsing (moved to pkg/version)
//   - recipe/header - Common header structures (moved to pkg/header)
package recipe
