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

import "errors"

// Exit codes for CLI commands, following Unix conventions.
// These codes enable predictable scripting and automation.
//
// Ranges:
//   - 0: Success
//   - 1: Generic error (catch-all)
//   - 2-63: Application-specific errors
const (
	// ExitSuccess indicates successful execution.
	ExitSuccess = 0

	// ExitError is the generic error code for unclassified failures.
	ExitError = 1

	// ExitInvalidInput indicates malformed input, validation failure, or bad arguments.
	// Maps to: ErrCodeInvalidRequest, ErrCodeMethodNotAllowed
	ExitInvalidInput = 2

	// ExitNotFound indicates a requested resource was not found.
	// Maps to: ErrCodeNotFound
	ExitNotFound = 3

	// ExitUnauthorized indicates authentication or authorization failure.
	// Maps to: ErrCodeUnauthorized
	ExitUnauthorized = 4

	// ExitTimeout indicates an operation exceeded its time limit.
	// Maps to: ErrCodeTimeout
	ExitTimeout = 5

	// ExitUnavailable indicates a service or resource is temporarily unavailable.
	// Maps to: ErrCodeUnavailable
	ExitUnavailable = 6

	// ExitRateLimited indicates the client exceeded a rate limit.
	// Maps to: ErrCodeRateLimitExceeded
	ExitRateLimited = 7

	// ExitInternal indicates an internal error (reserved for unexpected failures).
	// Maps to: ErrCodeInternal
	ExitInternal = 8
)

// ExitCodeFromError extracts an appropriate exit code from an error.
// It checks for StructuredError to determine the specific exit code,
// falling back to ExitError (1) for unstructured errors.
func ExitCodeFromError(err error) int {
	if err == nil {
		return ExitSuccess
	}

	var structErr *StructuredError
	if errors.As(err, &structErr) {
		return exitCodeFromErrorCode(structErr.Code)
	}

	// Default to generic error for unstructured errors
	return ExitError
}

// exitCodeFromErrorCode maps an ErrorCode to its corresponding exit code.
func exitCodeFromErrorCode(code ErrorCode) int {
	switch code {
	case ErrCodeInvalidRequest, ErrCodeMethodNotAllowed:
		return ExitInvalidInput
	case ErrCodeNotFound:
		return ExitNotFound
	case ErrCodeUnauthorized:
		return ExitUnauthorized
	case ErrCodeTimeout:
		return ExitTimeout
	case ErrCodeUnavailable:
		return ExitUnavailable
	case ErrCodeRateLimitExceeded:
		return ExitRateLimited
	case ErrCodeInternal:
		return ExitInternal
	default:
		return ExitError
	}
}
