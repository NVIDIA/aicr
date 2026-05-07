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

import (
	"context"
	"testing"
)

func TestRequestIDFromContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() context.Context
		want string
	}{
		{
			name: "value present",
			ctx: func() context.Context {
				return context.WithValue(context.Background(), contextKeyRequestID, "abc-123")
			},
			want: "abc-123",
		},
		{
			name: "value missing",
			ctx:  context.Background,
			want: "",
		},
		{
			name: "value of wrong type",
			ctx: func() context.Context {
				return context.WithValue(context.Background(), contextKeyRequestID, 42)
			},
			want: "",
		},
		{
			name: "empty string value",
			ctx: func() context.Context {
				return context.WithValue(context.Background(), contextKeyRequestID, "")
			},
			want: "",
		},
		{
			name: "nil context",
			//nolint:staticcheck // SA1012: intentionally testing nil-context guard
			ctx: func() context.Context {
				return nil
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RequestIDFromContext(tt.ctx())
			if got != tt.want {
				t.Errorf("RequestIDFromContext() = %q, want %q", got, tt.want)
			}
		})
	}
}
