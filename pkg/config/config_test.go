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

package config_test

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/config"
)

func newValid() *config.AICRConfig {
	return &config.AICRConfig{
		Kind:       config.Kind,
		APIVersion: config.APIVersion,
		Metadata:   config.Metadata{Name: "test"},
		Spec: config.Spec{
			Recipe: &config.RecipeSpec{
				Criteria: &config.CriteriaSpec{
					Service:     "eks",
					Accelerator: "h100",
					Intent:      "training",
					OS:          "ubuntu",
					Platform:    "kubeflow",
					Nodes:       8,
				},
			},
		},
	}
}

func TestValidate_HappyPath(t *testing.T) {
	if err := newValid().Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_BothSpecsPopulated(t *testing.T) {
	cfg := newValid()
	cfg.Spec.Bundle = &config.BundleSpec{
		Deployment: &config.DeploymentSpec{Deployer: "helm"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_BundleOnly(t *testing.T) {
	cfg := &config.AICRConfig{
		Kind:       config.Kind,
		APIVersion: config.APIVersion,
		Spec: config.Spec{
			Bundle: &config.BundleSpec{
				Input:      &config.BundleInputSpec{Recipe: "./recipe.yaml"},
				Deployment: &config.DeploymentSpec{Deployer: "argocd"},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_ValidateOnly(t *testing.T) {
	cfg := &config.AICRConfig{
		Kind:       config.Kind,
		APIVersion: config.APIVersion,
		Spec: config.Spec{
			Validate: &config.ValidateSpec{
				Input: &config.ValidateInputSpec{
					Recipe:   "./recipe.yaml",
					Snapshot: "./snapshot.yaml",
				},
				Execution: &config.ValidateExecutionSpec{
					Timeout: "10m",
					Phases:  []string{"deployment", "conformance"},
				},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_InvalidValidateTimeout(t *testing.T) {
	cfg := &config.AICRConfig{
		Kind:       config.Kind,
		APIVersion: config.APIVersion,
		Spec: config.Spec{
			Validate: &config.ValidateSpec{
				Execution: &config.ValidateExecutionSpec{Timeout: "abc"},
			},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "spec.validate.execution.timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestValidate_Errors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.AICRConfig)
		wantSub string
	}{
		{
			name: "wrong kind",
			mutate: func(c *config.AICRConfig) {
				c.Kind = "Recipe"
			},
			wantSub: "invalid kind",
		},
		{
			name: "wrong apiVersion",
			mutate: func(c *config.AICRConfig) {
				c.APIVersion = "v1"
			},
			wantSub: "invalid apiVersion",
		},
		{
			name: "no recipe, no bundle, and no validate",
			mutate: func(c *config.AICRConfig) {
				c.Spec.Recipe = nil
				c.Spec.Bundle = nil
				c.Spec.Validate = nil
			},
			wantSub: "none of spec.recipe, spec.bundle, spec.validate",
		},
		{
			name: "criteria and snapshot mutually exclusive",
			mutate: func(c *config.AICRConfig) {
				c.Spec.Recipe.Input = &config.RecipeInputSpec{Snapshot: "s.yaml"}
			},
			wantSub: "mutually exclusive",
		},
		{
			name: "invalid service",
			mutate: func(c *config.AICRConfig) {
				c.Spec.Recipe.Criteria.Service = "bogus"
			},
			wantSub: "criteria.service",
		},
		{
			name: "invalid accelerator",
			mutate: func(c *config.AICRConfig) {
				c.Spec.Recipe.Criteria.Accelerator = "h99999"
			},
			wantSub: "criteria.accelerator",
		},
		{
			name: "invalid intent",
			mutate: func(c *config.AICRConfig) {
				c.Spec.Recipe.Criteria.Intent = "mining"
			},
			wantSub: "criteria.intent",
		},
		{
			name: "invalid os",
			mutate: func(c *config.AICRConfig) {
				c.Spec.Recipe.Criteria.OS = "windows"
			},
			wantSub: "criteria.os",
		},
		{
			name: "invalid platform",
			mutate: func(c *config.AICRConfig) {
				c.Spec.Recipe.Criteria.Platform = "spark"
			},
			wantSub: "criteria.platform",
		},
		{
			name: "negative nodes",
			mutate: func(c *config.AICRConfig) {
				c.Spec.Recipe.Criteria.Nodes = -1
			},
			wantSub: "must be >= 0",
		},
		{
			name: "invalid format",
			mutate: func(c *config.AICRConfig) {
				c.Spec.Recipe.Output = &config.RecipeOutputSpec{Format: "xml"}
			},
			wantSub: "spec.recipe.output.format",
		},
		{
			name: "invalid deployer",
			mutate: func(c *config.AICRConfig) {
				c.Spec.Bundle = &config.BundleSpec{
					Deployment: &config.DeploymentSpec{Deployer: "fluxcd"},
				}
			},
			wantSub: "deployment.deployer",
		},
		{
			name: "negative bundle nodes",
			mutate: func(c *config.AICRConfig) {
				c.Spec.Bundle = &config.BundleSpec{
					Scheduling: &config.SchedulingSpec{Nodes: -1},
				}
			},
			wantSub: "scheduling.nodes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newValid()
			tt.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantSub)
			}
		})
	}
}

func TestValidate_NilReceiver(t *testing.T) {
	var c *config.AICRConfig
	if err := c.Validate(); err == nil {
		t.Errorf("expected error from nil receiver, got nil")
	}
}

func TestValidate_RecipeBundleHandoff(t *testing.T) {
	tests := []struct {
		name        string
		recipePath  string
		bundleInput string
		wantErrSub  string
	}{
		{"both empty is fine", "", "", ""},
		{"only recipe.output set is fine", "out.yaml", "", ""},
		{"only bundle.input set is fine", "", "in.yaml", ""},
		{"matching paths is fine", "shared.yaml", "shared.yaml", ""},
		{"mismatched paths rejected", "out.yaml", "different.yaml", "must reference the same file"},
		{"equivalent relative forms accepted", "./recipe.yaml", "recipe.yaml", ""},
		{"redundant separators accepted", "dir//file.yaml", "dir/file.yaml", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.AICRConfig{
				Kind:       config.Kind,
				APIVersion: config.APIVersion,
				Spec: config.Spec{
					Recipe: &config.RecipeSpec{
						Criteria: &config.CriteriaSpec{Service: "eks"},
					},
					Bundle: &config.BundleSpec{
						Deployment: &config.DeploymentSpec{Deployer: "helm"},
					},
				},
			}
			if tt.recipePath != "" {
				cfg.Spec.Recipe.Output = &config.RecipeOutputSpec{Path: tt.recipePath}
			}
			if tt.bundleInput != "" {
				cfg.Spec.Bundle.Input = &config.BundleInputSpec{Recipe: tt.bundleInput}
			}
			err := cfg.Validate()
			if tt.wantErrSub == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErrSub)
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantErrSub)
			}
		})
	}
}
