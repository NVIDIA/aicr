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

package deployer

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// assertErrorCode verifies err is a *errors.StructuredError with the given code.
// The substring check is secondary — the code is the authoritative contract.
func assertErrorCode(t *testing.T, err error, want errors.ErrorCode, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %s, got nil", want)
	}
	var structErr *errors.StructuredError
	if !stderrors.As(err, &structErr) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}
	if structErr.Code != want {
		t.Errorf("error code = %s, want %s (full error: %v)", structErr.Code, want, err)
	}
	if wantMsg != "" && !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("error message should contain %q, got: %v", wantMsg, err)
	}
}

func TestIsSafePathComponent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid component name", "gpu-operator", true},
		{"valid with dots", "cert-manager.io", true},
		{"dashes and numbers", "test-123", true},
		{"single dot", ".", true},
		{"empty string", "", false},
		{"path traversal", "../etc/passwd", false},
		{"double dot", "..", false},
		{"forward slash", "gpu/operator", false},
		{"backslash", "gpu\\operator", false},
		{"embedded double dot", "foo..bar", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSafePathComponent(tt.input); got != tt.want {
				t.Errorf("IsSafePathComponent(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSafeJoin(t *testing.T) {
	baseDir := t.TempDir()

	tests := []struct {
		name    string
		dir     string
		input   string
		wantErr bool
	}{
		{"valid component", baseDir, "gpu-operator", false},
		{"valid with dots", baseDir, "cert-manager", false},
		{"path traversal", baseDir, "../etc/passwd", true},
		{"double dot", baseDir, "..", true},
		{"absolute path rejected", baseDir, "/etc/passwd", true},
		{"empty name", baseDir, "", false},
		{"relative base", ".", "gpu-operator", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := SafeJoin(tt.dir, tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("SafeJoin(%q, %q) error = %v, wantErr %v", tt.dir, tt.input, err, tt.wantErr)
				return
			}
			if err == nil && result == "" {
				t.Errorf("SafeJoin(%q, %q) returned empty path", tt.dir, tt.input)
			}
		})
	}
}

func TestOutput_AddDataFiles(t *testing.T) {
	t.Run("empty input leaves Output unchanged", func(t *testing.T) {
		o := &Output{Files: []string{"existing"}, TotalSize: 10}
		if err := o.AddDataFiles(t.TempDir(), nil); err != nil {
			t.Fatalf("AddDataFiles(nil) error = %v", err)
		}
		if len(o.Files) != 1 || o.TotalSize != 10 {
			t.Errorf("empty input mutated output: files=%v size=%d", o.Files, o.TotalSize)
		}
	})

	t.Run("appends absolute paths and sums sizes", func(t *testing.T) {
		tmpDir := t.TempDir()
		absTmp, err := filepath.Abs(tmpDir)
		if err != nil {
			t.Fatalf("failed to resolve tmpDir: %v", err)
		}
		dataFiles := []string{"data/a.yaml", "data/nested/b.yaml"}
		contents := map[string]string{
			"data/a.yaml":        "aaa",
			"data/nested/b.yaml": "bbbbbb",
		}
		var expectedBytes int64
		for rel, c := range contents {
			full := filepath.Join(tmpDir, rel)
			if mkErr := os.MkdirAll(filepath.Dir(full), 0755); mkErr != nil {
				t.Fatalf("mkdir: %v", mkErr)
			}
			if wErr := os.WriteFile(full, []byte(c), 0600); wErr != nil {
				t.Fatalf("write: %v", wErr)
			}
			expectedBytes += int64(len(c))
		}

		o := &Output{TotalSize: 5}
		if err := o.AddDataFiles(tmpDir, dataFiles); err != nil {
			t.Fatalf("AddDataFiles() error = %v", err)
		}
		if len(o.Files) != len(dataFiles) {
			t.Fatalf("got %d Files, want %d", len(o.Files), len(dataFiles))
		}
		if o.TotalSize != 5+expectedBytes {
			t.Errorf("TotalSize = %d, want %d", o.TotalSize, 5+expectedBytes)
		}
		for i, p := range o.Files {
			if !strings.HasPrefix(p, absTmp+string(filepath.Separator)) {
				t.Errorf("path %q should be under %q", p, absTmp)
			}
			if !strings.HasSuffix(p, dataFiles[i]) {
				t.Errorf("path %q should end with %q", p, dataFiles[i])
			}
		}
	})

	t.Run("rejects path traversal without mutating output", func(t *testing.T) {
		o := &Output{Files: []string{"existing"}, TotalSize: 10}
		err := o.AddDataFiles(t.TempDir(), []string{"../../../etc/passwd"})
		assertErrorCode(t, err, errors.ErrCodeInvalidRequest, "escapes base directory")
		if len(o.Files) != 1 || o.Files[0] != "existing" || o.TotalSize != 10 {
			t.Errorf("output mutated on traversal error: files=%v size=%d", o.Files, o.TotalSize)
		}
	})

	t.Run("rejects absolute path", func(t *testing.T) {
		o := &Output{Files: []string{"existing"}, TotalSize: 10}
		err := o.AddDataFiles(t.TempDir(), []string{"/etc/passwd"})
		assertErrorCode(t, err, errors.ErrCodeInvalidRequest, "escapes base directory")
		if len(o.Files) != 1 || o.Files[0] != "existing" || o.TotalSize != 10 {
			t.Errorf("output mutated on absolute-path error: files=%v size=%d", o.Files, o.TotalSize)
		}
	})

	t.Run("errors when file missing", func(t *testing.T) {
		o := &Output{Files: []string{"existing"}, TotalSize: 10}
		err := o.AddDataFiles(t.TempDir(), []string{"data/does-not-exist.yaml"})
		assertErrorCode(t, err, errors.ErrCodeInternal, "failed to stat data file")
		if len(o.Files) != 1 || o.Files[0] != "existing" || o.TotalSize != 10 {
			t.Errorf("output mutated on stat error: files=%v size=%d", o.Files, o.TotalSize)
		}
	})

	t.Run("rejects nil receiver", func(t *testing.T) {
		var o *Output
		err := o.AddDataFiles(t.TempDir(), []string{"data/a.yaml"})
		assertErrorCode(t, err, errors.ErrCodeInvalidRequest, "output is required")
	})
}

func TestWriteValuesFile(t *testing.T) {
	t.Run("writes values", func(t *testing.T) {
		dir := t.TempDir()
		values := map[string]any{
			"key": "value",
			"nested": map[string]any{
				"inner": true,
			},
		}

		path, size, err := WriteValuesFile(values, dir, "values.yaml")
		if err != nil {
			t.Fatalf("WriteValuesFile() error = %v", err)
		}
		if size == 0 {
			t.Error("WriteValuesFile() size = 0")
		}

		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}

		s := string(content)
		if !strings.Contains(s, "Generated by AICR") {
			t.Error("missing header comment")
		}
		if !strings.Contains(s, "key: value") {
			t.Error("missing key: value")
		}
	})

	t.Run("empty values", func(t *testing.T) {
		dir := t.TempDir()

		path, size, err := WriteValuesFile(nil, dir, "empty.yaml")
		if err != nil {
			t.Fatalf("WriteValuesFile() error = %v", err)
		}
		if size == 0 {
			t.Error("WriteValuesFile() size = 0")
		}

		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if !strings.Contains(string(content), "---") {
			t.Error("missing YAML separator")
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		dir := t.TempDir()
		_, _, err := WriteValuesFile(nil, dir, "../escape.yaml")
		if err == nil {
			t.Error("expected error for path traversal")
		}
	})
}

func TestGenerateFromTemplate(t *testing.T) {
	t.Run("renders template", func(t *testing.T) {
		dir := t.TempDir()
		tmpl := "Hello {{.Name}}!"
		data := struct{ Name string }{Name: "World"}

		path, size, err := GenerateFromTemplate(tmpl, data, dir, "output.txt")
		if err != nil {
			t.Fatalf("GenerateFromTemplate() error = %v", err)
		}
		if size == 0 {
			t.Error("GenerateFromTemplate() size = 0")
		}

		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if string(content) != "Hello World!" {
			t.Errorf("got %q, want %q", string(content), "Hello World!")
		}
	})

	t.Run("invalid template", func(t *testing.T) {
		dir := t.TempDir()
		_, _, err := GenerateFromTemplate("{{.Invalid", nil, dir, "bad.txt")
		if err == nil {
			t.Error("expected error for invalid template")
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		dir := t.TempDir()
		_, _, err := GenerateFromTemplate("ok", nil, dir, "../escape.txt")
		if err == nil {
			t.Error("expected error for path traversal")
		}
	})

	t.Run("output in correct directory", func(t *testing.T) {
		dir := t.TempDir()
		path, _, err := GenerateFromTemplate("test", nil, dir, "sub.txt")
		if err != nil {
			t.Fatalf("GenerateFromTemplate() error = %v", err)
		}

		absDir, _ := filepath.Abs(dir)
		if !strings.HasPrefix(path, absDir) {
			t.Errorf("output path %q not under base dir %q", path, absDir)
		}
	})
}

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"v1.0.0", "1.0.0"},
		{"1.0.0", "1.0.0"},
		{"v25.3.3", "25.3.3"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := NormalizeVersion(tt.input)
			if result != tt.expected {
				t.Errorf("NormalizeVersion(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestNormalizeVersionWithDefault(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"v1.0.0", "1.0.0"},
		{"1.0.0", "1.0.0"},
		{"v0.1.0-alpha", "0.1.0-alpha"},
		{"", "0.1.0"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := NormalizeVersionWithDefault(tt.input)
			if result != tt.expected {
				t.Errorf("NormalizeVersionWithDefault(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestSortComponentRefsByDeploymentOrder(t *testing.T) {
	tests := []struct {
		name     string
		refs     []recipe.ComponentRef
		order    []string
		expected []string
	}{
		{
			name: "ordered",
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator"},
				{Name: "cert-manager"},
				{Name: "network-operator"},
			},
			order:    []string{"cert-manager", "gpu-operator", "network-operator"},
			expected: []string{"cert-manager", "gpu-operator", "network-operator"},
		},
		{
			name: "empty order preserves input",
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator"},
				{Name: "cert-manager"},
			},
			order:    []string{},
			expected: []string{"gpu-operator", "cert-manager"},
		},
		{
			name: "partial order",
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator"},
				{Name: "cert-manager"},
				{Name: "network-operator"},
			},
			order:    []string{"cert-manager"},
			expected: []string{"cert-manager", "gpu-operator", "network-operator"},
		},
		{
			name: "component not in order goes last",
			refs: []recipe.ComponentRef{
				{Name: "unknown"},
				{Name: "gpu-operator"},
			},
			order:    []string{"gpu-operator"},
			expected: []string{"gpu-operator", "unknown"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SortComponentRefsByDeploymentOrder(tt.refs, tt.order)
			if len(result) != len(tt.expected) {
				t.Fatalf("expected %d components, got %d", len(tt.expected), len(result))
			}
			for i, name := range tt.expected {
				if result[i].Name != name {
					t.Errorf("position %d: expected %s, got %s", i, name, result[i].Name)
				}
			}
		})
	}
}

func TestSortComponentNamesByDeploymentOrder(t *testing.T) {
	tests := []struct {
		name     string
		comps    []string
		order    []string
		expected []string
	}{
		{
			name:     "all in order map",
			comps:    []string{"gpu-operator", "cert-manager", "network-operator"},
			order:    []string{"cert-manager", "gpu-operator", "network-operator"},
			expected: []string{"cert-manager", "gpu-operator", "network-operator"},
		},
		{
			name:     "only one in order map",
			comps:    []string{"alpha", "gpu-operator"},
			order:    []string{"gpu-operator"},
			expected: []string{"gpu-operator", "alpha"},
		},
		{
			name:     "neither in order map",
			comps:    []string{"zebra", "alpha"},
			order:    []string{"gpu-operator"},
			expected: []string{"alpha", "zebra"},
		},
		{
			name:     "empty deployment order",
			comps:    []string{"b", "a"},
			order:    nil,
			expected: []string{"b", "a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SortComponentNamesByDeploymentOrder(tt.comps, tt.order)
			if len(result) != len(tt.expected) {
				t.Fatalf("expected %d components, got %d", len(tt.expected), len(result))
			}
			for i, name := range tt.expected {
				if result[i] != name {
					t.Errorf("position %d: expected %s, got %s", i, name, result[i])
				}
			}
		})
	}
}
