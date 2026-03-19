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

func TestBuildPodSpec_RuntimeClassName(t *testing.T) {
	tests := []struct {
		name             string
		runtimeClassName string
		wantSet          bool
	}{
		{
			name:             "not set when empty",
			runtimeClassName: "",
			wantSet:          false,
		},
		{
			name:             "set when configured",
			runtimeClassName: "nvidia",
			wantSet:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Deployer{
				config: Config{
					RuntimeClassName: tt.runtimeClassName,
					Image:            "test-image:latest",
				},
			}
			spec := d.buildPodSpec([]string{"snapshot"})

			if tt.wantSet {
				if spec.RuntimeClassName == nil {
					t.Fatal("RuntimeClassName is nil, want non-nil")
				}
				if *spec.RuntimeClassName != tt.runtimeClassName {
					t.Errorf("RuntimeClassName = %q, want %q", *spec.RuntimeClassName, tt.runtimeClassName)
				}
			} else if spec.RuntimeClassName != nil {
				t.Errorf("RuntimeClassName = %q, want nil", *spec.RuntimeClassName)
			}
		})
	}
}

func TestBuildEnvVars_RuntimeClassName(t *testing.T) {
	tests := []struct {
		name             string
		runtimeClassName string
		wantEnvVar       bool
	}{
		{
			name:             "NVIDIA_VISIBLE_DEVICES absent when no runtime class",
			runtimeClassName: "",
			wantEnvVar:       false,
		},
		{
			name:             "NVIDIA_VISIBLE_DEVICES=all when runtime class set",
			runtimeClassName: "nvidia",
			wantEnvVar:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Deployer{
				config: Config{
					RuntimeClassName: tt.runtimeClassName,
				},
			}
			envVars := d.buildEnvVars()

			var found bool
			for _, env := range envVars {
				if env.Name == "NVIDIA_VISIBLE_DEVICES" {
					found = true
					if env.Value != "all" {
						t.Errorf("NVIDIA_VISIBLE_DEVICES = %q, want %q", env.Value, "all")
					}
					break
				}
			}

			if found != tt.wantEnvVar {
				t.Errorf("NVIDIA_VISIBLE_DEVICES present = %v, want %v", found, tt.wantEnvVar)
			}
		})
	}
}

func TestBuildPodSpec_RequireGPU_And_RuntimeClassName_Independent(t *testing.T) {
	d := &Deployer{
		config: Config{
			Privileged:       true,
			RequireGPU:       true,
			RuntimeClassName: "",
			Image:            "test-image:latest",
		},
	}
	spec := d.buildPodSpec([]string{"snapshot"})

	if spec.RuntimeClassName != nil {
		t.Errorf("RuntimeClassName should be nil when only RequireGPU is set, got %q", *spec.RuntimeClassName)
	}

	container := spec.Containers[0]
	gpuFound := false
	for name := range container.Resources.Limits {
		if name == "nvidia.com/gpu" {
			gpuFound = true
		}
	}
	if !gpuFound {
		t.Error("nvidia.com/gpu resource limit not found when RequireGPU is true")
	}
}

func TestBuildPodSpec_RuntimeClassName_With_NodeSelector(t *testing.T) {
	nodeSelector := map[string]string{
		"nvidia.com/gpu.present": "true",
	}
	d := &Deployer{
		config: Config{
			RuntimeClassName: "nvidia",
			NodeSelector:     nodeSelector,
			Image:            "test-image:latest",
		},
	}
	spec := d.buildPodSpec([]string{"snapshot"})

	if spec.RuntimeClassName == nil {
		t.Fatal("RuntimeClassName is nil, want non-nil")
	}
	if *spec.RuntimeClassName != "nvidia" {
		t.Errorf("RuntimeClassName = %q, want %q", *spec.RuntimeClassName, "nvidia")
	}

	if len(spec.NodeSelector) != 1 {
		t.Fatalf("NodeSelector has %d entries, want 1", len(spec.NodeSelector))
	}
	if spec.NodeSelector["nvidia.com/gpu.present"] != "true" {
		t.Errorf("NodeSelector[nvidia.com/gpu.present] = %q, want %q",
			spec.NodeSelector["nvidia.com/gpu.present"], "true")
	}

	envVars := d.buildEnvVars()
	var nvidiaEnvFound bool
	for _, env := range envVars {
		if env.Name == "NVIDIA_VISIBLE_DEVICES" && env.Value == "all" {
			nvidiaEnvFound = true
			break
		}
	}
	if !nvidiaEnvFound {
		t.Error("NVIDIA_VISIBLE_DEVICES=all not found when RuntimeClassName is set with NodeSelector")
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
