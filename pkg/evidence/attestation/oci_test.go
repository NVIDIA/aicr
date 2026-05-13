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
	"testing"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
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

func TestNewKeylessSigner_DefaultURLs(t *testing.T) {
	s := NewKeylessSigner("token")
	if s.OIDCToken != "token" {
		t.Errorf("OIDCToken not set")
	}
	if s.FulcioURL != bundleattest.DefaultFulcioURL {
		t.Errorf("FulcioURL = %q", s.FulcioURL)
	}
	if s.RekorURL != bundleattest.DefaultRekorURL {
		t.Errorf("RekorURL = %q", s.RekorURL)
	}
}

func TestKeylessSigner_RejectsEmptyToken(t *testing.T) {
	s := &KeylessSigner{FulcioURL: bundleattest.DefaultFulcioURL, RekorURL: bundleattest.DefaultRekorURL}
	if _, err := s.Sign(context.Background(), []byte("statement")); err == nil {
		t.Errorf("expected error on empty OIDC token")
	}
}

func TestKeylessSigner_RejectsEmptyStatement(t *testing.T) {
	s := NewKeylessSigner("dummy")
	if _, err := s.Sign(context.Background(), nil); err == nil {
		t.Errorf("expected error on empty statement")
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
