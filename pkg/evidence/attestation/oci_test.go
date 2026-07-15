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
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
)

func TestPush_RejectsEmptyOpts(t *testing.T) {
	cases := []struct {
		name string
		opts PushOptions
	}{
		{"empty SourceDir", PushOptions{Reference: "oci://ghcr.io/x/y"}},
		{"empty Reference", PushOptions{SourceDir: "/tmp/x"}},
		{"non-OCI Reference", PushOptions{SourceDir: "/tmp/x", Reference: "/local/path"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Push(context.Background(), tt.opts); err == nil {
				t.Errorf("expected error for %s", tt.name)
			}
		})
	}
}

func TestAttachSigstoreBundleAsReferrerRequiresExcludedRoots(t *testing.T) {
	result, err := AttachSigstoreBundleAsReferrer(context.Background(), AttachReferrerOptions{
		Reference:  "oci://ghcr.io/test/evidence:1.2.3",
		BundleJSON: []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`),
		MainArtifact: MainArtifactDescriptor{
			Digest:    "sha256:" + strings.Repeat("a", 64),
			MediaType: ociv1.MediaTypeImageManifest,
			Size:      123,
		},
	})
	if !stderrors.Is(err, apperrors.New(apperrors.ErrCodeInvalidRequest, "")) ||
		!strings.Contains(err.Error(), "ExcludedRoots") {

		t.Fatalf("error = %v, want InvalidRequest for missing ExcludedRoots", err)
	}
	if result != nil {
		t.Fatalf("result = %+v without ExcludedRoots", result)
	}
}

func TestAttachSigstoreBundleAsReferrerForwardsExactExcludedRoots(t *testing.T) {
	excludedRoots := []string{t.TempDir(), t.TempDir()}
	var captured oci.ReferrerOptions
	deps := attachReferrerDependencies{
		pushReferrer: func(
			_ context.Context,
			opts oci.ReferrerOptions,
		) (*oci.PushResult, error) {

			captured = opts
			return &oci.PushResult{
				Digest:    "sha256:" + strings.Repeat("b", 64),
				MediaType: ociv1.MediaTypeImageManifest,
				Size:      456,
				Reference: "ghcr.io/test/evidence@sha256:" + strings.Repeat("b", 64),
			}, nil
		},
	}
	result, err := attachSigstoreBundleAsReferrerWithDependencies(
		context.Background(),
		AttachReferrerOptions{
			Reference:     "oci://ghcr.io/test/evidence:1.2.3",
			BundleJSON:    []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`),
			ExcludedRoots: excludedRoots,
			MainArtifact: MainArtifactDescriptor{
				Digest:    "sha256:" + strings.Repeat("a", 64),
				MediaType: ociv1.MediaTypeImageManifest,
				Size:      123,
			},
		},
		deps,
	)
	if err != nil || result == nil {
		t.Fatalf("attach result=%+v error=%v", result, err)
	}
	if !reflect.DeepEqual(captured.ExcludedRoots, excludedRoots) {
		t.Fatalf("forwarded ExcludedRoots = %#v, want %#v", captured.ExcludedRoots, excludedRoots)
	}
}

func TestSignBundle_RequiresBundleAndSigner(t *testing.T) {
	if _, err := SignBundle(context.Background(), nil, NoOpSigner{}); err == nil {
		t.Errorf("expected error on nil bundle")
	}
	if _, err := SignBundle(context.Background(), &Bundle{}, nil); err == nil {
		t.Errorf("expected error on nil signer")
	}
}

func TestPublishOCIRecursiveNilSourceFiles(t *testing.T) {
	sourceDir := t.TempDir()
	tempRoot := t.TempDir()
	writeEvidenceFixture(t, sourceDir)
	before := snapshotEvidenceTree(t, sourceDir)
	if err := os.Chmod(tempRoot, 0o700); err != nil {
		t.Fatalf("Chmod(%q): %v", tempRoot, err)
	}
	t.Setenv("TMPDIR", tempRoot)

	var packagedFiles map[string]string
	deps := evidencePushDependencies{
		newWorkspace: oci.NewPrivateWorkspace,
		packageAndPush: func(
			ctx context.Context,
			cfg oci.OutputConfig,
		) (*oci.PackageAndPushResult, error) {

			if cfg.SourceFiles != nil {
				t.Fatalf("evidence SourceFiles = %#v, want nil recursive selection", cfg.SourceFiles)
			}
			if cfg.SubDir != "" {
				t.Fatalf("evidence SubDir = %q, want empty", cfg.SubDir)
			}
			if cfg.Reference == nil {
				t.Fatal("evidence package reference is nil")
			}
			result, err := oci.Package(ctx, oci.PackageOptions{
				SourceDir:   cfg.SourceDir,
				OutputDir:   cfg.OutputDir,
				SourceFiles: cfg.SourceFiles,
				SubDir:      cfg.SubDir,
				Registry:    cfg.Reference.Registry,
				Repository:  cfg.Reference.Repository,
				Tag:         cfg.Reference.Tag,
				Annotations: cfg.Annotations,
			})
			if err != nil {
				return nil, err
			}
			packagedFiles = readEvidencePackageFiles(t, result.StorePath, result.Digest)
			return &oci.PackageAndPushResult{
				Digest:    result.Digest,
				MediaType: result.MediaType,
				Size:      result.Size,
				Reference: result.Reference,
				StorePath: result.StorePath,
			}, nil
		},
	}

	result, err := pushWithDependencies(context.Background(), PushOptions{
		SourceDir:   sourceDir,
		Reference:   "oci://ghcr.io/test/evidence:1.2.3",
		AICRVersion: "1.2.3",
	}, deps)
	if err != nil {
		t.Fatalf("pushWithDependencies() error = %v", err)
	}
	if result == nil || result.Digest == "" {
		t.Fatalf("pushWithDependencies() result = %+v, want packaged descriptor", result)
	}
	wantFiles := map[string]string{
		"summary.json":   `{"status":"passed"}`,
		"nested/log.txt": "validator output\n",
	}
	if !reflect.DeepEqual(packagedFiles, wantFiles) {
		t.Fatalf("packaged evidence files = %#v, want %#v", packagedFiles, wantFiles)
	}
	if after := snapshotEvidenceTree(t, sourceDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("evidence source changed: before=%v after=%v", before, after)
	}
	assertEvidenceDirectoryEmpty(t, tempRoot)
}

func TestPublishOCIRejectsInSourceTempBeforeRegistryIO(t *testing.T) {
	parent := t.TempDir()
	sourceDir := filepath.Join(parent, "evidence")
	tempRoot := filepath.Join(sourceDir, "tmp")
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", tempRoot, err)
	}
	writeEvidenceFixture(t, sourceDir)
	before := snapshotEvidenceTree(t, sourceDir)
	t.Setenv("TMPDIR", tempRoot)

	packageCalled := false
	deps := evidencePushDependencies{
		newWorkspace: oci.NewPrivateWorkspace,
		packageAndPush: func(
			context.Context,
			oci.OutputConfig,
		) (*oci.PackageAndPushResult, error) {

			packageCalled = true
			return nil, stderrors.New("unexpected registry call")
		},
	}
	result, err := pushWithDependencies(context.Background(), PushOptions{
		SourceDir:   sourceDir,
		Reference:   "oci://ghcr.io/test/evidence:1.2.3",
		AICRVersion: "1.2.3",
	}, deps)
	if !stderrors.Is(err, apperrors.New(apperrors.ErrCodeInternal, "")) {
		t.Fatalf("pushWithDependencies() error = %v, want %s", err, apperrors.ErrCodeInternal)
	}
	if result != nil {
		t.Fatalf("pushWithDependencies() result = %+v on unsafe temp topology", result)
	}
	if packageCalled {
		t.Fatal("package/registry helper called after unsafe temp topology")
	}
	if after := snapshotEvidenceTree(t, sourceDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("evidence source changed: before=%v after=%v", before, after)
	}
	assertEvidenceDirectoryEmpty(t, tempRoot)
}

func writeEvidenceFixture(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"summary.json":   `{"status":"passed"}`,
		"nested/log.txt": "validator output\n",
	}
	for rel, body := range files {
		fullPath := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(fullPath), err)
		}
		if err := os.WriteFile(fullPath, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%q): %v", fullPath, err)
		}
	}
}

func readEvidencePackageFiles(t *testing.T, storePath, manifestDigest string) map[string]string {
	t.Helper()
	manifestPath := filepath.Join(
		storePath, "blobs", "sha256", strings.TrimPrefix(manifestDigest, "sha256:"))
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile(manifest): %v", err)
	}
	var manifest ociv1.Manifest
	if unmarshalErr := json.Unmarshal(manifestData, &manifest); unmarshalErr != nil {
		t.Fatalf("Unmarshal(manifest): %v", unmarshalErr)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("manifest layers = %d, want 1", len(manifest.Layers))
	}
	layerPath := filepath.Join(
		storePath, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	layer, err := os.Open(layerPath)
	if err != nil {
		t.Fatalf("Open(layer): %v", err)
	}
	defer func() { _ = layer.Close() }()
	gzipReader, err := gzip.NewReader(layer)
	if err != nil {
		t.Fatalf("gzip.NewReader(): %v", err)
	}
	defer func() { _ = gzipReader.Close() }()

	files := make(map[string]string)
	tarReader := tar.NewReader(gzipReader)
	for {
		header, nextErr := tarReader.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatalf("tar.Next(): %v", nextErr)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, readErr := io.ReadAll(tarReader)
		if readErr != nil {
			t.Fatalf("ReadAll(%q): %v", header.Name, readErr)
		}
		files[strings.TrimPrefix(header.Name, "./")] = string(data)
	}
	return files
}

func snapshotEvidenceTree(t *testing.T, root string) []string {
	t.Helper()
	var snapshot []string
	err := filepath.WalkDir(root, func(fullPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, fullPath)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item := filepath.ToSlash(rel) + "|" + info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(fullPath)
			if err != nil {
				return err
			}
			item += "|" + string(data)
		}
		snapshot = append(snapshot, item)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%q): %v", root, err)
	}
	sort.Strings(snapshot)
	return snapshot
}

func assertEvidenceDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", path, err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory %q has residue: %v", path, entries)
	}
}
