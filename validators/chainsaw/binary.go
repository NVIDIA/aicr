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

// ChainsawBinary abstracts chainsaw CLI invocation for testability.
type ChainsawBinary interface {
	// RunTest executes chainsaw test against the given test directory.
	// Returns whether all tests passed, the combined output, and any execution error.
	RunTest(ctx context.Context, testDir string) (passed bool, output string, err error)
	// Available reports whether the chainsaw binary is callable from this
	// process. Used by the deployment validator to skip Chainsaw Test-format
	// dispatch when the binary is absent (e.g., the validator image has not
	// shipped chainsaw yet), preserving today's no-op behavior while
	// registry-declared HealthCheckAsserts content hydrates upstream in
	// pkg/recipe.
	Available() bool
}

type chainsawBinary struct {
	binPath   string
	available bool
}

// NewChainsawBinary creates a ChainsawBinary that invokes the chainsaw CLI.
// It resolves the binary path from PATH, falling back to /usr/local/bin/chainsaw.
// Availability is recorded at construction time so callers can branch on it
// without repeating the exec.LookPath probe.
func NewChainsawBinary() ChainsawBinary {
	binPath, err := exec.LookPath("chainsaw")
	if err != nil {
		// Fall through with the canonical install path; RunTest will surface
		// the missing-binary error if invoked, but Available() reports false
		// so the deployment validator can short-circuit upstream.
		return &chainsawBinary{binPath: "/usr/local/bin/chainsaw", available: false}
	}
	return &chainsawBinary{binPath: binPath, available: true}
}

func (b *chainsawBinary) Available() bool { return b.available }

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
