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

package config

import "testing"

// ComponentPath.Parse is exercised transitively via ParseValueOverrides and
// ParseDynamicValues — see TestParseValueOverrides and TestParseDynamicValues
// in config_test.go for the grammar coverage (error paths, path-segment
// validation, value handling). The tests below only cover the functional
// options that consume []ComponentPath, since those have no wrapper coverage.

// TestWithValueOverridePaths verifies entries with Value != nil are applied
// as value overrides; entries without Value are skipped.
func TestWithValueOverridePaths(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	cfg := NewConfig(WithValueOverridePaths([]ComponentPath{
		{Component: "gpuoperator", Path: "driver.version", Value: strPtr("570.86.16")},
		{Component: "gpuoperator", Path: "gds.enabled", Value: strPtr("true")},
		{Component: "alloy", Path: "clusterName"}, // Value nil — skipped
	}))

	overrides := cfg.ValueOverrides()
	if got := overrides["gpuoperator"]["driver.version"]; got != "570.86.16" {
		t.Errorf("overrides[gpuoperator][driver.version] = %q, want 570.86.16", got)
	}
	if got := overrides["gpuoperator"]["gds.enabled"]; got != "true" {
		t.Errorf("overrides[gpuoperator][gds.enabled] = %q, want true", got)
	}
	if _, ok := overrides["alloy"]; ok {
		t.Error("entry without Value should not produce a value override for alloy")
	}
}

// TestWithDynamicValuePaths verifies entries with Value == nil are applied
// as dynamic declarations; entries with Value != nil are skipped.
func TestWithDynamicValuePaths(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	cfg := NewConfig(WithDynamicValuePaths([]ComponentPath{
		{Component: "alloy", Path: "clusterName"},
		{Component: "alloy", Path: "namespace"},
		{Component: "gpuoperator", Path: "driver.version", Value: strPtr("570.86.16")}, // skipped
	}))

	dyn := cfg.DynamicValues()
	if n := len(dyn["alloy"]); n != 2 {
		t.Fatalf("dynamicValues[alloy] has %d paths, want 2", n)
	}
	if dyn["alloy"][0] != "clusterName" || dyn["alloy"][1] != "namespace" {
		t.Errorf("dynamicValues[alloy] = %v, want [clusterName namespace]", dyn["alloy"])
	}
	if _, ok := dyn["gpuoperator"]; ok {
		t.Error("entry with Value should not produce a dynamic declaration for gpuoperator")
	}
}

// TestWithValueOverridePaths_Empty verifies nil/empty input is a no-op.
func TestWithValueOverridePaths_Empty(t *testing.T) {
	cfg := NewConfig(WithValueOverridePaths(nil))
	if len(cfg.ValueOverrides()) != 0 {
		t.Error("ValueOverrides() should be empty for nil input")
	}
}

// TestWithDynamicValuePaths_Empty verifies nil/empty input is a no-op.
func TestWithDynamicValuePaths_Empty(t *testing.T) {
	cfg := NewConfig(WithDynamicValuePaths(nil))
	if cfg.HasDynamicValues() {
		t.Error("HasDynamicValues() should be false for nil input")
	}
}
