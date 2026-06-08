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

package chainsaw

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNewChainsawBinary_Available exercises every branch of the
// availability probe in NewChainsawBinary by manipulating PATH and the
// canonical fallback path so the binary is found / not found. The
// fallback-path branch was added in PR #1231 review (yuanchen8911):
// previously NewChainsawBinary reported Available()==false even when
// /usr/local/bin/chainsaw existed and was callable.
//
// canonicalChainsawPath is swapped to a TempDir entry per subtest so
// the real /usr/local/bin/chainsaw on developer machines doesn't make
// the unavailable assertions flake.
func TestNewChainsawBinary_Available(t *testing.T) {
	withCanonicalPath := func(t *testing.T, p string) {
		orig := canonicalChainsawPath
		canonicalChainsawPath = p
		t.Cleanup(func() { canonicalChainsawPath = orig })
	}

	t.Run("unavailable when missing from PATH and fallback path", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		withCanonicalPath(t, filepath.Join(t.TempDir(), "chainsaw"))
		bin := NewChainsawBinary()
		if bin.Available() {
			t.Fatal("Available() = true, want false when chainsaw is on neither PATH nor fallback")
		}
	})

	t.Run("available when discoverable on PATH", func(t *testing.T) {
		dir := t.TempDir()
		stub := filepath.Join(dir, "chainsaw")
		// 0o755: an exec.LookPath-discoverable executable is the whole
		// point of this fixture.
		if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write stub: %v", err)
		}
		t.Setenv("PATH", dir)
		withCanonicalPath(t, filepath.Join(t.TempDir(), "chainsaw"))
		bin := NewChainsawBinary()
		if !bin.Available() {
			t.Fatal("Available() = false, want true when chainsaw is on PATH")
		}
	})

	t.Run("available via fallback path when PATH misses", func(t *testing.T) {
		// PATH dir has no chainsaw, but the canonical fallback does.
		t.Setenv("PATH", t.TempDir())
		fallbackDir := t.TempDir()
		fallback := filepath.Join(fallbackDir, "chainsaw")
		if err := os.WriteFile(fallback, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write fallback: %v", err)
		}
		withCanonicalPath(t, fallback)
		bin := NewChainsawBinary()
		if !bin.Available() {
			t.Fatal("Available() = false, want true when fallback path is executable")
		}
	})
}

// TestIsExecutableFile covers the fallback probe used by NewChainsawBinary
// when PATH lookup misses.
func TestIsExecutableFile(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) string
		want  bool
	}{
		{
			name: "executable regular file",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "chainsaw")
				if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // test fixture
					t.Fatalf("write: %v", err)
				}
				return p
			},
			want: true,
		},
		{
			name: "regular file without exec bit",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "chainsaw")
				if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
				return p
			},
			want: false,
		},
		{
			name: "missing path",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist")
			},
			want: false,
		},
		{
			name: "directory (not a regular file)",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isExecutableFile(tt.setup(t)); got != tt.want {
				t.Fatalf("isExecutableFile = %v, want %v", got, tt.want)
			}
		})
	}
}
