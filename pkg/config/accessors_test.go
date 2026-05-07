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
	"testing"

	"github.com/NVIDIA/aicr/pkg/config"
)

func TestAccessors_NilTolerant(t *testing.T) {
	var nilCfg *config.AICRConfig
	if got := nilCfg.Recipe(); got != nil {
		t.Errorf("nil cfg Recipe() = %v, want nil", got)
	}
	if got := nilCfg.Bundle(); got != nil {
		t.Errorf("nil cfg Bundle() = %v, want nil", got)
	}

	var nilRecipe *config.RecipeSpec
	if got := nilRecipe.SnapshotPath(); got != "" {
		t.Errorf("nil RecipeSpec SnapshotPath = %q, want empty", got)
	}
	if got := nilRecipe.OutputPath(); got != "" {
		t.Errorf("nil RecipeSpec OutputPath = %q, want empty", got)
	}
	if got := nilRecipe.OutputFormat(); got != "" {
		t.Errorf("nil RecipeSpec OutputFormat = %q, want empty", got)
	}
	if got := nilRecipe.DataDir(); got != "" {
		t.Errorf("nil RecipeSpec DataDir = %q, want empty", got)
	}
}

func TestAccessors_EmptyReturnsEmpty(t *testing.T) {
	r := &config.RecipeSpec{}
	if got := r.SnapshotPath(); got != "" {
		t.Errorf("empty RecipeSpec SnapshotPath = %q", got)
	}
	if got := r.OutputPath(); got != "" {
		t.Errorf("empty RecipeSpec OutputPath = %q", got)
	}
	if got := r.OutputFormat(); got != "" {
		t.Errorf("empty RecipeSpec OutputFormat = %q", got)
	}
	if got := r.DataDir(); got != "" {
		t.Errorf("empty RecipeSpec DataDir = %q", got)
	}
}

func TestAccessors_PopulatedReturnsValues(t *testing.T) {
	cfg := &config.AICRConfig{
		Spec: config.Spec{
			Recipe: &config.RecipeSpec{
				Input:  &config.RecipeInputSpec{Snapshot: "snap.yaml"},
				Output: &config.RecipeOutputSpec{Path: "out.yaml", Format: "json"},
				Data:   "/data",
			},
			Bundle: &config.BundleSpec{},
		},
	}
	r := cfg.Recipe()
	if r == nil {
		t.Fatal("Recipe() = nil")
	}
	if got := r.SnapshotPath(); got != "snap.yaml" {
		t.Errorf("SnapshotPath = %q", got)
	}
	if got := r.OutputPath(); got != "out.yaml" {
		t.Errorf("OutputPath = %q", got)
	}
	if got := r.OutputFormat(); got != "json" {
		t.Errorf("OutputFormat = %q", got)
	}
	if got := r.DataDir(); got != "/data" {
		t.Errorf("DataDir = %q", got)
	}
	if cfg.Bundle() == nil {
		t.Fatal("Bundle() = nil")
	}
}
