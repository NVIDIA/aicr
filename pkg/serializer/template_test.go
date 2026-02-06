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

package serializer

import (
	"bytes"
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/eidos/pkg/errors"
)

// hasErrorCode checks if an error contains a StructuredError with the given code.
func hasErrorCode(err error, code errors.ErrorCode) bool {
	var structuredErr *errors.StructuredError
	if stderrors.As(err, &structuredErr) {
		return structuredErr.Code == code
	}
	return false
}

func TestTemplateWriter_BasicExecution(t *testing.T) {
	// Create temp template file
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "test.tmpl")
	templateContent := `Name: {{ .Name }}
Count: {{ .Count }}`
	if err := os.WriteFile(templatePath, []byte(templateContent), 0o644); err != nil {
		t.Fatalf("failed to write template file: %v", err)
	}

	// Create writer with buffer output
	var buf bytes.Buffer
	writer := NewTemplateWriter(templatePath, &buf)

	// Test data
	data := struct {
		Name  string
		Count int
	}{
		Name:  "test-snapshot",
		Count: 42,
	}

	// Execute
	ctx := context.Background()
	if err := writer.Serialize(ctx, data); err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}

	// Verify output
	expected := `Name: test-snapshot
Count: 42`
	if buf.String() != expected {
		t.Errorf("unexpected output:\ngot:  %q\nwant: %q", buf.String(), expected)
	}
}

func TestTemplateWriter_SprigFunctions(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "sprig.tmpl")
	// Test various sprig functions
	templateContent := `Upper: {{ .Name | upper }}
Default: {{ .Missing | default "fallback" }}
Join: {{ .Items | join "," }}`
	if err := os.WriteFile(templatePath, []byte(templateContent), 0o644); err != nil {
		t.Fatalf("failed to write template file: %v", err)
	}

	var buf bytes.Buffer
	writer := NewTemplateWriter(templatePath, &buf)

	data := struct {
		Name    string
		Missing string
		Items   []string
	}{
		Name:    "hello",
		Missing: "",
		Items:   []string{"a", "b", "c"},
	}

	ctx := context.Background()
	if err := writer.Serialize(ctx, data); err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}

	expected := `Upper: HELLO
Default: fallback
Join: a,b,c`
	if buf.String() != expected {
		t.Errorf("unexpected output:\ngot:  %q\nwant: %q", buf.String(), expected)
	}
}

func TestTemplateWriter_FileNotFound(t *testing.T) {
	var buf bytes.Buffer
	writer := NewTemplateWriter("/nonexistent/path/template.tmpl", &buf)

	ctx := context.Background()
	err := writer.Serialize(ctx, struct{}{})
	if err == nil {
		t.Fatal("expected error for nonexistent template file")
	}

	if !hasErrorCode(err, errors.ErrCodeNotFound) {
		t.Errorf("expected ErrCodeNotFound, got: %v", err)
	}
}

func TestTemplateWriter_InvalidSyntax(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "invalid.tmpl")
	// Invalid template syntax (unclosed action)
	templateContent := `{{ .Name`
	if err := os.WriteFile(templatePath, []byte(templateContent), 0o644); err != nil {
		t.Fatalf("failed to write template file: %v", err)
	}

	var buf bytes.Buffer
	writer := NewTemplateWriter(templatePath, &buf)

	ctx := context.Background()
	err := writer.Serialize(ctx, struct{ Name string }{Name: "test"})
	if err == nil {
		t.Fatal("expected error for invalid template syntax")
	}

	if !hasErrorCode(err, errors.ErrCodeInvalidRequest) {
		t.Errorf("expected ErrCodeInvalidRequest, got: %v", err)
	}
}

func TestTemplateWriter_ExecutionError(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "exec_error.tmpl")
	// Template that will fail during execution (calling method on nil)
	templateContent := `{{ .NonExistent.Method }}`
	if err := os.WriteFile(templatePath, []byte(templateContent), 0o644); err != nil {
		t.Fatalf("failed to write template file: %v", err)
	}

	var buf bytes.Buffer
	writer := NewTemplateWriter(templatePath, &buf)

	ctx := context.Background()
	err := writer.Serialize(ctx, struct{}{})
	if err == nil {
		t.Fatal("expected error for template execution failure")
	}

	if !hasErrorCode(err, errors.ErrCodeInternal) {
		t.Errorf("expected ErrCodeInternal, got: %v", err)
	}
}

func TestTemplateWriter_OutputToFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create template
	templatePath := filepath.Join(tmpDir, "template.tmpl")
	templateContent := `Hello {{ .Name }}`
	if err := os.WriteFile(templatePath, []byte(templateContent), 0o644); err != nil {
		t.Fatalf("failed to write template file: %v", err)
	}

	// Create output file path
	outputPath := filepath.Join(tmpDir, "output.txt")

	writer, err := NewTemplateFileWriter(templatePath, outputPath)
	if err != nil {
		t.Fatalf("NewTemplateFileWriter failed: %v", err)
	}
	defer writer.Close()

	data := struct{ Name string }{Name: "World"}
	ctx := context.Background()
	if serializeErr := writer.Serialize(ctx, data); serializeErr != nil {
		t.Fatalf("Serialize failed: %v", serializeErr)
	}

	// Close to flush
	if closeErr := writer.Close(); closeErr != nil {
		t.Fatalf("Close failed: %v", closeErr)
	}

	// Read and verify
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	expected := "Hello World"
	if string(content) != expected {
		t.Errorf("unexpected file content:\ngot:  %q\nwant: %q", string(content), expected)
	}
}

func TestTemplateWriter_OutputToStdout(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "template.tmpl")
	templateContent := `Test`
	if err := os.WriteFile(templatePath, []byte(templateContent), 0o644); err != nil {
		t.Fatalf("failed to write template file: %v", err)
	}

	tests := []struct {
		name       string
		outputPath string
	}{
		{"empty path", ""},
		{"dash", "-"},
		{"stdout uri", StdoutURI},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer, err := NewTemplateFileWriter(templatePath, tt.outputPath)
			if err != nil {
				t.Fatalf("NewTemplateFileWriter failed: %v", err)
			}
			defer writer.Close()

			// Verify it's stdout (closer should be nil)
			if writer.closer != nil {
				t.Error("expected stdout writer to have nil closer")
			}
		})
	}
}

func TestValidateTemplateFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a valid template file
	validPath := filepath.Join(tmpDir, "valid.tmpl")
	if err := os.WriteFile(validPath, []byte("{{ .Name }}"), 0o644); err != nil {
		t.Fatalf("failed to write valid template: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
		errCode errors.ErrorCode
	}{
		{
			name:    "valid file",
			path:    validPath,
			wantErr: false,
		},
		{
			name:    "nonexistent file",
			path:    filepath.Join(tmpDir, "nonexistent.tmpl"),
			wantErr: true,
			errCode: errors.ErrCodeNotFound,
		},
		{
			name:    "directory instead of file",
			path:    tmpDir,
			wantErr: true,
			errCode: errors.ErrCodeInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTemplateFile(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateTemplateFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && !hasErrorCode(err, tt.errCode) {
				t.Errorf("ValidateTemplateFile() error code = %v, want %v", err, tt.errCode)
			}
		})
	}
}

func TestExecuteTemplateToBytes(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "template.tmpl")
	templateContent := `Name: {{ .Name }}, Count: {{ .Count }}`
	if err := os.WriteFile(templatePath, []byte(templateContent), 0o644); err != nil {
		t.Fatalf("failed to write template file: %v", err)
	}

	data := struct {
		Name  string
		Count int
	}{
		Name:  "test",
		Count: 5,
	}

	ctx := context.Background()
	result, err := ExecuteTemplateToBytes(ctx, templatePath, data)
	if err != nil {
		t.Fatalf("ExecuteTemplateToBytes failed: %v", err)
	}

	expected := "Name: test, Count: 5"
	if string(result) != expected {
		t.Errorf("unexpected result:\ngot:  %q\nwant: %q", string(result), expected)
	}
}

func TestTemplateWriter_ContextCanceled(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "template.tmpl")
	if err := os.WriteFile(templatePath, []byte("{{ .Name }}"), 0o644); err != nil {
		t.Fatalf("failed to write template file: %v", err)
	}

	var buf bytes.Buffer
	writer := NewTemplateWriter(templatePath, &buf)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := writer.Serialize(ctx, struct{ Name string }{Name: "test"})
	if err == nil {
		t.Fatal("expected error for canceled context")
	}

	if !hasErrorCode(err, errors.ErrCodeTimeout) {
		t.Errorf("expected ErrCodeTimeout, got: %v", err)
	}
}

func TestTemplateWriter_ComplexNestedData(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "nested.tmpl")
	templateContent := `{{- range .Measurements }}
Type: {{ .Type }}
{{- range .Subtypes }}
  Subtype: {{ .Name }}
{{- end }}
{{- end }}`
	if err := os.WriteFile(templatePath, []byte(templateContent), 0o644); err != nil {
		t.Fatalf("failed to write template file: %v", err)
	}

	var buf bytes.Buffer
	writer := NewTemplateWriter(templatePath, &buf)

	// Simulate snapshot-like structure
	type Subtype struct {
		Name string
	}
	type Measurement struct {
		Type     string
		Subtypes []Subtype
	}
	data := struct {
		Measurements []Measurement
	}{
		Measurements: []Measurement{
			{
				Type: "GPU",
				Subtypes: []Subtype{
					{Name: "driver"},
					{Name: "devices"},
				},
			},
			{
				Type: "K8s",
				Subtypes: []Subtype{
					{Name: "version"},
				},
			},
		},
	}

	ctx := context.Background()
	if err := writer.Serialize(ctx, data); err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}

	// Verify structure is preserved
	output := buf.String()
	if !strings.Contains(output, "Type: GPU") {
		t.Errorf("output missing GPU type: %s", output)
	}
	if !strings.Contains(output, "Subtype: driver") {
		t.Errorf("output missing driver subtype: %s", output)
	}
	if !strings.Contains(output, "Type: K8s") {
		t.Errorf("output missing K8s type: %s", output)
	}
}

func TestTemplateWriter_URLTemplate(t *testing.T) {
	// Create a test HTTP server that serves a template
	templateContent := `Name: {{ .Name }}, Count: {{ .Count }}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(templateContent))
	}))
	defer server.Close()

	var buf bytes.Buffer
	writer := NewTemplateWriter(server.URL, &buf)

	data := struct {
		Name  string
		Count int
	}{
		Name:  "url-test",
		Count: 99,
	}

	ctx := context.Background()
	if err := writer.Serialize(ctx, data); err != nil {
		t.Fatalf("Serialize with URL template failed: %v", err)
	}

	expected := "Name: url-test, Count: 99"
	if buf.String() != expected {
		t.Errorf("unexpected output:\ngot:  %q\nwant: %q", buf.String(), expected)
	}
}

func TestTemplateWriter_URLTemplateNotFound(t *testing.T) {
	// Create a test HTTP server that returns 404
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	var buf bytes.Buffer
	writer := NewTemplateWriter(server.URL+"/notfound", &buf)

	ctx := context.Background()
	err := writer.Serialize(ctx, struct{}{})
	if err == nil {
		t.Fatal("expected error for non-existent URL template")
	}

	if !hasErrorCode(err, errors.ErrCodeUnavailable) {
		t.Errorf("expected ErrCodeUnavailable, got: %v", err)
	}
}

func TestValidateTemplateFile_SkipsURLValidation(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "http URL skips validation",
			path:    "http://example.com/template.tmpl",
			wantErr: false,
		},
		{
			name:    "https URL skips validation",
			path:    "https://example.com/template.tmpl",
			wantErr: false,
		},
		{
			name:    "local file that doesn't exist returns error",
			path:    "/nonexistent/path/template.tmpl",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTemplateFile(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateTemplateFile() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestExecuteTemplateToBytes_URL(t *testing.T) {
	// Create a test HTTP server that serves a template
	templateContent := `Result: {{ .Value }}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(templateContent))
	}))
	defer server.Close()

	data := struct {
		Value string
	}{
		Value: "from-url",
	}

	ctx := context.Background()
	result, err := ExecuteTemplateToBytes(ctx, server.URL, data)
	if err != nil {
		t.Fatalf("ExecuteTemplateToBytes with URL failed: %v", err)
	}

	expected := "Result: from-url"
	if string(result) != expected {
		t.Errorf("unexpected result:\ngot:  %q\nwant: %q", string(result), expected)
	}
}
