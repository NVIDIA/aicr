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
	"os"
	"os/exec"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// canonicalChainsawPath is the install location the deployment validator
// image will use once the chainsaw binary ships (see issue #1220). Probed
// as a fallback when exec.LookPath misses, e.g., when the image places
// chainsaw at a known absolute path but does not extend PATH.
//
// Mutable (var, not const) so tests can swap it for a TempDir entry — the
// real /usr/local/bin/chainsaw may exist on developer machines, which
// would otherwise make Available()==false assertions flake.
var canonicalChainsawPath = "/usr/local/bin/chainsaw"

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
// It resolves the binary path from PATH first, then probes the canonical
// install path (/usr/local/bin/chainsaw) for an executable file. The
// rationale for the fallback probe: a container image may place chainsaw
// at a known absolute path without extending PATH, in which case
// exec.LookPath misses but the file is still callable directly. Without
// the probe, Available() would report false while RunTest could in fact
// succeed — an inconsistency flagged in PR #1231 review.
//
// Availability is recorded at construction time so callers can branch on
// it without repeating the discovery.
func NewChainsawBinary() ChainsawBinary {
	if binPath, err := exec.LookPath("chainsaw"); err == nil {
		return &chainsawBinary{binPath: binPath, available: true}
	}
	if isExecutableFile(canonicalChainsawPath) {
		return &chainsawBinary{binPath: canonicalChainsawPath, available: true}
	}
	// Fall through with the canonical install path so the eventual
	// RunTest error names a real, expected location; Available() reports
	// false so the deployment validator can short-circuit Test-format
	// dispatch upstream.
	return &chainsawBinary{binPath: canonicalChainsawPath, available: false}
}

// isExecutableFile reports whether path resolves to a regular file with
// at least one execute bit set. Used by NewChainsawBinary's fallback
// probe; symlinks resolve via os.Stat. Errors (ENOENT, EACCES) are
// treated as "not executable" — the probe is best-effort and the
// caller's downstream RunTest will surface any deeper issue.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
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
