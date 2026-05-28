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

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
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
		// Template + ConfigMap URI output must be rejected: the template
		// writer only emits to local files, so a cm:// path would silently
		// create a file literally named "cm:..." instead of writing to K8s.
		{
			name:         "template with ConfigMap URI output is rejected",
			templatePath: templatePath,
			format:       "yaml",
			formatSet:    false,
			output:       "cm://aicr/snap",
			wantErr:      true,
			errContains:  "ConfigMap",
		},
		{
			name:         "no template with ConfigMap URI output is allowed",
			templatePath: "",
			format:       "yaml",
			formatSet:    false,
			output:       "cm://aicr/snap",
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Exercise the real production function rather than a hand-copied
			// mirror — the prior helper drifted from the actual validation
			// rules during the ConfigMap-rejection addition.
			cmd := buildSnapshotCmdForTemplateTest(t, tt.templatePath, tt.format, tt.formatSet, tt.output)
			outFormat := serializer.Format(tt.format)
			_, err := parseSnapshotTemplateOptions(cmd, outFormat, &config.SnapshotResolved{})

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errContains)
				} else if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
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

// buildSnapshotCmdForTemplateTest constructs a parsed *cli.Command with
// --template / --format / --output set so parseSnapshotTemplateOptions can be
// exercised in isolation. formatSet=false omits --format so the test exercises
// the cmd.IsSet("format") branch.
func buildSnapshotCmdForTemplateTest(t *testing.T, templatePath, format string, formatSet bool, output string) *cli.Command {
	t.Helper()
	cmd := snapshotCmd()
	app := &cli.Command{Name: "aicr", Commands: []*cli.Command{cmd}}

	args := []string{"aicr", "snapshot"}
	if templatePath != "" {
		args = append(args, "--template", templatePath)
	}
	if formatSet {
		args = append(args, "--format", format)
	}
	if output != "" {
		args = append(args, "--output", output)
	}

	var captured *cli.Command
	cmd.Action = func(_ context.Context, c *cli.Command) error {
		captured = c
		return nil
	}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("flag parse setup failed: %v", err)
	}
	if captured == nil {
		t.Fatal("flag parse setup did not capture cmd")
	}
	return captured
}

// TestParseResourceList covers the --requests / --limits parser, including
// the duplicate-key rejection added per PR #762 review.
func TestParseResourceList(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantNil     bool
		wantKeys    map[corev1.ResourceName]string
		wantErr     bool
		wantErrSubs string
	}{
		{
			name:    "empty input -> nil ResourceList (no override)",
			input:   "",
			wantNil: true,
		},
		{
			name:    "whitespace only -> nil",
			input:   "   ",
			wantNil: true,
		},
		{
			name:  "single entry",
			input: "memory=1Gi",
			wantKeys: map[corev1.ResourceName]string{
				corev1.ResourceMemory: "1Gi",
			},
		},
		{
			name:  "multiple entries with whitespace tolerated",
			input: " cpu=500m , memory=1Gi , ephemeral-storage=2Gi ",
			wantKeys: map[corev1.ResourceName]string{
				corev1.ResourceCPU:              "500m",
				corev1.ResourceMemory:           "1Gi",
				corev1.ResourceEphemeralStorage: "2Gi",
			},
		},
		{
			name:  "extended resource (nvidia.com/gpu)",
			input: "nvidia.com/gpu=4",
			wantKeys: map[corev1.ResourceName]string{
				corev1.ResourceName("nvidia.com/gpu"): "4",
			},
		},
		{
			name:        "missing equals -> error",
			input:       "cpu",
			wantErr:     true,
			wantErrSubs: "name=quantity",
		},
		{
			name:        "empty key -> error",
			input:       "=1Gi",
			wantErr:     true,
			wantErrSubs: "empty",
		},
		{
			name:        "empty value -> error",
			input:       "memory=",
			wantErr:     true,
			wantErrSubs: "empty",
		},
		{
			name:        "invalid quantity -> error",
			input:       "memory=not-a-quantity",
			wantErr:     true,
			wantErrSubs: "memory=not-a-quantity",
		},
		{
			name:        "negative quantity rejected (cpu)",
			input:       "cpu=-1",
			wantErr:     true,
			wantErrSubs: "negative quantity",
		},
		{
			name:        "negative quantity rejected (memory with suffix)",
			input:       "memory=-1Gi",
			wantErr:     true,
			wantErrSubs: "negative quantity",
		},
		{
			name:        "negative quantity in second entry rejected",
			input:       "cpu=1,memory=-256Mi",
			wantErr:     true,
			wantErrSubs: "negative quantity",
		},
		{
			name:  "zero quantity allowed",
			input: "cpu=0",
			wantKeys: map[corev1.ResourceName]string{
				corev1.ResourceCPU: "0",
			},
		},
		{
			name:        "duplicate key rejected",
			input:       "cpu=1,cpu=2",
			wantErr:     true,
			wantErrSubs: "duplicate key",
		},
		{
			name:        "duplicate key after whitespace normalization rejected",
			input:       "memory=1Gi, memory =2Gi",
			wantErr:     true,
			wantErrSubs: "duplicate key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := snapshotter.ParseResourceList(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.wantErrSubs != "" && !strings.Contains(err.Error(), tt.wantErrSubs) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSubs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil ResourceList, got %v", got)
				}
				return
			}
			if len(got) != len(tt.wantKeys) {
				t.Fatalf("got %d keys, want %d (got=%v want=%v)", len(got), len(tt.wantKeys), got, tt.wantKeys)
			}
			for k, want := range tt.wantKeys {
				v, ok := got[k]
				if !ok {
					t.Errorf("missing key %q", k)
					continue
				}
				if v.String() != want {
					t.Errorf("key %q: got %q, want %q", k, v.String(), want)
				}
			}
		})
	}
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
			output:      "cm://gpu-operator/aicr-snapshot",
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
