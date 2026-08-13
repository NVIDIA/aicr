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

package recipe

import (
	"testing"
)

// TestNVSentinelAssumeDriverInstalledScope pins which resolved recipes carry
// nvsentinel.labeler.assumeDriverInstalled, per platform and profile value.
//
// Why this matters (see issue #2175, upstream NVIDIA/NVSentinel#1583):
// the NVSentinel labeler infers driver presence from a driver pod. Where no
// pod source exists, nvsentinel.dgxc.nvidia.com/driver.installed is never
// applied and metadata-collector plus both syslog-health-monitor DaemonSets
// report 0 desired pods — silently, with no error and no event, while
// gpu-health-monitor stays healthy because it selects on the DCGM label.
//
// It is applied per overlay to the three families whose DEFAULT cluster shape
// has no driver pod — AKS (azure-managed), GKE-COS (gke-default) and OKE —
// and to no other family. It is NOT gated per gpuStack value even though the
// non-default values do have a driver pod: naming a component in a profile
// fragment also makes its presence profile-owned, which would turn NVSentinel
// into a mandatory component and reject the supported
// "aicr bundle --set nv-sentinel:enabled=false" path. See the values file
// header for the full trade-off.
//
// This is a temporary workaround. When the upstream labeler falls back to the
// GFD-published driver evidence (nvidia.com/cuda.driver-version.full), delete
// recipes/components/nvsentinel/values-preinstalled-driver.yaml, its three
// overlay references, and this test.
func TestNVSentinelAssumeDriverInstalledScope(t *testing.T) {
	tests := []struct {
		name       string
		criteria   *Criteria
		profile    string
		wantSet    bool
		wantAssume bool
	}{
		{
			name: "aks default (azure-managed) assumes the preinstalled driver",
			criteria: &Criteria{
				Service:     CriteriaServiceAKS,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
			},
			wantSet:    true,
			wantAssume: true,
		},
		{
			name: "aks operator-managed inherits the overlay value",
			criteria: &Criteria{
				Service:     CriteriaServiceAKS,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
			},
			profile:    "gpuStack=operator-managed",
			wantSet:    true,
			wantAssume: true,
		},
		{
			name: "gke-cos default (gke-default) assumes the bundled driver",
			criteria: &Criteria{
				Service:     CriteriaServiceGKE,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSCOS,
				Intent:      CriteriaIntentTraining,
			},
			wantSet:    true,
			wantAssume: true,
		},
		{
			name: "gke-cos driver-installer inherits the overlay value",
			criteria: &Criteria{
				Service:     CriteriaServiceGKE,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSCOS,
				Intent:      CriteriaIntentTraining,
			},
			profile:    "gpuStack=driver-installer",
			wantSet:    true,
			wantAssume: true,
		},
		{
			name: "oke assumes the node-image driver",
			criteria: &Criteria{
				Service:     CriteriaServiceOKE,
				Accelerator: CriteriaAcceleratorA100,
				OS:          CriteriaOSOracleLinux,
				Intent:      CriteriaIntentTraining,
			},
			wantSet:    true,
			wantAssume: true,
		},
		{
			name: "eks leaves the chart default alone (the operator installs the driver)",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
			},
			wantSet: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), tt.criteria, tt.profile)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile(%q) error = %v", tt.profile, err)
			}

			sentinel := result.GetComponentRef("nvsentinel")
			if sentinel == nil {
				t.Fatal("nvsentinel componentRef missing — the scope check would be vacuous")
			}

			if !sentinel.IsEnabled() {
				t.Fatal("nvsentinel is disabled — the scope check would be vacuous")
			}

			values, err := result.GetValuesForComponentWithContext(t.Context(), "nvsentinel")
			if err != nil {
				t.Fatalf("GetValuesForComponent(nvsentinel): %v", err)
			}

			labeler, ok := values["labeler"].(map[string]any)
			if !ok {
				if tt.wantSet {
					t.Fatalf("nvsentinel values carry no labeler block, want assumeDriverInstalled=%v.\n"+
						"  Without it the labeler never applies nvsentinel.dgxc.nvidia.com/driver.installed\n"+
						"  and metadata-collector plus both syslog-health-monitor DaemonSets silently report\n"+
						"  0 desired pods. See issue #2175 and upstream NVIDIA/NVSentinel#1583.", tt.wantAssume)
				}

				return
			}

			assume, present := labeler["assumeDriverInstalled"].(bool)
			if !present {
				if tt.wantSet {
					t.Fatalf("nvsentinel labeler block has no assumeDriverInstalled, want %v", tt.wantAssume)
				}

				return
			}

			if !tt.wantSet {
				t.Fatalf("nvsentinel.labeler.assumeDriverInstalled = %v, want it left unset.\n"+
					"  The knob skips driver-pod detection and labels every GPU node unconditionally,\n"+
					"  so it belongs only where no pod source installs the driver. See issue #2175.", assume)
			}

			if assume != tt.wantAssume {
				t.Fatalf("nvsentinel.labeler.assumeDriverInstalled = %v, want %v", assume, tt.wantAssume)
			}
		})
	}
}

// TestNVSentinelStaysDisableableUnderProfiles pins that the workaround above
// does not make NVSentinel mandatory.
//
// Naming a component in a gpuStack profile fragment also makes its PRESENCE
// profile-owned (the synthetic "enabled" path), and the profile lock then
// rejects any output where the component is absent or disabled — turning
// "aicr bundle --set nv-sentinel:enabled=false" into a hard failure on the
// AKS and GKE-COS families. That is why labeler.assumeDriverInstalled is set
// on the overlay's componentRefs instead of per profile value.
func TestNVSentinelStaysDisableableUnderProfiles(t *testing.T) {
	tests := []struct {
		name     string
		criteria *Criteria
		profile  string
	}{
		{
			name: "aks default",
			criteria: &Criteria{
				Service:     CriteriaServiceAKS,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
			},
		},
		{
			name: "gke-cos default",
			criteria: &Criteria{
				Service:     CriteriaServiceGKE,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSCOS,
				Intent:      CriteriaIntentTraining,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), tt.criteria, tt.profile)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile() error = %v", err)
			}

			selected := result.Metadata.SelectedProfile
			if selected == nil {
				t.Fatal("no profile selected — the ownership check would be vacuous")
			}

			if paths, owned := selected.OwnedPaths["nvsentinel"]; owned {
				t.Fatalf("gpuStack owns nvsentinel paths %v, want none.\n"+
					"  Profile ownership includes the synthetic presence path, so the profile lock\n"+
					"  rejects any bundle where nvsentinel is absent or disabled — breaking\n"+
					"  \"aicr bundle --set nv-sentinel:enabled=false\". Set\n"+
					"  labeler.assumeDriverInstalled on the overlay's componentRefs instead.\n"+
					"  See issue #2175.", paths)
			}
		})
	}
}
