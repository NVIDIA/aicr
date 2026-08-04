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
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestAssertSingleDocument_PropagatesStructuredCode guards the raw-path grace:
// an absent resource must surface ErrCodeNotFound (propagated from the fetcher),
// NOT a blanket ErrCodeInternal wrap. If it were wrapped as Internal,
// resourceObservedErr would latch sawResource=true on the first iteration and
// the absent-resource fast-fail grace would never fire in assertRawResources.
func TestAssertSingleDocument_PropagatesStructuredCode(t *testing.T) {
	expected := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      "missing",
			"namespace": "ns",
		},
	}
	err := assertSingleDocument(context.Background(), expected, newFakeFetcher())
	if err == nil {
		t.Fatal("expected an error for an absent resource, got nil")
	}
	if !isNotFoundErr(err) {
		t.Fatalf("expected the fetcher's NotFound to be propagated, got %v", err)
	}
	if resourceObservedErr(err) {
		t.Errorf("an absent resource must not be treated as observed (would defeat the raw-path grace): %v", err)
	}
}

// TestAssertRawResources_TerminalErrorFailsFast guards that the raw-resource
// retry loop honors terminal errors: a malformed assert (missing required
// fields => ErrCodeInvalidRequest) must fail fast, not retry until the timeout
// and hold a worker slot. Mirrors the in-process runAssertWithRetry behavior.
func TestAssertRawResources_TerminalErrorFailsFast(t *testing.T) {
	t.Parallel()
	// Raw (non-Test) YAML missing metadata.name => assertSingleDocument returns
	// ErrCodeInvalidRequest (terminal).
	ca := ComponentAssert{
		Name: "bad-assert",
		AssertYAML: `apiVersion: v1
kind: Pod
metadata:
  namespace: ns`,
	}
	start := time.Now()
	// Generous timeout: a correct terminal short-circuit returns immediately; a
	// regression (retrying the permanent error) blocks >= one AssertRetryInterval.
	r := assertRawResources(context.Background(), ca, time.Hour, newFakeFetcher())
	elapsed := time.Since(start)

	if r.Error == nil {
		t.Fatalf("expected a terminal error, got nil (Passed=%v)", r.Passed)
	}
	if !stderrors.Is(r.Error, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("expected ErrCodeInvalidRequest (terminal), got %v", r.Error)
	}
	if elapsed >= defaults.AssertRetryInterval {
		t.Fatalf("terminal error was retried (took %s >= AssertRetryInterval %s)", elapsed, defaults.AssertRetryInterval)
	}
}

func TestPreferSubstantiveErr(t *testing.T) {
	sub := errors.New(errors.ErrCodeNotFound, "resource not found")
	fallback := errors.Wrap(errors.ErrCodeInternal, "context canceled during assertion", context.Canceled)

	// Identity comparison is intentional: preferSubstantiveErr returns one of
	// its two arguments verbatim, so we assert the exact value, not error code
	// equivalence.
	if got := preferSubstantiveErr(sub, fallback); got != sub { //nolint:errorlint // identity check by design
		t.Errorf("preferSubstantiveErr(sub, fallback) = %v, want the substantive error", got)
	}
	if got := preferSubstantiveErr(nil, fallback); got != fallback { //nolint:errorlint // identity check by design
		t.Errorf("preferSubstantiveErr(nil, fallback) = %v, want the fallback", got)
	}
}

// TestRunChainsawTestInProcess_SurfacesSubstantiveErrorOnCancel verifies the fix
// for the masked-cancellation bug end to end through the retry loop: when the
// parent context is canceled mid-retry, the check must surface the last
// substantive assertion error (here NotFound) rather than the ErrCodeInternal
// context-cancellation wrap. A regression would return the opaque cancellation
// error and the check would report `other` instead of a clean `failed` with the
// real reason.
func TestRunChainsawTestInProcess_SurfacesSubstantiveErrorOnCancel(t *testing.T) {
	t.Parallel()
	// Assert a single resource the (empty) fake fetcher does not have, so every
	// evaluation returns a substantive ErrCodeNotFound.
	const yaml = `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: missing-resource
spec:
  steps:
    - name: assert-missing
      try:
        - assert:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                name: missing
                namespace: ns
`
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after the first evaluation records the substantive NotFound,
	// while the retry loop is parked in its select.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	// Generous step budget so the loop exits via context cancellation, not the
	// deadline.
	r := runChainsawTestInProcess(ctx, "missing-resource", yaml, time.Hour, newFakeFetcher())
	if r.Error == nil {
		t.Fatalf("expected an error, got nil (Passed=%v)", r.Passed)
	}
	if !isNotFoundErr(r.Error) {
		t.Fatalf("expected the substantive NotFound to be surfaced on cancel, got %v", r.Error)
	}
	if stderrors.Is(r.Error, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("error must not be the ErrCodeInternal cancellation wrap: %v", r.Error)
	}
}
