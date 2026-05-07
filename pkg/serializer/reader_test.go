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
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Test data structures
type testConfig struct {
	Name  string `json:"name" yaml:"name"`
	Value int    `json:"value" yaml:"value"`
}

func TestFormatFromPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected Format
	}{
		{
			name:     "json lowercase",
			path:     "config.json",
			expected: FormatJSON,
		},
		{
			name:     "json uppercase",
			path:     "CONFIG.JSON",
			expected: FormatJSON,
		},
		{
			name:     "yaml extension",
			path:     "config.yaml",
			expected: FormatYAML,
		},
		{
			name:     "yml extension",
			path:     "config.yml",
			expected: FormatYAML,
		},
		{
			name:     "yaml uppercase",
			path:     "CONFIG.YAML",
			expected: FormatYAML,
		},
		{
			name:     "table extension",
			path:     "output.table",
			expected: FormatTable,
		},
		{
			name:     "txt extension",
			path:     "output.txt",
			expected: FormatTable,
		},
		{
			name:     "unknown extension defaults to json",
			path:     "file.unknown",
			expected: FormatJSON,
		},
		{
			name:     "no extension defaults to json",
			path:     "filename",
			expected: FormatJSON,
		},
		{
			name:     "path with directories",
			path:     "/path/to/config.yaml",
			expected: FormatYAML,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatFromPath(tt.path)
			if result != tt.expected {
				t.Errorf("FormatFromPath(%q) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

func TestNewReader(t *testing.T) {
	t.Run("valid json format", func(t *testing.T) {
		input := strings.NewReader(`{"name":"test"}`)
		reader, err := NewReader(FormatJSON, input)
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}
		if reader == nil {
			t.Fatal("Expected non-nil reader")
			return
		}
		if reader.format != FormatJSON {
			t.Errorf("Expected format %v, got %v", FormatJSON, reader.format)
		}
	})

	t.Run("valid yaml format", func(t *testing.T) {
		input := strings.NewReader("name: test")
		reader, err := NewReader(FormatYAML, input)
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}
		if reader == nil {
			t.Fatal("Expected non-nil reader")
			return
		}
		if reader.format != FormatYAML {
			t.Errorf("Expected format %v, got %v", FormatYAML, reader.format)
		}
	})

	t.Run("table format returns error", func(t *testing.T) {
		input := strings.NewReader("data")
		reader, err := NewReader(FormatTable, input)
		if err == nil {
			t.Fatal("Expected error for table format")
		}
		if reader != nil {
			t.Error("Expected nil reader for unsupported format")
		}
		if !strings.Contains(err.Error(), "table format does not support deserialization") {
			t.Errorf("Expected table format error, got: %v", err)
		}
	})

	t.Run("unknown format returns error", func(t *testing.T) {
		input := strings.NewReader("data")
		reader, err := NewReader(Format("invalid"), input)
		if err == nil {
			t.Fatal("Expected error for unknown format")
		}
		if reader != nil {
			t.Error("Expected nil reader for unknown format")
		}
		if !strings.Contains(err.Error(), "unknown format") {
			t.Errorf("Expected unknown format error, got: %v", err)
		}
	})

	t.Run("stores closer if input implements io.Closer", func(t *testing.T) {
		// Create a temporary file
		tmpfile, err := os.CreateTemp("", testName)
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		reader, err := NewReader(FormatJSON, tmpfile)
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		if reader.closer == nil {
			t.Error("Expected closer to be set for io.Closer input")
		}

		// Clean up
		reader.Close()
	})
}

func TestReader_DeserializeJSON(t *testing.T) {
	t.Run("valid json object", func(t *testing.T) {
		jsonData := `{"name":"test","value":123}`
		reader, err := NewReader(FormatJSON, strings.NewReader(jsonData))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result testConfig
		err = reader.Deserialize(&result)
		if err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if result.Name != testName {
			t.Errorf("Expected name 'test', got %q", result.Name)
		}
		if result.Value != 123 {
			t.Errorf("Expected value 123, got %d", result.Value)
		}
	})

	t.Run("valid json array", func(t *testing.T) {
		jsonData := `[{"name":"test1","value":123},{"name":"test2","value":456}]`
		reader, err := NewReader(FormatJSON, strings.NewReader(jsonData))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result []testConfig
		err = reader.Deserialize(&result)
		if err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if len(result) != 2 {
			t.Fatalf("Expected 2 items, got %d", len(result))
		}
		if result[0].Name != "test1" || result[0].Value != 123 {
			t.Errorf("Unexpected first item: %+v", result[0])
		}
		if result[1].Name != "test2" || result[1].Value != 456 {
			t.Errorf("Unexpected second item: %+v", result[1])
		}
	})

	t.Run("invalid json returns error", func(t *testing.T) {
		jsonData := `{invalid json}`
		reader, err := NewReader(FormatJSON, strings.NewReader(jsonData))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result testConfig
		err = reader.Deserialize(&result)
		if err == nil {
			t.Fatal("Expected error for invalid JSON")
		}
		if !strings.Contains(err.Error(), "failed to decode JSON") {
			t.Errorf("Expected JSON decode error, got: %v", err)
		}
	})

	t.Run("empty input returns error", func(t *testing.T) {
		reader, err := NewReader(FormatJSON, strings.NewReader(""))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result testConfig
		err = reader.Deserialize(&result)
		if err == nil {
			t.Fatal("Expected error for empty input")
		}
	})
}

func TestReader_DeserializeYAML(t *testing.T) {
	t.Run("valid yaml object", func(t *testing.T) {
		yamlData := `name: test
value: 123`
		reader, err := NewReader(FormatYAML, strings.NewReader(yamlData))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result testConfig
		err = reader.Deserialize(&result)
		if err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if result.Name != testName {
			t.Errorf("Expected name 'test', got %q", result.Name)
		}
		if result.Value != 123 {
			t.Errorf("Expected value 123, got %d", result.Value)
		}
	})

	t.Run("valid yaml array", func(t *testing.T) {
		yamlData := `- name: test1
  value: 123
- name: test2
  value: 456`
		reader, err := NewReader(FormatYAML, strings.NewReader(yamlData))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result []testConfig
		err = reader.Deserialize(&result)
		if err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if len(result) != 2 {
			t.Fatalf("Expected 2 items, got %d", len(result))
		}
		if result[0].Name != "test1" || result[0].Value != 123 {
			t.Errorf("Unexpected first item: %+v", result[0])
		}
		if result[1].Name != "test2" || result[1].Value != 456 {
			t.Errorf("Unexpected second item: %+v", result[1])
		}
	})

	t.Run("invalid yaml returns error", func(t *testing.T) {
		yamlData := `name: test
value: [unclosed array`
		reader, err := NewReader(FormatYAML, strings.NewReader(yamlData))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result testConfig
		err = reader.Deserialize(&result)
		if err == nil {
			t.Fatal("Expected error for invalid YAML")
		}
		if !strings.Contains(err.Error(), "failed to decode YAML") {
			t.Errorf("Expected YAML decode error, got: %v", err)
		}
	})
}

func TestReader_DeserializeNilChecks(t *testing.T) {
	t.Run("nil reader", func(t *testing.T) {
		var reader *Reader
		var result testConfig
		err := reader.Deserialize(&result)
		if err == nil {
			t.Fatal("Expected error for nil reader")
		}
		if !strings.Contains(err.Error(), "reader is nil") {
			t.Errorf("Expected nil reader error, got: %v", err)
		}
	})

	t.Run("nil input", func(t *testing.T) {
		reader := &Reader{
			format: FormatJSON,
			input:  nil,
		}
		var result testConfig
		err := reader.Deserialize(&result)
		if err == nil {
			t.Fatal("Expected error for nil input")
		}
		if !strings.Contains(err.Error(), "input source is nil") {
			t.Errorf("Expected nil input error, got: %v", err)
		}
	})
}

func TestNewFileReader(t *testing.T) {
	t.Run("valid json file", func(t *testing.T) {
		// Create temporary file
		tmpfile, err := os.CreateTemp("", "test*.json")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		// Write test data
		data := testConfig{Name: testName, Value: 123}
		jsonData, _ := json.Marshal(data)
		if _, writeErr := tmpfile.Write(jsonData); writeErr != nil {
			t.Fatal(writeErr)
		}
		tmpfile.Close()

		// Create reader
		reader, err := NewFileReader(FormatJSON, tmpfile.Name())
		if err != nil {
			t.Fatalf("NewFileReader failed: %v", err)
		}
		defer reader.Close()

		// Deserialize
		var result testConfig
		if err := reader.Deserialize(&result); err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if result.Name != testName || result.Value != 123 {
			t.Errorf("Unexpected result: %+v", result)
		}
	})

	t.Run("valid yaml file", func(t *testing.T) {
		// Create temporary file
		tmpfile, err := os.CreateTemp("", "test*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		// Write test data
		data := testConfig{Name: testName, Value: 123}
		yamlData, _ := yaml.Marshal(data)
		if _, writeErr := tmpfile.Write(yamlData); writeErr != nil {
			t.Fatal(writeErr)
		}
		tmpfile.Close()

		// Create reader
		reader, err := NewFileReader(FormatYAML, tmpfile.Name())
		if err != nil {
			t.Fatalf("NewFileReader failed: %v", err)
		}
		defer reader.Close()

		// Deserialize
		var result testConfig
		if err := reader.Deserialize(&result); err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if result.Name != testName || result.Value != 123 {
			t.Errorf("Unexpected result: %+v", result)
		}
	})

	t.Run("nonexistent file returns NotFound", func(t *testing.T) {
		reader, err := NewFileReader(FormatJSON, "/nonexistent/file.json")
		if err == nil {
			t.Fatal("Expected error for nonexistent file")
		}
		if reader != nil {
			t.Error("Expected nil reader for nonexistent file")
		}
		// ENOENT is now classified as ErrCodeNotFound (HTTP 404 / Exit
		// NotFound) rather than ErrCodeInternal so callers can distinguish
		// "file missing" from other I/O failures.
		if !strings.Contains(err.Error(), "file not found") {
			t.Errorf("Expected NotFound error, got: %v", err)
		}
	})

	t.Run("unknown format returns error", func(t *testing.T) {
		reader, err := NewFileReader(Format("invalid"), "test.json")
		if err == nil {
			t.Fatal("Expected error for unknown format")
		}
		if reader != nil {
			t.Error("Expected nil reader for unknown format")
		}
		if !strings.Contains(err.Error(), "unknown format") {
			t.Errorf("Expected unknown format error, got: %v", err)
		}
	})

	t.Run("table format returns error", func(t *testing.T) {
		reader, err := NewFileReader(FormatTable, "test.table")
		if err == nil {
			t.Fatal("Expected error for table format")
		}
		if reader != nil {
			t.Error("Expected nil reader for table format")
		}
		if !strings.Contains(err.Error(), "table format does not support deserialization") {
			t.Errorf("Expected table format error, got: %v", err)
		}
	})
}

func TestNewFileReader_ErrorPaths(t *testing.T) {
	tests := []struct {
		name      string
		format    Format
		filePath  string
		wantErrSS string
	}{
		{
			name:      "empty path",
			format:    FormatJSON,
			filePath:  "",
			wantErrSS: "failed to open file",
		},
		{
			name:      "nested in nonexistent directory",
			format:    FormatJSON,
			filePath:  "/no/such/dir/file.json",
			wantErrSS: "file not found",
		},
		{
			name:      "path with null byte",
			format:    FormatYAML,
			filePath:  "/tmp/invalid\x00file.yaml",
			wantErrSS: "failed to open file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := NewFileReader(tt.format, tt.filePath)
			if err == nil {
				reader.Close()
				t.Fatal("expected error")
			}
			if reader != nil {
				t.Error("expected nil reader on error")
			}
			if !strings.Contains(err.Error(), tt.wantErrSS) {
				t.Errorf("error = %v, want substring %q", err, tt.wantErrSS)
			}
		})
	}
}

func TestNewFileReader_ReaderState(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		data   string
	}{
		{
			name:   "json reader has closer",
			format: FormatJSON,
			data:   `{"name":"state","value":1}`,
		},
		{
			name:   "yaml reader has closer",
			format: FormatYAML,
			data:   "name: state\nvalue: 1\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpfile, err := os.CreateTemp("", "state-test*")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(tmpfile.Name())
			tmpfile.WriteString(tt.data)
			tmpfile.Close()

			reader, err := NewFileReader(tt.format, tmpfile.Name())
			if err != nil {
				t.Fatalf("NewFileReader failed: %v", err)
			}
			defer reader.Close()

			if reader.format != tt.format {
				t.Errorf("format = %v, want %v", reader.format, tt.format)
			}
			if reader.closer == nil {
				t.Error("expected closer to be set for file-backed reader")
			}
			if reader.input == nil {
				t.Error("expected input to be set")
			}
		})
	}
}

func Test_newFileReaderAuto(t *testing.T) {
	t.Run("auto-detect json", func(t *testing.T) {
		// Create temporary file
		tmpfile, err := os.CreateTemp("", "test*.json")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		// Write test data
		data := testConfig{Name: testName, Value: 123}
		jsonData, _ := json.Marshal(data)
		if _, writeErr := tmpfile.Write(jsonData); writeErr != nil {
			t.Fatal(writeErr)
		}
		tmpfile.Close()

		// Create reader with auto-detection
		reader, err := newFileReaderAuto(tmpfile.Name())
		if err != nil {
			t.Fatalf("newFileReaderAuto failed: %v", err)
		}
		defer reader.Close()

		if reader.format != FormatJSON {
			t.Errorf("Expected format %v, got %v", FormatJSON, reader.format)
		}

		// Deserialize
		var result testConfig
		if err := reader.Deserialize(&result); err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if result.Name != testName || result.Value != 123 {
			t.Errorf("Unexpected result: %+v", result)
		}
	})

	t.Run("auto-detect yaml", func(t *testing.T) {
		// Create temporary file
		tmpfile, err := os.CreateTemp("", "test*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		// Write test data
		data := testConfig{Name: testName, Value: 123}
		yamlData, _ := yaml.Marshal(data)
		if _, writeErr := tmpfile.Write(yamlData); writeErr != nil {
			t.Fatal(writeErr)
		}
		tmpfile.Close()

		// Create reader with auto-detection
		reader, err := newFileReaderAuto(tmpfile.Name())
		if err != nil {
			t.Fatalf("newFileReaderAuto failed: %v", err)
		}
		defer reader.Close()

		if reader.format != FormatYAML {
			t.Errorf("Expected format %v, got %v", FormatYAML, reader.format)
		}

		// Deserialize
		var result testConfig
		if err := reader.Deserialize(&result); err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if result.Name != testName || result.Value != 123 {
			t.Errorf("Unexpected result: %+v", result)
		}
	})

	t.Run("unknown extension defaults to json", func(t *testing.T) {
		// Create temporary file with unknown extension
		tmpfile, err := os.CreateTemp("", "test*.unknown")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		// Write JSON data (since it will default to JSON format)
		data := testConfig{Name: testName, Value: 123}
		jsonData, _ := json.Marshal(data)
		if _, writeErr := tmpfile.Write(jsonData); writeErr != nil {
			t.Fatal(writeErr)
		}
		tmpfile.Close()

		// Create reader with auto-detection
		reader, err := newFileReaderAuto(tmpfile.Name())
		if err != nil {
			t.Fatalf("newFileReaderAuto failed: %v", err)
		}
		defer reader.Close()

		if reader.format != FormatJSON {
			t.Errorf("Expected format %v (default), got %v", FormatJSON, reader.format)
		}
	})
}

func TestReader_Close(t *testing.T) {
	t.Run("close file reader", func(t *testing.T) {
		// Create temporary file
		tmpfile, err := os.CreateTemp("", "test*.json")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())
		tmpfile.Close()

		reader, err := NewFileReader(FormatJSON, tmpfile.Name())
		if err != nil {
			t.Fatalf("NewFileReader failed: %v", err)
		}

		// Close should succeed
		if err := reader.Close(); err != nil {
			t.Errorf("Close failed: %v", err)
		}

		// Second close should not error
		if err := reader.Close(); err != nil {
			t.Errorf("Second Close failed: %v", err)
		}
	})

	t.Run("close nil reader", func(t *testing.T) {
		var reader *Reader
		err := reader.Close()
		if err != nil {
			t.Errorf("Close on nil reader should not error, got: %v", err)
		}
	})

	t.Run("close reader with no closer", func(t *testing.T) {
		reader, err := NewReader(FormatJSON, bytes.NewReader([]byte("{}")))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		err = reader.Close()
		if err != nil {
			t.Errorf("Close should not error for non-closer input, got: %v", err)
		}
	})
}

func TestReader_RoundTrip(t *testing.T) {
	t.Run("json round trip", func(t *testing.T) {
		// Create temporary file
		tmpfile, err := os.CreateTemp("", "test*.json")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		// Write with Writer
		writer := NewWriter(FormatJSON, tmpfile)
		original := []testConfig{
			{Name: "test1", Value: 123},
			{Name: "test2", Value: 456},
		}
		if serErr := writer.Serialize(context.Background(), original); serErr != nil {
			t.Fatalf("Writer.Serialize failed: %v", serErr)
		}
		if closeErr := writer.Close(); closeErr != nil {
			t.Fatalf("Writer.Close failed: %v", closeErr)
		}

		// Read with Reader
		reader, err := newFileReaderAuto(tmpfile.Name())
		if err != nil {
			t.Fatalf("newFileReaderAuto failed: %v", err)
		}
		defer reader.Close()

		var result []testConfig
		if err := reader.Deserialize(&result); err != nil {
			t.Fatalf("Reader.Deserialize failed: %v", err)
		}

		// Verify data matches
		if len(result) != len(original) {
			t.Fatalf("Expected %d items, got %d", len(original), len(result))
		}
		for i := range original {
			if result[i].Name != original[i].Name || result[i].Value != original[i].Value {
				t.Errorf("Item %d mismatch: got %+v, want %+v", i, result[i], original[i])
			}
		}
	})

	t.Run("yaml round trip", func(t *testing.T) {
		// Create temporary file
		tmpfile, err := os.CreateTemp("", "test*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		// Write with Writer
		writer := NewWriter(FormatYAML, tmpfile)
		original := []testConfig{
			{Name: "test1", Value: 123},
			{Name: "test2", Value: 456},
		}
		if serErr := writer.Serialize(context.Background(), original); serErr != nil {
			t.Fatalf("Writer.Serialize failed: %v", serErr)
		}
		if closeErr := writer.Close(); closeErr != nil {
			t.Fatalf("Writer.Close failed: %v", closeErr)
		}

		// Read with Reader
		reader, err := newFileReaderAuto(tmpfile.Name())
		if err != nil {
			t.Fatalf("newFileReaderAuto failed: %v", err)
		}
		defer reader.Close()

		var result []testConfig
		if err := reader.Deserialize(&result); err != nil {
			t.Fatalf("Reader.Deserialize failed: %v", err)
		}

		// Verify data matches
		if len(result) != len(original) {
			t.Fatalf("Expected %d items, got %d", len(original), len(result))
		}
		for i := range original {
			if result[i].Name != original[i].Name || result[i].Value != original[i].Value {
				t.Errorf("Item %d mismatch: got %+v, want %+v", i, result[i], original[i])
			}
		}
	})
}

func TestReader_ComplexStructures(t *testing.T) {
	type nested struct {
		Items []testConfig
		Meta  map[string]string
	}

	t.Run("nested json structure", func(t *testing.T) {
		jsonData := `{
			"items": [
				{"name":"test1","value":123},
				{"name":"test2","value":456}
			],
			"meta": {
				"version": "1.0",
				"author": "test"
			}
		}`

		reader, err := NewReader(FormatJSON, strings.NewReader(jsonData))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result nested
		if err := reader.Deserialize(&result); err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if len(result.Items) != 2 {
			t.Errorf("Expected 2 items, got %d", len(result.Items))
		}
		if result.Meta["version"] != "1.0" {
			t.Errorf("Expected version 1.0, got %q", result.Meta["version"])
		}
	})

	t.Run("nested yaml structure", func(t *testing.T) {
		yamlData := `items:
  - name: test1
    value: 123
  - name: test2
    value: 456
meta:
  version: "1.0"
  author: test`

		reader, err := NewReader(FormatYAML, strings.NewReader(yamlData))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result nested
		if err := reader.Deserialize(&result); err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if len(result.Items) != 2 {
			t.Errorf("Expected 2 items, got %d", len(result.Items))
		}
		if result.Meta["version"] != "1.0" {
			t.Errorf("Expected version 1.0, got %q", result.Meta["version"])
		}
	})
}

// Benchmark tests
func BenchmarkReader_DeserializeJSON(b *testing.B) {
	data := []testConfig{
		{Name: "test1", Value: 123},
		{Name: "test2", Value: 456},
		{Name: "test3", Value: 789},
	}
	jsonData, _ := json.Marshal(data)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reader, _ := NewReader(FormatJSON, bytes.NewReader(jsonData))
		var result []testConfig
		reader.Deserialize(&result)
	}
}

func BenchmarkReader_DeserializeYAML(b *testing.B) {
	data := []testConfig{
		{Name: "test1", Value: 123},
		{Name: "test2", Value: 456},
		{Name: "test3", Value: 789},
	}
	yamlData, _ := yaml.Marshal(data)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reader, _ := NewReader(FormatYAML, bytes.NewReader(yamlData))
		var result []testConfig
		reader.Deserialize(&result)
	}
}

// Example usage
func ExampleReader() {
	// Create a reader from a string
	jsonData := `{"name":"example","value":42}`
	reader, err := NewReader(FormatJSON, strings.NewReader(jsonData))
	if err != nil {
		panic(err)
	}

	// Deserialize into a struct
	var config testConfig
	if err := reader.Deserialize(&config); err != nil {
		panic(err)
	}

	// Use the data
	_ = config.Name  // "example"
	_ = config.Value // 42
}

func Test_newFileReaderAuto_example(t *testing.T) {
	tmpfile, err := os.CreateTemp("", "example*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())
	tmpfile.WriteString(`{"name":"example","value":42}`)
	tmpfile.Close()

	reader, err := newFileReaderAuto(tmpfile.Name())
	if err != nil {
		t.Fatalf("newFileReaderAuto failed: %v", err)
	}
	defer reader.Close()

	var config testConfig
	if err := reader.Deserialize(&config); err != nil {
		t.Fatalf("Deserialize failed: %v", err)
	}

	if config.Name != "example" {
		t.Errorf("Name = %q, want %q", config.Name, "example")
	}
}

// New comprehensive tests

func TestFromFile_Success(t *testing.T) {
	t.Run("load json file", func(t *testing.T) {
		// Create temporary file
		tmpfile, err := os.CreateTemp("", "test*.json")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		// Write test data
		data := testConfig{Name: "fromfile", Value: 999}
		jsonData, _ := json.Marshal(data)
		tmpfile.Write(jsonData)
		tmpfile.Close()

		// Use FromFile
		result, err := FromFile[testConfig](tmpfile.Name())
		if err != nil {
			t.Fatalf("FromFile failed: %v", err)
		}

		if result == nil {
			t.Fatal("Expected non-nil result")
			return
		}

		if result.Name != "fromfile" || result.Value != 999 {
			t.Errorf("Unexpected result: %+v", result)
		}
	})

	t.Run("load yaml file", func(t *testing.T) {
		tmpfile, err := os.CreateTemp("", "test*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		data := testConfig{Name: "yamltest", Value: 777}
		yamlData, _ := yaml.Marshal(data)
		tmpfile.Write(yamlData)
		tmpfile.Close()

		result, err := FromFile[testConfig](tmpfile.Name())
		if err != nil {
			t.Fatalf("FromFile failed: %v", err)
		}

		if result.Name != "yamltest" || result.Value != 777 {
			t.Errorf("Unexpected result: %+v", result)
		}
	})

	t.Run("load slice from json", func(t *testing.T) {
		tmpfile, err := os.CreateTemp("", "test*.json")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		data := []testConfig{
			{Name: "item1", Value: 111},
			{Name: "item2", Value: 222},
		}
		jsonData, _ := json.Marshal(data)
		tmpfile.Write(jsonData)
		tmpfile.Close()

		result, err := FromFile[[]testConfig](tmpfile.Name())
		if err != nil {
			t.Fatalf("FromFile failed: %v", err)
		}

		if len(*result) != 2 {
			t.Fatalf("Expected 2 items, got %d", len(*result))
		}
	})

	t.Run("load map from yaml", func(t *testing.T) {
		tmpfile, err := os.CreateTemp("", "test*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		data := map[string]int{"key1": 100, "key2": 200}
		yamlData, _ := yaml.Marshal(data)
		tmpfile.Write(yamlData)
		tmpfile.Close()

		result, err := FromFile[map[string]int](tmpfile.Name())
		if err != nil {
			t.Fatalf("FromFile failed: %v", err)
		}

		if (*result)["key1"] != 100 || (*result)["key2"] != 200 {
			t.Errorf("Unexpected result: %+v", *result)
		}
	})
}

func TestFromFile_Errors(t *testing.T) {
	t.Run("nonexistent file", func(t *testing.T) {
		_, err := FromFile[testConfig]("/nonexistent/file.json")
		if err == nil {
			t.Fatal("Expected error for nonexistent file")
		}
		if !strings.Contains(err.Error(), "failed to create serializer") {
			t.Errorf("Expected serializer creation error, got: %v", err)
		}
	})

	t.Run("invalid json format", func(t *testing.T) {
		tmpfile, err := os.CreateTemp("", "test*.json")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		tmpfile.WriteString("{invalid json}")
		tmpfile.Close()

		_, err = FromFile[testConfig](tmpfile.Name())
		if err == nil {
			t.Fatal("Expected error for invalid JSON")
		}
		if !strings.Contains(err.Error(), "failed to deserialize") {
			t.Errorf("Expected deserialization error, got: %v", err)
		}
	})

	t.Run("type mismatch", func(t *testing.T) {
		tmpfile, err := os.CreateTemp("", "test*.json")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		// Write array but try to deserialize as object
		tmpfile.WriteString(`[{"name":"test"}]`)
		tmpfile.Close()

		_, err = FromFile[testConfig](tmpfile.Name())
		if err == nil {
			t.Fatal("Expected error for type mismatch")
		}
	})
}

func TestReader_DeserializeTableFormat(t *testing.T) {
	reader := &Reader{
		format: FormatTable,
		input:  strings.NewReader("data"),
	}

	var result testConfig
	err := reader.Deserialize(&result)
	if err == nil {
		t.Fatal("Expected error for table format deserialization")
	}
	if !strings.Contains(err.Error(), "table format is not supported") {
		t.Errorf("Expected table format error, got: %v", err)
	}
}

func TestReader_DeserializeUnsupportedFormat(t *testing.T) {
	reader := &Reader{
		format: Format("unsupported"),
		input:  strings.NewReader("data"),
	}

	var result testConfig
	err := reader.Deserialize(&result)
	if err == nil {
		t.Fatal("Expected error for unsupported format")
	}
	if !strings.Contains(err.Error(), "unsupported format") {
		t.Errorf("Expected unsupported format error, got: %v", err)
	}
}

func TestReader_MultipleDeserialize(t *testing.T) {
	t.Run("multiple deserialize calls should work", func(t *testing.T) {
		// JSON decoder can handle multiple calls if we reset the reader
		jsonData := `{"name":"test1","value":123}`

		reader, err := NewReader(FormatJSON, strings.NewReader(jsonData))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result1 testConfig
		err = reader.Deserialize(&result1)
		if err != nil {
			t.Fatalf("First Deserialize failed: %v", err)
		}

		// Second deserialize should fail (EOF) because reader is consumed
		var result2 testConfig
		err = reader.Deserialize(&result2)
		if err == nil {
			t.Fatal("Expected error on second deserialize from exhausted reader")
		}
	})
}

func TestReader_EmptyFile(t *testing.T) {
	t.Run("empty json file", func(t *testing.T) {
		tmpfile, err := os.CreateTemp("", "test*.json")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())
		tmpfile.Close()

		reader, err := NewFileReader(FormatJSON, tmpfile.Name())
		if err != nil {
			t.Fatalf("NewFileReader failed: %v", err)
		}
		defer reader.Close()

		var result testConfig
		err = reader.Deserialize(&result)
		if err == nil {
			t.Fatal("Expected error for empty file")
		}
	})
}

func TestReader_LargeFile(t *testing.T) {
	t.Run("deserialize large array", func(t *testing.T) {
		tmpfile, err := os.CreateTemp("", "test*.json")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(tmpfile.Name())

		// Create large array (1000 items)
		var largeData []testConfig
		for i := 0; i < 1000; i++ {
			largeData = append(largeData, testConfig{
				Name:  fmt.Sprintf("item%d", i),
				Value: i,
			})
		}

		jsonData, _ := json.Marshal(largeData)
		tmpfile.Write(jsonData)
		tmpfile.Close()

		reader, err := newFileReaderAuto(tmpfile.Name())
		if err != nil {
			t.Fatalf("newFileReaderAuto failed: %v", err)
		}
		defer reader.Close()

		var result []testConfig
		err = reader.Deserialize(&result)
		if err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if len(result) != 1000 {
			t.Errorf("Expected 1000 items, got %d", len(result))
		}

		// Spot check first and last items
		if result[0].Name != "item0" || result[0].Value != 0 {
			t.Errorf("First item incorrect: %+v", result[0])
		}
		if result[999].Name != "item999" || result[999].Value != 999 {
			t.Errorf("Last item incorrect: %+v", result[999])
		}
	})
}

func TestReader_SpecialCharacters(t *testing.T) {
	t.Run("json with unicode", func(t *testing.T) {
		jsonData := `{"name":"测试","value":42}`
		reader, err := NewReader(FormatJSON, strings.NewReader(jsonData))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result testConfig
		err = reader.Deserialize(&result)
		if err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if result.Name != "测试" {
			t.Errorf("Expected name '测试', got %q", result.Name)
		}
	})

	t.Run("yaml with special characters", func(t *testing.T) {
		yamlData := `name: "test: with: colons"
value: 123`
		reader, err := NewReader(FormatYAML, strings.NewReader(yamlData))
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result testConfig
		err = reader.Deserialize(&result)
		if err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		if result.Name != "test: with: colons" {
			t.Errorf("Expected name with colons, got %q", result.Name)
		}
	})
}

func TestNewReader_NilInput(t *testing.T) {
	reader, err := NewReader(FormatJSON, nil)
	if err != nil {
		t.Fatalf("NewReader should succeed with nil input, got error: %v", err)
	}

	// But Deserialize should fail
	var result testConfig
	err = reader.Deserialize(&result)
	if err == nil {
		t.Fatal("Expected error when deserializing from nil input")
	}
	if !strings.Contains(err.Error(), "input source is nil") {
		t.Errorf("Expected nil input error, got: %v", err)
	}
}

func TestReader_CustomCloser(t *testing.T) {
	t.Run("custom closer is called", func(t *testing.T) {
		closeCalled := false
		customReader := &testClosableReader{
			Reader: strings.NewReader(`{"name":"test","value":123}`),
			onClose: func() error {
				closeCalled = true
				return nil
			},
		}

		reader, err := NewReader(FormatJSON, customReader)
		if err != nil {
			t.Fatalf("NewReader failed: %v", err)
		}

		var result testConfig
		if err := reader.Deserialize(&result); err != nil {
			t.Fatalf("Deserialize failed: %v", err)
		}

		// Close should call custom closer
		if err := reader.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}

		if !closeCalled {
			t.Error("Expected custom closer to be called")
		}
	})
}

// testClosableReader wraps a reader and adds a closer
type testClosableReader struct {
	io.Reader
	onClose func() error
}

func (r *testClosableReader) Close() error {
	if r.onClose != nil {
		return r.onClose()
	}
	return nil
}

func TestFormatFromPath_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected Format
	}{
		{"empty path", "", FormatJSON},
		{"only extension", ".json", FormatJSON},
		{"multiple dots", "file.backup.json", FormatJSON},
		{"mixed case", "File.YaMl", FormatYAML},
		{"absolute path", "/usr/local/config.yaml", FormatYAML},
		{"windows path", "C:\\Users\\config.json", FormatJSON},
		{"url-like path", "https://example.com/data.yaml", FormatYAML},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatFromPath(tt.path)
			if result != tt.expected {
				t.Errorf("FormatFromPath(%q) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

func TestReader_ConcurrentAccess(t *testing.T) {
	t.Run("concurrent deserialize should be safe with separate readers", func(t *testing.T) {
		jsonData := `{"name":"concurrent","value":100}`

		done := make(chan bool, 10)
		for i := 0; i < 10; i++ {
			go func() {
				reader, err := NewReader(FormatJSON, strings.NewReader(jsonData))
				if err != nil {
					t.Errorf("NewReader failed: %v", err)
					done <- false
					return
				}

				var result testConfig
				if err := reader.Deserialize(&result); err != nil {
					t.Errorf("Deserialize failed: %v", err)
					done <- false
					return
				}

				if result.Name != "concurrent" {
					t.Errorf("Expected name 'concurrent', got %q", result.Name)
					done <- false
					return
				}

				done <- true
			}()
		}

		// Wait for all goroutines
		for i := 0; i < 10; i++ {
			if !<-done {
				t.Fatal("At least one goroutine failed")
			}
		}
	})
}

// Benchmark for new tests
func BenchmarkFromFile_JSON(b *testing.B) {
	tmpfile, _ := os.CreateTemp("", "bench*.json")
	defer os.Remove(tmpfile.Name())

	data := testConfig{Name: "benchmark", Value: 12345}
	jsonData, _ := json.Marshal(data)
	tmpfile.Write(jsonData)
	tmpfile.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		FromFile[testConfig](tmpfile.Name())
	}
}

func BenchmarkFromFile_YAML(b *testing.B) {
	tmpfile, _ := os.CreateTemp("", "bench*.yaml")
	defer os.Remove(tmpfile.Name())

	data := testConfig{Name: "benchmark", Value: 12345}
	yamlData, _ := yaml.Marshal(data)
	tmpfile.Write(yamlData)
	tmpfile.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		FromFile[testConfig](tmpfile.Name())
	}
}
