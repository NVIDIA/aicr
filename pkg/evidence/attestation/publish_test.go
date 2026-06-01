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

package attestation

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// emitUnsignedBundle produces a real on-disk, unsigned bundle (the
// artifact `validate --emit-attestation` without --push leaves behind)
// and returns the OutDir that holds summary-bundle/ + pointer.yaml.
func emitUnsignedBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	rec := &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			Intent:      recipe.CriteriaIntentTraining,
		},
	}
	_, err := Emit(context.Background(), EmitOptions{
		OutDir:      dir,
		Recipe:      rec,
		Snapshot:    &snapshotter.Snapshot{},
		AICRVersion: "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return dir
}

func wantInvalidRequest(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}

func TestPublish_RequiresPush(t *testing.T) {
	err := Publish(context.Background(), PublishOptions{BundleDir: t.TempDir()})
	wantInvalidRequest(t, err)
}

func TestPublish_InvalidPushReference(t *testing.T) {
	dir := emitUnsignedBundle(t)
	err := Publish(context.Background(), PublishOptions{
		BundleDir: dir,
		Push:      "oci://not a valid ref",
	})
	wantInvalidRequest(t, err)
}

func TestPublish_MissingBundleDir(t *testing.T) {
	// A valid push ref but a directory with no bundle markers must fail
	// before any network work.
	err := Publish(context.Background(), PublishOptions{
		BundleDir: t.TempDir(),
		Push:      "ghcr.io/example/aicr-evidence",
	})
	wantInvalidRequest(t, err)
}

func TestLoadOnDiskBundle_ParentDir(t *testing.T) {
	dir := emitUnsignedBundle(t)

	bundle, outDir, err := loadOnDiskBundle(dir)
	if err != nil {
		t.Fatalf("loadOnDiskBundle: %v", err)
	}
	if outDir != filepath.Clean(dir) {
		t.Errorf("outDir = %q, want %q (pointer beside summary-bundle/)", outDir, dir)
	}
	if bundle.SummaryDir != filepath.Join(dir, SummaryBundleDirName) {
		t.Errorf("SummaryDir = %q, want %q", bundle.SummaryDir, filepath.Join(dir, SummaryBundleDirName))
	}
	if bundle.RecipeName != "h100-eks-training" {
		t.Errorf("RecipeName = %q, want h100-eks-training", bundle.RecipeName)
	}
	if bundle.Predicate == nil || bundle.SubjectDigest == "" {
		t.Fatalf("expected populated Predicate + SubjectDigest, got %+v", bundle)
	}
	if bundle.SubjectDigest != bundle.Predicate.Recipe.Digest {
		t.Errorf("SubjectDigest %q != predicate.recipe.digest %q",
			bundle.SubjectDigest, bundle.Predicate.Recipe.Digest)
	}
}

func TestLoadOnDiskBundle_SummaryDirItself(t *testing.T) {
	dir := emitUnsignedBundle(t)
	summaryDir := filepath.Join(dir, SummaryBundleDirName)

	bundle, outDir, err := loadOnDiskBundle(summaryDir)
	if err != nil {
		t.Fatalf("loadOnDiskBundle: %v", err)
	}
	// Pointing at summary-bundle/ directly puts the pointer in its parent,
	// matching the one-shot output layout.
	if outDir != filepath.Clean(dir) {
		t.Errorf("outDir = %q, want parent %q", outDir, dir)
	}
	if bundle.SummaryDir != filepath.Clean(summaryDir) {
		t.Errorf("SummaryDir = %q, want %q", bundle.SummaryDir, summaryDir)
	}
}

func TestLoadOnDiskBundle_EmptyArg(t *testing.T) {
	_, _, err := loadOnDiskBundle("")
	wantInvalidRequest(t, err)
}

func TestResolveSummaryDir_NotABundle(t *testing.T) {
	_, _, err := resolveSummaryDir(t.TempDir())
	wantInvalidRequest(t, err)
}

func TestReadBundlePredicate_MissingStatement(t *testing.T) {
	_, _, err := readBundlePredicate(t.TempDir())
	if err == nil {
		t.Fatalf("expected error for missing statement")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
}

func TestReadBundlePredicate_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, StatementFilename), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readBundlePredicate(dir)
	wantInvalidRequest(t, err)
}

func TestReadBundlePredicate_WrongPredicateType(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`{"predicateType":"https://example.com/other/v1","predicate":{}}`)
	if err := os.WriteFile(filepath.Join(dir, StatementFilename), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readBundlePredicate(dir)
	wantInvalidRequest(t, err)
}

func TestLoadOnDiskBundle_MissingRecipeIdentity(t *testing.T) {
	// A V1 statement whose predicate lacks recipe.{name,digest} must be
	// rejected — BuildArtifactStatement requires both downstream.
	dir := t.TempDir()
	summaryDir := filepath.Join(dir, SummaryBundleDirName)
	if err := os.MkdirAll(summaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{RecipeFilename, ManifestFilename} {
		if err := os.WriteFile(filepath.Join(summaryDir, f), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte(`{"predicateType":"` + PredicateTypeV1 + `","predicate":{"recipe":{}}}`)
	if err := os.WriteFile(filepath.Join(summaryDir, StatementFilename), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadOnDiskBundle(dir)
	wantInvalidRequest(t, err)
}
