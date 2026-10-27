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

package os

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// hostReadTimeout is a variable so tests can shorten it.
var hostReadTimeout = defaults.HostReadTimeout

// errHostReadStalled marks a read that readHostFile stopped waiting on.
var errHostReadStalled = stderrors.New("host read stalled")

// readHostFile returns read(path), giving up if read does not return within
// hostReadTimeout or before ctx ends. An abandoned read keeps its OS thread
// until the kernel returns, and the process cannot be reaped while it waits.
//
// Errors:
//   - ctx ends: ErrCodeTimeout, which is transient
//   - timeout: ErrCodeUnavailable wrapping errHostReadStalled, after a warning
//     that names path
func readHostFile[T any](ctx context.Context, path string, read func(string) (T, error)) (T, error) {
	type result struct {
		val T
		err error
	}
	// A procfs handler blocked on a kernel lock sleeps uninterruptibly, out of
	// reach of ctx and signals, so the read is abandoned rather than joined.
	// Buffered so an abandoned read that eventually returns can still exit.
	done := make(chan result, 1)
	go func() {
		val, err := read(path)
		done <- result{val: val, err: err}
	}()

	timer := time.NewTimer(hostReadTimeout)
	defer timer.Stop()

	var zero T
	select {
	case r := <-done:
		return r.val, r.err
	case <-ctx.Done():
		return zero, errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout,
			fmt.Sprintf("reading %s", path))
	case <-timer.C:
		slog.Warn("host read did not return, skipping it; the node may need attention, and the agent pod may be unable to exit",
			slog.String("path", path),
			slog.Duration("timeout", hostReadTimeout))
		// Not transient, so the OS collector skips only this subtype.
		return zero, errors.WrapWithContext(errors.ErrCodeUnavailable,
			fmt.Sprintf("read of %s did not return within %v", path, hostReadTimeout),
			errHostReadStalled, map[string]any{"path": path})
	}
}

// isHostReadStalled reports whether err came from a read readHostFile
// abandoned.
func isHostReadStalled(err error) bool {
	return stderrors.Is(err, errHostReadStalled)
}
