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

package bundler

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// TestReadBoundedFile pins the size-guard helper used for binary
// attestations and other in-bundle reads that could otherwise be swapped
// post-validation. The read-time bound is the authoritative limit, so
// both the limit-enforced and not-exist paths must return appropriate
// error codes.
func TestReadBoundedFile(t *testing.T) {
	t.Run("reads a file under the limit", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "small")
		want := []byte("payload")
		if err := os.WriteFile(path, want, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := readBoundedFile(path, 1024)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("rejects file that exceeds the limit", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "big")
		if err := os.WriteFile(path, make([]byte, 128), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := readBoundedFile(path, 32)
		if err == nil {
			t.Fatal("expected oversize error")
		}
		var se *aicrerrors.StructuredError
		if !stderrors.As(err, &se) {
			t.Fatalf("expected StructuredError, got %T", err)
		}
		if se.Code != aicrerrors.ErrCodeInvalidRequest {
			t.Errorf("code = %v, want %v", se.Code, aicrerrors.ErrCodeInvalidRequest)
		}
	})

	t.Run("propagates os.Open error for missing file", func(t *testing.T) {
		_, err := readBoundedFile(filepath.Join(t.TempDir(), "missing"), 1024)
		if err == nil {
			t.Fatal("expected error for missing file")
		}
		if !os.IsNotExist(err) {
			t.Errorf("err = %v, want os.IsNotExist", err)
		}
	})
}
