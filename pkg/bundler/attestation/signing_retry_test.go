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

package attestation

import (
	"context"
	stderrors "errors"
	"sync/atomic"
	"testing"
	"time"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestSignWithRetry_SuccessOnFirstAttempt verifies the happy path:
// attempt returns nil error → no retry, no backoff, immediate return.
func TestSignWithRetry_SuccessOnFirstAttempt(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	want := &protobundle.Bundle{}
	got, err := signWithRetry(context.Background(), func(_ context.Context) (*protobundle.Bundle, error) {
		calls.Add(1)
		return want, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("returned wrong bundle")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("expected 1 attempt, got %d", n)
	}
}

// TestSignWithRetry_SuccessAfterTransient verifies retry-on-transient:
// attempt fails once with a non-ctx error, then succeeds. Total wall
// clock should equal one backoff (SigstoreRetryInitialBackoff).
func TestSignWithRetry_SuccessAfterTransient(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	want := &protobundle.Bundle{}
	start := time.Now()
	got, err := signWithRetry(context.Background(), func(_ context.Context) (*protobundle.Bundle, error) {
		n := calls.Add(1)
		if n == 1 {
			return nil, stderrors.New("simulated Rekor 503")
		}
		return want, nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("returned wrong bundle")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("expected 2 attempts, got %d", n)
	}
	// Lower-bound only: the backoff was honored if elapsed >=
	// SigstoreRetryInitialBackoff. Upper bound dropped per PR #1251
	// review — loaded CI runners can legitimately push past a tight
	// multiple, and the attempt count above already proves there
	// wasn't an extra retry.
	if elapsed < defaults.SigstoreRetryInitialBackoff {
		t.Errorf("elapsed %v < expected backoff %v (backoff not honored?)",
			elapsed, defaults.SigstoreRetryInitialBackoff)
	}
}

// TestSignWithRetry_BudgetExhaustion verifies full-budget exhaustion:
// attempt always fails → all SigstoreRetryBudget attempts run, last
// error is wrapped as ErrCodeUnavailable.
func TestSignWithRetry_BudgetExhaustion(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	sentinel := stderrors.New("simulated Rekor 503")
	_, err := signWithRetry(context.Background(), func(_ context.Context) (*protobundle.Bundle, error) {
		calls.Add(1)
		return nil, sentinel
	})
	if err == nil {
		t.Fatal("expected error after budget exhaustion, got nil")
	}
	if n := calls.Load(); int(n) != defaults.SigstoreRetryBudget {
		t.Errorf("expected %d attempts, got %d", defaults.SigstoreRetryBudget, n)
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %T", err)
	}
	if se.Code != errors.ErrCodeUnavailable {
		t.Errorf("expected ErrCodeUnavailable, got %v", se.Code)
	}
	if !stderrors.Is(err, sentinel) {
		t.Errorf("expected wrapped sentinel error in chain, got: %v", err)
	}
}

// TestSignWithRetry_OuterDeadlineExceeded verifies that the outer ctx
// deadline expiring during an attempt short-circuits with
// ErrCodeTimeout — no further retries.
func TestSignWithRetry_OuterDeadlineExceeded(t *testing.T) {
	t.Parallel()

	// Outer ctx with a deadline shorter than one backoff cycle. The
	// first attempt fails, then the backoff sleep blows the deadline.
	ctx, cancel := context.WithTimeout(context.Background(), defaults.SigstoreRetryInitialBackoff/2)
	defer cancel()

	var calls atomic.Int32
	_, err := signWithRetry(ctx, func(_ context.Context) (*protobundle.Bundle, error) {
		calls.Add(1)
		return nil, stderrors.New("simulated Rekor 503")
	})
	if err == nil {
		t.Fatal("expected error after deadline, got nil")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %T", err)
	}
	if se.Code != errors.ErrCodeTimeout {
		t.Errorf("expected ErrCodeTimeout (outer deadline), got %v", se.Code)
	}
	// Should have run at most 2 attempts: one before backoff, possibly
	// one if the deadline check is racy. Never the full budget.
	if int(calls.Load()) >= defaults.SigstoreRetryBudget {
		t.Errorf("retry continued past deadline: %d attempts (budget %d)",
			calls.Load(), defaults.SigstoreRetryBudget)
	}
}

// TestSignWithRetry_OuterCanceled verifies that caller cancellation
// terminates the retry loop with ErrCodeUnavailable (not Timeout —
// cancellation is intent, not deadline).
func TestSignWithRetry_OuterCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	var calls atomic.Int32
	// Cancel during the first attempt's backoff sleep.
	go func() {
		time.Sleep(defaults.SigstoreRetryInitialBackoff / 4)
		cancel()
	}()

	_, err := signWithRetry(ctx, func(_ context.Context) (*protobundle.Bundle, error) {
		calls.Add(1)
		return nil, stderrors.New("simulated Rekor 503")
	})
	if err == nil {
		t.Fatal("expected error after cancel, got nil")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %T", err)
	}
	if se.Code != errors.ErrCodeUnavailable {
		t.Errorf("expected ErrCodeUnavailable (caller cancel), got %v", se.Code)
	}
}
