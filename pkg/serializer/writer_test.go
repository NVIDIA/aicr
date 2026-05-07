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

package serializer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	testName  = "test"
	test1Name = "test1"
)

func TestWriter_SerializeJSON(t *testing.T) {
	var buf bytes.Buffer
	writer := NewWriter(FormatJSON, &buf)

	data := []testConfig{
		{Name: test1Name, Value: 123},
		{Name: "test2", Value: 456},
	}

	err := writer.Serialize(context.Background(), data)
	if err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}

	// Verify it's valid JSON
	var result []testConfig
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	if len(result) != 2 {
		t.Errorf("Expected 2 items, got %d", len(result))
	}

	if result[0].Name != test1Name || result[0].Value != 123 {
		t.Errorf("Unexpected data: %+v", result[0])
	}
}

func TestWriter_SerializeYAML(t *testing.T) {
	var buf bytes.Buffer
	writer := NewWriter(FormatYAML, &buf)

	data := []testConfig{
		{Name: test1Name, Value: 123},
		{Name: "test2", Value: 456},
	}

	err := writer.Serialize(context.Background(), data)
	if err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}

	// Verify it's valid YAML
	var result []testConfig
	if err := yaml.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("Failed to unmarshal YAML: %v", err)
	}

	if len(result) != 2 {
		t.Errorf("Expected 2 items, got %d", len(result))
	}

	if result[0].Name != test1Name || result[0].Value != 123 {
		t.Errorf("Unexpected data: %+v", result[0])
	}
}

func TestWriter_SerializeTable(t *testing.T) {
	var buf bytes.Buffer
	writer := NewWriter(FormatTable, &buf)

	data := []any{
		testConfig{Name: test1Name, Value: 123},
		testConfig{Name: "test2", Value: 456},
	}

	err := writer.Serialize(context.Background(), data)
	if err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}

	output := buf.String()

	// Verify output contains expected elements
	if !strings.Contains(output, "FIELD") || !strings.Contains(output, "VALUE") {
		t.Error("Expected table header not found")
	}

	if !strings.Contains(output, "[0].Name") || !strings.Contains(output, "[1].Value") {
		t.Error("Expected flattened keys not found")
	}
}

func TestWriter_UnsupportedFormat(t *testing.T) {
	// Note: NewWriter now defaults unknown formats to JSON instead of erroring
	// This test is kept to verify the fallback behavior
	var buf bytes.Buffer
	writer := NewWriter("invalid", &buf)

	if writer == nil {
		t.Fatal("Expected non-nil writer with unknown format")
	}

	// Should succeed because it falls back to JSON
	data := testConfig{Name: "test", Value: 123}
	err := writer.Serialize(context.Background(), data)
	if err != nil {
		t.Fatalf("Serialize should not fail with unknown format (falls back to JSON): %v", err)
	}

	// Verify it was serialized as JSON
	var result testConfig
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("Failed to unmarshal as JSON: %v", err)
	}

	if result.Name != testName || result.Value != 123 {
		t.Errorf("Unexpected data: %+v", result)
	}
}

func TestWriter_NilOutput(t *testing.T) {
	// Should default to stdout
	writer := NewStdoutWriter(FormatJSON)

	if writer == nil {
		t.Fatal("Expected non-nil writer")
	}

	// Don't actually run Serialize as it would write to stdout
}

func TestNewWriter_DefaultsToStdout(t *testing.T) {
	writer := NewStdoutWriter(FormatJSON)
	if writer == nil {
		t.Fatal("Expected non-nil writer with nil output")
	}
}

func TestNewStdoutWriter(t *testing.T) {
	tests := []struct {
		name   string
		format Format
	}{
		{"json format", FormatJSON},
		{"yaml format", FormatYAML},
		{"unknown format defaults to JSON", Format("unknown")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewStdoutWriter(tt.format)
			if w == nil {
				t.Fatal("NewStdoutWriter() returned nil")
			}
		})
	}
}

func TestWriter_Close(t *testing.T) {
	// Test closing stdout writer (should be safe)
	writer := NewStdoutWriter(FormatJSON)
	err := writer.Close()
	if err != nil {
		t.Errorf("Close on stdout writer should not error: %v", err)
	}

	// Test closing multiple times (should be safe)
	err = writer.Close()
	if err != nil {
		t.Errorf("Multiple Close calls should not error: %v", err)
	}
}

func TestNewFileWriterOrStdout_EmptyPath(t *testing.T) {
	tests := []string{"", "  ", "\t", "\n", "-"}

	for _, path := range tests {
		writer, err := NewFileWriterOrStdout(FormatJSON, path)
		if err != nil {
			t.Fatalf("Expected no error for empty path %q, got: %v", path, err)
		}
		if writer == nil {
			t.Fatalf("Expected non-nil writer for empty path %q", path)
		}
		// Should default to stdout, so Close should be safe
		if closer, ok := writer.(Closer); ok {
			if err := closer.Close(); err != nil {
				t.Errorf("Close failed for empty path writer: %v", err)
			}
		}
	}
}

func TestNewFileWriterOrStdout_Success(t *testing.T) {
	// Create a temporary file path
	tmpFile := t.TempDir() + "/test_output.json"

	writer, err := NewFileWriterOrStdout(FormatJSON, tmpFile)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if writer == nil {
		t.Fatal("Expected non-nil writer")
	}

	// Write some data
	data := testConfig{Name: testName, Value: 123}
	err = writer.Serialize(context.Background(), data)
	if err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}

	// Close the writer
	if closer, ok := writer.(Closer); ok {
		err = closer.Close()
		if err != nil {
			t.Fatalf("Close failed: %v", err)
		}
	}

	// Verify file exists and has content
	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("Failed to read output file: %v", err)
	}

	if len(content) == 0 {
		t.Error("Expected file to have content")
	}

	// Verify it's valid JSON
	var result testConfig
	if err := json.Unmarshal(content, &result); err != nil {
		t.Fatalf("Failed to unmarshal file content: %v", err)
	}

	if result.Name != testName || result.Value != 123 {
		t.Errorf("Unexpected data in file: %+v", result)
	}
}

func TestNewFileWriterOrStdout_InvalidPath(t *testing.T) {
	// Try to create a file in a non-existent directory
	writer, err := NewFileWriterOrStdout(FormatJSON, "/nonexistent/path/file.json")

	// Should return an error instead of falling back to stdout
	if err == nil {
		t.Fatal("Expected error for invalid path")
	}
	if writer != nil {
		t.Error("Expected nil writer when error is returned")
	}

	// Verify error message is helpful
	if !strings.Contains(err.Error(), "failed to create output file") {
		t.Errorf("Expected helpful error message, got: %v", err)
	}
}

func TestNewFileWriterOrStdout_InvalidConfigMapURI(t *testing.T) {
	tests := []struct {
		name string
		uri  string
	}{
		{"missing name", "cm://namespace"},
		{"missing namespace", "cm:///name"},
		{"empty", "cm://"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer, err := NewFileWriterOrStdout(FormatJSON, tt.uri)
			if err == nil {
				t.Fatalf("Expected error for invalid ConfigMap URI %q", tt.uri)
			}
			if writer != nil {
				t.Error("Expected nil writer when error is returned")
			}
			if !strings.Contains(err.Error(), "invalid ConfigMap URI") {
				t.Errorf("Expected helpful error message, got: %v", err)
			}
		})
	}
}

func TestFormat_IsUnknown(t *testing.T) {
	tests := []struct {
		format Format
		want   bool
	}{
		{FormatJSON, false},
		{FormatYAML, false},
		{FormatTable, false},
		{Format("invalid"), true},
		{Format("xml"), true},
		{Format(""), true},
	}

	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			if got := tt.format.IsUnknown(); got != tt.want {
				t.Errorf("Format(%q).IsUnknown() = %v, want %v", tt.format, got, tt.want)
			}
		})
	}
}

func TestNewWriter_UnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	writer := NewWriter(Format("invalid"), &buf)

	if writer == nil {
		t.Fatal("Expected non-nil writer")
	}

	// Should default to JSON format
	data := testConfig{Name: "test", Value: 123}
	err := writer.Serialize(context.Background(), data)
	if err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}

	// Verify it serialized as JSON
	var result testConfig
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("Failed to unmarshal as JSON: %v", err)
	}
}

func TestWriter_SerializeTable_EmptyData(t *testing.T) {
	var buf bytes.Buffer
	writer := NewWriter(FormatTable, &buf)

	// Empty slice
	err := writer.Serialize(context.Background(), []testConfig{})
	if err != nil {
		t.Fatalf("Serialize empty slice failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "<empty>") {
		t.Errorf("Expected '<empty>' in output for empty data, got: %s", output)
	}
}

func TestWriter_SerializeTable_NestedStructs(t *testing.T) {
	var buf bytes.Buffer
	writer := NewWriter(FormatTable, &buf)

	type inner struct {
		Field1 string
		Field2 int
	}

	type outer struct {
		Name  string
		Inner inner
	}

	data := outer{
		Name: "test",
		Inner: inner{
			Field1: "value",
			Field2: 42,
		},
	}

	err := writer.Serialize(context.Background(), data)
	if err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}

	output := buf.String()

	// Should have flattened nested keys
	if !strings.Contains(output, "Inner.Field1") {
		t.Error("Expected flattened key 'Inner.Field1' not found")
	}

	if !strings.Contains(output, "Inner.Field2") {
		t.Error("Expected flattened key 'Inner.Field2' not found")
	}

	if !strings.Contains(output, "value") {
		t.Error("Expected value 'value' not found")
	}

	if !strings.Contains(output, "42") {
		t.Error("Expected value '42' not found")
	}
}

func TestWriter_SerializeTable_Maps(t *testing.T) {
	var buf bytes.Buffer
	writer := NewWriter(FormatTable, &buf)

	data := map[string]any{
		"key1": "value1",
		"key2": 123,
		"key3": true,
	}

	err := writer.Serialize(context.Background(), data)
	if err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}

	output := buf.String()

	// Should have all keys
	if !strings.Contains(output, "key1") || !strings.Contains(output, "key2") || !strings.Contains(output, "key3") {
		t.Error("Expected all keys in output")
	}
}

func TestWriter_SerializeTable_NilValues(t *testing.T) {
	var buf bytes.Buffer
	writer := NewWriter(FormatTable, &buf)

	type dataWithNil struct {
		Name  string
		Value *int
	}

	data := dataWithNil{
		Name:  "test",
		Value: nil,
	}

	err := writer.Serialize(context.Background(), data)
	if err != nil {
		t.Fatalf("Serialize failed: %v", err)
	}

	output := buf.String()

	// Should handle nil gracefully
	if !strings.Contains(output, "Name") {
		t.Error("Expected 'Name' field in output")
	}
}

func TestSupportedFormats(t *testing.T) {
	formats := SupportedFormats()

	// Verify we have expected formats
	expected := []string{string(FormatJSON), string(FormatYAML), string(FormatTable)}
	if len(formats) != len(expected) {
		t.Errorf("SupportedFormats() len = %d, want %d", len(formats), len(expected))
	}

	for _, exp := range expected {
		if !slices.Contains(formats, exp) {
			t.Errorf("SupportedFormats() missing %v", exp)
		}
	}
}

// Tests for internal serialize functions

func Test_serializeJSON(t *testing.T) {
	tests := []struct {
		name    string
		data    any
		wantErr bool
	}{
		{
			name:    "simple struct",
			data:    testConfig{Name: "test", Value: 123},
			wantErr: false,
		},
		{
			name:    "slice of structs",
			data:    []testConfig{{Name: "a", Value: 1}, {Name: "b", Value: 2}},
			wantErr: false,
		},
		{
			name:    "map",
			data:    map[string]int{"one": 1, "two": 2},
			wantErr: false,
		},
		{
			name:    "nested struct",
			data:    struct{ Inner testConfig }{Inner: testConfig{Name: "inner", Value: 42}},
			wantErr: false,
		},
		{
			name:    "nil",
			data:    nil,
			wantErr: false,
		},
		{
			name:    "empty slice",
			data:    []testConfig{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := serializeJSON(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("serializeJSON() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(got) == 0 {
				t.Error("serializeJSON() returned empty bytes")
			}
			// Verify it's valid JSON by unmarshaling
			if !tt.wantErr && tt.data != nil {
				var result any
				if err := json.Unmarshal(got, &result); err != nil {
					t.Errorf("serializeJSON() produced invalid JSON: %v", err)
				}
			}
		})
	}
}

func Test_serializeYAML(t *testing.T) {
	tests := []struct {
		name    string
		data    any
		wantErr bool
	}{
		{
			name:    "simple struct",
			data:    testConfig{Name: "test", Value: 123},
			wantErr: false,
		},
		{
			name:    "slice of structs",
			data:    []testConfig{{Name: "a", Value: 1}, {Name: "b", Value: 2}},
			wantErr: false,
		},
		{
			name:    "map",
			data:    map[string]int{"one": 1, "two": 2},
			wantErr: false,
		},
		{
			name:    "nested struct",
			data:    struct{ Inner testConfig }{Inner: testConfig{Name: "inner", Value: 42}},
			wantErr: false,
		},
		{
			name:    "nil",
			data:    nil,
			wantErr: false,
		},
		{
			name:    "unsupported type",
			data:    &yamlErrMarshaler{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := serializeYAML(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("serializeYAML() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && tt.data != nil && len(got) == 0 {
				t.Error("serializeYAML() returned empty bytes")
			}
			// Verify it's valid YAML by unmarshaling
			if !tt.wantErr && tt.data != nil {
				var result any
				if err := yaml.Unmarshal(got, &result); err != nil {
					t.Errorf("serializeYAML() produced invalid YAML: %v", err)
				}
			}
		})
	}
}

func Test_serializeTable(t *testing.T) {
	tests := []struct {
		name     string
		data     any
		wantErr  bool
		contains []string
	}{
		{
			name:     "simple struct",
			data:     testConfig{Name: "test", Value: 123},
			wantErr:  false,
			contains: []string{"FIELD", "VALUE", "Name", "test", "Value", "123"},
		},
		{
			name:     "map",
			data:     map[string]int{"one": 1, "two": 2},
			wantErr:  false,
			contains: []string{"FIELD", "VALUE", "one", "1", "two", "2"},
		},
		{
			name:     "empty struct",
			data:     struct{}{},
			wantErr:  false,
			contains: []string{"<empty>"},
		},
		{
			name:     "nil",
			data:     nil,
			wantErr:  false,
			contains: []string{"<empty>"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := serializeTable(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("serializeTable() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			output := string(got)
			for _, want := range tt.contains {
				if !strings.Contains(output, want) {
					t.Errorf("serializeTable() output missing %q, got: %s", want, output)
				}
			}
		})
	}
}

func TestWriteToFile(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		tmpDir := t.TempDir()
		path := tmpDir + "/test.txt"
		data := []byte("test content")

		err := WriteToFile(path, data)
		if err != nil {
			t.Fatalf("WriteToFile() error = %v", err)
		}

		// Verify file contents
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read file: %v", err)
		}
		if string(got) != string(data) {
			t.Errorf("WriteToFile() wrote %q, want %q", got, data)
		}
	})

	t.Run("empty data", func(t *testing.T) {
		tmpDir := t.TempDir()
		path := tmpDir + "/empty.txt"

		err := WriteToFile(path, []byte{})
		if err != nil {
			t.Fatalf("WriteToFile() error = %v", err)
		}

		// Verify file exists and is empty
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("file not created: %v", err)
		}
		if info.Size() != 0 {
			t.Errorf("expected empty file, got size %d", info.Size())
		}
	})

	t.Run("invalid path", func(t *testing.T) {
		err := WriteToFile("/nonexistent/directory/file.txt", []byte("data"))
		if err == nil {
			t.Error("WriteToFile() expected error for invalid path")
		}
		if !strings.Contains(err.Error(), "failed to create file") {
			t.Errorf("expected 'failed to create file' error, got: %v", err)
		}
	})

	t.Run("overwrite existing file", func(t *testing.T) {
		tmpDir := t.TempDir()
		path := tmpDir + "/overwrite.txt"

		// Write initial content
		if err := WriteToFile(path, []byte("initial")); err != nil {
			t.Fatalf("initial write failed: %v", err)
		}

		// Overwrite with new content
		if err := WriteToFile(path, []byte("overwritten")); err != nil {
			t.Fatalf("overwrite failed: %v", err)
		}

		// Verify new content
		got, _ := os.ReadFile(path)
		if string(got) != "overwritten" {
			t.Errorf("expected 'overwritten', got %q", got)
		}
	})
}

func TestSerializeJSONStandalone(t *testing.T) {
	tests := []struct {
		name    string
		data    any
		wantErr bool
	}{
		{
			name: "serializes struct",
			data: struct {
				Name string `json:"name"`
				Age  int    `json:"age"`
			}{Name: "test", Age: 42},
		},
		{
			name: "serializes map",
			data: map[string]any{"key": "value", "num": 123},
		},
		{
			name:    "handles unsupported type",
			data:    make(chan int),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := serializeJSON(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("serializeJSON() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(got) == 0 {
				t.Error("serializeJSON() returned empty bytes")
			}
		})
	}
}

func TestSerializeYAMLStandalone(t *testing.T) {
	tests := []struct {
		name    string
		data    any
		wantErr bool
	}{
		{
			name: "serializes struct",
			data: struct {
				Name string `yaml:"name"`
				Age  int    `yaml:"age"`
			}{Name: "test", Age: 42},
		},
		{
			name: "serializes map",
			data: map[string]string{"key": "value"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := serializeYAML(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("serializeYAML() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(got) == 0 {
				t.Error("serializeYAML() returned empty bytes")
			}
		})
	}
}

type errWriter struct{}

func (e *errWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

type yamlErrMarshaler struct{}

func (y *yamlErrMarshaler) MarshalYAML() (any, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestWriter_SerializeYAML_WriterError(t *testing.T) {
	w := NewWriter(FormatYAML, &errWriter{})
	err := w.Serialize(context.Background(), testConfig{Name: "test", Value: 1})
	if err == nil {
		t.Fatal("expected error from broken writer")
	}
	if !strings.Contains(err.Error(), "failed to serialize to YAML") {
		t.Errorf("error = %v, want error containing 'failed to serialize to YAML'", err)
	}
}

func TestWriter_SerializeJSON_WriterError(t *testing.T) {
	w := NewWriter(FormatJSON, &errWriter{})
	err := w.Serialize(context.Background(), testConfig{Name: "test", Value: 1})
	if err == nil {
		t.Fatal("expected error from broken writer")
	}
	if !strings.Contains(err.Error(), "failed to serialize to JSON") {
		t.Errorf("error = %v, want error containing 'failed to serialize to JSON'", err)
	}
}

func Test_serializeJSON_Formatting(t *testing.T) {
	data := testConfig{Name: "test", Value: 123}
	got, err := serializeJSON(data)
	if err != nil {
		t.Fatalf("serializeJSON() error = %v", err)
	}

	// Should be indented (pretty-printed)
	if !strings.Contains(string(got), "\n") {
		t.Error("serializeJSON() should produce indented output")
	}
	if !strings.Contains(string(got), "  ") {
		t.Error("serializeJSON() should use 2-space indentation")
	}
}
