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

package defaults

import (
	"testing"
	"time"
)

func TestNewHTTPTransport(t *testing.T) {
	tr := NewHTTPTransport()
	if tr == nil {
		t.Fatal("expected non-nil transport")
	}
	if tr.TLSHandshakeTimeout != HTTPTLSHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %v, want %v", tr.TLSHandshakeTimeout, HTTPTLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != HTTPResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, HTTPResponseHeaderTimeout)
	}
	if tr.IdleConnTimeout != HTTPIdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, HTTPIdleConnTimeout)
	}
	if tr.ExpectContinueTimeout != HTTPExpectContinueTimeout {
		t.Errorf("ExpectContinueTimeout = %v, want %v", tr.ExpectContinueTimeout, HTTPExpectContinueTimeout)
	}
}

func TestNewHTTPClient(t *testing.T) {
	tests := []struct {
		name        string
		timeout     time.Duration
		wantTimeout time.Duration
	}{
		{"zero uses default", 0, HTTPClientTimeout},
		{"custom timeout", 5 * time.Minute, 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewHTTPClient(tt.timeout)
			if c == nil {
				t.Fatal("expected non-nil client")
			}
			if c.Timeout != tt.wantTimeout {
				t.Errorf("Timeout = %v, want %v", c.Timeout, tt.wantTimeout)
			}
			if c.Transport == nil {
				t.Error("expected non-nil transport")
			}
		})
	}
}
