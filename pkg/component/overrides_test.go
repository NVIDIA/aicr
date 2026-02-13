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

package component

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

const testTolKeyDedicated = "dedicated"

// TestStruct is a test struct with various field types.
type TestStruct struct {
	// Simple fields
	Name    string
	Enabled string
	Count   int

	// Nested struct
	Driver struct {
		Version string
		Enabled string
	}

	// Acronym fields
	EnableGDS string
	MIG       struct {
		Strategy string
	}
	GPUOperator struct {
		Version string
	}

	// Complex nested
	DCGM struct {
		Exporter struct {
			Version string
			Enabled string
		}
	}
}

func TestApplyValueOverrides_SimpleFields(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		want      TestStruct
		wantErr   bool
	}{
		{
			name: "set string field",
			overrides: map[string]string{
				"name": "test-value",
			},
			want: TestStruct{
				Name: "test-value",
			},
		},
		{
			name: "set enabled field",
			overrides: map[string]string{
				"enabled": "true",
			},
			want: TestStruct{
				Enabled: "true",
			},
		},
		{
			name: "set multiple fields",
			overrides: map[string]string{
				"name":    "test",
				"enabled": "false",
			},
			want: TestStruct{
				Name:    "test",
				Enabled: "false",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TestStruct{}
			err := ApplyValueOverrides(&got, tt.overrides)

			if (err != nil) != tt.wantErr {
				t.Errorf("ApplyValueOverrides() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if got.Name != tt.want.Name {
				t.Errorf("Name = %v, want %v", got.Name, tt.want.Name)
			}
			if got.Enabled != tt.want.Enabled {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tt.want.Enabled)
			}
		})
	}
}

func TestApplyValueOverrides_NestedFields(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		want      TestStruct
		wantErr   bool
	}{
		{
			name: "set nested field",
			overrides: map[string]string{
				"driver.version": "550.127",
			},
			want: TestStruct{
				Driver: struct {
					Version string
					Enabled string
				}{
					Version: "550.127",
				},
			},
		},
		{
			name: "set multiple nested fields",
			overrides: map[string]string{
				"driver.version": "550.127",
				"driver.enabled": "true",
			},
			want: TestStruct{
				Driver: struct {
					Version string
					Enabled string
				}{
					Version: "550.127",
					Enabled: "true",
				},
			},
		},
		{
			name: "set deeply nested field",
			overrides: map[string]string{
				"dcgm.exporter.version": "3.3.11",
				"dcgm.exporter.enabled": "true",
			},
			want: TestStruct{
				DCGM: struct {
					Exporter struct {
						Version string
						Enabled string
					}
				}{
					Exporter: struct {
						Version string
						Enabled string
					}{
						Version: "3.3.11",
						Enabled: "true",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TestStruct{}
			err := ApplyValueOverrides(&got, tt.overrides)

			if (err != nil) != tt.wantErr {
				t.Errorf("ApplyValueOverrides() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.want.Driver.Version != "" && got.Driver.Version != tt.want.Driver.Version {
				t.Errorf("Driver.Version = %v, want %v", got.Driver.Version, tt.want.Driver.Version)
			}
			if tt.want.Driver.Enabled != "" && got.Driver.Enabled != tt.want.Driver.Enabled {
				t.Errorf("Driver.Enabled = %v, want %v", got.Driver.Enabled, tt.want.Driver.Enabled)
			}
			if tt.want.DCGM.Exporter.Version != "" && got.DCGM.Exporter.Version != tt.want.DCGM.Exporter.Version {
				t.Errorf("DCGM.Exporter.Version = %v, want %v", got.DCGM.Exporter.Version, tt.want.DCGM.Exporter.Version)
			}
		})
	}
}

func TestApplyValueOverrides_AcronymFields(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		want      TestStruct
		wantErr   bool
	}{
		{
			name: "set MIG strategy",
			overrides: map[string]string{
				"mig.strategy": "mixed",
			},
			want: TestStruct{
				MIG: struct {
					Strategy string
				}{
					Strategy: "mixed",
				},
			},
		},
		{
			name: "set GPU operator version",
			overrides: map[string]string{
				"gpu-operator.version": "25.3.3",
			},
			want: TestStruct{
				GPUOperator: struct {
					Version string
				}{
					Version: "25.3.3",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TestStruct{}
			err := ApplyValueOverrides(&got, tt.overrides)

			if (err != nil) != tt.wantErr {
				t.Errorf("ApplyValueOverrides() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.want.MIG.Strategy != "" && got.MIG.Strategy != tt.want.MIG.Strategy {
				t.Errorf("MIG.Strategy = %v, want %v", got.MIG.Strategy, tt.want.MIG.Strategy)
			}
			if tt.want.GPUOperator.Version != "" && got.GPUOperator.Version != tt.want.GPUOperator.Version {
				t.Errorf("GPUOperator.Version = %v, want %v", got.GPUOperator.Version, tt.want.GPUOperator.Version)
			}
		})
	}
}

func TestApplyValueOverrides_Errors(t *testing.T) {
	tests := []struct {
		name      string
		target    any
		overrides map[string]string
		wantErr   bool
		errMsg    string
	}{
		{
			name:   "non-pointer target",
			target: TestStruct{},
			overrides: map[string]string{
				"name": "test",
			},
			wantErr: true,
			errMsg:  "must be a pointer",
		},
		{
			name:      "nil overrides",
			target:    &TestStruct{},
			overrides: nil,
			wantErr:   false,
		},
		{
			name:      "empty overrides",
			target:    &TestStruct{},
			overrides: map[string]string{},
			wantErr:   false,
		},
		{
			name:   "non-existent field",
			target: &TestStruct{},
			overrides: map[string]string{
				"nonexistent": "value",
			},
			wantErr: true,
			errMsg:  "field not found",
		},
		{
			name:   "non-existent nested field",
			target: &TestStruct{},
			overrides: map[string]string{
				"driver.nonexistent": "value",
			},
			wantErr: true,
			errMsg:  "field not found",
		},
		{
			name:   "traverse through non-struct field",
			target: &TestStruct{},
			overrides: map[string]string{
				"name.sub": "value",
			},
			wantErr: true,
			errMsg:  "cannot traverse non-struct field",
		},
		{
			name:   "pointer to non-struct",
			target: new(string),
			overrides: map[string]string{
				"anything": "value",
			},
			wantErr: true,
			errMsg:  "must be a pointer to a struct",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ApplyValueOverrides(tt.target, tt.overrides)

			if (err != nil) != tt.wantErr {
				t.Errorf("ApplyValueOverrides() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && tt.errMsg != "" {
				if err == nil || !containsSubstring(err.Error(), tt.errMsg) {
					t.Errorf("Expected error containing %q, got %v", tt.errMsg, err)
				}
			}
		})
	}
}

func TestApplyValueOverrides_CaseInsensitive(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		want      TestStruct
	}{
		{
			name: "lowercase field name",
			overrides: map[string]string{
				"name": "test",
			},
			want: TestStruct{
				Name: "test",
			},
		},
		{
			name: "uppercase field name",
			overrides: map[string]string{
				"NAME": "test",
			},
			want: TestStruct{
				Name: "test",
			},
		},
		{
			name: "mixed case field name",
			overrides: map[string]string{
				"NaMe": "test",
			},
			want: TestStruct{
				Name: "test",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TestStruct{}
			err := ApplyValueOverrides(&got, tt.overrides)

			if err != nil {
				t.Errorf("ApplyValueOverrides() unexpected error = %v", err)
				return
			}

			if got.Name != tt.want.Name {
				t.Errorf("Name = %v, want %v", got.Name, tt.want.Name)
			}
		})
	}
}

// Test with actual GPU Operator-like struct
type GPUOperatorValues struct {
	EnableDriver string
	Driver       struct {
		Version string
		Enabled string
	}
	EnableGDS string
	GDS       struct {
		Enabled string
	}
	GDRCopy struct {
		Enabled string
	}
	MIG struct {
		Strategy string
	}
	DCGM struct {
		Version string
	}
}

func TestApplyValueOverrides_GPUOperatorScenarios(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		verify    func(t *testing.T, values *GPUOperatorValues)
	}{
		{
			name: "gdrcopy enabled override",
			overrides: map[string]string{
				"gdrcopy.enabled": "false",
			},
			verify: func(t *testing.T, values *GPUOperatorValues) {
				if values.GDRCopy.Enabled != "false" {
					t.Errorf("GDRCopy.Enabled = %v, want false", values.GDRCopy.Enabled)
				}
			},
		},
		{
			name: "gds enabled override",
			overrides: map[string]string{
				"gds.enabled": "true",
			},
			verify: func(t *testing.T, values *GPUOperatorValues) {
				// Should match either EnableGDS or GDS.Enabled
				if values.EnableGDS != "true" && values.GDS.Enabled != "true" {
					t.Errorf("GDS not enabled: EnableGDS=%v, GDS.Enabled=%v", values.EnableGDS, values.GDS.Enabled)
				}
			},
		},
		{
			name: "driver version override",
			overrides: map[string]string{
				"driver.version": "570.86.16",
			},
			verify: func(t *testing.T, values *GPUOperatorValues) {
				if values.Driver.Version != "570.86.16" {
					t.Errorf("Driver.Version = %v, want 570.86.16", values.Driver.Version)
				}
			},
		},
		{
			name: "mig strategy override",
			overrides: map[string]string{
				"mig.strategy": "mixed",
			},
			verify: func(t *testing.T, values *GPUOperatorValues) {
				if values.MIG.Strategy != "mixed" {
					t.Errorf("MIG.Strategy = %v, want mixed", values.MIG.Strategy)
				}
			},
		},
		{
			name: "multiple overrides",
			overrides: map[string]string{
				"gdrcopy.enabled": "false",
				"gds.enabled":     "true",
				"driver.version":  "570.86.16",
				"mig.strategy":    "mixed",
			},
			verify: func(t *testing.T, values *GPUOperatorValues) {
				if values.GDRCopy.Enabled != "false" {
					t.Errorf("GDRCopy.Enabled = %v, want false", values.GDRCopy.Enabled)
				}
				if values.Driver.Version != "570.86.16" {
					t.Errorf("Driver.Version = %v, want 570.86.16", values.Driver.Version)
				}
				if values.MIG.Strategy != "mixed" {
					t.Errorf("MIG.Strategy = %v, want mixed", values.MIG.Strategy)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := &GPUOperatorValues{}
			err := ApplyValueOverrides(values, tt.overrides)

			if err != nil {
				t.Fatalf("ApplyValueOverrides() unexpected error = %v", err)
			}

			tt.verify(t, values)
		})
	}
}

// containsSubstring checks if string contains substring (renamed to avoid collision)
func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && containsSubstringHelper(s, substr))
}

func containsSubstringHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestApplyNodeSelectorOverrides(t *testing.T) {
	tests := []struct {
		name         string
		values       map[string]any
		nodeSelector map[string]string
		paths        []string
		verify       func(t *testing.T, values map[string]any)
	}{
		{
			name:   "applies to top-level nodeSelector",
			values: make(map[string]any),
			nodeSelector: map[string]string{
				"nodeGroup": "system-cpu",
			},
			paths: []string{"nodeSelector"},
			verify: func(t *testing.T, values map[string]any) {
				ns, ok := values["nodeSelector"].(map[string]any)
				if !ok {
					t.Fatal("nodeSelector not found or wrong type")
				}
				if ns["nodeGroup"] != "system-cpu" {
					t.Errorf("nodeSelector.nodeGroup = %v, want system-cpu", ns["nodeGroup"])
				}
			},
		},
		{
			name: "applies to nested paths",
			values: map[string]any{
				"webhook": make(map[string]any),
			},
			nodeSelector: map[string]string{
				"role": "control-plane",
			},
			paths: []string{"nodeSelector", "webhook.nodeSelector"},
			verify: func(t *testing.T, values map[string]any) {
				// Check top-level
				ns, ok := values["nodeSelector"].(map[string]any)
				if !ok {
					t.Fatal("nodeSelector not found")
				}
				if ns["role"] != "control-plane" {
					t.Errorf("nodeSelector.role = %v, want control-plane", ns["role"])
				}
				// Check nested
				wh, ok := values["webhook"].(map[string]any)
				if !ok {
					t.Fatal("webhook not found")
				}
				whNs, ok := wh["nodeSelector"].(map[string]any)
				if !ok {
					t.Fatal("webhook.nodeSelector not found")
				}
				if whNs["role"] != "control-plane" {
					t.Errorf("webhook.nodeSelector.role = %v, want control-plane", whNs["role"])
				}
			},
		},
		{
			name:         "empty nodeSelector is no-op",
			values:       make(map[string]any),
			nodeSelector: map[string]string{},
			paths:        []string{"nodeSelector"},
			verify: func(t *testing.T, values map[string]any) {
				if _, ok := values["nodeSelector"]; ok {
					t.Error("nodeSelector should not be set for empty input")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ApplyNodeSelectorOverrides(tt.values, tt.nodeSelector, tt.paths...)
			tt.verify(t, tt.values)
		})
	}
}

func TestApplyTolerationsOverrides(t *testing.T) {
	tests := []struct {
		name        string
		values      map[string]any
		tolerations []corev1.Toleration
		paths       []string
		verify      func(t *testing.T, values map[string]any)
	}{
		{
			name:   "applies single toleration",
			values: make(map[string]any),
			tolerations: []corev1.Toleration{
				{
					Key:      testTolKeyDedicated,
					Value:    "system-workload",
					Operator: corev1.TolerationOpEqual,
					Effect:   corev1.TaintEffectNoSchedule,
				},
			},
			paths: []string{"tolerations"},
			verify: func(t *testing.T, values map[string]any) {
				tols, ok := values["tolerations"].([]any)
				if !ok {
					t.Fatal("tolerations not found or wrong type")
				}
				if len(tols) != 1 {
					t.Fatalf("expected 1 toleration, got %d", len(tols))
				}
				tol, ok := tols[0].(map[string]any)
				if !ok {
					t.Fatal("toleration entry wrong type")
				}
				if tol["key"] != testTolKeyDedicated {
					t.Errorf("key = %v, want dedicated", tol["key"])
				}
				if tol["value"] != "system-workload" {
					t.Errorf("value = %v, want system-workload", tol["value"])
				}
			},
		},
		{
			name: "applies to nested paths",
			values: map[string]any{
				"webhook": make(map[string]any),
			},
			tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			paths: []string{"tolerations", "webhook.tolerations"},
			verify: func(t *testing.T, values map[string]any) {
				// Check top-level
				tols, ok := values["tolerations"].([]any)
				if !ok {
					t.Fatal("tolerations not found")
				}
				if len(tols) != 1 {
					t.Fatalf("expected 1 toleration, got %d", len(tols))
				}
				// Check nested
				wh, ok := values["webhook"].(map[string]any)
				if !ok {
					t.Fatal("webhook not found")
				}
				whTols, ok := wh["tolerations"].([]any)
				if !ok {
					t.Fatal("webhook.tolerations not found")
				}
				if len(whTols) != 1 {
					t.Fatalf("expected 1 webhook toleration, got %d", len(whTols))
				}
			},
		},
		{
			name:        "empty tolerations is no-op",
			values:      make(map[string]any),
			tolerations: []corev1.Toleration{},
			paths:       []string{"tolerations"},
			verify: func(t *testing.T, values map[string]any) {
				if _, ok := values["tolerations"]; ok {
					t.Error("tolerations should not be set for empty input")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ApplyTolerationsOverrides(tt.values, tt.tolerations, tt.paths...)
			tt.verify(t, tt.values)
		})
	}
}

func TestTolerationsToPodSpec(t *testing.T) {
	tests := []struct {
		name        string
		tolerations []corev1.Toleration
		verify      func(t *testing.T, result []map[string]any)
	}{
		{
			name: "converts full toleration",
			tolerations: []corev1.Toleration{
				{
					Key:      testTolKeyDedicated,
					Operator: corev1.TolerationOpEqual,
					Value:    "gpu",
					Effect:   corev1.TaintEffectNoSchedule,
				},
			},
			verify: func(t *testing.T, result []map[string]any) {
				if len(result) != 1 {
					t.Fatalf("expected 1 result, got %d", len(result))
				}
				tol := result[0]
				if tol["key"] != testTolKeyDedicated {
					t.Errorf("key = %v, want dedicated", tol["key"])
				}
				if tol["operator"] != "Equal" {
					t.Errorf("operator = %v, want Equal", tol["operator"])
				}
				if tol["value"] != "gpu" {
					t.Errorf("value = %v, want gpu", tol["value"])
				}
				if tol["effect"] != "NoSchedule" {
					t.Errorf("effect = %v, want NoSchedule", tol["effect"])
				}
			},
		},
		{
			name: "omits empty fields",
			tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			verify: func(t *testing.T, result []map[string]any) {
				if len(result) != 1 {
					t.Fatalf("expected 1 result, got %d", len(result))
				}
				tol := result[0]
				if _, ok := tol["key"]; ok {
					t.Error("key should be omitted when empty")
				}
				if tol["operator"] != "Exists" {
					t.Errorf("operator = %v, want Exists", tol["operator"])
				}
				if _, ok := tol["value"]; ok {
					t.Error("value should be omitted when empty")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TolerationsToPodSpec(tt.tolerations)
			tt.verify(t, result)
		})
	}
}

func TestApplyMapOverrides(t *testing.T) {
	tests := []struct {
		name      string
		target    map[string]any
		overrides map[string]string
		wantErr   bool
		verify    func(t *testing.T, target map[string]any)
	}{
		{
			name:   "sets simple value",
			target: make(map[string]any),
			overrides: map[string]string{
				"key": "value",
			},
			wantErr: false,
			verify: func(t *testing.T, target map[string]any) {
				if target["key"] != "value" {
					t.Errorf("key = %v, want value", target["key"])
				}
			},
		},
		{
			name:   "sets nested value",
			target: make(map[string]any),
			overrides: map[string]string{
				"driver.version": "550.0.0",
			},
			wantErr: false,
			verify: func(t *testing.T, target map[string]any) {
				driver, ok := target["driver"].(map[string]any)
				if !ok {
					t.Fatal("driver not found or wrong type")
				}
				if driver["version"] != "550.0.0" {
					t.Errorf("driver.version = %v, want 550.0.0", driver["version"])
				}
			},
		},
		{
			name:   "sets deeply nested value",
			target: make(map[string]any),
			overrides: map[string]string{
				"dcgm.exporter.config.enabled": "true",
			},
			wantErr: false,
			verify: func(t *testing.T, target map[string]any) {
				dcgm := target["dcgm"].(map[string]any)
				exporter := dcgm["exporter"].(map[string]any)
				config := exporter["config"].(map[string]any)
				if config["enabled"] != true {
					t.Errorf("dcgm.exporter.config.enabled = %v, want true", config["enabled"])
				}
			},
		},
		{
			name: "merges with existing map",
			target: map[string]any{
				"driver": map[string]any{
					"enabled": true,
				},
			},
			overrides: map[string]string{
				"driver.version": "550.0.0",
			},
			wantErr: false,
			verify: func(t *testing.T, target map[string]any) {
				driver := target["driver"].(map[string]any)
				if driver["enabled"] != true {
					t.Error("existing enabled field was lost")
				}
				if driver["version"] != "550.0.0" {
					t.Errorf("driver.version = %v, want 550.0.0", driver["version"])
				}
			},
		},
		{
			name:      "nil target returns error",
			target:    nil,
			overrides: map[string]string{"key": "value"},
			wantErr:   true,
		},
		{
			name:      "empty overrides is no-op",
			target:    make(map[string]any),
			overrides: map[string]string{},
			wantErr:   false,
			verify: func(t *testing.T, target map[string]any) {
				if len(target) != 0 {
					t.Error("expected empty target")
				}
			},
		},
		{
			name: "path segment exists but is not a map",
			target: map[string]any{
				"driver": "string-value",
			},
			overrides: map[string]string{
				"driver.version": "550.0.0",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ApplyMapOverrides(tt.target, tt.overrides)
			if (err != nil) != tt.wantErr {
				t.Errorf("ApplyMapOverrides() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.verify != nil && !tt.wantErr {
				tt.verify(t, tt.target)
			}
		})
	}
}

func TestConvertMapValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  any
	}{
		{
			name:  "converts true",
			input: "true",
			want:  true,
		},
		{
			name:  "converts false",
			input: "false",
			want:  false,
		},
		{
			name:  "converts integer",
			input: "42",
			want:  int64(42),
		},
		{
			name:  "converts negative integer",
			input: "-100",
			want:  int64(-100),
		},
		{
			name:  "converts float",
			input: "3.14",
			want:  3.14,
		},
		{
			name:  "keeps string as string",
			input: "hello",
			want:  "hello",
		},
		{
			name:  "version string stays string",
			input: "v1.2.3",
			want:  "v1.2.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertMapValue(tt.input)
			if got != tt.want {
				t.Errorf("convertMapValue(%q) = %v (%T), want %v (%T)", tt.input, got, got, tt.want, tt.want)
			}
		})
	}
}

func TestParseBool(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    bool
		wantErr bool
	}{
		{name: "true", input: "true", want: true},
		{name: "True", input: "True", want: true},
		{name: "TRUE", input: "TRUE", want: true},
		{name: "yes", input: "yes", want: true},
		{name: "1", input: "1", want: true},
		{name: "on", input: "on", want: true},
		{name: "enabled", input: "enabled", want: true},
		{name: "false", input: "false", want: false},
		{name: "False", input: "False", want: false},
		{name: "FALSE", input: "FALSE", want: false},
		{name: "no", input: "no", want: false},
		{name: "0", input: "0", want: false},
		{name: "off", input: "off", want: false},
		{name: "disabled", input: "disabled", want: false},
		{name: "invalid", input: "maybe", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBool(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseBool(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseBool(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestSetFieldValue tests the setFieldValue function with various types
func TestSetFieldValue(t *testing.T) {
	type testStruct struct {
		StringField  string
		BoolField    bool
		IntField     int
		Int64Field   int64
		UintField    uint
		FloatField   float64
		Float32Field float32
	}

	tests := []struct {
		name      string
		fieldName string
		value     string
		verify    func(t *testing.T, s *testStruct)
		wantErr   bool
	}{
		{
			name:      "sets string field",
			fieldName: "StringField",
			value:     "test-value",
			verify: func(t *testing.T, s *testStruct) {
				if s.StringField != "test-value" {
					t.Errorf("StringField = %v, want test-value", s.StringField)
				}
			},
		},
		{
			name:      "sets bool field true",
			fieldName: "BoolField",
			value:     "true",
			verify: func(t *testing.T, s *testStruct) {
				if !s.BoolField {
					t.Error("BoolField should be true")
				}
			},
		},
		{
			name:      "sets bool field false",
			fieldName: "BoolField",
			value:     "false",
			verify: func(t *testing.T, s *testStruct) {
				if s.BoolField {
					t.Error("BoolField should be false")
				}
			},
		},
		{
			name:      "sets int field",
			fieldName: "IntField",
			value:     "42",
			verify: func(t *testing.T, s *testStruct) {
				if s.IntField != 42 {
					t.Errorf("IntField = %v, want 42", s.IntField)
				}
			},
		},
		{
			name:      "sets int64 field",
			fieldName: "Int64Field",
			value:     "9223372036854775807",
			verify: func(t *testing.T, s *testStruct) {
				if s.Int64Field != 9223372036854775807 {
					t.Errorf("Int64Field = %v, want max int64", s.Int64Field)
				}
			},
		},
		{
			name:      "sets uint field",
			fieldName: "UintField",
			value:     "100",
			verify: func(t *testing.T, s *testStruct) {
				if s.UintField != 100 {
					t.Errorf("UintField = %v, want 100", s.UintField)
				}
			},
		},
		{
			name:      "sets float64 field",
			fieldName: "FloatField",
			value:     "3.14159",
			verify: func(t *testing.T, s *testStruct) {
				if s.FloatField != 3.14159 {
					t.Errorf("FloatField = %v, want 3.14159", s.FloatField)
				}
			},
		},
		{
			name:      "sets float32 field",
			fieldName: "Float32Field",
			value:     "2.5",
			verify: func(t *testing.T, s *testStruct) {
				if s.Float32Field != 2.5 {
					t.Errorf("Float32Field = %v, want 2.5", s.Float32Field)
				}
			},
		},
		{
			name:      "invalid bool value",
			fieldName: "BoolField",
			value:     "not-a-bool",
			wantErr:   true,
		},
		{
			name:      "invalid int value",
			fieldName: "IntField",
			value:     "not-an-int",
			wantErr:   true,
		},
		{
			name:      "invalid uint value",
			fieldName: "UintField",
			value:     "-1",
			wantErr:   true,
		},
		{
			name:      "invalid float value",
			fieldName: "FloatField",
			value:     "not-a-float",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &testStruct{}
			err := ApplyValueOverrides(s, map[string]string{tt.fieldName: tt.value})
			if (err != nil) != tt.wantErr {
				t.Errorf("setFieldValue() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.verify != nil && !tt.wantErr {
				tt.verify(t, s)
			}
		})
	}
}

func TestPathToFieldName(t *testing.T) {
	tests := []struct {
		segment string
		want    string
	}{
		// Known acronyms
		{"gds", "GDS"},
		{"gpu", "GPU"},
		{"mig", "MIG"},
		{"dcgm", "DCGM"},
		{"cpu", "CPU"},
		{"api", "API"},
		{"cdi", "CDI"},
		{"gdr", "GDR"},
		{"rdma", "RDMA"},
		{"sriov", "SRIOV"},
		{"vfio", "VFIO"},
		{"vgpu", "VGPU"},
		{"ofed", "OFED"},
		{"crds", "CRDs"},
		{"rbac", "RBAC"},
		{"tls", "TLS"},
		{"nfd", "NFD"},
		{"gfd", "GFD"},
		// Case-insensitive acronym lookup
		{"GPU", "GPU"},
		{"Gds", "GDS"},
		// Underscore-separated with acronym
		{"mig_strategy", "MIGStrategy"},
		{"gpu_version", "GPUVersion"},
		// Underscore-separated without acronym
		{"node_selector", "NodeSelector"},
		// Dash-separated with acronym
		{"gpu-operator", "GPUOperator"},
		{"rdma-driver", "RDMADriver"},
		// Dash-separated without acronym
		{"network-operator", "NetworkOperator"},
		// Simple title case
		{"enabled", "Enabled"},
		{"version", "Version"},
		{"driver", "Driver"},
		{"strategy", "Strategy"},
	}

	for _, tt := range tests {
		t.Run(tt.segment, func(t *testing.T) {
			got := pathToFieldName(tt.segment)
			if got != tt.want {
				t.Errorf("pathToFieldName(%q) = %q, want %q", tt.segment, got, tt.want)
			}
		})
	}
}

func TestMatchesPattern(t *testing.T) {
	tests := []struct {
		name      string
		fieldName string
		segment   string
		want      bool
	}{
		// Containment
		{"field contains segment", "DriverVersion", "driver", true},
		{"field contains segment mixed case", "GPUOperatorVersion", "operator", true},
		// Enable prefix
		{"enable prefix match", "EnableGDS", "gds", true},
		{"enable prefix no match", "EnableGDS", "mig", false},
		// Starts with
		{"field starts with segment", "DriverVersion", "driverversion", true},
		// Dash-separated
		{"dash-separated match", "GPUOperator", "gpu-operator", true},
		{"dash-separated no match", "GPUOperator", "network-operator", false},
		// Negative cases
		{"no match at all", "EnableGDS", "version", false},
		{"empty segment", "DriverVersion", "", true}, // empty string is contained in anything
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesPattern(tt.fieldName, tt.segment)
			if got != tt.want {
				t.Errorf("matchesPattern(%q, %q) = %v, want %v", tt.fieldName, tt.segment, got, tt.want)
			}
		})
	}
}

func TestDeriveFlatFieldName(t *testing.T) {
	tests := []struct {
		prefix string
		suffix string
		want   string
	}{
		// Special mappings
		{"operator", "version", "GPUOperatorVersion"},
		{"toolkit", "version", "NvidiaContainerToolkitVersion"},
		{"driver", "repository", "DriverRegistry"},
		{"driver", "registry", "DriverRegistry"},
		{"ofed", "version", "OFEDVersion"},
		{"ofed", "deploy", "DeployOFED"},
		{"nic", "type", "NicType"},
		{"containerRuntime", "socket", "ContainerRuntimeSocket"},
		{"hostDevice", "enabled", "EnableHostDevice"},
		{"operator", "registry", "OperatorRegistry"},
		{"kubeRbacProxy", "version", "KubeRbacProxyVersion"},
		{"agent", "image", "SkyhookAgentImage"},
		// Tolerations
		{"tolerations", "key", "TolerationKey"},
		{"tolerations", "value", "TolerationValue"},
		// Sandbox / secure boot
		{"sandboxWorkloads", "enabled", "EnableSecureBoot"},
		// Open kernel module
		{"driver", "useOpenKernelModules", "UseOpenKernelModule"},
		{"driver", "useOpenKernelModule", "UseOpenKernelModule"},
		// Generic enabled suffix
		{"gds", "enabled", "EnableGDS"},
		{"mig", "enabled", "EnableMIG"},
		// Default concatenation
		{"driver", "version", "DriverVersion"},
		{"mig", "strategy", "MIGStrategy"},
	}

	for _, tt := range tests {
		t.Run(tt.prefix+"."+tt.suffix, func(t *testing.T) {
			got := deriveFlatFieldName(tt.prefix, tt.suffix)
			if got != tt.want {
				t.Errorf("deriveFlatFieldName(%q, %q) = %q, want %q", tt.prefix, tt.suffix, got, tt.want)
			}
		})
	}
}

func TestDeriveMultiSegmentFieldName(t *testing.T) {
	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{
			"cpu limit",
			[]string{"manager", "resources", "cpu", "limit"},
			"ManagerCPULimit",
		},
		{
			"memory limit",
			[]string{"manager", "resources", "memory", "limit"},
			"ManagerMemoryLimit",
		},
		{
			"cpu request",
			[]string{"manager", "resources", "cpu", "request"},
			"ManagerCPURequest",
		},
		{
			"memory request",
			[]string{"manager", "resources", "memory", "request"},
			"ManagerMemoryRequest",
		},
		{
			"wrong prefix",
			[]string{"worker", "resources", "cpu", "limit"},
			"",
		},
		{
			"wrong second segment",
			[]string{"manager", "config", "cpu", "limit"},
			"",
		},
		{
			"wrong length - 3 parts",
			[]string{"manager", "resources", "cpu"},
			"",
		},
		{
			"wrong length - 5 parts",
			[]string{"manager", "resources", "cpu", "limit", "extra"},
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveMultiSegmentFieldName(tt.parts)
			if got != tt.want {
				t.Errorf("deriveMultiSegmentFieldName(%v) = %q, want %q", tt.parts, got, tt.want)
			}
		})
	}
}

func TestSetNodeSelectorAtPath(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		nodeSelector map[string]string
		verify       func(t *testing.T, values map[string]any)
	}{
		{
			name: "single-segment path",
			path: "nodeSelector",
			nodeSelector: map[string]string{
				"role": "gpu",
			},
			verify: func(t *testing.T, values map[string]any) {
				ns, ok := values["nodeSelector"].(map[string]any)
				if !ok {
					t.Fatal("nodeSelector not found")
				}
				if ns["role"] != "gpu" {
					t.Errorf("nodeSelector.role = %v, want gpu", ns["role"])
				}
			},
		},
		{
			name: "multi-segment path creates intermediate maps",
			path: "webhook.nodeSelector",
			nodeSelector: map[string]string{
				"zone": "us-east",
			},
			verify: func(t *testing.T, values map[string]any) {
				wh, ok := values["webhook"].(map[string]any)
				if !ok {
					t.Fatal("webhook not found")
				}
				ns, ok := wh["nodeSelector"].(map[string]any)
				if !ok {
					t.Fatal("webhook.nodeSelector not found")
				}
				if ns["zone"] != "us-east" {
					t.Errorf("webhook.nodeSelector.zone = %v, want us-east", ns["zone"])
				}
			},
		},
		{
			name: "deep nesting",
			path: "a.b.c.nodeSelector",
			nodeSelector: map[string]string{
				"key": "val",
			},
			verify: func(t *testing.T, values map[string]any) {
				a, ok := values["a"].(map[string]any)
				if !ok {
					t.Fatal("a not found")
				}
				b, ok := a["b"].(map[string]any)
				if !ok {
					t.Fatal("a.b not found")
				}
				c, ok := b["c"].(map[string]any)
				if !ok {
					t.Fatal("a.b.c not found")
				}
				ns, ok := c["nodeSelector"].(map[string]any)
				if !ok {
					t.Fatal("a.b.c.nodeSelector not found")
				}
				if ns["key"] != "val" {
					t.Errorf("got %v, want val", ns["key"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := make(map[string]any)
			setNodeSelectorAtPath(values, tt.nodeSelector, tt.path)
			tt.verify(t, values)
		})
	}

	t.Run("intermediate path is non-map value", func(t *testing.T) {
		values := map[string]any{
			"webhook": "not-a-map", // string instead of map
		}
		setNodeSelectorAtPath(values, map[string]string{"key": "val"}, "webhook.nodeSelector")
		wh, ok := values["webhook"].(map[string]any)
		if !ok {
			t.Fatal("webhook should have been replaced with a map")
		}
		ns, ok := wh["nodeSelector"].(map[string]any)
		if !ok {
			t.Fatal("webhook.nodeSelector not found")
		}
		if ns["key"] != "val" {
			t.Errorf("got %v, want val", ns["key"])
		}
	})
}

func TestSetTolerationsAtPath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		tolerations []map[string]any
		verify      func(t *testing.T, values map[string]any)
	}{
		{
			name: "single-segment path",
			path: "tolerations",
			tolerations: []map[string]any{
				{"key": testTolKeyDedicated, "operator": "Equal", "value": "gpu", "effect": "NoSchedule"},
			},
			verify: func(t *testing.T, values map[string]any) {
				tols, ok := values["tolerations"].([]any)
				if !ok {
					t.Fatal("tolerations not found or wrong type")
				}
				if len(tols) != 1 {
					t.Fatalf("expected 1 toleration, got %d", len(tols))
				}
				tol, ok := tols[0].(map[string]any)
				if !ok {
					t.Fatal("toleration entry wrong type")
				}
				if tol["key"] != testTolKeyDedicated {
					t.Errorf("key = %v, want dedicated", tol["key"])
				}
			},
		},
		{
			name: "multi-segment path creates intermediate maps",
			path: "webhook.tolerations",
			tolerations: []map[string]any{
				{"operator": "Exists"},
			},
			verify: func(t *testing.T, values map[string]any) {
				wh, ok := values["webhook"].(map[string]any)
				if !ok {
					t.Fatal("webhook not found")
				}
				tols, ok := wh["tolerations"].([]any)
				if !ok {
					t.Fatal("webhook.tolerations not found")
				}
				if len(tols) != 1 {
					t.Fatalf("expected 1 toleration, got %d", len(tols))
				}
			},
		},
		{
			name: "multiple tolerations",
			path: "tolerations",
			tolerations: []map[string]any{
				{"key": "key1", "operator": "Equal", "value": "val1"},
				{"key": "key2", "operator": "Exists"},
			},
			verify: func(t *testing.T, values map[string]any) {
				tols, ok := values["tolerations"].([]any)
				if !ok {
					t.Fatal("tolerations not found or wrong type")
				}
				if len(tols) != 2 {
					t.Fatalf("expected 2 tolerations, got %d", len(tols))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := make(map[string]any)
			setTolerationsAtPath(values, tt.tolerations, tt.path)
			tt.verify(t, values)
		})
	}

	t.Run("intermediate path is non-map value", func(t *testing.T) {
		values := map[string]any{
			"webhook": "not-a-map",
		}
		tols := []map[string]any{{"operator": "Exists"}}
		setTolerationsAtPath(values, tols, "webhook.tolerations")
		wh, ok := values["webhook"].(map[string]any)
		if !ok {
			t.Fatal("webhook should have been replaced with a map")
		}
		result, ok := wh["tolerations"].([]any)
		if !ok {
			t.Fatal("webhook.tolerations not found")
		}
		if len(result) != 1 {
			t.Fatalf("expected 1 toleration, got %d", len(result))
		}
	})
}

func TestNodeSelectorToMatchExpressions(t *testing.T) {
	tests := []struct {
		name         string
		nodeSelector map[string]string
		verify       func(t *testing.T, result []map[string]any)
	}{
		{
			name: "converts single selector",
			nodeSelector: map[string]string{
				"nodeGroup": "gpu-nodes",
			},
			verify: func(t *testing.T, result []map[string]any) {
				if len(result) != 1 {
					t.Fatalf("expected 1 expression, got %d", len(result))
				}
				expr := result[0]
				if expr["key"] != "nodeGroup" {
					t.Errorf("key = %v, want nodeGroup", expr["key"])
				}
				if expr["operator"] != "In" {
					t.Errorf("operator = %v, want In", expr["operator"])
				}
				values, ok := expr["values"].([]string)
				if !ok {
					t.Fatal("values not a []string")
				}
				if len(values) != 1 || values[0] != "gpu-nodes" {
					t.Errorf("values = %v, want [gpu-nodes]", values)
				}
			},
		},
		{
			name: "converts multiple selectors",
			nodeSelector: map[string]string{
				"nodeGroup":   "gpu-nodes",
				"accelerator": "nvidia-h100",
			},
			verify: func(t *testing.T, result []map[string]any) {
				if len(result) != 2 {
					t.Fatalf("expected 2 expressions, got %d", len(result))
				}
				// Check both expressions exist (order may vary due to map iteration)
				foundNodeGroup := false
				foundAccelerator := false
				for _, expr := range result {
					if expr["key"] == "nodeGroup" {
						foundNodeGroup = true
						values := expr["values"].([]string)
						if values[0] != "gpu-nodes" {
							t.Errorf("nodeGroup values = %v, want [gpu-nodes]", values)
						}
					}
					if expr["key"] == "accelerator" {
						foundAccelerator = true
						values := expr["values"].([]string)
						if values[0] != "nvidia-h100" {
							t.Errorf("accelerator values = %v, want [nvidia-h100]", values)
						}
					}
				}
				if !foundNodeGroup {
					t.Error("missing nodeGroup expression")
				}
				if !foundAccelerator {
					t.Error("missing accelerator expression")
				}
			},
		},
		{
			name:         "returns nil for empty selector",
			nodeSelector: map[string]string{},
			verify: func(t *testing.T, result []map[string]any) {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
			},
		},
		{
			name:         "returns nil for nil selector",
			nodeSelector: nil,
			verify: func(t *testing.T, result []map[string]any) {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NodeSelectorToMatchExpressions(tt.nodeSelector)
			tt.verify(t, result)
		})
	}
}

// PointerTestStruct has a pointer-to-struct field for testing nil pointer traversal.
type PointerTestStruct struct {
	Config *PointerSubConfig
}

type PointerSubConfig struct {
	Value string
	Count int
}

// MultiSegmentTestStruct has fields for 4-segment path testing.
type MultiSegmentTestStruct struct {
	ManagerCPULimit      string
	ManagerMemoryLimit   string
	ManagerCPURequest    string
	ManagerMemoryRequest string
}

func TestApplyValueOverrides_PointerToStruct(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		verify    func(t *testing.T, s *PointerTestStruct)
	}{
		{
			name: "sets field through nil pointer to struct",
			overrides: map[string]string{
				"config.value": "initialized",
			},
			verify: func(t *testing.T, s *PointerTestStruct) {
				if s.Config == nil {
					t.Fatal("Config pointer should be initialized")
				}
				if s.Config.Value != "initialized" {
					t.Errorf("Config.Value = %v, want initialized", s.Config.Value)
				}
			},
		},
		{
			name: "sets field through non-nil pointer to struct",
			overrides: map[string]string{
				"config.count": "42",
			},
			verify: func(t *testing.T, s *PointerTestStruct) {
				if s.Config == nil {
					t.Fatal("Config pointer should be initialized")
				}
				if s.Config.Count != 42 {
					t.Errorf("Config.Count = %v, want 42", s.Config.Count)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &PointerTestStruct{}
			err := ApplyValueOverrides(s, tt.overrides)
			if err != nil {
				t.Fatalf("ApplyValueOverrides() unexpected error = %v", err)
			}
			tt.verify(t, s)
		})
	}
}

func TestApplyValueOverrides_NonStructTraversal(t *testing.T) {
	type FlatStruct struct {
		Name string
	}

	s := &FlatStruct{Name: "test"}
	err := ApplyValueOverrides(s, map[string]string{
		"name.sub": "value",
	})
	if err == nil {
		t.Fatal("expected error for non-struct traversal")
	}
	if !containsSubstring(err.Error(), "cannot traverse non-struct field") {
		t.Errorf("expected 'cannot traverse non-struct field' error, got: %v", err)
	}
}

func TestApplyValueOverrides_MultiSegmentPaths(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		verify    func(t *testing.T, s *MultiSegmentTestStruct)
	}{
		{
			name: "manager.resources.cpu.limit",
			overrides: map[string]string{
				"manager.resources.cpu.limit": "4",
			},
			verify: func(t *testing.T, s *MultiSegmentTestStruct) {
				if s.ManagerCPULimit != "4" {
					t.Errorf("ManagerCPULimit = %v, want 4", s.ManagerCPULimit)
				}
			},
		},
		{
			name: "manager.resources.memory.limit",
			overrides: map[string]string{
				"manager.resources.memory.limit": "8Gi",
			},
			verify: func(t *testing.T, s *MultiSegmentTestStruct) {
				if s.ManagerMemoryLimit != "8Gi" {
					t.Errorf("ManagerMemoryLimit = %v, want 8Gi", s.ManagerMemoryLimit)
				}
			},
		},
		{
			name: "manager.resources.cpu.request",
			overrides: map[string]string{
				"manager.resources.cpu.request": "2",
			},
			verify: func(t *testing.T, s *MultiSegmentTestStruct) {
				if s.ManagerCPURequest != "2" {
					t.Errorf("ManagerCPURequest = %v, want 2", s.ManagerCPURequest)
				}
			},
		},
		{
			name: "manager.resources.memory.request",
			overrides: map[string]string{
				"manager.resources.memory.request": "4Gi",
			},
			verify: func(t *testing.T, s *MultiSegmentTestStruct) {
				if s.ManagerMemoryRequest != "4Gi" {
					t.Errorf("ManagerMemoryRequest = %v, want 4Gi", s.ManagerMemoryRequest)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &MultiSegmentTestStruct{}
			err := ApplyValueOverrides(s, tt.overrides)
			if err != nil {
				t.Fatalf("ApplyValueOverrides() unexpected error = %v", err)
			}
			tt.verify(t, s)
		})
	}
}
