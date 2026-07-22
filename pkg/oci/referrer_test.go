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
	"encoding/json"
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/file"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

func TestPushReferrerPreCanceledContextReturnsTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := PushReferrer(ctx, validReferrerOptions())
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if result != nil {
		t.Fatalf("PushReferrer() result = %+v on canceled context", result)
	}
}

func validReferrerOptions() ReferrerOptions {
	return ReferrerOptions{
		Registry:     "ghcr.io",
		Repository:   "test/repo",
		ArtifactType: "application/vnd.test.referrer",
		LayerContent: []byte("payload"),
		Subject:      content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, []byte("subject")),
	}
}

func TestPushReferrerRejectsUnsafeTempTopologyBeforeRegistryIO(t *testing.T) {
	tests := []struct {
		name     string
		topology func(*testing.T) (excludedRoot, tempRoot string)
	}{
		{
			name: "temp equals excluded root",
			topology: func(t *testing.T) (string, string) {
				root := t.TempDir()
				return root, root
			},
		},
		{
			name: "temp below excluded root",
			topology: func(t *testing.T) (string, string) {
				root := t.TempDir()
				temp := filepath.Join(root, "tmp")
				if err := os.Mkdir(temp, 0o700); err != nil {
					t.Fatal(err)
				}
				return root, temp
			},
		},
		{
			name: "temp resolved below excluded root",
			topology: func(t *testing.T) (string, string) {
				root := t.TempDir()
				temp := filepath.Join(root, "tmp")
				if err := os.Mkdir(temp, 0o700); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return root, filepath.Join(alias, "tmp")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			excludedRoot, tempRoot := tt.topology(t)
			t.Setenv("TMPDIR", tempRoot)
			before := snapshotTree(t, excludedRoot)
			registryCalled := false
			copyCalled := false
			deps := defaultReferrerPushDependencies()
			deps.newTarget = func(PushOptions) (publicationTarget, error) {
				registryCalled = true
				return newTestPublicationTarget(), nil
			}
			deps.copy = func(
				context.Context,
				oras.ReadOnlyTarget,
				string,
				oras.Target,
				string,
				oras.CopyOptions,
			) (ociv1.Descriptor, error) {

				copyCalled = true
				return validReferrerOptions().Subject, nil
			}
			opts := validReferrerOptions()
			opts.ExcludedRoots = []string{excludedRoot}
			result, err := pushReferrerWithDependencies(context.Background(), opts, deps)
			assertErrorCode(t, err, apperrors.ErrCodeInternal)
			if result != nil {
				t.Fatalf("result = %+v on unsafe temp topology", result)
			}
			if registryCalled || copyCalled {
				t.Fatalf("registry called after unsafe temp topology: target=%t copy=%t",
					registryCalled, copyCalled)
			}
			if after := snapshotTree(t, excludedRoot); !slices.Equal(after, before) {
				t.Fatalf("excluded root changed: before=%v after=%v", before, after)
			}
		})
	}
}

func TestPushReferrerAppliesWholeFlowTimeout(t *testing.T) {
	safeOCITempRoot(t)
	deadlineSeen := false
	deps := defaultReferrerPushDependencies()
	deps.newTarget = func(PushOptions) (publicationTarget, error) {
		return newTestPublicationTarget(), nil
	}
	deps.copy = func(
		ctx context.Context,
		_ oras.ReadOnlyTarget,
		_ string,
		_ oras.Target,
		_ string,
		_ oras.CopyOptions,
	) (ociv1.Descriptor, error) {

		deadline, ok := ctx.Deadline()
		deadlineSeen = ok && time.Until(deadline) > 0 &&
			time.Until(deadline) <= defaults.OCIBundlePublishTimeout
		return validReferrerOptions().Subject, nil
	}
	result, err := pushReferrerWithDependencies(context.Background(), validReferrerOptions(), deps)
	if err != nil || result == nil {
		t.Fatalf("pushReferrerWithDependencies() result=%+v error=%v", result, err)
	}
	if !deadlineSeen {
		t.Fatal("referrer pack/copy flow did not receive the whole-operation deadline")
	}
}

func TestPushReferrerCopyCancellationReturnsTimeout(t *testing.T) {
	safeOCITempRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	deps := defaultReferrerPushDependencies()
	deps.newTarget = func(PushOptions) (publicationTarget, error) {
		return newTestPublicationTarget(), nil
	}
	deps.copy = func(
		copyCtx context.Context,
		_ oras.ReadOnlyTarget,
		_ string,
		_ oras.Target,
		_ string,
		_ oras.CopyOptions,
	) (ociv1.Descriptor, error) {

		cancel()
		<-copyCtx.Done()
		return ociv1.Descriptor{}, copyCtx.Err()
	}
	result, err := pushReferrerWithDependencies(ctx, validReferrerOptions(), deps)
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if result != nil {
		t.Fatalf("result = %+v on canceled copy", result)
	}
}

func TestPushReferrerSuccessCleansPrivateWorkspace(t *testing.T) {
	tempRoot := safeOCITempRoot(t)
	deps := successfulReferrerPushDependencies(nil)
	result, err := pushReferrerWithDependencies(
		context.Background(), validReferrerOptions(), deps)
	if err != nil || result == nil {
		t.Fatalf("pushReferrerWithDependencies() result=%+v error=%v", result, err)
	}
	assertReferrerTempRootEmpty(t, tempRoot)
}

func TestPushReferrerCleanupFailureClearsSuccess(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*referrerPackage, error)
	}{
		{
			name: "store close",
			inject: func(packed *referrerPackage, injected error) {
				closeStore := packed.closeStore
				packed.closeStore = func(store *file.Store) error {
					return stderrors.Join(closeStore(store), injected)
				}
			},
		},
		{
			name: "workspace remove",
			inject: func(packed *referrerPackage, injected error) {
				closeWorkspace := packed.closeWorkspace
				packed.closeWorkspace = func(workspace *Workspace) error {
					return stderrors.Join(closeWorkspace(workspace), injected)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempRoot := safeOCITempRoot(t)
			injected := stderrors.New("injected " + tt.name + " failure")
			deps := successfulReferrerPushDependencies(func(packed *referrerPackage) {
				tt.inject(packed, injected)
			})
			result, err := pushReferrerWithDependencies(
				context.Background(), validReferrerOptions(), deps)
			assertErrorCode(t, err, apperrors.ErrCodeInternal)
			if !stderrors.Is(err, injected) {
				t.Fatalf("error = %v, want injected cleanup cause", err)
			}
			if result != nil {
				t.Fatalf("result = %+v after cleanup failure", result)
			}
			assertReferrerTempRootEmpty(t, tempRoot)
		})
	}
}

func TestPushReferrerPrimaryFailureSurvivesCleanupFailure(t *testing.T) {
	tempRoot := safeOCITempRoot(t)
	primary := apperrors.New(apperrors.ErrCodeUnavailable, "registry unavailable")
	injected := stderrors.New("injected store close failure")
	deps := successfulReferrerPushDependencies(func(packed *referrerPackage) {
		closeStore := packed.closeStore
		packed.closeStore = func(store *file.Store) error {
			return stderrors.Join(closeStore(store), injected)
		}
	})
	deps.copy = func(
		context.Context,
		oras.ReadOnlyTarget,
		string,
		oras.Target,
		string,
		oras.CopyOptions,
	) (ociv1.Descriptor, error) {

		return ociv1.Descriptor{}, primary
	}
	result, err := pushReferrerWithDependencies(
		context.Background(), validReferrerOptions(), deps)
	if !stderrors.Is(err, primary) || err.Error() != primary.Error() {
		t.Fatalf("error = %v, want unchanged primary %v", err, primary)
	}
	if result != nil {
		t.Fatalf("result = %+v after primary failure", result)
	}
	assertReferrerTempRootEmpty(t, tempRoot)
}

func TestReferrerPackageCloseIsOrderedAndIdempotent(t *testing.T) {
	safeOCITempRoot(t)
	packed, err := packReferrer(context.Background(), validReferrerOptions())
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	closeStore := packed.closeStore
	packed.closeStore = func(store *file.Store) error {
		order = append(order, "store")
		return closeStore(store)
	}
	closeWorkspace := packed.closeWorkspace
	packed.closeWorkspace = func(workspace *Workspace) error {
		order = append(order, "workspace")
		return closeWorkspace(workspace)
	}
	if err := packed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := packed.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"store", "workspace"}) {
		t.Fatalf("cleanup order = %v, want [store workspace] once", order)
	}
}

func TestPushReferrerRejectsInvalidExcludedRootsBeforeRegistryIO(t *testing.T) {
	realDir := t.TempDir()
	filePath := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, symlinkPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	for _, tt := range []struct {
		name string
		root string
	}{
		{name: "empty", root: ""},
		{name: "missing", root: filepath.Join(t.TempDir(), "missing")},
		{name: "file", root: filePath},
		{name: "symlink", root: symlinkPath},
	} {
		t.Run(tt.name, func(t *testing.T) {
			safeOCITempRoot(t)
			registryCalled := false
			deps := defaultReferrerPushDependencies()
			deps.newTarget = func(PushOptions) (publicationTarget, error) {
				registryCalled = true
				return newTestPublicationTarget(), nil
			}
			opts := validReferrerOptions()
			opts.ExcludedRoots = []string{tt.root}
			result, err := pushReferrerWithDependencies(context.Background(), opts, deps)
			assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
			if result != nil {
				t.Fatalf("result = %+v for invalid excluded root", result)
			}
			if registryCalled {
				t.Fatal("registry called after invalid excluded root")
			}
		})
	}
}

func successfulReferrerPushDependencies(
	mutate func(*referrerPackage),
) referrerPushDependencies {

	deps := defaultReferrerPushDependencies()
	pack := deps.pack
	deps.pack = func(ctx context.Context, opts ReferrerOptions) (*referrerPackage, error) {
		packed, err := pack(ctx, opts)
		if err == nil && mutate != nil {
			mutate(packed)
		}
		return packed, err
	}
	deps.newTarget = func(PushOptions) (publicationTarget, error) {
		return newTestPublicationTarget(), nil
	}
	deps.copy = func(
		context.Context,
		oras.ReadOnlyTarget,
		string,
		oras.Target,
		string,
		oras.CopyOptions,
	) (ociv1.Descriptor, error) {

		return content.NewDescriptorFromBytes(
			ociv1.MediaTypeImageManifest, []byte("referrer manifest")), nil
	}
	return deps
}

func assertReferrerTempRootEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary root contains residue: %v", entries)
	}
}

func digestFromString(t *testing.T, s string) digest.Digest {
	t.Helper()
	d, err := digest.Parse(s)
	if err != nil {
		t.Fatalf("digest.Parse(%q): %v", s, err)
	}
	return d
}

// TestPackReferrer_ManifestHasSubject is the cosign-discovery regression
// guard: cosign's OCI 1.1 Referrers API queries the registry for
// referrers of an artifact digest, and only manifests whose Subject
// field points at that digest are returned. If packReferrer ever stops
// setting Subject (or sets it to the wrong shape), cosign verify
// silently finds no signatures.
func TestPackReferrer_ManifestHasSubject(t *testing.T) {
	safeOCITempRoot(t)
	mainDigest := "sha256:" + strings.Repeat("a", 64)
	mainMediaType := "application/vnd.oci.image.manifest.v1+json"
	const mainSize = int64(1234)

	subject := ociv1.Descriptor{
		MediaType: mainMediaType,
		Digest:    digestFromString(t, mainDigest),
		Size:      mainSize,
	}

	packed, err := packReferrer(context.Background(), ReferrerOptions{
		Registry:     "ghcr.io",
		Repository:   "example/repo",
		ArtifactType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		LayerContent: []byte(`{"sigstore": "bundle"}`),
		Subject:      subject,
	})
	if err != nil {
		t.Fatalf("packReferrer: %v", err)
	}
	defer func() {
		if closeErr := packed.Close(); closeErr != nil {
			t.Errorf("packed.Close(): %v", closeErr)
		}
	}()
	fs := packed.store
	tag := packed.tag

	manifestDesc, err := fs.Resolve(context.Background(), tag)
	if err != nil {
		t.Fatalf("resolve manifest by tag: %v", err)
	}
	if manifestDesc.Digest == "" {
		t.Fatal("manifest descriptor missing digest")
	}
	if tag != strings.TrimPrefix(manifestDesc.Digest.String(), "sha256:") {
		t.Errorf("tag mismatch: tag=%q digest=%q", tag, manifestDesc.Digest.String())
	}

	rc, err := fs.Fetch(context.Background(), manifestDesc)
	if err != nil {
		t.Fatalf("fetch manifest from store: %v", err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read manifest body: %v", err)
	}

	var m struct {
		MediaType    string             `json:"mediaType"`
		ArtifactType string             `json:"artifactType"`
		Subject      *ociv1.Descriptor  `json:"subject"`
		Layers       []ociv1.Descriptor `json:"layers"`
	}
	if jsonErr := json.Unmarshal(body, &m); jsonErr != nil {
		t.Fatalf("unmarshal manifest: %v\nbody=%s", jsonErr, body)
	}
	if m.Subject == nil {
		t.Fatal("manifest.subject must be set for OCI Referrers discovery; got nil")
	}
	if m.Subject.Digest.String() != mainDigest {
		t.Errorf("manifest.subject.digest = %q, want %q", m.Subject.Digest, mainDigest)
	}
	if m.Subject.MediaType != mainMediaType {
		t.Errorf("manifest.subject.mediaType = %q, want %q", m.Subject.MediaType, mainMediaType)
	}
	if m.Subject.Size != mainSize {
		t.Errorf("manifest.subject.size = %d, want %d", m.Subject.Size, mainSize)
	}
	if m.ArtifactType != "application/vnd.dev.sigstore.bundle.v0.3+json" {
		t.Errorf("manifest.artifactType = %q, want sigstore bundle media type", m.ArtifactType)
	}
	if len(m.Layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(m.Layers))
	}
	if m.Layers[0].MediaType != "application/vnd.dev.sigstore.bundle.v0.3+json" {
		t.Errorf("layer mediaType = %q, want sigstore bundle media type", m.Layers[0].MediaType)
	}
}

func TestPackReferrer_RejectsMissingFields(t *testing.T) {
	subject := ociv1.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    digestFromString(t, "sha256:"+strings.Repeat("a", 64)),
		Size:      100,
	}
	tests := []struct {
		name    string
		opts    ReferrerOptions
		wantErr string
	}{
		{
			name: "missing artifact type",
			opts: ReferrerOptions{
				LayerContent: []byte("x"),
				Subject:      subject,
			},
			wantErr: "ArtifactType is required",
		},
		{
			name: "empty layer content",
			opts: ReferrerOptions{
				ArtifactType: "application/x",
				Subject:      subject,
			},
			wantErr: "LayerContent must be non-empty",
		},
		{
			name: "missing subject digest",
			opts: ReferrerOptions{
				ArtifactType: "application/x",
				LayerContent: []byte("x"),
			},
			wantErr: "Subject.Digest is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := packReferrer(context.Background(), tt.opts)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}
