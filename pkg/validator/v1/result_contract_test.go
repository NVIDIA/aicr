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

import (
	"slices"
	"testing"
)

func TestIsObservationOnlyCheck(t *testing.T) {
	tests := []struct {
		name  string
		check string
		want  bool
	}{
		{"cluster-autoscaling records observations", ClusterAutoscalingCheckName, true},
		{"a behavioral check is not observation-only", DriverRuntimeManagementCheckName, false},
		{"an unknown name is not observation-only", "no-such-check", false},
		{"the empty name is not observation-only", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsObservationOnlyCheck(tt.check); got != tt.want {
				t.Errorf("IsObservationOnlyCheck(%q) = %v, want %v", tt.check, got, tt.want)
			}
		})
	}
}

func TestObservationOnlyChecks(t *testing.T) {
	got := ObservationOnlyChecks()
	slices.Sort(got)
	want := []string{ClusterAutoscalingCheckName}
	if !slices.Equal(got, want) {
		t.Errorf("ObservationOnlyChecks() = %v, want %v", got, want)
	}
	for _, name := range got {
		if !IsObservationOnlyCheck(name) {
			t.Errorf("IsObservationOnlyCheck(%q) = false for a listed check", name)
		}
	}
}
