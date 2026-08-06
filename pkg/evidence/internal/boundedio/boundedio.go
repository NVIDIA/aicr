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

// Package boundedio puts blocking local-filesystem work behind a cancellation
// boundary so the evidence publish/sign/verify paths cannot hang indefinitely
// on a dead NFS/FUSE mount.
//
// Why a goroutine and not a context: a context deadline bounds *observation*,
// not the syscall. os.Stat, os.Open, and the ReadDir calls inside
// filepath.WalkDir block in the kernel on a wedged mount, so a ctx.Err() check
// between chunks or between walk entries never runs. Only moving the syscall
// off the calling goroutine lets the caller return.
//
// The accepted tradeoff: on timeout the worker stays parked in the syscall
// until the kernel returns or the process exits. That is fine for the
// short-lived CLI leaf commands that use this package, and it is the reason
// this is internal rather than a general-purpose utility.
package boundedio

import (
	"context"
	stderrors "errors"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Do runs fn behind a defaults.FileReadTimeout-bounded derivative of ctx and
// returns fn's own error, or a boundary error if the bound elapses or the
// operator aborts first. fn must be a self-contained unit of blocking
// filesystem work that owns everything it opens: once Do returns, the caller
// must not touch any handle fn is still using.
func Do(ctx context.Context, what string, fn func() error) error {
	return doWithTimeout(ctx, defaults.FileReadTimeout, what, fn)
}

// IsCanceled reports whether err is a deliberate operator abort rather than an
// environmental fault. Callers that bucket failures as retry-vs-stop use this
// to keep a Ctrl-C out of the retryable bucket.
func IsCanceled(err error) bool {
	return stderrors.Is(err, errors.New(errors.ErrCodeCanceled, ""))
}

func doWithTimeout(parent context.Context, timeout time.Duration, what string, fn func() error) error {
	// Fail closed and deterministically when the caller's context is already
	// done, before launching the worker — otherwise a fast syscall could win
	// the select race against an already-expired context.
	if parent.Err() != nil {
		return boundaryError(parent.Err(), what, timeout)
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	done := make(chan error, 1) // buffered so an abandoned worker never blocks
	go func() { done <- fn() }()

	select {
	case err := <-done:
		// A completed fn does not outrank an abort. When done and ctx.Done()
		// are both ready the select chooses pseudo-randomly, so without this
		// recheck a canceled run could return fn's success — the boundary
		// would fail OPEN, nondeterministically, on exactly the abort it
		// exists to enforce. Recheck explicitly so the outcome is decided by
		// the context, not by scheduler chance.
		if cause := terminationCause(parent, ctx); cause != nil {
			return boundaryError(cause, what, timeout)
		}
		return err
	case <-ctx.Done():
		return boundaryError(terminationCause(parent, ctx), what, timeout)
	}
}

// terminationCause reports why the operation should be abandoned, or nil if it
// should not be. The parent takes precedence: an operator abort must be
// attributed to the operator, not to the derived deadline that fires with it.
func terminationCause(parent, ctx context.Context) error {
	if err := parent.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// boundaryError shapes the two outcomes the boundary itself can produce.
// Keeping them distinct matters downstream: ErrCodeTimeout drives the
// verifier's transient failure class and the retryable bucket, and reporting a
// deliberate Ctrl-C as "timed out" would put an operator's abort into both.
func boundaryError(cause error, what string, timeout time.Duration) error {
	if stderrors.Is(cause, context.Canceled) {
		return errors.WrapWithContext(errors.ErrCodeCanceled,
			"canceled while reading "+what, cause,
			map[string]any{"operation": what})
	}
	return errors.WrapWithContext(errors.ErrCodeTimeout,
		"timed out reading "+what, cause,
		map[string]any{"operation": what, "timeout": timeout.String()})
}
