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

package aicr

import (
	"context"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

// The verify cap was an unconditional ceiling: each method wrapped the caller's
// context, and context.WithTimeout takes the smaller of the two, so a caller
// deliberately allowing 20 minutes for a slow OCI pull was still cut off at
// five (#2225).
//
// These assert the deadline the caller actually gets, not that a field was
// copied. The zero value must keep today's cap, because that is what a caller
// who never considered this receives.

func TestApplyVerifyCap(t *testing.T) {
	twenty := 20 * time.Minute
	uncapped := time.Duration(0)
	negative := -1 * time.Second

	tests := []struct {
		name    string
		timeout *time.Duration
		// wantDeadline is the cap the returned context must carry, or 0 for
		// "no deadline of its own".
		wantDeadline time.Duration
	}{
		{
			name:         "nil keeps the default cap",
			timeout:      nil,
			wantDeadline: defaults.VerifyOperationTimeout,
		},
		{
			name:         "explicit longer cap is honored",
			timeout:      &twenty,
			wantDeadline: twenty,
		},
		{
			name:         "zero imposes no cap",
			timeout:      &uncapped,
			wantDeadline: 0,
		},
		{
			name:         "negative is treated as uncapped, not as an instant deadline",
			timeout:      &negative,
			wantDeadline: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := applyVerifyCap(context.Background(), tt.timeout)
			defer cancel()

			deadline, ok := ctx.Deadline()
			if tt.wantDeadline == 0 {
				if ok {
					t.Errorf("context carries a deadline in %v; the caller asked for "+
						"no facade cap", time.Until(deadline))
				}
				return
			}
			if !ok {
				t.Fatalf("context carries no deadline, want ~%v", tt.wantDeadline)
			}
			// Generous window: the assertion is which cap was chosen, not
			// scheduling precision.
			if remaining := time.Until(deadline); remaining > tt.wantDeadline ||
				remaining < tt.wantDeadline-time.Minute {

				t.Errorf("deadline in %v, want ~%v", remaining, tt.wantDeadline)
			}
		})
	}
}

// TestApplyVerifyCap_DoesNotExtendTheCallersDeadline pins the direction that
// must not change.
//
// The escape hatch lets a caller ask for MORE time than the facade default. It
// must never grant more than the caller's own context allows: a 1-minute caller
// deadline still wins over an uncapped facade.
func TestApplyVerifyCap_DoesNotExtendTheCallersDeadline(t *testing.T) {
	caller, cancelCaller := context.WithTimeout(context.Background(), time.Minute)
	defer cancelCaller()

	uncapped := time.Duration(0)
	ctx, cancel := applyVerifyCap(caller, &uncapped)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("caller's own deadline was discarded; an uncapped facade must " +
			"still respect the context it was given")
	}
	if remaining := time.Until(deadline); remaining > time.Minute {
		t.Errorf("deadline in %v, want no more than the caller's 1m", remaining)
	}
}
