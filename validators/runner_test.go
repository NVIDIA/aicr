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

package validators

import (
	"errors"
	"strings"
	"testing"
)

func TestSkip(t *testing.T) {
	tests := []struct {
		name          string
		reason        string
		wantIsErrSkip bool
		wantSubstring string
	}{
		{
			name:          "wraps errSkip sentinel",
			reason:        "GPU not present",
			wantIsErrSkip: true,
			wantSubstring: "GPU not present",
		},
		{
			name:          "empty reason still wraps errSkip",
			reason:        "",
			wantIsErrSkip: true,
			wantSubstring: "skip",
		},
		{
			name:          "reason with special characters",
			reason:        "node taint: gpu=true:NoSchedule",
			wantIsErrSkip: true,
			wantSubstring: "gpu=true:NoSchedule",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Skip(tt.reason)
			if err == nil {
				t.Fatal("expected non-nil error from Skip()")
			}
			if got := errors.Is(err, errSkip); got != tt.wantIsErrSkip {
				t.Errorf("errors.Is(Skip(%q), errSkip) = %v, want %v", tt.reason, got, tt.wantIsErrSkip)
			}
			if msg := err.Error(); !strings.Contains(msg, tt.wantSubstring) {
				t.Errorf("Skip(%q).Error() = %q, want substring %q", tt.reason, msg, tt.wantSubstring)
			}
		})
	}
}

func TestSkipIsNotGenericError(t *testing.T) {
	// A plain error must NOT match errSkip.
	plain := errors.New("some other error")
	if errors.Is(plain, errSkip) {
		t.Error("plain error should not match errSkip")
	}
}
