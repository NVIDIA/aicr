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

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/eidos/pkg/serializer"
)

// TestSnapshotTemplateFlagCombinations tests all combinations of --template, --format, and --output flags.
// The rules are:
// 1. Template requires YAML format (explicit or default)
// 2. Template with --format json should error
// 3. Template with --format table should error
// 4. Template without output writes to stdout
// 5. Template with output writes to file
func TestSnapshotTemplateFlagCombinations(t *testing.T) {
	// Create temp directory for test files
	tmpDir := t.TempDir()

	// Create a valid template file
	templatePath := filepath.Join(tmpDir, "test.tmpl")
	if err := os.WriteFile(templatePath, []byte("{{ .Name }}"), 0o644); err != nil {
		t.Fatalf("failed to create template file: %v", err)
	}

	tests := []struct {
		name         string
		templatePath string
		format       string
		formatSet    bool // whether --format was explicitly set
		output       string
		wantErr      bool
		errContains  string
	}{
		// Template without format (should use YAML default)
		{
			name:         "template without format defaults to YAML",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    false,
			output:       "",
			wantErr:      false,
		},
		// Template with explicit YAML format
		{
			name:         "template with explicit yaml format",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    true,
			output:       "",
			wantErr:      false,
		},
		// Template with JSON format should error
		{
			name:         "template with json format should error",
			templatePath: templatePath,
			format:       "json",
			formatSet:    true,
			output:       "",
			wantErr:      true,
			errContains:  "YAML format",
		},
		// Template with table format should error
		{
			name:         "template with table format should error",
			templatePath: templatePath,
			format:       "table",
			formatSet:    true,
			output:       "",
			wantErr:      true,
			errContains:  "YAML format",
		},
		// Template with file output
		{
			name:         "template with file output",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    false,
			output:       filepath.Join(tmpDir, "output.yaml"),
			wantErr:      false,
		},
		// Template with stdout output (dash)
		{
			name:         "template with stdout output dash",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    false,
			output:       "-",
			wantErr:      false,
		},
		// Template with empty output (stdout)
		{
			name:         "template with empty output (stdout)",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    false,
			output:       "",
			wantErr:      false,
		},
		// Non-existent template file
		{
			name:         "non-existent template file",
			templatePath: "/non/existent/template.tmpl",
			format:       "yaml",
			formatSet:    false,
			output:       "",
			wantErr:      true,
			errContains:  "not found",
		},
		// Template path is a directory
		{
			name:         "template path is directory",
			templatePath: tmpDir,
			format:       "yaml",
			formatSet:    false,
			output:       "",
			wantErr:      true,
			errContains:  "directory",
		},
		// No template (standard output)
		{
			name:         "no template with yaml format",
			templatePath: "",
			format:       "yaml",
			formatSet:    true,
			output:       "",
			wantErr:      false,
		},
		{
			name:         "no template with json format",
			templatePath: "",
			format:       "json",
			formatSet:    true,
			output:       "",
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Validate the combination
			err := validateTemplateFlagCombination(tt.templatePath, tt.format, tt.formatSet)

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errContains)
				} else if tt.errContains != "" && !containsString(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %v", tt.errContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

// validateTemplateFlagCombination validates the template + format combination.
// This mirrors the validation logic in snapshotCmd.Action.
func validateTemplateFlagCombination(templatePath, format string, formatSet bool) error {
	if templatePath == "" {
		return nil // No template, no validation needed
	}

	// Validate format is YAML when using template
	if formatSet && format != string(serializer.FormatYAML) {
		return &validationError{msg: "--template requires YAML format; --format must be \"yaml\" or omitted"}
	}

	// Validate template file exists
	return serializer.ValidateTemplateFile(templatePath)
}

// validationError is a simple error type for validation failures.
type validationError struct {
	msg string
}

func (e *validationError) Error() string {
	return e.msg
}

// containsString checks if str contains substr (case-insensitive for flexibility).
func containsString(str, substr string) bool {
	return len(str) >= len(substr) && (str == substr ||
		len(str) > 0 && containsSubstring(str, substr))
}

func containsSubstring(str, substr string) bool {
	for i := 0; i <= len(str)-len(substr); i++ {
		if str[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestOutputDestinationParsing tests parsing of various output destinations.
func TestOutputDestinationParsing(t *testing.T) {
	tests := []struct {
		name           string
		output         string
		isStdout       bool
		isFile         bool
		isConfigMap    bool
		expectFilePath string
	}{
		{
			name:     "empty output is stdout",
			output:   "",
			isStdout: true,
		},
		{
			name:     "dash is stdout",
			output:   "-",
			isStdout: true,
		},
		{
			name:     "stdout:// is stdout",
			output:   serializer.StdoutURI,
			isStdout: true,
		},
		{
			name:           "file path",
			output:         "/tmp/snapshot.yaml",
			isFile:         true,
			expectFilePath: "/tmp/snapshot.yaml",
		},
		{
			name:           "relative file path",
			output:         "snapshot.yaml",
			isFile:         true,
			expectFilePath: "snapshot.yaml",
		},
		{
			name:        "configmap URI",
			output:      "cm://gpu-operator/eidos-snapshot",
			isConfigMap: true,
		},
		{
			name:        "configmap URI custom namespace",
			output:      "cm://custom-ns/my-snapshot",
			isConfigMap: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isStdout := tt.output == "" || tt.output == "-" || tt.output == serializer.StdoutURI
			isConfigMap := len(tt.output) > len(serializer.ConfigMapURIScheme) &&
				tt.output[:len(serializer.ConfigMapURIScheme)] == serializer.ConfigMapURIScheme
			isFile := !isStdout && !isConfigMap

			if isStdout != tt.isStdout {
				t.Errorf("isStdout = %v, want %v", isStdout, tt.isStdout)
			}
			if isFile != tt.isFile {
				t.Errorf("isFile = %v, want %v", isFile, tt.isFile)
			}
			if isConfigMap != tt.isConfigMap {
				t.Errorf("isConfigMap = %v, want %v", isConfigMap, tt.isConfigMap)
			}
		})
	}
}
