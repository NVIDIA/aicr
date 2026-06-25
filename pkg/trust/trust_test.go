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

package trust

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestGetTrustedMaterial(t *testing.T) {
	t.Parallel()

	material, err := GetTrustedMaterial()
	if err != nil {
		t.Fatalf("GetTrustedMaterial() error: %v", err)
	}
	if material == nil {
		t.Fatal("GetTrustedMaterial() returned nil")
	}

	// Should have at least one Fulcio CA
	cas := material.FulcioCertificateAuthorities()
	if len(cas) == 0 {
		t.Error("expected at least one Fulcio certificate authority")
	}

	// Should have at least one Rekor log
	logs := material.RekorLogs()
	if len(logs) == 0 {
		t.Error("expected at least one Rekor transparency log")
	}
}

// TestUpdate_Success contacts the public Sigstore TUF CDN (tuf-repo-cdn.sigstore.dev).
// This is an integration test that requires network access. The CDN is public
// infrastructure with high availability — if it's down, Sigstore keyless signing
// is also down globally.
func TestUpdate_Success(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	material, err := Update(ctx)
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	if material == nil {
		t.Fatal("Update() returned nil material")
	}

	// Updated material should have at least one Fulcio CA and one Rekor log
	cas := material.FulcioCertificateAuthorities()
	if len(cas) == 0 {
		t.Error("expected at least one Fulcio certificate authority after update")
	}
	logs := material.RekorLogs()
	if len(logs) == 0 {
		t.Error("expected at least one Rekor transparency log after update")
	}
}

func TestUpdate_CancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := Update(ctx)
	if err == nil {
		t.Error("Update() with cancelled context should return error")
	}
}

func TestLoadTrustedMaterialFromFile(t *testing.T) {
	t.Run("valid file", func(t *testing.T) {
		tm, err := LoadTrustedMaterialFromFile("testdata/trusted_root.json")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tm == nil {
			t.Fatal("expected non-nil trusted material")
		}
	})
	t.Run("missing file is InvalidRequest", func(t *testing.T) {
		_, err := LoadTrustedMaterialFromFile("testdata/does-not-exist.json")
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("want ErrCodeInvalidRequest, got %v", err)
		}
	})
	t.Run("malformed JSON is InvalidRequest", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(p, []byte("{not valid json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadTrustedMaterialFromFile(p)
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("want ErrCodeInvalidRequest, got %v", err)
		}
	})
	t.Run("empty file is InvalidRequest", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "empty.json")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadTrustedMaterialFromFile(p)
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("want ErrCodeInvalidRequest, got %v", err)
		}
	})
	t.Run("oversized file is InvalidRequest", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "big.json")
		if err := os.WriteFile(p, make([]byte, defaults.MaxTrustedRootBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadTrustedMaterialFromFile(p)
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("want ErrCodeInvalidRequest, got %v", err)
		}
		// Assert on the distinct message to prove the size guard runs BEFORE
		// parse, rather than passing only because the zero bytes fail to parse.
		if !strings.Contains(err.Error(), "trust root file exceeds size limit") {
			t.Fatalf("want size-limit error message, got %v", err)
		}
	})
}
