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

import (
	"fmt"
	"testing"
)

func TestExitCodeFromError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected int
	}{
		{
			name:     "nil error returns success",
			err:      nil,
			expected: ExitSuccess,
		},
		{
			name:     "structured error with invalid request",
			err:      New(ErrCodeInvalidRequest, "bad input"),
			expected: ExitInvalidInput,
		},
		{
			name:     "structured error with not found",
			err:      New(ErrCodeNotFound, "resource missing"),
			expected: ExitNotFound,
		},
		{
			name:     "structured error with unauthorized",
			err:      New(ErrCodeUnauthorized, "access denied"),
			expected: ExitUnauthorized,
		},
		{
			name:     "structured error with timeout",
			err:      New(ErrCodeTimeout, "operation timed out"),
			expected: ExitTimeout,
		},
		{
			name:     "structured error with unavailable",
			err:      New(ErrCodeUnavailable, "service down"),
			expected: ExitUnavailable,
		},
		{
			name:     "structured error with rate limit",
			err:      New(ErrCodeRateLimitExceeded, "too many requests"),
			expected: ExitRateLimited,
		},
		{
			name:     "structured error with internal",
			err:      New(ErrCodeInternal, "unexpected failure"),
			expected: ExitInternal,
		},
		{
			name:     "structured error with method not allowed",
			err:      New(ErrCodeMethodNotAllowed, "POST not allowed"),
			expected: ExitInvalidInput,
		},
		{
			name:     "wrapped structured error",
			err:      fmt.Errorf("command failed: %w", New(ErrCodeNotFound, "file missing")),
			expected: ExitNotFound,
		},
		{
			name:     "plain error returns generic exit code",
			err:      fmt.Errorf("something went wrong"),
			expected: ExitError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExitCodeFromError(tt.err)
			if result != tt.expected {
				t.Errorf("ExitCodeFromError() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestExitCodeFromErrorCode(t *testing.T) {
	tests := []struct {
		code     ErrorCode
		expected int
	}{
		{ErrCodeInvalidRequest, ExitInvalidInput},
		{ErrCodeMethodNotAllowed, ExitInvalidInput},
		{ErrCodeNotFound, ExitNotFound},
		{ErrCodeUnauthorized, ExitUnauthorized},
		{ErrCodeTimeout, ExitTimeout},
		{ErrCodeUnavailable, ExitUnavailable},
		{ErrCodeRateLimitExceeded, ExitRateLimited},
		{ErrCodeInternal, ExitInternal},
		{ErrorCode("UNKNOWN"), ExitError},
	}

	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			result := exitCodeFromErrorCode(tt.code)
			if result != tt.expected {
				t.Errorf("exitCodeFromErrorCode(%s) = %d, want %d", tt.code, result, tt.expected)
			}
		})
	}
}

func TestExitCodeConstants(t *testing.T) {
	// Verify exit codes follow conventions
	if ExitSuccess != 0 {
		t.Errorf("ExitSuccess should be 0, got %d", ExitSuccess)
	}
	if ExitError != 1 {
		t.Errorf("ExitError should be 1, got %d", ExitError)
	}

	// Verify application codes are in valid range (2-63)
	appCodes := []int{ExitInvalidInput, ExitNotFound, ExitUnauthorized, ExitTimeout, ExitUnavailable, ExitRateLimited, ExitInternal}
	for _, code := range appCodes {
		if code < 2 || code > 63 {
			t.Errorf("Application exit code %d should be in range 2-63", code)
		}
	}
}
