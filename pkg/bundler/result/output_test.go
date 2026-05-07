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

package result

import (
	"errors"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/types"
)

// TestOutput_New tests Output initialization
func TestOutput_New(t *testing.T) {
	outputDir := "/output/bundles"
	output := &Output{
		OutputDir: outputDir,
		Results:   []*Result{},
		Errors:    []BundleError{},
	}

	if output.OutputDir != outputDir {
		t.Errorf("OutputDir = %s, want %s", output.OutputDir, outputDir)
	}

	if output.Results == nil {
		t.Error("Results slice should not be nil")
	}

	if output.Errors == nil {
		t.Error("Errors slice should not be nil")
	}

	if output.TotalSize != 0 {
		t.Errorf("TotalSize = %d, want 0", output.TotalSize)
	}

	if output.TotalFiles != 0 {
		t.Errorf("TotalFiles = %d, want 0", output.TotalFiles)
	}

	if output.TotalDuration != 0 {
		t.Errorf("TotalDuration = %v, want 0", output.TotalDuration)
	}
}

// TestOutput_HasErrors tests error detection
func TestOutput_HasErrors(t *testing.T) {
	tests := []struct {
		name   string
		errors []BundleError
		want   bool
	}{
		{
			name:   "no errors",
			errors: []BundleError{},
			want:   false,
		},
		{
			name: "single error",
			errors: []BundleError{
				{BundlerType: types.BundleType("gpu-operator"), Error: "failed"},
			},
			want: true,
		},
		{
			name: "multiple errors",
			errors: []BundleError{
				{BundlerType: types.BundleType("gpu-operator"), Error: "error 1"},
				{BundlerType: types.BundleType("network-operator"), Error: "error 2"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := &Output{
				Errors: tt.errors,
			}

			got := output.HasErrors()
			if got != tt.want {
				t.Errorf("HasErrors() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBundleError tests BundleError struct
func TestBundleError(t *testing.T) {
	bundlerType := types.BundleType("gpu-operator")
	errorMsg := "failed to generate bundle"

	bundleErr := BundleError{
		BundlerType: bundlerType,
		Error:       errorMsg,
	}

	if bundleErr.BundlerType != bundlerType {
		t.Errorf("BundlerType = %s, want %s", bundleErr.BundlerType, bundlerType)
	}

	if bundleErr.Error != errorMsg {
		t.Errorf("Error = %s, want %s", bundleErr.Error, errorMsg)
	}
}

// TestOutput_CompleteWorkflow tests a complete output workflow
func TestOutput_CompleteWorkflow(t *testing.T) {
	result1 := New(types.BundleType("gpu-operator"))
	result1.AddFile("/output/gpu/values.yaml", 1024)
	result1.AddFile("/output/gpu/manifest.yaml", 2048)
	result1.Duration = 3 * time.Second
	result1.MarkSuccess()

	result2 := New(types.BundleType("network-operator"))
	result2.AddFile("/output/network/values.yaml", 512)
	result2.AddError(errors.New("template error"))
	result2.Duration = 2 * time.Second

	result3 := New(types.BundleType("custom"))
	result3.AddFile("/output/custom/file1.yaml", 256)
	result3.AddFile("/output/custom/file2.yaml", 128)
	result3.Duration = 1 * time.Second
	result3.MarkSuccess()

	output := &Output{
		OutputDir:     "/output/bundles",
		Results:       []*Result{result1, result2, result3},
		TotalSize:     1024 + 2048 + 512 + 256 + 128,
		TotalFiles:    5,
		TotalDuration: 6 * time.Second,
		Errors: []BundleError{
			{BundlerType: types.BundleType("network-operator"), Error: "template error"},
		},
	}

	if !output.HasErrors() {
		t.Error("Output should have errors")
	}

	if len(output.Results) != 3 {
		t.Errorf("Results length = %d, want 3", len(output.Results))
	}

	if output.TotalFiles != 5 {
		t.Errorf("TotalFiles = %d, want 5", output.TotalFiles)
	}
}

// TestOutput_NilResults tests output with nil results slice
func TestOutput_NilResults(t *testing.T) {
	output := &Output{
		Results: nil,
	}

	if output.HasErrors() {
		t.Error("Output with nil results should not have errors")
	}
}
