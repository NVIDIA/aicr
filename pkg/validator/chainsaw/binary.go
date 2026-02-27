// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
}

type chainsawBinary struct {
	binPath string
}

// NewChainsawBinary creates a ChainsawBinary that invokes the chainsaw CLI.
// It resolves the binary path from PATH, falling back to /usr/local/bin/chainsaw.
func NewChainsawBinary() ChainsawBinary {
	binPath, err := exec.LookPath("chainsaw")
	if err != nil {
		binPath = "/usr/local/bin/chainsaw"
	}
	return &chainsawBinary{binPath: binPath}
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
