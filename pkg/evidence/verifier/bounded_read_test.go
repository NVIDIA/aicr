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
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestReadBoundedFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	body, err := readBoundedFile(path, "test file", 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want %q", body, `{"ok":true}`)
	}
}

// TestReadBoundedFile_RejectsOversize verifies that a payload one byte over
// the cap is rejected as ErrCodeInvalidRequest — the +1 LimitReader trick
// must surface the boundary, not silently truncate.
func TestReadBoundedFile_RejectsOversize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.json")
	// Cap = 16 bytes, file = 17 bytes
	if err := os.WriteFile(path, make([]byte, 17), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := readBoundedFile(path, "test file", 16)
	if err == nil {
		t.Fatal("expected error for oversized file, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}

func TestReadBoundedFile_ExactlyAtCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exact.bin")
	if err := os.WriteFile(path, make([]byte, 16), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	body, err := readBoundedFile(path, "test file", 16)
	if err != nil {
		t.Fatalf("at-cap read should succeed, got %v", err)
	}
	if len(body) != 16 {
		t.Errorf("len=%d, want 16", len(body))
	}
}

func TestReadBoundedFile_MissingFile(t *testing.T) {
	_, err := readBoundedFile(filepath.Join(t.TempDir(), "nope"), "missing", 1024)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
}
