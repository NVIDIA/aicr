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

package catalog_test

import (
	"context"
	stderrors "errors"
	"io/fs"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/catalog"
)

// stubProvider is an in-memory recipe.DataProvider used for testing
// catalog.ComputeDigest without touching the real embedded data.
type stubProvider struct {
	files map[string][]byte
}

func (p *stubProvider) ReadFile(_ context.Context, path string) ([]byte, error) {
	data, ok := p.files[path]
	if !ok {
		return nil, errors.New(errors.ErrCodeNotFound, "file not found: "+path)
	}
	return data, nil
}

func (p *stubProvider) WalkDir(_ context.Context, _ string, _ fs.WalkDirFunc) error {
	return nil
}

func (p *stubProvider) Source(path string) string {
	return "stub://" + path
}

func newFullProvider() *stubProvider {
	return &stubProvider{
		files: map[string][]byte{
			recipe.RegistryFileName: []byte("components:\n  - name: gpu-operator\n    displayName: GPU Operator\n"),
			recipe.CatalogFileName:  []byte("validators:\n  - id: k8s-version\n    description: Kubernetes version check\n"),
		},
	}
}

func TestComputeDigest_Deterministic(t *testing.T) {
	ctx := context.Background()
	p := newFullProvider()

	first, err := catalog.ComputeDigest(ctx, p)
	if err != nil {
		t.Fatalf("first ComputeDigest failed: %v", err)
	}
	second, err := catalog.ComputeDigest(ctx, p)
	if err != nil {
		t.Fatalf("second ComputeDigest failed: %v", err)
	}

	if first.Digest["sha256"] != second.Digest["sha256"] {
		t.Errorf("digests differ across calls: %q vs %q",
			first.Digest["sha256"], second.Digest["sha256"])
	}
	if len(first.ResolvedDependencies) != len(second.ResolvedDependencies) {
		t.Fatalf("dependency count mismatch: %d vs %d",
			len(first.ResolvedDependencies), len(second.ResolvedDependencies))
	}
	for i := range first.ResolvedDependencies {
		if first.ResolvedDependencies[i].Digest["sha256"] != second.ResolvedDependencies[i].Digest["sha256"] {
			t.Errorf("dependency %d digest differs: %q vs %q", i,
				first.ResolvedDependencies[i].Digest["sha256"],
				second.ResolvedDependencies[i].Digest["sha256"])
		}
	}
}

func TestComputeDigest_MissingRegistry(t *testing.T) {
	ctx := context.Background()
	p := &stubProvider{
		files: map[string][]byte{
			recipe.CatalogFileName: []byte("validators: []\n"),
		},
	}

	_, err := catalog.ComputeDigest(ctx, p)
	if err == nil {
		t.Fatal("expected error for missing registry, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
}

func TestComputeDigest_MissingCatalog(t *testing.T) {
	ctx := context.Background()
	p := &stubProvider{
		files: map[string][]byte{
			recipe.RegistryFileName: []byte("components: []\n"),
		},
	}

	_, err := catalog.ComputeDigest(ctx, p)
	if err == nil {
		t.Fatal("expected error for missing catalog, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
}

func TestComputeDigest_SubjectShape(t *testing.T) {
	ctx := context.Background()
	subject, err := catalog.ComputeDigest(ctx, newFullProvider())
	if err != nil {
		t.Fatalf("ComputeDigest failed: %v", err)
	}

	if subject.Name != "recipe-catalog" {
		t.Errorf("subject.Name = %q, want %q", subject.Name, "recipe-catalog")
	}
	if got := subject.Digest["sha256"]; got == "" {
		t.Error("subject.Digest[sha256] is empty")
	} else if len(got) != 64 {
		t.Errorf("subject.Digest[sha256] length = %d, want 64", len(got))
	}
	if len(subject.ResolvedDependencies) != 2 {
		t.Fatalf("ResolvedDependencies len = %d, want 2", len(subject.ResolvedDependencies))
	}

	wantURIs := map[string]bool{
		"file://" + recipe.RegistryFileName: false,
		"file://" + recipe.CatalogFileName:  false,
	}
	for _, dep := range subject.ResolvedDependencies {
		if _, ok := wantURIs[dep.URI]; !ok {
			t.Errorf("unexpected dependency URI: %q", dep.URI)
			continue
		}
		wantURIs[dep.URI] = true
		if got := dep.Digest["sha256"]; got == "" || len(got) != 64 {
			t.Errorf("dependency %q sha256 invalid: %q", dep.URI, got)
		}
	}
	for uri, seen := range wantURIs {
		if !seen {
			t.Errorf("missing expected dependency URI: %q", uri)
		}
	}
}
