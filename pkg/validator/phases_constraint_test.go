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

package validator

import (
	"testing"
)

func TestParseConstraintResult(t *testing.T) {
	tests := []struct {
		name     string
		output   []string
		expected *ConstraintValidation
	}{
		{
			name: "valid constraint result",
			output: []string{
				"some log line",
				"CONSTRAINT_RESULT: name=Deployment.gpu-operator.version expected=>= v24.6.0 actual=v24.6.0 passed=true",
				"more log lines",
			},
			expected: &ConstraintValidation{
				Name:     "Deployment.gpu-operator.version",
				Expected: ">= v24.6.0",
				Actual:   "v24.6.0",
				Status:   ConstraintStatusPassed,
			},
		},
		{
			name: "failed constraint",
			output: []string{
				"CONSTRAINT_RESULT: name=K8s.version expected=>= 1.32 actual=1.30 passed=false",
			},
			expected: &ConstraintValidation{
				Name:     "K8s.version",
				Expected: ">= 1.32",
				Actual:   "1.30",
				Status:   ConstraintStatusFailed,
			},
		},
		{
			name: "no constraint result",
			output: []string{
				"just normal test output",
				"nothing special here",
			},
			expected: nil,
		},
		{
			name: "malformed constraint result",
			output: []string{
				"CONSTRAINT_RESULT: invalid format",
			},
			expected: nil,
		},
		{
			name: "partial constraint result (missing fields)",
			output: []string{
				"CONSTRAINT_RESULT: name=test expected=value",
			},
			expected: nil,
		},
		{
			name:     "empty output",
			output:   []string{},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseConstraintResult(tt.output)

			if tt.expected == nil {
				if result != nil {
					t.Errorf("Expected nil result, got %+v", result)
				}
				return
			}

			if result == nil {
				t.Fatalf("Expected result %+v, got nil", tt.expected)
			}

			if result.Name != tt.expected.Name {
				t.Errorf("Name: got %q, want %q", result.Name, tt.expected.Name)
			}
			if result.Expected != tt.expected.Expected {
				t.Errorf("Expected: got %q, want %q", result.Expected, tt.expected.Expected)
			}
			if result.Actual != tt.expected.Actual {
				t.Errorf("Actual: got %q, want %q", result.Actual, tt.expected.Actual)
			}
			if result.Status != tt.expected.Status {
				t.Errorf("Status: got %q, want %q", result.Status, tt.expected.Status)
			}
		})
	}
}
