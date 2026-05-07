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
	"fmt"
	"net"
	"net/url"
	"syscall"
	"testing"
)

func TestIsNetworkError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "plain error",
			err:  fmt.Errorf("something went wrong"),
			want: false,
		},
		{
			name: "net.OpError dial timeout",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &net.DNSError{
					Err:       "no such host",
					Name:      "api.example.com",
					IsTimeout: true,
				},
			},
			want: true,
		},
		{
			name: "net.OpError connection refused",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: syscall.ECONNREFUSED,
			},
			want: true,
		},
		{
			name: "syscall ECONNREFUSED",
			err:  syscall.ECONNREFUSED,
			want: true,
		},
		{
			name: "syscall EHOSTUNREACH",
			err:  syscall.EHOSTUNREACH,
			want: true,
		},
		{
			name: "syscall ENETUNREACH",
			err:  syscall.ENETUNREACH,
			want: true,
		},
		{
			name: "syscall EPERM is not network",
			err:  syscall.EPERM,
			want: false,
		},
		{
			name: "url.Error wrapping net.OpError",
			err: &url.Error{
				Op:  "Get",
				URL: "https://api.example.com",
				Err: &net.OpError{
					Op:  "dial",
					Net: "tcp",
					Err: &net.DNSError{Err: "no such host", Name: "api.example.com"},
				},
			},
			want: true,
		},
		{
			name: "wrapped string with dial tcp",
			err:  fmt.Errorf("request failed: dial tcp 10.0.0.1:443: i/o timeout"),
			want: true,
		},
		{
			name: "wrapped string with no such host",
			err:  fmt.Errorf("request failed: no such host"),
			want: true,
		},
		{
			name: "wrapped string with connection refused",
			err:  fmt.Errorf("request failed: connection refused"),
			want: true,
		},
		{
			name: "wrapped string with TLS handshake timeout",
			err:  fmt.Errorf("request failed: TLS handshake timeout"),
			want: true,
		},
		{
			name: "context.DeadlineExceeded is not network",
			err:  context.DeadlineExceeded,
			want: false,
		},
		{
			name: "context.Canceled is not network",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "wrapped context.DeadlineExceeded is not network",
			err:  fmt.Errorf("operation failed: %w", context.DeadlineExceeded),
			want: false,
		},
		{
			name: "StructuredError wrapping network error",
			err: &StructuredError{
				Code:    ErrCodeInternal,
				Message: "failed to deploy",
				Cause: &net.OpError{
					Op:  "dial",
					Net: "tcp",
					Err: syscall.ECONNREFUSED,
				},
			},
			want: true,
		},
		{
			name: "DNS error",
			err: &net.DNSError{
				Err:  "lookup failed",
				Name: "api.example.com",
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsNetworkError(tt.err)
			if got != tt.want {
				t.Errorf("IsNetworkError() = %v, want %v", got, tt.want)
			}
		})
	}
}
