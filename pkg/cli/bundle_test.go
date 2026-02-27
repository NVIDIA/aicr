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

package cli

import (
	"reflect"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
)

func TestParseSetFlags(t *testing.T) {
	tests := []struct {
		name     string
		setFlags []string
		want     map[string]map[string]string
		wantErr  bool
	}{
		{
			name:     "empty flags",
			setFlags: []string{},
			want:     map[string]map[string]string{},
			wantErr:  false,
		},
		{
			name:     "single flag",
			setFlags: []string{"gpuoperator:gds.enabled=true"},
			want: map[string]map[string]string{
				"gpuoperator": {
					"gds.enabled": "true",
				},
			},
			wantErr: false,
		},
		{
			name: "multiple flags same bundler",
			setFlags: []string{
				"gpuoperator:gds.enabled=true",
				"gpuoperator:driver.version=570.86.16",
			},
			want: map[string]map[string]string{
				"gpuoperator": {
					"gds.enabled":    "true",
					"driver.version": "570.86.16",
				},
			},
			wantErr: false,
		},
		{
			name: "multiple flags different bundlers",
			setFlags: []string{
				"gpuoperator:gds.enabled=true",
				"networkoperator:rdma.enabled=true",
			},
			want: map[string]map[string]string{
				"gpuoperator": {
					"gds.enabled": "true",
				},
				"networkoperator": {
					"rdma.enabled": "true",
				},
			},
			wantErr: false,
		},
		{
			name:     "value with equals sign",
			setFlags: []string{"gpuoperator:image.tag=v25.3.0=beta"},
			want: map[string]map[string]string{
				"gpuoperator": {
					"image.tag": "v25.3.0=beta",
				},
			},
			wantErr: false,
		},
		{
			name:     "value with spaces",
			setFlags: []string{"gpuoperator:custom.label=hello world"},
			want: map[string]map[string]string{
				"gpuoperator": {
					"custom.label": "hello world",
				},
			},
			wantErr: false,
		},
		{
			name:     "missing colon",
			setFlags: []string{"gpuoperatorgds.enabled=true"},
			wantErr:  true,
		},
		{
			name:     "missing equals sign",
			setFlags: []string{"gpuoperator:gds.enabledtrue"},
			wantErr:  true,
		},
		{
			name:     "empty path",
			setFlags: []string{"gpuoperator:=true"},
			wantErr:  true,
		},
		{
			name:     "empty value",
			setFlags: []string{"gpuoperator:gds.enabled="},
			wantErr:  true,
		},
		{
			name:     "only bundler name",
			setFlags: []string{"gpuoperator:"},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.ParseValueOverrides(tt.setFlags)

			if (err != nil) != tt.wantErr {
				t.Errorf("ParseValueOverrides() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseValueOverrides() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestParseOutputTarget is now in pkg/oci/reference_test.go
// The oci.ParseOutputTarget function handles OCI URI parsing.

func TestBundleCmd(t *testing.T) {
	cmd := bundleCmd()

	// Verify command configuration
	if cmd.Name != "bundle" {
		t.Errorf("expected command name 'bundle', got %q", cmd.Name)
	}

	// Verify required flags exist
	flagNames := make(map[string]bool)
	for _, flag := range cmd.Flags {
		names := flag.Names()
		for _, name := range names {
			flagNames[name] = true
		}
	}

	// Required flags for the new URI-based output approach
	requiredFlags := []string{"recipe", "r", "output", "o", "set", "plain-http", "insecure-tls"}
	for _, flag := range requiredFlags {
		if !flagNames[flag] {
			t.Errorf("expected flag %q to be defined", flag)
		}
	}

	// Verify node selector/toleration and scheduling flags exist
	nodeFlags := []string{
		"system-node-selector",
		"system-node-toleration",
		"accelerated-node-selector",
		"accelerated-node-toleration",
		"nodes",
	}
	for _, flag := range nodeFlags {
		if !flagNames[flag] {
			t.Errorf("expected flag %q to be defined", flag)
		}
	}

	// Verify removed flags don't exist (replaced by oci:// URI in --output)
	removedFlags := []string{"output-format", "registry", "repository", "tag", "push", "F"}
	for _, flag := range removedFlags {
		if flagNames[flag] {
			t.Errorf("flag %q should have been removed (use --output oci://... instead)", flag)
		}
	}
}
