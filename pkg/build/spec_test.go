// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/eidos/pkg/errors"
)

func TestLoadSpec(t *testing.T) {
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
}

func TestLoadSpec_CancelledContext(t *testing.T) {
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
	if spec.Spec.Registry.Repository != "eidos-runtime" {
		t.Errorf("Registry.Repository = %q, want %q", spec.Spec.Registry.Repository, "eidos-runtime")
	}
	if spec.APIVersion != ExpectedAPIVersion {
		t.Errorf("APIVersion = %q, want %q", spec.APIVersion, ExpectedAPIVersion)
	}
}

func TestLoadSpec_WithStatus(t *testing.T) {
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
			err := tt.spec.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildSpec_WriteBack(t *testing.T) {
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
