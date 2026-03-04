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

package file

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewParser(t *testing.T) {
	tests := []struct {
		name                    string
		opts                    []Option
		expectedDelimiter       string
		expectedMaxSize         int
		expectedSkipComments    bool
		expectedKVDelimiter     string
		expectedVDefault        string
		expectedVTrimChars      string
		expectedSkipEmptyValues bool
	}{
		{
			name:                    "default options",
			opts:                    nil,
			expectedDelimiter:       "\n",
			expectedMaxSize:         1 << 20, // 1MB
			expectedSkipComments:    true,
			expectedKVDelimiter:     "=",
			expectedVDefault:        "",
			expectedVTrimChars:      "",
			expectedSkipEmptyValues: false,
		},
		{
			name:                    "custom delimiter",
			opts:                    []Option{WithDelimiter(";")},
			expectedDelimiter:       ";",
			expectedMaxSize:         1 << 20,
			expectedSkipComments:    true,
			expectedKVDelimiter:     "=",
			expectedVDefault:        "",
			expectedVTrimChars:      "",
			expectedSkipEmptyValues: false,
		},
		{
			name:                    "custom max size",
			opts:                    []Option{WithMaxSize(1024)},
			expectedDelimiter:       "\n",
			expectedMaxSize:         1024,
			expectedSkipComments:    true,
			expectedKVDelimiter:     "=",
			expectedVDefault:        "",
			expectedVTrimChars:      "",
			expectedSkipEmptyValues: false,
		},
		{
			name: "all options",
			opts: []Option{
				WithDelimiter(":"),
				WithMaxSize(2048),
				WithSkipComments(false),
				WithKVDelimiter(":"),
				WithVDefault("N/A"),
				WithVTrimChars(`"'`),
				WithSkipEmptyValues(true),
			},
			expectedDelimiter:       ":",
			expectedMaxSize:         2048,
			expectedSkipComments:    false,
			expectedKVDelimiter:     ":",
			expectedVDefault:        "N/A",
			expectedVTrimChars:      `"'`,
			expectedSkipEmptyValues: true,
		},
		{
			name:                    "skip comments enabled",
			opts:                    []Option{WithSkipComments(true)},
			expectedDelimiter:       "\n",
			expectedMaxSize:         1 << 20,
			expectedSkipComments:    true,
			expectedKVDelimiter:     "=",
			expectedVDefault:        "",
			expectedVTrimChars:      "",
			expectedSkipEmptyValues: false,
		},
		{
			name:                    "skip comments disabled",
			opts:                    []Option{WithSkipComments(false)},
			expectedDelimiter:       "\n",
			expectedMaxSize:         1 << 20,
			expectedSkipComments:    false,
			expectedKVDelimiter:     "=",
			expectedVDefault:        "",
			expectedVTrimChars:      "",
			expectedSkipEmptyValues: false,
		},
		{
			name:                    "custom kv delimiter",
			opts:                    []Option{WithKVDelimiter(":")},
			expectedDelimiter:       "\n",
			expectedMaxSize:         1 << 20,
			expectedSkipComments:    true,
			expectedKVDelimiter:     ":",
			expectedVDefault:        "",
			expectedVTrimChars:      "",
			expectedSkipEmptyValues: false,
		},
		{
			name:                    "custom vDefault",
			opts:                    []Option{WithVDefault("true")},
			expectedDelimiter:       "\n",
			expectedMaxSize:         1 << 20,
			expectedSkipComments:    true,
			expectedKVDelimiter:     "=",
			expectedVDefault:        "true",
			expectedVTrimChars:      "",
			expectedSkipEmptyValues: false,
		},
		{
			name:                    "custom vTrimChars",
			opts:                    []Option{WithVTrimChars(`"'`)},
			expectedDelimiter:       "\n",
			expectedMaxSize:         1 << 20,
			expectedSkipComments:    true,
			expectedKVDelimiter:     "=",
			expectedVDefault:        "",
			expectedVTrimChars:      `"'`,
			expectedSkipEmptyValues: false,
		},
		{
			name:                    "skip empty values enabled",
			opts:                    []Option{WithSkipEmptyValues(true)},
			expectedDelimiter:       "\n",
			expectedMaxSize:         1 << 20,
			expectedSkipComments:    true,
			expectedKVDelimiter:     "=",
			expectedVDefault:        "",
			expectedVTrimChars:      "",
			expectedSkipEmptyValues: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewParser(tt.opts...)
			if p == nil {
				t.Fatal("NewParser() returned nil")
				return
			}
			if p.delimiter != tt.expectedDelimiter {
				t.Errorf("delimiter = %q, want %q", p.delimiter, tt.expectedDelimiter)
			}
			if p.maxSize != tt.expectedMaxSize {
				t.Errorf("maxSize = %d, want %d", p.maxSize, tt.expectedMaxSize)
			}
			if p.skipComments != tt.expectedSkipComments {
				t.Errorf("skipComments = %v, want %v", p.skipComments, tt.expectedSkipComments)
			}
			if p.kvDelimiter != tt.expectedKVDelimiter {
				t.Errorf("kvDelimiter = %q, want %q", p.kvDelimiter, tt.expectedKVDelimiter)
			}
			if p.vDefault != tt.expectedVDefault {
				t.Errorf("vDefault = %q, want %q", p.vDefault, tt.expectedVDefault)
			}
			if p.vTrimChars != tt.expectedVTrimChars {
				t.Errorf("vTrimChars = %q, want %q", p.vTrimChars, tt.expectedVTrimChars)
			}
			if p.skipEmptyValues != tt.expectedSkipEmptyValues {
				t.Errorf("skipEmptyValues = %v, want %v", p.skipEmptyValues, tt.expectedSkipEmptyValues)
			}
		})
	}
}

func TestGetLines(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		delimiter string
		maxSize   int
		expected  []string
		wantErr   bool
		errMsg    string
	}{
		{
			name:      "simple newline-delimited",
			content:   "line1\nline2\nline3",
			delimiter: "\n",
			expected:  []string{"line1", "line2", "line3"},
			wantErr:   false,
		},
		{
			name:      "trailing newline filtered",
			content:   "line1\nline2\n",
			delimiter: "\n",
			expected:  []string{"line1", "line2"},
			wantErr:   false,
		},
		{
			name:      "multiple trailing newlines filtered",
			content:   "line1\nline2\n\n\n",
			delimiter: "\n",
			expected:  []string{"line1", "line2"},
			wantErr:   false,
		},
		{
			name:      "custom delimiter semicolon",
			content:   "part1;part2;part3",
			delimiter: ";",
			expected:  []string{"part1", "part2", "part3"},
			wantErr:   false,
		},
		{
			name:      "custom delimiter with trailing",
			content:   "part1;part2;",
			delimiter: ";",
			expected:  []string{"part1", "part2"},
			wantErr:   false,
		},
		{
			name:      "empty file",
			content:   "",
			delimiter: "\n",
			expected:  []string{},
			wantErr:   false,
		},
		{
			name:      "single line no delimiter",
			content:   "single line",
			delimiter: "\n",
			expected:  []string{"single line"},
			wantErr:   false,
		},
		{
			name:      "only newlines",
			content:   "\n\n\n",
			delimiter: "\n",
			expected:  []string{},
			wantErr:   false,
		},
		{
			name:      "file too large",
			content:   strings.Repeat("a", 2000),
			delimiter: "\n",
			maxSize:   1000,
			wantErr:   true,
			errMsg:    "exceeds maximum size",
		},
		{
			name:      "invalid UTF-8",
			content:   "valid\xff\xfeinvalid",
			delimiter: "\n",
			wantErr:   true,
			errMsg:    "not valid UTF-8",
		},
		{
			name:      "space delimiter",
			content:   "word1 word2 word3",
			delimiter: " ",
			expected:  []string{"word1", "word2", "word3"},
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp file
			tmpfile, err := os.CreateTemp("", "test-*.txt")
			if err != nil {
				t.Fatalf("Failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			// Write content
			if _, writeErr := tmpfile.WriteString(tt.content); writeErr != nil {
				t.Fatalf("Failed to write temp file: %v", writeErr)
			}
			tmpfile.Close()

			// Create parser with options
			opts := []Option{WithDelimiter(tt.delimiter)}
			if tt.maxSize > 0 {
				opts = append(opts, WithMaxSize(tt.maxSize))
			}
			p := NewParser(opts...)

			// Test GetLines
			result, err := p.GetLines(tmpfile.Name())

			if tt.wantErr {
				if err == nil {
					t.Errorf("GetLines() expected error containing %q, got nil", tt.errMsg)
					return
				}
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("GetLines() error = %q, want error containing %q", err.Error(), tt.errMsg)
				}
				return
			}

			if err != nil {
				t.Errorf("GetLines() unexpected error: %v", err)
				return
			}

			if len(result) != len(tt.expected) {
				t.Errorf("GetLines() returned %d lines, want %d\nGot: %v\nWant: %v",
					len(result), len(tt.expected), result, tt.expected)
				return
			}

			for i := range result {
				if result[i] != tt.expected[i] {
					t.Errorf("GetLines()[%d] = %q, want %q", i, result[i], tt.expected[i])
				}
			}
		})
	}
}

func TestGetLines_EmptyPath(t *testing.T) {
	p := NewParser()
	_, err := p.GetLines("")
	if err == nil {
		t.Error("GetLines(\"\") expected error, got nil")
	}
	if !strings.Contains(err.Error(), "cannot be empty") {
		t.Errorf("GetLines(\"\") error = %q, want error containing 'cannot be empty'", err.Error())
	}
}

func TestGetLines_NonExistentFile(t *testing.T) {
	p := NewParser()
	_, err := p.GetLines("/nonexistent/file/path.txt")
	if err == nil {
		t.Error("GetLines() with nonexistent file expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to read file") {
		t.Errorf("GetLines() error = %q, want error containing 'failed to read file'", err.Error())
	}
}

func TestGetLines_SkipComments(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		skipComments bool
		expected     []string
	}{
		{
			name:         "skip comments enabled - comments removed",
			content:      "# This is a comment\nline1\n# Another comment\nline2",
			skipComments: true,
			expected:     []string{"line1", "line2"},
		},
		{
			name:         "skip comments disabled - comments included",
			content:      "# This is a comment\nline1\n# Another comment\nline2",
			skipComments: false,
			expected:     []string{"# This is a comment", "line1", "# Another comment", "line2"},
		},
		{
			name:         "only comments with skip enabled",
			content:      "# Comment 1\n# Comment 2\n# Comment 3",
			skipComments: true,
			expected:     []string{},
		},
		{
			name:         "only comments with skip disabled",
			content:      "# Comment 1\n# Comment 2\n# Comment 3",
			skipComments: false,
			expected:     []string{"# Comment 1", "# Comment 2", "# Comment 3"},
		},
		{
			name:         "mixed content with skip enabled",
			content:      "line1\n# Comment\nline2\n\nline3\n# Another comment",
			skipComments: true,
			expected:     []string{"line1", "line2", "line3"},
		},
		{
			name:         "mixed content with skip disabled",
			content:      "line1\n# Comment\nline2\n\nline3\n# Another comment",
			skipComments: false,
			expected:     []string{"line1", "# Comment", "line2", "line3", "# Another comment"},
		},
		{
			name:         "comment with leading spaces skip enabled",
			content:      "line1\n   # Indented comment\nline2",
			skipComments: true,
			expected:     []string{"line1", "line2"},
		},
		{
			name:         "hash not at start skip enabled",
			content:      "line1\nvalue # inline comment\nline2",
			skipComments: true,
			expected:     []string{"line1", "value # inline comment", "line2"},
		},
		{
			name:         "empty file with skip enabled",
			content:      "",
			skipComments: true,
			expected:     []string{},
		},
		{
			name:         "no comments with skip enabled",
			content:      "line1\nline2\nline3",
			skipComments: true,
			expected:     []string{"line1", "line2", "line3"},
		},
		{
			name:         "comments and empty lines with skip enabled",
			content:      "# Comment\n\nline1\n\n# Another\nline2\n\n",
			skipComments: true,
			expected:     []string{"line1", "line2"},
		},
		{
			name:         "comments and empty lines with skip disabled",
			content:      "# Comment\n\nline1\n\n# Another\nline2\n\n",
			skipComments: false,
			expected:     []string{"# Comment", "line1", "# Another", "line2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp file
			tmpfile, err := os.CreateTemp("", "test-comments-*.txt")
			if err != nil {
				t.Fatalf("Failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			// Write content
			if _, writeErr := tmpfile.WriteString(tt.content); writeErr != nil {
				t.Fatalf("Failed to write temp file: %v", writeErr)
			}
			tmpfile.Close()

			// Create parser with skipComments option
			p := NewParser(WithSkipComments(tt.skipComments))

			// Test GetLines
			result, err := p.GetLines(tmpfile.Name())
			if err != nil {
				t.Fatalf("GetLines() unexpected error: %v", err)
			}

			if len(result) != len(tt.expected) {
				t.Errorf("GetLines() returned %d lines, want %d\nGot: %v\nWant: %v",
					len(result), len(tt.expected), result, tt.expected)
				return
			}

			for i := range result {
				if result[i] != tt.expected[i] {
					t.Errorf("GetLines()[%d] = %q, want %q", i, result[i], tt.expected[i])
				}
			}

			// Verify no comment lines when skipComments is enabled
			if tt.skipComments {
				for _, line := range result {
					if strings.HasPrefix(line, "#") {
						t.Errorf("Found comment line %q when skipComments is enabled", line)
					}
				}
			}
		})
	}
}

func TestGetMap(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		kvDel    string
		expected map[string]string
		wantErr  bool
	}{
		{
			name:    "simple key-value pairs",
			content: "key1=value1\nkey2=value2\nkey3=value3",
			kvDel:   "=",
			expected: map[string]string{
				"key1": "value1",
				"key2": "value2",
				"key3": "value3",
			},
			wantErr: false,
		},
		{
			name:    "key-value with spaces",
			content: "key1 = value1\nkey2 = value2",
			kvDel:   "=",
			expected: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
			wantErr: false,
		},
		{
			name:    "colon delimiter",
			content: "name:John\nage:30\ncity:NYC",
			kvDel:   ":",
			expected: map[string]string{
				"name": "John",
				"age":  "30",
				"city": "NYC",
			},
			wantErr: false,
		},
		{
			name:    "lines without delimiter get default value",
			content: "valid=1\ninvalid line\nvalid2=2",
			kvDel:   "=",
			expected: map[string]string{
				"valid":        "1",
				"invalid line": "",
				"valid2":       "2",
			},
			wantErr: false,
		},
		{
			name:     "empty file",
			content:  "",
			kvDel:    "=",
			expected: map[string]string{},
			wantErr:  false,
		},
		{
			name:    "all lines without delimiter get default",
			content: "no delimiter here\nnor here",
			kvDel:   "=",
			expected: map[string]string{
				"no delimiter here": "",
				"nor here":          "",
			},
			wantErr: false,
		},
		{
			name:    "value with delimiter",
			content: "key=value=with=equals",
			kvDel:   "=",
			expected: map[string]string{
				"key": "value=with=equals",
			},
			wantErr: false,
		},
		{
			name:    "trailing newlines filtered",
			content: "key1=value1\n\n\n",
			kvDel:   "=",
			expected: map[string]string{
				"key1": "value1",
			},
			wantErr: false,
		},
		{
			name:    "duplicate keys last wins",
			content: "key=first\nkey=second",
			kvDel:   "=",
			expected: map[string]string{
				"key": "second",
			},
			wantErr: false,
		},
		{
			name:    "space delimiter",
			content: "NAME John\nAGE 30",
			kvDel:   " ",
			expected: map[string]string{
				"NAME": "John",
				"AGE":  "30",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp file
			tmpfile, err := os.CreateTemp("", "test-*.txt")
			if err != nil {
				t.Fatalf("Failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			// Write content
			if _, writeErr := tmpfile.WriteString(tt.content); writeErr != nil {
				t.Fatalf("Failed to write temp file: %v", writeErr)
			}
			tmpfile.Close()

			// Create parser with kv delimiter
			p := NewParser(WithKVDelimiter(tt.kvDel))

			// Test GetMap with new signature
			result, err := p.GetMap(tmpfile.Name())

			if tt.wantErr && err == nil {
				t.Error("GetMap() expected error, got nil")
				return
			}

			if !tt.wantErr && err != nil {
				t.Errorf("GetMap() unexpected error: %v", err)
				return
			}

			if len(result) != len(tt.expected) {
				t.Errorf("GetMap() returned %d entries, want %d\nGot: %v\nWant: %v",
					len(result), len(tt.expected), result, tt.expected)
				return
			}

			for key, expectedVal := range tt.expected {
				actualVal, exists := result[key]
				if !exists {
					t.Errorf("GetMap() missing key %q", key)
					continue
				}
				if actualVal != expectedVal {
					t.Errorf("GetMap()[%q] = %q, want %q", key, actualVal, expectedVal)
				}
			}
		})
	}
}

func TestGetMap_EmptyPath(t *testing.T) {
	p := NewParser()
	_, err := p.GetMap("")
	if err == nil {
		t.Error("GetMap(\"\") expected error, got nil")
	}
}

func TestGetMap_PropagatesGetLinesError(t *testing.T) {
	p := NewParser(WithMaxSize(10))
	tmpfile, err := os.CreateTemp("", "test-*.txt")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	// Write content larger than max size
	content := strings.Repeat("a", 100)
	if _, writeErr := tmpfile.WriteString(content); writeErr != nil {
		t.Fatalf("Failed to write temp file: %v", writeErr)
	}
	tmpfile.Close()

	_, err = p.GetMap(tmpfile.Name())
	if err == nil {
		t.Error("GetMap() expected error from GetLines, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("GetMap() error = %q, want error containing 'exceeds maximum size'", err.Error())
	}
}

func TestWithOptions(t *testing.T) {
	t.Run("WithDelimiter", func(t *testing.T) {
		p := &Parser{}
		opt := WithDelimiter(";")
		opt(p)
		if p.delimiter != ";" {
			t.Errorf("WithDelimiter() set delimiter to %q, want %q", p.delimiter, ";")
		}
	})

	t.Run("WithMaxSize", func(t *testing.T) {
		p := &Parser{}
		opt := WithMaxSize(5000)
		opt(p)
		if p.maxSize != 5000 {
			t.Errorf("WithMaxSize() set maxSize to %d, want %d", p.maxSize, 5000)
		}
	})
}

func TestGetLines_SpecialCharacters(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "unicode characters",
			content: "Hello 世界\nこんにちは\n🚀 emoji",
		},
		{
			name:    "tabs and special chars",
			content: "tab\there\nnewline\test",
		},
		{
			name:    "quotes and escapes",
			content: "key=\"value\"\npath=/usr/bin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpfile, err := os.CreateTemp("", "special-*.txt")
			if err != nil {
				t.Fatalf("Failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			tmpfile.WriteString(tt.content)
			tmpfile.Close()

			p := NewParser()
			lines, err := p.GetLines(tmpfile.Name())
			if err != nil {
				t.Errorf("GetLines() with special characters error: %v", err)
			}

			if len(lines) == 0 {
				t.Error("GetLines() returned no lines for content with special characters")
			}
		})
	}
}

func TestGetMap_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		kvDel    string
		expected int // number of expected entries
	}{
		{
			name:     "empty value",
			content:  "key=",
			kvDel:    "=",
			expected: 1,
		},
		{
			name:     "empty key",
			content:  "=value",
			kvDel:    "=",
			expected: 1,
		},
		{
			name:     "only delimiter",
			content:  "=",
			kvDel:    "=",
			expected: 1,
		},
		{
			name:     "multiple delimiters",
			content:  "key==value",
			kvDel:    "=",
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpdir := t.TempDir()
			tmpfile := filepath.Join(tmpdir, "test.txt")

			if err := os.WriteFile(tmpfile, []byte(tt.content), 0644); err != nil {
				t.Fatalf("Failed to write temp file: %v", err)
			}

			p := NewParser(WithKVDelimiter(tt.kvDel))
			result, err := p.GetMap(tmpfile)
			if err != nil {
				t.Errorf("GetMap() unexpected error: %v", err)
				return
			}

			if len(result) != tt.expected {
				t.Errorf("GetMap() returned %d entries, want %d", len(result), tt.expected)
			}
		})
	}
}

func BenchmarkGetLines(b *testing.B) {
	tmpfile, err := os.CreateTemp("", "bench-*.txt")
	if err != nil {
		b.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	var lines []string
	for i := 0; i < 1000; i++ {
		lines = append(lines, "This is test line number "+string(rune(i)))
	}
	content := strings.Join(lines, "\n")
	if _, writeErr := tmpfile.WriteString(content); writeErr != nil {
		b.Fatalf("Failed to write temp file: %v", writeErr)
	}
	tmpfile.Close()

	p := NewParser()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := p.GetLines(tmpfile.Name())
		if err != nil {
			b.Fatalf("GetLines() error: %v", err)
		}
	}
}

func BenchmarkGetMap(b *testing.B) {
	tmpfile, err := os.CreateTemp("", "bench-*.txt")
	if err != nil {
		b.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	var lines []string
	for i := 0; i < 1000; i++ {
		lines = append(lines, "key"+string(rune(i))+"=value"+string(rune(i)))
	}
	content := strings.Join(lines, "\n")
	if _, err := tmpfile.WriteString(content); err != nil {
		b.Fatalf("Failed to write temp file: %v", err)
	}
	tmpfile.Close()

	p := NewParser()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := p.GetMap(tmpfile.Name())
		if err != nil {
			b.Fatalf("GetMap() error: %v", err)
		}
	}
}

// TestGetMap_WithDefaultValue tests the vDefault parameter functionality
func TestGetMap_WithDefaultValue(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		kvDel    string
		vDefault string
		expected map[string]string
	}{
		{
			name:     "grub-style key-only params with empty default",
			content:  "ro quiet splash root=/dev/sda1",
			kvDel:    "=",
			vDefault: "",
			expected: map[string]string{
				"ro":     "",
				"quiet":  "",
				"splash": "",
				"root":   "/dev/sda1",
			},
		},
		{
			name:     "grub-style with custom default",
			content:  "ro quiet panic=-1",
			kvDel:    "=",
			vDefault: "true",
			expected: map[string]string{
				"ro":    "true",
				"quiet": "true",
				"panic": "-1",
			},
		},
		{
			name:     "mixed with multiline",
			content:  "key1=value1 flag1 key2=value2 flag2",
			kvDel:    "=",
			vDefault: "enabled",
			expected: map[string]string{
				"key1":  "value1",
				"flag1": "enabled",
				"key2":  "value2",
				"flag2": "enabled",
			},
		},
		{
			name:     "all key-only with default",
			content:  "opt1 opt2 opt3",
			kvDel:    "=",
			vDefault: "on",
			expected: map[string]string{
				"opt1": "on",
				"opt2": "on",
				"opt3": "on",
			},
		},
		{
			name:     "space delimiter with default",
			content:  "BOOT_IMAGE=/boot/vmlinuz ro quiet",
			kvDel:    "=",
			vDefault: "",
			expected: map[string]string{
				"BOOT_IMAGE": "/boot/vmlinuz",
				"ro":         "",
				"quiet":      "",
			},
		},
		{
			name:     "empty default preserves key-value behavior",
			content:  "a=1 b=2 c=3",
			kvDel:    "=",
			vDefault: "default",
			expected: map[string]string{
				"a": "1",
				"b": "2",
				"c": "3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp file
			tmpfile, err := os.CreateTemp("", "test-*.txt")
			if err != nil {
				t.Fatalf("Failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			// Write content
			if _, writeErr := tmpfile.WriteString(tt.content); writeErr != nil {
				t.Fatalf("Failed to write temp file: %v", writeErr)
			}
			tmpfile.Close()

			// Create parser with space delimiter for GRUB-like format and custom default
			p := NewParser(
				WithDelimiter(" "),
				WithKVDelimiter(tt.kvDel),
				WithVDefault(tt.vDefault),
			)

			// Test GetMap with new signature
			result, err := p.GetMap(tmpfile.Name())
			if err != nil {
				t.Fatalf("GetMap() error: %v", err)
			}

			if len(result) != len(tt.expected) {
				t.Errorf("GetMap() returned %d entries, want %d\nGot: %v\nWant: %v",
					len(result), len(tt.expected), result, tt.expected)
			}

			for key, expectedVal := range tt.expected {
				actualVal, exists := result[key]
				if !exists {
					t.Errorf("GetMap() missing key %q", key)
					continue
				}
				if actualVal != expectedVal {
					t.Errorf("GetMap()[%q] = %q, want %q", key, actualVal, expectedVal)
				}
			}
		})
	}
}

func TestGetMap_SkipComments(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		kvDel        string
		skipComments bool
		expected     map[string]string
	}{
		{
			name:         "os-release with comments skip enabled",
			content:      "# This is a comment\nNAME=\"Ubuntu\"\n# Another comment\nVERSION_ID=\"22.04\"",
			kvDel:        "=",
			skipComments: true,
			expected: map[string]string{
				"NAME":       "\"Ubuntu\"",
				"VERSION_ID": "\"22.04\"",
			},
		},
		{
			name:         "os-release with comments skip disabled",
			content:      "# This is a comment\nNAME=\"Ubuntu\"\n# Another comment\nVERSION_ID=\"22.04\"",
			kvDel:        "=",
			skipComments: false,
			expected: map[string]string{
				"# This is a comment": "",
				"NAME":                "\"Ubuntu\"",
				"# Another comment":   "",
				"VERSION_ID":          "\"22.04\"",
			},
		},
		{
			name:         "config file with inline comments skip enabled",
			content:      "# Comment at start\nhost=localhost\nport=8080\n# End comment",
			kvDel:        "=",
			skipComments: true,
			expected: map[string]string{
				"host": "localhost",
				"port": "8080",
			},
		},
		{
			name:         "config file with inline comments skip disabled",
			content:      "# Comment at start\nhost=localhost\nport=8080\n# End comment",
			kvDel:        "=",
			skipComments: false,
			expected: map[string]string{
				"# Comment at start": "",
				"host":               "localhost",
				"port":               "8080",
				"# End comment":      "",
			},
		},
		{
			name:         "only comments skip enabled",
			content:      "# Comment 1\n# Comment 2\n# Comment 3",
			kvDel:        "=",
			skipComments: true,
			expected:     map[string]string{},
		},
		{
			name:         "only comments skip disabled",
			content:      "# Comment 1\n# Comment 2\n# Comment 3",
			kvDel:        "=",
			skipComments: false,
			expected: map[string]string{
				"# Comment 1": "",
				"# Comment 2": "",
				"# Comment 3": "",
			},
		},
		{
			name:         "mixed with empty lines skip enabled",
			content:      "# Comment\n\nkey1=value1\n\n# Another\nkey2=value2",
			kvDel:        "=",
			skipComments: true,
			expected: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
		},
		{
			name:         "hash in value skip enabled",
			content:      "# Comment\nurl=http://example.com#anchor\nkey=value#withHash",
			kvDel:        "=",
			skipComments: true,
			expected: map[string]string{
				"url": "http://example.com#anchor",
				"key": "value#withHash",
			},
		},
		{
			name:         "comment with leading spaces skip enabled",
			content:      "key1=value1\n   # Indented comment\nkey2=value2",
			kvDel:        "=",
			skipComments: true,
			expected: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
		},
		{
			name:         "alternate delimiter with comments",
			content:      "# Comment\nkey1:value1\n# Another\nkey2:value2",
			kvDel:        ":",
			skipComments: true,
			expected: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp file
			tmpfile, err := os.CreateTemp("", "test-map-comments-*.txt")
			if err != nil {
				t.Fatalf("Failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			// Write content
			if _, writeErr := tmpfile.WriteString(tt.content); writeErr != nil {
				t.Fatalf("Failed to write temp file: %v", writeErr)
			}
			tmpfile.Close()

			// Create parser with skipComments and kvDelimiter options
			p := NewParser(
				WithSkipComments(tt.skipComments),
				WithKVDelimiter(tt.kvDel),
			)

			// Test GetMap with new signature
			result, err := p.GetMap(tmpfile.Name())
			if err != nil {
				t.Fatalf("GetMap() unexpected error: %v", err)
			}

			if len(result) != len(tt.expected) {
				t.Errorf("GetMap() returned %d entries, want %d\nGot: %v\nWant: %v",
					len(result), len(tt.expected), result, tt.expected)
			}

			for key, expectedVal := range tt.expected {
				actualVal, exists := result[key]
				if !exists {
					t.Errorf("GetMap() missing key %q", key)
					continue
				}
				if actualVal != expectedVal {
					t.Errorf("GetMap()[%q] = %q, want %q", key, actualVal, expectedVal)
				}
			}

			// Verify no comment keys when skipComments is enabled
			if tt.skipComments {
				for key := range result {
					if strings.HasPrefix(key, "#") {
						t.Errorf("Found comment key %q when skipComments is enabled", key)
					}
				}
			}
		})
	}
}

// TestGetMap_VTrimChars tests the value trimming functionality
func TestGetMap_VTrimChars(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		vTrimChars string
		expected   map[string]string
	}{
		{
			name:       "os-release double quotes",
			content:    "NAME=\"Ubuntu\"\nVERSION_ID=\"22.04\"\nPRETTY_NAME=\"Ubuntu 22.04 LTS\"",
			vTrimChars: `"`,
			expected: map[string]string{
				"NAME":        "Ubuntu",
				"VERSION_ID":  "22.04",
				"PRETTY_NAME": "Ubuntu 22.04 LTS",
			},
		},
		{
			name:       "single quotes",
			content:    "NAME='Ubuntu'\nVERSION='22.04'",
			vTrimChars: `'`,
			expected: map[string]string{
				"NAME":    "Ubuntu",
				"VERSION": "22.04",
			},
		},
		{
			name:       "both quote types",
			content:    "NAME=\"Ubuntu\"\nVERSION='22.04'\nID=ubuntu",
			vTrimChars: `"'`,
			expected: map[string]string{
				"NAME":    "Ubuntu",
				"VERSION": "22.04",
				"ID":      "ubuntu",
			},
		},
		{
			name:       "nested quotes",
			content:    "NAME=\"Ubuntu 'LTS'\"\nVERSION='22.04 \"Jammy\"'",
			vTrimChars: `"'`,
			expected: map[string]string{
				"NAME":    "Ubuntu 'LTS",
				"VERSION": "22.04 \"Jammy",
			},
		},
		{
			name:       "no quotes",
			content:    "NAME=Ubuntu\nVERSION=22.04",
			vTrimChars: `"'`,
			expected: map[string]string{
				"NAME":    "Ubuntu",
				"VERSION": "22.04",
			},
		},
		{
			name:       "trim spaces and quotes",
			content:    "NAME= \"Ubuntu\" \nVERSION= '22.04' ",
			vTrimChars: `"' `,
			expected: map[string]string{
				"NAME":    "Ubuntu",
				"VERSION": "22.04",
			},
		},
		{
			name:       "empty trim chars",
			content:    "NAME=\"Ubuntu\"\nVERSION='22.04'",
			vTrimChars: "",
			expected: map[string]string{
				"NAME":    `"Ubuntu"`,
				"VERSION": `'22.04'`,
			},
		},
		{
			name:       "mixed content with paths",
			content:    "PATH=\"/usr/bin:/bin\"\nHOME='/home/user'",
			vTrimChars: `"'`,
			expected: map[string]string{
				"PATH": "/usr/bin:/bin",
				"HOME": "/home/user",
			},
		},
		{
			name:       "value with only quotes",
			content:    "EMPTY=\"\"\nALSO_EMPTY=''",
			vTrimChars: `"'`,
			expected: map[string]string{
				"EMPTY":      "",
				"ALSO_EMPTY": "",
			},
		},
		{
			name:       "trim brackets",
			content:    "option1=[value1]\noption2={value2}",
			vTrimChars: "[]{}",
			expected: map[string]string{
				"option1": "value1",
				"option2": "value2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp file
			tmpfile, err := os.CreateTemp("", "test-trim-*.txt")
			if err != nil {
				t.Fatalf("Failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			// Write content
			if _, writeErr := tmpfile.WriteString(tt.content); writeErr != nil {
				t.Fatalf("Failed to write temp file: %v", writeErr)
			}
			tmpfile.Close()

			// Create parser with vTrimChars option
			p := NewParser(WithVTrimChars(tt.vTrimChars))

			// Test GetMap
			result, err := p.GetMap(tmpfile.Name())
			if err != nil {
				t.Fatalf("GetMap() unexpected error: %v", err)
			}

			if len(result) != len(tt.expected) {
				t.Errorf("GetMap() returned %d entries, want %d\nGot: %v\nWant: %v",
					len(result), len(tt.expected), result, tt.expected)
			}

			for key, expectedVal := range tt.expected {
				actualVal, exists := result[key]
				if !exists {
					t.Errorf("GetMap() missing key %q", key)
					continue
				}
				if actualVal != expectedVal {
					t.Errorf("GetMap()[%q] = %q, want %q", key, actualVal, expectedVal)
				}
			}
		})
	}
}

// TestGetMap_SkipEmptyValues tests the empty value filtering functionality
func TestGetMap_SkipEmptyValues(t *testing.T) {
	tests := []struct {
		name            string
		content         string
		skipEmptyValues bool
		vTrimChars      string
		expected        map[string]string
		expectedSkipped []string // keys that should be skipped
	}{
		{
			name:            "basic empty value filtering enabled",
			content:         "key1=value1\nkey2=\nkey3=value3",
			skipEmptyValues: true,
			expected: map[string]string{
				"key1": "value1",
				"key3": "value3",
			},
			expectedSkipped: []string{"key2"},
		},
		{
			name:            "basic empty value filtering disabled",
			content:         "key1=value1\nkey2=\nkey3=value3",
			skipEmptyValues: false,
			expected: map[string]string{
				"key1": "value1",
				"key2": "",
				"key3": "value3",
			},
		},
		{
			name:            "empty after quote trimming",
			content:         "NAME=\"Ubuntu\"\nVERSION=\"\"\nID=ubuntu",
			skipEmptyValues: true,
			vTrimChars:      `"`,
			expected: map[string]string{
				"NAME": "Ubuntu",
				"ID":   "ubuntu",
			},
			expectedSkipped: []string{"VERSION"},
		},
		{
			name:            "only empty values",
			content:         "key1=\nkey2=\nkey3=",
			skipEmptyValues: true,
			expected:        map[string]string{},
			expectedSkipped: []string{"key1", "key2", "key3"},
		},
		{
			name:            "no empty values",
			content:         "key1=value1\nkey2=value2",
			skipEmptyValues: true,
			expected: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
		},
		{
			name:            "mixed with key-only entries",
			content:         "key1=value1\nkey2=\nkey3",
			skipEmptyValues: true,
			expected: map[string]string{
				"key1": "value1",
			},
			expectedSkipped: []string{"key2", "key3"},
		},
		{
			name:            "whitespace values become empty after trimming",
			content:         "key1= \nkey2=\t\nkey3=  ",
			skipEmptyValues: true,
			expected:        map[string]string{},
			expectedSkipped: []string{"key1", "key2", "key3"},
		},
		{
			name:            "combined with skipComments",
			content:         "# Comment\nkey1=value1\nkey2=\n# Another\nkey3=value3",
			skipEmptyValues: true,
			expected: map[string]string{
				"key1": "value1",
				"key3": "value3",
			},
			expectedSkipped: []string{"key2"},
		},
		{
			name:            "empty after trimming spaces",
			content:         "key1=value1\nkey2= \" \" \nkey3=value3",
			skipEmptyValues: true,
			vTrimChars:      `" `,
			expected: map[string]string{
				"key1": "value1",
				"key3": "value3",
			},
			expectedSkipped: []string{"key2"},
		},
		{
			name:            "os-release realistic example",
			content:         "NAME=\"Ubuntu\"\nVERSION_ID=\"22.04\"\nBUILD_ID=\"\"\nVARIANT=\"\"",
			skipEmptyValues: true,
			vTrimChars:      `"`,
			expected: map[string]string{
				"NAME":       "Ubuntu",
				"VERSION_ID": "22.04",
			},
			expectedSkipped: []string{"BUILD_ID", "VARIANT"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp file
			tmpfile, err := os.CreateTemp("", "test-skip-empty-*.txt")
			if err != nil {
				t.Fatalf("Failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			// Write content
			if _, writeErr := tmpfile.WriteString(tt.content); writeErr != nil {
				t.Fatalf("Failed to write temp file: %v", writeErr)
			}
			tmpfile.Close()

			// Create parser with options
			opts := []Option{
				WithSkipEmptyValues(tt.skipEmptyValues),
				WithSkipComments(true), // Always skip comments for cleaner tests
			}
			if tt.vTrimChars != "" {
				opts = append(opts, WithVTrimChars(tt.vTrimChars))
			}
			p := NewParser(opts...)

			// Test GetMap
			result, err := p.GetMap(tmpfile.Name())
			if err != nil {
				t.Fatalf("GetMap() unexpected error: %v", err)
			}

			if len(result) != len(tt.expected) {
				t.Errorf("GetMap() returned %d entries, want %d\nGot: %v\nWant: %v",
					len(result), len(tt.expected), result, tt.expected)
			}

			for key, expectedVal := range tt.expected {
				actualVal, exists := result[key]
				if !exists {
					t.Errorf("GetMap() missing key %q", key)
					continue
				}
				if actualVal != expectedVal {
					t.Errorf("GetMap()[%q] = %q, want %q", key, actualVal, expectedVal)
				}
			}

			// Verify expected keys are skipped
			for _, skippedKey := range tt.expectedSkipped {
				if _, exists := result[skippedKey]; exists {
					t.Errorf("GetMap() should have skipped key %q but it exists with value %q", skippedKey, result[skippedKey])
				}
			}
		})
	}
}
