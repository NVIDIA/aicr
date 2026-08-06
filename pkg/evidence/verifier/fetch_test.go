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
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestMaterializeBundle_DirAcceptsParentOrSummary(t *testing.T) {
	bundleDir := buildTestBundle(t)

	mat, err := MaterializeBundle(context.Background(),
		VerifyOptions{Input: bundleDir}, InputFormDir, nil)
	if err != nil {
		t.Fatalf("MaterializeBundle(parent): %v", err)
	}
	mat.Cleanup()

	mat2, err := MaterializeBundle(context.Background(),
		VerifyOptions{Input: summaryDirOf(t, bundleDir)}, InputFormDir, nil)
	if err != nil {
		t.Fatalf("MaterializeBundle(summary): %v", err)
	}
	mat2.Cleanup()
}

func TestMaterializeBundle_DirRejectsNonBundle(t *testing.T) {
	_, err := MaterializeBundle(context.Background(),
		VerifyOptions{Input: t.TempDir()}, InputFormDir, nil)
	if err == nil {
		t.Errorf("expected error for empty directory")
	}
}

// TestBundleMarkerProbe_CanceledCtxPropagatesAbort verifies that a
// canceled caller context surfaces as an ErrCodeCanceled error through the
// bundle-marker probe rather than being flattened into a benign "not a
// bundle" (ErrCodeInvalidRequest) answer: at these probe sites a hung NFS/FUSE
// mount fails closed, not open. Both marker-probing sites are exercised:
// materializeDir (directory input) and resolveBundleDir (pulled-artifact
// input), which share attestation.HasBundleMarkers underneath.
//
// Scope: this bounds the marker probe only. It is not an end-to-end guarantee
// for `aicr evidence verify` — DetectInputForm stats the input dir before the
// probe, and several post-materialize reads are still unbounded; both are
// tracked in #2083. The test calls the probes directly, so it must not be
// read as proving the whole verify path is hang-immune.
func TestBundleMarkerProbe_CanceledCtxPropagatesAbort(t *testing.T) {
	// A real bundle so a live context would succeed — proving the failure
	// comes from cancellation, not a missing bundle.
	bundleDir := buildTestBundle(t)

	tests := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{
			name: "materializeDir",
			call: func(ctx context.Context) error {
				_, err := materializeDir(ctx, bundleDir)
				return err
			},
		},
		{
			name: "resolveBundleDir",
			call: func(ctx context.Context) error {
				_, err := resolveBundleDir(ctx, bundleDir)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := tt.call(ctx)
			if err == nil {
				t.Fatalf("%s: expected error on canceled ctx, got nil", tt.name)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeCanceled, "")) {
				t.Errorf("%s: got %v, want ErrCodeCanceled (must not flatten to \"not a bundle\")", tt.name, err)
			}
			if stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("%s: canceled ctx was misreported as ErrCodeInvalidRequest (fail-open): %v", tt.name, err)
			}
		})
	}
}

// TestBundleMarkerProbe_LiveCtxSucceeds is the positive control: with an
// uncanceled context the same probes accept a real bundle, confirming the
// timeout paths above are cancellation-driven, not always-erroring.
func TestBundleMarkerProbe_LiveCtxSucceeds(t *testing.T) {
	bundleDir := buildTestBundle(t)

	mat, err := materializeDir(context.Background(), bundleDir)
	if err != nil {
		t.Fatalf("materializeDir(live ctx): %v", err)
	}
	mat.Cleanup()

	if _, err := resolveBundleDir(context.Background(), bundleDir); err != nil {
		t.Fatalf("resolveBundleDir(live ctx): %v", err)
	}
}

func TestFormatOCIReference(t *testing.T) {
	tests := []struct {
		name              string
		registry, repo, t string
		want              string
	}{
		{"tag", "ghcr.io", "owner/repo", "v1", "ghcr.io/owner/repo:v1"},
		{"digest", "ghcr.io", "owner/repo", "sha256:" + strings.Repeat("a", 64),
			"ghcr.io/owner/repo@sha256:" + strings.Repeat("a", 64)},
		{"localhost tag", "localhost:5000", "repo", "latest", "localhost:5000/repo:latest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatOCIReference(tt.registry, tt.repo, tt.t)
			if got != tt.want {
				t.Errorf("formatOCIReference = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseOCIReference(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantReg    string
		wantRepo   string
		wantTarget string
		wantErr    bool
	}{
		{"with tag", "ghcr.io/owner/aicr-evidence:v1", "ghcr.io", "owner/aicr-evidence", "v1", false},
		{"with digest", "ghcr.io/owner/aicr-evidence@sha256:" + strings.Repeat("a", 64),
			"ghcr.io", "owner/aicr-evidence", "sha256:" + strings.Repeat("a", 64), false},
		{"oci scheme prefix", "oci://ghcr.io/owner/aicr-evidence:v1",
			"ghcr.io", "owner/aicr-evidence", "v1", false},
		{"missing target", "ghcr.io/owner/aicr-evidence", "", "", "", true},
		{"invalid", "::not-a-ref", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, repo, target, err := parseOCIReference(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if reg != tt.wantReg || repo != tt.wantRepo || target != tt.wantTarget {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)",
					reg, repo, target, tt.wantReg, tt.wantRepo, tt.wantTarget)
			}
		})
	}
}

func TestPointerPullRef(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name    string
		ref     string
		digest  string
		want    string
		wantErr bool
	}{
		{
			name:   "tag ref with digest pins by digest",
			ref:    "ghcr.io/owner/aicr-evidence:h100-eks-ubuntu-training",
			digest: digest,
			want:   "ghcr.io/owner/aicr-evidence@" + digest,
		},
		{
			name:   "scheme-prefixed tag ref with digest",
			ref:    "oci://ghcr.io/owner/aicr-evidence:v1",
			digest: digest,
			want:   "ghcr.io/owner/aicr-evidence@" + digest,
		},
		{
			name:   "already digest ref with digest re-pins to same digest",
			ref:    "ghcr.io/owner/aicr-evidence@" + digest,
			digest: digest,
			want:   "ghcr.io/owner/aicr-evidence@" + digest,
		},
		{
			name:   "empty digest returns ref unchanged",
			ref:    "ghcr.io/owner/aicr-evidence:v1",
			digest: "",
			want:   "ghcr.io/owner/aicr-evidence:v1",
		},
		{
			name:    "invalid ref with digest errors",
			ref:     "::not-a-ref",
			digest:  digest,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pointerPullRef(tt.ref, tt.digest)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("pointerPullRef(%q, %q) = %q, want %q", tt.ref, tt.digest, got, tt.want)
			}
		})
	}
}
