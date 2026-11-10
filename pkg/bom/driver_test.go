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

package bom

import (
	"strings"
	"testing"
)

func TestDriverImageQualify(t *testing.T) {
	const digest = "sha256:9a831bfdf85f313b1f5749b7c9b2673bb8fff18b4ff768c9242dabaa4468e449"
	gpu := DriverImage{Kind: DriverKindGPU, Ref: "nvcr.io/nvidia/driver:580.173.02"}
	doca := DriverImage{Kind: DriverKindDOCA, Ref: "nvcr.io/nvidia/mellanox/doca-driver:doca3.2.0-25.10-1.2.8.0-2"}
	ubuntu := DriverTarget{OSID: "ubuntu", OSVersion: "24.04"}

	tests := []struct {
		name       string
		driver     DriverImage
		target     DriverTarget
		wantRef    string
		wantReason string // substring; empty when ok
	}{
		{"gpu ubuntu", gpu, ubuntu, "nvcr.io/nvidia/driver:580.173.02-ubuntu24.04", ""},
		{"gpu rhel", gpu, DriverTarget{OSID: "rhel", OSVersion: "9.4"}, "nvcr.io/nvidia/driver:580.173.02-rhel9.4", ""},
		{"gpu ignores arch", gpu, DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: ArchARM64},
			"nvcr.io/nvidia/driver:580.173.02-ubuntu24.04", ""},
		{"doca amd64", doca, DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: ArchAMD64},
			"nvcr.io/nvidia/mellanox/doca-driver:doca3.2.0-25.10-1.2.8.0-2-ubuntu24.04-amd64", ""},
		{"doca arm64", doca, DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: ArchARM64},
			"nvcr.io/nvidia/mellanox/doca-driver:doca3.2.0-25.10-1.2.8.0-2-ubuntu24.04-arm64", ""},
		{"doca without arch", doca, ubuntu, "", "node architecture"},
		{"doca unknown arch", doca, DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: "s390x"}, "", "node architecture"},
		{"missing os id", gpu, DriverTarget{OSVersion: "24.04"}, "", "does not pin one OS"},
		{"missing os version", gpu, DriverTarget{OSID: "ubuntu"}, "", "does not pin one OS"},
		{"amazon linux", gpu, DriverTarget{OSID: "amzn", OSVersion: "2023"}, "", `OS "amzn"`},
		{"sles", gpu, DriverTarget{OSID: "sles", OSVersion: "15.7"}, "", `OS "sles"`},
		{"talos", gpu, DriverTarget{OSID: "talos"}, "", "does not pin one OS"},
		{"gpu precompiled", DriverImage{Kind: DriverKindGPU, Ref: gpu.Ref, Precompiled: true}, ubuntu, "", "kernel"},
		{"doca precompiled", DriverImage{Kind: DriverKindDOCA, Ref: doca.Ref, Precompiled: true}, ubuntu, "", "kernel"},
		{"gpu digest passes through", DriverImage{Kind: DriverKindGPU, Ref: "nvcr.io/nvidia/driver@" + digest},
			DriverTarget{}, "nvcr.io/nvidia/driver@" + digest, ""},
		{"doca digest passes through", DriverImage{Kind: DriverKindDOCA, Ref: "nvcr.io/nvidia/mellanox/doca-driver@" + digest},
			DriverTarget{}, "nvcr.io/nvidia/mellanox/doca-driver@" + digest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, reason, ok := tt.driver.Qualify(tt.target)
			if wantOK := tt.wantReason == ""; ok != wantOK {
				t.Fatalf("Qualify() ok = %v, want %v (reason %q)", ok, wantOK, reason)
			}
			if ref != tt.wantRef {
				t.Errorf("Qualify() ref = %q, want %q", ref, tt.wantRef)
			}
			if !strings.Contains(reason, tt.wantReason) {
				t.Errorf("Qualify() reason = %q, want it to contain %q", reason, tt.wantReason)
			}
		})
	}
}

func TestDriverImageTagPattern(t *testing.T) {
	if got := (DriverImage{Kind: DriverKindGPU}).TagPattern(); got != "<version>-<os><os-version>" {
		t.Errorf("GPU TagPattern() = %q", got)
	}
	if got := (DriverImage{Kind: DriverKindDOCA}).TagPattern(); got != "<version>-<os><os-version>-<arch>" {
		t.Errorf("DOCA TagPattern() = %q", got)
	}
}
