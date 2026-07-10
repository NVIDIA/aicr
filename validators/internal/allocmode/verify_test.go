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

package allocmode

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// TestVerify pins the fail-closed policy × cluster-state comparison: a
// configured policy requires its mechanism usable, unspecified verifies
// nothing, and unknown or reserved policies never run unverified.
func TestVerify(t *testing.T) {
	bothUsable := &Mode{DRAUsable: true, DevicePluginUsable: true}
	draOnly := &Mode{DRAUsable: true, DRADetail: "full-GPU DRA usable"}
	pluginOnly := &Mode{DevicePluginUsable: true, DevicePluginDetail: "device plugin usable"}
	neither := &Mode{DRADetail: "full-GPU DRA not usable", DevicePluginDetail: "device plugin not usable"}

	tests := []struct {
		name    string
		policy  string
		mode    *Mode
		wantErr bool
		// wantInMsg asserts the mismatch message is actionable: it must name
		// the configured policy and carry the observed state.
		wantInMsg []string
	}{
		{"unspecified verifies nothing (neither usable)", v1.GPUAllocationPolicyUnspecified, neither, false, nil},
		{"empty policy verifies nothing", "", neither, false, nil},
		{"dra policy with usable DRA", v1.GPUAllocationPolicyDRAResourceClaim, draOnly, false, nil},
		{"dra policy on dual cluster", v1.GPUAllocationPolicyDRAResourceClaim, bothUsable, false, nil},
		{
			"dra policy without usable DRA fails closed",
			v1.GPUAllocationPolicyDRAResourceClaim, pluginOnly, true,
			[]string{"dra-resource-claim", "device plugin usable", "drifted"},
		},
		{"plugin policy with usable plugin", v1.GPUAllocationPolicyDevicePluginExtendedResource, pluginOnly, false, nil},
		{"plugin policy on dual cluster", v1.GPUAllocationPolicyDevicePluginExtendedResource, bothUsable, false, nil},
		{
			"plugin policy without usable plugin fails closed",
			v1.GPUAllocationPolicyDevicePluginExtendedResource, draOnly, true,
			[]string{"device-plugin-extended-resource", "full-GPU DRA usable", "drifted"},
		},
		{
			"dra policy with nil mode fails closed",
			v1.GPUAllocationPolicyDRAResourceClaim, nil, true,
			[]string{"no inspection facts"},
		},
		{
			"plugin policy with nil mode fails closed",
			v1.GPUAllocationPolicyDevicePluginExtendedResource, nil, true,
			[]string{"no inspection facts"},
		},
		{
			"reserved dra-extended-resource fails closed",
			v1.GPUAllocationPolicyDRAExtendedResource, bothUsable, true,
			[]string{"reserved"},
		},
		{
			"unknown policy fails closed",
			"device-plugin", bothUsable, true,
			[]string{"unknown GPU allocation policy"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.policy, tt.mode)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Verify() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
			for _, want := range tt.wantInMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error message missing %q:\n%s", want, err.Error())
				}
			}
		})
	}
}
