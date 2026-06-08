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

// TestNewChainsawBinary covers the two discovery branches:
//   - exec.LookPath hit  → use the PATH-resolved path
//   - exec.LookPath miss → fall back to the canonical install path
//
// canonicalChainsawPath is patched per case so a developer machine with a
// real /usr/local/bin/chainsaw can't influence the miss branch.
func TestNewChainsawBinary(t *testing.T) {
	tests := []struct {
		name string
		// createStub controls whether an executable stub is placed in the
		// PATH dir for this case. true → simulates "chainsaw on PATH";
		// false → simulates "chainsaw missing from PATH".
		createStub bool
		// wantBinPath is a function so the assertion can refer to the
		// case-local TempDir paths that aren't known until t.Run runs.
		wantBinPath func(pathDir, canonical string) string
	}{
		{
			name:       "PATH hit uses the resolved path",
			createStub: true,
			wantBinPath: func(pathDir, _ string) string {
				return filepath.Join(pathDir, "chainsaw")
			},
		},
		{
			name:       "PATH miss falls back to canonical path string",
			createStub: false,
			wantBinPath: func(_, canonical string) string {
				return canonical
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pathDir := t.TempDir()
			if tt.createStub {
				stub := filepath.Join(pathDir, "chainsaw")
				if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test fixture
					t.Fatalf("write stub: %v", err)
				}
			}
			t.Setenv("PATH", pathDir)

			fakeCanonical := filepath.Join(t.TempDir(), "chainsaw")
			origCanonical := canonicalChainsawPath
			canonicalChainsawPath = fakeCanonical
			t.Cleanup(func() { canonicalChainsawPath = origCanonical })

			bin := NewChainsawBinary().(*chainsawBinary)
			want := tt.wantBinPath(pathDir, fakeCanonical)
			if bin.binPath != want {
				t.Errorf("binPath = %q, want %q", bin.binPath, want)
			}
		})
	}
}
