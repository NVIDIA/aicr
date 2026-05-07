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

package build

import (
	"context"
	stderrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestLoadSpec(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name    string
		file    string
		wantErr bool
	}{
		{
			name:    "valid spec",
			file:    "testdata/valid_spec.yaml",
			wantErr: false,
		},
		{
			name:    "valid spec with recipe",
			file:    "testdata/valid_spec_with_recipe.yaml",
			wantErr: false,
		},
		{
			name:    "spec with existing status",
			file:    "testdata/spec_with_status.yaml",
			wantErr: false,
		},
		{
			name:    "invalid yaml",
			file:    "testdata/invalid_yaml.yaml",
			wantErr: true,
		},
		{
			name:    "nonexistent file",
			file:    "testdata/does_not_exist.yaml",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spec, err := LoadSpec(ctx, tt.file)
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadSpec() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && spec == nil {
				t.Error("LoadSpec() returned nil spec without error")
			}
		})
	}
}

func TestLoadSpec_NotFound(t *testing.T) {
	t.Parallel()

	_, err := LoadSpec(context.Background(), "testdata/does_not_exist.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}

	var sErr *errors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != errors.ErrCodeNotFound {
		t.Errorf("error code = %v, want %v", sErr.Code, errors.ErrCodeNotFound)
	}
	// The structured error must still chain to fs.ErrNotExist so callers
	// using stderrors.Is continue to work after the os.IsNotExist swap.
	if !stderrors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected error to wrap fs.ErrNotExist, chain = %v", err)
	}
}

func TestLoadSpec_CancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := LoadSpec(ctx, "testdata/valid_spec.yaml")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}

	var sErr *errors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != errors.ErrCodeTimeout {
		t.Errorf("error code = %v, want %v", sErr.Code, errors.ErrCodeTimeout)
	}
}

func TestLoadSpec_Fields(t *testing.T) {
	t.Parallel()

	spec, err := LoadSpec(context.Background(), "testdata/valid_spec.yaml")
	if err != nil {
		t.Fatalf("LoadSpec() unexpected error: %v", err)
	}

	if spec.Spec.Recipe != "/data/recipes/eks-training.yaml" {
		t.Errorf("Recipe = %q, want %q", spec.Spec.Recipe, "/data/recipes/eks-training.yaml")
	}
	if spec.Spec.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", spec.Spec.Version, "1.0.0")
	}
	if spec.Spec.Registry.Host != "https://registry.example.com" {
		t.Errorf("Registry.Host = %q, want %q", spec.Spec.Registry.Host, "https://registry.example.com")
	}
	if spec.Spec.Registry.Repository != "aicr-runtime" {
		t.Errorf("Registry.Repository = %q, want %q", spec.Spec.Registry.Repository, "aicr-runtime")
	}
	if spec.APIVersion != ExpectedAPIVersion {
		t.Errorf("APIVersion = %q, want %q", spec.APIVersion, ExpectedAPIVersion)
	}
}

func TestLoadSpec_WithStatus(t *testing.T) {
	t.Parallel()

	spec, err := LoadSpec(context.Background(), "testdata/spec_with_status.yaml")
	if err != nil {
		t.Fatalf("LoadSpec() unexpected error: %v", err)
	}

	if spec.Status.Images == nil {
		t.Fatal("Status.Images is nil, expected map")
	}
	charts, ok := spec.Status.Images["charts"]
	if !ok {
		t.Fatal("Status.Images missing 'charts' key")
	}
	if charts.Registry != "registry.example.com" {
		t.Errorf("charts.Registry = %q, want %q", charts.Registry, "registry.example.com")
	}
	if charts.Digest != "sha256:abcdef1234567890" {
		t.Errorf("charts.Digest = %q, want %q", charts.Digest, "sha256:abcdef1234567890")
	}
}

func TestBuildSpec_Validate(t *testing.T) {
	t.Parallel()

	validBase := func() BuildSpec {
		return BuildSpec{
			APIVersion: ExpectedAPIVersion,
			Kind:       ExpectedKind,
		}
	}

	tests := []struct {
		name    string
		spec    BuildSpec
		wantErr bool
	}{
		{
			name: "valid with recipe",
			spec: func() BuildSpec {
				s := validBase()
				s.Spec = BuildSpecConfig{
					Recipe:   "/data/recipes/eks-training.yaml",
					Registry: RegistryConfig{Host: "registry.example.com", Repository: "test"},
				}
				return s
			}(),
			wantErr: false,
		},
		{
			name: "wrong apiVersion",
			spec: BuildSpec{
				APIVersion: "wrong/v1",
				Kind:       ExpectedKind,
				Spec: BuildSpecConfig{
					Recipe:   "/data/recipes/eks-training.yaml",
					Registry: RegistryConfig{Host: "registry.example.com", Repository: "test"},
				},
			},
			wantErr: true,
		},
		{
			name: "wrong kind",
			spec: BuildSpec{
				APIVersion: ExpectedAPIVersion,
				Kind:       "WrongKind",
				Spec: BuildSpecConfig{
					Recipe:   "/data/recipes/eks-training.yaml",
					Registry: RegistryConfig{Host: "registry.example.com", Repository: "test"},
				},
			},
			wantErr: true,
		},
		{
			name: "missing registry host",
			spec: func() BuildSpec {
				s := validBase()
				s.Spec = BuildSpecConfig{
					Recipe:   "/data/recipes/eks-training.yaml",
					Registry: RegistryConfig{Repository: "test"},
				}
				return s
			}(),
			wantErr: true,
		},
		{
			name: "missing registry repository",
			spec: func() BuildSpec {
				s := validBase()
				s.Spec = BuildSpecConfig{
					Recipe:   "/data/recipes/eks-training.yaml",
					Registry: RegistryConfig{Host: "registry.example.com"},
				}
				return s
			}(),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.spec.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildSpec_WriteBack(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	spec := &BuildSpec{
		APIVersion: ExpectedAPIVersion,
		Kind:       ExpectedKind,
		Spec: BuildSpecConfig{
			Recipe:   "/data/recipes/eks-training.yaml",
			Version:  "1.0.0",
			Registry: RegistryConfig{Host: "registry.example.com", Repository: "test"},
		},
	}

	spec.SetImageStatus("charts", ImageStatus{
		Path:       "/tmp/output/charts",
		Registry:   "registry.example.com",
		Repository: "test/charts",
		Tag:        "eks-training-1.0.0",
		Digest:     "sha256:abc123",
	})

	dir := t.TempDir()
	outPath := filepath.Join(dir, "spec.yaml")

	if err := spec.WriteBack(ctx, outPath); err != nil {
		t.Fatalf("WriteBack() unexpected error: %v", err)
	}

	// Read back and verify
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}

	var readBack BuildSpec
	if err := yaml.Unmarshal(data, &readBack); err != nil {
		t.Fatalf("failed to unmarshal written file: %v", err)
	}

	if readBack.Spec.Recipe != "/data/recipes/eks-training.yaml" {
		t.Errorf("Recipe = %q, want %q", readBack.Spec.Recipe, "/data/recipes/eks-training.yaml")
	}

	charts, ok := readBack.Status.Images["charts"]
	if !ok {
		t.Fatal("Status.Images missing 'charts' after writeback")
	}
	if charts.Digest != "sha256:abc123" {
		t.Errorf("charts.Digest = %q, want %q", charts.Digest, "sha256:abc123")
	}
	if charts.Tag != "eks-training-1.0.0" {
		t.Errorf("charts.Tag = %q, want %q", charts.Tag, "eks-training-1.0.0")
	}
}

func TestBuildSpec_WriteBack_CancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	spec := &BuildSpec{}
	dir := t.TempDir()
	outPath := filepath.Join(dir, "spec.yaml")

	err := spec.WriteBack(ctx, outPath)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}

	var sErr *errors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != errors.ErrCodeTimeout {
		t.Errorf("error code = %v, want %v", sErr.Code, errors.ErrCodeTimeout)
	}
}

func TestBuildSpec_SetImageStatus(t *testing.T) {
	t.Parallel()

	spec := &BuildSpec{}

	// First call should initialize the map
	spec.SetImageStatus("charts", ImageStatus{
		Registry:   "registry.example.com",
		Repository: "test/charts",
		Tag:        "v1.0.0",
	})

	if spec.Status.Images == nil {
		t.Fatal("Status.Images is nil after SetImageStatus")
	}

	charts, ok := spec.Status.Images["charts"]
	if !ok {
		t.Fatal("'charts' not found in Status.Images")
	}
	if charts.Tag != "v1.0.0" {
		t.Errorf("Tag = %q, want %q", charts.Tag, "v1.0.0")
	}

	// Second call should add to existing map
	spec.SetImageStatus("apps", ImageStatus{
		Registry:   "registry.example.com",
		Repository: "test/apps",
		Tag:        "v1.0.0",
	})

	if len(spec.Status.Images) != 2 {
		t.Errorf("len(Status.Images) = %d, want 2", len(spec.Status.Images))
	}
}

func TestLoadSpec_OversizeFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "huge.yaml")

	// Build a payload larger than MaxSpecFileBytes. Use valid YAML so the
	// size check is what trips, not the parser.
	var b strings.Builder
	b.WriteString("apiVersion: aicr.nvidia.com/v1beta1\nkind: AICRRuntime\nspec:\n  registry:\n    host: r\n    repository: r\n  comment: \"")
	pad := strings.Repeat("x", defaults.MaxSpecFileBytes+16)
	b.WriteString(pad)
	b.WriteString("\"\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	_, err := LoadSpec(context.Background(), path)
	if err == nil {
		t.Fatal("expected error for oversize file")
	}

	var sErr *errors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("error code = %v, want %v", sErr.Code, errors.ErrCodeInvalidRequest)
	}
}

func TestLoadSpec_UnknownField(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "typo.yaml")

	// "regestry" instead of "registry" — KnownFields(true) must reject it.
	content := `apiVersion: aicr.nvidia.com/v1beta1
kind: AICRRuntime
spec:
  regestry:
    host: registry.example.com
    repository: aicr-runtime
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	_, err := LoadSpec(context.Background(), path)
	if err == nil {
		t.Fatal("expected error for unknown YAML field")
	}

	var sErr *errors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("error code = %v, want %v", sErr.Code, errors.ErrCodeInvalidRequest)
	}
	if !strings.Contains(err.Error(), "regestry") {
		t.Errorf("expected error message to contain unknown key %q, got %q", "regestry", err.Error())
	}
}

func TestBuildSpec_WriteBack_AtomicNoTempLeftover(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	spec := &BuildSpec{
		APIVersion: ExpectedAPIVersion,
		Kind:       ExpectedKind,
		Spec: BuildSpecConfig{
			Recipe:   "/data/recipes/eks-training.yaml",
			Version:  "1.0.0",
			Registry: RegistryConfig{Host: "registry.example.com", Repository: "test"},
		},
	}

	dir := t.TempDir()
	outPath := filepath.Join(dir, "spec.yaml")

	if err := spec.WriteBack(ctx, outPath); err != nil {
		t.Fatalf("WriteBack() unexpected error: %v", err)
	}

	// Destination must exist with restrictive perms.
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if got := info.Mode().Perm(); got != defaults.SpecFileMode.Perm() {
		t.Errorf("dest perms = %v, want %v", got, defaults.SpecFileMode.Perm())
	}

	// .tmp-* siblings must not remain after a successful write.
	matches, err := filepath.Glob(outPath + ".tmp-*")
	if err != nil {
		t.Fatalf("glob tmp leftovers: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no .tmp-* leftover, got %v", matches)
	}

	// Repeated writes must overwrite cleanly without leaking the .tmp file.
	spec.SetImageStatus("charts", ImageStatus{
		Registry:   "registry.example.com",
		Repository: "test/charts",
		Tag:        "v2",
	})
	if err = spec.WriteBack(ctx, outPath); err != nil {
		t.Fatalf("WriteBack() second call: %v", err)
	}
	matches2, err := filepath.Glob(outPath + ".tmp-*")
	if err != nil {
		t.Fatalf("glob tmp leftovers (rewrite): %v", err)
	}
	if len(matches2) != 0 {
		t.Errorf("expected no .tmp-* leftover after rewrite, got %v", matches2)
	}
}

// TestBuildSpec_WriteBack_RenameFailurePreservesOriginal verifies that when
// the atomic rename step fails (after a successful temp write), WriteBack
// returns an error, the existing destination remains byte-for-byte
// unchanged, and the temp file is cleaned up. Uses the renameFn seam to
// inject a deterministic rename failure.
func TestBuildSpec_WriteBack_RenameFailurePreservesOriginal(t *testing.T) {
	// Not parallel: mutates package-level renameFn.

	ctx := context.Background()

	// Seed an existing destination with known content.
	original := &BuildSpec{
		APIVersion: ExpectedAPIVersion,
		Kind:       ExpectedKind,
		Spec: BuildSpecConfig{
			Recipe:   "/data/recipes/original.yaml",
			Registry: RegistryConfig{Host: "registry.example.com", Repository: "test"},
		},
	}
	dir := t.TempDir()
	outPath := filepath.Join(dir, "spec.yaml")
	if err := original.WriteBack(ctx, outPath); err != nil {
		t.Fatalf("seed WriteBack: %v", err)
	}
	originalBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}

	// Inject a rename failure for the next WriteBack call.
	injected := stderrors.New("injected rename failure")
	prev := renameFn
	renameFn = func(string, string) error { return injected }
	t.Cleanup(func() { renameFn = prev })

	updated := &BuildSpec{
		APIVersion: ExpectedAPIVersion,
		Kind:       ExpectedKind,
		Spec: BuildSpecConfig{
			Recipe:   "/data/recipes/updated.yaml",
			Registry: RegistryConfig{Host: "registry.example.com", Repository: "test"},
		},
	}
	err = updated.WriteBack(ctx, outPath)
	if err == nil {
		t.Fatal("expected WriteBack to fail when rename is stubbed to fail")
	}
	if !stderrors.Is(err, injected) {
		t.Errorf("expected wrapped injected rename failure, got %v", err)
	}
	var sErr *errors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != errors.ErrCodeInternal {
		t.Errorf("error code = %v, want %v", sErr.Code, errors.ErrCodeInternal)
	}

	// Original destination must be untouched.
	currentBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read after failed rename: %v", err)
	}
	if string(currentBytes) != string(originalBytes) {
		t.Errorf("original file was modified after failed rename:\nbefore: %s\nafter:  %s",
			originalBytes, currentBytes)
	}

	// Temp file should have been cleaned up.
	matches, err := filepath.Glob(outPath + ".tmp-*")
	if err != nil {
		t.Fatalf("glob tmp leftovers: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no .tmp-* leftover after failed rename, got %v", matches)
	}
}

// TestBuildSpec_WriteBack_TempCreateFailurePreservesOriginal verifies that
// when the temp file cannot be created (e.g., the destination directory is
// read-only), WriteBack returns an error and the existing destination file
// remains byte-for-byte unchanged. This exercises the os.CreateTemp failure
// branch — the rename branch is unreachable here.
func TestBuildSpec_WriteBack_TempCreateFailurePreservesOriginal(t *testing.T) {
	// Not parallel: chmod's effect is process-wide; concurrent tests in
	// the same temp dir would race.
	if runtime.GOOS == "windows" {
		t.Skip("chmod-readonly directory blocking is POSIX-specific")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	ctx := context.Background()

	// Seed an existing destination with known content.
	original := &BuildSpec{
		APIVersion: ExpectedAPIVersion,
		Kind:       ExpectedKind,
		Spec: BuildSpecConfig{
			Recipe:   "/data/recipes/original.yaml",
			Registry: RegistryConfig{Host: "registry.example.com", Repository: "test"},
		},
	}
	dir := t.TempDir()
	outPath := filepath.Join(dir, "spec.yaml")
	if err := original.WriteBack(ctx, outPath); err != nil {
		t.Fatalf("seed WriteBack: %v", err)
	}
	originalBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}

	// Make the directory read-only so os.CreateTemp fails inside WriteBack.
	// Restore writability on test exit so t.TempDir's cleanup can run.
	if err = os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir read-only: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
	})

	updated := &BuildSpec{
		APIVersion: ExpectedAPIVersion,
		Kind:       ExpectedKind,
		Spec: BuildSpecConfig{
			Recipe:   "/data/recipes/updated.yaml",
			Registry: RegistryConfig{Host: "registry.example.com", Repository: "test"},
		},
	}
	err = updated.WriteBack(ctx, outPath)
	if err == nil {
		t.Fatal("expected WriteBack to fail when destination directory is read-only")
	}
	var sErr *errors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != errors.ErrCodeInternal {
		t.Errorf("error code = %v, want %v", sErr.Code, errors.ErrCodeInternal)
	}

	// Original destination must be untouched.
	currentBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read after failed WriteBack: %v", err)
	}
	if string(currentBytes) != string(originalBytes) {
		t.Errorf("original file was modified after failed WriteBack:\nbefore: %s\nafter:  %s",
			originalBytes, currentBytes)
	}

	// And no .tmp-* debris should remain alongside it.
	matches, err := filepath.Glob(filepath.Join(dir, "spec.yaml.tmp-*"))
	if err != nil {
		t.Fatalf("glob tmp leftovers: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no .tmp-* leftover after failed WriteBack, got %v", matches)
	}
}
