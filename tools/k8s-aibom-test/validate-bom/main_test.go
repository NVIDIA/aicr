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

package main

import (
	"bytes"
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	projecterrors "github.com/NVIDIA/aicr/pkg/errors"
)

// validBOM is the minimum shape run() must accept: CycloneDX 1.6, with
// metadata and at least one component.
const validBOM = `{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "version": 1,
  "metadata": {"component": {"type": "application", "name": "aicr-aibom-fixture"}},
  "components": [{"type": "machine-learning-model", "name": "nvidia/aicr-aibom-fixture"}]
}`

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bom  string
		// wantErrFragment is empty when run() must succeed; otherwise the
		// failure message must name the specific reason, so a case cannot
		// pass by failing for an unrelated cause.
		wantErrFragment string
	}{
		{name: "valid CycloneDX 1.6", bom: validBOM},
		{
			name: "wrong spec version",
			bom: `{"bomFormat":"CycloneDX","specVersion":"1.5","version":1,
			       "metadata":{},"components":[{"type":"library","name":"x"}]}`,
			wantErrFragment: "specVersion",
		},
		{
			name:            "wrong bom format",
			bom:             `{"bomFormat":"SPDX","specVersion":"1.6","version":1,"metadata":{},"components":[]}`,
			wantErrFragment: "bomFormat",
		},
		{
			name: "no components is not a pass",
			bom: `{"bomFormat":"CycloneDX","specVersion":"1.6","version":1,
			       "metadata":{},"components":[]}`,
			wantErrFragment: "no components",
		},
		{
			name: "missing metadata",
			bom: `{"bomFormat":"CycloneDX","specVersion":"1.6","version":1,
			       "components":[{"type":"library","name":"x"}]}`,
			wantErrFragment: "metadata",
		},
		{name: "malformed JSON", bom: `{`, wantErrFragment: "decode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			bomPath := filepath.Join(t.TempDir(), "bom.json")
			if err := os.WriteFile(bomPath, []byte(tt.bom), 0o600); err != nil {
				t.Fatalf("write BOM: %v", err)
			}

			var output bytes.Buffer
			err := run([]string{bomPath}, &output)

			if tt.wantErrFragment == "" {
				if err != nil {
					t.Fatalf("run() error = %v, want nil", err)
				}
				if !strings.Contains(output.String(), "1.6") {
					t.Errorf("run() output = %q, want it to name the spec version", output.String())
				}
				return
			}
			if err == nil {
				t.Fatalf("run() error = nil, want one naming %q", tt.wantErrFragment)
			}
			if !strings.Contains(err.Error(), tt.wantErrFragment) {
				t.Errorf("run() error = %v, want it to name %q", err, tt.wantErrFragment)
			}
			if !stderrors.Is(err, projecterrors.New(projecterrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("run() error code = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestRunRequiresExactlyOnePath(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{nil, {"a", "b"}} {
		err := run(args, &bytes.Buffer{})
		if !stderrors.Is(err, projecterrors.New(projecterrors.ErrCodeInvalidRequest, "")) {
			t.Errorf("run(%v) error = %v, want ErrCodeInvalidRequest", args, err)
		}
	}
}

func TestReadBounded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name string, content []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}

	tests := []struct {
		name string
		path string
		// wantData is checked only when wantCode is nil.
		wantData string
		wantCode *projecterrors.StructuredError
	}{
		{
			name:     "reads a file under the cap",
			path:     write("small", []byte("content")),
			wantData: "content",
		},
		{
			name: "reads a file exactly at the cap",
			path: write("exact", bytes.Repeat([]byte{'x'}, maxInputBytes)),
			// Boundary case: the LimitReader is given maxInputBytes+1, so an
			// off-by-one in the comparison would reject a legal document.
			wantData: string(bytes.Repeat([]byte{'x'}, maxInputBytes)),
		},
		{
			name: "refuses one byte over the cap",
			path: write("large", bytes.Repeat([]byte{'x'}, maxInputBytes+1)),
			// Refused, not truncated: a truncated BOM fails to decode and
			// reports a misleading parse error instead of a size error.
			wantCode: projecterrors.New(projecterrors.ErrCodeInvalidRequest, ""),
		},
		{
			name:     "missing file",
			path:     filepath.Join(dir, "missing"),
			wantCode: projecterrors.New(projecterrors.ErrCodeNotFound, ""),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := readBounded(tt.path)
			if tt.wantCode != nil {
				if !stderrors.Is(err, tt.wantCode) {
					t.Fatalf("readBounded() error = %v, want code %v", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("readBounded() error = %v, want nil", err)
			}
			if string(data) != tt.wantData {
				t.Errorf("readBounded() returned %d bytes, want %d", len(data), len(tt.wantData))
			}
		})
	}
}
