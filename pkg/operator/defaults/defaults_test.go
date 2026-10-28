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

package defaults

import "testing"

// client-go's leader elector rejects LeaseDuration <= RenewDeadline and
// RenewDeadline <= RetryPeriod * JitterFactor (1.2).
func TestLeaderElectionTimingsAreValid(t *testing.T) {
	t.Parallel()
	if LeaseDuration <= RenewDeadline {
		t.Fatalf("LeaseDuration %v must exceed RenewDeadline %v", LeaseDuration, RenewDeadline)
	}
	if float64(RenewDeadline) <= 1.2*float64(RetryPeriod) {
		t.Fatalf("RenewDeadline %v must exceed RetryPeriod %v x 1.2", RenewDeadline, RetryPeriod)
	}
}

func TestShutdownIsBounded(t *testing.T) {
	t.Parallel()
	if GracefulShutdownTimeout <= 0 {
		t.Fatal("GracefulShutdownTimeout must be positive; an unbounded shutdown can hang a rollout")
	}
}
