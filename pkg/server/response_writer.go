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

package server

import "net/http"

// responseWriter wraps http.ResponseWriter to track response status and prevent
// writing headers after the body has been written. This ensures proper HTTP semantics
// and helps catch middleware bugs where headers are set too late.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

// newResponseWriter creates a new responseWriter that wraps the provided http.ResponseWriter.
func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		written:        false,
	}
}

// WriteHeader writes the HTTP status code. It only writes the first call,
// subsequent calls are ignored to prevent duplicate header writes.
func (rw *responseWriter) WriteHeader(statusCode int) {
	if rw.written {
		return // Prevent duplicate writes
	}
	rw.statusCode = statusCode
	rw.ResponseWriter.WriteHeader(statusCode)
	rw.written = true
}

// Write writes the response body. If WriteHeader hasn't been called,
// it automatically calls it with http.StatusOK.
func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// Status returns the HTTP status code that was written.
func (rw *responseWriter) Status() int {
	return rw.statusCode
}
