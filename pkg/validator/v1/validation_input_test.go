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

package v1

import (
	"encoding/json"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"gopkg.in/yaml.v3"
)

func TestValidationInputJSONMarshal(t *testing.T) {
	validation := &ValidationInput{
		APIVersion: "validator.nvidia.com/v1alpha1",
		Kind:       KindValidationInput,
		Metadata: &ValidationMetadata{
			Name:    "test-validation",
			Version: "1.0.0",
		},
		Config: ValidationConfig{
			Deployment: &ValidationPhase{
				Timeout: "10m",
				Checks:  []string{"check-1"},
			},
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator"},
		},
		Criteria: recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
		},
		Constraints: []recipe.Constraint{
			{Name: "test", Value: "value"},
		},
	}

	data, err := json.Marshal(validation)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}

	// Verify it contains expected fields
	jsonStr := string(data)
	expectedFields := []string{
		`"apiVersion":"validator.nvidia.com/v1alpha1"`,
		`"kind":"ValidationInput"`,
		`"name":"test-validation"`,
		`"version":"1.0.0"`,
		`"config"`,
	}
	for _, field := range expectedFields {
		if !contains(jsonStr, field) {
			t.Errorf("JSON missing field %s\nGot: %s", field, jsonStr)
		}
	}
}

func TestValidationInputJSONUnmarshal(t *testing.T) {
	jsonData := []byte(`{
		"apiVersion": "validator.nvidia.com/v1alpha1",
		"kind": "ValidationInput",
		"metadata": {
			"name": "test-validation",
			"version": "1.0.0"
		},
		"config": {
			"deployment": {
				"timeout": "10m",
				"checks": ["check-1"]
			}
		},
		"componentRefs": [
			{"name": "gpu-operator"}
		],
		"criteria": {
			"service": "eks",
			"accelerator": "h100"
		},
		"constraints": [
			{"name": "test", "value": "value"}
		]
	}`)

	var validation ValidationInput
	if err := json.Unmarshal(jsonData, &validation); err != nil {
		t.Fatalf("json.Unmarshal() failed: %v", err)
	}

	if validation.APIVersion != "validator.nvidia.com/v1alpha1" {
		t.Errorf("APIVersion = %q, want %q", validation.APIVersion, "validator.nvidia.com/v1alpha1")
	}
	if validation.Kind != KindValidationInput {
		t.Errorf("Kind = %q, want %q", validation.Kind, KindValidationInput)
	}
	if validation.Metadata == nil {
		t.Fatal("Metadata is nil")
	}
	if validation.Metadata.Name != "test-validation" {
		t.Errorf("Metadata.Name = %q, want %q", validation.Metadata.Name, "test-validation")
	}
	if validation.Config.Deployment == nil {
		t.Fatal("Config.Deployment is nil")
	}
	if validation.Config.Deployment.Timeout != "10m" {
		t.Errorf("Config.Deployment.Timeout = %q, want %q", validation.Config.Deployment.Timeout, "10m")
	}
}

func TestValidationInputYAMLMarshal(t *testing.T) {
	validation := &ValidationInput{
		APIVersion: "validator.nvidia.com/v1alpha1",
		Kind:       KindValidationInput,
		Metadata: &ValidationMetadata{
			Name:    "test-validation",
			Version: "1.0.0",
		},
		Config: ValidationConfig{
			Readiness: &ValidationPhase{
				Timeout: "5m",
			},
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator"},
		},
		Criteria: recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
		},
	}

	data, err := yaml.Marshal(validation)
	if err != nil {
		t.Fatalf("yaml.Marshal() failed: %v", err)
	}

	yamlStr := string(data)
	expectedFields := []string{
		"apiVersion: validator.nvidia.com/v1alpha1",
		"kind: ValidationInput",
		"name: test-validation",
		"config:",
	}
	for _, field := range expectedFields {
		if !contains(yamlStr, field) {
			t.Errorf("YAML missing field %s\nGot: %s", field, yamlStr)
		}
	}
}

func TestValidationInputOmitEmpty(t *testing.T) {
	// ValidationInput without optional fields - should serialize cleanly
	validation := &ValidationInput{
		Config: ValidationConfig{},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator"},
		},
		Criteria: recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
		},
	}

	data, err := json.Marshal(validation)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}

	jsonStr := string(data)
	// Verify optional fields are omitted
	if contains(jsonStr, `"apiVersion"`) {
		t.Error("JSON should not contain apiVersion when empty")
	}
	if contains(jsonStr, `"kind"`) {
		t.Error("JSON should not contain kind when empty")
	}
	if contains(jsonStr, `"metadata"`) {
		t.Error("JSON should not contain metadata when nil")
	}

	// But required/present fields should be there
	if !contains(jsonStr, `"config"`) {
		t.Error("JSON should contain config")
	}
	if !contains(jsonStr, `"componentRefs"`) {
		t.Error("JSON should contain componentRefs")
	}
}

func TestValidationInputEmbedding(t *testing.T) {
	// Simulate embedding in a CR spec
	type ValidationJobSpec struct {
		Validation ValidationInput `json:"validation"`
		Timeout    string          `json:"timeout"`
	}

	spec := ValidationJobSpec{
		Validation: ValidationInput{
			// No APIVersion/Kind/Metadata - clean embedding
			Config: ValidationConfig{
				Readiness: &ValidationPhase{
					Timeout: "5m",
				},
			},
			ComponentRefs: []recipe.ComponentRef{
				{Name: "gpu-operator"},
			},
			Criteria: recipe.Criteria{
				Service:     recipe.CriteriaServiceEKS,
				Accelerator: recipe.CriteriaAcceleratorH100,
			},
		},
		Timeout: "30m",
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}

	jsonStr := string(data)
	// Verify clean embedding without resource metadata
	if contains(jsonStr, `"apiVersion"`) {
		t.Error("Embedded validation should not contain apiVersion")
	}
	if contains(jsonStr, `"kind"`) {
		t.Error("Embedded validation should not contain kind")
	}
	// Verify config field is present
	if !contains(jsonStr, `"config"`) {
		t.Error("Embedded validation should contain config field")
	}
}

func TestNewValidationInput(t *testing.T) {
	validation := NewValidationInput()
	if validation == nil {
		t.Fatal("NewValidationInput returned nil")
	}
	if validation.ComponentRefs == nil {
		t.Error("ComponentRefs should be initialized to empty slice")
	}
	if validation.Constraints == nil {
		t.Error("Constraints should be initialized to empty slice")
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
