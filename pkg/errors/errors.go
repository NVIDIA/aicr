// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

import "fmt"

// ErrorCode represents a structured error classification.
type ErrorCode string

const (
	// ErrCodeNotFound indicates a requested resource was not found.
	ErrCodeNotFound ErrorCode = "NOT_FOUND"
	// ErrCodeUnauthorized indicates authentication or authorization failure.
	ErrCodeUnauthorized ErrorCode = "UNAUTHORIZED"
	// ErrCodeTimeout indicates an operation exceeded its time limit.
	ErrCodeTimeout ErrorCode = "TIMEOUT"
	// ErrCodeInternal indicates an internal system error.
	ErrCodeInternal ErrorCode = "INTERNAL"
	// ErrCodeInvalidRequest indicates malformed or invalid input.
	ErrCodeInvalidRequest ErrorCode = "INVALID_REQUEST"
	// ErrCodeRateLimitExceeded indicates the client exceeded an enforced request limit.
	ErrCodeRateLimitExceeded ErrorCode = "RATE_LIMIT_EXCEEDED"
	// ErrCodeMethodNotAllowed indicates the HTTP method is not allowed for the resource.
	ErrCodeMethodNotAllowed ErrorCode = "METHOD_NOT_ALLOWED"
	// ErrCodeUnavailable indicates a service or resource is temporarily unavailable.
	//
	// Note: this value is aligned with the public API error contract.
	ErrCodeUnavailable ErrorCode = "SERVICE_UNAVAILABLE"
)

// StructuredError provides structured error information for better observability.
// It includes an error code for programmatic handling, a human-readable message,
// the underlying cause, and optional context for debugging.
type StructuredError struct {
	Code    ErrorCode
	Message string
	Cause   error
	Context map[string]any
}

// Error implements the error interface.
func (e *StructuredError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// Unwrap returns the underlying cause for errors.Is and errors.As support.
func (e *StructuredError) Unwrap() error {
	return e.Cause
}

// New creates a new StructuredError with the given code and message.
func New(code ErrorCode, message string) *StructuredError {
	return &StructuredError{
		Code:    code,
		Message: message,
	}
}

// NewWithContext creates a new StructuredError with context information.
func NewWithContext(code ErrorCode, message string, context map[string]any) *StructuredError {
	return &StructuredError{
		Code:    code,
		Message: message,
		Context: context,
	}
}

// Wrap wraps an existing error with additional context.
func Wrap(code ErrorCode, message string, cause error) *StructuredError {
	return &StructuredError{
		Code:    code,
		Message: message,
		Cause:   cause,
	}
}

// WrapWithContext wraps an error with additional context information.
func WrapWithContext(code ErrorCode, message string, cause error, context map[string]any) *StructuredError {
	return &StructuredError{
		Code:    code,
		Message: message,
		Cause:   cause,
		Context: context,
	}
}
