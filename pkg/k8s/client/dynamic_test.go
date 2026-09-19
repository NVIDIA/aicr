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

package client

import (
	stderrors "errors"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"k8s.io/client-go/rest"
)

// TestBoundedConfig pins the request bound applied to clients built here: the
// discovery half of a fetcher reaches the apiserver through the context-free
// DiscoveryInterface, so without this it is governed only by client-go's own
// 32s discovery default — and the dynamic client gets no client-level bound at
// all.
func TestBoundedConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		in          *rest.Config
		wantTimeout time.Duration
	}{
		{
			name:        "unset timeout is bounded",
			in:          &rest.Config{Host: "https://127.0.0.1:6443"},
			wantTimeout: defaults.K8sClientRequestTimeout,
		},
		{
			name:        "caller timeout is preserved",
			in:          &rest.Config{Host: "https://127.0.0.1:6443", Timeout: 5 * time.Second},
			wantTimeout: 5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := tt.in.Timeout
			got := BoundedConfig(tt.in)
			if got.Timeout != tt.wantTimeout {
				t.Errorf("Timeout = %v, want %v", got.Timeout, tt.wantTimeout)
			}
			if tt.in.Timeout != before {
				t.Errorf("caller config mutated: Timeout = %v, want %v", tt.in.Timeout, before)
			}
		})
	}
}

func TestNewDynamicClientForConfig(t *testing.T) {
	t.Parallel()

	t.Run("nil config is rejected", func(t *testing.T) {
		t.Parallel()

		got, err := NewDynamicClientForConfig(nil)
		if err == nil {
			t.Fatal("expected an error for a nil rest config")
		}
		if got != nil {
			t.Errorf("client = %v, want nil on error", got)
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
		}
	})

	t.Run("valid config yields a client", func(t *testing.T) {
		t.Parallel()

		got, err := NewDynamicClientForConfig(&rest.Config{Host: "https://127.0.0.1:6443"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Error("client = nil, want a constructed client")
		}
	})
}
