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

package server

import (
	"errors"
	"net/http"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/google/uuid"
)

// ErrorResponse represents error responses as per OpenAPI spec
type ErrorResponse struct {
	Code      string         `json:"code" yaml:"code"`
	Message   string         `json:"message" yaml:"message"`
	Details   map[string]any `json:"details,omitempty" yaml:"details,omitempty"`
	RequestID string         `json:"requestId" yaml:"requestId"`
	Timestamp time.Time      `json:"timestamp" yaml:"timestamp"`
	Retryable bool           `json:"retryable" yaml:"retryable"`
}

// writeError writes error response
func WriteError(w http.ResponseWriter, r *http.Request, statusCode int,
	code aicrerrors.ErrorCode, message string, retryable bool, details map[string]any) {

	requestID, _ := r.Context().Value(contextKeyRequestID).(string)
	if requestID == "" {
		requestID = uuid.New().String()
	}

	errResp := ErrorResponse{
		Code:      string(code),
		Message:   message,
		Details:   details,
		RequestID: requestID,
		Timestamp: time.Now().UTC(),
		Retryable: retryable,
	}

	serializer.RespondJSON(w, statusCode, errResp)
}

// HTTPStatusFromCode maps a canonical error code to an HTTP status.
// This keeps transport-layer semantics centralized.
func HTTPStatusFromCode(code aicrerrors.ErrorCode) int {
	switch code {
	case aicrerrors.ErrCodeInvalidRequest:
		return http.StatusBadRequest
	case aicrerrors.ErrCodeUnauthorized:
		return http.StatusUnauthorized
	case aicrerrors.ErrCodeNotFound:
		return http.StatusNotFound
	case aicrerrors.ErrCodeMethodNotAllowed:
		return http.StatusMethodNotAllowed
	case aicrerrors.ErrCodeRateLimitExceeded:
		return http.StatusTooManyRequests
	case aicrerrors.ErrCodeUnavailable:
		return http.StatusServiceUnavailable
	case aicrerrors.ErrCodeTimeout:
		// Prefer 504 for upstream timeouts and internal deadline exceeded.
		return http.StatusGatewayTimeout
	case aicrerrors.ErrCodeInternal:
		fallthrough
	default:
		return http.StatusInternalServerError
	}
}

func retryableFromCode(code aicrerrors.ErrorCode) bool {
	switch code {
	case aicrerrors.ErrCodeInvalidRequest,
		aicrerrors.ErrCodeUnauthorized,
		aicrerrors.ErrCodeNotFound,
		aicrerrors.ErrCodeMethodNotAllowed:
		return false
	case aicrerrors.ErrCodeTimeout,
		aicrerrors.ErrCodeUnavailable,
		aicrerrors.ErrCodeRateLimitExceeded,
		aicrerrors.ErrCodeInternal:
		return true
	}

	// Defensive fallback (should be unreachable if codes are kept in sync).
	return false
}

func mergeDetails(a, b map[string]any) map[string]any {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// WriteErrorFromErr writes an ErrorResponse based on a canonical structured error.
// If err is not a *errors.StructuredError, it falls back to INTERNAL.
func WriteErrorFromErr(w http.ResponseWriter, r *http.Request, err error, fallbackMessage string, extraDetails map[string]any) {
	if err == nil {
		WriteError(w, r, http.StatusInternalServerError, aicrerrors.ErrCodeInternal,
			fallbackMessage, true, extraDetails)
		return
	}

	var se *aicrerrors.StructuredError
	if errors.As(err, &se) {
		msg := se.Message
		if msg == "" {
			msg = fallbackMessage
		}

		details := mergeDetails(se.Context, extraDetails)
		if se.Cause != nil {
			details = mergeDetails(details, map[string]any{"error": se.Cause.Error()})
		}

		WriteError(w, r, HTTPStatusFromCode(se.Code), se.Code, msg, retryableFromCode(se.Code), details)
		return
	}

	WriteError(w, r, http.StatusInternalServerError, aicrerrors.ErrCodeInternal,
		fallbackMessage, true, mergeDetails(extraDetails, map[string]any{"error": err.Error()}))
}
