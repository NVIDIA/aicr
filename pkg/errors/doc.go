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

// Package errors provides structured error types for better observability
// and programmatic error handling across the application.
//
// # Overview
//
// This package implements a structured error system with error codes for
// programmatic handling, human-readable messages, cause chaining, and
// optional context for debugging. It supports the standard errors.Is and
// errors.As functions through the Unwrap interface.
//
// # Error Codes
//
// Predefined error codes align with the API error contract:
//   - ErrCodeNotFound: Resource not found (HTTP 404)
//   - ErrCodeUnauthorized: Authentication/authorization failure (HTTP 401/403)
//   - ErrCodeTimeout: Operation timeout (HTTP 504)
//   - ErrCodeInternal: Internal server error (HTTP 500)
//   - ErrCodeInvalidRequest: Malformed or invalid input (HTTP 400)
//   - ErrCodeRateLimitExceeded: Rate limit exceeded (HTTP 429)
//   - ErrCodeMethodNotAllowed: HTTP method not allowed (HTTP 405)
//   - ErrCodeUnavailable: Service temporarily unavailable (HTTP 503)
//   - ErrCodeConflict: Resource state conflict, e.g., already exists or
//     version mismatch (HTTP 409). Distinct from ErrCodeInvalidRequest
//     because the request itself is well-formed.
//
// # Usage
//
// Create a simple error:
//
//	err := errors.New(errors.ErrCodeNotFound, "GPU not found")
//
// Wrap an existing error:
//
//	err := errors.Wrap(errors.ErrCodeInternal, "collection failed", originalErr)
//
// Wrap with additional context:
//
//	err := errors.WrapWithContext(
//	    errors.ErrCodeTimeout,
//	    "failed to collect GPU metrics",
//	    ctx.Err(),
//	    map[string]any{
//	        "command":  "nvidia-smi",
//	        "node":     nodeName,
//	        "timeout":  "10s",
//	    },
//	)
//
// # Error Handling
//
// The StructuredError type implements the standard error interface and
// supports error unwrapping:
//
//	var structErr *errors.StructuredError
//	if errors.As(err, &structErr) {
//	    log.Printf("Error code: %s, Message: %s", structErr.Code, structErr.Message)
//	    if structErr.Context != nil {
//	        log.Printf("Context: %v", structErr.Context)
//	    }
//	}
//
// # Code-Based Matching with errors.Is
//
// StructuredError.Is reports a match when the target is also a
// *StructuredError with the same Code. This enables idiomatic
// errors.Is checks without reaching for errors.As + manual code
// comparison:
//
//	if errors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
//	    // err (or any error in its Unwrap chain) carries
//	    // ErrCodeNotFound.
//	}
//
// Message and Cause are not compared; for cause-chain matching, rely
// on Unwrap as usual.
//
// # Thread Safety
//
// All functions in this package are thread-safe and can be called concurrently.
// Note: StructuredError.Context is a mutable map; if consumers mutate it
// post-construction, that mutation is the caller's responsibility.
package errors
