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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	storeoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

type testRecipeArtifact struct {
	descriptor ociv1.Descriptor
	manifest   ociv1.Manifest
	manifestB  []byte
	layer      []byte
}

type testArchiveEntry struct {
	header  tar.Header
	content []byte
}

func TestStageRecipeArtifactAcceptsPackageOutput(t *testing.T) {
	safeOCITempRoot(t)
	ctx := context.Background()
	source := t.TempDir()
	parent := t.TempDir()
	packageParent := t.TempDir()
	writeTestFile(t, filepath.Join(source, "registry.yaml"), []byte("components: []\n"))
	writeTestFile(t, filepath.Join(source, "components", "demo", "values.yaml"), []byte("replicas: 1\n"))

	packaged, err := Package(ctx, PackageOptions{
		SourceDir:  source,
		OutputDir:  packageParent,
		Registry:   "ghcr.io",
		Repository: "nvidia/aicr-recipes",
		Tag:        "v1",
	})
	if err != nil {
		t.Fatalf("Package() error = %v", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(packaged.StorePath); removeErr != nil {
			t.Errorf("remove packaged layout: %v", removeErr)
		}
	}()
	local, err := storeoci.NewFromFS(ctx, os.DirFS(packaged.StorePath))
	if err != nil {
		t.Fatalf("open packaged OCI layout: %v", err)
	}
	repository := &testRecipeRepository{
		target:      local,
		fetchErrors: make(map[digest.Digest][]error),
		fetchCalls:  make(map[digest.Digest]int),
	}
	deps := testRecipePullDependencies(repository)

	staged, err := stageRecipeArtifactWithDependencies(ctx, RecipePullOptions{
		Repository: "oci://ghcr.io/nvidia/aicr-recipes",
		Selector:   "v1",
		TempDir:    parent,
	}, deps)
	if err != nil {
		t.Fatalf("stageRecipeArtifactWithDependencies() error = %v", err)
	}
	if got, want := staged.Descriptor().Digest, digest.Digest(packaged.Digest); got != want {
		t.Errorf("resolved digest = %s, want %s", got, want)
	}
	if got, want := staged.Reference(), "ghcr.io/nvidia/aicr-recipes@"+packaged.Digest; got != want {
		t.Errorf("Reference() = %q, want %q", got, want)
	}
	authorizeStagedRecipeForTest(staged)
	materialized, err := staged.Materialize(ctx)
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	assertTestFile(t, filepath.Join(materialized, "registry.yaml"), "components: []\n")
	assertTestFile(t, filepath.Join(materialized, "components", "demo", "values.yaml"), "replicas: 1\n")
	if err := staged.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("caller temp parent was removed: %v", err)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatalf("caller temp parent after Close = entries %v, error %v", entries, err)
	}
}

func TestStageRecipeArtifactFreezesResolvedTagAcrossRetry(t *testing.T) {
	artifact := newTestRecipeArtifact(t, []testArchiveEntry{{
		header:  tar.Header{Name: "registry.yaml", Mode: 0o644, Typeflag: tar.TypeReg},
		content: []byte("components: []\n"),
	}})
	repository := newTestRecipeRepository(artifact)
	repository.fetchErrors[artifact.manifest.Layers[0].Digest] = []error{
		&errcode.ErrorResponse{StatusCode: http.StatusServiceUnavailable}, nil,
	}
	var waits []time.Duration
	deps := testRecipePullDependencies(repository)
	deps.maxAttempts = 3
	deps.initialBackoff = 2 * time.Millisecond
	deps.waitBackoff = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	parent := t.TempDir()
	staged, err := stageRecipeArtifactWithDependencies(context.Background(), RecipePullOptions{
		Repository: "ghcr.io/nvidia/aicr-recipes", Selector: "stable", TempDir: parent,
	}, deps)
	if err != nil {
		t.Fatalf("stageRecipeArtifactWithDependencies() error = %v", err)
	}
	defer func() { _ = staged.Close() }()
	if repository.resolveCalls != 1 {
		t.Errorf("Resolve calls = %d, want 1", repository.resolveCalls)
	}
	if got := repository.fetchCalls[artifact.descriptor.Digest]; got != 1 {
		t.Errorf("manifest Fetch calls = %d, want 1", got)
	}
	if got := repository.fetchCalls[artifact.manifest.Config.Digest]; got != 1 {
		t.Errorf("config Fetch calls = %d, want 1", got)
	}
	if got := repository.fetchCalls[artifact.manifest.Layers[0].Digest]; got != 2 {
		t.Errorf("layer Fetch calls = %d, want 2", got)
	}
	if len(waits) != 1 || waits[0] != 2*time.Millisecond {
		t.Errorf("backoff waits = %v, want [2ms]", waits)
	}
}

func TestStageRecipeArtifactAcceptsVerifiedContentWhenResponseCloseFails(t *testing.T) {
	artifact := newTestRecipeArtifact(t, []testArchiveEntry{{
		header: tarHeader(), content: []byte("components: []\n"),
	}})
	contents := []struct {
		name       string
		descriptor ociv1.Descriptor
		data       []byte
	}{
		{name: "manifest", descriptor: artifact.descriptor, data: artifact.manifestB},
		{name: "config", descriptor: artifact.manifest.Config, data: ociv1.DescriptorEmptyJSON.Data},
		{name: "layer", descriptor: artifact.manifest.Layers[0], data: artifact.layer},
	}
	for _, tt := range contents {
		t.Run(tt.name, func(t *testing.T) {
			repository := newTestRecipeRepository(artifact)
			closeErr := stderrors.New("response close failed")
			reader := newTestResponseCloseErrorReader(tt.data, closeErr)
			repository.fetchReaders[tt.descriptor.Digest] = []io.ReadCloser{
				reader,
			}
			var fetchedBytes int64
			repository.fetchedBytes = &fetchedBytes
			deps := testRecipePullDependencies(repository)
			deps.maxAttempts = 2

			staged, err := stageRecipeArtifactWithDependencies(t.Context(), RecipePullOptions{
				Repository: "ghcr.io/nvidia/aicr-recipes",
				Selector:   "stable",
				TempDir:    t.TempDir(),
			}, deps)
			if err != nil {
				t.Fatalf("stageRecipeArtifactWithDependencies() error = %v", err)
			}
			t.Cleanup(func() { _ = staged.Close() })
			if got := repository.fetchCalls[tt.descriptor.Digest]; got != 1 {
				t.Errorf("%s Fetch calls = %d, want 1", tt.name, got)
			}
			if reader.closeCalls != 1 {
				t.Errorf("%s response Close calls = %d, want 1", tt.name, reader.closeCalls)
			}
			wantBytes := artifact.descriptor.Size + artifact.manifest.Config.Size +
				artifact.manifest.Layers[0].Size
			if fetchedBytes != wantBytes {
				t.Errorf("response-body bytes = %d, want one verified graph (%d)",
					fetchedBytes, wantBytes)
			}
		})
	}
}

func TestStageRecipeArtifactRetriesInterruptedContent(t *testing.T) {
	artifact := newTestRecipeArtifact(t, []testArchiveEntry{{
		header: tarHeader(), content: []byte("components: []\n"),
	}})
	tests := []struct {
		name   string
		digest digest.Digest
		data   []byte
	}{
		{name: "manifest", digest: artifact.descriptor.Digest, data: artifact.manifestB},
		{name: "config", digest: artifact.manifest.Config.Digest, data: ociv1.DescriptorEmptyJSON.Data},
		{name: "layer", digest: artifact.manifest.Layers[0].Digest, data: artifact.layer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repository := newTestRecipeRepository(artifact)
			var interruptedBytes int64
			closeErr := stderrors.New("interrupted response close failed")
			repository.fetchReaders[tt.digest] = []io.ReadCloser{
				newTestCloseErrorReadCloser(
					newCountedUnexpectedEOFReader(
						tt.data, max(1, len(tt.data)/2), &interruptedBytes),
					closeErr),
			}
			deps := testRecipePullDependencies(repository)
			deps.maxAttempts = 2
			deps.maxDownloadBytes = artifact.manifest.Config.Size +
				artifact.manifest.Layers[0].Size
			deps.maxRetryTraffic = 2 *
				(artifact.descriptor.Size + deps.maxDownloadBytes + 1)

			staged, err := stageRecipeArtifactWithDependencies(t.Context(), RecipePullOptions{
				Repository: "ghcr.io/nvidia/aicr-recipes",
				Selector:   "stable",
				TempDir:    t.TempDir(),
			}, deps)
			if err != nil {
				t.Fatalf("stageRecipeArtifactWithDependencies() error = %v", err)
			}
			t.Cleanup(func() { _ = staged.Close() })
			if interruptedBytes <= 0 || interruptedBytes >= int64(len(tt.data)) {
				t.Errorf("interrupted bytes = %d, want within (0, %d)",
					interruptedBytes, len(tt.data))
			}
			if got := repository.fetchCalls[tt.digest]; got != 2 {
				t.Errorf("%s Fetch calls = %d, want 2", tt.name, got)
			}
			if repository.resolveCalls != 1 {
				t.Errorf("Resolve calls = %d, want frozen single resolution", repository.resolveCalls)
			}
		})
	}
}

func TestStageRecipeArtifactInterruptedContentExhaustsAsUnavailable(t *testing.T) {
	artifact := newTestRecipeArtifact(t, []testArchiveEntry{{
		header: tarHeader(), content: []byte("components: []\n"),
	}})
	repository := newTestRecipeRepository(artifact)
	layer := artifact.manifest.Layers[0]
	var interruptedBytes int64
	closeErr := stderrors.New("interrupted response close failed")
	repository.fetchReaders[layer.Digest] = []io.ReadCloser{
		newTestCloseErrorReadCloser(
			newCountedUnexpectedEOFReader(
				artifact.layer, len(artifact.layer)/2, &interruptedBytes), closeErr),
		newTestCloseErrorReadCloser(
			newCountedUnexpectedEOFReader(
				artifact.layer, len(artifact.layer)/2, &interruptedBytes), closeErr),
	}
	deps := testRecipePullDependencies(repository)
	deps.maxAttempts = 2
	_, err := stageRecipeArtifactWithDependencies(t.Context(), RecipePullOptions{
		Repository: "ghcr.io/nvidia/aicr-recipes", Selector: "stable", TempDir: t.TempDir(),
	}, deps)
	assertErrorCode(t, err, apperrors.ErrCodeUnavailable)
	if !stderrors.Is(err, closeErr) {
		t.Errorf("error = %v, want response close cause %v", err, closeErr)
	}
	if got := repository.fetchCalls[layer.Digest]; got != 2 {
		t.Errorf("layer Fetch calls = %d, want 2", got)
	}
	if interruptedBytes != int64(len(artifact.layer)/2*2) {
		t.Errorf("interrupted bytes = %d, want %d",
			interruptedBytes, len(artifact.layer)/2*2)
	}
}

func TestStageRecipeArtifactRetryTrafficLimitIsUnavailable(t *testing.T) {
	artifact := newTestRecipeArtifact(t, []testArchiveEntry{{
		header: tarHeader(), content: []byte("components: []\n"),
	}})
	repository := newTestRecipeRepository(artifact)
	var fetchedBytes int64
	repository.fetchedBytes = &fetchedBytes
	layer := artifact.manifest.Layers[0]
	cutoff := len(artifact.layer) / 2
	var interruptedBytes int64
	closeErr := stderrors.New("interrupted response close failed")
	repository.fetchReaders[layer.Digest] = []io.ReadCloser{
		newTestCloseErrorReadCloser(
			newCountedUnexpectedEOFReader(artifact.layer, cutoff, &interruptedBytes), closeErr),
	}
	deps := testRecipePullDependencies(repository)
	deps.maxAttempts = 2
	deps.maxRetryTraffic = artifact.descriptor.Size + artifact.manifest.Config.Size +
		int64(cutoff) + layer.Size

	_, err := stageRecipeArtifactWithDependencies(t.Context(), RecipePullOptions{
		Repository: "ghcr.io/nvidia/aicr-recipes", Selector: "stable", TempDir: t.TempDir(),
	}, deps)
	assertErrorCode(t, err, apperrors.ErrCodeUnavailable)
	if got := repository.fetchCalls[layer.Digest]; got != 1 {
		t.Errorf("layer Fetch calls = %d, want retry rejected before second body", got)
	}
	wantBytes := artifact.descriptor.Size + artifact.manifest.Config.Size + int64(cutoff)
	if fetchedBytes != wantBytes {
		t.Errorf("response-body bytes = %d, want %d without close-error double accounting",
			fetchedBytes, wantBytes)
	}
	if interruptedBytes != int64(cutoff) {
		t.Errorf("interrupted layer bytes = %d, want %d", interruptedBytes, cutoff)
	}
}

func TestStageRecipeArtifactFailureClassificationAndCleanup(t *testing.T) {
	tests := []struct {
		name      string
		resolve   error
		wantCode  apperrors.ErrorCode
		wantCalls int
	}{
		{"unauthorized", &errcode.ErrorResponse{StatusCode: http.StatusUnauthorized}, apperrors.ErrCodeUnauthorized, 1},
		{"missing Basic credential", auth.ErrBasicCredentialNotFound, apperrors.ErrCodeUnauthorized, 1},
		{"not found", &errcode.ErrorResponse{StatusCode: http.StatusNotFound}, apperrors.ErrCodeNotFound, 1},
		{"rate limited", &errcode.ErrorResponse{StatusCode: http.StatusTooManyRequests}, apperrors.ErrCodeRateLimitExceeded, 3},
		{"server unavailable", &errcode.ErrorResponse{StatusCode: http.StatusBadGateway}, apperrors.ErrCodeUnavailable, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			repository := &testRecipeRepository{resolveErrors: []error{tt.resolve, tt.resolve, tt.resolve}}
			deps := testRecipePullDependencies(repository)
			deps.maxAttempts = 3
			_, err := stageRecipeArtifactWithDependencies(context.Background(), RecipePullOptions{
				Repository: "ghcr.io/nvidia/aicr-recipes", Selector: "v1", TempDir: parent,
			}, deps)
			if !stderrors.Is(err, apperrors.New(tt.wantCode, "")) {
				t.Errorf("error = %v, want code %s", err, tt.wantCode)
			}
			if repository.resolveCalls != tt.wantCalls {
				t.Errorf("Resolve calls = %d, want %d", repository.resolveCalls, tt.wantCalls)
			}
			if entries, readErr := os.ReadDir(parent); readErr != nil || len(entries) != 0 {
				t.Errorf("caller parent after failure = entries %v, error %v", entries, readErr)
			}
		})
	}
}

func TestClassifyRecipePullFailureDigestHeaderMismatch(t *testing.T) {
	requested := "sha256:" + strings.Repeat("a", 64)
	received := "sha256:" + strings.Repeat("b", 64)
	tests := []struct {
		name string
		err  error
		code apperrors.ErrorCode
	}{
		{
			name: "HEAD digest contradiction",
			err: stderrors.New(`HEAD "https://registry.example.test/v2/aicr/manifests/` + requested +
				`": invalid response; digest mismatch in Docker-Content-Digest: received "` + received +
				`" when expecting "` + requested + `"`),
			code: apperrors.ErrCodeInvalidRequest,
		},
		{
			name: "GET digest contradiction",
			err: stderrors.New(`GET "https://registry.example.test/v2/aicr/manifests/` + requested +
				`": invalid response; digest mismatch in Docker-Content-Digest: received "` + received +
				`" when expecting "` + requested + `"`),
			code: apperrors.ErrCodeInvalidRequest,
		},
		{
			name: "similar arbitrary error is not reclassified",
			err: stderrors.New(`HEAD "https://registry.example.test/v2/aicr/manifests/` + requested +
				`": digest mismatch: received "` + received + `" when expecting "` + requested + `"`),
			code: apperrors.ErrCodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrorCode(t, classifyRecipePullFailure(tt.err, false), tt.code)
		})
	}
}

func TestStageRecipeArtifactContextBoundsAndCleanup(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		parent := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := stageRecipeArtifactWithDependencies(ctx, RecipePullOptions{
			Repository: "ghcr.io/nvidia/aicr-recipes", Selector: "v1", TempDir: parent,
		}, testRecipePullDependencies(&blockingResolveRepository{}))
		if !stderrors.Is(err, apperrors.New(apperrors.ErrCodeCanceled, "")) {
			t.Errorf("error = %v, want ErrCodeCanceled", err)
		}
		assertEmptyDirectory(t, parent)
	})

	t.Run("attempt deadlines are finite", func(t *testing.T) {
		parent := t.TempDir()
		repository := &blockingResolveRepository{}
		deps := testRecipePullDependencies(repository)
		deps.maxAttempts = 2
		deps.perAttemptTimeout = 5 * time.Millisecond
		started := time.Now()
		_, err := stageRecipeArtifactWithDependencies(context.Background(), RecipePullOptions{
			Repository: "ghcr.io/nvidia/aicr-recipes", Selector: "v1", TempDir: parent,
		}, deps)
		if !stderrors.Is(err, apperrors.New(apperrors.ErrCodeUnavailable, "")) {
			t.Errorf("error = %v, want ErrCodeUnavailable", err)
		}
		if repository.calls != 2 {
			t.Errorf("Resolve calls = %d, want 2", repository.calls)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Errorf("bounded attempts took %v, want < 1s", elapsed)
		}
		assertEmptyDirectory(t, parent)
	})
}

func TestRunRecipePullAttemptPreservesCompletedResultAtDeadline(t *testing.T) {
	tests := []struct {
		name       string
		attemptErr error
	}{
		{name: "success"},
		{
			name: "structured failure",
			attemptErr: apperrors.New(
				apperrors.ErrCodeInvalidRequest, "invalid completed response"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runRecipePullAttempt(t.Context(), 0, func(attemptCtx context.Context) error {
				<-attemptCtx.Done()
				return tt.attemptErr
			})
			if !stderrors.Is(got, tt.attemptErr) {
				t.Errorf("runRecipePullAttempt() error = %v, want preserved completed result %v",
					got, tt.attemptErr)
			}
		})
	}
}

func TestStageRecipeArtifactRejectsTamperingAndDigestMismatch(t *testing.T) {
	artifact := newTestRecipeArtifact(t, []testArchiveEntry{{
		header:  tar.Header{Name: "registry.yaml", Mode: 0o644, Typeflag: tar.TypeReg},
		content: []byte("components: []\n"),
	}})
	tests := []struct {
		name        string
		selector    string
		fetchDigest digest.Digest
		mutate      func(*testRecipeRepository)
	}{
		{
			name:        "manifest bytes tampered",
			selector:    "v1",
			fetchDigest: artifact.descriptor.Digest,
			mutate: func(repository *testRecipeRepository) {
				repository.blobs[artifact.descriptor.Digest] = append([]byte(nil), artifact.manifestB...)
				repository.blobs[artifact.descriptor.Digest][0] ^= 0xff
			},
		},
		{
			name:     "digest selector resolves elsewhere",
			selector: "sha256:" + strings.Repeat("0", 64),
		},
		{
			name:        "layer response has trailing data",
			selector:    "v1",
			fetchDigest: artifact.manifest.Layers[0].Digest,
			mutate: func(repository *testRecipeRepository) {
				layer := artifact.manifest.Layers[0]
				repository.blobs[layer.Digest] = append(append([]byte(nil), artifact.layer...), 0)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			repository := newTestRecipeRepository(artifact)
			if tt.mutate != nil {
				tt.mutate(repository)
			}
			deps := testRecipePullDependencies(repository)
			deps.maxAttempts = 3
			_, err := stageRecipeArtifactWithDependencies(context.Background(), RecipePullOptions{
				Repository: "ghcr.io/nvidia/aicr-recipes", Selector: tt.selector, TempDir: parent,
			}, deps)
			if !stderrors.Is(err, apperrors.New(apperrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
			}
			if entries, readErr := os.ReadDir(parent); readErr != nil || len(entries) != 0 {
				t.Errorf("caller parent after rejection = entries %v, error %v", entries, readErr)
			}
			if repository.resolveCalls != 1 {
				t.Errorf("Resolve calls = %d, want non-retryable single call", repository.resolveCalls)
			}
			if tt.fetchDigest != "" && repository.fetchCalls[tt.fetchDigest] != 1 {
				t.Errorf("tampered content Fetch calls = %d, want non-retryable single call",
					repository.fetchCalls[tt.fetchDigest])
			}
		})
	}
}

func TestStageRecipeArtifactTamperingRemainsPrimaryWhenResponseCloseFails(t *testing.T) {
	artifact := newTestRecipeArtifact(t, []testArchiveEntry{{
		header: tarHeader(), content: []byte("components: []\n"),
	}})
	layer := artifact.manifest.Layers[0]
	tampered := append([]byte(nil), artifact.layer...)
	tampered[len(tampered)/2] ^= 0xff
	closeErr := &errcode.ErrorResponse{StatusCode: http.StatusServiceUnavailable}
	repository := newTestRecipeRepository(artifact)
	repository.fetchReaders[layer.Digest] = []io.ReadCloser{
		newTestResponseCloseErrorReader(tampered, closeErr),
	}
	deps := testRecipePullDependencies(repository)
	deps.maxAttempts = 3

	_, err := stageRecipeArtifactWithDependencies(t.Context(), RecipePullOptions{
		Repository: "ghcr.io/nvidia/aicr-recipes",
		Selector:   "stable",
		TempDir:    t.TempDir(),
	}, deps)
	assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
	if !stderrors.Is(err, closeErr) {
		t.Errorf("error = %v, want response close cause %v", err, closeErr)
	}
	if got := repository.fetchCalls[layer.Digest]; got != 1 {
		t.Errorf("tampered layer Fetch calls = %d, want non-retryable single call", got)
	}
}

func TestRecipeLimitedReaderEnforcesActualStreamBudgets(t *testing.T) {
	tests := []struct {
		name      string
		blobLimit int64
		budget    int64
	}{
		{"per blob", 3, 10},
		{"aggregate", 10, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			budget := &recipeDownloadBudget{limit: tt.budget}
			reader := &recipeLimitedReader{
				reader: bytes.NewReader([]byte("12345")), limit: tt.blobLimit, budget: budget,
			}
			_, err := io.ReadAll(reader)
			if !stderrors.Is(err, errOCIRecipeDownloadLimit) {
				t.Errorf("ReadAll() error = %v, want download limit", err)
			}
			if reader.read != 4 {
				t.Errorf("streamed bytes = %d, want limit+1 (4)", reader.read)
			}
		})
	}
}

func TestValidateRecipePullOptions(t *testing.T) {
	digestSelector := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name       string
		opts       RecipePullOptions
		wantRepo   string
		wantDigest digest.Digest
		wantErr    bool
	}{
		{"tag", RecipePullOptions{Repository: "ghcr.io/nvidia/aicr-recipes", Selector: "v1"}, "ghcr.io/nvidia/aicr-recipes", "", false},
		{"optional scheme", RecipePullOptions{Repository: "oci://ghcr.io/nvidia/aicr-recipes", Selector: digestSelector}, "ghcr.io/nvidia/aicr-recipes", digest.Digest(digestSelector), false},
		{"empty repository", RecipePullOptions{Selector: "v1"}, "", "", true},
		{"unsupported scheme", RecipePullOptions{Repository: "https://ghcr.io/nvidia/aicr", Selector: "v1"}, "", "", true},
		{"embedded tag", RecipePullOptions{Repository: "ghcr.io/nvidia/aicr:v1", Selector: "v2"}, "", "", true},
		{"empty selector", RecipePullOptions{Repository: "ghcr.io/nvidia/aicr"}, "", "", true},
		{"invalid selector", RecipePullOptions{Repository: "ghcr.io/nvidia/aicr", Selector: "bad/tag"}, "", "", true},
		{"non sha256", RecipePullOptions{Repository: "ghcr.io/nvidia/aicr", Selector: "sha512:" + strings.Repeat("a", 128)}, "", "", true},
		{"temp whitespace", RecipePullOptions{Repository: "ghcr.io/nvidia/aicr", Selector: "v1", TempDir: " /tmp"}, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, _, gotDigest, err := validateRecipePullOptions(tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateRecipePullOptions() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.wantRepo || gotDigest != tt.wantDigest {
				t.Errorf("validateRecipePullOptions() = (%q, %q), want (%q, %q)",
					got, gotDigest, tt.wantRepo, tt.wantDigest)
			}
		})
	}
}

func TestRecipePullPublicAndGuardPaths(t *testing.T) {
	var nilArtifact *StagedRecipeArtifact
	if got := nilArtifact.Descriptor(); got.Digest != "" {
		t.Errorf("nil Descriptor() = %+v, want zero value", got)
	}
	if got := nilArtifact.Reference(); got != "" {
		t.Errorf("nil Reference() = %q, want empty", got)
	}
	if err := nilArtifact.Close(); err != nil {
		t.Errorf("nil Close() error = %v", err)
	}
	_, nilMaterializeErr := nilArtifact.Materialize(context.Background())
	if !stderrors.Is(nilMaterializeErr, apperrors.New(apperrors.ErrCodeInvalidRequest, "")) {
		t.Errorf("nil Materialize() error = %v, want ErrCodeInvalidRequest", nilMaterializeErr)
	}
	_, stageErr := StageRecipeArtifact(context.Background(), RecipePullOptions{})
	if !stderrors.Is(stageErr, apperrors.New(apperrors.ErrCodeInvalidRequest, "")) {
		t.Errorf("StageRecipeArtifact() error = %v, want ErrCodeInvalidRequest", stageErr)
	}

	closed := &StagedRecipeArtifact{closed: true}
	_, closedMaterializeErr := closed.Materialize(context.Background())
	if !stderrors.Is(closedMaterializeErr, apperrors.New(apperrors.ErrCodeUnavailable, "")) {
		t.Errorf("closed Materialize() error = %v, want ErrCodeUnavailable", closedMaterializeErr)
	}
	incomplete := &StagedRecipeArtifact{authorization: recipeMaterializationDigestAuthorized}
	_, incompleteMaterializeErr := incomplete.Materialize(context.Background())
	if !stderrors.Is(incompleteMaterializeErr, apperrors.New(apperrors.ErrCodeInternal, "")) {
		t.Errorf("incomplete Materialize() error = %v, want ErrCodeInternal", incompleteMaterializeErr)
	}
	already := &StagedRecipeArtifact{
		materialized:  "/private/recipe",
		authorization: recipeMaterializationDigestAuthorized,
	}
	if got, err := already.Materialize(context.Background()); err != nil || got != "/private/recipe" {
		t.Errorf("completed Materialize() = (%q, %v), want (/private/recipe, nil)", got, err)
	}

	t.Setenv("DOCKER_CONFIG", t.TempDir())
	repository, err := newRemoteRecipeArtifactRepository(
		t.Context(), "ghcr.io/nvidia/aicr-recipes")
	if err != nil {
		t.Fatalf("newRemoteRecipeArtifactRepository() error = %v", err)
	}
	remoteRepository, ok := repository.(*remoteRecipeArtifactRepository)
	if !ok {
		t.Fatalf("repository type = %T, want *remoteRecipeArtifactRepository", repository)
	}
	if remoteRepository.client.Timeout != defaults.OCIRecipePullAttemptTimeout {
		t.Errorf("HTTP timeout = %v, want %v",
			remoteRepository.client.Timeout, defaults.OCIRecipePullAttemptTimeout)
	}
	if remoteRepository.repository.MaxMetadataBytes != defaults.MaxOCIRecipeManifestBytes {
		t.Errorf("metadata cap = %d, want %d",
			remoteRepository.repository.MaxMetadataBytes, defaults.MaxOCIRecipeManifestBytes)
	}
	transport, ok := remoteRepository.client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion == 0 {
		t.Errorf("HTTP transport is not TLS hardened: %#v", remoteRepository.client.Transport)
	}
	repository.CloseIdleConnections()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	backoffErr := waitRecipePullBackoff(canceled, time.Hour)
	if !stderrors.Is(backoffErr, apperrors.New(apperrors.ErrCodeCanceled, "")) {
		t.Errorf("canceled backoff error = %v, want ErrCodeCanceled", backoffErr)
	}
	if err := waitRecipePullBackoff(context.Background(), 0); err != nil {
		t.Errorf("zero backoff error = %v", err)
	}
}

func TestValidateRecipeDescriptors(t *testing.T) {
	artifact := newTestRecipeArtifact(t, nil)
	valid := artifact.descriptor
	valid.ArtifactType = artifactType
	tests := []struct {
		name     string
		mutate   func(*ociv1.Descriptor)
		selector digest.Digest
		wantErr  bool
	}{
		{"valid producer descriptor", func(*ociv1.Descriptor) {}, "", false},
		{"valid remote descriptor omits artifact type", func(d *ociv1.Descriptor) { d.ArtifactType = "" }, "", false},
		{"wrong media type", func(d *ociv1.Descriptor) { d.MediaType = ociv1.MediaTypeImageIndex }, "", true},
		{"wrong algorithm", func(d *ociv1.Descriptor) {
			d.Digest = digest.NewDigestFromEncoded(digest.SHA512, strings.Repeat("a", 128))
		}, "", true},
		{"invalid digest", func(d *ociv1.Descriptor) { d.Digest = "sha256:short" }, "", true},
		{"selector mismatch", func(*ociv1.Descriptor) {}, digest.FromString("elsewhere"), true},
		{"negative size", func(d *ociv1.Descriptor) { d.Size = -1 }, "", true},
		{"oversized", func(d *ociv1.Descriptor) { d.Size = defaults.MaxOCIRecipeManifestBytes + 1 }, "", true},
		{"URL", func(d *ociv1.Descriptor) { d.URLs = []string{"https://example.test"} }, "", true},
		{"embedded data", func(d *ociv1.Descriptor) { d.Data = []byte("manifest") }, "", true},
		{"platform", func(d *ociv1.Descriptor) { d.Platform = &ociv1.Platform{OS: "linux"} }, "", true},
		{"wrong artifact type", func(d *ociv1.Descriptor) { d.ArtifactType = "application/example" }, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			descriptor := cloneRootStoreDescriptor(valid)
			tt.mutate(&descriptor)
			err := validateRecipeManifestDescriptor(descriptor, tt.selector)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateRecipeManifestDescriptor() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	layer := artifact.manifest.Layers[0]
	if err := validateRecipeBlobDescriptor(layer, layer.Size, layer.Size); err != nil {
		t.Errorf("valid blob descriptor error = %v", err)
	}
	invalidAlgorithm := layer
	invalidAlgorithm.Digest = digest.NewDigestFromEncoded(digest.SHA512, strings.Repeat("a", 128))
	if err := validateRecipeBlobDescriptor(invalidAlgorithm, layer.Size, layer.Size); err == nil {
		t.Error("non-sha256 blob descriptor was accepted")
	}
	invalidDigest := layer
	invalidDigest.Digest = "sha256:short"
	if err := validateRecipeBlobDescriptor(invalidDigest, layer.Size, layer.Size); err == nil {
		t.Error("invalid blob digest was accepted")
	}
	oversized := layer
	oversized.Size++
	if err := validateRecipeBlobDescriptor(oversized, layer.Size, layer.Size); err == nil {
		t.Error("oversized blob descriptor was accepted")
	}
}

func TestValidateRecipeManifestRejectsUnsupportedShapes(t *testing.T) {
	artifact := newTestRecipeArtifact(t, nil)
	tests := []struct {
		name   string
		mutate func(*ociv1.Manifest)
	}{
		{"wrong schema", func(m *ociv1.Manifest) { m.SchemaVersion = 1 }},
		{"wrong artifact", func(m *ociv1.Manifest) { m.ArtifactType = "application/example" }},
		{"subject", func(m *ociv1.Manifest) { m.Subject = &ociv1.DescriptorEmptyJSON }},
		{"noncanonical config", func(m *ociv1.Manifest) { m.Config.Data = []byte("null") }},
		{"no layer", func(m *ociv1.Manifest) { m.Layers = nil }},
		{"extra layer", func(m *ociv1.Manifest) { m.Layers = append(m.Layers, m.Layers[0]) }},
		{"wrong layer type", func(m *ociv1.Manifest) { m.Layers[0].MediaType = ociv1.MediaTypeImageLayer }},
		{"layer URL", func(m *ociv1.Manifest) { m.Layers[0].URLs = []string{"https://example.test/layer"} }},
		{"embedded layer", func(m *ociv1.Manifest) { m.Layers[0].Data = []byte("layer") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := cloneRecipeManifest(artifact.manifest)
			tt.mutate(&manifest)
			data, err := jsonMarshalTest(manifest)
			if err != nil {
				t.Fatal(err)
			}
			_, err = validateRecipeManifest(data)
			if !stderrors.Is(err, apperrors.New(apperrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("validateRecipeManifest() error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestValidateRecipeManifestRejectsIntrinsicAggregateOversize(t *testing.T) {
	artifact := newTestRecipeArtifact(t, nil)
	manifest := cloneRecipeManifest(artifact.manifest)
	manifest.Layers[0].Size = defaults.MaxOCIRecipeDownloadBytes
	data, err := jsonMarshalTest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	_, err = validateRecipeManifest(data)
	assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
}

func TestExtractRecipeArchiveSafetyAndLimits(t *testing.T) {
	regular := func(name, body string) testArchiveEntry {
		return testArchiveEntry{
			header:  tar.Header{Name: name, Mode: 0o777, Typeflag: tar.TypeReg},
			content: []byte(body),
		}
	}
	tests := []struct {
		name    string
		entries []testArchiveEntry
		limits  recipeExtractLimits
		wantErr bool
	}{
		{"valid", []testArchiveEntry{
			{header: tar.Header{Name: "components/", Mode: 0o777, Typeflag: tar.TypeDir}},
			regular("components/values.yaml", "abc"),
		}, recipeExtractLimits{maxTotal: 4096, maxFile: 10, maxFiles: 2}, false},
		{"absolute", []testArchiveEntry{regular("/escape", "x")}, recipeExtractLimits{4096, 10, 1}, true},
		{"traversal", []testArchiveEntry{regular("../escape", "x")}, recipeExtractLimits{4096, 10, 1}, true},
		{"noncanonical traversal", []testArchiveEntry{regular("a/../escape", "x")}, recipeExtractLimits{4096, 10, 1}, true},
		{"backslash", []testArchiveEntry{regular(`a\escape`, "x")}, recipeExtractLimits{4096, 10, 1}, true},
		{"symlink", []testArchiveEntry{{header: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}}}, recipeExtractLimits{4096, 10, 1}, true},
		{"hardlink", []testArchiveEntry{{header: tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "target"}}}, recipeExtractLimits{4096, 10, 1}, true},
		{"fifo", []testArchiveEntry{{header: tar.Header{Name: "pipe", Typeflag: tar.TypeFifo}}}, recipeExtractLimits{4096, 10, 1}, true},
		{"device", []testArchiveEntry{{header: tar.Header{Name: "dev", Typeflag: tar.TypeChar}}}, recipeExtractLimits{4096, 10, 1}, true},
		{"duplicate", []testArchiveEntry{regular("a", "x"), regular("a", "y")}, recipeExtractLimits{4096, 10, 2}, true},
		{"type conflict", []testArchiveEntry{regular("a", "x"), regular("a/b", "y")}, recipeExtractLimits{4096, 10, 2}, true},
		{"per file limit", []testArchiveEntry{regular("a", "123")}, recipeExtractLimits{4096, 2, 1}, true},
		{"total limit", []testArchiveEntry{regular("a", "12"), regular("b", "12")}, recipeExtractLimits{3, 3, 2}, true},
		{"file count", []testArchiveEntry{regular("a", "1"), regular("b", "2")}, recipeExtractLimits{4096, 10, 1}, true},
		{"implicit directory count", []testArchiveEntry{regular("a/b", "1")}, recipeExtractLimits{4096, 10, 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive := buildTestRecipeArchive(t, tt.entries)
			rootPath := t.TempDir()
			root, err := os.OpenRoot(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			err = extractRecipeArchive(context.Background(), bytes.NewReader(archive), root, tt.limits)
			if closeErr := root.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("extractRecipeArchive() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !stderrors.Is(err, apperrors.New(apperrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
			}
			if !tt.wantErr {
				assertTestFile(t, filepath.Join(rootPath, "components", "values.yaml"), "abc")
				info, statErr := os.Stat(filepath.Join(rootPath, "components"))
				if statErr != nil || info.Mode().Perm() != 0o700 {
					t.Errorf("directory mode = %v, error %v, want 0700", info.Mode().Perm(), statErr)
				}
			}
		})
	}
}

func TestExtractRecipeArchiveRejectsTruncationConcatenationAndCancellation(t *testing.T) {
	archive := buildTestRecipeArchive(t, []testArchiveEntry{{
		header: tar.Header{Name: "registry.yaml", Typeflag: tar.TypeReg}, content: []byte("ok"),
	}})
	second := buildTestRecipeArchive(t, []testArchiveEntry{{
		header: tar.Header{Name: "other", Typeflag: tar.TypeReg}, content: []byte("other"),
	}})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		data []byte
	}{
		{"truncated", context.Background(), archive[:len(archive)-4]},
		{"concatenated", context.Background(), append(append([]byte(nil), archive...), second...)},
		{"canceled", canceled, archive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			err = extractRecipeArchive(tt.ctx, bytes.NewReader(tt.data), root,
				recipeExtractLimits{maxTotal: 4096, maxFile: 10, maxFiles: 2})
			_ = root.Close()
			if err == nil {
				t.Fatal("extractRecipeArchive() error = nil")
			}
		})
	}
}

func TestExtractRecipeArchiveBoundsPostEndExpandedTail(t *testing.T) {
	entries := []testArchiveEntry{{
		header: tar.Header{Name: "registry.yaml", Typeflag: tar.TypeReg}, content: []byte("ok"),
	}}
	rawTar := buildTestRecipeTar(t, entries)
	archive := gzipTestBytes(t, append(append([]byte(nil), rawTar...), bytes.Repeat([]byte("x"), 64)...))
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = extractRecipeArchive(context.Background(), bytes.NewReader(archive), root,
		recipeExtractLimits{maxTotal: int64(len(rawTar) + 8), maxFile: 10, maxFiles: 2})
	_ = root.Close()
	if !stderrors.Is(err, errOCIRecipeExpandedLimit) {
		t.Fatalf("extractRecipeArchive() error = %v, want expanded-stream limit", err)
	}
	if !strings.Contains(err.Error(), "expanded size limit") {
		t.Errorf("error = %q, want expanded size limit context", err)
	}
}

func TestStagedRecipeArtifactMaterializeFailureCleansAndCanRetry(t *testing.T) {
	artifactData := newTestRecipeArtifact(t, []testArchiveEntry{{
		header: tar.Header{Name: "registry.yaml", Typeflag: tar.TypeReg}, content: []byte("ok"),
	}})
	parent := t.TempDir()
	staged, err := stageRecipeArtifactWithDependencies(context.Background(), RecipePullOptions{
		Repository: "ghcr.io/nvidia/aicr-recipes", Selector: "v1", TempDir: parent,
	}, testRecipePullDependencies(newTestRecipeRepository(artifactData)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = staged.Close() }()
	authorizeStagedRecipeForTest(staged)

	staged.extract = recipeExtractLimits{maxTotal: 1, maxFile: 10, maxFiles: 2}
	_, firstErr := staged.Materialize(context.Background())
	if !stderrors.Is(firstErr, apperrors.New(apperrors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("first Materialize() error = %v, want ErrCodeInvalidRequest", firstErr)
	}
	assertNoMaterializedChildren(t, staged.layout.Path())

	staged.extract = defaultRecipeExtractLimits()
	materialized, err := staged.Materialize(context.Background())
	if err != nil {
		t.Fatalf("retry Materialize() error = %v", err)
	}
	assertTestFile(t, filepath.Join(materialized, "registry.yaml"), "ok")
}

func TestStagedRecipeArtifactCloseCoordinatesWithMaterialize(t *testing.T) {
	artifactData := newTestRecipeArtifact(t, []testArchiveEntry{{
		header: tar.Header{Name: "registry.yaml", Typeflag: tar.TypeReg}, content: []byte("ok"),
	}})
	parent := t.TempDir()
	repository := newTestRecipeRepository(artifactData)
	staged, err := stageRecipeArtifactWithDependencies(context.Background(), RecipePullOptions{
		Repository: "ghcr.io/nvidia/aicr-recipes", Selector: "v1", TempDir: parent,
	}, testRecipePullDependencies(repository))
	if err != nil {
		t.Fatal(err)
	}
	authorizeStagedRecipeForTest(staged)

	start := make(chan struct{})
	release := make(chan struct{})
	originalStore := staged.store
	staged.store = &rootOCIStore{
		root: staged.layout.child,
		readOnly: &blockingReadOnlyStorage{
			base: originalStore, start: start, release: release,
		},
		validate: staged.layout.validate,
		deps:     originalStore.deps,
		tags:     make(map[string]ociv1.Descriptor),
	}
	materializeDone := make(chan error, 1)
	go func() {
		_, materializeErr := staged.Materialize(context.Background())
		materializeDone <- materializeErr
	}()
	<-start
	closeDone := make(chan error, 1)
	go func() { closeDone <- staged.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close() returned before Materialize completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-materializeDone; err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := staged.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	_, postCloseErr := staged.Materialize(context.Background())
	if !stderrors.Is(postCloseErr, apperrors.New(apperrors.ErrCodeUnavailable, "")) {
		t.Errorf("post-close Materialize() error = %v, want ErrCodeUnavailable", postCloseErr)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Errorf("caller parent after concurrent Close = entries %v, error %v", entries, err)
	}
}

func TestStagedRecipeArtifactDescriptorIsDefensive(t *testing.T) {
	original := ociv1.Descriptor{
		MediaType:   ociv1.MediaTypeImageManifest,
		Digest:      digest.FromString("manifest"),
		Size:        8,
		URLs:        []string{"https://example.test"},
		Annotations: map[string]string{"key": "value"},
		Data:        []byte("manifest"),
	}
	staged := &StagedRecipeArtifact{descriptor: original}
	got := staged.Descriptor()
	got.URLs[0] = "mutated"
	got.Annotations["key"] = "mutated"
	got.Data[0] = 'x'
	again := staged.Descriptor()
	if again.URLs[0] != original.URLs[0] || again.Annotations["key"] != "value" ||
		string(again.Data) != "manifest" {

		t.Errorf("Descriptor() aliases internal fields: %+v", again)
	}
}

type testRecipeRepository struct {
	mu     sync.Mutex
	target interface {
		Resolve(context.Context, string) (ociv1.Descriptor, error)
		Fetch(context.Context, ociv1.Descriptor) (io.ReadCloser, error)
	}
	referrerTarget interface {
		Fetch(context.Context, ociv1.Descriptor) (io.ReadCloser, error)
	}
	descriptor     ociv1.Descriptor
	blobs          map[digest.Digest][]byte
	resolveErrors  []error
	fetchErrors    map[digest.Digest][]error
	fetchReaders   map[digest.Digest][]io.ReadCloser
	fetchedBytes   *int64
	resolveCalls   int
	fetchCalls     map[digest.Digest]int
	referrerPages  [][]ociv1.Descriptor
	referrerErrors []error
	referrerCalls  int
	closed         int
}

func newTestRecipeRepository(artifact testRecipeArtifact) *testRecipeRepository {
	return &testRecipeRepository{
		descriptor: artifact.descriptor,
		blobs: map[digest.Digest][]byte{
			artifact.descriptor.Digest:         append([]byte(nil), artifact.manifestB...),
			artifact.manifest.Config.Digest:    append([]byte(nil), ociv1.DescriptorEmptyJSON.Data...),
			artifact.manifest.Layers[0].Digest: append([]byte(nil), artifact.layer...),
		},
		fetchErrors:  make(map[digest.Digest][]error),
		fetchReaders: make(map[digest.Digest][]io.ReadCloser),
		fetchCalls:   make(map[digest.Digest]int),
	}
}

func (r *testRecipeRepository) Resolve(ctx context.Context, selector string) (ociv1.Descriptor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolveCalls++
	if len(r.resolveErrors) != 0 {
		err := r.resolveErrors[0]
		r.resolveErrors = r.resolveErrors[1:]
		if err != nil {
			return ociv1.Descriptor{}, err
		}
	}
	if r.target != nil {
		return r.target.Resolve(ctx, selector)
	}
	return cloneRootStoreDescriptor(r.descriptor), nil
}

func (r *testRecipeRepository) Fetch(
	ctx context.Context,
	descriptor ociv1.Descriptor,
) (io.ReadCloser, error) {

	r.mu.Lock()
	r.fetchCalls[descriptor.Digest]++
	if errors := r.fetchErrors[descriptor.Digest]; len(errors) != 0 {
		err := errors[0]
		r.fetchErrors[descriptor.Digest] = errors[1:]
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
	}
	if readers := r.fetchReaders[descriptor.Digest]; len(readers) != 0 {
		reader := readers[0]
		r.fetchReaders[descriptor.Digest] = readers[1:]
		tracked := countTestReadCloser(reader, r.fetchedBytes)
		r.mu.Unlock()
		return tracked, nil
	}
	target := r.target
	referrerTarget := r.referrerTarget
	data, ok := r.blobs[descriptor.Digest]
	r.mu.Unlock()
	if ok {
		return countTestReadCloser(io.NopCloser(bytes.NewReader(data)), r.fetchedBytes), nil
	}
	if target != nil {
		reader, targetErr := target.Fetch(ctx, descriptor)
		if targetErr == nil {
			return reader, nil
		}
		if !stderrors.Is(targetErr, errdef.ErrNotFound) && !os.IsNotExist(targetErr) {
			return nil, targetErr
		}
	}
	if referrerTarget != nil {
		return referrerTarget.Fetch(ctx, descriptor)
	}
	return nil, os.ErrNotExist
}

func (r *testRecipeRepository) Referrers(
	ctx context.Context,
	_ ociv1.Descriptor,
	_ string,
	callback func([]ociv1.Descriptor) error,
) error {

	r.mu.Lock()
	r.referrerCalls++
	if len(r.referrerErrors) != 0 {
		err := r.referrerErrors[0]
		r.referrerErrors = r.referrerErrors[1:]
		if err != nil {
			r.mu.Unlock()
			return err
		}
	}
	pages := make([][]ociv1.Descriptor, len(r.referrerPages))
	for i := range r.referrerPages {
		pages[i] = append([]ociv1.Descriptor(nil), r.referrerPages[i]...)
	}
	r.mu.Unlock()
	for i := range pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := callback(pages[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *testRecipeRepository) CloseIdleConnections() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed++
}

func testRecipePullDependencies(repository recipeArtifactRepository) recipePullDependencies {
	deps := defaultRecipePullDependencies()
	deps.newRepository = func(context.Context, string) (recipeArtifactRepository, error) {
		return repository, nil
	}
	deps.maxAttempts = 1
	deps.initialBackoff = 0
	deps.perAttemptTimeout = time.Second
	deps.waitBackoff = func(context.Context, time.Duration) error { return nil }
	return deps
}

func newTestRecipeArtifact(t *testing.T, entries []testArchiveEntry) testRecipeArtifact {
	t.Helper()
	layer := buildTestRecipeArchive(t, entries)
	layerDescriptor := ociv1.Descriptor{
		MediaType: ociv1.MediaTypeImageLayerGzip,
		Digest:    digest.FromBytes(layer),
		Size:      int64(len(layer)),
	}
	manifest := ociv1.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ociv1.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Config:       cloneRootStoreDescriptor(ociv1.DescriptorEmptyJSON),
		Layers:       []ociv1.Descriptor{layerDescriptor},
	}
	manifestBytes, err := jsonMarshalTest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return testRecipeArtifact{
		descriptor: ociv1.Descriptor{
			MediaType: ociv1.MediaTypeImageManifest,
			Digest:    digest.FromBytes(manifestBytes),
			Size:      int64(len(manifestBytes)),
		},
		manifest:  manifest,
		manifestB: manifestBytes,
		layer:     layer,
	}
}

func buildTestRecipeArchive(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()
	return gzipTestBytes(t, buildTestRecipeTar(t, entries))
}

func buildTestRecipeTar(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	tarWriter := tar.NewWriter(&output)
	for _, entry := range entries {
		header := entry.header
		if header.Typeflag == tar.TypeReg {
			header.Size = int64(len(entry.content))
		}
		if err := tarWriter.WriteHeader(&header); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if len(entry.content) != 0 {
			if _, err := tarWriter.Write(entry.content); err != nil {
				t.Fatalf("write tar content: %v", err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return output.Bytes()
}

func gzipTestBytes(t *testing.T, input []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	if _, err := gzipWriter.Write(input); err != nil {
		t.Fatalf("write gzip content: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return output.Bytes()
}

func writeTestFile(t *testing.T, name string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertTestFile(t *testing.T, name, content string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if string(data) != content {
		t.Errorf("%s content = %q, want %q", name, data, content)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s mode = %o, want 0600", name, info.Mode().Perm())
	}
}

func assertEmptyDirectory(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("directory %s = entries %v, error %v; want empty", dir, entries, err)
	}
}

func assertNoMaterializedChildren(t *testing.T, workspace string) {
	t.Helper()
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), recipeMaterializePrefix) {
			t.Errorf("partial materialization remains at %s", entry.Name())
		}
	}
}

type blockingReadOnlyStorage struct {
	base interface {
		Exists(context.Context, ociv1.Descriptor) (bool, error)
		Fetch(context.Context, ociv1.Descriptor) (io.ReadCloser, error)
	}
	start   chan struct{}
	release chan struct{}
	once    sync.Once
}

type blockingResolveRepository struct {
	calls int
}

type countedUnexpectedEOFReader struct {
	reader *bytes.Reader
	read   *int64
}

type testCountingReadCloser struct {
	io.ReadCloser
	read *int64
}

type testCloseErrorReadCloser struct {
	io.ReadCloser
	closeErr   error
	closeCalls int
}

func countTestReadCloser(reader io.ReadCloser, read *int64) io.ReadCloser {
	if read == nil {
		return reader
	}
	return &testCountingReadCloser{ReadCloser: reader, read: read}
}

func (r *testCountingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	*r.read += int64(n)
	return n, err
}

func newTestResponseCloseErrorReader(data []byte, closeErr error) *testCloseErrorReadCloser {
	return newTestCloseErrorReadCloser(io.NopCloser(bytes.NewReader(data)), closeErr)
}

func newTestCloseErrorReadCloser(
	reader io.ReadCloser,
	closeErr error,
) *testCloseErrorReadCloser {

	return &testCloseErrorReadCloser{ReadCloser: reader, closeErr: closeErr}
}

func (r *testCloseErrorReadCloser) Close() error {
	r.closeCalls++
	return stderrors.Join(r.ReadCloser.Close(), r.closeErr)
}

func newCountedUnexpectedEOFReader(data []byte, cutoff int, read *int64) io.ReadCloser {
	if cutoff < 0 || cutoff > len(data) {
		panic("invalid interrupted-reader cutoff")
	}
	return io.NopCloser(&countedUnexpectedEOFReader{
		reader: bytes.NewReader(data[:cutoff]),
		read:   read,
	})
}

func (r *countedUnexpectedEOFReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	*r.read += int64(n)
	if stderrors.Is(err, io.EOF) {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *blockingResolveRepository) Resolve(ctx context.Context, _ string) (ociv1.Descriptor, error) {
	r.calls++
	<-ctx.Done()
	return ociv1.Descriptor{}, ctx.Err()
}

func (*blockingResolveRepository) Fetch(
	context.Context,
	ociv1.Descriptor,
) (io.ReadCloser, error) {

	return nil, stderrors.New("unexpected Fetch")
}

func (*blockingResolveRepository) CloseIdleConnections() {}

func (*blockingResolveRepository) Referrers(
	context.Context,
	ociv1.Descriptor,
	string,
	func([]ociv1.Descriptor) error,
) error {

	return stderrors.New("unexpected Referrers")
}

func (s *blockingReadOnlyStorage) Exists(ctx context.Context, desc ociv1.Descriptor) (bool, error) {
	return s.base.Exists(ctx, desc)
}

func (s *blockingReadOnlyStorage) Fetch(
	ctx context.Context,
	desc ociv1.Descriptor,
) (io.ReadCloser, error) {

	s.once.Do(func() { close(s.start) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return s.base.Fetch(ctx, desc)
	}
}

func jsonMarshalTest(value any) ([]byte, error) {
	return json.Marshal(value)
}

func authorizeStagedRecipeForTest(staged *StagedRecipeArtifact) {
	staged.authorization = recipeMaterializationDigestAuthorized
}

func tarHeader() tar.Header {
	return tar.Header{Name: "registry.yaml", Typeflag: tar.TypeReg}
}
