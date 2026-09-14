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

package cli

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestRootCommand(t *testing.T) {
	cmd := RootCommand()
	if cmd == nil {
		t.Fatal("RootCommand() returned nil")
	}
	if len(cmd.Commands) == 0 {
		t.Fatal("RootCommand() has no subcommands")
	}
	// It must return the same assembled tree as the internal builder.
	if got, want := len(cmd.Commands), len(newRootCmd().Commands); got != want {
		t.Errorf("RootCommand() subcommand count = %d, want %d", got, want)
	}
}

func TestDefaultAgentImage(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		expected string
	}{
		{"dev build", "dev", defaults.AgentImageRepository + ":latest"},
		{"snapshot with v prefix", "v0.8.10-next", defaults.AgentImageRepository + ":latest"},
		{"snapshot without v prefix", "0.8.10-next", defaults.AgentImageRepository + ":latest"},
		{"release without v prefix", "0.8.10", defaults.AgentImageRepository + ":v0.8.10"},
		{"release with v prefix", "v0.8.10", defaults.AgentImageRepository + ":v0.8.10"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := version
			version = tt.version
			t.Cleanup(func() { version = original })

			got := defaultAgentImage()
			if got != tt.expected {
				t.Errorf("defaultAgentImage() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestEmbeddedClientPreservesClientErrorCode(t *testing.T) {
	tests := []struct {
		name     string
		ctx      func() (context.Context, context.CancelFunc)
		wantCode errors.ErrorCode
	}{
		{
			name: "canceled context",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx, cancel
			},
			wantCode: errors.ErrCodeCanceled,
		},
		{
			name: "expired deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			},
			wantCode: errors.ErrCodeTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := tt.ctx()
			defer cancel()

			client, err := embeddedClient(ctx)
			if client != nil {
				_ = client.Close()
				t.Errorf("embeddedClient() client = %v, want nil", client)
			}
			if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("embeddedClient() error = %v, want code %s", err, tt.wantCode)
			}
		})
	}
}
