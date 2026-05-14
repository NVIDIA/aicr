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

package verifier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// fakeFetcher is an in-memory referrerFetcher keyed by sha256 digest.
type fakeFetcher struct {
	blobs map[digest.Digest][]byte
	err   error // forces every Fetch to fail
}

func (f *fakeFetcher) Fetch(_ context.Context, target ociv1.Descriptor) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	body, ok := f.blobs[target.Digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func sha256Digest(b []byte) digest.Digest {
	sum := sha256.Sum256(b)
	return digest.NewDigestFromHex("sha256", hex.EncodeToString(sum[:]))
}

// buildReferrerFixture stages a single-layer referrer manifest plus
// its layer blob in a fakeFetcher and returns the manifest descriptor
// that fetchAndWriteReferrerLayer would receive from the Referrers API.
func buildReferrerFixture(t *testing.T, bundleBody []byte) (*fakeFetcher, ociv1.Descriptor) {
	t.Helper()
	layerDigest := sha256Digest(bundleBody)
	layerDesc := ociv1.Descriptor{
		MediaType: attestation.SigstoreBundleMediaType,
		Digest:    layerDigest,
		Size:      int64(len(bundleBody)),
	}
	manifest := ociv1.Manifest{
		MediaType:    ociv1.MediaTypeImageManifest,
		ArtifactType: attestation.SigstoreBundleMediaType,
		Layers:       []ociv1.Descriptor{layerDesc},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest := sha256Digest(manifestBytes)
	manifestDesc := ociv1.Descriptor{
		MediaType:    ociv1.MediaTypeImageManifest,
		Digest:       manifestDigest,
		Size:         int64(len(manifestBytes)),
		ArtifactType: attestation.SigstoreBundleMediaType,
	}
	return &fakeFetcher{
		blobs: map[digest.Digest][]byte{
			manifestDigest: manifestBytes,
			layerDigest:    bundleBody,
		},
	}, manifestDesc
}

func TestFetchAndWriteReferrerLayer_HappyPath(t *testing.T) {
	bundleBody := []byte(`{"fake":"sigstore bundle"}`)
	fetcher, manifestDesc := buildReferrerFixture(t, bundleBody)
	bundleDir := t.TempDir()

	if err := fetchAndWriteReferrerLayer(context.Background(), fetcher, manifestDesc, bundleDir); err != nil {
		t.Fatalf("fetchAndWriteReferrerLayer: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(bundleDir, attestation.AttestationFilename))
	if err != nil {
		t.Fatalf("read written attestation: %v", err)
	}
	if !bytes.Equal(got, bundleBody) {
		t.Errorf("written attestation = %q, want %q", got, bundleBody)
	}
}

func TestFetchAndWriteReferrerLayer_RejectsMultiLayerManifest(t *testing.T) {
	// Build a manifest with two layers — single-layer Sigstore Bundle
	// referrers are the V1 contract; anything else is malformed.
	layerA := []byte("a")
	layerB := []byte("b")
	manifest := ociv1.Manifest{
		MediaType:    ociv1.MediaTypeImageManifest,
		ArtifactType: attestation.SigstoreBundleMediaType,
		Layers: []ociv1.Descriptor{
			{Digest: sha256Digest(layerA), Size: 1},
			{Digest: sha256Digest(layerB), Size: 1},
		},
	}
	manifestBytes, _ := json.Marshal(manifest)
	manifestDigest := sha256Digest(manifestBytes)
	fetcher := &fakeFetcher{blobs: map[digest.Digest][]byte{manifestDigest: manifestBytes}}
	manifestDesc := ociv1.Descriptor{Digest: manifestDigest, Size: int64(len(manifestBytes))}

	err := fetchAndWriteReferrerLayer(context.Background(), fetcher, manifestDesc, t.TempDir())
	if err == nil {
		t.Fatalf("expected error for multi-layer manifest")
	}
	if !strings.Contains(err.Error(), "single-layer") {
		t.Errorf("error should mention single-layer; got %v", err)
	}
}

func TestFetchAndWriteReferrerLayer_RejectsOversizedManifest(t *testing.T) {
	// A manifest body larger than the limit triggers the bound check.
	body := make([]byte, maxReferrerManifestBytes+1024)
	for i := range body {
		body[i] = '{'
	}
	manifestDigest := sha256Digest(body)
	fetcher := &fakeFetcher{blobs: map[digest.Digest][]byte{manifestDigest: body}}
	desc := ociv1.Descriptor{Digest: manifestDigest, Size: int64(len(body))}

	err := fetchAndWriteReferrerLayer(context.Background(), fetcher, desc, t.TempDir())
	if err == nil {
		t.Fatalf("expected error for oversize manifest")
	}
}

func TestFetchAndWriteReferrerLayer_RejectsOversizedLayer(t *testing.T) {
	// Manifest is small, but layer descriptor claims a size that
	// exceeds the Sigstore Bundle limit — refuse before fetching.
	layerDigest := digest.NewDigestFromHex("sha256", strings.Repeat("a", 64))
	manifest := ociv1.Manifest{
		MediaType:    ociv1.MediaTypeImageManifest,
		ArtifactType: attestation.SigstoreBundleMediaType,
		Layers: []ociv1.Descriptor{
			{Digest: layerDigest, Size: 1 << 40}, // 1 TiB
		},
	}
	manifestBytes, _ := json.Marshal(manifest)
	manifestDigest := sha256Digest(manifestBytes)
	fetcher := &fakeFetcher{blobs: map[digest.Digest][]byte{manifestDigest: manifestBytes}}
	desc := ociv1.Descriptor{Digest: manifestDigest, Size: int64(len(manifestBytes))}

	err := fetchAndWriteReferrerLayer(context.Background(), fetcher, desc, t.TempDir())
	if err == nil {
		t.Fatalf("expected error for oversize layer claim")
	}
	if !strings.Contains(err.Error(), "size limit") {
		t.Errorf("error should mention size limit; got %v", err)
	}
}

func TestFetchAndWriteReferrerLayer_RejectsBadManifestJSON(t *testing.T) {
	bogus := []byte("not json")
	manifestDigest := sha256Digest(bogus)
	fetcher := &fakeFetcher{blobs: map[digest.Digest][]byte{manifestDigest: bogus}}
	desc := ociv1.Descriptor{Digest: manifestDigest, Size: int64(len(bogus))}

	if err := fetchAndWriteReferrerLayer(context.Background(), fetcher, desc, t.TempDir()); err == nil {
		t.Fatalf("expected error for malformed manifest JSON")
	}
}
