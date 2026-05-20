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

package helm

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWriteValuesFile(t *testing.T) {
	values := map[string]any{
		"driver": map[string]any{
			"version": "570.86.16",
			"enabled": true,
		},
		"toolkit": map[string]any{
			"enabled": true,
		},
	}

	path, err := writeValuesFile(values)
	if err != nil {
		t.Fatalf("writeValuesFile() error = %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read temp file: %v", err)
	}

	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("temp file is not valid YAML: %v", err)
	}

	driver, ok := parsed["driver"].(map[string]any)
	if !ok {
		t.Fatal("expected driver key in parsed values")
	}
	if v, ok := driver["version"].(string); !ok || v != "570.86.16" {
		t.Errorf("driver.version = %v, want 570.86.16", driver["version"])
	}
}

func TestWriteValuesFileEmpty(t *testing.T) {
	path, err := writeValuesFile(map[string]any{})
	if err != nil {
		t.Fatalf("writeValuesFile() error = %v", err)
	}
	defer os.Remove(path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat temp file: %v", err)
	}
	if info.Size() == 0 {
		t.Error("expected non-empty file for empty map (YAML produces '{}')")
	}
}

func TestLastPathSegment(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"nvidia/gpu-operator", "gpu-operator"},
		{"gpu-operator", "gpu-operator"},
		{"a/b/c", "c"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := lastPathSegment(tt.input); got != tt.want {
				t.Errorf("lastPathSegment(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestChartInputOCIArgs(t *testing.T) {
	// Verify that an OCI chart produces a full URL as the chart arg
	// and no --repo flag. We can't run helm template without a real
	// chart, but we can verify the input mapping via lastPathSegment.
	input := ChartInput{
		Name:       "gpu-operator",
		Chart:      "gpu-operator",
		Repository: "oci://ghcr.io/nvidia",
		Version:    "v25.3.0",
		Namespace:  "gpu-operator",
	}

	// OCI: chart arg = repo/chart
	expected := "oci://ghcr.io/nvidia/gpu-operator"
	got := input.Repository + "/" + lastPathSegment(input.Chart)
	if got != expected {
		t.Errorf("OCI chart URL = %q, want %q", got, expected)
	}
}

func TestChartInputHTTPArgs(t *testing.T) {
	input := ChartInput{
		Name:       "aws-ebs-csi-driver",
		Chart:      "aws-ebs-csi-driver",
		Repository: "https://kubernetes-sigs.github.io/aws-ebs-csi-driver",
		Version:    "2.40.0",
	}

	// HTTP: chart arg = bare chart name, repo goes to --repo flag
	got := lastPathSegment(input.Chart)
	if got != "aws-ebs-csi-driver" {
		t.Errorf("HTTP chart name = %q, want %q", got, "aws-ebs-csi-driver")
	}
}

func TestRenderChartNoChart(t *testing.T) {
	_, err := RenderChart(t.Context(), ChartInput{
		Name: "test",
	})
	if err == nil {
		t.Fatal("expected error for empty chart, got nil")
	}
}
