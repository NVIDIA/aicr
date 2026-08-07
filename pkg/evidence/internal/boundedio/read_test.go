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

package boundedio

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func writeFile(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadFile_ReadsRegularFile(t *testing.T) {
	want := []byte("schemaVersion: 1.0.0\n")
	path := writeFile(t, t.TempDir(), "recipe.yaml", want)

	got, err := ReadFile(context.Background(), path, "bundle recipe.yaml", 1024)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("ReadFile = %q, want %q", got, want)
	}
}

// TestReadFile_RejectsOversize proves the size cap fires without allocating
// the whole file: the read must refuse at the cap rather than returning
// truncated bytes that a caller would then parse as if complete.
func TestReadFile_RejectsOversize(t *testing.T) {
	path := writeFile(t, t.TempDir(), "big.json", bytes.Repeat([]byte("a"), 512))

	got, err := ReadFile(context.Background(), path, "in-toto Statement", 128)
	if got != nil {
		t.Errorf("expected no body on oversize, got %d bytes", len(got))
	}
	if !isCode(err, errors.ErrCodeInvalidRequest) {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}

// TestReadFile_RejectsSymlink guards the O_NOFOLLOW hardening: an
// attacker-influenced bundle root can point a manifest-named file at
// /proc/self/mem or an unrelated secret, so the reader must refuse the
// indirection rather than read through it.
func TestReadFile_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := writeFile(t, dir, "target.yaml", []byte("secret"))
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := ReadFile(context.Background(), link, "pointer", 1024); !isCode(err, errors.ErrCodeInvalidRequest) {
		t.Errorf("expected ErrCodeInvalidRequest for a symlink, got %v", err)
	}
}

func TestReadFile_RejectsDirectory(t *testing.T) {
	if _, err := ReadFile(context.Background(), t.TempDir(), "bundle recipe.yaml", 1024); err == nil {
		t.Error("expected an error reading a directory, got nil")
	}
}

func TestReadFile_MissingFileIsNotFound(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	if _, err := ReadFile(context.Background(), missing, "bundle recipe.yaml", 1024); !isCode(err, errors.ErrCodeNotFound) {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
}

// TestReadFile_CanceledContextFailsClosed proves the read inherits the
// boundary: against a real, readable file (so an unbounded read would succeed
// instantly) an aborted caller gets a cancellation, never file contents.
func TestReadFile_CanceledContextFailsClosed(t *testing.T) {
	path := writeFile(t, t.TempDir(), "recipe.yaml", []byte("data"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := ReadFile(ctx, path, "bundle recipe.yaml", 1024)
	if got != nil {
		t.Errorf("expected no body after cancellation, got %q", got)
	}
	if !IsCanceled(err) {
		t.Errorf("expected a cancellation error, got %v", err)
	}
}

// TestReadFile_StorageFaultsAreNotVerdicts is the classification that keeps a
// soft-mounted NFS from being reported as a bad bundle. EIO/EACCES/ESTALE mean
// the filesystem could not answer; they say nothing about the file. Mapping
// them to ErrCodeInternal (or worse, NotFound) let the verifier fold them into
// the inventory mismatch rows and render a storage fault as an integrity
// failure — "this bundle has been tampered with" — at process exit 2.
//
// The opener is injected because chmod-based EACCES silently no-ops under root.
func TestReadFile_StorageFaultsAreNotVerdicts(t *testing.T) {
	tests := []struct {
		name     string
		injected error
		wantCode errors.ErrorCode
	}{
		{"I/O error", syscall.EIO, errors.ErrCodeUnavailable},
		{"stale NFS handle", syscall.ESTALE, errors.ErrCodeUnavailable},
		{"permission denied", syscall.EACCES, errors.ErrCodeUnavailable},
		{"no such device or address", syscall.ENXIO, errors.ErrCodeUnavailable},
		{"connection timed out", syscall.ETIMEDOUT, errors.ErrCodeUnavailable},
		// Absence is a real answer about the file, not a fault.
		{"absent", syscall.ENOENT, errors.ErrCodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			open := func(name string, _ int, _ os.FileMode) (*os.File, error) {
				return nil, &os.PathError{Op: "open", Path: name, Err: tt.injected}
			}

			_, err := readBoundedWithOpener("/bundle/recipe.yaml", "bundle recipe.yaml", 1024, open)
			if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Fatalf("got %v, want code %s", err, tt.wantCode)
			}
			// The chain must stay walkable so callers can still see the errno.
			if !stderrors.Is(err, tt.injected) {
				t.Errorf("underlying %v lost from the error chain: %v", tt.injected, err)
			}
		})
	}
}

// TestStatError_PostOpenClassification covers the descriptor-check branch a
// real file cannot reach: closing a descriptor yields os.ErrClosed, which is
// not a storage errno, so driving this through an actual file only ever
// exercises the Internal arm and would stay green if the IsStorageFault check
// were deleted outright.
func TestStatError_PostOpenClassification(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode errors.ErrorCode
	}{
		{"I/O error at stat", syscall.EIO, errors.ErrCodeUnavailable},
		{"stale handle at stat", syscall.ESTALE, errors.ErrCodeUnavailable},
		{"permission at stat", syscall.EACCES, errors.ErrCodeUnavailable},
		// Not an environmental fault: a closed descriptor is our own bug.
		{"closed descriptor", os.ErrClosed, errors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := statError("bundle recipe.yaml", tt.err)
			if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Fatalf("got %v, want code %s", err, tt.wantCode)
			}
			if !stderrors.Is(err, tt.err) {
				t.Errorf("underlying %v lost from the error chain: %v", tt.err, err)
			}
		})
	}
}

// TestReadFile_PostOpenStatFailureFailsClosed is the end-to-end companion: it
// drives a real descriptor failure through readBoundedWithOpener and asserts
// the reader fails closed rather than reporting absence. The precise code is
// covered by TestStatError_PostOpenClassification above.
func TestReadFile_PostOpenStatFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "recipe.yaml", []byte("data"))

	open := func(string, int, os.FileMode) (*os.File, error) {
		f, err := os.Open(path) //nolint:gosec // test fixture
		if err != nil {
			return nil, err
		}
		if cErr := f.Close(); cErr != nil {
			return nil, cErr
		}
		return f, nil
	}

	_, err := readBoundedWithOpener(path, "bundle recipe.yaml", 1024, open)
	if err == nil {
		t.Fatal("expected an error when the descriptor cannot be inspected")
	}
	if stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("post-open fault reported as absence: %v", err)
	}
}

// faultingReader yields n bytes and then fails, standing in for a mount that
// answers the open and faults mid-transfer.
type faultingReader struct {
	remaining int
	err       error
}

func (r *faultingReader) Read(p []byte) (int, error) {
	if r.remaining > 0 {
		n := min(len(p), r.remaining)
		for i := range n {
			p[i] = 'a'
		}
		r.remaining -= n
		return n, nil
	}
	return 0, r.err
}

// TestReadAllBounded_MidReadFaults covers the case the package exists for and
// that no earlier test reached: the mount answers open and stat, then faults
// part-way through the transfer. The opener seam cannot provoke this — it only
// faults before any bytes move — so without a reader seam a regression dropping
// the mid-read classification would misreport a storage fault as an exit-2
// "tampered bundle" and still pass CI.
func TestReadAllBounded_MidReadFaults(t *testing.T) {
	tests := []struct {
		name     string
		injected error
		wantCode errors.ErrorCode
	}{
		{"I/O error mid-read", syscall.EIO, errors.ErrCodeUnavailable},
		{"stale handle mid-read", syscall.ESTALE, errors.ErrCodeUnavailable},
		{"connection timed out mid-read", syscall.ETIMEDOUT, errors.ErrCodeUnavailable},
		// Not environmental: a corrupt archive is a content problem.
		{"unexpected EOF mid-read", io.ErrUnexpectedEOF, errors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &faultingReader{remaining: 8, err: tt.injected}

			body, err := readAllBounded(r, "bundle recipe.yaml", 1024)
			if body != nil {
				t.Errorf("expected no body on a mid-read fault, got %q", body)
			}
			if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Fatalf("got %v, want code %s", err, tt.wantCode)
			}
			if !stderrors.Is(err, tt.injected) {
				t.Errorf("underlying %v lost from the error chain: %v", tt.injected, err)
			}
		})
	}
}

// TestReadAllBounded_GrewAfterStat covers the other reader-side branch: the
// pre-read size check passed, then the file grew before the transfer. The +1 on
// the LimitReader is what catches it, and the oversize tests elsewhere all
// return at the pre-read stat, so this arm was previously unproven.
func TestReadAllBounded_GrewAfterStat(t *testing.T) {
	// 200 readable bytes against a 128-byte cap: stat said it fit, the read
	// says otherwise.
	r := &faultingReader{remaining: 200, err: io.EOF}

	body, err := readAllBounded(r, "bundle recipe.yaml", 128)
	if body != nil {
		t.Errorf("expected no body when the payload exceeds the cap, got %d bytes", len(body))
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("got %v, want ErrCodeInvalidRequest", err)
	}
}

// TestReadFile_NonErrnoOpenFailure covers the default open arm: an error that
// is neither ELOOP, nor absence, nor a storage errno must still be classified
// rather than falling through unwrapped.
func TestReadFile_NonErrnoOpenFailure(t *testing.T) {
	open := func(string, int, os.FileMode) (*os.File, error) {
		return nil, stderrors.New("some non-errno failure")
	}

	_, err := readBoundedWithOpener("/bundle/recipe.yaml", "bundle recipe.yaml", 1024, open)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("got %v, want ErrCodeInternal", err)
	}
}
