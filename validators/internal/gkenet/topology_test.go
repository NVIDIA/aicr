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

// TestParseNICInfoRealShape uses the annotation shape captured from a live
// a3-megagpu-8g node (eth0 at 00:0c.0, GPU NICs at the observed 06/07/0d/0e/86/87/8d/8e
// slots). It is the regression guard against a parser/detector that misfires on
// real data — it must parse healthy with no missing, no extra, no deviation.
func TestParseNICInfoRealShape(t *testing.T) {
	t.Parallel()
	pairs := make([][2]string, 0, 1+len(tcpXOInterfaces))
	pairs = append(pairs, [2]string{"eth0", "0000:0c:00.0"})
	for i, name := range tcpXOInterfaces {
		pairs = append(pairs, [2]string{name, observedA3GPUNICSlots[i]})
	}
	info, err := ParseNICInfo(nicAnnotation(pairs))
	if err != nil {
		t.Fatalf("ParseNICInfo on the real captured shape: %v", err)
	}
	if info.GPUNICInterfaces != 8 || len(info.MissingInterfaces) != 0 || len(info.ExtraInterfaces) != 0 {
		t.Errorf("real shape must be healthy, got interfaces=%d missing=%v extra=%v",
			info.GPUNICInterfaces, info.MissingInterfaces, info.ExtraInterfaces)
	}
	if len(info.ObservedGPUNICSlotDeviation()) != 0 {
		t.Errorf("real shape must not deviate from the observed layout, got %v", info.ObservedGPUNICSlotDeviation())
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
