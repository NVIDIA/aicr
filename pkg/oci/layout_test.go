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

package oci

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	oras "oras.land/oras-go/v2"
	storeoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

func TestParseOutputTargetLayout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantDir string
		wantTag string
		wantErr bool
	}{
		{name: "directory only", input: "oci-layout:./layout", wantDir: "./layout"},
		{name: "directory and tag", input: "oci-layout:out:v1.2.3", wantDir: "out", wantTag: "v1.2.3"},
		{name: "absolute directory and tag", input: "oci-layout:/tmp/out:1.0.0", wantDir: "/tmp/out", wantTag: "1.0.0"},
		{name: "colon inside path is not a tag", input: "oci-layout:a:b/c", wantDir: "a:b/c"},
		{name: "invalid tag stays in path", input: "oci-layout:out:bad tag", wantDir: "out:bad tag"},
		{name: "empty", input: "oci-layout:", wantErr: true},
		{name: "tag without directory", input: "oci-layout::v1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref, err := ParseOutputTarget(tt.input)
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, apperrors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("ParseOutputTarget(%q) error = %v", tt.input, err)
			}
			if !ref.IsLayout || ref.IsOCI || ref.LocalPath != tt.wantDir || ref.Tag != tt.wantTag {
				t.Fatalf("ParseOutputTarget(%q) = %+v, want layout dir %q tag %q", tt.input, ref, tt.wantDir, tt.wantTag)
			}
			if ref.ChartName() != "" || ref.ParentNamespace() != "" {
				t.Fatalf("layout reference derived registry values: %+v", ref)
			}
			if tt.wantTag != "" && ref.String() != tt.input {
				t.Fatalf("String() = %q, want %q", ref.String(), tt.input)
			}
			if got := ref.WithTag("v9"); !got.IsLayout || got.LocalPath != tt.wantDir || got.Tag != "v9" {
				t.Fatalf("WithTag() = %+v", got)
			}
		})
	}
}

func writeBundleFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"README.md":                "bundle\n",
		"deploy.sh":                "#!/bin/sh\n",
		"gpu-operator/values.yaml": "driver:\n  enabled: true\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return dir
}

// assertLayoutPublishes checks the layout resolves tag to digest and that
// copying it to a registry, as `oras cp --from-oci-layout` does, publishes
// that same digest.
func assertLayoutPublishes(t *testing.T, layoutDir, tag, digest, registryHost string) {
	t.Helper()
	ctx := context.Background()
	store, err := storeoci.New(layoutDir)
	if err != nil {
		t.Fatalf("open layout: %v", err)
	}
	desc, err := store.Resolve(ctx, tag)
	if err != nil {
		t.Fatalf("resolve layout tag: %v", err)
	}
	if desc.Digest.String() != digest {
		t.Fatalf("layout tag digest = %s, want %s", desc.Digest, digest)
	}
	repo, err := remote.NewRepository(registryHost + "/copied/bundle")
	if err != nil {
		t.Fatalf("remote repository: %v", err)
	}
	repo.PlainHTTP = true
	copied, err := oras.Copy(ctx, store, tag, repo, tag, oras.DefaultCopyOptions)
	if err != nil {
		t.Fatalf("copy layout to registry: %v", err)
	}
	if copied.Digest.String() != digest {
		t.Fatalf("copied digest = %s, want %s", copied.Digest, digest)
	}
	if _, statErr := os.Stat(filepath.Join(layoutDir, rootStoreIngestDir)); !os.IsNotExist(statErr) {
		t.Fatalf("layout keeps store scratch directory: %v", statErr)
	}
}

func TestWriteLayoutMatchesPushedDigest(t *testing.T) {
	registry := newFakeOCIRegistry(t)
	t.Cleanup(registry.Close)
	host := strings.TrimPrefix(registry.URL, "http://")
	source := writeBundleFixture(t)

	pushed, err := PackageAndPush(context.Background(), OutputConfig{
		SourceDir: source,
		OutputDir: t.TempDir(),
		Reference: &Reference{IsOCI: true, Registry: host, Repository: "test/aicr-bundle", Tag: "v1.2.3"},
		Version:   "v1.2.3",
		PlainHTTP: true,
	})
	if err != nil {
		t.Fatalf("PackageAndPush() error = %v", err)
	}

	layoutDir := filepath.Join(t.TempDir(), "layout")
	written, err := WriteLayout(context.Background(), OutputConfig{
		SourceDir: source,
		OutputDir: t.TempDir(),
		Reference: &Reference{IsLayout: true, LocalPath: layoutDir, Tag: "v1.2.3"},
		Version:   "v1.2.3",
	})
	if err != nil {
		t.Fatalf("WriteLayout() error = %v", err)
	}
	if written.Digest != pushed.Digest {
		t.Fatalf("layout digest = %s, pushed digest = %s", written.Digest, pushed.Digest)
	}
	if written.StorePath != layoutDir || written.Reference != "oci-layout:"+layoutDir+":v1.2.3" {
		t.Fatalf("WriteLayout() = %+v", written)
	}
	assertLayoutPublishes(t, layoutDir, "v1.2.3", pushed.Digest, host)
}

func TestWriteHelmChartLayoutMatchesPushedDigest(t *testing.T) {
	registry := newFakeOCIRegistry(t)
	t.Cleanup(registry.Close)
	host := strings.TrimPrefix(registry.URL, "http://")
	source := writeHelmChartFixture(t, "aicr-bundle")

	pushed, err := PackageAndPushHelmChart(context.Background(), HelmChartOptions{
		SourceDir: source,
		OutputDir: t.TempDir(),
		Reference: &Reference{IsOCI: true, Registry: host, Repository: "test/aicr-bundle", Tag: "1.0.0"},
		Version:   "v1.0.0",
		PlainHTTP: true,
	})
	if err != nil {
		t.Fatalf("PackageAndPushHelmChart() error = %v", err)
	}

	layoutDir := filepath.Join(t.TempDir(), "chart")
	written, err := WriteHelmChartLayout(context.Background(), HelmChartOptions{
		SourceDir: source,
		OutputDir: t.TempDir(),
		Reference: &Reference{IsLayout: true, LocalPath: layoutDir, Tag: "1.0.0"},
		Version:   "v1.0.0",
	})
	if err != nil {
		t.Fatalf("WriteHelmChartLayout() error = %v", err)
	}
	if written.Digest != pushed.Digest {
		t.Fatalf("layout digest = %s, pushed digest = %s", written.Digest, pushed.Digest)
	}
	assertLayoutPublishes(t, layoutDir, "1.0.0", pushed.Digest, host)
}

func TestWriteLayoutFailsClosed(t *testing.T) {
	source := writeBundleFixture(t)

	tests := []struct {
		name     string
		setup    func(t *testing.T, parent string) *Reference
		wantCode apperrors.ErrorCode
	}{
		{
			name: "existing directory",
			setup: func(t *testing.T, parent string) *Reference {
				if err := os.Mkdir(filepath.Join(parent, "layout"), 0o755); err != nil {
					t.Fatal(err)
				}
				return &Reference{IsLayout: true, LocalPath: filepath.Join(parent, "layout"), Tag: "v1"}
			},
			wantCode: apperrors.ErrCodeConflict,
		},
		{
			name: "existing file",
			setup: func(t *testing.T, parent string) *Reference {
				if err := os.WriteFile(filepath.Join(parent, "layout"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return &Reference{IsLayout: true, LocalPath: filepath.Join(parent, "layout"), Tag: "v1"}
			},
			wantCode: apperrors.ErrCodeConflict,
		},
		{
			name: "missing parent",
			setup: func(_ *testing.T, parent string) *Reference {
				return &Reference{IsLayout: true, LocalPath: filepath.Join(parent, "missing", "layout"), Tag: "v1"}
			},
			wantCode: apperrors.ErrCodeInvalidRequest,
		},
		{
			name: "missing tag",
			setup: func(_ *testing.T, parent string) *Reference {
				return &Reference{IsLayout: true, LocalPath: filepath.Join(parent, "layout")}
			},
			wantCode: apperrors.ErrCodeInvalidRequest,
		},
		{
			name: "registry reference",
			setup: func(_ *testing.T, _ string) *Reference {
				return &Reference{IsOCI: true, Registry: "ghcr.io", Repository: "nvidia/bundle", Tag: "v1"}
			},
			wantCode: apperrors.ErrCodeInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			ref := tt.setup(t, parent)
			before := dirNames(t, parent)
			_, err := WriteLayout(context.Background(), OutputConfig{
				SourceDir: source,
				OutputDir: t.TempDir(),
				Reference: ref,
			})
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if after := dirNames(t, parent); strings.Join(after, ",") != strings.Join(before, ",") {
				t.Fatalf("parent entries changed from %v to %v", before, after)
			}
		})
	}
}

// TestOwnedLayoutReleaseAsRejectsLateTarget covers a target that appears
// after WriteLayout's early check: the release fails closed and removes the
// staged layout.
func TestOwnedLayoutReleaseAsRejectsLateTarget(t *testing.T) {
	parent := t.TempDir()
	layout, err := newOwnedLayout(context.Background(), parent)
	if err != nil {
		t.Fatalf("newOwnedLayout() error = %v", err)
	}
	if mkdirErr := os.Mkdir(filepath.Join(parent, "layout"), 0o755); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	_, err = layout.releaseAs("layout")
	errorstest.WantReportedCode(t, err, apperrors.ErrCodeConflict)
	if got := dirNames(t, parent); strings.Join(got, ",") != "layout" {
		t.Fatalf("parent entries = %v, want only the pre-existing target", got)
	}
	if closeErr := layout.Close(); closeErr == nil {
		t.Fatal("Close() after failed release returned nil, want the release error")
	}
}

func TestPackageAndPushHelmChartRejectsLayoutReference(t *testing.T) {
	_, err := PackageAndPushHelmChart(context.Background(), HelmChartOptions{
		SourceDir: writeHelmChartFixture(t, "aicr-bundle"),
		OutputDir: t.TempDir(),
		Reference: &Reference{IsLayout: true, LocalPath: filepath.Join(t.TempDir(), "x"), Tag: "1.0.0"},
	})
	errorstest.WantReportedCode(t, err, apperrors.ErrCodeInvalidRequest)
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestCheckLayoutTarget(t *testing.T) {
	parent := t.TempDir()
	existing := filepath.Join(parent, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		ref      *Reference
		wantCode apperrors.ErrorCode
	}{
		{name: "new directory", ref: &Reference{IsLayout: true, LocalPath: filepath.Join(parent, "new"), Tag: "v1"}},
		{name: "existing directory", ref: &Reference{IsLayout: true, LocalPath: existing, Tag: "v1"}, wantCode: apperrors.ErrCodeConflict},
		{name: "filesystem root", ref: &Reference{IsLayout: true, LocalPath: "/", Tag: "v1"}, wantCode: apperrors.ErrCodeInvalidRequest},
		{name: "invalid tag", ref: &Reference{IsLayout: true, LocalPath: filepath.Join(parent, "new"), Tag: "bad tag"}, wantCode: apperrors.ErrCodeInvalidRequest},
		{name: "nil reference", wantCode: apperrors.ErrCodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckLayoutTarget(tt.ref)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("CheckLayoutTarget() error = %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
		})
	}
}

func TestValidateChartName(t *testing.T) {
	tests := []struct {
		name    string
		chart   string
		wantErr bool
	}{
		{name: "default name", chart: "aicr-bundle"},
		{name: "dots and underscores", chart: "my.chart_v2"},
		{name: "empty", chart: "", wantErr: true},
		{name: "uppercase", chart: "MyChart", wantErr: true},
		{name: "path separator", chart: "org/chart", wantErr: true},
		{name: "parent directory", chart: "..", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateChartName(tt.chart)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ValidateChartName(%q) error = %v", tt.chart, err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, apperrors.ErrCodeInvalidRequest)
		})
	}
}
