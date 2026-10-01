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

package gkenet

import (
	"fmt"
	"strings"
	"testing"
)

// nicEntry builds one real-schema nic-info entry (gke-networking-api shape).
func nicEntry(name, pci string) string {
	return fmt.Sprintf(`{"birthIP":"","birthName":%q,"pciAddress":%q}`, name, pci)
}

func nicAnnotation(pairs [][2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, nicEntry(p[0], p[1]))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// healthyPairs: eth0 + eth1..eth8 at the observed a3 slots.
func healthyPairs() [][2]string {
	p := make([][2]string, 0, 1+len(tcpXOInterfaces))
	p = append(p, [2]string{"eth0", "0000:00:05.0"})
	for i, name := range tcpXOInterfaces {
		p = append(p, [2]string{name, observedA3GPUNICSlots[i]})
	}
	return p
}

func TestParseNICInfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		annotation     string
		wantErr        bool
		wantInterfaces int
		wantMissing    []string
		wantExtra      []string
	}{
		{
			name:           "healthy node",
			annotation:     nicAnnotation(healthyPairs()),
			wantInterfaces: 8,
		},
		{
			name: "extra interface beyond eth8",
			annotation: nicAnnotation(append(healthyPairs(),
				[2]string{"eth9", "0000:20:00.0"})),
			wantInterfaces: 8,
			wantExtra:      []string{"eth9"},
		},
		{
			name: "fewer than 8 GPU NICs",
			annotation: nicAnnotation(append([][2]string{{"eth0", "0000:00:05.0"}},
				func() [][2]string {
					var p [][2]string
					for i := 0; i < 7; i++ {
						p = append(p, [2]string{tcpXOInterfaces[i], observedA3GPUNICSlots[i]})
					}
					return p
				}()...)),
			wantInterfaces: 7,
			wantMissing:    []string{"eth8"},
		},
		{
			name:       "duplicate interface name fails",
			annotation: `[` + nicEntry("eth1", "0000:06:00.0") + `,` + nicEntry("eth1", "0000:07:00.0") + `]`,
			wantErr:    true,
		},
		{
			name:       "empty annotation fails",
			annotation: "",
			wantErr:    true,
		},
		{
			name:       "malformed JSON fails",
			annotation: "{not json",
			wantErr:    true,
		},
		{
			name:       "empty list fails",
			annotation: "[]",
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info, err := ParseNICInfo(tt.annotation)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", info)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if info.GPUNICInterfaces != tt.wantInterfaces {
				t.Errorf("GPUNICInterfaces = %d, want %d", info.GPUNICInterfaces, tt.wantInterfaces)
			}
			if got := info.SortedMissing(); strings.Join(got, ",") != strings.Join(tt.wantMissing, ",") {
				t.Errorf("missing = %v, want %v", got, tt.wantMissing)
			}
			if strings.Join(info.ExtraInterfaces, ",") != strings.Join(tt.wantExtra, ",") {
				t.Errorf("extra = %v, want %v", info.ExtraInterfaces, tt.wantExtra)
			}
		})
	}
}

// liveA3NICAnnotation is the real networking.gke.io/nic-info annotation captured
// from a healthy a3-megagpu-8g node on staging: eth0 (gVNIC) at 0000:00:0c.0 and
// the 8 GPU NICs at the observed slots 06,07,0d,0e,86,87,8d,8e.
const liveA3NICAnnotation = `[{"birthIP":"10.0.0.9","birthName":"eth0","pciAddress":"0000:00:0c.0"},{"birthIP":"10.0.16.3","birthName":"eth1","pciAddress":"0000:06:00.0"},{"birthIP":"10.0.32.3","birthName":"eth2","pciAddress":"0000:07:00.0"},{"birthIP":"10.0.48.3","birthName":"eth3","pciAddress":"0000:0d:00.0"},{"birthIP":"10.0.64.3","birthName":"eth4","pciAddress":"0000:0e:00.0"},{"birthIP":"10.0.80.3","birthName":"eth5","pciAddress":"0000:86:00.0"},{"birthIP":"10.0.96.3","birthName":"eth6","pciAddress":"0000:87:00.0"},{"birthIP":"10.0.112.3","birthName":"eth7","pciAddress":"0000:8d:00.0"},{"birthIP":"10.0.128.3","birthName":"eth8","pciAddress":"0000:8e:00.0"}]`

// Golden: the real captured annotation parses healthy (no missing/extra, no
// deviation) and maps eth1..eth8 to the observed slots.
func TestParseNICInfoLiveAnnotation(t *testing.T) {
	t.Parallel()
	info, err := ParseNICInfo(liveA3NICAnnotation)
	if err != nil {
		t.Fatalf("ParseNICInfo on the live captured annotation: %v", err)
	}
	if info.GPUNICInterfaces != 8 || len(info.MissingInterfaces) != 0 || len(info.ExtraInterfaces) != 0 {
		t.Errorf("live annotation must be healthy, got interfaces=%d missing=%v extra=%v",
			info.GPUNICInterfaces, info.MissingInterfaces, info.ExtraInterfaces)
	}
	if len(info.ObservedGPUNICSlotDeviation()) != 0 {
		t.Errorf("live annotation must not deviate from the observed layout, got %v", info.ObservedGPUNICSlotDeviation())
	}
	want := map[string]string{"eth1": "0000:06:00.0", "eth8": "0000:8e:00.0"}
	for iface, pci := range want {
		if info.Interfaces[iface] != pci {
			t.Errorf("%s must map to %s, got %s", iface, pci, info.Interfaces[iface])
		}
	}
}

// Observed-slot deviation is informational only and never feeds pass/fail.
func TestObservedSlotDeviationIsInformational(t *testing.T) {
	t.Parallel()
	// A node whose eth1..eth8 are at DIFFERENT (but complete) slots should parse
	// fine and report a deviation — pass/fail is the caller's, not this field's.
	pairs := make([][2]string, 0, 1+len(tcpXOInterfaces))
	pairs = append(pairs, [2]string{"eth0", "0000:00:05.0"})
	for i, name := range tcpXOInterfaces {
		pairs = append(pairs, [2]string{name, fmt.Sprintf("0001:0%d:00.0", i)})
	}
	info, err := ParseNICInfo(nicAnnotation(pairs))
	if err != nil {
		t.Fatalf("ParseNICInfo: %v", err)
	}
	if info.GPUNICInterfaces != 8 {
		t.Errorf("complete-but-different slots must still count 8, got %d", info.GPUNICInterfaces)
	}
	if len(info.ObservedGPUNICSlotDeviation()) != 8 {
		t.Errorf("all 8 should be flagged as deviating from the observed norm, got %v", info.ObservedGPUNICSlotDeviation())
	}
}

// Canonical extra-interface detection: anything not a round-trip eth0..eth8 is
// extra — a non-eth name (gve1), a padded/signed numeric (eth08, eth-1, eth+5),
// eth9+, or two interfaces sharing one PCI address.
func TestParseNICInfoCanonicalExtras(t *testing.T) {
	t.Parallel()
	base := func() [][2]string { // healthy eth1..eth8 + eth0
		pairs := make([][2]string, 0, 1+len(tcpXOInterfaces))
		pairs = append(pairs, [2]string{"eth0", "0000:00:0c.0"})
		for i, name := range tcpXOInterfaces {
			pairs = append(pairs, [2]string{name, observedA3GPUNICSlots[i]})
		}
		return pairs
	}
	cases := []struct {
		name      string
		add       [2]string
		wantExtra string
	}{
		{"extra gVNIC gve1", [2]string{"gve1", "0000:0f:00.0"}, "gve1"},
		{"padded eth08", [2]string{"eth08", "0000:0f:00.0"}, "eth08"},
		{"negative eth-1", [2]string{"eth-1", "0000:0f:00.0"}, "eth-1"},
		{"eth9 beyond range", [2]string{"eth9", "0000:0f:00.0"}, "eth9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pairs := append(base(), tc.add)
			info, err := ParseNICInfo(nicAnnotation(pairs))
			if err != nil {
				t.Fatalf("ParseNICInfo: %v", err)
			}
			found := false
			for _, e := range info.ExtraInterfaces {
				if e == tc.wantExtra {
					found = true
				}
			}
			if !found {
				t.Errorf("%q must be flagged extra, got %v", tc.wantExtra, info.ExtraInterfaces)
			}
		})
	}
}

// Two interfaces sharing one PCI address land in DuplicatePCIs (deterministic,
// sorted), NOT in ExtraInterfaces.
func TestParseNICInfoDuplicatePCI(t *testing.T) {
	t.Parallel()
	pairs := make([][2]string, 0, 2+len(tcpXOInterfaces))
	pairs = append(pairs, [2]string{"eth0", "0000:00:0c.0"})
	for i, name := range tcpXOInterfaces {
		pairs = append(pairs, [2]string{name, observedA3GPUNICSlots[i]})
	}
	pairs = append(pairs, [2]string{"eth9", observedA3GPUNICSlots[0]}) // eth9 shares eth1's PCI
	info, err := ParseNICInfo(nicAnnotation(pairs))
	if err != nil {
		t.Fatalf("ParseNICInfo: %v", err)
	}
	if len(info.DuplicatePCIs) != 1 {
		t.Fatalf("expected 1 duplicate PCI, got %v", info.DuplicatePCIs)
	}
	// eth9 is beyond eth8 so it is ALSO an extra; the duplicate must name both names.
	if !strings.Contains(info.DuplicatePCIs[0], "eth1") || !strings.Contains(info.DuplicatePCIs[0], "eth9") {
		t.Errorf("duplicate PCI must name both interfaces, got %v", info.DuplicatePCIs)
	}
}
