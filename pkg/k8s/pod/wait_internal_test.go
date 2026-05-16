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

package pod

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestClassifyReGetError pins the wait-loop re-Get classifier so a deadline
// race between watch-channel close and the re-Get surfaces as ErrCodeTimeout,
// not ErrCodeUnavailable. Without this, an upstream caller distinguishing
// transient apiserver unavailability from its own deadline would misroute
// the failure.
func TestClassifyReGetError(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		getErr  error
		wantTo  errors.ErrorCode
		wantMsg string
	}{
		{
			name:    "context already canceled",
			ctx:     canceledCtx,
			getErr:  stderrors.New("get failed"),
			wantTo:  errors.ErrCodeTimeout,
			wantMsg: "ctx canceled",
		},
		{
			name:    "getErr is DeadlineExceeded",
			ctx:     context.Background(),
			getErr:  context.DeadlineExceeded,
			wantTo:  errors.ErrCodeTimeout,
			wantMsg: "deadline exceeded",
		},
		{
			name:    "getErr is Canceled",
			ctx:     context.Background(),
			getErr:  context.Canceled,
			wantTo:  errors.ErrCodeTimeout,
			wantMsg: "canceled",
		},
		{
			name:    "transient apiserver error",
			ctx:     context.Background(),
			getErr:  stderrors.New("connection refused"),
			wantTo:  errors.ErrCodeUnavailable,
			wantMsg: "unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyReGetError(tt.ctx, "wait test", tt.getErr)
			if err == nil {
				t.Fatalf("expected non-nil error")
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("error is not StructuredError: %v", err)
			}
			if se.Code != tt.wantTo {
				t.Errorf("code = %q, want %q (%s)", se.Code, tt.wantTo, tt.wantMsg)
			}
		})
	}
}
