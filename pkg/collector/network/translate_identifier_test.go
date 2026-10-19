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

package network

import (
	"testing"

	l8kconfig "github.com/nvidia/k8s-launch-kit/pkg/config"
)

// TestBuildIdentitySubtype_IdentifierFallback pins how the identity context
// behaves on input from l8k's read-only discovery, which never assigns a
// group identifier: the collector derives the same value l8k's mutating path
// would have, and omits the key only when it cannot.
func TestBuildIdentitySubtype_IdentifierFallback(t *testing.T) {
	tests := []struct {
		name  string
		group l8kconfig.ClusterConfig
		want  string // "" means the identifier key must be absent
	}{
		{
			name:  "explicit identifier wins",
			group: l8kconfig.ClusterConfig{Identifier: "explicit", MachineType: "ARS-121GL-NB3-LCC-R1", GPUType: "NVIDIA-GB300"},
			want:  "explicit",
		},
		{
			name:  "derived from machine and gpu type when empty",
			group: l8kconfig.ClusterConfig{MachineType: "ARS-121GL-NB3-LCC-R1", GPUType: "NVIDIA-GB300"},
			want:  l8kconfig.GeneratedGroupIdentifier("ARS-121GL-NB3-LCC-R1", "NVIDIA-GB300"),
		},
		{
			name:  "absent when machine type is unknown",
			group: l8kconfig.ClusterConfig{GPUType: "NVIDIA-GB300"},
		},
		{
			name:  "absent when gpu type is unknown",
			group: l8kconfig.ClusterConfig{MachineType: "ARS-121GL-NB3-LCC-R1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.want == "" && (tt.group.MachineType != "" && tt.group.GPUType != "") {
				t.Fatal("test bug: derived case must expect a non-empty identifier")
			}
			st := buildIdentitySubtype(tt.group)
			got, present := st.Context[identityCtxIdentifier]
			if tt.want == "" {
				if present {
					t.Errorf("identifier = %q, want key absent", got)
				}
				return
			}
			if got == "" || got != tt.want {
				t.Errorf("identifier = %q, want %q", got, tt.want)
			}
		})
	}
}
