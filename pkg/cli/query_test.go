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
	"bytes"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/serializer"
)

func TestWriteQueryResult(t *testing.T) {
	tests := []struct {
		name     string
		val      any
		format   serializer.Format
		contains string
	}{
		{
			name:     "string yaml",
			val:      "570.86.16",
			format:   serializer.FormatYAML,
			contains: "570.86.16",
		},
		{
			name:     "string json is valid json",
			val:      "570.86.16",
			format:   serializer.FormatJSON,
			contains: `"570.86.16"`,
		},
		{
			name:     "bool yaml",
			val:      true,
			format:   serializer.FormatYAML,
			contains: "true",
		},
		{
			name:     "bool json is valid json",
			val:      true,
			format:   serializer.FormatJSON,
			contains: "true",
		},
		{
			name:     "int yaml",
			val:      42,
			format:   serializer.FormatYAML,
			contains: "42",
		},
		{
			name:     "int json is valid json",
			val:      42,
			format:   serializer.FormatJSON,
			contains: "42",
		},
		{
			name:     "float64 yaml",
			val:      3.14,
			format:   serializer.FormatYAML,
			contains: "3.14",
		},
		{
			name:     "map yaml",
			val:      map[string]any{"key": "value"},
			format:   serializer.FormatYAML,
			contains: "key: value",
		},
		{
			name:     "map json",
			val:      map[string]any{"key": "value"},
			format:   serializer.FormatJSON,
			contains: `"key": "value"`,
		},
		{
			name:     "slice yaml",
			val:      []string{"a", "b"},
			format:   serializer.FormatYAML,
			contains: "- a",
		},
		{
			name:     "slice json",
			val:      []string{"a", "b"},
			format:   serializer.FormatJSON,
			contains: `"a"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if writeErr := writeQueryResult(&buf, tt.val, tt.format); writeErr != nil {
				t.Fatalf("writeQueryResult returned error: %v", writeErr)
			}

			output := buf.String()

			if !strings.Contains(output, tt.contains) {
				t.Errorf("output %q does not contain %q", output, tt.contains)
			}
		})
	}
}

func TestQueryCmdFlagsExcludesOutput(t *testing.T) {
	flags := queryCmdFlags()
	for _, f := range flags {
		names := f.Names()
		for _, n := range names {
			if n == "output" {
				t.Error("queryCmdFlags should not include --output flag")
			}
		}
	}
}

func TestQueryCmdFlagsIncludesSelector(t *testing.T) {
	flags := queryCmdFlags()
	found := false
	for _, f := range flags {
		names := f.Names()
		for _, n := range names {
			if n == "selector" {
				found = true
			}
		}
	}
	if !found {
		t.Error("queryCmdFlags must include --selector flag")
	}
}
