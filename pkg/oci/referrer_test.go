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
	"io"
	"os"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

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
	mainDigest := "sha256:" + strings.Repeat("a", 64)
	mainMediaType := "application/vnd.oci.image.manifest.v1+json"
	const mainSize = int64(1234)

	subject := ociv1.Descriptor{
		MediaType: mainMediaType,
		Digest:    digestFromString(t, mainDigest),
		Size:      mainSize,
	}

	fs, tmpDir, tag, err := packReferrer(context.Background(), ReferrerOptions{
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
		_ = fs.Close()
		_ = os.RemoveAll(tmpDir)
	}()

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
			_, _, _, err := packReferrer(context.Background(), tt.opts)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}
