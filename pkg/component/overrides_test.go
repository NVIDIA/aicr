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

package component

import (
	"fmt"
	"strings"
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

func TestApplyMapOverrides(t *testing.T) {
	tests := []struct {
		name       string
		target     map[string]any
		overrides  map[string]string
		wantErr    bool
		wantErrMsg string
		verify     func(t *testing.T, got map[string]any)
	}{
		{
			name:      "flat override with version string",
			target:    map[string]any{"driver": map[string]any{"version": "570.86.16"}},
			overrides: map[string]string{"driver.version": "580.86.16"},
			verify: func(t *testing.T, got map[string]any) {
				driver := got["driver"].(map[string]any)
				if driver["version"] != "580.86.16" {
					t.Errorf("driver.version = %v (%T), want \"580.86.16\"", driver["version"], driver["version"])
				}
			},
		},
		{
			name:      "creates intermediate maps",
			target:    map[string]any{},
			overrides: map[string]string{"a.b.c": "value"},
			verify: func(t *testing.T, got map[string]any) {
				a, ok := got["a"].(map[string]any)
				if !ok {
					t.Fatalf("a is not a map: %T", got["a"])
				}
				b, ok := a["b"].(map[string]any)
				if !ok {
					t.Fatalf("a.b is not a map: %T", a["b"])
				}
				if b["c"] != "value" {
					t.Errorf("a.b.c = %v, want \"value\"", b["c"])
				}
			},
		},
		{
			name:      "type conversion",
			target:    map[string]any{},
			overrides: map[string]string{"b": "true", "i": "42", "f": "3.14", "s": "hello"},
			verify: func(t *testing.T, got map[string]any) {
				if got["b"] != true {
					t.Errorf("b = %v (%T), want true", got["b"], got["b"])
				}
				if got["i"] != int64(42) {
					t.Errorf("i = %v (%T), want int64(42)", got["i"], got["i"])
				}
				if got["f"] != 3.14 {
					t.Errorf("f = %v (%T), want 3.14", got["f"], got["f"])
				}
				if got["s"] != "hello" {
					t.Errorf("s = %v, want \"hello\"", got["s"])
				}
			},
		},
		{
			name:      "multiple overrides on distinct paths",
			target:    map[string]any{},
			overrides: map[string]string{"driver.version": "v580", "mig.strategy": "mixed"},
			verify: func(t *testing.T, got map[string]any) {
				if got["driver"].(map[string]any)["version"] != "v580" {
					t.Errorf("driver.version = %v", got["driver"])
				}
				if got["mig"].(map[string]any)["strategy"] != "mixed" {
					t.Errorf("mig.strategy = %v", got["mig"])
				}
			},
		},
		{
			name:      "empty overrides is a no-op",
			target:    map[string]any{"keep": "me"},
			overrides: map[string]string{},
			verify: func(t *testing.T, got map[string]any) {
				if got["keep"] != "me" {
					t.Errorf("existing key mutated: %v", got)
				}
				if len(got) != 1 {
					t.Errorf("map gained keys: %v", got)
				}
			},
		},
		{
			name:      "nil overrides is a no-op",
			target:    map[string]any{"keep": "me"},
			overrides: nil,
			verify: func(t *testing.T, got map[string]any) {
				if len(got) != 1 || got["keep"] != "me" {
					t.Errorf("unexpected mutation: %v", got)
				}
			},
		},
		{
			name:       "nil target rejected",
			target:     nil,
			overrides:  map[string]string{"a": "b"},
			wantErr:    true,
			wantErrMsg: "target map cannot be nil",
		},
		{
			name: "strict mode rejects non-map intermediate segment",
			target: map[string]any{
				"driver": "not-a-map",
			},
			overrides:  map[string]string{"driver.version": "580"},
			wantErr:    true,
			wantErrMsg: "exists but is not a map",
		},
		{
			name: "error accumulates across overrides",
			target: map[string]any{
				"driver": "not-a-map",
				"mig":    "also-not-a-map",
			},
			overrides:  map[string]string{"driver.version": "580", "mig.strategy": "mixed"},
			wantErr:    true,
			wantErrMsg: "failed to apply map overrides",
		},
		{
			name:      "single-segment path sets top-level key",
			target:    map[string]any{},
			overrides: map[string]string{"enabled": "true"},
			verify: func(t *testing.T, got map[string]any) {
				if got["enabled"] != true {
					t.Errorf("enabled = %v, want true", got["enabled"])
				}
			},
		},
		{
			name:      "overwrites existing non-map leaf",
			target:    map[string]any{"count": int64(1)},
			overrides: map[string]string{"count": "42"},
			verify: func(t *testing.T, got map[string]any) {
				if got["count"] != int64(42) {
					t.Errorf("count = %v (%T), want int64(42)", got["count"], got["count"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ApplyMapOverrides(tt.target, tt.overrides)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ApplyMapOverrides() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrMsg)
				}
				return
			}
			if tt.verify != nil {
				tt.verify(t, tt.target)
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

func Test_nodeSelectorToMatchExpressions(t *testing.T) {
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
			result := nodeSelectorToMatchExpressions(tt.nodeSelector)
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

func TestGetValueByPath(t *testing.T) {
	tests := []struct {
		name   string
		target map[string]any
		path   string
		want   any
		found  bool
	}{
		{
			name:   "top-level key",
			target: map[string]any{"clusterName": "prod"},
			path:   "clusterName",
			want:   "prod",
			found:  true,
		},
		{
			name: "nested key",
			target: map[string]any{
				"driver": map[string]any{
					"version": "580.105.08",
				},
			},
			path:  "driver.version",
			want:  "580.105.08",
			found: true,
		},
		{
			name: "deeply nested key",
			target: map[string]any{
				"network": map[string]any{
					"subnet": map[string]any{
						"id": "subnet-123",
					},
				},
			},
			path:  "network.subnet.id",
			want:  "subnet-123",
			found: true,
		},
		{
			name:   "missing top-level key",
			target: map[string]any{"other": "value"},
			path:   "missing",
			want:   nil,
			found:  false,
		},
		{
			name: "missing intermediate key",
			target: map[string]any{
				"driver": map[string]any{
					"version": "580",
				},
			},
			path:  "driver.missing.field",
			want:  nil,
			found: false,
		},
		{
			name: "intermediate is not a map",
			target: map[string]any{
				"driver": "scalar-value",
			},
			path:  "driver.version",
			want:  nil,
			found: false,
		},
		{
			name: "value is a map",
			target: map[string]any{
				"driver": map[string]any{
					"config": map[string]any{"a": "b"},
				},
			},
			path:  "driver.config",
			want:  map[string]any{"a": "b"},
			found: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := GetValueByPath(tt.target, tt.path)
			if found != tt.found {
				t.Errorf("GetValueByPath() found = %v, want %v", found, tt.found)
			}
			if !tt.found {
				return
			}
			// Use fmt.Sprintf for comparison to handle map types
			if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", tt.want) {
				t.Errorf("GetValueByPath() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRemoveValueByPath(t *testing.T) {
	tests := []struct {
		name    string
		target  map[string]any
		path    string
		removed bool
		verify  func(t *testing.T, m map[string]any)
	}{
		{
			name:    "remove top-level key",
			target:  map[string]any{"clusterName": "prod", "other": "keep"},
			path:    "clusterName",
			removed: true,
			verify: func(t *testing.T, m map[string]any) {
				if _, ok := m["clusterName"]; ok {
					t.Error("clusterName should have been removed")
				}
				if m["other"] != "keep" {
					t.Error("other key should still exist")
				}
			},
		},
		{
			name: "remove nested key",
			target: map[string]any{
				"driver": map[string]any{
					"version":  "580",
					"registry": "nvcr.io",
				},
			},
			path:    "driver.version",
			removed: true,
			verify: func(t *testing.T, m map[string]any) {
				driver := m["driver"].(map[string]any)
				if _, ok := driver["version"]; ok {
					t.Error("driver.version should have been removed")
				}
				if driver["registry"] != "nvcr.io" {
					t.Error("driver.registry should still exist")
				}
			},
		},
		{
			name:    "missing top-level key",
			target:  map[string]any{"other": "value"},
			path:    "missing",
			removed: false,
			verify:  func(t *testing.T, m map[string]any) {},
		},
		{
			name: "missing intermediate key",
			target: map[string]any{
				"driver": map[string]any{"version": "580"},
			},
			path:    "missing.version",
			removed: false,
			verify:  func(t *testing.T, m map[string]any) {},
		},
		{
			name: "intermediate is not a map",
			target: map[string]any{
				"driver": "scalar",
			},
			path:    "driver.version",
			removed: false,
			verify:  func(t *testing.T, m map[string]any) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			removed := RemoveValueByPath(tt.target, tt.path)
			if removed != tt.removed {
				t.Errorf("RemoveValueByPath() = %v, want %v", removed, tt.removed)
			}
			tt.verify(t, tt.target)
		})
	}
}

// TestSetValueByPath verifies setting values at dot-notation paths,
// including creating intermediate maps and overwriting existing values.
func TestSetValueByPath(t *testing.T) {
	tests := []struct {
		name   string
		target map[string]any
		path   string
		value  any
		verify func(t *testing.T, m map[string]any)
	}{
		{
			name:   "set top-level key",
			target: map[string]any{},
			path:   "clusterName",
			value:  "prod",
			verify: func(t *testing.T, m map[string]any) {
				if m["clusterName"] != "prod" {
					t.Errorf("got %v, want prod", m["clusterName"])
				}
			},
		},
		{
			name:   "creates intermediate maps",
			target: map[string]any{},
			path:   "driver.version",
			value:  "580",
			verify: func(t *testing.T, m map[string]any) {
				driver, ok := m["driver"].(map[string]any)
				if !ok {
					t.Fatal("driver should be a map")
				}
				if driver["version"] != "580" {
					t.Errorf("got %v, want 580", driver["version"])
				}
			},
		},
		{
			name:   "deeply nested path",
			target: map[string]any{},
			path:   "network.subnet.id",
			value:  "subnet-123",
			verify: func(t *testing.T, m map[string]any) {
				val, found := GetValueByPath(m, "network.subnet.id")
				if !found || val != "subnet-123" {
					t.Errorf("got %v (found=%v), want subnet-123", val, found)
				}
			},
		},
		{
			name:   "overwrites existing value",
			target: map[string]any{"driver": map[string]any{"version": "old"}},
			path:   "driver.version",
			value:  "new",
			verify: func(t *testing.T, m map[string]any) {
				if m["driver"].(map[string]any)["version"] != "new" {
					t.Error("should overwrite existing value")
				}
			},
		},
		{
			name:   "preserves sibling keys",
			target: map[string]any{"driver": map[string]any{"version": "580", "registry": "nvcr.io"}},
			path:   "driver.version",
			value:  "",
			verify: func(t *testing.T, m map[string]any) {
				driver := m["driver"].(map[string]any)
				if driver["version"] != "" {
					t.Errorf("version should be empty, got %v", driver["version"])
				}
				if driver["registry"] != "nvcr.io" {
					t.Error("registry should be preserved")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetValueByPath(tt.target, tt.path, tt.value)
			tt.verify(t, tt.target)
		})
	}
}
