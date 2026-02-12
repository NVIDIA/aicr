// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package recipe

import (
	"strings"
	"testing"
)

func TestAllowLists_ValidateCriteria(t *testing.T) {
	tests := []struct {
		name       string
		allowLists *AllowLists
		criteria   *Criteria
		wantErr    bool
		errContain string
	}{
		{
			name:       "nil allowlists allows everything",
			allowLists: nil,
			criteria: &Criteria{
				Accelerator: CriteriaAcceleratorGB200,
				Service:     CriteriaServiceEKS,
			},
			wantErr: false,
		},
		{
			name:       "empty allowlists allows everything",
			allowLists: &AllowLists{},
			criteria: &Criteria{
				Accelerator: CriteriaAcceleratorGB200,
				Service:     CriteriaServiceEKS,
			},
			wantErr: false,
		},
		{
			name: "allowed accelerator passes",
			allowLists: &AllowLists{
				Accelerators: []CriteriaAcceleratorType{CriteriaAcceleratorH100, CriteriaAcceleratorL40},
			},
			criteria: &Criteria{
				Accelerator: CriteriaAcceleratorH100,
			},
			wantErr: false,
		},
		{
			name: "disallowed accelerator fails",
			allowLists: &AllowLists{
				Accelerators: []CriteriaAcceleratorType{CriteriaAcceleratorH100, CriteriaAcceleratorL40},
			},
			criteria: &Criteria{
				Accelerator: CriteriaAcceleratorGB200,
			},
			wantErr:    true,
			errContain: "accelerator type not allowed",
		},
		{
			name: "any accelerator always allowed",
			allowLists: &AllowLists{
				Accelerators: []CriteriaAcceleratorType{CriteriaAcceleratorH100},
			},
			criteria: &Criteria{
				Accelerator: CriteriaAcceleratorAny,
			},
			wantErr: false,
		},
		{
			name: "allowed service passes",
			allowLists: &AllowLists{
				Services: []CriteriaServiceType{CriteriaServiceEKS, CriteriaServiceGKE},
			},
			criteria: &Criteria{
				Service: CriteriaServiceEKS,
			},
			wantErr: false,
		},
		{
			name: "disallowed service fails",
			allowLists: &AllowLists{
				Services: []CriteriaServiceType{CriteriaServiceEKS, CriteriaServiceGKE},
			},
			criteria: &Criteria{
				Service: CriteriaServiceAKS,
			},
			wantErr:    true,
			errContain: "service type not allowed",
		},
		{
			name: "allowed intent passes",
			allowLists: &AllowLists{
				Intents: []CriteriaIntentType{CriteriaIntentTraining},
			},
			criteria: &Criteria{
				Intent: CriteriaIntentTraining,
			},
			wantErr: false,
		},
		{
			name: "disallowed intent fails",
			allowLists: &AllowLists{
				Intents: []CriteriaIntentType{CriteriaIntentTraining},
			},
			criteria: &Criteria{
				Intent: CriteriaIntentInference,
			},
			wantErr:    true,
			errContain: "intent type not allowed",
		},
		{
			name: "allowed OS passes",
			allowLists: &AllowLists{
				OSTypes: []CriteriaOSType{CriteriaOSUbuntu, CriteriaOSRHEL},
			},
			criteria: &Criteria{
				OS: CriteriaOSUbuntu,
			},
			wantErr: false,
		},
		{
			name: "disallowed OS fails",
			allowLists: &AllowLists{
				OSTypes: []CriteriaOSType{CriteriaOSUbuntu},
			},
			criteria: &Criteria{
				OS: CriteriaOSRHEL,
			},
			wantErr:    true,
			errContain: "OS type not allowed",
		},
		{
			name: "multiple criteria all valid",
			allowLists: &AllowLists{
				Accelerators: []CriteriaAcceleratorType{CriteriaAcceleratorH100},
				Services:     []CriteriaServiceType{CriteriaServiceEKS},
				Intents:      []CriteriaIntentType{CriteriaIntentTraining},
				OSTypes:      []CriteriaOSType{CriteriaOSUbuntu},
			},
			criteria: &Criteria{
				Accelerator: CriteriaAcceleratorH100,
				Service:     CriteriaServiceEKS,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
			},
			wantErr: false,
		},
		{
			name: "multiple criteria one invalid",
			allowLists: &AllowLists{
				Accelerators: []CriteriaAcceleratorType{CriteriaAcceleratorH100},
				Services:     []CriteriaServiceType{CriteriaServiceEKS},
			},
			criteria: &Criteria{
				Accelerator: CriteriaAcceleratorH100,
				Service:     CriteriaServiceGKE, // not allowed
			},
			wantErr:    true,
			errContain: "service type not allowed",
		},
		{
			name: "nil criteria passes",
			allowLists: &AllowLists{
				Accelerators: []CriteriaAcceleratorType{CriteriaAcceleratorH100},
			},
			criteria: nil,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.allowLists.ValidateCriteria(tt.criteria)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCriteria() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContain != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("ValidateCriteria() error = %v, should contain %q", err, tt.errContain)
				}
			}
		})
	}
}

func TestAllowLists_IsEmpty(t *testing.T) {
	tests := []struct {
		name       string
		allowLists *AllowLists
		want       bool
	}{
		{
			name:       "nil is empty",
			allowLists: nil,
			want:       true,
		},
		{
			name:       "empty struct is empty",
			allowLists: &AllowLists{},
			want:       true,
		},
		{
			name: "with accelerators not empty",
			allowLists: &AllowLists{
				Accelerators: []CriteriaAcceleratorType{CriteriaAcceleratorH100},
			},
			want: false,
		},
		{
			name: "with services not empty",
			allowLists: &AllowLists{
				Services: []CriteriaServiceType{CriteriaServiceEKS},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.allowLists.IsEmpty(); got != tt.want {
				t.Errorf("IsEmpty() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllowLists_StringMethods(t *testing.T) {
	al := &AllowLists{
		Accelerators: []CriteriaAcceleratorType{CriteriaAcceleratorH100, CriteriaAcceleratorL40},
		Services:     []CriteriaServiceType{CriteriaServiceEKS, CriteriaServiceGKE},
		Intents:      []CriteriaIntentType{CriteriaIntentTraining},
		OSTypes:      []CriteriaOSType{CriteriaOSUbuntu},
	}

	t.Run("AcceleratorStrings", func(t *testing.T) {
		got := al.AcceleratorStrings()
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
	})

	t.Run("AcceleratorStrings nil", func(t *testing.T) {
		var nilAL *AllowLists
		if got := nilAL.AcceleratorStrings(); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("ServiceStrings", func(t *testing.T) {
		got := al.ServiceStrings()
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
	})

	t.Run("ServiceStrings nil", func(t *testing.T) {
		var nilAL *AllowLists
		if got := nilAL.ServiceStrings(); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("IntentStrings", func(t *testing.T) {
		got := al.IntentStrings()
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
	})

	t.Run("IntentStrings nil", func(t *testing.T) {
		var nilAL *AllowLists
		if got := nilAL.IntentStrings(); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("OSTypeStrings", func(t *testing.T) {
		got := al.OSTypeStrings()
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
	})

	t.Run("OSTypeStrings nil", func(t *testing.T) {
		var nilAL *AllowLists
		if got := nilAL.OSTypeStrings(); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
}

func TestParseAllowListsFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		envVars map[string]string
		wantNil bool
		wantErr bool
		check   func(*AllowLists) bool
	}{
		{
			name:    "no env vars returns nil",
			envVars: map[string]string{},
			wantNil: true,
			wantErr: false,
		},
		{
			name: "valid accelerators",
			envVars: map[string]string{
				EnvAllowedAccelerators: "h100,l40",
			},
			wantNil: false,
			wantErr: false,
			check: func(al *AllowLists) bool {
				return len(al.Accelerators) == 2 &&
					al.Accelerators[0] == CriteriaAcceleratorH100 &&
					al.Accelerators[1] == CriteriaAcceleratorL40
			},
		},
		{
			name: "valid services with spaces",
			envVars: map[string]string{
				EnvAllowedServices: " eks , gke ",
			},
			wantNil: false,
			wantErr: false,
			check: func(al *AllowLists) bool {
				return len(al.Services) == 2 &&
					al.Services[0] == CriteriaServiceEKS &&
					al.Services[1] == CriteriaServiceGKE
			},
		},
		{
			name: "invalid accelerator fails",
			envVars: map[string]string{
				EnvAllowedAccelerators: "h100,invalid",
			},
			wantNil: false,
			wantErr: true,
		},
		{
			name: "invalid service fails",
			envVars: map[string]string{
				EnvAllowedServices: "invalid",
			},
			wantNil: false,
			wantErr: true,
		},
		{
			name: "any value is skipped",
			envVars: map[string]string{
				EnvAllowedAccelerators: "h100,any",
			},
			wantNil: false,
			wantErr: false,
			check: func(al *AllowLists) bool {
				return len(al.Accelerators) == 1 && al.Accelerators[0] == CriteriaAcceleratorH100
			},
		},
		{
			name: "all criteria types",
			envVars: map[string]string{
				EnvAllowedAccelerators: "h100",
				EnvAllowedServices:     "eks",
				EnvAllowedIntents:      "training",
				EnvAllowedOSTypes:      "ubuntu",
			},
			wantNil: false,
			wantErr: false,
			check: func(al *AllowLists) bool {
				return len(al.Accelerators) == 1 &&
					len(al.Services) == 1 &&
					len(al.Intents) == 1 &&
					len(al.OSTypes) == 1
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear and set env vars
			for _, key := range []string{EnvAllowedAccelerators, EnvAllowedServices, EnvAllowedIntents, EnvAllowedOSTypes} {
				t.Setenv(key, "")
			}
			for key, val := range tt.envVars {
				t.Setenv(key, val)
			}

			got, err := ParseAllowListsFromEnv()
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseAllowListsFromEnv() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantNil && got != nil {
				t.Errorf("ParseAllowListsFromEnv() = %v, want nil", got)
				return
			}
			if !tt.wantNil && got == nil && !tt.wantErr {
				t.Error("ParseAllowListsFromEnv() = nil, want non-nil")
				return
			}
			if tt.check != nil && got != nil && !tt.check(got) {
				t.Errorf("ParseAllowListsFromEnv() check failed, got %+v", got)
			}
		})
	}
}
