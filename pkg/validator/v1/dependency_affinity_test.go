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
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestDependencyAffinityValidate(t *testing.T) {
	tests := []struct {
		name    string
		dep     DependencyAffinity
		wantErr bool
	}{
		{
			name: "valid required with selector",
			dep: DependencyAffinity{
				ComponentRef:     "kube-prometheus-stack",
				PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
				Requirement:      DependencyRequirementRequired,
			},
			wantErr: false,
		},
		{
			name: "valid preferred with default topology",
			dep: DependencyAffinity{
				ComponentRef:     "kube-prometheus-stack",
				PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
				Requirement:      DependencyRequirementPreferred,
			},
			wantErr: false,
		},
		{
			name: "valid empty requirement defaults to preferred",
			dep: DependencyAffinity{
				ComponentRef:     "kube-prometheus-stack",
				PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
				// Requirement intentionally empty
			},
			wantErr: false,
		},
		{
			name: "empty componentRef rejected",
			dep: DependencyAffinity{
				PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
				Requirement:      DependencyRequirementPreferred,
			},
			wantErr: true,
		},
		{
			name: "empty selector rejected",
			dep: DependencyAffinity{
				ComponentRef: "kube-prometheus-stack",
				Requirement:  DependencyRequirementPreferred,
			},
			wantErr: true,
		},
		{
			name: "unknown requirement rejected",
			dep: DependencyAffinity{
				ComponentRef:     "kube-prometheus-stack",
				PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
				Requirement:      "soft",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.dep.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
				}
			}
		})
	}
}

func TestDependencyAffinityRequirementOrDefault(t *testing.T) {
	tests := []struct {
		name string
		dep  DependencyAffinity
		want DependencyRequirement
	}{
		{"empty defaults to preferred", DependencyAffinity{}, DependencyRequirementPreferred},
		{"explicit required preserved", DependencyAffinity{Requirement: DependencyRequirementRequired}, DependencyRequirementRequired},
		{"explicit preferred preserved", DependencyAffinity{Requirement: DependencyRequirementPreferred}, DependencyRequirementPreferred},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.dep.RequirementOrDefault(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDependencyAffinityTopologyKeyOrDefault(t *testing.T) {
	tests := []struct {
		name        string
		topologyKey string
		want        string
	}{
		{"empty defaults to hostname", "", "kubernetes.io/hostname"},
		{"explicit zone preserved", "topology.kubernetes.io/zone", "topology.kubernetes.io/zone"},
		{"explicit hostname preserved", "kubernetes.io/hostname", "kubernetes.io/hostname"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DependencyAffinity{TopologyKey: tt.topologyKey}
			if got := d.TopologyKeyOrDefault(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
