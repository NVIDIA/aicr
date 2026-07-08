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

package config

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestParseArgoDeployerOptions(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		want      *ArgoDeployerOptions
		wantErr   bool
		errSubstr string
	}{
		{"nil map returns nil", nil, nil, false, ""},
		{"empty map returns nil", map[string]string{}, nil, false, ""},
		{"valid namePrefix", map[string]string{"namePrefix": "tenant-a-"},
			&ArgoDeployerOptions{NamePrefix: "tenant-a-"}, false, ""},
		{"valid destinationServer", map[string]string{"destinationServer": "https://10.0.0.1:6443"},
			&ArgoDeployerOptions{DestinationServer: "https://10.0.0.1:6443"}, false, ""},
		{"valid project", map[string]string{"project": "tenant-a"},
			&ArgoDeployerOptions{Project: "tenant-a"}, false, ""},
		{"valid cascadeDelete", map[string]string{"cascadeDelete": "true"},
			&ArgoDeployerOptions{CascadeDelete: true}, false, ""},
		{"all keys together", map[string]string{
			"namePrefix": "t1-", "destinationServer": "https://remote.example.com:6443",
			"project": "t1", "cascadeDelete": "true"},
			&ArgoDeployerOptions{NamePrefix: "t1-", DestinationServer: "https://remote.example.com:6443",
				Project: "t1", CascadeDelete: true}, false, ""},
		{"unknown key rejected", map[string]string{"parentProject": "x"}, nil, true, "unknown deployer option"},
		{"uppercase namePrefix rejected", map[string]string{"namePrefix": "Tenant-"}, nil, true, "namePrefix"},
		{"namePrefix leading hyphen rejected", map[string]string{"namePrefix": "-a"}, nil, true, "namePrefix"},
		{"http destinationServer rejected", map[string]string{"destinationServer": "http://insecure:6443"}, nil, true, "destinationServer"},
		{"destinationServer with credentials rejected", map[string]string{"destinationServer": "https://u:p@host:6443"}, nil, true, "destinationServer"},
		{"destinationServer port without hostname rejected", map[string]string{"destinationServer": "https://:6443"}, nil, true, "destinationServer"},
		{"destinationServer uppercase scheme rejected", map[string]string{"destinationServer": "HTTPS://host:6443"}, nil, true, "destinationServer"},
		{"destinationServer at-sign in path rejected", map[string]string{"destinationServer": "https://host/path@thing"}, nil, true, "destinationServer"},
		{"destinationServer with apostrophe rejected", map[string]string{"destinationServer": "https://host/o'brien:6443"}, nil, true, "must not contain quotes"},
		{"destinationServer with double quote rejected", map[string]string{"destinationServer": `https://host/a"b:6443`}, nil, true, "must not contain quotes"},
		// Sorted key iteration makes multi-error reporting deterministic:
		// with two invalid keys, the alphabetically-first one is reported.
		{"multiple unknown keys report the first sorted key", map[string]string{"zzz": "1", "aaa": "1"}, nil, true, `"aaa"`},
		{"invalid project rejected", map[string]string{"project": "Tenant_A"}, nil, true, "project"},
		{"empty project rejected", map[string]string{"project": ""}, nil, true, "must not be empty"},
		// ValidateProject mirrors IsDNS1123Subdomain exactly: a 64-char
		// label is a legal Kubernetes object name (only the 253-char total
		// is capped), so an AppProject with such a name can exist and the
		// reference must be accepted — matching the install-time schema.
		{"project 64-char label accepted", map[string]string{"project": strings.Repeat("a", 64)},
			&ArgoDeployerOptions{Project: strings.Repeat("a", 64)}, false, ""},
		{"project dotted labels accepted", map[string]string{"project": strings.Repeat("a", 63) + "." + strings.Repeat("b", 63)},
			&ArgoDeployerOptions{Project: strings.Repeat("a", 63) + "." + strings.Repeat("b", 63)}, false, ""},
		{"non-bool cascadeDelete rejected", map[string]string{"cascadeDelete": "yes-please"}, nil, true, "cascadeDelete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseArgoDeployerOptions(tt.overrides)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q does not mention %q", err.Error(), tt.errSubstr)
				}
				return
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestReservedDeployerKeyConstantsMatch pins the layering contract:
// pkg/recipe cannot import pkg/bundler/config, so the registry-load
// guard uses its own recipe.ReservedDeployerKey constant. This test is
// the single place that asserts the two constants stay equal.
func TestReservedDeployerKeyConstantsMatch(t *testing.T) {
	if recipe.ReservedDeployerKey != DeployerOverrideKey {
		t.Errorf("recipe.ReservedDeployerKey = %q, config.DeployerOverrideKey = %q; the reserved --set deployer: prefix must be identical in both packages",
			recipe.ReservedDeployerKey, DeployerOverrideKey)
	}
}
