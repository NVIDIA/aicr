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

package errors

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestErrorFormatting(t *testing.T) {
	t.Parallel()
	if got := New(ErrCodeNotFound, "plan missing").Error(); got != "[NOT_FOUND] plan missing" {
		t.Fatalf("New().Error() = %q", got)
	}
	cause := stderrors.New("boom")
	if got := Wrap(ErrCodeInternal, "render failed", cause).Error(); got != "[INTERNAL] render failed: boom" {
		t.Fatalf("Wrap().Error() = %q", got)
	}
}

func TestUnwrapAndIs(t *testing.T) {
	t.Parallel()
	cause := stderrors.New("boom")
	err := fmt.Errorf("outer: %w", Wrap(ErrCodeTimeout, "job deadline", cause))

	if !stderrors.Is(err, cause) {
		t.Fatal("errors.Is did not reach the wrapped cause")
	}
	if !stderrors.Is(err, New(ErrCodeTimeout, "")) {
		t.Fatal("errors.Is did not match by code")
	}
	if stderrors.Is(err, New(ErrCodeInternal, "")) {
		t.Fatal("errors.Is matched a different code")
	}
	if stderrors.Is(New("", "x"), New("", "y")) {
		t.Fatal("errors.Is matched empty codes")
	}
	if New(ErrCodeTimeout, "").Is(stderrors.New("plain")) {
		t.Fatal("Is matched a non-structured error")
	}
}

func TestCodeOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{"nil", nil, ""},
		{"structured", New(ErrCodeConflict, "x"), ErrCodeConflict},
		{"wrapped structured", fmt.Errorf("ctx: %w", New(ErrCodeNotFound, "x")), ErrCodeNotFound},
		{"canceled", context.Canceled, ErrCodeCanceled},
		{"deadline", fmt.Errorf("wait: %w", context.DeadlineExceeded), ErrCodeTimeout},
		{"plain", stderrors.New("plain"), ErrCodeInternal},
		{"empty code falls back", &StructuredError{Message: "x", Cause: context.Canceled}, ErrCodeCanceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := CodeOf(tt.err); got != tt.want {
				t.Fatalf("CodeOf() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The AICR exit-category mapping: invalid input, not found and unauthorized
// are permanent; timeout, unavailable and rate limited are transient; internal
// is neither (bounded retries, then stall); canceled is neither (shutdown).
func TestTransientAndPermanentClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code      ErrorCode
		transient bool
		permanent bool
	}{
		{ErrCodeInvalidRequest, false, true},
		{ErrCodeNotFound, false, true},
		{ErrCodeUnauthorized, false, true},
		{ErrCodeConflict, false, true},
		{ErrCodeTimeout, true, false},
		{ErrCodeUnavailable, true, false},
		{ErrCodeRateLimitExceeded, true, false},
		{ErrCodeInternal, false, false},
		{ErrCodeCanceled, false, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			t.Parallel()
			err := New(tt.code, "x")
			if got := IsTransient(err); got != tt.transient {
				t.Errorf("IsTransient = %v, want %v", got, tt.transient)
			}
			if got := IsPermanent(err); got != tt.permanent {
				t.Errorf("IsPermanent = %v, want %v", got, tt.permanent)
			}
		})
	}
	if IsTransient(nil) || IsPermanent(nil) {
		t.Fatal("nil classified as transient or permanent")
	}
}

func TestWithContextCopies(t *testing.T) {
	t.Parallel()
	base := New(ErrCodeInvalidRequest, "bad override").WithContext(map[string]any{"component": "gpu-operator"})
	derived := base.WithContext(map[string]any{"path": "driver.enabled"})

	if len(base.Context) != 1 {
		t.Fatalf("base context mutated: %v", base.Context)
	}
	if derived.Context["component"] != "gpu-operator" || derived.Context["path"] != "driver.enabled" {
		t.Fatalf("derived context = %v", derived.Context)
	}
	if derived.Code != ErrCodeInvalidRequest {
		t.Fatalf("derived code = %q", derived.Code)
	}
}

// Consumers bound every call with a context; a canceled or expired context
// must surface as CANCELED or TIMEOUT, never as INTERNAL.
func TestWrapCtxErrAtBoundary(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := CodeOf(WrapCtxErr(ctx.Err(), ErrCodeInternal, "list jobs")); got != ErrCodeCanceled {
		t.Fatalf("canceled context code = %q", got)
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if got := CodeOf(WrapCtxErr(ctx.Err(), ErrCodeInternal, "list jobs")); got != ErrCodeTimeout {
		t.Fatalf("expired context code = %q", got)
	}

	if got := CodeOf(WrapCtxErr(stderrors.New("refused"), ErrCodeUnavailable, "registry")); got != ErrCodeUnavailable {
		t.Fatalf("fallback code = %q", got)
	}
}

// An API request the server will always refuse is permanent, so the caller
// fails or stalls with its cause; one that may succeed later stays
// transient.
func TestWrapCtxErrClassifiesAPIErrors(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Group: "argoproj.io", Resource: "applications"}
	for name, tc := range map[string]struct {
		err  error
		want ErrorCode
	}{
		"admission denial (Invalid)":   {apierrors.NewInvalid(schema.GroupKind{Group: "argoproj.io", Kind: "Application"}, "x", nil), ErrCodeInvalidRequest},
		"admission denial (Forbidden)": {apierrors.NewForbidden(gr, "x", stderrors.New("ValidatingAdmissionPolicy denied request")), ErrCodeUnauthorized},
		"bad request":                  {apierrors.NewBadRequest("bad"), ErrCodeInvalidRequest},
		"too large":                    {apierrors.NewRequestEntityTooLargeError("limit"), ErrCodeInvalidRequest},
		"conflict":                     {apierrors.NewConflict(gr, "x", stderrors.New("modified")), ErrCodeUnavailable},
		"server timeout":               {apierrors.NewServerTimeout(gr, "update", 1), ErrCodeUnavailable},
		"throttled":                    {apierrors.NewTooManyRequests("slow down", 1), ErrCodeUnavailable},
		"internal":                     {apierrors.NewInternalError(stderrors.New("etcd")), ErrCodeUnavailable},
		"not found":                    {apierrors.NewNotFound(gr, "x"), ErrCodeUnavailable},
	} {
		got := WrapCtxErr(tc.err, ErrCodeUnavailable, "apply Application x")
		if got.Code != tc.want || IsPermanent(got) != IsPermanent(New(tc.want, "")) || !stderrors.Is(got, tc.err) {
			t.Errorf("%s: code %s, want %s", name, got.Code, tc.want)
		}
	}
}

func TestWithReason(t *testing.T) {
	t.Parallel()
	err := New(ErrCodeInvalidRequest, "bad").WithReason("AICRVersionMismatch")
	if got := ReasonOf(fmt.Errorf("wrapped: %w", err)); got != "AICRVersionMismatch" {
		t.Fatalf("ReasonOf() = %q", got)
	}
	if ReasonOf(New(ErrCodeInternal, "x")) != "" || ReasonOf(stderrors.New("plain")) != "" {
		t.Fatal("ReasonOf() invented a reason")
	}
}
