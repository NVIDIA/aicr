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

import "context"

// contextKey is a custom type for context keys to avoid collisions
type contextKey string

const (
	// contextKeyRequestID is the context key for request ID
	contextKeyRequestID contextKey = "requestID"
	// contextKeyAPIVersion is the context key for API version
	contextKeyAPIVersion contextKey = "apiVersion"
)

// RequestIDFromContext returns the request ID stored in ctx by
// requestIDMiddleware. Returns an empty string if the value is missing
// or not of type string. Safe to call with a nil context.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(contextKeyRequestID).(string)
	return id
}
