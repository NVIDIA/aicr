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

package boundedio

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func isCode(err error, code errors.ErrorCode) bool {
	return stderrors.Is(err, errors.New(code, ""))
}

// TestDo_PropagatesFnError proves the boundary is transparent on the happy
// path: fn's own coded error reaches the caller unchanged, so callers keep
// their NotFound/InvalidRequest diagnostics instead of everything becoming a
// timeout.
func TestDo_PropagatesFnError(t *testing.T) {
	want := errors.New(errors.ErrCodeNotFound, "no such thing")
	got := Do(context.Background(), "probe", func() error { return want })
	if !stderrors.Is(got, want) {
		t.Errorf("Do returned %v, want the fn error %v", got, want)
	}
}

// TestDo_DeadlineSurfacesTimeout covers the wedged-mount case: fn never
// returns, the boundary's own deadline fires, and the caller gets
// ErrCodeTimeout so classifyFailure can treat it as transient.
func TestDo_DeadlineSurfacesTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	err := doWithTimeout(context.Background(), 10*time.Millisecond, "wedged read", func() error {
		<-release
		return nil
	})
	if !isCode(err, errors.ErrCodeTimeout) {
		t.Fatalf("expected ErrCodeTimeout, got %v", err)
	}
	if !stderrors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected the deadline in the chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "wedged read") {
		t.Errorf("expected the label in the message, got %q", err.Error())
	}
}

// TestDo_OperatorCancelIsNotTimeout is the load-bearing distinction: a
// deliberate Ctrl-C (parent cancellation) must NOT be reported as a timeout,
// because ErrCodeTimeout drives both the transient failure class and the
// retryable bucket. A canceled command is neither.
func TestDo_OperatorCancelIsNotTimeout(t *testing.T) {
	tests := []struct {
		name       string
		cancelWhen string
	}{
		{"canceled before the call", "before"},
		{"canceled while fn is blocked", "inflight"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var err error
			switch tt.cancelWhen {
			case "before":
				cancel()
				err = Do(ctx, "probe", func() error { return nil })
			default:
				started := make(chan struct{})
				release := make(chan struct{})
				defer close(release)
				errCh := make(chan error, 1)
				go func() {
					errCh <- Do(ctx, "probe", func() error {
						close(started)
						<-release
						return nil
					})
				}()
				<-started
				cancel()
				err = <-errCh
			}

			if isCode(err, errors.ErrCodeTimeout) {
				t.Errorf("operator cancellation was classified as a timeout: %v", err)
			}
			if !IsCanceled(err) {
				t.Errorf("expected IsCanceled to report true, got %v", err)
			}
			if strings.Contains(err.Error(), "timed out") {
				t.Errorf("cancellation message reads as a timeout: %q", err.Error())
			}
		})
	}
}

// TestDo_CallerUnblocksWhileWorkerRuns proves the boundary actually delivers
// hang-immunity: the caller returns while fn is still executing. It does NOT
// prove the worker is reclaimed — a worker wedged in kernel I/O parks until
// the syscall returns or the process exits, which is the documented tradeoff.
// The release/join below only confirms an unblocked worker completes normally.
func TestDo_CallerUnblocksWhileWorkerRuns(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})

	errCh := make(chan error, 1)
	go func() {
		errCh <- doWithTimeout(context.Background(), 10*time.Millisecond, "wedged read", func() error {
			close(started)
			<-release
			close(finished)
			return nil
		})
	}()

	<-started
	if err := <-errCh; !isCode(err, errors.ErrCodeTimeout) {
		t.Fatalf("caller did not unblock with a timeout, got %v", err)
	}

	select {
	case <-finished:
		t.Fatal("worker finished before release — the test did not exercise an in-flight bound")
	default:
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("released worker never completed")
	}
}

// TestDo_CompletionDoesNotOutrankCancellation pins the select-race behavior.
// When fn finishes at the same moment the context terminates, both channels
// are ready and Go's select chooses pseudo-randomly. Without an explicit
// recheck the boundary would sometimes return fn's success for a run the
// operator had already canceled — failing open, and nondeterministically, on
// exactly the abort it exists to enforce.
//
// fn cancels its own parent immediately before returning, so both cases are
// always ready when the select runs.
func TestDo_CompletionDoesNotOutrankCancellation(t *testing.T) {
	for i := range 200 {
		ctx, cancel := context.WithCancel(context.Background())

		err := Do(ctx, "probe", func() error {
			cancel() // both done and ctx.Done() are now ready
			return nil
		})

		if !IsCanceled(err) {
			t.Fatalf("iteration %d: canceled run returned %v, want a cancellation", i, err)
		}
	}
}

// TestDo_SuccessSurvivesALiveContext is the converse guard: the recheck above
// must not discard a good result when nothing was canceled.
func TestDo_SuccessSurvivesALiveContext(t *testing.T) {
	for i := range 200 {
		if err := Do(context.Background(), "probe", func() error { return nil }); err != nil {
			t.Fatalf("iteration %d: healthy run returned %v", i, err)
		}
	}
}
