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

// Package errors provides the operator's structured errors.
//
// The codes match NVIDIA/aicr's pkg/errors vocabulary, so an AICR exit
// category maps onto the same code here. The operator imports no
// AICR package, so this is a separate, smaller implementation of the same
// shape.
package errors

import (
	"context"
	stderrors "errors"
	"fmt"
	"maps"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ErrorCode classifies an error for handling: permanent codes wait for an
// input change, transient codes retry with backoff.
type ErrorCode string

// Error codes. Values match NVIDIA/aicr's pkg/errors.
const (
	ErrCodeNotFound          ErrorCode = "NOT_FOUND"
	ErrCodeUnauthorized      ErrorCode = "UNAUTHORIZED"
	ErrCodeTimeout           ErrorCode = "TIMEOUT"
	ErrCodeInternal          ErrorCode = "INTERNAL"
	ErrCodeInvalidRequest    ErrorCode = "INVALID_REQUEST"
	ErrCodeRateLimitExceeded ErrorCode = "RATE_LIMIT_EXCEEDED"
	ErrCodeUnavailable       ErrorCode = "SERVICE_UNAVAILABLE"
	ErrCodeConflict          ErrorCode = "CONFLICT"
	ErrCodeCanceled          ErrorCode = "CANCELED"
)

// StructuredError is an error with a code, a message safe to surface in
// conditions and Events, and optional context for structured logs.
type StructuredError struct {
	Code    ErrorCode
	Message string
	Context map[string]any
	Cause   error
}

// Error implements error.
func (e *StructuredError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// Unwrap returns the cause.
func (e *StructuredError) Unwrap() error { return e.Cause }

// Is reports whether target is a StructuredError with the same code, so
// errors.Is(err, New(ErrCodeTimeout, "")) matches any timeout.
func (e *StructuredError) Is(target error) bool {
	var t *StructuredError
	if !stderrors.As(target, &t) {
		return false
	}
	if e.Code == "" || t.Code == "" {
		return false
	}
	return e.Code == t.Code
}

// New returns a StructuredError without a cause.
func New(code ErrorCode, message string) *StructuredError {
	return &StructuredError{Code: code, Message: message}
}

// Wrap returns a StructuredError that wraps cause.
func Wrap(code ErrorCode, message string, cause error) *StructuredError {
	return &StructuredError{Code: code, Message: message, Cause: cause}
}

// ReasonKey is the context key carrying a condition reason for an error.
const ReasonKey = "reason"

// WithReason records the condition reason a controller reports for e.
func (e *StructuredError) WithReason(reason string) *StructuredError {
	return e.WithContext(map[string]any{ReasonKey: reason})
}

// ReasonOf returns the condition reason an error carries, if any.
func ReasonOf(err error) string {
	var se *StructuredError
	if !stderrors.As(err, &se) {
		return ""
	}
	r, _ := se.Context[ReasonKey].(string)
	return r
}

// WithContext returns a copy of e with the given key/value context added.
func (e *StructuredError) WithContext(ctx map[string]any) *StructuredError {
	out := *e
	out.Context = make(map[string]any, len(e.Context)+len(ctx))
	maps.Copy(out.Context, e.Context)
	maps.Copy(out.Context, ctx)
	return &out
}

// CodeOf returns the code of the first StructuredError in err's chain. A bare
// context.Canceled is CANCELED and a bare context.DeadlineExceeded is TIMEOUT;
// any other error is INTERNAL. A nil error has no code.
func CodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var se *StructuredError
	if stderrors.As(err, &se) && se.Code != "" {
		return se.Code
	}
	switch {
	case stderrors.Is(err, context.Canceled):
		return ErrCodeCanceled
	case stderrors.Is(err, context.DeadlineExceeded):
		return ErrCodeTimeout
	default:
		return ErrCodeInternal
	}
}

// IsTransient reports whether err should be retried with backoff:
// timeouts, unavailability and rate limiting.
func IsTransient(err error) bool {
	switch CodeOf(err) {
	case ErrCodeTimeout, ErrCodeUnavailable, ErrCodeRateLimitExceeded:
		return true
	case ErrCodeNotFound, ErrCodeUnauthorized, ErrCodeInternal, ErrCodeInvalidRequest,
		ErrCodeConflict, ErrCodeCanceled:
		return false
	default:
		return false
	}
}

// IsPermanent reports whether err should stall until its input changes:
// invalid input, not found, unauthorized and conflicts.
func IsPermanent(err error) bool {
	switch CodeOf(err) {
	case ErrCodeInvalidRequest, ErrCodeNotFound, ErrCodeUnauthorized, ErrCodeConflict:
		return true
	case ErrCodeTimeout, ErrCodeInternal, ErrCodeRateLimitExceeded, ErrCodeUnavailable,
		ErrCodeCanceled:
		return false
	default:
		return false
	}
}

// WrapCtxErr wraps cause, preserving cancellation and deadline as CANCELED
// and TIMEOUT so callers never misreport shutdown or a bounded wait as an
// internal failure. A Kubernetes API refusal that no retry of the same
// request can change (an invalid object, an admission or authorization
// denial, a malformed or oversized request) is permanent, so the caller
// fails or stalls with its cause instead of retrying until a deadline;
// conflicts, timeouts, throttling and server errors stay transient. Other
// causes get fallbackCode.
func WrapCtxErr(cause error, fallbackCode ErrorCode, message string) *StructuredError {
	switch {
	case stderrors.Is(cause, context.Canceled):
		return Wrap(ErrCodeCanceled, message, cause)
	case stderrors.Is(cause, context.DeadlineExceeded):
		return Wrap(ErrCodeTimeout, message, cause)
	case apierrors.IsInvalid(cause), apierrors.IsBadRequest(cause), apierrors.IsRequestEntityTooLargeError(cause):
		return Wrap(ErrCodeInvalidRequest, message, cause)
	case apierrors.IsForbidden(cause):
		return Wrap(ErrCodeUnauthorized, message, cause)
	default:
		return Wrap(fallbackCode, message, cause)
	}
}
