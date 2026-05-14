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

package verifier

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectInputForm(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlPath := filepath.Join(tmp, "pointer.yaml")
	if err := os.WriteFile(yamlPath, []byte("schemaVersion: 1.0.0\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name    string
		in      string
		want    InputForm
		wantErr bool
	}{
		{"empty rejected", "", "", true},
		{"https rejected", "https://example.com/x", "", true},
		{"http rejected", "http://example.com/x", "", true},
		{"directory accepted", dir, InputFormDir, false},
		{"yaml pointer accepted", yamlPath, InputFormPointer, false},
		{"oci-scheme accepted", "oci://ghcr.io/x/y:v1", InputFormOCI, false},
		{"bare oci with digest", "ghcr.io/owner/repo@sha256:abc", InputFormOCI, false},
		{"bare oci with tag", "ghcr.io/owner/repo:v1", InputFormOCI, false},
		{"localhost registry", "localhost:5000/repo:v1", InputFormOCI, false},
		{"relative path rejected", "./out/summary-bundle", "", true},
		{"parent path rejected", "../some-bundle", "", true},
		{"nonsense rejected", "not-an-input", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DetectInputForm(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
