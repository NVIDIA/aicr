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
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// canonicalChainsawPath is the install location the deployment validator
// image ships chainsaw at (see issue #1220 / Dockerfile multi-stage
// COPY). Used as a fallback when exec.LookPath misses — e.g., an
// environment where /usr/local/bin is not on PATH but the binary is in
// fact present at that absolute path.
//
// Mutable (var, not const) so tests can substitute a TempDir-resolved
// path for the canonical one when needed.
var canonicalChainsawPath = "/usr/local/bin/chainsaw"

// ChainsawBinary abstracts chainsaw CLI invocation for testability.
type ChainsawBinary interface {
	// RunTest executes chainsaw test against the given test directory.
	// Returns whether all tests passed, the combined output, and any execution error.
	RunTest(ctx context.Context, testDir string) (passed bool, output string, err error)
}

type chainsawBinary struct {
	binPath string
}

// NewChainsawBinary creates a ChainsawBinary that invokes the chainsaw CLI.
// Resolves the binary path from PATH first, then falls back to the
// canonical install path shipped in the deployment validator image
// (#1220). If neither yields an executable, the constructor still
// returns a binary pointing at the canonical path so a subsequent
// RunTest invocation surfaces a clean "no such file or directory" — an
// image regression that must fail loudly rather than silently no-op.
//
// The Available()-gated skip path that briefly existed in PR #1231 was
// removed when #1220 made the chainsaw binary a hard requirement of the
// deployment validator image.
func NewChainsawBinary() ChainsawBinary {
	if binPath, err := exec.LookPath("chainsaw"); err == nil {
		return &chainsawBinary{binPath: binPath}
	}
	return &chainsawBinary{binPath: canonicalChainsawPath}
}

func (b *chainsawBinary) RunTest(ctx context.Context, testDir string) (bool, string, error) {
	slog.Debug("executing chainsaw binary", "binPath", b.binPath, "testDir", testDir)

	cmd := exec.CommandContext(ctx, b.binPath, "test", "--test-dir", testDir, "--no-color") //nolint:gosec // binPath is resolved from PATH or hardcoded, testDir is from os.MkdirTemp

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	output := buf.String()

	if err != nil {
		// Exit code != 0 means tests failed (not an execution error).
		var exitErr *exec.ExitError
		if stderrors.As(err, &exitErr) {
			if output == "" {
				output = fmt.Sprintf("chainsaw exited with code %d (no output captured)", exitErr.ExitCode())
			}
			return false, output, nil
		}
		return false, output, errors.Wrap(errors.ErrCodeInternal, "failed to execute chainsaw", err)
	}

	return true, output, nil
}
