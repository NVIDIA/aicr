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

package helm

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Binary is the helm executable Run looks up on PATH.
const Binary = "helm"

// commandFunc has the signature of exec.CommandContext, which builds the
// command run executes outside tests.
type commandFunc func(ctx context.Context, name string, args ...string) *exec.Cmd

// Run runs helm with args, stopping after timeout or when ctx ends, and
// returns its stdout. On failure it also returns the stdout written so far,
// with an error that has code
//   - ErrCodeInvalidRequest when args is empty
//   - ErrCodeInternal when stdout exceeds outputLimit bytes
//   - ErrCodeTimeout or ErrCodeCanceled when timeout or ctx ends first
//   - ErrCodeInternal otherwise, with helm's "Error:" line in the message and
//     the full stderr in its context
func Run(ctx context.Context, timeout time.Duration, outputLimit int64, args ...string) ([]byte, error) {
	return run(ctx, exec.CommandContext, nil, timeout, outputLimit, args...)
}

func run(ctx context.Context, command commandFunc, stdin io.Reader, timeout time.Duration, outputLimit int64,
	args ...string) ([]byte, error) {

	if len(args) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "helm command is required")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdoutBuf, stderr bytes.Buffer
	stdout := &limitedWriter{w: &stdoutBuf, limit: outputLimit}
	cmd := command(ctx, Binary, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdoutBuf.Bytes(), nil
	}
	name := "helm " + args[0]
	if stdout.exceeded {
		return stdoutBuf.Bytes(), errors.New(errors.ErrCodeInternal, name+" output exceeds size limit")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdoutBuf.Bytes(), errors.WrapCtxErr(ctxErr, errors.ErrCodeTimeout, name+" did not finish")
	}
	detail := strings.TrimSpace(stderr.String())
	msg := name + " failed"
	if summary := summarizeHelmError(detail); summary != "" {
		msg += ": " + summary
	}
	return stdoutBuf.Bytes(), errors.WrapWithContext(errors.ErrCodeInternal, msg, err,
		map[string]any{"args": strings.Join(args, " "), "stderr": detail})
}
