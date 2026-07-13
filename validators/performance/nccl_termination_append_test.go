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

package main

import "testing"

// resultsRows is a two-row NCCL results block (largest size last) as it would be
// written to /dev/termination-log by the launcher. launcherTerminationTail (the
// read half of the success-path append) is covered by TestLauncherTerminationTail
// in nccl_all_reduce_bw_constraint_test.go; these cover the append half.
const resultsRows = " 8589934592    2147483648     float     sum      -1   48298   177.85  333.47      0   48292   177.87  333.51      0\n" +
	"17179869184    4294967296     float     sum      -1    95340  180.20  337.87      0    95495  179.90  337.32      0"

func TestAppendTerminationResults(t *testing.T) {
	tests := []struct {
		name string
		logs string
		term string
		want string
	}{
		{"empty term leaves logs unchanged", "streamed log", "", "streamed log"},
		{"non-empty term appended after newline", "streamed log", "term", "streamed log\nterm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appendTerminationResults(tt.logs, tt.term); got != tt.want {
				t.Errorf("appendTerminationResults(%q, %q) = %q, want %q", tt.logs, tt.term, got, tt.want)
			}
		})
	}
}

// TestAppendTerminationResultsRecoversRotatedBandwidth ties the append helper to
// the parser: a teardown-only (rotated) streamed log plus the termination
// message must parse to the largest-row bandwidth — the recovery the success
// path relies on.
func TestAppendTerminationResultsRecoversRotatedBandwidth(t *testing.T) {
	rotated := "node-0-0:870:938 [0] NCCL INFO NET/FasTrak: SCTP Closed\n" +
		"node-0-0:870:938 [0] NCCL INFO NET/FasTrak: Socket closed, handler exiting."

	// Sanity: the rotated log alone has no parseable results.
	if _, err := parseBandwidthFromLogs(rotated); err == nil {
		t.Fatal("expected rotated (teardown-only) log to fail bandwidth parsing")
	}

	bw, err := parseBandwidthFromLogs(appendTerminationResults(rotated, resultsRows))
	if err != nil {
		t.Fatalf("expected bandwidth recovered from appended termination message, got %v", err)
	}
	if bw != 337.87 {
		t.Errorf("expected largest-row busbw 337.87, got %v", bw)
	}
}
