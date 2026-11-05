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

package validation

import "testing"

func TestIsChainsawTest(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "valid chainsaw test",
			raw: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-health`,
			want: true,
		},
		{
			name: "raw k8s resource",
			raw: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator`,
			want: false,
		},
		{
			name: "has apiVersion but wrong kind",
			raw: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Configuration`,
			want: false,
		},
		{
			name: "has kind Test but wrong apiVersion",
			raw: `apiVersion: v1
kind: Test`,
			want: false,
		},
		{
			name: "empty string",
			raw:  "",
			want: false,
		},
		{
			name: "quoted kind is still Test format",
			raw: `apiVersion: "chainsaw.kyverno.io/v1alpha1"
kind: "Test"
metadata:
  name: gpu-operator-health`,
			want: true,
		},
		{
			name: "single-quoted kind is still Test format",
			raw: `apiVersion: 'chainsaw.kyverno.io/v1alpha1'
kind: 'Test'`,
			want: true,
		},
		{
			name: "future chainsaw API version still dispatches to the guarded path",
			raw: `apiVersion: chainsaw.kyverno.io/v1alpha2
kind: Test`,
			want: true,
		},
		{
			name: "raw k8s YAML mentioning the strings in a comment is not a Test",
			raw: `# migrated from chainsaw.kyverno.io, formerly kind: Test
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator`,
			want: false,
		},
		{
			name: "raw k8s YAML carrying the strings in data is not a Test",
			raw: `apiVersion: v1
kind: ConfigMap
metadata:
  name: checks
data:
  note: |
    apiVersion: chainsaw.kyverno.io/v1alpha1
    kind: Test`,
			want: false,
		},
		{
			name: "multi-document stream with a Test in the second document",
			raw: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
---
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-health`,
			want: true,
		},
		{
			name: "bare group without a version is not a Test",
			raw: `apiVersion: chainsaw.kyverno.io
kind: Test`,
			want: false,
		},
		{
			name: "unparseable YAML is not a Test",
			raw:  "\tapiVersion: chainsaw.kyverno.io/v1alpha1\n  kind: Test\n:::",
			want: false,
		},
		{
			name: "scalar document is not a Test",
			raw:  `"chainsaw.kyverno.io kind: Test"`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsChainsawTest(tt.raw)
			if got != tt.want {
				t.Errorf("IsChainsawTest() = %v, want %v", got, tt.want)
			}
		})
	}
}
