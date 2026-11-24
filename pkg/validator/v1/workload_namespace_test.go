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

package v1

import "testing"

const testWorkloadRunID = "20261106-173024-0123456789abcdef"

// The literal pins the derivation itself: namespaces created by an earlier
// release must still be recognized after a refactor.
func TestWorkloadNamespaceSuffix(t *testing.T) {
	if got := WorkloadNamespaceSuffix(testWorkloadRunID); got != "d38f6cde" {
		t.Errorf("WorkloadNamespaceSuffix = %q, want d38f6cde", got)
	}
}

func TestIsRunWorkloadNamespace(t *testing.T) {
	tests := []struct {
		name  string
		ns    string
		runID string
		want  bool
	}{
		{"inference-perf", "aicr-inference-perf-d38f6cde", testWorkloadRunID, true},
		{"nccl default", "aicr-nccl-perf-default-d38f6cde", testWorkloadRunID, true},
		{"nccl net", "aicr-nccl-perf-net-d38f6cde", testWorkloadRunID, true},
		{"nccl nvls", "aicr-nccl-perf-nvls-d38f6cde", testWorkloadRunID, true},
		{"another run's suffix", "aicr-inference-perf-0badc0de", testWorkloadRunID, false},
		{"unknown nccl variant", "aicr-nccl-perf-other-d38f6cde", testWorkloadRunID, false},
		{"unrelated name", "workloads", testWorkloadRunID, false},
		{"suffix without prefix", "d38f6cde", testWorkloadRunID, false},
		{"empty run ID", InferencePerfNamespace(WorkloadNamespaceSuffix("")), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRunWorkloadNamespace(tt.ns, tt.runID); got != tt.want {
				t.Errorf("IsRunWorkloadNamespace(%q) = %v, want %v", tt.ns, got, tt.want)
			}
		})
	}
}
