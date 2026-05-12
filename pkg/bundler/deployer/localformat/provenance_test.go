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

package localformat

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestWriteProvenance(t *testing.T) {
	dir := t.TempDir()

	// Records intentionally out of name order; writer must sort.
	records := []VendorRecord{
		{Name: "zoo", Chart: "zoo", Version: "1", Repository: "https://r/z", SHA256: "z"},
		{Name: "alpha", Chart: "alpha", Version: "2", Repository: "oci://r/a", SHA256: "a"},
	}

	path, size, err := WriteProvenance(context.Background(), dir, records)
	if err != nil {
		t.Fatalf("WriteProvenance: %v", err)
	}
	if size <= 0 {
		t.Errorf("size = %d, want > 0", size)
	}
	if filepath.Base(path) != ProvenanceFileName {
		t.Errorf("unexpected basename: got %s, want %s", filepath.Base(path), ProvenanceFileName)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var got provenanceFile
	if err := yaml.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	if got.APIVersion != ProvenanceAPIVersion {
		t.Errorf("apiVersion = %q, want %q", got.APIVersion, ProvenanceAPIVersion)
	}
	if got.Kind != ProvenanceKind {
		t.Errorf("kind = %q, want %q", got.Kind, ProvenanceKind)
	}
	if len(got.VendoredCharts) != 2 {
		t.Fatalf("got %d entries, want 2", len(got.VendoredCharts))
	}
	if got.VendoredCharts[0].Name != "alpha" || got.VendoredCharts[1].Name != "zoo" {
		t.Errorf("entries not sorted by name: %+v", got.VendoredCharts)
	}
}

func TestWriteProvenance_EmptyRejected(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := WriteProvenance(context.Background(), dir, nil); err == nil {
		t.Error("expected error for empty records slice")
	}
}

func TestWriteProvenance_RespectsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := WriteProvenance(ctx, dir, []VendorRecord{
		{Name: "a", Chart: "a", Version: "1", Repository: "https://r", SHA256: "x"},
	})
	if err == nil {
		t.Fatal("expected error when context is already canceled")
	}
}
