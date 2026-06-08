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
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/catalog"
)

func TestSign_NoOp(t *testing.T) {
	ctx := context.Background()
	outPath := filepath.Join(t.TempDir(), "bundle.json")

	result, err := catalog.Sign(ctx, newFullProvider(), catalog.SignOptions{
		Attester:    attestation.NewNoOpAttester(),
		Output:      outPath,
		ToolVersion: "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	if result.BundleJSON != nil {
		t.Errorf("BundleJSON = %d bytes, want nil for NoOpAttester", len(result.BundleJSON))
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Errorf("expected no file at %q, stat err = %v", outPath, statErr)
	}
}

func TestSign_ReturnsDigest(t *testing.T) {
	ctx := context.Background()
	p := newFullProvider()

	result, err := catalog.Sign(ctx, p, catalog.SignOptions{
		Attester:    attestation.NewNoOpAttester(),
		ToolVersion: "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	if result.Digest == "" {
		t.Fatal("result.Digest is empty")
	}
	if len(result.Digest) != 64 {
		t.Errorf("result.Digest length = %d, want 64", len(result.Digest))
	}

	subject, err := catalog.ComputeDigest(ctx, p)
	if err != nil {
		t.Fatalf("ComputeDigest failed: %v", err)
	}
	if subject.Digest["sha256"] != result.Digest {
		t.Errorf("Sign digest %q != ComputeDigest %q", result.Digest, subject.Digest["sha256"])
	}
}

// fakeAttester is a minimal Attester that returns canned bytes from Attest,
// used to exercise Sign's bundle-write path without invoking real signing.
type fakeAttester struct{ bundle []byte }

func (f *fakeAttester) Attest(_ context.Context, _ attestation.AttestSubject) ([]byte, error) {
	return f.bundle, nil
}
func (f *fakeAttester) Identity() string    { return "fake" }
func (f *fakeAttester) HasRekorEntry() bool { return false }

func TestSign_WritesBundleToFile(t *testing.T) {
	ctx := context.Background()
	outPath := filepath.Join(t.TempDir(), "bundle.sigstore.json")
	want := []byte(`{"test":"bundle"}`)

	result, err := catalog.Sign(ctx, newFullProvider(), catalog.SignOptions{
		Attester:    &fakeAttester{bundle: want},
		Output:      outPath,
		ToolVersion: "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	if string(result.BundleJSON) != string(want) {
		t.Errorf("result.BundleJSON = %q, want %q", result.BundleJSON, want)
	}
	got, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatalf("read output: %v", readErr)
	}
	if string(got) != string(want) {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

func TestSign_MissingRegistry(t *testing.T) {
	ctx := context.Background()
	p := &stubProvider{
		files: map[string][]byte{
			recipe.CatalogFileName: []byte("validators: []\n"),
		},
	}

	_, err := catalog.Sign(ctx, p, catalog.SignOptions{
		Attester:    attestation.NewNoOpAttester(),
		ToolVersion: "v0.0.0-test",
	})
	if err == nil {
		t.Fatal("expected error for missing registry, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
}

func TestSign_RejectsNilAttester(t *testing.T) {
	_, err := catalog.Sign(context.Background(), newFullProvider(), catalog.SignOptions{
		ToolVersion: "v0.0.0-test",
	})
	if err == nil {
		t.Fatal("expected error for nil Attester, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}
