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

// TestNewChainsawBinary_Available exercises both branches of the
// availability probe in NewChainsawBinary by manipulating PATH so the
// chainsaw binary is found / not found. Required so the deployment
// validator's runtime gate (which keeps issue #1219 runtime-neutral
// until #1220 ships the binary) is itself covered.
func TestNewChainsawBinary_Available(t *testing.T) {
	t.Run("unavailable when not on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		bin := NewChainsawBinary()
		if bin.Available() {
			t.Fatal("Available() = true, want false when chainsaw is not on PATH")
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
		bin := NewChainsawBinary()
		if !bin.Available() {
			t.Fatal("Available() = false, want true when chainsaw is on PATH")
		}
	})
}
