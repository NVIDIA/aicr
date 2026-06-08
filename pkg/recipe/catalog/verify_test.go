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

package catalog_test

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe/catalog"
)

func TestVerify_BundleNotFound(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "does-not-exist.sigstore.json")

	_, err := catalog.Verify(ctx, missing, newFullProvider(), catalog.VerifyOptions{})
	if err == nil {
		t.Fatal("expected error for missing bundle, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
}

func TestVerify_InvalidBundle(t *testing.T) {
	ctx := context.Background()
	bundlePath := filepath.Join(t.TempDir(), "garbage.sigstore.json")
	if err := os.WriteFile(bundlePath, []byte(`{"not":"a real bundle"}`), 0o600); err != nil {
		t.Fatalf("write garbage bundle: %v", err)
	}

	_, err := catalog.Verify(ctx, bundlePath, newFullProvider(), catalog.VerifyOptions{})
	if err == nil {
		t.Fatal("expected error for invalid bundle, got nil")
	}
}

// TestVerify_RejectsOverlyBroadIdentityPattern locks in the safety contract
// documented on VerifyOptions.CertificateIdentityRegexp: an override that
// does not contain "NVIDIA/aicr" must be rejected with ErrCodeInvalidRequest
// rather than silently disabling effective repo pinning. Without this guard
// `aicr recipe verify-catalog --identity-pattern '.*'` would treat any
// GitHub Actions OIDC identity as valid.
func TestVerify_RejectsOverlyBroadIdentityPattern(t *testing.T) {
	ctx := context.Background()
	bundlePath := filepath.Join(t.TempDir(), "bundle.sigstore.json")
	if err := os.WriteFile(bundlePath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("seed bundle: %v", err)
	}

	_, err := catalog.Verify(ctx, bundlePath, newFullProvider(), catalog.VerifyOptions{
		CertificateIdentityRegexp: ".*",
	})
	if err == nil {
		t.Fatal("expected error for overly broad identity pattern, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}
