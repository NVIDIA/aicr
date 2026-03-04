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

package agent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestToLocalObjectReferences(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []corev1.LocalObjectReference
	}{
		{
			name: "nil input",
			in:   nil,
			want: nil,
		},
		{
			name: "empty slice",
			in:   []string{},
			want: nil,
		},
		{
			name: "single item",
			in:   []string{"my-secret"},
			want: []corev1.LocalObjectReference{
				{Name: "my-secret"},
			},
		},
		{
			name: "multiple items",
			in:   []string{"secret-a", "secret-b", "secret-c"},
			want: []corev1.LocalObjectReference{
				{Name: "secret-a"},
				{Name: "secret-b"},
				{Name: "secret-c"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toLocalObjectReferences(tt.in)

			if tt.want == nil {
				if got != nil {
					t.Errorf("toLocalObjectReferences(%v) = %v, want nil", tt.in, got)
				}
				return
			}

			if len(got) != len(tt.want) {
				t.Fatalf("toLocalObjectReferences(%v) len = %d, want %d", tt.in, len(got), len(tt.want))
			}

			for i := range tt.want {
				if got[i].Name != tt.want[i].Name {
					t.Errorf("toLocalObjectReferences(%v)[%d].Name = %q, want %q",
						tt.in, i, got[i].Name, tt.want[i].Name)
				}
			}
		})
	}
}

func TestMustParseQuantity(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"cpu cores", "2"},
		{"memory", "8Gi"},
		{"millicores", "100m"},
		{"storage", "4Gi"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := mustParseQuantity(tt.input)
			if q.String() != tt.input {
				t.Errorf("mustParseQuantity(%q) = %q, want %q", tt.input, q.String(), tt.input)
			}
		})
	}
}
