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

package component

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/types"
)

func TestNewBaseBundler(t *testing.T) {
	tests := []struct {
		name        string
		config      *config.Config
		bundlerType types.BundleType
		wantNilCfg  bool
	}{
		{
			name:        "with config",
			config:      config.NewConfig(),
			bundlerType: types.BundleType("gpu-operator"),
			wantNilCfg:  false,
		},
		{
			name:        "nil config creates default",
			config:      nil,
			bundlerType: types.BundleType("gpu-operator"),
			wantNilCfg:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBaseBundler(tt.config, tt.bundlerType)

			if b == nil {
				t.Fatal("NewBaseBundler() returned nil")
			}

			if b.Config == nil {
				t.Error("NewBaseBundler() Config is nil")
			}

			if b.Result == nil {
				t.Error("NewBaseBundler() Result is nil")
			}

			if b.Result.Type != tt.bundlerType {
				t.Errorf("Result.Type = %v, want %v", b.Result.Type, tt.bundlerType)
			}
		})
	}
}

func TestBaseBundler_CreateBundleDir(t *testing.T) {
	tmpDir := t.TempDir()
	b := NewBaseBundler(nil, types.BundleType("gpu-operator"))

	dirs, err := b.CreateBundleDir(tmpDir, "test-bundle")
	if err != nil {
		t.Fatalf("CreateBundleDir() error = %v", err)
	}

	// Verify only the root directory is created (subdirectories created on-demand)
	if _, err := os.Stat(dirs.Root); os.IsNotExist(err) {
		t.Errorf("Root directory %s was not created", dirs.Root)
	}

	// Verify subdirectories are NOT created yet (created on-demand when writing files)
	for _, dir := range []string{dirs.Scripts, dirs.Manifests} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("Subdirectory %s should not exist yet (created on-demand)", dir)
		}
	}

	// Verify directory paths are correct
	if dirs.Root != filepath.Join(tmpDir, "test-bundle") {
		t.Errorf("Root dir = %s, want %s", dirs.Root, filepath.Join(tmpDir, "test-bundle"))
	}

	if dirs.Scripts != filepath.Join(dirs.Root, "scripts") {
		t.Errorf("Scripts dir = %s, want %s", dirs.Scripts, filepath.Join(dirs.Root, "scripts"))
	}

	if dirs.Manifests != filepath.Join(dirs.Root, "manifests") {
		t.Errorf("Manifests dir = %s, want %s", dirs.Manifests, filepath.Join(dirs.Root, "manifests"))
	}
}

func TestBaseBundler_WriteFile(t *testing.T) {
	tmpDir := t.TempDir()
	b := NewBaseBundler(nil, types.BundleType("gpu-operator"))

	testFile := filepath.Join(tmpDir, "test.txt")
	content := []byte("test content")

	err := b.WriteFile(testFile, content, 0644)
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// Verify file was created
	if _, statErr := os.Stat(testFile); os.IsNotExist(statErr) {
		t.Error("File was not created")
	}

	// Verify content
	readContent, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	if string(readContent) != string(content) {
		t.Errorf("File content = %s, want %s", readContent, content)
	}

	// Verify result was updated
	if len(b.Result.Files) != 1 {
		t.Errorf("Result.Files length = %d, want 1", len(b.Result.Files))
	}

	if b.Result.Size != int64(len(content)) {
		t.Errorf("Result.Size = %d, want %d", b.Result.Size, len(content))
	}
}

func TestBaseBundler_WriteFileString(t *testing.T) {
	tmpDir := t.TempDir()
	b := NewBaseBundler(nil, types.BundleType("gpu-operator"))

	testFile := filepath.Join(tmpDir, "test.txt")
	content := "test string content"

	err := b.WriteFileString(testFile, content, 0644)
	if err != nil {
		t.Fatalf("WriteFileString() error = %v", err)
	}

	// Verify file was created with correct content
	readContent, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	if string(readContent) != content {
		t.Errorf("File content = %s, want %s", readContent, content)
	}
}

func TestBaseBundler_RenderTemplate(t *testing.T) {
	b := NewBaseBundler(nil, types.BundleType("gpu-operator"))

	tests := []struct {
		name    string
		tmpl    string
		data    any
		want    string
		wantErr bool
	}{
		{
			name: "simple template",
			tmpl: "Hello, {{.Name}}!",
			data: map[string]string{"Name": "World"},
			want: "Hello, World!",
		},
		{
			name: "template with iteration",
			tmpl: "{{range .Items}}{{.}} {{end}}",
			data: map[string][]string{"Items": {"a", "b", "c"}},
			want: "a b c ",
		},
		{
			name:    "invalid template",
			tmpl:    "{{.Invalid",
			data:    map[string]string{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := b.RenderTemplate(tt.tmpl, tt.name, tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("RenderTemplate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && got != tt.want {
				t.Errorf("RenderTemplate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBaseBundler_RenderAndWriteTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	b := NewBaseBundler(nil, types.BundleType("gpu-operator"))

	tmpl := "Hello, {{.Name}}!"
	data := map[string]string{"Name": "World"}
	outputPath := filepath.Join(tmpDir, "output.txt")

	err := b.RenderAndWriteTemplate(tmpl, "test", outputPath, data, 0644)
	if err != nil {
		t.Fatalf("RenderAndWriteTemplate() error = %v", err)
	}

	// Verify file content
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	want := "Hello, World!"
	if string(content) != want {
		t.Errorf("File content = %s, want %s", content, want)
	}
}

func TestBaseBundler_GenerateChecksums(t *testing.T) {
	tmpDir := t.TempDir()
	b := NewBaseBundler(nil, types.BundleType("gpu-operator"))
	ctx := context.Background()

	// Create bundle directory
	bundleDir := filepath.Join(tmpDir, "test-bundle")
	if err := os.MkdirAll(bundleDir, 0755); err != nil {
		t.Fatalf("Failed to create bundle dir: %v", err)
	}

	// Write some test files
	testFiles := []struct {
		name    string
		content string
	}{
		{"file1.txt", "content1"},
		{"file2.txt", "content2"},
	}

	for _, tf := range testFiles {
		path := filepath.Join(bundleDir, tf.name)
		if err := b.WriteFileString(path, tf.content, 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}
	}

	// Generate checksums
	err := b.GenerateChecksums(ctx, bundleDir)
	if err != nil {
		t.Fatalf("GenerateChecksums() error = %v", err)
	}

	// Verify checksums file exists
	checksumPath := filepath.Join(bundleDir, "checksums.txt")
	if _, statErr := os.Stat(checksumPath); os.IsNotExist(statErr) {
		t.Error("Checksums file was not created")
	}

	// Verify checksums content
	content, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatalf("Failed to read checksums: %v", err)
	}

	contentStr := string(content)
	for _, tf := range testFiles {
		if !filepath.IsAbs(tf.name) && !strings.Contains(contentStr, tf.name) {
			t.Errorf("Checksums file does not contain %s", tf.name)
		}
	}
}

func TestBaseBundler_GenerateChecksums_ContextCancelled(t *testing.T) {
	tmpDir := t.TempDir()
	b := NewBaseBundler(nil, types.BundleType("gpu-operator"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := b.GenerateChecksums(ctx, tmpDir)
	if err == nil {
		t.Error("GenerateChecksums() should return error for cancelled context")
	}
}

func TestBaseBundler_MakeExecutable(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T) string
		wantErr   bool
		wantExec  bool
		wantResEr bool
	}{
		{
			name: "valid file",
			setup: func(t *testing.T) string {
				t.Helper()
				f := filepath.Join(t.TempDir(), "script.sh")
				if err := os.WriteFile(f, []byte("#!/bin/bash\necho test"), 0644); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return f
			},
			wantExec: true,
		},
		{
			name: "nonexistent file",
			setup: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "does-not-exist.sh")
			},
			wantErr:   true,
			wantResEr: true,
		},
		{
			name: "nonexistent directory",
			setup: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "no-such-dir", "script.sh")
			},
			wantErr:   true,
			wantResEr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBaseBundler(nil, types.BundleType("gpu-operator"))
			path := tt.setup(t)

			err := b.MakeExecutable(path)
			if (err != nil) != tt.wantErr {
				t.Errorf("MakeExecutable() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantResEr && len(b.Result.Errors) == 0 {
				t.Error("expected error in Result.Errors")
			}

			if tt.wantExec {
				info, statErr := os.Stat(path)
				if statErr != nil {
					t.Fatalf("stat: %v", statErr)
				}
				if info.Mode()&0111 == 0 {
					t.Error("file is not executable")
				}
			}
		})
	}
}

func TestBaseBundler_Finalize(t *testing.T) {
	b := NewBaseBundler(nil, types.BundleType("gpu-operator"))

	// Add some files to result
	b.Result.AddFile("/tmp/file1.txt", 100)
	b.Result.AddFile("/tmp/file2.txt", 200)

	start := time.Now()
	time.Sleep(10 * time.Millisecond) // Ensure some duration
	b.Finalize(start)

	if !b.Result.Success {
		t.Error("Result.Success should be true after Finalize()")
	}

	if b.Result.Duration == 0 {
		t.Error("Result.Duration should be set after Finalize()")
	}

	if b.Result.Duration < 10*time.Millisecond {
		t.Error("Result.Duration should be at least 10ms")
	}
}

func TestBaseBundler_CheckContext(t *testing.T) {
	b := NewBaseBundler(nil, types.BundleType("gpu-operator"))

	tests := []struct {
		name    string
		ctx     context.Context
		wantErr bool
	}{
		{
			name:    "active context",
			ctx:     context.Background(),
			wantErr: false,
		},
		{
			name: "cancelled context",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			}(),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := b.CheckContext(tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckContext() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBaseBundler_AddError(t *testing.T) {
	b := NewBaseBundler(nil, types.BundleType("gpu-operator"))

	// Add nil error - should not panic
	b.AddError(nil)
	if len(b.Result.Errors) != 0 {
		t.Error("AddError(nil) should not add to errors")
	}

	// Add real error
	testErr := os.ErrNotExist
	b.AddError(testErr)

	if len(b.Result.Errors) != 1 {
		t.Errorf("Result.Errors length = %d, want 1", len(b.Result.Errors))
	}

	if b.Result.Errors[0] != testErr.Error() {
		t.Errorf("Result.Errors[0] = %s, want %s", b.Result.Errors[0], testErr.Error())
	}
}

func TestBaseBundler_BuildBaseConfigMap(t *testing.T) {
	cfg := config.NewConfig(
		config.WithVersion("v1.2.3"),
	)

	b := NewBaseBundler(cfg, types.BundleType("gpu-operator"))
	configMap := b.BuildBaseConfigMap()

	// Test bundler version is set
	if configMap["bundler_version"] != "v1.2.3" {
		t.Errorf("bundler_version = %s, want v1.2.3", configMap["bundler_version"])
	}
}

func TestBaseBundler_GenerateFileFromTemplate(t *testing.T) {
	templates := map[string]string{
		"test.yaml": "name: {{.Name}}\nvalue: {{.Value}}",
	}

	getTemplate := func(name string) (string, bool) {
		tmpl, ok := templates[name]
		return tmpl, ok
	}

	tmpDir := t.TempDir()
	b := NewBaseBundler(nil, types.BundleType("gpu-operator"))

	tests := []struct {
		name         string
		ctx          context.Context
		templateName string
		outputPath   string
		data         any
		wantErr      bool
		wantContent  string
	}{
		{
			name:         "successful generation",
			ctx:          context.Background(),
			templateName: "test.yaml",
			outputPath:   filepath.Join(tmpDir, "test1.yaml"),
			data: map[string]any{
				"Name":  "TestName",
				"Value": "TestValue",
			},
			wantErr:     false,
			wantContent: "name: TestName\nvalue: TestValue",
		},
		{
			name:         "template not found",
			ctx:          context.Background(),
			templateName: "missing.yaml",
			outputPath:   filepath.Join(tmpDir, "test2.yaml"),
			data:         map[string]any{},
			wantErr:      true,
		},
		{
			name: "cancelled context",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			}(),
			templateName: "test.yaml",
			outputPath:   filepath.Join(tmpDir, "test3.yaml"),
			data:         map[string]any{},
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := b.GenerateFileFromTemplate(tt.ctx, getTemplate, tt.templateName,
				tt.outputPath, tt.data, 0644)

			if (err != nil) != tt.wantErr {
				t.Errorf("GenerateFileFromTemplate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && tt.wantContent != "" {
				content, err := os.ReadFile(tt.outputPath)
				if err != nil {
					t.Fatalf("Failed to read generated file: %v", err)
				}
				if string(content) != tt.wantContent {
					t.Errorf("Generated content = %s, want %s", string(content), tt.wantContent)
				}
			}
		})
	}
}
func TestGetBundlerVersion(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]string
		want   string
	}{
		{
			name:   "version exists",
			config: map[string]string{bundlerVersionKey: "v1.2.3"},
			want:   "v1.2.3",
		},
		{
			name:   "version missing",
			config: map[string]string{},
			want:   "unknown",
		},
		{
			name:   "empty version",
			config: map[string]string{bundlerVersionKey: ""},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetBundlerVersion(tt.config)
			if got != tt.want {
				t.Errorf("GetBundlerVersion() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetRecipeBundlerVersion(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]string
		want   string
	}{
		{
			name:   "version exists",
			config: map[string]string{recipeBundlerVersionKey: "v2.0.0"},
			want:   "v2.0.0",
		},
		{
			name:   "version missing",
			config: map[string]string{},
			want:   "unknown",
		},
		{
			name:   "with other keys",
			config: map[string]string{"other": "value", recipeBundlerVersionKey: "v1.0.0"},
			want:   "v1.0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetRecipeBundlerVersion(tt.config)
			if got != tt.want {
				t.Errorf("GetRecipeBundlerVersion() = %v, want %v", got, tt.want)
			}
		})
	}
}

// mockRecipeInput implements the interface for BuildConfigMapFromInput.
type mockRecipeInput struct {
	version string
}

func (m *mockRecipeInput) GetVersion() string {
	return m.version
}

func TestBaseBundler_BuildConfigMapFromInput(t *testing.T) {
	cfg := config.NewConfig(
		config.WithVersion("bundler-v1.2.3"),
	)

	b := NewBaseBundler(cfg, types.BundleType("gpu-operator"))

	tests := []struct {
		name    string
		input   *mockRecipeInput
		wantKey string
		wantVal string
	}{
		{
			name:    "includes recipe version",
			input:   &mockRecipeInput{version: "recipe-v1.0.0"},
			wantKey: recipeBundlerVersionKey,
			wantVal: "recipe-v1.0.0",
		},
		{
			name:    "includes bundler version",
			input:   &mockRecipeInput{version: "v1.0.0"},
			wantKey: bundlerVersionKey,
			wantVal: "bundler-v1.2.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := b.BuildConfigMapFromInput(tt.input)
			if got[tt.wantKey] != tt.wantVal {
				t.Errorf("BuildConfigMapFromInput()[%s] = %v, want %v", tt.wantKey, got[tt.wantKey], tt.wantVal)
			}
		})
	}
}
