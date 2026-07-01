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

package main

import "testing"

func TestExtractGeneratedSection(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    string
		wantErr bool
	}{
		{
			name: "extracts between markers",
			doc:  "prose\n" + bomBeginMarker + "\nGEN\n" + bomEndMarker + "\nmore prose",
			want: "\nGEN\n",
		},
		{
			name:    "missing begin marker",
			doc:     "prose\n" + bomEndMarker + "\n",
			wantErr: true,
		},
		{
			name:    "missing end marker",
			doc:     "prose\n" + bomBeginMarker + "\nGEN\n",
			wantErr: true,
		},
		{
			name:    "end before begin",
			doc:     bomEndMarker + "\nGEN\n" + bomBeginMarker,
			wantErr: true,
		},
		{
			name:    "duplicate begin marker",
			doc:     bomBeginMarker + "\nGEN\n" + bomEndMarker + "\n" + bomBeginMarker + "\nSTALE\n" + bomEndMarker,
			wantErr: true,
		},
		{
			name:    "duplicate end marker",
			doc:     bomBeginMarker + "\nGEN\n" + bomEndMarker + "\nSTALE\n" + bomEndMarker,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractGeneratedSection(tt.doc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseBOMVersionTable(t *testing.T) {
	const header = "| Component | Type | Chart | Pinned Version | Images |\n" +
		"|-----------|------|-------|----------------|--------|\n"

	tests := []struct {
		name    string
		section string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "parses rows and resolves columns by header",
			section: header +
				"| gpu-operator | helm | gpu-operator | v26.3.2 | 9 |\n" +
				"| some-kustomize | kustomize | — | v1.2.3 | 2 |\n" +
				"| a-manifest | manifest | — | — | 1 |\n",
			want: map[string]string{
				"gpu-operator":   "v26.3.2",
				"some-kustomize": "v1.2.3",
				"a-manifest":     "—",
			},
		},
		{
			name: "rejects duplicate component rows",
			section: header +
				"| gpu-operator | helm | gpu-operator | v26.3.2 | 9 |\n" +
				"| gpu-operator | helm | gpu-operator | v0.0.1 | 9 |\n",
			wantErr: true,
		},
		{
			name: "ignores an unrelated pipe table before the components table",
			section: "| Other | Column |\n|-------|--------|\n| foo | bar |\n\n" +
				header +
				"| gpu-operator | helm | gpu-operator | v26.3.2 | 9 |\n",
			want: map[string]string{"gpu-operator": "v26.3.2"},
		},
		{
			name:    "returns empty for a section with no components table",
			section: "just prose, no table\n",
			want:    map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBOMVersionTable(tt.section)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d rows, want %d: %v", len(got), len(tt.want), got)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("row %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
